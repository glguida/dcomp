package component_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/glguida/dcomp/component"
	"github.com/glguida/dcomp/composition"
	examplev1 "github.com/glguida/dcomp/gen/dcomp/example/v1"
	"github.com/glguida/dcomp/proxy"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/peer"
)

func TestGRPCComponentServesThroughClientOnlyProxyConnection(t *testing.T) {
	runtimeRoot, err := os.MkdirTemp("/tmp", "dcomp-component-proxy-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(runtimeRoot)
	resolved := composition.ResolvedSpec{
		Name: "demo",
		Components: []composition.ResolvedComponent{
			{
				Name: "provider",
				Definition: composition.Definition{Outputs: []composition.Endpoint{{
					Name: "echo", Service: "dcomp.example.v1.Echo",
				}}},
			},
			{
				Name: "consumer",
				Definition: composition.Definition{Inputs: []composition.Endpoint{{
					Name: "upstream", Service: "dcomp.example.v1.Echo",
				}}},
			},
		},
		Links: []composition.Link{{
			Input:  composition.EndpointRef{Component: "consumer", Endpoint: "upstream"},
			Output: composition.EndpointRef{Component: "provider", Endpoint: "echo"},
		}},
	}
	config, err := proxy.NewConfig(
		resolved, filepath.Join(runtimeRoot, "demo"), "component-contract-test",
	)
	if err != nil {
		t.Fatal(err)
	}
	proxyCtx, stopProxy := context.WithCancel(context.Background())
	proxyReady := make(chan struct{})
	proxyResult := make(chan error, 1)
	go func() {
		proxyResult <- proxy.Run(proxyCtx, config, func(proxy.Status) error {
			close(proxyReady)
			return nil
		})
	}()
	select {
	case <-proxyReady:
	case err := <-proxyResult:
		t.Fatalf("proxy failed before readiness: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("proxy did not become ready")
	}
	defer func() {
		stopProxy()
		if err := <-proxyResult; err != nil {
			t.Fatalf("proxy: %v", err)
		}
	}()

	t.Setenv(
		"DCOMP_OUT_ECHO",
		"unix://"+proxy.HostSocket(config.RuntimeDir, proxy.DirectionOutput, "provider", "echo"),
	)
	server, err := component.NewServer(component.WithOutput("echo"),
		component.WithGRPCOptions(grpc.UnaryInterceptor(func(
			ctx context.Context, request interface{}, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler,
		) (interface{}, error) {
			origin, ok := peer.FromContext(ctx)
			if !ok || origin.Addr.Network() != "dcomp" || origin.Addr.String() != "consumer.upstream" {
				t.Errorf("unexpected RPC peer: %#v", origin)
			}
			return handler(ctx, request)
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	examplev1.RegisterEchoServer(server, echoService{})
	serveCtx, stopServer := context.WithCancel(context.Background())
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve(serveCtx) }()

	dialCtx, cancelDial := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelDial()
	input := proxy.HostSocket(config.RuntimeDir, proxy.DirectionInput, "consumer", "upstream")
	connection, err := grpc.DialContext(
		dialCtx,
		"unix://"+input,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		t.Fatalf("dial proxied component: %v", err)
	}
	response, err := examplev1.NewEchoClient(connection).Echo(
		dialCtx, &examplev1.EchoRequest{Text: "through proxy"},
	)
	if err != nil {
		t.Fatalf("proxied Echo: %v", err)
	}
	if got := response.GetText(); got != "through proxy" {
		t.Fatalf("Echo text = %q", got)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}

	stopServer()
	select {
	case err := <-serveResult:
		if err != nil {
			t.Fatalf("component server: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("component server did not stop")
	}
}
