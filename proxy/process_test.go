package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestParseProxyLogLineUsesWrittenTimestamp(t *testing.T) {
	fallback := time.Date(2030, time.January, 2, 3, 4, 5, 0, time.UTC)
	record := parseProxyLogLine(
		"2026/08/20 15:47:51.123456 connection paired input=a output=b",
		fallback,
	)
	want := time.Date(2026, time.August, 20, 15, 47, 51, 123456000, time.UTC)
	if !record.Timestamp.Equal(want) {
		t.Fatalf("timestamp = %s, want %s", record.Timestamp, want)
	}
	if got, want := record.Message, "connection paired input=a output=b"; got != want {
		t.Fatalf("message = %q, want %q", got, want)
	}

	unstructured := parseProxyLogLine("unstructured record", fallback)
	if !unstructured.Timestamp.Equal(fallback) || unstructured.Message != "unstructured record" {
		t.Fatalf("unstructured record = %#v", unstructured)
	}
}

func TestVerifyStatusIdentityRequiresCurrentProxyVersion(t *testing.T) {
	process := Process{InstanceID: "recorded", Digest: "sha256:wiring"}
	status := Status{
		Version: ConfigVersion, ControlProtocolVersion: ControlProtocolVersion,
		InstanceID: process.InstanceID, Digest: "sha256:observed", Ready: false,
	}
	if err := verifyStatusIdentity(process, status); err != nil {
		t.Fatalf("verify current status: %v", err)
	}
	for _, version := range []int{1, 2, ConfigVersion + 1} {
		status.Version = version
		if err := verifyStatusIdentity(process, status); err == nil {
			t.Fatalf("proxy status version %d was accepted", version)
		}
	}
	status.Version = ConfigVersion
	for _, version := range []int{0, ControlProtocolVersion + 1} {
		status.ControlProtocolVersion = version
		if err := verifyStatusIdentity(process, status); !errors.Is(err, ErrControlProtocolMismatch) {
			t.Fatalf("control protocol version %d error = %v", version, err)
		}
	}
}

func TestStopRefusesCleanupWhenControlIsMissingButRecordedPIDIsLive(t *testing.T) {
	runtimeDir := t.TempDir()
	pidPath := filepath.Join(runtimeDir, PIDFileName)
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0600); err != nil {
		t.Fatal(err)
	}
	process := Process{
		InstanceID: "live-without-control",
		PID:        os.Getpid(),
		RuntimeDir: runtimeDir,
		Control:    ControlSocket(runtimeDir),
		Log:        filepath.Join(runtimeDir, LogFileName),
	}
	err := (&ProcessManager{}).Stop(context.Background(), process)
	if err == nil || errors.Is(err, ErrNotRunning) || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("Stop error = %v", err)
	}
	if _, err := os.Stat(pidPath); err != nil {
		t.Fatalf("live proxy cleanup marker was removed: %v", err)
	}
}

func TestCleanupDoesNotTrustMissingPIDMarkerWhenConfigIsUnavailable(t *testing.T) {
	runtimeDir := t.TempDir()
	process := Process{
		InstanceID: "unknown-cleanup",
		PID:        os.Getpid(),
		RuntimeDir: runtimeDir,
		Control:    ControlSocket(runtimeDir),
		Log:        filepath.Join(runtimeDir, LogFileName),
	}
	finished, err := finishStoppedProxyCleanup(process, false)
	if err != nil {
		t.Fatal(err)
	}
	if finished {
		t.Fatal("live process was treated as cleaned without a reliable completion marker")
	}
}

func TestEnsureRefusesRuntimeCleanupWhileRecordedProxyPIDIsLive(t *testing.T) {
	stale := configWithInstance(t, testConfig(t, false), "stale-instance")
	endpointPath := writeRecordedProxyRuntime(t, stale, os.Getpid(), true)
	target := configWithInstance(t, stale, "target-instance")

	_, err := (&ProcessManager{
		Binary: filepath.Join(t.TempDir(), "missing-dcomp-proxy"),
	}).Ensure(context.Background(), target)
	if err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("Ensure error = %v, want live-PID cleanup refusal", err)
	}

	data, err := os.ReadFile(filepath.Join(stale.RuntimeDir, ConfigFileName))
	if err != nil {
		t.Fatalf("read preserved proxy config: %v", err)
	}
	preserved, err := LoadConfig(data)
	if err != nil {
		t.Fatalf("load preserved proxy config: %v", err)
	}
	if preserved.InstanceID != stale.InstanceID || preserved.Version != ConfigVersion {
		t.Fatalf("preserved config = instance %q version %d", preserved.InstanceID, preserved.Version)
	}
	for _, path := range []string{
		filepath.Join(stale.RuntimeDir, PIDFileName),
		filepath.Join(stale.RuntimeDir, ReadyFileName),
		endpointPath,
	} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("live proxy artifact %s was removed: %v", path, err)
		}
	}
}

