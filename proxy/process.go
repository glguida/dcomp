package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/glguida/dcomp/hostfs"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Process is the durable identity of one running per-system proxy. Lifecycle
// code records it alongside Docker resource identities.
type Process struct {
	InstanceID string `json:"instance_id"`
	Digest     string `json:"digest"`
	PID        int    `json:"pid"`
	RuntimeDir string `json:"runtime_dir"`
	Control    string `json:"control"`
	Log        string `json:"log"`
}

// Validate checks a durable proxy record before lifecycle code trusts its PID
// or host paths.
func (process Process) Validate() error {
	return validateProcess(process, true)
}

type LogLine struct {
	Timestamp time.Time
	Message   string
}

// Manager is the lifecycle boundary for a per-system proxy. The concrete
// ProcessManager launches the dcomp-proxy binary; tests can supply an
// in-memory implementation.
type Manager interface {
	Ensure(context.Context, Config) (Process, error)
	Inspect(context.Context, Process) (Status, error)
	Resync(context.Context, Process, Wiring, string) (Status, error)
	Stop(context.Context, Process) error
	Logs(context.Context, Process, bool, func(LogLine) error) error
}

type ProcessManager struct {
	Binary       string
	StartTimeout time.Duration
	StopTimeout  time.Duration
}

const (
	proxyLogTimestampLayout      = "2006/01/02 15:04:05.000000"
	startedProcessTerminateGrace = 2 * time.Second
)

