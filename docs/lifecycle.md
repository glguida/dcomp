# Lifecycle and recovery

## State model

DComp is invoked as a short-lived CLI. Durable state records one committed
deployment and at most one incomplete operation per system. A non-stale kernel
lock serializes mutation; shared locks give observers a coherent generation.

The default state root is `$XDG_STATE_HOME/dcomp` or
`$HOME/.local/state/dcomp`. `DCOMP_STATE_ROOT` and `--state-root` select another
absolute path. The root is bound write-once to Docker's stable Engine ID.

State format 3, introduced by DComp 0.2, records:

- the canonical resolved system and digest;
- the transient runtime root;
- exact immutable Docker egress-network and container IDs;
- the proxy instance ID, PID, wiring digest, runtime directory, control
  socket, and log path;
- operation phase and completed step keys;
- endpoint-cleanup records containing the exact container, network, endpoint
  name, and immutable endpoint ID; and
- create requests whose result may have been lost.

Named volumes have deterministic names rather than immutable IDs. Their
driver and complete DComp ownership label set are verified on every use.

## Identity and authority

Human-readable names are discovery aids, not mutation authority. Before a
Docker mutation, DComp inspects the recorded immutable ID and verifies the
expected name, image, labels, launch security, mounts, environment, ports, and
network attachments.

The proxy is controlled through its Unix control socket. Requests carry the
recorded instance ID; status must return the same ID, wiring digest, and PID.
DComp does not send a fallback signal to a PID unless that live identity was
just verified. This prevents a stale state record from signalling an unrelated
process after PID reuse.

## Apply operation

The 0.2 apply phase sequence is:

```text
retire -> networks -> proxy -> create -> attach -> start -> commit
```

### Retire

DComp preflights the complete previous deployment and all target names before
the first destructive call. Components whose immutable component digest and
socket mounts remain valid may be retained.

The target proxy digest covers runtime directory, endpoints, and links. If it
matches the running recorded proxy, that process can be retained. If wiring
changes or the proxy is absent, no component container is retained: a Docker
bind mount points to a particular socket inode and cannot follow a pathname to
a newly created listener. DComp retires those containers, then stops the old
proxy.

Before removing an egress container, DComp inspects its recorded bridge and
durably journals the exact Docker endpoint name and immutable endpoint ID.
Container retirement is not complete until a second network inspection proves
that endpoint absent. If Docker removed the container but stranded the
endpoint, resume first confirms both the recorded container ID and name are
absent, verifies the endpoint and network identities, force-disconnects that
exact endpoint by name, and reinspects the network. An unmatched endpoint is
never removed. This reconciliation runs before strict unknown-member
preflight and can derive the journal entry for an interrupted state-format-3
operation that predates this fix.

### Networks

DComp creates or recovers one dedicated bridge for each component that
declares `egress`. Components without that declaration have no network
resource and run with Docker network mode `none`. There are no link networks
in 0.2. Each egress bridge contains only its component and is non-internal.

Network create intent is durable before the Docker request. If the response is
lost, resume inspects the deterministic name, requires the current operation
label, and records the returned immutable ID.

### Proxy

DComp derives strict JSON wiring and durably marks the proxy create pending.
The process creates all endpoint and control listeners, writes its PID and
readiness files, and reports readiness through an inherited descriptor.

If the parent command loses the start result, resume uses the existing config,
PID file, and identity-checked control status to recover the exact process. A
different live proxy in the same runtime directory is never replaced.

If that proxy later disappears before commit, resume first removes the target
containers so no bind mount can retain one of its old socket inodes, returns
the operation to this phase, and recreates the proxy and containers.
Superseding the interrupted target performs the same cleanup before abort.

### Create

After proxy readiness, DComp creates component containers. Each request has:

- its immutable image ID and ownership labels;
- either network mode `none` or one dedicated egress network;
- only its generated endpoint socket bind mounts and declared user mounts;
- `DCOMP_IN_*`, `DCOMP_OUT_*`, and `DCOMP_COMPONENT_NAME`;
- normalized args and published ports; and
- the fixed container security policy.

