package lifecycle

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/glguida/dcomp/composition"
	"github.com/glguida/dcomp/engine"
	"github.com/glguida/dcomp/state"
)

func TestApplyResumesAfterContainerRemovalLeavesEndpoint(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installImages(
		fake,
		"worker:v1", "worker:v2",
		"sha256:worker-v1", "sha256:worker-v2",
	)
	initial := workerSpec("worker:v1", true)
	if err := controller.Up(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	previous := requireDesired(t, controller.State, initial.Name)
	container := previous.Containers["worker"]
	network := previous.Networks[componentNetworkKey("worker")]

	fake.mu.Lock()
	fake.orphanOnRemove[container.ID] = true
	fake.forceDisconnectErrors[network.ID+"->"+container.Name] = []error{
		errors.New("interrupted endpoint cleanup"),
	}
	fake.mu.Unlock()

	err := controller.Up(context.Background(), workerSpec("worker:v2", true))
	if err == nil || !strings.Contains(err.Error(), "interrupted endpoint cleanup") {
		t.Fatalf("replacement Up error = %v", err)
	}
	operation := requireOperation(t, controller.State, initial.Name)
	if len(operation.EndpointCleanups) != 1 {
		t.Fatalf("endpoint cleanup journal = %#v", operation.EndpointCleanups)
	}
	requireOrphanEndpoint(t, fake, network.ID, container.Name)

	if err := controller.Resume(context.Background(), initial.Name); err != nil {
		t.Fatal(err)
	}
	requireNoOperation(t, controller.State, initial.Name)
	deployed := requireDesired(t, controller.State, initial.Name)
	if deployed.Containers["worker"].ID == container.ID {
		t.Fatal("resume retained the removed container")
	}
	requireMembers(t, requireNetwork(t, fake, network.ID), deployed.Containers["worker"].ID)
}

func TestApplyRecoversUnjournaledOrphanFromAffectedState(t *testing.T) {
	for _, completed := range []bool{false, true} {
		name := "not checkpointed"
		if completed {
			name = "already checkpointed"
		}
		t.Run(name, func(t *testing.T) {
			controller, fake, previous, operation := interruptedWorkerReplacement(t)
			container := previous.Containers["worker"]
			network := previous.Networks[componentNetworkKey("worker")]
			if completed {
				operation.Completed["retire/worker"] = true
			}
			if err := controller.State.WriteOperation(
				operation.Target.Name,
				operation,
			); err != nil {
				t.Fatal(err)
			}
			fake.orphanContainerOutOfBand(container.ID)
			requireOrphanEndpoint(t, fake, network.ID, container.Name)

			if err := controller.Resume(context.Background(), operation.Target.Name); err != nil {
				t.Fatal(err)
			}
			requireNoOperation(t, controller.State, operation.Target.Name)
			deployed := requireDesired(t, controller.State, operation.Target.Name)
			requireMembers(
				t,
				requireNetwork(t, fake, network.ID),
				deployed.Containers["worker"].ID,
			)
		})
	}
}

func TestEndpointCleanupFailsClosedOnIdentityMismatch(t *testing.T) {
	tests := map[string]func(*engine.NetworkEndpoint){
		"name": func(endpoint *engine.NetworkEndpoint) {
			endpoint.Name = "dcomp.demo.container.foreign"
		},
		"endpoint ID": func(endpoint *engine.NetworkEndpoint) {
			endpoint.EndpointID = "foreign-endpoint"
			endpoint.Key = "ep-foreign-endpoint"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			controller, fake, previous, operation := interruptedWorkerReplacement(t)
			container := previous.Containers["worker"]
			networkResource := previous.Networks[componentNetworkKey("worker")]
			network := requireNetwork(t, fake, networkResource.ID)
			endpoint := network.Endpoints[0]
			operation.EndpointCleanups[endpoint.EndpointID] = state.EndpointCleanup{
				ContainerID: container.ID, ContainerName: container.Name,
				NetworkKey:   componentNetworkKey("worker"),
				NetworkID:    networkResource.ID,
				NetworkName:  networkResource.Name,
				EndpointID:   endpoint.EndpointID,
				EndpointName: container.Name,
			}
			if err := controller.State.WriteOperation(
				operation.Target.Name,
				operation,
			); err != nil {
				t.Fatal(err)
			}
			fake.orphanContainerOutOfBand(container.ID)
			fake.mu.Lock()
			changed := fake.networks[networkResource.ID]
			mutate(&changed.Endpoints[0])
			fake.networks[networkResource.ID] = changed
			fake.mu.Unlock()
			fake.resetCalls()

			err := controller.Resume(context.Background(), operation.Target.Name)
			if err == nil || !strings.Contains(err.Error(), "changed") {
				t.Fatalf("Resume mismatch error = %v", err)
			}
			if calls := fake.callsFor("force-disconnect-network"); len(calls) != 0 {
				t.Fatalf("identity mismatch was disconnected: %#v", calls)
			}
			if len(requireOperation(t, controller.State, operation.Target.Name).EndpointCleanups) != 1 {
				t.Fatal("identity mismatch discarded the cleanup journal")
			}
		})
	}
}

