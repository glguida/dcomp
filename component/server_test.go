package component

import (
	"context"
	"errors"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func TestServerHealthLifecycle(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(WithGraceTimeout(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- server.ServeListener(ctx, listener)
	}()

	dialCtx, dialCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer dialCancel()
	connection, err := grpc.DialContext(
		dialCtx,
		listener.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		t.Fatalf("dial component: %v", err)
	}
	defer connection.Close()

	response, err := healthpb.NewHealthClient(connection).Check(
		dialCtx,
		&healthpb.HealthCheckRequest{},
	)
	if err != nil {
		t.Fatalf("health check: %v", err)
	}
	if got := response.GetStatus(); got != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("health status = %v, want SERVING", got)
	}

	cancel()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("ServeListener: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not stop")
	}
}

func TestServerCannotBeReused(t *testing.T) {
	server, err := NewServer()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	first, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := server.ServeListener(ctx, first); err != nil {
		t.Fatalf("first ServeListener: %v", err)
	}

	// A context cancelled before startup deliberately does not consume the
	// server. The next real start remains valid.
	second, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	runCtx, stop := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- server.ServeListener(runCtx, second) }()
	deadline := time.Now().Add(time.Second)
	for {
		server.mu.Lock()
		started := server.started
		server.mu.Unlock()
		if started {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not start")
		}
		time.Sleep(time.Millisecond)
	}
	stop()
	if err := <-result; err != nil {
		t.Fatalf("second ServeListener: %v", err)
	}

	third, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := server.ServeListener(context.Background(), third); err == nil {
		t.Fatal("third ServeListener unexpectedly succeeded")
	}
}

func TestFatalListenerErrorShutsDownExistingTransports(t *testing.T) {
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	fail := make(chan struct{})
	listener := &failAfterFirstAccept{
		Listener: base,
		fail:     fail,
		err:      errors.New("injected accept failure"),
	}
	server, err := NewServer(WithGraceTimeout(100 * time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		result <- server.ServeListener(context.Background(), listener)
	}()

	dialCtx, cancelDial := context.WithTimeout(context.Background(), time.Second)
	defer cancelDial()
	connection, err := grpc.DialContext(
		dialCtx,
		base.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		t.Fatalf("dial component: %v", err)
	}
	defer connection.Close()

	healthClient := healthpb.NewHealthClient(connection)
	if _, err := healthClient.Check(dialCtx, &healthpb.HealthCheckRequest{}); err != nil {
		t.Fatalf("initial health check: %v", err)
	}
	close(fail)

	select {
	case err := <-result:
		if err == nil || !strings.Contains(err.Error(), "injected accept failure") {
			t.Fatalf("ServeListener error = %v, want listener failure", err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not stop after listener failure")
	}

	checkCtx, cancelCheck := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancelCheck()
	if _, err := healthClient.Check(checkCtx, &healthpb.HealthCheckRequest{}); err == nil {
		t.Fatal("existing transport remained usable after fatal listener failure")
	}
}

func TestDialListenerReconnectsAfterUnclaimedStreamCloses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "output.sock")
	upstream, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listener := newDialListener(ctx, path)
	defer listener.Close()
	upstreamResult := make(chan error, 1)
	go func() {
		first, acceptErr := upstream.Accept()
		if acceptErr != nil {
			upstreamResult <- acceptErr
			return
		}
		_ = first.Close()
		second, acceptErr := upstream.Accept()
		if acceptErr == nil {
			_, acceptErr = second.Write([]byte("x"))
			_ = second.Close()
		}
		upstreamResult <- acceptErr
	}()

	connection, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	var value [1]byte
	if _, err := io.ReadFull(connection, value[:]); err != nil {
		t.Fatal(err)
	}
	if value[0] != 'x' {
		t.Fatalf("first byte = %q", value[0])
	}
	if err := <-upstreamResult; err != nil {
		t.Fatal(err)
	}
}

type failAfterFirstAccept struct {
	net.Listener
	fail     <-chan struct{}
	err      error
	accepted bool
}

func (listener *failAfterFirstAccept) Accept() (net.Conn, error) {
	if !listener.accepted {
		connection, err := listener.Listener.Accept()
		if err == nil {
			listener.accepted = true
		}
		return connection, err
	}
	<-listener.fail
	return nil, listener.err
}
