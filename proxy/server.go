package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/glguida/dcomp/hostfs"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/glguida/dcomp/internal/runtimecontract"
)

const (
	maxControlMessage      = 16 * 1024 * 1024
	ControlProtocolVersion = 2
	controlRequestTimeout  = 30 * time.Second
)

var (
	ErrNotRunning              = errors.New("dcomp proxy is not running")
	ErrIdentityMismatch        = errors.New("dcomp proxy identity mismatch")
	ErrControlProtocolMismatch = errors.New("dcomp proxy control protocol mismatch")
)

type ControlRequest struct {
	Command         string    `json:"command"`
	InstanceID      string    `json:"instance_id"`
	ProtocolVersion int       `json:"protocol_version"`
	Wiring          *Wiring   `json:"wiring,omitempty"`
	Digest          string    `json:"digest,omitempty"`
	Wire            *WireEdit `json:"wire,omitempty"`
	Global          *Global   `json:"global,omitempty"`
	ExpectedDigest  string    `json:"expected_digest,omitempty"`
}

type Status struct {
	Globals                []Global      `json:"globals,omitempty"`
	Version                int           `json:"version"`
	ControlProtocolVersion int           `json:"control_protocol_version"`
	System                 string        `json:"system"`
	InstanceID             string        `json:"instance_id"`
	Digest                 string        `json:"digest,omitempty"`
	PID                    int           `json:"pid"`
	Ready                  bool          `json:"ready"`
	Inputs                 int           `json:"inputs"`
	Outputs                int           `json:"outputs"`
	ActiveConnections      int64         `json:"active_connections"`
	PendingInputs          int64         `json:"pending_inputs"`
	PendingOutputs         int64         `json:"pending_outputs"`
	Links                  []LinkMetrics `json:"links"`
	Error                  string        `json:"error,omitempty"`
}

// LinkMetrics is one cumulative, per-link data-plane snapshot. Connections
// is a gauge; byte counters increase until that exact link identity is
// removed. Recreating a removed link starts new counters at zero.
type LinkMetrics struct {
	InputComponent     string `json:"input_component"`
	InputEndpoint      string `json:"input_endpoint"`
	OutputComponent    string `json:"output_component"`
	OutputEndpoint     string `json:"output_endpoint"`
	ActiveConnections  int64  `json:"active_connections"`
	BytesInputToOutput uint64 `json:"bytes_input_to_output"`
	BytesOutputToInput uint64 `json:"bytes_output_to_input"`
}

// Run owns the control listener for the process lifetime. Endpoint listeners,
// routes, and link registries are mutable through identity-checked resyncs.
func Run(ctx context.Context, config Config, ready func(Status) error) error {
	return run(ctx, config, ready, nil)
}

func run(
	ctx context.Context,
	config Config,
	ready func(Status) error,
	faults resyncFaultInjector,
) error {
	if ctx == nil {
		return fmt.Errorf("proxy context is nil")
	}
	if err := config.Validate(); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(ctx)
	server := &server{
		system:         config.System,
		instanceID:     config.InstanceID,
		runtimeDir:     config.RuntimeDir,
		config:         config,
		wiring:         config.Wiring(),
		digest:         config.Digest,
		ready:          true,
		runCtx:         runCtx,
		cancel:         cancel,
		endpoints:      make(map[string]*endpointRuntime),
		routes:         make(map[string]Link),
		linkMetrics:    make(map[string]*linkCounters),
		pendingInputs:  make(map[string][]*pendingConnection),
		pendingOutputs: make(map[string][]*pendingConnection),
		pairs:          make(map[string]map[*streamPair]struct{}),
		connections:    make(map[net.Conn]struct{}),
		failures:       make(chan error, 1),
		faults:         faults,
	}
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
	log.Printf(
		"proxy ready system=%s digest=%s inputs=%d outputs=%d",
		config.System, config.Digest, status.Inputs, status.Outputs,
	)

	server.startInitialEndpoints()
	server.wg.Add(1)
	go server.acceptControl()

	var runErr error
	select {
	case <-runCtx.Done():
	case runErr = <-server.failures:
		cancel()
	}
	server.controlMu.Lock()
	server.close()
	server.controlMu.Unlock()
	server.wg.Wait()
	server.connectionWG.Wait()
	return runErr
}

