package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glguida/dcomp/composition"
	"github.com/glguida/dcomp/engine"
	"github.com/glguida/dcomp/lifecycle"
	"github.com/glguida/dcomp/proxy"
)

func writeViewFixture(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	source := filepath.Join(base, "components", "source")
	filter := filepath.Join(base, "components", "filter")
	for directory, manifest := range map[string]string{
		source: "docker example/source:1\n" +
			"output example.document.v1.Documents documents\n",
		filter: "docker example/filter:1\n" +
			"input example.document.v1.Documents documents\n" +
			"output example.document.v1.Documents filtered\n",
	} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(directory, "component.dcomp")
		if err := os.WriteFile(path, []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	system := filepath.Join(base, "system.dcomp")
	content := "system document-system\n" +
		"component source components/source\n" +
		"component filter components/filter\n" +
		"egress filter\n" +
		"link filter.documents source.documents\n"
	if err := os.WriteFile(system, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return system
}

func TestViewFileModeEmitsTopologyWithoutDocker(t *testing.T) {
	system := writeViewFixture(t)
	root := t.TempDir()
	t.Setenv("DCOMP_STATE_ROOT", root)
	t.Setenv("DOCKER_HOST", "unix://"+filepath.Join(root, "missing.sock"))

	output, code := captureStdout(t, func() int {
		return run([]string{"view", "--json", system})
	})
	if code != 0 {
		t.Fatalf("view exit code = %d, want 0", code)
	}
	var document viewDocument
	if err := json.Unmarshal([]byte(output), &document); err != nil {
		t.Fatalf("view output is not JSON: %v", err)
	}
	if document.APIVersion != apiVersion || document.Source != "file" {
		t.Fatalf("unexpected header: %+v", document)
	}
	if document.Name != "document-system" {
		t.Fatalf("system name = %q", document.Name)
	}
	if len(document.Components) != 2 {
		t.Fatalf("components = %d, want 2", len(document.Components))
	}
	if document.Components[0].Name != "filter" || !document.Components[0].Egress {
		t.Fatalf("first component = %+v", document.Components[0])
	}
	if document.Components[0].Status != nil {
		t.Fatalf("file mode must not report status")
	}
	if len(document.Links) != 1 {
		t.Fatalf("links = %d, want 1", len(document.Links))
	}
	link := document.Links[0]
	if link.Service != "example.document.v1.Documents" ||
		link.Input.Component != "filter" || link.Output.Component != "source" {
		t.Fatalf("link = %+v", link)
	}
	if link.Active != nil || link.ActiveConnections != nil || link.Activity != nil {
		t.Fatalf("file mode invented runtime link facts: %+v", link)
	}
}

func TestViewMissingFileWithPathSyntaxFails(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DCOMP_STATE_ROOT", root)
	t.Setenv("DOCKER_HOST", "unix://"+filepath.Join(root, "missing.sock"))
	if code := run([]string{"view", filepath.Join(root, "absent.dcomp")}); code != 1 {
		t.Fatalf("view exit code = %d, want 1", code)
	}
}

func TestViewAbsentSystemReportsEmptyStateDocument(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DCOMP_STATE_ROOT", root)
	t.Setenv("DOCKER_HOST", "unix://"+filepath.Join(root, "missing.sock"))

	output, code := captureStdout(t, func() int {
		return run([]string{"view", "--json", "ghost"})
	})
	if code != 0 {
		t.Fatalf("view exit code = %d, want 0", code)
	}
	var document viewDocument
	if err := json.Unmarshal([]byte(output), &document); err != nil {
		t.Fatal(err)
	}
	if document.Source != "state" || document.Desired || document.Operational {
		t.Fatalf("absent system document = %+v", document)
	}
	if len(document.Components) != 0 || len(document.Links) != 0 {
		t.Fatalf("absent system must have empty topology: %+v", document)
	}
}

func TestViewFromStatusJoinsTopologyAndObservation(t *testing.T) {
	spec := composition.ResolvedSpec{
		Name:   "demo",
		Digest: "sha256:feed",
		Components: []composition.ResolvedComponent{
			{
				Name:     "filter",
				ImageRef: "example/filter:1",
				ImageID:  "sha256:abcd",
				Definition: composition.Definition{
					Inputs: []composition.Endpoint{
						{Name: "documents", Service: "example.document.v1.Documents"},
					},
					Outputs: []composition.Endpoint{
						{Name: "filtered", Service: "example.document.v1.Documents"},
					},
				},
			},
			{
				Name:     "source",
				ImageRef: "example/source:1",
				ImageID:  "sha256:bcde",
				Definition: composition.Definition{
					Outputs: []composition.Endpoint{
						{Name: "documents", Service: "example.document.v1.Documents"},
					},
				},
			},
		},
		Links: []composition.Link{
			{
				Input:  composition.EndpointRef{Component: "filter", Endpoint: "documents"},
				Output: composition.EndpointRef{Component: "source", Endpoint: "documents"},
			},
		},
	}
	status := lifecycle.Status{
		Name:    "demo",
		Desired: true,
		Digest:  spec.Digest,
		Spec:    &spec,
		Proxy: lifecycle.ProxyStatus{
			Ready: true, Inputs: 1, Outputs: 2, ActiveConnections: 3,
			Links: []proxy.LinkMetrics{{
				InputComponent: "filter", InputEndpoint: "documents",
				OutputComponent: "source", OutputEndpoint: "documents",
				ActiveConnections:  1,
				BytesInputToOutput: 125, BytesOutputToInput: 250,
			}},
		},
		Components: []lifecycle.ComponentStatus{
			{Name: "filter", ID: "c1", Status: "running", Health: engine.HealthHealthy},
			{Name: "source", ID: "c2", Status: "exited", ExitCode: 3},
		},
	}

	document := viewFromStatus(status)
	if document.Source != "state" || !document.Desired {
		t.Fatalf("document header = %+v", document)
	}
	if document.Proxy == nil || !document.Proxy.Ready ||
		document.Proxy.Inputs != 1 || document.Proxy.Outputs != 2 ||
		document.Proxy.ActiveConnections != 3 {
		t.Fatalf("proxy = %+v", document.Proxy)
	}
	if len(document.Components) != 2 {
		t.Fatalf("components = %d", len(document.Components))
	}
	filter := document.Components[0]
	if filter.Name != "filter" || filter.Status == nil ||
		filter.Status.Status != "running" ||
		filter.Status.Health != string(engine.HealthHealthy) {
		t.Fatalf("filter = %+v", filter)
	}
	source := document.Components[1]
	if source.Status == nil || source.Status.Status != "exited" ||
		source.Status.ExitCode != 3 {
		t.Fatalf("source = %+v", source)
	}
	if len(document.Links) != 1 ||
		document.Links[0].Service != "example.document.v1.Documents" {
		t.Fatalf("links = %+v", document.Links)
	}
	link := document.Links[0]
	if link.Active == nil || !*link.Active ||
		link.ActiveConnections == nil || *link.ActiveConnections != 1 ||
		link.Activity == nil || link.Activity.BytesInputToOutput != 125 ||
		link.Activity.BytesOutputToInput != 250 {
		t.Fatalf("link observation = %+v", link)
	}
}

func TestPrintViewListsWiring(t *testing.T) {
	system := writeViewFixture(t)
	root := t.TempDir()
	t.Setenv("DCOMP_STATE_ROOT", root)
	t.Setenv("DOCKER_HOST", "unix://"+filepath.Join(root, "missing.sock"))

	output, code := captureStdout(t, func() int {
		return run([]string{"view", system})
	})
	if code != 0 {
		t.Fatalf("view exit code = %d, want 0", code)
	}
	for _, want := range []string{
		"document-system source=file",
		"filter\texample/filter:1\tegress",
		"filter.documents\tsource.documents\texample.document.v1.Documents",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("view output missing %q:\n%s", want, output)
		}
	}
}
