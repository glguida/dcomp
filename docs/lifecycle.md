# Lifecycle and recovery

## State model

DComp is invoked as a short-lived CLI. Durable state records one committed
deployment and at most one incomplete operation per system. A non-stale kernel
lock serializes mutation; shared locks give observers a coherent generation.

The default state root is `$XDG_STATE_HOME/dcomp` or
`$HOME/.local/state/dcomp`. `DCOMP_STATE_ROOT` and `--state-root` select another
absolute path. The root is bound write-once to Docker's stable Engine ID. By
default, transient proxy state lives in its `run` subdirectory; an explicit
runtime-root option or environment variable may place that tree elsewhere.
The cleaned absolute state-root path also determines a 128-bit Docker
namespace. Every owned container, egress network, and named volume includes
that namespace in both its physical name and ownership labels, so another
state root can control an independently named system on the same Engine.

State format 4, introduced by DComp 0.2.1, records:

- the canonical resolved system and digest;
- the transient runtime root;
- exact immutable Docker egress-network and container IDs;
- the proxy instance ID, PID, wiring digest, runtime directory, control
  socket, and log path;
- operation phase and completed step keys;
- for apply operations, the complete target wiring and its
  process-independent digest;
- endpoint-cleanup records containing the exact container, network, endpoint
  name, and immutable endpoint ID; and
- create requests whose result may have been lost.

Earlier state formats and engine bindings are rejected. DComp does not infer
new digest or wiring semantics from an older record.

Named volumes have deterministic names rather than immutable IDs. Their
driver and complete DComp ownership label set are verified on every use.

## Identity and authority

Human-readable names are discovery aids, not mutation authority. Physical
Docker names have the form `dcomp.<namespace>.<system>.<kind>...`, where the
32-hex-character namespace is the first 128 bits of SHA-256 over the cleaned
absolute state-root path. Before a Docker mutation, DComp inspects the
recorded immutable ID and verifies the expected namespace, name, image,
labels, launch security, mounts, environment, ports, and network attachments.

The proxy is controlled through its Unix control socket. Requests carry the
recorded instance ID; status must return the same process ID and PID. The
wiring digest is mutable observed state rather than process identity.
DComp does not send a fallback signal to a PID unless that live identity was
just verified. This prevents a stale state record from signalling an unrelated
process after PID reuse.

## Apply operation

The 0.2.1 apply phase sequence is:

```text
retire -> networks -> resync -> create -> attach -> start -> commit
```

### Retire

DComp preflights the complete previous deployment and all target names before
the first destructive call. A component is retained when its
container-definition digest is unchanged and the recorded proxy is still
identity-verified. That digest covers image ID, runtime policy, publications,
egress, mounts, and the component's own endpoint set; it deliberately excludes
link targets. Components being removed or recreated are stopped and removed
before socket publication.

Before removing an egress container, DComp inspects its recorded bridge and
durably journals the exact Docker endpoint name and immutable endpoint ID.
Container retirement is not complete until a second network inspection proves
that endpoint absent. If Docker removed the container but stranded the
endpoint, resume first confirms both the recorded container ID and name are
absent, verifies the endpoint and network identities, force-disconnects that
exact endpoint by name, and reinspects the network. An unmatched endpoint is
never removed. This reconciliation runs before strict unknown-member
preflight and can derive cleanup authority when a recorded container
disappeared out of band before DComp could journal its endpoint.

### Networks

DComp creates or recovers one dedicated bridge for each component that
declares `egress`. Components without that declaration have no network
resource and run with Docker network mode `none`. There are no link networks
in 0.2. Each egress bridge contains only its component and is non-internal.

Network create intent is durable before the Docker request. If the response is
lost, resume inspects the deterministic name, requires the current operation
label, and records the returned immutable ID.

### Resync

If no proxy runs, DComp starts one with the complete target wiring as before.
Otherwise it verifies immutable process identity and sends the complete
journaled wiring and digest. The proxy prepares new listeners at temporary
names, atomically publishes additions and swaps the routing table under one
lock, then tears down removed listeners and link-owned connections. It reports
the target digest only after teardown completes. During commit or teardown it
reports `ready=false` and no digest.

An endpoint whose identity survives is never rebound, renamed, or unlinked.
Retained containers therefore keep the socket inode pinned by their bind mount,
and established streams on surviving links remain open. Removed links close
both directions immediately. Pending inputs belong to their captured full link
identity; pending outputs remain in their endpoint pool while any target link
can consume them.

If the parent command loses a proxy start result, resume uses the existing
config, PID file, and identity-checked status to recover the exact process. A
different live proxy in the same runtime directory is never replaced.

Resume dispatches by observed digest. The previous digest is resynced again;
the target digest continues with create; a non-converged or unexpected digest
is retried and then identity-verified shutdown falls back to full replacement.
A dead proxy takes the same full-replacement path. That fallback removes the
complete component fleet, including components without endpoints, before
publishing replacement sockets. Networks are reconciled separately and named
volumes survive.
Process-identity, proxy-format, and control-protocol mismatches are terminal;
they never enter replacement fallback.

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

Container-definition digests cover immutable image ID, the component's own
endpoint definition, and normalized runtime policy. Link targets are excluded.
The proxy has a separate wiring digest over endpoint triples and full link
pairs only.

- An image-only change replaces that component while retaining the proxy and
  unrelated running containers.
- Adding a component publishes its listeners and creates only that container.
- Removing a component retires it, then removes only its endpoints and links.
- Relinking existing endpoints performs zero container operations.
- Changing a component's own endpoint set recreates only that component and
  resyncs the proxy.
- Runtime-root changes likewise replace the proxy and containers.
- Named volumes survive every replacement.

## Resume, supersede, and abort

`dcomp resume NAME` continues the exact stored target and phase. Repeated
steps converge by inspecting ownership and immutable identity before mutation.

`dcomp up FILE` resumes a pending apply only when both the resolved digest and
runtime root match. Otherwise it first resolves pending creates and safely
aborts the stale target before applying the new one.

`dcomp abort NAME` removes operation-owned target containers first, then
reverse-resyncs the retained proxy to the previous committed wiring. It remains
not-rollback in the general case because resources retired earlier may already
be gone, but a successful reverse resync approximates rollback. If reverse
resync transiently cannot converge, abort takes the full-replacement path and
recreates the previous fleet. Identity or protocol mismatches are terminal.
Abort refuses to proceed while any create result remains unresolved.

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

Proxy cleanup removes readiness and only endpoint/control socket objects proven
owned by its internal ledger. It removes the PID completion marker after the
ledger is empty; manager cleanup then removes config, log, and empty runtime
directories. A pathname relinquished by resync is never reclaimed from wiring
config or directory membership; if another owner reuses it, that object and
any non-empty containing directories remain. Unexpected non-socket entries are
not silently deleted.

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
2. Different state roots use different Docker resource namespaces.
3. Every mutation follows exact identity and ownership verification.
4. A component has no Docker network unless it declares `egress`; then it has
   exactly one dedicated, externally routed bridge.
5. No application link creates a Docker network.
6. The proxy is ready before a component is created or started.
7. A container sees only its own interface socket files.
8. Components connect to interface sockets; only the proxy binds/listens.
9. Components stop before the proxy during `down`.
10. Persistent named volumes are never deleted implicitly.
11. Ambiguous create results remain durable until resolved.
12. Egress-container retirement is complete only after its recorded endpoint
    is proven absent.
13. Within one system runtime directory, a retained endpoint identity keeps the
    same socket inode across every resync.
14. At each endpoint publication, no existing container mounts that host path.
