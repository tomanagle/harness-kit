import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtemp, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileSyncOutbox } from "../src/outbox.node.ts";
import { remoteClient, RemoteError, ServerSession, Servers, scopeKey } from "../src/remote.ts";
import type { Grant, Push, ServerScope } from "../src/remote.ts";
import type { SettingsSnapshot } from "../src/protocol.ts";
const batch: Push = {
  epoch: 0,
  operationId: "op-1",
  sessionId: "session-1",
  expectedSeq: 0,
  title: "Test",
  visibility: "private",
  events: [{ type: "message", text: "hello" }],
};
const scope: ServerScope = {
  serverId: "work",
  url: "https://work.example",
  accountId: "alice",
  tenantId: "company",
  applicationId: "sprint",
};
const grant: Grant = {
  accountId: "alice",
  tenantId: "company",
  applicationId: "sprint",
  deviceId: "laptop",
  accessToken: "access",
  refreshToken: "refresh",
  expiresAt: "2000-01-01T00:00:00Z",
};
const ok = (body: unknown) =>
  new Response(JSON.stringify(body), { headers: { "Content-Type": "application/json" } });

test("durable outbox survives restart, isolates scopes, and rejects changed retries", async () => {
  const dir = await mkdtemp(join(tmpdir(), "hk-outbox-"));
  try {
    const outbox = fileSyncOutbox(dir, { maxOperations: 2 });
    const mutable = structuredClone(batch);
    await outbox.put("work", mutable);
    mutable.title = "mutated";
    await outbox.put("work", batch);
    await assert.rejects(outbox.put("work", { ...batch, title: "different" }), /different payload/);
    const reopened = fileSyncOutbox(dir, { maxOperations: 2 });
    assert.deepEqual(await reopened.list("work", 100), [batch]);
    assert.deepEqual(await reopened.list("personal", 100), []);
    await reopened.put("work", { ...batch, operationId: "op-2" });
    await assert.rejects(reopened.put("work", { ...batch, operationId: "op-3" }), /capacity/);
    await reopened.acknowledge("work", "op-1");
    assert.equal((await reopened.list("work", 100))[0]?.operationId, "op-2");
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("concurrent requests rotate credentials once and never forward on redirects", async () => {
  let saved = { ...grant };
  let rotations = 0;
  const auth: string[] = [];
  const client = new ServerSession(
    scope,
    {
      applicationId: "sprint",
      credentials: {
        async get() {
          return saved;
        },
        async set(_key, value) {
          saved = value;
        },
        async delete() {},
      },
      fetch: (async (input, init) => {
        assert.equal(init?.redirect, "error");
        assert.equal(init?.credentials, "omit");
        if (String(input).endsWith("refresh")) {
          rotations++;
          await new Promise((resolve) => setTimeout(resolve, 20));
          return ok({
            ...grant,
            accessToken: "next",
            refreshToken: "next-refresh",
            expiresAt: "2099-01-01T00:00:00Z",
          });
        }
        auth.push((init?.headers as Record<string, string> | undefined)?.Authorization ?? "");
        return ok({ changes: [], cursor: 0 });
      }) as typeof fetch,
    },
    async () => {},
  );
  await Promise.all(
    Array.from({ length: 20 }, () => client.protocol.changes({ after: 0, limit: 100, waitMs: 0 })),
  );
  assert.equal(rotations, 1);
  assert.deepEqual(new Set(auth), new Set(["Bearer next"]));
  assert.notEqual(scopeKey(scope), scopeKey({ ...scope, tenantId: "personal" }));
  assert.notEqual(scopeKey(scope), scopeKey({ ...scope, url: "https://another.example" }));
});

test("lost acknowledgement leaves durable operation available for retry", async () => {
  const dir = await mkdtemp(join(tmpdir(), "hk-retry-"));
  try {
    let fail = true;
    const bodies: unknown[] = [];
    const session = new ServerSession(
      scope,
      {
        applicationId: "sprint",
        outbox: fileSyncOutbox(dir),
        credentials: {
          async get() {
            return { ...grant, expiresAt: "2099-01-01T00:00:00Z" };
          },
          async set() {},
          async delete() {},
        },
        fetch: (async (_input, init) => {
          bodies.push(JSON.parse(init?.body as string));
          if (fail) {
            fail = false;
            throw new TypeError("connection lost after server commit");
          }
          return ok({ sessionId: batch.sessionId, lastSeq: 1, cursor: 1 });
        }) as typeof fetch,
      },
      async () => {},
    );
    await session.enqueue(batch);
    await assert.rejects(session.flush(), /connection lost/);
    assert.equal((await fileSyncOutbox(dir).list(scopeKey(scope), 100)).length, 1);
    await session.flush();
    assert.deepEqual(bodies, [batch, batch]);
    assert.deepEqual(await fileSyncOutbox(dir).list(scopeKey(scope), 100), []);
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test("runtime disable stops existing connections and config uses settings revisions", async () => {
  let snapshot = {
    revision: 2,
    values: {
      servers: { connections: [{ id: "work", name: "Work", url: scope.url, enabled: true }] },
    },
  } as SettingsSnapshot;
  const settings = {
    async get() {
      return structuredClone(snapshot);
    },
    async configure(params: any) {
      assert.equal(params.expectedRevision, snapshot.revision);
      snapshot = {
        ...snapshot,
        revision: snapshot.revision + 1,
        values: { ...snapshot.values, servers: params.patch.servers },
      };
      return structuredClone(snapshot);
    },
  };
  let requests = 0;
  const servers = new Servers(settings, {
    applicationId: "sprint",
    credentials: {
      async get() {
        return { ...grant, expiresAt: "2099-01-01T00:00:00Z" };
      },
      async set() {},
      async delete() {},
    },
    fetch: (async () => {
      requests++;
      return ok({ changes: [], cursor: 0 });
    }) as typeof fetch,
  });
  const connection = await servers.connect("work", { accountId: "alice", tenantId: "company" });
  await connection.protocol.changes({ after: 0, limit: 100, waitMs: 0 });
  await servers.add({
    id: "personal",
    name: "Personal",
    url: "https://personal.example",
    enabled: true,
  });
  await servers.remove("work");
  await assert.rejects(
    connection.protocol.changes({ after: 0, limit: 100, waitMs: 0 }),
    /missing or disabled/,
  );
  assert.equal(requests, 1);
});

test("remote errors and insecure endpoints are rejected", async () => {
  assert.throws(() => remoteClient({ url: "http://work.example" }), /HTTPS/);
  assert.throws(() => remoteClient({ url: "https://user:password@work.example" }), /HTTPS/);
  const client = remoteClient({
    url: scope.url,
    fetch: (async () =>
      new Response(JSON.stringify({ code: "forbidden" }), { status: 403 })) as typeof fetch,
  });
  await assert.rejects(
    client.changes({ after: 0, limit: 100, waitMs: 0 }),
    (error: unknown) => error instanceof RemoteError && error.code === "forbidden",
  );
});

test("lost refresh response never replays a possibly rotated refresh credential", async () => {
  let requests = 0;
  const session = new ServerSession(
    scope,
    {
      applicationId: "sprint",
      credentials: {
        async get() {
          return grant;
        },
        async set() {},
        async delete() {
          throw new Error("keychain unavailable");
        },
      },
      fetch: (async () => {
        requests++;
        throw new TypeError("response lost after rotation");
      }) as typeof fetch,
    },
    async () => {},
  );
  await assert.rejects(
    session.protocol.changes({ after: 0, limit: 100, waitMs: 0 }),
    /response lost/,
  );
  await assert.rejects(
    session.protocol.changes({ after: 0, limit: 100, waitMs: 0 }),
    (error: unknown) => error instanceof RemoteError && error.code === "reauthorization_required",
  );
  assert.equal(requests, 1);
});
