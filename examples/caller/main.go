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
	repeat := flag.Duration("repeat", 0, "repeat the request at this interval")
	flag.Parse()
	if *repeat < 0 {
		log.Fatal("--repeat must not be negative")
	}
	if *wait && *repeat != 0 {
		log.Fatal("--wait and --repeat cannot be used together")
	}
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
	if *repeat > 0 {
		if flag.NArg() > 1 {
			log.Fatal("usage: caller [--timeout DURATION] --repeat DURATION [TEXT]")
		}
		text := "dcomp demo traffic"
		if flag.NArg() == 1 {
			text = flag.Arg(0)
		}
		target, err := component.InputTarget("upstream")
		if err != nil {
			log.Fatal(err)
		}
		ctx, stop := signal.NotifyContext(
			context.Background(), syscall.SIGINT, syscall.SIGTERM,
		)
		defer stop()
		connection, err := grpc.DialContext(
			ctx,
			target,
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		if err != nil {
			log.Fatalf("create upstream client: %v", err)
		}
		defer connection.Close()
		log.Printf("autocaller started interval=%s", *repeat)
		runRepeatedly(
			ctx,
			examplev1.NewEchoClient(connection),
			*timeout,
			*repeat,
			text,
		)
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

func runRepeatedly(
	ctx context.Context,
	client examplev1.EchoClient,
	timeout time.Duration,
	interval time.Duration,
	text string,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	failed := false
	for {
		requestCtx, cancel := context.WithTimeout(ctx, timeout)
		_, err := client.Echo(
			requestCtx,
			&examplev1.EchoRequest{Text: text},
			grpc.WaitForReady(true),
		)
		cancel()
		if err != nil && ctx.Err() == nil {
			if !failed {
				log.Printf("autocaller request failed: %v", err)
			}
			failed = true
		} else if err == nil && failed {
			log.Printf("autocaller requests recovered")
			failed = false
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