type server struct {
	system     string
	instanceID string
	runtimeDir string
	runCtx     context.Context
	cancel     context.CancelFunc

	// controlMu serializes resync and shutdown. Status remains observable while
	// either operation is running and reads state through mu.
	controlMu sync.Mutex
	mu        sync.Mutex

	config     Config
	wiring     Wiring
	digest     string
	ready      bool
	transition *resyncTransition

	control        net.Listener
	endpoints      map[string]*endpointRuntime
	routes         map[string]Link
	linkMetrics    map[string]*linkCounters
	pendingInputs  map[string][]*pendingConnection
	pendingOutputs map[string][]*pendingConnection
	pairs          map[string]map[*streamPair]struct{}
	connections    map[net.Conn]struct{}
	ownership      *socketOwnershipLedger
	pidFile        *ownedFile
	readyFile      *ownedFile
	failures       chan error
	faults         resyncFaultInjector
	wg             sync.WaitGroup
	connectionWG   sync.WaitGroup
	closed         bool

	active         atomic.Int64
	pendingInputN  atomic.Int64
	pendingOutputN atomic.Int64
}

type endpointRuntime struct {
	identity EndpointIdentity
	socket   string
	listener net.Listener
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{}
	started  bool
}

type pendingConnection struct {
	connection net.Conn
}

type streamPair struct {
	link     Link
	consumer net.Conn
	producer net.Conn
	metrics  *linkCounters
}

type linkCounters struct {
	active             atomic.Int64
	bytesInputToOutput atomic.Uint64
	bytesOutputToInput atomic.Uint64
}

type preparedEndpoint struct {
	identity    EndpointIdentity
	publication *socketPublication
}

type resyncTransition struct {
	target       Wiring
	digest       string
	config       Config
	additions    []*preparedEndpoint
	removed      []*endpointRuntime
	closeInputs  []*pendingConnection
	closeOutputs []*pendingConnection
	closePairs   []*streamPair
	swapped      bool
}

func (server *server) prepare() error {
	if err := hostfs.MkdirAll(filepath.Join(server.runtimeDir, "in"), 0700); err != nil {
		return fmt.Errorf("create proxy input directory: %w", err)
	}
	if err := hostfs.MkdirAll(filepath.Join(server.runtimeDir, "out"), 0700); err != nil {
		return fmt.Errorf("create proxy output directory: %w", err)
	}
	if err := hostfs.RestrictDirectory(server.runtimeDir); err != nil {
		return fmt.Errorf("set proxy runtime permissions: %w", err)
	}

	// PID presence brackets every socket mutation. It is created before the
	// ownership ledger and removed only after the ledger has been emptied, so
	// recovery can distinguish an interrupted startup/shutdown from completed
	// graceful cleanup without reconstructing ownership from directory names.
	pidPath := filepath.Join(server.runtimeDir, PIDFileName)
	pidFile, err := writeOwnedFileExclusive(
		pidPath, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0600,
	)
	if err != nil {
		return fmt.Errorf("claim proxy runtime with PID marker: %w", err)
	}
	server.pidFile = pidFile
	ownership, err := createSocketOwnershipLedger(server.runtimeDir, server.instanceID)
	if err != nil {
		return err
	}
	ownership.faults = server.faults
	server.ownership = ownership

	// The control socket is the per-runtime-directory ownership boundary. It is
	// published through the same ledger-backed abstraction as every endpoint.
	controlPath := controlSocketFile(server.runtimeDir)
	controlPublication, err := prepareSocketPublication(
		server.ownership, controlPath, 0600, nil,
	)
	if err != nil {
		return fmt.Errorf("prepare proxy control socket: %w", err)
	}
	if err := controlPublication.Publish(); err != nil {
		return errors.Join(
			fmt.Errorf("publish proxy control socket: %w", err),
			controlPublication.Abort(),
		)
	}
	server.control = controlPublication.Listener()

	for _, endpoint := range server.config.Endpoints {
		publication, err := prepareSocketPublication(
			server.ownership, endpoint.Socket, 0666, nil,
		)
		if err != nil {
			return fmt.Errorf(
				"prepare %s endpoint %s.%s: %w",
				endpoint.Direction, endpoint.Component, endpoint.Name, err,
			)
		}
		if err := publication.Publish(); err != nil {
			return errors.Join(
				fmt.Errorf(
					"publish %s endpoint %s.%s: %w",
					endpoint.Direction, endpoint.Component, endpoint.Name, err,
				),
				publication.Abort(),
			)
		}
		identity := EndpointIdentity{
			Component: endpoint.Component, Name: endpoint.Name, Direction: endpoint.Direction,
		}
		runtime := server.endpointRuntime(identity, endpoint.Socket, publication.Listener())
		server.endpoints[endpointIdentityKey(identity)] = runtime
	}
	server.installRoutesAndMetricsLocked(server.wiring, nil)

	readyData, err := json.Marshal(server.statusLocked())
	if err != nil {
		return fmt.Errorf("encode proxy readiness: %w", err)
	}
	readyPath := filepath.Join(server.runtimeDir, ReadyFileName)
	readyFile, err := writeOwnedFile(readyPath, append(readyData, '\n'), 0600)
	if err != nil {
		return err
	}
	server.readyFile = readyFile
	return nil
}

