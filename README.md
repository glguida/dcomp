# dcomp

`dcomp` is a small substrate for building systems from independently built
Docker components. A project owns its interface definitions, component source,
Dockerfiles, and wiring. DComp validates nominal interface names and runs the
resulting system with a private base network for each component and a private
network for each direct link.

There is no DComp daemon, service registry, proxy, or sidecar. Once started,
containers call one another directly. DComp is not in the application data
path.

## A DComp project

A typical project is an ordinary repository:

```text
document-system/
├── system.dcomp
├── interfaces/
│   └── document/v1/document.proto
└── components/
    ├── source/
    │   ├── component.dcomp
    │   ├── Dockerfile
    │   └── src/
    └── filter/
        ├── component.dcomp
        ├── Dockerfile
        └── src/
```

Each component directory contains a descriptor. It names an existing Docker
image and the component's named inputs and outputs:

```text
# components/source/component.dcomp
docker document-source:dev
output document.v1.Documents documents
```

```text
# components/filter/component.dcomp
docker document-filter:dev
input document.v1.Documents documents
output document.v1.Documents filtered
```

The system file creates instances and connects interface endpoints directly:

```text
system document-system

component source components/source
component filter components/filter

link filter.documents source.documents
```

The left side of a link must be an input, the right side must be an output, and
their declared interface identifiers must match exactly. This is nominal
matching: DComp does not load schemas or prove wire compatibility. Output
fan-out and cyclic interface wiring are valid. Links are address bindings, not
lifecycle dependencies.

The system file can also give each declared instance explicit runtime policy:

```text
bind source ./source-config /etc/document-source ro
volume filter cache /var/lib/document-filter rw
args filter serve --mode strict
publish filter tcp 127.0.0.1 8080 8080
egress source
egress filter
```

Runtime directives must follow the corresponding `component` declaration.
`bind` sources are resolved relative to the system file, must exist, and are
stored as canonical absolute paths. Mount targets are absolute container paths;
`ro` or `rw` is mandatory. `volume` names component-owned persistent storage.
`args` uses ordinary whitespace-separated arguments and replaces the image
command arguments, not its entrypoint. `publish` requires an explicit protocol,
host IP, and ports; host port `0` requests a Docker-allocated port reported by
`dcomp status --json` and may change after restart. Because Docker cannot publish a port from a component
whose bridges are all internal, `publish` also requires an explicit `egress`
directive; DComp never grants that route implicitly.

Component paths may be absolute or relative to the system file. They may name
either a component directory or its `component.dcomp` directly. The resolved
system digest covers direct links, normalized runtime policy, and immutable
image IDs. Descriptor paths and mutable image tags are not runtime identity;
canonical bind source paths are.

## Component contract

A component is an ordinary OCI image whose entrypoint:

- when it declares outputs, accepts them on TCP `0.0.0.0:50051`;
- reads linked input addresses from `DCOMP_LINK_<INPUT_NAME>`;
- responds meaningfully to its image's OCI `HEALTHCHECK`; and
- performs bounded graceful shutdown when Docker stops it.

For the example above, DComp gives `filter`:

```text
DCOMP_LINK_DOCUMENTS=dns:///source:50051
```

All declared outputs share port `50051`; the component decides how to dispatch
them. A sink or workload component may declare inputs without any output and
need not open that listener. The fixed port creates no host conflict because
every component has its own network namespace.

Each image must define a meaningful Docker `HEALTHCHECK`. DComp rejects images
with no health check and observes Docker's resulting health state; it does not
mandate or call an application-level health protocol. An OCI `VOLUME` target is
accepted only when that exact target has an explicit `bind` or `volume`
directive, so Docker cannot create anonymous storage outside DComp's verified
runtime.

Interface identifiers are dotted names compared as opaque strings. Projects
may conventionally use fully qualified protobuf service names and protobuf RPC,
but DComp neither loads nor compares `.proto` definitions and never decodes,
validates, or transforms application payloads.

The optional Go package `github.com/glguida/dcomp/component` implements one
convention: gRPC multiplexing, standard gRPC health, reflection, link lookup,
and bounded shutdown. These are helper-package features, not substrate
requirements. Components may use any language and need not use that package.

## Install and run

Runtime requirements:

- Linux;
- Docker Engine 25 or newer (API 1.44+).

Building from source additionally requires Go 1.25.0 or newer. From a source
checkout, build as the current user and install under `/usr/local`:

```sh
make build
sudo make install
```

For an installation owned by the current user, choose a prefix already on the
user's path:

```sh
make build
make install PREFIX="$HOME/.local"
```

`make install` installs `dcomp`, the optional `dcomp-healthcheck` component
probe, the licence, the project README, and the design documentation. It
accepts conventional `PREFIX`, `BINDIR`, `DOCDIR`, and `DESTDIR` overrides.
Installation only copies artifacts produced by `make build`; it never invokes
the Go compiler or `sudo`. Package builders can stage an installation with,
for example:

```sh
make build
make install DESTDIR="$pkgdir" PREFIX=/usr
```

Remove exactly those installed files with the same path settings:

```sh
sudo make uninstall
```

