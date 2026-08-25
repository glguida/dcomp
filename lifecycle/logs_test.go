package lifecycle

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glguida/dcomp/engine"
	"github.com/glguida/dcomp/proxy"
	"github.com/glguida/dcomp/state"
)

type logCall struct {
	containerID string
	options     engine.LogOptions
}

type logBehavior func(
	context.Context,
	engine.LogOptions,
	func(engine.LogLine) error,
) error

type fakeLogEngine struct {
	*fakeEngine

	logMu        sync.Mutex
	logCalls     []logCall
	logLines     map[string][]engine.LogLine
	logErrors    map[string]error
	logBehaviors map[string]logBehavior
	activeLogs   int
}

type proxyLogManager struct {
	proxy.Manager
	lines []proxy.LogLine
	calls int
}

func (manager *proxyLogManager) Logs(
	ctx context.Context,
	_ proxy.Process,
	_ bool,
	emit func(proxy.LogLine) error,
) error {
	manager.calls++
	for _, line := range manager.lines {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := emit(line); err != nil {
			return err
		}
	}
	return nil
}

func newFakeLogEngine(fake *fakeEngine) *fakeLogEngine {
	return &fakeLogEngine{
		fakeEngine:   fake,
		logLines:     make(map[string][]engine.LogLine),
		logErrors:    make(map[string]error),
		logBehaviors: make(map[string]logBehavior),
	}
}

func (fake *fakeLogEngine) ContainerLogs(
	ctx context.Context,
	containerID string,
	options engine.LogOptions,
	emit func(engine.LogLine) error,
) error {
	fake.logMu.Lock()
	fake.logCalls = append(fake.logCalls, logCall{
		containerID: containerID,
		options:     options,
	})
	lines := append([]engine.LogLine(nil), fake.logLines[containerID]...)
	streamErr := fake.logErrors[containerID]
	behavior := fake.logBehaviors[containerID]
	fake.activeLogs++
	fake.logMu.Unlock()

	defer func() {
		fake.logMu.Lock()
		fake.activeLogs--
		fake.logMu.Unlock()
	}()

	if behavior != nil {
		return behavior(ctx, options, emit)
	}
	for _, line := range lines {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := emit(line); err != nil {
			return err
		}
	}
	return streamErr
}

func (fake *fakeLogEngine) calls() []logCall {
	fake.logMu.Lock()
	defer fake.logMu.Unlock()
	return append([]logCall(nil), fake.logCalls...)
}

func (fake *fakeLogEngine) active() int {
	fake.logMu.Lock()
	defer fake.logMu.Unlock()
	return fake.activeLogs
}

func newLogControllerHarness(t *testing.T) (*Controller, *fakeLogEngine) {
	t.Helper()
	controller, fake := newControllerHarness(t)
	logEngine := newFakeLogEngine(fake)
	controller.Engine = logEngine
	return controller, logEngine
}

