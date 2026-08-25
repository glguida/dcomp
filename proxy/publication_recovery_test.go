package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The socket ownership ledger is recovery authority for artifacts that are
// not discoverable by scanning the runtime directory, most importantly the
// hidden listener used for a shortened Unix path. If ordinary unwind cannot
// remove every artifact, a process death at that point must remain recoverable.
func TestPublicationUnwindFailureRemainsCrashRecoverable(t *testing.T) {
	for _, shortened := range []bool{false, true} {
		for _, published := range []bool{false, true} {
			name := "direct/prepared"
			if shortened {
				name = "shortened/prepared"
			}
			if published {
				name = name[:len(name)-len("prepared")] + "published"
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
				artifacts := append([]string(nil), publication.Paths()...)
				blockedDirectory := filepath.Dir(publication.ActualPath())
				t.Cleanup(func() { _ = publication.Abort() })

				// Replacing the listener's parent with a regular file makes unlink
				// fail with ENOTDIR even when the tests run as root. It deterministically
				// models a failable cleanup step without a production fault hook.
				restoreDirectory := obstructDirectory(t, blockedDirectory)
				abortErr := publication.Abort()
				if abortErr == nil {
					t.Fatal("publication unwind unexpectedly removed an obstructed artifact")
				}
				if _, err := os.Lstat(ownership.path); err != nil {
					t.Fatalf("partial unwind lost socket ownership ledger: %v", err)
				}
				restoreDirectory()

				if err := cleanupCrashedRuntime(processForConfig(config, 0)); err != nil {
					t.Fatal(err)
				}
				for _, artifact := range artifacts {
					if _, err := os.Lstat(artifact); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("crash recovery left publication artifact %s: %v", artifact, err)
					}
				}
			})
		}
	}
}

// Shutdown is itself a recoverable cleanup phase. It must not remove the
// ownership ledger (and then the PID completion marker) while a shortened
// artifact could not be removed, because the manager otherwise cannot safely
// finish that cleanup on the next pass.
func TestShutdownCleanupFailurePreservesPublicationRecovery(t *testing.T) {
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
			encoded, err := json.Marshal(config)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(
				filepath.Join(config.RuntimeDir, ConfigFileName), encoded, 0600,
			); err != nil {
				t.Fatal(err)
			}

			fault := &oneShotResyncFault{step: resyncStepPersistConfig}
			cancel, result := startTestProxyWithFaults(t, config, fault.inject)
			stopped := false
			t.Cleanup(func() {
				if stopped {
					return
				}
				cancel()
				<-result
			})
			target := wiringWithArchive(config.Wiring())
			digest, err := target.Digest()
			if err != nil {
				t.Fatal(err)
			}
			process := processForConfig(config, os.Getpid())
			if _, err := (&ProcessManager{}).Resync(
				context.Background(), process, target, digest,
			); err == nil {
				t.Fatal("resync unexpectedly passed the persist-config fault")
			}

			final := HostSocket(
				config.RuntimeDir, DirectionInput, "archive", "documents",
			)
			actual := socketListenPath(final)
			if gotShortened := actual != final; gotShortened != shortened {
				t.Fatalf("shortened listener = %t, want %t", gotShortened, shortened)
			}
			restoreDirectory := obstructDirectory(t, filepath.Dir(actual))
			cancel()
			if err := <-result; err != nil {
				t.Fatalf("proxy shutdown: %v", err)
			}
			stopped = true
			for markerName, path := range map[string]string{
				"ownership ledger": filepath.Join(config.RuntimeDir, socketOwnershipFileName),
				"PID marker":       filepath.Join(config.RuntimeDir, PIDFileName),
			} {
				if _, err := os.Lstat(path); err != nil {
					t.Errorf("failed shutdown lost %s: %v", markerName, err)
				}
			}
			restoreDirectory()

			if err := cleanupCrashedRuntime(process); err != nil {
				t.Fatal(err)
			}
			for _, artifact := range []string{final, actual} {
				if _, err := os.Lstat(artifact); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("manager cleanup left publication artifact %s: %v", artifact, err)
				}
			}
		})
	}
}

func obstructDirectory(t *testing.T, directory string) func() {
	t.Helper()
	held, err := os.MkdirTemp(filepath.Dir(directory), ".dcomp-held-*")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(held); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(directory, held); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(directory, []byte("directory obstruction\n"), 0600); err != nil {
		_ = os.Rename(held, directory)
		t.Fatal(err)
	}

	restored := false
	restore := func() {
		t.Helper()
		if restored {
			return
		}
		if err := os.Remove(directory); err != nil {
			t.Fatalf("remove directory obstruction: %v", err)
		}
		if err := os.Rename(held, directory); err != nil {
			t.Fatalf("restore obstructed directory: %v", err)
		}
		restored = true
	}
	t.Cleanup(restore)
	return restore
}
