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
	"sync"
	"syscall"
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

func TestProxyReportsPerLinkConnectionAndByteMetrics(t *testing.T) {
	config := testConfig(t, true)
	cancel, result := startTestProxy(t, config)
	defer func() {
		cancel()
		if err := <-result; err != nil {
			t.Fatalf("proxy Run: %v", err)
		}
	}()

	filterProducer := dialUnix(t, HostSocket(
		config.RuntimeDir, DirectionOutput, "source", "documents",
	))
	filterConsumer := dialUnix(t, HostSocket(
		config.RuntimeDir, DirectionInput, "filter", "documents",
	))
	defer filterProducer.Close()
	defer filterConsumer.Close()
	writeAndRead(t, filterConsumer, filterProducer, "filter request")
	writeAndRead(t, filterProducer, filterConsumer, "filter response")

	archiveProducer := dialUnix(t, HostSocket(
		config.RuntimeDir, DirectionOutput, "source", "documents",
	))
	archiveConsumer := dialUnix(t, HostSocket(
		config.RuntimeDir, DirectionInput, "archive", "documents",
	))
	defer archiveProducer.Close()
	defer archiveConsumer.Close()
	writeAndRead(t, archiveConsumer, archiveProducer, "archive request")
	writeAndRead(t, archiveProducer, archiveConsumer, "archive response")

	status, err := (&ProcessManager{}).Inspect(
		context.Background(), processForConfig(config, os.Getpid()),
	)
	if err != nil {
		t.Fatal(err)
	}
	if status.ActiveConnections != 2 || len(status.Links) != 2 {
		t.Fatalf("proxy metrics = %#v", status)
	}
	byInput := make(map[string]LinkMetrics, len(status.Links))
	for _, link := range status.Links {
		byInput[link.InputComponent+"."+link.InputEndpoint] = link
	}
	assertLinkMetrics := func(
		name string,
		requestBytes,
		responseBytes uint64,
	) {
		t.Helper()
		metrics, exists := byInput[name]
		if !exists {
			t.Fatalf("metrics for %s are absent: %#v", name, status.Links)
		}
		if metrics.ActiveConnections != 1 ||
			metrics.BytesInputToOutput != requestBytes ||
			metrics.BytesOutputToInput != responseBytes {
			t.Fatalf("metrics for %s = %#v", name, metrics)
		}
	}
	assertLinkMetrics("filter.documents", uint64(len("filter request")), uint64(len("filter response")))
	assertLinkMetrics("archive.documents", uint64(len("archive request")), uint64(len("archive response")))

	_ = filterConsumer.Close()
	_ = filterProducer.Close()
	_ = archiveConsumer.Close()
	_ = archiveProducer.Close()
	deadline := time.Now().Add(time.Second)
	for {
		status, err = (&ProcessManager{}).Inspect(
			context.Background(), processForConfig(config, os.Getpid()),
		)
		if err != nil {
			t.Fatal(err)
		}
		allInactive := status.ActiveConnections == 0
		for _, metrics := range status.Links {
			allInactive = allInactive && metrics.ActiveConnections == 0
		}
		if allInactive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("link gauges remained active after disconnect: %#v", status.Links)
		}
		time.Sleep(5 * time.Millisecond)
	}
	for _, metrics := range status.Links {
		if metrics.BytesInputToOutput == 0 || metrics.BytesOutputToInput == 0 {
			t.Fatalf("disconnect cleared cumulative counters: %#v", metrics)
		}
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

func TestControlRequiresCurrentProtocolVersionForEveryCommand(t *testing.T) {
	commands := []string{"status", "shutdown", "resync", "unknown"}
	versions := []struct {
		name    string
		version int
	}{
		{name: "missing", version: 0},
		{name: "future", version: ControlProtocolVersion + 1},
	}
	for _, command := range commands {
		for _, version := range versions {
			t.Run(command+"/"+version.name, func(t *testing.T) {
				config := testConfig(t, false)
				cancel, result := startTestProxy(t, config)
				defer func() {
					cancel()
					if err := <-result; err != nil {
						t.Fatalf("proxy Run: %v", err)
					}
				}()

				request := ControlRequest{
					Command: command, InstanceID: config.InstanceID,
					ProtocolVersion: version.version,
				}
				if command == "resync" {
					wiring := config.Wiring()
					request.Wiring = &wiring
					request.Digest = config.Digest
				}
				response := controlRoundTrip(t, config.RuntimeDir, request)
				if !strings.Contains(response.Error, "unsupported control protocol version") {
					t.Fatalf("response error = %q", response.Error)
				}

				// In particular, a rejected shutdown must leave the proxy live.
				status := controlRoundTrip(t, config.RuntimeDir, ControlRequest{
					Command: "status", InstanceID: config.InstanceID,
					ProtocolVersion: ControlProtocolVersion,
				})
				if status.Error != "" || !status.Ready {
					t.Fatalf("status after rejected %s = %#v", command, status)
				}
			})
		}
	}
}

func TestShutdownClosesIdleControlConnectionsBeforeCleanup(t *testing.T) {
	config := testConfig(t, false)
	_, result := startTestProxy(t, config)
	idle, err := net.Dial("unix", ControlSocket(config.RuntimeDir))
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()

	manager := &ProcessManager{StopTimeout: 2 * time.Second}
	process := processForConfig(config, os.Getpid())
	// A successful request made after the idle dial establishes that the
	// accept loop has observed the earlier connection.
	if _, err := manager.Inspect(context.Background(), process); err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(context.Background(), process); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("proxy Run: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("proxy shutdown returned before idle control connection cleanup completed")
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
		!strings.Contains(err.Error(), "claim proxy runtime") {
		t.Fatalf("second proxy error = %v", err)
	}
	pidData, err := os.ReadFile(filepath.Join(config.RuntimeDir, PIDFileName))
	if err != nil {
		t.Fatalf("live proxy PID marker was removed: %v", err)
	}
	if pid, err := ParsePIDFile(pidData); err != nil || pid != os.Getpid() {
		t.Fatalf("live proxy PID marker = %q, error = %v", pidData, err)
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
	wrong.InstanceID = "different-recorded-instance"
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

func TestResyncPreservesRetainedInodesStreamsAndMetrics(t *testing.T) {
	config := testConfig(t, true)
	cancel, result := startTestProxy(t, config)
	defer func() {
		cancel()
		if err := <-result; err != nil {
			t.Fatalf("proxy Run: %v", err)
		}
	}()

	producerPath := HostSocket(config.RuntimeDir, DirectionOutput, "source", "documents")
	consumerPath := HostSocket(config.RuntimeDir, DirectionInput, "filter", "documents")
	producerInode := socketInode(t, producerPath)
	consumerInode := socketInode(t, consumerPath)
	producer := dialUnix(t, producerPath)
	consumer := dialUnix(t, consumerPath)
	defer producer.Close()
	defer consumer.Close()
	writeAndRead(t, consumer, producer, "before")

	target := config.Wiring()
	target.Endpoints = filterEndpointIdentities(target.Endpoints, func(endpoint EndpointIdentity) bool {
		return endpoint.Component != "archive"
	})
	target.Links = filterLinks(target.Links, func(link Link) bool {
		return link.InputComponent != "archive"
	})
	digest, err := target.Digest()
	if err != nil {
		t.Fatal(err)
	}
	manager := &ProcessManager{}
	process := processForConfig(config, os.Getpid())
	status, err := manager.Resync(context.Background(), process, target, digest)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Ready || status.Digest != digest {
		t.Fatalf("resync status = %#v", status)
	}
	if got := socketInode(t, producerPath); got != producerInode {
		t.Fatalf("retained output inode changed: got %d, want %d", got, producerInode)
	}
	if got := socketInode(t, consumerPath); got != consumerInode {
		t.Fatalf("retained input inode changed: got %d, want %d", got, consumerInode)
	}
	writeAndRead(t, consumer, producer, "after-removal")

	// Re-adding the unrelated endpoint/link still leaves the established pair
	// and retained endpoint inodes untouched.
	original := config.Wiring()
	originalDigest, err := original.Digest()
	if err != nil {
		t.Fatal(err)
	}
	status, err = manager.Resync(context.Background(), process, original, originalDigest)
	if err != nil {
		t.Fatal(err)
	}
	writeAndRead(t, producer, consumer, "after-readd")
	status, err = manager.Inspect(context.Background(), process)
	if err != nil {
		t.Fatal(err)
	}
	if got := socketInode(t, producerPath); got != producerInode {
		t.Fatalf("retained output inode changed after re-add: got %d, want %d", got, producerInode)
	}
	filterMetrics := findLinkMetrics(t, status.Links, "filter", "documents")
	if filterMetrics.BytesInputToOutput < uint64(len("before")+len("after-removal")) ||
		filterMetrics.BytesOutputToInput < uint64(len("after-readd")) {
		t.Fatalf("surviving link counters were reset: %#v", filterMetrics)
	}

	// A repeated resync to the installed digest is an observable no-op.
	beforeNoOp := findLinkMetrics(t, status.Links, "filter", "documents")
	status, err = manager.Resync(context.Background(), process, original, originalDigest)
	if err != nil {
		t.Fatal(err)
	}
	afterNoOp := findLinkMetrics(t, status.Links, "filter", "documents")
	if beforeNoOp != afterNoOp {
		t.Fatalf("idempotent resync changed metrics: before=%#v after=%#v", beforeNoOp, afterNoOp)
	}
}

func TestRemovedThenReaddedLinkMetricsRestartAtZero(t *testing.T) {
	config := testConfig(t, true)
	cancel, result := startTestProxy(t, config)
	defer func() {
		cancel()
		if err := <-result; err != nil {
			t.Fatalf("proxy Run: %v", err)
		}
	}()

	producer := dialUnix(t, HostSocket(
		config.RuntimeDir, DirectionOutput, "source", "documents",
	))
	consumer := dialUnix(t, HostSocket(
		config.RuntimeDir, DirectionInput, "archive", "documents",
	))
	writeAndRead(t, consumer, producer, "request-before-removal")
	writeAndRead(t, producer, consumer, "response-before-removal")
	_ = consumer.Close()
	_ = producer.Close()

	manager := &ProcessManager{}
	process := processForConfig(config, os.Getpid())
	deadline := time.Now().Add(time.Second)
	for {
		status, err := manager.Inspect(context.Background(), process)
		if err != nil {
			t.Fatal(err)
		}
		metrics := findLinkMetrics(t, status.Links, "archive", "documents")
		if metrics.ActiveConnections == 0 &&
			metrics.BytesInputToOutput > 0 && metrics.BytesOutputToInput > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("archive metrics did not settle before removal: %#v", metrics)
		}
		time.Sleep(5 * time.Millisecond)
	}

	withoutArchive := config.Wiring()
	withoutArchive.Endpoints = filterEndpointIdentities(
		withoutArchive.Endpoints,
		func(endpoint EndpointIdentity) bool { return endpoint.Component != "archive" },
	)
	withoutArchive.Links = filterLinks(
		withoutArchive.Links,
		func(link Link) bool { return link.InputComponent != "archive" },
	)
	withoutArchiveDigest, err := withoutArchive.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Resync(
		context.Background(), process, withoutArchive, withoutArchiveDigest,
	); err != nil {
		t.Fatal(err)
	}

	original := config.Wiring()
	originalDigest, err := original.Digest()
	if err != nil {
		t.Fatal(err)
	}
	status, err := manager.Resync(
		context.Background(), process, original, originalDigest,
	)
	if err != nil {
		t.Fatal(err)
	}
	metrics := findLinkMetrics(t, status.Links, "archive", "documents")
	if metrics.ActiveConnections != 0 || metrics.BytesInputToOutput != 0 ||
		metrics.BytesOutputToInput != 0 {
		t.Fatalf("recreated link inherited removed metrics: %#v", metrics)
	}
}

func TestResyncClosesRemovedLinkPairOnBothSides(t *testing.T) {
	config := testConfig(t, true)
	cancel, result := startTestProxy(t, config)
	defer func() {
		cancel()
		if err := <-result; err != nil {
			t.Fatalf("proxy Run: %v", err)
		}
	}()

	producer := dialUnix(t, HostSocket(
		config.RuntimeDir, DirectionOutput, "source", "documents",
	))
	consumer := dialUnix(t, HostSocket(
		config.RuntimeDir, DirectionInput, "archive", "documents",
	))
	defer producer.Close()
	defer consumer.Close()
	writeAndRead(t, consumer, producer, "paired")

	target := config.Wiring()
	target.Endpoints = filterEndpointIdentities(target.Endpoints, func(endpoint EndpointIdentity) bool {
		return endpoint.Component != "archive"
	})
	target.Links = filterLinks(target.Links, func(link Link) bool {
		return link.InputComponent != "archive"
	})
	digest, err := target.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (&ProcessManager{}).Resync(
		context.Background(), processForConfig(config, os.Getpid()), target, digest,
	); err != nil {
		t.Fatal(err)
	}
	assertConnectionClosedPromptly(t, producer)
	assertConnectionClosedPromptly(t, consumer)
}

func TestResyncClosesPendingInputCapturedByRemovedLink(t *testing.T) {
	config := testConfig(t, true)
	cancel, result := startTestProxy(t, config)
	defer func() {
		cancel()
		if err := <-result; err != nil {
			t.Fatalf("proxy Run: %v", err)
		}
	}()
	manager := &ProcessManager{}
	process := processForConfig(config, os.Getpid())
	consumer := dialUnix(t, HostSocket(
		config.RuntimeDir, DirectionInput, "archive", "documents",
	))
	defer consumer.Close()
	waitForPendingInputs(t, manager, process, 1)

	target := wiringWithoutComponent(config.Wiring(), "archive")
	digest, err := target.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Resync(context.Background(), process, target, digest); err != nil {
		t.Fatal(err)
	}
	assertConnectionClosedPromptly(t, consumer)
}

func TestResyncRetainsFanInOutputPoolUntilLastConsumerIsRemoved(t *testing.T) {
	config := testConfig(t, true)
	cancel, result := startTestProxy(t, config)
	defer func() {
		cancel()
		if err := <-result; err != nil {
			t.Fatalf("proxy Run: %v", err)
		}
	}()
	manager := &ProcessManager{}
	process := processForConfig(config, os.Getpid())
	outputPath := HostSocket(config.RuntimeDir, DirectionOutput, "source", "documents")
	producer := dialUnix(t, outputPath)
	defer producer.Close()
	waitForPendingOutputs(t, manager, process, 1)

	archiveOnly := config.Wiring()
	archiveOnly.Endpoints = filterEndpointIdentities(
		archiveOnly.Endpoints,
		func(endpoint EndpointIdentity) bool { return endpoint.Component != "filter" },
	)
	archiveOnly.Links = filterLinks(
		archiveOnly.Links,
		func(link Link) bool { return link.InputComponent != "filter" },
	)
	digest, err := archiveOnly.Digest()
	if err != nil {
		t.Fatal(err)
	}
	status, err := manager.Resync(context.Background(), process, archiveOnly, digest)
	if err != nil {
		t.Fatal(err)
	}
	if status.PendingOutputs != 1 {
		t.Fatalf("pending output was not retained across fan-in removal: %#v", status)
	}
	consumer := dialUnix(t, HostSocket(
		config.RuntimeDir, DirectionInput, "archive", "documents",
	))
	writeAndRead(t, consumer, producer, "survived")
	_ = consumer.Close()
	_ = producer.Close()

	lastProducer := dialUnix(t, outputPath)
	defer lastProducer.Close()
	waitForPendingOutputs(t, manager, process, 1)
	outputOnly := Wiring{Endpoints: []EndpointIdentity{{
		Component: "source", Name: "documents", Direction: DirectionOutput,
	}}}
	lastDigest, err := outputOnly.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Resync(
		context.Background(), process, outputOnly, lastDigest,
	); err != nil {
		t.Fatal(err)
	}
	assertConnectionClosedPromptly(t, lastProducer)
}

func TestProducerReconnectClosesWhenOutputHasNoConsumers(t *testing.T) {
	config := testConfig(t, false)
	cancel, result := startTestProxy(t, config)
	defer func() {
		cancel()
		if err := <-result; err != nil {
			t.Fatalf("proxy Run: %v", err)
		}
	}()
	manager := &ProcessManager{}
	process := processForConfig(config, os.Getpid())
	outputOnly := Wiring{Endpoints: []EndpointIdentity{{
		Component: "source", Name: "documents", Direction: DirectionOutput,
	}}}
	digest, err := outputOnly.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Resync(
		context.Background(), process, outputOnly, digest,
	); err != nil {
		t.Fatal(err)
	}

	producer := dialUnix(t, HostSocket(
		config.RuntimeDir, DirectionOutput, "source", "documents",
	))
	assertConnectionClosedPromptly(t, producer)
	_ = producer.Close()
	status, err := manager.Inspect(context.Background(), process)
	if err != nil {
		t.Fatal(err)
	}
	if status.PendingOutputs != 0 {
		t.Fatalf("unconsumable producer entered pending pool: %#v", status)
	}

	restored := config.Wiring()
	restoredDigest, err := restored.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Resync(
		context.Background(), process, restored, restoredDigest,
	); err != nil {
		t.Fatal(err)
	}
	producer = dialUnix(t, HostSocket(
		config.RuntimeDir, DirectionOutput, "source", "documents",
	))
	defer producer.Close()
	waitForPendingOutputs(t, manager, process, 1)
	consumer := dialUnix(t, HostSocket(
		config.RuntimeDir, DirectionInput, "filter", "documents",
	))
	defer consumer.Close()
	writeAndRead(t, consumer, producer, "available again")
}

func TestResyncPrepareRefusesOccupiedTargetAndKeepsOldWiring(t *testing.T) {
	config := testConfig(t, false)
	cancel, result := startTestProxy(t, config)
	defer func() {
		cancel()
		if err := <-result; err != nil {
			t.Fatalf("proxy Run: %v", err)
		}
	}()
	retainedPath := HostSocket(config.RuntimeDir, DirectionInput, "filter", "documents")
	retainedInode := socketInode(t, retainedPath)
	target := config.Wiring()
	target.Endpoints = append(target.Endpoints, EndpointIdentity{
		Component: "archive", Name: "documents", Direction: DirectionInput,
	})
	target.Links = append(target.Links, Link{
		InputComponent: "archive", InputEndpoint: "documents",
		OutputComponent: "source", OutputEndpoint: "documents",
	})
	digest, err := target.Digest()
	if err != nil {
		t.Fatal(err)
	}
	occupied := HostSocket(config.RuntimeDir, DirectionInput, "archive", "documents")
	if err := os.WriteFile(occupied, []byte("operator-owned"), 0600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(occupied)
	manager := &ProcessManager{}
	process := processForConfig(config, os.Getpid())
	if _, err := manager.Resync(context.Background(), process, target, digest); err == nil ||
		!strings.Contains(err.Error(), "occupied target") {
		t.Fatalf("resync error = %v", err)
	}
	status, err := manager.Inspect(context.Background(), process)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Ready || status.Digest != config.Digest {
		t.Fatalf("prepare failure changed live wiring: %#v", status)
	}
	if got := socketInode(t, retainedPath); got != retainedInode {
		t.Fatalf("prepare failure changed retained inode: got %d, want %d", got, retainedInode)
	}
	data, err := os.ReadFile(occupied)
	if err != nil || string(data) != "operator-owned" {
		t.Fatalf("occupied target was replaced: data=%q error=%v", data, err)
	}
}

func TestPublicationFailureDoesNotRemoveConcurrentTargetOccupant(t *testing.T) {
	for _, shortened := range []bool{false, true} {
		name := "direct"
		if shortened {
			name = "shortened"
		}
		t.Run(name, func(t *testing.T) {
			config := testConfig(t, false)
			if shortened {
				config = configWithLongRuntime(t, config)
			}
			for _, directory := range []string{"in", "out"} {
				if err := os.MkdirAll(filepath.Join(config.RuntimeDir, directory), 0700); err != nil {
					t.Fatal(err)
				}
			}
			ownership := createTestOwnershipLedger(t, config)
			final := HostSocket(
				config.RuntimeDir, DirectionInput, "archive", "documents",
			)
			publication, err := prepareSocketPublication(
				ownership, final, 0666, nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			const contents = "concurrent-owner"
			if err := os.WriteFile(final, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			if err := publication.Publish(); err == nil {
				t.Fatal("publish unexpectedly replaced a concurrent target occupant")
			}
			for _, owned := range publication.Paths() {
				if owned == final {
					t.Fatal("unpublished final path was marked as publication-owned")
				}
			}
			if err := publication.Abort(); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(final)
			if err != nil || string(data) != contents {
				t.Fatalf(
					"concurrent target occupant changed: data=%q error=%v",
					data,
					err,
				)
			}
		})
	}
}

func TestResyncPublicationFailuresExposeOnlyAllowedDigestStatesAndRetry(t *testing.T) {
	tests := []struct {
		name          string
		step          string
		shortened     bool
		wantOldDigest bool
	}{
		{name: "ownership-claim", step: resyncStepPrepareClaim, wantOldDigest: true},
		{name: "bind", step: resyncStepPrepareBind, wantOldDigest: true},
		{name: "shortened-link", step: resyncStepPrepareLink, shortened: true, wantOldDigest: true},
		{name: "commit-locked", step: resyncStepCommitLocked},
		{name: "publish", step: resyncStepPublish},
		{name: "persist-config", step: resyncStepPersistConfig},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := testConfig(t, false)
			if test.shortened {
				config = configWithLongRuntime(t, config)
			}
			fault := &oneShotResyncFault{step: test.step}
			cancel, result := startTestProxyWithFaults(t, config, fault.inject)
			defer func() {
				cancel()
				if err := <-result; err != nil {
					t.Fatalf("proxy Run: %v", err)
				}
			}()

			target := wiringWithArchive(config.Wiring())
			digest, err := target.Digest()
			if err != nil {
				t.Fatal(err)
			}
			manager := &ProcessManager{}
			process := processForConfig(config, os.Getpid())
			response, err := manager.Resync(context.Background(), process, target, digest)
			if err == nil || !strings.Contains(err.Error(), test.step) {
				t.Fatalf("first Resync error = %v", err)
			}
			if !fault.didFire() {
				t.Fatalf("fault %q did not fire", test.step)
			}
			if test.wantOldDigest {
				if !response.Ready || response.Digest != config.Digest {
					t.Fatalf("prepare failure status = %#v, want old digest", response)
				}
			} else if response.Ready || response.Digest != "" {
				t.Fatalf("post-commit failure status = %#v, want unconverged", response)
			}

			response, err = manager.Resync(context.Background(), process, target, digest)
			if err != nil {
				t.Fatalf("retry Resync: %v", err)
			}
			if !response.Ready || response.Digest != digest {
				t.Fatalf("retry status = %#v", response)
			}
			assertNoPublicationArtifacts(t, config.RuntimeDir)
		})
	}
}

func TestConcurrentPairRegistrationObservesCompleteRelink(t *testing.T) {
	config := crossRelinkConfig(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var blockOnce sync.Once
	faults := func(step string) error {
		if step == resyncStepCommitLocked {
			blockOnce.Do(func() {
				close(entered)
				<-release
			})
		}
		return nil
	}
	cancel, result := startTestProxyWithFaults(t, config, faults)
	defer func() {
		cancel()
		if err := <-result; err != nil {
			t.Fatalf("proxy Run: %v", err)
		}
	}()

	producerX := dialUnix(t, HostSocket(
		config.RuntimeDir, DirectionOutput, "provider-x", "stream",
	))
	producerY := dialUnix(t, HostSocket(
		config.RuntimeDir, DirectionOutput, "provider-y", "stream",
	))
	defer producerX.Close()
	defer producerY.Close()
	manager := &ProcessManager{}
	process := processForConfig(config, os.Getpid())
	waitForPendingOutputs(t, manager, process, 2)

	target := config.Wiring()
	for index := range target.Links {
		link := &target.Links[index]
		switch link.InputComponent {
		case "consumer-a":
			link.OutputComponent = "provider-y"
		case "consumer-b":
			link.OutputComponent = "provider-x"
		}
	}
	digest, err := target.Digest()
	if err != nil {
		t.Fatal(err)
	}
	type resyncResult struct {
		status Status
		err    error
	}
	resynced := make(chan resyncResult, 1)
	go func() {
		status, err := manager.Resync(context.Background(), process, target, digest)
		resynced <- resyncResult{status: status, err: err}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("resync did not reach locked commit")
	}

	// These accepts occur while the wiring lock is held. Registration must wait
	// until the complete cross-swap is visible; neither input may capture one
	// half of the old routing table.
	consumerA := dialUnix(t, HostSocket(
		config.RuntimeDir, DirectionInput, "consumer-a", "stream",
	))
	consumerB := dialUnix(t, HostSocket(
		config.RuntimeDir, DirectionInput, "consumer-b", "stream",
	))
	defer consumerA.Close()
	defer consumerB.Close()
	close(release)
	completed := <-resynced
	if completed.err != nil {
		t.Fatal(completed.err)
	}
	if !completed.status.Ready || completed.status.Digest != digest {
		t.Fatalf("resync status = %#v", completed.status)
	}
	writeAndRead(t, consumerA, producerY, "a-to-y")
	writeAndRead(t, consumerB, producerX, "b-to-x")
}

func TestResyncTeardownUnlinkFailureRetriesForDirectAndShortenedSockets(t *testing.T) {
	for _, shortened := range []bool{false, true} {
		name := "direct"
		if shortened {
			name = "shortened"
		}
		t.Run(name, func(t *testing.T) {
			config := testConfig(t, true)
			if shortened {
				config = configWithLongRuntime(t, config)
			}
			fault := &oneShotResyncFault{step: resyncStepTeardownUnlink}
			cancel, result := startTestProxyWithFaults(t, config, fault.inject)
			defer func() {
				cancel()
				if err := <-result; err != nil {
					t.Fatalf("proxy Run: %v", err)
				}
			}()

			target := wiringWithoutComponent(config.Wiring(), "archive")
			digest, err := target.Digest()
			if err != nil {
				t.Fatal(err)
			}
			archivePath := HostSocket(
				config.RuntimeDir, DirectionInput, "archive", "documents",
			)
			manager := &ProcessManager{}
			process := processForConfig(config, os.Getpid())
			response, err := manager.Resync(context.Background(), process, target, digest)
			if err == nil || !strings.Contains(err.Error(), resyncStepTeardownUnlink) {
				t.Fatalf("first Resync error = %v", err)
			}
			if response.Ready || response.Digest != "" {
				t.Fatalf("teardown failure status = %#v, want unconverged", response)
			}
			if _, err := os.Lstat(archivePath); err != nil {
				t.Fatalf("failed unlink did not leave retry artifact: %v", err)
			}

			response, err = manager.Resync(context.Background(), process, target, digest)
			if err != nil {
				t.Fatalf("retry Resync: %v", err)
			}
			if !response.Ready || response.Digest != digest {
				t.Fatalf("retry status = %#v", response)
			}
			for _, path := range []string{archivePath, socketListenPath(archivePath)} {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("removed endpoint artifact %s remains: %v", path, err)
				}
			}
		})
	}
}

// Teardown is a retryable per-endpoint operation. Once one endpoint has been
// removed and disowned, a later endpoint's failure must not put the completed
// endpoint back into the retry set: its pathname may legitimately be occupied
// by another owner before DComp retries the resync.
func TestResyncTeardownRetryPreservesEverySuccessfullyRelinquishedPath(t *testing.T) {
	for _, shortened := range []bool{false, true} {
		publication := "direct"
		if shortened {
			publication = "shortened"
		}
		for failAt := 1; failAt <= 3; failAt++ {
			t.Run(fmt.Sprintf("%s/failure-%d", publication, failAt), func(t *testing.T) {
				config := testConfig(t, true)
				if shortened {
					config = configWithLongRuntime(t, config)
				}
				var faultMu sync.Mutex
				unlinkCalls := 0
				faults := func(step string) error {
					if step != resyncStepTeardownUnlink {
						return nil
					}
					faultMu.Lock()
					defer faultMu.Unlock()
					unlinkCalls++
					if unlinkCalls == failAt {
						return errors.New("selected teardown failure")
					}
					return nil
				}
				cancel, result := startTestProxyWithFaults(t, config, faults)
				defer func() {
					cancel()
					if err := <-result; err != nil {
						t.Fatalf("proxy Run: %v", err)
					}
				}()

				target := Wiring{}
				digest, err := target.Digest()
				if err != nil {
					t.Fatal(err)
				}
				manager := &ProcessManager{}
				process := processForConfig(config, os.Getpid())
				response, err := manager.Resync(
					context.Background(), process, target, digest,
				)
				if err == nil || !strings.Contains(err.Error(), "selected teardown failure") {
					t.Fatalf("first Resync error = %v, want selected teardown failure", err)
				}
				if response.Ready || response.Digest != "" {
					t.Fatalf("teardown failure status = %#v, want unconverged", response)
				}
				faultMu.Lock()
				gotUnlinkCalls := unlinkCalls
				faultMu.Unlock()
				if gotUnlinkCalls != len(config.Endpoints) {
					t.Fatalf(
						"first teardown unlink calls = %d, want %d",
						gotUnlinkCalls,
						len(config.Endpoints),
					)
				}

				type foreignOccupant struct {
					listener net.Listener
					paths    []string
				}
				var occupants []foreignOccupant
				for _, endpoint := range config.Endpoints {
					final := endpoint.Socket
					actual := socketListenPath(final)
					_, finalErr := os.Lstat(final)
					_, actualErr := os.Lstat(actual)
					removed := errors.Is(finalErr, os.ErrNotExist) &&
						errors.Is(actualErr, os.ErrNotExist)
					if !removed {
						if finalErr != nil || actualErr != nil {
							t.Fatalf(
								"partially removed endpoint %s: final=%v actual=%v",
								endpointIdentityKey(EndpointIdentity{
									Component: endpoint.Component,
									Name:      endpoint.Name,
									Direction: endpoint.Direction,
								}),
								finalErr,
								actualErr,
							)
						}
						continue
					}

					occupant := foreignOccupant{}
					if actual != final {
						if err := os.WriteFile(final, []byte("foreign owner\n"), 0600); err != nil {
							t.Fatal(err)
						}
						occupant.paths = append(occupant.paths, final)
					}
					listener, err := net.Listen("unix", actual)
					if err != nil {
						t.Fatalf("reoccupy relinquished path %s: %v", actual, err)
					}
					occupant.listener = listener
					occupant.paths = append(occupant.paths, actual)
					occupants = append(occupants, occupant)
				}
				if got, want := len(occupants), len(config.Endpoints)-1; got != want {
					t.Fatalf("successfully removed endpoints = %d, want %d", got, want)
				}
				defer func() {
					for _, occupant := range occupants {
						_ = occupant.listener.Close()
						for _, path := range occupant.paths {
							_ = os.Remove(path)
						}
					}
				}()

				response, err = manager.Resync(
					context.Background(), process, target, digest,
				)
				if err != nil {
					t.Fatalf("retry Resync: %v", err)
				}
				if !response.Ready || response.Digest != digest {
					t.Fatalf("retry status = %#v", response)
				}
				for _, occupant := range occupants {
					for _, path := range occupant.paths {
						if _, err := os.Lstat(path); err != nil {
							t.Fatalf(
								"retry removed path relinquished after partial teardown %s: %v",
								path,
								err,
							)
						}
					}
				}
			})
		}
	}
}

func TestStatusReportsNoDigestWhileResyncTeardownIsInProgress(t *testing.T) {
	config := testConfig(t, true)
	entered := make(chan struct{})
	release := make(chan struct{})
	var blockOnce sync.Once
	faults := func(step string) error {
		if step == resyncStepTeardownUnlink {
			blockOnce.Do(func() {
				close(entered)
				<-release
			})
		}
		return nil
	}
	cancel, result := startTestProxyWithFaults(t, config, faults)
	defer func() {
		cancel()
		if err := <-result; err != nil {
			t.Fatalf("proxy Run: %v", err)
		}
	}()
	manager := &ProcessManager{}
	process := processForConfig(config, os.Getpid())
	target := wiringWithoutComponent(config.Wiring(), "archive")
	digest, err := target.Digest()
	if err != nil {
		t.Fatal(err)
	}
	resynced := make(chan error, 1)
	go func() {
		_, err := manager.Resync(context.Background(), process, target, digest)
		resynced <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("resync did not reach teardown")
	}
	status, err := manager.Inspect(context.Background(), process)
	if err != nil {
		t.Fatal(err)
	}
	if status.Ready || status.Digest != "" {
		t.Fatalf("in-progress status = %#v, want ready=false without digest", status)
	}
	close(release)
	if err := <-resynced; err != nil {
		t.Fatal(err)
	}
}

func TestShortenedPathResyncPublishesRoutesAndPreservesRetainedInodes(t *testing.T) {
	config := configWithLongRuntime(t, testConfig(t, false))
	cancel, result := startTestProxy(t, config)
	defer func() {
		cancel()
		if err := <-result; err != nil {
			t.Fatalf("proxy Run: %v", err)
		}
	}()

	outputPath := HostSocket(config.RuntimeDir, DirectionOutput, "source", "documents")
	retainedInode := socketInode(t, outputPath)
	target := wiringWithArchive(config.Wiring())
	digest, err := target.Digest()
	if err != nil {
		t.Fatal(err)
	}
	manager := &ProcessManager{}
	process := processForConfig(config, os.Getpid())
	if _, err := manager.Resync(context.Background(), process, target, digest); err != nil {
		t.Fatal(err)
	}
	archivePath := HostSocket(config.RuntimeDir, DirectionInput, "archive", "documents")
	archiveActual := shortenedSocketPath(archivePath)
	if archiveActual == archivePath || len(publicationTemporaryPath(archivePath, config.InstanceID)) < 104 {
		t.Fatalf("test did not select shortened publication: final=%q actual=%q", archivePath, archiveActual)
	}
	if socketInode(t, archivePath) != socketInode(t, archiveActual) {
		t.Fatal("shortened listener and exposed mount source are not hard links")
	}
	if got := socketInode(t, outputPath); got != retainedInode {
		t.Fatalf("retained output inode changed: got %d, want %d", got, retainedInode)
	}

	producer := dialUnix(t, socketListenPath(outputPath))
	consumer := dialUnix(t, archiveActual)
	writeAndRead(t, consumer, producer, "shortened-resync")
	_ = consumer.Close()
	_ = producer.Close()

	target = wiringWithoutComponent(target, "archive")
	digest, err = target.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Resync(context.Background(), process, target, digest); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{archivePath, archiveActual} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("shortened teardown left %s: %v", path, err)
		}
	}
	if got := socketInode(t, outputPath); got != retainedInode {
		t.Fatalf("retained output inode changed during teardown: got %d, want %d", got, retainedInode)
	}
}

func TestShutdownPreservesPathsRelinquishedBySuccessfulResync(t *testing.T) {
	for _, shortened := range []bool{false, true} {
		name := "direct"
		if shortened {
			name = "shortened"
		}
		t.Run(name, func(t *testing.T) {
			config := testConfig(t, false)
			if shortened {
				config = configWithLongRuntime(t, config)
			}
			cancel, result := startTestProxy(t, config)
			stopped := false
			defer func() {
				if !stopped {
					cancel()
					<-result
				}
			}()

			manager := &ProcessManager{}
			process := processForConfig(config, os.Getpid())
			withArchive := wiringWithArchive(config.Wiring())
			withArchiveDigest, err := withArchive.Digest()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := manager.Resync(
				context.Background(), process, withArchive, withArchiveDigest,
			); err != nil {
				t.Fatal(err)
			}

			archivePath := HostSocket(
				config.RuntimeDir, DirectionInput, "archive", "documents",
			)
			withoutArchive := wiringWithoutComponent(withArchive, "archive")
			withoutArchiveDigest, err := withoutArchive.Digest()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := manager.Resync(
				context.Background(), process, withoutArchive, withoutArchiveDigest,
			); err != nil {
				t.Fatal(err)
			}

			actualPath := socketListenPath(archivePath)
			var foreignPaths []string
			var foreignListener net.Listener
			if shortened {
				if err := os.WriteFile(archivePath, []byte("not proxy owned\n"), 0600); err != nil {
					t.Fatal(err)
				}
				foreignPaths = append(foreignPaths, archivePath)
				foreignListener, err = net.Listen("unix", actualPath)
			} else {
				foreignListener, err = net.Listen("unix", archivePath)
			}
			if err != nil {
				t.Fatalf("reoccupy removed endpoint: %v", err)
			}
			defer foreignListener.Close()
			foreignPaths = append(foreignPaths, actualPath)

			cancel()
			if err := <-result; err != nil {
				t.Fatalf("proxy Run: %v", err)
			}
			stopped = true
			for _, path := range foreignPaths {
				if _, err := os.Lstat(path); err != nil {
					t.Fatalf("relinquished path %s was removed during shutdown: %v", path, err)
				}
			}
			for _, endpoint := range config.Endpoints {
				for _, path := range []string{endpoint.Socket, socketListenPath(endpoint.Socket)} {
					if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("owned live endpoint %s remains after shutdown: %v", path, err)
					}
				}
			}
		})
	}
}

func TestCleanupRuntimeRemovesPreparedAndPublishedResyncArtifacts(t *testing.T) {
	for _, shortened := range []bool{false, true} {
		for _, published := range []bool{false, true} {
			name := "direct/prepared"
			if shortened {
				name = "shortened/prepared"
			}
			if published {
				name = strings.TrimSuffix(name, "prepared") + "published"
			}
			t.Run(name, func(t *testing.T) {
				config := testConfig(t, false)
				if shortened {
					config = configWithLongRuntime(t, config)
				}
				for _, directory := range []string{"in", "out"} {
					if err := os.MkdirAll(filepath.Join(config.RuntimeDir, directory), 0700); err != nil {
						t.Fatal(err)
					}
				}
				encoded, err := json.Marshal(config)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(
					filepath.Join(config.RuntimeDir, ConfigFileName), encoded, 0600,
				); err != nil {
					t.Fatal(err)
				}
				ownership := createTestOwnershipLedger(t, config)
				final := HostSocket(
					config.RuntimeDir, DirectionInput, "archive", "documents",
				)
				publication, err := prepareSocketPublication(
					ownership, final, 0666, nil,
				)
				if err != nil {
					t.Fatal(err)
				}
				if published {
					if err := publication.Publish(); err != nil {
						t.Fatal(err)
					}
				}
				paths := publication.Paths()
				if err := publication.Listener().Close(); err != nil {
					t.Fatal(err)
				}
				if err := cleanupCrashedRuntime(processForConfig(config, 0)); err != nil {
					t.Fatal(err)
				}
				for _, path := range paths {
					if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("crash artifact %s remains: %v", path, err)
					}
				}
				if _, err := os.Lstat(config.RuntimeDir); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("runtime directory remains: %v", err)
				}
			})
		}
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
	ownership := createTestOwnershipLedger(t, config)
	for _, path := range paths {
		publication, err := prepareSocketPublication(ownership, path, 0600, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := publication.Publish(); err != nil {
			t.Fatal(err)
		}
		if err := publication.Listener().Close(); err != nil {
			t.Fatal(err)
		}
	}
	if err := cleanupCrashedRuntime(processForConfig(config, 0)); err != nil {
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
	return startTestProxyWithFaults(t, config, nil)
}

func startTestProxyWithFaults(
	t *testing.T,
	config Config,
	faults resyncFaultInjector,
) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		result <- run(ctx, config, func(Status) error {
			close(ready)
			return nil
		}, faults)
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

type oneShotResyncFault struct {
	mu    sync.Mutex
	step  string
	fired bool
}

func (fault *oneShotResyncFault) inject(step string) error {
	fault.mu.Lock()
	defer fault.mu.Unlock()
	if fault.fired || step != fault.step {
		return nil
	}
	fault.fired = true
	return errors.New("test fault")
}

func (fault *oneShotResyncFault) didFire() bool {
	fault.mu.Lock()
	defer fault.mu.Unlock()
	return fault.fired
}

func configWithLongRuntime(t *testing.T, config Config) Config {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "dcomp-proxy-long-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	config.RuntimeDir = filepath.Join(root, strings.Repeat("r", 63), config.System)
	config.Endpoints = append([]Endpoint(nil), config.Endpoints...)
	for index := range config.Endpoints {
		endpoint := &config.Endpoints[index]
		endpoint.Socket = HostSocket(
			config.RuntimeDir, endpoint.Direction, endpoint.Component, endpoint.Name,
		)
	}
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	return config
}

func crossRelinkConfig(t *testing.T) Config {
	t.Helper()
	runtimeRoot, err := os.MkdirTemp("/tmp", "dcomp-proxy-cross-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeRoot) })
	const service = "example.stream.v1.Stream"
	config, err := NewConfig(composition.ResolvedSpec{
		Name: "cross",
		Components: []composition.ResolvedComponent{
			{Name: "provider-x", Definition: composition.Definition{Outputs: []composition.Endpoint{{Name: "stream", Service: service}}}},
			{Name: "provider-y", Definition: composition.Definition{Outputs: []composition.Endpoint{{Name: "stream", Service: service}}}},
			{Name: "consumer-a", Definition: composition.Definition{Inputs: []composition.Endpoint{{Name: "stream", Service: service}}}},
			{Name: "consumer-b", Definition: composition.Definition{Inputs: []composition.Endpoint{{Name: "stream", Service: service}}}},
		},
		Links: []composition.Link{
			{
				Input:  composition.EndpointRef{Component: "consumer-a", Endpoint: "stream"},
				Output: composition.EndpointRef{Component: "provider-x", Endpoint: "stream"},
			},
			{
				Input:  composition.EndpointRef{Component: "consumer-b", Endpoint: "stream"},
				Output: composition.EndpointRef{Component: "provider-y", Endpoint: "stream"},
			},
		},
	}, filepath.Join(runtimeRoot, "cross"), "cross-instance")
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func wiringWithArchive(wiring Wiring) Wiring {
	wiring.Endpoints = append(wiring.Endpoints, EndpointIdentity{
		Component: "archive", Name: "documents", Direction: DirectionInput,
	})
	wiring.Links = append(wiring.Links, Link{
		InputComponent: "archive", InputEndpoint: "documents",
		OutputComponent: "source", OutputEndpoint: "documents",
	})
	return wiring
}

func wiringWithoutComponent(wiring Wiring, component string) Wiring {
	wiring.Endpoints = filterEndpointIdentities(
		wiring.Endpoints,
		func(endpoint EndpointIdentity) bool { return endpoint.Component != component },
	)
	wiring.Links = filterLinks(wiring.Links, func(link Link) bool {
		return link.InputComponent != component && link.OutputComponent != component
	})
	return wiring
}

func assertNoPublicationArtifacts(t *testing.T, runtimeDir string) {
	t.Helper()
	entries, err := os.ReadDir(runtimeDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".proxy-tmp-") {
			t.Fatalf("publication artifact remains: %s", entry.Name())
		}
	}
	for _, directory := range []string{"in", "out"} {
		entries, err := os.ReadDir(filepath.Join(runtimeDir, directory))
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".p-") {
				t.Fatalf("temporary publication remains: %s/%s", directory, entry.Name())
			}
		}
	}
}

func createTestOwnershipLedger(t *testing.T, config Config) *socketOwnershipLedger {
	t.Helper()
	ledger, err := createSocketOwnershipLedger(config.RuntimeDir, config.InstanceID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = ledger.CleanupAll()
		_ = ledger.Close()
	})
	return ledger
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

func controlRoundTrip(t *testing.T, runtimeDir string, request ControlRequest) Status {
	t.Helper()
	connection := dialUnix(t, ControlSocket(runtimeDir))
	defer connection.Close()
	if err := writeControlMessage(connection, request); err != nil {
		t.Fatal(err)
	}
	var response Status
	if err := readControlMessage(connection, &response); err != nil {
		t.Fatal(err)
	}
	return response
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

func socketInode(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("socket stat type = %T", info.Sys())
	}
	return stat.Ino
}

func filterEndpointIdentities(
	input []EndpointIdentity,
	keep func(EndpointIdentity) bool,
) []EndpointIdentity {
	result := make([]EndpointIdentity, 0, len(input))
	for _, endpoint := range input {
		if keep(endpoint) {
			result = append(result, endpoint)
		}
	}
	return result
}

func filterLinks(input []Link, keep func(Link) bool) []Link {
	result := make([]Link, 0, len(input))
	for _, link := range input {
		if keep(link) {
			result = append(result, link)
		}
	}
	return result
}

func findLinkMetrics(
	t *testing.T,
	links []LinkMetrics,
	component,
	endpoint string,
) LinkMetrics {
	t.Helper()
	for _, link := range links {
		if link.InputComponent == component && link.InputEndpoint == endpoint {
			return link
		}
	}
	t.Fatalf("metrics for %s.%s are absent: %#v", component, endpoint, links)
	return LinkMetrics{}
}

func assertConnectionClosedPromptly(t *testing.T, connection net.Conn) {
	t.Helper()
	if err := connection.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var buffer [1]byte
	if _, err := connection.Read(buffer[:]); err == nil {
		t.Fatal("removed connection remained readable")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("removed connection did not close promptly")
	}
}

func waitForPendingOutputs(
	t *testing.T,
	manager *ProcessManager,
	process Process,
	want int64,
) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		status, err := manager.Inspect(context.Background(), process)
		if err != nil {
			t.Fatal(err)
		}
		if status.PendingOutputs == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pending outputs = %d, want %d", status.PendingOutputs, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitForPendingInputs(
	t *testing.T,
	manager *ProcessManager,
	process Process,
	want int64,
) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		status, err := manager.Inspect(context.Background(), process)
		if err != nil {
			t.Fatal(err)
		}
		if status.PendingInputs == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pending inputs = %d, want %d", status.PendingInputs, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
