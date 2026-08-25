package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	proxyCrashHelperEnvironment = "DCOMP_TEST_PROXY_CRASH_HELPER"
	proxyCrashConfigEnvironment = "DCOMP_TEST_PROXY_CRASH_CONFIG"
	proxyCrashStepEnvironment   = "DCOMP_TEST_PROXY_CRASH_STEP"
)

// This is the process half of TestProxySIGKILLRecoveryAtEveryOwnershipBoundary.
// The parent executes only this test in a separate copy of the test binary,
// drives a real control-socket resync, and kills the process while the selected
// production fault boundary is blocked.
func TestProxyCrashBoundaryHelperProcess(t *testing.T) {
	if os.Getenv(proxyCrashHelperEnvironment) != "1" {
		return
	}
	data, err := os.ReadFile(os.Getenv(proxyCrashConfigEnvironment))
	if err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	targetStep := os.Getenv(proxyCrashStepEnvironment)
	if targetStep == "" {
		t.Fatal("crash helper has no target step")
	}
	faults := func(step string) error {
		if step != targetStep {
			return nil
		}
		if _, err := fmt.Fprintln(os.Stdout, "boundary"); err != nil {
			return err
		}
		select {}
	}
	err = run(context.Background(), config, func(Status) error {
		_, readyErr := fmt.Fprintln(os.Stdout, "ready")
		return readyErr
	}, faults)
	if err != nil {
		t.Fatal(err)
	}
	t.Fatal("crash helper proxy exited without being killed")
}

// TestProxySIGKILLRecoveryAtEveryOwnershipBoundary verifies the ordering with
// a real proxy process, real Unix listeners, an actual control request, and
// SIGKILL. The white-box state tests remain useful for exact local failures;
// this test proves those states are reachable through the production server.
func TestProxySIGKILLRecoveryAtEveryOwnershipBoundary(t *testing.T) {
	type crashBoundary struct {
		name          string
		step          string
		removal       bool
		shortenedOnly bool
		wantState     socketOwnershipState
		wantRecord    bool
		reoccupyAll   bool
	}
	boundaries := []crashBoundary{
		{
			name: "before-reservation", step: resyncStepPrepareClaim,
		},
		{
			name: "reserved-before-bind", step: resyncStepPrepareBind,
			wantState: socketReserved, wantRecord: true,
		},
		{
			name: "bound-before-anchor", step: resyncStepPrepareAnchor,
			wantState: socketReserved, wantRecord: true,
		},
		{
			name: "anchored-before-exposed-link", step: resyncStepPrepareLink,
			shortenedOnly: true, wantState: socketAnchored, wantRecord: true,
		},
		{
			name: "anchored-before-commit", step: resyncStepCommitLocked,
			wantState: socketAnchored, wantRecord: true,
		},
		{
			name: "commit-before-publication", step: resyncStepPublish,
			wantState: socketAnchored, wantRecord: true,
		},
		{
			name: "published-before-swap", step: resyncStepPublished,
			wantState: socketAnchored, wantRecord: true,
		},
		{
			name: "swapped-before-release", step: resyncStepTeardownUnlink,
			removal: true, wantState: socketAnchored, wantRecord: true,
		},
		{
			name: "released-record", step: resyncStepOwnershipReleased,
			removal: true, wantState: socketReleased, wantRecord: true,
			reoccupyAll: true,
		},
		{
			name: "teardown-complete-before-config", step: resyncStepPersistConfig,
			removal: true,
		},
	}

	for _, shortened := range []bool{false, true} {
		publication := "direct"
		if shortened {
			publication = "shortened"
		}
		for _, boundary := range boundaries {
			boundary := boundary
			if boundary.shortenedOnly && !shortened {
				continue
			}
			t.Run(publication+"/"+boundary.name, func(t *testing.T) {
				config := testConfig(t, boundary.removal)
				if shortened {
					config = configWithLongRuntime(t, config)
				}
				if err := os.MkdirAll(config.RuntimeDir, 0700); err != nil {
					t.Fatal(err)
				}
				encoded, err := json.MarshalIndent(config, "", "  ")
				if err != nil {
					t.Fatal(err)
				}
				configPath := filepath.Join(config.RuntimeDir, ConfigFileName)
				if err := os.WriteFile(configPath, append(encoded, '\n'), 0600); err != nil {
					t.Fatal(err)
				}

				command := exec.Command(
					os.Args[0], "-test.run=^TestProxyCrashBoundaryHelperProcess$",
				)
				command.Env = append(os.Environ(),
					proxyCrashHelperEnvironment+"=1",
					proxyCrashConfigEnvironment+"="+configPath,
					proxyCrashStepEnvironment+"="+boundary.step,
				)
				stdout, err := command.StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
				var stderr bytes.Buffer
				command.Stderr = &stderr
				if err := command.Start(); err != nil {
					t.Fatal(err)
				}
				waited := false
				t.Cleanup(func() {
					if waited {
						return
					}
					_ = command.Process.Kill()
					_ = command.Wait()
				})
				events := make(chan string)
				go func() {
					scanner := bufio.NewScanner(stdout)
					for scanner.Scan() {
						events <- scanner.Text()
					}
					close(events)
				}()
				waitForCrashHelperEvent(t, events, "ready")

				process := processForConfig(config, command.Process.Pid)
				target := config.Wiring()
				if boundary.removal {
					target = wiringWithoutComponent(target, "archive")
				} else {
					target = wiringWithArchive(target)
				}
				digest, err := target.Digest()
				if err != nil {
					t.Fatal(err)
				}
				resyncResult := make(chan error, 1)
				go func() {
					_, resyncErr := (&ProcessManager{}).Resync(
						context.Background(), process, target, digest,
					)
					resyncResult <- resyncErr
				}()
				waitForCrashHelperEvent(t, events, "boundary")

				final := HostSocket(
					config.RuntimeDir, DirectionInput, "archive", "documents",
				)
				ledger, err := loadSocketOwnershipLedger(
					filepath.Join(config.RuntimeDir, socketOwnershipFileName),
					config.RuntimeDir,
					config.InstanceID,
				)
				if err != nil {
					t.Fatal(err)
				}
				record, recorded := ledger.records[final]
				if recorded != boundary.wantRecord {
					t.Fatalf(
						"changed endpoint recorded = %t, want %t; ledger=%#v",
						recorded, boundary.wantRecord, ledger.records,
					)
				}
				if recorded && record.State != boundary.wantState {
					t.Fatalf("ownership state = %q, want %q", record.State, boundary.wantState)
				}
				assertCrashBoundaryArtifacts(t, boundary.name, record, recorded)

				if err := command.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				waitErr := command.Wait()
				waited = true
				var exitErr *exec.ExitError
				if !errors.As(waitErr, &exitErr) || exitErr.ProcessState.Success() {
					t.Fatalf("crash helper wait error = %v; stderr=%s", waitErr, stderr.String())
				}
				select {
				case err := <-resyncResult:
					if err == nil {
						t.Fatal("resync unexpectedly succeeded after proxy SIGKILL")
					}
				case <-time.After(5 * time.Second):
					t.Fatal("resync did not return after proxy SIGKILL")
				}

				foreign := make(map[string]struct{})
				if boundary.reoccupyAll {
					for _, path := range uniqueNonemptyPaths(record.Final, record.Actual) {
						if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(path, []byte("foreign owner\n"), 0600); err != nil {
							t.Fatal(err)
						}
						foreign[path] = struct{}{}
						t.Cleanup(func() { _ = os.Remove(path) })
					}
				}

				if err := (&ProcessManager{}).Stop(context.Background(), process); err != nil {
					t.Fatalf("recover killed proxy: %v", err)
				}
				for path := range foreign {
					if _, err := os.Lstat(path); err != nil {
						t.Fatalf("crash cleanup removed reoccupied path %s: %v", path, err)
					}
				}
				assertKilledProxyArtifactsRemoved(t, config, target, foreign)
			})
		}
	}
}

