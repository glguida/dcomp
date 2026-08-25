# Manager shutdown removes relinquished sockets

| Field | Value |
| --- | --- |
| Status | Fixed |
| Severity | High |
| Area | Proxy / process manager / shutdown cleanup |
| Reproducibility | Deterministic |

## Summary

After a successful resync removes and disowns an endpoint, a different owner
may publish a socket at the relinquished path. The proxy's live cleanup
preserves it, but `ProcessManager.Stop` subsequently reconstructs ownership by
sweeping the endpoint directories and removes the new socket.

## Reproduction

Run:

```text
go test ./proxy -run '^TestProcessManagerStopPreservesSocketReoccupyingRelinquishedEndpoint$' -count=1
```

The test covers direct and shortened socket publication, removes one endpoint,
reoccupies it, and performs an identity-verified manager shutdown.

## Behavior before the fix

Manager cleanup removes the reoccupied exposed socket. For shortened
publication it may leave only the hidden sibling, splitting the foreign
publication.

## Expected behavior

Successful resync permanently relinquishes every artifact of the removed
endpoint. No later shutdown or recovery pass removes a new object merely
because it uses the same pathname.

## Impact

An ordinary manager-driven shutdown can delete another owner's live socket and
violate the resync pathname-ownership invariant.

## Acceptance criteria

- Direct and shortened reoccupied sockets survive `ProcessManager.Stop`.
- Sockets still owned by the stopped proxy are removed.
- Cleanup does not infer ownership from directory membership.

## Regression coverage

`TestProcessManagerStopPreservesSocketReoccupyingRelinquishedEndpoint` and
`TestCrashCleanupDoesNotInferSocketOwnershipFromConfig`
