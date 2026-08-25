# Apply resume does not revalidate resources from completed phases

| Field | Value |
| --- | --- |
| Status | Fixed |
| Severity | High |
| Area | Lifecycle / apply resume / phase prerequisites |
| Reproducibility | Deterministic |

## Summary

Once apply advanced beyond `networks` or `create`, resume trusted the resource
IDs recorded by those completed phases. If a target egress network or target
container disappeared afterward, later phases repeatedly used the stale ID.
At `commit`, apply could even publish desired state containing the absent
resource.

## Reproduction

Run the phase-boundary matrices:

```text
go test ./lifecycle -run '^TestApplyResumeRepairsTarget(NetworkLostAfterNetworksPhase|ContainerLostAfterCreatePhase)$' -count=1
```

The network matrix removes the recorded network at `resync`, `create`,
`attach`, `start`, and `commit`. The container matrix removes a recorded target
container at `attach`, `start`, and `commit`.

## Behavior before the fix

At `resync` and `create`, container creation repeatedly failed against the
missing network ID. At `attach` and `start`, inspection or connection failed
against a stale resource. At `commit`, apply succeeded and wrote a deployment
whose network or container did not exist.

## Expected behavior

Before consuming facts produced by an earlier phase, apply re-inspects their
exact recorded identities. A missing network durably rewinds to `networks`; any
container configured for that network generation is safely removed first. A
missing container durably rewinds to `create`. Unrelated containers and the
healthy proxy are retained.

## Impact

A Docker daemon restart, operator action, or external cleanup during apply can
leave the operation permanently retrying stale IDs or can turn incomplete
runtime state into committed desired state. Manual state surgery is otherwise
required to recover.

## Acceptance criteria

- Every phase after `networks` repairs a missing target network.
- Every phase after `create` repairs a missing target container.
- Only a container dependent on the missing network is recreated.
- Unrelated container IDs and the healthy proxy identity remain unchanged.
- Orphan endpoint cleanup is journaled and resumes after interruption.
- Foreign attachments are refused before any stop or remove call.
- A different network at the recorded deterministic name is refused before
  any stop or remove call.
- The converged deployment passes full observed-state matching.

## Regression coverage

- `TestApplyResumeRepairsTargetNetworkLostAfterNetworksPhase`
- `TestApplyResumeRepairsTargetContainerLostAfterCreatePhase`
- `TestApplyResumeJournalsOrphanCleanupForMissingTargetContainer`
- `TestApplyNetworkRecoveryRefusesForeignContainerAttachmentBeforeMutation`
- `TestApplyNetworkRecoveryRefusesReplacementAtRecordedNameBeforeMutation`
