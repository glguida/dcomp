<img src="docs/assets/banner.svg" alt="dcomp — Components, wired by hand. A Docker component substrate. V0.3.1, MIT, Linux, Docker Engine 25+." width="100%">

# DComp

DComp runs declarative, single-host systems of Docker components. A system
names component instances and links their typed input and output endpoints.

DComp separates programming in the large from programming in the small. The
system description is the architecture: named instances and typed, explicitly
linked endpoints. Components are the implementation: ordinary OCI images
speaking their own protocols over connected byte streams.

The architecture is under direct control. A system runs only by applying its
description. A component reaches other components only through its declared
endpoints, and external reach — binds, published ports, egress — is explicit
policy in the same description. Every change is a recorded lifecycle
operation, so the description and the running system cannot silently diverge.

DComp routes every application interface through one small
`dcomp-proxy` process per running system. The proxy owns the Unix domain
sockets; components are clients that only call `connect()`. DComp bind-mounts
each container's own sockets individually and read-only, so a component cannot
see another component's endpoints or replace its mounted socket files.

DComp remains a short-lived CLI and Go library. The only long-lived host
process it creates is the per-system data-plane proxy. It is Docker-specific,
single-host, and intentionally has no cluster control plane.

## Description files

A `component.dcomp` declares an existing image and its locally named
interfaces. A `system.dcomp` creates instances and links inputs to compatible
outputs, directly or through system-global interface names:

```text
# components/filter/component.dcomp
docker example/filter:1
input example.document.v1.Documents documents
output example.document.v1.Documents filtered

# system.dcomp
system document-system
component source components/source
component filter components/filter
link filter.documents source.documents
```

The nominal service identifier must match across a link, but DComp does not
inspect schemas or payloads. System files can additionally declare bounded
binds, persistent volumes, arguments, published non-DComp ports, and egress.
See the [CLI and description-file reference](docs/reference.md) for the exact
grammar, path rules, validation, and command behavior.

## Incremental composition and global interfaces

Independent programs can add and remove their own component instances in one
running system. A global name identifies one typed output interface, within
that system. `@provider_endpoint` follows reassignment; `provider.api` always
names that specific component output. Globals cannot point to other globals.

```text
# system.dcomp
system agents
component provider components/provider
component team components/team
global provider_endpoint example.Provider provider.api
link team.inference @provider_endpoint
```

A global can be declared unbound by omitting its target. Unconnected inputs and
empty systems are valid. An input with no route, or a global target that is
unbound, closes connections immediately rather than queuing for a provider.

```sh
# Add the first component; creates the system when absent.
dcomp add-component agents provider components/provider
dcomp assign-global agents provider_endpoint provider.api
# Consumers keep a symbolic reference to the global name.
dcomp add-component --link inference=@provider_endpoint agents team components/team
# Insert a wrapper, binding its upstream directly to the previous endpoint.
dcomp add-component --link upstream=provider.api agents wrapper components/wrapper
dcomp assign-global agents provider_endpoint wrapper.api
# Removing the wrapper unbinds the name; team stays linked to it.
dcomp rm-component agents wrapper
# Restore service without editing the team's wire.
dcomp assign-global agents provider_endpoint provider.api
```

`mod-wire SYSTEM COMPONENT.INPUT COMPONENT.OUTPUT|@GLOBAL|-` replaces one wire
or disconnects it with `-`. `assign-global SYSTEM NAME -` unbinds a name. To
create an initially unbound name use `assign-global --service TYPE SYSTEM NAME -`.
Existing names retain their service type when reassigned.

Incremental commands wait for the system lock, read committed state, and apply
one change through the durable lifecycle journal. Existing images are pinned
to their recorded IDs. Interrupted operations must be resumed or aborted before
another incremental edit. The Go `lifecycle.Controller` exposes the same
operations and an `Edit` callback for a batch of composition changes applied
as one recoverable operation. Docker changes are not an atomic transaction;
an interrupted apply requires `resume` or `abort`.

Version 0.3.1 adds `add-component --user UID:GID` and the system directive
`user COMPONENT UID:GID` for running a container with an explicit identity.
Incremental edits preserve existing mounted containers when an old host bind
path has moved; new or recreated containers still require valid source paths.

Reassignment disconnects streams whose concrete route changed; subsequent
connections use the new output. Unaffected streams and endpoint socket inodes
survive. Removing a component drops its direct wires, unbinds its global
exports, and retains other components and persistent volumes. The last
component may be removed without discarding the global namespace.

