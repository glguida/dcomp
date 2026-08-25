# Abort fallback rejects a valid previous-generation container

| Field | Value |
| --- | --- |
| Status | Fixed |
| Severity | High |
| Area | Lifecycle / abort recovery |
| Reproducibility | Deterministic |

## Summary

If an apply is aborted before its retire phase has removed the previous fleet,
the fallback path can reject a valid previous-generation container at DComp's
deterministic container name as foreign occupancy. The abort is then durable but
cannot make progress on retry.

The simplest case is a runtime-root change interrupted after the operation is
journaled and before retirement. The target operation intentionally retains no
old containers or proxy, even though the previous deployment is still live and
valid.

## Reproduction

Add this test to `lifecycle/controller_test.go` in package `lifecycle`:

```go
func TestAbortFallbackBeforeRetireAcceptsPreviousRuntimeFleet(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installImages(
		fake,
		"provider:v1", "consumer:v1",
		"sha256:provider", "sha256:consumer",
	)
	initial := linkedSpec("provider:v1", "consumer:v1")
	if err := controller.Up(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	previous := requireDesired(t, controller.State, initial.Name)
	target, err := controller.resolve(context.Background(), initial)
	if err != nil {
		t.Fatal(err)
	}

	newRuntimeRoot := t.TempDir()
	operation, err := state.NewOperation(
		kindApply,
		phaseRetire,
		target,
		&previous,
		newRuntimeRoot,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.selectRetainedResources(context.Background(), &operation); err != nil {
		t.Fatal(err)
	}
	if len(operation.Containers) != 0 || operation.Proxy != nil {
		t.Fatalf("runtime-root change unexpectedly retained resources: %#v", operation)
	}
	if err := controller.State.WriteOperation(target.Name, operation); err != nil {
		t.Fatal(err)
	}
	controller.RuntimeRoot = newRuntimeRoot

	if err := controller.Abort(context.Background(), target.Name); err != nil {
		t.Fatal(err)
	}
}
```

The test needs the existing `context`, `testing`, and
`github.com/glguida/dcomp/state` imports. Run:

```text
go test ./lifecycle -run '^TestAbortFallbackBeforeRetireAcceptsPreviousRuntimeFleet$' -count=1
```

## Behavior before the fix

The test failed with an error containing:

```text
remove socket-mounted containers during abort fallback: container name "..." is occupied
```

`restorePreviousProxyDuringAbort` selects fleet recreation because the target
operation has no retained proxy. `removeContainersWithStaleProxyMounts` then
looks up target container names but recognizes only `operation.Containers` and
containers labelled with the current operation ID. The still-valid containers
recorded by `operation.Previous` satisfy neither condition.

The fallback choice is persisted in `AbortRecreatePrevious`, so resuming the
abort reaches the same failure.

## Expected behavior

Abort recognizes the exact containers recorded by `operation.Previous` as
DComp-owned previous-generation occupancy. It safely restores or recreates the
previous deployment and clears the operation without treating those containers
as foreign.

## Other affected transition

The same defect occurs without a runtime-root change:

1. Apply a two-component system.
2. Change one component's container definition.
3. Journal the new operation at `phaseRetire`, but do not execute retirement.
4. Make the recorded proxy unavailable so abort selects fallback.
5. Abort.

The changed component is absent from `operation.Containers` but its valid
previous-generation container still occupies the deterministic name, producing
the same error.

## Impact

A crash followed by abort can leave a healthy previous fleet running while the
journal is permanently stuck. Normal `up`, `restart`, and `down` operations are
then blocked by the pending operation.

## Acceptance criteria

- Both the runtime-root and changed-container cases above converge.
- Only the exact IDs and ownership recorded by the previous deployment are
  accepted; an unrelated container at the deterministic name is still refused.
- Repeated abort/resume calls are idempotent at every persisted checkpoint.
- No previous-generation container is removed before any endpoint cleanup
  needed for its live Docker network attachment is durably recorded.

## Regression coverage

- `TestAbortFallbackBeforeRetireAcceptsPreviousRuntimeFleet`
- `TestAbortFallbackBeforeRetireAcceptsChangedPreviousContainer`
- `TestAbortFallbackRejectsForeignContainerAtPreviousName`
- `TestAbortFallbackRecreatesPreviousOnlySocketMountedContainer`
