# Lifecycle

## Objective

DComp applies independently built components without turning Docker calls into
an imaginary transaction. It records intent before mutation, verifies observed
Docker facts, and can continue after the CLI, host, or Docker request is
interrupted.

An apply is incremental. A change to one component does not imply replacing its
siblings, and adding a consumer does not restart an unchanged provider.

## Persistent state

The state root contains one write-once engine binding and one private
directory per system:

```text
engine.json
systems/<name>/
  lock
  desired.json
  operation.json       # present only while an operation is incomplete
```

`engine.json` binds the complete state root to Docker's stable `/info` engine
ID before the first lifecycle mutation. A different local Docker socket may
not reuse or silently rebind that state. Observational commands reject recorded
state when the connected engine ID differs.

`desired.json` records the last committed resolved specification together with
the exact immutable container and network IDs.

`operation.json` records:

- a random operation ID and operation kind;
- the complete immutable target;
- the previous committed deployment, when one exists;
- the current durable phase;
- exact container and network IDs acquired so far;
- create requests that may have reached Docker but do not yet have a recorded
  result;
- the selected components for a targeted restart; and
- completed per-resource steps.

There is one committed deployment and at most one incomplete operation per
system. Each mutating command holds an exclusive, non-blocking `flock`.
`status` holds a shared lock for its complete snapshot, so it observes one
lifecycle generation. State files are atomically replaced and the files and
containing directories are synchronized before Docker mutation proceeds.

Persistent named volumes are different from deployment resources. Docker does
not expose immutable IDs for them, so DComp uses deterministic names and
verifies ownership labels and the local driver before mounting. Their contents
are never part of a digest, and DComp never deletes them implicitly.

## Identity and retention

Names are discovery keys and Docker DNS aliases, not destructive-mutation
authority. Container and network mutations use full immutable IDs after
verifying names, ownership labels, expected digests, and the fixed container
policy (`init`, restart `no`, `no-new-privileges`, dropped `NET_RAW`, and a
2048-process PIDs limit).

The resolved system has both a complete-system digest and one digest per
component. A component digest covers:

- its immutable image ID;
- its declared protobuf interfaces;
- its normalized binds, volumes, arguments, published ports, and egress policy;
  and
- the target component of each inbound link.

Outgoing links do not affect a provider's component digest. On `up`, an
existing running container with the expected component digest and immutable
runtime configuration is retained. Network attachments are reconciled
separately, so a retained provider can join a new consumer's link network while
remaining running.

## `up`

`up FILE` first parses and validates all descriptors and runtime directives,
resolves image references to immutable local image IDs, requires Docker health
checks, and verifies that image-declared `VOLUME` targets have explicit mounts.

If the complete resolved target is already applied with all recorded
containers still running, `up` is a no-op.
Otherwise it records an apply operation and advances through durable phases:

```text
retire -> networks -> create -> attach -> start -> commit
```

The phases:

1. stop and remove only removed or changed component containers;
2. create or recover the required private component and link networks;
3. verify or create declared persistent volumes, then create only new or
   changed containers;
4. connect retained and new containers to exactly their planned networks and
   disconnect obsolete attachments, then remove empty obsolete networks;
5. start new containers; and
6. atomically commit the new deployment and clear the operation.

Docker health is then an observed per-component result. An unhealthy or exited
component remains committed and visible rather than holding an unrelated
system-wide transaction open.

Every component has a one-member base bridge. It is internal unless that
component declares `egress`. Every direct link has a separate internal bridge
whose only members are its provider and consumer. Link changes therefore do
not expose unrelated components to one another.

Cycles remain valid. Links describe communication capabilities, not a global
provider-before-consumer lifecycle order.

## `restart`

`restart NAME COMPONENT...` records a targeted restart of the selected
committed container IDs. It applies Docker's single restart operation to those
exact immutable IDs and leaves unselected components running. Their resulting
health is reported by `status`.

With no component names, restart selects every component. Restart does not
resolve mutable image tags, recreate containers, change mounts or ports, or
reconcile a new system file; use `up FILE` for configuration changes.

If Docker applies a restart but its response is lost, `resume` repeats that
same restart operation. This can restart a selected component more than once,
but every retry has the same final condition: the exact container is running.
DComp never guesses between separate stop and start operations.

## `down`

`down NAME` records removal, gracefully stops and removes every exact verified
component container, removes the transient base and link networks after they
are empty, clears desired state, and clears the operation.

Declared persistent volumes survive `down`. A later `up` verifies and reuses
them.

## Resume and abort

While `operation.json` exists, mutating commands other than `resume` and
`abort` refuse to run.

`resume NAME` repeats the recorded phase from current Docker inspection:

- a verified object that already satisfies a step is retained;
- a definitively absent required object may be created;
- an incomplete network attachment may be retried after inspection;
- a deterministic name is recovered only when its operation and ownership
  labels match; and
- an unknown or contradictory observation preserves the operation and stops.

`abort NAME` is cleanup-to-a-safe-absent-target, not rollback. For an
interrupted incremental apply it removes verified target objects that were not
part of the previous committed deployment. It does not restart a previous
component that the apply had already retired; a later `up` reconciles the
desired system. Once cleanup succeeds, abort restores the previous committed
`desired.json`, or clears desired state when the interrupted apply had no
previous deployment. Persistent volumes remain. Abort refuses without changing
Docker or operation state while any create result is unresolved; run
`resume NAME` first.

## Ambiguous Docker results

A deadline or connection reset does not establish whether Docker applied an
effect. DComp preserves the operation instead of issuing a speculative inverse
mutation.

Before sending a container or network create request, DComp durably records a
pending-create marker. The marker is cleared only in the same state update that
records the verified object. `resume` is the only command that resolves such a
marker: it inspects a response-lost object or issues the same deterministic-name
create, then records the verified result. `abort` refuses while any marker
remains, so cleanup never doubles as an unresolved forward mutation.

Network connect and disconnect are recovered by inspecting the exact recorded
network name and, once Docker exposes it, its immutable ID. A same-named
foreign or differently owned object stops recovery and is never modified.

A daemon-confirmed start rejection, such as a host bind conflict, also leaves
the apply explicit. Correct the external condition and run `resume`, or run
`abort`. This differs from a process that starts and then exits: that component
is committed as a stable failed state so its status and logs remain available.

## Failed components and observation

A component that exits or becomes unhealthy remains available to `status` and
`logs`; DComp does not delete failure evidence automatically. Unchanged
retained components remain separately identifiable even when another component
fails during an incremental apply.

`status` and `logs` are observational. They verify recorded component and
network identity but never repair, start, stop, connect, or remove resources.
Each unterminated Docker log record is bounded to one MiB before it is emitted,
so a component cannot grow host-side line assembly without limit.

## Invariants

1. At most one mutating operation exists per system.
2. A state root is bound write-once to one Docker engine ID.
3. Intent, including every possibly in-flight create, is durable before its
   Docker mutation.
4. Abort refuses without mutation while any create result remains unresolved.
5. Destructive mutations use verified immutable IDs.
6. Foreign objects are never adopted or changed.
7. Ambiguous results never trigger speculative cleanup.
8. Every component has a private base network; only `egress` makes it external.
9. Every direct link has a private internal network containing only its
   declared provider and consumer.
10. Unchanged component containers retain their IDs across incremental apply.
11. Persistent volumes are never removed by apply, abort, restart, or down.
12. Completion is committed after target resource identity and attachment have
    been verified; component health remains independently observable.
