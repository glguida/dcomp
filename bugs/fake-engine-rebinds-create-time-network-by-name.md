# Fake engine rebinds an unstarted container to a same-name network

| Field | Value |
| --- | --- |
| Status | Fixed |
| Severity | Medium |
| Area | Tests / fake Docker engine / network identity |
| Reproducibility | Deterministic |

## Summary

The lifecycle fake represented an unstarted container's network only by the
name visible in inspect output. At first start it resolved that name through
the fake's current network-name map. If the original network had disappeared
and a new generation reused the deterministic name, the fake silently attached
the container to the replacement.

## Reproduction

Run:

```text
go test ./lifecycle -run '^TestFakeEnginePreservesUnstartedContainerNetworkGeneration$' -count=1
```

The test creates a network and an unstarted container configured with its exact
ID, removes the network, creates another network with the same name, and starts
the container.

## Behavior before the fix

`StartContainer` returned success and treated the same-name replacement as the
container's configured network. This masked lifecycle bugs involving stale
network IDs and could make an invalid recovery path look converged.

## Expected behavior

The fake stores create/connect-time network configuration separately from the
possibly incomplete inspect representation. Start validates and attaches the
exact configured IDs. If one is absent, start returns `engine.ErrNotFound` and
does not mutate the container or replacement network.

## Impact

Tests could pass even though real Docker retained a different network
generation in the container configuration. In particular, same-name network
replacement tests did not reliably exercise the lifecycle identity boundary.

## Acceptance criteria

- Create and connect pin exact network IDs independently of inspect output.
- Start never resolves an empty observed ID through the current name map.
- A same-name replacement remains unattached after the failed start.
- Disconnect and removal use the pinned generation rather than a current name
  lookup.

## Regression coverage

`TestFakeEnginePreservesUnstartedContainerNetworkGeneration`
