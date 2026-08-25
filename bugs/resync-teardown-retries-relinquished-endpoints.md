# Resync teardown retries already relinquished endpoints

| Field | Value |
| --- | --- |
| Status | Fixed |
| Severity | Medium |
| Area | Proxy / resync teardown / retry bookkeeping |
| Reproducibility | Deterministic |

## Summary

When one removed endpoint failed during resync teardown, the transition kept
all removed endpoints in its retry list. A pathname successfully removed and
disowned earlier in the same attempt could be legitimately reoccupied before
retry; retry then unlinked the new owner's path.

## Reproduction

Run:

```text
go test ./proxy -run '^TestResyncTeardownRetryPreservesEverySuccessfullyRelinquishedPath$' -count=1
```

The test removes three endpoints, injects failure at each teardown position,
reoccupies every successfully relinquished direct or shortened publication,
and retries the same resync.

## Behavior before the fix

Every direct and shortened-path case converged but removed at least one foreign
occupant created after the first attempt.

## Expected behavior

Teardown tracks only endpoints whose publication removal remains incomplete.
Successful per-endpoint work leaves the retry set immediately. Detached
connections are likewise closed once rather than replayed on every attempt.

## Impact

A transient teardown failure could make retry unlink a socket or filesystem
entry that the proxy had already relinquished, violating pathname ownership.

## Acceptance criteria

- Failure at every endpoint position remains retryable.
- Direct and shortened publication implementations behave identically.
- Every successfully relinquished final and actual path survives retry.
- The proxy reports no digest until the remaining teardown completes.

## Regression coverage

`TestResyncTeardownRetryPreservesEverySuccessfullyRelinquishedPath`
