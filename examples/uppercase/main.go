package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/glguida/dcomp/component"
	examplev1 "github.com/glguida/dcomp/gen/dcomp/example/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type uppercaseServer struct {
	examplev1.UnimplementedEchoServer
	upstream examplev1.EchoClient
	requests atomic.Uint64
}

func (server *uppercaseServer) Echo(ctx context.Context, request *examplev1.EchoRequest) (*examplev1.EchoResponse, error) {
	server.requests.Add(1)
	log.Printf("rpc=%s upstream=echo", examplev1.Echo_Echo_FullMethodName)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	response, err := server.upstream.Echo(ctx, request)
	if err != nil {
		return nil, err
	}
	return &examplev1.EchoResponse{Text: strings.ToUpper(response.GetText())}, nil
}

func (server *uppercaseServer) adminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = response.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /metrics", func(response http.ResponseWriter, _ *http.Request) {
		response.Header().Set(
			"Content-Type",
			"text/plain; version=0.0.4; charset=utf-8",
		)
		_, _ = fmt.Fprintf(
			response,
			"# HELP dcomp_example_uppercase_requests_total Echo requests handled.\n"+
				"# TYPE dcomp_example_uppercase_requests_total counter\n"+
				"dcomp_example_uppercase_requests_total %d\n",
			server.requests.Load(),
		)
	})
	return mux
}

func main() {
	target, err := component.InputTarget("upstream")
	if err != nil {
		log.Fatal(err)
	}
	connection, err := grpc.Dial(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("connect to upstream: %v", err)
	}
	defer connection.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	server, err := component.NewServer(component.WithOutput("echo"))
	if err != nil {
		log.Fatal(err)
	}
	application := &uppercaseServer{
		upstream: examplev1.NewEchoClient(connection),
	}
	examplev1.RegisterEchoServer(server, application)

	// This is an ordinary component-owned TCP listener, not a DComp interface.
	// The system file must explicitly publish it before it is host-reachable.
	admin := &http.Server{
		Addr:              "0.0.0.0:8080",
		Handler:           application.adminHandler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	adminErrors := make(chan error, 1)
	go func() {
		log.Printf("admin=http://%s", admin.Addr)
		err := admin.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		adminErrors <- err
	}()
	grpcErrors := make(chan error, 1)
	go func() {
		grpcErrors <- server.Serve(ctx)
	}()

	var runErr error
	select {
	case runErr = <-adminErrors:
	case runErr = <-grpcErrors:
	case <-ctx.Done():
	}
	stop()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := admin.Shutdown(shutdownCtx); runErr == nil && err != nil {
		runErr = fmt.Errorf("shut down admin server: %w", err)
	}
	if runErr != nil {
		log.Fatal(runErr)
	}
}
