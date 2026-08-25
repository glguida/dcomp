package lifecycle

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/glguida/dcomp/composition"
	"github.com/glguida/dcomp/engine"
	"github.com/glguida/dcomp/proxy"
	"github.com/glguida/dcomp/state"
)

// The real proxy is addressed by its control socket, not looked up by the
// instance ID supplied by its caller. Keep the lifecycle fake faithful to that
// identity boundary so recovery tests cannot mistake a different live proxy
// for an absent one or launch two processes into one runtime directory.
func TestFakeProxyManagerModelsControlSocketIdentity(t *testing.T) {
	manager := newFakeProxyManager()
	runtimeDir := t.TempDir()
	spec := composition.ResolvedSpec{Name: "demo"}
	firstConfig, err := proxy.NewConfig(spec, runtimeDir, "first-instance")
	if err != nil {
		t.Fatal(err)
	}
	first, err := manager.Ensure(context.Background(), firstConfig)
	if err != nil {
		t.Fatal(err)
	}

	wrongIdentity := first
	wrongIdentity.InstanceID = "second-instance"
	if _, err := manager.Inspect(context.Background(), wrongIdentity); !errors.Is(
		err,
		proxy.ErrIdentityMismatch,
	) {
		t.Fatalf("Inspect with wrong identity error = %v, want identity mismatch", err)
	}

	secondConfig, err := proxy.NewConfig(spec, runtimeDir, "second-instance")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Ensure(context.Background(), secondConfig); err == nil {
		t.Fatal("Ensure in occupied runtime succeeded")
	}
	manager.mu.Lock()
	processes := len(manager.processes)
	manager.mu.Unlock()
	if processes != 1 {
		t.Fatalf("live fake proxy processes = %d, want 1", processes)
	}
}

