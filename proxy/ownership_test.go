package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareRecoversLedgerClaimLeftByFailedUnwind(t *testing.T) {
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
			prepareOwnershipTestRuntime(t, config, false)
			ledger := createTestOwnershipLedger(t, config)
			final := HostSocket(
				config.RuntimeDir, DirectionInput, "archive", "documents",
			)
			publication, err := prepareSocketPublication(ledger, final, 0666, nil)
			if err != nil {
				t.Fatal(err)
			}
			blocked := filepath.Dir(publication.record.Temporary)
			if publication.record.Actual != publication.record.Final {
				blocked = filepath.Dir(publication.record.Actual)
			}
			restore := obstructDirectory(t, blocked)
			if err := publication.Abort(); err == nil {
				t.Fatal("publication unwind unexpectedly succeeded through obstruction")
			}
			if !ledger.Has(final) {
				t.Fatal("failed unwind discarded its durable socket claim")
			}
			restore()

			retry, err := prepareSocketPublication(ledger, final, 0666, nil)
			if err != nil {
				t.Fatalf("retry prepare: %v", err)
			}
			if err := retry.Publish(); err != nil {
				t.Fatalf("retry publish: %v", err)
			}
			if err := retry.Abort(); err != nil {
				t.Fatalf("retry cleanup: %v", err)
			}
		})
	}
}

func TestCrashCleanupCoversEveryLedgerPublicationState(t *testing.T) {
	states := []string{"reserved", "bound-unrecorded", "bound-recorded", "published"}
	for _, shortened := range []bool{false, true} {
		publicationKind := "direct"
		if shortened {
			publicationKind = "shortened"
		}
		for _, state := range states {
			t.Run(publicationKind+"/"+state, func(t *testing.T) {
				config := testConfig(t, false)
				if shortened {
					config = configWithLongRuntime(t, config)
				}
				process := prepareOwnershipTestRuntime(t, config, true)
				ledger := createTestOwnershipLedger(t, config)
				final := HostSocket(
					config.RuntimeDir, DirectionInput, "archive", "documents",
				)
				var listener net.Listener
				switch state {
				case "reserved", "bound-unrecorded":
					record, err := ledger.Reserve(final)
					if err != nil {
						t.Fatal(err)
					}
					if state == "bound-unrecorded" {
						bound := record.Temporary
						if record.Actual != record.Final {
							bound = record.Actual
						}
						if err := os.MkdirAll(filepath.Dir(bound), 0700); err != nil {
							t.Fatal(err)
						}
						listener, err = listenUnixFresh(bound, 0666)
						if err != nil {
							t.Fatal(err)
						}
					}
				case "bound-recorded", "published":
					publication, err := prepareSocketPublication(ledger, final, 0666, nil)
					if err != nil {
						t.Fatal(err)
					}
					listener = publication.Listener()
					if state == "published" {
						if err := publication.Publish(); err != nil {
							t.Fatal(err)
						}
					}
				}
				if listener != nil {
					if err := listener.Close(); err != nil {
						t.Fatal(err)
					}
				}
				var artifacts []string
				for path := range ledger.CandidatePaths() {
					if _, err := os.Lstat(path); err == nil {
						artifacts = append(artifacts, path)
					} else if !errors.Is(err, os.ErrNotExist) {
						t.Fatal(err)
					}
				}

				if err := cleanupCrashedRuntime(process); err != nil {
					t.Fatal(err)
				}
				for _, path := range artifacts {
					if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("crash cleanup left %s: %v", path, err)
					}
				}
				for _, path := range []string{
					ledger.path,
					filepath.Join(config.RuntimeDir, ConfigFileName),
					filepath.Join(config.RuntimeDir, PIDFileName),
				} {
					if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("completed crash cleanup left authority %s: %v", path, err)
					}
				}
			})
		}
	}
}

