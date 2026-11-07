import { RemoteProtocol } from "./remote.gen.ts";
import { SessionSync } from "./sync.ts";
import type { DurableHistory, SyncStore } from "./sync.ts";
export { SessionSync, SyncError } from "./sync.ts";
export type {
  SyncStore,
  SyncStatus,
  SyncPhase,
  CachedSession,
  SyncBinding,
  SyncCheckpoint,
  DurableHistory,
} from "./sync.ts";
import type { Ack, Changes, DeviceAuthorization, Grant, Push } from "./remote.gen.ts";
import type { ConfigureSettingsParams, ServerConnection, SettingsSnapshot } from "./protocol.ts";

export * from "./remote.gen.ts";
export interface RemoteRequestOptions {
  signal?: AbortSignal;
}
export interface RemoteTransport {
  request<T>(path: string, params: unknown, options?: RemoteRequestOptions): Promise<T>;
}
export class RemoteError extends Error {
  readonly status: number;
  readonly code: string;
  constructor(status: number, code: string) {
    super(`Server request failed: ${code}`);
    this.name = "RemoteError";
    this.status = status;
    this.code = code;
  }
}
export function serverOrigin(value: string): string {
  const u = new URL(value);
  if (
    u.username ||
    u.password ||
    u.pathname !== "/" ||
    u.search ||
    u.hash ||
    (u.protocol !== "https:" &&
      !(u.protocol === "http:" && ["localhost", "127.0.0.1", "[::1]"].includes(u.hostname)))
  ) {
    throw new Error("Server URL must be an HTTPS origin or loopback HTTP");
  }
  return u.origin;
}
export interface RemoteClientOptions {
  url: string;
  accessToken?: () => string | Promise<string>;
  fetch?: typeof globalThis.fetch;
  beforeRequest?: () => Promise<void>;
}
/** A client bound to one origin. Redirects never receive its credentials. */
export function remoteClient(options: RemoteClientOptions): RemoteProtocol {
  const origin = serverOrigin(options.url);
  const fetcher = options.fetch ?? globalThis.fetch;
  return new RemoteProtocol({
    async request<T>(path: string, params: unknown, opts?: RemoteRequestOptions): Promise<T> {
      await options.beforeRequest?.();
      const token = await options.accessToken?.();
      const response = await fetcher(origin + path, {
        method: "POST",
        redirect: "error",
        credentials: "omit",
        headers: {
          "Content-Type": "application/json",
          ...(token ? { Authorization: `Bearer ${token}` } : {}),
        },
        body: JSON.stringify(params),
        signal: opts?.signal
          ? AbortSignal.any([opts.signal, AbortSignal.timeout(35_000)])
          : AbortSignal.timeout(35_000),
      });
      // Bound untrusted server responses even when Content-Length is absent.
      const reader = response.body?.getReader();
      const chunks: Uint8Array[] = [];
      let size = 0;
      if (reader) {
        try {
          for (;;) {
            const { done, value } = await reader.read();
            if (done) break;
            size += value.byteLength;
            if (size > 2 * 1024 * 1024) {
              await reader.cancel();
              throw new Error("Server response exceeds 2 MiB");
            }
            chunks.push(value);
          }
        } finally {
          reader.releaseLock();
        }
      }
      const bytes = new Uint8Array(size);
      let offset = 0;
      for (const chunk of chunks) {
        bytes.set(chunk, offset);
        offset += chunk.length;
      }
      const body = JSON.parse(new TextDecoder().decode(bytes));
      if (!response.ok)
        throw new RemoteError(
          response.status,
          typeof body.code === "string" ? body.code : "http_error",
        );
      return body as T;
    },
  });
}

/** Credentials belong in the OS keychain or another host-injected secure store,
 * never settings, localStorage, transcripts, or logs. One registry owns rotation. */