func TestMinimalApplyRetainsStoppedVerifiedContainerOnSameSpec(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installImages(
		fake,
		"provider:v1", "consumer:v1",
		"sha256:provider", "sha256:consumer",
	)
	spec := linkedSpec("provider:v1", "consumer:v1")
	if err := controller.Up(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	before := requireDesired(t, controller.State, spec.Name)
	providerID := before.Containers["provider"].ID
	fake.exitContainerOutOfBand(providerID)
	fake.resetCalls()

	if err := controller.Up(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	after := requireDesired(t, controller.State, spec.Name)
	requireSameContainer(t, after, "provider", providerID)
	requireSameContainer(t, after, "consumer", before.Containers["consumer"].ID)
	if after.Proxy.InstanceID != before.Proxy.InstanceID {
		t.Fatal("repairing a stopped container replaced the proxy")
	}
	want := []engineCall{{Method: "start-container", Target: providerID}}
	if got := fake.mutationCalls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("same-spec stopped-container mutations:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestMinimalApplyRelinkPlanIsIndependentOfContainerExitTiming(t *testing.T) {
	tests := []struct {
		name         string
		beforeApply  bool
		duringResync bool
	}{
		{name: "exited before retention selection", beforeApply: true},
		{name: "exited during proxy resync", duringResync: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			controller, fake := newControllerHarness(t)
			installImages(
				fake,
				"provider-a:v1", "provider-b:v1", "consumer:v1",
				"sha256:provider-a", "sha256:provider-b", "sha256:consumer",
			)
			initial := relinkSpec("provider-a")
			if err := controller.Up(context.Background(), initial); err != nil {
				t.Fatal(err)
			}
			before := requireDesired(t, controller.State, initial.Name)
			consumerID := before.Containers["consumer"].ID
			if test.beforeApply {
				fake.exitContainerOutOfBand(consumerID)
			}
			manager := controller.Proxy.(*fakeProxyManager)
			if test.duringResync {
				manager.beforeResync = func(proxy.Wiring) error {
					fake.exitContainerOutOfBand(consumerID)
					return nil
				}
			}
			fake.resetCalls()

			target := relinkSpec("provider-b")
			if err := controller.Up(context.Background(), target); err != nil {
				t.Fatal(err)
			}
			after := requireDesired(t, controller.State, target.Name)
			if after.Proxy.InstanceID != before.Proxy.InstanceID {
				t.Fatal("relink replaced the proxy")
			}
			for _, component := range []string{"provider-a", "provider-b", "consumer"} {
				requireSameContainer(t, after, component, before.Containers[component].ID)
			}
			want := []engineCall{{Method: "start-container", Target: consumerID}}
			if got := fake.mutationCalls(); !reflect.DeepEqual(got, want) {
				t.Fatalf("relink mutations:\n got: %#v\nwant: %#v", got, want)
			}
		})
	}
}

func TestMinimalApplyRepairsStoppedRetainedContainerAttachmentBeforeStart(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installImages(
		fake,
		"provider:v1", "consumer:v1",
		"sha256:provider", "sha256:consumer",
	)
	spec := linkedSpec("provider:v1", "consumer:v1")
	spec.Components[1].Runtime.ExternalEgress = true
	if err := controller.Up(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	before := requireDesired(t, controller.State, spec.Name)
	consumerID := before.Containers["consumer"].ID
	networkID := before.Networks[componentNetworkKey("consumer")].ID
	fake.exitContainerOutOfBand(consumerID)
	if err := fake.DisconnectNetwork(context.Background(), networkID, consumerID); err != nil {
		t.Fatal(err)
	}
	fake.resetCalls()

	if err := controller.Up(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	after := requireDesired(t, controller.State, spec.Name)
	requireSameContainer(t, after, "consumer", consumerID)
	requireSameContainer(t, after, "provider", before.Containers["provider"].ID)
	want := []engineCall{
		{Method: "connect-network", Target: networkID + "->" + consumerID},
		{Method: "start-container", Target: consumerID},
	}
	if got := fake.mutationCalls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("stopped egress repair mutations:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestMinimalApplyRecreatesContainerOnlyWhenReuseFactsChange(t *testing.T) {
	tests := []struct {
		name   string
		change func(*fakeEngine, string)
		target func() composition.Spec
	}{
		{
			name: "container disappeared",
			change: func(fake *fakeEngine, id string) {
				fake.deleteContainerOutOfBand(id)
			},
			target: func() composition.Spec {
				return linkedSpec("provider:v1", "consumer:v1")
			},
		},
		{
			name: "container definition changed while stopped",
			change: func(fake *fakeEngine, id string) {
				fake.exitContainerOutOfBand(id)
			},
			target: func() composition.Spec {
				return linkedSpec("provider:v2", "consumer:v1")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			controller, fake := newControllerHarness(t)
			installImages(
				fake,
				"provider:v1", "provider:v2", "consumer:v1",
				"sha256:provider-v1", "sha256:provider-v2", "sha256:consumer",
			)
			initial := linkedSpec("provider:v1", "consumer:v1")
			if err := controller.Up(context.Background(), initial); err != nil {
				t.Fatal(err)
			}
			before := requireDesired(t, controller.State, initial.Name)
			providerID := before.Containers["provider"].ID
			test.change(fake, providerID)

			target := test.target()
			if err := controller.Up(context.Background(), target); err != nil {
				t.Fatal(err)
			}
			after := requireDesired(t, controller.State, target.Name)
			if after.Containers["provider"].ID == providerID {
				t.Fatal("container without reusable identity facts was retained")
			}
			requireSameContainer(t, after, "consumer", before.Containers["consumer"].ID)
			if after.Proxy.InstanceID != before.Proxy.InstanceID {
				t.Fatal("container-only repair replaced the proxy")
			}
		})
	}
}

// Selection is only a snapshot. A retained container can change state while
// the proxy is being resynced, so the final create/start phases must converge
// from observed state rather than treating "retained" as "already complete".
func TestMinimalApplyConvergesRetainedContainerStateChangesDuringResync(t *testing.T) {
	tests := []struct {
		name           string
		change         func(*fakeEngine, string)
		wantRetainedID bool
	}{
		{
			name: "exited",
			change: func(fake *fakeEngine, id string) {
				fake.exitContainerOutOfBand(id)
			},
			wantRetainedID: true,
		},
		{
			name: "removed",
			change: func(fake *fakeEngine, id string) {
				fake.deleteContainerOutOfBand(id)
			},
			wantRetainedID: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			controller, fake := newControllerHarness(t)
			installImages(
				fake,
				"provider:v1", "consumer:v1", "observer:v1",
				"sha256:provider", "sha256:consumer", "sha256:observer",
			)
			initial := linkedSpec("provider:v1", "consumer:v1")
			if err := controller.Up(context.Background(), initial); err != nil {
				t.Fatal(err)
			}
			before := requireDesired(t, controller.State, initial.Name)
			providerID := before.Containers["provider"].ID
			manager := controller.Proxy.(*fakeProxyManager)
			manager.beforeResync = func(_ proxy.Wiring) error {
				test.change(fake, providerID)
				return nil
			}

			target := fanoutSpec("provider:v1", "consumer:v1", "observer:v1")
			if err := controller.Up(context.Background(), target); err != nil {
				t.Fatal(err)
			}
			after := requireDesired(t, controller.State, target.Name)
			if after.Proxy.InstanceID != before.Proxy.InstanceID {
				t.Fatal("container drift during resync replaced the proxy")
			}
			if got := after.Containers["provider"].ID; (got == providerID) != test.wantRetainedID {
				t.Fatalf(
					"provider ID after %s = %q (before %q), retained=%t",
					test.name, got, providerID, got == providerID,
				)
			}
			actual, err := fake.InspectContainer(
				context.Background(), after.Containers["provider"].ID,
			)
			if err != nil {
				t.Fatal(err)
			}
			if !actual.Running {
				t.Fatalf("provider is %q after successful apply", actual.Status)
			}
		})
	}
}

func TestApplyResumeRepairsTargetNetworkLostAfterNetworksPhase(t *testing.T) {
	for _, phase := range []string{
		phaseResync,
		phaseCreate,
		phaseAttach,
		phaseStart,
		phaseCommit,
	} {
		t.Run(phase, func(t *testing.T) {
			fixture := prepareInitialEgressApplyAtPhase(t, phase)
			lost := fixture.operation.Networks[componentNetworkKey("consumer")]
			consumerBefore, consumerExists := fixture.operation.Containers["consumer"]
			providerBefore, providerExists := fixture.operation.Containers["provider"]
			fixture.fake.deleteNetworkOutOfBand(lost.ID)

			if err := fixture.controller.Resume(context.Background(), fixture.target.Name); err != nil {
				t.Fatal(err)
			}
			deployed := requireDesired(t, fixture.controller.State, fixture.target.Name)
			requireNoOperation(t, fixture.controller.State, fixture.target.Name)
			if got := deployed.Networks[componentNetworkKey("consumer")].ID; got == lost.ID {
				t.Fatalf("deployment retained missing network %q", got)
			}
			if consumerExists && deployed.Containers["consumer"].ID == consumerBefore.ID {
				t.Fatalf(
					"deployment retained consumer %q configured for missing network %q",
					consumerBefore.ID,
					lost.ID,
				)
			}
			if providerExists {
				requireSameContainer(t, deployed, "provider", providerBefore.ID)
			}
			if fixture.operation.Proxy != nil &&
				deployed.Proxy.InstanceID != fixture.operation.Proxy.InstanceID {
				t.Fatal("network repair replaced the healthy proxy")
			}
			assertDeploymentMatches(t, fixture.controller, deployed, fixture.target)
		})
	}
}

// A retained container's create-time network ID is an immutable dependency,
// even when Docker no longer reports that attachment after the network has
// disappeared. Exercise every durable apply phase so recovery cannot be made
// correct at one phase boundary while leaving the adjacent boundary stale.
func TestApplyRecreatesContainersPinnedToMissingNetworkGenerationAtEveryPhase(t *testing.T) {
	tests := []struct {
		name                string
		phase               string
		lostBeforeSelection bool
	}{
		{name: "before retention selection", phase: phaseRetire, lostBeforeSelection: true},
		{name: phaseRetire, phase: phaseRetire},
		{name: phaseNetworks, phase: phaseNetworks},
		{name: phaseResync, phase: phaseResync},
		{name: phaseCreate, phase: phaseCreate},
		{name: phaseAttach, phase: phaseAttach},
		{name: phaseStart, phase: phaseStart},
		{name: phaseCommit, phase: phaseCommit},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := prepareRetainedEgressApplyAtPhase(
				t,
				test.phase,
				func(fake *fakeEngine, previous state.Deployment) {
					if test.lostBeforeSelection {
						fake.deleteNetworkOutOfBand(
							previous.Networks[componentNetworkKey("consumer")].ID,
						)
					}
				},
			)
			lost := fixture.previous.Networks[componentNetworkKey("consumer")]
			consumerBefore := fixture.previous.Containers["consumer"]
			providerBefore := fixture.previous.Containers["provider"]
			proxyBefore := fixture.previous.Proxy.InstanceID

			_, consumerRetained := fixture.operation.Containers["consumer"]
			if consumerRetained == test.lostBeforeSelection {
				t.Fatalf(
					"consumer retained = %t after network loss before selection = %t",
					consumerRetained,
					test.lostBeforeSelection,
				)
			}
			if !test.lostBeforeSelection {
				fixture.fake.deleteNetworkOutOfBand(lost.ID)
			}

			if err := fixture.controller.Resume(
				context.Background(), fixture.target.Name,
			); err != nil {
				t.Fatal(err)
			}
			deployed := requireDesired(t, fixture.controller.State, fixture.target.Name)
			requireNoOperation(t, fixture.controller.State, fixture.target.Name)
			if got := deployed.Networks[componentNetworkKey("consumer")].ID; got == lost.ID {
				t.Fatalf("deployment retained missing network generation %q", got)
			}
			if got := deployed.Containers["consumer"].ID; got == consumerBefore.ID {
				t.Fatalf(
					"deployment retained consumer %q pinned to missing network %q",
					got,
					lost.ID,
				)
			}
			requireSameContainer(t, deployed, "provider", providerBefore.ID)
			if deployed.Proxy.InstanceID != proxyBefore {
				t.Fatal("network-generation repair replaced the healthy proxy")
			}
			assertDeploymentMatches(t, fixture.controller, deployed, fixture.target)
		})
	}
}

func TestApplyResumeRepairsTargetContainerLostAfterCreatePhase(t *testing.T) {
	for _, phase := range []string{phaseAttach, phaseStart, phaseCommit} {
		t.Run(phase, func(t *testing.T) {
			fixture := prepareInitialEgressApplyAtPhase(t, phase)
			lost := fixture.operation.Containers["consumer"]
			provider := fixture.operation.Containers["provider"]
			network := fixture.operation.Networks[componentNetworkKey("consumer")]
			proxyID := fixture.operation.Proxy.InstanceID
			fixture.fake.deleteContainerOutOfBand(lost.ID)

			if err := fixture.controller.Resume(context.Background(), fixture.target.Name); err != nil {
				t.Fatal(err)
			}
			deployed := requireDesired(t, fixture.controller.State, fixture.target.Name)
			requireNoOperation(t, fixture.controller.State, fixture.target.Name)
			if got := deployed.Containers["consumer"].ID; got == lost.ID {
				t.Fatalf("deployment retained missing consumer %q", got)
			}
			requireSameContainer(t, deployed, "provider", provider.ID)
			if got := deployed.Networks[componentNetworkKey("consumer")].ID; got != network.ID {
				t.Fatalf("container repair replaced network %q with %q", network.ID, got)
			}
			if deployed.Proxy.InstanceID != proxyID {
				t.Fatal("container repair replaced the healthy proxy")
			}
			assertDeploymentMatches(t, fixture.controller, deployed, fixture.target)
		})
	}
}

func TestApplyResumeJournalsOrphanCleanupForMissingTargetContainer(t *testing.T) {
	fixture := prepareInitialEgressApplyAtPhase(t, phaseCommit)
	lost := fixture.operation.Containers["consumer"]
	provider := fixture.operation.Containers["provider"]
	network := fixture.operation.Networks[componentNetworkKey("consumer")]
	proxyID := fixture.operation.Proxy.InstanceID
	cleanupErr := errors.New("interrupt missing-container endpoint cleanup")
	fixture.fake.forceDisconnectErrors[network.ID+"->"+lost.Name] = []error{cleanupErr}
	fixture.fake.orphanContainerOutOfBand(lost.ID)
	requireOrphanEndpoint(t, fixture.fake, network.ID, lost.Name)

	err := fixture.controller.Resume(context.Background(), fixture.target.Name)
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("first Resume error = %v, want interrupted endpoint cleanup", err)
	}
	interrupted := requireOperation(t, fixture.controller.State, fixture.target.Name)
	if len(interrupted.EndpointCleanups) != 1 {
		t.Fatalf("endpoint cleanup journal = %#v, want one entry", interrupted.EndpointCleanups)
	}

	if err := fixture.controller.Resume(context.Background(), fixture.target.Name); err != nil {
		t.Fatal(err)
	}
	deployed := requireDesired(t, fixture.controller.State, fixture.target.Name)
	requireNoOperation(t, fixture.controller.State, fixture.target.Name)
	if deployed.Containers["consumer"].ID == lost.ID {
		t.Fatalf("deployment retained missing consumer %q", lost.ID)
	}
	requireSameContainer(t, deployed, "provider", provider.ID)
	if got := deployed.Networks[componentNetworkKey("consumer")].ID; got != network.ID {
		t.Fatalf("container recovery replaced network %q with %q", network.ID, got)
	}
	if deployed.Proxy.InstanceID != proxyID {
		t.Fatal("container recovery replaced the healthy proxy")
	}
	assertDeploymentMatches(t, fixture.controller, deployed, fixture.target)
}

// Dead-proxy fallback must not discard a missing target container's durable
// identity before using it to recover an orphaned Docker endpoint. The start
// case models Docker start and its completion markers landing before the
// journal advances to commit.
func TestApplyProxyLossFallbackRecoversOrphanedTargetContainer(t *testing.T) {
	for _, phase := range []string{phaseStart, phaseCommit} {
		t.Run(phase, func(t *testing.T) {
			fixture := prepareInitialEgressApplyAtPhase(t, phase)
			if phase == phaseStart {
				if err := fixture.controller.ensureTargetContainersRunning(
					context.Background(),
					&fixture.operation,
				); err != nil {
					t.Fatal(err)
				}
			}

			lost := fixture.operation.Containers["consumer"]
			network := fixture.operation.Networks[componentNetworkKey("consumer")]
			proxyID := fixture.operation.Proxy.InstanceID
			proxyPID := fixture.operation.Proxy.PID
			fixture.fake.orphanContainerOutOfBand(lost.ID)
			requireOrphanEndpoint(t, fixture.fake, network.ID, lost.Name)
			killFakeProxyOutOfBand(
				fixture.controller.Proxy.(*fakeProxyManager),
				proxyID,
			)

			if err := fixture.controller.Resume(
				context.Background(),
				fixture.target.Name,
			); err != nil {
				t.Fatal(err)
			}
			deployed := requireDesired(
				t,
				fixture.controller.State,
				fixture.target.Name,
			)
			requireNoOperation(t, fixture.controller.State, fixture.target.Name)
			if deployed.Containers["consumer"].ID == lost.ID {
				t.Fatalf("deployment retained missing consumer %q", lost.ID)
			}
			if deployed.Proxy.PID == proxyPID {
				t.Fatalf("deployment retained missing proxy PID %d", proxyPID)
			}
			if got := deployed.Networks[componentNetworkKey("consumer")].ID; got != network.ID {
				t.Fatalf("fallback replaced egress network %q with %q", network.ID, got)
			}
			assertDeploymentMatches(t, fixture.controller, deployed, fixture.target)
		})
	}
}

func TestApplyProxyLossFallbackPersistsOrphanCleanupBeforeDiscardingTarget(t *testing.T) {
	fixture := prepareInitialEgressApplyAtPhase(t, phaseCommit)
	lost := fixture.operation.Containers["consumer"]
	network := fixture.operation.Networks[componentNetworkKey("consumer")]
	cleanupErr := errors.New("interrupt target endpoint cleanup before proxy fallback")
	fixture.fake.forceDisconnectErrors[network.ID+"->"+lost.Name] = []error{cleanupErr}
	fixture.fake.orphanContainerOutOfBand(lost.ID)
	killFakeProxyOutOfBand(
		fixture.controller.Proxy.(*fakeProxyManager),
		fixture.operation.Proxy.InstanceID,
	)

	err := fixture.controller.Resume(context.Background(), fixture.target.Name)
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("first Resume error = %v, want interrupted endpoint cleanup", err)
	}
	interrupted := requireOperation(t, fixture.controller.State, fixture.target.Name)
	if len(interrupted.EndpointCleanups) != 1 {
		t.Fatalf("endpoint cleanup journal = %#v, want one entry", interrupted.EndpointCleanups)
	}
	requireOrphanEndpoint(t, fixture.fake, network.ID, lost.Name)

	if err := fixture.controller.Resume(context.Background(), fixture.target.Name); err != nil {
		t.Fatal(err)
	}
	deployed := requireDesired(t, fixture.controller.State, fixture.target.Name)
	requireNoOperation(t, fixture.controller.State, fixture.target.Name)
	assertDeploymentMatches(t, fixture.controller, deployed, fixture.target)
}

