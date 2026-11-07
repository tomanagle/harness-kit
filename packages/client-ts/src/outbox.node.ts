import { createHash, randomUUID } from "node:crypto";
import { mkdir, open, readFile, rename, rm, stat } from "node:fs/promises";
import { join } from "node:path";
import type { Push, SyncOutbox } from "./remote.ts";

export interface FileOutboxOptions {
  maxBytes?: number;
  maxOperations?: number;
}
interface Journal {
  version: 1;
  scope: string;
  items: Push[];
}
/** A bounded durable upload queue for Node/Electron main processes. One instance
 * owns this directory; do not share it between sidecars or OS processes. Files
 * contain explicitly enqueued session content, never authentication credentials. */
export function fileSyncOutbox(directory: string, options: FileOutboxOptions = {}): SyncOutbox {
  const maxBytes = options.maxBytes ?? 32 * 1024 * 1024;
  const maxOperations = options.maxOperations ?? 1000;
  if (
    !Number.isSafeInteger(maxBytes) ||
    maxBytes < 1 ||
    !Number.isSafeInteger(maxOperations) ||
    maxOperations < 1
  )
    throw new Error("Outbox limits must be positive integers");
  const locks = new Map<string, Promise<unknown>>();
  const pathFor = (scope: string) =>
    join(directory, createHash("sha256").update(scope).digest("hex") + ".json");
  async function locked<T>(scope: string, work: () => Promise<T>): Promise<T> {
    const previous = locks.get(scope) ?? Promise.resolve();
    const next = previous.catch(() => {}).then(work);
    locks.set(scope, next);
    try {
      return await next;
    } finally {
      if (locks.get(scope) === next) locks.delete(scope);
    }
  }
  async function read(scope: string): Promise<Journal> {
    const path = pathFor(scope);
    try {
      if ((await stat(path)).size > maxBytes)
        throw new Error("Outbox exceeds configured byte limit");
      const journal = JSON.parse(await readFile(path, "utf8")) as Journal;
      if (journal.version !== 1 || journal.scope !== scope || !Array.isArray(journal.items))
        throw new Error("Invalid outbox journal");
      return journal;
    } catch (error) {
      if ((error as NodeJS.ErrnoException).code === "ENOENT")
        return { version: 1, scope, items: [] };
      throw error;
    }
  }
  async function write(journal: Journal): Promise<void> {
    const bytes = JSON.stringify(journal);
    if (Buffer.byteLength(bytes) > maxBytes || journal.items.length > maxOperations)
      throw new Error("Outbox capacity reached; flush before adding more operations");
    await mkdir(directory, { recursive: true, mode: 0o700 });
    const path = pathFor(journal.scope);
    const temporary = path + "." + randomUUID() + ".tmp";
    try {
      const file = await open(temporary, "wx", 0o600);
      try {
        await file.writeFile(bytes);
        await file.sync();
      } finally {
        await file.close();
      }
      await rename(temporary, path);
      // Persist the rename on platforms that permit fsync on directory handles.
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
  return {
    async put(scope, batch) {
      // Snapshot before yielding so caller mutations cannot alter queued operations.
      const encoded = JSON.stringify(batch);
      const snapshot = JSON.parse(encoded) as Push;
      if (
        !snapshot.operationId ||
        !snapshot.sessionId ||
        !Number.isSafeInteger(snapshot.expectedSeq) ||
        snapshot.expectedSeq < 0 ||
        !Array.isArray(snapshot.events)
      )
        throw new Error("Invalid sync batch");
      await locked(scope, async () => {
        const journal = await read(scope);
        const existing = journal.items.find((item) => item.operationId === snapshot.operationId);
        if (existing) {
          if (JSON.stringify(existing) !== encoded)
            throw new Error("Operation ID already has a different payload");
          return;
        }
        journal.items.push(snapshot);
        await write(journal);
      });
    },
    async list(scope, limit) {
      if (!Number.isSafeInteger(limit) || limit < 1 || limit > 100)
        throw new Error("Outbox page limit must be 1 to 100");
      return locked(scope, async () => (await read(scope)).items.slice(0, limit));
    },
    async acknowledge(scope, operationId) {
      await locked(scope, async () => {
        const journal = await read(scope);
        journal.items = journal.items.filter((item) => item.operationId !== operationId);
        await write(journal);
      });
    },
  };
}
