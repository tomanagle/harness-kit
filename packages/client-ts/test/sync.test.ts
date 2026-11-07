import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { SessionSync, SyncError } from "../src/sync.ts";
import type { SyncStore } from "../src/sync.ts";
import { fileSyncStore } from "../src/sync.node.ts";
import { RemoteError, scopeKey } from "../src/remote.ts";
import type { ServerScope, Push, Ack, Change, Event } from "../src/remote.ts";
import type { EventPage, HarnessEvent, SyncSettings } from "../src/protocol.ts";

const scope: ServerScope = {
  serverId: "work",
  url: "https://work.example",
  accountId: "alice",
  tenantId: "work",
  applicationId: "sprint",
};
const policy: SyncSettings = {
  paused: false,
  pollIntervalMs: 100,
  batchEvents: 2,
  workPerCycle: 2,
};
const history = (id: string, count = 7) =>
  Array.from(
    { length: count },
    (_, i) =>
      ({
        sessionId: id,
        seq: i + 1,
        time: "2026-01-01T00:00:00Z",
        type: "message.delta",
        data: { text: `${i}` },
      }) as HarnessEvent,
  );
function fixture(store: SyncStore) {
  const remote = new Map<string, Event[]>();
  const receipts = new Map<string, Ack>();
  const changes: Change[] = [];
  const calls: Push[] = [];
  let lostAck = false;
  let blocked = "";
  let gap = false;
  const settings = { ...policy };
  const options = {
    scope,
    store,
    settings: async () => settings,
    source: {
      async durableEventsPage(
        id: string,
        opts: { afterSeq: number; limit: number },
      ): Promise<EventPage> {
        const all = history(id);
        const events = all.slice(opts.afterSeq, opts.afterSeq + opts.limit);
        return {
          events,
          nextSeq: opts.afterSeq + events.length,
          hasMore: opts.afterSeq + events.length < all.length,
          truncated: gap,
        };
      },
    },
    protocol: {
      async getSession() {
        throw new Error("not configured");
      },
      async sendCommand() {
        throw new Error("not configured");
      },
      async updateSession() {
        throw new Error("not configured");
      },
      async deleteSession() {
        throw new Error("not configured");
      },
      async push(batch: Push) {
        calls.push(structuredClone(batch));
        if (blocked === batch.sessionId) throw new RemoteError(409, "conflict");
        if (receipts.has(batch.operationId)) return receipts.get(batch.operationId)!;
        const events = remote.get(batch.sessionId) ?? [];
        assert.equal(batch.expectedSeq, events.length);
        events.push(...batch.events.map((data, i) => ({ seq: batch.expectedSeq + i + 1, data })));
        remote.set(batch.sessionId, events);
        const change = {
          revision: 1,
          tags: [],
          metadata: {},
          archived: false,
          deleted: false,
          writerId: "laptop",
          writerEpoch: 0,
          cursor: changes.length + 1,
          sessionId: batch.sessionId,
          lastSeq: events.length,
          title: batch.title,
          visibility: batch.visibility,
          ownerId: "alice",
        };
        changes.push(change);
        const ack = { sessionId: batch.sessionId, lastSeq: events.length, cursor: change.cursor };
        receipts.set(batch.operationId, ack);
        if (lostAck) {
          lostAck = false;
          throw new TypeError("Lost committed ACK");
        }
        return ack;
      },
      async changes({ after }: { after: number }) {
        const change = changes.find((c) => c.cursor > after);
        return { changes: change ? [change] : [], cursor: change?.cursor ?? after };
      },
      async events({
        sessionId,
        after,
        limit,
      }: {
        sessionId: string;
        after: number;
        limit: number;
      }) {
        const all = remote.get(sessionId) ?? [];
        const events = all.slice(after, after + limit);
        return {
          events,
          lastSeq: all.length,
          nextAfter: after + events.length,
          hasMore: after + events.length < all.length,
        };
      },
    },
  };
  return {
    options,
    remote,
    calls,
    changes,
    settings,
    loseAck() {
      lostAck = true;
    },
    block(id: string) {
      blocked = id;
    },
    gap() {
      gap = true;
    },
  };
}
async function temporary(t: { after: (fn: () => Promise<void>) => void }) {
  const dir = await mkdtemp(join(tmpdir(), "hk-sync-"));
  t.after(() => rm(dir, { recursive: true, force: true }));
  return dir;
}

