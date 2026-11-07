import { Servers, type ServerOptions } from "./remote.ts";
import { Conversation, type ConversationOptions } from "./conversation.ts";
export {
  Conversation,
  ConversationState,
  type ConversationSnapshot,
  type ConversationMessage,
  type ConversationOptions,
} from "./conversation.ts";
// @tomanagle/harness-kit: a typed client for the Harness Kit sidecar protocol.
//
// This entry point has no Node.js dependencies, so it works in Electron
// main and renderer processes, Wails frontends and browsers alike. To spawn
// the sidecar from Node.js, use `stdio()` from "@tomanagle/harness-kit/node".

import { JsonRpcConnection, type RequestOptions, type Transport } from "./rpc.ts";
import { ProtocolClient } from "./methods.gen.ts";
import {
  PROTOCOL_VERSION,
  ErrorCodes,
  type HookName,
  type HookRequest,
  type HookResponse,
  type HookSpec,
  type ApprovalRequest,
  type CreateSessionParams,
  type ConfigPatch,
  type CodexOptions,
  type ClaudeOptions,
  type SettingsPatch,
  type SettingsTarget,
  type SettingsSnapshot,
  type ConfigureSettingsParams,
  type ResourceStats,
  type EventPage,
  type Decision,
  type Discovery,
  type HarnessEvent,
  type InitializeResult,
  type InputPart,
  type McpListResult,
  type Outcome,
  type RuntimeInfo,
  type RuntimeSession,
  type RpcMethods,
  type SessionInfo,
  type SessionContext,
  type LogNotification,
  type ToolCallParams,
  type ToolResult,
  type ToolSpec,
  type TurnCompletedData,
} from "./protocol.ts";

/** Lifecycle contexts are narrowed by the registered hook name. */
export type HookContext<N extends HookName = HookName> = Omit<
  HookRequest,
  "name" | "create" | "session" | "input" | "result"
> & { name: N; signal: AbortSignal } & (N extends "session.beforeCreate"
    ? { create: NonNullable<HookRequest["create"]> }
    : {
        session: NonNullable<HookRequest["session"]>;
        input: InputPart[];
        turnId: string;
      } & (N extends "turn.after" ? { result: NonNullable<HookRequest["result"]> } : {}));
export type HookHandler<N extends HookName = HookName> = (
  context: HookContext<N>,
) => void | HookResponse | Promise<void | HookResponse>;

export * from "./protocol.ts";
export { ProtocolClient } from "./methods.gen.ts";
export {
  ConnectionClosedError,
  HarnessKitError,
  JsonRpcConnection,
  LineDecoder,
  type RequestOptions,
  type Transport,
} from "./rpc.ts";

export interface HarnessKitOptions {
  /** Optional remote coordination. Preferences stay in settings; credentials stay in your secure store. */
  servers?: ServerOptions;
  transport: Transport;
  clientInfo?: { name: string; version?: string };
  /** Explicit owner-supplied live settings, applied before this promise resolves.
   * Use stdio({ settings }) for boot wiring such as store/runtime paths.
   * Omit when attaching another window to an already-configured host. */
  initialSettings?: SettingsPatch;
}

export type RuntimeOptions<R extends string> = R extends "codex"
  ? CodexOptions
  : R extends "claude"
    ? ClaudeOptions
    : Record<string, unknown>;
export type CreateSessionOptions<R extends string = string> = Omit<
  CreateSessionParams,
  "runtime" | "options"
> & { runtime?: R; options?: NoInfer<RuntimeOptions<R>> };

export type EventListener = (event: HarnessEvent) => void;

/** Context passed to a tool handler. */
export interface ToolCallContext {
  sessionId: string;
  turnId?: string;
  name: string;
  /** Aborted if the call is abandoned (e.g. the turn was cancelled). */
  signal: AbortSignal;
}

/**
 * A tool whose handler runs in your process. Return a string, a ToolResult,
 * or any JSON-serialisable value; throw to report an error to the model.
 */
export interface ToolDefinition<A = any> extends ToolSpec {
  handler: (args: A, ctx: ToolCallContext) => unknown | Promise<unknown>;
}

function toToolResult(v: unknown): ToolResult {
  if (typeof v === "string") return { content: [{ type: "text", text: v }] };
  if (v === undefined || v === null) return { content: [] };
  if (typeof v === "object" && Array.isArray((v as ToolResult).content)) return v as ToolResult;
  const text = JSON.stringify(v);
  return {
    content: [{ type: "text", text }],
    ...(typeof v === "object" && !Array.isArray(v) ? { structuredContent: v } : {}),
  };
}

