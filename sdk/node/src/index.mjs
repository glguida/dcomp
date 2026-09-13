import { createConnection } from "node:net";
import { posix } from "node:path";

const ENDPOINT_NAME = /^[a-z][a-z0-9-]*$/u;
const DEFAULT_RETRY_DELAY_MS = 25;
const MAX_ORIGIN_HEADER = 8 + 63 + 1 + 63 + 1; // Prefix, two names, dot, LF.
const ORIGIN_HEADER = /^DCOMP\/1 ([a-z][a-z0-9-]{0,62}\.[a-z][a-z0-9-]{0,62})\n$/u;
const ignoreClaimedConnectionError = () => {};

export function inputEnv(name) {
  return endpointEnv("DCOMP_IN_", name);
}

export function outputEnv(name) {
  return endpointEnv("DCOMP_OUT_", name);
}

export function inputTarget(name, env = process.env) {
  return endpointTarget(inputEnv(name), "input", name, env);
}

export function outputTarget(name, env = process.env) {
  return endpointTarget(outputEnv(name), "output", name, env);
}

export function inputPath(name, env = process.env) {
  return unixPath(inputTarget(name, env));
}

export function outputPath(name, env = process.env) {
  return unixPath(outputTarget(name, env));
}

export function unixPath(target) {
  if (typeof target !== "string" || !target || target.trim() !== target) {
    throw new TypeError("DComp target must be a non-empty canonical string");
  }
  let parsed;
  try {
    parsed = new URL(target);
  } catch (error) {
    throw new TypeError("DComp target must contain an absolute unix:/// path", {
      cause: error,
    });
  }
  if (
    !target.startsWith("unix:///") ||
    parsed.protocol !== "unix:" ||
    parsed.host ||
    target !== `unix://${parsed.pathname}` ||
    !parsed.pathname.startsWith("/") ||
    parsed.pathname.startsWith("//") ||
    posix.normalize(parsed.pathname) !== parsed.pathname ||
    parsed.search ||
    parsed.hash ||
    parsed.pathname.includes("%") ||
    parsed.pathname.includes("\0")
  ) {
    throw new TypeError("DComp target must contain an absolute unix:/// path");
  }
  return parsed.pathname;
}

export function inputHttpOptions(name, env = process.env) {
  return Object.freeze({ socketPath: inputPath(name, env) });
}

export function connectInput(name, { env = process.env, ...options } = {}) {
  return createConnection({
    allowHalfOpen: true,
    ...options,
    path: inputPath(name, env),
  });
}

export function connectOutput(name, { env = process.env, ...options } = {}) {
  return createConnection({
    allowHalfOpen: true,
    ...options,
    path: outputPath(name, env),
  });
}

export async function* outputConnections(
  name,
  {
    env = process.env,
    signal,
    retryDelayMs = DEFAULT_RETRY_DELAY_MS,
  } = {},
) {
  const path = outputPath(name, env);
  validateSignal(signal);
  validateRetryDelay(retryDelayMs);
  while (!signal?.aborted) {
    const connection = await claimedConnection(path, signal);
    if (connection !== undefined) {
      yield connection;
      continue;
    }
    if (!(await retry(signal, retryDelayMs))) return;
  }
}

export async function serveOutput(server, name, options = {}) {
  if (
    server === null ||
    typeof server !== "object" ||
    typeof server.emit !== "function" ||
    typeof server.listenerCount !== "function"
  ) {
    throw new TypeError("server must be a Node.js connection server");
  }
  if (server.listenerCount("connection") === 0) {
    throw new TypeError("server must handle Node.js connection events");
  }
  for await (const connection of outputConnections(name, options)) {
    try {
      server.emit("connection", connection);
      connection.resume();
    } catch (error) {
      connection.destroy();
      throw error;
    }
  }
}

function endpointEnv(prefix, name) {
  if (typeof name !== "string" || !ENDPOINT_NAME.test(name)) {
    throw new TypeError(
      `invalid DComp interface name ${JSON.stringify(name)}: ` +
        "use lower-case letters, digits, and hyphens",
    );
  }
  return prefix + name.replaceAll("-", "_").toUpperCase();
}