func deployLogFixture(t *testing.T) (*Controller, *fakeLogEngine, state.Deployment) {
	t.Helper()
	controller, fake := newLogControllerHarness(t)
	installImages(
		fake.fakeEngine,
		"provider:v1", "consumer:v1",
		"sha256:provider", "sha256:consumer",
	)
	spec := linkedSpec("provider:v1", "consumer:v1")
	if err := controller.Up(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	fake.resetCalls()
	return controller, fake, requireDesired(t, controller.State, spec.Name)
}

func TestLogsSnapshotAggregatesVerifiedContainersWithoutMutation(t *testing.T) {
	controller, fake, deployment := deployLogFixture(t)
	start := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	providerID := deployment.Containers["provider"].ID
	consumerID := deployment.Containers["consumer"].ID
	fake.logLines[providerID] = []engine.LogLine{
		{Timestamp: start, Stream: engine.LogStdout, Message: "provider ready"},
		{
			Timestamp: start.Add(time.Second),
			Stream:    engine.LogStderr,
			Message:   "provider warning",
		},
	}
	fake.logLines[consumerID] = []engine.LogLine{{
		Timestamp: start.Add(2 * time.Second),
		Stream:    engine.LogStdout,
		Message:   "consumer ready",
	}}

	var callbackActive int32
	var callbackOverlap int32
	var records []LogRecord
	err := controller.Logs(
		context.Background(),
		deployment.Spec.Name,
		false,
		func(record LogRecord) error {
			if atomic.AddInt32(&callbackActive, 1) != 1 {
				atomic.StoreInt32(&callbackOverlap, 1)
			}
			records = append(records, record)
			atomic.AddInt32(&callbackActive, -1)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&callbackOverlap) != 0 {
		t.Fatal("log receiver was invoked concurrently")
	}

	got := make(map[LogRecord]int)
	for _, record := range records {
		got[record]++
	}
	want := map[LogRecord]int{
		{
			Component: "provider",
			Line: engine.LogLine{
				Timestamp: start,
				Stream:    engine.LogStdout,
				Message:   "provider ready",
			},
		}: 1,
		{
			Component: "provider",
			Line: engine.LogLine{
				Timestamp: start.Add(time.Second),
				Stream:    engine.LogStderr,
				Message:   "provider warning",
			},
		}: 1,
		{
			Component: "consumer",
			Line: engine.LogLine{
				Timestamp: start.Add(2 * time.Second),
				Stream:    engine.LogStdout,
				Message:   "consumer ready",
			},
		}: 1,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("records:\n got: %#v\nwant: %#v", got, want)
	}
	requireLogCalls(t, fake, false, providerID, consumerID)
	if mutations := fake.mutationCalls(); len(mutations) != 0 {
		t.Fatalf("Logs mutated Docker: %#v", mutations)
	}
}

func TestLogsFiltersExactRecordedComponentNames(t *testing.T) {
	controller, fake, deployment := deployLogFixture(t)
	providerID := deployment.Containers["provider"].ID
	consumerID := deployment.Containers["consumer"].ID
	fake.logLines[providerID] = []engine.LogLine{{
		Stream: engine.LogStdout, Message: "provider output",
	}}
	fake.logLines[consumerID] = []engine.LogLine{{
		Stream: engine.LogStdout, Message: "consumer output",
	}}

	var records []LogRecord
	if err := controller.Logs(
		context.Background(),
		deployment.Spec.Name,
		false,
		func(record LogRecord) error {
			records = append(records, record)
			return nil
		},
		"consumer",
	); err != nil {
		t.Fatal(err)
	}
	if want := []LogRecord{{
		Component: "consumer",
		Line: engine.LogLine{
			Stream: engine.LogStdout, Message: "consumer output",
		},
	}}; !reflect.DeepEqual(records, want) {
		t.Fatalf("filtered records = %#v, want %#v", records, want)
	}
	requireLogCalls(t, fake, false, consumerID)
}

func TestLogsCanSelectOnlyProxy(t *testing.T) {
	controller, fake, deployment := deployLogFixture(t)
	timestamp := time.Date(2026, time.August, 20, 12, 0, 0, 0, time.UTC)
	manager := &proxyLogManager{
		Manager: controller.Proxy,
		lines: []proxy.LogLine{{
			Timestamp: timestamp,
			Message:   "proxy ready",
		}},
	}
	controller.Proxy = manager

	var records []LogRecord
	if err := controller.Logs(
		context.Background(),
		deployment.Spec.Name,
		false,
		func(record LogRecord) error {
			records = append(records, record)
			return nil
		},
		ProxyLogSource,
	); err != nil {
		t.Fatal(err)
	}
	want := []LogRecord{{
		Component: ProxyLogSource,
		Line: engine.LogLine{
			Timestamp: timestamp,
			Stream:    engine.LogStderr,
			Message:   "proxy ready",
		},
	}}
	if !reflect.DeepEqual(records, want) {
		t.Fatalf("proxy records = %#v, want %#v", records, want)
	}
	if manager.calls != 1 {
		t.Fatalf("proxy log calls = %d, want 1", manager.calls)
	}
	if calls := fake.calls(); len(calls) != 0 {
		t.Fatalf("proxy-only logs opened component streams: %#v", calls)
	}
}

func TestLogsRejectsUnknownOrDuplicateComponentBeforeDockerAccess(t *testing.T) {
	tests := []struct {
		name       string
		components []string
		message    string
	}{
		{
			name: "unknown", components: []string{"provid"},
			message: `component "provid" is not present`,
		},
		{
			name: "duplicate", components: []string{"provider", "provider"},
			message: `component "provider" was requested more than once`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			controller, fake, deployment := deployLogFixture(t)
			err := controller.Logs(
				context.Background(),
				deployment.Spec.Name,
				false,
				func(LogRecord) error { return nil },
				test.components...,
			)
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("Logs error = %v, want substring %q", err, test.message)
			}
			if calls := fake.calls(); len(calls) != 0 {
				t.Fatalf("invalid filter accessed Docker logs: %#v", calls)
			}
			if mutations := fake.mutationCalls(); len(mutations) != 0 {
				t.Fatalf("invalid filter mutated Docker: %#v", mutations)
			}
		})
	}
}

