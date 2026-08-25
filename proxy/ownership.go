package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

const socketOwnershipFileName = "proxy.ownership.json"

// artifactIdentity is sufficient for process-private metadata removed in one
// cleanup pass. Publicly reusable socket paths use durable hard-link anchors
// below, because device/inode pairs alone can be reused after unlink.
type artifactIdentity struct {
	device uint64
	inode  uint64
}

type ownedFile struct {
	path     string
	identity artifactIdentity
}

func writeOwnedFile(path string, data []byte, mode os.FileMode) (*ownedFile, error) {
	if err := writeAtomic(path, data, mode); err != nil {
		return nil, err
	}
	identity, err := artifactIdentityAt(path)
	if err != nil {
		return nil, err
	}
	return &ownedFile{path: path, identity: identity}, nil
}

func writeOwnedFileExclusive(path string, data []byte, mode os.FileMode) (*ownedFile, error) {
	if err := writeExclusive(path, data, mode); err != nil {
		return nil, err
	}
	identity, err := artifactIdentityAt(path)
	if err != nil {
		return nil, err
	}
	return &ownedFile{path: path, identity: identity}, nil
}

func (file *ownedFile) Remove() error {
	if file == nil {
		return nil
	}
	matches, err := artifactAtPathMatches(file.path, file.identity)
	if err != nil {
		return err
	}
	if !matches {
		if _, err := os.Lstat(file.path); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("owned file identity changed at %s", file.path)
	}
	if err := os.Remove(file.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

type socketOwnershipRecord struct {
	Final     string               `json:"final"`
	Temporary string               `json:"temporary"`
	Actual    string               `json:"actual"`
	Anchor    string               `json:"anchor"`
	State     socketOwnershipState `json:"state"`
}

type socketOwnershipState string

const (
	socketReserved socketOwnershipState = "reserved"
	socketAnchored socketOwnershipState = "anchored"
	socketReleased socketOwnershipState = "released"
)

type socketOwnershipDocument struct {
	InstanceID string                  `json:"instance_id"`
	RuntimeDir string                  `json:"runtime_dir"`
	Sockets    []socketOwnershipRecord `json:"sockets"`
}

// socketOwnershipLedger is the single source of pathname ownership for one
// proxy process. The same records drive live teardown, graceful shutdown, and
// dead-process recovery. Config describes wiring; it is deliberately not used
// to infer ownership.
type socketOwnershipLedger struct {
	instanceID string
	runtimeDir string
	path       string
	records    map[string]socketOwnershipRecord
	faults     resyncFaultInjector
}

func (ledger *socketOwnershipLedger) Has(final string) bool {
	if ledger == nil {
		return false
	}
	_, exists := ledger.records[final]
	return exists
}

func createSocketOwnershipLedger(
	runtimeDir,
	instanceID string,
) (*socketOwnershipLedger, error) {
	ledger := &socketOwnershipLedger{
		instanceID: instanceID,
		runtimeDir: runtimeDir,
		path:       filepath.Join(runtimeDir, socketOwnershipFileName),
		records:    make(map[string]socketOwnershipRecord),
	}
	encoded, err := ledger.encode(ledger.records)
	if err != nil {
		return nil, err
	}
	if err := writeExclusive(ledger.path, encoded, 0600); err != nil {
		return nil, fmt.Errorf("create socket ownership ledger: %w", err)
	}
	return ledger, nil
}

func loadSocketOwnershipLedger(
	path,
	runtimeDir,
	instanceID string,
) (*socketOwnershipLedger, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var document socketOwnershipDocument
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("socket ownership ledger contains trailing data")
	}
	if document.InstanceID != instanceID || document.RuntimeDir != runtimeDir {
		return nil, fmt.Errorf("%w: socket ownership ledger does not match recorded process", ErrIdentityMismatch)
	}
	ledger := &socketOwnershipLedger{
		instanceID: instanceID,
		runtimeDir: runtimeDir,
		path:       path,
		records:    make(map[string]socketOwnershipRecord, len(document.Sockets)),
	}
	claimed := make(map[string]string)
	for _, record := range document.Sockets {
		if err := validateSocketOwnershipRecord(runtimeDir, instanceID, record); err != nil {
			return nil, err
		}
		if _, exists := ledger.records[record.Final]; exists {
			return nil, fmt.Errorf("duplicate socket ownership record for %s", record.Final)
		}
		for _, candidate := range socketOwnershipAllPaths(record) {
			if owner, exists := claimed[candidate]; exists && owner != record.Final {
				return nil, fmt.Errorf(
					"socket ownership path %s is claimed by both %s and %s",
					candidate, owner, record.Final,
				)
			}
			claimed[candidate] = record.Final
		}
		ledger.records[record.Final] = record
	}
	return ledger, nil
}

func (ledger *socketOwnershipLedger) Reserve(final string) (socketOwnershipRecord, error) {
	if ledger == nil {
		return socketOwnershipRecord{}, fmt.Errorf("socket ownership ledger is nil")
	}
	if _, exists := ledger.records[final]; exists {
		// A failed prepare can leave its write-ahead claim in the live ledger.
		// Identity-checked cleanup makes reclaiming that exact final path safe.
		if err := ledger.Release(final); err != nil {
			return socketOwnershipRecord{}, fmt.Errorf("recover previous socket claim: %w", err)
		}
	}
	temporary := publicationTemporaryPath(final, ledger.instanceID)
	actual := final
	if len(temporary) >= 104 {
		actual = shortenedSocketPath(final)
	}
	record := socketOwnershipRecord{
		Final: final, Temporary: temporary, Actual: actual,
		Anchor: socketOwnershipAnchorPath(ledger.runtimeDir, ledger.instanceID, final),
		State:  socketReserved,
	}
	if err := validateSocketOwnershipRecord(
		ledger.runtimeDir, ledger.instanceID, record,
	); err != nil {
		return socketOwnershipRecord{}, err
	}
	if err := requireAbsent(record.Anchor); err != nil {
		return socketOwnershipRecord{}, err
	}
	next := cloneSocketOwnershipRecords(ledger.records)
	next[final] = record
	if err := ledger.persist(next); err != nil {
		return socketOwnershipRecord{}, err
	}
	ledger.records = next
	return record, nil
}

func (ledger *socketOwnershipLedger) Anchor(final string) (socketOwnershipRecord, error) {
	record, exists := ledger.records[final]
	if !exists {
		return socketOwnershipRecord{}, fmt.Errorf("socket ownership claim is missing for %s", final)
	}
	if record.State == socketAnchored {
		return record, nil
	}
	if record.State != socketReserved {
		return socketOwnershipRecord{}, fmt.Errorf(
			"cannot anchor socket ownership in state %q", record.State,
		)
	}
	bound := socketOwnershipBindPath(record)
	boundInfo, err := os.Lstat(bound)
	if err != nil {
		return socketOwnershipRecord{}, err
	}
	if boundInfo.Mode()&os.ModeSocket == 0 {
		return socketOwnershipRecord{}, fmt.Errorf("bound socket artifact is not a socket: %s", bound)
	}
	anchorInfo, err := os.Lstat(record.Anchor)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Link(bound, record.Anchor); err != nil {
			return socketOwnershipRecord{}, fmt.Errorf("anchor socket ownership: %w", err)
		}
	} else if err != nil {
		return socketOwnershipRecord{}, err
	} else if !os.SameFile(boundInfo, anchorInfo) {
		return socketOwnershipRecord{}, fmt.Errorf("socket ownership anchor names another object")
	}
	record.State = socketAnchored
	next := cloneSocketOwnershipRecords(ledger.records)
	next[final] = record
	if err := ledger.persist(next); err != nil {
		return socketOwnershipRecord{}, err
	}
	ledger.records = next
	return record, nil
}