See [the reference](docs/reference.md) for command options and the proxy's
incremental control messages.

## Component contract

At runtime DComp injects one address per declared endpoint:

```text
DCOMP_IN_DOCUMENTS=unix:///run/dcomp/in/documents
DCOMP_OUT_FILTERED=unix:///run/dcomp/out/filtered
```

Environment names use the endpoint name in uppercase with `-` converted to
`_`. For every input and output, the component connects to the supplied Unix
socket. On outputs, the SDK consumes the proxy's connection-origin header;
the application then speaks its protocol on the resulting stream.

Components must not bind or listen on these interface paths. The removed
0.1 contract—`DCOMP_LINK_*`, Docker DNS, and fixed port `50051`—is not
supported by 0.3.0 components.

The repository ships helpers for Go/gRPC, dependency-free
[Python](sdk/python/README.md), and dependency-free
[Node.js](sdk/node/README.md). All validate the same address contract and
connect as clients; none binds a DComp interface path. The language guides
describe their framework boundaries and shutdown/reconnection behavior.

Images must still declare a meaningful Docker `HEALTHCHECK`. The bundled
`dcomp-healthcheck --socket PATH` can verify that an orchestrator-owned socket
is mounted; applications may provide a stronger protocol-specific check.

See [Component contract](docs/component-contract.md) for the complete image,
filesystem, shutdown, and runtime-policy rules.

## Proxy data plane

The host runtime layout defaults to `<state-root>/run`, so selecting another
state root also selects an independent proxy and socket tree by default:

```text
<state-root>/run/<system>/
├── proxy.json
├── proxy.pid
├── proxy.log
├── proxy.ready
├── proxy.ownership.json
├── proxy.sock
├── .a-*                    # private socket-identity anchors
├── in/
│   └── <instance>.<input>
└── out/
    └── <instance>.<output>
```

Use `--runtime-root DIR` or `DCOMP_RUNTIME_ROOT` when another absolute host
path is required. The runtime tree is reconstructible even though its default
location is inside the durable state root. The ownership ledger and anchors
are proxy-internal recovery artifacts; components never mount them.

Inside a component, only its own endpoints are mounted:

```text
/run/dcomp/
├── in/documents
└── out/filtered
```

For each consumer connection, the proxy takes one connection from the linked
producer output pool and copies bytes in both directions. Fan-out uses
independent stream pairs rather than broadcasting or merging bytes, so
bidirectional protocols such as HTTP/2 and gRPC remain valid. Disconnected
components may reconnect without restarting the proxy.

The proxy creates every listener before reporting readiness. `dcomp up` waits
for that readiness before creating or starting component containers.

## Documentation

- [CLI and description-file reference](docs/reference.md)
- [Machine API 2](docs/machine-api.md)
- [System view and dashboard](docs/view.md)
- [Component contract](docs/component-contract.md)
- [Architecture](docs/architecture.md)
- [Lifecycle and recovery](docs/lifecycle.md)
- [Worked examples](examples/README.md)
- [Python component helpers](sdk/python/README.md)
- [Node.js component helpers](sdk/node/README.md)
- [Prior art](docs/prior-art.md)

## Build and install

Requirements are Go 1.25 or newer, Linux, and a local Docker Engine with API
1.44 or newer. The complete test suite additionally uses Python 3.10 or newer
and Node.js 20 or newer for the component SDKs.

```sh
make build
make test
sudo make install
```

Run only the Python and Node.js SDK tests with `make sdk-test`.

`make build` creates `bin/dcomp`, `bin/dcomp-proxy`, and
`bin/dcomp-healthcheck`. The proxy binary must be installed beside `dcomp`;
`DCOMP_PROXY_BINARY` may name an absolute development build instead.

Build the examples and run the Docker integration test with:

```sh
make examples
make integration
```

## CLI

The common workflow is deliberately small:

```sh
dcomp check system.dcomp
dcomp up system.dcomp
dcomp status document-system
dcomp view system.dcomp
dcomp dashboard
dcomp logs -f document-system
dcomp restart document-system filter
dcomp down document-system
```

Lifecycle operations are durable. If a command is interrupted, `resume`
continues its exact recorded operation and `abort` removes verified new
resources when safe. The [CLI and description-file reference](docs/reference.md)
covers every command, flag, exit status, environment override, and authored
directive. Automation should use [Machine API 2](docs/machine-api.md).

`dcomp view [--json] FILE|NAME` describes a system's components, declared
interfaces, links, and external routes. `dcomp dashboard` serves the same read-only
documents with a bundled topology viewer. See [System view](docs/view.md).