func TestLogsRefusesUnverifiedContainerBeforeOpeningAnyStream(t *testing.T) {
	tests := []struct {
		name      string
		change    func(*engine.Container)
		wantError string
	}{
		{
			name: "foreign ownership",
			change: func(container *engine.Container) {
				container.Labels[LabelOwner] = "somebody-else"
			},
			wantError: "not the expected owned provider component",
		},
		{
			name: "changed immutable image",
			change: func(container *engine.Container) {
				container.ImageID = "sha256:foreign"
			},
			wantError: "uses image sha256:foreign",
		},
		{
			name: "undeclared network attachment",
			change: func(container *engine.Container) {
				container.Networks["foreign"] = engine.NetworkAttachment{
					NetworkID: "foreign-network",
				}
			},
			wantError: "networks",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			controller, fake, deployment := deployLogFixture(t)
			providerID := deployment.Containers["provider"].ID
			fake.mu.Lock()
			container := fake.containers[providerID]
			test.change(&container)
			fake.containers[providerID] = container
			fake.mu.Unlock()

			err := controller.Logs(
				context.Background(),
				deployment.Spec.Name,
				false,
				func(LogRecord) error { return nil },
			)
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("Logs error = %v, want substring %q", err, test.wantError)
			}
			if calls := fake.calls(); len(calls) != 0 {
				t.Fatalf("logs opened before every source was verified: %#v", calls)
			}
			if mutations := fake.mutationCalls(); len(mutations) != 0 {
				t.Fatalf("Logs mutated invalid deployment: %#v", mutations)
			}
		})
	}
}

func TestLogsCanReadFailedComponentOutput(t *testing.T) {
	controller, fake, deployment := deployLogFixture(t)
	consumerID := deployment.Containers["consumer"].ID
	fake.mu.Lock()
	consumer := fake.containers[consumerID]
	consumer.Status = "exited"
	consumer.Running = false
	consumer.Health = engine.HealthNone
	consumer.ExitCode = 75
	fake.containers[consumerID] = consumer
	fake.mu.Unlock()
	fake.logLines[consumerID] = []engine.LogLine{{
		Timestamp: time.Now(),
		Stream:    engine.LogStderr,
		Message:   "component failed",
	}}

	var sawFailure bool
	if err := controller.Logs(
		context.Background(),
		deployment.Spec.Name,
		false,
		func(record LogRecord) error {
			if record.Component == "consumer" &&
				record.Line.Message == "component failed" {
				sawFailure = true
			}
			return nil
		},
	); err != nil {
		t.Fatal(err)
	}
	if !sawFailure {
		t.Fatal("failed component output was not available")
	}
}

func TestLogsCanDiagnoseComponentWithNonconformingStandardIOPolicy(t *testing.T) {
	controller, fake, deployment := deployLogFixture(t)
	providerID := deployment.Containers["provider"].ID
	fake.mu.Lock()
	provider := fake.containers[providerID]
	provider.OpenStdin = false
	fake.containers[providerID] = provider
	fake.mu.Unlock()
	fake.logLines[providerID] = []engine.LogLine{{
		Timestamp: time.Now(),
		Stream:    engine.LogStderr,
		Message:   "policy diagnostic",
	}}

	var records []LogRecord
	if err := controller.Logs(
		context.Background(),
		deployment.Spec.Name,
		false,
		func(record LogRecord) error {
			records = append(records, record)
			return nil
		},
		"provider",
	); err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Line.Message != "policy diagnostic" {
		t.Fatalf("nonconforming component records = %#v", records)
	}
}

