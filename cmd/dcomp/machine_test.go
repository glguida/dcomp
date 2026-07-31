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
	const want = "{\"version\":\"0.1.0\",\"api_version\":1}\n"
	if got := output.String(); got != want {
		t.Fatalf("version JSON = %q, want %q", got, want)
	}
}

func TestWriteStatusJSONReportsEffectivePublishedPorts(t *testing.T) {
	status := lifecycle.Status{
		Name: "demo", Desired: true, Digest: "sha256:system",
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
	const want = "{\"api_version\":1,\"name\":\"demo\",\"desired\":true," +
		"\"operational\":true,\"digest\":\"sha256:system\",\"operation\":\"\"," +
		"\"phase\":\"\",\"networks\":[{\"key\":\"component/service\"," +
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

func TestWriteStatusJSONUsesArraysForAbsentSystem(t *testing.T) {
	var output bytes.Buffer
	if err := writeStatusJSON(&output, lifecycle.Status{Name: "absent"}); err != nil {
		t.Fatal(err)
	}
	const want = "{\"api_version\":1,\"name\":\"absent\",\"desired\":false," +
		"\"operational\":false,\"digest\":\"\",\"operation\":\"\",\"phase\":\"\"," +
		"\"networks\":[],\"components\":[]}\n"
	if got := output.String(); got != want {
		t.Fatalf("absent status JSON = %q, want %q", got, want)
	}
}
