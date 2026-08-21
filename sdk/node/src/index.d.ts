import type { IpcNetConnectOpts, Server, Socket } from "node:net";

export type ComponentEnvironment = Record<string, string | undefined>;

export interface ConnectOptions extends Omit<IpcNetConnectOpts, "path"> {
  env?: ComponentEnvironment;
}

export interface OutputOptions {
  env?: ComponentEnvironment;
  signal?: AbortSignal;
  retryDelayMs?: number;
}

export function inputEnv(name: string): string;
export function outputEnv(name: string): string;
export function inputTarget(name: string, env?: ComponentEnvironment): string;
export function outputTarget(name: string, env?: ComponentEnvironment): string;
export function inputPath(name: string, env?: ComponentEnvironment): string;
export function outputPath(name: string, env?: ComponentEnvironment): string;
export function unixPath(target: string): string;
export function inputHttpOptions(
  name: string,
  env?: ComponentEnvironment,
): Readonly<{ socketPath: string }>;
export function connectInput(name: string, options?: ConnectOptions): Socket;
export function connectOutput(name: string, options?: ConnectOptions): Socket;
export function outputConnections(
  name: string,
  options?: OutputOptions,
): AsyncGenerator<Socket, void, void>;
export function serveOutput(
  server: Server,
  name: string,
  options?: OutputOptions,
): Promise<void>;
