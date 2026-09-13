package proxy

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestGlobalReassignmentPreservesDirectStreamsAndUnbindFails(t *testing.T) {
	config := testConfig(t, true)
	config.Endpoints = append(config.Endpoints, Endpoint{Component: "next", Name: "api", Direction: DirectionOutput, Socket: HostSocket(config.RuntimeDir, DirectionOutput, "next", "api")})
	config.Globals = []Global{{Name: "provider_endpoint", OutputComponent: "source", OutputEndpoint: "documents"}}
	for i := range config.Links {
		if config.Links[i].InputComponent == "filter" {
			config.Links[i].Global = "provider_endpoint"
			config.Links[i].OutputComponent = ""
			config.Links[i].OutputEndpoint = ""
		}
	}
	var err error
	config.Digest, err = config.Wiring().Digest()
	if err != nil {
		t.Fatal(err)
	}
	cancel, result := startTestProxy(t, config)
	defer func() {
		cancel()
		if err := <-result; err != nil {
			t.Error(err)
		}
	}()
	request := func(command string, global *Global, wire *WireEdit) Status {
		t.Helper()
		status := controlRoundTrip(t, config.RuntimeDir, ControlRequest{Command: command, InstanceID: config.InstanceID, ProtocolVersion: ControlProtocolVersion, Global: global, Wire: wire})
		if status.Error != "" || !status.Ready {
			t.Fatalf("%s: %#v", command, status)
		}
		return status
	}
	inputPath := HostSocket(config.RuntimeDir, DirectionInput, "filter", "documents")
	before, err := os.Stat(inputPath)
	if err != nil {
		t.Fatal(err)
	}
	directOutput := dialUnix(t, HostSocket(config.RuntimeDir, DirectionOutput, "source", "documents"))
	defer directOutput.Close()
	directInput := dialUnix(t, HostSocket(config.RuntimeDir, DirectionInput, "archive", "documents"))
	defer directInput.Close()
	assertOrigin(t, directOutput, "archive.documents")
	oldOutput := dialUnix(t, HostSocket(config.RuntimeDir, DirectionOutput, "source", "documents"))
	defer oldOutput.Close()
	oldInput := dialUnix(t, inputPath)
	defer oldInput.Close()
	assertOrigin(t, oldOutput, "filter.documents")
	writeAndRead(t, oldInput, oldOutput, "old route")
	request("assign-global", &Global{Name: "provider_endpoint", OutputComponent: "next", OutputEndpoint: "api"}, nil)
	after, err := os.Stat(inputPath)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("input inode changed: %v", err)
	}
	_ = oldInput.SetReadDeadline(time.Now().Add(time.Second))
	var b [1]byte
	if _, err := oldInput.Read(b[:]); err == nil {
		t.Fatal("old global stream survived reassignment")
	} else if timeout, ok := err.(interface{ Timeout() bool }); ok && timeout.Timeout() {
		t.Fatal("old stream did not close")
	}
	writeAndRead(t, directInput, directOutput, "direct survives reassignment")
	nextOutput := dialUnix(t, HostSocket(config.RuntimeDir, DirectionOutput, "next", "api"))
	defer nextOutput.Close()
	nextInput := dialUnix(t, inputPath)
	defer nextInput.Close()
	assertOrigin(t, nextOutput, "filter.documents")
	writeAndRead(t, nextInput, nextOutput, "new route")
	request("assign-global", &Global{Name: "provider_endpoint"}, nil)
	failed := dialUnix(t, inputPath)
	defer failed.Close()
	_ = failed.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := failed.Read(b[:]); err == nil {
		t.Fatal("unbound global accepted traffic")
	} else if timeout, ok := err.(interface{ Timeout() bool }); ok && timeout.Timeout() {
		t.Fatal("unbound global queued instead of failing")
	}
	writeAndRead(t, directInput, directOutput, "direct survives unbind")
	request("assign-global", &Global{Name: "provider_endpoint", OutputComponent: "source", OutputEndpoint: "documents"}, nil)
	reboundOutput := dialUnix(t, HostSocket(config.RuntimeDir, DirectionOutput, "source", "documents"))
	defer reboundOutput.Close()
	reboundInput := dialUnix(t, inputPath)
	defer reboundInput.Close()
	assertOrigin(t, reboundOutput, "filter.documents")
	writeAndRead(t, reboundInput, reboundOutput, "rebound")
	// A per-input edit preserves every other link and the global registry.
	request("mod-wire", nil, &WireEdit{Link: Link{InputComponent: "filter", InputEndpoint: "documents"}, Disconnect: true})
	writeAndRead(t, directInput, directOutput, "direct survives mod-wire")
	request("mod-wire", nil, &WireEdit{Link: Link{InputComponent: "filter", InputEndpoint: "documents", Global: "provider_endpoint"}})
	data, err := os.ReadFile(filepath.Join(config.RuntimeDir, ConfigFileName))
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := LoadConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(persisted.Globals, config.Globals) {
		t.Fatalf("global not persisted: %#v", persisted.Globals)
	}
	found := false
	for _, link := range persisted.Links {
		if link.InputComponent == "filter" {
			found = link.Global == "provider_endpoint" && link.OutputComponent == ""
		}
	}
	if !found {
		t.Fatal("global reference was flattened in persisted configuration")
	}
	// Stale conditional mutations must not change the routing digest.
	status := controlRoundTrip(t, config.RuntimeDir, ControlRequest{Command: "assign-global", InstanceID: config.InstanceID, ProtocolVersion: ControlProtocolVersion, ExpectedDigest: "stale", Global: &Global{Name: "provider_endpoint"}})
	if status.Error == "" || status.Digest != persisted.Digest {
		t.Fatalf("stale edit changed state: %#v", status)
	}
}

func TestGlobalAssignmentRecoveredAfterInterruptedResync(t *testing.T) {
	config := testConfig(t, false)
	fault := &oneShotResyncFault{step: resyncStepPersistConfig}
	cancel, result := startTestProxyWithFaults(t, config, fault.inject)
	defer func() {
		cancel()
		if err := <-result; err != nil {
			t.Error(err)
		}
	}()
	request := ControlRequest{Command: "assign-global", InstanceID: config.InstanceID, ProtocolVersion: ControlProtocolVersion, Global: &Global{Name: "provider_endpoint", OutputComponent: "source", OutputEndpoint: "documents"}}
	status := controlRoundTrip(t, config.RuntimeDir, request)
	if status.Error == "" || status.Ready || status.Digest != "" {
		t.Fatalf("interrupted assignment: %#v", status)
	}
	status = controlRoundTrip(t, config.RuntimeDir, request)
	if status.Error != "" || !status.Ready {
		t.Fatalf("retry assignment: %#v", status)
	}
	if _, err := (&ProcessManager{}).Inspect(context.Background(), processForConfig(config, os.Getpid())); err != nil {
		t.Fatal(err)
	}
}
