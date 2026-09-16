# CLI and description-file reference

This document is the complete user-facing reference for the DComp 0.3 command
line, `system.dcomp`, and `component.dcomp`. For the component process contract,
see [Component contract](component-contract.md). For transaction and recovery
semantics, see [Lifecycle and recovery](lifecycle.md).

## Command-line shape

```text
dcomp [GLOBAL_OPTIONS] COMMAND [COMMAND_OPTIONS] [ARGUMENTS...]
```

Global options must appear before the command. Command-specific options appear
after it.

The global options are:

- `--state-root DIR`: use an absolute durable-state directory instead of the
  default;
- `--runtime-root DIR`: use an absolute, clean proxy-runtime directory instead
  of the default; and
- `-h` or `--help`: print the command synopsis.

The state-root precedence is `--state-root`, `DCOMP_STATE_ROOT`,
`$XDG_STATE_HOME/dcomp`, then `$HOME/.local/state/dcomp`. The runtime-root
precedence is `--runtime-root`, `DCOMP_RUNTIME_ROOT`, then `<state-root>/run`.
The runtime filesystem must support Unix-domain socket files and hard links
within a system's runtime tree; the proxy uses private hard-link anchors for
identity-safe retry and crash cleanup.
The cleaned absolute state-root path also selects the Docker resource
namespace used in physical container, egress-network, and named-volume names
and ownership labels. Consequently, two state roots can use the same system
name on one Docker Engine without sharing those resources. `--runtime-root`
does not affect the Docker namespace.

DComp uses `DOCKER_HOST` when it names a local `unix://` Docker socket and
otherwise defaults to `unix:///var/run/docker.sock`. Remote TCP/HTTP Docker
daemons are rejected. `DCOMP_PROXY_BINARY` may select an absolute development
proxy binary; normally `dcomp-proxy` must be installed beside `dcomp`.

## Commands

### `version`

```text
dcomp version [--json]
```

Print the DComp semantic version. `--json` emits the machine-version document
described in [Machine API](machine-api.md). This command does not connect to
Docker or open the state root.

### `check`

```text
dcomp [GLOBAL_OPTIONS] check FILE
```

Parse the system and component files, inspect the referenced images, validate
their health checks and declared volumes, and calculate the resolved system
digest. It prints `SYSTEM DIGEST` and does not mutate Docker, durable state, or
the runtime root. Image inspection is still a read-only Docker operation.

### `up`

```text
dcomp [GLOBAL_OPTIONS] up FILE
```

Resolve and apply a system. DComp records intent before mutation, retains
unchanged resources when their immutable identity and policy still match, and
prints lifecycle progress on standard error.

If the same resolved target and runtime root already have an interrupted apply,
`up` resumes it. A different target first resolves and safely supersedes the
old operation. See [Lifecycle and recovery](lifecycle.md) for the exact phase
model.

### Incremental edits

Global options still precede the command; command options precede positional
arguments. These commands operate on committed state and do not edit a source
file. They wait for concurrent lifecycle operations with cancellation support;
if a previous operation is incomplete, they fail and direct callers to
`resume`/`abort`. `up FILE` continues to replace the complete desired system.

Existing images remain pinned to their committed IDs. Use the same state
root and runtime root as the running system.

### `add-component`

```text
dcomp [GLOBAL_OPTIONS] add-component [OPTIONS] SYSTEM NAME PATH
```

`add-component` loads `PATH` as a component directory or manifest and rejects
an existing instance name. It creates the system if absent, creates and mounts
its proxy endpoints, and starts the new container. Options:

- `--link INPUT=TARGET` supplies a direct or `@GLOBAL` input target; repeatable.
- `--bind SOURCE,TARGET,ro|rw` mounts a host path; repeatable. Sources are relative
  to the caller's working directory, then resolved through symlinks.
- `--volume NAME,TARGET,ro|rw` adds a persistent logical volume; repeatable.
- `--arg VALUE` supplies one literal command argument; repeatable. Use
  `--arg=--option` for arguments beginning with a dash.
- `--egress` enables the component's dedicated external network.
- `--user UID:GID` overrides the image user with a numeric user and group.
- `--publish PROTOCOL,HOST_IP,HOST_PORT,CONTAINER_PORT` publishes a port;
  repeatable, and requires `--egress`.

