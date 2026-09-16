package lifecycle

import (
	"context"
	"testing"
)

func TestComponentUserIsLaunchedVerifiedAndReplaced(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installEditImages(fake, "provider", "consumer")
	spec := linkedSpec("provider:v1", "consumer:v1")
	spec.Components[0].Runtime.User = "1001:1002"
	ctx := context.Background()
	if err := controller.Up(ctx, spec); err != nil {
		t.Fatal(err)
	}
	first := requireDesired(t, controller.State, "demo")
	resource := first.Containers[spec.Components[0].Name]
	actual, err := fake.InspectContainer(ctx, resource.ID)
	if err != nil {
		t.Fatal(err)
	}
	if actual.User != "1001:1002" {
		t.Fatalf("container user = %q", actual.User)
	}
	altered := actual
	altered.User = "0:0"
	fake.replaceContainer(resource.ID, altered)
	if err := controller.Up(ctx, spec); err == nil {
		t.Fatal("accepted changed container user")
	}
	fake.replaceContainer(resource.ID, actual)
	spec.Components[0].Runtime.User = "1003:1004"
	if err := controller.Up(ctx, spec); err != nil {
		t.Fatal(err)
	}
	second := requireDesired(t, controller.State, "demo")
	if second.Containers[spec.Components[0].Name].ID == resource.ID {
		t.Fatal("changed user retained the container")
	}
	if second.Containers[spec.Components[1].Name].ID != first.Containers[spec.Components[1].Name].ID {
		t.Fatal("changed user replaced unrelated container")
	}
}
