package proxy

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOwnershipPersistenceFailuresResumeFromDurableState(t *testing.T) {
	for _, shortened := range []bool{false, true} {
		pathForm := "direct"
		if shortened {
			pathForm = "shortened"
		}
		t.Run(pathForm+"/reserve", func(t *testing.T) {
			config, ledger, final := newPersistenceFailureLedger(t, shortened)
			restore := makeLedgerPersistenceFail(t, ledger)
			if _, err := ledger.Reserve(final); err == nil {
				t.Fatal("Reserve unexpectedly survived a ledger persistence failure")
			}
			restore()
			if ledger.Has(final) {
				t.Fatal("failed Reserve changed the in-memory ledger")
			}
			assertPersistedOwnershipState(t, config, final, "")
			if _, err := ledger.Reserve(final); err != nil {
				t.Fatalf("retry Reserve: %v", err)
			}
			if err := ledger.Release(final); err != nil {
				t.Fatalf("release retried reservation: %v", err)
			}
		})

		t.Run(pathForm+"/anchor", func(t *testing.T) {
			config, ledger, final := newPersistenceFailureLedger(t, shortened)
			record, err := ledger.Reserve(final)
			if err != nil {
				t.Fatal(err)
			}
			bound := socketOwnershipBindPath(record)
			if err := os.MkdirAll(filepath.Dir(bound), 0700); err != nil {
				t.Fatal(err)
			}
			listener, err := listenUnixFresh(bound, 0666)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()

			restore := makeLedgerPersistenceFail(t, ledger)
			if _, err := ledger.Anchor(final); err == nil {
				t.Fatal("Anchor unexpectedly survived a ledger persistence failure")
			}
			restore()
			if got := ledger.records[final].State; got != socketReserved {
				t.Fatalf("in-memory state = %q, want reserved", got)
			}
			assertPersistedOwnershipState(t, config, final, socketReserved)
			if owned, err := socketOwnershipOwnsPath(ledger.records[final], bound); err != nil {
				t.Fatal(err)
			} else if !owned {
				t.Fatal("failed Anchor did not retain its inode-identifying hard link")
			}
			if _, err := ledger.Anchor(final); err != nil {
				t.Fatalf("retry Anchor: %v", err)
			}
			if err := listener.Close(); err != nil {
				t.Fatal(err)
			}
			if err := ledger.Release(final); err != nil {
				t.Fatalf("release retried anchor: %v", err)
			}
		})

		t.Run(pathForm+"/release-artifacts", func(t *testing.T) {
			config, ledger, final := newPersistenceFailureLedger(t, shortened)
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

			restore := makeLedgerPersistenceFail(t, ledger)
			if err := ledger.Release(final); err == nil {
				t.Fatal("Release unexpectedly survived a ledger persistence failure")
			}
			restore()
			if got := ledger.records[final].State; got != socketAnchored {
				t.Fatalf("in-memory state = %q, want anchored", got)
			}
			assertPersistedOwnershipState(t, config, final, socketAnchored)
			for _, path := range socketOwnershipCandidates(record) {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("failed Release left candidate %s: %v", path, err)
				}
			}
			foreign := reoccupySocketCandidates(t, record)
			if err := ledger.Release(final); err != nil {
				t.Fatalf("retry Release: %v", err)
			}
			assertForeignCandidatesPreserved(t, foreign)
		})

		t.Run(pathForm+"/delete-record", func(t *testing.T) {
			config, ledger, final := newPersistenceFailureLedger(t, shortened)
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

			restore := makeLedgerPersistenceFail(t, ledger)
			if err := ledger.Release(final); err == nil {
				t.Fatal("released-record deletion unexpectedly survived persistence failure")
			}
			restore()
			if got := ledger.records[final].State; got != socketReleased {
				t.Fatalf("in-memory state = %q, want released", got)
			}
			assertPersistedOwnershipState(t, config, final, socketReleased)
			if _, err := os.Lstat(record.Anchor); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("failed record deletion left anchor: %v", err)
			}
			foreign := reoccupySocketCandidates(t, record)
			if err := ledger.Release(final); err != nil {
				t.Fatalf("retry released-record deletion: %v", err)
			}
			assertForeignCandidatesPreserved(t, foreign)
		})
	}
}

