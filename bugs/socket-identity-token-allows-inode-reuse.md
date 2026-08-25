# Socket identity token allows inode reuse

| Field | Value |
| --- | --- |
| Status | Fixed |
| Severity | High |
| Area | Proxy / socket ownership / recovery |
| Reproducibility | Deterministic |

## Summary

An initial ownership-ledger implementation identified sockets with only the
filesystem device and inode numbers recorded before publication. After an
owned path was unlinked, Linux could immediately reuse that inode for a new
socket at the same path. Cleanup then mistook the new socket for the old one.

## Reproduction

Run:

```text
go test ./proxy -run '^TestCrashCleanupPreservesEveryReoccupiedSocketArtifact$' -count=1
```

The direct-path case unlinks a published socket, immediately binds a new one at
the same path, and invokes dead-process cleanup. It deterministically exercised
inode reuse in the test filesystem.

## Behavior before the fix

The replacement received the same device/inode pair and crash cleanup removed
it even though the proxy had relinquished the original pathname.

## Expected behavior

Filesystem identity must remain unambiguous until every public path has been
removed or proven reused. A replacement object must never compare equal merely
because the filesystem recycled an inode number.

## Impact

The proposed structural fix would still have deleted foreign live sockets in
exactly the recovery path it was intended to make safe.

## Acceptance criteria

- Direct and both shortened-path half-removal cases preserve replacements.
- Identity remains stable across hard-link publication and teardown retries.
- The mechanism does not depend on inode numbers remaining globally unique.

## Resolution

Each ledger record now owns a private hard-link anchor. The anchor keeps the
original inode allocated while public names are compared and removed. The
record durably enters `released` before the anchor is removed, so later retries
never inspect public paths again.

## Regression coverage

`TestCrashCleanupPreservesEveryReoccupiedSocketArtifact`