Comma-separated mount/publication fields cannot themselves contain commas.
The Go API accepts structured values. Existing mount, image health-check,
volume, security and publication validation applies. Unlinked inputs are valid
and close connections until wired.

### `rm-component`

```text
dcomp [GLOBAL_OPTIONS] rm-component SYSTEM NAME
```

`rm-component` rejects an unknown name. It removes that container and its
endpoints, drops wires to/from its direct endpoints, and leaves every global
name it exported declared but unbound. Consumers referencing those global
names keep their symbolic wires. Other components and persistent volumes
survive. Removing the last component leaves an empty, editable system.

### `mod-wire`

```text
dcomp [GLOBAL_OPTIONS] mod-wire SYSTEM COMPONENT.INPUT COMPONENT.OUTPUT|@GLOBAL|-
```

`mod-wire` replaces the target of exactly one existing input. `-` disconnects
it. `@NAME` is always a symbolic global reference; `COMPONENT.OUTPUT` is always
direct. A global reference must name a declared global of the input's service
type. A direct target must exist and have that same service type.

### `assign-global`

```text
dcomp [GLOBAL_OPTIONS] assign-global [--service TYPE] SYSTEM NAME COMPONENT.OUTPUT|-
```

`assign-global` creates or changes a global output name. A new bound name's
service type is inferred from its target; `--service` can explicitly assert it.
`-` unbinds an existing name, or declares a new unbound name when `--service` is
provided. Names use `[a-z][a-z0-9_-]{0,62}` and are scoped to one system and state
root. A global targets one specific output; alias chains and input targets are
rejected. `assign-global` preserves an existing name's service type.

### Go lifecycle API

The Go API exposes `Controller.AddComponent`, `RemoveComponent`, `ModifyWire`,
`AssignGlobal`, and `Edit`. `Edit` runs a composition callback under the system's
exclusive lock and applies the complete batch as one recoverable operation.
Callbacks must not invoke nested lifecycle operations or retain the mutable
specification for later use.

### `ps`

```text
dcomp [GLOBAL_OPTIONS] ps [-a|--all] [--json] [NAME]
```

List recorded component containers across all systems, or only `NAME` when it
is supplied. By default only containers reported as running are included.
`-a`/`--all` also includes created, exited, missing, and degraded records.

The text output includes system, component, container state, health, exit code,
effective published ports, pending operation, and any verification problem.
`--json` emits the API 2 processes document.

### `status`

```text
dcomp [GLOBAL_OPTIONS] status [--json] NAME
```

Observe one system without repairing it. DComp verifies the recorded proxy,
containers, egress networks, endpoint mounts, environment, security policy,
and Docker identity. During an operation it also reports previous-generation
resources still awaiting retirement.

The command exits 0 only when the system is committed and operational. It
exits 1 for an absent, pending, stopped, unhealthy, missing, or otherwise
degraded system, after still producing its normal text or JSON status.

### `view`

```text
dcomp [GLOBAL_OPTIONS] view [--json] FILE|NAME
```

Describe one system's declared topology without changing lifecycle state.
When the argument is an existing regular file, DComp parses it and reports its
components, interfaces, links, mounts, arguments, and external routes without
resolving images or contacting Docker. When the argument is a system name,
DComp joins the recorded resolved topology with current proxy, network, and
component observations, including each link's connection state and cumulative
directional byte counters. A missing argument containing a path separator or
ending in `.dcomp` is treated as a missing file rather than a system name.

Plain output is intended for terminals. `--json` emits the API 2 view
document. Unlike `status`, observing an absent named system succeeds with an
empty, non-operational view; consumers should inspect `desired` and
`operational`.

### `dashboard`

```text
dcomp [GLOBAL_OPTIONS] dashboard [--listen ADDRESS] [FILE|NAME...]
```

Serve the bundled read-only system viewer and API 2 view documents until the
process is interrupted. The default address is `127.0.0.1:8199`. With no
arguments, the dashboard lists every recorded system. Arguments restrict it
to named systems and parsed system files; two files declaring the same system
name are rejected. Observations of recorded systems are cached for one second.