func (server *server) endpointRuntime(
	identity EndpointIdentity,
	socket string,
	listener net.Listener,
) *endpointRuntime {
	ctx, cancel := context.WithCancel(server.runCtx)
	return &endpointRuntime{
		identity: identity,
		socket:   socket,
		listener: listener,
		ctx:      ctx,
		cancel:   cancel,
		done:     make(chan struct{}),
	}
}

func (server *server) startInitialEndpoints() {
	server.mu.Lock()
	var start []*endpointRuntime
	for _, endpoint := range server.endpoints {
		if server.scheduleEndpointLocked(endpoint) {
			start = append(start, endpoint)
		}
	}
	server.mu.Unlock()
	for _, endpoint := range start {
		go server.acceptEndpoint(endpoint)
	}
}

func (server *server) scheduleEndpointLocked(endpoint *endpointRuntime) bool {
	if endpoint.started {
		return false
	}
	endpoint.started = true
	server.wg.Add(1)
	return true
}

func (server *server) acceptEndpoint(endpoint *endpointRuntime) {
	defer server.wg.Done()
	defer close(endpoint.done)
	key := endpointIdentityKey(endpoint.identity)
	for {
		connection, err := endpoint.listener.Accept()
		if err != nil {
			if endpoint.ctx.Err() == nil && server.runCtx.Err() == nil {
				server.fail(fmt.Errorf("accept %s: %w", key, err))
			}
			return
		}
		server.mu.Lock()
		current, live := server.endpoints[key]
		if server.closed || !live || current != endpoint {
			server.mu.Unlock()
			_ = connection.Close()
			continue
		}
		server.trackLocked(connection)
		var pair *streamPair
		if endpoint.identity.Direction == DirectionInput {
			pair = server.registerInputLocked(key, connection)
		} else {
			pair = server.registerOutputLocked(key, connection)
		}
		server.mu.Unlock()
		if pair != nil {
			go server.runPair(pair)
		}
	}
}

// registerInputLocked captures the link and registers or pairs the connection
// under the same lock. A concurrent resync can therefore observe either the
// old link registration or the new one, never a mixed routing table.
func (server *server) registerInputLocked(inputKey string, connection net.Conn) *streamPair {
	link, exists := server.routes[inputKey]
	if !exists {
		server.untrackLocked(connection)
		_ = connection.Close()
		return nil
	}
	outputKey := linkOutputKey(link)
	if output := popPending(&server.pendingOutputs, outputKey); output != nil {
		server.pendingOutputN.Add(-1)
		return server.registerPairLocked(link, connection, output.connection)
	}
	key := linkKey(link)
	server.pendingInputs[key] = append(
		server.pendingInputs[key], &pendingConnection{connection: connection},
	)
	server.pendingInputN.Add(1)
	return nil
}

