package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/glguida/dcomp/composition"
)

func TestViewShowsSymbolicNameAndResolvedOrUnboundOutput(t *testing.T) {
	spec := composition.Spec{Name: "demo", Globals: []composition.Global{{Name: "provider_endpoint", Service: "example.Provider", Target: composition.EndpointRef{Component: "provider", Endpoint: "api"}}},
		Links: []composition.Link{{Input: composition.EndpointRef{Component: "team", Endpoint: "inference"}, Output: composition.EndpointRef{Global: "provider_endpoint"}}}}
	for _, bound := range []bool{true, false} {
		if !bound {
			spec.Globals[0].Target = composition.EndpointRef{}
		}
		document := viewFromSpec(spec)
		if document.Links[0].Global != "provider_endpoint" {
			t.Fatal("symbolic name absent from view")
		}
		if (document.Links[0].Output.Component == "provider") != bound {
			t.Fatal("incorrect concrete target")
		}
		var output bytes.Buffer
		printView(&output, document)
		want := "@provider_endpoint -> provider.api"
		if !bound {
			want = "@provider_endpoint -> unbound"
		}
		if !strings.Contains(output.String(), want) {
			t.Fatalf("text view missing %q: %s", want, &output)
		}
	}
}

func TestIncrementalCommandUsage(t *testing.T) {
	t.Setenv("DCOMP_STATE_ROOT", t.TempDir())
	for _, args := range [][]string{
		{"add-component"}, {"rm-component", "demo"}, {"mod-wire", "demo", "team.input"},
		{"mod-wire", "demo", "@global", "provider.api"}, {"assign-global", "demo", "name", "@another"},
		{"assign-global", "--unknown"},
	} {
		if code := run(args); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}