func TestCrashCleanupPreservesEveryReoccupiedSocketArtifact(t *testing.T) {
	tests := []struct {
		name       string
		shortened  bool
		relinquish string
	}{
		{name: "direct", relinquish: "final"},
		{name: "shortened-final", shortened: true, relinquish: "final"},
		{name: "shortened-actual", shortened: true, relinquish: "actual"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := testConfig(t, false)
			if test.shortened {
				config = configWithLongRuntime(t, config)
			}
			process := prepareOwnershipTestRuntime(t, config, true)
			ledger := createTestOwnershipLedger(t, config)
			final := HostSocket(
				config.RuntimeDir, DirectionInput, "archive", "documents",
			)
			publication, err := prepareSocketPublication(ledger, final, 0666, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := publication.Publish(); err != nil {
				t.Fatal(err)
			}
			if err := publication.Listener().Close(); err != nil {
				t.Fatal(err)
			}

			foreignPath := publication.FinalPath()
			remainingOwnedPath := publication.ActualPath()
			if test.relinquish == "actual" {
				foreignPath = publication.ActualPath()
				remainingOwnedPath = publication.FinalPath()
			}
			if err := os.Remove(foreignPath); err != nil {
				t.Fatal(err)
			}
			var foreign net.Listener
			if len(foreignPath) < 104 {
				foreign, err = listenUnixFresh(foreignPath, 0666)
			} else {
				err = os.WriteFile(foreignPath, []byte("foreign owner\n"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if foreign != nil {
					_ = foreign.Close()
				}
				_ = os.Remove(foreignPath)
			})

			if err := cleanupCrashedRuntime(process); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(foreignPath); err != nil {
				t.Fatalf("crash cleanup removed reoccupied path %s: %v", foreignPath, err)
			}
			if remainingOwnedPath != foreignPath {
				if _, err := os.Lstat(remainingOwnedPath); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("crash cleanup left owned sibling %s: %v", remainingOwnedPath, err)
				}
			}
		})
	}
}

func TestCrashCleanupDoesNotInferSocketOwnershipFromConfig(t *testing.T) {
	config := testConfig(t, false)
	process := prepareOwnershipTestRuntime(t, config, true)
	foreignPath := config.Endpoints[0].Socket
	foreign, err := listenUnixFresh(foreignPath, 0666)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = foreign.Close()
		_ = os.Remove(foreignPath)
	})

	if err := cleanupCrashedRuntime(process); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(foreignPath); err != nil {
		t.Fatalf("config-only cleanup claim removed foreign socket: %v", err)
	}
}

func TestCrashCleanupFailurePreservesLedgerAndProcessAuthority(t *testing.T) {
	config := configWithLongRuntime(t, testConfig(t, false))
	process := prepareOwnershipTestRuntime(t, config, true)
	ledger := createTestOwnershipLedger(t, config)
	final := HostSocket(
		config.RuntimeDir, DirectionInput, "archive", "documents",
	)
	publication, err := prepareSocketPublication(ledger, final, 0666, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := publication.Publish(); err != nil {
		t.Fatal(err)
	}
	if err := publication.Listener().Close(); err != nil {
		t.Fatal(err)
	}
	restore := obstructDirectory(t, filepath.Dir(publication.ActualPath()))
	if err := cleanupCrashedRuntime(process); err == nil {
		t.Fatal("crash cleanup unexpectedly completed through an obstructed owned artifact")
	}
	for _, path := range []string{
		ledger.path,
		filepath.Join(config.RuntimeDir, ConfigFileName),
		filepath.Join(config.RuntimeDir, PIDFileName),
	} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("failed crash cleanup removed recovery authority %s: %v", path, err)
		}
	}
	restore()
	if err := cleanupCrashedRuntime(process); err != nil {
		t.Fatalf("retry crash cleanup: %v", err)
	}
}