func (server *server) registerOutputLocked(outputKey string, connection net.Conn) *streamPair {
	if link, input := server.popInputForOutputLocked(outputKey); input != nil {
		server.pendingInputN.Add(-1)
		return server.registerPairLocked(link, input.connection, connection)
	}
	if !server.outputHasConsumerLocked(outputKey) {
		server.untrackLocked(connection)
		_ = connection.Close()
		return nil
	}
	server.pendingOutputs[outputKey] = append(
		server.pendingOutputs[outputKey], &pendingConnection{connection: connection},
	)
	server.pendingOutputN.Add(1)
	return nil
}

func (server *server) outputHasConsumerLocked(outputKey string) bool {
	for _, link := range server.wiring.ResolvedLinks() {
		if linkOutputKey(link) == outputKey {
			return true
		}
	}
	return false
}

func (server *server) popInputForOutputLocked(outputKey string) (Link, *pendingConnection) {
	// Wiring is canonical, which makes selection deterministic across fan-in
	// links while preserving FIFO order within each link.
	for _, link := range server.wiring.ResolvedLinks() {
		if linkOutputKey(link) != outputKey {
			continue
		}
		if input := popPending(&server.pendingInputs, linkKey(link)); input != nil {
			return link, input
		}
	}
	return Link{}, nil
}

func popPending(
	registry *map[string][]*pendingConnection,
	key string,
) *pendingConnection {
	queue := (*registry)[key]
	if len(queue) == 0 {
		return nil
	}
	result := queue[0]
	if len(queue) == 1 {
		delete(*registry, key)
	} else {
		(*registry)[key] = queue[1:]
	}
	return result
}

func (server *server) registerPairLocked(
	link Link,
	consumer,
	producer net.Conn,
) *streamPair {
	key := linkKey(link)
	pair := &streamPair{
		link: link, consumer: consumer, producer: producer, metrics: server.linkMetrics[key],
	}
	if server.pairs[key] == nil {
		server.pairs[key] = make(map[*streamPair]struct{})
	}
	server.pairs[key][pair] = struct{}{}
	// Count the goroutine before releasing mu so shutdown cannot begin Wait
	// between registration and Add.
	server.connectionWG.Add(1)
	return pair
}

func (server *server) runPair(pair *streamPair) {
	defer server.connectionWG.Done()
	server.forwardWithMetrics(
		server.runCtx,
		linkInputKey(pair.link),
		linkOutputKey(pair.link),
		pair.link.InputComponent+"."+pair.link.InputEndpoint,
		pair.consumer,
		pair.producer,
		pair.metrics,
	)
	server.mu.Lock()
	key := linkKey(pair.link)
	delete(server.pairs[key], pair)
	if len(server.pairs[key]) == 0 {
		delete(server.pairs, key)
	}
	server.mu.Unlock()
}

