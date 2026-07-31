package component

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/reflection"
)

const (
	// DefaultListenAddress is reachable only according to the container's
	// Docker-network attachments; dcomp never publishes it on the host.
	DefaultListenAddress = "0.0.0.0:50051"
	DefaultGraceTimeout  = 8 * time.Second
)

type serverConfig struct {
	listenAddress string
	graceTimeout  time.Duration
	grpcOptions   []grpc.ServerOption
}

// Option configures a Server.
type Option func(*serverConfig) error

// WithListenAddress changes the component's listen address.
func WithListenAddress(address string) Option {
	return func(config *serverConfig) error {
		if strings.TrimSpace(address) == "" {
			return errors.New("listen address must not be empty")
		}
		config.listenAddress = address
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
	listen       string
	graceTimeout time.Duration

	mu       sync.Mutex
	started  bool
	services map[string]struct{}
}

// NewServer constructs a component server. Register application services on
// the returned value using their generated Register...Server function.
func NewServer(options ...Option) (*Server, error) {
	config := serverConfig{
		listenAddress: DefaultListenAddress,
		graceTimeout:  DefaultGraceTimeout,
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
		listen:       config.listenAddress,
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

// Serve listens on the configured address and blocks until ctx is cancelled,
// the server fails, or shutdown completes.
func (server *Server) Serve(ctx context.Context) error {
	listener, err := net.Listen("tcp", server.listen)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", server.listen, err)
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
	if serveErr != nil && !errors.Is(serveErr, grpc.ErrServerStopped) {
		return fmt.Errorf("serve gRPC: %w", serveErr)
	}
	return nil
}