export interface CredentialStore {
  get(key: string): Promise<Grant | undefined>;
  set(key: string, value: Grant): Promise<void>;
  delete(key: string): Promise<void>;
}
export interface ServerScope {
  serverId: string;
  url: string;
  accountId: string;
  tenantId: string;
  applicationId: string;
}
export function scopeKey(scope: ServerScope): string {
  return JSON.stringify([
    scope.serverId,
    serverOrigin(scope.url),
    scope.accountId,
    scope.tenantId,
    scope.applicationId,
  ]);
}
/** A durable store must preserve an immutable batch until acknowledge succeeds.
 * Implementations must reject an existing operationId with a different payload. */
export interface SyncOutbox {
  put(scope: string, batch: Push): Promise<void>;
  list(scope: string, limit: number): Promise<Push[]>;
  acknowledge(scope: string, operationId: string): Promise<void>;
}
export interface ServerOptions {
  applicationId: string;
  credentials: CredentialStore;
  outbox?: SyncOutbox;
  syncStore?: SyncStore;
  fetch?: typeof globalThis.fetch;
}
interface SettingsAPI {
  get(): Promise<SettingsSnapshot>;
  configure(params: ConfigureSettingsParams): Promise<SettingsSnapshot>;
}
export interface DeviceAuthorizationOptions {
  onCode: (authorization: DeviceAuthorization) => void | Promise<void>;
  signal?: AbortSignal;
}

