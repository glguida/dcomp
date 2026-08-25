package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestInvalidSocketOwnershipLedgerFailsClosedWithoutMutation(t *testing.T) {
	tests := []struct {
		name         string
		mutate       func(*socketOwnershipDocument)
		transform    func([]byte) []byte
		wantIdentity bool
	}{
		{
			name: "malformed JSON",
			transform: func([]byte) []byte {
				return []byte(`{"instance_id":`)
			},
		},
		{
			name: "unknown document field",
			transform: func(encoded []byte) []byte {
				return bytes.Replace(encoded, []byte("{"), []byte(`{"unknown":true,`), 1)
			},
		},
		{
			name: "unknown record field",
			transform: func(encoded []byte) []byte {
				return bytes.Replace(
					encoded,
					[]byte(`"final":`),
					[]byte(`"unknown":true,"final":`),
					1,
				)
			},
		},
		{
			name: "trailing document",
			transform: func(encoded []byte) []byte {
				return append(encoded, []byte("{}\n")...)
			},
		},
		{
			name: "different instance",
			mutate: func(document *socketOwnershipDocument) {
				document.InstanceID = "different-instance"
			},
			wantIdentity: true,
		},
		{
			name: "different runtime",
			mutate: func(document *socketOwnershipDocument) {
				document.RuntimeDir = filepath.Join(document.RuntimeDir, "different")
			},
			wantIdentity: true,
		},
		{
			name: "unsafe final path",
			mutate: func(document *socketOwnershipDocument) {
				document.Sockets[0].Final = filepath.Join(os.TempDir(), "outside.sock")
			},
		},
		{
			name: "unclean final path",
			mutate: func(document *socketOwnershipDocument) {
				record := &document.Sockets[0]
				record.Final = filepath.Join(document.RuntimeDir, "in", "alias") +
					"/../archive.documents"
				*record = ownershipRecordForFinal(
					document.RuntimeDir,
					document.InstanceID,
					record.Final,
					record.State,
				)
			},
		},
		{
			name: "unsafe temporary path",
			mutate: func(document *socketOwnershipDocument) {
				document.Sockets[0].Temporary = filepath.Join(document.RuntimeDir, "foreign.tmp")
			},
		},
		{
			name: "unsafe actual path",
			mutate: func(document *socketOwnershipDocument) {
				document.Sockets[0].Actual = filepath.Join(document.RuntimeDir, "foreign.sock")
			},
		},
		{
			name: "unsafe anchor path",
			mutate: func(document *socketOwnershipDocument) {
				document.Sockets[0].Anchor = filepath.Join(document.RuntimeDir, ".a-foreign")
			},
		},
		{
			name: "unknown state",
			mutate: func(document *socketOwnershipDocument) {
				document.Sockets[0].State = "unknown"
			},
		},
		{
			name: "duplicate final claim",
			mutate: func(document *socketOwnershipDocument) {
				document.Sockets = append(document.Sockets, document.Sockets[0])
			},
		},
		{
			name: "overlapping derived path claims",
			mutate: func(document *socketOwnershipDocument) {
				first := document.Sockets[0]
				document.Sockets = append(
					document.Sockets,
					ownershipRecordForFinal(
						document.RuntimeDir,
						document.InstanceID,
						first.Temporary,
						socketReserved,
					),
				)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := testConfig(t, false)
			process := prepareOwnershipTestRuntime(t, config, true)
			final := HostSocket(
				config.RuntimeDir, DirectionInput, "archive", "documents",
			)
			document := socketOwnershipDocument{
				InstanceID: config.InstanceID,
				RuntimeDir: config.RuntimeDir,
				Sockets: []socketOwnershipRecord{ownershipRecordForFinal(
					config.RuntimeDir,
					config.InstanceID,
					final,
					socketReserved,
				)},
			}
			if test.mutate != nil {
				test.mutate(&document)
			}
			encoded, err := json.MarshalIndent(document, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			encoded = append(encoded, '\n')
			if test.transform != nil {
				encoded = test.transform(encoded)
			}
			ownershipPath := filepath.Join(config.RuntimeDir, socketOwnershipFileName)
			if err := os.WriteFile(ownershipPath, encoded, 0600); err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(config.RuntimeDir, "in", "foreign-entry")
			if err := os.WriteFile(sentinel, []byte("foreign owner\n"), 0600); err != nil {
				t.Fatal(err)
			}

			_, loadErr := loadSocketOwnershipLedger(
				ownershipPath, config.RuntimeDir, config.InstanceID,
			)
			if loadErr == nil {
				t.Fatal("invalid socket ownership ledger was accepted")
			}
			if test.wantIdentity && !errors.Is(loadErr, ErrIdentityMismatch) {
				t.Fatalf("load error = %v, want identity mismatch", loadErr)
			}

			cleanupErr := cleanupCrashedRuntime(process)
			if cleanupErr == nil {
				t.Fatal("crash cleanup mutated a runtime with an invalid ownership ledger")
			}
			if test.wantIdentity && !errors.Is(cleanupErr, ErrIdentityMismatch) {
				t.Fatalf("cleanup error = %v, want identity mismatch", cleanupErr)
			}
			for _, path := range []string{
				ownershipPath,
				filepath.Join(config.RuntimeDir, ConfigFileName),
				filepath.Join(config.RuntimeDir, PIDFileName),
				sentinel,
			} {
				if _, err := os.Lstat(path); err != nil {
					t.Fatalf("failed-closed cleanup removed %s: %v", path, err)
				}
			}
			after, err := os.ReadFile(ownershipPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, encoded) {
				t.Fatal("failed-closed cleanup rewrote the invalid ownership ledger")
			}
		})
	}
}

func TestSocketOwnershipLedgerRoundTripsEveryStateAndPathForm(t *testing.T) {
	for _, shortened := range []bool{false, true} {
		pathForm := "direct"
		if shortened {
			pathForm = "shortened"
		}
		for _, state := range []socketOwnershipState{
			socketReserved,
			socketAnchored,
			socketReleased,
		} {
			t.Run(pathForm+"/"+string(state), func(t *testing.T) {
				config := testConfig(t, false)
				if shortened {
					config = configWithLongRuntime(t, config)
				}
				if err := os.MkdirAll(config.RuntimeDir, 0700); err != nil {
					t.Fatal(err)
				}
				final := HostSocket(
					config.RuntimeDir, DirectionInput, "archive", "documents",
				)
				record := ownershipRecordForFinal(
					config.RuntimeDir, config.InstanceID, final, state,
				)
				document := socketOwnershipDocument{
					InstanceID: config.InstanceID,
					RuntimeDir: config.RuntimeDir,
					Sockets:    []socketOwnershipRecord{record},
				}
				encoded, err := json.Marshal(document)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(config.RuntimeDir, socketOwnershipFileName)
				if err := os.WriteFile(path, encoded, 0600); err != nil {
					t.Fatal(err)
				}
				loaded, err := loadSocketOwnershipLedger(
					path, config.RuntimeDir, config.InstanceID,
				)
				if err != nil {
					t.Fatal(err)
				}
				if got := loaded.records[final]; got != record {
					t.Fatalf("round-tripped record = %#v, want %#v", got, record)
				}
			})
		}
	}
}

func ownershipRecordForFinal(
	runtimeDir,
	instanceID,
	final string,
	state socketOwnershipState,
) socketOwnershipRecord {
	temporary := publicationTemporaryPath(final, instanceID)
	actual := final
	if len(temporary) >= 104 {
		actual = shortenedSocketPath(final)
	}
	return socketOwnershipRecord{
		Final: final, Temporary: temporary, Actual: actual,
		Anchor: socketOwnershipAnchorPath(runtimeDir, instanceID, final),
		State:  state,
	}
}
