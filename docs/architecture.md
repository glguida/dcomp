# Architecture

## Scope

DComp runs independently built Docker components on one Linux host. The CLI is
a short-lived controller; each running system has one long-lived
`dcomp-proxy` data-plane process. There is no global daemon, scheduler, or
multi-host control plane. Each system may declare its own namespace of typed
global output interfaces.

Downstream projects own their interface definitions, component images,
`component.dcomp` files, and composition through `system.dcomp` or incremental
CLI/API calls. DComp owns Docker resource lifecycle and every declared
interface socket.

## Static description and resolution

A component declares an existing image and named input/output endpoints:

```text
docker example/filter:1
input example.document.v1.Documents documents
output example.document.v1.Documents filtered
```

A system creates instances and explicit links:

```text
system example
component source components/source
component filter components/filter
link filter.documents source.documents
```

Every input has zero or one link. Outputs may fan out; cycles are valid. The
two endpoint service identifiers must match. DComp treats the resulting byte
streams as opaque and does not load schemas.

Before mutation DComp validates descriptors and runtime policy, resolves every
image reference to an immutable ID, checks the image health-check declaration,
and incorporates endpoint definitions, global assignments, and symbolic or
direct links into stable digests.

## Incremental composition and indirection

`add-component`, `rm-component`, `mod-wire`, and `assign-global` edit a running
system without requiring its complete configuration.
The lifecycle controller merges each edit with committed state under a waiting
exclusive lock, pins existing image IDs, then journals a normal apply.
Independent lifecycle callers therefore cannot overwrite each other through
a stale read/modify/write sequence. Incomplete
operations must be resolved before the next edit.

Global assignments are stored in the authored and resolved compositions and
in proxy wiring. Links retain either a direct endpoint or a global name.
The proxy derives concrete routes from that symbolic configuration. Unbound
globals have no route, so calls fail immediately. Reassignment preserves
unchanged concrete routes and closes changed ones, retaining all surviving
endpoint inodes. Removing the exporting component unbinds its names and
retains symbolic consumers. Global names require no SDK or address changes
beyond the 0.2.2 component contract.

The proxy itself also accepts incremental `mod-wire` and `assign-global`
messages, serialized with resync and shutdown. The lifecycle CLI uses its
journaled full-target apply internally so restart/recovery retains the exact
intent. Raw proxy edits alone do not update durable controller state.

## Runtime topology

By default a component runs with Docker network mode `none`: it has a network
namespace but no Docker network attachment. If `egress INSTANCE` explicitly
requests external routing, DComp creates one dedicated, non-internal bridge
with only that component as a member. The bridge provides egress policy; it
does not carry DComp interface traffic.

There are no per-link Docker networks. Components linked to each other have no
Docker network path to one another.

Each state root has a deterministic Docker namespace: the first 128 bits of
SHA-256 over its cleaned absolute path, encoded as 32 lowercase hexadecimal
characters. DComp prefixes every container, egress-network, and named-volume
name with `dcomp.<namespace>.<system>.` and includes the namespace in its
ownership labels. Thus identical system and component names under different
state roots do not collide on the same Docker Engine. An explicit runtime root
changes proxy placement only; it does not change this Docker namespace.

The proxy creates one Unix listener for every resolved endpoint:

```text
<runtime-root>/<system>/in/<instance>.<input>
<runtime-root>/<system>/out/<instance>.<output>
```

The default runtime root is `<state-root>/run`; an explicit `--runtime-root`
or `DCOMP_RUNTIME_ROOT` may place the reconstructible proxy tree elsewhere. A
component receives individual bind mounts at:

```text
/run/dcomp/in/<input>
/run/dcomp/out/<output>
```

Mounting files read-only rather than mounting the containing host directories
enforces endpoint visibility and prevents replacement of socket pathnames;
socket communication itself remains bidirectional. User binds and named
volumes are rejected if their targets overlap `/run/dcomp`.

The corresponding environment is:

```text
DCOMP_IN_<INPUT>=unix:///run/dcomp/in/<input>
DCOMP_OUT_<OUTPUT>=unix:///run/dcomp/out/<output>
```

Names are uppercased and hyphens become underscores. Components connect to
both input and output addresses. They never bind or listen on interface paths.

