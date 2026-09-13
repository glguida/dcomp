import assert from "node:assert/strict";
import { once } from "node:events";
import { mkdtemp, rm } from "node:fs/promises";
import { createServer as createHttpServer, request as httpRequest } from "node:http";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createServer as createNetServer } from "node:net";
import test from "node:test";

import {
  connectInput,
  inputEnv,
  inputHttpOptions,
  inputPath,
  inputTarget,
  outputConnections,
  outputEnv,
  outputPath,
  outputTarget,
  serveOutput,
  unixPath,
} from "../src/index.mjs";

test("environment names and targets", () => {
  const env = {
    DCOMP_IN_MODEL_PROVIDER: "unix:///run/dcomp/in/model-provider",
    DCOMP_OUT_FILTERED: "unix:///run/dcomp/out/filtered",
  };
  assert.equal(inputEnv("model-provider"), "DCOMP_IN_MODEL_PROVIDER");
  assert.equal(outputEnv("filtered"), "DCOMP_OUT_FILTERED");
  assert.equal(inputTarget("model-provider", env), env.DCOMP_IN_MODEL_PROVIDER);
  assert.equal(inputPath("model-provider", env), "/run/dcomp/in/model-provider");
  assert.deepEqual(inputHttpOptions("model-provider", env), {
    socketPath: "/run/dcomp/in/model-provider",
  });
  assert.equal(outputTarget("filtered", env), env.DCOMP_OUT_FILTERED);
  assert.equal(outputPath("filtered", env), "/run/dcomp/out/filtered");
});

test("invalid names and targets are rejected", () => {
  for (const name of ["", "UPSTREAM", "two_words", "1upstream", "with.dot", null]) {
    assert.throws(() => inputEnv(name), /invalid DComp interface name/u);
  }
  assert.throws(() => inputTarget("upstream", {}), /DCOMP_IN_UPSTREAM is empty/u);
  for (const target of [
    " dns:///old:50051",
    "dns:///old:50051",
    "unix:relative.sock",
    "unix:/run/dcomp/in/upstream",
    "unix:////run/dcomp/in/upstream",
    "unix://host/run/dcomp/in/upstream",
    "unix:///run/dcomp/in/../upstream",
    "unix:///run/dcomp/in/%75pstream",
    "unix:///run/dcomp/in/upstream?query=yes",
    "unix:///run/dcomp/in/upstream#fragment",
  ]) {
    assert.throws(() => unixPath(target), TypeError);
  }
});

test("connectInput returns a raw bidirectional stream", async (t) => {
  const fixture = await unixFixture(t, "input.sock");
  const accepted = once(fixture.server, "connection");
  const client = connectInput("upstream", {
    env: { DCOMP_IN_UPSTREAM: `unix://${fixture.path}` },
  });
  t.after(() => client.destroy());
  await once(client, "connect");
  const [peer] = await accepted;
  t.after(() => peer.destroy());

  const request = once(peer, "data");
  client.write("request");
  assert.equal(String((await request)[0]), "request");
  const response = once(client, "data");
  peer.write("response");
  assert.equal(String((await response)[0]), "response");
});

test("inputHttpOptions connects a native HTTP client over the input socket", async (t) => {
  const fixture = await unixFixture(t, "input-http.sock");
  fixture.server.once("connection", (connection) => {
    connection.once("data", (request) => {
      assert.match(String(request), /^GET \/health HTTP\/1\.1\r\n/u);
      connection.end(
        "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok",
      );
    });
  });
  const response = await new Promise((resolve, reject) => {
    const request = httpRequest(
      {
        ...inputHttpOptions("upstream", {
          DCOMP_IN_UPSTREAM: `unix://${fixture.path}`,
        }),
        method: "GET",
        path: "/health",
      },
      resolve,
    );
    request.once("error", reject);
    request.end();
  });
  response.setEncoding("utf8");
  let body = "";
  for await (const chunk of response) body += chunk;
  assert.equal(body, "ok");
});

test("outputConnections consumes origin and preserves application bytes", async (t) => {
  const fixture = await unixFixture(t, "output.sock");
  const controller = new AbortController();
  t.after(() => controller.abort());
  const outputs = outputConnections("filtered", {
    env: { DCOMP_OUT_FILTERED: `unix://${fixture.path}` },
    signal: controller.signal,
  });
  const next = outputs.next();
  const [producer] = await once(fixture.server, "connection");
  t.after(() => producer.destroy());

  const state = await Promise.race([
    next.then(() => "returned"),
    new Promise((resolve) => setTimeout(() => resolve("waiting"), 50)),
  ]);
  assert.equal(state, "waiting");

  producer.write("DCOMP/1 consumer.upstream\nrequest");
  const result = await next;
  assert.equal(result.done, false);
  const connection = result.value;
  t.after(() => connection.destroy());
  assert.equal(connection.origin, "consumer.upstream");
  assert.ok(connection.listenerCount("error") > 0);
  assert.equal(String(connection.read()), "request");
  const response = once(producer, "data");
  connection.write("response");
  assert.equal(String((await response)[0]), "response");
  controller.abort();
  await outputs.return();
});

