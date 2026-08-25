# Retained missing container skips orphan endpoint cleanup

| Field | Value |
| --- | --- |
| Status | Fixed |
| Severity | High |
| Area | Lifecycle / apply recovery / endpoint cleanup |
| Reproducibility | Deterministic |

## Summary

Endpoint recovery skipped every previous container selected for retention.
If such a container disappeared out of band and Docker left an `ep-*` network
member, strict network preflight observed the orphan before any code revoked
the stale retention decision.

A second path in container creation deleted the missing resource record
without invoking the durable endpoint-cleanup mechanism.

## Reproduction

Run:

```text
go test ./lifecycle -run 'TestApply(RecoversOrphanFromRetainedContainerAtEveryPhase|RetainedContainerOrphanCleanupIsDurableAcrossInterruption|RecoversRetainedContainerOrphanedDuringResync)$' -count=1
```

Coverage includes disappearance after the operation journal at every durable
apply phase, an interrupted forced endpoint cleanup, and disappearance during
proxy resync after the initial recovery observation.

## Behavior before the fix

Resume failed with:

```text
network component/consumer has undeclared member ep-endpoint-1
```

The cleanup-interruption case never reached the cleanup call, so no durable
`EndpointCleanups` record was written.

## Expected behavior

Retention remains a plan rather than proof of continued existence. Previous
external-egress containers are observed before strict member validation, and
every missing target-container path routes through the same durable endpoint
recovery before its resource record is removed.

## Impact

An operator removal or Docker failure during apply could leave a valid journal
that could not resume or abort without manual network cleanup.

## Acceptance criteria

- Orphans are recovered from every durable apply phase.
- A cleanup failure leaves an exact durable cleanup record and retry converges.
- Disappearance during resync is handled by the create phase.
- The egress network, unrelated containers, and healthy proxy are retained.
- The resulting deployment passes observed-state matching.

## Regression coverage

- `TestApplyRecoversOrphanFromRetainedContainerAtEveryPhase`
- `TestApplyRetainedContainerOrphanCleanupIsDurableAcrossInterruption`
- `TestApplyRecoversRetainedContainerOrphanedDuringResync`
