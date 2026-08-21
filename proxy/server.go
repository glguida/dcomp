package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const maxControlMessage = 64 * 1024

var (
	ErrNotRunning       = errors.New("dcomp proxy is not running")
	ErrIdentityMismatch = errors.New("dcomp proxy identity mismatch")
)

type ControlRequest struct {
	Command    string `json:"command"`
	InstanceID string `json:"instance_id"`
}

type Status struct {
	Version           int           `json:"version"`
	System            string        `json:"system"`
	InstanceID        string        `json:"instance_id"`
	Digest            string        `json:"digest"`
	PID               int           `json:"pid"`
	Ready             bool          `json:"ready"`
	Inputs            int           `json:"inputs"`
	Outputs           int           `json:"outputs"`
	ActiveConnections int64         `json:"active_connections"`
	PendingInputs     int64         `json:"pending_inputs"`
	PendingOutputs    int64         `json:"pending_outputs"`
	Links             []LinkMetrics `json:"links"`
	Error             string        `json:"error,omitempty"`
}

// LinkMetrics is one cumulative, per-link data-plane snapshot. Connections
// is a gauge; byte counters increase for the lifetime of the proxy process.
// The direction names follow the component contract: input is the consuming
// side and output is the providing side.
type LinkMetrics struct {
	InputComponent     string `json:"input_component"`
	InputEndpoint      string `json:"input_endpoint"`
	OutputComponent    string `json:"output_component"`
	OutputEndpoint     string `json:"output_endpoint"`
	ActiveConnections  int64  `json:"active_connections"`
	BytesInputToOutput uint64 `json:"bytes_input_to_output"`
	BytesOutputToInput uint64 `json:"bytes_output_to_input"`
}

// Run owns all configured listeners until ctx is cancelled or a listener
// fails. ready is called after every endpoint and the control socket is live.
func Run(ctx context.Context, config Config, ready func(Status) error) error {
	if ctx == nil {
		return fmt.Errorf("proxy context is nil")
	}
	if err := config.Validate(); err != nil {
		return err
	}
	server := &server{
		config:      config,
		outputs:     make(map[string]chan net.Conn),
		linkMetrics: make(map[string]*linkCounters),
		connections: make(map[net.Conn]struct{}),
	}
	runCtx, cancel := context.WithCancel(ctx)
	server.cancel = cancel
	defer cancel()
	if err := server.prepare(); err != nil {
		server.cleanup()
		return err
	}
	defer server.cleanup()

	status := server.status()
	if ready != nil {
		if err := ready(status); err != nil {
			return fmt.Errorf("report proxy readiness: %w", err)
		}
	}
	log.Printf("proxy ready system=%s digest=%s inputs=%d outputs=%d", config.System, config.Digest, status.Inputs, status.Outputs)

	for _, output := range server.outputEndpoints() {
		server.wg.Add(1)
		go server.acceptOutputs(runCtx, output)
	}
	for _, input := range server.inputEndpoints() {
		server.wg.Add(1)
		go server.acceptInputs(runCtx, input)
	}
	server.wg.Add(1)
	go server.acceptControl(runCtx)

	select {
	case <-runCtx.Done():
	case err := <-server.failures:
		cancel()
		server.close()
		server.wg.Wait()
		server.connectionWG.Wait()
		return err
	}
	server.close()
	server.wg.Wait()
	server.connectionWG.Wait()
	return nil
}

type server struct {
	config Config
	cancel context.CancelFunc

	mu           sync.Mutex
	listeners    map[string]net.Listener
	control      net.Listener
	outputs      map[string]chan net.Conn
	routes       map[string]string
	linkMetrics  map[string]*linkCounters
	connections  map[net.Conn]struct{}
	ownedPaths   []string
	failures     chan error
	wg           sync.WaitGroup
	connectionWG sync.WaitGroup
	closed       bool

	active         atomic.Int64
	pendingInputs  atomic.Int64
	pendingOutputs atomic.Int64
}

type linkCounters struct {
	active             atomic.Int64
	bytesInputToOutput atomic.Uint64
	bytesOutputToInput atomic.Uint64
}

