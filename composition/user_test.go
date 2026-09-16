package composition

import (
	"strings"
	"testing"
)

func TestUserRuntimePolicy(t *testing.T) {
	root := t.TempDir()
	writeComponent(t, root, "worker", "docker worker:dev")
	spec, err := Parse(strings.NewReader("system demo\ncomponent worker worker\nuser worker 1001:1002\n"), root)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Components[0].Runtime.User != "1001:1002" {
		t.Fatal("user was not preserved")
	}
	images := map[string]ResolvedImage{"worker": {ID: "sha256:worker", HasHealthcheck: true}}
	first, err := Resolve(spec, images)
	if err != nil {
		t.Fatal(err)
	}
	if first.Authored().Components[0].Runtime.User != "1001:1002" {
		t.Fatal("user lost when editing")
	}
	spec.Components[0].Runtime.User = "1002:1002"
	second, err := Resolve(spec, images)
	if err != nil {
		t.Fatal(err)
	}
	if first.Components[0].Digest == second.Components[0].Digest {
		t.Fatal("changing the user must replace the container")
	}
	for _, user := range []string{"1000", "node:node", "-1:1000", "1:-1", "01:1", "1:2:3", "4294967295:1"} {
		if err := ValidateRuntime(Runtime{User: user}); err == nil {
			t.Errorf("accepted invalid user %q", user)
		}
	}
	for _, suffix := range []string{"user worker 1:1\nuser worker 2:2", "user absent 1:1", "user worker"} {
		if _, err := Parse(strings.NewReader("system demo\ncomponent worker worker\n"+suffix), root); err == nil {
			t.Errorf("accepted %q", suffix)
		}
	}
}
