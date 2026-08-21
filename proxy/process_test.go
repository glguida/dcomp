package proxy

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
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

	legacy := parseProxyLogLine("unstructured legacy record", fallback)
	if !legacy.Timestamp.Equal(fallback) || legacy.Message != "unstructured legacy record" {
		t.Fatalf("legacy record = %#v", legacy)
	}
}

func TestVerifyStatusAcceptsRecordedVersionOneProxyForUpgrade(t *testing.T) {
	process := Process{InstanceID: "recorded", Digest: "sha256:wiring"}
	status := Status{
		Version: 1, InstanceID: process.InstanceID, Digest: process.Digest,
		Ready: true,
	}
	if err := verifyStatus(process, status); err != nil {
		t.Fatalf("verify v1 status: %v", err)
	}
	status.Version = ConfigVersion + 1
	if err := verifyStatus(process, status); err == nil {
		t.Fatal("future proxy status version was accepted")
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
