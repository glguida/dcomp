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
			Type: engine.MountVolume, Source: "dcomp.demo.volume.consumer.data",
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
	volume, exists := fake.volumes["dcomp.demo.volume.consumer.data"]
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

	withObserver := fanoutSpec("provider:v1", "consumer:v1", "observer:v1")
	fake.resetCalls()
	if err := controller.Up(context.Background(), withObserver); err != nil {
		t.Fatal(err)
	}
	second := requireDesired(t, controller.State, withObserver.Name)
	if second.Containers["provider"].ID == providerID ||
		second.Containers["consumer"].ID == consumerID {
		t.Fatal("wiring change retained a container with a stale socket mount")
	}
	providerID = second.Containers["provider"].ID
	consumerID = second.Containers["consumer"].ID
	observerV1 := second.Containers["observer"].ID

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

	fake.resetCalls()
	if err := controller.Up(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	final := requireDesired(t, controller.State, initial.Name)
	if final.Containers["provider"].ID == providerID ||
		final.Containers["consumer"].ID == consumerID {
		t.Fatal("removing wiring retained a container with a stale socket mount")
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

func TestRetargetedInputReplacesProxyAndSocketMountedComponents(t *testing.T) {
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
	if second.Proxy.InstanceID == proxyID {
		t.Fatal("retargeting retained the old proxy wiring")
	}
	for name, oldID := range map[string]string{
		"provider-a": providerAID,
		"provider-b": providerBID,
		"consumer":   consumerID,
	} {
		if second.Containers[name].ID == oldID {
			t.Fatalf("retargeting retained %s with a stale socket mount", name)
		}
	}
	request := requestsByComponent(fake.containerRequests)["consumer"]
	if got, want := request.Environment["DCOMP_IN_UPSTREAM"], "unix:///run/dcomp/in/upstream"; got != want {
		t.Fatalf("consumer upstream = %q, want %q", got, want)
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
	volume := "dcomp.storage.volume.worker.data"
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
	fake.createNetworkErrors["dcomp.demo.component.consumer"] =
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
