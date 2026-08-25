# Proxy queues producer connections when no link can consume them

| Field | Value |
| --- | --- |
| Status | Fixed |
| Severity | Medium |
| Area | Proxy / connection registration / resync |
| Reproducibility | Deterministic |

## Summary

After the last inbound link to a surviving output endpoint is removed, producer
connections that were pending during resync are closed correctly. A producer
that reconnects after the resync, however, is accepted and queued forever even
though no live link can consume it.

This makes the result depend on whether the producer connected just before or
just after the routing-table swap.

## Reproduction

Add this test to `proxy/server_test.go` in package `proxy`:

```go
func TestProducerReconnectClosesWhenOutputHasNoConsumers(t *testing.T) {
	config := testConfig(t, false)
	cancel, result := startTestProxy(t, config)
	defer func() {
		cancel()
		if err := <-result; err != nil {
			t.Fatalf("proxy Run: %v", err)
		}
	}()

	manager := &ProcessManager{}
	process := processForConfig(config, os.Getpid())
	outputOnly := Wiring{Endpoints: []EndpointIdentity{{
		Component: "source",
		Name:      "documents",
		Direction: DirectionOutput,
	}}}
	digest, err := outputOnly.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Resync(
		context.Background(),
		process,
		outputOnly,
		digest,
	); err != nil {
		t.Fatal(err)
	}

	producer := dialUnix(
		t,
		HostSocket(config.RuntimeDir, DirectionOutput, "source", "documents"),
	)
	defer producer.Close()
	assertConnectionClosedPromptly(t, producer)
}
```

The test needs the existing `context`, `os`, and `testing` imports. Run:

```text
go test ./proxy -run '^TestProducerReconnectClosesWhenOutputHasNoConsumers$' -count=1
```

## Behavior before the fix

After one second, the test failed with:

```text
removed connection did not close promptly
```

Status also reports one pending output. `registerOutputLocked` tries to pair the
connection and otherwise appends it to `pendingOutputs` without checking whether
the current wiring contains any link targeting that output. Resync teardown only
closes unconsumable outputs that were already pending at the transition.

## Expected behavior

When an output endpoint has no current inbound link, a newly accepted producer
connection is closed immediately and is not counted as pending. If at least one
inbound link survives, pending outputs remain available to all surviving
consumers, preserving fan-in semantics.

## Impact

Producers can mistake a locally accepted Unix connection for viable routing and
block or buffer indefinitely. Repeated reconnect attempts grow the proxy's
pending registry and open-file count until a link is added or the proxy exits.

## Acceptance criteria

- The reproduction observes EOF/reset promptly and `PendingOutputs == 0`.
- An output with one or more consuming links retains the existing pending-pool
  behavior.
- Removing one of several inbound links does not close pending outputs while a
  consumer remains.
- Removing the last consuming link closes outputs already pending and rejects
  outputs accepted afterward.
- Adding a first consuming link makes subsequent producer connections eligible
  for pairing without replacing the endpoint listener.

## Regression coverage

`TestProducerReconnectClosesWhenOutputHasNoConsumers` verifies rejection with
no consuming link, an empty pending pool, and successful pairing after the link
is restored. Existing fan-in coverage verifies retention until the last link is
removed.