func (manager *ProcessManager) Ensure(ctx context.Context, config Config) (Process, error) {
	if err := config.Validate(); err != nil {
		return Process{}, err
	}
	expected := processForConfig(config, 0)
	if status, err := manager.Inspect(ctx, expected); err == nil {
		if !status.Ready || status.Digest != config.Digest {
			return Process{}, fmt.Errorf(
				"proxy instance %s is live with wiring %q, expected %q",
				config.InstanceID, status.Digest, config.Digest,
			)
		}
		return processForConfig(config, status.PID), nil
	} else if !errors.Is(err, ErrNotRunning) && !errors.Is(err, ErrIdentityMismatch) {
		return Process{}, fmt.Errorf("inspect existing proxy: %w", err)
	}
	existingPath := filepath.Join(config.RuntimeDir, ConfigFileName)
	var staleProcess *Process
	if existingData, readErr := os.ReadFile(existingPath); readErr == nil {
		existing, loadErr := LoadConfig(existingData)
		if loadErr != nil {
			return Process{}, fmt.Errorf("load existing proxy config: %w", loadErr)
		}
		if existing.RuntimeDir != config.RuntimeDir || existing.System != config.System {
			return Process{}, fmt.Errorf("existing proxy config does not belong to %s", config.RuntimeDir)
		}
		pid := 0
		pidData, pidErr := os.ReadFile(filepath.Join(config.RuntimeDir, PIDFileName))
		if pidErr == nil {
			var parseErr error
			pid, parseErr = ParsePIDFile(pidData)
			if parseErr != nil {
				return Process{}, parseErr
			}
			existingProcess := processForConfig(existing, pid)
			if _, inspectErr := manager.Inspect(ctx, existingProcess); inspectErr == nil {
				return Process{}, fmt.Errorf(
					"runtime directory %s already contains proxy instance %s",
					config.RuntimeDir, existing.InstanceID,
				)
			} else if !errors.Is(inspectErr, ErrNotRunning) {
				return Process{}, fmt.Errorf("inspect existing proxy identity: %w", inspectErr)
			}
		} else if !errors.Is(pidErr, os.ErrNotExist) {
			return Process{}, fmt.Errorf("read existing proxy PID: %w", pidErr)
		}
		process := processForConfig(existing, pid)
		staleProcess = &process
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return Process{}, fmt.Errorf("read existing proxy config: %w", readErr)
	}
	if connection, dialErr := (&net.Dialer{Timeout: 250 * time.Millisecond}).DialContext(
		ctx, "unix", ControlSocket(config.RuntimeDir),
	); dialErr == nil {
		_ = connection.Close()
		return Process{}, fmt.Errorf("proxy control socket is already active at %s", ControlSocket(config.RuntimeDir))
	} else if !errors.Is(dialErr, os.ErrNotExist) && !errors.Is(dialErr, syscall.ECONNREFUSED) {
		return Process{}, fmt.Errorf("inspect proxy control socket: %w", dialErr)
	}
	if staleProcess != nil {
		finished, cleanupErr := finishStoppedProxyCleanup(*staleProcess, true)
		if cleanupErr != nil {
			return Process{}, fmt.Errorf("reclaim stale proxy runtime: %w", cleanupErr)
		}
		if !finished {
			return Process{}, fmt.Errorf(
				"proxy control is unavailable while recorded PID %d is still running; refusing runtime cleanup",
				staleProcess.PID,
			)
		}
	}

	if err := hostfs.MkdirAll(config.RuntimeDir, 0700); err != nil {
		return Process{}, fmt.Errorf("create proxy runtime directory: %w", err)
	}
	encoded, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return Process{}, fmt.Errorf("encode proxy config: %w", err)
	}
	encoded = append(encoded, '\n')
	configPath := filepath.Join(config.RuntimeDir, ConfigFileName)
	if err := writeAtomic(configPath, encoded, 0600); err != nil {
		return Process{}, fmt.Errorf("write proxy config: %w", err)
	}
	logPath := filepath.Join(config.RuntimeDir, LogFileName)
	logFile, err := hostfs.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return Process{}, fmt.Errorf("open proxy log: %w", err)
	}
	defer logFile.Close()

	binary, err := manager.binaryPath()
	if err != nil {
		return Process{}, err
	}
	readyReader, readyWriter, err := os.Pipe()
	if err != nil {
		return Process{}, fmt.Errorf("create proxy readiness pipe: %w", err)
	}
	defer readyReader.Close()
	command := exec.Command(binary, "--config", configPath, "--ready-fd", "3")
	command.Stdin = nil
	command.Stdout = logFile
	command.Stderr = logFile
	command.ExtraFiles = []*os.File{readyWriter}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		_ = readyWriter.Close()
		return Process{}, fmt.Errorf("start dcomp-proxy: %w", err)
	}
	_ = readyWriter.Close()
	process := processForConfig(config, command.Process.Pid)

	timeout := manager.StartTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	readyCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		var marker [1]byte
		_, readErr := io.ReadFull(readyReader, marker[:])
		if readErr == nil && marker[0] != 1 {
			readErr = fmt.Errorf("invalid readiness marker %d", marker[0])
		}
		result <- readErr
	}()
	select {
	case err := <-result:
		if err != nil {
			return Process{}, abortStartedCommand(
				command,
				fmt.Errorf("dcomp-proxy exited before readiness: %w", err),
			)
		}
	case <-readyCtx.Done():
		return Process{}, abortStartedCommand(
			command,
			fmt.Errorf("wait for dcomp-proxy readiness: %w", readyCtx.Err()),
		)
	}
	status, err := manager.Inspect(ctx, process)
	if err != nil {
		return Process{}, abortStartedCommand(
			command,
			fmt.Errorf("verify started dcomp-proxy: %w", err),
		)
	}
	if status.PID != process.PID {
		return Process{}, abortStartedCommand(
			command,
			fmt.Errorf("started dcomp-proxy reported PID %d, expected %d", status.PID, process.PID),
		)
	}
	if !status.Ready || status.Digest != config.Digest {
		return Process{}, abortStartedCommand(
			command,
			fmt.Errorf(
				"started dcomp-proxy reported ready=%t digest=%q, expected %q",
				status.Ready, status.Digest, config.Digest,
			),
		)
	}
	if err := command.Process.Release(); err != nil {
		return Process{}, abortStartedCommand(
			command,
			fmt.Errorf("release dcomp-proxy process handle: %w", err),
		)
	}
	return process, nil
}

func abortStartedCommand(command *exec.Cmd, cause error) error {
	if err := terminateStartedCommand(command, startedProcessTerminateGrace); err != nil {
		return errors.Join(cause, fmt.Errorf("terminate failed dcomp-proxy: %w", err))
	}
	return cause
}