When a pair is established, the proxy sends the consumer's component and input
endpoint identity to the output. SDK adapters consume this transport header
and expose the origin separately from the unchanged application stream. See
the [connection-origin contract](component-contract.md#connection-origin).

## Connection routing

Each output listener feeds a pool of producer-side connections. When a client
connects to an input listener, the proxy takes one connection from the linked
output pool and starts two byte-copy directions. EOF and shutdown close both
sides cleanly.

An output linked to several inputs shares its connection pool across those
routes. Each consumer connection receives a distinct producer connection;
bytes are never broadcast or merged. This is the fan-out model required for
bidirectional protocols such as gRPC.

If either component disconnects, the stream pair is released. Subsequent
connections are paired normally, allowing one component container to restart
without restarting the proxy or its peers.

Disconnected connections are also removed while waiting for a peer, on both
inputs and outputs. Pairing skips connections that have already hung up, so a
provider restart cannot leave dead queue entries ahead of its replacement.
Waiting connections retain buffered bytes and write-half-close semantics: a
client may finish sending its request before a provider arrives and still
receive the response. Queue cleanup does not consume application data.

## Proxy process and readiness

`dcomp-proxy` receives a strict JSON configuration containing the system,
instance identity, wiring digest, endpoint paths, global assignments, and links. Before publishing
any socket it writes `proxy.pid` and creates an internal socket-ownership
ledger. It then publishes the control and endpoint listeners, writes
`proxy.ready`, and signals readiness through an inherited file descriptor.
Components do not start before that signal.

The ledger, rather than the wiring config or a runtime-directory scan, is the
source of pathname cleanup authority. Each socket has a private hard-link
anchor that keeps its inode alive while public names are removed. Records move
through `reserved`, `anchored`, and `released` states, so live teardown,
graceful shutdown, and dead-process cleanup all use the same idempotent
transition. A reused public pathname is preserved because it is not the same
file as the anchor. `proxy.pid` disappears only after the ledger is empty;
therefore an absent PID marker from a current proxy proves pathname cleanup has
finished and manager cleanup performs no socket sweep.

The control socket supports `status`, `shutdown`, authoritative `resync`,
`mod-wire`, and `assign-global`. Every
request carries the recorded proxy instance ID and the exact current control-
protocol version; a mismatch is rejected before command dispatch. Shutdown and
all wiring mutations are serialized. Messages are newline-delimited JSON limited to 16 MiB
in both directions.

Status reports the PID, mutable wiring digest, readiness, endpoint counts,
pending connections, and system-wide active stream-pair count. During a resync
transition it reports `ready=false` without claiming a digest. For every
resolved concrete link identity it also reports an active-pair gauge and
cumulative bytes successfully forwarded in both directions. Surviving links keep their
counters; counters reset when a proxy is replaced or a removed link is
recreated. Unbound globals have no concrete route or link metric.

Unix socket pathnames have a small kernel limit. If a valid runtime endpoint
would exceed it, the proxy binds a deterministic short path in a private,
user-owned hidden directory in the runtime-root filesystem and hard-links that
socket inode into the documented runtime tree. Keeping both names on the same
filesystem also works when `/var/run` and `/tmp` are different mounts. Docker
still mounts the named runtime-tree file and the container address remains
unchanged.

For group-shared state, the hidden directory belongs to the selected group and
its name uses the group ID. This gives every member the same shortened socket
path when observing or replacing a proxy started by another member.

The ownership ledger and anchors are disposable runtime metadata, not part of
the machine API or durable DComp state format. They do not change endpoint
paths, bind mounts, or the component wire contract.

Endpoint sockets are mode `0666` because component images may run under
arbitrary non-root UIDs. Runtime directories, configuration, PID, readiness,
control, and log files are owner-only. `SO_PEERCRED` enforcement is optional
future hardening; DComp provides no encryption or application authentication.

## Apply lifecycle

For a new deployment DComp:

1. records the target operation;
2. creates a dedicated bridge for each component that declares `egress`;
3. starts the proxy and waits for readiness;
4. creates containers with only their endpoint socket mounts;
5. verifies exact mounts, environment, security policy, and network mode;
6. starts components; and
7. commits Docker and proxy identities as desired state.

An image-only change keeps the proxy and unrelated containers. Wiring changes
resync the live proxy: unchanged endpoint identities keep their socket inodes,
surviving links keep established streams, and only removed links are cut.
Changing a component's own endpoint set first retires that component, so no
container mounts a path when the proxy publishes its new listener. A runtime-
root change still replaces the complete proxy and component fleet, including
components without endpoints.

`down` verifies recorded ownership, stops/removes component containers first,
stops the proxy second, then removes transient egress networks. Named volumes
survive.

## Identity and recovery

Docker mutations use immutable IDs after ownership verification. Networks,
containers, volumes, and the proxy have separate state records. A proxy record
contains its instance ID, PID, wiring digest, runtime directory, control
socket, and log path. DComp never signals a PID until the live control socket
has confirmed the same instance and PID.

Operation intent, pending creates, and egress endpoint cleanup identities are
written before mutation. A lost Docker response is recovered by inspecting
the operation-owned name. Container removal is not checkpointed until network
inspection proves its exact endpoint absent; a stranded endpoint is removed
only after the recorded container ID and name are both absent. A proxy start
lost before state publication is recovered by its configuration and
identity-checked control socket. See [Lifecycle](lifecycle.md).

## Observation

`status` observes one coherent state generation under a shared lock and
reports proxy, network, and component health, including previous-generation
resources still awaiting retirement. `view` joins the resolved topology to
that observation, while `dashboard` serves the same read-only document to the
bundled local viewer. `logs` concurrently merges Docker logs with `proxy.log`;
proxy records use the source name `@proxy`. Observation never repairs
resources.

## Trust boundary and limits

DComp trusts the host, its durable and runtime roots, and the local Docker
socket. Components receive no Docker socket. Fixed container policy enables an
init process, restart policy `no`, `no-new-privileges`, drops `NET_RAW`, and
sets a 2048-process limit.

DComp provides no multi-host overlay, replication, automatic
failover, payload inspection, protocol translation, encryption, arbitrary
environment or privilege passthrough, or global long-lived daemon.
