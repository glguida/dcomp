package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/glguida/dcomp/component"
	examplev1 "github.com/glguida/dcomp/gen/dcomp/example/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	timeout := flag.Duration("timeout", 10*time.Second, "connection and request timeout")
	flag.Parse()
	if flag.NArg() != 1 {
		log.Fatal("usage: caller [--timeout DURATION] TEXT")
	}

	target, err := component.LinkTarget("upstream")
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	connection, err := grpc.DialContext(
		ctx,
		target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		log.Fatalf("connect to upstream: %v", err)
	}
	defer connection.Close()

	response, err := examplev1.NewEchoClient(connection).Echo(
		ctx,
		&examplev1.EchoRequest{Text: flag.Arg(0)},
	)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(response.GetText())
}
