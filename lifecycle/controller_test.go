package lifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/glguida/dcomp/composition"
	"github.com/glguida/dcomp/engine"
	"github.com/glguida/dcomp/proxy"
	"github.com/glguida/dcomp/state"
)

const echoService = "example.echo.v1.Echo"

func TestUpUsesNoneWithoutEgressAndPassesRuntimeResources(t *testing.T) {
	controller, fake := newControllerHarness(t)
	source := filepath.Join(t.TempDir(), "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	fake.images["provider:v1"] = healthyImage("sha256:provider-v1")
	fake.images["consumer:v1"] = engine.Image{
		ID:              "sha256:consumer-v1",
		HasHealthcheck:  true,
		DeclaredVolumes: []string{"/var/lib/consumer"},
	}
	spec := linkedSpec("provider:v1", "consumer:v1")
	spec.Components[1].Runtime = composition.Runtime{
		Binds: []composition.BindMount{{
			Source: source, Target: "/work/source", ReadOnly: true,
		}},
		Volumes: []composition.VolumeMount{{
			Name: "data", Target: "/var/lib/consumer",
		}},
		Args: []string{"run", "--mode=test"},
		Ports: []composition.PublishedPort{{
			Protocol: "tcp", HostIP: "127.0.0.1",
			HostPort: 15051, ContainerPort: 8080,
		}},
		ExternalEgress: true,
	}

	if err := controller.Up(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	physicalVolume := volumeName(controller.dockerScope(spec.Name), "consumer", "data")

	deployment := requireDesired(t, controller.State, spec.Name)
	requireNoOperation(t, controller.State, spec.Name)
	if got, want := len(deployment.Networks), 1; got != want {
		t.Fatalf("network count = %d, want %d", got, want)
	}

	if _, exists := deployment.Networks["component/provider"]; exists {
		t.Fatal("provider without egress was given a network")
	}
	consumerBase := requireNetwork(t, fake, deployment.Networks["component/consumer"].ID)
	if consumerBase.Internal {
		t.Fatal("consumer egress network is internal")
	}
	requireMembers(t, consumerBase, deployment.Containers["consumer"].ID)

	requests := requestsByComponent(fake.containerRequests)
	provider := requests["provider"]
	if provider.NetworkID != "" || len(provider.NetworkAliases) != 0 {
		t.Fatalf("provider network request = %#v, want network mode none", provider)
	}
	if networks := fake.containers[deployment.Containers["provider"].ID].Networks; len(networks) != 0 {
		t.Fatalf("provider networks = %#v, want none", networks)
	}
	consumer := requests["consumer"]
	if consumer.NetworkID != consumerBase.ID {
		t.Fatalf("consumer primary network = %q, want %q", consumer.NetworkID, consumerBase.ID)
	}
	if got, want := consumer.Environment["DCOMP_IN_UPSTREAM"], "unix:///run/dcomp/in/upstream"; got != want {
		t.Fatalf("upstream = %q, want %q", got, want)
	}
	wantMounts := []engine.Mount{
		{
			Type:     engine.MountBind,
			Source:   filepath.Join(deployment.Proxy.RuntimeDir, "in", "consumer.upstream"),
			Target:   "/run/dcomp/in/upstream",
			ReadOnly: true,
		},
		{
			Type: engine.MountVolume, Source: physicalVolume,
			Target: "/var/lib/consumer",
		},
		{
			Type: engine.MountBind, Source: source,
			Target: "/work/source", ReadOnly: true,
		},
	}
	sort.Slice(wantMounts, func(i, j int) bool { return wantMounts[i].Target < wantMounts[j].Target })
	if !reflect.DeepEqual(consumer.Mounts, wantMounts) {
		t.Fatalf("consumer mounts:\n got: %#v\nwant: %#v", consumer.Mounts, wantMounts)
	}
	if !reflect.DeepEqual(consumer.Args, []string{"run", "--mode=test"}) {
		t.Fatalf("consumer args = %#v", consumer.Args)
	}
	if !reflect.DeepEqual(consumer.Security, componentSecurity()) {
		t.Fatalf(
			"consumer security = %#v, want %#v",
			consumer.Security,
			componentSecurity(),
		)
	}
	wantPorts := []engine.PortBinding{{
		ContainerPort: 8080,
		Protocol:      engine.ProtocolTCP,
		HostIP:        "127.0.0.1",
		HostPort:      15051,
	}}
	if !reflect.DeepEqual(consumer.PortBindings, wantPorts) {
		t.Fatalf("consumer ports = %#v, want %#v", consumer.PortBindings, wantPorts)
	}
	volume, exists := fake.volumes[physicalVolume]
	if !exists {
		t.Fatal("declared persistent volume was not created")
	}
	if volume.Labels[LabelComponent] != "consumer" ||
		volume.Labels[LabelVolumeLogical] != "data" {
		t.Fatalf("unexpected volume labels: %#v", volume.Labels)
	}

	mutations := fake.mutationCalls()
	lastCreate := lastCallIndex(mutations, "create-container")
	firstStart := callIndex(mutations, "start-container", "")
	if lastCreate < 0 || firstStart < 0 || lastCreate >= firstStart {
		t.Fatalf("not all containers were created before start: %#v", mutations)
	}
}

func TestIncrementalApplyPreservesUnchangedContainers(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installImages(
		fake,
		"provider:v1", "consumer:v1", "observer:v1", "observer:v2",
		"sha256:provider-v1", "sha256:consumer-v1",
		"sha256:observer-v1", "sha256:observer-v2",
	)
	initial := linkedSpec("provider:v1", "consumer:v1")
	if err := controller.Up(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	first := requireDesired(t, controller.State, initial.Name)
	providerID := first.Containers["provider"].ID
	consumerID := first.Containers["consumer"].ID
	proxyID := first.Proxy.InstanceID
	manager := controller.Proxy.(*fakeProxyManager)

	withObserver := fanoutSpec("provider:v1", "consumer:v1", "observer:v1")
	fake.resetCalls()
	if err := controller.Up(context.Background(), withObserver); err != nil {
		t.Fatal(err)
	}
	second := requireDesired(t, controller.State, withObserver.Name)
	requireSameContainer(t, second, "provider", providerID)
	requireSameContainer(t, second, "consumer", consumerID)
	if second.Proxy.InstanceID != proxyID {
		t.Fatal("adding a component replaced the proxy")
	}
	assertContainerUntouched(t, fake, providerID)
	assertContainerUntouched(t, fake, consumerID)
	observerV1 := second.Containers["observer"].ID
	manager.mu.Lock()
	resyncsBeforeImageChange := manager.resyncs
	manager.mu.Unlock()

	changedObserver := fanoutSpec("provider:v1", "consumer:v1", "observer:v2")
	fake.resetCalls()
	if err := controller.Up(context.Background(), changedObserver); err != nil {
		t.Fatal(err)
	}
	third := requireDesired(t, controller.State, changedObserver.Name)
	requireSameContainer(t, third, "provider", providerID)
	requireSameContainer(t, third, "consumer", consumerID)
	if third.Containers["observer"].ID == observerV1 {
		t.Fatal("changed observer image retained the old container")
	}
	assertContainerUntouched(t, fake, providerID)
	assertContainerUntouched(t, fake, consumerID)
	manager.mu.Lock()
	resyncsAfterImageChange := manager.resyncs
	manager.mu.Unlock()
	if resyncsAfterImageChange != resyncsBeforeImageChange {
		t.Fatal("image-only change resynced unchanged wiring")
	}

	fake.resetCalls()
	if err := controller.Up(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	final := requireDesired(t, controller.State, initial.Name)
	requireSameContainer(t, final, "provider", providerID)
	requireSameContainer(t, final, "consumer", consumerID)
	if final.Proxy.InstanceID != proxyID {
		t.Fatal("removing a component replaced the proxy")
	}
	if _, exists := final.Containers["observer"]; exists {
		t.Fatal("removed observer remains in desired state")
	}
}

func TestUpRecreatesSocketMountedContainersWhenProxyIsMissing(t *testing.T) {
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
	manager := controller.Proxy.(*fakeProxyManager)
	manager.mu.Lock()
	delete(manager.processes, before.Proxy.InstanceID)
	delete(manager.configs, before.Proxy.InstanceID)
	manager.mu.Unlock()

	if err := controller.Up(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	after := requireDesired(t, controller.State, spec.Name)
	if after.Proxy.InstanceID == before.Proxy.InstanceID {
		t.Fatal("missing proxy was not replaced")
	}
	for _, name := range []string{"provider", "consumer"} {
		if after.Containers[name].ID == before.Containers[name].ID {
			t.Fatalf("%s retained a bind mount to the missing proxy socket", name)
		}
	}
	for key, resource := range before.Networks {
		if after.Networks[key].ID != resource.ID {
			t.Fatalf("unaffected network %s was replaced", key)
		}
	}
}

func TestEndpointPublicationOccursAfterMountingContainerRetires(t *testing.T) {
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
	before := requireDesired(t, controller.State, initial.Name)
	newEndpointPath := proxy.HostSocket(
		before.Proxy.RuntimeDir, proxy.DirectionInput, "consumer", "secondary",
	)
	manager := controller.Proxy.(*fakeProxyManager)
	publicationChecks := 0
	var mountedAtPublication string
	manager.beforeResync = func(_ proxy.Wiring) error {
		publicationChecks++
		fake.mu.Lock()
		defer fake.mu.Unlock()
		for _, container := range fake.containers {
			for _, mount := range container.Mounts {
				if mount.Source == newEndpointPath {
					mountedAtPublication = container.ID
				}
			}
		}
		return nil
	}

	target := linkedSpec("provider:v1", "consumer:v1")
	target.Components[1].Component.Definition.Inputs = append(
		target.Components[1].Component.Definition.Inputs,
		composition.Endpoint{Name: "secondary", Service: echoService},
	)
	target.Links = append(
		target.Links,
		link("consumer", "secondary", "provider", "echo"),
	)
	if err := controller.Up(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	after := requireDesired(t, controller.State, target.Name)
	if publicationChecks != 1 {
		t.Fatalf("endpoint publication checks = %d, want 1", publicationChecks)
	}
	if mountedAtPublication != "" {
		t.Fatalf("container %s mounted new endpoint at publication", mountedAtPublication)
	}
	if after.Containers["consumer"].ID == before.Containers["consumer"].ID {
		t.Fatal("component with a changed endpoint set was not recreated")
	}
	requireSameContainer(t, after, "provider", before.Containers["provider"].ID)
	if after.Proxy.InstanceID != before.Proxy.InstanceID {
		t.Fatal("endpoint-set change replaced the proxy process")
	}
}

func TestResumeRecreatesSocketMountedContainersWhenProxyDiesDuringApply(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installImages(
		fake,
		"provider:v1", "consumer:v1",
		"sha256:provider", "sha256:consumer",
	)
	spec := linkedSpec("provider:v1", "consumer:v1")
	fake.startFailures["consumer"] = []error{errors.New("transient start failure")}

	if err := controller.Up(context.Background(), spec); err == nil {
		t.Fatal("initial Up unexpectedly succeeded")
	}
	interrupted := requireOperation(t, controller.State, spec.Name)
	if interrupted.Phase != phaseStart {
		t.Fatalf("operation phase = %q, want %q", interrupted.Phase, phaseStart)
	}
	oldProxyPID := interrupted.Proxy.PID
	oldContainerIDs := make(map[string]string, len(interrupted.Containers))
	for name, resource := range interrupted.Containers {
		oldContainerIDs[name] = resource.ID
	}
	manager := controller.Proxy.(*fakeProxyManager)
	manager.mu.Lock()
	delete(manager.processes, interrupted.Proxy.InstanceID)
	delete(manager.configs, interrupted.Proxy.InstanceID)
	manager.mu.Unlock()

	if err := controller.Resume(context.Background(), spec.Name); err != nil {
		t.Fatal(err)
	}
	deployed := requireDesired(t, controller.State, spec.Name)
	requireNoOperation(t, controller.State, spec.Name)
	if deployed.Proxy.PID == oldProxyPID {
		t.Fatal("dead proxy process was not replaced")
	}
	for name, oldID := range oldContainerIDs {
		if deployed.Containers[name].ID == oldID {
			t.Fatalf("%s retained a bind mount to the dead proxy socket", name)
		}
		if _, exists := fake.containers[oldID]; exists {
			t.Fatalf("stale %s container %s still exists", name, oldID)
		}
	}
}

func TestResumeDispatchesJournaledAndLandedResyncByObservedDigest(t *testing.T) {
	for _, landed := range []bool{false, true} {
		name := "journaled-before-call"
		if landed {
			name = "landed-before-create"
		}
		t.Run(name, func(t *testing.T) {
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
			target, err := controller.resolve(
				context.Background(),
				fanoutSpec("provider:v1", "consumer:v1", "observer:v1"),
			)
			if err != nil {
				t.Fatal(err)
			}
			operation, err := state.NewOperation(
				kindApply, phaseResync, target, &previous, controller.RuntimeRoot,
			)
			if err != nil {
				t.Fatal(err)
			}
			if err := controller.selectRetainedResources(
				context.Background(), &operation,
			); err != nil {
				t.Fatal(err)
			}
			if err := controller.State.WriteOperation(target.Name, operation); err != nil {
				t.Fatal(err)
			}
			manager := controller.Proxy.(*fakeProxyManager)
			manager.mu.Lock()
			resyncsBefore := manager.resyncs
			manager.mu.Unlock()
			if landed {
				if _, err := manager.Resync(
					context.Background(),
					*operation.Proxy,
					operation.TargetWiring,
					operation.TargetWiringDigest,
				); err != nil {
					t.Fatal(err)
				}
			}

			if err := controller.Resume(context.Background(), target.Name); err != nil {
				t.Fatal(err)
			}
			deployed := requireDesired(t, controller.State, target.Name)
			requireSameContainer(t, deployed, "provider", previous.Containers["provider"].ID)
			requireSameContainer(t, deployed, "consumer", previous.Containers["consumer"].ID)
			if deployed.Proxy.InstanceID != previous.Proxy.InstanceID {
				t.Fatal("resumed minimal apply replaced the proxy")
			}
			if deployed.Containers["observer"].ID == "" {
				t.Fatal("resumed apply did not create observer")
			}
			manager.mu.Lock()
			resyncsAfter := manager.resyncs
			manager.mu.Unlock()
			if got := resyncsAfter - resyncsBefore; got != 1 {
				t.Fatalf("resyncs after journal point = %d, want exactly 1", got)
			}
		})
	}
}

func TestResumeRetriesUnconvergedProxyThenFallsBackToFullReplacement(t *testing.T) {
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
	fake.startFailures["observer"] = []error{errors.New("interrupt after resync")}
	target := fanoutSpec("provider:v1", "consumer:v1", "observer:v1")
	if err := controller.Up(context.Background(), target); err == nil ||
		!strings.Contains(err.Error(), "interrupt after resync") {
		t.Fatalf("target Up error = %v", err)
	}
	interrupted := requireOperation(t, controller.State, target.Name)
	oldProxy := interrupted.Proxy.InstanceID
	oldContainers := cloneResources(interrupted.Containers)
	manager := controller.Proxy.(*fakeProxyManager)
	manager.mu.Lock()
	process := manager.processes[oldProxy]
	process.Digest = ""
	manager.processes[oldProxy] = process
	manager.ready[oldProxy] = false
	manager.resyncErrors = []error{
		errors.New("resync still unconverged"),
		errors.New("resync still unconverged"),
	}
	attemptsBefore := manager.resyncAttempts
	manager.mu.Unlock()

	if err := controller.Resume(context.Background(), target.Name); err != nil {
		t.Fatal(err)
	}
	deployed := requireDesired(t, controller.State, target.Name)
	if deployed.Proxy.InstanceID == oldProxy {
		t.Fatal("unconverged proxy was not replaced")
	}
	for name, old := range oldContainers {
		if deployed.Containers[name].ID == old.ID {
			t.Fatalf("%s retained a socket mount from the unconverged proxy", name)
		}
		if _, exists := fake.containers[old.ID]; exists {
			t.Fatalf("old %s container %s remains after fallback", name, old.ID)
		}
	}
	manager.mu.Lock()
	attempts := manager.resyncAttempts - attemptsBefore
	stops := manager.stops
	manager.mu.Unlock()
	if attempts != 2 || stops != 1 {
		t.Fatalf("fallback attempts/stops = %d/%d, want 2/1", attempts, stops)
	}
}

func TestIncompatibleProxyCapabilityIsTerminal(t *testing.T) {
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
	manager := controller.Proxy.(*fakeProxyManager)
	manager.mu.Lock()
	manager.controlVersions[previous.Proxy.InstanceID] = 0
	attemptsBefore, ensuresBefore, stopsBefore :=
		manager.resyncAttempts, manager.ensures, manager.stops
	manager.mu.Unlock()

	target := fanoutSpec("provider:v1", "consumer:v1", "observer:v1")
	err := controller.Up(context.Background(), target)
	if !errors.Is(err, proxy.ErrControlProtocolMismatch) {
		t.Fatalf("Up error = %v, want ErrControlProtocolMismatch", err)
	}
	if _, exists, readErr := controller.State.ReadOperation(target.Name); readErr != nil {
		t.Fatal(readErr)
	} else if exists {
		t.Fatal("protocol mismatch journaled an apply operation")
	}
	unchanged := requireDesired(t, controller.State, target.Name)
	if unchanged.Proxy == nil || unchanged.Proxy.InstanceID != previous.Proxy.InstanceID {
		t.Fatalf("incompatible proxy identity changed: %#v", unchanged.Proxy)
	}
	for _, name := range []string{"provider", "consumer"} {
		if unchanged.Containers[name].ID != previous.Containers[name].ID {
			t.Fatalf("%s changed before capability mismatch was reported", name)
		}
	}
	manager.mu.Lock()
	attempts := manager.resyncAttempts - attemptsBefore
	ensures := manager.ensures - ensuresBefore
	stops := manager.stops - stopsBefore
	manager.mu.Unlock()
	if attempts != 0 || ensures != 0 || stops != 0 {
		t.Fatalf("capability mismatch attempts/ensures/stops = %d/%d/%d, want 0/0/0", attempts, ensures, stops)
	}
}

func TestDownIncompatibleProxyCapabilityIsTerminalBeforeResourceMutation(t *testing.T) {
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
	manager := controller.Proxy.(*fakeProxyManager)
	manager.mu.Lock()
	manager.controlVersions[previous.Proxy.InstanceID] = 0
	stopsBefore := manager.stops
	manager.mu.Unlock()
	fake.resetCalls()

	err := controller.Down(context.Background(), spec.Name)
	if !errors.Is(err, proxy.ErrControlProtocolMismatch) {
		t.Fatalf("Down error = %v, want ErrControlProtocolMismatch", err)
	}
	operation := requireOperation(t, controller.State, spec.Name)
	if operation.Kind != kindDown || len(operation.Completed) != 0 ||
		operation.Proxy == nil || operation.Proxy.InstanceID != previous.Proxy.InstanceID {
		t.Fatalf("down operation after protocol mismatch = %#v", operation)
	}
	unchanged := requireDesired(t, controller.State, spec.Name)
	for _, name := range []string{"provider", "consumer"} {
		requireSameContainer(t, unchanged, name, previous.Containers[name].ID)
		assertContainerUntouched(t, fake, previous.Containers[name].ID)
	}
	manager.mu.Lock()
	stops := manager.stops - stopsBefore
	manager.mu.Unlock()
	if stops != 0 {
		t.Fatalf("protocol mismatch stopped %d proxies", stops)
	}
}

func TestProxyDeathDuringResyncFallsBackToFreshProxyAndMounts(t *testing.T) {
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
	manager := controller.Proxy.(*fakeProxyManager)
	manager.mu.Lock()
	manager.dieOnResync = true
	manager.mu.Unlock()

	target := fanoutSpec("provider:v1", "consumer:v1", "observer:v1")
	if err := controller.Up(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	deployed := requireDesired(t, controller.State, target.Name)
	if deployed.Proxy.InstanceID == previous.Proxy.InstanceID {
		t.Fatal("proxy that died during resync was not replaced")
	}
	for _, name := range []string{"provider", "consumer"} {
		if deployed.Containers[name].ID == previous.Containers[name].ID {
			t.Fatalf("%s retained a bind mount from the dead proxy", name)
		}
		if _, exists := fake.containers[previous.Containers[name].ID]; exists {
			t.Fatalf("stale %s container remains after dead-proxy fallback", name)
		}
	}
}

func TestMissingProxyControlRetainsIdentityUntilCleanupCompletes(t *testing.T) {
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
	manager := controller.Proxy.(*fakeProxyManager)
	manager.mu.Lock()
	manager.inspectErrors = []error{proxy.ErrNotRunning, proxy.ErrNotRunning}
	manager.stopErrors = []error{errors.New("proxy cleanup is still in progress")}
	ensuresBefore := manager.ensures
	manager.mu.Unlock()

	target := fanoutSpec("provider:v1", "consumer:v1", "observer:v1")
	err := controller.Up(context.Background(), target)
	if err == nil || !strings.Contains(err.Error(), "proxy cleanup is still in progress") {
		t.Fatalf("Up error = %v, want incomplete cleanup", err)
	}
	operation := requireOperation(t, controller.State, target.Name)
	if operation.Proxy == nil || operation.Proxy.InstanceID != previous.Proxy.InstanceID {
		t.Fatalf("recorded proxy identity was discarded: %#v", operation.Proxy)
	}
	manager.mu.Lock()
	ensureDelta := manager.ensures - ensuresBefore
	stopAttempts := manager.stopAttempts
	manager.mu.Unlock()
	if ensureDelta != 0 || stopAttempts != 1 {
		t.Fatalf("replacement ensures/cleanup attempts = %d/%d, want 0/1", ensureDelta, stopAttempts)
	}
}

func TestPostResyncProxyLossRetainsIdentityUntilCleanupCompletes(t *testing.T) {
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
	fake.startFailures["observer"] = []error{errors.New("interrupt target start")}
	target := fanoutSpec("provider:v1", "consumer:v1", "observer:v1")
	if err := controller.Up(context.Background(), target); err == nil ||
		!strings.Contains(err.Error(), "interrupt target start") {
		t.Fatalf("target Up error = %v", err)
	}
	interrupted := requireOperation(t, controller.State, target.Name)
	proxyID := interrupted.Proxy.InstanceID
	manager := controller.Proxy.(*fakeProxyManager)
	manager.mu.Lock()
	manager.inspectErrors = []error{proxy.ErrNotRunning}
	manager.stopErrors = []error{errors.New("proxy cleanup is still in progress")}
	ensuresBefore := manager.ensures
	stopAttemptsBefore := manager.stopAttempts
	manager.mu.Unlock()

	err := controller.Resume(context.Background(), target.Name)
	if err == nil || !strings.Contains(err.Error(), "proxy cleanup is still in progress") {
		t.Fatalf("Resume error = %v, want incomplete cleanup", err)
	}
	operation := requireOperation(t, controller.State, target.Name)
	if operation.Proxy == nil || operation.Proxy.InstanceID != proxyID {
		t.Fatalf("post-resync proxy identity was discarded: %#v", operation.Proxy)
	}
	manager.mu.Lock()
	ensureDelta := manager.ensures - ensuresBefore
	stopAttempts := manager.stopAttempts - stopAttemptsBefore
	manager.mu.Unlock()
	if ensureDelta != 0 || stopAttempts != 1 {
		t.Fatalf("replacement ensures/cleanup attempts = %d/%d, want 0/1", ensureDelta, stopAttempts)
	}
}

func TestAbortReverseResyncRestoresWiringWithoutRestartingRetainedComponents(t *testing.T) {
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
	if err := controller.Up(context.Background(), target); err == nil ||
		!strings.Contains(err.Error(), "interrupt target start") {
		t.Fatalf("target Up error = %v", err)
	}
	operation := requireOperation(t, controller.State, target.Name)
	proxyID := operation.Proxy.InstanceID
	manager := controller.Proxy.(*fakeProxyManager)
	manager.mu.Lock()
	resyncsBefore := manager.resyncs
	manager.mu.Unlock()
	fake.resetCalls()

	if err := controller.Abort(context.Background(), target.Name); err != nil {
		t.Fatal(err)
	}
	deployed := requireDesired(t, controller.State, initial.Name)
	if deployed.Proxy.InstanceID != proxyID {
		t.Fatal("successful reverse resync replaced the proxy")
	}
	for _, name := range []string{"provider", "consumer"} {
		requireSameContainer(t, deployed, name, previous.Containers[name].ID)
		assertContainerUntouched(t, fake, previous.Containers[name].ID)
	}
	if _, exists := deployed.Containers["observer"]; exists {
		t.Fatal("aborted observer remains in desired state")
	}
	manager.mu.Lock()
	resyncsAfter, stops := manager.resyncs, manager.stops
	manager.mu.Unlock()
	if resyncsAfter-resyncsBefore != 1 || stops != 0 {
		t.Fatalf("reverse resync delta/stops = %d/%d, want 1/0", resyncsAfter-resyncsBefore, stops)
	}
}

func TestAbortIncompatibleProxyCapabilityIsTerminal(t *testing.T) {
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
	fake.startFailures["observer"] = []error{errors.New("interrupt target start")}
	target := fanoutSpec("provider:v1", "consumer:v1", "observer:v1")
	if err := controller.Up(context.Background(), target); err == nil ||
		!strings.Contains(err.Error(), "interrupt target start") {
		t.Fatalf("target Up error = %v", err)
	}
	operation := requireOperation(t, controller.State, target.Name)
	proxyID := operation.Proxy.InstanceID
	manager := controller.Proxy.(*fakeProxyManager)
	manager.mu.Lock()
	manager.controlVersions[proxyID] = 0
	attemptsBefore, stopsBefore := manager.resyncAttempts, manager.stops
	manager.mu.Unlock()

	err := controller.Abort(context.Background(), target.Name)
	if !errors.Is(err, proxy.ErrControlProtocolMismatch) {
		t.Fatalf("Abort error = %v, want ErrControlProtocolMismatch", err)
	}
	aborting := requireOperation(t, controller.State, target.Name)
	if aborting.Proxy == nil || aborting.Proxy.InstanceID != proxyID {
		t.Fatalf("abort changed incompatible proxy identity: %#v", aborting.Proxy)
	}
	if aborting.AbortRecreatePrevious {
		t.Fatal("capability mismatch selected abort replacement fallback")
	}
	manager.mu.Lock()
	attempts := manager.resyncAttempts - attemptsBefore
	stops := manager.stops - stopsBefore
	manager.mu.Unlock()
	if attempts != 0 || stops != 0 {
		t.Fatalf("abort capability mismatch attempts/stops = %d/%d, want 0/0", attempts, stops)
	}
}

func TestRetargetedInputResyncsWithoutContainerOperations(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installImages(
		fake,
		"provider-a:v1", "provider-b:v1", "consumer:v1",
		"sha256:provider-a", "sha256:provider-b", "sha256:consumer",
	)
	makeSpec := func(provider string) composition.Spec {
		return composition.Spec{
			Name: "retarget",
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

	initial := makeSpec("provider-a")
	if err := controller.Up(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	first := requireDesired(t, controller.State, initial.Name)
	providerAID := first.Containers["provider-a"].ID
	providerBID := first.Containers["provider-b"].ID
	consumerID := first.Containers["consumer"].ID
	proxyID := first.Proxy.InstanceID
	fake.resetCalls()

	retargeted := makeSpec("provider-b")
	if err := controller.Up(context.Background(), retargeted); err != nil {
		t.Fatal(err)
	}
	second := requireDesired(t, controller.State, retargeted.Name)
	if second.Proxy.InstanceID != proxyID {
		t.Fatal("retargeting replaced the proxy process")
	}
	for name, oldID := range map[string]string{
		"provider-a": providerAID,
		"provider-b": providerBID,
		"consumer":   consumerID,
	} {
		if second.Containers[name].ID != oldID {
			t.Fatalf("retargeting replaced unchanged %s", name)
		}
		assertContainerUntouched(t, fake, oldID)
	}
	if got := len(fake.containerRequests); got != 0 {
		t.Fatalf("pure relink issued %d container creates", got)
	}
}

func TestFanoutUsesProxySocketsWithoutLinkNetworks(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installImages(
		fake,
		"provider:v1", "consumer:v1", "observer:v1",
		"sha256:provider", "sha256:consumer", "sha256:observer",
	)
	spec := fanoutSpec("provider:v1", "consumer:v1", "observer:v1")
	if err := controller.Up(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	deployment := requireDesired(t, controller.State, spec.Name)

	if got := len(deployment.Networks); got != 0 {
		t.Fatalf("network count = %d, want none", got)
	}
	for _, name := range []string{"provider", "consumer", "observer"} {
		request := requestsByComponent(fake.containerRequests)[name]
		if request.NetworkID != "" || len(request.NetworkAliases) != 0 {
			t.Fatalf("%s network request = %#v, want network mode none", name, request)
		}
		if networks := fake.containers[deployment.Containers[name].ID].Networks; len(networks) != 0 {
			t.Fatalf("%s networks = %#v, want none", name, networks)
		}
	}
	requests := requestsByComponent(fake.containerRequests)
	for _, name := range []string{"consumer", "observer"} {
		request := requests[name]
		if request.Environment["DCOMP_IN_UPSTREAM"] != "unix:///run/dcomp/in/upstream" {
			t.Fatalf("%s has unexpected input environment: %#v", name, request.Environment)
		}
		if !hasMountTarget(request.Mounts, "/run/dcomp/in/upstream") {
			t.Fatalf("%s has no private input socket mount", name)
		}
	}
}

func TestCyclesAreValidAndCreatedBeforeStart(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installImages(
		fake,
		"alpha:v1", "beta:v1",
		"sha256:alpha", "sha256:beta",
	)
	spec := cycleSpec()
	if err := controller.Up(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	deployment := requireDesired(t, controller.State, spec.Name)
	if got := len(deployment.Networks); got != 0 {
		t.Fatalf("cycle network count = %d, want none", got)
	}
	requests := requestsByComponent(fake.containerRequests)
	for _, name := range []string{"alpha", "beta"} {
		if requests[name].NetworkID != "" {
			t.Fatalf("%s received a Docker network", name)
		}
	}
	if got := requests["alpha"].Environment["DCOMP_IN_PEER"]; got != "unix:///run/dcomp/in/peer" {
		t.Fatalf("alpha peer = %q", got)
	}
	if got := requests["beta"].Environment["DCOMP_IN_PEER"]; got != "unix:///run/dcomp/in/peer" {
		t.Fatalf("beta peer = %q", got)
	}
	mutations := fake.mutationCalls()
	if lastCallIndex(mutations, "create-container") >= callIndex(mutations, "start-container", "") {
		t.Fatalf("cycle started before every container existed: %#v", mutations)
	}
}

func TestPersistentVolumeSurvivesReplacementAndDown(t *testing.T) {
	controller, fake := newControllerHarness(t)
	fake.images["worker:v1"] = engine.Image{
		ID: "sha256:worker-v1", HasHealthcheck: true,
		DeclaredVolumes: []string{"/data"},
	}
	fake.images["worker:v2"] = engine.Image{
		ID: "sha256:worker-v2", HasHealthcheck: true,
		DeclaredVolumes: []string{"/data"},
	}
	v1 := volumeSpec("worker:v1")
	if err := controller.Up(context.Background(), v1); err != nil {
		t.Fatal(err)
	}
	first := requireDesired(t, controller.State, v1.Name)
	firstID := first.Containers["worker"].ID
	volume := volumeName(controller.dockerScope(v1.Name), "worker", "data")
	firstVolume := fake.volumes[volume]
	if firstVolume.Name == "" {
		t.Fatal("persistent volume was not created")
	}

	if err := controller.Up(context.Background(), volumeSpec("worker:v2")); err != nil {
		t.Fatal(err)
	}
	second := requireDesired(t, controller.State, v1.Name)
	if second.Containers["worker"].ID == firstID {
		t.Fatal("image replacement retained old worker container")
	}
	if got := fake.volumes[volume]; !reflect.DeepEqual(got, firstVolume) {
		t.Fatalf("volume changed across replacement:\n got: %#v\nwant: %#v", got, firstVolume)
	}

	if err := controller.Down(context.Background(), v1.Name); err != nil {
		t.Fatal(err)
	}
	requireNoDesired(t, controller.State, v1.Name)
	if got := fake.volumes[volume]; !reflect.DeepEqual(got, firstVolume) {
		t.Fatalf("volume changed during down:\n got: %#v\nwant: %#v", got, firstVolume)
	}
	inspected, err := controller.InspectPersistentVolume(
		context.Background(), "storage", "worker", "data",
	)
	if err != nil {
		t.Fatalf("inspect persistent volume after down: %v", err)
	}
	if inspected.Name != volume {
		t.Fatalf("volume after down = %q, want %q", inspected.Name, volume)
	}
	if len(fake.containers) != 0 || len(fake.networks) != 0 {
		t.Fatalf("down left transient resources: containers=%#v networks=%#v",
			fake.containers, fake.networks)
	}
}

func TestUnknownNetworkAttachmentFailsClosedBeforeMutation(t *testing.T) {
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
	deployment := requireDesired(t, controller.State, spec.Name)
	consumerID := deployment.Containers["consumer"].ID

	fake.mu.Lock()
	container := fake.containers[consumerID]
	container.Networks["foreign"] = engine.NetworkAttachment{
		NetworkID: "foreign-network",
		Aliases:   []string{"consumer"},
	}
	fake.containers[consumerID] = container
	fake.mu.Unlock()
	fake.resetCalls()

	err := controller.Up(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "undeclared network") {
		t.Fatalf("Up error = %v, want undeclared-network refusal", err)
	}
	if mutations := fake.mutationCalls(); len(mutations) != 0 {
		t.Fatalf("foreign attachment caused mutations: %#v", mutations)
	}
	requireNoOperation(t, controller.State, spec.Name)
}

func TestSameSpecRepairsMissingKnownAttachmentWithoutRestart(t *testing.T) {
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
	deployment := requireDesired(t, controller.State, spec.Name)
	baseID := deployment.Networks["component/consumer"].ID
	consumerID := deployment.Containers["consumer"].ID
	if err := fake.DisconnectNetwork(
		context.Background(),
		baseID,
		consumerID,
	); err != nil {
		t.Fatal(err)
	}
	fake.resetCalls()

	if err := controller.Up(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	repaired := requireDesired(t, controller.State, spec.Name)
	requireSameContainer(t, repaired, "provider", deployment.Containers["provider"].ID)
	requireSameContainer(t, repaired, "consumer", consumerID)
	if !containsCall(
		fake.mutationCalls(),
		"connect-network",
		baseID+"->"+consumerID,
	) {
		t.Fatalf("missing base attachment was not restored: %#v", fake.mutationCalls())
	}
	assertContainerUntouched(t, fake, deployment.Containers["provider"].ID)
	assertContainerUntouched(t, fake, consumerID)
}

func TestSameSpecRepairsMissingNetworkAliasWithoutRestart(t *testing.T) {
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
	deployment := requireDesired(t, controller.State, spec.Name)
	baseID := deployment.Networks["component/consumer"].ID
	consumerID := deployment.Containers["consumer"].ID
	fake.mu.Lock()
	consumer := fake.containers[consumerID]
	for name, attachment := range consumer.Networks {
		if attachment.NetworkID == baseID {
			attachment.Aliases = nil
			consumer.Networks[name] = attachment
		}
	}
	fake.containers[consumerID] = consumer
	fake.mu.Unlock()
	fake.resetCalls()

	if err := controller.Up(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	mutations := fake.mutationCalls()
	target := baseID + "->" + consumerID
	if !containsCall(mutations, "disconnect-network", target) ||
		!containsCall(mutations, "connect-network", target) {
		t.Fatalf("missing alias was not restored by reattachment: %#v", mutations)
	}
	assertContainerUntouched(t, fake, deployment.Containers["provider"].ID)
	assertContainerUntouched(t, fake, consumerID)
}

func TestAliasRepairResumesAfterLostDisconnectResponse(t *testing.T) {
	controller, fake := newControllerHarness(t)
	fake.images["worker:v1"] = healthyImage("sha256:worker")
	spec := composition.Spec{
		Name: "solo",
		Components: []composition.Instance{
			component("worker", "worker:v1", nil, nil),
		},
	}
	spec.Components[0].Runtime.ExternalEgress = true
	if err := controller.Up(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	deployment := requireDesired(t, controller.State, spec.Name)
	workerID := deployment.Containers["worker"].ID
	baseID := deployment.Networks["component/worker"].ID
	fake.mu.Lock()
	worker := fake.containers[workerID]
	for name, attachment := range worker.Networks {
		attachment.Aliases = nil
		worker.Networks[name] = attachment
	}
	fake.containers[workerID] = worker
	fake.disconnectErrors[baseID+"->"+workerID] = []error{
		errors.New("lost disconnect response"),
	}
	fake.mu.Unlock()
	fake.resetCalls()

	err := controller.Up(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "lost disconnect response") {
		t.Fatalf("Up error = %v, want lost disconnect response", err)
	}
	requireOperation(t, controller.State, spec.Name)
	fake.resetCalls()

	if err := controller.Resume(context.Background(), spec.Name); err != nil {
		t.Fatal(err)
	}
	requireNoOperation(t, controller.State, spec.Name)
	repaired := requireDesired(t, controller.State, spec.Name)
	requireSameContainer(t, repaired, "worker", workerID)
	if !containsCall(
		fake.mutationCalls(),
		"connect-network",
		baseID+"->"+workerID,
	) {
		t.Fatalf("resume did not restore base attachment: %#v", fake.mutationCalls())
	}
	assertContainerUntouched(t, fake, workerID)
}

func TestResumeRepairsEgressNetworkRemovedByFailedStart(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installImages(
		fake,
		"provider:v1", "consumer:v1",
		"sha256:provider", "sha256:consumer",
	)
	spec := linkedSpec("provider:v1", "consumer:v1")
	spec.Components[1].Runtime.ExternalEgress = true
	fake.startFailures["consumer"] = []error{errors.New("bind address already in use")}

	err := controller.Up(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "bind address already in use") {
		t.Fatalf("Up error = %v, want injected start failure", err)
	}
	operation := requireOperation(t, controller.State, spec.Name)
	if operation.Phase != phaseStart {
		t.Fatalf("operation phase = %q, want %q", operation.Phase, phaseStart)
	}
	consumerID := operation.Containers["consumer"].ID
	baseID := operation.Networks[componentNetworkKey("consumer")].ID
	if err := fake.DisconnectNetwork(
		context.Background(),
		baseID,
		consumerID,
	); err != nil {
		t.Fatal(err)
	}
	fake.resetCalls()

	if err := controller.Resume(context.Background(), spec.Name); err != nil {
		t.Fatal(err)
	}
	repaired := requireDesired(t, controller.State, spec.Name)
	requireNoOperation(t, controller.State, spec.Name)
	requireSameContainer(t, repaired, "consumer", consumerID)
	if !containsCall(
		fake.mutationCalls(),
		"connect-network",
		baseID+"->"+consumerID,
	) {
		t.Fatalf("resume did not restore the egress network: %#v", fake.mutationCalls())
	}
	if !fake.containers[consumerID].Running {
		t.Fatal("consumer was not started after its network was repaired")
	}
}

func TestUpResumesMatchingInterruptedApply(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installImages(
		fake,
		"provider:v1", "consumer:v1",
		"sha256:provider", "sha256:consumer",
	)
	spec := linkedSpec("provider:v1", "consumer:v1")
	fake.startFailures["consumer"] = []error{errors.New("transient start failure")}

	if err := controller.Up(context.Background(), spec); err == nil {
		t.Fatal("initial Up unexpectedly succeeded")
	}
	if err := controller.Up(context.Background(), spec); err != nil {
		t.Fatalf("matching Up did not resume: %v", err)
	}
	requireNoOperation(t, controller.State, spec.Name)
	if deployment := requireDesired(t, controller.State, spec.Name); len(deployment.Containers) != 2 {
		t.Fatalf("resumed deployment = %#v", deployment)
	}
}

func TestUpSupersedesInterruptedApplyForDifferentTarget(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installImages(
		fake,
		"provider:v1", "consumer:v1", "consumer:v2",
		"sha256:provider", "sha256:consumer-v1", "sha256:consumer-v2",
	)
	initial := linkedSpec("provider:v1", "consumer:v1")
	fake.startFailures["consumer"] = []error{errors.New("old target cannot start")}

	if err := controller.Up(context.Background(), initial); err == nil {
		t.Fatal("initial Up unexpectedly succeeded")
	}
	interrupted := requireOperation(t, controller.State, initial.Name)
	oldProviderID := interrupted.Containers["provider"].ID
	oldConsumerID := interrupted.Containers["consumer"].ID
	fake.resetCalls()

	replacement := linkedSpec("provider:v1", "consumer:v2")
	if err := controller.Up(context.Background(), replacement); err != nil {
		t.Fatalf("replacement Up did not supersede stale target: %v", err)
	}
	requireNoOperation(t, controller.State, replacement.Name)
	deployed := requireDesired(t, controller.State, replacement.Name)
	if got := fake.containers[deployed.Containers["consumer"].ID].ImageID; got != "sha256:consumer-v2" {
		t.Fatalf("replacement consumer image = %q", got)
	}
	for _, oldID := range []string{oldProviderID, oldConsumerID} {
		if _, exists := fake.containers[oldID]; exists {
			t.Fatalf("superseded container %s still exists", oldID)
		}
		if !containsCall(fake.mutationCalls(), "remove-container", oldID) {
			t.Fatalf("superseded container %s was not removed", oldID)
		}
	}
}

func TestChangedApplyPreflightsEveryOldContainerBeforeMutation(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installImages(
		fake,
		"provider:v1", "consumer:v1", "provider:v2", "consumer:v2",
		"sha256:provider-v1", "sha256:consumer-v1",
		"sha256:provider-v2", "sha256:consumer-v2",
	)
	initial := linkedSpec("provider:v1", "consumer:v1")
	if err := controller.Up(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	deployment := requireDesired(t, controller.State, initial.Name)
	providerID := deployment.Containers["provider"].ID
	fake.mu.Lock()
	provider := fake.containers[providerID]
	provider.Networks["foreign"] = engine.NetworkAttachment{
		NetworkID: "foreign-network",
		Aliases:   []string{"provider"},
	}
	fake.containers[providerID] = provider
	fake.mu.Unlock()
	fake.resetCalls()

	err := controller.Up(
		context.Background(),
		linkedSpec("provider:v2", "consumer:v2"),
	)
	if err == nil || !strings.Contains(err.Error(), "undeclared network") {
		t.Fatalf("Up error = %v, want undeclared-network refusal", err)
	}
	if mutations := fake.mutationCalls(); len(mutations) != 0 {
		t.Fatalf("preflight failure followed mutations: %#v", mutations)
	}
	requireOperation(t, controller.State, initial.Name)
}

func TestDownCanCleanDeploymentWithMissingRecordedContainer(t *testing.T) {
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
	deployment := requireDesired(t, controller.State, spec.Name)
	fake.deleteContainerOutOfBand(deployment.Containers["consumer"].ID)

	if err := controller.Down(context.Background(), spec.Name); err != nil {
		t.Fatal(err)
	}
	requireNoDesired(t, controller.State, spec.Name)
	requireNoOperation(t, controller.State, spec.Name)
	if len(fake.containers) != 0 || len(fake.networks) != 0 {
		t.Fatalf(
			"down left resources after external loss: containers=%#v networks=%#v",
			fake.containers,
			fake.networks,
		)
	}
}

func TestLostCreateResponseIsRecoveredWithoutDuplicateResources(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installImages(
		fake,
		"provider:v1", "consumer:v1",
		"sha256:provider", "sha256:consumer",
	)
	spec := linkedSpec("provider:v1", "consumer:v1")
	spec.Components[1].Runtime.ExternalEgress = true
	fake.createNetworkErrors[componentNetworkName(
		controller.dockerScope(spec.Name), "consumer",
	)] =
		[]error{errors.New("lost create response")}

	if err := controller.Up(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	requireDesired(t, controller.State, spec.Name)
	requireNoOperation(t, controller.State, spec.Name)
	if got := len(fake.callsFor("create-network")); got != 1 {
		t.Fatalf("network creates = %d, want exactly 1", got)
	}
	if got := len(fake.networks); got != 1 {
		t.Fatalf("network objects = %d, want 1", got)
	}
}

func TestAbortIncrementalApplyPreservesRetainedComponents(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installImages(
		fake,
		"provider:v1", "consumer:v1", "observer:v1", "observer:v2",
		"sha256:provider", "sha256:consumer", "sha256:observer-v1", "sha256:observer-v2",
	)
	initial := fanoutSpec("provider:v1", "consumer:v1", "observer:v1")
	if err := controller.Up(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	previous := requireDesired(t, controller.State, initial.Name)
	providerID := previous.Containers["provider"].ID
	consumerID := previous.Containers["consumer"].ID

	fake.startFailures["observer"] = []error{errors.New("observer start failed")}
	err := controller.Up(
		context.Background(),
		fanoutSpec("provider:v1", "consumer:v1", "observer:v2"),
	)
	if err == nil || !strings.Contains(err.Error(), "observer start failed") {
		t.Fatalf("Up error = %v, want injected start error", err)
	}
	requireOperation(t, controller.State, initial.Name)
	fake.resetCalls()

	if err := controller.Abort(context.Background(), initial.Name); err != nil {
		t.Fatal(err)
	}
	requireNoOperation(t, controller.State, initial.Name)
	current := requireDesired(t, controller.State, initial.Name)
	requireSameContainer(t, current, "provider", providerID)
	requireSameContainer(t, current, "consumer", consumerID)
	if _, exists := fake.containers[providerID]; !exists {
		t.Fatal("abort deleted retained provider")
	}
	if _, exists := fake.containers[consumerID]; !exists {
		t.Fatal("abort deleted retained consumer")
	}
	assertContainerUntouched(t, fake, providerID)
	assertContainerUntouched(t, fake, consumerID)
}

func TestTargetedRestartTouchesOnlySelectedComponent(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installImages(
		fake,
		"provider:v1", "consumer:v1", "observer:v1",
		"sha256:provider", "sha256:consumer", "sha256:observer",
	)
	spec := fanoutSpec("provider:v1", "consumer:v1", "observer:v1")
	if err := controller.Up(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	deployment := requireDesired(t, controller.State, spec.Name)
	consumerID := deployment.Containers["consumer"].ID
	fake.resetCalls()

	if err := controller.Restart(context.Background(), spec.Name, "consumer"); err != nil {
		t.Fatal(err)
	}
	requireNoOperation(t, controller.State, spec.Name)
	want := []engineCall{
		{Method: "restart-container", Target: consumerID},
	}
	if got := fake.mutationCalls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("targeted restart mutations:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestRestartRequiresConvergedCommittedProxy(t *testing.T) {
	tests := []struct {
		name   string
		ready  bool
		digest string
	}{
		{name: "not ready without digest"},
		{name: "not ready at committed digest", digest: "committed"},
		{name: "ready at other digest", ready: true, digest: "other"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
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
			deployed := requireDesired(t, controller.State, spec.Name)
			manager := controller.Proxy.(*fakeProxyManager)
			manager.mu.Lock()
			process := manager.processes[deployed.Proxy.InstanceID]
			switch test.digest {
			case "committed":
				process.Digest = deployed.Proxy.Digest
			default:
				process.Digest = test.digest
			}
			manager.processes[deployed.Proxy.InstanceID] = process
			manager.ready[deployed.Proxy.InstanceID] = test.ready
			manager.mu.Unlock()
			fake.resetCalls()

			if err := controller.Restart(
				context.Background(),
				spec.Name,
				"consumer",
			); err == nil {
				t.Fatal("restart succeeded with an unconverged proxy")
			}
			if got := fake.mutationCalls(); len(got) != 0 {
				t.Fatalf("failed restart preflight performed mutations: %#v", got)
			}
			requireOperation(t, controller.State, spec.Name)

			manager.mu.Lock()
			process = manager.processes[deployed.Proxy.InstanceID]
			process.Digest = deployed.Proxy.Digest
			manager.processes[deployed.Proxy.InstanceID] = process
			manager.ready[deployed.Proxy.InstanceID] = true
			manager.mu.Unlock()
			fake.resetCalls()
			if err := controller.Resume(context.Background(), spec.Name); err != nil {
				t.Fatal(err)
			}
			requireNoOperation(t, controller.State, spec.Name)
			want := []engineCall{{
				Method: "restart-container",
				Target: deployed.Containers["consumer"].ID,
			}}
			if got := fake.mutationCalls(); !reflect.DeepEqual(got, want) {
				t.Fatalf("resumed restart mutations:\n got: %#v\nwant: %#v", got, want)
			}
		})
	}
}

func TestRestartLostResponseRetriesTheSameConvergentOperation(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installImages(
		fake,
		"provider:v1", "consumer:v1", "observer:v1",
		"sha256:provider", "sha256:consumer", "sha256:observer",
	)
	spec := fanoutSpec("provider:v1", "consumer:v1", "observer:v1")
	if err := controller.Up(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	deployment := requireDesired(t, controller.State, spec.Name)
	consumerID := deployment.Containers["consumer"].ID
	fake.restartFailures["consumer"] = []error{errors.New("lost restart response")}
	fake.resetCalls()

	err := controller.Restart(context.Background(), spec.Name, "consumer")
	if err == nil || !strings.Contains(err.Error(), "lost restart response") {
		t.Fatalf("Restart error = %v, want injected response loss", err)
	}
	operation := requireOperation(t, controller.State, spec.Name)
	if operation.Phase != phaseRestart {
		t.Fatalf("restart phase = %q, want %q", operation.Phase, phaseRestart)
	}
	if !fake.containers[consumerID].Running {
		t.Fatal("lost response occurred after Docker had restarted the container")
	}

	fake.resetCalls()
	if err := controller.Resume(context.Background(), spec.Name); err != nil {
		t.Fatal(err)
	}
	requireNoOperation(t, controller.State, spec.Name)
	want := []engineCall{{Method: "restart-container", Target: consumerID}}
	if got := fake.mutationCalls(); !reflect.DeepEqual(got, want) {
		t.Fatalf("resume mutations:\n got: %#v\nwant: %#v", got, want)
	}
	if !fake.containers[consumerID].Running {
		t.Fatal("repeated restart did not converge to a running container")
	}
}

func newControllerHarness(t *testing.T) (*Controller, *fakeEngine) {
	t.Helper()
	fake := newFakeEngine()
	runtimeRoot := t.TempDir()
	controller := &Controller{
		Engine:         fake,
		Proxy:          newFakeProxyManager(),
		State:          state.Store{Root: t.TempDir()},
		RuntimeRoot:    runtimeRoot,
		RequestTimeout: time.Second,
		StopTimeout:    time.Millisecond,
	}
	return controller, fake
}

func healthyImage(id string) engine.Image {
	return engine.Image{ID: id, HasHealthcheck: true}
}

func linkedSpec(providerImage, consumerImage string) composition.Spec {
	return composition.Spec{
		Name: "demo",
		Components: []composition.Instance{
			component("provider", providerImage, nil, []string{"echo"}),
			component("consumer", consumerImage, []string{"upstream"}, nil),
		},
		Links: []composition.Link{link("consumer", "upstream", "provider", "echo")},
	}
}

func fanoutSpec(providerImage, consumerImage, observerImage string) composition.Spec {
	return composition.Spec{
		Name: "demo",
		Components: []composition.Instance{
			component("provider", providerImage, nil, []string{"echo"}),
			component("consumer", consumerImage, []string{"upstream"}, nil),
			component("observer", observerImage, []string{"upstream"}, nil),
		},
		Links: []composition.Link{
			link("consumer", "upstream", "provider", "echo"),
			link("observer", "upstream", "provider", "echo"),
		},
	}
}

func cycleSpec() composition.Spec {
	return composition.Spec{
		Name: "cycle",
		Components: []composition.Instance{
			component("alpha", "alpha:v1", []string{"peer"}, []string{"echo"}),
			component("beta", "beta:v1", []string{"peer"}, []string{"echo"}),
		},
		Links: []composition.Link{
			link("alpha", "peer", "beta", "echo"),
			link("beta", "peer", "alpha", "echo"),
		},
	}
}

func volumeSpec(image string) composition.Spec {
	instance := component("worker", image, nil, nil)
	instance.Runtime.Volumes = []composition.VolumeMount{{
		Name: "data", Target: "/data",
	}}
	return composition.Spec{Name: "storage", Components: []composition.Instance{instance}}
}

func component(
	name string,
	image string,
	inputs []string,
	outputs []string,
) composition.Instance {
	definition := composition.Definition{}
	for _, input := range inputs {
		definition.Inputs = append(definition.Inputs, composition.Endpoint{
			Name: input, Service: echoService,
		})
	}
	for _, output := range outputs {
		definition.Outputs = append(definition.Outputs, composition.Endpoint{
			Name: output, Service: echoService,
		})
	}
	return composition.Instance{
		Name: name,
		Component: composition.Component{
			Image: image, Definition: definition,
		},
	}
}

func link(
	consumer, input, producer, output string,
) composition.Link {
	return composition.Link{
		Input:  composition.EndpointRef{Component: consumer, Endpoint: input},
		Output: composition.EndpointRef{Component: producer, Endpoint: output},
	}
}

func installImages(fake *fakeEngine, values ...string) {
	if len(values)%2 != 0 {
		panic("installImages expects references followed by IDs")
	}
	half := len(values) / 2
	for index := 0; index < half; index++ {
		fake.images[values[index]] = healthyImage(values[index+half])
	}
}

func requestsByComponent(
	requests []engine.ContainerRequest,
) map[string]engine.ContainerRequest {
	result := make(map[string]engine.ContainerRequest, len(requests))
	for _, request := range requests {
		result[request.Labels[LabelComponent]] = request
	}
	return result
}

func requireDesired(t *testing.T, store state.Store, name string) state.Deployment {
	t.Helper()
	deployment, exists, err := store.ReadDesired(name)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatalf("desired state for %q is absent", name)
	}
	return deployment
}

func requireNoDesired(t *testing.T, store state.Store, name string) {
	t.Helper()
	_, exists, err := store.ReadDesired(name)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatalf("desired state for %q still exists", name)
	}
}

func requireOperation(t *testing.T, store state.Store, name string) state.Operation {
	t.Helper()
	operation, exists, err := store.ReadOperation(name)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Fatalf("operation for %q is absent", name)
	}
	return operation
}

func requireNoOperation(t *testing.T, store state.Store, name string) {
	t.Helper()
	_, exists, err := store.ReadOperation(name)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatalf("operation for %q still exists", name)
	}
}

func requireNetwork(t *testing.T, fake *fakeEngine, id string) engine.Network {
	t.Helper()
	network, exists := fake.networks[id]
	if !exists {
		t.Fatalf("network %q does not exist", id)
	}
	return network
}

func requireMembers(t *testing.T, network engine.Network, want ...string) {
	t.Helper()
	got := make([]string, 0, len(network.Endpoints))
	for _, endpoint := range network.Endpoints {
		got = append(got, endpoint.Key)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s members:\n got: %#v\nwant: %#v", network.Name, got, want)
	}
}

func hasMountTarget(mounts []engine.Mount, target string) bool {
	for _, mount := range mounts {
		if mount.Target == target {
			return true
		}
	}
	return false
}

func requireSameContainer(
	t *testing.T,
	deployment state.Deployment,
	component string,
	wantID string,
) {
	t.Helper()
	if got := deployment.Containers[component].ID; got != wantID {
		t.Fatalf("%s container ID = %q, want retained %q", component, got, wantID)
	}
}

func assertContainerUntouched(t *testing.T, fake *fakeEngine, id string) {
	t.Helper()
	for _, method := range []string{
		"stop-container", "start-container", "remove-container",
	} {
		for _, call := range fake.callsFor(method) {
			if call.Target == id {
				t.Fatalf("unchanged container %s received %s", id, method)
			}
		}
	}
}

func callIndex(calls []engineCall, method, target string) int {
	for index, call := range calls {
		if call.Method == method && (target == "" || call.Target == target) {
			return index
		}
	}
	return -1
}

func containsCall(calls []engineCall, method, target string) bool {
	return callIndex(calls, method, target) >= 0
}

func lastCallIndex(calls []engineCall, method string) int {
	for index := len(calls) - 1; index >= 0; index-- {
		if calls[index].Method == method {
			return index
		}
	}
	return -1
}
