# @dcomp/component for Node.js

This dependency-free package implements DComp 0.2's component-facing Unix
socket contract. It works with raw streams and Node's native HTTP server; it
does not depend on protobuf, gRPC, ConnectRPC, or Cyclo.

Install it from a DComp checkout with `npm install /path/to/dcomp/sdk/node`; it
is also a normal npm package for release publishing.

For a raw input stream:

```js
import { connectInput } from "@dcomp/component";

const connection = connectInput("upstream");
connection.write(request);
```

Node HTTP clients use the socket path as a request option. ConnectRPC's Node
HTTP/1.1 transport accepts the same option:

```js
import { inputHttpOptions } from "@dcomp/component";
import { createConnectTransport } from "@connectrpc/connect-node";

const transport = createConnectTransport({
  baseUrl: "http://dcomp",
  httpVersion: "1.1",
  nodeOptions: inputHttpOptions("upstream"),
});
```

`serveOutput()` adapts a native `net.Server` or `http.Server`. The server does
not call `listen()` on the DComp interface:

```js
import { createServer } from "node:http";
import { serveOutput } from "@dcomp/component";

const controller = new AbortController();
const server = createServer((_request, response) => response.end("ok"));
await serveOutput(server, "api", { signal: controller.signal });
```

The helper repeatedly connects to `DCOMP_OUT_API`, waits until the proxy pairs
the stream with a real consumer, restores the first byte, and emits the normal
Node `connection` event. This adapter is intended for client-first protocols
such as HTTP and gRPC. `outputConnections()` exposes claimed sockets directly;
a server-first protocol can use `connectOutput()` and manage its connection
pool explicitly.

Aborting `serveOutput()` stops acquisition of new connections and wakes an
unclaimed connection. Connections already handed to the server remain under
the server's shutdown policy.

Run the package tests from its directory:

```sh
npm test
```