test("outputConnections reconnects after an unclaimed stream closes", async (t) => {
  const fixture = await unixFixture(t, "reconnect.sock");
  const controller = new AbortController();
  t.after(() => controller.abort());
  const outputs = outputConnections("result", {
    env: { DCOMP_OUT_RESULT: `unix://${fixture.path}` },
    signal: controller.signal,
  });
  const next = outputs.next();
  const [abandoned] = await once(fixture.server, "connection");
  abandoned.destroy();
  const [claimed] = await once(fixture.server, "connection");
  t.after(() => claimed.destroy());
  claimed.write("DCOMP/1 consumer.upstream\nx");

  const result = await next;
  assert.equal(String(result.value.read()), "x");
  result.value.destroy();
  controller.abort();
  await outputs.return();
});

test("fragmented origin accepts server-first after rejecting bad headers", { timeout: 5000 }, async (t) => {
  const fixture = await unixFixture(t, "headers.sock");
  const controller = new AbortController();
  t.after(() => controller.abort());
  const outputs = outputConnections("result", {
    env: { DCOMP_OUT_RESULT: `unix://${fixture.path}` },
    signal: controller.signal,
  });
  const next = outputs.next();
  for (const header of [
    "DCOMP/1 partial",
    "DCOMP/2 a.b\n",
    "DCOMP/1 a.b.c\n",
    Buffer.from("DCOMP/1 a.\xff\n", "latin1"),
    "x".repeat(136),
    "DCOMP/1 " + "a".repeat(64) + ".b\n",
  ]) {
    const [connection] = await once(fixture.server, "connection");
    connection.end(header);
  }
  const [claimed] = await once(fixture.server, "connection");
  let returned = false;
  next.then(() => { returned = true; });
  claimed.write("DCOMP/1 " + "a".repeat(63) + ".");
  await new Promise((resolve) => setTimeout(resolve, 30));
  assert.equal(returned, false);
  claimed.write("b".repeat(63) + "\n");
  const { value: connection } = await next;
  t.after(() => connection.destroy());
  assert.equal(connection.origin, "a".repeat(63) + "." + "b".repeat(63));
  assert.equal(connection.read(), null);
  const response = once(claimed, "data");
  connection.write("hello");
  assert.equal(String((await response)[0]), "hello");
  controller.abort();
  await outputs.return();
});

test("aborting outputConnections wakes an unclaimed connection", async (t) => {
  const fixture = await unixFixture(t, "abort.sock");
  const controller = new AbortController();
  const outputs = outputConnections("result", {
    env: { DCOMP_OUT_RESULT: `unix://${fixture.path}` },
    signal: controller.signal,
  });
  const next = outputs.next();
  const [pending] = await once(fixture.server, "connection");
  t.after(() => pending.destroy());
  pending.write("DCOMP/1 partial");
  controller.abort();
  assert.deepEqual(await next, { done: true, value: undefined });
});

test("serveOutput adapts an HTTP server without binding an interface", async (t) => {
  const fixture = await unixFixture(t, "http.sock");
  const server = createHttpServer((request, response) => {
    assert.equal(request.socket.origin, "consumer.upstream");
    response.end("ok");
  });
  const controller = new AbortController();
  assert.equal(server.listening, false);
  const serving = serveOutput(server, "api", {
    env: { DCOMP_OUT_API: `unix://${fixture.path}` },
    signal: controller.signal,
  });
  const [producer] = await once(fixture.server, "connection");
  t.after(() => producer.destroy());

  let response = "";
  producer.setEncoding("utf8");
  producer.on("data", (chunk) => {
    response += chunk;
  });
  producer.end("DCOMP/1 consumer.upstream\nGET / HTTP/1.1\r\nHost: dcomp\r\nConnection: close\r\n\r\n");
  await once(producer, "close");
  assert.equal(response.split("\r\n\r\n")[1], "ok");

  controller.abort();
  await serving;
  assert.equal(server.listening, false);
});

async function unixFixture(t, name) {
  const directory = await mkdtemp(join(tmpdir(), "dcomp-node-sdk-"));
  const path = join(directory, name);
  const server = createNetServer();
  const connections = new Set();
  server.on("connection", (connection) => {
    connections.add(connection);
    connection.once("close", () => connections.delete(connection));
  });
  server.listen(path);
  await once(server, "listening");
  t.after(async () => {
    for (const connection of connections) connection.destroy();
    if (server.listening) {
      server.close();
      await once(server, "close");
    }
    await rm(directory, { recursive: true, force: true });
  });
  return { path, server };
}
