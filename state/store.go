// Package state provides the complete durable state used by the dcomp host
// controller: one committed deployment and at most one pending operation.
package state

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/glguida/dcomp/hostfs"
	"io"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/glguida/dcomp/composition"
	"github.com/glguida/dcomp/proxy"
)

const (
	formatVersion        = 5
	engineBindingVersion = 2
	documentSizeLimit    = 16 * 1024 * 1024
)

type Resource struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Deployment struct {
	Version     int                      `json:"version"`
	Spec        composition.ResolvedSpec `json:"spec"`
	RuntimeRoot string                   `json:"runtime_root"`
	Proxy       *proxy.Process           `json:"proxy"`
	Networks    map[string]Resource      `json:"networks"`
	Containers  map[string]Resource      `json:"containers"`
}

// EndpointCleanup is the durable identity of one container endpoint that
// must be absent before its container-removal step is complete.
type EndpointCleanup struct {
	ContainerID   string `json:"container_id"`
	ContainerName string `json:"container_name"`
	NetworkKey    string `json:"network_key"`
	NetworkID     string `json:"network_id"`
	NetworkName   string `json:"network_name"`
	EndpointID    string `json:"endpoint_id"`
	EndpointName  string `json:"endpoint_name"`
}

type Operation struct {
	Version            int                      `json:"version"`
	ID                 string                   `json:"id"`
	Kind               string                   `json:"kind"`
	Phase              string                   `json:"phase"`
	Target             composition.ResolvedSpec `json:"target"`
	TargetWiring       proxy.Wiring             `json:"target_wiring,omitempty,omitzero"`
	TargetWiringDigest string                   `json:"target_wiring_digest,omitempty"`
	RuntimeRoot        string                   `json:"runtime_root"`
	Proxy              *proxy.Process           `json:"proxy,omitempty"`
	Previous           *Deployment              `json:"previous,omitempty"`
	Networks           map[string]Resource      `json:"networks"`
	Containers         map[string]Resource      `json:"containers"`
	Components         []string                 `json:"components,omitempty"`
	Completed          map[string]bool          `json:"completed,omitempty"`
	// AbortRecreatePrevious records that reverse resync could not converge and
	// abort must finish by applying the previous deployment as a fresh fleet.
	AbortRecreatePrevious bool `json:"abort_recreate_previous,omitempty"`
	// PendingCreates records a create request before it is sent to Docker and
	// is cleared in the same durable update that records the returned object.
	PendingCreates map[string]bool `json:"pending_creates,omitempty"`
	// EndpointCleanups records an endpoint before its container is removed.
	// It is cleared only after network inspection proves the endpoint absent.
	EndpointCleanups map[string]EndpointCleanup `json:"endpoint_cleanups,omitempty"`
}

func NewOperation(
	kind, phase string,
	target composition.ResolvedSpec,
	previous *Deployment,
	runtimeRoot string,
) (Operation, error) {
	if err := validateRuntimeRoot(runtimeRoot); err != nil {
		return Operation{}, err
	}
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return Operation{}, fmt.Errorf("generate operation ID: %w", err)
	}
	operation := Operation{
		Version:          formatVersion,
		ID:               hex.EncodeToString(idBytes),
		Kind:             kind,
		Phase:            phase,
		Target:           target,
		RuntimeRoot:      runtimeRoot,
		Previous:         previous,
		Networks:         make(map[string]Resource),
		Containers:       make(map[string]Resource),
		Completed:        make(map[string]bool),
		PendingCreates:   make(map[string]bool),
		EndpointCleanups: make(map[string]EndpointCleanup),
	}
	if kind != "apply" {
		return operation, nil
	}
	wiring, err := proxy.NewWiring(target)
	if err != nil {
		return Operation{}, fmt.Errorf("derive target proxy wiring: %w", err)
	}
	wiringDigest, err := wiring.Digest()
	if err != nil {
		return Operation{}, fmt.Errorf("digest target proxy wiring: %w", err)
	}
	operation.TargetWiring = wiring
	operation.TargetWiringDigest = wiringDigest
	return operation, nil
}

type engineBinding struct {
	Version int    `json:"version"`
	ID      string `json:"id"`
}