// Release advances one durable ownership record through explicit states. The
// private hard-link anchor keeps the socket inode alive while public paths are
// removed, so inode reuse can never make a new occupant compare as ours.
func (ledger *socketOwnershipLedger) Release(final string) error {
	for {
		record, exists := ledger.records[final]
		if !exists {
			return nil
		}
		switch record.State {
		case socketReserved:
			anchorInfo, anchorErr := os.Lstat(record.Anchor)
			if errors.Is(anchorErr, os.ErrNotExist) {
				bound := socketOwnershipBindPath(record)
				boundInfo, boundErr := os.Lstat(bound)
				if errors.Is(boundErr, os.ErrNotExist) {
					record.State = socketReleased
					if err := ledger.replaceRecord(record); err != nil {
						return err
					}
					if err := injectResyncFault(
						ledger.faults, resyncStepOwnershipReleased,
					); err != nil {
						return err
					}
					continue
				}
				if boundErr != nil {
					return boundErr
				}
				if boundInfo.Mode()&os.ModeSocket == 0 {
					return fmt.Errorf("refusing to recover non-socket pending artifact %s", bound)
				}
				if err := os.Link(bound, record.Anchor); err != nil {
					return fmt.Errorf("anchor pending socket ownership: %w", err)
				}
			} else if anchorErr != nil {
				return anchorErr
			} else if anchorInfo.Mode()&os.ModeSocket == 0 {
				return fmt.Errorf("socket ownership anchor is not a socket: %s", record.Anchor)
			}
			record.State = socketAnchored
			if err := ledger.replaceRecord(record); err != nil {
				return err
			}
		case socketAnchored:
			if err := removeSocketOwnershipArtifacts(record); err != nil {
				return err
			}
			record.State = socketReleased
			if err := ledger.replaceRecord(record); err != nil {
				return err
			}
			if err := injectResyncFault(
				ledger.faults, resyncStepOwnershipReleased,
			); err != nil {
				return err
			}
		case socketReleased:
			if info, err := os.Lstat(record.Anchor); err == nil {
				if info.IsDir() {
					return fmt.Errorf("refusing to remove directory at socket ownership anchor %s", record.Anchor)
				}
				if err := os.Remove(record.Anchor); err != nil {
					return err
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			next := cloneSocketOwnershipRecords(ledger.records)
			delete(next, final)
			if err := ledger.persist(next); err != nil {
				return err
			}
			ledger.records = next
			return nil
		default:
			return fmt.Errorf("unknown socket ownership state %q", record.State)
		}
	}
}

func (ledger *socketOwnershipLedger) CleanupAll() error {
	if ledger == nil {
		return nil
	}
	finals := make([]string, 0, len(ledger.records))
	for final := range ledger.records {
		finals = append(finals, final)
	}
	sort.Strings(finals)
	var result error
	for _, final := range finals {
		if err := ledger.Release(final); err != nil {
			result = errors.Join(result, fmt.Errorf("remove owned socket %s: %w", final, err))
		}
	}
	return result
}

func (ledger *socketOwnershipLedger) Close() error {
	if ledger == nil {
		return nil
	}
	if len(ledger.records) != 0 {
		return fmt.Errorf("socket ownership ledger still contains %d claim(s)", len(ledger.records))
	}
	if err := os.Remove(ledger.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (ledger *socketOwnershipLedger) CandidatePaths() map[string]struct{} {
	paths := make(map[string]struct{})
	if ledger == nil {
		return paths
	}
	for _, record := range ledger.records {
		for _, path := range socketOwnershipCandidates(record) {
			paths[path] = struct{}{}
		}
	}
	return paths
}

func (ledger *socketOwnershipLedger) ShortRoots() []string {
	roots := make(map[string]struct{})
	if ledger != nil {
		for _, record := range ledger.records {
			if record.Actual != record.Final {
				roots[filepath.Dir(record.Actual)] = struct{}{}
			}
		}
	}
	result := make([]string, 0, len(roots))
	for root := range roots {
		result = append(result, root)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(result)))
	return result
}

func (ledger *socketOwnershipLedger) persist(
	records map[string]socketOwnershipRecord,
) error {
	encoded, err := ledger.encode(records)
	if err != nil {
		return err
	}
	if err := writeAtomic(ledger.path, encoded, 0600); err != nil {
		return fmt.Errorf("persist socket ownership ledger: %w", err)
	}
	return nil
}

func (ledger *socketOwnershipLedger) replaceRecord(record socketOwnershipRecord) error {
	next := cloneSocketOwnershipRecords(ledger.records)
	next[record.Final] = record
	if err := ledger.persist(next); err != nil {
		return err
	}
	ledger.records = next
	return nil
}

func (ledger *socketOwnershipLedger) encode(
	records map[string]socketOwnershipRecord,
) ([]byte, error) {
	document := socketOwnershipDocument{
		InstanceID: ledger.instanceID,
		RuntimeDir: ledger.runtimeDir,
		Sockets:    make([]socketOwnershipRecord, 0, len(records)),
	}
	for _, record := range records {
		document.Sockets = append(document.Sockets, record)
	}
	sort.Slice(document.Sockets, func(left, right int) bool {
		return document.Sockets[left].Final < document.Sockets[right].Final
	})
	encoded, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode socket ownership ledger: %w", err)
	}
	return append(encoded, '\n'), nil
}

func validateSocketOwnershipRecord(
	runtimeDir,
	instanceID string,
	record socketOwnershipRecord,
) error {
	if !filepath.IsAbs(record.Final) || filepath.Clean(record.Final) != record.Final {
		return fmt.Errorf("socket ownership record has unclean final path %s", record.Final)
	}
	control := controlSocketFile(runtimeDir)
	finalDirectory := filepath.Dir(record.Final)
	if record.Final != control &&
		finalDirectory != filepath.Join(runtimeDir, "in") &&
		finalDirectory != filepath.Join(runtimeDir, "out") {
		return fmt.Errorf("socket ownership record has unsafe final path %s", record.Final)
	}
	expectedTemporary := publicationTemporaryPath(record.Final, instanceID)
	if record.Temporary != expectedTemporary {
		return fmt.Errorf("socket ownership record has unsafe temporary path %s", record.Temporary)
	}
	expectedActual := record.Final
	if len(expectedTemporary) >= 104 {
		expectedActual = shortenedSocketPath(record.Final)
	}
	if record.Actual != expectedActual {
		return fmt.Errorf("socket ownership record has unsafe actual path %s", record.Actual)
	}
	if record.Anchor != socketOwnershipAnchorPath(runtimeDir, instanceID, record.Final) {
		return fmt.Errorf("socket ownership record has unsafe anchor path %s", record.Anchor)
	}
	switch record.State {
	case socketReserved, socketAnchored, socketReleased:
	default:
		return fmt.Errorf("socket ownership record has unknown state %q", record.State)
	}
	return nil
}

func removeSocketOwnershipArtifacts(record socketOwnershipRecord) error {
	anchorInfo, err := os.Lstat(record.Anchor)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("socket ownership anchor is missing for %s", record.Final)
		}
		return err
	}
	if anchorInfo.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("socket ownership anchor is not a socket: %s", record.Anchor)
	}
	var result error
	for _, path := range socketOwnershipCandidates(record) {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		if !os.SameFile(info, anchorInfo) {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
		}
	}
	return result
}

