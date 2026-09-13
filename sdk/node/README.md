# @dcomp/component for Node.js

This dependency-free package implements DComp 0.3.0's component-facing Unix
socket contract. It works with raw streams and Node's native HTTP/1.1 server;
it does not depend on protobuf, gRPC, ConnectRPC, or Cyclo.

Install it from a DComp checkout with `npm install /path/to/dcomp/sdk/node`.
The directory is also a normal npm package for release publishing. Node.js 20
or newer is required.

## Raw inputs

`connectInput()` returns a normal `net.Socket` and performs one connection
attempt:

```js
import { once } from "node:events";
import { connectInput } from "@dcomp/component";

const connection = connectInput("upstream");
await once(connection, "connect");
connection.write(request);
```

Connection failures follow the normal Node stream contract and emit `error`.
Application-level retry, request framing, deadlines, and reconnect policy stay
with the component.

## ConnectRPC inputs

ConnectRPC's Node HTTP/1.1 transport accepts the socket path in its native
request options:

```js
import { createClient } from "@connectrpc/connect";
import { createConnectTransport } from "@connectrpc/connect-node";
import { inputHttpOptions } from "@dcomp/component";

import { Upstream } from "./gen/upstream_pb.js";

const transport = createConnectTransport({
  baseUrl: "http://dcomp",
  httpVersion: "1.1",
  nodeOptions: inputHttpOptions("upstream"),
});
const upstream = createClient(Upstream, transport);
```

The URL supplies the HTTP authority and request path; `socketPath` selects the
actual DComp Unix connection.

## ConnectRPC outputs

`serveOutput()` feeds claimed proxy connections into a native `http.Server`.
It replaces the old `server.listen()` call:

```js
import { createServer } from "node:http";

import { connectNodeAdapter } from "@connectrpc/connect-node";
import { serveOutput } from "@dcomp/component";

import { Example } from "./gen/example_pb.js";

const shutdown = new AbortController();
const server = createServer(
  connectNodeAdapter({
    connect: true,
    grpc: false,
    grpcWeb: false,
    shutdownSignal: shutdown.signal,
    routes(router) {
      router.service(Example, implementation);
    },
  }),
);

process.once("SIGTERM", () => shutdown.abort());
process.once("SIGINT", () => shutdown.abort());
await serveOutput(server, "api", { signal: shutdown.signal });
```

Do not call `server.listen()` for the DComp interface. The helper repeatedly
connects to `DCOMP_OUT_API`, waits until the proxy pairs the stream with a real
consumer, consumes the proxy header, and emits the server's normal `connection`
event. The socket's read-only `origin` string identifies the immediate consumer
as `component.input-endpoint`; HTTP handlers can read `request.socket.origin`. The ConnectRPC adapter then processes the HTTP/1.1 request normally.

Aborting `serveOutput()` stops acquisition of new connections and wakes an
unclaimed connection. Connections already handed to the server remain under
the application's request-draining and shutdown policy.

To expose the same server on several declared outputs, run one acquisition loop
per output:

```js
await Promise.all(
  ["component", "provider"].map((name) =>
    serveOutput(server, name, { signal: shutdown.signal }),
  ),
);
```

Every handler registered on that server is then reachable through each output.
Use separate servers when outputs must expose different routes.

## Raw outputs

`outputConnections()` is the lower-level async generator used by
`serveOutput()`:

```js
import { outputConnections } from "@dcomp/component";

for await (const connection of outputConnections("events", {
  signal: shutdown.signal,
})) {
  dispatch(connection);
}
```

It retries proxy connection failures, discards producer streams that close
before a consumer appears, and yields one paused `net.Socket` per real consumer
with application bytes untouched and its origin available as `connection.origin`.
The receiver owns each yielded socket.

The claimed-connection adapters support both client-first and server-first
protocols: they wait for the proxy header, not an application byte.

`connectOutput()` is a raw transport helper: the returned socket includes
the proxy header and has no `origin` property. Custom adapters using it must
consume the header according to the
[connection-origin contract](../../docs/component-contract.md#connection-origin).
Ordinary servers should use `outputConnections()` or `serveOutput()`.

## API summary

- `inputEnv(name)` / `outputEnv(name)` return the contract environment name.
- `inputTarget(name)` / `outputTarget(name)` return a validated `unix:///`
  target.
- `inputPath(name)` / `outputPath(name)` return the native socket path.
- `unixPath(target)` validates a target and returns its path.
- `inputHttpOptions(name)` returns a frozen `{ socketPath }` request option.
- `connectInput(name, options)` / `connectOutput(name, options)` perform one raw
  connection.
- `outputConnections(name, options)` acquires claimed output sockets with
  retry and optional `AbortSignal` cancellation.
- `serveOutput(server, name, options)` feeds those sockets into a native
  `net.Server` or HTTP/1.1 `http.Server`.

Target helpers accept an explicit environment object. Connection helpers use
`options.env`; this is useful in tests without mutating `process.env`.

Run the package tests from its directory:

```sh
npm test
```