type Store struct {
	Root string
}

// Systems returns every system name with a state directory, in stable order.
// It observes the state root only and never creates directories or lock files.
func (store Store) Systems() ([]string, error) {
	if err := store.validateRoot(); err != nil {
		return nil, err
	}
	directory := filepath.Join(store.Root, "systems")
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list systems: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !composition.ValidName(entry.Name()) {
			return nil, fmt.Errorf(
				"invalid entry %q in systems state directory",
				entry.Name(),
			)
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names, nil
}

func DefaultRoot() (string, error) {
	if root := os.Getenv("DCOMP_STATE_ROOT"); root != "" {
		if !filepath.IsAbs(root) {
			return "", fmt.Errorf("DCOMP_STATE_ROOT must be an absolute path")
		}
		return root, nil
	}
	if root := os.Getenv("XDG_STATE_HOME"); root != "" {
		if !filepath.IsAbs(root) {
			return "", fmt.Errorf("XDG_STATE_HOME must be an absolute path")
		}
		return filepath.Join(root, "dcomp"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find home directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "dcomp"), nil
}

type Lock struct {
	file *os.File
}

func (lock *Lock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	unlockErr := syscall.Flock(int(lock.file.Fd()), syscall.LOCK_UN)
	closeErr := lock.file.Close()
	if unlockErr != nil {
		return unlockErr
	}
	return closeErr
}

// Acquire obtains a non-stale kernel lock for one system. The lock file
// persists, but ownership does not: the kernel releases it on process death.
func (store Store) Acquire(name string) (*Lock, error) {
	return store.acquire(context.Background(), name, false)
}

// AcquireContext waits for another program's operation to finish. Cancellation
// releases the waiting descriptor; the kernel still owns lock lifetime.
func (store Store) AcquireContext(ctx context.Context, name string) (*Lock, error) {
	return store.acquire(ctx, name, true)
}

func (store Store) acquire(ctx context.Context, name string, wait bool) (*Lock, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	directory, err := store.directory(name)
	if err != nil {
		return nil, err
	}
	if err := makeDirectoryDurable(directory, 0700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	file, err := hostfs.OpenFile(filepath.Join(directory, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("open state lock: %w", err)
	}
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			file.Close()
			return nil, fmt.Errorf("lock system %q: %w", name, err)
		}
		if !wait {
			file.Close()
			return nil, fmt.Errorf("another dcomp operation is running for %q", name)
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			file.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return &Lock{file: file}, nil
}

// AcquireShared obtains a read lock for an existing system. It never creates
// state for an absent system. Readers wait for a mutating command to release
// its exclusive lock, so a status snapshot cannot combine lifecycle
// generations.
func (store Store) AcquireShared(ctx context.Context, name string) (*Lock, bool, error) {
	directory, err := store.directory(name)
	if err != nil {
		return nil, false, err
	}
	file, err := hostfs.OpenFile(filepath.Join(directory, "lock"), os.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("open state lock: %w", err)
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
		if err == nil {
			return &Lock{file: file}, true, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			file.Close()
			return nil, false, fmt.Errorf("read-lock system %q: %w", name, err)
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, false, ctx.Err()
		case <-ticker.C:
		}
	}
}

// AcquireAttachment serializes writable standard-I/O attachments to one
// recorded component. The caller must already hold the system's shared lock,
// which prevents lifecycle replacement for the attachment's lifetime.
func (store Store) AcquireAttachment(
	ctx context.Context,
	system string,
	component string,
) (*Lock, error) {
	if !composition.ValidName(component) {
		return nil, fmt.Errorf("invalid component name %q", component)
	}
	directory, err := store.directory(system)
	if err != nil {
		return nil, err
	}
	file, err := hostfs.OpenFile(
		filepath.Join(directory, "attach-"+component+".lock"),
		os.O_CREATE|os.O_RDWR,
		0600,
	)
	if err != nil {
		return nil, fmt.Errorf("open component attachment lock: %w", err)
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &Lock{file: file}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) &&
			!errors.Is(err, syscall.EAGAIN) {
			file.Close()
			return nil, fmt.Errorf("lock component attachment %s.%s: %w", system, component, err)
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// BindEngine binds this state root to one Docker engine ID. The first writer
// publishes engine.json without replacement; concurrent writers can therefore
// never silently rebind the root.
func (store Store) BindEngine(id string) error {
	if id == "" {
		return fmt.Errorf("Docker engine ID is empty")
	}
	if err := store.validateRoot(); err != nil {
		return err
	}
	if binding, exists, err := store.ReadEngine(); err != nil {
		return err
	} else if exists {
		if err := compareEngineBinding(binding, id); err != nil {
			return err
		}
		return syncDirectory(store.Root)
	}
	if err := makeDirectoryDurable(store.Root, 0700); err != nil {
		return fmt.Errorf("create state root: %w", err)
	}
	temporary, err := os.CreateTemp(store.Root, ".engine.json.tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary engine binding: %w", err)
	}
	temporaryName := temporary.Name()
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryName)
	}()
	if err := temporary.Chmod(hostfs.Mode(temporary.Name(), 0600)); err != nil {
		return fmt.Errorf("set engine binding permissions: %w", err)
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(engineBinding{Version: engineBindingVersion, ID: id}); err != nil {
		return fmt.Errorf("encode engine binding: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync engine binding: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close engine binding: %w", err)
	}
	destination := filepath.Join(store.Root, "engine.json")
	if err := os.Link(temporaryName, destination); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("publish engine binding: %w", err)
		}
		binding, exists, readErr := store.ReadEngine()
		if readErr != nil {
			return readErr
		}
		if !exists {
			return fmt.Errorf("engine binding disappeared during publication")
		}
		if err := compareEngineBinding(binding, id); err != nil {
			return err
		}
		return syncDirectory(store.Root)
	}
	return syncDirectory(store.Root)
}

// ReadEngine returns the Docker engine ID bound to this state root.
func (store Store) ReadEngine() (string, bool, error) {
	if err := store.validateRoot(); err != nil {
		return "", false, err
	}
	file, err := os.Open(filepath.Join(store.Root, "engine.json"))
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("open engine binding: %w", err)
	}
	defer file.Close()
	var binding engineBinding
	decoder := json.NewDecoder(io.LimitReader(file, 1024*1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&binding); err != nil {
		return "", false, fmt.Errorf("decode engine binding: %w", err)
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return "", false, fmt.Errorf("decode engine binding: trailing data")
	}
	if binding.Version != engineBindingVersion {
		return "", false, fmt.Errorf(
			"unsupported engine binding version %d", binding.Version,
		)
	}
	if binding.ID == "" {
		return "", false, fmt.Errorf("engine binding has an empty ID")
	}
	return binding.ID, true, nil
}

func (store Store) ReadDesired(name string) (Deployment, bool, error) {
	var deployment Deployment
	exists, err := store.read(name, "desired.json", &deployment)
	if err != nil || !exists {
		return Deployment{}, exists, err
	}
	if err := validateDeployment(name, deployment); err != nil {
		return Deployment{}, false, err
	}
	return deployment, true, nil
}

func (store Store) WriteDesired(name string, deployment Deployment) error {
	deployment.Version = formatVersion
	if err := validateDeployment(name, deployment); err != nil {
		return err
	}
	return store.write(name, "desired.json", deployment)
}

func (store Store) ClearDesired(name string) error {
	return store.remove(name, "desired.json")
}

func (store Store) ReadOperation(name string) (Operation, bool, error) {
	var operation Operation
	exists, err := store.read(name, "operation.json", &operation)
	if err != nil || !exists {
		return Operation{}, exists, err
	}
	if err := validateOperation(name, operation); err != nil {
		return Operation{}, false, err
	}
	if operation.Completed == nil {
		operation.Completed = make(map[string]bool)
	}
	if operation.PendingCreates == nil {
		operation.PendingCreates = make(map[string]bool)
	}
	if operation.EndpointCleanups == nil {
		operation.EndpointCleanups = make(map[string]EndpointCleanup)
	}
	return operation, true, nil
}

func (store Store) WriteOperation(name string, operation Operation) error {
	operation.Version = formatVersion
	if operation.Previous != nil {
		previous := *operation.Previous
		previous.Version = formatVersion
		operation.Previous = &previous
	}
	if err := validateOperation(name, operation); err != nil {
		return err
	}
	return store.write(name, "operation.json", operation)
}

func validateDeployment(name string, deployment Deployment) error {
	if deployment.Version != formatVersion && deployment.Version != 4 {
		return fmt.Errorf("unsupported desired state version %d", deployment.Version)
	}
	if deployment.Spec.Name != name {
		return fmt.Errorf(
			"desired state belongs to system %q, not %q", deployment.Spec.Name, name,
		)
	}
	if deployment.Containers == nil {
		return fmt.Errorf("desired state has no container map")
	}
	if deployment.Networks == nil {
		return fmt.Errorf("desired state has no network map")
	}
	if err := validateRuntimeRoot(deployment.RuntimeRoot); err != nil {
		return fmt.Errorf("desired state: %w", err)
	}
	if deployment.Proxy == nil {
		return fmt.Errorf("desired state has no proxy record")
	}
	if err := deployment.Proxy.Validate(); err != nil {
		return fmt.Errorf("desired state has invalid proxy record: %w", err)
	}
	if deployment.Proxy.Digest == "" {
		return fmt.Errorf("desired state proxy has no wiring digest")
	}
	if deployment.Proxy.RuntimeDir != filepath.Join(deployment.RuntimeRoot, name) {
		return fmt.Errorf("desired state proxy is outside its runtime root")
	}
	return nil
}

func validateOperation(name string, operation Operation) error {
	if operation.Version != formatVersion && operation.Version != 4 {
		return fmt.Errorf("unsupported operation state version %d", operation.Version)
	}
	if operation.ID == "" || operation.Kind == "" || operation.Phase == "" {
		return fmt.Errorf("operation state is incomplete")
	}
	if operation.Target.Name != name {
		return fmt.Errorf(
			"operation state belongs to system %q, not %q", operation.Target.Name, name,
		)
	}
	if operation.Containers == nil {
		return fmt.Errorf("operation state has no container map")
	}
	if operation.Networks == nil {
		return fmt.Errorf("operation state has no network map")
	}
	if err := validateRuntimeRoot(operation.RuntimeRoot); err != nil {
		return fmt.Errorf("operation state: %w", err)
	}
	if operation.Proxy != nil {
		if err := operation.Proxy.Validate(); err != nil {
			return fmt.Errorf("operation state has invalid proxy record: %w", err)
		}
		if operation.Proxy.RuntimeDir != filepath.Join(operation.RuntimeRoot, name) {
			return fmt.Errorf("operation state proxy is outside its runtime root")
		}
	}
	if operation.Previous != nil {
		if err := validateDeployment(name, *operation.Previous); err != nil {
			return fmt.Errorf("previous deployment is invalid: %w", err)
		}
	}
	if operation.Kind == "apply" && operation.TargetWiringDigest == "" {
		return fmt.Errorf("apply operation has no target wiring digest")
	}
	if operation.TargetWiringDigest != "" {
		digest, err := operation.TargetWiring.Digest()
		if err != nil {
			return fmt.Errorf("operation target wiring is invalid: %w", err)
		}
		if digest != operation.TargetWiringDigest {
			return fmt.Errorf(
				"operation target wiring digest mismatch: expected %s, found %s",
				digest, operation.TargetWiringDigest,
			)
		}
		expected, err := proxy.NewWiring(operation.Target)
		if err != nil {
			return fmt.Errorf("derive operation target wiring: %w", err)
		}
		expectedDigest, err := expected.Digest()
		if err != nil {
			return fmt.Errorf("digest operation target wiring: %w", err)
		}
		if expectedDigest != operation.TargetWiringDigest {
			return fmt.Errorf("operation target wiring does not match its resolved system")
		}
	}
	for endpointID, cleanup := range operation.EndpointCleanups {
		if endpointID == "" || endpointID != cleanup.EndpointID ||
			cleanup.ContainerID == "" ||
			cleanup.ContainerName == "" || cleanup.NetworkKey == "" ||
			cleanup.NetworkID == "" || cleanup.NetworkName == "" ||
			cleanup.EndpointName == "" ||
			cleanup.EndpointName != cleanup.ContainerName {
			return fmt.Errorf("operation state has invalid endpoint cleanup %q", endpointID)
		}
	}
	return nil
}

func validateRuntimeRoot(root string) error {
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return fmt.Errorf("runtime root is not an absolute clean path")
	}
	return nil
}

func (store Store) ClearOperation(name string) error {
	return store.remove(name, "operation.json")
}

func (store Store) read(name, filename string, output interface{}) (bool, error) {
	directory, err := store.directory(name)
	if err != nil {
		return false, err
	}
	file, err := os.Open(filepath.Join(directory, filename))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, documentSizeLimit+1))
	if err != nil {
		return false, fmt.Errorf("read %s: %w", filename, err)
	}
	if len(data) > documentSizeLimit {
		return false, fmt.Errorf(
			"decode %s: document exceeds %d bytes",
			filename,
			documentSizeLimit,
		)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return false, fmt.Errorf("decode %s: %w", filename, err)
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("decode %s: trailing data", filename)
	}
	return true, nil
}

