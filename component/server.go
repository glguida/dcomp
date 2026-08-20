package component

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
)

const (
	DefaultGraceTimeout = 8 * time.Second
)

type serverConfig struct {
	outputs      []string
	graceTimeout time.Duration
	grpcOptions  []grpc.ServerOption
}

// Option configures a Server.
type Option func(*serverConfig) error

// WithOutput adds one declared output endpoint to Serve. The component dials
// the proxy-provided Unix socket; it never binds or listens on that path.
func WithOutput(name string) Option {
	return func(config *serverConfig) error {
		if _, err := OutputEnv(name); err != nil {
			return err
		}
		for _, existing := range config.outputs {
			if existing == name {
				return fmt.Errorf("output %q is configured more than once", name)
			}
		}
		config.outputs = append(config.outputs, name)
		return nil
	}
}

// WithGraceTimeout bounds graceful shutdown before outstanding RPCs are
// forcibly stopped.
func WithGraceTimeout(timeout time.Duration) Option {
	return func(config *serverConfig) error {
		if timeout <= 0 {
			return errors.New("grace timeout must be positive")
		}
		config.graceTimeout = timeout
		return nil
	}
}

// WithGRPCOptions passes options to the underlying gRPC server.
func WithGRPCOptions(options ...grpc.ServerOption) Option {
	return func(config *serverConfig) error {
		config.grpcOptions = append(config.grpcOptions, options...)
		return nil
	}
}

// Server is a normal gRPC service registrar with standard health and
// reflection enabled. It owns no application protocol and keeps no host state.
type Server struct {
	grpc         *grpc.Server
	health       *health.Server
	outputs      []string
	graceTimeout time.Duration

	mu       sync.Mutex
	started  bool
	services map[string]struct{}
}

// NewServer constructs a component server. Register application services on
// the returned value using their generated Register...Server function.
func NewServer(options ...Option) (*Server, error) {
	config := serverConfig{
		graceTimeout: DefaultGraceTimeout,
	}
	for _, option := range options {
		if option == nil {
			return nil, errors.New("nil server option")
		}
		if err := option(&config); err != nil {
			return nil, err
		}
	}

	grpcServer := grpc.NewServer(config.grpcOptions...)
	healthServer := health.NewServer()
	healthServer.SetServingStatus("", healthpb.HealthCheckResponse_NOT_SERVING)
	healthpb.RegisterHealthServer(grpcServer, healthServer)
	reflection.Register(grpcServer)

	return &Server{
		grpc:         grpcServer,
		health:       healthServer,
		outputs:      append([]string(nil), config.outputs...),
		graceTimeout: config.graceTimeout,
		services:     make(map[string]struct{}),
	}, nil
}

// RegisterService implements grpc.ServiceRegistrar. Generated protobuf
// registration functions can therefore receive *Server directly.
func (server *Server) RegisterService(description *grpc.ServiceDesc, implementation interface{}) {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.started {
		panic("dcomp: cannot register a service after Serve")
	}
	server.grpc.RegisterService(description, implementation)
	server.services[description.ServiceName] = struct{}{}
	server.health.SetServingStatus(description.ServiceName, healthpb.HealthCheckResponse_NOT_SERVING)
}

// Serve connects to every configured output socket and blocks until ctx is
// cancelled, the server fails, or shutdown completes.
func (server *Server) Serve(ctx context.Context) error {
	if ctx == nil {
		return errors.New("nil serve context")
	}
	if len(server.outputs) == 0 {
		return errors.New("dcomp server has no output; configure WithOutput")
	}
	listeners := make([]net.Listener, 0, len(server.outputs))
	for _, output := range server.outputs {
		target, err := OutputTarget(output)
		if err != nil {
			closeListeners(listeners)
			return err
		}
		path, err := unixPath(target)
		if err != nil {
			closeListeners(listeners)
			return err
		}
		listeners = append(listeners, newDialListener(ctx, path))
	}
	listener := net.Listener(listeners[0])
	if len(listeners) > 1 {
		listener = newMultiListener(ctx, listeners)
	}
	return server.ServeListener(ctx, listener)
}