func socketOwnershipCandidates(record socketOwnershipRecord) []string {
	return uniqueNonemptyPaths(record.Temporary, record.Final, record.Actual)
}

func socketOwnershipAllPaths(record socketOwnershipRecord) []string {
	return uniqueNonemptyPaths(record.Temporary, record.Final, record.Actual, record.Anchor)
}

func socketOwnershipBindPath(record socketOwnershipRecord) string {
	if record.Actual != record.Final {
		return record.Actual
	}
	return record.Temporary
}

func socketOwnershipAnchorPath(runtimeDir, instanceID, final string) string {
	return filepath.Join(
		runtimeDir,
		".a-"+strings.TrimPrefix(filepath.Base(publicationTemporaryPath(final, instanceID)), ".p-"),
	)
}

func socketOwnershipOwnsPath(record socketOwnershipRecord, path string) (bool, error) {
	anchorInfo, err := os.Lstat(record.Anchor)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return os.SameFile(info, anchorInfo), nil
}

func uniqueNonemptyPaths(paths ...string) []string {
	result := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		if _, exists := seen[path]; exists {
			continue
		}
		seen[path] = struct{}{}
		result = append(result, path)
	}
	return result
}

func artifactIdentityAt(path string) (artifactIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return artifactIdentity{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return artifactIdentity{}, fmt.Errorf("unsupported filesystem stat type %T", info.Sys())
	}
	return artifactIdentity{device: uint64(stat.Dev), inode: stat.Ino}, nil
}

func artifactAtPathMatches(path string, want artifactIdentity) (bool, error) {
	got, err := artifactIdentityAt(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return got == want, nil
}

func cloneSocketOwnershipRecords(
	records map[string]socketOwnershipRecord,
) map[string]socketOwnershipRecord {
	clone := make(map[string]socketOwnershipRecord, len(records))
	for final, record := range records {
		clone[final] = record
	}
	return clone
}