The dashboard has no authentication. Its loopback default is the security
boundary; choosing a non-loopback listen address exposes topology, host bind
paths, image identities, resource state, and diagnostics to clients that can
reach that address. See [System view](view.md) for the document and HTTP
contracts.

### `volume`

```text
dcomp [GLOBAL_OPTIONS] volume [--json] SYSTEM COMPONENT LOGICAL
```

Resolve one persistent logical volume to its deterministic Docker volume name.
DComp returns the name only after verifying the volume's local driver and full
ownership labels. This remains usable after `down`, because named volumes and
the state-root engine binding survive system removal.

The physical name is
`dcomp.<state-root-namespace>.<system>.volume.<component>.<logical>`.

Plain output is the Docker volume name. `--json` emits the API 2 volume
document.

### `logs`

```text
dcomp [GLOBAL_OPTIONS] logs [-f|--follow] NAME [COMPONENT...]
```

Merge Docker logs from the selected components. With no selectors it includes
all recorded components and the proxy. Use `@proxy` as a selector for the
proxy, including by itself for proxy-only logs. Duplicate or unknown selectors
are rejected.

Each line is tab-separated:

```text
RFC3339_NANO_TIMESTAMP  SOURCE  STREAM  MESSAGE
```

`-f`/`--follow` continues until cancellation. Logs are observational and do
not repair lifecycle state.

### `attach`

```text
dcomp [GLOBAL_OPTIONS] attach [--ready-fd FD] SYSTEM COMPONENT
```

Attach the caller's standard input, output, and error to one running component.
The proxy and exact container identity are verified first. DComp holds a shared
system lock for the attachment and permits only one writable attachment to a
component at a time.

`--ready-fd FD` is an integration handshake. `FD` must be an inherited
descriptor numbered 3 or higher. Once Docker has accepted the attachment,
DComp writes one byte with value `0x01` and closes that descriptor. No byte is
written if setup fails.

### `restart`

```text
dcomp [GLOBAL_OPTIONS] restart NAME [COMPONENT...]
```

Restart the selected committed containers without replacing the proxy or
other components. With no component selectors, restart every component in the
system. Unknown or duplicate selectors are rejected. Components must reconnect
their DComp streams after restart.

### `down`

```text
dcomp [GLOBAL_OPTIONS] down NAME
```

Stop and remove exact component containers, then stop the proxy and remove
transient egress networks. Persistent named volumes and the state-root Docker
Engine binding are preserved. Repeating `down` for an already absent system is
safe.

### `resume`

```text
dcomp [GLOBAL_OPTIONS] resume NAME
```

Continue the exact durable apply, down, restart, or abort operation recorded
for `NAME`. It fails if no operation is pending.

### `abort`

```text
dcomp [GLOBAL_OPTIONS] abort NAME
```

Stop a pending operation using verified observed state. For an apply, abort
removes only target resources that were not part of the previous committed
deployment, then reverse-resyncs a retained proxy to the previous wiring. It is
not rollback in the general case because resources already retired may be
gone. If reverse resync transiently cannot converge, abort takes the full-
replacement path and recreates the previous fleet. Proxy identity or protocol
mismatches are terminal. An operation with unresolved create results must be
resumed far enough to resolve them before it can be aborted.

### `inspect-image`

```text
dcomp [GLOBAL_OPTIONS] inspect-image IMAGE
```

Resolve an image through Docker with a 30-second timeout and print its immutable
ID, whether it declares a meaningful health check, and every OCI `VOLUME`
target. This is a diagnostic read-only operation and has no JSON mode.

## Exit status

- `0`: the command succeeded; for `status`, the system is operational;
- `1`: a lifecycle, Docker, state, validation, or observation error occurred,
  or `status` successfully observed a non-operational system;
- `2`: command-line usage or a global path was invalid; and
- `130`: the active operation was cancelled by `SIGINT` or `SIGTERM`.

Diagnostics and lifecycle progress go to standard error. Commands producing a
requested value or machine document write it to standard output.

## Lexical rules for description files

Description files are deliberately not a shell language:

- one directive occupies one line;
- tokens are separated by whitespace;
- blank lines are ignored;
- `#` begins a comment through the end of the line; and
- there is no quoting, escaping, environment expansion, or command
  substitution.

