import { RemoteError, scopeKey } from "./remote.ts";
import { HarnessKitError } from "./rpc.ts";
import type { ServerScope } from "./remote.ts";
import type {
  Change,
  Event,
  Push,
  RemoteProtocol,
  UpdateSession,
  DeleteSession,
  SendCommand,
} from "./remote.gen.ts";
import type { EventPage, SyncSettings } from "./protocol.ts";

export interface SyncBinding {
  sessionId: string;
  title: string;
  visibility: "private" | "shared";
  uploadedSeq: number;
  deleted?: boolean;
  epoch?: number;
  /** Persisted before HTTP; cleared atomically with uploadedSeq after its ACK. */
  pending?: Push;
}
export interface SyncCheckpoint {
  cursor: number;
  commands?: SendCommand[];
  bindings: SyncBinding[];
  mutations?: (
    { kind: "update"; input: UpdateSession } | { kind: "delete"; input: DeleteSession }
  )[];
}
export interface CachedSession extends Change {
  cachedSeq: number;
}
/** One owner per store directory/database namespace. Writes must be atomic and
 * durable before resolving. Cache append must be contiguous and crash-safe;
 * publishing its head happens only after event data is durable. Reads return
 * independent snapshots. No credentials. */
export interface SyncStore {
  claim(sessionId: string, scope: string): Promise<void>;
  load(scope: string): Promise<SyncCheckpoint>;
  save(scope: string, checkpoint: SyncCheckpoint): Promise<void>;
  list(scope: string): Promise<CachedSession[]>;
  head(scope: string, sessionId: string): Promise<number>;
  append(scope: string, change: Change, events: Event[]): Promise<void>;
  events(scope: string, sessionId: string, after: number, limit: number): Promise<Event[]>;
}
export interface DurableHistory {
  durableEventsPage(
    sessionId: string,
    options: { afterSeq: number; limit: number },
  ): Promise<EventPage>;
}
export type SyncPhase =
  "stopped" | "paused" | "syncing" | "idle" | "offline" | "authentication_required" | "blocked";
export interface SyncStatus {
  phase: SyncPhase;
  /** Stable, credential-free reason. Never includes response bodies or tokens. */
  code?: string;
  sessionId?: string;
  operationId?: string;
}
export class SyncError extends Error {
  readonly code: string;
  constructor(code: string) {
    super(code);
    this.code = code;
    this.name = "SyncError";
  }
}
export interface SessionSyncOptions {
  scope: ServerScope;
  protocol: Pick<
    RemoteProtocol,
    "push" | "changes" | "events" | "updateSession" | "deleteSession" | "sendCommand" | "getSession"
  >;
  store: SyncStore;
  source: DurableHistory;
  settings: () => Promise<SyncSettings>;
}

/** A scope-bound worker. publish explicitly opts in a local session; start
 * resumes persisted bindings. Incoming transcripts stay in a separate cache:
 * they never start a runtime or overwrite a local execution session. */