test("lost ACK and restart retry the immutable operation and reconstruct the complete cache", async (t) => {
  const dir = await temporary(t);
  const f = fixture(fileSyncStore(dir));
  let sync = new SessionSync(f.options);
  await sync.publish("s1", { title: "Thread" });
  f.loseAck();
  await assert.rejects(sync.syncOnce(), /Lost committed/);
  assert.equal(sync.status.phase, "offline");
  const pending = (await f.options.store.load(scopeKey(scope))).bindings[0]!.pending;
  assert.ok(pending);
  const reopened = fileSyncStore(dir);
  sync = new SessionSync({ ...f.options, store: reopened });
  for (let i = 0; i < 6; i++) await sync.syncOnce();
  assert.deepEqual(f.calls[0], f.calls[1]);
  assert.equal(f.changes.length, 4);
  assert.equal((await reopened.load(scopeKey(scope))).bindings[0]!.uploadedSeq, 7);
  assert.deepEqual(
    (await sync.eventsPage("s1")).map((e) => e.data),
    history("s1"),
  );
  assert.equal((await sync.sessions())[0]!.cachedSeq, 7);
});

test("failed upload checkpoint save retries the persisted batch instead of skipping it", async (t) => {
  const base = fileSyncStore(await temporary(t));
  let fail = true;
  const store = {
    ...base,
    async save(key: string, state: Parameters<SyncStore["save"]>[1]) {
      if (state.bindings[0]?.uploadedSeq === 2 && fail) {
        fail = false;
        throw new Error("disk full");
      }
      await base.save(key, state);
    },
  };
  const f = fixture(store);
  const sync = new SessionSync(f.options);
  await sync.publish("s1", { title: "Test" });
  await assert.rejects(sync.syncOnce(), /disk full/);
  assert.equal((await base.load(scopeKey(scope))).bindings[0]!.uploadedSeq, 0);
  await sync.syncOnce();
  assert.deepEqual(f.calls[0], f.calls[1]);
  assert.equal(f.changes.length, 1);
});

test("cache persistence failure never advances the feed cursor and restart safely replays", async (t) => {
  const dir = await temporary(t);
  const base = fileSyncStore(dir);
  let fail = true;
  const store: SyncStore = {
    ...base,
    async append(...args) {
      await base.append(...args);
      if (fail) {
        fail = false;
        throw new Error("lost cache acknowledgement");
      }
    },
  };
  const f = fixture(store);
  const sync = new SessionSync(f.options);
  await sync.publish("s1", { title: "Test" });
  await assert.rejects(sync.syncOnce(), /lost cache/);
  assert.equal((await base.load(scopeKey(scope))).cursor, 0);
  const reopened = new SessionSync({ ...f.options, store: fileSyncStore(dir) });
  for (let i = 0; i < 5; i++) await reopened.syncOnce();
  assert.deepEqual(
    (await reopened.eventsPage("s1")).map((e) => e.data),
    history("s1"),
  );
});

test("explicit destination is immutable across tenants and fresh store instances", async (t) => {
  const dir = await temporary(t);
  const f = fixture(fileSyncStore(dir));
  const sync = new SessionSync(f.options);
  await sync.syncOnce();
  assert.equal(f.calls.length, 0);
  await sync.publish("s1", { title: "Private" });
  const other = new SessionSync({
    ...f.options,
    store: fileSyncStore(dir),
    scope: { ...scope, tenantId: "personal" },
  });
  await assert.rejects(other.publish("s1", { title: "Private" }), /destination_is_immutable/);
  await assert.rejects(sync.publish("s1", { title: "Renamed" }), /binding_is_immutable/);
  assert.deepEqual(await other.sessions(), []);
  assert.deepEqual(await other.eventsPage("s1"), []);
});

test("durable gaps block publication; a conflicted session does not starve another", async (t) => {
  const f = fixture(fileSyncStore(await temporary(t)));
  const sync = new SessionSync(f.options);
  await sync.publish("s1", { title: "One" });
  await sync.publish("s2", { title: "Two" });
  f.block("s1");
  await assert.rejects(sync.syncOnce(), /conflict/);
  assert.equal(sync.status.code, "conflict");
  assert.equal(f.remote.get("s2")!.length, 2);
  f.gap();
  await assert.rejects(sync.publish("s3", { title: "Gap" }), /history_gap/);
});

test("background worker observes runtime pause and stops without leaving a timer", async (t) => {
  const f = fixture(fileSyncStore(await temporary(t)));
  const sync = new SessionSync(f.options);
  t.after(() => sync.stop());
  await sync.publish("s1", { title: "Thread" });
  f.settings.paused = true;
  sync.onStatus(() => {
    throw new Error("broken UI observer");
  });
  sync.start();
  sync.start();
  await new Promise((resolve) => setTimeout(resolve, 40));
  assert.equal(sync.status.phase, "paused");
  assert.equal(f.calls.length, 0);
  f.settings.paused = false;
  for (let i = 0; i < 50 && f.remote.get("s1")?.length !== 7; i++)
    await new Promise((resolve) => setTimeout(resolve, 25));
  await sync.stop();
  assert.equal(f.remote.get("s1")?.length, 7);
  assert.equal(sync.status.phase, "stopped");
});

