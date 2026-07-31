# Architecture

## Purpose

DComp runs a system of independently built Docker components on one host. A
downstream project owns:

- its application interface definitions, which may be `.proto` files;
- the component source and Dockerfiles;
- one `component.dcomp` beside each component; and
- a `system.dcomp` that creates instances and links their interfaces.

DComp supplies a host-side Go library and CLI for validation and lifecycle
control. It is not a daemon, service registry, router, proxy, sidecar, image
builder, or container scheduler. After startup, component traffic does not pass
through DComp.

## Static system description

A component descriptor names an existing image and its local interface
endpoints:

```text
docker example/filter:1
input example.document.v1.Documents documents
output example.document.v1.Documents filtered
```

A system file creates component instances and links one input directly to one
output:

```text
system example
component source components/source
component filter components/filter
bind source ./source.conf /etc/source.conf ro
volume filter cache /var/lib/filter rw
args filter serve --strict
publish filter tcp 127.0.0.1 8080 8080
egress source
egress filter
link filter.documents source.documents
```

The component path may be absolute. A relative path is resolved from the
directory containing `system.dcomp`. It may name either a directory containing
`component.dcomp` or the descriptor itself.

The instance runtime directives are:

- `bind INSTANCE SOURCE TARGET ro|rw`;
- `volume INSTANCE LOGICAL_NAME TARGET ro|rw`;
- `args INSTANCE ARG...`;
- `publish INSTANCE tcp|udp HOST_IP HOST_PORT CONTAINER_PORT`; and
- `egress INSTANCE`.

They must follow the component declaration. Bind sources are made canonical and
must exist. Container mount targets are absolute, clean, and non-overlapping.
Published host sockets and volume names must be unique in their applicable
scope. A published port requires the same instance to declare `egress`, because
Docker cannot publish from an internal-only bridge. These directives are typed
policy, not arbitrary Docker argument passthrough.

The two sides of a link are explicit endpoint references. DComp does not infer
links from interface names. Every input must have exactly one link; an output
may serve any number of inputs. The input and output must declare the same
nominal interface identifier.

Links describe addresses, not startup dependencies. Cycles are valid.

Before changing Docker, DComp:

1. parses all component and system descriptors;
2. validates names, endpoint directions, complete input binding, and nominal
   interface-name matches;
3. validates and canonicalizes each instance's runtime policy;
4. resolves every image reference to an immutable image ID;
5. requires each image to define a Docker `HEALTHCHECK`; and
6. requires every image-declared OCI `VOLUME` target to have an explicit bind
   or named-volume mount.

DComp does not read interface declarations from OCI labels. It does not build
or pull images. Each image must already be resolvable by the local Docker
Engine.

## Runtime topology

DComp creates one private base bridge for each component. A base bridge has one
member and is internal unless that component explicitly declares `egress`.
Giving two components egress does not place them together on a shared external
network.

Each direct link receives another private internal bridge containing exactly
the output component and the input component. Output fan-out therefore creates
separate networks for separate consumers. Components have no network-level path
to unlinked components, and a consumer linked through an intermediate cannot
reach the upstream component directly.

Ports are published only by an explicit `publish` directive. DComp does not
inject the Docker socket or its state directory.

A component with declared outputs listens on TCP `0.0.0.0:50051`. Separate
container network namespaces make the fixed port unambiguous. How a component
dispatches multiple declared outputs on that listener is application protocol
behavior, not DComp behavior. A sink or workload component may declare only
inputs and does not need to open that listener.

For each linked input, DComp injects:

```text
DCOMP_LINK_<INPUT_NAME>=dns:///<OUTPUT_COMPONENT>:50051
```

Input names are converted to uppercase and hyphens become underscores. For
example, `model-store` becomes `DCOMP_LINK_MODEL_STORE`. The receiving
component consumes that injected address. Components communicate directly
through Docker DNS; DComp never forwards, decodes, validates, or transforms
application messages.

Every component image must define a meaningful OCI `HEALTHCHECK`. DComp checks
that the declaration exists and observes Docker's health result. It does not
mandate an application health protocol, inspect a reflection service, or
compare running services with schema definitions. Standard gRPC health,
reflection, and protobuf/gRPC multiplexing are conventions implemented by the
optional Go helper package.