<img src="docs/assets/system-view.png" alt="The dashboard viewer drawing a system as an isometric board: wireframe components, routed link traces, hazard tape on components with external reach, and a published entry on the boundary" width="100%">

## State and identity

Durable state defaults to `$XDG_STATE_HOME/dcomp` or
`$HOME/.local/state/dcomp`; `--state-root` and `DCOMP_STATE_ROOT` override it.
The root is bound to one Docker Engine ID and defines its own Docker resource
namespace. Containers, egress networks, and named volumes use physical names
beginning `dcomp.<namespace>.`; the namespace is the first 128 bits of SHA-256
over the cleaned absolute state-root path. Two state roots can therefore run
the same system name independently on one Docker Engine.

State records immutable container and network IDs plus the proxy instance ID,
PID, wiring digest, control socket, log path, and runtime directory. An
incomplete apply operation also journals its complete target wiring; any
operation may journal the exact network endpoint identity that must be removed
after its container. Proxy shutdown and resync verify the control-socket
identity before signalling a recorded PID, avoiding unsafe PID-only process
control.

Changing only one image can retain unrelated containers and the existing
proxy. Adding or removing a component publishes or removes only its endpoint
sockets. Relinking existing endpoints performs no container operation. The
proxy never rebinds a surviving endpoint identity, so Docker's per-file socket
mounts retain the same inode and established streams on surviving links remain
open.

## Migration to 0.3.0

Before upgrading, use the old binary to run `dcomp down NAME` for each system.
Install the matching 0.3.0 CLI and proxy, then apply the systems again. Named
volumes and the state-root namespace are preserved; do not delete the state
root. Durable state format 5 is written by 0.3.0; format 4 remains readable.
Older binaries reject format 5. Proxy configuration/status format 4 and control
protocol 2 require a matching proxy; an older live proxy is rejected before
mutation.

The component stream contract is unchanged from 0.2.2. When upgrading from
0.2.1, rebuild output components with the current SDK: the proxy now sends
`DCOMP/1 component.input\n` before output application bytes, and SDK adapters
consume it. Input clients and application payloads are unchanged.

Existing direct-link system files continue to work. `up` remains an authoritative
whole-system apply; incremental commands modify the recorded system without
requiring or rewriting its source file.

## Migration from 0.2.0 to 0.2.1

In this earlier upgrade, the component addresses and wire contract were
unchanged, so existing 0.2 component images and SDKs remained valid. Host state
and live proxies are not
upgrade-compatible: DComp 0.2.1 rejects 0.2.0 durable state, engine bindings,
proxy configurations, and control protocols without mutating them.

Before installing 0.2.1, run `dcomp down NAME` for every system with the 0.2.0
binary, then remove the old DComp state root. Reusing the same cleaned state-
root path preserves its deterministic Docker namespace; `down` does not remove
declared named volumes. Back up important volume data before an upgrade as
usual. A 0.2.0 system left running must be shut down with the 0.2.0 binary.

## Migration from 0.1.x

The 0.1-to-0.2 migration changed the component wire contract as follows. For
upgrading to the current release, also follow [Migration to 0.3.0](#migration-to-030).

1. Run `dcomp down NAME` with the 0.1 binary before upgrading. Version 0.2
   rejects the old durable-state format instead of guessing ownership.
2. Remove component listeners on `0.0.0.0:50051`.
3. Replace `DCOMP_LINK_<INPUT>` reads with `DCOMP_IN_<INPUT>` and connect to
   the supplied Unix address.
4. Make each declared output connect to `DCOMP_OUT_<OUTPUT>` and serve its
   protocol on that connected stream. Go/gRPC components can use
   `component.WithOutput`.
5. Update health checks that assumed local TCP port `50051`.
6. Install `dcomp-proxy` beside the `dcomp` executable.

That migration kept the `system.dcomp` and `component.dcomp` grammars unchanged.
Version 0.3 adds optional global declarations and symbolic link targets.
Per-link Docker bridges and `DCOMP_LINK_*` are removed completely.

## Deliberate limits

DComp 0.3.1 provides no multi-host overlay, replicas, automatic failover,
encryption, arbitrary Docker option passthrough, secret store, image build/pull
workflow, or long-lived control-plane daemon. Unix
socket permissions are the local trust boundary; optional peer-credential
policy can be added without changing the component address contract.

The project is licensed under the [MIT License](LICENSE).