The Go packages are consumed through the module system rather than copied into
the installation. The example source, Dockerfiles, and example protobuf remain
in the source distribution because they are development material, not DComp
runtime data. Installing only the `dcomp` command through Go remains possible
with `go install github.com/glguida/dcomp/cmd/dcomp@v0.1.0` after that version
has been published.

Build component images using the project's ordinary Docker tooling:

```sh
docker build -t document-source:dev components/source
docker build -t document-filter:dev components/filter
```

The images named by `component.dcomp` must already exist in the local Docker
Engine. DComp does not build images, pull images, or infer build contexts.

Then operate the complete system through its one entry file:

```sh
dcomp check system.dcomp
dcomp up system.dcomp
dcomp status document-system
dcomp status --json document-system
dcomp volume --json document-system filter cache
dcomp logs document-system
dcomp logs -f document-system filter
dcomp logs -f document-system
dcomp restart document-system filter
dcomp down document-system
```

Programs embedding the CLI must first check `dcomp version --json`. The
`version`, `status`, and `volume` JSON documents include `api_version`.
Version 1 status is one JSON object with system state, network diagnostics,
component state, and each component's effective `published_ports`. A
non-operational system still emits the complete status object and exits 1.
Human-readable `status` output remains intended for terminals.

`dcomp volume [--json] SYSTEM COMPONENT LOGICAL` returns the deterministic
Docker name only after inspecting that existing local volume and verifying its
DComp owner, system, component, and logical-name labels. The version 1 JSON
object contains exactly `api_version`, `system`, `component`, `logical_name`,
and `name`. The command exits nonzero for an absent, foreign, or malformed
volume. It remains available after `down`, when the system's persistent volumes
survive but its component containers and networks no longer exist.

`up` resolves image references to immutable IDs before changing Docker. It
retains running components whose component digest is unchanged, reconciles
their link attachments, and creates or replaces only changed instances.
Adding a new consumer therefore does not restart its unchanged provider.

Running `up` again is idempotent when the resolved system is already applied.
Every apply remains crash-resumable. `restart NAME COMPONENT...` applies
Docker's single restart operation to only the selected committed component
IDs; omitting component names restarts the complete system.

`logs` reads the persisted Docker stdout and stderr streams for every verified
component container and prefixes each record with its timestamp, component,
and stream. Exact component names after the system name restrict the stream;
unknown names fail before Docker logs are opened. `-f` continues with live output. DComp does not intercept
application payloads; components choose what application activity they write
to their logs.

## Failure handling

DComp records operation intent before the first Docker mutation. Docker errors
may be ambiguous—a timeout can occur before or after an effect—so DComp never
guesses and never performs speculative rollback.

If the CLI or host stops during an operation, the operation remains explicit:

```sh
dcomp resume document-system
dcomp abort document-system
```

`resume` continues from verified Docker facts. If a create result is
unresolved, `abort` refuses without changing Docker or operation state; run
`resume` so DComp can recover or establish the exact deterministic object.
After all creates are resolved, `abort` removes verified target objects
introduced by the interrupted operation and restores the previous committed
configuration. It does not restart a previous component already retired by the
operation and does not delete persistent volumes; a later `up` reconciles
those resources.

Every mutation addresses an immutable Docker object ID after verifying image,
network, and DComp ownership labels. A same-named foreign object is an error and
is never modified.

State defaults to `${XDG_STATE_HOME}/dcomp` or
`~/.local/state/dcomp`. Set `DCOMP_STATE_ROOT` or use `--state-root` to choose
an absolute directory. The state root is bound write-once to the stable ID of
the local Docker engine; connecting that state to another local daemon is an
error. A system name is still host-wide: choosing another state root does not
create a second Docker namespace for the same `system NAME`.

## Trust and scope

DComp trusts the local host, its state directory, and the local Docker Unix
socket. DComp does not inject the socket or host-side state into components.

Each component has its own base bridge. It is internal by default and contains
only that component; `egress` makes only that base bridge externally routed.
Each link receives a separate internal bridge containing exactly its producer
and consumer. Fan-out consumers therefore do not share a network, and an
intermediate component cannot be bypassed through a system-wide bridge.
Host ports exist only for explicit `publish` directives.

Bind mounts expose exactly the named host path with the declared access mode.
Named volumes are created and verified by DComp and survive replacement,
`down`, and `abort`; DComp never removes persistent data implicitly. Use
`dcomp volume SYSTEM COMPONENT LOGICAL` to obtain a verified Docker volume name
for host-side backup or inspection tooling.

DComp deliberately does not provide multi-host scheduling, replicas, automatic
restart policy, configurable resource limits, secret injection, arbitrary
environment or entrypoint overrides, privileged containers, or transparent
traffic interception. Every component does receive the same small fixed
container policy: `no-new-privileges`, dropped `NET_RAW`, and a 2048-process
PIDs limit.

## Development

```sh
make test
make test-race
make integration
```

Generated Go protobuf files are checked in. Maintainers install the pinned
generators with `make tools`, then run `make generate`. Ordinary builds do not
require `protoc`.

Further design details:

- [Architecture](docs/architecture.md)
- [Component contract](docs/component-contract.md)
- [Lifecycle and crash consistency](docs/lifecycle.md)
- [Prior art and scope](docs/prior-art.md)
