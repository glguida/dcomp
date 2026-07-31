package component_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/glguida/dcomp/component"
	examplev1 "github.com/glguida/dcomp/gen/dcomp/example/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	reflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
)

type echoService struct {
	examplev1.UnimplementedEchoServer
}

func (echoService) Echo(_ context.Context, request *examplev1.EchoRequest) (*examplev1.EchoResponse, error) {
	return &examplev1.EchoResponse{Text: request.GetText()}, nil
}

func TestArbitraryServiceHealthAndReflection(t *testing.T) {
	server, err := component.NewServer()
	if err != nil {
		t.Fatal(err)
	}
	examplev1.RegisterEchoServer(server, echoService{})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- server.ServeListener(ctx, listener) }()

	dialCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	connection, err := grpc.DialContext(
		dialCtx,
		listener.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer connection.Close()

	healthResponse, err := healthpb.NewHealthClient(connection).Check(
		dialCtx,
		&healthpb.HealthCheckRequest{Service: "dcomp.example.v1.Echo"},
	)
	if err != nil {
		t.Fatalf("service health: %v", err)
	}
	if healthResponse.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("service health = %s, want SERVING", healthResponse.GetStatus())
	}

	reflection := reflectionpb.NewServerReflectionClient(connection)
	stream, err := reflection.ServerReflectionInfo(dialCtx)
	if err != nil {
		t.Fatalf("reflection stream: %v", err)
	}
	if err := stream.Send(&reflectionpb.ServerReflectionRequest{
		MessageRequest: &reflectionpb.ServerReflectionRequest_ListServices{
			ListServices: "",
		},
	}); err != nil {
		t.Fatalf("request service list: %v", err)
	}
	reflectionResponse, err := stream.Recv()
	if err != nil {
		t.Fatalf("receive service list: %v", err)
	}
	found := false
	for _, service := range reflectionResponse.GetListServicesResponse().GetService() {
		if service.GetName() == "dcomp.example.v1.Echo" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("reflection did not list dcomp.example.v1.Echo")
	}

	echoResponse, err := examplev1.NewEchoClient(connection).Echo(
		dialCtx,
		&examplev1.EchoRequest{Text: "transparent"},
	)
	if err != nil {
		t.Fatalf("Echo: %v", err)
	}
	if got := echoResponse.GetText(); got != "transparent" {
		t.Fatalf("Echo text = %q", got)
	}

	stop()
	if err := <-result; err != nil {
		t.Fatalf("ServeListener: %v", err)
	}
}