func waitForCrashHelperEvent(t *testing.T, events <-chan string, want string) {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event, open := <-events:
			if !open {
				t.Fatalf("proxy crash helper exited before %q", want)
			}
			if event == want {
				return
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for proxy crash helper event %q", want)
		}
	}
}

func assertCrashBoundaryArtifacts(
	t *testing.T,
	boundary string,
	record socketOwnershipRecord,
	recorded bool,
) {
	t.Helper()
	if !recorded {
		return
	}
	want := make(map[string]bool)
	switch boundary {
	case "before-reservation":
	case "reserved-before-bind":
	case "bound-before-anchor":
		want[socketOwnershipBindPath(record)] = true
	case "anchored-before-exposed-link":
		want[record.Anchor] = true
		want[record.Actual] = true
	case "anchored-before-commit", "commit-before-publication":
		want[record.Anchor] = true
		want[socketOwnershipBindPath(record)] = true
		if record.Actual != record.Final {
			want[record.Temporary] = true
		}
	case "published-before-swap", "swapped-before-release":
		want[record.Anchor] = true
		want[record.Final] = true
		want[record.Actual] = true
	case "released-record":
		want[record.Anchor] = true
	default:
		t.Fatalf("unknown crash boundary %q", boundary)
	}
	for _, path := range socketOwnershipAllPaths(record) {
		_, err := os.Lstat(path)
		exists := err == nil
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		if exists != want[path] {
			t.Fatalf("artifact %s exists = %t, want %t at %s", path, exists, want[path], boundary)
		}
	}
}

func assertKilledProxyArtifactsRemoved(
	t *testing.T,
	config Config,
	target Wiring,
	foreign map[string]struct{},
) {
	t.Helper()
	paths := map[string]struct{}{
		filepath.Join(config.RuntimeDir, socketOwnershipFileName): {},
		filepath.Join(config.RuntimeDir, PIDFileName):             {},
		filepath.Join(config.RuntimeDir, ReadyFileName):           {},
		filepath.Join(config.RuntimeDir, ConfigFileName):          {},
	}
	addSocket := func(final string) {
		record := socketOwnershipRecord{
			Final: final, Temporary: publicationTemporaryPath(final, config.InstanceID),
			Anchor: socketOwnershipAnchorPath(config.RuntimeDir, config.InstanceID, final),
		}
		record.Actual = record.Final
		if len(record.Temporary) >= 104 {
			record.Actual = shortenedSocketPath(final)
		}
		for _, path := range socketOwnershipAllPaths(record) {
			paths[path] = struct{}{}
		}
	}
	addSocket(controlSocketFile(config.RuntimeDir))
	for _, endpoint := range config.Wiring().Endpoints {
		addSocket(HostSocket(
			config.RuntimeDir, endpoint.Direction, endpoint.Component, endpoint.Name,
		))
	}
	for _, endpoint := range target.Endpoints {
		addSocket(HostSocket(
			config.RuntimeDir, endpoint.Direction, endpoint.Component, endpoint.Name,
		))
	}
	for path := range paths {
		if _, preserved := foreign[path]; preserved {
			continue
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("killed-proxy recovery left artifact %s: %v", path, err)
		}
	}
	entries, err := os.ReadDir(config.RuntimeDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".proxy-tmp-") {
			t.Fatalf("killed-proxy recovery left temporary metadata %s", entry.Name())
		}
	}
}
