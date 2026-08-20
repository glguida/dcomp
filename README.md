<img src="docs/assets/banner.svg" alt="dcomp — Components, wired by hand. A Docker component substrate. V0.2.0, MIT, Linux, Docker Engine 25+." width="100%">

# DComp

DComp runs declarative, single-host systems of Docker components. A system
names component instances and links their typed input and output endpoints.

Version 0.2 routes every application interface through one small
`dcomp-proxy` process per running system. The proxy owns the Unix domain
sockets; components are clients that only call `connect()`. DComp bind-mounts
each container's own sockets individually and read-only, so a component cannot
see another component's endpoints or replace its mounted socket files.

DComp remains a short-lived CLI and Go library. The only long-lived host
process it creates is the per-system data-plane proxy. It is Docker-specific,
single-host, and intentionally has no cluster control plane.

## Description files

A `component.dcomp` declares an existing image and locally named interfaces:

```text
docker example/filter:1
input example.document.v1.Documents documents
output example.document.v1.Documents filtered
```

An input or output line is:

```text
input PROTOBUF_SERVICE LOCAL_NAME
output PROTOBUF_SERVICE LOCAL_NAME
```

The nominal service identifier must match across a link. DComp does not load
protobuf descriptors or inspect application messages; other stream protocols
may use the same declaration mechanism.

A `system.dcomp` creates instances and wires each input to one output:

```text
system document-system

component source components/source
component filter components/filter

link filter.documents source.documents
```

Every input must be linked exactly once. Outputs may be unused or fan out to
several inputs, and cycles are valid.

The existing bounded runtime directives remain available:

```text
bind filter ./filter.conf /etc/filter.conf ro
volume filter cache /var/lib/filter rw
args filter serve --strict
publish filter tcp 127.0.0.1 8080 8080
egress filter
```

`publish` and `egress` apply to non-DComp services a component intentionally
exposes. Declared DComp interfaces never use published TCP ports. User mounts
may not overlap the reserved `/run/dcomp` tree.

## Component contract

At runtime DComp injects one address per declared endpoint:

```text
DCOMP_IN_DOCUMENTS=unix:///run/dcomp/in/documents
DCOMP_OUT_FILTERED=unix:///run/dcomp/out/filtered
```

Environment names use the endpoint name in uppercase with `-` converted to
`_`. For every input and output, the component connects to the supplied Unix
socket and speaks its application protocol on the resulting stream.

Components must not bind or listen on these interface paths. The removed
0.1 contract—`DCOMP_LINK_*`, Docker DNS, and fixed port `50051`—is not
supported by 0.2 components.

The optional Go/gRPC helper implements the client-only output side with a
listener adapter over proxy connections:

```go
target, err := component.InputTarget("upstream")
if err != nil {
    log.Fatal(err)
}
connection, err := grpc.Dial(
    target,
    grpc.WithTransportCredentials(insecure.NewCredentials()),
)

server, err := component.NewServer(component.WithOutput("filtered"))
examplev1.RegisterDocumentsServer(server, implementation)
err = server.Serve(ctx)
```

Images must still declare a meaningful Docker `HEALTHCHECK`. The bundled
`dcomp-healthcheck --socket PATH` can verify that an orchestrator-owned socket
is mounted; applications may provide a stronger protocol-specific check.

See [Component Contract](docs/component-contract.md) for the complete image,
filesystem, shutdown, and runtime-policy rules.

## Proxy data plane

The host runtime layout defaults to `/var/run/dcomp`:

```text
/var/run/dcomp/<system>/
├── proxy.json
├── proxy.pid
├── proxy.log
├── proxy.ready
├── proxy.sock
├── in/
│   └── <instance>.<input>
└── out/
    └── <instance>.<output>
```

Use `--runtime-root DIR` or `DCOMP_RUNTIME_ROOT` when another absolute host
path is required. This tree is transient and is distinct from durable state.

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

## Build and install

Requirements are Go 1.25 or newer, Linux, and a local Docker Engine with API
1.44 or newer.

```sh
make build
make test
sudo make install
```

`make build` creates `bin/dcomp`, `bin/dcomp-proxy`, and
`bin/dcomp-healthcheck`. The proxy binary must be installed beside `dcomp`;
`DCOMP_PROXY_BINARY` may name an absolute development build instead.

Build the examples and run the Docker integration test with:

```sh
make examples
make integration
```

## CLI

```text
dcomp version [--json]
dcomp [--state-root DIR] [--runtime-root DIR] check FILE
dcomp [--state-root DIR] [--runtime-root DIR] up FILE
dcomp [--state-root DIR] [--runtime-root DIR] status [--json] NAME
dcomp [--state-root DIR] [--runtime-root DIR] ps [-a|--all] [--json] [NAME]
dcomp [--state-root DIR] [--runtime-root DIR] volume [--json] SYSTEM COMPONENT LOGICAL
dcomp [--state-root DIR] [--runtime-root DIR] logs [-f|--follow] NAME [COMPONENT...]
dcomp [--state-root DIR] [--runtime-root DIR] attach [--ready-fd FD] SYSTEM COMPONENT
dcomp [--state-root DIR] [--runtime-root DIR] restart NAME [COMPONENT...]
dcomp [--state-root DIR] [--runtime-root DIR] down NAME
dcomp [--state-root DIR] [--runtime-root DIR] resume NAME
dcomp [--state-root DIR] [--runtime-root DIR] abort NAME
dcomp [--state-root DIR] [--runtime-root DIR] inspect-image IMAGE
```

`check` parses and resolves images without changing Docker or host state.
`up` starts the proxy, creates dedicated bridges only for components that
declare `egress`, mounts endpoint sockets, and starts components. Other
components run with Docker network mode `none`. `down` stops and removes
components first, then stops the proxy and removes transient egress networks;
named volumes survive.

`status` reports proxy readiness and connection counts alongside component
and network diagnostics. `logs` includes proxy records under the source name
`@proxy`; pass `@proxy` explicitly to select only that stream. Machine-readable
documents use API version 2.

Lifecycle operations are durable. If a command is interrupted, `resume`
continues its exact recorded operation and `abort` removes verified new
resources when safe. See [Lifecycle](docs/lifecycle.md).

## State and identity

Durable state defaults to `$XDG_STATE_HOME/dcomp` or
`$HOME/.local/state/dcomp`; `--state-root` and `DCOMP_STATE_ROOT` override it.
The root is bound to one Docker Engine ID.

State records immutable container and network IDs plus the proxy instance ID,
PID, wiring digest, control socket, log path, and runtime directory. Proxy
shutdown verifies the control-socket identity before signalling a recorded
PID, avoiding unsafe PID-only process control.

Changing only one image can retain unrelated containers and the existing
proxy. Changing endpoints or links replaces the proxy and component
containers, because individual Docker socket bind mounts retain the old socket
inode. Dynamic rewiring without component restart is deliberately outside the
0.2 scope.

## Migration from 0.1.x

0.2 is wire-incompatible with 0.1 components.

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

The `system.dcomp` and `component.dcomp` grammars themselves are unchanged.
Per-link Docker bridges and `DCOMP_LINK_*` are removed completely.

## Deliberate limits

DComp 0.2 provides no multi-host overlay, replicas, automatic failover,
encryption, dynamic rewiring, arbitrary Docker option passthrough, secret
store, image build/pull workflow, or long-lived control-plane daemon. Unix
socket permissions are the local trust boundary; optional peer-credential
policy can be added without changing the component address contract.

The project is licensed under the [MIT License](LICENSE).
