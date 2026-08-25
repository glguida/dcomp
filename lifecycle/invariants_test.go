package lifecycle

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/glguida/dcomp/engine"
	"github.com/glguida/dcomp/proxy"
	"github.com/glguida/dcomp/state"
)

func TestEngineBindingRejectsObservationAndMutationOnAnotherDaemon(t *testing.T) {
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
	fake.mu.Lock()
	fake.identity = "another-engine"
	fake.mu.Unlock()
	fake.resetCalls()

	if _, err := controller.Status(context.Background(), spec.Name); err == nil ||
		!strings.Contains(err.Error(), "bound to Docker engine") {
		t.Fatalf("Status engine mismatch error = %v", err)
	}
	if err := controller.Down(context.Background(), spec.Name); err == nil ||
		!strings.Contains(err.Error(), "bound to Docker engine") {
		t.Fatalf("Down engine mismatch error = %v", err)
	}
	if mutations := fake.mutationCalls(); len(mutations) != 0 {
		t.Fatalf("engine mismatch mutated Docker: %#v", mutations)
	}
	requireDesired(t, controller.State, spec.Name)
}

func TestDifferentStateRootsIsolateSameNamedDockerResources(t *testing.T) {
	fake := newFakeEngine()
	installImages(fake, "worker:v1", "sha256:worker")
	first := controllerForEngine(t, fake)
	second := controllerForEngine(t, fake)
	spec := volumeSpec("worker:v1")
	spec.Components[0].Runtime.ExternalEgress = true

	if err := first.Up(context.Background(), spec); err != nil {
		t.Fatalf("first state root: %v", err)
	}
	if err := second.Up(context.Background(), spec); err != nil {
		t.Fatalf("second state root: %v", err)
	}

	firstDeployment := requireDesired(t, first.State, spec.Name)
	secondDeployment := requireDesired(t, second.State, spec.Name)
	firstScope := first.dockerScope(spec.Name)
	secondScope := second.dockerScope(spec.Name)
	if firstScope.Namespace == secondScope.Namespace {
		t.Fatalf("different roots share namespace %q", firstScope.Namespace)
	}
	firstContainer := firstDeployment.Containers["worker"]
	secondContainer := secondDeployment.Containers["worker"]
	if firstContainer.Name == secondContainer.Name {
		t.Fatalf("containers share physical name %q", firstContainer.Name)
	}
	firstNetwork := firstDeployment.Networks[componentNetworkKey("worker")]
	secondNetwork := secondDeployment.Networks[componentNetworkKey("worker")]
	if firstNetwork.Name == secondNetwork.Name {
		t.Fatalf("networks share physical name %q", firstNetwork.Name)
	}
	firstVolume := volumeName(firstScope, "worker", "data")
	secondVolume := volumeName(secondScope, "worker", "data")
	if firstVolume == secondVolume {
		t.Fatalf("volumes share physical name %q", firstVolume)
	}
	for namespace, labels := range map[string]map[string]string{
		firstScope.Namespace:  fake.containers[firstContainer.ID].Labels,
		secondScope.Namespace: fake.containers[secondContainer.ID].Labels,
	} {
		if labels[LabelNamespace] != namespace {
			t.Fatalf("container namespace labels = %#v, want %q", labels, namespace)
		}
	}
	for _, resource := range []struct {
		namespace string
		labels    map[string]string
	}{
		{firstScope.Namespace, fake.networks[firstNetwork.ID].Labels},
		{secondScope.Namespace, fake.networks[secondNetwork.ID].Labels},
		{firstScope.Namespace, fake.volumes[firstVolume].Labels},
		{secondScope.Namespace, fake.volumes[secondVolume].Labels},
	} {
		if resource.labels[LabelNamespace] != resource.namespace {
			t.Fatalf(
				"resource namespace labels = %#v, want %q",
				resource.labels,
				resource.namespace,
			)
		}
	}
	if _, exists := fake.volumes[firstVolume]; !exists {
		t.Fatalf("first namespaced volume %q is absent", firstVolume)
	}
	if _, exists := fake.volumes[secondVolume]; !exists {
		t.Fatalf("second namespaced volume %q is absent", secondVolume)
	}

	if err := first.Down(context.Background(), spec.Name); err != nil {
		t.Fatalf("down first state root: %v", err)
	}
	status, err := second.Status(context.Background(), spec.Name)
	if err != nil {
		t.Fatalf("observe second state root after first down: %v", err)
	}
	if !status.Operational() {
		t.Fatalf("first down disturbed second state root: %#v", status)
	}
	if _, exists := fake.containers[secondContainer.ID]; !exists {
		t.Fatal("first down removed the second root's container")
	}
	if _, exists := fake.networks[secondNetwork.ID]; !exists {
		t.Fatal("first down removed the second root's network")
	}
}

