package dockerengine

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/glguida/dcomp/engine"
)

func TestNewFromEnvironmentRejectsRemoteAndInvalidSockets(t *testing.T) {
	tests := []struct {
		name string
		host string
	}{
		{name: "TCP", host: "tcp://127.0.0.1:2375"},
		{name: "HTTP", host: "http://127.0.0.1:2375"},
		{name: "SSH", host: "ssh://docker@example.invalid"},
		{name: "Windows named pipe", host: "npipe:////./pipe/docker_engine"},
		{name: "empty Unix path", host: "unix://"},
		{name: "relative Unix path", host: "unix://docker.sock"},
		{name: "dot-relative Unix path", host: "unix://./docker.sock"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("DOCKER_HOST", test.host)
			if _, err := NewFromEnvironment(); err == nil {
				t.Fatalf("NewFromEnvironment accepted DOCKER_HOST=%q", test.host)
			}
		})
	}
}

func TestNewFromEnvironmentDefaultsToLocalUnixSocket(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	client, err := NewFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := client.socketPath, "/var/run/docker.sock"; got != want {
		t.Fatalf("socketPath = %q, want %q", got, want)
	}
}

func TestIdentityUsesDockerInfoID(t *testing.T) {
	client := newUnixDockerClient(t, dockerHandler(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/v1.47/info" {
			t.Errorf("unexpected Docker request: %s %s", request.Method, request.RequestURI)
			http.Error(writer, "unexpected request", http.StatusInternalServerError)
			return
		}
		writeJSON(t, writer, http.StatusOK, map[string]string{"ID": "daemon-identity"})
	}))

	id, err := client.Identity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if id != "daemon-identity" {
		t.Fatalf("Docker identity = %q", id)
	}
}

func TestIdentityRejectsEmptyDockerInfoID(t *testing.T) {
	client := newUnixDockerClient(t, dockerHandler(func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(t, writer, http.StatusOK, map[string]string{"ID": ""})
	}))
	if _, err := client.Identity(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "empty engine ID") {
		t.Fatalf("Identity error = %v", err)
	}
}

func TestAPIVersionNegotiationAndCapping(t *testing.T) {
	tests := []struct {
		name          string
		serverVersion string
		wantPrefix    string
	}{
		{name: "minimum supported daemon version", serverVersion: "1.44", wantPrefix: "/v1.44"},
		{name: "new daemon capped to client maximum", serverVersion: "1.99", wantPrefix: "/v1.47"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var mu sync.Mutex
			versionCalls := 0
			imageCalls := 0
			client := newUnixDockerClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch {
				case request.URL.Path == "/version":
					versionCalls++
					writeJSON(t, writer, http.StatusOK, map[string]string{"ApiVersion": test.serverVersion})
				case strings.HasPrefix(request.URL.Path, test.wantPrefix+"/images/"):
					imageCalls++
					writeJSON(t, writer, http.StatusOK, map[string]interface{}{
						"Id": "sha256:image",
						"Config": map[string]interface{}{
							"Labels":      map[string]string{},
							"Healthcheck": map[string]interface{}{"Test": []string{"CMD", "true"}},
						},
					})
				default:
					t.Errorf("unexpected Docker request: %s %s", request.Method, request.RequestURI)
					http.Error(writer, "unexpected request", http.StatusInternalServerError)
				}
			}))

			for _, reference := range []string{"one:latest", "two:latest"} {
				if _, err := client.ResolveImage(context.Background(), reference); err != nil {
					t.Fatal(err)
				}
			}

			mu.Lock()
			defer mu.Unlock()
			if versionCalls != 1 {
				t.Fatalf("/version called %d times, want once", versionCalls)
			}
			if imageCalls != 2 {
				t.Fatalf("image inspection called %d times, want twice", imageCalls)
			}
		})
	}
}

func TestAPIVersionRejectsOldDaemonBeforeVersionedRequest(t *testing.T) {
	versionedCalls := 0
	client := newUnixDockerClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/version" {
			writeJSON(t, writer, http.StatusOK, map[string]string{"ApiVersion": "1.40"})
			return
		}
		versionedCalls++
		http.Error(writer, "unexpected request", http.StatusInternalServerError)
	}))

	_, err := client.ResolveImage(context.Background(), "example:latest")
	if err == nil || !strings.Contains(err.Error(), "requires at least 1.44") {
		t.Fatalf("expected minimum-version error, got %v", err)
	}
	if versionedCalls != 0 {
		t.Fatalf("old daemon received %d versioned requests, want none", versionedCalls)
	}
}