func (server *server) forwardWithMetrics(
	ctx context.Context,
	inputKey,
	outputKey,
	origin string,
	consumer,
	producer net.Conn,
	metrics *linkCounters,
) {
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
		// Write the proxy header under the same cancellation/teardown handling
		// as the stream copies, before any consumer bytes, outside byte metrics.
		if direction == "input-to-output" {
			if err := runtimecontract.WriteOrigin(destination, origin); err != nil {
				done <- copyResult{direction: direction, err: err}
				return
			}
		}
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
		// A copy error makes the stream pair unusable. A clean EOF keeps Unix
		// half-close semantics so the peer can finish its response.
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

func (server *server) acceptControl() {
	defer server.wg.Done()
	for {
		connection, err := server.control.Accept()
		if err != nil {
			if server.runCtx.Err() == nil {
				server.fail(fmt.Errorf("accept control connection: %w", err))
			}
			return
		}
		server.connectionWG.Add(1)
		server.track(connection)
		go server.handleControl(connection)
	}
}

func (server *server) handleControl(connection net.Conn) {
	defer server.connectionWG.Done()
	defer server.untrackAndClose(connection)
	_ = connection.SetDeadline(time.Now().Add(controlRequestTimeout))
	var request ControlRequest
	if err := readControlMessage(connection, &request); err != nil {
		_ = writeControlMessage(connection, Status{Error: "invalid control request"})
		return
	}
	if request.InstanceID != server.instanceID {
		status := server.status()
		status.Error = "proxy instance ID mismatch"
		_ = writeControlMessage(connection, status)
		return
	}
	if request.ProtocolVersion != ControlProtocolVersion {
		status := server.status()
		status.Error = fmt.Sprintf(
			"unsupported control protocol version %d", request.ProtocolVersion,
		)
		_ = writeControlMessage(connection, status)
		return
	}

	switch request.Command {
	case "status":
		_ = writeControlMessage(connection, server.status())
	case "shutdown":
		server.controlMu.Lock()
		status := server.status()
		writeErr := writeControlMessage(connection, status)
		server.cancel()
		server.controlMu.Unlock()
		_ = writeErr
	case "mod-wire", "assign-global":
		server.controlMu.Lock()
		status := server.handleEdit(request)
		_ = writeControlMessage(connection, status)
		server.controlMu.Unlock()
	case "resync":
		server.controlMu.Lock()
		status := server.handleResync(request)
		_ = writeControlMessage(connection, status)
		server.controlMu.Unlock()
	default:
		status := server.status()
		status.Error = "unknown control command"
		_ = writeControlMessage(connection, status)
	}
}

func (server *server) handleResync(request ControlRequest) Status {
	if request.Wiring == nil {
		status := server.status()
		status.Error = "resync wiring is missing"
		return status
	}
	if err := server.resync(*request.Wiring, request.Digest); err != nil {
		status := server.status()
		status.Error = err.Error()
		return status
	}
	return server.status()
}

func (server *server) resync(target Wiring, digest string) error {
	target.canonicalize()
	computed, err := target.Digest()
	if err != nil {
		return err
	}
	if digest != computed {
		return fmt.Errorf(
			"target wiring digest mismatch: expected %s, found %s", computed, digest,
		)
	}

	server.mu.Lock()
	if server.closed {
		server.mu.Unlock()
		return ErrNotRunning
	}
	if transition := server.transition; transition != nil {
		if transition.digest != digest {
			server.mu.Unlock()
			return fmt.Errorf(
				"proxy is already converging to wiring %s", transition.digest,
			)
		}
		server.mu.Unlock()
		return server.advanceTransition(transition)
	}
	if server.ready && server.digest == digest {
		server.mu.Unlock()
		return nil
	}
	live := make(map[string]struct{}, len(server.endpoints))
	for key := range server.endpoints {
		live[key] = struct{}{}
	}
	server.mu.Unlock()

	transition := &resyncTransition{target: target, digest: digest}
	for _, identity := range target.Endpoints {
		key := endpointIdentityKey(identity)
		if _, exists := live[key]; exists {
			continue
		}
		path := HostSocket(
			server.runtimeDir, identity.Direction, identity.Component, identity.Name,
		)
		publication, prepareErr := prepareSocketPublication(
			server.ownership, path, 0666, server.faults,
		)
		if prepareErr != nil {
			cleanupErr := cleanupPreparedEndpoints(transition.additions)
			return errors.Join(
				fmt.Errorf("prepare endpoint %s: %w", key, prepareErr), cleanupErr,
			)
		}
		transition.additions = append(transition.additions, &preparedEndpoint{
			identity: identity, publication: publication,
		})
	}
	transition.config = server.configForWiring(target, digest)

	server.mu.Lock()
	if server.closed {
		server.mu.Unlock()
		return errors.Join(ErrNotRunning, cleanupPreparedEndpoints(transition.additions))
	}
	if server.transition != nil {
		server.mu.Unlock()
		return errors.Join(
			fmt.Errorf("another resync started during prepare"),
			cleanupPreparedEndpoints(transition.additions),
		)
	}
	server.transition = transition
	server.mu.Unlock()
	return server.advanceTransition(transition)
}

func (server *server) advanceTransition(transition *resyncTransition) error {
	server.mu.Lock()
	if server.transition != transition {
		server.mu.Unlock()
		return fmt.Errorf("resync transition is no longer current")
	}
	server.ready = false
	server.digest = ""
	var start []*endpointRuntime
	if !transition.swapped {
		if err := injectResyncFault(server.faults, resyncStepCommitLocked); err != nil {
			server.mu.Unlock()
			return err
		}
		for _, addition := range transition.additions {
			if err := addition.publication.Publish(); err != nil {
				server.mu.Unlock()
				return fmt.Errorf(
					"publish endpoint %s: %w",
					endpointIdentityKey(addition.identity), err,
				)
			}
		}
		if err := injectResyncFault(server.faults, resyncStepPublished); err != nil {
			server.mu.Unlock()
			return err
		}

		targetEndpoints := make(map[string]EndpointIdentity, len(transition.target.Endpoints))
		for _, identity := range transition.target.Endpoints {
			targetEndpoints[endpointIdentityKey(identity)] = identity
		}
		for key, endpoint := range server.endpoints {
			if _, retained := targetEndpoints[key]; retained {
				continue
			}
			delete(server.endpoints, key)
			transition.removed = append(transition.removed, endpoint)
		}
		for _, addition := range transition.additions {
			publication := addition.publication
			runtime := server.endpointRuntime(
				addition.identity, publication.FinalPath(), publication.Listener(),
			)
			server.endpoints[endpointIdentityKey(addition.identity)] = runtime
			if server.scheduleEndpointLocked(runtime) {
				start = append(start, runtime)
			}
		}

		oldMetrics := server.linkMetrics
		server.wiring = transition.target
		server.config = transition.config
		server.installRoutesAndMetricsLocked(transition.target, oldMetrics)
		server.detachRemovedConnectionsLocked(transition)
		transition.swapped = true
	}
	server.mu.Unlock()

	for _, endpoint := range start {
		go server.acceptEndpoint(endpoint)
	}
	if err := server.teardownTransition(transition); err != nil {
		return err
	}
	if err := server.persistTransition(transition); err != nil {
		return err
	}

	server.mu.Lock()
	if server.transition != transition {
		server.mu.Unlock()
		return fmt.Errorf("resync transition changed before completion")
	}
	server.digest = transition.digest
	server.ready = true
	server.transition = nil
	server.mu.Unlock()
	log.Printf("proxy resynced system=%s digest=%s", server.system, transition.digest)
	return nil
}

func (server *server) installRoutesAndMetricsLocked(
	wiring Wiring,
	previous map[string]*linkCounters,
) {
	routes := make(map[string]Link, len(wiring.Links))
	metrics := make(map[string]*linkCounters, len(wiring.Links))
	for _, link := range wiring.ResolvedLinks() {
		routes[linkInputKey(link)] = link
		key := linkKey(link)
		if retained := previous[key]; retained != nil {
			metrics[key] = retained
		} else {
			metrics[key] = &linkCounters{}
		}
	}
	server.routes = routes
	server.linkMetrics = metrics
}

func (server *server) detachRemovedConnectionsLocked(transition *resyncTransition) {
	targetLinks := make(map[string]struct{}, len(transition.target.Links))
	consumableOutputs := make(map[string]struct{}, len(transition.target.Links))
	for _, link := range transition.target.ResolvedLinks() {
		targetLinks[linkKey(link)] = struct{}{}
		consumableOutputs[linkOutputKey(link)] = struct{}{}
	}
	targetEndpoints := make(map[string]struct{}, len(transition.target.Endpoints))
	for _, endpoint := range transition.target.Endpoints {
		targetEndpoints[endpointIdentityKey(endpoint)] = struct{}{}
	}
	for key, pending := range server.pendingInputs {
		if _, retained := targetLinks[key]; retained {
			continue
		}
		delete(server.pendingInputs, key)
		server.pendingInputN.Add(-int64(len(pending)))
		transition.closeInputs = append(transition.closeInputs, pending...)
	}
	for outputKey, pending := range server.pendingOutputs {
		_, endpointRetained := targetEndpoints[outputKey]
		_, consumable := consumableOutputs[outputKey]
		if endpointRetained && consumable {
			continue
		}
		delete(server.pendingOutputs, outputKey)
		server.pendingOutputN.Add(-int64(len(pending)))
		transition.closeOutputs = append(transition.closeOutputs, pending...)
	}
	for key, pairs := range server.pairs {
		if _, retained := targetLinks[key]; retained {
			continue
		}
		delete(server.pairs, key)
		for pair := range pairs {
			transition.closePairs = append(transition.closePairs, pair)
		}
	}
}

func (server *server) teardownTransition(transition *resyncTransition) error {
	var result error
	for _, endpoint := range transition.removed {
		endpoint.cancel()
		if err := endpoint.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			result = errors.Join(result, err)
		}
	}
	for _, endpoint := range transition.removed {
		if endpoint.started {
			<-endpoint.done
		}
	}
	for _, pending := range transition.closeInputs {
		server.untrackAndClose(pending.connection)
	}
	transition.closeInputs = nil
	for _, pending := range transition.closeOutputs {
		server.untrackAndClose(pending.connection)
	}
	transition.closeOutputs = nil
	for _, pair := range transition.closePairs {
		_ = pair.consumer.Close()
		_ = pair.producer.Close()
	}
	transition.closePairs = nil
	remaining := make([]*endpointRuntime, 0, len(transition.removed))
	for _, endpoint := range transition.removed {
		if err := injectResyncFault(server.faults, resyncStepTeardownUnlink); err != nil {
			result = errors.Join(result, fmt.Errorf(
				"remove endpoint %s: %w", endpointIdentityKey(endpoint.identity), err,
			))
			remaining = append(remaining, endpoint)
			continue
		}
		if err := server.ownership.Release(endpoint.socket); err != nil {
			result = errors.Join(result, fmt.Errorf(
				"remove endpoint %s: %w", endpointIdentityKey(endpoint.identity), err,
			))
			remaining = append(remaining, endpoint)
			continue
		}
	}
	transition.removed = remaining
	return result
}