func TestSupersedingApplyRecoversOrphanedTargetBeforeProxyFallback(t *testing.T) {
	fixture := prepareInitialEgressApplyAtPhase(t, phaseCommit)
	lost := fixture.operation.Containers["consumer"]
	network := fixture.operation.Networks[componentNetworkKey("consumer")]
	fixture.fake.orphanContainerOutOfBand(lost.ID)
	requireOrphanEndpoint(t, fixture.fake, network.ID, lost.Name)
	killFakeProxyOutOfBand(
		fixture.controller.Proxy.(*fakeProxyManager),
		fixture.operation.Proxy.InstanceID,
	)
	installImages(fixture.fake, "consumer:v2", "sha256:consumer-v2")
	replacement := linkedSpec("provider:v1", "consumer:v2")
	replacement.Components[1].Runtime.ExternalEgress = true
	resolvedReplacement, err := fixture.controller.resolve(
		context.Background(),
		replacement,
	)
	if err != nil {
		t.Fatal(err)
	}

	if err := fixture.controller.Up(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	deployed := requireDesired(t, fixture.controller.State, fixture.target.Name)
	requireNoOperation(t, fixture.controller.State, fixture.target.Name)
	if actual := fixture.fake.containers[deployed.Containers["consumer"].ID]; actual.ImageID != "sha256:consumer-v2" {
		t.Fatalf("replacement consumer image = %q", actual.ImageID)
	}
	if deployed.Spec.Digest != resolvedReplacement.Digest {
		t.Fatalf("deployed digest = %q, want %q", deployed.Spec.Digest, resolvedReplacement.Digest)
	}
	assertDeploymentMatches(t, fixture.controller, deployed, resolvedReplacement)
}

// A retained resource is only a plan. If its container disappears, any Docker
// endpoint left behind must be recovered before the phase's strict member
// preflight, regardless of which durable phase is resumed.
func TestApplyRecoversOrphanFromRetainedContainerAtEveryPhase(t *testing.T) {
	for _, phase := range []string{
		phaseRetire,
		phaseNetworks,
		phaseResync,
		phaseCreate,
		phaseAttach,
		phaseStart,
		phaseCommit,
	} {
		t.Run(phase, func(t *testing.T) {
			fixture := prepareRetainedEgressApplyAtPhase(t, phase, nil)
			lost := fixture.operation.Containers["consumer"]
			network := fixture.operation.Networks[componentNetworkKey("consumer")]
			provider := fixture.previous.Containers["provider"]
			proxyID := fixture.previous.Proxy.InstanceID
			fixture.fake.orphanContainerOutOfBand(lost.ID)
			requireOrphanEndpoint(t, fixture.fake, network.ID, lost.Name)

			if err := fixture.controller.Resume(
				context.Background(), fixture.target.Name,
			); err != nil {
				t.Fatal(err)
			}
			deployed := requireDesired(t, fixture.controller.State, fixture.target.Name)
			requireNoOperation(t, fixture.controller.State, fixture.target.Name)
			if deployed.Containers["consumer"].ID == lost.ID {
				t.Fatalf("deployment retained missing consumer %q", lost.ID)
			}
			requireSameContainer(t, deployed, "provider", provider.ID)
			if got := deployed.Networks[componentNetworkKey("consumer")].ID; got != network.ID {
				t.Fatalf("orphan recovery replaced network %q with %q", network.ID, got)
			}
			if deployed.Proxy.InstanceID != proxyID {
				t.Fatal("orphan recovery replaced the healthy proxy")
			}
			assertDeploymentMatches(t, fixture.controller, deployed, fixture.target)
		})
	}
}

func TestApplyRetainedContainerOrphanCleanupIsDurableAcrossInterruption(t *testing.T) {
	fixture := prepareRetainedEgressApplyAtPhase(t, phaseRetire, nil)
	lost := fixture.operation.Containers["consumer"]
	network := fixture.operation.Networks[componentNetworkKey("consumer")]
	cleanupErr := errors.New("interrupt retained-container endpoint cleanup")
	fixture.fake.forceDisconnectErrors[network.ID+"->"+lost.Name] = []error{cleanupErr}
	fixture.fake.orphanContainerOutOfBand(lost.ID)

	err := fixture.controller.Resume(context.Background(), fixture.target.Name)
	if !errors.Is(err, cleanupErr) {
		t.Fatalf("first Resume error = %v, want interrupted endpoint cleanup", err)
	}
	interrupted := requireOperation(t, fixture.controller.State, fixture.target.Name)
	if len(interrupted.EndpointCleanups) != 1 {
		t.Fatalf("endpoint cleanup journal = %#v, want one entry", interrupted.EndpointCleanups)
	}
	requireOrphanEndpoint(t, fixture.fake, network.ID, lost.Name)

	if err := fixture.controller.Resume(context.Background(), fixture.target.Name); err != nil {
		t.Fatal(err)
	}
	deployed := requireDesired(t, fixture.controller.State, fixture.target.Name)
	requireNoOperation(t, fixture.controller.State, fixture.target.Name)
	if deployed.Containers["consumer"].ID == lost.ID {
		t.Fatalf("deployment retained missing consumer %q", lost.ID)
	}
	assertDeploymentMatches(t, fixture.controller, deployed, fixture.target)
}

func TestApplyRecoversRetainedContainerOrphanedDuringResync(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installImages(
		fake,
		"provider:v1", "consumer:v1", "observer:v1",
		"sha256:provider", "sha256:consumer", "sha256:observer",
	)
	initial := linkedSpec("provider:v1", "consumer:v1")
	initial.Components[1].Runtime.ExternalEgress = true
	if err := controller.Up(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	previous := requireDesired(t, controller.State, initial.Name)
	lost := previous.Containers["consumer"]
	network := previous.Networks[componentNetworkKey("consumer")]
	manager := controller.Proxy.(*fakeProxyManager)
	manager.beforeResync = func(proxy.Wiring) error {
		fake.orphanContainerOutOfBand(lost.ID)
		return nil
	}

	target := fanoutSpec("provider:v1", "consumer:v1", "observer:v1")
	target.Components[1].Runtime.ExternalEgress = true
	resolvedTarget, err := controller.resolve(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Up(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	deployed := requireDesired(t, controller.State, target.Name)
	requireNoOperation(t, controller.State, target.Name)
	if deployed.Containers["consumer"].ID == lost.ID {
		t.Fatalf("deployment retained consumer %q removed during resync", lost.ID)
	}
	requireSameContainer(t, deployed, "provider", previous.Containers["provider"].ID)
	if got := deployed.Networks[componentNetworkKey("consumer")].ID; got != network.ID {
		t.Fatalf("resync orphan recovery replaced network %q with %q", network.ID, got)
	}
	if deployed.Proxy.InstanceID != previous.Proxy.InstanceID {
		t.Fatal("resync orphan recovery replaced the healthy proxy")
	}
	assertDeploymentMatches(t, controller, deployed, resolvedTarget)
}

func TestApplyNetworkRecoveryRefusesForeignContainerAttachmentBeforeMutation(t *testing.T) {
	fixture := prepareInitialEgressApplyAtPhase(t, phaseAttach)
	consumer := fixture.operation.Containers["consumer"]
	network := fixture.operation.Networks[componentNetworkKey("consumer")]
	fixture.fake.mu.Lock()
	actual := fixture.fake.containers[consumer.ID]
	actual.Networks["foreign"] = engine.NetworkAttachment{NetworkID: "foreign-network"}
	fixture.fake.containers[consumer.ID] = actual
	fixture.fake.mu.Unlock()
	fixture.fake.deleteNetworkOutOfBand(network.ID)
	fixture.fake.resetCalls()

	err := fixture.controller.Resume(context.Background(), fixture.target.Name)
	if err == nil || !strings.Contains(err.Error(), "undeclared network") {
		t.Fatalf("Resume error = %v, want undeclared-network refusal", err)
	}
	for _, method := range []string{"stop-container", "remove-container"} {
		if calls := fixture.fake.callsFor(method); len(calls) != 0 {
			t.Fatalf("foreign attachment caused %s calls: %#v", method, calls)
		}
	}
	if _, err := fixture.fake.InspectContainer(
		context.Background(),
		consumer.ID,
	); err != nil {
		t.Fatalf("foreign-attached container was mutated: %v", err)
	}
	requireOperation(t, fixture.controller.State, fixture.target.Name)
}

func TestApplyNetworkRecoveryRefusesReplacementAtRecordedNameBeforeMutation(t *testing.T) {
	fixture := prepareInitialEgressApplyAtPhase(t, phaseAttach)
	consumer := fixture.operation.Containers["consumer"]
	lost := fixture.operation.Networks[componentNetworkKey("consumer")]
	fixture.fake.deleteNetworkOutOfBand(lost.ID)
	replacement, err := fixture.fake.CreateNetwork(
		context.Background(),
		engine.NetworkRequest{Name: lost.Name},
	)
	if err != nil {
		t.Fatal(err)
	}
	fixture.fake.resetCalls()

	err = fixture.controller.Resume(context.Background(), fixture.target.Name)
	if err == nil || !strings.Contains(err.Error(), "no longer identifies recorded network") {
		t.Fatalf("Resume error = %v, want recorded-network identity refusal", err)
	}
	for _, method := range []string{"stop-container", "remove-container"} {
		if calls := fixture.fake.callsFor(method); len(calls) != 0 {
			t.Fatalf("network identity mismatch caused %s calls: %#v", method, calls)
		}
	}
	if _, err := fixture.fake.InspectContainer(
		context.Background(),
		consumer.ID,
	); err != nil {
		t.Fatalf("dependent container was mutated: %v", err)
	}
	if _, err := fixture.fake.InspectNetwork(
		context.Background(),
		replacement.ID,
	); err != nil {
		t.Fatalf("replacement network was mutated: %v", err)
	}
	requireOperation(t, fixture.controller.State, fixture.target.Name)
}

type initialEgressApplyFixture struct {
	controller *Controller
	fake       *fakeEngine
	target     composition.ResolvedSpec
	operation  state.Operation
}

type retainedEgressApplyFixture struct {
	controller *Controller
	fake       *fakeEngine
	previous   state.Deployment
	target     composition.ResolvedSpec
	operation  state.Operation
}

func prepareRetainedEgressApplyAtPhase(
	t *testing.T,
	phase string,
	beforeSelection func(*fakeEngine, state.Deployment),
) retainedEgressApplyFixture {
	t.Helper()
	ctx := context.Background()
	controller, fake := newControllerHarness(t)
	installImages(
		fake,
		"provider:v1", "consumer:v1",
		"sha256:provider", "sha256:consumer",
	)
	spec := linkedSpec("provider:v1", "consumer:v1")
	spec.Components[1].Runtime.ExternalEgress = true
	if err := controller.Up(ctx, spec); err != nil {
		t.Fatal(err)
	}
	previous := requireDesired(t, controller.State, spec.Name)
	target, err := controller.resolve(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := state.NewOperation(
		kindApply,
		phaseRetire,
		target,
		&previous,
		controller.RuntimeRoot,
	)
	if err != nil {
		t.Fatal(err)
	}
	if beforeSelection != nil {
		beforeSelection(fake, previous)
	}
	if err := controller.selectRetainedResources(ctx, &operation); err != nil {
		t.Fatal(err)
	}
	operation.Phase = phase
	if err := controller.State.WriteOperation(target.Name, operation); err != nil {
		t.Fatal(err)
	}
	return retainedEgressApplyFixture{
		controller: controller,
		fake:       fake,
		previous:   previous,
		target:     target,
		operation:  operation,
	}
}

func prepareInitialEgressApplyAtPhase(
	t *testing.T,
	phase string,
) initialEgressApplyFixture {
	t.Helper()
	ctx := context.Background()
	controller, fake := newControllerHarness(t)
	installImages(
		fake,
		"provider:v1", "consumer:v1",
		"sha256:provider", "sha256:consumer",
	)
	spec := linkedSpec("provider:v1", "consumer:v1")
	spec.Components[1].Runtime.ExternalEgress = true
	target, err := controller.resolve(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := state.NewOperation(
		kindApply,
		phaseRetire,
		target,
		nil,
		controller.RuntimeRoot,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.State.WriteOperation(target.Name, operation); err != nil {
		t.Fatal(err)
	}
	if err := controller.retireChangedContainers(ctx, &operation); err != nil {
		t.Fatal(err)
	}
	if err := controller.setPhase(&operation, phaseNetworks); err != nil {
		t.Fatal(err)
	}
	if err := controller.ensureTargetNetworks(ctx, &operation); err != nil {
		t.Fatal(err)
	}
	if err := controller.setPhase(&operation, phaseResync); err != nil {
		t.Fatal(err)
	}
	if phase == phaseResync {
		return initialEgressApplyFixture{controller, fake, target, operation}
	}
	if err := controller.ensureTargetProxy(ctx, &operation); err != nil {
		t.Fatal(err)
	}
	if err := controller.setPhase(&operation, phaseCreate); err != nil {
		t.Fatal(err)
	}
	if phase == phaseCreate {
		return initialEgressApplyFixture{controller, fake, target, operation}
	}
	if err := controller.ensureTargetContainers(ctx, &operation); err != nil {
		t.Fatal(err)
	}
	if err := controller.setPhase(&operation, phaseAttach); err != nil {
		t.Fatal(err)
	}
	if phase == phaseAttach {
		return initialEgressApplyFixture{controller, fake, target, operation}
	}
	if err := controller.reconcileAttachments(ctx, &operation); err != nil {
		t.Fatal(err)
	}
	if err := controller.setPhase(&operation, phaseStart); err != nil {
		t.Fatal(err)
	}
	if phase == phaseStart {
		return initialEgressApplyFixture{controller, fake, target, operation}
	}
	if err := controller.ensureTargetContainersRunning(ctx, &operation); err != nil {
		t.Fatal(err)
	}
	if err := controller.setPhase(&operation, phaseCommit); err != nil {
		t.Fatal(err)
	}
	if phase != phaseCommit {
		t.Fatalf("unsupported apply phase %q", phase)
	}
	return initialEgressApplyFixture{controller, fake, target, operation}
}

func assertDeploymentMatches(
	t *testing.T,
	controller *Controller,
	deployed state.Deployment,
	target composition.ResolvedSpec,
) {
	t.Helper()
	matches, err := controller.deploymentMatches(context.Background(), deployed, target)
	if err != nil {
		t.Fatal(err)
	}
	if !matches {
		t.Fatal("resumed deployment does not match its target")
	}
}

// A caller cancellation is not evidence that reverse resync cannot converge.
// The abort journal must leave that decision retryable; otherwise a transient
// CLI timeout is durably promoted into the full-replacement recovery path.
func TestAbortCancellationDoesNotCommitReplacementFallback(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installImages(
		fake,
		"provider:v1", "consumer:v1", "observer:v1",
		"sha256:provider", "sha256:consumer", "sha256:observer",
	)
	initial := linkedSpec("provider:v1", "consumer:v1")
	if err := controller.Up(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	previous := requireDesired(t, controller.State, initial.Name)

	fake.startFailures["observer"] = []error{errors.New("interrupt target start")}
	target := fanoutSpec("provider:v1", "consumer:v1", "observer:v1")
	if err := controller.Up(context.Background(), target); err == nil {
		t.Fatal("target apply unexpectedly succeeded")
	}
	interrupted := requireOperation(t, controller.State, target.Name)
	manager := controller.Proxy.(*fakeProxyManager)
	ctx, cancel := context.WithCancel(context.Background())
	manager.beforeResync = func(_ proxy.Wiring) error {
		cancel()
		return context.Canceled
	}
	manager.mu.Lock()
	stopAttemptsBefore := manager.stopAttempts
	manager.mu.Unlock()

	err := controller.Abort(ctx, target.Name)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Abort error = %v, want context cancellation", err)
	}
	aborting := requireOperation(t, controller.State, target.Name)
	if aborting.AbortRecreatePrevious {
		t.Error("canceled reverse resync durably selected replacement fallback")
	}
	if aborting.Proxy == nil || interrupted.Proxy == nil ||
		aborting.Proxy.InstanceID != interrupted.Proxy.InstanceID {
		t.Fatalf("canceled abort changed proxy identity: %#v", aborting.Proxy)
	}
	manager.mu.Lock()
	stopAttempts := manager.stopAttempts - stopAttemptsBefore
	manager.beforeResync = nil
	manager.mu.Unlock()
	if stopAttempts != 0 {
		t.Fatalf("canceled abort attempted %d proxy stops", stopAttempts)
	}

	fake.resetCalls()
	if err := controller.Resume(context.Background(), target.Name); err != nil {
		t.Fatal(err)
	}
	restored := requireDesired(t, controller.State, initial.Name)
	if restored.Proxy.InstanceID != previous.Proxy.InstanceID {
		t.Fatal("resuming a canceled reverse resync replaced the live proxy")
	}
	for _, name := range []string{"provider", "consumer"} {
		requireSameContainer(t, restored, name, previous.Containers[name].ID)
		assertContainerUntouched(t, fake, previous.Containers[name].ID)
	}
}

func TestAbortFallbackBeforeRetireAcceptsPreviousRuntimeFleet(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installImages(
		fake,
		"provider:v1", "consumer:v1",
		"sha256:provider", "sha256:consumer",
	)
	initial := linkedSpec("provider:v1", "consumer:v1")
	if err := controller.Up(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	previous := requireDesired(t, controller.State, initial.Name)
	target, err := controller.resolve(context.Background(), initial)
	if err != nil {
		t.Fatal(err)
	}
	newRuntimeRoot := t.TempDir()
	operation, err := state.NewOperation(
		kindApply,
		phaseRetire,
		target,
		&previous,
		newRuntimeRoot,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.selectRetainedResources(context.Background(), &operation); err != nil {
		t.Fatal(err)
	}
	if len(operation.Containers) != 0 || operation.Proxy != nil {
		t.Fatalf("runtime-root change unexpectedly retained resources: %#v", operation)
	}
	if err := controller.State.WriteOperation(target.Name, operation); err != nil {
		t.Fatal(err)
	}
	controller.RuntimeRoot = newRuntimeRoot

	if err := controller.Abort(context.Background(), target.Name); err != nil {
		t.Fatal(err)
	}
	restored := requireDesired(t, controller.State, target.Name)
	requireNoOperation(t, controller.State, target.Name)
	if restored.Spec.Digest != previous.Spec.Digest ||
		restored.RuntimeRoot != previous.RuntimeRoot {
		t.Fatalf(
			"restored deployment = digest %q, runtime root %q; want %q, %q",
			restored.Spec.Digest,
			restored.RuntimeRoot,
			previous.Spec.Digest,
			previous.RuntimeRoot,
		)
	}
}

func TestAbortFallbackBeforeRetireAcceptsChangedPreviousContainer(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installImages(
		fake,
		"provider:v1", "provider:v2", "consumer:v1",
		"sha256:provider-v1", "sha256:provider-v2", "sha256:consumer",
	)
	initial := linkedSpec("provider:v1", "consumer:v1")
	if err := controller.Up(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	previous := requireDesired(t, controller.State, initial.Name)
	target, err := controller.resolve(
		context.Background(),
		linkedSpec("provider:v2", "consumer:v1"),
	)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := state.NewOperation(
		kindApply,
		phaseRetire,
		target,
		&previous,
		controller.RuntimeRoot,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.selectRetainedResources(context.Background(), &operation); err != nil {
		t.Fatal(err)
	}
	if _, retained := operation.Containers["provider"]; retained {
		t.Fatal("changed provider unexpectedly retained")
	}
	if err := controller.State.WriteOperation(target.Name, operation); err != nil {
		t.Fatal(err)
	}
	killFakeProxyOutOfBand(controller.Proxy.(*fakeProxyManager), previous.Proxy.InstanceID)

	if err := controller.Abort(context.Background(), target.Name); err != nil {
		t.Fatal(err)
	}
	restored := requireDesired(t, controller.State, target.Name)
	requireNoOperation(t, controller.State, target.Name)
	if restored.Spec.Digest != previous.Spec.Digest {
		t.Fatalf("restored digest = %q, want %q", restored.Spec.Digest, previous.Spec.Digest)
	}
}

func TestAbortFallbackRejectsForeignContainerAtPreviousName(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installImages(
		fake,
		"provider:v1", "consumer:v1",
		"sha256:provider", "sha256:consumer",
	)
	initial := linkedSpec("provider:v1", "consumer:v1")
	if err := controller.Up(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	previous := requireDesired(t, controller.State, initial.Name)
	target, err := controller.resolve(context.Background(), initial)
	if err != nil {
		t.Fatal(err)
	}
	newRuntimeRoot := t.TempDir()
	operation, err := state.NewOperation(
		kindApply,
		phaseRetire,
		target,
		&previous,
		newRuntimeRoot,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.selectRetainedResources(context.Background(), &operation); err != nil {
		t.Fatal(err)
	}
	if err := controller.State.WriteOperation(target.Name, operation); err != nil {
		t.Fatal(err)
	}
	controller.RuntimeRoot = newRuntimeRoot

	provider := previous.Containers["provider"]
	fake.deleteContainerOutOfBand(provider.ID)
	foreign, err := fake.CreateContainer(context.Background(), engine.ContainerRequest{
		Name:    provider.Name,
		ImageID: "sha256:foreign",
	})
	if err != nil {
		t.Fatal(err)
	}
	fake.resetCalls()

	err = controller.Abort(context.Background(), target.Name)
	if err == nil || !strings.Contains(err.Error(), "no longer identifies recorded container") {
		t.Fatalf("Abort error = %v, want recorded-container identity refusal", err)
	}
	if _, exists := fake.containers[foreign.ID]; !exists {
		t.Fatal("abort removed foreign container")
	}
	if got := fake.mutationCalls(); len(got) != 0 {
		t.Fatalf("identity refusal performed mutations: %#v", got)
	}
}

func TestAbortFallbackRecreatesPreviousOnlySocketMountedContainer(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installImages(
		fake,
		"provider:v1", "consumer:v1", "observer:v1",
		"sha256:provider", "sha256:consumer", "sha256:observer",
	)
	initial := fanoutSpec("provider:v1", "consumer:v1", "observer:v1")
	if err := controller.Up(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	previous := requireDesired(t, controller.State, initial.Name)
	target, err := controller.resolve(
		context.Background(),
		linkedSpec("provider:v1", "consumer:v1"),
	)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := state.NewOperation(
		kindApply,
		phaseRetire,
		target,
		&previous,
		controller.RuntimeRoot,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.selectRetainedResources(context.Background(), &operation); err != nil {
		t.Fatal(err)
	}
	if _, retained := operation.Containers["observer"]; retained {
		t.Fatal("target unexpectedly retained removed observer")
	}
	if err := controller.State.WriteOperation(target.Name, operation); err != nil {
		t.Fatal(err)
	}
	killFakeProxyOutOfBand(controller.Proxy.(*fakeProxyManager), previous.Proxy.InstanceID)

	if err := controller.Abort(context.Background(), target.Name); err != nil {
		t.Fatal(err)
	}
	restored := requireDesired(t, controller.State, target.Name)
	requireNoOperation(t, controller.State, target.Name)
	if got := restored.Containers["observer"].ID; got == previous.Containers["observer"].ID {
		t.Fatalf("previous-only observer retained stale socket mounts in %s", got)
	}
}

func TestAbortFallbackUsesPreviousMissingEgressNetworkRecord(t *testing.T) {
	tests := []struct {
		name              string
		consumerImage     string
		persistedFallback bool
	}{
		{name: "retained container"},
		{name: "recreated container", consumerImage: "consumer:v2"},
		{name: "persisted fallback", persistedFallback: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			controller, fake := newControllerHarness(t)
			installImages(
				fake,
				"provider:v1", "consumer:v1", "consumer:v2", "observer:v1",
				"sha256:provider", "sha256:consumer-v1", "sha256:consumer-v2", "sha256:observer",
			)
			initial := linkedSpec("provider:v1", "consumer:v1")
			initial.Components[1].Runtime.ExternalEgress = true
			if err := controller.Up(context.Background(), initial); err != nil {
				t.Fatal(err)
			}
			previous := requireDesired(t, controller.State, initial.Name)
			fake.deleteNetworkOutOfBand(
				previous.Networks[componentNetworkKey("consumer")].ID,
			)

			consumerImage := test.consumerImage
			if consumerImage == "" {
				consumerImage = "consumer:v1"
			}
			targetSpec := fanoutSpec("provider:v1", consumerImage, "observer:v1")
			targetSpec.Components[1].Runtime.ExternalEgress = true
			target, err := controller.resolve(context.Background(), targetSpec)
			if err != nil {
				t.Fatal(err)
			}
			operation, err := state.NewOperation(
				kindApply,
				phaseRetire,
				target,
				&previous,
				controller.RuntimeRoot,
			)
			if err != nil {
				t.Fatal(err)
			}
			if err := controller.selectRetainedResources(
				context.Background(),
				&operation,
			); err != nil {
				t.Fatal(err)
			}
			if _, retained := operation.Networks[componentNetworkKey("consumer")]; retained {
				t.Fatal("missing egress network was unexpectedly retained")
			}
			killFakeProxyOutOfBand(
				controller.Proxy.(*fakeProxyManager),
				previous.Proxy.InstanceID,
			)
			if test.persistedFallback {
				operation.Phase = phaseAbort
				operation.AbortRecreatePrevious = true
				operation.Proxy = nil
			}
			if err := controller.State.WriteOperation(target.Name, operation); err != nil {
				t.Fatal(err)
			}

			if test.persistedFallback {
				err = controller.Resume(context.Background(), target.Name)
			} else {
				err = controller.Abort(context.Background(), target.Name)
			}
			if err != nil {
				t.Fatal(err)
			}
			restored := requireDesired(t, controller.State, target.Name)
			requireNoOperation(t, controller.State, target.Name)
			if restored.Spec.Digest != previous.Spec.Digest {
				t.Fatalf(
					"restored digest = %q, want %q",
					restored.Spec.Digest,
					previous.Spec.Digest,
				)
			}
		})
	}
}

func TestResumeAfterProxyLossAcceptsUnstartedContainerOnRecreatedNetwork(t *testing.T) {
	for _, phase := range []string{phaseCreate, phaseAttach, phaseStart} {
		t.Run(phase, func(t *testing.T) {
			fixture := prepareUnstartedSameNameNetworkReplacement(t, phase)
			killFakeProxyOutOfBand(fixture.manager, fixture.proxyID)

			if err := fixture.controller.Resume(context.Background(), fixture.target.Name); err != nil {
				t.Fatal(err)
			}
			assertSameNameNetworkFallbackConverged(t, fixture, fixture.target)
			deployed := requireDesired(t, fixture.controller.State, fixture.target.Name)
			if got := deployed.Networks[componentNetworkKey("consumer")].ID; got != fixture.targetNetwork.ID {
				t.Fatalf("resumed network ID = %q, want retained target network %q", got, fixture.targetNetwork.ID)
			}
		})
	}
}

func TestAbortAfterProxyLossAcceptsUnstartedContainerOnRecreatedNetwork(t *testing.T) {
	for _, phase := range []string{phaseCreate, phaseAttach, phaseStart} {
		t.Run(phase, func(t *testing.T) {
			fixture := prepareUnstartedSameNameNetworkReplacement(t, phase)
			killFakeProxyOutOfBand(fixture.manager, fixture.proxyID)

			if err := fixture.controller.Abort(context.Background(), fixture.target.Name); err != nil {
				t.Fatal(err)
			}
			assertSameNameNetworkFallbackConverged(t, fixture, fixture.previous.Spec)
		})
	}
}

func TestProxyLossFallbackRejectsForeignNetworkBeforeMutation(t *testing.T) {
	fixture := prepareUnstartedSameNameNetworkReplacement(t, phaseAttach)
	fixture.fake.mu.Lock()
	consumer := fixture.fake.containers[fixture.createdConsumer.ID]
	consumer.Networks["foreign"] = engine.NetworkAttachment{
		NetworkID: "foreign-network",
		Aliases:   []string{"consumer"},
	}
	fixture.fake.containers[fixture.createdConsumer.ID] = consumer
	fixture.fake.mu.Unlock()
	fixture.fake.resetCalls()
	killFakeProxyOutOfBand(fixture.manager, fixture.proxyID)

	err := fixture.controller.Resume(context.Background(), fixture.target.Name)
	if err == nil || !strings.Contains(err.Error(), "undeclared network") {
		t.Fatalf("Resume error = %v, want undeclared-network refusal", err)
	}
	if mutations := fixture.fake.mutationCalls(); len(mutations) != 0 {
		t.Fatalf("foreign attachment caused fallback mutations: %#v", mutations)
	}
	requireOperation(t, fixture.controller.State, fixture.target.Name)
}

type sameNameNetworkFallbackFixture struct {
	controller      *Controller
	fake            *fakeEngine
	manager         *fakeProxyManager
	previous        state.Deployment
	target          composition.ResolvedSpec
	createdConsumer state.Resource
	targetNetwork   state.Resource
	proxyID         string
}

func prepareUnstartedSameNameNetworkReplacement(
	t *testing.T,
	phase string,
) sameNameNetworkFallbackFixture {
	t.Helper()
	ctx := context.Background()
	controller, fake := newControllerHarness(t)
	installImages(
		fake,
		"provider:v1", "consumer:v1", "consumer:v2",
		"sha256:provider", "sha256:consumer-v1", "sha256:consumer-v2",
	)
	initial := linkedSpec("provider:v1", "consumer:v1")
	initial.Components[1].Runtime.ExternalEgress = true
	if err := controller.Up(ctx, initial); err != nil {
		t.Fatal(err)
	}
	previous := requireDesired(t, controller.State, initial.Name)
	previousNetwork := previous.Networks[componentNetworkKey("consumer")]
	fake.deleteNetworkOutOfBand(previousNetwork.ID)

	targetSpec := linkedSpec("provider:v1", "consumer:v2")
	targetSpec.Components[1].Runtime.ExternalEgress = true
	target, err := controller.resolve(ctx, targetSpec)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := state.NewOperation(
		kindApply,
		phaseRetire,
		target,
		&previous,
		controller.RuntimeRoot,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.selectRetainedResources(ctx, &operation); err != nil {
		t.Fatal(err)
	}
	if err := controller.State.WriteOperation(target.Name, operation); err != nil {
		t.Fatal(err)
	}
	if err := controller.retireChangedContainers(ctx, &operation); err != nil {
		t.Fatal(err)
	}
	if err := controller.setPhase(&operation, phaseNetworks); err != nil {
		t.Fatal(err)
	}
	if err := controller.ensureTargetNetworks(ctx, &operation); err != nil {
		t.Fatal(err)
	}
	targetNetwork := operation.Networks[componentNetworkKey("consumer")]
	if targetNetwork.Name != previousNetwork.Name || targetNetwork.ID == previousNetwork.ID {
		t.Fatalf(
			"replacement network = %#v, previous = %#v; want same name and a new ID",
			targetNetwork,
			previousNetwork,
		)
	}
	if err := controller.setPhase(&operation, phaseResync); err != nil {
		t.Fatal(err)
	}
	if err := controller.ensureTargetProxy(ctx, &operation); err != nil {
		t.Fatal(err)
	}
	if err := controller.setPhase(&operation, phaseCreate); err != nil {
		t.Fatal(err)
	}
	if err := controller.ensureTargetContainers(ctx, &operation); err != nil {
		t.Fatal(err)
	}
	switch phase {
	case phaseCreate:
	case phaseAttach:
		if err := controller.setPhase(&operation, phaseAttach); err != nil {
			t.Fatal(err)
		}
	case phaseStart:
		if err := controller.setPhase(&operation, phaseAttach); err != nil {
			t.Fatal(err)
		}
		if err := controller.reconcileAttachments(ctx, &operation); err != nil {
			t.Fatal(err)
		}
		if err := controller.setPhase(&operation, phaseStart); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unsupported test phase %q", phase)
	}

	createdConsumer := operation.Containers["consumer"]
	actual, err := fake.InspectContainer(ctx, createdConsumer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if actual.Running {
		t.Fatal("replacement consumer unexpectedly started before proxy loss")
	}
	attachment, exists := actual.Networks[targetNetwork.Name]
	if !exists || attachment.NetworkID != "" {
		t.Fatalf(
			"pre-start network attachment = %#v, present=%t; want an empty Docker network ID",
			attachment,
			exists,
		)
	}
	if operation.Proxy == nil {
		t.Fatal("prepared operation has no proxy")
	}
	return sameNameNetworkFallbackFixture{
		controller:      controller,
		fake:            fake,
		manager:         controller.Proxy.(*fakeProxyManager),
		previous:        previous,
		target:          target,
		createdConsumer: createdConsumer,
		targetNetwork:   targetNetwork,
		proxyID:         operation.Proxy.InstanceID,
	}
}

func assertSameNameNetworkFallbackConverged(
	t *testing.T,
	fixture sameNameNetworkFallbackFixture,
	want composition.ResolvedSpec,
) {
	t.Helper()
	ctx := context.Background()
	deployed := requireDesired(t, fixture.controller.State, fixture.target.Name)
	requireNoOperation(t, fixture.controller.State, fixture.target.Name)
	if deployed.Spec.Digest != want.Digest {
		t.Fatalf("deployed digest = %q, want %q", deployed.Spec.Digest, want.Digest)
	}
	if deployed.Proxy.InstanceID == fixture.proxyID {
		t.Fatalf("fallback retained missing proxy identity %q", fixture.proxyID)
	}
	if got := deployed.Containers["consumer"].ID; got == fixture.createdConsumer.ID {
		t.Fatalf("fallback retained stale-mount consumer %q", got)
	}
	if _, err := fixture.fake.InspectContainer(ctx, fixture.createdConsumer.ID); !errors.Is(err, engine.ErrNotFound) {
		t.Fatalf("stale-mount consumer inspection error = %v, want not found", err)
	}
	matches, err := fixture.controller.deploymentMatches(ctx, deployed, want)
	if err != nil {
		t.Fatal(err)
	}
	if !matches {
		t.Fatal("fallback result does not match its desired deployment")
	}
}

func TestAbortFallbackResumeCompletesTheDurableReplacementDecision(t *testing.T) {
	for _, stopLands := range []bool{false, true} {
		name := "shutdown failed before taking effect"
		if stopLands {
			name = "shutdown landed but response was lost"
		}
		t.Run(name, func(t *testing.T) {
			fixture := prepareSameRuntimeAbortFallback(t, stopLands)
			targetProxyID := fixture.aborting.Proxy.InstanceID
			fixture.manager.mu.Lock()
			resyncAttemptsBefore := fixture.manager.resyncAttempts
			fixture.manager.mu.Unlock()
			fixture.fake.resetCalls()

			if err := fixture.controller.Resume(context.Background(), fixture.target.Name); err != nil {
				t.Fatal(err)
			}
			restored := requireDesired(t, fixture.controller.State, fixture.target.Name)
			requireNoOperation(t, fixture.controller.State, fixture.target.Name)
			if restored.Spec.Digest != fixture.previous.Spec.Digest {
				t.Fatalf("restored digest = %q, want %q", restored.Spec.Digest, fixture.previous.Spec.Digest)
			}
			if restored.Proxy.InstanceID == targetProxyID {
				t.Fatal("durable full-replacement fallback retained the target proxy")
			}
			for _, component := range []string{"provider", "consumer"} {
				if restored.Containers[component].ID == fixture.previous.Containers[component].ID {
					t.Fatalf("durable full-replacement fallback retained %s", component)
				}
			}
			fixture.manager.mu.Lock()
			_, targetStillLive := fixture.manager.processes[targetProxyID]
			liveProcesses := len(fixture.manager.processes)
			resyncAttempts := fixture.manager.resyncAttempts - resyncAttemptsBefore
			fixture.manager.mu.Unlock()
			if targetStillLive || liveProcesses != 1 {
				t.Fatalf(
					"target proxy live=%t, live proxy count=%d, want false/1",
					targetStillLive,
					liveProcesses,
				)
			}
			if resyncAttempts != 0 {
				t.Fatalf("resume reconsidered reverse resync %d times after fallback was durable", resyncAttempts)
			}
		})
	}
}

func TestAbortFallbackResumesEachDurableCleanupCheckpoint(t *testing.T) {
	for _, stopLands := range []bool{false, true} {
		name := "proxy still running"
		if stopLands {
			name = "proxy already absent after lost response"
		}
		t.Run(name, func(t *testing.T) {
			fixture := prepareSameRuntimeAbortFallback(t, stopLands)
			operation := fixture.aborting
			targetProxyID := operation.Proxy.InstanceID

			if err := fixture.controller.restorePreviousProxyDuringAbort(
				context.Background(),
				&operation,
			); err != nil {
				t.Fatal(err)
			}
			assertAbortFallbackCleanupComplete(t, fixture, targetProxyID)
		})
	}

	t.Run("proxy checkpoint durable with partial container cleanup", func(t *testing.T) {
		fixture := prepareSameRuntimeAbortFallback(t, true)
		operation := fixture.aborting
		operation.Proxy = nil
		provider := operation.Containers["provider"]
		fixture.fake.deleteContainerOutOfBand(provider.ID)
		delete(operation.Containers, "provider")
		if err := fixture.controller.State.WriteOperation(operation.Target.Name, operation); err != nil {
			t.Fatal(err)
		}

		if err := fixture.controller.restorePreviousProxyDuringAbort(
			context.Background(),
			&operation,
		); err != nil {
			t.Fatal(err)
		}
		durable := requireOperation(t, fixture.controller.State, operation.Target.Name)
		if durable.Proxy != nil || len(durable.Containers) != 0 {
			t.Fatalf("partial cleanup did not converge: proxy=%#v containers=%#v", durable.Proxy, durable.Containers)
		}
		for _, component := range []string{"consumer", "observer"} {
			resource := fixture.aborting.Containers[component]
			if _, exists := fixture.fake.containers[resource.ID]; exists {
				t.Fatalf("%s container %s survived resumed stale-mount cleanup", component, resource.ID)
			}
		}
	})
}

func TestAbortFallbackStillRejectsAProxyIdentityMismatch(t *testing.T) {
	fixture := prepareSameRuntimeAbortFallback(t, false)
	operation := fixture.aborting
	targetProxyID := operation.Proxy.InstanceID
	fixture.manager.mu.Lock()
	actual := fixture.manager.processes[targetProxyID]
	config := fixture.manager.configs[targetProxyID]
	ready := fixture.manager.ready[targetProxyID]
	controlVersion := fixture.manager.controlVersions[targetProxyID]
	delete(fixture.manager.processes, targetProxyID)
	delete(fixture.manager.configs, targetProxyID)
	delete(fixture.manager.ready, targetProxyID)
	delete(fixture.manager.controlVersions, targetProxyID)
	actual.InstanceID = "foreign-proxy"
	actual.PID++
	fixture.manager.processes[actual.InstanceID] = actual
	fixture.manager.configs[actual.InstanceID] = config
	fixture.manager.ready[actual.InstanceID] = ready
	fixture.manager.controlVersions[actual.InstanceID] = controlVersion
	fixture.manager.mu.Unlock()

	err := fixture.controller.restorePreviousProxyDuringAbort(context.Background(), &operation)
	if !errors.Is(err, proxy.ErrIdentityMismatch) {
		t.Fatalf("fallback identity mismatch error = %v, want ErrIdentityMismatch", err)
	}
	durable := requireOperation(t, fixture.controller.State, operation.Target.Name)
	if durable.Proxy == nil || durable.Proxy.InstanceID != targetProxyID {
		t.Fatalf("identity mismatch changed recorded proxy: %#v", durable.Proxy)
	}
	for _, component := range []string{"provider", "consumer"} {
		resource := fixture.previous.Containers[component]
		if _, exists := fixture.fake.containers[resource.ID]; !exists {
			t.Fatalf("identity mismatch removed %s container %s", component, resource.ID)
		}
	}
}

func TestAbortFallbackResumeDoesNotLeakChangedRuntimeProxy(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installImages(
		fake,
		"provider:v1", "consumer:v1",
		"sha256:provider", "sha256:consumer",
	)
	spec := linkedSpec("provider:v1", "consumer:v1")
	if err := controller.Up(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	previous := requireDesired(t, controller.State, spec.Name)
	controller.RuntimeRoot = t.TempDir()
	fake.startFailures["consumer"] = []error{errors.New("interrupt replacement start")}
	if err := controller.Up(context.Background(), spec); err == nil ||
		!strings.Contains(err.Error(), "interrupt replacement start") {
		t.Fatalf("runtime-root replacement error = %v", err)
	}
	interrupted := requireOperation(t, controller.State, spec.Name)
	if interrupted.Proxy == nil || interrupted.Proxy.InstanceID == previous.Proxy.InstanceID {
		t.Fatalf("runtime-root replacement proxy = %#v", interrupted.Proxy)
	}
	targetProxy := *interrupted.Proxy
	manager := controller.Proxy.(*fakeProxyManager)
	manager.mu.Lock()
	manager.stopErrors = []error{errors.New("transient target proxy shutdown failure")}
	manager.mu.Unlock()

	err := controller.Abort(context.Background(), spec.Name)
	if err == nil || !strings.Contains(err.Error(), "transient target proxy shutdown failure") {
		t.Fatalf("Abort error = %v, want target proxy shutdown failure", err)
	}
	aborting := requireOperation(t, controller.State, spec.Name)
	if !aborting.AbortRecreatePrevious || aborting.Proxy == nil {
		t.Fatalf("abort fallback was not durably pending: %#v", aborting)
	}
	if err := controller.Resume(context.Background(), spec.Name); err != nil {
		t.Fatal(err)
	}
	restored := requireDesired(t, controller.State, spec.Name)
	requireNoOperation(t, controller.State, spec.Name)
	if restored.RuntimeRoot != previous.RuntimeRoot {
		t.Fatalf("restored runtime root = %q, want %q", restored.RuntimeRoot, previous.RuntimeRoot)
	}
	manager.mu.Lock()
	_, targetStillLive := manager.processes[targetProxy.InstanceID]
	liveProcesses := len(manager.processes)
	for _, process := range manager.processes {
		if process.RuntimeDir == targetProxy.RuntimeDir {
			targetStillLive = true
		}
	}
	manager.mu.Unlock()
	if targetStillLive || liveProcesses != 1 {
		t.Fatalf(
			"changed-runtime target proxy live=%t, live proxy count=%d, want false/1",
			targetStillLive,
			liveProcesses,
		)
	}
}

type abortFallbackFixture struct {
	controller *Controller
	fake       *fakeEngine
	manager    *fakeProxyManager
	previous   state.Deployment
	target     composition.Spec
	aborting   state.Operation
}

func prepareSameRuntimeAbortFallback(t *testing.T, stopLands bool) abortFallbackFixture {
	t.Helper()
	controller, fake := newControllerHarness(t)
	installImages(
		fake,
		"provider:v1", "consumer:v1", "observer:v1",
		"sha256:provider", "sha256:consumer", "sha256:observer",
	)
	initial := linkedSpec("provider:v1", "consumer:v1")
	if err := controller.Up(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	previous := requireDesired(t, controller.State, initial.Name)
	target := fanoutSpec("provider:v1", "consumer:v1", "observer:v1")
	fake.startFailures["observer"] = []error{errors.New("interrupt target start")}
	if err := controller.Up(context.Background(), target); err == nil ||
		!strings.Contains(err.Error(), "interrupt target start") {
		t.Fatalf("target apply error = %v", err)
	}
	manager := controller.Proxy.(*fakeProxyManager)
	manager.mu.Lock()
	manager.beforeResync = func(proxy.Wiring) error {
		return errors.New("forced reverse resync failure")
	}
	stopError := errors.New("lost target proxy shutdown result")
	if stopLands {
		manager.stopAfterErrors = []error{stopError}
	} else {
		manager.stopErrors = []error{stopError}
	}
	manager.mu.Unlock()

	err := controller.Abort(context.Background(), target.Name)
	if err == nil || !strings.Contains(err.Error(), stopError.Error()) {
		t.Fatalf("Abort error = %v, want %q", err, stopError)
	}
	aborting := requireOperation(t, controller.State, target.Name)
	if aborting.Phase != phaseAbort || !aborting.AbortRecreatePrevious || aborting.Proxy == nil {
		t.Fatalf("durable abort fallback = %#v", aborting)
	}
	manager.mu.Lock()
	manager.beforeResync = nil
	manager.mu.Unlock()
	return abortFallbackFixture{
		controller: controller,
		fake:       fake,
		manager:    manager,
		previous:   previous,
		target:     target,
		aborting:   aborting,
	}
}

func assertAbortFallbackCleanupComplete(
	t *testing.T,
	fixture abortFallbackFixture,
	targetProxyID string,
) {
	t.Helper()
	durable := requireOperation(t, fixture.controller.State, fixture.target.Name)
	if durable.Proxy != nil || len(durable.Containers) != 0 {
		t.Fatalf("fallback cleanup state: proxy=%#v containers=%#v", durable.Proxy, durable.Containers)
	}
	for component, resource := range fixture.aborting.Containers {
		if _, exists := fixture.fake.containers[resource.ID]; exists {
			t.Fatalf("%s container %s survived stale-mount cleanup", component, resource.ID)
		}
	}
	fixture.manager.mu.Lock()
	_, targetStillLive := fixture.manager.processes[targetProxyID]
	fixture.manager.mu.Unlock()
	if targetStillLive {
		t.Fatalf("target proxy %s survived fallback cleanup", targetProxyID)
	}
}

func killFakeProxyOutOfBand(manager *fakeProxyManager, instanceID string) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	delete(manager.processes, instanceID)
	delete(manager.configs, instanceID)
	delete(manager.ready, instanceID)
	delete(manager.controlVersions, instanceID)
}

func relinkSpec(provider string) composition.Spec {
	return composition.Spec{
		Name: "relink",
		Components: []composition.Instance{
			component("provider-a", "provider-a:v1", nil, []string{"echo"}),
			component("provider-b", "provider-b:v1", nil, []string{"echo"}),
			component("consumer", "consumer:v1", []string{"upstream"}, nil),
		},
		Links: []composition.Link{
			link("consumer", "upstream", provider, "echo"),
		},
	}
}