Every image-declared OCI volume target must have an explicit bind or named
volume, preventing anonymous volume creation.

### Attach

The attachment phase reconciles only declared egress networks and aliases. It
also verifies that components without egress have no attachments, and rejects
foreign attachments before mutation. This phase remains separate so a lost
Docker connect/disconnect result can be retried from observed facts.

### Start and commit

DComp verifies proxy readiness, mounts, environment, and network policy before
starting each new container. Components are all created before the first
start, so cycles do not impose startup ordering.

An unhealthy or exited component is committed as an inspectable system state;
health is not a transaction rollback signal. Commit writes the exact Docker
and proxy identities to `desired.json`, then clears the operation.

## Incremental changes

Component digests cover immutable image ID, endpoint definition, normalized
runtime policy, and inbound target identity. The proxy has a separate wiring
digest.

- An image-only change replaces that component while retaining the proxy and
  unrelated running containers.
- A link or endpoint change replaces the proxy and all socket-mounted
  containers.
- Runtime-root changes likewise replace the proxy and containers.
- Named volumes survive every replacement.

Dynamic rewiring without component restart is intentionally not implemented.

## Resume, supersede, and abort

`dcomp resume NAME` continues the exact stored target and phase. Repeated
steps converge by inspecting ownership and immutable identity before mutation.

`dcomp up FILE` resumes a pending apply only when both the resolved digest and
runtime root match. Otherwise it first resolves pending creates and safely
aborts the stale target before applying the new one.

`dcomp abort NAME` is not rollback. For an interrupted apply it removes only
operation-owned target containers, proxy, and networks that were not part of
the previous committed deployment, then republishes the previous state record.
If earlier phases already retired previous resources, a later `up` repairs
them. Abort refuses to proceed while any create result remains unresolved.

Non-apply operations can be aborted by clearing their durable operation after
the command has established that no create is pending.

## Down

`down` first preflights the committed deployment. It then:

1. stops and removes every exact component container;
2. sends identity-checked shutdown to the proxy and waits for exit;
3. removes transient component egress networks;
4. clears desired and operation state; and
5. preserves named volumes and the state-root Engine binding.

Container removal during `down` and abort uses the same durable endpoint
transaction as apply retirement.

Proxy cleanup removes endpoint/control sockets, PID, readiness, config, log,
and the system runtime directory. Unexpected non-socket entries are not
silently deleted.

## Restart

`restart NAME [COMPONENT...]` records selected immutable container IDs and
uses Docker's single-container restart operation. The proxy and unselected
components are untouched. A lost restart response is retried as the same
convergent Docker operation.

The component must reconnect its input/output streams after restart. The proxy
accepts new producer and consumer connections without rewiring.

## Observation

`status` verifies the state-root Engine binding, proxy identity/readiness,
every recorded egress network, and every component. It reports missing or
degraded resources but never repairs them. During an operation it separately
reports previous components and networks that still await retirement. A
healthy system with no egress networks is operational.

`ps` builds on coherent status snapshots. `logs` reads verified Docker logs in
parallel and the recorded proxy log as `@proxy`; `-f` follows all selected
streams until cancellation. `attach` holds a shared system lock and a
component-specific attachment lock, verifies the proxy and container, then
attaches standard I/O.

## Invariants

1. A state root controls resources on one Docker Engine only.
2. Every mutation follows exact identity and ownership verification.
3. A component has no Docker network unless it declares `egress`; then it has
   exactly one dedicated, externally routed bridge.
4. No application link creates a Docker network.
5. The proxy is ready before a component is created or started.
6. A container sees only its own interface socket files.
7. Components connect to interface sockets; only the proxy binds/listens.
8. Components stop before the proxy during `down`.
9. Persistent named volumes are never deleted implicitly.
10. Ambiguous create results remain durable until resolved.
11. Egress-container retirement is complete only after its recorded endpoint
    is proven absent.
