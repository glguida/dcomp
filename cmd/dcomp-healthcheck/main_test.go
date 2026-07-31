package main

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/glguida/dcomp/component"
)

func TestCheckServingComponent(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := component.NewServer()
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- server.ServeListener(ctx, listener)
	}()

	checkCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := check(checkCtx, listener.Addr().String(), ""); err != nil {
		t.Fatalf("check: %v", err)
	}

	stop()
	if err := <-result; err != nil {
		t.Fatalf("ServeListener: %v", err)
	}
}

func TestRunRejectsInvalidTimeout(t *testing.T) {
	if got := run([]string{"--timeout=0s"}, ioDiscard{}); got != 2 {
		t.Fatalf("run status = %d, want 2", got)
	}
}

type ioDiscard struct{}

func (ioDiscard) Write(data []byte) (int, error) {
	return len(data), nil
}