func TestAPIVersionNegotiationRetriesAfterTransientFailure(t *testing.T) {
	versionCalls := 0
	client := newUnixDockerClient(t, http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/version" {
			versionCalls++
			if versionCalls == 1 {
				writeJSON(t, writer, http.StatusServiceUnavailable, map[string]string{"message": "try again"})
				return
			}
			writeJSON(t, writer, http.StatusOK, map[string]string{"ApiVersion": "1.47"})
			return
		}
		if strings.HasPrefix(request.URL.Path, "/v1.47/images/") {
			writeJSON(t, writer, http.StatusOK, map[string]interface{}{
				"Id": "sha256:image",
				"Config": map[string]interface{}{
					"Labels":      map[string]string{},
					"Healthcheck": map[string]interface{}{"Test": []string{"CMD", "true"}},
				},
			})
			return
		}
		http.Error(writer, "unexpected request", http.StatusInternalServerError)
	}))

	if _, err := client.ResolveImage(context.Background(), "example:first"); err == nil {
		t.Fatal("transient version failure unexpectedly succeeded")
	}
	if _, err := client.ResolveImage(context.Background(), "example:second"); err != nil {
		t.Fatalf("client remained poisoned after transient version failure: %v", err)
	}
	if versionCalls != 2 {
		t.Fatalf("/version calls = %d, want 2", versionCalls)
	}
}

func TestNotFoundImplementsEngineContract(t *testing.T) {
	client := newUnixDockerClient(t, dockerHandler(func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(t, writer, http.StatusNotFound, map[string]string{
			"message": "No such container: immutable-id",
		})
	}))

	_, err := client.InspectContainer(context.Background(), "immutable-id")
	if !errors.Is(err, engine.ErrNotFound) {
		t.Fatalf("error %v does not match engine.ErrNotFound", err)
	}
	var apiError *APIError
	if errors.As(err, &apiError) {
		t.Fatalf("not-found error unexpectedly exposed as generic APIError: %v", err)
	}
}

func TestAPIErrorPreservesStatusAndMessage(t *testing.T) {
	client := newUnixDockerClient(t, dockerHandler(func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(t, writer, http.StatusConflict, map[string]string{
			"message": "name is already in use",
		})
	}))

	_, err := client.InspectContainer(context.Background(), "worker")
	var apiError *APIError
	if !errors.As(err, &apiError) {
		t.Fatalf("error %v is not an *APIError", err)
	}
	if apiError.StatusCode != http.StatusConflict || apiError.Message != "name is already in use" {
		t.Fatalf("unexpected APIError: %#v", apiError)
	}
}

func TestResolveImageParsesHealthcheck(t *testing.T) {
	healthTests := map[string]interface{}{
		"missing": nil,
		"empty":   map[string]interface{}{"Test": []string{}},
		"none":    map[string]interface{}{"Test": []string{"NONE"}},
		"command": map[string]interface{}{"Test": []string{"CMD", "grpc-health-probe"}},
		"shell":   map[string]interface{}{"Test": []string{"CMD-SHELL", "grpc-health-probe"}},
	}
	client := newUnixDockerClient(t, dockerHandler(func(writer http.ResponseWriter, request *http.Request) {
		name := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/v1.47/images/"), "/json")
		healthcheck, exists := healthTests[name]
		if !exists {
			t.Errorf("unexpected image inspection path %q", request.URL.Path)
			http.Error(writer, "unexpected image", http.StatusNotFound)
			return
		}
		config := map[string]interface{}{}
		if healthcheck != nil {
			config["Healthcheck"] = healthcheck
		}
		writeJSON(t, writer, http.StatusOK, map[string]interface{}{
			"Id":     "sha256:" + name,
			"Config": config,
		})
	}))

	for _, test := range []struct {
		name       string
		wantHealth bool
	}{
		{name: "missing", wantHealth: false},
		{name: "empty", wantHealth: false},
		{name: "none", wantHealth: false},
		{name: "command", wantHealth: true},
		{name: "shell", wantHealth: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			image, err := client.ResolveImage(context.Background(), test.name)
			if err != nil {
				t.Fatal(err)
			}
			if image.ID != "sha256:"+test.name {
				t.Fatalf("image ID = %q", image.ID)
			}
			if image.HasHealthcheck != test.wantHealth {
				t.Fatalf("HasHealthcheck = %t, want %t", image.HasHealthcheck, test.wantHealth)
			}
		})
	}
}

func TestResolveImageReportsDeclaredVolumes(t *testing.T) {
	client := newUnixDockerClient(t, dockerHandler(func(writer http.ResponseWriter, request *http.Request) {
		writeJSON(t, writer, http.StatusOK, map[string]interface{}{
			"Id": "sha256:component",
			"Config": map[string]interface{}{
				"Volumes": map[string]interface{}{
					"/var/lib/component": map[string]interface{}{},
					"/cache":             map[string]interface{}{},
				},
			},
		})
	}))

	image, err := client.ResolveImage(context.Background(), "component:dev")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/cache", "/var/lib/component"}
	if !reflect.DeepEqual(image.DeclaredVolumes, want) {
		t.Fatalf("DeclaredVolumes = %#v, want %#v", image.DeclaredVolumes, want)
	}
}