function endpointTarget(variable, direction, name, env) {
  if (env === null || (typeof env !== "object" && typeof env !== "function")) {
    throw new TypeError("env must provide component environment variables");
  }
  const target = env[variable];
  if (typeof target !== "string" || !target.trim()) {
    throw new TypeError(
      `required DComp ${direction} ${JSON.stringify(name)} is not configured ` +
        `(${variable} is empty)`,
    );
  }
  try {
    unixPath(target);
  } catch (error) {
    throw new TypeError(`${variable} must contain an absolute unix:/// path`, {
      cause: error,
    });
  }
  return target;
}

function validateSignal(signal) {
  if (
    signal !== undefined &&
    (signal === null ||
      typeof signal !== "object" ||
      typeof signal.aborted !== "boolean" ||
      typeof signal.addEventListener !== "function")
  ) {
    throw new TypeError("signal must be an AbortSignal");
  }
}

function validateRetryDelay(value) {
  if (!Number.isSafeInteger(value) || value < 0) {
    throw new TypeError("retryDelayMs must be a non-negative safe integer");
  }
}

function claimedConnection(path, signal) {
  if (signal?.aborted) return Promise.resolve(undefined);
  return new Promise((resolve) => {
    const connection = createConnection({ allowHalfOpen: true, path });
    let settled = false;
    let header = Buffer.alloc(0);

    const finish = (result) => {
      if (settled) return;
      settled = true;
      connection.off("data", onData);
      connection.off("end", onEnd);
      connection.off("close", onClose);
      connection.off("error", onError);
      signal?.removeEventListener("abort", onAbort);
      resolve(result);
    };
    const rejectConnection = () => {
      connection.destroy();
      finish(undefined);
    };
    const onData = (chunk) => {
      const newline = chunk.indexOf(10);
      const length = newline === -1 ? chunk.length : newline + 1;
      if (header.length + length > MAX_ORIGIN_HEADER) {
        rejectConnection();
        return;
      }
      header = Buffer.concat([header, chunk.subarray(0, length)]);
      if (newline === -1) {
        if (header.length === MAX_ORIGIN_HEADER) rejectConnection();
        return;
      }
      const match = ORIGIN_HEADER.exec(header.toString("utf8"));
      if (match === null) {
        rejectConnection();
        return;
      }
      connection.pause();
      Object.defineProperty(connection, "origin", { value: match[1], enumerable: true });
      if (length < chunk.length) connection.unshift(chunk.subarray(length));
      // A stream error can arrive between resolving this promise and the
      // consumer or server installing its own handler. Keep a fallback so a
      // peer reset in that handoff window cannot terminate the Node process.
      // Additional user/server error listeners still receive the same event.
      connection.on("error", ignoreClaimedConnectionError);
      connection.once("close", () => {
        connection.off("error", ignoreClaimedConnectionError);
      });
      finish(connection);
    };
    const onEnd = () => rejectConnection();
    const onClose = () => finish(undefined);
    const onError = () => rejectConnection();
    const onAbort = () => rejectConnection();

    connection.on("data", onData);
    connection.once("end", onEnd);
    connection.once("close", onClose);
    connection.once("error", onError);
    signal?.addEventListener("abort", onAbort, { once: true });
    if (signal?.aborted) onAbort();
  });
}

function retry(signal, delayMs) {
  if (signal?.aborted) return Promise.resolve(false);
  return new Promise((resolve) => {
    let timer;
    const finish = (retryNow) => {
      if (timer !== undefined) clearTimeout(timer);
      signal?.removeEventListener("abort", onAbort);
      resolve(retryNow);
    };
    const onAbort = () => finish(false);
    timer = setTimeout(() => finish(true), delayMs);
    signal?.addEventListener("abort", onAbort, { once: true });
    if (signal?.aborted) onAbort();
  });
}
