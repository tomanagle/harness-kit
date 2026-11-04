// Minimal JSON-RPC 2.0 connection over a line-oriented Transport.

import { ErrorCodes, type ErrorReason } from "./protocol.ts";

/**
 * A bidirectional message channel to a Harness Kit server. Each message is
 * one complete JSON-RPC message (without the trailing newline).
 *
 * Implementations: `stdio()` from "@tomanagle/harness-kit/node" spawns the
 * sidecar; `callbackTransport()` adapts any message channel (Electron IPC,
 * Wails events, a WebSocket you operate yourself).
 */
export interface Transport {
  send(message: string): void;
  onMessage(handler: (message: string) => void): void;
  onClose(handler: (error?: Error) => void): void;
  close(): void | Promise<void>;
}

/** Splits a text stream into newline-delimited messages. */
export class LineDecoder {
  #buf = "";
  push(chunk: string): string[] {
    this.#buf += chunk;
    const lines: string[] = [];
    let i: number;
    while ((i = this.#buf.indexOf("\n")) >= 0) {
      const line = this.#buf.slice(0, i).replace(/\r$/, "");
      this.#buf = this.#buf.slice(i + 1);
      if (line.trim() !== "") lines.push(line);
    }
    return lines;
  }
}

/** An error returned by the Harness Kit server. Switch on `reason`. */
export class HarnessKitError extends Error {
  readonly code: number;
  readonly reason?: ErrorReason;
  readonly data?: unknown;
  constructor(code: number, message: string, data?: unknown) {
    super(message);
    this.name = "HarnessKitError";
    this.code = code;
    this.data = data;
    if (data && typeof data === "object" && "reason" in data) {
      this.reason = (data as { reason: ErrorReason }).reason;
    }
  }
}

/** Raised for requests that fail because the connection closed. */
export class ConnectionClosedError extends Error {
  constructor(cause?: Error) {
    super("harness-kit connection closed" + (cause ? `: ${cause.message}` : ""));
    this.name = "ConnectionClosedError";
  }
}

interface Pending {
  resolve: (v: unknown) => void;
  reject: (e: Error) => void;
}

export interface RequestOptions {
  signal?: AbortSignal;
}

/**
 * Handles a request from the server. The signal aborts if the server
 * cancels the request ($/cancelRequest). Throw to reply with an error.
 */
export type RequestHandler = (
  method: string,
  params: unknown,
  signal: AbortSignal,
) => Promise<unknown>;

export class JsonRpcConnection {
  #transport: Transport;
  #nextId = 1;
  #pending = new Map<number, Pending>();
  #closed: Error | undefined;
  #onNotification: (method: string, params: unknown) => void;
  #closeHandlers: ((e?: Error) => void)[] = [];
  #requestHandler: RequestHandler | undefined;
  #inbound = new Map<number | string, AbortController>();

  constructor(transport: Transport, onNotification: (method: string, params: unknown) => void) {
    this.#transport = transport;
    this.#onNotification = onNotification;
    transport.onMessage((m) => this.#receive(m));
    transport.onClose((err) => this.#shutdown(err ?? new Error("transport closed")));
  }

  get closed(): boolean {
    return this.#closed !== undefined;
  }

  /** Sets the handler for server-to-client requests (e.g. tool.call). */
  setRequestHandler(handler: RequestHandler): void {
    this.#requestHandler = handler;
  }

  /** Registers a close handler; returns a function that removes it. */
  onClose(handler: (error?: Error) => void): () => void {
    if (this.#closed) {
      queueMicrotask(() => handler(this.#closed));
      return () => {};
    }
    this.#closeHandlers.push(handler);
    return () => {
      const i = this.#closeHandlers.indexOf(handler);
      if (i >= 0) this.#closeHandlers.splice(i, 1);
    };
  }

  request<T>(method: string, params?: unknown, opts: RequestOptions = {}): Promise<T> {
    if (this.#closed) return Promise.reject(new ConnectionClosedError(this.#closed));
    const id = this.#nextId++;
    return new Promise<T>((resolve, reject) => {
      const onAbort = () => {
        if (!this.#pending.delete(id)) return;
        this.notify("$/cancelRequest", { id });
        reject(opts.signal?.reason ?? new Error("aborted"));
      };
      if (opts.signal?.aborted) return reject(opts.signal.reason ?? new Error("aborted"));
      opts.signal?.addEventListener("abort", onAbort, { once: true });
      this.#pending.set(id, {
        resolve: (v) => {
          opts.signal?.removeEventListener("abort", onAbort);
          resolve(v as T);
        },
        reject: (e) => {
          opts.signal?.removeEventListener("abort", onAbort);
          reject(e);
        },
      });
      this.#send({ jsonrpc: "2.0", id, method, ...(params === undefined ? {} : { params }) });
    });
  }

  notify(method: string, params?: unknown): void {
    if (this.#closed) return;
    this.#send({ jsonrpc: "2.0", method, ...(params === undefined ? {} : { params }) });
  }

  #send(msg: object): void {
    try {
      this.#transport.send(JSON.stringify(msg));
    } catch (e) {
      this.#shutdown(e instanceof Error ? e : new Error(String(e)));
    }
  }

  #receive(line: string): void {
    let msg: {
      id?: number | string | null;
      method?: string;
      params?: unknown;
      result?: unknown;
      error?: { code: number; message: string; data?: unknown };
    };
    try {
      msg = JSON.parse(line);
    } catch {
      return; // not our protocol; ignore
    }
    if (typeof msg.method === "string") {
      if (msg.id === undefined || msg.id === null) {
        if (msg.method === "$/cancelRequest") {
          const id = (msg.params as { id?: number | string } | undefined)?.id;
          if (id !== undefined) this.#inbound.get(id)?.abort(new Error("cancelled by the server"));
          return;
        }
        this.#onNotification(msg.method, msg.params);
      } else {
        this.#handleRequest(msg.id, msg.method, msg.params);
      }
      return;
    }
    if (typeof msg.id !== "number") return;
    const p = this.#pending.get(msg.id);
    if (!p) return;
    this.#pending.delete(msg.id);
    if (msg.error) p.reject(new HarnessKitError(msg.error.code, msg.error.message, msg.error.data));
    else p.resolve(msg.result);
  }

  #handleRequest(id: number | string, method: string, params: unknown): void {
    const handler = this.#requestHandler;
    if (!handler) {
      this.#send({
        jsonrpc: "2.0",
        id,
        error: { code: ErrorCodes.MethodNotFound, message: "method not found: " + method },
      });
      return;
    }
    const ac = new AbortController();
    this.#inbound.set(id, ac);
    Promise.resolve()
      .then(() => handler(method, params, ac.signal))
      .then(
        (result) => this.#send({ jsonrpc: "2.0", id, result: result ?? null }),
        (e: unknown) => {
          const err = e as { code?: number; message?: string };
          this.#send({
            jsonrpc: "2.0",
            id,
            error: {
              code: typeof err?.code === "number" ? err.code : ErrorCodes.InternalError,
              message: err?.message ?? String(e),
            },
          });
        },
      )
      .finally(() => this.#inbound.delete(id));
  }

  #shutdown(err: Error): void {
    if (this.#closed) return;
    this.#closed = err;
    for (const p of this.#pending.values()) p.reject(new ConnectionClosedError(err));
    this.#pending.clear();
    for (const ac of this.#inbound.values()) ac.abort(err);
    this.#inbound.clear();
    for (const h of this.#closeHandlers.splice(0)) h(err);
  }
}
