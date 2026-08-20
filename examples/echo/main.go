package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	"github.com/glguida/dcomp/component"
	examplev1 "github.com/glguida/dcomp/gen/dcomp/example/v1"
)

type echoServer struct {
	examplev1.UnimplementedEchoServer
}

func (echoServer) Echo(_ context.Context, request *examplev1.EchoRequest) (*examplev1.EchoResponse, error) {
	log.Printf("rpc=%s status=ok", examplev1.Echo_Echo_FullMethodName)
	return &examplev1.EchoResponse{Text: request.GetText()}, nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	server, err := component.NewServer(component.WithOutput("echo"))
	if err != nil {
		log.Fatal(err)
	}
	examplev1.RegisterEchoServer(server, echoServer{})
	if err := server.Serve(ctx); err != nil {
		log.Fatal(err)
	}
}