// terminateStartedCommand reaps a child while bounding both graceful and
// forced termination. Wait runs concurrently because a process may have
// already exited before the readiness failure is observed.
func terminateStartedCommand(command *exec.Cmd, grace time.Duration) error {
	if command == nil || command.Process == nil {
		return fmt.Errorf("started command has no process")
	}
	if grace <= 0 {
		grace = startedProcessTerminateGrace
	}
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()

	signalErr := command.Process.Signal(syscall.SIGTERM)
	timer := time.NewTimer(grace)
	select {
	case waitErr := <-waited:
		timer.Stop()
		if signalErr != nil && !errors.Is(signalErr, os.ErrProcessDone) {
			return signalErr
		}
		return expectedTerminationWait(waitErr)
	case <-timer.C:
	}

	if err := command.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("kill unresponsive process: %w", err)
	}
	timer.Reset(grace)
	select {
	case waitErr := <-waited:
		timer.Stop()
		return expectedTerminationWait(waitErr)
	case <-timer.C:
		return fmt.Errorf("process did not exit after SIGKILL within %s", grace)
	}
}

func expectedTerminationWait(err error) error {
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return nil
	}
	return err
}

func (manager *ProcessManager) Inspect(ctx context.Context, process Process) (Status, error) {
	if err := validateProcess(process, false); err != nil {
		return Status{}, err
	}
	dialer := net.Dialer{Timeout: 2 * time.Second}
	connection, err := dialer.DialContext(ctx, "unix", process.Control)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
			return Status{}, ErrNotRunning
		}
		return Status{}, err
	}
	defer connection.Close()
	deadline := time.Now().Add(3 * time.Second)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	_ = connection.SetDeadline(deadline)
	if err := writeControlMessage(connection, ControlRequest{
		Command: "status", InstanceID: process.InstanceID,
		ProtocolVersion: ControlProtocolVersion,
	}); err != nil {
		return Status{}, err
	}
	var status Status
	if err := readControlMessage(connection, &status); err != nil {
		return Status{}, err
	}
	if err := verifyStatusIdentity(process, status); err != nil {
		return Status{}, err
	}
	if status.Error != "" {
		return Status{}, fmt.Errorf("proxy control error: %s", status.Error)
	}
	return status, nil
}

// Resync asks one identity-verified proxy process using the current control
// protocol to converge to a complete target Wiring.
func (manager *ProcessManager) Resync(
	ctx context.Context,
	process Process,
	wiring Wiring,
	digest string,
) (Status, error) {
	if err := validateProcess(process, false); err != nil {
		return Status{}, err
	}
	computed, err := wiring.Digest()
	if err != nil {
		return Status{}, err
	}
	if digest != computed {
		return Status{}, fmt.Errorf(
			"target wiring digest mismatch: expected %s, found %s",
			computed, digest,
		)
	}
	_, err = manager.Inspect(ctx, process)
	if err != nil {
		return Status{}, err
	}
	connection, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(
		ctx, "unix", process.Control,
	)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
			return Status{}, ErrNotRunning
		}
		return Status{}, err
	}
	defer connection.Close()
	deadline := time.Now().Add(controlRequestTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	_ = connection.SetDeadline(deadline)
	if err := writeControlMessage(connection, ControlRequest{
		Command: "resync", InstanceID: process.InstanceID,
		ProtocolVersion: ControlProtocolVersion,
		Wiring:          &wiring,
		Digest:          digest,
	}); err != nil {
		return Status{}, err
	}
	var response Status
	if err := readControlMessage(connection, &response); err != nil {
		return Status{}, err
	}
	if err := verifyStatusIdentity(process, response); err != nil {
		return response, err
	}
	if response.Error != "" {
		return response, fmt.Errorf("proxy resync failed: %s", response.Error)
	}
	return response, nil
}