Consequently, a token cannot contain whitespace or `#`.

System, component-instance, endpoint, and logical-volume names use
`[a-z][a-z0-9-]{0,62}`. Global interface names additionally allow underscores:
`[a-z][a-z0-9_-]{0,62}`. A nominal service identifier contains at least two
dot-separated protobuf-style identifiers. DComp compares service identifiers
for exact equality but does not inspect schemas or application bytes.

## `component.dcomp`

A component path names either a directory containing `component.dcomp` or the
manifest itself. Its grammar is:

```text
docker IMAGE
input PROTOBUF_SERVICE LOCAL_NAME
output PROTOBUF_SERVICE LOCAL_NAME
```

Exactly one `docker` directive is required. Inputs and outputs may appear in
any order and either set may be empty. Names must be unique within their
direction; the input and output namespaces are separate.

Example:

```text
docker registry.example/document-filter:1.4
input example.document.v1.Documents documents
output example.document.v1.Documents filtered
```

The image must resolve to an immutable Docker image ID and declare a meaningful
OCI health check. Every image-declared `VOLUME` target must be covered exactly
by a `bind` or `volume` directive in the system file, preventing anonymous
volume creation.

## `system.dcomp`

A system file uses these directives:

```text
system NAME
component INSTANCE PATH
bind INSTANCE SOURCE TARGET ro|rw
volume INSTANCE LOGICAL_NAME TARGET ro|rw
args INSTANCE ARG...
user INSTANCE UID:GID
publish INSTANCE tcp|udp HOST_IP HOST_PORT CONTAINER_PORT
egress INSTANCE
global NAME SERVICE [INSTANCE.OUTPUT]
link INSTANCE.INPUT INSTANCE.OUTPUT|@GLOBAL
```

`system` is required exactly once, and an empty system is allowed.
Although final graph validation is order-independent, put `system` first and
declare a component before any `bind`, `volume`, `args`, `user`, `publish`, or `egress`
directive referring to it.

### `component`

```text
component INSTANCE PATH
```

Create one system-local component instance. `PATH` may name a component
directory or its exact `component.dcomp`. Relative paths are resolved from the
directory containing the system file. Instance names are unique, and the same
component definition may be instantiated under several names.

### `global`

```text
global NAME SERVICE [INSTANCE.OUTPUT]
```

Declare one system-global name for a specific output interface. `SERVICE` must
match the target output's nominal service identifier. Omit the target to leave
the name unbound. Names are unique within the system; globals cannot target
inputs or other globals.

A link to `@NAME` retains that symbolic reference. Reassigning the name changes
its consumers' resolved routes without editing their links. Streams whose
concrete route changes close on both sides; applications reconnect through the
same input sockets. An unbound global closes input connections immediately,
without a protocol-specific response frame. Docker health and operational
status do not imply that every global is bound.

### `link`

```text
link CONSUMER.INPUT PROVIDER.OUTPUT|@GLOBAL
```

Connect an input to a concrete output or a declared global with the identical
nominal service identifier. An input may have zero or one link. Outputs may
be unused or linked from several inputs, and cycles are valid.

`PROVIDER.OUTPUT` always names that specific component output; `@GLOBAL`
always follows that global's assignment. An unlinked input closes connections
immediately. Adding or reassigning a global never changes a direct wire.

The link authorizes only these orchestrator-owned endpoint sockets. It does not
create a Docker network or grant general connectivity between the containers.

### `bind`

```text
bind INSTANCE SOURCE TARGET ro|rw
```

Mount an existing host file or directory. Relative `SOURCE` paths are resolved
from the system-file directory. Symlinks are resolved before the canonical
absolute source becomes deployment identity.

`TARGET` must be an absolute clean container path other than `/`. Mount targets
must not overlap one another or the reserved `/run/dcomp` tree. `ro` and `rw`
control the filesystem mount and do not affect DComp socket stream direction.

The source must exist when preparing a new or recreated container. Incremental
edits retain existing containers' recorded mounts even if their host source
was subsequently moved or removed. Such a path does not block unrelated adds,
rewiring, or removal; recreating that container still requires its mount source.

### `volume`

```text
volume INSTANCE LOGICAL_NAME TARGET ro|rw
```

