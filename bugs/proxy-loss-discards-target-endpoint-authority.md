# Proxy-loss fallback discards target endpoint cleanup authority

| Field | Value |
| --- | --- |
| Status | Fixed |
| Severity | High |
| Area | Lifecycle / missing-proxy fallback / endpoint cleanup |
| Reproducibility | Deterministic |

## Summary

If a started target egress container disappeared while leaving an orphaned
Docker network endpoint and the proxy also died, missing-proxy fallback removed
the container's durable resource record without first reconciling that endpoint.

## Reproduction

Run:

```text
go test ./lifecycle -run '^(TestApplyProxyLossFallback|TestSupersedingApplyRecoversOrphanedTargetBeforeProxyFallback)' -count=1
```

Coverage includes the `start` and `commit` journal boundaries, an interrupted
force-disconnect after cleanup intent is durable, retry, and a different `up`
superseding the interrupted target.

## Behavior before the fix

Resume failed with:

```text
network component/consumer has undeclared member ep-endpoint-1
```

The superseding apply instead reached abort and failed because the same orphan
still occupied the target network. The cleanup-interruption case never reached
the injected force-disconnect failure and wrote no endpoint-cleanup record.

`removeContainersWithStaleProxyMounts` looked up each deterministic container
name. When the name was absent it skipped candidate construction, but its final
loop still deleted the matching entry from `operation.Containers`. The later
missing-target reconciliation therefore no longer had the exact container ID
needed to derive safe endpoint-cleanup authority.

## Expected behavior

Missing-proxy fallback routes every missing recorded target container through
the ordinary target-container removal boundary. Any orphaned endpoint is
durably recorded and proven absent before the container resource is discarded.
A retained previous-generation container uses its previous network record as
the cleanup authority.

## Impact

A valid apply journal could not resume or be superseded after the conjunction
of proxy loss and an orphaned target endpoint. Manual Docker network cleanup or
an explicit abort was otherwise required.

## Acceptance criteria

- Resume converges from both started `start` state and `commit` state.
- An interrupted force-disconnect leaves one exact durable cleanup record.
- A second resume completes cleanup and deployment.
- Superseding `up` cleans the stale target before aborting it.
- The retained egress network is not unnecessarily recreated during resume.
- No lifecycle state field or fallback branch is added.

## Resolution

Missing-proxy fallback now delegates a missing target record to
`removeTargetContainerForRecreate`. Its absent-container path shares one helper
that records and completes endpoint cleanup before deleting resource authority.
When the target record is a retained previous container, cleanup uses the
previous deployment's component and network generation.

## Regression coverage

- `TestApplyProxyLossFallbackRecoversOrphanedTargetContainer`
- `TestApplyProxyLossFallbackPersistsOrphanCleanupBeforeDiscardingTarget`
- `TestSupersedingApplyRecoversOrphanedTargetBeforeProxyFallback`