func (manager *ProcessManager) Stop(ctx context.Context, process Process) error {
	if err := validateProcess(process, true); err != nil {
		return err
	}
	status, err := manager.Inspect(ctx, process)
	if errors.Is(err, ErrNotRunning) {
		markerReliable, markerErr := cleanupCompletionMarkerSupported(process)
		if markerErr != nil {
			return markerErr
		}
		finished, cleanupErr := finishStoppedProxyCleanup(process, markerReliable)
		if cleanupErr != nil {
			return cleanupErr
		}
		if finished {
			return nil
		}
		return fmt.Errorf(
			"proxy control is unavailable while recorded PID %d is still running; refusing runtime cleanup",
			process.PID,
		)
	}
	if err != nil {
		return err
	}
	connection, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "unix", process.Control)
	if err != nil {
		return err
	}
	_ = connection.SetDeadline(time.Now().Add(3 * time.Second))
	if err := writeControlMessage(connection, ControlRequest{
		Command: "shutdown", InstanceID: process.InstanceID,
		ProtocolVersion: ControlProtocolVersion,
	}); err != nil {
		_ = connection.Close()
		return err
	}
	var response Status
	err = readControlMessage(connection, &response)
	_ = connection.Close()
	if err != nil {
		return err
	}
	if err := verifyStatusIdentity(process, response); err != nil {
		return err
	}
	if response.Error != "" {
		return fmt.Errorf("proxy control error: %s", response.Error)
	}

	timeout := manager.StopTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	stopCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, inspectErr := manager.Inspect(stopCtx, process)
		if errors.Is(inspectErr, ErrNotRunning) {
			finished, cleanupErr := finishStoppedProxyCleanup(process, true)
			if cleanupErr != nil {
				return cleanupErr
			}
			if finished {
				return nil
			}
		}
		if errors.Is(inspectErr, ErrIdentityMismatch) && stopCtx.Err() == nil {
			return inspectErr
		}
		// Closing a Unix listener can reset an already accepted status request.
		// After an identity-checked shutdown response this is a transient exit
		// state, not evidence that the recorded process changed. Keep polling
		// until the socket is gone or the bounded stop context expires.
		select {
		case <-stopCtx.Done():
			finished, cleanupErr := finishStoppedProxyCleanup(process, true)
			if cleanupErr != nil {
				return cleanupErr
			}
			if finished {
				return nil
			}
			// The instance was verified immediately before shutdown. Signal only
			// that exact PID as a bounded fallback, then leave state for resume.
			if status.PID == process.PID {
				if target, findErr := os.FindProcess(process.PID); findErr == nil {
					_ = target.Signal(syscall.SIGTERM)
				}
			}
			return fmt.Errorf("wait for dcomp-proxy shutdown: %w", stopCtx.Err())
		case <-ticker.C:
		}
	}
}

func cleanupCompletionMarkerSupported(process Process) (bool, error) {
	data, err := os.ReadFile(filepath.Join(process.RuntimeDir, ConfigFileName))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	config, err := LoadConfig(data)
	if err != nil {
		return false, err
	}
	if config.InstanceID != process.InstanceID || config.RuntimeDir != process.RuntimeDir {
		return false, fmt.Errorf("%w: runtime config does not match recorded process", ErrIdentityMismatch)
	}
	return true, nil
}

// finishStoppedProxyCleanup distinguishes an unavailable control socket from
// completed process cleanup. Current proxies remove their PID marker only
// after every owned socket path. Without a current config proving that marker
// contract, the recorded process must exit before cleanup.
func finishStoppedProxyCleanup(process Process, markerReliable bool) (bool, error) {
	pidPath := filepath.Join(process.RuntimeDir, PIDFileName)
	_, markerErr := os.Lstat(pidPath)
	if markerErr != nil && !errors.Is(markerErr, os.ErrNotExist) {
		return false, fmt.Errorf("inspect proxy cleanup marker: %w", markerErr)
	}
	if errors.Is(markerErr, os.ErrNotExist) && markerReliable {
		if err := cleanupCompletedRuntime(process); err != nil {
			return false, err
		}
		return true, nil
	}
	if process.PID <= 0 {
		return false, fmt.Errorf(
			"proxy cleanup cannot be verified because its recorded PID is unavailable",
		)
	}
	killErr := syscall.Kill(process.PID, 0)
	if killErr == nil || errors.Is(killErr, syscall.EPERM) {
		return false, nil
	}
	if !errors.Is(killErr, syscall.ESRCH) {
		return false, fmt.Errorf("inspect recorded proxy PID %d: %w", process.PID, killErr)
	}
	if err := cleanupCrashedRuntime(process); err != nil {
		return false, err
	}
	return true, nil
}

