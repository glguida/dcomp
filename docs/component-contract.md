# Component contract

This document defines the DComp 0.3.0 image and process contract. It is
wire-incompatible with 0.1.x.

## Descriptor

Each component directory contains `component.dcomp`:

```text
docker registry.example/document-filter:1.4
input example.document.v1.Documents documents
output example.document.v1.Documents filtered
```

The grammar is line-oriented. Blank lines and text after `#` are ignored.
There is one `docker IMAGE` directive followed by any number of input/output
declarations:

```text
input PROTOBUF_SERVICE LOCAL_NAME
output PROTOBUF_SERVICE LOCAL_NAME
```

Local names begin with a lowercase letter and contain lowercase letters,
digits, or hyphens. Input and output names are separate namespaces. The service
identifier is nominal: linked endpoints must use the same identifier, but
DComp does not load descriptors or constrain the stream protocol.

## Runtime addresses

For every declared input DComp injects and mounts:

```text
DCOMP_IN_DOCUMENTS=unix:///run/dcomp/in/documents
/run/dcomp/in/documents
```

For every declared output:

```text
DCOMP_OUT_FILTERED=unix:///run/dcomp/out/filtered
/run/dcomp/out/filtered
```

The environment spelling is deterministic:

1. use `DCOMP_IN_` or `DCOMP_OUT_`;
2. uppercase the local endpoint name; and
3. replace `-` with `_`.

Only the component's own endpoint socket files are bind-mounted. These mounts
are read-only at the filesystem layer while the socket streams remain
bidirectional. The component does not receive the host runtime directory or
sockets belonging to peers.

The component must call `connect()` for every interface it uses. It must never
call `bind()` or `listen()` on these paths. The proxy is the only listener and
the supplied URI is the only stable endpoint identity.

Inputs and outputs are symmetric at the transport layer. An input component
normally acts as an application client on its connected stream. An output
component normally acts as an application server on its connected stream. A
server framework can use a listener adapter that yields connections made to
the proxy rather than binding a local address.

## Connection behavior

The proxy pairs one input connection with one connection from the linked
output. It sends the connection-origin header to the output, then forwards an
opaque, ordered application byte stream in both directions.

Components MUST treat connection loss as reconnectable. Consumers MUST retry
failed connects with bounded backoff. Producers MUST detect dying dialed
streams and reconnect to maintain their output pool. Link removal during
minimal reconciliation deliberately closes both sides, just as a peer restart
already can. A producer output may receive several independent connections
when it fans out or when clients reconnect; it must not assume that one output
has exactly one lifetime connection.

DComp does not prescribe deadlines, request framing, retry semantics, or
application-level health. Those belong to the selected protocol.

### Connection origin

Upon pairing, the proxy writes exactly one ASCII header to the output stream:

```text
DCOMP/1 <component>.<input-endpoint>\n
```

Here `\n` denotes a single LF byte, not two literal characters. Both names
follow the ordinary DComp name grammar, `[a-z][a-z0-9-]{0,62}`. The existing
63-byte name limits bound the complete header to 136 bytes, including its
prefix and LF. There is no header on the input stream.

The origin identifies the immediate consumer component instance and its input
endpoint in this system. The proxy derives it from the socket's configured
wiring, never from consumer-supplied bytes. It is not an end-user identity or
a propagated identity from earlier calls. Components may use it for access
checks or ignore it. Its trust depends on DComp's socket-mount isolation;
host operators with access to those sockets can impersonate a component.

Output SDK adapters consume and validate the header before handing the socket
to application code. An invalid, oversized, or incomplete header is discarded
with its connection, and acquisition retries. Adapters do not guess whether an
older raw stream is a header. Custom raw-output adapters must implement the
same framing and must leave all subsequent bytes untouched.

The header arrives as soon as a consumer is paired, even before it sends any
application data. Both client-first and server-first protocols are supported.
The header is excluded from application byte metrics.

## Language helpers

DComp ships dependency-free Python and Node.js packages in `sdk/python` and
`sdk/node`, alongside the Go package. The common layer is deliberately small:

- derive and validate `DCOMP_IN_*` and `DCOMP_OUT_*` names;
- require a canonical absolute `unix:///` target;
- expose either the URI or native Unix socket path; and
- connect raw streams without binding an interface path.

Python's `DialListener` returns claimed output sockets through the conventional
`(connection, address)` accept shape. Node's `outputConnections()` yields the
same kind of claimed sockets, while `serveOutput()` injects them into a native
`net.Server` or `http.Server`. The Node helper also provides
`inputHttpOptions()` for HTTP/1.1 clients such as ConnectRPC's Node transport.

