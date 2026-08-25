# Absent status example claims an empty proxy digest

| Field | Value |
| --- | --- |
| Status | Fixed |
| Severity | Low |
| Area | Machine API / documentation |
| Reproducibility | Deterministic |

## Summary

The machine API correctly omits `proxy.digest` when no converged proxy digest
is observed, but the documented absent-system example included
`"digest": ""` inside the proxy object.

## Reproduction

Run:

```text
go test ./cmd/dcomp -run '^TestAbsentStatusDocumentationMatchesMachineJSON$' -count=1
```

The test decodes the documented absent-system JSON and the actual serialized
machine document and compares their structures.

## Behavior before the fix

The comparison found one extra documented field: `proxy.digest` with an empty
string value.

## Expected behavior

The example matches the machine output exactly. Digest absence means unknown or
not converged and is represented by field omission, not an empty digest claim.

## Impact

Clients generated or tested against the example could treat an absent digest
differently from the actual API response.

## Acceptance criteria

- The absent-system example omits `proxy.digest`.
- Converged status still emits the observed wiring digest.
- The documentation/example comparison remains in the Go test suite.

## Regression coverage

`TestAbsentStatusDocumentationMatchesMachineJSON`
