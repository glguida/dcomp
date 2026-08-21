package lifecycle

import (
	"strings"
	"testing"

	"github.com/glguida/dcomp/composition"
)

func TestResolvedTopologyOnlyCreatesEgressNetworks(t *testing.T) {
	scope := newDockerScope("/var/lib/dcomp/one", "demo")
	plans, err := resolvedTopology(scope, composition.ResolvedSpec{
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
	root := "/var/lib/dcomp/one"
	tests := []struct {
		name  string
		left  string
		right string
	}{
		{
			name:  "system and component",
			left:  containerName(newDockerScope(root, "a-b"), "c"),
			right: containerName(newDockerScope(root, "a"), "b-c"),
		},
		{
			name:  "component and volume",
			left:  volumeName(newDockerScope(root, "demo"), "a-b", "c"),
			right: volumeName(newDockerScope(root, "demo"), "a", "b-c"),
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

func TestDockerScopeIsStablePerCleanStateRootAndDistinctAcrossRoots(t *testing.T) {
	first := newDockerScope("/var/lib/dcomp/one/../one", "demo")
	alias := newDockerScope("/var/lib/dcomp/one", "demo")
	second := newDockerScope("/var/lib/dcomp/two", "demo")

	if first != alias {
		t.Fatalf("clean aliases produced different scopes: %#v and %#v", first, alias)
	}
	if first.Namespace == second.Namespace {
		t.Fatalf("different state roots share namespace %q", first.Namespace)
	}
	if got := len(first.Namespace); got != dockerNamespaceSize*2 {
		t.Fatalf("namespace length = %d, want %d", got, dockerNamespaceSize*2)
	}
	if containerName(first, "worker") == containerName(second, "worker") {
		t.Fatal("different state roots produced the same container name")
	}
}

func TestDockerResourceNamesFitMaximumDescriptionNames(t *testing.T) {
	maximum := "a" + strings.Repeat("b", 62)
	scope := newDockerScope("/var/lib/dcomp/maximum", maximum)
	for kind, name := range map[string]string{
		"container": containerName(scope, maximum),
		"network":   componentNetworkName(scope, maximum),
		"volume":    volumeName(scope, maximum, maximum),
	} {
		if len(name) > 255 {
			t.Fatalf("maximum %s name has %d bytes: %q", kind, len(name), name)
		}
		if !strings.HasPrefix(name, "dcomp."+scope.Namespace+"."+maximum+".") {
			t.Fatalf("%s name is outside its Docker namespace: %q", kind, name)
		}
	}
}