test("disk cache crosses chunk boundaries, pages bounded reads, and preserves its head on capacity failure", async (t) => {
  const dir = await temporary(t);
  const store = fileSyncStore(dir);
  const change = {
    revision: 1,
    tags: [],
    metadata: {},
    archived: false,
    deleted: false,
    writerId: "laptop",
    writerEpoch: 0,
    cursor: 1,
    sessionId: "s1",
    lastSeq: 73,
    title: "Long",
    visibility: "private",
    ownerId: "alice",
  };
  const all = history("s1", 73).map((data) => ({ seq: data.seq, data }));
  await store.append("scope", change, all.slice(0, 23));
  await store.append("scope", change, all.slice(23));
  const reopened = fileSyncStore(dir);
  assert.deepEqual(await reopened.events("scope", "s1", 24, 40), all.slice(24, 64));
  const limited = fileSyncStore(dir, { maxSessionBytes: 1 });
  await assert.rejects(
    limited.append("scope", { ...change, lastSeq: 74 }, [{ seq: 74, data: {} }]),
    SyncError,
  );
  assert.equal(await reopened.head("scope", "s1"), 73);
  await assert.rejects(
    reopened.append("scope", change, [{ seq: 75, data: {} }]),
    /remote_history_gap/,
  );
});

test("metadata and commands retain operation IDs after lost ACK; tombstones erase cached payloads", async (t) => {
  const dir = await temporary(t),
    store = fileSyncStore(dir),
    f = fixture(store);
  const sync = new SessionSync(f.options);
  await sync.publish("s1", { title: "Original" });
  for (let i = 0; i < 5; i++) await sync.syncOnce();
  let updateID = "",
    commandID = "",
    updateCalls = 0,
    commandCalls = 0;
  f.options.protocol.updateSession = async (input: any) => {
    updateCalls++;
    if (updateID) assert.equal(input.operationId, updateID);
    else updateID = input.operationId;
    const change = {
      ...f.changes.at(-1)!,
      cursor: f.changes.length + 1,
      revision: 2,
      title: input.title,
    };
    if (updateCalls === 1) {
      f.changes.push(change);
      throw new TypeError("lost metadata ACK");
    }
    return { ...change, cursor: change.cursor - 1 };
  };
  f.options.protocol.sendCommand = async (input: any) => {
    commandCalls++;
    if (commandID) assert.equal(input.id, commandID);
    else commandID = input.id;
    if (commandCalls === 1) throw new TypeError("lost command ACK");
    return { ...input, status: "pending", result: {} };
  };
  await sync.update({
    sessionId: "s1",
    expectedRevision: 1,
    title: "Renamed",
    tags: [],
    metadata: {},
    archived: false,
  });
  await sync.command({ sessionId: "s1", epoch: 1, kind: "prompt", payload: { text: "hello" } });
  await assert.rejects(sync.syncOnce(), /lost/);
  const reopened = new SessionSync({ ...f.options, store: fileSyncStore(dir) });
  await reopened.syncOnce();
  assert.equal(updateCalls, 2);
  assert.equal(commandCalls, 2);
  assert.equal((await reopened.pending()).mutations?.length, 0);
  assert.equal((await reopened.pending()).commands?.length, 0);
  const tombstone = {
    ...f.changes.at(-1)!,
    deleted: true,
    cursor: f.changes.length + 1,
    revision: 3,
  };
  f.changes.push(tombstone);
  await reopened.syncOnce();
  assert.deepEqual(await reopened.eventsPage("s1"), []);
  assert.equal((await store.load(scopeKey(scope))).bindings[0]!.deleted, true);
  const limited = fileSyncStore(dir, { maxSessions: 1 });
  await assert.rejects(
    limited.append(scopeKey(scope), { ...tombstone, sessionId: "extra" }, []),
    /store_capacity/,
  );
});

test("lease recovery resolves committed receipts before rebasing definitively rejected uploads", async (t) => {
  for (const committed of [true, false]) {
    const f = fixture(fileSyncStore(await temporary(t))),
      sync = new SessionSync(f.options);
    await sync.publish("s1", { title: "Recovery" });
    if (committed) f.loseAck();
    else f.block("s1");
    await assert.rejects(sync.syncOnce());
    const before = (await f.options.store.load(scopeKey(scope))).bindings[0]!.pending!;
    f.options.protocol.getSession = async () =>
      ({
        ...f.changes[0],
        sessionId: "s1",
        writerId: "laptop",
        writerEpoch: 2,
        lastSeq: 0,
        deleted: false,
      }) as Change;
    await sync.useLease({
      sessionId: "s1",
      deviceId: "laptop",
      epoch: 2,
      expiresAt: new Date(Date.now() + 30000).toISOString(),
    });
    const after = (await f.options.store.load(scopeKey(scope))).bindings[0]!;
    assert.equal(after.epoch, 2);
    if (committed) {
      assert.equal(after.uploadedSeq, 2);
      assert.equal(after.pending, undefined);
    } else {
      assert.notEqual(after.pending!.operationId, before.operationId);
      assert.equal(after.pending!.epoch, 2);
      assert.deepEqual(after.pending!.events, before.events);
    }
  }
});