func (store Store) write(name, filename string, value interface{}) (returnErr error) {
	directory, err := store.directory(name)
	if err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", filename, err)
	}
	encoded = append(encoded, '\n')
	if len(encoded) > documentSizeLimit {
		return fmt.Errorf(
			"encode %s: document exceeds %d bytes",
			filename,
			documentSizeLimit,
		)
	}
	if err := makeDirectoryDurable(directory, 0700); err != nil {
		return fmt.Errorf("create state directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, "."+filename+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary state: %w", err)
	}
	temporaryName := temporary.Name()
	defer func() {
		_ = temporary.Close()
		if returnErr != nil {
			_ = os.Remove(temporaryName)
		}
	}()
	if err := temporary.Chmod(hostfs.Mode(temporary.Name(), 0600)); err != nil {
		return err
	}
	if _, err := temporary.Write(encoded); err != nil {
		return fmt.Errorf("write %s: %w", filename, err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync %s: %w", filename, err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close %s: %w", filename, err)
	}
	destination := filepath.Join(directory, filename)
	if err := os.Rename(temporaryName, destination); err != nil {
		return fmt.Errorf("commit %s: %w", filename, err)
	}
	return syncDirectory(directory)
}

func (store Store) remove(name, filename string) error {
	directory, err := store.directory(name)
	if err != nil {
		return err
	}
	err = os.Remove(filepath.Join(directory, filename))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return syncDirectory(directory)
}

