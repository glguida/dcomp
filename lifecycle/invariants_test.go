package lifecycle

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/glguida/dcomp/engine"
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
	operation, err := state.NewOperation(kindApply, phaseCommit, target, &previous)
	if err != nil {
		t.Fatal(err)
	}
	operation.Networks = cloneResources(previous.Networks)
	operation.Containers = cloneResources(previous.Containers)
	if err := controller.State.WriteOperation(target.Name, operation); err != nil {
		t.Fatal(err)
	}
	if err := controller.State.WriteDesired(target.Name, state.Deployment{
		Spec:       target,
		Networks:   cloneResources(operation.Networks),
		Containers: cloneResources(operation.Containers),
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
	operation, err := state.NewOperation(kindApply, phaseCommit, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.State.WriteOperation(target.Name, operation); err != nil {
		t.Fatal(err)
	}
	if err := controller.State.WriteDesired(target.Name, state.Deployment{
		Spec: target, Networks: map[string]state.Resource{},
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
		name:       "dcomp.demo.component.consumer",
	}
	controller := controllerForEngine(t, fake)
	installImages(
		base,
		"provider:v1", "consumer:v1",
		"sha256:provider", "sha256:consumer",
	)
	spec := linkedSpec("provider:v1", "consumer:v1")
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
		name:       "dcomp.demo.component.consumer",
	}
	controller := controllerForEngine(t, fake)
	installImages(
		base,
		"provider:v1", "consumer:v1", "consumer:v2",
		"sha256:provider", "sha256:consumer-v1", "sha256:consumer-v2",
	)
	initial := linkedSpec("provider:v1", "consumer:v1")
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

func TestAbortRefusesPendingContainerCreateWithoutChangingOperation(t *testing.T) {
	base := newFakeEngine()
	fake := &failFirstContainerCreate{
		fakeEngine: base,
		name:       "dcomp.demo.container.provider",
	}
	controller := controllerForEngine(t, fake)
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
		State:          state.Store{Root: t.TempDir()},
		RequestTimeout: time.Second,
		StopTimeout:    time.Millisecond,
	}
}

var _ engine.Engine = (*failFirstNetworkCreate)(nil)
var _ engine.Engine = (*failFirstContainerCreate)(nil)
