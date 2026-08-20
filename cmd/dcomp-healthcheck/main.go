// dcomp-healthcheck is a tiny standard gRPC health probe intended for an
// image's Docker HEALTHCHECK.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

func check(ctx context.Context, target, service string) error {
	connection, err := grpc.DialContext(
		ctx,
		target,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		return fmt.Errorf("connect to %s: %w", target, err)
	}
	defer connection.Close()

	response, err := healthpb.NewHealthClient(connection).Check(
		ctx,
		&healthpb.HealthCheckRequest{Service: service},
	)
	if err != nil {
		return fmt.Errorf("check %s: %w", target, err)
	}
	if response.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		return fmt.Errorf("%s reports %s", target, response.GetStatus())
	}
	return nil
}

func run(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("dcomp-healthcheck", flag.ContinueOnError)
	flags.SetOutput(stderr)
	target := flags.String("target", "", "explicit gRPC target")
	socket := flags.String("socket", "", "require an orchestrator-mounted Unix socket")
	service := flags.String("service", "", "fully-qualified service name")
	timeout := flags.Duration("timeout", 2*time.Second, "total connection and RPC timeout")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "dcomp-healthcheck: unexpected positional arguments")
		return 2
	}
	if *timeout <= 0 {
		fmt.Fprintln(stderr, "dcomp-healthcheck: timeout must be positive")
		return 2
	}
	if (*target == "") == (*socket == "") {
		fmt.Fprintln(stderr, "dcomp-healthcheck: exactly one of --target or --socket is required")
		return 2
	}
	if *socket != "" {
		info, err := os.Stat(*socket)
		if err != nil {
			fmt.Fprintf(stderr, "dcomp-healthcheck: inspect socket: %v\n", err)
			return 1
		}
		if info.Mode()&os.ModeSocket == 0 {
			fmt.Fprintf(stderr, "dcomp-healthcheck: %s is not a Unix socket\n", *socket)
			return 1
		}
		return 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	if err := check(ctx, *target, *service); err != nil {
		if !errors.Is(err, context.Canceled) {
			fmt.Fprintf(stderr, "dcomp-healthcheck: %v\n", err)
		}
		return 1
	}
	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}