func TestReleasedLedgerRecordNeverRevisitsPublicPaths(t *testing.T) {
	config := testConfig(t, false)
	process := prepareOwnershipTestRuntime(t, config, true)
	ledger := createTestOwnershipLedger(t, config)
	final := HostSocket(
		config.RuntimeDir, DirectionInput, "archive", "documents",
	)
	publication, err := prepareSocketPublication(ledger, final, 0666, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := publication.Publish(); err != nil {
		t.Fatal(err)
	}
	if err := publication.Listener().Close(); err != nil {
		t.Fatal(err)
	}
	record := ledger.records[final]
	if err := removeSocketOwnershipArtifacts(record); err != nil {
		t.Fatal(err)
	}
	record.State = socketReleased
	if err := ledger.replaceRecord(record); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(record.Anchor); err != nil {
		t.Fatal(err)
	}
	foreign, err := listenUnixFresh(final, 0666)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = foreign.Close()
		_ = os.Remove(final)
	})

	if err := cleanupCrashedRuntime(process); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(final); err != nil {
		t.Fatalf("released-state recovery revisited public path: %v", err)
	}
}

func TestOwnershipLedgerTracksExactlyTheLiveSocketSet(t *testing.T) {
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
			defer func() {
				cancel()
				if err := <-result; err != nil {
					t.Fatalf("proxy Run: %v", err)
				}
			}()
			assertLiveOwnershipLedger(t, config, len(config.Endpoints)+1)

			manager := &ProcessManager{}
			process := processForConfig(config, os.Getpid())
			withArchive := wiringWithArchive(config.Wiring())
			digest, err := withArchive.Digest()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := manager.Resync(
				context.Background(), process, withArchive, digest,
			); err != nil {
				t.Fatal(err)
			}
			assertLiveOwnershipLedger(t, config, len(config.Endpoints)+2)

			withoutArchive := wiringWithoutComponent(withArchive, "archive")
			digest, err = withoutArchive.Digest()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := manager.Resync(
				context.Background(), process, withoutArchive, digest,
			); err != nil {
				t.Fatal(err)
			}
			assertLiveOwnershipLedger(t, config, len(config.Endpoints)+1)
		})
	}
}

func TestCompletedCleanupRefusesOutstandingSocketClaims(t *testing.T) {
	config := testConfig(t, false)
	process := prepareOwnershipTestRuntime(t, config, false)
	ledger := createTestOwnershipLedger(t, config)
	final := HostSocket(
		config.RuntimeDir, DirectionInput, "archive", "documents",
	)
	if _, err := ledger.Reserve(final); err != nil {
		t.Fatal(err)
	}

	if err := cleanupCompletedRuntime(process); err == nil {
		t.Fatal("completed cleanup accepted a non-empty ownership ledger without a PID marker")
	}
	for _, path := range []string{
		ledger.path,
		filepath.Join(config.RuntimeDir, ConfigFileName),
	} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("ambiguous completed cleanup removed recovery authority %s: %v", path, err)
		}
	}
}

func assertLiveOwnershipLedger(t *testing.T, config Config, want int) {
	t.Helper()
	ledger, err := loadSocketOwnershipLedger(
		filepath.Join(config.RuntimeDir, socketOwnershipFileName),
		config.RuntimeDir,
		config.InstanceID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(ledger.records); got != want {
		t.Fatalf("owned sockets = %d, want %d", got, want)
	}
	for _, record := range ledger.records {
		if record.State != socketAnchored {
			t.Fatalf("live socket %s state = %q", record.Final, record.State)
		}
		if owned, err := socketOwnershipOwnsPath(record, record.Final); err != nil {
			t.Fatal(err)
		} else if !owned {
			t.Fatalf("live final path %s does not match its anchor", record.Final)
		}
		if owned, err := socketOwnershipOwnsPath(record, record.Actual); err != nil {
			t.Fatal(err)
		} else if !owned {
			t.Fatalf("live actual path %s does not match its anchor", record.Actual)
		}
	}
}

func prepareOwnershipTestRuntime(t *testing.T, config Config, withPID bool) Process {
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
	process := processForConfig(config, 1<<30)
	if withPID {
		if err := os.WriteFile(
			filepath.Join(config.RuntimeDir, PIDFileName), []byte("1073741824\n"), 0600,
		); err != nil {
			t.Fatal(err)
		}
	}
	return process
}
