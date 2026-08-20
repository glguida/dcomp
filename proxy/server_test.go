package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/glguida/dcomp/composition"
)

func TestProxyForwardsBidirectionallyAndReconnects(t *testing.T) {
	config := testConfig(t, false)
	cancel, result := startTestProxy(t, config)
	defer func() {
		cancel()
		if err := <-result; err != nil {
			t.Fatalf("proxy Run: %v", err)
		}
	}()

	for round := 0; round < 2; round++ {
		producer := dialUnix(t, HostSocket(config.RuntimeDir, DirectionOutput, "source", "documents"))
		consumer := dialUnix(t, HostSocket(config.RuntimeDir, DirectionInput, "filter", "documents"))

		writeAndRead(t, consumer, producer, "request")
		writeAndRead(t, producer, consumer, "response")
		_ = consumer.Close()
		_ = producer.Close()
	}
}

func TestForwardLogsCopyErrorsAndReleasesPair(t *testing.T) {
	var logs bytes.Buffer
	previousOutput, previousFlags, previousPrefix := log.Writer(), log.Flags(), log.Prefix()
	log.SetOutput(&logs)
	log.SetFlags(0)
	log.SetPrefix("")
	defer func() {
		log.SetOutput(previousOutput)
		log.SetFlags(previousFlags)
		log.SetPrefix(previousPrefix)
	}()

	consumer, consumerPeer := net.Pipe()
	producer, producerPeer := net.Pipe()
	defer consumerPeer.Close()
	defer producerPeer.Close()
	server := &server{connections: make(map[net.Conn]struct{})}
	server.track(consumer)
	server.track(producer)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	forwarded := make(chan struct{})
	go func() {
		server.forward(ctx, "input/client/upstream", "output/server/api", consumer, producer)
		close(forwarded)
	}()

	if err := consumerPeer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := producerPeer.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := producerPeer.Write([]byte("trigger closed destination")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-forwarded:
	case <-time.After(time.Second):
		t.Fatal("forwarding pair did not close after copy failure")
	}
	if !strings.Contains(logs.String(), "connection copy failed") {
		t.Fatalf("forwarding log omitted copy failure: %q", logs.String())
	}
}

func TestProxyFanoutUsesIndependentBidirectionalConnections(t *testing.T) {
	config := testConfig(t, true)
	cancel, result := startTestProxy(t, config)
	defer func() {
		cancel()
		if err := <-result; err != nil {
			t.Fatalf("proxy Run: %v", err)
		}
	}()

	producerA := dialUnix(t, HostSocket(config.RuntimeDir, DirectionOutput, "source", "documents"))
	consumerA := dialUnix(t, HostSocket(config.RuntimeDir, DirectionInput, "filter", "documents"))
	defer producerA.Close()
	defer consumerA.Close()

	// Output connections are pooled in acceptance order. Each consumer gets a
	// dedicated stream; neither request is broadcast onto the other stream.
	writeAndRead(t, consumerA, producerA, "filter-request")
	writeAndRead(t, producerA, consumerA, "filter-response")

	producerB := dialUnix(t, HostSocket(config.RuntimeDir, DirectionOutput, "source", "documents"))
	consumerB := dialUnix(t, HostSocket(config.RuntimeDir, DirectionInput, "archive", "documents"))
	defer producerB.Close()
	defer consumerB.Close()
	writeAndRead(t, consumerB, producerB, "archive-request")
	writeAndRead(t, producerB, consumerB, "archive-response")
}

func TestControlStatusAndShutdown(t *testing.T) {
	config := testConfig(t, false)
	_, result := startTestProxy(t, config)
	manager := &ProcessManager{}
	process := processForConfig(config, os.Getpid())
	status, err := manager.Inspect(context.Background(), process)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Ready || status.Inputs != 1 || status.Outputs != 1 {
		t.Fatalf("unexpected proxy status: %#v", status)
	}
	if err := manager.Stop(context.Background(), process); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatalf("proxy Run: %v", err)
	}
	if _, err := os.Stat(ControlSocket(config.RuntimeDir)); !os.IsNotExist(err) {
		t.Fatalf("control socket remains after shutdown: %v", err)
	}
}

