# Abort fallback loses the egress-network authority for a retained container

| Field | Value |
| --- | --- |
| Status | Fixed |
| Severity | High |
| Area | Lifecycle / abort recovery / endpoint cleanup |
| Reproducibility | Deterministic |

## Summary

Abort fallback can fail before removing a retained external-egress container
when that container's target network was already observed missing. The fallback
has the previous network record available, but passes only the incomplete target
network map to endpoint-cleanup preparation.

## Reproduction

Add this test to `lifecycle/controller_test.go` in package `lifecycle`:

```go
func TestAbortFallbackUsesPreviousMissingEgressNetworkRecord(t *testing.T) {
	controller, fake := newControllerHarness(t)
	installImages(
		fake,
		"provider:v1", "consumer:v1", "observer:v1",
		"sha256:provider", "sha256:consumer", "sha256:observer",
	)
	initial := linkedSpec("provider:v1", "consumer:v1")
	initial.Components[1].Runtime.ExternalEgress = true
	if err := controller.Up(context.Background(), initial); err != nil {
		t.Fatal(err)
	}
	previous := requireDesired(t, controller.State, initial.Name)

	fake.deleteNetworkOutOfBand(
		previous.Networks[componentNetworkKey("consumer")].ID,
	)
	targetSpec := fanoutSpec("provider:v1", "consumer:v1", "observer:v1")
	targetSpec.Components[1].Runtime.ExternalEgress = true
	target, err := controller.resolve(context.Background(), targetSpec)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := state.NewOperation(
		kindApply,
		phaseRetire,
		target,
		&previous,
		controller.RuntimeRoot,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.selectRetainedResources(context.Background(), &operation); err != nil {
		t.Fatal(err)
	}
	if _, retained := operation.Containers["consumer"]; !retained {
		t.Fatal("unchanged consumer was not retained")
	}
	if _, retained := operation.Networks[componentNetworkKey("consumer")]; retained {
		t.Fatal("missing egress network was unexpectedly retained")
	}
	if err := controller.State.WriteOperation(target.Name, operation); err != nil {
		t.Fatal(err)
	}

	manager := controller.Proxy.(*fakeProxyManager)
	manager.mu.Lock()
	delete(manager.processes, previous.Proxy.InstanceID)
	delete(manager.configs, previous.Proxy.InstanceID)
	delete(manager.ready, previous.Proxy.InstanceID)
	delete(manager.controlVersions, previous.Proxy.InstanceID)
	manager.mu.Unlock()

	if err := controller.Abort(context.Background(), target.Name); err != nil {
		t.Fatal(err)
	}
}
```

The test needs the existing `context`, `testing`, and
`github.com/glguida/dcomp/state` imports. Run:

```text
go test ./lifecycle -run '^TestAbortFallbackUsesPreviousMissingEgressNetworkRecord$' -count=1
```

## Behavior before the fix

The test failed with:

```text
record consumer endpoint cleanup after proxy loss: consumer has no recorded egress network
```

`removeContainersWithStaleProxyMounts` correctly constructs a union of target
and previous network records for ownership verification. It does not use that
same authority for cleanup: `prepareEndpointCleanup` receives
`operation.Networks` only. Because selection deliberately omitted the observed
missing target network, `componentEndpointNetwork` fails before it can inspect
the previous network ID and confirm that no live endpoint remains.

## Expected behavior

The fallback uses the applicable previous deployment's network identity to
check for a live endpoint. If the recorded network is absent, cleanup preparation
is complete and the container can be retired. If it exists, its exact endpoint
identity is journaled before removal as usual.

## Impact

An out-of-band network loss combined with proxy loss makes abort unable to
recover even though the missing network means there is no endpoint left to
clean. The persisted operation continues blocking all other lifecycle commands.

## Acceptance criteria

- The reproduction converges and restores the previous desired deployment.
- A present previous network is still fully verified before its endpoint is
  journaled or mutated.
- A foreign or identity-mismatched network is refused.
- Missing networks remain an accepted, idempotent terminal observation.
- Tests cover retained and recreated external-egress containers, before and
  after fallback selection has been persisted.

## Regression coverage

`TestAbortFallbackUsesPreviousMissingEgressNetworkRecord` covers retained and
recreated containers as well as a previously persisted fallback decision.