func TestStatusWaitsForLifecycleGenerationLock(t *testing.T) {
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
	lock, err := controller.State.Acquire(spec.Name)
	if err != nil {
		t.Fatal(err)
	}
	type statusResult struct {
		status Status
		err    error
	}
	result := make(chan statusResult, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() {
		status, err := controller.Status(ctx, spec.Name)
		result <- statusResult{status: status, err: err}
	}()
	select {
	case early := <-result:
		t.Fatalf("Status crossed an exclusive lifecycle lock: %#v", early)
	case <-time.After(40 * time.Millisecond):
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case completed := <-result:
		if completed.err != nil {
			t.Fatal(completed.err)
		}
		if !completed.status.Operational() {
			t.Fatalf("status after lock release is not operational: %#v", completed.status)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestAbortRestoresPreviousDesiredAtCommitBoundary(t *testing.T) {
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
		kindApply, phaseCommit, target, &previous, controller.RuntimeRoot,
	)
	if err != nil {
		t.Fatal(err)
	}
	operation.Networks = cloneResources(previous.Networks)
	operation.Containers = cloneResources(previous.Containers)
	operation.Proxy = cloneProxy(previous.Proxy)
	if err := controller.State.WriteOperation(target.Name, operation); err != nil {
		t.Fatal(err)
	}
	if err := controller.State.WriteDesired(target.Name, state.Deployment{
		Spec:        target,
		RuntimeRoot: controller.RuntimeRoot,
		Proxy:       cloneProxy(operation.Proxy),
		Networks:    cloneResources(operation.Networks),
		Containers:  cloneResources(operation.Containers),
	}); err != nil {
		t.Fatal(err)
	}

	if err := controller.Abort(context.Background(), target.Name); err != nil {
		t.Fatal(err)
	}
	got := requireDesired(t, controller.State, target.Name)
	if !reflect.DeepEqual(got, previous) {
		t.Fatalf("desired state after abort:\n got: %#v\nwant: %#v", got, previous)
	}
	requireNoOperation(t, controller.State, target.Name)
}

func TestAbortInitialApplyClearsDesiredWrittenAtCommitBoundary(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installImages(
		fake,
		"provider:v1", "consumer:v1",
		"sha256:provider", "sha256:consumer",
	)
	target, err := controller.resolve(
		context.Background(),
		linkedSpec("provider:v1", "consumer:v1"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.State.BindEngine(fake.identity); err != nil {
		t.Fatal(err)
	}
	operation, err := state.NewOperation(
		kindApply, phaseCommit, target, nil, controller.RuntimeRoot,
	)
	if err != nil {
		t.Fatal(err)
	}
	config, err := proxyConfig(target, controller.RuntimeRoot, operation.ID)
	if err != nil {
		t.Fatal(err)
	}
	process, err := controller.Proxy.Ensure(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	operation.Proxy = &process
	if err := controller.State.WriteOperation(target.Name, operation); err != nil {
		t.Fatal(err)
	}
	if err := controller.State.WriteDesired(target.Name, state.Deployment{
		Spec: target, RuntimeRoot: controller.RuntimeRoot,
		Proxy: cloneProxy(operation.Proxy), Networks: map[string]state.Resource{},
		Containers: map[string]state.Resource{},
	}); err != nil {
		t.Fatal(err)
	}

	if err := controller.Abort(context.Background(), target.Name); err != nil {
		t.Fatal(err)
	}
	requireNoDesired(t, controller.State, target.Name)
	requireNoOperation(t, controller.State, target.Name)
}

func TestAbortFallbackDurablyHandsOffToResumablePreviousApply(t *testing.T) {
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

	// Land the target wiring and containers, but interrupt the target apply
	// before commit so abort has target-owned work to remove.
	target := fanoutSpec("provider:v1", "consumer:v1", "observer:v1")
	fake.startFailures["observer"] = []error{errors.New("interrupt target start")}
	if err := controller.Up(context.Background(), target); err == nil ||
		!strings.Contains(err.Error(), "interrupt target start") {
		t.Fatalf("target Up error = %v", err)
	}
	interrupted := requireOperation(t, controller.State, target.Name)
	if interrupted.Phase != phaseStart {
		t.Fatalf("interrupted phase = %q, want %q", interrupted.Phase, phaseStart)
	}

	// Make reverse resync fail so abort chooses full replacement, then fail the
	// first start in that replacement. The replacement apply journal must
	// already be durable when Abort returns the injected error.
	manager := controller.Proxy.(*fakeProxyManager)
	manager.beforeResync = func(proxy.Wiring) error {
		return errors.New("forced reverse resync failure")
	}
	fake.startFailures["provider"] = []error{errors.New("interrupt previous recreation")}
	err := controller.Abort(context.Background(), target.Name)
	if err == nil || !strings.Contains(err.Error(), "interrupt previous recreation") {
		t.Fatalf("Abort error = %v", err)
	}
	replacement := requireOperation(t, controller.State, initial.Name)
	if replacement.Kind != kindApply || replacement.Phase != phaseStart {
		t.Fatalf(
			"replacement operation = kind %q phase %q, want apply/start",
			replacement.Kind,
			replacement.Phase,
		)
	}
	if replacement.Target.Digest != previous.Spec.Digest ||
		replacement.RuntimeRoot != previous.RuntimeRoot {
		t.Fatalf("replacement operation does not target previous deployment: %#v", replacement)
	}

	manager.beforeResync = nil
	if err := controller.Resume(context.Background(), initial.Name); err != nil {
		t.Fatal(err)
	}
	deployed := requireDesired(t, controller.State, initial.Name)
	if deployed.Spec.Digest != previous.Spec.Digest {
		t.Fatalf("resumed deployment digest = %q, want %q", deployed.Spec.Digest, previous.Spec.Digest)
	}
	requireNoOperation(t, controller.State, initial.Name)
}

type failFirstNetworkCreate struct {
	*fakeEngine
	name   string
	failed bool
}

func (fake *failFirstNetworkCreate) CreateNetwork(
	ctx context.Context,
	request engine.NetworkRequest,
) (engine.Network, error) {
	fake.mu.Lock()
	if request.Name == fake.name && !fake.failed {
		fake.failed = true
		fake.record("create-network", request.Name)
		fake.mu.Unlock()
		return engine.Network{}, errors.New("ambiguous network create")
	}
	fake.mu.Unlock()
	return fake.fakeEngine.CreateNetwork(ctx, request)
}

type failFirstContainerCreate struct {
	*fakeEngine
	name   string
	failed bool
}

func (fake *failFirstContainerCreate) CreateContainer(
	ctx context.Context,
	request engine.ContainerRequest,
) (engine.Container, error) {
	fake.mu.Lock()
	if request.Name == fake.name && !fake.failed {
		fake.failed = true
		fake.record("create-container", request.Name)
		fake.mu.Unlock()
		return engine.Container{}, errors.New("ambiguous container create")
	}
	fake.mu.Unlock()
	return fake.fakeEngine.CreateContainer(ctx, request)
}

func TestAbortRefusesPendingNetworkCreateWithoutChangingOperation(t *testing.T) {
	base := newFakeEngine()
	fake := &failFirstNetworkCreate{
		fakeEngine: base,
	}
	controller := controllerForEngine(t, fake)
	fake.name = componentNetworkName(controller.dockerScope("demo"), "consumer")
	installImages(
		base,
		"provider:v1", "consumer:v1",
		"sha256:provider", "sha256:consumer",
	)
	spec := linkedSpec("provider:v1", "consumer:v1")
	spec.Components[1].Runtime.ExternalEgress = true
	err := controller.Up(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "ambiguous network create") {
		t.Fatalf("Up error = %v", err)
	}
	operation := requireOperation(t, controller.State, spec.Name)
	if !operation.PendingCreates["network/component/consumer"] {
		t.Fatalf("pending network create was not durable: %#v", operation.PendingCreates)
	}
	mutationsBeforeAbort := append([]engineCall(nil), base.mutationCalls()...)

	err = controller.Abort(context.Background(), spec.Name)
	if err == nil ||
		!strings.Contains(err.Error(), "unresolved creates") ||
		!strings.Contains(err.Error(), "dcomp resume demo") {
		t.Fatalf("Abort error = %v", err)
	}
	afterAbort := requireOperation(t, controller.State, spec.Name)
	if !reflect.DeepEqual(afterAbort, operation) {
		t.Fatalf("abort changed operation:\n got: %#v\nwant: %#v", afterAbort, operation)
	}
	requireNoDesired(t, controller.State, spec.Name)
	if mutations := base.mutationCalls(); !reflect.DeepEqual(mutations, mutationsBeforeAbort) {
		t.Fatalf("abort mutated Docker:\n got: %#v\nwant: %#v", mutations, mutationsBeforeAbort)
	}
	if got := countEngineCalls(
		base.callsFor("create-network"),
		"create-network",
		fake.name,
	); got != 1 {
		t.Fatalf("network create calls = %d, want only interrupted request", got)
	}

	if err := controller.Resume(context.Background(), spec.Name); err != nil {
		t.Fatal(err)
	}
	requireNoOperation(t, controller.State, spec.Name)
	requireDesired(t, controller.State, spec.Name)
}

func TestDifferentUpResolvesPendingCreateThenSupersedes(t *testing.T) {
	base := newFakeEngine()
	fake := &failFirstNetworkCreate{
		fakeEngine: base,
	}
	controller := controllerForEngine(t, fake)
	fake.name = componentNetworkName(controller.dockerScope("demo"), "consumer")
	installImages(
		base,
		"provider:v1", "consumer:v1", "consumer:v2",
		"sha256:provider", "sha256:consumer-v1", "sha256:consumer-v2",
	)
	initial := linkedSpec("provider:v1", "consumer:v1")
	initial.Components[1].Runtime.ExternalEgress = true
	if err := controller.Up(context.Background(), initial); err == nil {
		t.Fatal("initial Up unexpectedly succeeded")
	}
	operation := requireOperation(t, controller.State, initial.Name)
	if !operation.PendingCreates["network/component/consumer"] {
		t.Fatalf("pending network create was not durable: %#v", operation.PendingCreates)
	}

	replacement := linkedSpec("provider:v1", "consumer:v2")
	if err := controller.Up(context.Background(), replacement); err != nil {
		t.Fatalf("replacement Up did not resolve and supersede: %v", err)
	}
	requireNoOperation(t, controller.State, replacement.Name)
	deployed := requireDesired(t, controller.State, replacement.Name)
	consumer := base.containers[deployed.Containers["consumer"].ID]
	if consumer.ImageID != "sha256:consumer-v2" {
		t.Fatalf("replacement consumer image = %q", consumer.ImageID)
	}
}

func TestDifferentUpDoesNotResolvePendingContainerAgainstDeadProxy(t *testing.T) {
	base := newFakeEngine()
	fake := &failFirstContainerCreate{
		fakeEngine: base,
	}
	controller := controllerForEngine(t, fake)
	fake.name = containerName(controller.dockerScope("demo"), "provider")
	installImages(
		base,
		"provider:v1", "consumer:v1", "consumer:v2",
		"sha256:provider", "sha256:consumer-v1", "sha256:consumer-v2",
	)
	initial := linkedSpec("provider:v1", "consumer:v1")
	if err := controller.Up(context.Background(), initial); err == nil {
		t.Fatal("initial Up unexpectedly succeeded")
	}
	interrupted := requireOperation(t, controller.State, initial.Name)
	if !interrupted.PendingCreates["container/provider"] {
		t.Fatalf("pending container create was not durable: %#v", interrupted.PendingCreates)
	}
	manager := controller.Proxy.(*fakeProxyManager)
	manager.mu.Lock()
	delete(manager.processes, interrupted.Proxy.InstanceID)
	delete(manager.configs, interrupted.Proxy.InstanceID)
	manager.mu.Unlock()

	replacement := linkedSpec("provider:v1", "consumer:v2")
	if err := controller.Up(context.Background(), replacement); err != nil {
		t.Fatal(err)
	}
	if got := countEngineCalls(
		base.callsFor("create-container"),
		"create-container",
		fake.name,
	); got != 2 {
		t.Fatalf(
			"provider create calls = %d, want interrupted request plus replacement only",
			got,
		)
	}
	deployed := requireDesired(t, controller.State, replacement.Name)
	if got := base.containers[deployed.Containers["consumer"].ID].ImageID; got != "sha256:consumer-v2" {
		t.Fatalf("replacement consumer image = %q", got)
	}
}

func TestAbortRefusesPendingContainerCreateWithoutChangingOperation(t *testing.T) {
	base := newFakeEngine()
	fake := &failFirstContainerCreate{
		fakeEngine: base,
	}
	controller := controllerForEngine(t, fake)
	fake.name = containerName(controller.dockerScope("demo"), "provider")
	installImages(
		base,
		"provider:v1", "consumer:v1",
		"sha256:provider", "sha256:consumer",
	)
	spec := linkedSpec("provider:v1", "consumer:v1")
	err := controller.Up(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "ambiguous container create") {
		t.Fatalf("Up error = %v", err)
	}
	operation := requireOperation(t, controller.State, spec.Name)
	if !operation.PendingCreates["container/provider"] {
		t.Fatalf("pending container create was not durable: %#v", operation.PendingCreates)
	}
	mutationsBeforeAbort := append([]engineCall(nil), base.mutationCalls()...)

	err = controller.Abort(context.Background(), spec.Name)
	if err == nil ||
		!strings.Contains(err.Error(), "unresolved creates") ||
		!strings.Contains(err.Error(), "dcomp resume demo") {
		t.Fatalf("Abort error = %v", err)
	}
	afterAbort := requireOperation(t, controller.State, spec.Name)
	if !reflect.DeepEqual(afterAbort, operation) {
		t.Fatalf("abort changed operation:\n got: %#v\nwant: %#v", afterAbort, operation)
	}
	requireNoDesired(t, controller.State, spec.Name)
	if mutations := base.mutationCalls(); !reflect.DeepEqual(mutations, mutationsBeforeAbort) {
		t.Fatalf("abort mutated Docker:\n got: %#v\nwant: %#v", mutations, mutationsBeforeAbort)
	}
	if got := countEngineCalls(
		base.callsFor("create-container"),
		"create-container",
		fake.name,
	); got != 1 {
		t.Fatalf("container create calls = %d, want only interrupted request", got)
	}

	if err := controller.Resume(context.Background(), spec.Name); err != nil {
		t.Fatal(err)
	}
	requireNoOperation(t, controller.State, spec.Name)
	requireDesired(t, controller.State, spec.Name)
}

func countEngineCalls(calls []engineCall, method, target string) int {
	count := 0
	for _, call := range calls {
		if call.Method == method && call.Target == target {
			count++
		}
	}
	return count
}

func controllerForEngine(t *testing.T, containerEngine engine.Engine) *Controller {
	t.Helper()
	return &Controller{
		Engine:         containerEngine,
		Proxy:          newFakeProxyManager(),
		State:          state.Store{Root: t.TempDir()},
		RuntimeRoot:    t.TempDir(),
		RequestTimeout: time.Second,
		StopTimeout:    time.Millisecond,
	}
}

var _ engine.Engine = (*failFirstNetworkCreate)(nil)
var _ engine.Engine = (*failFirstContainerCreate)(nil)
