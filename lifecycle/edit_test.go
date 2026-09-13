package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/glguida/dcomp/composition"
)

func installEditImages(fake *fakeEngine, names ...string) {
	for _, name := range names {
		image := healthyImage("sha256:" + name)
		fake.images[name+":v1"] = image
		fake.images[image.ID] = image
	}
}

func TestInitialGlobalSystemAndProxyReplacementRetainNames(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installEditImages(fake, "provider", "consumer")
	spec := linkedSpec("provider:v1", "consumer:v1")
	spec.Globals = []composition.Global{{Name: "provider_endpoint", Service: echoService, Target: spec.Links[0].Output}}
	spec.Links[0].Output = composition.EndpointRef{Global: "provider_endpoint"}
	if err := controller.Up(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	before := requireDesired(t, controller.State, "demo")
	manager := controller.Proxy.(*fakeProxyManager)
	if len(manager.configs[before.Proxy.InstanceID].Globals) != 1 {
		t.Fatal("initial proxy lost global assignments")
	}
	manager.mu.Lock()
	delete(manager.processes, before.Proxy.InstanceID)
	delete(manager.configs, before.Proxy.InstanceID)
	manager.mu.Unlock()
	if err := controller.Up(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	after := requireDesired(t, controller.State, "demo")
	if after.Proxy.InstanceID == before.Proxy.InstanceID {
		t.Fatal("dead proxy not replaced")
	}
	config := manager.configs[after.Proxy.InstanceID]
	if len(config.Globals) != 1 || config.Links[0].Global != "provider_endpoint" {
		t.Fatalf("replacement lost symbolic wiring: %#v", config)
	}
}

func TestIncrementalProviderPipelineLifecycle(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installEditImages(fake, "provider", "consumer", "wrapper")
	ctx := context.Background()
	if err := controller.AddComponent(ctx, "demo", component("provider", "provider:v1", nil, []string{"echo"}), nil); err != nil {
		t.Fatal(err)
	}
	provider := composition.EndpointRef{Component: "provider", Endpoint: "echo"}
	if err := controller.AssignGlobal(ctx, "demo", "provider_endpoint", "", provider); err != nil {
		t.Fatal(err)
	}
	consumer := component("consumer", "consumer:v1", []string{"upstream"}, nil)
	input := composition.EndpointRef{Component: "consumer", Endpoint: "upstream"}
	if err := controller.AddComponent(ctx, "demo", consumer, []composition.Link{{Input: input, Output: composition.EndpointRef{Global: "provider_endpoint"}}}); err != nil {
		t.Fatal(err)
	}
	before := requireDesired(t, controller.State, "demo")
	// A moved image tag must not replace the already running provider.
	fake.images["provider:v1"] = healthyImage("sha256:changed-tag")
	wrapper := component("wrapper", "wrapper:v1", []string{"upstream"}, []string{"echo"})
	if err := controller.AddComponent(ctx, "demo", wrapper, []composition.Link{link("wrapper", "upstream", "provider", "echo")}); err != nil {
		t.Fatal(err)
	}
	if err := controller.AssignGlobal(ctx, "demo", "provider_endpoint", "", composition.EndpointRef{Component: "wrapper", Endpoint: "echo"}); err != nil {
		t.Fatal(err)
	}
	after := requireDesired(t, controller.State, "demo")
	for name, resource := range before.Containers {
		if after.Containers[name].ID != resource.ID {
			t.Fatalf("unrelated %s was recreated", name)
		}
	}
	if after.Proxy.InstanceID != before.Proxy.InstanceID {
		t.Fatal("proxy was replaced")
	}
	for _, link := range after.Spec.Links {
		if link.Input.Component == "consumer" && link.Output.Global != "provider_endpoint" {
			t.Fatal("consumer no longer follows name")
		}
		if link.Input.Component == "wrapper" && link.Output != provider {
			t.Fatal("wrapper did not retain the concrete upstream")
		}
	}
	if err := controller.RemoveComponent(ctx, "demo", "wrapper"); err != nil {
		t.Fatal(err)
	}
	removed := requireDesired(t, controller.State, "demo")
	if removed.Spec.Globals[0].Target != (composition.EndpointRef{}) || removed.Spec.Globals[0].Name != "provider_endpoint" {
		t.Fatal("removal did not retain an unbound global")
	}
	if removed.Spec.Links[0].Output.Global != "provider_endpoint" {
		t.Fatal("removal dropped consumer's global reference")
	}
	if removed.Containers["consumer"].ID != before.Containers["consumer"].ID {
		t.Fatal("removal recreated consumer")
	}
	if err := controller.AssignGlobal(ctx, "demo", "provider_endpoint", "", provider); err != nil {
		t.Fatal(err)
	}
	if err := controller.ModifyWire(ctx, "demo", input, provider); err != nil {
		t.Fatal(err)
	}
	if err := controller.RemoveComponent(ctx, "demo", "provider"); err != nil {
		t.Fatal(err)
	}
	if len(requireDesired(t, controller.State, "demo").Spec.Links) != 0 {
		t.Fatal("removed provider retained direct incoming wire")
	}
	if err := controller.RemoveComponent(ctx, "demo", "consumer"); err != nil {
		t.Fatal(err)
	}
	empty := requireDesired(t, controller.State, "demo")
	if len(empty.Containers) != 0 || len(empty.Spec.Globals) != 1 {
		t.Fatalf("last component removal: %#v", empty)
	}
	// An empty system can be repopulated, retaining the global namespace.
	if err := controller.AddComponent(ctx, "demo", component("provider", "sha256:provider", nil, []string{"echo"}), nil); err != nil {
		t.Fatal(err)
	}
	if err := controller.AssignGlobal(ctx, "demo", "provider_endpoint", "", provider); err != nil {
		t.Fatal(err)
	}
	if err := controller.Down(ctx, "demo"); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentIncrementalAddsDoNotLoseContributions(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installEditImages(fake, "provider")
	ctx := context.Background()
	const count = 8
	results := make(chan error, count)
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("worker-%d", i)
		go func() {
			results <- controller.AddComponent(ctx, "demo", component(name, "provider:v1", nil, []string{"echo"}), nil)
		}()
	}
	for i := 0; i < count; i++ {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	deployment := requireDesired(t, controller.State, "demo")
	if len(deployment.Spec.Components) != count || len(deployment.Containers) != count {
		t.Fatalf("lost concurrent contribution: %#v", deployment.Containers)
	}
}

func TestInterruptedAddResumesExactTargetAndRejectsOtherEdits(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installEditImages(fake, "provider", "wrapper")
	ctx := context.Background()
	if err := controller.AddComponent(ctx, "demo", component("provider", "provider:v1", nil, []string{"echo"}), nil); err != nil {
		t.Fatal(err)
	}
	before := requireDesired(t, controller.State, "demo")
	fake.startFailures["wrapper"] = []error{errors.New("injected start failure")}
	if err := controller.AddComponent(ctx, "demo", component("wrapper", "wrapper:v1", nil, []string{"echo"}), nil); err == nil {
		t.Fatal("injected failure did not interrupt add")
	}
	if err := controller.RemoveComponent(ctx, "demo", "provider"); err == nil {
		t.Fatal("edit overwrote pending operation")
	}
	if err := controller.Resume(ctx, "demo"); err != nil {
		t.Fatal(err)
	}
	after := requireDesired(t, controller.State, "demo")
	if len(after.Containers) != 2 || after.Containers["provider"] != before.Containers["provider"] {
		t.Fatalf("resume changed unrelated contribution: %#v", after.Containers)
	}
}

func TestInvalidIncrementalEditsLeaveDesiredUnchanged(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installEditImages(fake, "provider", "consumer")
	ctx := context.Background()
	if err := controller.Up(ctx, linkedSpec("provider:v1", "consumer:v1")); err != nil {
		t.Fatal(err)
	}
	if err := controller.AssignGlobal(ctx, "demo", "provider_endpoint", "", composition.EndpointRef{Component: "provider", Endpoint: "echo"}); err != nil {
		t.Fatal(err)
	}
	before := requireDesired(t, controller.State, "demo")
	for _, edit := range []func() error{
		func() error {
			return controller.AddComponent(ctx, "demo", component("provider", "provider:v1", nil, nil), nil)
		},
		func() error { return controller.RemoveComponent(ctx, "demo", "missing") },
		func() error {
			return controller.ModifyWire(ctx, "demo", composition.EndpointRef{Component: "consumer", Endpoint: "upstream"}, composition.EndpointRef{Global: "missing"})
		},
		func() error {
			return controller.AssignGlobal(ctx, "demo", "provider_endpoint", "example.Other", composition.EndpointRef{})
		},
		func() error {
			return controller.AssignGlobal(ctx, "demo", "provider_endpoint", "", composition.EndpointRef{Global: "other"})
		},
	} {
		if err := edit(); err == nil {
			t.Fatal("invalid edit succeeded")
		}
		if got := requireDesired(t, controller.State, "demo"); !reflect.DeepEqual(got, before) {
			t.Fatal("invalid edit changed desired state")
		}
		requireNoOperation(t, controller.State, "demo")
	}
}
