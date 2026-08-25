# Shortened teardown loses per-path ownership

| Field | Value |
| --- | --- |
| Status | Fixed |
| Severity | High |
| Area | Proxy / shortened publication / teardown retry |
| Reproducibility | Deterministic |

## Summary

A shortened Unix socket has an exposed path and a hidden actual path. Teardown
removes both through one function but records success only for the endpoint as
a whole. If removal of one path succeeds and the other fails, retry forgets
which path was already relinquished and can delete its new occupant.

## Reproduction

Run:

```text
go test ./proxy -run '^TestShortenedTeardownTracksEachArtifactIndependently$' -count=1
```

The test forces each possible half-removal, reoccupies the successfully removed
path, and exercises both resync retry and proxy shutdown.

## Behavior before the fix

Retry and shutdown remove the new occupant because the remaining endpoint
record still claims both pathnames.

## Expected behavior

Every pathname is checked against the socket identity originally published by
the proxy. A missing or identity-mismatched path is relinquished independently
of its sibling.

## Impact

A single teardown error can turn an idempotent resync retry into deletion of a
foreign filesystem object.

## Acceptance criteria

- Both partial-removal orders are covered.
- Retry and shutdown preserve a reoccupied artifact.
- The sibling still owned by the proxy is removed.
- Direct and shortened publication use the same ownership abstraction.

## Regression coverage

`TestShortenedTeardownTracksEachArtifactIndependently` and
`TestCrashCleanupPreservesEveryReoccupiedSocketArtifact`