func TestCreateNetworkUsesPrivateBridgeAndInspectsImmutableID(t *testing.T) {
	var createBody map[string]interface{}
	client := newUnixDockerClient(t, dockerHandler(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v1.47/networks/create":
			if contentType := request.Header.Get("Content-Type"); contentType != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", contentType)
			}
			decodeJSON(t, request.Body, &createBody)
			writeJSON(t, writer, http.StatusCreated, map[string]string{"Id": "network-id"})
		case request.Method == http.MethodGet && request.URL.Path == "/v1.47/networks/network-id":
			writeJSON(t, writer, http.StatusOK, map[string]interface{}{
				"Id":       "network-id",
				"Name":     "dcomp-demo",
				"Driver":   "bridge",
				"Internal": true,
				"Labels":   map[string]string{"io.dcomp.owner": "test"},
				"Containers": map[string]interface{}{
					"container-id": map[string]interface{}{"Name": "worker"},
				},
			})
		default:
			t.Errorf("unexpected Docker request: %s %s", request.Method, request.RequestURI)
			http.Error(writer, "unexpected request", http.StatusInternalServerError)
		}
	}))

	network, err := client.CreateNetwork(context.Background(), engine.NetworkRequest{
		Name: "dcomp-demo", Internal: true,
		Labels: map[string]string{
			"io.dcomp.owner": "test",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	wantBody := map[string]interface{}{
		"Name":     "dcomp-demo",
		"Driver":   "bridge",
		"Internal": true,
		"Labels": map[string]interface{}{
			"io.dcomp.owner": "test",
		},
	}
	if !reflect.DeepEqual(createBody, wantBody) {
		t.Fatalf("network create body:\n got: %#v\nwant: %#v", createBody, wantBody)
	}
	if network.ID != "network-id" || network.Name != "dcomp-demo" ||
		network.Driver != "bridge" || !network.Internal {
		t.Fatalf("unexpected inspected network: %#v", network)
	}
	if len(network.Containers) != 1 || network.Containers[0] != "container-id" {
		t.Fatalf("unexpected network containers: %#v", network.Containers)
	}
}

func TestCreateNetworkUsesExplicitNonInternalBridge(t *testing.T) {
	var createBody map[string]interface{}
	client := newUnixDockerClient(t, dockerHandler(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v1.47/networks/create":
			decodeJSON(t, request.Body, &createBody)
			writeJSON(t, writer, http.StatusCreated, map[string]string{"Id": "egress-network-id"})
		case request.Method == http.MethodGet && request.URL.Path == "/v1.47/networks/egress-network-id":
			writeJSON(t, writer, http.StatusOK, map[string]interface{}{
				"Id": "egress-network-id", "Name": "dcomp-demo-egress",
				"Driver": "bridge", "Internal": false,
				"Labels": map[string]string{"io.dcomp.kind": "egress"},
			})
		default:
			t.Errorf("unexpected Docker request: %s %s", request.Method, request.RequestURI)
			http.Error(writer, "unexpected request", http.StatusInternalServerError)
		}
	}))

	network, err := client.CreateNetwork(context.Background(), engine.NetworkRequest{
		Name: "dcomp-demo-egress", Internal: false,
		Labels: map[string]string{"io.dcomp.kind": "egress"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if internal, ok := createBody["Internal"].(bool); !ok || internal {
		t.Fatalf("network create Internal = %#v, want false", createBody["Internal"])
	}
	if network.ID != "egress-network-id" || network.Internal {
		t.Fatalf("unexpected inspected egress network: %#v", network)
	}
}

func TestCreateAndInspectPersistentVolume(t *testing.T) {
	var createBody map[string]interface{}
	client := newUnixDockerClient(t, dockerHandler(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v1.47/volumes/create":
			decodeJSON(t, request.Body, &createBody)
			writeJSON(t, writer, http.StatusCreated, map[string]interface{}{
				"Name": "dcomp-team-state", "Driver": "local",
				"Mountpoint": "/var/lib/docker/volumes/dcomp-team-state/_data",
				"Labels":     map[string]string{"io.dcomp.owner": "test"},
			})
		case request.Method == http.MethodGet &&
			request.URL.Path == "/v1.47/volumes/dcomp-team-state":
			writeJSON(t, writer, http.StatusOK, map[string]interface{}{
				"Name": "dcomp-team-state", "Driver": "local",
				"Mountpoint": "/var/lib/docker/volumes/dcomp-team-state/_data",
				"Labels":     map[string]string{"io.dcomp.owner": "test"},
			})
		default:
			t.Errorf("unexpected Docker request: %s %s", request.Method, request.RequestURI)
			http.Error(writer, "unexpected request", http.StatusInternalServerError)
		}
	}))

	created, err := client.CreateVolume(context.Background(), engine.VolumeRequest{
		Name:   "dcomp-team-state",
		Labels: map[string]string{"io.dcomp.owner": "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	inspected, err := client.InspectVolume(context.Background(), "dcomp-team-state")
	if err != nil {
		t.Fatal(err)
	}
	wantBody := map[string]interface{}{
		"Name": "dcomp-team-state", "Driver": "local",
		"Labels": map[string]interface{}{"io.dcomp.owner": "test"},
	}
	if !reflect.DeepEqual(createBody, wantBody) {
		t.Fatalf("volume create body:\n got: %#v\nwant: %#v", createBody, wantBody)
	}
	want := engine.Volume{
		Name: "dcomp-team-state", Driver: "local",
		Mountpoint: "/var/lib/docker/volumes/dcomp-team-state/_data",
		Labels:     map[string]string{"io.dcomp.owner": "test"},
	}
	if !reflect.DeepEqual(created, want) || !reflect.DeepEqual(inspected, want) {
		t.Fatalf("created = %#v, inspected = %#v, want %#v", created, inspected, want)
	}
}

func TestCreateContainerUsesExactLaunchResourcesAndFixedPolicy(t *testing.T) {
	var createBody map[string]interface{}
	client := newUnixDockerClient(t, dockerHandler(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v1.47/containers/create":
			if got := request.URL.Query().Get("name"); got != "worker" {
				t.Errorf("container name query = %q, want worker", got)
			}
			decodeJSON(t, request.Body, &createBody)
			writeJSON(t, writer, http.StatusCreated, map[string]interface{}{
				"Id":       "container-id",
				"Warnings": []string{},
			})
		case request.Method == http.MethodGet && request.URL.Path == "/v1.47/containers/container-id/json":
			writeJSON(t, writer, http.StatusOK, map[string]interface{}{
				"Id":    "container-id",
				"Name":  "/worker",
				"Image": "sha256:image-id",
				"Config": map[string]interface{}{
					"Labels":    map[string]string{"io.dcomp.owner": "test"},
					"Cmd":       []string{"serve", "--listen=:50051"},
					"Env":       []string{"A_FIRST=a", "Z_LAST=z"},
					"OpenStdin": true,
					"StdinOnce": false,
					"Tty":       false,
				},
				"HostConfig": map[string]interface{}{
					"NetworkMode": "network-id",
					"Init":        true,
					"RestartPolicy": map[string]interface{}{
						"Name": "no",
					},
					"SecurityOpt": []string{"no-new-privileges"},
					"CapDrop":     []string{"NET_RAW"},
					"PidsLimit":   2048,
					"PortBindings": map[string]interface{}{
						"50051/tcp": []map[string]string{{
							"HostIp": "127.0.0.1", "HostPort": "15051",
						}},
						"5353/udp": []map[string]string{{
							"HostIp": "", "HostPort": "15353",
						}},
					},
				},
				"Mounts": []map[string]interface{}{
					{
						"Type": "volume", "Name": "team-state",
						"Source":      "/var/lib/docker/volumes/team-state/_data",
						"Destination": "/state", "RW": true,
					},
					{
						"Type": "bind", "Source": "/srv/project",
						"Destination": "/workspace", "RW": false,
					},
				},
				"State": map[string]interface{}{
					"Status":   "created",
					"Running":  false,
					"ExitCode": 0,
					"Error":    "",
				},
				"NetworkSettings": map[string]interface{}{
					"Networks": map[string]interface{}{
						// Docker leaves NetworkID empty until a created container
						// is started; the map key remains the exact network name.
						"dcomp-demo": map[string]interface{}{
							"NetworkID": "",
							"Aliases":   []string{"backend", "provider"},
						},
					},
				},
			})
		default:
			t.Errorf("unexpected Docker request: %s %s", request.Method, request.RequestURI)
			http.Error(writer, "unexpected request", http.StatusInternalServerError)
		}
	}))

	container, err := client.CreateContainer(context.Background(), engine.ContainerRequest{
		Name:           "worker",
		ImageID:        "sha256:image-id",
		NetworkID:      "network-id",
		NetworkAliases: []string{"backend", "provider"},
		Labels:         map[string]string{"io.dcomp.owner": "test"},
		Environment: map[string]string{
			"Z_LAST":  "z",
			"A_FIRST": "a",
		},
		Mounts: []engine.Mount{
			{
				Type: engine.MountBind, Source: "/srv/project",
				Target: "/workspace", ReadOnly: true,
			},
			{
				Type: engine.MountVolume, Source: "team-state",
				Target: "/state", ReadOnly: false,
			},
		},
		Args: []string{"serve", "--listen=:50051"},
		PortBindings: []engine.PortBinding{
			{
				ContainerPort: 50051, Protocol: engine.ProtocolTCP,
				HostIP: "127.0.0.1", HostPort: 15051,
			},
			{
				ContainerPort: 5353, Protocol: engine.ProtocolUDP,
				HostPort: 15353,
			},
		},
		StopTimeout: 3 * time.Second,
		Security: engine.ContainerSecurity{
			NoNewPrivileges:     true,
			DroppedCapabilities: []string{"NET_RAW"},
			PIDsLimit:           2048,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	wantBody := map[string]interface{}{
		"Image":     "sha256:image-id",
		"Env":       []interface{}{"A_FIRST=a", "Z_LAST=z"},
		"Cmd":       []interface{}{"serve", "--listen=:50051"},
		"OpenStdin": true,
		"StdinOnce": false,
		"Tty":       false,
		"ExposedPorts": map[string]interface{}{
			"50051/tcp": map[string]interface{}{},
			"5353/udp":  map[string]interface{}{},
		},
		"Labels":      map[string]interface{}{"io.dcomp.owner": "test"},
		"StopTimeout": float64(3),
		"HostConfig": map[string]interface{}{
			"NetworkMode": "network-id",
			"Init":        true,
			"RestartPolicy": map[string]interface{}{
				"Name": "no",
			},
			"SecurityOpt": []interface{}{"no-new-privileges"},
			"CapDrop":     []interface{}{"NET_RAW"},
			"PidsLimit":   float64(2048),
			"Mounts": []interface{}{
				map[string]interface{}{
					"Type": "bind", "Source": "/srv/project",
					"Target": "/workspace", "ReadOnly": true,
				},
				map[string]interface{}{
					"Type": "volume", "Source": "team-state",
					"Target": "/state", "ReadOnly": false,
				},
			},
			"PortBindings": map[string]interface{}{
				"50051/tcp": []interface{}{
					map[string]interface{}{"HostIp": "127.0.0.1", "HostPort": "15051"},
				},
				"5353/udp": []interface{}{
					map[string]interface{}{"HostIp": "", "HostPort": "15353"},
				},
			},
		},
		"NetworkingConfig": map[string]interface{}{
			"EndpointsConfig": map[string]interface{}{
				"network-id": map[string]interface{}{
					"Aliases": []interface{}{"backend", "provider"},
				},
			},
		},
	}
	if !reflect.DeepEqual(createBody, wantBody) {
		t.Fatalf("container create body:\n got: %#v\nwant: %#v", createBody, wantBody)
	}
	if container.ID != "container-id" || container.Name != "worker" {
		t.Fatalf("unexpected inspected container: %#v", container)
	}
	if container.ImageID != "sha256:image-id" {
		t.Fatalf("inspected image ID = %q", container.ImageID)
	}
	wantAttachment := engine.NetworkAttachment{
		NetworkID: "", Aliases: []string{"backend", "provider"},
	}
	if got := container.Networks["dcomp-demo"]; !reflect.DeepEqual(got, wantAttachment) {
		t.Fatalf("pre-start inspected network = %#v, want %#v", got, wantAttachment)
	}
	if container.Health != engine.HealthNone {
		t.Fatalf("health = %q, want %q", container.Health, engine.HealthNone)
	}
	if !container.Init || container.RestartPolicy != "no" {
		t.Fatalf(
			"inspected create policy = init:%t restart:%q, want true/no",
			container.Init, container.RestartPolicy,
		)
	}
	if !container.OpenStdin || container.StdinOnce || container.TTY {
		t.Fatalf(
			"inspected standard I/O policy = open:%t once:%t tty:%t",
			container.OpenStdin,
			container.StdinOnce,
			container.TTY,
		)
	}
	wantSecurity := engine.ContainerSecurity{
		NoNewPrivileges:     true,
		DroppedCapabilities: []string{"NET_RAW"},
		PIDsLimit:           2048,
	}
	if !reflect.DeepEqual(container.Security, wantSecurity) {
		t.Fatalf("inspected security = %#v, want %#v", container.Security, wantSecurity)
	}
	if want := []string{"serve", "--listen=:50051"}; !reflect.DeepEqual(container.Args, want) {
		t.Fatalf("inspected args = %#v, want %#v", container.Args, want)
	}
	wantEnvironment := map[string]string{"A_FIRST": "a", "Z_LAST": "z"}
	if !reflect.DeepEqual(container.Environment, wantEnvironment) {
		t.Fatalf(
			"inspected environment = %#v, want %#v",
			container.Environment, wantEnvironment,
		)
	}
	wantMounts := []engine.Mount{
		{
			Type: engine.MountVolume, Source: "team-state",
			Target: "/state", ReadOnly: false,
		},
		{
			Type: engine.MountBind, Source: "/srv/project",
			Target: "/workspace", ReadOnly: true,
		},
	}
	if !reflect.DeepEqual(container.Mounts, wantMounts) {
		t.Fatalf("inspected mounts = %#v, want %#v", container.Mounts, wantMounts)
	}
	wantPorts := []engine.PortBinding{
		{
			ContainerPort: 5353, Protocol: engine.ProtocolUDP,
			HostPort: 15353,
		},
		{
			ContainerPort: 50051, Protocol: engine.ProtocolTCP,
			HostIP: "127.0.0.1", HostPort: 15051,
		},
	}
	if !reflect.DeepEqual(container.PortBindings, wantPorts) {
		t.Fatalf(
			"inspected port bindings = %#v, want %#v",
			container.PortBindings, wantPorts,
		)
	}
}

func TestCreateContainerRejectsUnsupportedLaunchResourcesBeforeDockerRequest(t *testing.T) {
	client := newUnixDockerClient(t, dockerHandler(func(writer http.ResponseWriter, request *http.Request) {
		t.Errorf("invalid launch resource reached Docker: %s %s", request.Method, request.RequestURI)
		http.Error(writer, "unexpected request", http.StatusInternalServerError)
	}))
	tests := []struct {
		name    string
		request engine.ContainerRequest
	}{
		{
			name: "unsupported mount type",
			request: engine.ContainerRequest{Mounts: []engine.Mount{{
				Type: "tmpfs", Source: "ignored", Target: "/target",
			}}},
		},
		{
			name: "empty mount source",
			request: engine.ContainerRequest{Mounts: []engine.Mount{{
				Type: engine.MountBind, Target: "/target",
			}}},
		},
		{
			name: "empty mount target",
			request: engine.ContainerRequest{Mounts: []engine.Mount{{
				Type: engine.MountVolume, Source: "state",
			}}},
		},
		{
			name: "zero container port",
			request: engine.ContainerRequest{PortBindings: []engine.PortBinding{{
				ContainerPort: 0, Protocol: engine.ProtocolTCP, HostPort: 8080,
			}}},
		},
		{
			name: "unsupported protocol",
			request: engine.ContainerRequest{PortBindings: []engine.PortBinding{{
				ContainerPort: 50051, Protocol: "sctp", HostPort: 8080,
			}}},
		},
		{
			name: "invalid host port",
			request: engine.ContainerRequest{PortBindings: []engine.PortBinding{{
				ContainerPort: 50051, Protocol: engine.ProtocolUDP, HostPort: 65536,
			}}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.request.Name = "invalid"
			test.request.ImageID = "sha256:image"
			if _, err := client.CreateContainer(context.Background(), test.request); err == nil {
				t.Fatal("CreateContainer unexpectedly accepted invalid launch resource")
			}
		})
	}
}

func TestCreateContainerWithoutPrimaryNetworkUsesNone(t *testing.T) {
	var createBody map[string]interface{}
	client := newUnixDockerClient(t, dockerHandler(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v1.47/containers/create":
			decodeJSON(t, request.Body, &createBody)
			writeJSON(t, writer, http.StatusCreated, map[string]string{"Id": "isolated-id"})
		case request.Method == http.MethodGet &&
			request.URL.Path == "/v1.47/containers/isolated-id/json":
			writeJSON(t, writer, http.StatusOK, map[string]interface{}{
				"Id": "isolated-id", "Name": "/isolated", "Image": "sha256:image",
				"Config": map[string]interface{}{
					"Labels": map[string]string{"io.dcomp.owner": "test"},
				},
				"HostConfig": map[string]interface{}{
					"NetworkMode": "none", "Init": true,
					"RestartPolicy": map[string]string{"Name": "no"},
				},
				"State": map[string]interface{}{"Status": "created"},
				"NetworkSettings": map[string]interface{}{
					"Networks": map[string]interface{}{
						"none": map[string]interface{}{"NetworkID": ""},
					},
				},
			})
		default:
			t.Errorf("unexpected Docker request: %s %s", request.Method, request.RequestURI)
			http.Error(writer, "unexpected request", http.StatusInternalServerError)
		}
	}))

	container, err := client.CreateContainer(context.Background(), engine.ContainerRequest{
		Name: "isolated", ImageID: "sha256:image",
		Labels: map[string]string{"io.dcomp.owner": "test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := createBody["NetworkingConfig"]; got != nil {
		t.Fatalf("isolated create unexpectedly has NetworkingConfig: %#v", got)
	}
	if _, exists := createBody["Cmd"]; exists {
		t.Fatalf("nil args must preserve the image command, body: %#v", createBody)
	}
	hostConfig, ok := createBody["HostConfig"].(map[string]interface{})
	if !ok {
		t.Fatalf("HostConfig = %#v", createBody["HostConfig"])
	}
	if got := hostConfig["NetworkMode"]; got != "none" {
		t.Fatalf("NetworkMode = %#v, want none", got)
	}
	if got := hostConfig["Init"]; got != true {
		t.Fatalf("Init = %#v, want true", got)
	}
	if len(container.Networks) != 0 {
		t.Fatalf("unexpected isolated network inspection: %#v", container)
	}
	if !container.Init || container.RestartPolicy != "no" {
		t.Fatalf("unexpected fixed create policy: %#v", container)
	}
}

func TestInspectContainerSeparatesRequestedAndEffectiveDynamicPort(t *testing.T) {
	client := newUnixDockerClient(t, dockerHandler(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		if request.Method != http.MethodGet ||
			request.URL.Path != "/v1.47/containers/dynamic/json" {
			t.Errorf("unexpected Docker request: %s %s", request.Method, request.RequestURI)
			http.Error(writer, "unexpected request", http.StatusInternalServerError)
			return
		}
		writeJSON(t, writer, http.StatusOK, map[string]interface{}{
			"Id": "dynamic", "Name": "/dynamic", "Image": "sha256:image",
			"Config": map[string]interface{}{"Labels": map[string]string{}},
			"HostConfig": map[string]interface{}{
				"Init":          true,
				"RestartPolicy": map[string]string{"Name": "no"},
				"PortBindings": map[string]interface{}{
					"8080/tcp": []map[string]string{{
						"HostIp": "127.0.0.1", "HostPort": "",
					}},
				},
			},
			"State": map[string]interface{}{"Status": "running", "Running": true},
			"NetworkSettings": map[string]interface{}{
				"Networks": map[string]interface{}{},
				"Ports": map[string]interface{}{
					"8080/tcp": []map[string]string{{
						"HostIp": "127.0.0.1", "HostPort": "49152",
					}},
				},
			},
		})
	}))

	container, err := client.InspectContainer(context.Background(), "dynamic")
	if err != nil {
		t.Fatal(err)
	}
	wantRequested := []engine.PortBinding{{
		ContainerPort: 8080, Protocol: engine.ProtocolTCP,
		HostIP: "127.0.0.1", HostPort: 0,
	}}
	if !reflect.DeepEqual(container.PortBindings, wantRequested) {
		t.Fatalf(
			"configured port bindings = %#v, want %#v",
			container.PortBindings, wantRequested,
		)
	}
	wantEffective := []engine.PortBinding{{
		ContainerPort: 8080, Protocol: engine.ProtocolTCP,
		HostIP: "127.0.0.1", HostPort: 49152,
	}}
	if !reflect.DeepEqual(container.PublishedPorts, wantEffective) {
		t.Fatalf(
			"effective published ports = %#v, want %#v",
			container.PublishedPorts, wantEffective,
		)
	}
}

func TestCreateContainerDistinguishesEmptyArgsFromNilArgs(t *testing.T) {
	var createBody map[string]interface{}
	client := newUnixDockerClient(t, dockerHandler(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/v1.47/containers/create":
			decodeJSON(t, request.Body, &createBody)
			writeJSON(t, writer, http.StatusCreated, map[string]string{"Id": "empty-args-id"})
		case request.Method == http.MethodGet &&
			request.URL.Path == "/v1.47/containers/empty-args-id/json":
			writeJSON(t, writer, http.StatusOK, map[string]interface{}{
				"Id": "empty-args-id", "Name": "/empty-args", "Image": "sha256:image",
				"Config": map[string]interface{}{"Cmd": []string{}},
				"HostConfig": map[string]interface{}{
					"NetworkMode": "none", "Init": true,
					"RestartPolicy": map[string]string{"Name": "no"},
				},
				"State":           map[string]interface{}{"Status": "created"},
				"NetworkSettings": map[string]interface{}{"Networks": map[string]interface{}{}},
			})
		default:
			t.Errorf("unexpected Docker request: %s %s", request.Method, request.RequestURI)
			http.Error(writer, "unexpected request", http.StatusInternalServerError)
		}
	}))

	container, err := client.CreateContainer(context.Background(), engine.ContainerRequest{
		Name: "empty-args", ImageID: "sha256:image", Args: []string{},
	})
	if err != nil {
		t.Fatal(err)
	}
	args, exists := createBody["Cmd"]
	if !exists {
		t.Fatalf("explicit empty args were omitted from create body: %#v", createBody)
	}
	if want := []interface{}{}; !reflect.DeepEqual(args, want) {
		t.Fatalf("Cmd = %#v, want explicit empty array", args)
	}
	if container.Args == nil || len(container.Args) != 0 {
		t.Fatalf("inspected Args = %#v, want non-nil empty slice", container.Args)
	}
}

func TestInspectContainerReportsEveryExactNetworkAttachment(t *testing.T) {
	client := newUnixDockerClient(t, dockerHandler(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet ||
			request.URL.Path != "/v1.47/containers/immutable-container-id/json" {
			t.Errorf("unexpected Docker request: %s %s", request.Method, request.RequestURI)
			http.Error(writer, "unexpected request", http.StatusInternalServerError)
			return
		}
		writeJSON(t, writer, http.StatusOK, map[string]interface{}{
			"Id": "immutable-container-id", "Name": "/worker", "Image": "sha256:image",
			"Config":     map[string]interface{}{},
			"HostConfig": map[string]interface{}{"NetworkMode": "base-network-id"},
			"State":      map[string]interface{}{"Status": "created"},
			"NetworkSettings": map[string]interface{}{
				"Networks": map[string]interface{}{
					"dcomp-link-a": map[string]interface{}{
						"NetworkID": "link-network-id",
						"Aliases":   []string{"worker", "consumer"},
					},
					"dcomp-egress": map[string]interface{}{
						"NetworkID": "egress-network-id",
						"Aliases":   []string{"worker"},
					},
				},
			},
		})
	}))

	container, err := client.InspectContainer(
		context.Background(), "immutable-container-id",
	)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]engine.NetworkAttachment{
		"dcomp-link-a": {
			NetworkID: "link-network-id", Aliases: []string{"worker", "consumer"},
		},
		"dcomp-egress": {
			NetworkID: "egress-network-id", Aliases: []string{"worker"},
		},
	}
	if !reflect.DeepEqual(container.Networks, want) {
		t.Fatalf("Networks = %#v, want %#v", container.Networks, want)
	}
}

