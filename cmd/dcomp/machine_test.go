package main

import (
	"bytes"
	"testing"

	"github.com/glguida/dcomp/engine"
	"github.com/glguida/dcomp/lifecycle"
)

func TestWriteVersionJSONIsStableAndSelfDescribing(t *testing.T) {
	var output bytes.Buffer
	if err := writeVersion(&output, true); err != nil {
		t.Fatal(err)
	}
	const want = "{\"version\":\"0.2.0\",\"api_version\":2}\n"
	if got := output.String(); got != want {
		t.Fatalf("version JSON = %q, want %q", got, want)
	}
}

func TestWriteStatusJSONReportsEffectivePublishedPorts(t *testing.T) {
	status := lifecycle.Status{
		Name: "demo", Desired: true, Digest: "sha256:system",
		Proxy: lifecycle.ProxyStatus{
			InstanceID: "proxy-id", Digest: "sha256:proxy", PID: 123,
			Ready: true, Inputs: 1, Outputs: 1, ActiveConnections: 2,
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
		"\"inputs\":1,\"outputs\":1,\"active_connections\":2,\"problem\":\"\"}," +
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
		"\"proxy\":{\"instance_id\":\"\",\"digest\":\"\",\"pid\":0," +
		"\"ready\":false,\"inputs\":0,\"outputs\":0,\"active_connections\":0," +
		"\"problem\":\"\"},\"networks\":[],\"components\":[]}\n"
	if got := output.String(); got != want {
		t.Fatalf("absent status JSON = %q, want %q", got, want)
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