export class SessionSync {
  readonly #options: SessionSyncOptions;
  readonly #key: string;
  #tail: Promise<unknown> = Promise.resolve();
  #controller?: AbortController;
  #worker?: Promise<void>;
  #offset = 0;
  #status: SyncStatus = { phase: "stopped" };
  readonly #listeners = new Set<(status: SyncStatus) => void>();
  constructor(options: SessionSyncOptions) {
    this.#options = options;
    this.#key = scopeKey(options.scope);
  }
  get status(): SyncStatus {
    return { ...this.#status };
  }
  onStatus(listener: (status: SyncStatus) => void): () => void {
    this.#listeners.add(listener);
    return () => {
      this.#listeners.delete(listener);
    };
  }
  #set(status: SyncStatus): void {
    this.#status = status;
    for (const listener of this.#listeners) {
      // UI observer failures must not change persistence or worker progress.
      try {
        listener({ ...status });
      } catch {
        /* observer-owned failure */
      }
    }
  }
  #exclusive<T>(work: () => Promise<T>): Promise<T> {
    const next = this.#tail.catch(() => {}).then(work);
    this.#tail = next;
    return next;
  }
  /** The initial upload title and visibility are immutable. Use update() for
   * renames. Selecting another tenant never redirects existing uploads. */
  publish(
    sessionId: string,
    options: { title: string; visibility?: "private" | "shared" },
  ): Promise<void> {
    return this.#exclusive(async () => {
      if (
        !/^[A-Za-z0-9_-]{1,128}$/.test(sessionId) ||
        new TextEncoder().encode(options.title).length > 500
      )
        throw new SyncError("invalid_binding");
      const binding: SyncBinding = {
        sessionId,
        title: options.title,
        visibility: options.visibility ?? "private",
        uploadedSeq: 0,
      };
      if (!["private", "shared"].includes(binding.visibility))
        throw new SyncError("invalid_binding");
      const state = await this.#options.store.load(this.#key);
      const existing = state.bindings.find((b) => b.sessionId === sessionId);
      if (
        existing &&
        (existing.title !== binding.title || existing.visibility !== binding.visibility)
      )
        throw new SyncError("binding_is_immutable");
      // A failure here must never silently fall back to UI/live history.
      const page = await this.#options.source.durableEventsPage(sessionId, {
        afterSeq: 0,
        limit: 1,
      });
      if (page.truncated) throw new SyncError("history_gap");
      await this.#options.store.claim(sessionId, this.#key);
      if (!existing) {
        state.bindings.push(binding);
        await this.#options.store.save(this.#key, state);
      }
    });
  }
  sessions(): Promise<CachedSession[]> {
    return this.#options.store.list(this.#key);
  }
  /** Persist a newly acquired execution epoch and reconcile pending uploads.
   * Unknown network outcomes stay pending; remote-history conflicts stop recovery. */
  useLease(lease: import("./remote.gen.ts").Lease): Promise<void> {
    return this.#exclusive(async () => {
      const state = await this.#options.store.load(this.#key);
      const binding = state.bindings.find((b) => b.sessionId === lease.sessionId);
      if (!binding || lease.epoch < 1) throw new SyncError("binding_required");
      if (binding.pending) {
        const batch = binding.pending;
        try {
          // Receipts remain valid after expiry. Resolve a possibly committed old
          // operation before changing its ID or fencing epoch.
          const ack = await this.#options.protocol.push(batch);
          if (
            ack.sessionId !== batch.sessionId ||
            ack.lastSeq !== batch.expectedSeq + batch.events.length
          )
            throw new SyncError("invalid_acknowledgement");
          binding.uploadedSeq = ack.lastSeq;
          delete binding.pending;
        } catch (error) {
          if (!(error instanceof RemoteError) || error.status !== 409) throw error;
          const current = await this.#options.protocol.getSession({ sessionId: lease.sessionId });
          if (
            current.writerId !== lease.deviceId ||
            current.writerEpoch !== lease.epoch ||
            current.lastSeq !== batch.expectedSeq ||
            current.deleted
          )
            throw new SyncError("history_conflict");
          // The new epoch fences all old uploads. A rejected, uncommitted batch
          // can now be resubmitted without re-executing the local prompt.
          binding.pending = {
            ...batch,
            epoch: lease.epoch,
            operationId: globalThis.crypto.randomUUID(),
          };
        }
      }
      binding.epoch = lease.epoch;
      await this.#options.store.save(this.#key, state);
    });
  }
  /** Durably queue a full metadata replacement. A stale revision blocks with a
   * conflict; the host decides how to merge instead of losing somebody's edits. */
  update(input: Omit<UpdateSession, "operationId">): Promise<string> {
    return this.#queueMutation({
      kind: "update",
      input: { ...input, operationId: globalThis.crypto.randomUUID() },
    });
  }
  delete(input: Omit<DeleteSession, "operationId">): Promise<string> {
    return this.#queueMutation({
      kind: "delete",
      input: { ...input, operationId: globalThis.crypto.randomUUID() },
    });
  }
  #queueMutation(mutation: NonNullable<SyncCheckpoint["mutations"]>[number]): Promise<string> {
    const snapshot = structuredClone(mutation);
    return this.#exclusive(async () => {
      const state = await this.#options.store.load(this.#key);
      (state.mutations ??= []).push(snapshot);
      await this.#options.store.save(this.#key, state);
      return snapshot.input.operationId;
    });
  }
  /** Explicitly discard a rejected metadata edit before queueing a merged edit.
   * Never call for an uncertain network outcome: retry its operation ID first. */
  discardMutation(operationId: string): Promise<void> {
    return this.#exclusive(async () => {
      const state = await this.#options.store.load(this.#key);
      state.mutations = state.mutations?.filter((m) => m.input.operationId !== operationId);
      await this.#options.store.save(this.#key, state);
    });
  }
  /** Commands are durable before sending and retain their ID across retries.
   * Completion means host admission; inspect getCommand and the transcript for
   * the outcome. An uncertain command is never automatically executed again. */
  command(input: Omit<SendCommand, "id">): Promise<string> {
    const snapshot = structuredClone({ ...input, id: globalThis.crypto.randomUUID() });
    return this.#exclusive(async () => {
      const state = await this.#options.store.load(this.#key);
      (state.commands ??= []).push(snapshot);
      await this.#options.store.save(this.#key, state);
      return snapshot.id;
    });
  }
  pending(): Promise<Pick<SyncCheckpoint, "mutations" | "commands">> {
    return this.#exclusive(async () => {
      const { mutations, commands } = await this.#options.store.load(this.#key);
      return { mutations, commands };
    });
  }
  /** Only discard a definitively rejected command, never a lost response. */
  discardCommand(id: string): Promise<void> {
    return this.#exclusive(async () => {
      const state = await this.#options.store.load(this.#key);
      state.commands = state.commands?.filter((c) => c.id !== id);
      await this.#options.store.save(this.#key, state);
    });
  }
  eventsPage(
    sessionId: string,
    options: { after?: number; limit?: number } = {},
  ): Promise<Event[]> {
    return this.#options.store.events(
      this.#key,
      sessionId,
      options.after ?? 0,
      options.limit ?? 100,
    );
  }
  /** One bounded pass, also useful for hosts with their own scheduler. Errors
   * remain observable; start() handles retries and updates status automatically. */
  syncOnce(signal?: AbortSignal): Promise<void> {
    return this.#exclusive(async () => {
      signal?.throwIfAborted();
      const { store, source, protocol } = this.#options;
      let failure: unknown;
      let failedSession: string | undefined;
      let failedOperation: string | undefined;
      try {
        const policy = await this.#options.settings();
        if (policy.paused) {
          this.#set({ phase: "paused" });
          return;
        }
        this.#set({ phase: "syncing" });
        const state = await store.load(this.#key);
        for (const mutation of (state.mutations ?? []).slice(0, policy.workPerCycle)) {
          try {
            const change = await (mutation.kind === "update"
              ? protocol.updateSession(mutation.input, { signal })
              : protocol.deleteSession(mutation.input, { signal }));
            if (
              change.sessionId !== mutation.input.sessionId ||
              change.revision !== mutation.input.expectedRevision + 1
            )
              throw new SyncError("invalid_acknowledgement");
            state.mutations = state.mutations!.filter(
              (m) => m.input.operationId !== mutation.input.operationId,
            );
            await store.save(this.#key, state);
          } catch (error) {
            if (!failure) failedOperation = mutation.input.operationId;
            failure ??= error;
            failedSession ??= mutation.input.sessionId;
            state.mutations = (await store.load(this.#key)).mutations;
            break;
          }
        }
        for (const command of (state.commands ?? []).slice(0, policy.workPerCycle)) {
          try {
            const ack = await protocol.sendCommand(command, { signal });
            if (
              ack.id !== command.id ||
              ack.sessionId !== command.sessionId ||
              ack.epoch !== command.epoch
            )
              throw new SyncError("invalid_acknowledgement");
            state.commands = state.commands!.filter((c) => c.id !== command.id);
            await store.save(this.#key, state);
          } catch (error) {
            if (!failure) failedOperation = command.id;
            failure ??= error;
            failedSession ??= command.sessionId;
            state.commands = (await store.load(this.#key)).commands;
            break;
          }
        }
        // Round-robin prevents a large or broken session starving its siblings.
        const count = Math.min(state.bindings.length, policy.workPerCycle);
        for (let i = 0; i < count; i++) {
          signal?.throwIfAborted();
          const binding = state.bindings[this.#offset++ % state.bindings.length]!;
          if (binding.deleted) continue;
          try {
            if (!binding.pending) {
              const page = await source.durableEventsPage(binding.sessionId, {
                afterSeq: binding.uploadedSeq,
                limit: policy.batchEvents,
              });
              if (page.truncated) throw new SyncError("history_gap");
              let seq = binding.uploadedSeq;
              const events = [];
              let bytes = 0;
              for (const event of page.events) {
                if (event.seq !== seq + 1 || event.sessionId !== binding.sessionId)
                  throw new SyncError("history_gap");
                const size = new TextEncoder().encode(JSON.stringify(event)).length;
                if (size > 65536) throw new SyncError("event_too_large");
                if (bytes + size > 900_000) break;
                events.push(event);
                bytes += size;
                seq++;
              }
              if (!events.length) continue;
              binding.pending = {
                operationId: globalThis.crypto.randomUUID(),
                sessionId: binding.sessionId,
                expectedSeq: binding.uploadedSeq,
                epoch: binding.epoch ?? 0,
                title: binding.title,
                visibility: binding.visibility,
                events,
              };
              await store.save(this.#key, state);
            }
            const batch = binding.pending;
            const ack = await protocol.push(batch, { signal });
            if (
              ack.sessionId !== binding.sessionId ||
              ack.lastSeq !== batch.expectedSeq + batch.events.length
            )
              throw new SyncError("invalid_acknowledgement");
            binding.uploadedSeq = ack.lastSeq;
            delete binding.pending;
            await store.save(this.#key, state);
          } catch (error) {
            // Reload after a failed write: never reuse an in-memory checkpoint
            // whose durable commit is uncertain, even for a different binding.
            failure ??= error;
            failedSession ??= binding.sessionId;
            const saved = await store.load(this.#key);
            state.bindings = saved.bindings;
          }
        }
        // A feed cursor moves only after all corresponding event pages are saved.
        // One change per fetch keeps memory and per-cycle replay work bounded.
        for (let i = 0; i < policy.workPerCycle; i++) {
          signal?.throwIfAborted();
          const page = await protocol.changes(
            { after: state.cursor, limit: 1, waitMs: 0 },
            { signal },
          );
          if (
            !Number.isSafeInteger(page.cursor) ||
            page.cursor < state.cursor ||
            page.changes.length > 1
          )
            throw new SyncError("invalid_change_cursor");
          const change = page.changes[0];
          if (change) {
            if (
              change.cursor <= state.cursor ||
              change.cursor > page.cursor ||
              !Number.isSafeInteger(change.lastSeq) ||
              change.lastSeq < 0
            )
              throw new SyncError("invalid_change_cursor");
            let head = await store.head(this.#key, change.sessionId);
            if (change.deleted) {
              await store.append(this.#key, change, []);
              const binding = state.bindings.find((b) => b.sessionId === change.sessionId);
              if (binding) {
                binding.deleted = true;
                delete binding.pending;
              }
            } else if (head < change.lastSeq) {
              const events = await protocol.events(
                { sessionId: change.sessionId, after: head, limit: policy.batchEvents },
                { signal },
              );
              if (!events.events.length || events.events.length > policy.batchEvents)
                throw new SyncError("remote_history_gap");
              for (const event of events.events)
                if (event.seq !== ++head) throw new SyncError("remote_history_gap");
              if (events.nextAfter !== head) throw new SyncError("remote_history_gap");
              await store.append(this.#key, change, events.events);
            } else await store.append(this.#key, change, []);
            if (!change.deleted && head < change.lastSeq) continue;
          }
          state.cursor = page.cursor;
          await store.save(this.#key, state);
          if (!change) break;
        }
        if (failure) throw failure;
        this.#set({ phase: "idle" });
      } catch (error) {
        if (!signal?.aborted)
          this.#set({ ...classify(error), sessionId: failedSession, operationId: failedOperation });
        throw error;
      }
    });
  }
  /** Idempotent. No unhandled background promise; inspect status/onStatus. */
  start(): void {
    if (this.#worker) return;
    const controller = new AbortController();
    this.#controller = controller;
    this.#worker = (async () => {
      let failures = 0;
      while (!controller.signal.aborted) {
        try {
          await this.syncOnce(controller.signal);
          failures = 0;
        } catch {
          if (controller.signal.aborted) break;
          failures++;
        }
        let interval = 1000;
        try {
          interval = (await this.#options.settings()).pollIntervalMs;
        } catch {
          /* disconnected sidecar */
        }
        const backoff = failures
          ? Math.min(30_000, interval * 2 ** Math.min(failures, 5))
          : interval;
        await wait(backoff, controller.signal);
      }
    })().finally(() => {
      this.#worker = undefined;
      this.#controller = undefined;
      this.#set({ phase: "stopped" });
    });
  }
  async stop(): Promise<void> {
    this.#controller?.abort();
    await this.#worker;
    await this.#tail.catch(() => {});
    this.#set({ phase: "stopped" });
  }
}
function classify(error: unknown): SyncStatus {
  if (error instanceof HarnessKitError && error.reason === "durable_history_unavailable")
    return { phase: "blocked", code: "durable_history_unavailable" };
  if (error instanceof RemoteError) {
    if (error.status === 401 || error.status === 403)
      return { phase: "authentication_required", code: "authorization_required" };
    if (error.status >= 500 || error.status === 429)
      return { phase: "offline", code: "server_unavailable" };
    return { phase: "blocked", code: error.status === 409 ? "conflict" : "request_rejected" };
  }
  if (error instanceof TypeError || (error instanceof Error && error.name === "TimeoutError"))
    return { phase: "offline", code: "network_unavailable" };
  return { phase: "blocked", code: error instanceof SyncError ? error.code : "local_sync_error" };
}
function wait(ms: number, signal: AbortSignal): Promise<void> {
  if (signal.aborted) return Promise.resolve();
  return new Promise((resolve) => {
    const done = () => {
      clearTimeout(timer);
      signal.removeEventListener("abort", done);
      resolve();
    };
    const timer = setTimeout(done, ms);
    signal.addEventListener("abort", done, { once: true });
  });
}