func TestConnectAndDisconnectNetworkUseExactImmutableIDs(t *testing.T) {
	type observedRequest struct {
		Method string
		URI    string
		Body   map[string]interface{}
	}
	var requests []observedRequest
	client := newUnixDockerClient(t, dockerHandler(func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]interface{}
		decodeJSON(t, request.Body, &body)
		requests = append(requests, observedRequest{
			Method: request.Method, URI: request.RequestURI, Body: body,
		})
		writer.WriteHeader(http.StatusNoContent)
	}))

	const networkID = "sha256:network/id"
	const containerID = "sha256:container/id"
	if err := client.ConnectNetwork(
		context.Background(), networkID, containerID, []string{"provider", "gateway"},
	); err != nil {
		t.Fatal(err)
	}
	if err := client.DisconnectNetwork(
		context.Background(), networkID, containerID,
	); err != nil {
		t.Fatal(err)
	}

	want := []observedRequest{
		{
			Method: http.MethodPost,
			URI:    "/v1.47/networks/sha256:network%2Fid/connect",
			Body: map[string]interface{}{
				"Container": containerID,
				"EndpointConfig": map[string]interface{}{
					"Aliases": []interface{}{"provider", "gateway"},
				},
			},
		},
		{
			Method: http.MethodPost,
			URI:    "/v1.47/networks/sha256:network%2Fid/disconnect",
			Body: map[string]interface{}{
				"Container": containerID,
				"Force":     false,
			},
		},
	}
	if !reflect.DeepEqual(requests, want) {
		t.Fatalf("network attachment requests:\n got: %#v\nwant: %#v", requests, want)
	}
}

