package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os/signal"
	"syscall"
	"time"

	"github.com/glguida/dcomp/component"
	examplev1 "github.com/glguida/dcomp/gen/dcomp/example/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func main() {
	timeout := flag.Duration("timeout", 10*time.Second, "connection and request timeout")
	wait := flag.Bool("wait", false, "remain idle as a system-managed test consumer")
	flag.Parse()
	if *wait {
		if flag.NArg() != 0 {
			log.Fatal("usage: caller --wait")
		}
		target, err := component.InputTarget("upstream")
		if err != nil {
			log.Fatal(err)
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		connection, err := grpc.DialContext(
			ctx,
			target,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithBlock(),
		)
		if err != nil {
			if ctx.Err() == nil {
				log.Fatalf("connect to upstream: %v", err)
			}
			return
		}
		defer connection.Close()
		<-ctx.Done()
		return
	}
	if flag.NArg() != 1 {
		log.Fatal("usage: caller [--timeout DURATION] TEXT")
	}

	target, err := component.InputTarget("upstream")
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