func TestDownResumesAfterContainerRemovalLeavesEndpoint(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installImages(fake, "worker:v1", "sha256:worker-v1")
	spec := workerSpec("worker:v1", true)
	if err := controller.Up(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	previous := requireDesired(t, controller.State, spec.Name)
	container := previous.Containers["worker"]
	network := previous.Networks[componentNetworkKey("worker")]
	fake.mu.Lock()
	fake.orphanOnRemove[container.ID] = true
	fake.forceDisconnectErrors[network.ID+"->"+container.Name] = []error{
		errors.New("interrupted endpoint cleanup"),
	}
	fake.mu.Unlock()

	if err := controller.Down(context.Background(), spec.Name); err == nil {
		t.Fatal("Down unexpectedly completed")
	}
	if got := len(requireOperation(t, controller.State, spec.Name).EndpointCleanups); got != 1 {
		t.Fatalf("down endpoint cleanup count = %d, want 1", got)
	}
	if err := controller.Resume(context.Background(), spec.Name); err != nil {
		t.Fatal(err)
	}
	requireNoOperation(t, controller.State, spec.Name)
	requireNoDesired(t, controller.State, spec.Name)
	if _, exists := fake.networks[network.ID]; exists {
		t.Fatal("Down resume left the egress network")
	}
}

func TestAbortReconcilesEndpointCleanupBeforeClearingOperation(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installImages(
		fake,
		"worker:v1", "worker:v2",
		"sha256:worker-v1", "sha256:worker-v2",
	)
	initial := workerSpec("worker:v1", true)
	if err := controller.Up(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	previous := requireDesired(t, controller.State, initial.Name)
	container := previous.Containers["worker"]
	network := previous.Networks[componentNetworkKey("worker")]
	fake.mu.Lock()
	fake.orphanOnRemove[container.ID] = true
	fake.forceDisconnectErrors[network.ID+"->"+container.Name] = []error{
		errors.New("interrupted endpoint cleanup"),
	}
	fake.mu.Unlock()
	if err := controller.Up(
		context.Background(),
		workerSpec("worker:v2", true),
	); err == nil {
		t.Fatal("replacement Up unexpectedly completed")
	}

	if err := controller.Abort(context.Background(), initial.Name); err != nil {
		t.Fatal(err)
	}
	requireNoOperation(t, controller.State, initial.Name)
	if endpoints := requireNetwork(t, fake, network.ID).Endpoints; len(endpoints) != 0 {
		t.Fatalf("Abort left orphan endpoints: %#v", endpoints)
	}
	if got := requireDesired(t, controller.State, initial.Name); got.Spec.Digest != previous.Spec.Digest {
		t.Fatalf("Abort restored digest %q, want %q", got.Spec.Digest, previous.Spec.Digest)
	}
}

func TestStatusShowsPreviousResourcesAwaitingRetirement(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installImages(
		fake,
		"worker:v1", "worker:v2",
		"sha256:worker-v1", "sha256:worker-v2",
	)
	initial := workerSpec("worker:v1", true)
	if err := controller.Up(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	previous := requireDesired(t, controller.State, initial.Name)
	target, err := controller.resolve(
		context.Background(),
		workerSpec("worker:v2", false),
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
	if err := controller.State.WriteOperation(target.Name, operation); err != nil {
		t.Fatal(err)
	}
	fake.resetCalls()

	status, err := controller.Status(context.Background(), target.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.RetiringComponents) != 1 ||
		status.RetiringComponents[0].ID != previous.Containers["worker"].ID {
		t.Fatalf("retiring components = %#v", status.RetiringComponents)
	}
	if len(status.RetiringNetworks) != 1 ||
		status.RetiringNetworks[0].ID != previous.Networks[componentNetworkKey("worker")].ID {
		t.Fatalf("retiring networks = %#v", status.RetiringNetworks)
	}
	if mutations := fake.mutationCalls(); len(mutations) != 0 {
		t.Fatalf("Status mutated retirement resources: %#v", mutations)
	}
}

func TestNetworkNoneComponentCreatesNoEndpointCleanup(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installImages(fake, "worker:v1", "sha256:worker-v1")
	spec := workerSpec("worker:v1", false)
	if err := controller.Up(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	previous := requireDesired(t, controller.State, spec.Name)
	component, _ := previous.Spec.Component("worker")
	operation, err := state.NewOperation(
		kindApply,
		phaseRetire,
		previous.Spec,
		&previous,
		controller.RuntimeRoot,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.prepareEndpointCleanup(
		context.Background(),
		&operation,
		previous.Spec,
		previous.Networks,
		component,
		previous.Containers["worker"],
	); err != nil {
		t.Fatal(err)
	}
	if len(operation.EndpointCleanups) != 0 {
		t.Fatalf("network-none cleanup journal = %#v", operation.EndpointCleanups)
	}
	if len(fake.networks) != 0 {
		t.Fatalf("network-none component created networks: %#v", fake.networks)
	}
}

func interruptedWorkerReplacement(
	t *testing.T,
) (*Controller, *fakeEngine, state.Deployment, state.Operation) {
	t.Helper()
	controller, fake := newControllerHarness(t)
	installImages(
		fake,
		"worker:v1", "worker:v2",
		"sha256:worker-v1", "sha256:worker-v2",
	)
	initial := workerSpec("worker:v1", true)
	if err := controller.Up(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	previous := requireDesired(t, controller.State, initial.Name)
	target, err := controller.resolve(
		context.Background(),
		workerSpec("worker:v2", true),
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
	return controller, fake, previous, operation
}

func workerSpec(image string, egress bool) composition.Spec {
	worker := component("worker", image, nil, nil)
	worker.Runtime.ExternalEgress = egress
	return composition.Spec{Name: "demo", Components: []composition.Instance{worker}}
}

func requireOrphanEndpoint(
	t *testing.T,
	fake *fakeEngine,
	networkID string,
	containerName string,
) engine.NetworkEndpoint {
	t.Helper()
	network := requireNetwork(t, fake, networkID)
	for _, endpoint := range network.Endpoints {
		if endpoint.Name == containerName &&
			endpoint.Key == "ep-"+endpoint.EndpointID {
			return endpoint
		}
	}
	t.Fatalf("network %s has no orphan for %s: %#v", networkID, containerName, network.Endpoints)
	return engine.NetworkEndpoint{}
}