func TestContainerControlAddressesOnlyTheExactImmutableID(t *testing.T) {
	var requests []string
	client := newUnixDockerClient(t, dockerHandler(func(writer http.ResponseWriter, request *http.Request) {
		requests = append(requests, request.Method+" "+request.RequestURI)
		writer.WriteHeader(http.StatusNoContent)
	}))

	const immutableID = "sha256:dead/beef"
	if err := client.StartContainer(context.Background(), immutableID); err != nil {
		t.Fatal(err)
	}
	if err := client.StopContainer(context.Background(), immutableID, 7*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := client.RestartContainer(context.Background(), immutableID, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := client.RemoveContainer(context.Background(), immutableID); err != nil {
		t.Fatal(err)
	}
	if err := client.StopContainer(context.Background(), immutableID, 1100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if err := client.RemoveNetwork(context.Background(), immutableID); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"POST /v1.47/containers/sha256:dead%2Fbeef/start",
		"POST /v1.47/containers/sha256:dead%2Fbeef/stop?t=7",
		"POST /v1.47/containers/sha256:dead%2Fbeef/restart?t=3",
		"DELETE /v1.47/containers/sha256:dead%2Fbeef?v=0&force=0",
		"POST /v1.47/containers/sha256:dead%2Fbeef/stop?t=2",
		"DELETE /v1.47/networks/sha256:dead%2Fbeef",
	}
	if !reflect.DeepEqual(requests, want) {
		t.Fatalf("mutation requests:\n got: %#v\nwant: %#v", requests, want)
	}
}

// dockerHandler handles version negotiation and delegates every versioned
// request. Tests using it therefore exercise the same API boundary as Docker.
func dockerHandler(versioned http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == "/version" {
			writeJSON(nil, writer, http.StatusOK, map[string]string{"ApiVersion": "1.47"})
			return
		}
		versioned(writer, request)
	})
}

func newUnixDockerClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	socketPath := filepath.Join(t.TempDir(), "docker.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	stopped := make(chan error, 1)
	go func() {
		stopped <- server.Serve(listener)
	}()
	t.Cleanup(func() {
		_ = server.Close()
		_ = listener.Close()
		err := <-stopped
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("fake Docker server: %v", err)
		}
	})

	t.Setenv("DOCKER_HOST", "unix://"+socketPath)
	client, err := NewFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func writeJSON(t *testing.T, writer http.ResponseWriter, status int, value interface{}) {
	if t != nil {
		t.Helper()
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	if err := json.NewEncoder(writer).Encode(value); err != nil && t != nil {
		t.Errorf("encode fake Docker response: %v", err)
	}
}

func decodeJSON(t *testing.T, reader io.Reader, value interface{}) {
	t.Helper()
	decoder := json.NewDecoder(reader)
	if err := decoder.Decode(value); err != nil {
		t.Errorf("decode Docker request: %v", err)
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Errorf("Docker request contains trailing JSON: %v", err)
	}
}
