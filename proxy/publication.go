package proxy

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/glguida/dcomp/hostfs"
	"io"
	"net"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

type resyncFaultInjector func(string) error

const (
	resyncStepPrepareClaim      = "prepare-claim"
	resyncStepPrepareBind       = "prepare-bind"
	resyncStepPrepareAnchor     = "prepare-anchor"
	resyncStepPrepareLink       = "prepare-link"
	resyncStepPublish           = "publish"
	resyncStepPublished         = "published"
	resyncStepCommitLocked      = "commit-locked"
	resyncStepTeardownUnlink    = "teardown-unlink"
	resyncStepOwnershipReleased = "ownership-released"
	resyncStepPersistConfig     = "persist-config"
)

func injectResyncFault(injector resyncFaultInjector, step string) error {
	if injector == nil {
		return nil
	}
	if err := injector(step); err != nil {
		return fmt.Errorf("injected %s failure: %w", step, err)
	}
	return nil
}

// socketPublication keeps every failable bind and validation operation in the
// prepare half of resync. Its durable ledger claim survives transfer to an
// endpointRuntime and remains the sole ownership authority until teardown.
type socketPublication struct {
	ledger   *socketOwnershipLedger
	record   socketOwnershipRecord
	listener net.Listener
	faults   resyncFaultInjector
}

func prepareSocketPublication(
	ledger *socketOwnershipLedger,
	final string,
	mode os.FileMode,
	faults resyncFaultInjector,
) (*socketPublication, error) {
	if ledger == nil {
		return nil, fmt.Errorf("socket ownership ledger is nil")
	}
	runtimeDir := ledger.runtimeDir
	if filepath.Dir(final) != filepath.Join(runtimeDir, "in") &&
		filepath.Dir(final) != filepath.Join(runtimeDir, "out") &&
		final != controlSocketFile(runtimeDir) {
		return nil, fmt.Errorf("endpoint path is outside the proxy endpoint directories")
	}
	if ledger.Has(final) {
		if err := ledger.Release(final); err != nil {
			return nil, fmt.Errorf("recover previous socket claim: %w", err)
		}
	}
	if err := requireAbsent(final); err != nil {
		return nil, err
	}

	temporary := publicationTemporaryPath(final, ledger.instanceID)
	actual := final
	if len(temporary) >= 104 {
		actual = shortenedSocketPath(final)
	}
	direct := actual == final
	bindPath := temporary
	if !direct {
		bindPath = actual
	}
	for _, path := range []string{temporary, bindPath} {
		if path == final {
			continue
		}
		if err := requireAbsent(path); err != nil {
			return nil, err
		}
	}
	if err := hostfs.SocketDirectory(filepath.Dir(bindPath), runtimeDir); err != nil {
		return nil, fmt.Errorf("create shortened socket directory: %w", err)
	}
	if err := validateSocketDirectory(filepath.Dir(bindPath)); err != nil {
		return nil, err
	}

	if err := injectResyncFault(faults, resyncStepPrepareClaim); err != nil {
		return nil, err
	}
	record, err := ledger.Reserve(final)
	if err != nil {
		return nil, fmt.Errorf("reserve socket ownership: %w", err)
	}
	publication := &socketPublication{ledger: ledger, record: record, faults: faults}

	if err := injectResyncFault(faults, resyncStepPrepareBind); err != nil {
		return nil, errors.Join(err, publication.Abort())
	}
	listener, err := listenUnixFresh(bindPath, mode)
	if err != nil {
		return nil, errors.Join(err, publication.Abort())
	}
	publication.listener = listener
	if err := injectResyncFault(faults, resyncStepPrepareAnchor); err != nil {
		return nil, errors.Join(err, publication.Abort())
	}
	recorded, err := ledger.Anchor(final)
	if err != nil {
		return nil, errors.Join(err, publication.Abort())
	}
	publication.record = recorded
	if !direct {
		if err := injectResyncFault(faults, resyncStepPrepareLink); err != nil {
			return nil, errors.Join(err, publication.Abort())
		}
		if err := os.Link(actual, temporary); err != nil {
			return nil, errors.Join(
				fmt.Errorf("create temporary exposed socket link: %w", err),
				publication.Abort(),
			)
		}
	}
	return publication, nil
}

func (publication *socketPublication) Publish() error {
	if publication == nil {
		return fmt.Errorf("socket publication is nil")
	}
	if publication.record.State != socketAnchored {
		return fmt.Errorf("socket publication is not anchored")
	}
	if matches, err := socketOwnershipOwnsPath(
		publication.record, publication.record.Final,
	); err != nil {
		return err
	} else if matches {
		return nil
	}
	if err := injectResyncFault(publication.faults, resyncStepPublish); err != nil {
		return err
	}
	if matches, err := socketOwnershipOwnsPath(
		publication.record, publication.record.Temporary,
	); err != nil {
		return err
	} else if !matches {
		return fmt.Errorf("temporary socket publication is no longer owned")
	}
	if err := renameNoReplace(publication.record.Temporary, publication.record.Final); err != nil {
		return err
	}
	return nil
}