func (server *server) persistTransition(transition *resyncTransition) error {
	encoded, err := json.MarshalIndent(transition.config, "", "  ")
	if err != nil {
		return fmt.Errorf("encode resynced proxy config: %w", err)
	}
	if err := injectResyncFault(server.faults, resyncStepPersistConfig); err != nil {
		return err
	}
	if err := writeAtomic(
		filepath.Join(server.runtimeDir, ConfigFileName), append(encoded, '\n'), 0600,
	); err != nil {
		return fmt.Errorf("persist resynced proxy config: %w", err)
	}
	return nil
}

func (server *server) configForWiring(wiring Wiring, digest string) Config {
	config := Config{
		Version: ConfigVersion, System: server.system, InstanceID: server.instanceID,
		RuntimeDir: server.runtimeDir, Digest: digest,
		Links:   append([]Link(nil), wiring.Links...),
		Globals: append([]Global(nil), wiring.Globals...),
	}
	for _, identity := range wiring.Endpoints {
		config.Endpoints = append(config.Endpoints, Endpoint{
			Component: identity.Component,
			Name:      identity.Name,
			Direction: identity.Direction,
			Socket: HostSocket(
				server.runtimeDir, identity.Direction, identity.Component, identity.Name,
			),
		})
	}
	config.canonicalize()
	return config
}