/** All connection configuration goes through the existing settings revision API. */
export class Servers {
  readonly #settings: SettingsAPI;
  readonly #options?: ServerOptions;
  readonly #source?: DurableHistory;
  readonly #connections = new Map<string, ServerSession>();
  constructor(settings: SettingsAPI, options?: ServerOptions, source?: DurableHistory) {
    this.#settings = settings;
    this.#options = options;
    this.#source = source;
  }
  async list(): Promise<ServerConnection[]> {
    return (await this.#settings.get()).values.servers.connections;
  }
  async add(connection: ServerConnection): Promise<void> {
    const current = await this.#settings.get();
    if (current.values.servers.connections.some((c) => c.id === connection.id))
      throw new Error("Server ID already exists");
    await this.#settings.configure({
      expectedRevision: current.revision,
      patch: {
        servers: {
          connections: [
            ...current.values.servers.connections,
            { ...connection, url: serverOrigin(connection.url) },
          ],
        },
      },
    });
  }
  async remove(serverId: string): Promise<void> {
    const current = await this.#settings.get();
    await this.#settings.configure({
      expectedRevision: current.revision,
      patch: {
        servers: {
          connections: current.values.servers.connections.filter((c) => c.id !== serverId),
        },
      },
    });
  }
  async #configured(serverId: string): Promise<ServerConnection> {
    const connection = (await this.list()).find((c) => c.id === serverId);
    if (!connection?.enabled) throw new Error("Server connection is missing or disabled");
    return connection;
  }
  #optionsOrThrow(): ServerOptions {
    if (!this.#options)
      throw new Error("Supply servers.applicationId and servers.credentials when creating the kit");
    return this.#options;
  }
  async authorize(serverId: string, options: DeviceAuthorizationOptions): Promise<ServerSession> {
    const config = await this.#configured(serverId);
    const host = this.#optionsOrThrow();
    const client = remoteClient({ url: config.url, fetch: host.fetch });
    const authorization = await client.beginDevice(
      { applicationId: host.applicationId, name: config.name },
      options,
    );
    const deadline = Date.now() + authorization.expiresIn * 1000;
    await options.onCode(authorization);
    let interval = authorization.interval * 1000;
    for (;;) {
      await delay(interval, options.signal);
      if (Date.now() >= deadline) throw new RemoteError(400, "expired_token");
      await this.#assertEndpoint(serverId, config.url);
      try {
        const grant = await client.pollDevice({ deviceCode: authorization.deviceCode }, options);
        if (grant.applicationId !== host.applicationId)
          throw new Error("Server returned an unexpected application scope");
        const scope = {
          serverId,
          url: serverOrigin(config.url),
          accountId: grant.accountId,
          tenantId: grant.tenantId,
          applicationId: grant.applicationId,
        };
        await host.credentials.set(scopeKey(scope), grant);
        await this.#connections.get(scopeKey(scope))?.stopSync();
        this.#connections.delete(scopeKey(scope));
        return this.#session(scope, host);
      } catch (error) {
        if (!(error instanceof RemoteError)) throw error;
        if (error.code === "slow_down") {
          interval += 5000;
          continue;
        }
        if (error.code !== "authorization_pending") throw error;
      }
    }
  }
  async connect(
    serverId: string,
    identity: { accountId: string; tenantId: string },
  ): Promise<ServerSession> {
    const config = await this.#configured(serverId);
    const host = this.#optionsOrThrow();
    return this.#session(
      { serverId, url: serverOrigin(config.url), ...identity, applicationId: host.applicationId },
      host,
    );
  }
  #session(scope: ServerScope, options: ServerOptions): ServerSession {
    const key = scopeKey(scope);
    let connection = this.#connections.get(key);
    if (!connection) {
      connection = new ServerSession(
        scope,
        options,
        () => this.#assertEndpoint(scope.serverId, scope.url),
        this.#source,
        async () => {
          const { values } = await this.#settings.get();
          const enabled = values.servers.connections.some(
            (c) => c.id === scope.serverId && c.enabled,
          );
          return { ...values.sync, paused: values.sync.paused || !enabled };
        },
      );
      this.#connections.set(key, connection);
    }
    return connection;
  }
  async #assertEndpoint(serverId: string, url: string): Promise<void> {
    if (serverOrigin((await this.#configured(serverId)).url) !== serverOrigin(url))
      throw new Error("Server URL changed; reconnect using the new endpoint");
  }
  async close(): Promise<void> {
    await Promise.all([...this.#connections.values()].map((connection) => connection.stopSync()));
  }
}

export class ServerSession {
  readonly scope: Readonly<ServerScope>;
  readonly protocol: RemoteProtocol;
  readonly #options: ServerOptions;
  readonly #key: string;
  #refresh?: Promise<Grant>;
  #reauthorize = false;
  #flush?: Promise<Ack[]>;
  readonly #sync?: SessionSync;
  constructor(
    scope: ServerScope,
    options: ServerOptions,
    check: () => Promise<void>,
    source?: DurableHistory,
    settings?: () => Promise<import("./protocol.ts").SyncSettings>,
  ) {
    this.scope = Object.freeze({ ...scope });
    this.#options = options;
    this.#key = scopeKey(scope);
    this.protocol = remoteClient({
      url: scope.url,
      fetch: options.fetch,
      beforeRequest: check,
      accessToken: async () => (await this.#grant()).accessToken,
    });
    if (options.syncStore && source && settings)
      this.#sync = new SessionSync({
        scope: this.scope,
        protocol: this.protocol,
        store: options.syncStore,
        source,
        settings,
      });
  }
  get sync(): SessionSync {
    if (!this.#sync) throw new Error("Supply servers.syncStore when creating the kit");
    return this.#sync;
  }
  async stopSync(): Promise<void> {
    await this.#sync?.stop();
  }
  async #grant(): Promise<Grant> {
    if (this.#reauthorize) throw new RemoteError(401, "reauthorization_required");
    if (!this.#refresh) {
      this.#refresh = (async () => {
        const grant = await this.#options.credentials.get(this.#key);
        if (!grant) throw new RemoteError(401, "authorization_required");
        if (
          grant.accountId !== this.scope.accountId ||
          grant.tenantId !== this.scope.tenantId ||
          grant.applicationId !== this.scope.applicationId
        )
          throw new Error("Credential scope mismatch");
        if (Date.parse(grant.expiresAt) > Date.now() + 30_000) return grant;
        try {
          const next = await remoteClient({
            url: this.scope.url,
            fetch: this.#options.fetch,
          }).refreshDevice({ refreshToken: grant.refreshToken });
          if (
            next.accountId !== grant.accountId ||
            next.tenantId !== grant.tenantId ||
            next.applicationId !== grant.applicationId ||
            next.deviceId !== grant.deviceId
          )
            throw new Error("Refresh changed credential scope");
          await this.#options.credentials.set(this.#key, next);
          return next;
        } catch (error) {
          // A failed response or credential write may follow a committed rotation.
          // Never automatically replay the previous refresh credential.
          this.#reauthorize = true;
          await this.#options.credentials.delete(this.#key).catch(() => {});
          throw error;
        }
      })();
    }
    const pending = this.#refresh;
    try {
      return await pending;
    } finally {
      if (this.#refresh === pending) this.#refresh = undefined;
    }
  }

  async revoke(): Promise<void> {
    await this.stopSync();
    const grant = await this.#grant();
    await this.protocol.revokeDevice({ deviceId: grant.deviceId });
    await this.#options.credentials.delete(this.#key);
  }
  /** Persist before sending. Retrying a batch must retain its operationId. */
  async enqueue(batch: Push): Promise<void> {
    if (!this.#options.outbox) throw new Error("Supply a durable sync outbox before enqueueing");
    await this.#options.outbox.put(this.#key, batch);
  }
  /** Drain a bounded number of queued operations. On failure, unacknowledged
   * batches stay on disk. Calling flush again safely retries them. */
  async flush(options?: RemoteRequestOptions): Promise<Ack[]> {
    if (this.#flush) return this.#flush;
    const outbox = this.#options.outbox;
    if (!outbox) throw new Error("No sync outbox configured");
    this.#flush = (async () => {
      const acks: Ack[] = [];
      for (const batch of await outbox.list(this.#key, 100)) {
        const ack = await this.protocol.push(batch, options);
        if (
          ack.sessionId !== batch.sessionId ||
          ack.lastSeq !== batch.expectedSeq + batch.events.length
        )
          throw new Error("Invalid sync acknowledgement");
        await outbox.acknowledge(this.#key, batch.operationId);
        acks.push(ack);
      }
      return acks;
    })();
    try {
      return await this.#flush;
    } finally {
      this.#flush = undefined;
    }
  }
  /** Consumer persists each page before asking for the next. Resume with the
   * last committed cursor. Scope never follows a UI's active tenant. */
  async *changes(after = 0, options?: RemoteRequestOptions): AsyncGenerator<Changes> {
    let cursor = after;
    let backoff = 250;
    for (;;) {
      options?.signal?.throwIfAborted();
      try {
        const page = await this.protocol.changes(
          { after: cursor, limit: 100, waitMs: 25_000 },
          options,
        );
        if (!Number.isSafeInteger(page.cursor) || page.cursor < cursor)
          throw new Error("Invalid sync cursor");
        yield page;
        cursor = page.cursor;
        backoff = 250;
      } catch (error) {
        if (error instanceof RemoteError && error.status < 500 && error.status !== 429) throw error;
        if (
          !(error instanceof TypeError) &&
          !(error instanceof RemoteError) &&
          !(error instanceof Error && error.name === "TimeoutError")
        )
          throw error;
        await delay(backoff + Math.random() * backoff, options?.signal);
        backoff = Math.min(backoff * 2, 10_000);
      }
    }
  }
}
function delay(ms: number, signal?: AbortSignal): Promise<void> {
  signal?.throwIfAborted();
  return new Promise((resolve, reject) => {
    const onAbort = () => {
      clearTimeout(timer);
      reject(signal?.reason);
    };
    const timer = setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    signal?.addEventListener("abort", onAbort, { once: true });
  });
}

export { serveExecution, type ExecutionOwner, type ExecutionHandler } from "./execution.ts";
