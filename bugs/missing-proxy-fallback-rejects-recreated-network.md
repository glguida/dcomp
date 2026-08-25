# Missing-proxy fallback rejects a valid recreated network attachment

| Field | Value |
| --- | --- |
| Status | Fixed |
| Severity | High |
| Area | Lifecycle / apply resume / missing-proxy fallback |
| Reproducibility | Deterministic |

## Summary

Apply resume can reject a valid replacement container when its external-egress
network was recreated under the same deterministic Docker name and the
container has not started yet. Docker reports the configured network name but
can leave its network ID empty before first start.

The fallback verifier combines target and previous network records before
matching the attachment. The two records have the same name and different IDs,
so an empty-ID attachment appears ambiguous even though the container's exact
record identifies which deployment generation applies.

## Reproduction

Run the regression matrix:

```text
go test ./lifecycle -run '^TestResumeAfterProxyLossAcceptsUnstartedContainerOnRecreatedNetwork$' -count=1
```

Each subtest performs the following sequence at the `create`, `attach`, and
`start` phase boundaries:

1. Apply a system whose consumer uses an external-egress network.
2. Remove that network out of band.
3. Change the consumer image so the apply recreates both the network and the
   container.
4. Verify that the replacement network has the old deterministic name and a
   new ID.
5. Verify that the unstarted replacement container reports that network name
   with an empty ID.
6. Kill the recorded proxy and resume the operation.

## Behavior before the fix

All three resume cases failed with:

```text
consumer is attached to undeclared network dcomp.<scope>.demo.component.consumer
```

`removeContainersWithStaleProxyMounts` indexed the target and previous network
records together. `matchContainerNetwork` correctly refuses to choose between
two same-name records when Docker supplies no ID, but the caller had discarded
the already-known fact that the container belongs to the target generation.

The operation remained journaled and could not reach the documented
dead-proxy full-replacement recovery path.

## Expected behavior

Network verification first matches an empty-ID attachment against the exact
container candidate's deployment generation. Only attachments not found there
are matched against the union of target and previous records. Attachments
outside both authorities are still refused before mutation.

## Impact

A proxy failure after container creation but before first start can make apply
resume unavailable after a same-name network replacement. Recovery fails
safely, but the journal blocks subsequent lifecycle operations until manual
intervention.

Normal abort is not affected by this exact ordering because it removes
target-only containers before restoring or replacing the previous proxy.

## Acceptance criteria

- Resume converges from the `create`, `attach`, and `start` boundaries.
- The stale socket-mounted replacement container is removed and recreated
  against the replacement proxy.
- The valid target network is retained during apply fallback.
- Abort from the same three boundaries still restores the previous deployment.
- A foreign network attachment is rejected before any engine mutation.
- No new journal fields or lifecycle branches are introduced.

## Regression coverage

- `TestResumeAfterProxyLossAcceptsUnstartedContainerOnRecreatedNetwork`
- `TestAbortAfterProxyLossAcceptsUnstartedContainerOnRecreatedNetwork`
- `TestProxyLossFallbackRejectsForeignNetworkBeforeMutation`
