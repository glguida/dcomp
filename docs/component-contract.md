# Component contract

A DComp component consists of:

1. an independently built OCI image; and
2. a project-owned `component.dcomp` describing the image and its nominal
   interface endpoints.

DComp applies only the explicit runtime policy declared by `system.dcomp` and
supplies direct input targets. It defines no application message envelope and
does not inspect request or response bodies.

## `component.dcomp`

The format is line-oriented:

```text
docker IMAGE
input INTERFACE_NAME LOCAL_NAME
output INTERFACE_NAME LOCAL_NAME
```

Blank lines and text after `#` are ignored. `docker` appears exactly once.
`input` and `output` may each appear zero or more times.

System, instance, and endpoint names begin with a lowercase ASCII letter and
contain only lowercase letters, digits, and hyphens, up to 63 characters.

Example:

```text
docker example/transform:1
input example.echo.v1.Echo upstream
input example.audit.v1.Audit audit
output example.transform.v1.Transform transform
```

`INTERFACE_NAME` is a dotted nominal identifier, such as
`example.echo.v1.Echo`. Its syntax accommodates fully qualified protobuf
service names, but DComp treats it only as an opaque compatibility name.
`LOCAL_NAME` identifies the endpoint within that input or output list. Input
names and output names are separate namespaces.

The descriptor names an image that has already been built or otherwise made
available to the local Docker Engine. DComp does not execute `docker build`,
pull images, or infer a Dockerfile or build context.

Application schemas, including any `.proto` files, remain normal project
source. DComp stores no central interface catalogue, loads no schema
definitions, and generates no application bindings.

## System links

The system format is also line-oriented:

```text
system NAME
component INSTANCE PATH
bind INSTANCE SOURCE TARGET ro|rw
volume INSTANCE LOGICAL_NAME TARGET ro|rw
args INSTANCE ARG...
publish INSTANCE tcp|udp HOST_IP HOST_PORT CONTAINER_PORT
egress INSTANCE
link INSTANCE.INPUT INSTANCE.OUTPUT
```

It uses the same blank-line and `#` comment rules.

A `system.dcomp` creates instances of components and binds endpoint references:

```text
system demo
component backend components/backend
component frontend components/frontend
link frontend.upstream backend.echo
```

The left reference must name a declared input. The right reference must name a
declared output. Their interface names must match exactly. This is nominal
matching: DComp does not compare schemas, descriptor sets, or wire formats.

Every input must be linked exactly once. Outputs may fan out. Links may form
cycles because they configure addresses, not lifecycle dependencies.

A component path may name either a directory containing `component.dcomp` or
the file itself. Relative paths are resolved from the system file; absolute
paths are accepted.

## Instance runtime policy

Runtime directives must appear after the named `component` declaration. They
are optional; the defaults are no mounts, inherited image command arguments, no
published ports, and no external egress.

`bind` mounts an existing host path. A relative source is resolved from
`system.dcomp`; DComp resolves symlinks and stores a canonical absolute path.
The target must be a clean absolute container path. The access mode is always
explicit:

```text
bind frontend ./frontend.conf /etc/frontend.conf ro
```

`volume` creates or reuses component-owned persistent storage:

```text
volume frontend state /var/lib/frontend rw
```

The logical name uses the same lowercase local-name syntax as components.
DComp derives and verifies the Docker volume name. It does not adopt an
arbitrary existing volume, and it does not delete declared volumes during
replacement, `down`, or `abort`.

Host tooling can request the physical Docker name without duplicating DComp's
naming rules:

```sh
dcomp volume --json example frontend state
```

The lookup is read-only. It succeeds only when the exact local volume exists,
the selected state root is bound to the current Docker engine, and all DComp
ownership, system, component, and logical-name labels match. It also succeeds
after `dcomp down example`, because persistent volumes intentionally outlive
the deployment.

`args` replaces the image's command argument array without changing its
entrypoint:

```text
args frontend serve --mode production
```

Arguments are ordinary whitespace-delimited tokens. There is no quoting,
interpolation, or shell evaluation.

`publish` binds one TCP or UDP container port on an explicit host IP:

```text
publish frontend tcp 127.0.0.1 8080 8080
egress frontend
```