func (publication *socketPublication) Abort() error {
	if publication == nil {
		return nil
	}
	var result error
	if publication.listener != nil {
		if err := publication.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			result = errors.Join(result, err)
		}
	}
	result = errors.Join(result, publication.ledger.Release(publication.record.Final))
	return result
}

func (publication *socketPublication) FinalPath() string      { return publication.record.Final }
func (publication *socketPublication) ActualPath() string     { return publication.record.Actual }
func (publication *socketPublication) Listener() net.Listener { return publication.listener }

func (publication *socketPublication) Paths() []string {
	if publication == nil {
		return nil
	}
	if publication.record.State != socketAnchored {
		return nil
	}
	var paths []string
	for _, path := range socketOwnershipCandidates(publication.record) {
		matches, err := socketOwnershipOwnsPath(publication.record, path)
		if err == nil && matches {
			paths = append(paths, path)
		}
	}
	return paths
}

func publicationTemporaryPath(final, instanceID string) string {
	sum := sha256.Sum256([]byte(instanceID + "\x00" + final))
	return filepath.Join(filepath.Dir(final), ".p-"+hex.EncodeToString(sum[:8]))
}

func requireAbsent(path string) error {
	_, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("refusing to replace occupied target path %s", path)
}

func validateSocketDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("proxy socket directory %s is not a directory", path)
	}
	if err := hostfs.RestrictDirectory(path); err != nil {
		return err
	}
	return nil
}

func renameNoReplace(oldPath, newPath string) error {
	err := unix.Renameat2(
		unix.AT_FDCWD, oldPath, unix.AT_FDCWD, newPath, unix.RENAME_NOREPLACE,
	)
	if err == nil {
		return nil
	}
	if !errors.Is(err, unix.ENOSYS) && !errors.Is(err, unix.EINVAL) &&
		!errors.Is(err, unix.EOPNOTSUPP) {
		return err
	}
	// A hard link followed by unlink has the same atomic visibility property
	// for a Unix-domain socket inode and never replaces an occupied target.
	if err := os.Link(oldPath, newPath); err != nil {
		return err
	}
	if err := os.Remove(oldPath); err != nil {
		_ = os.Remove(newPath)
		return err
	}
	return nil
}

func listenUnix(path string, mode os.FileMode) (net.Listener, error) {
	listenPath := socketListenPath(path)
	if err := hostfs.SocketDirectory(filepath.Dir(listenPath), filepath.Dir(path)); err != nil {
		return nil, err
	}
	if err := validateSocketDirectory(filepath.Dir(listenPath)); err != nil {
		return nil, err
	}
	candidates := []string{listenPath}
	if listenPath != path {
		candidates = append(candidates, path)
	}
	existing := make(map[string]os.FileInfo, len(candidates))
	for _, candidate := range candidates {
		info, err := os.Lstat(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("refusing to replace non-socket path %s", candidate)
		}
		existing[candidate] = info
	}
	for _, candidate := range candidates {
		if _, exists := existing[candidate]; !exists {
			continue
		}
		if err := os.Remove(candidate); err != nil {
			return nil, err
		}
	}
	listener, err := listenUnixFresh(listenPath, mode)
	if err != nil {
		return nil, err
	}
	if listenPath != path {
		if err := os.Link(listenPath, path); err != nil {
			_ = listener.Close()
			_ = os.Remove(listenPath)
			return nil, fmt.Errorf("expose long proxy socket path: %w", err)
		}
	}
	return listener, nil
}

func listenUnixFresh(path string, mode os.FileMode) (net.Listener, error) {
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if unixListener, ok := listener.(*net.UnixListener); ok {
		unixListener.SetUnlinkOnClose(false)
	}
	if err := os.Chmod(path, hostfs.Mode(path, mode)); err != nil {
		_ = listener.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return listener, nil
}

func readControlMessage(reader io.Reader, target interface{}) error {
	limited := &io.LimitedReader{R: reader, N: maxControlMessage + 1}
	line, err := bufio.NewReader(limited).ReadBytes('\n')
	if len(line) > maxControlMessage {
		return fmt.Errorf("control message exceeds %d bytes", maxControlMessage)
	}
	if err != nil {
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("control message is not newline terminated")
		}
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("control message contains trailing data")
	}
	return nil
}

func writeControlMessage(writer io.Writer, value interface{}) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(encoded)+1 > maxControlMessage {
		return fmt.Errorf("control message exceeds %d bytes", maxControlMessage)
	}
	encoded = append(encoded, '\n')
	_, err = writer.Write(encoded)
	return err
}

func writeExclusive(path string, data []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".proxy-tmp-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer func() {
		_ = file.Close()
		_ = os.Remove(temporary)
	}()
	if err := file.Chmod(hostfs.Mode(path, mode)); err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return renameNoReplace(temporary, path)
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	file, err := os.CreateTemp(directory, ".proxy-tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary proxy file: %w", err)
	}
	name := file.Name()
	defer os.Remove(name)
	if err := file.Chmod(hostfs.Mode(path, mode)); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return nil
}