func TestSecondProxyCannotReplaceLiveSockets(t *testing.T) {
	config := testConfig(t, false)
	cancel, result := startTestProxy(t, config)
	defer func() {
		cancel()
		if err := <-result; err != nil {
			t.Fatalf("proxy Run: %v", err)
		}
	}()

	second := config
	second.InstanceID = "second-instance"
	digest, err := second.computeDigest()
	if err != nil {
		t.Fatal(err)
	}
	second.Digest = digest
	if err := Run(context.Background(), second, nil); err == nil ||
		!strings.Contains(err.Error(), "already active") {
		t.Fatalf("second proxy error = %v", err)
	}

	producer := dialUnix(t, HostSocket(config.RuntimeDir, DirectionOutput, "source", "documents"))
	consumer := dialUnix(t, HostSocket(config.RuntimeDir, DirectionInput, "filter", "documents"))
	defer producer.Close()
	defer consumer.Close()
	writeAndRead(t, consumer, producer, "still-live")
}

func TestIdentityMismatchIsNotReportedAsStopped(t *testing.T) {
	config := testConfig(t, false)
	cancel, result := startTestProxy(t, config)
	defer func() {
		cancel()
		if err := <-result; err != nil {
			t.Fatalf("proxy Run: %v", err)
		}
	}()

	manager := &ProcessManager{}
	wrong := processForConfig(config, os.Getpid())
	wrong.InstanceID = "older-recorded-instance"
	_, err := manager.Inspect(context.Background(), wrong)
	if !errors.Is(err, ErrIdentityMismatch) || errors.Is(err, ErrNotRunning) {
		t.Fatalf("Inspect error = %v", err)
	}
	if err := manager.Stop(context.Background(), wrong); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("Stop error = %v", err)
	}
	if _, err := os.Lstat(HostSocket(config.RuntimeDir, DirectionInput, "filter", "documents")); err != nil {
		t.Fatalf("live endpoint was removed: %v", err)
	}
}

func TestUnixSocketHardLinkAcceptsConnections(t *testing.T) {
	directory := t.TempDir()
	original := filepath.Join(directory, "original")
	alias := filepath.Join(directory, "alias")
	listener, err := net.Listen("unix", original)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Link(original, alias); err != nil {
		t.Fatal(err)
	}
	accepted := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err == nil {
			_ = connection.Close()
		}
		accepted <- err
	}()
	connection, err := net.Dial("unix", alias)
	if err != nil {
		t.Fatal(err)
	}
	_ = connection.Close()
	if err := <-accepted; err != nil {
		t.Fatal(err)
	}
}

func TestLongRuntimeSocketPathUsesShortListenerAndExposesMountSource(t *testing.T) {
	directory := filepath.Join(t.TempDir(), strings.Repeat("long-runtime-segment", 4))
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	exposed := filepath.Join(directory, "component.endpoint")
	if len(exposed) < 104 {
		t.Fatalf("test path is only %d bytes", len(exposed))
	}
	listener, err := listenUnix(exposed, 0666)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(exposed)
		_ = os.Remove(socketListenPath(exposed))
	}()
	info, err := os.Lstat(exposed)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("exposed mount source is not a socket: %s", info.Mode())
	}
	accepted := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err == nil {
			_ = connection.Close()
		}
		accepted <- err
	}()
	connection, err := net.Dial("unix", socketListenPath(exposed))
	if err != nil {
		t.Fatal(err)
	}
	_ = connection.Close()
	if err := <-accepted; err != nil {
		t.Fatal(err)
	}
}

func TestLongDefaultRuntimePathUsesRuntimeFilesystemNamespace(t *testing.T) {
	runtimeRoot := "/var/run/dcomp"
	runtimeDir := filepath.Join(runtimeRoot, strings.Repeat("s", 63))
	exposed := HostSocket(
		runtimeDir,
		DirectionOutput,
		strings.Repeat("c", 63),
		strings.Repeat("e", 63),
	)
	actual := socketListenPath(exposed)
	wantRoot := filepath.Join(runtimeRoot, fmt.Sprintf(".dcomp-proxy-%d", os.Getuid()))
	if filepath.Dir(actual) != wantRoot {
		t.Fatalf("short socket directory = %q, want %q", filepath.Dir(actual), wantRoot)
	}
}

