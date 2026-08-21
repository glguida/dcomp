# Architecture

## Scope

DComp runs independently built Docker components on one Linux host. The CLI is
a short-lived controller; each running system has one long-lived
`dcomp-proxy` data-plane process. There is no global daemon, scheduler, service
registry, or multi-host control plane.

Downstream projects own their interface definitions, component images,
`component.dcomp` files, and `system.dcomp`. DComp owns Docker resource
lifecycle and every declared interface socket.

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

Every input has exactly one link. Outputs may fan out; cycles are valid. The
two endpoint service identifiers must match. DComp treats the resulting byte
streams as opaque and does not load schemas.

Before mutation DComp validates descriptors and runtime policy, resolves every
image reference to an immutable ID, checks the image health-check declaration,
and incorporates endpoint definitions and links into stable digests.

## Runtime topology

By default a component runs with Docker network mode `none`: it has a network
namespace but no Docker network attachment. If `egress INSTANCE` explicitly
requests external routing, DComp creates one dedicated, non-internal bridge
with only that component as a member. The bridge provides egress policy; it
does not carry DComp interface traffic.

There are no per-link Docker networks. Components linked to each other have no
Docker network path to one another.

The proxy creates one Unix listener for every resolved endpoint:

```text
<runtime-root>/<system>/in/<instance>.<input>
<runtime-root>/<system>/out/<instance>.<output>
```

The default runtime root is `/var/run/dcomp`. A component receives individual
bind mounts at:

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

## Proxy process and readiness

`dcomp-proxy` receives a strict JSON configuration containing the system,
instance identity, wiring digest, endpoint paths, and links. It creates all
listeners, writes `proxy.pid` and `proxy.ready`, opens a local control socket,
then signals readiness through an inherited file descriptor. Components do not
start before that signal.

The control socket supports identity-checked status and graceful shutdown.
Status reports the PID, wiring digest, endpoint counts, pending connections,
and active stream-pair count. A shutdown request must carry the recorded proxy
instance ID.

Unix socket pathnames have a small kernel limit. If a valid runtime endpoint
would exceed it, the proxy binds a deterministic short path in a private,
user-owned hidden directory in the runtime-root filesystem and hard-links that
socket inode into the documented runtime tree. Keeping both names on the same
filesystem also works when `/var/run` and `/tmp` are different mounts. Docker
still mounts the named runtime-tree file and the container address remains
unchanged.

Endpoint sockets are mode `0666` because component images may run under
arbitrary non-root UIDs. Runtime directories, configuration, PID, readiness,
control, and log files are owner-only. `SO_PEERCRED` enforcement is optional
future hardening; there is no encryption or application authentication in
0.2.

## Apply lifecycle

For a new deployment DComp:

1. records the target operation;
2. creates a dedicated bridge for each component that declares `egress`;
3. starts the proxy and waits for readiness;
4. creates containers with only their endpoint socket mounts;
5. verifies exact mounts, environment, security policy, and network mode;
6. starts components; and
7. commits Docker and proxy identities as desired state.

An image-only change may keep the proxy and unrelated containers. A wiring or
endpoint change gets a new proxy wiring digest. DComp then retires containers,
stops the old proxy, starts the new proxy, and recreates containers so their
individual bind mounts refer to the new socket inodes. This is why dynamic
rewiring without component restart is not a 0.2 feature.

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
that observation, while `dash` serves the same read-only document to the
bundled local viewer. `logs` concurrently merges Docker logs with `proxy.log`;
proxy records use the source name `@proxy`. Observation never repairs
resources.

## Trust boundary and limits

DComp trusts the host, its durable and runtime roots, and the local Docker
socket. Components receive no Docker socket. Fixed container policy enables an
init process, restart policy `no`, `no-new-privileges`, drops `NET_RAW`, and
sets a 2048-process limit.

Version 0.2 deliberately has no multi-host overlay, runtime rewiring,
replication, automatic failover, payload inspection, protocol translation,
encryption, arbitrary environment or privilege passthrough, or global
long-lived daemon.