These helpers do not define an application protocol or depend on protobuf,
gRPC, ConnectRPC, or Cyclo. Framework-specific packages remain free to layer
their own health, reflection, routing, and graceful-shutdown behavior on top.
The listener-style adapters wait for the proxy's origin header, not an
application byte. Python's `accept()` returns `(socket, "component.endpoint")`.
Node sockets yielded by `outputConnections()` or passed to `serveOutput()`
have a read-only `origin` string property; HTTP handlers can read
`request.socket.origin`. Go's `component.Server` exposes the origin through
the standard gRPC `peer.FromContext(ctx)`: `peer.Addr.String()` is the identity
and `peer.Addr.Network()` is `"dcomp"`. Caller-supplied `ServeListener`
listeners retain their own peer-address semantics.

The raw `connect_output()`/`connectOutput()` helpers do not consume the header.
Prefer the listener-style adapters unless implementing the transport contract.

See the complete [Python helper guide](../sdk/python/README.md) and
[Node.js helper guide](../sdk/node/README.md) for raw connections, framework
boundaries, reconnection, multiple outputs, and shutdown examples.

### Go/gRPC

The `component` package validates addresses and adapts a gRPC server to the
client-only output contract.

An input client:

```go
target, err := component.InputTarget("upstream")
if err != nil {
    log.Fatal(err)
}
connection, err := grpc.Dial(
    target,
    grpc.WithTransportCredentials(insecure.NewCredentials()),
)
```

An output server:

```go
server, err := component.NewServer(component.WithOutput("filtered"))
if err != nil {
    log.Fatal(err)
}
examplev1.RegisterDocumentsServer(server, implementation)
if err := server.Serve(ctx); err != nil {
    log.Fatal(err)
}
```

`Server` connects to the output Unix socket, waits for the proxy's connection-origin
header, and presents the resulting connection to `grpc.Server`. It includes
standard gRPC health and reflection services and performs bounded graceful
shutdown. Multiple `WithOutput` options are supported, although every
registered gRPC service is then available on each configured output.

`ServeListener` remains available for tests and custom embedding. DComp-managed
interface paths must still follow the client-only rule.

### Python

```python
from dcomp_component import DialListener, connect_input

upstream = connect_input("upstream")

with DialListener("filtered") as listener:
    connection, _address = listener.accept()
    serve_protocol(connection)
```

### Node.js

```js
import { createServer } from "node:http";
import { inputHttpOptions, serveOutput } from "@dcomp/component";

const upstream = inputHttpOptions("upstream");
const server = createServer(handler);
await serveOutput(server, "filtered", { signal });
```

## Image requirements

The referenced image must already exist locally or be resolvable by Docker.
DComp does not build or pull it. The image must:

- contain a long-running entrypoint for a managed component;
- declare a meaningful OCI `HEALTHCHECK`;
- receive and act on Docker's stop signal; and
- explicitly mount every image-declared OCI `VOLUME` through a system
  `bind` or `volume` directive.

The image may run under any UID. Interface sockets are connectable by
non-root component users. Shell entrypoints should use `exec` so the
application receives termination signals.

The bundled health checker can validate an endpoint mount without consuming a
proxy connection:

```dockerfile
HEALTHCHECK CMD ["/dcomp-healthcheck", "--socket", "/run/dcomp/out/filtered"]
```

Applications are encouraged to use a stronger check when they can expose one
without binding a declared interface path. Docker reports stopped containers
independently of health-check results.

## Runtime policy

System files may add bounded policy after the component declaration:

```text
bind filter ./config.json /etc/filter/config.json ro
volume filter cache /var/lib/filter rw
args filter serve --strict
publish filter tcp 127.0.0.1 8080 8080
egress filter
```

Bind sources are canonical existing host paths. Named volumes are
system/component scoped and persist through replacement and `down`. Mount
targets must be absolute, clean, non-overlapping, and outside `/run/dcomp`.

`args` replaces image command arguments without changing the entrypoint.
There is no shell expansion.

`publish` and `egress` are for an additional, explicitly component-owned
service. They do not expose declared DComp inputs or outputs. A published port
requires `egress` because components otherwise use Docker network mode `none`.

Every container is created with:

- an init process;
- restart policy `no`;
- `no-new-privileges`;
- capability `NET_RAW` dropped;
- a 2048-process PIDs limit;
- open, non-TTY standard input for `dcomp attach`; and
- Docker network mode `none`, or its one dedicated bridge when `egress` is
  declared; and
- only typed user mounts and endpoint socket mounts.

DComp supplies no arbitrary environment, entrypoint, user, capability,
privilege, device, Docker-socket, mount-propagation, or resource-limit escape
hatch.

## Shutdown and restart

On termination a component should stop accepting new work, drain outstanding
requests within a bounded interval, and exit before Docker's stop timeout.

`dcomp restart SYSTEM COMPONENT` restarts only that existing container. The
proxy and other components keep running; output and input implementations must
reconnect normally.

## Removed 0.1 contract

The following behavior is invalid:

```text
DCOMP_LINK_UPSTREAM=dns:///provider:50051
listen 0.0.0.0:50051
```

There are no `DCOMP_LINK_*` variables, fixed interface ports, per-link Docker
bridges, or direct Docker-DNS application calls. Components using any of those
assumptions must be rebuilt for 0.3.0.