func (manager *ProcessManager) Logs(
	ctx context.Context,
	process Process,
	follow bool,
	emit func(LogLine) error,
) error {
	if emit == nil {
		return fmt.Errorf("proxy log receiver is nil")
	}
	if err := validateProcess(process, true); err != nil {
		return err
	}
	file, err := os.Open(process.Log)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	reader := bufio.NewReaderSize(file, 64*1024)
	for {
		line, readErr := reader.ReadString('\n')
		if len(line) != 0 {
			if len(line) > 1024*1024 {
				return fmt.Errorf("proxy log record exceeds one MiB")
			}
			record := parseProxyLogLine(
				strings.TrimSuffix(line, "\n"),
				time.Now().UTC(),
			)
			if err := emit(record); err != nil {
				return err
			}
		}
		if readErr == nil {
			continue
		}
		if !errors.Is(readErr, io.EOF) {
			return readErr
		}
		if !follow {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func parseProxyLogLine(line string, fallback time.Time) LogLine {
	prefixLength := len(proxyLogTimestampLayout)
	if len(line) > prefixLength && line[prefixLength] == ' ' {
		if timestamp, err := time.ParseInLocation(
			proxyLogTimestampLayout,
			line[:prefixLength],
			time.UTC,
		); err == nil {
			return LogLine{Timestamp: timestamp, Message: line[prefixLength+1:]}
		}
	}
	return LogLine{Timestamp: fallback, Message: line}
}

func processForConfig(config Config, pid int) Process {
	return Process{
		InstanceID: config.InstanceID, Digest: config.Digest, PID: pid,
		RuntimeDir: config.RuntimeDir,
		Control:    ControlSocket(config.RuntimeDir),
		Log:        filepath.Join(config.RuntimeDir, LogFileName),
	}
}

func validateProcess(process Process, requirePID bool) error {
	if process.InstanceID == "" {
		return fmt.Errorf("proxy process identity is incomplete")
	}
	if requirePID && process.PID <= 0 {
		return fmt.Errorf("proxy PID is invalid")
	}
	if !filepath.IsAbs(process.RuntimeDir) || filepath.Clean(process.RuntimeDir) != process.RuntimeDir {
		return fmt.Errorf("proxy runtime directory is invalid")
	}
	if process.Control != ControlSocket(process.RuntimeDir) ||
		process.Log != filepath.Join(process.RuntimeDir, LogFileName) {
		return fmt.Errorf("proxy process paths do not match its runtime directory")
	}
	return nil
}

func verifyStatusIdentity(process Process, status Status) error {
	if status.Error != "" {
		if strings.Contains(status.Error, "instance ID mismatch") {
			return fmt.Errorf("%w: %s", ErrIdentityMismatch, status.Error)
		}
	}
	if status.Version != ConfigVersion {
		return fmt.Errorf("unsupported proxy status version %d", status.Version)
	}
	if status.InstanceID != process.InstanceID {
		return fmt.Errorf("%w: status does not match its recorded identity", ErrIdentityMismatch)
	}
	if process.PID > 0 && status.PID != process.PID {
		return fmt.Errorf(
			"%w: proxy PID changed: expected %d, found %d",
			ErrIdentityMismatch, process.PID, status.PID,
		)
	}
	if status.ControlProtocolVersion != ControlProtocolVersion {
		return fmt.Errorf(
			"%w: expected %d, found %d",
			ErrControlProtocolMismatch,
			ControlProtocolVersion,
			status.ControlProtocolVersion,
		)
	}
	return nil
}

func (manager *ProcessManager) binaryPath() (string, error) {
	if manager.Binary != "" {
		if !filepath.IsAbs(manager.Binary) {
			return "", fmt.Errorf("dcomp-proxy binary path must be absolute")
		}
		return manager.Binary, nil
	}
	if configured := os.Getenv("DCOMP_PROXY_BINARY"); configured != "" {
		if !filepath.IsAbs(configured) {
			return "", fmt.Errorf("DCOMP_PROXY_BINARY must be an absolute path")
		}
		return configured, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locate dcomp executable: %w", err)
	}
	candidate := filepath.Join(filepath.Dir(executable), "dcomp-proxy")
	if info, err := os.Stat(candidate); err != nil || info.IsDir() || info.Mode()&0111 == 0 {
		return "", fmt.Errorf("dcomp-proxy is not installed beside dcomp at %s", candidate)
	}
	return candidate, nil
}

func cleanupCrashedRuntime(process Process) error {
	if err := validateProcess(process, false); err != nil {
		return err
	}
	configPath := filepath.Join(process.RuntimeDir, ConfigFileName)
	if data, err := os.ReadFile(configPath); err == nil {
		loaded, loadErr := LoadConfig(data)
		if loadErr != nil {
			return loadErr
		}
		if loaded.InstanceID != process.InstanceID || loaded.RuntimeDir != process.RuntimeDir {
			return fmt.Errorf("%w: runtime config does not match recorded process", ErrIdentityMismatch)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	// Load and validate the complete ownership ledger before removing anything.
	// Config describes wiring and is never treated as pathname ownership.
	ownershipPath := filepath.Join(process.RuntimeDir, socketOwnershipFileName)
	var ownership *socketOwnershipLedger
	if _, err := os.Lstat(ownershipPath); err == nil {
		loaded, loadErr := loadSocketOwnershipLedger(
			ownershipPath, process.RuntimeDir, process.InstanceID,
		)
		if loadErr != nil {
			return fmt.Errorf("load socket ownership ledger: %w", loadErr)
		}
		ownership = loaded
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	shortRoots := ownership.ShortRoots()
	var temporaryFiles []string
	entries, err := os.ReadDir(process.RuntimeDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(process.RuntimeDir, name)
		if strings.HasPrefix(name, ".proxy-tmp-") {
			if entry.IsDir() {
				return fmt.Errorf("refusing to remove unexpected proxy temporary directory %s", path)
			}
			temporaryFiles = append(temporaryFiles, path)
		}
	}

	// Only ledger claims are socket-cleanup candidates. Other endpoint entries
	// are foreign regardless of file type and are preserved; the separately
	// reserved .proxy-tmp-* namespace contains interrupted atomic metadata
	// writes. The ledger remains present on partial socket cleanup, and
	// proxy.json/proxy.pid remain until every ownership claim has been resolved.
	if ownership != nil {
		if err := ownership.CleanupAll(); err != nil {
			return err
		}
		if err := ownership.Close(); err != nil {
			return err
		}
	}
	for _, path := range temporaryFiles {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Remove(filepath.Join(process.RuntimeDir, ReadyFileName)); err != nil &&
		!errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, directory := range []string{"in", "out"} {
		path := filepath.Join(process.RuntimeDir, directory)
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			if !errors.Is(err, syscall.ENOTEMPTY) {
				return err
			}
		}
	}
	if err := os.Remove(process.Log); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, root := range shortRoots {
		if err := os.Remove(root); err != nil &&
			!errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) {
			return err
		}
	}
	// PID is removed before config: its absence declares socket cleanup
	// complete, while the still-present config lets a retry validate identity.
	if err := os.Remove(filepath.Join(process.RuntimeDir, PIDFileName)); err != nil &&
		!errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(configPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(process.RuntimeDir); err != nil &&
		!errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) {
		return err
	}
	return nil
}

// cleanupCompletedRuntime runs only after a current proxy has removed its PID
// completion marker. It removes manager-owned metadata and empty directories,
// but deliberately performs no socket cleanup: every socket pathname has
// already been removed or relinquished by the process that held the ledger.
func cleanupCompletedRuntime(process Process) error {
	if err := validateProcess(process, false); err != nil {
		return err
	}
	configPath := filepath.Join(process.RuntimeDir, ConfigFileName)
	if data, err := os.ReadFile(configPath); err == nil {
		config, loadErr := LoadConfig(data)
		if loadErr != nil {
			return loadErr
		}
		if config.InstanceID != process.InstanceID || config.RuntimeDir != process.RuntimeDir {
			return fmt.Errorf("%w: runtime config does not match recorded process", ErrIdentityMismatch)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ownershipPath := filepath.Join(process.RuntimeDir, socketOwnershipFileName)
	if _, err := os.Lstat(ownershipPath); err == nil {
		ownership, loadErr := loadSocketOwnershipLedger(
			ownershipPath, process.RuntimeDir, process.InstanceID,
		)
		if loadErr != nil {
			return fmt.Errorf("load socket ownership ledger after completed cleanup: %w", loadErr)
		}
		if len(ownership.records) != 0 {
			return fmt.Errorf(
				"proxy PID marker is absent but socket ownership ledger has %d claim(s)",
				len(ownership.records),
			)
		}
		if err := ownership.Close(); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(process.Log); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, directory := range []string{"in", "out"} {
		if err := os.Remove(filepath.Join(process.RuntimeDir, directory)); err != nil &&
			!errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) {
			return err
		}
	}
	if err := os.Remove(configPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(process.RuntimeDir); err != nil &&
		!errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTEMPTY) {
		return err
	}
	return nil
}

// ParsePIDFile is used by diagnostics and tests; process control never trusts
// a PID file without also verifying the control-socket identity.
func ParsePIDFile(data []byte) (int, error) {
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("invalid proxy PID file")
	}
	return pid, nil
}