func TestLogsFollowCancellationStopsEveryReader(t *testing.T) {
	controller, fake, deployment := deployLogFixture(t)
	started := make(chan string, len(deployment.Containers))
	for _, resource := range deployment.Containers {
		id := resource.ID
		fake.logBehaviors[id] = func(
			ctx context.Context,
			options engine.LogOptions,
			_ func(engine.LogLine) error,
		) error {
			if !options.Follow {
				return errors.New("expected follow mode")
			}
			started <- id
			<-ctx.Done()
			return ctx.Err()
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- controller.Logs(ctx, deployment.Spec.Name, true, func(LogRecord) error {
			return nil
		})
	}()
	awaitReaders(t, started, len(deployment.Containers))
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Logs error = %v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Logs did not return after cancellation")
	}
	if active := fake.active(); active != 0 {
		t.Fatalf("%d log readers remain active", active)
	}
}

func TestLogsReceiverErrorCancelsAndJoinsEveryReader(t *testing.T) {
	controller, fake, deployment := deployLogFixture(t)
	providerID := deployment.Containers["provider"].ID
	consumerID := deployment.Containers["consumer"].ID
	started := make(chan string, 2)
	release := make(chan struct{})
	receiverErr := errors.New("receiver stopped")

	fake.logBehaviors[providerID] = func(
		_ context.Context,
		_ engine.LogOptions,
		emit func(engine.LogLine) error,
	) error {
		started <- providerID
		<-release
		return emit(engine.LogLine{
			Timestamp: time.Now(), Stream: engine.LogStdout, Message: "one line",
		})
	}
	fake.logBehaviors[consumerID] = func(
		ctx context.Context,
		_ engine.LogOptions,
		_ func(engine.LogLine) error,
	) error {
		started <- consumerID
		<-release
		<-ctx.Done()
		return ctx.Err()
	}

	result := make(chan error, 1)
	go func() {
		result <- controller.Logs(
			context.Background(),
			deployment.Spec.Name,
			true,
			func(LogRecord) error { return receiverErr },
		)
	}()
	awaitReaders(t, started, 2)
	close(release)

	select {
	case err := <-result:
		if !errors.Is(err, receiverErr) {
			t.Fatalf("Logs error = %v, want receiver error", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Logs did not return after receiver failure")
	}
	if active := fake.active(); active != 0 {
		t.Fatalf("%d log readers remain active", active)
	}
}

func TestLogsRejectsNilReceiverAndUnsupportedEngine(t *testing.T) {
	controller, _, deployment := deployLogFixture(t)
	if err := controller.Logs(
		context.Background(), deployment.Spec.Name, false, nil,
	); err == nil || !strings.Contains(err.Error(), "receiver is nil") {
		t.Fatalf("nil receiver error = %v", err)
	}

	plainController, _ := newControllerHarness(t)
	if err := plainController.Logs(
		context.Background(), "demo", false, func(LogRecord) error { return nil },
	); err == nil || !strings.Contains(err.Error(), "does not support logs") {
		t.Fatalf("unsupported engine error = %v", err)
	}
}

func requireLogCalls(
	t *testing.T,
	fake *fakeLogEngine,
	follow bool,
	wantIDs ...string,
) {
	t.Helper()
	calls := fake.calls()
	gotIDs := make([]string, 0, len(calls))
	for _, call := range calls {
		if call.options.Follow != follow {
			t.Fatalf(
				"ContainerLogs(%s) follow = %t, want %t",
				call.containerID,
				call.options.Follow,
				follow,
			)
		}
		gotIDs = append(gotIDs, call.containerID)
	}
	sort.Strings(gotIDs)
	sort.Strings(wantIDs)
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("log container IDs:\n got: %#v\nwant: %#v", gotIDs, wantIDs)
	}
}

func awaitReaders(t *testing.T, started <-chan string, count int) {
	t.Helper()
	seen := make(map[string]struct{}, count)
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for len(seen) < count {
		select {
		case id := <-started:
			seen[id] = struct{}{}
		case <-deadline.C:
			t.Fatalf("only %d of %d log readers started", len(seen), count)
		}
	}
}
