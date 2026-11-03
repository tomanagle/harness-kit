// Node.js transport: spawns the harness-kit sidecar and speaks to it over
// stdio. Works in Electron's main process and any Node.js program.

import { spawn, type ChildProcess } from "node:child_process";
import { LineDecoder, type Transport } from "./rpc.ts";
import type { SettingsPatch } from "./protocol.ts";

export interface StdioOptions {
  /** Path to the harness-kit binary. */
  command: string;
  /** Arguments; defaults to ["serve"]. */
  args?: string[];
  /** Owner-supplied boot settings, including process wiring. Uses the same
   * document as kit.settings.configure; explicit args override these values. */
  settings?: SettingsPatch;
  env?: NodeJS.ProcessEnv;
  cwd?: string;
  /**
   * What to do with the sidecar's stderr (its log output): "inherit"
   * (default), "ignore", or a callback receiving each chunk.
   */
  stderr?: "inherit" | "ignore" | ((chunk: string) => void);
  /** How long close() waits for a clean exit before killing. Default 5000ms. */
  killAfterMs?: number;
}

export interface StdioTransport extends Transport {
  readonly process: ChildProcess;
}

/** Spawns the sidecar and returns a Transport over its stdin/stdout. */
export function stdio(options: StdioOptions): StdioTransport {
  const stderrMode = typeof options.stderr === "function" ? "pipe" : (options.stderr ?? "inherit");
  const args = [...(options.args ?? ["serve"])];
  if (options.settings !== undefined) args.push("--settings-json", JSON.stringify(options.settings));
  const child = spawn(options.command, args, {
    cwd: options.cwd,
    env: options.env ?? process.env,
    stdio: ["pipe", "pipe", stderrMode],
    windowsHide: true,
  });
  if (typeof options.stderr === "function") {
    const onChunk = options.stderr;
    child.stderr!.setEncoding("utf8");
    child.stderr!.on("data", (c: string) => onChunk(c));
  }

  const decoder = new LineDecoder();
  const messageHandlers: ((m: string) => void)[] = [];
  const closeHandlers: ((e?: Error) => void)[] = [];
  let closed = false;
  const emitClose = (err?: Error) => {
    if (closed) return;
    closed = true;
    for (const h of closeHandlers) h(err);
  };

  child.stdout!.setEncoding("utf8");
  child.stdout!.on("data", (chunk: string) => {
    for (const line of decoder.push(chunk)) for (const h of messageHandlers) h(line);
  });
  child.on("error", (err) => emitClose(err));
  child.on("exit", (code, signal) => {
    emitClose(code === 0 ? undefined : new Error(`harness-kit exited (${signal ?? `code ${code}`})`));
  });
  // Writes after the child died surface as EPIPE; the exit handler reports it.
  child.stdin!.on("error", () => {});

  const exited = new Promise<void>((resolve) => {
    if (child.exitCode !== null) resolve();
    else child.once("exit", () => resolve());
  });

  return {
    process: child,
    send(message) {
      if (closed) throw new Error("harness-kit transport closed");
      child.stdin!.write(message + "\n");
    },
    onMessage(handler) {
      messageHandlers.push(handler);
    },
    onClose(handler) {
      closeHandlers.push(handler);
    },
    async close() {
      // Closing stdin is the sidecar's signal to shut down.
      child.stdin!.end();
      const timer = setTimeout(() => child.kill("SIGKILL"), options.killAfterMs ?? 5000);
      await exited;
      clearTimeout(timer);
    },
  };
}
