import { createHash, randomUUID } from "node:crypto";
import { mkdir, open, readFile, rename, rm, stat } from "node:fs/promises";
import { join } from "node:path";
import { SyncError } from "./sync.ts";
import type { CachedSession, SyncCheckpoint, SyncStore } from "./sync.ts";
import type { Event } from "./remote.gen.ts";

export interface FileSyncStoreOptions {
  /** Bounds checkpoint/index decoding, including pending uploads. Default 32 MiB. */
  maxStateBytes?: number;
  /** Per scope. Default 1000 local bindings and 1000 cached sessions. */
  maxSessions?: number;
  /** Per cached transcript; exhaustion stops replay without advancing its cursor. Default 256 MiB. */
  maxSessionBytes?: number;
}
interface CacheHead extends CachedSession {
  bytes: number;
  purgeThrough?: number;
}

/** Node/Electron durable sync state. Use one instance/owner per directory;
 * separate processes need separate directories. Events are sharded into fixed
 * 25-event pages, so neither appending nor replay loads a whole transcript.
 * Settings/credentials and remote runtime execution state are not stored here. */
export function fileSyncStore(directory: string, options: FileSyncStoreOptions = {}): SyncStore {
  const maxStateBytes = options.maxStateBytes ?? 32 * 1024 * 1024;
  const maxSessions = options.maxSessions ?? 1000;
  const maxSessionBytes = options.maxSessionBytes ?? 256 * 1024 * 1024;
  for (const value of [maxStateBytes, maxSessions, maxSessionBytes])
    if (!Number.isSafeInteger(value) || value < 1)
      throw new Error("Sync store limits must be positive integers");
  const path = (...keys: (string | number)[]) =>
    join(directory, createHash("sha256").update(JSON.stringify(keys)).digest("hex") + ".json");
  let tail: Promise<unknown> = Promise.resolve();
  function lock<T>(work: () => Promise<T>): Promise<T> {
    const next = tail.catch(() => {}).then(work);
    tail = next;
    return next;
  }
  async function read<T>(file: string, fallback: T, limit = maxStateBytes): Promise<T> {
    try {
      if ((await stat(file)).size > limit) throw new SyncError("store_capacity");
      return JSON.parse(await readFile(file, "utf8")) as T;
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === "ENOENT") return fallback;
      throw error;
    }
  }
  async function write(file: string, value: unknown, limit = maxStateBytes): Promise<void> {
    const bytes = JSON.stringify(value);
    if (Buffer.byteLength(bytes) > limit) throw new SyncError("store_capacity");
    await mkdir(directory, { recursive: true, mode: 0o700 });
    const temporary = file + "." + randomUUID() + ".tmp";
    try {
      const handle = await open(temporary, "wx", 0o600);
      try {
        await handle.writeFile(bytes);
        await handle.sync();
      } finally {
        await handle.close();
      }
      await rename(temporary, file);
      if (process.platform !== "win32") {
        const dir = await open(directory, "r");
        try {
          await dir.sync();
        } finally {
          await dir.close();
        }
      }
    } finally {
      await rm(temporary, { force: true });
    }
  }
  const index = (scope: string) => read<CacheHead[]>(path("cache", scope), []);
  const chunkSize = 25;
  const chunkLimit = 2 * 1024 * 1024;
  return {
    claim(sessionId, scope) {
      return lock(async () => {
        const key = path("destination", sessionId);
        const existing = await read<string | undefined>(key, undefined);
        if (existing && existing !== scope) throw new SyncError("destination_is_immutable");
        if (!existing) await write(key, scope);
      });
    },
    load(scope) {
      return lock(async () => {
        const state = await read<SyncCheckpoint>(path("state", scope), { cursor: 0, bindings: [] });
        if (
          !Number.isSafeInteger(state.cursor) ||
          state.cursor < 0 ||
          !Array.isArray(state.bindings) ||
          state.bindings.length > maxSessions
        )
          throw new SyncError("invalid_checkpoint");
        return state;
      });
    },
    save(scope, state) {
      const snapshot = structuredClone(state);
      return lock(async () => {
        if (snapshot.bindings.length > maxSessions) throw new SyncError("store_capacity");
        await write(path("state", scope), snapshot);
      });
    },
    list(scope) {
      return lock(async () =>
        (await index(scope)).map(
          ({ bytes: _bytes, purgeThrough: _purgeThrough, ...session }) => session,
        ),
      );
    },
    head(scope, sessionId) {
      return lock(
        async () => (await index(scope)).find((s) => s.sessionId === sessionId)?.cachedSeq ?? 0,
      );
    },
    append(scope, change, events) {
      const incoming = structuredClone(events);
      const metadata = structuredClone(change);
      return lock(async () => {
        const sessions = await index(scope);
        const existing = sessions.find((s) => s.sessionId === metadata.sessionId);
        const previous = existing?.cachedSeq ?? 0;
        if (!existing && sessions.length >= maxSessions) throw new SyncError("store_capacity");
        if (metadata.deleted || existing?.deleted) {
          const tombstone = {
            ...metadata,
            deleted: true,
            cachedSeq: 0,
            bytes: 0,
            purgeThrough: existing?.purgeThrough ?? previous,
          };
          if (existing) sessions[sessions.indexOf(existing)] = tombstone;
          else sessions.push(tombstone);
          await write(path("cache", scope), sessions);
          for (let block = 0; block < Math.ceil(tombstone.purgeThrough / chunkSize); block++)
            await rm(path("events", scope, metadata.sessionId, block), { force: true });
          tombstone.purgeThrough = 0;
          await write(path("cache", scope), sessions);
          return;
        }
        let seq = previous;
        let bytes = existing?.bytes ?? 0;
        if (incoming.length > 100) throw new SyncError("invalid_cache_page");
        for (const event of incoming) {
          if (event.seq !== ++seq) throw new SyncError("remote_history_gap");
          const size = Buffer.byteLength(JSON.stringify(event));
          if (size > 66000) throw new SyncError("event_too_large");
          bytes += size;
        }
        if (bytes > maxSessionBytes || (!existing && sessions.length >= maxSessions))
          throw new SyncError("store_capacity");
        // Overwrite an unpublished tail after a crash. Readers only observe
        // chunks up to the committed index head, never speculative file bytes.
        for (let offset = 0; offset < incoming.length;) {
          const start = incoming[offset]!.seq;
          const block = Math.floor((start - 1) / chunkSize);
          const key = path("events", scope, metadata.sessionId, block);
          const prefixLength = (start - 1) % chunkSize;
          const old = await read<Event[]>(key, [], chunkLimit);
          const prefix = old.slice(0, prefixLength);
          if (prefix.length !== prefixLength) throw new SyncError("cache_gap");
          const part = incoming.slice(offset, offset + chunkSize - prefixLength);
          await write(key, [...prefix, ...part], chunkLimit);
          offset += part.length;
        }
        const next: CacheHead = { ...metadata, cachedSeq: seq, bytes };
        // Replaying an older change may fill more events, but cannot roll back metadata.
        if (existing && existing.cursor > next.cursor)
          Object.assign(next, existing, { cachedSeq: seq, bytes });
        if (existing) sessions[sessions.indexOf(existing)] = next;
        else sessions.push(next);
        await write(path("cache", scope), sessions);
      });
    },
    events(scope, sessionId, after, limit) {
      return lock(async () => {
        if (
          !Number.isSafeInteger(after) ||
          after < 0 ||
          !Number.isSafeInteger(limit) ||
          limit < 1 ||
          limit > 100
        )
          throw new SyncError("invalid_cache_page");
        const head = (await index(scope)).find((s) => s.sessionId === sessionId)?.cachedSeq ?? 0;
        const result: Event[] = [];
        let bytes = 0;
        for (let seq = after + 1; seq <= head && result.length < limit;) {
          const block = Math.floor((seq - 1) / chunkSize);
          const chunk = await read<Event[]>(
            path("events", scope, sessionId, block),
            [],
            chunkLimit,
          );
          while (
            seq <= head &&
            Math.floor((seq - 1) / chunkSize) === block &&
            result.length < limit
          ) {
            const event = chunk[(seq - 1) % chunkSize];
            if (event?.seq !== seq) throw new SyncError("cache_gap");
            bytes += Buffer.byteLength(JSON.stringify(event));
            if (bytes > 1024 * 1024 && result.length) return result;
            result.push(event);
            seq++;
          }
        }
        return result;
      });
    },
  };
}
