package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	examplev1 "github.com/glguida/dcomp/gen/dcomp/example/v1"
	"google.golang.org/grpc"
)

type countingEchoClient struct {
	calls  atomic.Int64
	cancel context.CancelFunc
}

func (client *countingEchoClient) Echo(
	context.Context,
	*examplev1.EchoRequest,
	...grpc.CallOption,
) (*examplev1.EchoResponse, error) {
	if client.calls.Add(1) == 3 {
		client.cancel()
	}
	return &examplev1.EchoResponse{Text: "ok"}, nil
}

func TestRunRepeatedlyCallsImmediatelyAndAtEveryInterval(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client := &countingEchoClient{cancel: cancel}
	runRepeatedly(ctx, client, 100*time.Millisecond, time.Millisecond, "traffic")
	if calls := client.calls.Load(); calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
}