func TestEnsureRejectsUnsupportedConfigWithoutMutatingRuntime(t *testing.T) {
	for _, version := range []int{1, 2, ConfigVersion + 1} {
		t.Run(fmt.Sprintf("version-%d", version), func(t *testing.T) {
			current := testConfig(t, false)
			unsupported := current
			unsupported.Version = version
			endpointPath := writeRecordedProxyRuntime(t, unsupported, 0, false)
			configPath := filepath.Join(current.RuntimeDir, ConfigFileName)
			before, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}

			target := configWithInstance(t, current, "target-instance")
			_, err = (&ProcessManager{
				Binary: filepath.Join(t.TempDir(), "missing-dcomp-proxy"),
			}).Ensure(context.Background(), target)
			if err == nil || !strings.Contains(err.Error(), "unsupported proxy config version") {
				t.Fatalf("Ensure version %d error = %v", version, err)
			}

			after, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != string(before) {
				t.Fatal("Ensure rewrote the unsupported proxy config")
			}
			for _, path := range []string{
				endpointPath,
				filepath.Join(current.RuntimeDir, ReadyFileName),
			} {
				if _, err := os.Lstat(path); err != nil {
					t.Fatalf("Ensure removed unsupported runtime artifact %s: %v", path, err)
				}
			}
		})
	}
}

func TestEnsureReclaimsRuntimeAfterCleanupCanBeProvenComplete(t *testing.T) {
	tests := []struct {
		name     string
		pid      int
		writePID bool
	}{
		{
			name: "current-proxy-removed-cleanup-marker",
			pid:  os.Getpid(),
		},
		{
			name:     "current-proxy-recorded-pid-exited",
			pid:      1 << 30,
			writePID: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stale := configWithInstance(t, testConfig(t, false), "stale-instance")
			endpointPath := stale.Endpoints[0].Socket
			if test.writePID {
				endpointPath = writeOwnedProxyRuntime(t, stale, test.pid)
			} else {
				writeCompletedProxyRuntime(t, stale)
			}
			target := configWithInstance(t, stale, "target-instance")

			_, err := (&ProcessManager{
				Binary: filepath.Join(t.TempDir(), "missing-dcomp-proxy"),
			}).Ensure(context.Background(), target)
			if err == nil || !strings.Contains(err.Error(), "start dcomp-proxy") {
				t.Fatalf("Ensure error = %v, want replacement launch attempt", err)
			}
			if _, err := os.Lstat(endpointPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("stale endpoint remains after proven cleanup: %v", err)
			}
			data, err := os.ReadFile(filepath.Join(target.RuntimeDir, ConfigFileName))
			if err != nil {
				t.Fatalf("read replacement proxy config: %v", err)
			}
			written, err := LoadConfig(data)
			if err != nil {
				t.Fatalf("load replacement proxy config: %v", err)
			}
			if written.InstanceID != target.InstanceID || written.Version != ConfigVersion {
				t.Fatalf("replacement config = instance %q version %d", written.InstanceID, written.Version)
			}
		})
	}
}

func writeCompletedProxyRuntime(t *testing.T, config Config) {
	t.Helper()
	if err := os.MkdirAll(config.RuntimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(config.RuntimeDir, ConfigFileName), append(encoded, '\n'), 0600,
	); err != nil {
		t.Fatal(err)
	}
}

func writeOwnedProxyRuntime(t *testing.T, config Config, pid int) string {
	t.Helper()
	for _, directory := range []string{"in", "out"} {
		if err := os.MkdirAll(filepath.Join(config.RuntimeDir, directory), 0700); err != nil {
			t.Fatal(err)
		}
	}
	encoded, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(config.RuntimeDir, ConfigFileName), append(encoded, '\n'), 0600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(config.RuntimeDir, PIDFileName),
		[]byte(fmt.Sprintf("%d\n", pid)),
		0600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(config.RuntimeDir, ReadyFileName), []byte("stale-ready\n"), 0600,
	); err != nil {
		t.Fatal(err)
	}
	ledger := createTestOwnershipLedger(t, config)
	publication, err := prepareSocketPublication(
		ledger, config.Endpoints[0].Socket, 0666, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := publication.Publish(); err != nil {
		t.Fatal(err)
	}
	if err := publication.Listener().Close(); err != nil {
		t.Fatal(err)
	}
	return config.Endpoints[0].Socket
}