Mount a DComp-owned persistent Docker volume. The logical name is scoped by
system and component; it is not an arbitrary Docker volume name. Logical names
must be unique within one component. Target and overlap rules are the same as
for bind mounts. Volumes survive replacement and `down`.

### `args`

```text
args INSTANCE ARG...
```

Replace the image command arguments without changing its entrypoint. Exactly
one `args` directive may be supplied per component and it must contain at least
one argument. Arguments are literal tokens; DComp performs no shell parsing or
expansion.

### `user`

```text
user INSTANCE UID:GID
```

Run the component as this numeric user and primary group instead of the image's
default user. Both IDs must be canonical unsigned decimal values below
4294967295. The setting is optional and may be supplied once per component.
It is included in the component identity; changing it replaces the container.
DComp neither changes ownership of mounted files nor remaps host user IDs.
For a host workspace, select the UID/GID of the host user who owns its files.

### `egress`

```text
egress INSTANCE
```

Give the component one dedicated, non-internal Docker bridge for external
routing. Without this declaration the container uses Docker network mode
`none`. The bridge never carries DComp interface traffic and is not shared with
another component.

### `publish`

```text
publish INSTANCE tcp|udp HOST_IP HOST_PORT CONTAINER_PORT
```

Publish an additional component-owned TCP or UDP service. This does not
publish a declared DComp interface. `HOST_IP` must be an IP literal; hostnames
are not accepted. `HOST_PORT` is `0` for Docker allocation or a value from 1 to
65535. `CONTAINER_PORT` is from 1 to 65535.

A component with any published port must also declare `egress`, because Docker
cannot publish a port from network mode `none`. Conflicting fixed host bindings
are rejected across the complete system; unspecified addresses conflict with
specific addresses in the same IP family.

## Complete example

```text
system document-system

component source components/source
component filter components/filter

bind filter ./filter.json /etc/filter.json ro
volume filter cache /var/lib/filter rw
args filter serve --strict
publish filter tcp 127.0.0.1 8080 8080
egress filter

link filter.documents source.documents
```

Run `dcomp check system.dcomp` before `up` to resolve images and validate the
complete authored and image-derived contract without changing runtime state.

## Proxy control protocol 2

The host-only `proxy.sock` accepts one newline-delimited JSON request per
connection and replies with one status document. The 16 MiB message bound and
30-second request deadline apply. Every command carries `instance_id` and
`protocol_version: 2`. These are identity/version checks; host socket access is
the trust boundary.

`status`, `shutdown`, and authoritative `resync` retain their meanings. `resync`
carries the complete `wiring` and its canonical digest. Wiring now optionally
includes `globals` (`name`, `output_component`, `output_endpoint`); an unbound
global has empty output fields. A symbolic link has `global` and empty output
fields, while retaining its concrete input fields. Direct links are unchanged.

Incremental proxy commands operate on the current wiring under the same lock
as resync and shutdown:

```json
{"command":"assign-global","instance_id":"...","protocol_version":2,"global":{"name":"provider_endpoint","output_component":"provider","output_endpoint":"api"}}
{"command":"mod-wire","instance_id":"...","protocol_version":2,"wire":{"link":{"input_component":"team","input_endpoint":"inference","global":"provider_endpoint"}}}
{"command":"mod-wire","instance_id":"...","protocol_version":2,"wire":{"link":{"input_component":"team","input_endpoint":"inference"},"disconnect":true}}
```

Either edit can include `expected_digest` to reject a stale update. Without it,
the edit applies to current wiring at execution time. Invalid requests leave
wiring unchanged. Successful edits persist the symbolic configuration before
reporting ready. Retrying the same assignment/edit converges after interrupted
publication. Status optionally reports the global assignments in `globals`;
link metrics describe currently resolved concrete routes. Unbound global links
have no concrete route or link metric. During a transition, readiness is false
and the digest is absent.

This is a data-plane protocol: it does not create Docker containers, check
application service types, or update the controller's durable lifecycle state.
Use lifecycle CLI/API commands for managed systems; direct proxy edits are for
controllers that coordinate their own desired state and resource lifecycle.
In particular, do not bypass lifecycle state with raw edits and expect a later
`up` to preserve them. Container creation/removal must retain the established
socket publication and retirement ordering.
