# Restart mutates containers while the proxy is not converged

| Field | Value |
| --- | --- |
| Status | Fixed |
| Severity | Medium |
| Area | Lifecycle / restart preflight |
| Reproducibility | Deterministic |

## Summary

`dcomp restart` verifies the recorded proxy's immutable process identity but
does not require the proxy to be ready at the committed wiring digest. It can
therefore restart components while the proxy reports `ready=false` and no
digest, the explicit state used during failed or incomplete resync.

## Reproduction

Add this test to `lifecycle/controller_test.go` in package `lifecycle`:

```go
func TestRestartRejectsUnconvergedProxy(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installImages(
		fake,
		"provider:v1", "consumer:v1",
		"sha256:provider", "sha256:consumer",
	)
	spec := linkedSpec("provider:v1", "consumer:v1")
	if err := controller.Up(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	deployed := requireDesired(t, controller.State, spec.Name)

	manager := controller.Proxy.(*fakeProxyManager)
	manager.mu.Lock()
	process := manager.processes[deployed.Proxy.InstanceID]
	process.Digest = ""
	manager.processes[deployed.Proxy.InstanceID] = process
	manager.ready[deployed.Proxy.InstanceID] = false
	manager.mu.Unlock()
	fake.resetCalls()

	if err := controller.Restart(
		context.Background(),
		spec.Name,
		"consumer",
	); err == nil {
		t.Fatalf(
			"restart mutated a component while its proxy was unconverged: %#v",
			fake.mutationCalls(),
		)
	}
	if got := fake.mutationCalls(); len(got) != 0 {
		t.Fatalf("restart performed mutations after failed preflight: %#v", got)
	}
}
```

The test needs the existing `context` and `testing` imports. Run:

```text
go test ./lifecycle -run '^TestRestartRejectsUnconvergedProxy$' -count=1
```

## Behavior before the fix

`Restart` returned success and the failure reported a mutation like:

```text
restart mutated a component while its proxy was unconverged: []lifecycle.engineCall{{Method:"restart-container", ...}}
```

`preflightSelectedContainers` calls `inspectProxy` but discards the returned
status. That became insufficient when proxy inspection was deliberately changed
to verify process identity separately from mutable wiring convergence.

## Expected behavior

Restart fails preflight before writing any engine mutation unless the proxy is
ready and reports the committed deployment's wiring digest. Recovery of an
unconverged proxy remains the responsibility of `up`/resume rather than being
hidden inside restart.

## Impact

The selected components can restart into a proxy whose listeners, routing table,
or socket teardown are only partially converged. The outcome is timing-dependent
and bypasses the journaled apply recovery path responsible for repairing that
state.

## Acceptance criteria

- Restart refuses `ready=false`, an absent digest, and a non-committed digest
  before any container mutation.
- Restart succeeds when identity, readiness, and the committed wiring digest all
  match.
- A failed preflight remains safely resumable or abortable and does not mark any
  component complete.
- Tests cover both initial restart and resume of a journaled restart operation.

## Regression coverage

`TestRestartRequiresConvergedCommittedProxy` covers an absent digest,
`ready=false` with the committed digest, a ready proxy at another digest, and
successful resume after convergence is restored.
