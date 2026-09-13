package composition

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGlobalDescriptionsRetainSymbolicTargets(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"provider": "docker provider:1\noutput example.Provider api\n",
		"team":     "docker team:1\ninput example.Provider inference\n",
	} {
		if err := os.Mkdir(filepath.Join(dir, name), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "component.dcomp"), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	text := "system demo\ncomponent provider provider\ncomponent team team\nglobal provider_endpoint example.Provider provider.api\nlink team.inference @provider_endpoint\n"
	spec, err := Parse(strings.NewReader(text), dir)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Links[0].Output != (EndpointRef{Global: "provider_endpoint"}) {
		t.Fatalf("global target was flattened: %#v", spec.Links)
	}
	images := map[string]ResolvedImage{"provider": {ID: "sha256:provider", HasHealthcheck: true}, "team": {ID: "sha256:team", HasHealthcheck: true}}
	before, err := Resolve(spec, images)
	if err != nil {
		t.Fatal(err)
	}
	spec.Globals[0].Target = EndpointRef{}
	after, err := Resolve(spec, images)
	if err != nil {
		t.Fatal(err)
	}
	if before.Digest == after.Digest {
		t.Fatal("unbinding did not change system identity")
	}
	for i := range before.Components {
		if before.Components[i].Digest != after.Components[i].Digest {
			t.Fatal("unbinding changed a container definition")
		}
	}
	if after.Links[0].Output.Global != "provider_endpoint" {
		t.Fatal("unbinding dropped the symbolic reference")
	}
	if _, _, bound := after.LinkTarget("team", "inference"); bound {
		t.Fatal("unbound global resolved")
	}
	if _, _, bound := before.LinkTarget("team", "inference"); !bound {
		t.Fatal("assigned global did not resolve")
	}
}

func TestGlobalValidation(t *testing.T) {
	base := func() Spec {
		return Spec{Name: "demo", Components: []Instance{
			{Name: "provider", Component: Component{Image: "provider", Definition: Definition{Outputs: []Endpoint{{Name: "api", Service: "example.Provider"}}}}},
			{Name: "team", Component: Component{Image: "team", Definition: Definition{Inputs: []Endpoint{{Name: "inference", Service: "example.Provider"}}}}},
		}, Globals: []Global{{Name: "provider_endpoint", Service: "example.Provider", Target: EndpointRef{Component: "provider", Endpoint: "api"}}},
			Links: []Link{{Input: EndpointRef{Component: "team", Endpoint: "inference"}, Output: EndpointRef{Global: "provider_endpoint"}}}}
	}
	for name, change := range map[string]func(*Spec){
		"duplicate global":   func(s *Spec) { s.Globals = append(s.Globals, s.Globals[0]) },
		"unknown global":     func(s *Spec) { s.Links[0].Output.Global = "missing" },
		"wrong type":         func(s *Spec) { s.Globals[0].Service = "example.Other" },
		"unbound wrong type": func(s *Spec) { s.Globals[0].Target = EndpointRef{}; s.Globals[0].Service = "example.Other" },
		"alias chain":        func(s *Spec) { s.Globals[0].Target = EndpointRef{Global: "another"} },
		"mixed reference":    func(s *Spec) { s.Links[0].Output.Component = "provider" },
		"input assignment":   func(s *Spec) { s.Globals[0].Target = s.Links[0].Input },
		"invalid name":       func(s *Spec) { s.Globals[0].Name = "has.dot" },
	} {
		t.Run(name, func(t *testing.T) {
			spec := base()
			change(&spec)
			if err := Validate(spec); err == nil {
				t.Fatal("invalid global configuration accepted")
			}
		})
	}
	spec := base()
	spec.Links = nil
	if err := Validate(spec); err != nil {
		t.Fatalf("unconnected input: %v", err)
	}
	if err := Validate(Spec{Name: "empty"}); err != nil {
		t.Fatalf("empty system: %v", err)
	}
}
