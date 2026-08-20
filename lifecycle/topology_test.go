package lifecycle

import (
	"testing"

	"github.com/glguida/dcomp/composition"
)

func TestResolvedTopologyOnlyCreatesEgressNetworks(t *testing.T) {
	plans, err := resolvedTopology(composition.ResolvedSpec{
		Name: "demo",
		Components: []composition.ResolvedComponent{
			{Name: "isolated"},
			{Name: "online", Runtime: composition.Runtime{ExternalEgress: true}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(plans), 1; got != want {
		t.Fatalf("network plan count = %d, want %d", got, want)
	}
	if _, exists := plans[componentNetworkKey("isolated")]; exists {
		t.Fatal("component without egress received a network plan")
	}
	plan, exists := plans[componentNetworkKey("online")]
	if !exists {
		t.Fatal("component with egress has no network plan")
	}
	if plan.Internal {
		t.Fatal("egress network is internal")
	}
	if _, exists := plan.Members["online"]; !exists {
		t.Fatalf("egress network members = %#v", plan.Members)
	}
}

func TestDockerResourceNamesPreserveTupleBoundaries(t *testing.T) {
	tests := []struct {
		name  string
		left  string
		right string
	}{
		{
			name:  "system and component",
			left:  containerName("a-b", "c"),
			right: containerName("a", "b-c"),
		},
		{
			name:  "component and volume",
			left:  volumeName("demo", "a-b", "c"),
			right: volumeName("demo", "a", "b-c"),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if test.left == test.right {
				t.Fatalf("distinct resource tuples both map to %q", test.left)
			}
		})
	}
}