/** Connects to a Harness Kit server and performs the initialize handshake. */
export async function createHarnessKit(options: HarnessKitOptions): Promise<HarnessKit> {
  const kit = new HarnessKit(options.transport, options.servers);
  try {
    await kit._initialize(options.clientInfo ?? { name: "@tomanagle/harness-kit" });
    if (options.initialSettings !== undefined)
      await kit.settings.configure({ patch: options.initialSettings });
    return kit;
  } catch (error) {
    await options.transport.close();
    throw error;
  }
}

/** A connected Harness Kit client. Create one with `createHarnessKit`. */
export class HarnessKit {
  readonly servers: Servers;
  /** Schema-generated methods, with the protocol's exact request and result shapes. */
  readonly protocol: ProtocolClient;
  readonly #rpc: JsonRpcConnection;
  readonly #transport: Transport;
  readonly #listeners = new Set<EventListener>();
  readonly #settingsListeners = new Set<(snapshot: SettingsSnapshot) => void>();
  readonly #logListeners = new Set<(notification: LogNotification) => void>();
  readonly #settingsRevisions = new Map<string, number>();
  #activeHooks = 0;
  readonly #hooks = new Map<string, HookHandler>();
  readonly #tools = new Map<string, ToolDefinition>();
  #server: InitializeResult | undefined;

