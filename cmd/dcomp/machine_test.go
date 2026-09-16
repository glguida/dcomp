package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/glguida/dcomp/engine"
	"github.com/glguida/dcomp/lifecycle"
	"github.com/glguida/dcomp/proxy"
)

func TestWriteVersionJSONIsStableAndSelfDescribing(t *testing.T) {
	var output bytes.Buffer
	if err := writeVersion(&output, true); err != nil {
		t.Fatal(err)
	}
	const want = "{\"version\":\"0.3.1\",\"api_version\":2}\n"
	if got := output.String(); got != want {
		t.Fatalf("version JSON = %q, want %q", got, want)
	}
}

func TestWriteStatusJSONIncludesRetiringResources(t *testing.T) {
	status := lifecycle.Status{
		Name: "demo", Operation: "apply", Phase: "retire",
		RetiringNetworks: []lifecycle.NetworkStatus{{
			Key: "component/worker", ID: "old-network",
		}},
		RetiringComponents: []lifecycle.ComponentStatus{{
			Name: "worker", ID: "old-container", Status: "exited",
		}},
	}
	var output bytes.Buffer
	if err := writeStatusJSON(&output, status); err != nil {
		t.Fatal(err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(output.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"retiring_networks", "retiring_components"} {
		if string(document[field]) == "" || string(document[field]) == "[]" {
			t.Fatalf("status JSON omitted %s: %s", field, output.String())
		}
	}
}

func TestWriteStatusJSONOmitsUnconvergedProxyDigest(t *testing.T) {
	status := lifecycle.Status{
		Name: "demo", Operation: "apply", Phase: "resync",
		Proxy: lifecycle.ProxyStatus{
			InstanceID: "proxy-id", PID: 123, Ready: false,
		},
	}
	var output bytes.Buffer
	if err := writeStatusJSON(&output, status); err != nil {
		t.Fatal(err)
	}
	var document struct {
		Proxy map[string]json.RawMessage `json:"proxy"`
	}
	if err := json.Unmarshal(output.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if _, exists := document.Proxy["digest"]; exists {
		t.Fatalf("unconverged proxy status claims a digest: %s", output.String())
	}
}

func TestWriteStatusJSONReportsEffectivePublishedPorts(t *testing.T) {
	status := lifecycle.Status{
		Name: "demo", Desired: true, Digest: "sha256:system",
		Proxy: lifecycle.ProxyStatus{
			InstanceID: "proxy-id", Digest: "sha256:proxy", PID: 123,
			Ready: true, Inputs: 1, Outputs: 1, ActiveConnections: 2,
			Links: []proxy.LinkMetrics{{
				InputComponent: "service", InputEndpoint: "upstream",
				OutputComponent: "source", OutputEndpoint: "echo",
				ActiveConnections: 2, BytesInputToOutput: 120,
				BytesOutputToInput: 240,
			}},
		},
		Networks: []lifecycle.NetworkStatus{{
			Key: "component/service", ID: "network-id", Internal: false,
		}},
		Components: []lifecycle.ComponentStatus{{
			Name: "service", ID: "container-id", Status: "running",
			Health: engine.HealthHealthy,
			PublishedPorts: []engine.PortBinding{{
				Protocol: engine.ProtocolTCP, HostIP: "127.0.0.1",
				HostPort: 49152, ContainerPort: 8080,
			}},
		}},
	}

	var output bytes.Buffer
	if err := writeStatusJSON(&output, status); err != nil {
		t.Fatal(err)
	}
	const want = "{\"api_version\":2,\"name\":\"demo\",\"desired\":true," +
		"\"operational\":true,\"digest\":\"sha256:system\",\"operation\":\"\"," +
		"\"phase\":\"\",\"proxy\":{\"instance_id\":\"proxy-id\"," +
		"\"digest\":\"sha256:proxy\",\"pid\":123,\"ready\":true," +
		"\"inputs\":1,\"outputs\":1,\"active_connections\":2," +
		"\"links\":[{\"input_component\":\"service\"," +
		"\"input_endpoint\":\"upstream\",\"output_component\":\"source\"," +
		"\"output_endpoint\":\"echo\",\"active_connections\":2," +
		"\"bytes_input_to_output\":120,\"bytes_output_to_input\":240}]," +
		"\"problem\":\"\"}," +
		"\"networks\":[{\"key\":\"component/service\"," +
		"\"id\":\"network-id\",\"internal\":false,\"problem\":\"\"}]," +
		"\"components\":[{\"name\":\"service\",\"container_id\":\"container-id\"," +
		"\"status\":\"running\",\"health\":\"healthy\",\"exit_code\":0," +
		"\"problem\":\"\",\"published_ports\":[{\"protocol\":\"tcp\"," +
		"\"host_ip\":\"127.0.0.1\",\"host_port\":49152," +
		"\"container_port\":8080}]}]}\n"
	if got := output.String(); got != want {
		t.Fatalf("status JSON:\n got: %s\nwant: %s", got, want)
	}
}

func TestWriteProcessesJSONIsStableAndSelfDescribing(t *testing.T) {
	processes := []lifecycle.ComponentProcess{{
		System: "demo", Operation: "apply", Phase: "start",
		ComponentStatus: lifecycle.ComponentStatus{
			Name: "service", ID: "container-id", Status: "running",
			Health: engine.HealthUnhealthy, Problem: "not ready",
			PublishedPorts: []engine.PortBinding{{
				Protocol: engine.ProtocolTCP, HostIP: "127.0.0.1",
				HostPort: 8080, ContainerPort: 80,
			}},
		},
	}}

	var output bytes.Buffer
	if err := writeProcessesJSON(&output, processes); err != nil {
		t.Fatal(err)
	}
	const want = "{\"api_version\":2,\"components\":[{" +
		"\"system\":\"demo\",\"component\":\"service\"," +
		"\"container_id\":\"container-id\",\"status\":\"running\"," +
		"\"health\":\"unhealthy\",\"exit_code\":0," +
		"\"problem\":\"not ready\",\"operation\":\"apply\"," +
		"\"phase\":\"start\",\"published_ports\":[{" +
		"\"protocol\":\"tcp\",\"host_ip\":\"127.0.0.1\"," +
		"\"host_port\":8080,\"container_port\":80}]}]}\n"
	if got := output.String(); got != want {
		t.Fatalf("processes JSON:\n got: %s\nwant: %s", got, want)
	}
}

func TestWriteStatusJSONUsesArraysForAbsentSystem(t *testing.T) {
	var output bytes.Buffer
	if err := writeStatusJSON(&output, lifecycle.Status{Name: "absent"}); err != nil {
		t.Fatal(err)
	}
	const want = "{\"api_version\":2,\"name\":\"absent\",\"desired\":false," +
		"\"operational\":false,\"digest\":\"\",\"operation\":\"\",\"phase\":\"\"," +
		"\"proxy\":{\"instance_id\":\"\",\"pid\":0," +
		"\"ready\":false,\"inputs\":0,\"outputs\":0,\"active_connections\":0," +
		"\"problem\":\"\"},\"networks\":[],\"components\":[]}\n"
	if got := output.String(); got != want {
		t.Fatalf("absent status JSON = %q, want %q", got, want)
	}
}

func TestAbsentStatusDocumentationMatchesMachineJSON(t *testing.T) {
	var output bytes.Buffer
	if err := writeStatusJSON(&output, lifecycle.Status{Name: "absent"}); err != nil {
		t.Fatal(err)
	}
	var generated map[string]any
	if err := json.Unmarshal(output.Bytes(), &generated); err != nil {
		t.Fatal(err)
	}

	documentationPath := filepath.Join("..", "..", "docs", "machine-api.md")
	contents, err := os.ReadFile(documentationPath)
	if err != nil {
		t.Fatal(err)
	}
	const heading = "An absent system is a successful observation with a non-operational result:"
	_, afterHeading, found := strings.Cut(string(contents), heading)
	if !found {
		t.Fatalf("%s has no absent-system example", documentationPath)
	}
	_, afterFence, found := strings.Cut(afterHeading, "```json\n")
	if !found {
		t.Fatalf("%s absent-system example has no JSON fence", documentationPath)
	}
	example, _, found := strings.Cut(afterFence, "\n```")
	if !found {
		t.Fatalf("%s absent-system JSON fence is not closed", documentationPath)
	}
	var documented map[string]any
	if err := json.Unmarshal([]byte(example), &documented); err != nil {
		t.Fatalf("decode documented absent-system JSON: %v", err)
	}
	if !reflect.DeepEqual(documented, generated) {
		t.Fatalf(
			"documented absent-system JSON differs from the machine API:\n documented: %#v\n generated:  %#v",
			documented,
			generated,
		)
	}
}

func TestWriteVolumeJSONIsStableAndSelfDescribing(t *testing.T) {
	var output bytes.Buffer
	if err := writeVolumeJSON(&output, lifecycle.PersistentVolume{
		System: "demo", Component: "worker", LogicalName: "state",
		Name: "dcomp.demo.volume.worker.state",
	}); err != nil {
		t.Fatal(err)
	}
	const want = "{\"api_version\":2,\"system\":\"demo\"," +
		"\"component\":\"worker\",\"logical_name\":\"state\"," +
		"\"name\":\"dcomp.demo.volume.worker.state\"}\n"
	if got := output.String(); got != want {
		t.Fatalf("volume JSON = %q, want %q", got, want)
	}
}
