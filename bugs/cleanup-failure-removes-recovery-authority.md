# Cleanup failure removes recovery authority

| Field | Value |
| --- | --- |
| Status | Fixed |
| Severity | Medium |
| Area | Proxy / crash recovery / runtime cleanup |
| Reproducibility | Deterministic |

## Summary

Runtime cleanup removes `proxy.json` and `proxy.pid` before validating endpoint
directories and performing later failable work. If a later step fails, the
runtime remains partially populated but the files needed to identify and
resume cleanup are already gone.

## Reproduction

Run:

```text
go test ./proxy -run '^TestCrashCleanupFailurePreservesLedgerAndProcessAuthority$' -count=1
```

The test makes removal of a ledger-owned shortened artifact fail, invokes
dead-process cleanup, and checks the ledger, configuration, and PID completion
marker after the expected failure.

## Behavior before the fix

Cleanup reports the invalid entry only after deleting both recovery-authority
files.

## Expected behavior

Cleanup validates its complete input before mutation. During execution it
removes recovery authority only after all socket ownership claims have been
resolved successfully.

## Impact

A retry can no longer prove which proxy owned the remaining artifacts, and a
subsequent ensure may misclassify an incomplete runtime as fresh.

## Acceptance criteria

- The complete ownership ledger is validated before removal starts; unclaimed
  runtime entries are preserved rather than treated as cleanup authority.
- Any socket cleanup failure leaves proxy identity and completion authority.
- The PID marker disappears only after every owned pathname is gone or proven
  relinquished.

## Regression coverage

`TestCrashCleanupFailurePreservesLedgerAndProcessAuthority` and
`TestCrashCleanupCoversEveryLedgerPublicationState`
