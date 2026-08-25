# Retained container pins a missing network generation

| Field | Value |
| --- | --- |
| Status | Fixed |
| Severity | High |
| Area | Lifecycle / retention / network recovery |
| Reproducibility | Deterministic |

## Summary

Apply selected an unchanged external-egress container independently from its
network. If the recorded network generation was missing, apply recreated the
network under the same deterministic name but retained a container whose
create-time configuration still pinned the old network ID.

## Reproduction

Run the phase matrix:

```text
go test ./lifecycle -run '^TestApplyRecreatesContainersPinnedToMissingNetworkGenerationAtEveryPhase$' -count=1
```

The matrix removes the network before retention selection and after selection
at every durable apply phase from `retire` through `commit`.

## Behavior before the fix

Loss before selection still retained the dependent container. Loss at
`retire` or `networks` recreated the network and later failed with an exact-ID
attachment error:

```text
attach consumer to network network-2: container container-1 is already attached to network-2
```

Later phases happened to work because their prerequisite-recovery path already
invalidated containers tied to a missing network.

## Expected behavior

Container retention is dependency-closed. An external-egress container is
retained only with its exact recorded network generation. If that generation
disappears after selection, the dependent container is safely removed before
the network is recreated. Unrelated containers and the healthy proxy remain.

## Impact

A same-spec apply could become permanently retryable after Docker or an
operator removed an egress network. The faithful fake Docker engine exposed
the production identity error that its earlier name-based model had hidden.

## Acceptance criteria

- The invariant holds before selection and at every durable apply phase.
- The dependent container and missing network receive new IDs.
- Unrelated container IDs and proxy process identity are retained.
- A different occupant at the deterministic network name is refused before
  container mutation.
- The resulting deployment passes observed-state matching.

## Regression coverage

`TestApplyRecreatesContainersPinnedToMissingNetworkGenerationAtEveryPhase`