func cleanupPreparedEndpoints(additions []*preparedEndpoint) error {
	var result error
	for _, addition := range additions {
		result = errors.Join(result, addition.publication.Abort())
	}
	return result
}

func (server *server) status() Status {
	server.mu.Lock()
	defer server.mu.Unlock()
	return server.statusLocked()
}

func (server *server) statusLocked() Status {
	return server.statusForWiringLocked(server.wiring, server.digest, server.ready)
}

func (server *server) statusForWiringLocked(
	wiring Wiring,
	digest string,
	ready bool,
) Status {
	inputs, outputs := 0, 0
	for _, endpoint := range wiring.Endpoints {
		if endpoint.Direction == DirectionInput {
			inputs++
		} else {
			outputs++
		}
	}
	links := make([]LinkMetrics, 0, len(wiring.Links))
	for _, link := range wiring.ResolvedLinks() {
		metrics := server.linkMetrics[linkKey(link)]
		item := LinkMetrics{
			InputComponent:  link.InputComponent,
			InputEndpoint:   link.InputEndpoint,
			OutputComponent: link.OutputComponent,
			OutputEndpoint:  link.OutputEndpoint,
		}
		if metrics != nil {
			item.ActiveConnections = metrics.active.Load()
			item.BytesInputToOutput = metrics.bytesInputToOutput.Load()
			item.BytesOutputToInput = metrics.bytesOutputToInput.Load()
		}
		links = append(links, item)
	}
	return Status{
		Version: ConfigVersion, ControlProtocolVersion: ControlProtocolVersion,
		System: server.system, InstanceID: server.instanceID, Digest: digest,
		PID: os.Getpid(), Ready: ready, Inputs: inputs, Outputs: outputs,
		ActiveConnections: server.active.Load(),
		PendingInputs:     server.pendingInputN.Load(), PendingOutputs: server.pendingOutputN.Load(),
		Links: links, Globals: append([]Global(nil), wiring.Globals...),
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
	server.trackLocked(connection)
}

func (server *server) trackLocked(connection net.Conn) {
	if server.connections == nil {
		server.connections = make(map[net.Conn]struct{})
	}
	if server.closed {
		_ = connection.Close()
		return
	}
	server.connections[connection] = struct{}{}
}

func (server *server) untrackLocked(connection net.Conn) {
	delete(server.connections, connection)
}

func (server *server) untrackAndClose(connection net.Conn) {
	server.mu.Lock()
	server.untrackLocked(connection)
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
	server.ready = false
	server.digest = ""
	var endpoints []*endpointRuntime
	for _, endpoint := range server.endpoints {
		endpoints = append(endpoints, endpoint)
	}
	var connections []net.Conn
	for connection := range server.connections {
		connections = append(connections, connection)
	}
	var publications []*socketPublication
	if server.transition != nil {
		for _, addition := range server.transition.additions {
			publications = append(publications, addition.publication)
		}
	}
	control := server.control
	server.mu.Unlock()

	for _, endpoint := range endpoints {
		endpoint.cancel()
		_ = endpoint.listener.Close()
	}
	for _, publication := range publications {
		_ = publication.Listener().Close()
	}
	for _, connection := range connections {
		_ = connection.Close()
	}
	if control != nil {
		_ = control.Close()
	}
}

func (server *server) cleanup() {
	server.close()
	var cleanupErr error
	if err := server.readyFile.Remove(); err != nil {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove proxy readiness: %w", err))
	}
	if server.ownership != nil {
		if err := server.ownership.CleanupAll(); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		} else if err := server.ownership.Close(); err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove socket ownership ledger: %w", err))
		}
	}
	// PID is the cleanup-completion marker. Once it disappears this process
	// performs no more pathname removals, so an orchestrator may safely start a
	// replacement in the same runtime directory.
	if cleanupErr == nil {
		if err := server.pidFile.Remove(); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	if cleanupErr != nil {
		log.Printf("proxy cleanup incomplete system=%s error=%v", server.system, cleanupErr)
	}
	log.Printf("proxy stopped system=%s", server.system)
}
