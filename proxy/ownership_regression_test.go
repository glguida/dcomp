package proxy

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// A successful resync relinquishes every pathname of a removed endpoint.
// ProcessManager.Stop must respect that live ownership decision instead of
// reconstructing endpoint ownership by sweeping the runtime directories.
func TestProcessManagerStopPreservesSocketReoccupyingRelinquishedEndpoint(t *testing.T) {
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
			cancel, result := startTestProxy(t, config)
			stopped := false
			t.Cleanup(func() {
				if stopped {
					return
				}
				cancel()
				<-result
			})

			manager := &ProcessManager{}
			process := processForConfig(config, os.Getpid())
			target := wiringWithoutComponent(config.Wiring(), "archive")
			digest, err := target.Digest()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := manager.Resync(context.Background(), process, target, digest); err != nil {
				t.Fatal(err)
			}

			final := HostSocket(
				config.RuntimeDir, DirectionInput, "archive", "documents",
			)
			actual := socketListenPath(final)
			foreign, err := listenUnix(final, 0666)
			if err != nil {
				t.Fatalf("reoccupy relinquished endpoint: %v", err)
			}
			t.Cleanup(func() {
				_ = foreign.Close()
				_ = os.Remove(final)
				_ = os.Remove(actual)
			})

			if err := manager.Stop(context.Background(), process); err != nil {
				t.Fatalf("Stop: %v", err)
			}
			if err := <-result; err != nil {
				t.Fatalf("proxy Run: %v", err)
			}
			stopped = true
			for _, path := range uniquePaths(final, actual) {
				if _, err := os.Lstat(path); err != nil {
					t.Fatalf("manager cleanup removed relinquished path %s: %v", path, err)
				}
			}
		})
	}
}

// A shortened publication owns two visible names for one socket inode. If
// teardown removes only one name, retry and shutdown must remember that the
// removed name was relinquished even though its sibling still needs cleanup.
func TestShortenedTeardownTracksEachArtifactIndependently(t *testing.T) {
	for _, removedFirst := range []string{"final", "actual"} {
		for _, finish := range []string{"retry", "shutdown"} {
			t.Run(removedFirst+"-then-"+finish, func(t *testing.T) {
				config := configWithLongRuntime(t, testConfig(t, true))
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
				stopped := false
				t.Cleanup(func() {
					if stopped {
						return
					}
					cancel()
					<-result
				})

				final := HostSocket(
					config.RuntimeDir, DirectionInput, "archive", "documents",
				)
				actual := socketListenPath(final)
				if actual == final {
					t.Fatal("test did not select shortened socket publication")
				}
				target := wiringWithoutComponent(config.Wiring(), "archive")
				digest, err := target.Digest()
				if err != nil {
					t.Fatal(err)
				}
				manager := &ProcessManager{}
				process := processForConfig(config, os.Getpid())
				resynced := make(chan error, 1)
				go func() {
					_, resyncErr := manager.Resync(
						context.Background(), process, target, digest,
					)
					resynced <- resyncErr
				}()
				<-entered

				blockedDirectory := filepath.Dir(actual)
				foreignPath := final
				remainingOwnedPath := actual
				if removedFirst == "actual" {
					blockedDirectory = filepath.Dir(final)
					foreignPath = actual
					remainingOwnedPath = final
				}
				restoreDirectory := obstructDirectory(t, blockedDirectory)
				close(release)
				if err := <-resynced; err == nil {
					t.Fatal("resync unexpectedly completed through an obstructed teardown")
				}
				restoreDirectory()

				if _, err := os.Lstat(foreignPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("first teardown did not relinquish %s: %v", foreignPath, err)
				}
				if _, err := os.Lstat(remainingOwnedPath); err != nil {
					t.Fatalf("first teardown lost retry artifact %s: %v", remainingOwnedPath, err)
				}

				var foreign net.Listener
				if foreignPath == actual {
					foreign, err = listenUnixFresh(foreignPath, 0666)
				} else {
					err = os.WriteFile(foreignPath, []byte("foreign owner\n"), 0600)
				}
				if err != nil {
					t.Fatalf("reoccupy partially relinquished path: %v", err)
				}
				t.Cleanup(func() {
					if foreign != nil {
						_ = foreign.Close()
					}
					_ = os.Remove(foreignPath)
				})

				switch finish {
				case "retry":
					status, err := manager.Resync(
						context.Background(), process, target, digest,
					)
					if err != nil {
						t.Fatalf("retry Resync: %v", err)
					}
					if !status.Ready || status.Digest != digest {
						t.Fatalf("retry status = %#v", status)
					}
				case "shutdown":
					cancel()
					if err := <-result; err != nil {
						t.Fatalf("proxy Run: %v", err)
					}
					stopped = true
				}
				if _, err := os.Lstat(foreignPath); err != nil {
					t.Fatalf("%s removed relinquished path %s: %v", finish, foreignPath, err)
				}
				if _, err := os.Lstat(remainingOwnedPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("%s left owned teardown artifact %s: %v", finish, remainingOwnedPath, err)
				}
			})
		}
	}
}

// Runtime-directory membership is not ownership. Crash cleanup must preserve
// an unledgered entry of any type instead of deleting it or blocking cleanup.
func TestCrashedRuntimeCleanupPreservesUnownedEntries(t *testing.T) {
	config := testConfig(t, false)
	process := processForConfig(config, os.Getpid())
	endpoint := writeRecordedProxyRuntime(t, config, process.PID, true)
	unexpected := filepath.Join(config.RuntimeDir, "in", "not-a-proxy-socket")
	if err := os.WriteFile(unexpected, []byte("foreign owner\n"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := cleanupCrashedRuntime(process); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{endpoint, unexpected} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("crash cleanup removed unowned entry %s: %v", path, err)
		}
	}
	for _, path := range []string{
		filepath.Join(config.RuntimeDir, ConfigFileName),
		filepath.Join(config.RuntimeDir, PIDFileName),
	} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("completed cleanup left recovery metadata %s: %v", path, err)
		}
	}
}

func uniquePaths(paths ...string) []string {
	unique := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if _, exists := seen[path]; exists {
			continue
		}
		seen[path] = struct{}{}
		unique = append(unique, path)
	}
	return unique
}