  /** @internal Use createHarnessKit. */
  constructor(transport: Transport, servers?: ServerOptions) {
    this.servers = new Servers(this.settings, servers, this.sessions);
    this.#transport = transport;
    this.#rpc = new JsonRpcConnection(transport, (method, params) => {
      if (method === "log") {
        // oxlint-disable-next-line unicorn/no-useless-spread -- Snapshot subscriptions before callbacks can change them.
        for (const listener of [...this.#logListeners]) {
          try {
            listener(params as LogNotification);
          } catch (error) {
            queueMicrotask(() => {
              throw error;
            });
          }
        }
        return;
      }
      if (method === "settings.updated") {
        this.#settingsChanged(params as SettingsSnapshot);
        return;
      }
      if (method !== "event") return;
      const ev = params as HarnessEvent;
      // oxlint-disable-next-line unicorn/no-useless-spread -- Snapshot subscriptions before callbacks can change them.
      for (const l of [...this.#listeners]) {
        try {
          l(ev);
        } catch (e) {
          queueMicrotask(() => {
            throw e;
          });
        }
      }
    });
    this.protocol = new ProtocolClient((method, params, opts) =>
      this.#rpc.request(method, params, opts),
    );
    this.#rpc.setRequestHandler(async (method, params, signal) => {
      if (method === "hook.call") {
        const request = params as HookRequest;
        const handler = this.#hooks.get(request.hookId);
        if (!handler) throw new Error(`Hook ${request.hookId} is not registered by this client.`);
        const limit = request.concurrencyLimit > 0 ? request.concurrencyLimit : 32;
        if (this.#activeHooks >= limit) throw new Error("Hook callback concurrency limit reached.");
        this.#activeHooks++;
        try {
          return (
            (await handler({ ...request, input: request.input ?? [], signal } as HookContext)) ?? {}
          );
        } finally {
          this.#activeHooks--;
        }
      }
      if (method !== "tool.call")
        throw Object.assign(new Error("method not found: " + method), {
          code: ErrorCodes.MethodNotFound,
        });
      const p = params as ToolCallParams;
      const def = this.#tools.get(p.name);
      if (!def)
        return toToolResult({
          content: [{ type: "text", text: `tool ${p.name} is not registered` }],
          isError: true,
        });
      try {
        return toToolResult(
          await def.handler(p.arguments, {
            sessionId: p.sessionId,
            turnId: p.turnId,
            name: p.name,
            signal,
          }),
        );
      } catch (e) {
        return {
          content: [{ type: "text", text: e instanceof Error ? e.message : String(e) }],
          isError: true,
        } satisfies ToolResult;
      }
    });
  }

  /** @internal */
  async _initialize(clientInfo: { name: string; version?: string }): Promise<void> {
    this.#server = await this.protocol.initialize({
      protocolVersion: PROTOCOL_VERSION,
      clientInfo,
    });
  }

  /** The server's initialize result: version, runtimes and limits. */
  get server(): InitializeResult {
    if (!this.#server) throw new Error("not initialized");
    return this.#server;
  }

  /** Sends a raw request. Prefer the typed helpers. */
  request<T>(method: string, params?: unknown, opts?: RequestOptions): Promise<T> {
    return this.#rpc.request<T>(method, params, opts);
  }

  /**
   * Observes every event from every session, in per-session order. A
   * `stream.gap` event means the server dropped events for this client
   * because it read too slowly; fetch them with `session.events()`.
   * Returns an unsubscribe function.
   */
  onEvent(listener: EventListener): () => void {
    this.#listeners.add(listener);
    return () => this.#listeners.delete(listener);
  }

  /** Called when the connection ends (e.g. the sidecar exited). Returns an unsubscribe function. */
  onClose(handler: (error?: Error) => void): () => void {
    return this.#rpc.onClose(handler);
  }

  #settingsChanged(snapshot: SettingsSnapshot): void {
    const target = snapshot.target?.sessionId ?? "";
    if (!snapshot.resync && snapshot.revision <= (this.#settingsRevisions.get(target) ?? -1))
      return;
    this.#settingsRevisions.set(
      target,
      Math.max(snapshot.revision, this.#settingsRevisions.get(target) ?? -1),
    );
    // oxlint-disable-next-line unicorn/no-useless-spread -- Snapshot subscriptions before callbacks can change them.
    for (const listener of [...this.#settingsListeners]) {
      try {
        listener(snapshot);
      } catch (error) {
        queueMicrotask(() => {
          throw error;
        });
      }
    }
  }

  /** Live diagnostics from the host's slog stream. No history is retained. */
  readonly logs = {
    /** Installs the listener before enabling delivery. Await this before producing
     * logs you need to observe. Each listener is independent; closing the last
     * subscription stops server delivery. Slow readers receive dropped markers. */
    subscribe: async (
      listener: (notification: LogNotification) => void,
      options?: RequestOptions,
    ): Promise<() => Promise<void>> => {
      options?.signal?.throwIfAborted();
      const subscription = (notification: LogNotification) => listener(notification);
      this.#logListeners.add(subscription);
      try {
        await this.protocol.logsSubscribe({}, options);
      } catch (error) {
        this.#logListeners.delete(subscription);
        if (this.#logListeners.size === 0) {
          // Setup cancellation must reject immediately even if the peer stopped
          // responding. Bound cleanup so it cannot leave a pending RPC forever.
          void this.protocol
            .logsUnsubscribe({}, { signal: AbortSignal.timeout(1000) })
            .catch(() => {});
        }
        throw error;
      }
      let closed = false;
      return async () => {
        if (closed) return;
        closed = true;
        this.#logListeners.delete(subscription);
        if (this.#logListeners.size === 0) await this.protocol.logsUnsubscribe({});
      };
    },
  };

  /** One settings surface for host policy, session defaults and scoped changes. */
  readonly settings = {
    get: (target: SettingsTarget = {}): Promise<SettingsSnapshot> =>
      this.protocol.settingsGet({ target }),
    configure: async (params: ConfigureSettingsParams): Promise<SettingsSnapshot> => {
      const snapshot = await this.protocol.settingsConfigure(params);
      this.#settingsChanged(snapshot);
      return snapshot;
    },
    /** Subscribe before fetching current settings. Revisions identify stale results;
     * a resync snapshot means refresh all displayed targets with get(). */
    onChanged: (listener: (snapshot: SettingsSnapshot) => void): (() => void) => {
      this.#settingsListeners.add(listener);
      return () => this.#settingsListeners.delete(listener);
    },
    stats: (): Promise<ResourceStats> => this.protocol.settingsStats(),
  };

  /** Optional Git operations on a session's repository. These methods never
   * stage, commit or contact remotes implicitly. Host policy uses settings.git. */
  readonly git = {
    status: (sessionId: string, opts?: RequestOptions) =>
      this.protocol.gitStatus({ sessionId }, opts),
    diff: (
      sessionId: string,
      params: Omit<RpcMethods["git.diff"]["params"], "sessionId"> = {},
      opts?: RequestOptions,
    ) => this.protocol.gitDiff({ sessionId, ...params }, opts),
    branches: (sessionId: string, opts?: RequestOptions) =>
      this.protocol.gitBranches({ sessionId }, opts),
    log: (
      sessionId: string,
      params: Omit<RpcMethods["git.log"]["params"], "sessionId"> = {},
      opts?: RequestOptions,
    ) => this.protocol.gitLog({ sessionId, ...params }, opts),
    stage: async (sessionId: string, paths: string[], opts?: RequestOptions): Promise<void> => {
      await this.protocol.gitStage({ sessionId, paths }, opts);
    },
    unstage: async (sessionId: string, paths: string[], opts?: RequestOptions): Promise<void> => {
      await this.protocol.gitUnstage({ sessionId, paths }, opts);
    },
    commit: (sessionId: string, message: string, opts?: RequestOptions) =>
      this.protocol.gitCommit({ sessionId, message }, opts),
    checkout: async (
      sessionId: string,
      params: Omit<RpcMethods["git.checkout"]["params"], "sessionId">,
      opts?: RequestOptions,
    ): Promise<void> => {
      await this.protocol.gitCheckout({ sessionId, ...params }, opts);
    },
    fetch: (
      sessionId: string,
      params: Omit<RpcMethods["git.fetch"]["params"], "sessionId"> = {},
      opts?: RequestOptions,
    ) => this.protocol.gitFetch({ sessionId, ...params }, opts),
    pull: (
      sessionId: string,
      params: Omit<RpcMethods["git.pull"]["params"], "sessionId"> = {},
      opts?: RequestOptions,
    ) => this.protocol.gitPull({ sessionId, ...params }, opts),
    push: (
      sessionId: string,
      params: Omit<RpcMethods["git.push"]["params"], "sessionId"> = {},
      opts?: RequestOptions,
    ) => this.protocol.gitPush({ sessionId, ...params }, opts),
  };

  readonly runtimes = {
    /** Registered runtimes (no probing). */
    list: async (): Promise<RuntimeInfo[]> => (await this.protocol.runtimeList()).runtimes,
    /** Probes installation, version and sign-in status. */
    discover: async (
      opts: { runtimes?: string[]; signal?: AbortSignal } = {},
    ): Promise<Discovery[]> =>
      (await this.protocol.runtimeDiscover({ runtimes: opts.runtimes }, { signal: opts.signal }))
        .runtimes,
    /**
     * Lists a runtime's own stored sessions, including ones started outside
     * Harness Kit, newest first. Fails with reason "unsupported" for
     * runtimes without `capabilities.listSessions`.
     */
    sessions: async (
      runtime: string,
      opts: { cwd?: string; limit?: number } = {},
    ): Promise<RuntimeSession[]> =>
      (await this.protocol.runtimeSessions({ runtime, ...opts })).sessions,
  };

  readonly sessions = {
    /** Observe persisted history and live state without waking the runtime. */
    observe: (sessionId: string, options?: ConversationOptions): Conversation =>
      new Conversation(this, sessionId, options),
    /** Reads a saved turn-boundary diff without starting an agent or running Git. */
    diff: (
      sessionId: string,
      turnId: string,
      options: { maxBytes?: number } = {},
    ): Promise<import("./protocol.ts").WorkspaceDiff> =>
      this.protocol.sessionDiff({ sessionId, turnId, ...options }),
    /** Explicitly removes only a clean owned worktree; first close/suspend its runtime. */
    removeWorkspace: async (sessionId: string): Promise<void> => {
      await this.protocol.workspaceRemove({ sessionId });
    },
    /** Starts a session (or resumes `resumeId`). */
    create: async <R extends string = string>(
      params: CreateSessionOptions<R>,
    ): Promise<Session> => {
      const { session } = await this.protocol.sessionCreate(params as CreateSessionParams);
      return new Session(this, session);
    },
    get: async (sessionId: string): Promise<SessionInfo> =>
      (await this.protocol.sessionGet({ sessionId })).session,
    /** Reads the latest context observation without starting the runtime or making a model request. */
    context: (sessionId: string, options?: RequestOptions): Promise<SessionContext> =>
      this.protocol.sessionContext({ sessionId }, options),
    /** Compacts an idle live session and waits for native completion. Await the previous turn first. */
    compact: (sessionId: string, options?: RequestOptions): Promise<SessionContext> =>
      this.protocol.sessionCompact({ sessionId }, options),
    list: async (
      options: import("./protocol.js").ListSessionsOptions = {},
    ): Promise<SessionInfo[]> => (await this.protocol.sessionList(options)).sessions,
    /** Branches the whole conversation; forkPending means native materialization waits for the first prompt. */
    fork: async (
      sessionId: string,
      options: import("./protocol.js").ForkSessionOptions = {},
    ): Promise<Session> => {
      const { session } = await this.protocol.sessionFork({ sessionId, ...options });
      return new Session(this, session);
    },
    archive: async (sessionId: string, archived = true): Promise<SessionInfo> =>
      (await this.protocol.sessionArchive({ sessionId, archived })).session,
    setTags: async (sessionId: string, tags: string[]): Promise<SessionInfo> =>
      (await this.protocol.sessionTags({ sessionId, tags })).session,
    /** Reads one bounded page without starting the runtime. Use nextSeq while hasMore. */
    durableEventsPage: (
      sessionId: string,
      opts: { afterSeq?: number; limit?: number } = {},
    ): Promise<EventPage> => this.protocol.sessionDurableEventsPage({ sessionId, ...opts }),
    eventsPage: async (
      sessionId: string,
      opts: { afterSeq?: number; limit?: number } = {},
    ): Promise<EventPage> => {
      const page = await this.protocol.sessionEvents({ sessionId, ...opts });
      return {
        ...page,
        nextSeq: page.nextSeq ?? page.events.at(-1)?.seq ?? opts.afterSeq ?? 0,
        hasMore: page.hasMore ?? false,
      };
    },
    /** Reads saved history in bounded RPC pages and collects it in the caller.
     * Use eventsPage to bound the application's memory as well. */
    events: async (
      sessionId: string,
      opts: { afterSeq?: number; limit?: number } = {},
    ): Promise<{ events: HarnessEvent[]; truncated: boolean }> => {
      const events: HarnessEvent[] = [];
      let afterSeq = opts.afterSeq ?? 0;
      let truncated = false;
      for (;;) {
        const remaining = opts.limit && opts.limit > 0 ? opts.limit - events.length : undefined;
        if (remaining !== undefined && remaining <= 0) break;
        const page = await this.sessions.eventsPage(sessionId, { afterSeq, limit: remaining });
        events.push(...page.events);
        truncated ||= page.truncated;
        if (!page.hasMore) break;
        if (page.nextSeq <= afterSeq) throw new Error("history page did not advance");
        afterSeq = page.nextSeq;
      }
      return { events, truncated };
    },
    /** Releases cached transcript data while retaining durable history. */
    evict: async (sessionId: string): Promise<void> => {
      await this.protocol.sessionEvict({ sessionId });
    },
    /** Stops an idle runtime; a subsequent restore can resume the conversation. */
    suspend: async (sessionId: string): Promise<void> => {
      await this.protocol.sessionSuspend({ sessionId });
    },
    /** Convenience for the same setting available at settings.configure(memory.pinned). */
    pin: async (sessionId: string, pinned = true): Promise<SessionInfo> => {
      await this.settings.configure({ target: { sessionId }, patch: { memory: { pinned } } });
      return this.sessions.get(sessionId);
    },
    /** Returns a handle on an existing session, e.g. after a UI reload. */
    attach: async (sessionId: string): Promise<Session> =>
      new Session(this, await this.sessions.get(sessionId)),
    /**
     * Restarts a stored session under the same ID, resuming the runtime's
     * conversation (the sidecar needs `--store` for sessions from earlier runs).
     */
    restore: async (sessionId: string): Promise<Session> => {
      const { session } = await this.protocol.sessionRestore({ sessionId });
      return new Session(this, session);
    },
    /**
     * Sets a session's title (one line, up to 200 characters; empty is
     * rejected). Works for stored sessions that aren't live.
     */
    rename: async (sessionId: string, title: string): Promise<SessionInfo> =>
      (await this.protocol.sessionRename({ sessionId, title })).session,
    /** Compatibility helper. New settings UIs can use settings.configure with a session target. */
    configure: async (sessionId: string, config: ConfigPatch): Promise<SessionInfo> =>
      (await this.protocol.sessionConfigure({ sessionId, config })).session,
    /** Closes a session if live and deletes its record and saved events. The runtime's transcript is kept. */
    delete: async (sessionId: string): Promise<void> => {
      await this.protocol.sessionDelete({ sessionId });
    },
  };

  /** Ordered lifecycle handlers owned by this connection. */
  readonly hooks = {
    /** Register before creating/prompting sessions. Returns an async disposer.
     * Named IDs can be required through settings.hooks.required. */
    register: async <N extends HookName>(
      name: N,
      handler: HookHandler<N>,
      options: Omit<HookSpec, "name" | "id"> & { id?: string } = {},
    ): Promise<() => Promise<void>> => {
      const id = options.id ?? crypto.randomUUID();
      if (this.#hooks.has(id)) throw new Error(`Hook ${id} is already registered.`);
      this.#hooks.set(id, handler as HookHandler);
      try {
        await this.protocol.hooksRegister({ hook: { ...options, id, name } });
      } catch (error) {
        this.#hooks.delete(id);
        throw error;
      }
      return () => this.hooks.unregister(id);
    },
    unregister: async (id: string): Promise<void> => {
      await this.protocol.hooksUnregister({ id });
      this.#hooks.delete(id);
    },
    list: async (): Promise<HookSpec[]> => (await this.protocol.hooksList({})).hooks,
  };

  /** Host tools, served to runtimes through the kit MCP bridge. */
  readonly tools = {
    register: async (defs: ToolDefinition | ToolDefinition[]): Promise<void> => {
      const list = Array.isArray(defs) ? defs : [defs];
      for (const d of list) {
        if (this.#tools.has(d.name)) throw new Error(`tool ${d.name} is already registered`);
      }
      for (const d of list) this.#tools.set(d.name, d); // before the request, so an immediate call finds it
      try {
        await this.protocol.toolsRegister({
          tools: list.map(({ handler: _handler, ...spec }) => spec),
        });
      } catch (e) {
        for (const d of list) this.#tools.delete(d.name);
        throw e;
      }
    },
    unregister: async (...names: string[]): Promise<void> => {
      await this.protocol.toolsUnregister({ names });
      for (const n of names) this.#tools.delete(n);
    },
    /** Every tool registered on the server (by any client). */
    list: async (): Promise<ToolSpec[]> => (await this.protocol.toolsList()).tools,
  };

  readonly mcp = {
    /**
     * The kit MCP servers (user and project files; needs `serve --kit-mcp`)
     * and, for `runtimes`, the servers configured in each runtime itself.
     */
    list: (opts: { cwd?: string; runtimes?: string[] } = {}): Promise<McpListResult> =>
      this.protocol.mcpList(opts),
  };

  readonly approvals = {
    /** Answers an approval request. */
    respond: async (approvalId: string, decision: Outcome | Decision): Promise<void> => {
      const d = typeof decision === "string" ? { outcome: decision } : decision;
      await this.protocol.approvalRespond({ approvalId, ...d });
    },
    /** Pending approvals (e.g. to rebuild a UI after reload). */
    list: async (sessionId?: string): Promise<ApprovalRequest[]> =>
      (await this.protocol.approvalList({ sessionId })).approvals,
  };

  /** Shuts the server down (closing every session) and closes the transport. */
  async close(): Promise<void> {
    await this.servers.close();
    if (!this.#rpc.closed) {
      try {
        await withTimeout(this.protocol.shutdown(), 10_000);
      } catch {
        // already gone, or slow: close the transport regardless
      }
    }
    await this.#transport.close();
  }
}

/** A handle on one session. */
export class Session {
  readonly #kit: HarnessKit;
  #info: SessionInfo;

  constructor(kit: HarnessKit, info: SessionInfo) {
    this.#kit = kit;
    this.#info = info;
  }

  get id(): string {
    return this.#info.sessionId;
  }
  get runtime(): string {
    return this.#info.runtime;
  }
  observe(options?: ConversationOptions): Conversation {
    return this.#kit.sessions.observe(this.id, options);
  }

  /** The session info as last fetched; see refresh(). */
  get info(): SessionInfo {
    return this.#info;
  }

  async refresh(): Promise<SessionInfo> {
    this.#info = await this.#kit.sessions.get(this.id);
    return this.#info;
  }

  /** Reads the current context observation; unknown counts remain null. */
  context(options?: RequestOptions): Promise<SessionContext> {
    return this.#kit.sessions.context(this.id, options);
  }

  /** Waits for native compaction. Abort the signal or call cancel() to interrupt it. */
  compact(options?: RequestOptions): Promise<SessionContext> {
    return this.#kit.sessions.compact(this.id, options);
  }

  /** Sets the session's title. */
  async rename(title: string): Promise<SessionInfo> {
    this.#info = await this.#kit.sessions.rename(this.id, title);
    return this.#info;
  }

  /** Updates defaults for future turns, preserving an already-running turn. */
  async configure(config: ConfigPatch): Promise<SessionInfo> {
    this.#info = await this.#kit.sessions.configure(this.id, config);
    return this.#info;
  }

  /**
   * Starts a turn. Iterate the result for its events (ending with
   * turn.completed), or await `turn.done`. Aborting `signal` cancels the
   * turn.
   */
  prompt(input: string | InputPart[], opts: PromptOptions = {}): Turn {
    const parts: InputPart[] = typeof input === "string" ? [{ type: "text", text: input }] : input;
    return new Turn(
      this.#kit,
      this.id,
      opts.turnId ?? newId(),
      parts,
      opts.signal,
      opts.config,
      opts.settings,
      opts.events,
    );
  }

  /** Runs a prompt without retaining streaming events. Subscribe separately if needed. */
  run(
    input: string | InputPart[],
    opts: Omit<PromptOptions, "events"> = {},
  ): Promise<TurnCompletedData> {
    return this.prompt(input, { ...opts, events: { mode: "none" } }).done;
  }

  /** Interrupts the active turn or manual compaction, if any. */
  async cancel(): Promise<void> {
    await this.#kit.protocol.sessionCancel({ sessionId: this.id });
  }

  /** Closes the session and releases its runtime. */
  async close(): Promise<void> {
    await this.#kit.protocol.sessionClose({ sessionId: this.id });
  }

  /** Fetches retained events with seq > afterSeq (for replay or gap recovery). */
  async events(
    opts: { afterSeq?: number; limit?: number } = {},
  ): Promise<{ events: HarnessEvent[]; truncated: boolean }> {
    return this.#kit.sessions.events(this.id, opts);
  }

  /** Reads one bounded history page without waking the runtime. */
  eventsPage(opts: { afterSeq?: number; limit?: number } = {}): Promise<EventPage> {
    return this.#kit.sessions.eventsPage(this.id, opts);
  }

  /** Observes this session's events. Returns an unsubscribe function. */
  onEvent(listener: EventListener): () => void {
    return this.#kit.onEvent((ev) => {
      if (ev.sessionId === this.id) listener(ev);
    });
  }
}

export interface PromptOptions {
  signal?: AbortSignal;
  turnId?: string;
  config?: ConfigPatch;
  settings?: SettingsPatch;
  events?: TurnEventOptions;
}

export interface TurnEventOptions {
  /** "none" keeps only the result; buffered is the default. */
  mode?: "buffered" | "none";
  /** Maximum retained events (default 256). Zero disables retention. */
  maxEvents?: number;
  /** Maximum UTF-8 JSON bytes retained (default 1 MiB). */
  maxBytes?: number;
}

/** The iterator fell behind. The agent continues; done still reports its result.
 * Read persisted session history to recover events instead of silently losing data. */
export class EventBufferOverflowError extends Error {
  constructor() {
    super("Turn event buffer exceeded its limit; read session history to recover events.");
    this.name = "EventBufferOverflowError";
  }
}

/**
 * A running turn: an async iterable of its events, in order, ending with
 * turn.completed. Events dropped by the server for a slow client are
 * recovered transparently. Iterate at most once.
 */
export class Turn implements AsyncIterable<HarnessEvent> {
  readonly id: string;
  readonly sessionId: string;
  /** Resolves with the turn.completed payload; rejects if the turn could not start. */
  readonly done: Promise<TurnCompletedData>;

  readonly #kit: HarnessKit;
  readonly #unsubscribeEvents: () => void;
  readonly #unsubscribeClose: () => void;
  #lastSeq = 0;
  #recovering = false;
  #recoveryNeeded = false;
  #wantedSeq = 0;
  #bufferBytes = 0;
  #maxEvents: number;
  #maxBytes: number;
  #discard = false;
  #iterated = false;
  #iteratorError: unknown;
  #buffer: HarnessEvent[] = [];
  #wake: (() => void) | undefined;
  #accepted: Promise<void> = Promise.resolve();
  #finished = false;
  #error: unknown;
  #resolve!: (d: TurnCompletedData) => void;
  #reject!: (e: unknown) => void;

  constructor(
    kit: HarnessKit,
    sessionId: string,
    turnId: string,
    input: InputPart[],
    signal?: AbortSignal,
    config?: ConfigPatch,
    settings?: SettingsPatch,
    events: TurnEventOptions = {},
  ) {
    this.#maxEvents = events.maxEvents ?? 256;
    this.#maxBytes = events.maxBytes ?? 1024 * 1024;
    for (const limit of [this.#maxEvents, this.#maxBytes])
      if (!Number.isSafeInteger(limit) || limit < 0)
        throw new RangeError("Turn buffer limits must be nonnegative safe integers.");
    if (events.mode !== undefined && !["buffered", "none"].includes(events.mode))
      throw new TypeError("Invalid turn event mode.");
    this.#discard = events.mode === "none" || this.#maxEvents === 0 || this.#maxBytes === 0;
    this.#kit = kit;
    this.id = turnId;
    this.sessionId = sessionId;
    this.done = new Promise((res, rej) => {
      this.#resolve = res;
      this.#reject = rej;
    });
    this.done.catch(() => {}); // avoid unhandled rejection if only iterated
    // Subscribe before sending the prompt so no event can be missed.
    this.#unsubscribeEvents = kit.onEvent((ev) => this.#ingest(ev));
    this.#unsubscribeClose = kit.onClose((err) =>
      this.#fail(err ?? new Error("connection closed")),
    );

    const onAbort = () => void kit.protocol.sessionCancel({ sessionId }).catch(() => {});
    if (signal) {
      if (signal.aborted) {
        queueMicrotask(() => this.#fail(signal.reason ?? new Error("Aborted")));
        return;
      } else signal.addEventListener("abort", onAbort, { once: true });
      void this.done.finally(() => signal.removeEventListener("abort", onAbort)).catch(() => {});
    }
    this.#accepted = kit.protocol
      .sessionPrompt({ sessionId, turnId, input, config, settings }, { signal })
      .then(
        () => {},
        (e) => this.#fail(e),
      );
  }

  /** Waits for prompt admission before interrupting this turn, so an immediate
   * cancel cannot overtake the prompt. Completed or rejected turns are no-ops. */
  async cancel(): Promise<void> {
    await this.#accepted;
    if (!this.#finished) await this.#kit.protocol.sessionCancel({ sessionId: this.sessionId });
  }

  get bufferedEvents(): number {
    return this.#buffer.length;
  }
  get bufferedBytes(): number {
    return this.#bufferBytes;
  }

  /** Stop retaining events without cancelling the agent. Idempotent. */
  discardEvents(): void {
    this.#discard = true;
    this.#buffer = [];
    this.#bufferBytes = 0;
    this.#wake?.();
  }

  #ingest(ev: HarnessEvent): void {
    if (this.#finished || ev.sessionId !== this.sessionId) return;
    if (ev.type === "stream.gap") {
      this.#recoveryNeeded = true;
      if (!this.#recovering) {
        this.#recovering = true;
        void this.#recover();
      }
    } else if (ev.turnId === this.id) {
      if (this.#recovering) {
        // History is the queue: retain only a high-water mark while replaying.
        this.#wantedSeq = Math.max(this.#wantedSeq, ev.seq);
      } else this.#accept(ev);
    }
  }

  async #recover(): Promise<void> {
    if (this.#finished) return;
    try {
      let cursor = this.#lastSeq;
      do {
        this.#recoveryNeeded = false;
        for (;;) {
          const page = await this.#kit.sessions.eventsPage(this.sessionId, {
            afterSeq: cursor,
            limit: Math.max(1, Math.min(200, this.#maxEvents || 200)),
          });
          if (page.truncated && !this.#discard) {
            this.#iteratorError = new Error(
              "Retained history omitted turn events; the event stream is incomplete.",
            );
            this.discardEvents();
          }
          for (const ev of page.events) if (ev.turnId === this.id) this.#accept(ev);
          if (this.#finished) break;
          const advanced = page.nextSeq > cursor;
          cursor = Math.max(cursor, page.nextSeq);
          if (!page.hasMore) {
            if (!advanced && this.#wantedSeq > this.#lastSeq)
              throw new Error("Retained history cannot recover the turn gap.");
            break;
          }
          if (!advanced) throw new Error("History page did not advance during gap recovery.");
        }
      } while ((this.#recoveryNeeded || this.#wantedSeq > this.#lastSeq) && !this.#finished);
    } catch (e) {
      this.#fail(e);
    } finally {
      this.#recovering = false;
    }
  }

  #accept(ev: HarnessEvent): void {
    if (this.#finished || ev.seq <= this.#lastSeq) return;
    this.#lastSeq = ev.seq;
    if (!this.#discard && !this.#iteratorError) {
      const bytes = eventBytes(ev);
      if (this.#buffer.length >= this.#maxEvents || this.#bufferBytes + bytes > this.#maxBytes) {
        this.#iteratorError = new EventBufferOverflowError();
        this.discardEvents();
      } else {
        this.#buffer.push(structuredClone(ev));
        this.#bufferBytes += bytes;
      }
    }
    if (ev.type === "turn.completed") {
      this.#finished = true;
      this.#detach();
      this.#resolve(ev.data);
    }
    this.#wake?.();
  }

  #fail(err: unknown): void {
    if (this.#finished) return;
    this.#finished = true;
    this.#error = err;
    this.#detach();
    this.#reject(err);
    this.#wake?.();
  }

  #detach(): void {
    this.#unsubscribeEvents();
    this.#unsubscribeClose();
  }

  async *[Symbol.asyncIterator](): AsyncIterator<HarnessEvent> {
    if (this.#iterated) throw new Error("A turn can only be iterated once.");
    this.#iterated = true;
    try {
      for (;;) {
        if (this.#iteratorError) throw this.#iteratorError;
        if (this.#buffer.length) {
          const event = this.#buffer.shift()!;
          this.#bufferBytes -= eventBytes(event);
          yield event;
          continue;
        }
        if (this.#finished || this.#discard) {
          if (this.#error !== undefined) throw this.#error;
          return;
        }
        await new Promise<void>((resolve) => {
          this.#wake = resolve;
        });
        this.#wake = undefined;
      }
    } finally {
      this.discardEvents();
    }
  }
}

/**
 * Adapts a callback-style message channel into a Transport, e.g. Electron
 * IPC between renderer and main, or Wails events to a Go backend that
 * forwards lines to the sidecar.
 */
export function callbackTransport(channel: {
  send: (message: string) => void;
  subscribe: (onMessage: (message: string) => void) => () => void;
  close?: () => void | Promise<void>;
}): Transport & { emitClose: (error?: Error) => void } {
  const closeHandlers: ((e?: Error) => void)[] = [];
  let unsubscribe: (() => void) | undefined;
  return {
    send: channel.send,
    onMessage(handler) {
      unsubscribe = channel.subscribe(handler);
    },
    onClose(handler) {
      closeHandlers.push(handler);
    },
    emitClose(error) {
      for (const h of closeHandlers) h(error);
    },
    async close() {
      unsubscribe?.();
      await channel.close?.();
      for (const h of closeHandlers) h();
    },
  };
}

function newId(): string {
  const c = (globalThis as { crypto?: { randomUUID?: () => string } }).crypto;
  if (c?.randomUUID) return c.randomUUID();
  return "turn_" + Math.random().toString(36).slice(2) + Date.now().toString(36);
}

function withTimeout<T>(p: Promise<T>, ms: number): Promise<T> {
  return new Promise((resolve, reject) => {
    const t = setTimeout(() => reject(new Error("timeout")), ms);
    p.then(
      (v) => {
        clearTimeout(t);
        resolve(v);
      },
      (e) => {
        clearTimeout(t);
        reject(e);
      },
    );
  });
}

function eventBytes(event: HarnessEvent): number {
  return new TextEncoder().encode(JSON.stringify(event)).byteLength;
}