func (store Store) directory(name string) (string, error) {
	if !composition.ValidName(name) {
		return "", fmt.Errorf("invalid system name %q", name)
	}
	if err := store.validateRoot(); err != nil {
		return "", err
	}
	return filepath.Join(store.Root, "systems", name), nil
}

func (store Store) validateRoot() error {
	if !filepath.IsAbs(store.Root) {
		return fmt.Errorf("state root must be an absolute path")
	}
	return nil
}

func compareEngineBinding(bound, current string) error {
	if bound != current {
		return fmt.Errorf(
			"state root is bound to Docker engine %q, current engine is %q",
			bound,
			current,
		)
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync state directory: %w", err)
	}
	return nil
}

// makeDirectoryDurable is MkdirAll with the missing durability step: every
// parent directory is synced after its new child entry is created. This keeps a
// newly written operation reachable after sudden host power loss, not merely
// after process termination.
func makeDirectoryDurable(path string, mode os.FileMode) error {
	path = filepath.Clean(path)
	var missing []string
	for current := path; ; current = filepath.Dir(current) {
		info, err := os.Stat(current)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("%s exists and is not a directory", current)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		missing = append(missing, current)
		parent := filepath.Dir(current)
		if parent == current {
			return fmt.Errorf("no existing parent directory for %s", path)
		}
	}

	for index := len(missing) - 1; index >= 0; index-- {
		directory := missing[index]
		if err := hostfs.Mkdir(directory, mode); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		info, err := os.Stat(directory)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return fmt.Errorf("%s exists and is not a directory", directory)
		}
		if err := syncDirectory(filepath.Dir(directory)); err != nil {
			return fmt.Errorf("sync parent of %s: %w", directory, err)
		}
	}
	return nil
}