IPs must be literals. Container ports must be in `1..65535`. A host port in
`1..65535` requests that exact port; host port `0` asks Docker to allocate a
free port. The effective allocation is reported by `dcomp status --json`.
Docker may choose a different allocation after a component restart, so callers
must treat that status as the current endpoint rather than persisted identity.
Duplicate fixed host protocol/IP/port tuples are rejected. Within one address family, an
unspecified-address binding (`0.0.0.0` or `::`) also conflicts with every
specific address using the same protocol and host port. IPv4 and IPv6 are
separate; any remaining daemon-confirmed bind failure leaves the operation
available for `resume` or `abort`.

`egress INSTANCE` gives only that component an externally routed base network.
Without it, all of the component's networks are internal. Docker does not
realize published ports on an internal-only bridge, so an instance using
`publish` must also declare `egress`; DComp does not grant that route
implicitly.

Bind and volume targets may not be equal, nested, or otherwise overlap within
one component. DComp supplies no arbitrary Docker option, environment,
entrypoint, privilege, capability, device, or mount-propagation escape hatch.
Every component is launched with `no-new-privileges`, `NET_RAW` dropped, and a
2048-process PIDs limit.

## Required runtime behavior

The image entrypoint must:

- if it declares outputs, listen on TCP `0.0.0.0:50051` and serve every
  declared output interface on that listener;
- read linked input addresses from `DCOMP_LINK_*`;
- satisfy its image's meaningful OCI `HEALTHCHECK`; and
- shut down cleanly when Docker delivers its stop signal.

All declared outputs share port `50051`. Dispatch or multiplexing is the
component's application-protocol responsibility. A sink or workload component
may have inputs and no outputs; its health check remains the readiness
contract.

The component must not assume a fixed IP address. Docker DNS and the supplied
link addresses are the stable addressing interface.

## Link injection

For this link:

```text
link frontend.model-store backend.models
```

DComp injects into `frontend`:

```text
DCOMP_LINK_MODEL_STORE=dns:///backend:50051
```

Environment variable names are derived from input names:

1. prefix with `DCOMP_LINK_`;
2. convert letters to uppercase; and
3. replace `-` with `_`.

The value is an injected address in `dns:///COMPONENT:50051` form. DComp does
not interpret it after injection. A gRPC client can use it directly; another
protocol implementation may parse it itself. The component selects its own
client implementation, deadlines, retry policy, and application behavior.

## Health

The image must define a meaningful Docker `HEALTHCHECK` for the component's
actual readiness. DComp rejects images without a health-check declaration and
observes the health state reported by Docker. It does not invoke or prescribe
an application-level health API.

The probe must use a bounded deadline and return non-zero when the component
cannot serve its declared outputs. The exact probe and protocol belong to the
component.

## Optional Go/gRPC convention

The `component` Go package provides a convenient implementation based on
protobuf/gRPC. Its server multiplexes generated gRPC services on port `50051`,
serves standard `grpc.health.v1.Health`, enables server reflection, reads link
addresses, and coordinates bounded shutdown.

Those behaviors are conventions of the optional helper, not requirements
enforced by DComp. DComp does not query health RPCs or reflection, load
protobuf descriptors, compare message schemas, or inspect application
payloads.

## Graceful shutdown

The entrypoint must receive Docker's stop signal or relay it correctly. Shell
wrappers should use `exec`.

On termination, the server:

1. stops accepting new work;
2. drains in-flight operations within a bounded deadline; and
3. exits before Docker's stop timeout.

An image may set `STOPSIGNAL`; DComp does not override it.

## Network and filesystem constraints

Every component has a private base bridge containing no peer. The bridge is
internal unless the instance declares `egress`. Every direct input link has a
separate internal bridge containing exactly its consumer and provider. An
output serving several consumers is therefore attached to several isolated
link networks; the consumers do not share a network.

DComp publishes only declared host ports and mounts only declared binds and
volumes. It never mounts the Docker socket or host-side DComp state
automatically. An image may declare OCI `VOLUME` targets only when every target
is exactly covered by an explicit `bind` or `volume`, preventing Docker from
creating untracked anonymous storage.

## Minimal image pattern

The implementation language and base image are unrestricted:

```dockerfile
FROM example/runtime:version

COPY component /usr/local/bin/component
COPY component-healthcheck /usr/local/bin/component-healthcheck

HEALTHCHECK --interval=2s --timeout=1s --retries=10 \
  CMD ["/usr/local/bin/component-healthcheck", "127.0.0.1:50051"]

ENTRYPOINT ["/usr/local/bin/component"]
```

No DComp interface labels are required. The corresponding
`component.dcomp`, owned by the downstream project, is the interface contract.