## Lifecycle control

The controller is a library invoked by a short-lived CLI process. It connects
only to the local Docker Unix socket; remote Docker hosts are rejected.

For a new system, DComp:

1. creates the required component and link networks;
2. creates the component containers;
3. attaches each container only to its planned networks;
4. starts the components; and
5. commits their exact resource identities.

Health is reported per component after start. An unhealthy or exited component
is a stable, inspectable system state; it does not leave the whole apply
transaction pending.

For an existing system, component digests select which containers can be
retained. An unchanged container keeps its immutable ID and stays running while
link networks are reconciled around it. Only added or changed components are
created; removed or superseded components are retired. Links still impose no
provider/consumer startup order, and cycles remain valid.

Docker is the source of truth for observed objects. Small host-side files
record the last committed deployment and at most one incomplete operation.
DComp records intent before Docker mutation and can resume from verified Docker
facts after interruption. The state root is bound write-once to Docker's
stable engine ID, preventing recorded immutable IDs from being reused against
another local daemon. See [Lifecycle](lifecycle.md).

## Identity and mutation authority

DComp distinguishes:

1. an immutable image ID;
2. an immutable container or network ID;
3. a digest for each resolved component; and
4. a digest of the complete resolved system.

A component digest covers its immutable image ID, interface declaration,
normalized runtime policy, and inbound target component. Outgoing links do not
change a provider's component digest, so adding a consumer does not replace the
provider. The system digest additionally covers the complete direct-link set.
Descriptor paths and mutable image references are excluded; canonical bind
source paths are runtime identity.

Docker named volumes expose no immutable ID. DComp instead uses deterministic
component-scoped names and verifies their driver and ownership labels before
mounting them. Persistent volumes are never deleted implicitly.

System names are host-wide Docker identities. A different host-side state root
does not create another Docker namespace for the same system name.

Human-readable names provide configuration and Docker DNS, not mutation
authority. DComp labels objects it creates, records their full IDs, and verifies
identity, ownership, image, and network immediately before mutation. A
same-named foreign object is an error. Start, stop, and remove operations use
full immutable IDs.

## Observation

`dcomp status NAME` takes a shared lifecycle lock and inspects the recorded
networks and component containers as one coherent generation.
`dcomp logs NAME` reads the Docker stdout and stderr snapshots for all verified
component containers. `dcomp logs -f NAME` follows them concurrently and emits
one host-side stream with timestamp, component, stream, and message. Individual
log records are capped at one MiB while being assembled.

Log aggregation is observational. DComp does not install an agent or sidecar,
intercept RPCs, or define what a component logs.

## Process behavior

Docker starts the image entrypoint and delivers its configured stop signal.
DComp requests a bounded Docker stop; the component must stop accepting work,
drain in-flight operations within a bounded deadline, and exit before Docker's
timeout.

Containers use Docker restart policy `no`, an init process,
`no-new-privileges`, a dropped `NET_RAW` capability, and a 2048-process PIDs
limit. These are fixed DComp policy rather than configuration. `dcomp restart
NAME COMPONENT...` is an explicit, recorded operation on selected existing
immutable container IDs. With no component arguments it restarts every
component.

## Trust boundary

DComp trusts the host, its state directory, and the local Docker socket.
Possession of that socket is effectively host-administrative authority.

Internal component and link bridges block external routing by default. Only an
explicit `egress` component base bridge is externally routed. DComp also
rejects unexpected network attachments when verifying a container.

## Deliberate limits

DComp is single-host and Docker-specific. It deliberately provides no:

- multi-host placement, replicas, or failover;
- dynamic registry, discovery protocol, or runtime rewiring;
- router, proxy, service mesh, or sidecar;
- image build or pull workflow;
- automatic container restart policy;
- secret, user, arbitrary environment, entrypoint, privileged, configurable
  capability or resource-limit, or device configuration;
- bind propagation or adoption of arbitrary existing Docker volumes; or
- speculative repair when ownership cannot be proved.

The narrow boundary keeps the component contract language-neutral and the
host controller small.
