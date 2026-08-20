package main

import (
	"context"
	"log"
	"os/signal"
	"strings"
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
}

func (server uppercaseServer) Echo(ctx context.Context, request *examplev1.EchoRequest) (*examplev1.EchoResponse, error) {
	log.Printf("rpc=%s upstream=echo", examplev1.Echo_Echo_FullMethodName)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	response, err := server.upstream.Echo(ctx, request)
	if err != nil {
		return nil, err
	}
	return &examplev1.EchoResponse{Text: strings.ToUpper(response.GetText())}, nil
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
	examplev1.RegisterEchoServer(server, uppercaseServer{
		upstream: examplev1.NewEchoClient(connection),
	})
	if err := server.Serve(ctx); err != nil {
		log.Fatal(err)
	}
}