// ServeListener is Serve with a caller-supplied listener. It is primarily
// useful to embedders and tests; ownership of listener transfers to Server.
func (server *Server) ServeListener(ctx context.Context, listener net.Listener) error {
	if listener == nil {
		return errors.New("nil listener")
	}
	if ctx == nil {
		_ = listener.Close()
		return errors.New("nil serve context")
	}
	if err := ctx.Err(); err != nil {
		_ = listener.Close()
		return nil
	}

	server.mu.Lock()
	if server.started {
		server.mu.Unlock()
		_ = listener.Close()
		return errors.New("dcomp server may only be served once")
	}
	server.started = true
	services := make([]string, 0, len(server.services))
	for service := range server.services {
		services = append(services, service)
	}
	server.mu.Unlock()

	for _, service := range services {
		server.health.SetServingStatus(service, healthpb.HealthCheckResponse_SERVING)
	}
	server.health.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)

	serveResult := make(chan error, 1)
	go func() {
		serveResult <- server.grpc.Serve(listener)
	}()

	var serveErr error
	select {
	case serveErr = <-serveResult:
	case <-ctx.Done():
	}
	server.health.Shutdown()

	graceful := make(chan struct{})
	go func() {
		server.grpc.GracefulStop()
		close(graceful)
	}()

	timer := time.NewTimer(server.graceTimeout)
	defer timer.Stop()
	select {
	case <-graceful:
	case <-timer.C:
		server.grpc.Stop()
		<-graceful
	}

	if serveErr == nil {
		serveErr = <-serveResult
	}
	if serveErr != nil &&
		!errors.Is(serveErr, grpc.ErrServerStopped) &&
		!(ctx.Err() != nil && errors.Is(serveErr, net.ErrClosed)) {
		return fmt.Errorf("serve gRPC: %w", serveErr)
	}
	return nil
}

type dialListener struct {
	ctx    context.Context
	cancel context.CancelFunc
	path   string
	once   sync.Once
}

func newDialListener(ctx context.Context, path string) *dialListener {
	listenerCtx, cancel := context.WithCancel(ctx)
	return &dialListener{ctx: listenerCtx, cancel: cancel, path: path}
}

func (listener *dialListener) Accept() (net.Conn, error) {
	for {
		connection, err := (&net.Dialer{}).DialContext(listener.ctx, "unix", listener.path)
		if err != nil {
			if listener.ctx.Err() != nil {
				return nil, net.ErrClosed
			}
			if !listener.retry() {
				return nil, net.ErrClosed
			}
			continue
		}
		// The proxy accepts producer connections before a consumer necessarily
		// exists. A gRPC server would otherwise spin through the Unix listen
		// backlog and accumulate idle transports. Wait for the first client byte,
		// then replay it to gRPC; this also makes one output connection correspond
		// to one actual consumer connection.
		type readResult struct {
			value byte
			err   error
		}
		read := make(chan readResult, 1)
		go func() {
			var first [1]byte
			_, readErr := connection.Read(first[:])
			read <- readResult{value: first[0], err: readErr}
		}()
		select {
		case result := <-read:
			if result.err == nil {
				return &prefixedConn{Conn: connection, prefix: []byte{result.value}}, nil
			}
			_ = connection.Close()
			if !listener.retry() {
				return nil, net.ErrClosed
			}
		case <-listener.ctx.Done():
			_ = connection.Close()
			<-read
			return nil, net.ErrClosed
		}
	}
}

func (listener *dialListener) retry() bool {
	timer := time.NewTimer(25 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-listener.ctx.Done():
		return false
	}
}

func (listener *dialListener) Close() error {
	listener.once.Do(listener.cancel)
	return nil
}

func (listener *dialListener) Addr() net.Addr {
	return &net.UnixAddr{Name: listener.path, Net: "unix"}
}

type prefixedConn struct {
	net.Conn
	prefix []byte
}

func (connection *prefixedConn) Read(buffer []byte) (int, error) {
	if len(connection.prefix) != 0 && len(buffer) != 0 {
		buffer[0] = connection.prefix[0]
		connection.prefix = connection.prefix[1:]
		return 1, nil
	}
	return connection.Conn.Read(buffer)
}

type acceptResult struct {
	connection net.Conn
	err        error
}

type multiListener struct {
	ctx       context.Context
	cancel    context.CancelFunc
	listeners []net.Listener
	accepted  chan acceptResult
	once      sync.Once
}

func newMultiListener(ctx context.Context, listeners []net.Listener) *multiListener {
	listenerCtx, cancel := context.WithCancel(ctx)
	combined := &multiListener{
		ctx: listenerCtx, cancel: cancel,
		listeners: listeners, accepted: make(chan acceptResult),
	}
	for _, listener := range listeners {
		listener := listener
		go func() {
			for {
				connection, err := listener.Accept()
				select {
				case combined.accepted <- acceptResult{connection: connection, err: err}:
				case <-listenerCtx.Done():
					if connection != nil {
						_ = connection.Close()
					}
					return
				}
				if err != nil {
					return
				}
			}
		}()
	}
	return combined
}

func (listener *multiListener) Accept() (net.Conn, error) {
	select {
	case result := <-listener.accepted:
		return result.connection, result.err
	case <-listener.ctx.Done():
		return nil, net.ErrClosed
	}
}

func (listener *multiListener) Close() error {
	listener.once.Do(func() {
		listener.cancel()
		closeListeners(listener.listeners)
	})
	return nil
}

func (listener *multiListener) Addr() net.Addr {
	return listener.listeners[0].Addr()
}

func closeListeners(listeners []net.Listener) {
	for _, listener := range listeners {
		_ = listener.Close()
	}
}