func TestOwnershipAnchorCollisionFailsClosedAndRetries(t *testing.T) {
	for _, shortened := range []bool{false, true} {
		pathForm := "direct"
		if shortened {
			pathForm = "shortened"
		}
		t.Run(pathForm, func(t *testing.T) {
			config, ledger, final := newPersistenceFailureLedger(t, shortened)
			record, err := ledger.Reserve(final)
			if err != nil {
				t.Fatal(err)
			}
			bound := socketOwnershipBindPath(record)
			if err := os.MkdirAll(filepath.Dir(bound), 0700); err != nil {
				t.Fatal(err)
			}
			listener, err := listenUnixFresh(bound, 0666)
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			foreignData := []byte("foreign anchor occupant\n")
			if err := os.WriteFile(record.Anchor, foreignData, 0600); err != nil {
				t.Fatal(err)
			}

			if _, err := ledger.Anchor(final); err == nil {
				t.Fatal("Anchor accepted a foreign anchor occupant")
			}
			if err := ledger.Release(final); err == nil {
				t.Fatal("Release removed a foreign anchor occupant")
			}
			got, err := os.ReadFile(record.Anchor)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(foreignData) {
				t.Fatal("failed ownership operations changed the foreign anchor occupant")
			}
			if _, err := os.Lstat(bound); err != nil {
				t.Fatalf("failed ownership operations removed the bound socket: %v", err)
			}
			assertPersistedOwnershipState(t, config, final, socketReserved)

			if err := os.Remove(record.Anchor); err != nil {
				t.Fatal(err)
			}
			if _, err := ledger.Anchor(final); err != nil {
				t.Fatalf("retry Anchor after collision removal: %v", err)
			}
			if err := listener.Close(); err != nil {
				t.Fatal(err)
			}
			if err := ledger.Release(final); err != nil {
				t.Fatalf("release after collision recovery: %v", err)
			}
		})
	}
}

func newPersistenceFailureLedger(
	t *testing.T,
	shortened bool,
) (Config, *socketOwnershipLedger, string) {
	t.Helper()
	config := testConfig(t, false)
	if shortened {
		config = configWithLongRuntime(t, config)
	}
	prepareOwnershipTestRuntime(t, config, false)
	ledger := createTestOwnershipLedger(t, config)
	return config, ledger, HostSocket(
		config.RuntimeDir, DirectionInput, "archive", "documents",
	)
}

func makeLedgerPersistenceFail(
	t *testing.T,
	ledger *socketOwnershipLedger,
) func() {
	t.Helper()
	original := ledger.path
	ledger.path = filepath.Join(ledger.runtimeDir, "missing-ledger-parent", "ledger.json")
	restored := false
	restore := func() {
		if restored {
			return
		}
		ledger.path = original
		restored = true
	}
	t.Cleanup(restore)
	return restore
}

func assertPersistedOwnershipState(
	t *testing.T,
	config Config,
	final string,
	want socketOwnershipState,
) {
	t.Helper()
	loaded, err := loadSocketOwnershipLedger(
		filepath.Join(config.RuntimeDir, socketOwnershipFileName),
		config.RuntimeDir,
		config.InstanceID,
	)
	if err != nil {
		t.Fatal(err)
	}
	record, exists := loaded.records[final]
	if want == "" {
		if exists {
			t.Fatalf("persisted ledger unexpectedly contains %#v", record)
		}
		return
	}
	if !exists || record.State != want {
		t.Fatalf("persisted state = %#v, want %q", record, want)
	}
}

func reoccupySocketCandidates(
	t *testing.T,
	record socketOwnershipRecord,
) map[string][]byte {
	t.Helper()
	foreign := make(map[string][]byte)
	for _, path := range uniqueNonemptyPaths(record.Final, record.Actual) {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		data := []byte("foreign owner at " + path + "\n")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		foreign[path] = data
		t.Cleanup(func() { _ = os.Remove(path) })
	}
	return foreign
}

func assertForeignCandidatesPreserved(t *testing.T, foreign map[string][]byte) {
	t.Helper()
	for path, want := range foreign {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read reoccupied path %s: %v", path, err)
		}
		if string(got) != string(want) {
			t.Fatalf("reoccupied path %s content changed", path)
		}
	}
}