func configWithInstance(t *testing.T, config Config, instanceID string) Config {
	t.Helper()
	config.InstanceID = instanceID
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	return config
}

func writeRecordedProxyRuntime(
	t *testing.T,
	config Config,
	pid int,
	writePID bool,
) string {
	t.Helper()
	for _, directory := range []string{"in", "out"} {
		if err := os.MkdirAll(filepath.Join(config.RuntimeDir, directory), 0700); err != nil {
			t.Fatal(err)
		}
	}
	encoded, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(config.RuntimeDir, ConfigFileName), append(encoded, '\n'), 0600,
	); err != nil {
		t.Fatal(err)
	}
	if writePID {
		if err := os.WriteFile(
			filepath.Join(config.RuntimeDir, PIDFileName),
			[]byte(fmt.Sprintf("%d\n", pid)),
			0600,
		); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(
		filepath.Join(config.RuntimeDir, ReadyFileName), []byte("stale-ready\n"), 0600,
	); err != nil {
		t.Fatal(err)
	}
	endpointPath := config.Endpoints[0].Socket
	if err := os.WriteFile(endpointPath, []byte("stale-endpoint\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return endpointPath
}

func TestResyncProtocolGateNeverSendsUnsupportedRequest(t *testing.T) {
	runtimeDir := t.TempDir()
	control := ControlSocket(runtimeDir)
	listener, err := net.Listen("unix", control)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	process := Process{
		InstanceID: "incompatible-instance",
		Digest:     "sha256:current",
		RuntimeDir: runtimeDir,
		Control:    control,
		Log:        filepath.Join(runtimeDir, LogFileName),
	}
	requests := make(chan ControlRequest, 2)
	serverErr := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		var request ControlRequest
		if readErr := readControlMessage(connection, &request); readErr != nil {
			_ = connection.Close()
			serverErr <- readErr
			return
		}
		requests <- request
		writeErr := writeControlMessage(connection, Status{
			Version: ConfigVersion, InstanceID: process.InstanceID, Digest: process.Digest,
			PID: 123, Ready: true, ControlProtocolVersion: 0,
		})
		_ = connection.Close()
		if writeErr != nil {
			serverErr <- writeErr
			return
		}
		_ = listener.(*net.UnixListener).SetDeadline(time.Now().Add(150 * time.Millisecond))
		second, secondErr := listener.Accept()
		if secondErr == nil {
			_ = second.Close()
			serverErr <- fmt.Errorf("incompatible proxy received a second control request")
			return
		}
		if timeout, ok := secondErr.(net.Error); !ok || !timeout.Timeout() {
			serverErr <- secondErr
			return
		}
		serverErr <- nil
	}()
	wiring := Wiring{}
	digest, err := wiring.Digest()
	if err != nil {
		t.Fatal(err)
	}
	_, err = (&ProcessManager{}).Resync(
		context.Background(), process, wiring, digest,
	)
	if !errors.Is(err, ErrControlProtocolMismatch) {
		t.Fatalf("Resync error = %v, want ErrControlProtocolMismatch", err)
	}
	if request := <-requests; request.Command != "status" ||
		request.ProtocolVersion != ControlProtocolVersion {
		t.Fatalf("incompatible proxy received request %#v", request)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestTerminateStartedCommandEscalatesAndReturns(t *testing.T) {
	command := exec.Command(os.Args[0], "-test.run=^TestProxyTerminationHelperProcess$")
	command.Env = append(os.Environ(), "DCOMP_PROXY_TEST_IGNORE_TERM=1")
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan error, 1)
	go func() {
		line, readErr := bufio.NewReader(stdout).ReadString('\n')
		if readErr == nil && line != "ready\n" {
			readErr = fmt.Errorf("helper readiness line = %q", line)
		}
		ready <- readErr
	}()
	select {
	case err := <-ready:
		if err != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		_ = command.Process.Kill()
		_ = command.Wait()
		t.Fatal("termination helper did not become ready")
	}

	grace := 25 * time.Millisecond
	started := time.Now()
	if err := terminateStartedCommand(command, grace); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	if elapsed < grace {
		t.Fatalf("process exited in %s without waiting for ignored SIGTERM", elapsed)
	}
	if elapsed > time.Second {
		t.Fatalf("forced process termination took %s", elapsed)
	}
}

func TestProxyTerminationHelperProcess(t *testing.T) {
	if os.Getenv("DCOMP_PROXY_TEST_IGNORE_TERM") != "1" {
		return
	}
	signal.Ignore(syscall.SIGTERM)
	if _, err := fmt.Fprintln(os.Stdout, "ready"); err != nil {
		os.Exit(2)
	}
	for {
		time.Sleep(time.Hour)
	}
}