func TestCleanupRuntimeRemovesShortenedCrashArtifacts(t *testing.T) {
	name := strings.Repeat("s", 63)
	producerName := strings.Repeat("p", 63)
	consumerName := strings.Repeat("c", 63)
	outputName := strings.Repeat("o", 63)
	inputName := strings.Repeat("i", 63)
	runtimeDir := filepath.Join(t.TempDir(), name)
	config, err := NewConfig(composition.ResolvedSpec{
		Name: name,
		Components: []composition.ResolvedComponent{
			{Name: producerName, Definition: composition.Definition{Outputs: []composition.Endpoint{{Name: outputName, Service: "example.v1.Service"}}}},
			{Name: consumerName, Definition: composition.Definition{Inputs: []composition.Endpoint{{Name: inputName, Service: "example.v1.Service"}}}},
		},
		Links: []composition.Link{{
			Input:  composition.EndpointRef{Component: consumerName, Endpoint: inputName},
			Output: composition.EndpointRef{Component: producerName, Endpoint: outputName},
		}},
	}, runtimeDir, "crashed-instance")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(runtimeDir, "in"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(runtimeDir, "out"), 0700); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, ConfigFileName), encoded, 0600); err != nil {
		t.Fatal(err)
	}

	paths := []string{controlSocketFile(runtimeDir)}
	for _, endpoint := range config.Endpoints {
		paths = append(paths, endpoint.Socket)
	}
	for _, path := range paths {
		listener, err := listenUnix(path, 0600)
		if err != nil {
			t.Fatal(err)
		}
		unixListener, ok := listener.(*net.UnixListener)
		if !ok {
			t.Fatalf("listener type = %T", listener)
		}
		unixListener.SetUnlinkOnClose(false)
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := cleanupRuntime(processForConfig(config, 0)); err != nil {
		t.Fatal(err)
	}
	for _, exposed := range paths {
		for _, path := range []string{exposed, socketListenPath(exposed)} {
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stale socket %s remains: %v", path, err)
			}
		}
	}
	if _, err := os.Stat(runtimeDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("runtime directory remains: %v", err)
	}
}

func testConfig(t *testing.T, fanout bool) Config {
	t.Helper()
	runtimeRoot, err := os.MkdirTemp("/tmp", "dcomp-proxy-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeRoot) })
	components := []composition.ResolvedComponent{
		{
			Name: "source",
			Definition: composition.Definition{Outputs: []composition.Endpoint{{
				Name: "documents", Service: "example.document.v1.Documents",
			}}},
		},
		{
			Name: "filter",
			Definition: composition.Definition{Inputs: []composition.Endpoint{{
				Name: "documents", Service: "example.document.v1.Documents",
			}}},
		},
	}
	links := []composition.Link{{
		Input:  composition.EndpointRef{Component: "filter", Endpoint: "documents"},
		Output: composition.EndpointRef{Component: "source", Endpoint: "documents"},
	}}
	if fanout {
		components = append(components, composition.ResolvedComponent{
			Name: "archive",
			Definition: composition.Definition{Inputs: []composition.Endpoint{{
				Name: "documents", Service: "example.document.v1.Documents",
			}}},
		})
		links = append(links, composition.Link{
			Input:  composition.EndpointRef{Component: "archive", Endpoint: "documents"},
			Output: composition.EndpointRef{Component: "source", Endpoint: "documents"},
		})
	}
	config, err := NewConfig(composition.ResolvedSpec{
		Name: "demo", Components: components, Links: links,
	}, filepath.Join(runtimeRoot, "demo"), "test-instance")
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func startTestProxy(t *testing.T, config Config) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- Run(ctx, config, func(Status) error {
			close(ready)
			return nil
		})
	}()
	select {
	case <-ready:
	case err := <-result:
		t.Fatalf("proxy failed before readiness: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("proxy did not become ready")
	}
	return cancel, result
}

func dialUnix(t *testing.T, path string) net.Conn {
	t.Helper()
	connection, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return connection
}

func writeAndRead(t *testing.T, writer, reader net.Conn, value string) {
	t.Helper()
	if _, err := io.WriteString(writer, value); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, len(value))
	if _, err := io.ReadFull(reader, buffer); err != nil {
		t.Fatal(err)
	}
	if got := string(buffer); got != value {
		t.Fatalf("forwarded value = %q, want %q", got, value)
	}
}