func (server *server) prepare() error {
	if err := os.MkdirAll(filepath.Join(server.config.RuntimeDir, "in"), 0700); err != nil {
		return fmt.Errorf("create proxy input directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(server.config.RuntimeDir, "out"), 0700); err != nil {
		return fmt.Errorf("create proxy output directory: %w", err)
	}
	if err := os.Chmod(server.config.RuntimeDir, 0700); err != nil {
		return fmt.Errorf("set proxy runtime permissions: %w", err)
	}
	server.listeners = make(map[string]net.Listener, len(server.config.Endpoints))
	server.routes = make(map[string]string, len(server.config.Links))
	server.failures = make(chan error, 1)

	// Claim the control socket before touching endpoint sockets. This is the
	// per-runtime-directory ownership boundary and prevents a second proxy from
	// unlinking the live data plane while trying to start.
	controlPath := controlSocketFile(server.config.RuntimeDir)
	control, err := listenUnixExclusive(controlPath, 0600)
	if err != nil {
		return fmt.Errorf("listen on proxy control socket: %w", err)
	}
	server.control = control
	server.ownSocket(controlPath)

	for _, endpoint := range server.config.Endpoints {
		listener, err := listenUnix(endpoint.Socket, 0666)
		if err != nil {
			return fmt.Errorf("listen on %s endpoint %s.%s: %w", endpoint.Direction, endpoint.Component, endpoint.Name, err)
		}
		key := endpointKey(endpoint.Direction, endpoint.Component, endpoint.Name)
		server.listeners[key] = listener
		server.ownSocket(endpoint.Socket)
		if endpoint.Direction == DirectionOutput {
			server.outputs[key] = make(chan net.Conn)
		}
	}
	for _, link := range server.config.Links {
		input := endpointKey(DirectionInput, link.InputComponent, link.InputEndpoint)
		output := endpointKey(DirectionOutput, link.OutputComponent, link.OutputEndpoint)
		server.routes[input] = output
		server.linkMetrics[input] = &linkCounters{}
	}
	pidPath := filepath.Join(server.config.RuntimeDir, PIDFileName)
	if err := writeAtomic(pidPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0600); err != nil {
		return err
	}
	server.ownedPaths = append(server.ownedPaths, pidPath)
	readyData, err := json.Marshal(server.status())
	if err != nil {
		return fmt.Errorf("encode proxy readiness: %w", err)
	}
	readyData = append(readyData, '\n')
	readyPath := filepath.Join(server.config.RuntimeDir, ReadyFileName)
	if err := writeAtomic(readyPath, readyData, 0600); err != nil {
		return err
	}
	server.ownedPaths = append(server.ownedPaths, readyPath)
	return nil
}

func listenUnix(path string, mode os.FileMode) (net.Listener, error) {
	return listenUnixWithPolicy(path, mode, false)
}

func listenUnixExclusive(path string, mode os.FileMode) (net.Listener, error) {
	return listenUnixWithPolicy(path, mode, true)
}

func listenUnixWithPolicy(path string, mode os.FileMode, refuseActive bool) (net.Listener, error) {
	listenPath := socketListenPath(path)
	if listenPath != path {
		shortRoot := filepath.Dir(listenPath)
		if err := os.MkdirAll(shortRoot, 0700); err != nil {
			return nil, err
		}
		info, err := os.Lstat(shortRoot)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("short proxy socket root %s is not a directory", shortRoot)
		}
		if err := os.Chmod(shortRoot, 0700); err != nil {
			return nil, err
		}
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
	if refuseActive && len(existing) != 0 {
		if _, exists := existing[listenPath]; !exists {
			return nil, fmt.Errorf("refusing to replace orphaned control socket %s", path)
		}
		connection, dialErr := net.DialTimeout("unix", listenPath, 250*time.Millisecond)
		if dialErr == nil {
			_ = connection.Close()
			return nil, fmt.Errorf("proxy control socket is already active at %s", listenPath)
		}
		if !errors.Is(dialErr, syscall.ECONNREFUSED) &&
			!errors.Is(dialErr, os.ErrNotExist) {
			return nil, fmt.Errorf("refusing to replace control socket %s: %w", listenPath, dialErr)
		}
	}
	for _, candidate := range candidates {
		if _, exists := existing[candidate]; !exists {
			continue
		}
		if err := os.Remove(candidate); err != nil {
			return nil, err
		}
	}
	listener, err := net.Listen("unix", listenPath)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(listenPath, mode); err != nil {
		_ = listener.Close()
		_ = os.Remove(listenPath)
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

func (server *server) ownSocket(path string) {
	server.ownedPaths = append(server.ownedPaths, path)
	if actual := socketListenPath(path); actual != path {
		server.ownedPaths = append(server.ownedPaths, actual)
	}
}

func (server *server) outputEndpoints() []Endpoint {
	var result []Endpoint
	for _, endpoint := range server.config.Endpoints {
		if endpoint.Direction == DirectionOutput {
			result = append(result, endpoint)
		}
	}
	return result
}

func (server *server) inputEndpoints() []Endpoint {
	var result []Endpoint
	for _, endpoint := range server.config.Endpoints {
		if endpoint.Direction == DirectionInput {
			result = append(result, endpoint)
		}
	}
	return result
}

func (server *server) acceptOutputs(ctx context.Context, endpoint Endpoint) {
	defer server.wg.Done()
	key := endpointKey(DirectionOutput, endpoint.Component, endpoint.Name)
	listener := server.listeners[key]
	pool := server.outputs[key]
	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() == nil {
				server.fail(fmt.Errorf("accept output %s.%s: %w", endpoint.Component, endpoint.Name, err))
			}
			return
		}
		server.track(connection)
		server.pendingOutputs.Add(1)
		select {
		case pool <- connection:
			server.pendingOutputs.Add(-1)
		case <-ctx.Done():
			server.pendingOutputs.Add(-1)
			server.untrackAndClose(connection)
			return
		}
	}
}

func (server *server) acceptInputs(ctx context.Context, endpoint Endpoint) {
	defer server.wg.Done()
	inputKey := endpointKey(DirectionInput, endpoint.Component, endpoint.Name)
	outputKey := server.routes[inputKey]
	pool := server.outputs[outputKey]
	listener := server.listeners[inputKey]
	for {
		connection, err := listener.Accept()
		if err != nil {
			if ctx.Err() == nil {
				server.fail(fmt.Errorf("accept input %s.%s: %w", endpoint.Component, endpoint.Name, err))
			}
			return
		}
		server.track(connection)
		server.pendingInputs.Add(1)
		server.connectionWG.Add(1)
		go func(consumer net.Conn) {
			defer server.connectionWG.Done()
			select {
			case producer := <-pool:
				server.pendingInputs.Add(-1)
				server.forward(ctx, inputKey, outputKey, consumer, producer)
			case <-ctx.Done():
				server.pendingInputs.Add(-1)
				server.untrackAndClose(consumer)
			}
		}(connection)
	}
}

func (server *server) forward(ctx context.Context, inputKey, outputKey string, consumer, producer net.Conn) {
	metrics := server.linkMetrics[inputKey]
	server.active.Add(1)
	defer server.active.Add(-1)
	if metrics != nil {
		metrics.active.Add(1)
		defer metrics.active.Add(-1)
	}
	defer server.untrackAndClose(consumer)
	defer server.untrackAndClose(producer)
	log.Printf("connection paired input=%s output=%s", inputKey, outputKey)

	type copyResult struct {
		direction string
		err       error
	}
	done := make(chan copyResult, 2)
	copyOneWay := func(
		direction string,
		destination,
		source net.Conn,
		counter *atomic.Uint64,
	) {
		writer := io.Writer(destination)
		if counter != nil {
			writer = &countingWriter{writer: destination, counter: counter}
		}
		_, copyErr := io.Copy(writer, source)
		if closer, ok := destination.(interface{ CloseWrite() error }); ok {
			_ = closer.CloseWrite()
		}
		done <- copyResult{direction: direction, err: copyErr}
	}
	var inputToOutput, outputToInput *atomic.Uint64
	if metrics != nil {
		inputToOutput = &metrics.bytesInputToOutput
		outputToInput = &metrics.bytesOutputToInput
	}
	go copyOneWay("output-to-input", consumer, producer, outputToInput)
	go copyOneWay("input-to-output", producer, consumer, inputToOutput)
	var results []copyResult
	select {
	case <-ctx.Done():
		_ = consumer.Close()
		_ = producer.Close()
		results = append(results, <-done, <-done)
	case first := <-done:
		results = append(results, first)
		// A copy error means one stream direction is unusable, so force the
		// other direction to wake. A clean EOF retains Unix half-close
		// semantics and lets the peer finish sending a response.
		if first.err != nil {
			_ = consumer.Close()
			_ = producer.Close()
		}
		select {
		case second := <-done:
			results = append(results, second)
		case <-ctx.Done():
			_ = consumer.Close()
			_ = producer.Close()
			results = append(results, <-done)
		}
	}
	for _, result := range results {
		if result.err == nil || (ctx.Err() != nil && errors.Is(result.err, net.ErrClosed)) {
			continue
		}
		log.Printf(
			"connection copy failed input=%s output=%s direction=%s error=%v",
			inputKey, outputKey, result.direction, result.err,
		)
	}
	log.Printf("connection closed input=%s output=%s", inputKey, outputKey)
}

type countingWriter struct {
	writer  io.Writer
	counter *atomic.Uint64
}

func (writer *countingWriter) Write(buffer []byte) (int, error) {
	written, err := writer.writer.Write(buffer)
	writer.counter.Add(uint64(written))
	return written, err
}

func (server *server) acceptControl(ctx context.Context) {
	defer server.wg.Done()
	for {
		connection, err := server.control.Accept()
		if err != nil {
			if ctx.Err() == nil {
				server.fail(fmt.Errorf("accept control connection: %w", err))
			}
			return
		}
		server.connectionWG.Add(1)
		go func() {
			defer server.connectionWG.Done()
			defer connection.Close()
			_ = connection.SetDeadline(time.Now().Add(5 * time.Second))
			decoder := json.NewDecoder(io.LimitReader(connection, maxControlMessage))
			decoder.DisallowUnknownFields()
			var request ControlRequest
			if err := decoder.Decode(&request); err != nil {
				_ = json.NewEncoder(connection).Encode(Status{Error: "invalid control request"})
				return
			}
			status := server.status()
			if request.InstanceID != server.config.InstanceID {
				status.Error = "proxy instance ID mismatch"
				_ = json.NewEncoder(connection).Encode(status)
				return
			}
			switch request.Command {
			case "status":
				_ = json.NewEncoder(connection).Encode(status)
			case "shutdown":
				_ = json.NewEncoder(connection).Encode(status)
				go server.cancel()
			default:
				status.Error = "unknown control command"
				_ = json.NewEncoder(connection).Encode(status)
			}
		}()
	}
}

func (server *server) status() Status {
	inputs, outputs := 0, 0
	for _, endpoint := range server.config.Endpoints {
		if endpoint.Direction == DirectionInput {
			inputs++
		} else {
			outputs++
		}
	}
	links := make([]LinkMetrics, 0, len(server.config.Links))
	for _, link := range server.config.Links {
		key := endpointKey(DirectionInput, link.InputComponent, link.InputEndpoint)
		metrics := server.linkMetrics[key]
		item := LinkMetrics{
			InputComponent: link.InputComponent, InputEndpoint: link.InputEndpoint,
			OutputComponent: link.OutputComponent, OutputEndpoint: link.OutputEndpoint,
		}
		if metrics != nil {
			item.ActiveConnections = metrics.active.Load()
			item.BytesInputToOutput = metrics.bytesInputToOutput.Load()
			item.BytesOutputToInput = metrics.bytesOutputToInput.Load()
		}
		links = append(links, item)
	}
	return Status{
		Version: ConfigVersion, System: server.config.System,
		InstanceID: server.config.InstanceID, Digest: server.config.Digest,
		PID: os.Getpid(), Ready: true, Inputs: inputs, Outputs: outputs,
		ActiveConnections: server.active.Load(),
		PendingInputs:     server.pendingInputs.Load(), PendingOutputs: server.pendingOutputs.Load(),
		Links: links,
	}
}

func (server *server) fail(err error) {
	select {
	case server.failures <- err:
	default:
	}
}

func (server *server) track(connection net.Conn) {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.closed {
		_ = connection.Close()
		return
	}
	server.connections[connection] = struct{}{}
}

func (server *server) untrackAndClose(connection net.Conn) {
	server.mu.Lock()
	delete(server.connections, connection)
	server.mu.Unlock()
	_ = connection.Close()
}

func (server *server) close() {
	server.mu.Lock()
	if server.closed {
		server.mu.Unlock()
		return
	}
	server.closed = true
	var listeners []net.Listener
	for _, listener := range server.listeners {
		listeners = append(listeners, listener)
	}
	if server.control != nil {
		listeners = append(listeners, server.control)
	}
	var connections []net.Conn
	for connection := range server.connections {
		connections = append(connections, connection)
	}
	server.mu.Unlock()
	for _, listener := range listeners {
		_ = listener.Close()
	}
	for _, connection := range connections {
		_ = connection.Close()
	}
}

func (server *server) cleanup() {
	server.close()
	paths := append([]string(nil), server.ownedPaths...)
	sort.Strings(paths)
	for index, path := range paths {
		if index != 0 && path == paths[index-1] {
			continue
		}
		_ = os.Remove(path)
	}
	log.Printf("proxy stopped system=%s", server.config.System)
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	file, err := os.CreateTemp(directory, ".proxy-tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary proxy file: %w", err)
	}
	name := file.Name()
	defer os.Remove(name)
	if err := file.Chmod(mode); err != nil {
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
