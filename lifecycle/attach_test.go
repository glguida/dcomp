package lifecycle

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glguida/dcomp/engine"
)

type attachCall struct {
	containerID string
	options     engine.AttachOptions
}

type attachFakeEngine struct {
	*fakeEngine

	mu      sync.Mutex
	calls   []attachCall
	entered chan struct{}
	release chan struct{}
}

func (fake *attachFakeEngine) ContainerAttach(
	ctx context.Context,
	containerID string,
	options engine.AttachOptions,
) error {
	fake.mu.Lock()
	fake.calls = append(fake.calls, attachCall{
		containerID: containerID,
		options:     options,
	})
	fake.mu.Unlock()
	if options.Ready != nil {
		if err := options.Ready(); err != nil {
			return err
		}
	}
	if fake.entered != nil {
		select {
		case fake.entered <- struct{}{}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if fake.release != nil {
		select {
		case <-fake.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (fake *attachFakeEngine) attachCalls() []attachCall {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return append([]attachCall(nil), fake.calls...)
}

func TestAttachVerifiesAndUsesExactRecordedRunningComponent(t *testing.T) {
	controller, base := newControllerHarness(t)
	base.images["provider:v1"] = healthyImage("sha256:provider-v1")
	base.images["consumer:v1"] = healthyImage("sha256:consumer-v1")
	spec := linkedSpec("provider:v1", "consumer:v1")
	if err := controller.Up(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	deployment := requireDesired(t, controller.State, spec.Name)
	fake := &attachFakeEngine{fakeEngine: base}
	controller.Engine = fake
	ready := 0
	stdin := strings.NewReader("input")

	if err := controller.Attach(
		context.Background(),
		spec.Name,
		"consumer",
		engine.AttachOptions{
			Stdin: stdin, Stdout: io.Discard, Stderr: io.Discard,
			Ready: func() error { ready++; return nil },
		},
	); err != nil {
		t.Fatal(err)
	}
	calls := fake.attachCalls()
	if len(calls) != 1 {
		t.Fatalf("attachment calls = %d, want 1", len(calls))
	}
	if calls[0].containerID != deployment.Containers["consumer"].ID {
		t.Fatalf("attached container = %q", calls[0].containerID)
	}
	if calls[0].options.Stdin != stdin || calls[0].options.Stdout != io.Discard ||
		calls[0].options.Stderr != io.Discard {
		t.Fatal("standard streams were not preserved")
	}
	if ready != 1 {
		t.Fatalf("ready called %d times", ready)
	}
}

func TestAttachHoldsGenerationStableAndSerializesComponentInput(t *testing.T) {
	controller, base := newControllerHarness(t)
	base.images["provider:v1"] = healthyImage("sha256:provider-v1")
	base.images["consumer:v1"] = healthyImage("sha256:consumer-v1")
	spec := linkedSpec("provider:v1", "consumer:v1")
	if err := controller.Up(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	fake := &attachFakeEngine{
		fakeEngine: base,
		entered:    make(chan struct{}, 2),
		release:    make(chan struct{}),
	}
	controller.Engine = fake
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- controller.Attach(
			context.Background(),
			spec.Name,
			"consumer",
			engine.AttachOptions{Stdin: strings.NewReader(""), Stdout: io.Discard},
		)
	}()
	<-fake.entered

	if err := controller.Restart(
		context.Background(), spec.Name, "consumer",
	); err == nil || !strings.Contains(err.Error(), "another dcomp operation") {
		t.Fatalf("restart while attached error = %v", err)
	}

	secondCtx, cancelSecond := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancelSecond()
	err := controller.Attach(
		secondCtx,
		spec.Name,
		"consumer",
		engine.AttachOptions{Stdin: strings.NewReader(""), Stdout: io.Discard},
	)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second attachment error = %v", err)
	}
	if calls := len(fake.attachCalls()); calls != 1 {
		t.Fatalf("engine received %d concurrent attachments, want 1", calls)
	}

	close(fake.release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
}

func TestAttachRejectsUnverifiedOrStoppedComponentBeforeEngineAttach(t *testing.T) {
	controller, base := newControllerHarness(t)
	base.images["provider:v1"] = healthyImage("sha256:provider-v1")
	base.images["consumer:v1"] = healthyImage("sha256:consumer-v1")
	spec := linkedSpec("provider:v1", "consumer:v1")
	if err := controller.Up(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	deployment := requireDesired(t, controller.State, spec.Name)
	container := base.containers[deployment.Containers["consumer"].ID]
	container.Running = false
	container.Status = "exited"
	base.replaceContainer(container.ID, container)
	fake := &attachFakeEngine{fakeEngine: base}
	controller.Engine = fake

	err := controller.Attach(
		context.Background(),
		spec.Name,
		"consumer",
		engine.AttachOptions{Stdout: io.Discard},
	)
	if err == nil || !strings.Contains(err.Error(), "is not running") {
		t.Fatalf("stopped component attach error = %v", err)
	}
	if len(fake.attachCalls()) != 0 {
		t.Fatal("engine attach was called for a stopped component")
	}
}

func TestAttachRejectsComponentWithoutCurrentStandardIOPolicy(t *testing.T) {
	controller, base := newControllerHarness(t)
	base.images["provider:v1"] = healthyImage("sha256:provider-v1")
	base.images["consumer:v1"] = healthyImage("sha256:consumer-v1")
	spec := linkedSpec("provider:v1", "consumer:v1")
	if err := controller.Up(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	deployment := requireDesired(t, controller.State, spec.Name)
	container := base.containers[deployment.Containers["consumer"].ID]
	container.OpenStdin = false
	base.replaceContainer(container.ID, container)
	fake := &attachFakeEngine{fakeEngine: base}
	controller.Engine = fake

	err := controller.Attach(
		context.Background(),
		spec.Name,
		"consumer",
		engine.AttachOptions{Stdout: io.Discard},
	)
	if err == nil || !strings.Contains(err.Error(), "standard I/O policy mismatch") {
		t.Fatalf("legacy component attach error = %v", err)
	}
	if len(fake.attachCalls()) != 0 {
		t.Fatal("engine attach was called for a legacy component")
	}
}

var _ engine.AttachEngine = (*attachFakeEngine)(nil)
