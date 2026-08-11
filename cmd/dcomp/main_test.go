package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/glguida/dcomp/state"
)

func TestHelpSucceeds(t *testing.T) {
	t.Setenv("DCOMP_STATE_ROOT", "invalid-relative-default")
	if code := run([]string{"-h"}); code != 0 {
		t.Fatalf("help exit code = %d, want 0", code)
	}
}

func TestRelativeStateRootIsRejectedBeforeDockerAccess(t *testing.T) {
	if code := run([]string{"--state-root", "relative", "status", "demo"}); code != 2 {
		t.Fatalf("relative state-root exit code = %d, want 2", code)
	}
}

func TestPSJSONListsNoComponentsForEmptyState(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DCOMP_STATE_ROOT", root)
	t.Setenv("DOCKER_HOST", "unix://"+filepath.Join(root, "missing.sock"))

	output, code := captureStdout(t, func() int {
		return run([]string{"ps", "--json"})
	})
	if code != 0 {
		t.Fatalf("ps exit code = %d, want 0", code)
	}
	const want = "{\"api_version\":1,\"components\":[]}\n"
	if output != want {
		t.Fatalf("ps output = %q, want %q", output, want)
	}
}

func TestPSRejectsMoreThanOneSystemName(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DCOMP_STATE_ROOT", root)
	t.Setenv("DOCKER_HOST", "unix://"+filepath.Join(root, "missing.sock"))
	if code := run([]string{"ps", "first", "second"}); code != 2 {
		t.Fatalf("ps exit code = %d, want 2", code)
	}
}

func TestVolumeJSONInspectsExactOwnedVolume(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "docker.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Type", "application/json")
			switch request.URL.Path {
			case "/version":
				_ = json.NewEncoder(writer).Encode(map[string]string{
					"ApiVersion": "1.47",
				})
			case "/v1.47/info":
				_ = json.NewEncoder(writer).Encode(map[string]string{
					"ID": "test-engine",
				})
			case "/v1.47/volumes/dcomp.demo.volume.worker.state":
				_ = json.NewEncoder(writer).Encode(map[string]interface{}{
					"Name":   "dcomp.demo.volume.worker.state",
					"Driver": "local",
					"Labels": map[string]string{
						"io.dcomp.owner":          "dcomp",
						"io.dcomp.system":         "demo",
						"io.dcomp.kind":           "volume",
						"io.dcomp.component":      "worker",
						"io.dcomp.volume":         "1",
						"io.dcomp.volume-logical": "state",
					},
				})
			default:
				http.Error(writer, "unexpected Docker request", http.StatusNotFound)
			}
		},
	)}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	root := t.TempDir()
	store := state.Store{Root: root}
	if err := store.BindEngine("test-engine"); err != nil {
		t.Fatal(err)
	}
	lock, err := store.Acquire("demo")
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_HOST", "unix://"+socket)
	t.Setenv("DCOMP_STATE_ROOT", root)

	output, code := captureStdout(t, func() int {
		return run([]string{"volume", "--json", "demo", "worker", "state"})
	})
	if code != 0 {
		t.Fatalf("volume exit code = %d, want 0", code)
	}
	const want = "{\"api_version\":1,\"system\":\"demo\"," +
		"\"component\":\"worker\",\"logical_name\":\"state\"," +
		"\"name\":\"dcomp.demo.volume.worker.state\"}\n"
	if output != want {
		t.Fatalf("volume output = %q, want %q", output, want)
	}

	output, code = captureStdout(t, func() int {
		return run([]string{"volume", "demo", "worker", "state"})
	})
	if code != 0 {
		t.Fatalf("plain volume exit code = %d, want 0", code)
	}
	if want := "dcomp.demo.volume.worker.state\n"; output != want {
		t.Fatalf("plain volume output = %q, want %q", output, want)
	}
}

func TestVolumeRequiresExactlyThreeCoordinates(t *testing.T) {
	if code := run([]string{"volume", "--json", "demo", "worker"}); code != 2 {
		t.Fatalf("volume exit code = %d, want 2", code)
	}
}

func captureStdout(t *testing.T, action func() int) (string, int) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdout
	os.Stdout = writer
	code := func() int {
		defer func() { os.Stdout = previous }()
		return action()
	}()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(reader)
	if closeErr := reader.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(output), code
}
