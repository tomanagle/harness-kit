// Integration tests: build the real Go sidecar and drive it over stdio with
// the deterministic fake runtime. This is exactly the path an Electron main
// process (or any Node.js host) takes.

import { test, before, after } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { existsSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";

import { createHarnessKit, HarnessKitError, LineDecoder, type HarnessEvent, type HarnessKit } from "../src/index.ts";
import { stdio } from "../src/node.ts";

const coreRoot = resolve(import.meta.dirname, "..", "..", "..", "core");
const binDir = mkdtempSync(join(tmpdir(), "hk-ts-"));
const bin = join(binDir, process.platform === "win32" ? "harness-kit.exe" : "harness-kit");

before(() => {
  execFileSync("go", ["build", "-o", bin, "./cmd/harness-kit"], { cwd: coreRoot, stdio: "inherit" });
});
after(() => rmSync(binDir, { recursive: true, force: true }));

async function connect(extraArgs: string[] = []): Promise<HarnessKit> {
  return createHarnessKit({
    transport: stdio({ command: bin, args: ["serve", "--runtimes", "", "--fake-runtime", "--log-level", "error", ...extraArgs] }),
    clientInfo: { name: "client-ts-tests", version: "0.0.0" },
  });
}

test("LineDecoder splits chunked input", () => {
  const d = new LineDecoder();
  assert.deepEqual(d.push('{"a":1}\n{"b"'), ['{"a":1}']);
  assert.deepEqual(d.push(':2}\r\n\n{"c":3}\n'), ['{"b":2}', '{"c":3}']);
});

test("workspace settings, saved diff and explicit cleanup use the typed client", async () => {
  const repo = mkdtempSync(join(binDir, "workspace-repo-"));
  execFileSync("git", ["init", "-b", "main", repo], {stdio: "ignore"});
  execFileSync("git", ["-C", repo, "-c", "user.name=Harness Test", "-c", "user.email=test@example.invalid",
    "commit", "--allow-empty", "-m", "initial"], {stdio: "ignore"});
  const root = mkdtempSync(join(binDir, "workspace-root-"));
  const kit = await connect();
  try {
    assert.equal(kit.server.capabilities.workspace, true);
    const conversation = await kit.sessions.create({
      settings: {
        session: {runtime: "fake", cwd: repo},
        workspace: {mode: "worktree", root, captureDiff: true},
      },
    });
    const binding = conversation.info.workspace!;
    assert.equal(binding.owned, true);
    assert.notEqual(binding.path, repo);
    assert.equal(existsSync(binding.path), true);
    const turn = conversation.prompt("no filesystem changes");
    await turn.done;
    const diff = await kit.sessions.diff(conversation.id, turn.id, {maxBytes: 64});
    assert.equal(diff.complete, true);
    assert.equal(diff.workingTree, "");
    assert.equal(diff.index, "");
    assert.equal(diff.turnId, turn.id);
    const config = await kit.settings.get({sessionId: conversation.id});
    assert.equal(config.values.workspace.captureDiff, true);
    assert.equal(config.fields["workspace.captureDiff"]?.when, "nextTurn");
    await conversation.close();
    await kit.sessions.removeWorkspace(conversation.id);
    assert.equal(existsSync(binding.path), false);
    assert.equal((await kit.sessions.diff(conversation.id, turn.id)).complete, true);
  } finally {
    await kit.close();
  }
});

test("handshake, discovery and a streamed turn", async () => {
  const kit = await connect();
  try {
    assert.equal(kit.server.protocolVersion, 1);
    assert.deepEqual(kit.server.capabilities.runtimes.map((r) => r.name), ["fake"]);

    const discovered = await kit.runtimes.discover();
    assert.equal(discovered[0]?.ready, true);
    assert.equal(discovered[0]?.auth.state, "authenticated");

    const session = await kit.sessions.create({ runtime: "fake", cwd: binDir, metadata: { app: "tests" } });
    assert.equal(session.info.state, "idle");
    assert.equal(session.info.metadata?.app, "tests");

    const turn = session.prompt("hello from typescript");
    const seen: HarnessEvent[] = [];
    let text = "";
    for await (const ev of turn) {
      seen.push(ev);
      if (ev.type === "message.delta") text += ev.data.text;
    }
    assert.equal(text, "Echo: hello from typescript");
    assert.equal(seen[0]?.type, "turn.started");
    assert.equal(seen.at(-1)?.type, "turn.completed");
    for (let i = 1; i < seen.length; i++) assert.ok(seen[i]!.seq > seen[i - 1]!.seq, "events are ordered");
    const done = await turn.done;
    assert.equal(done.status, "completed");
    assert.ok(done.usage?.outputTokens);

    const info = await session.refresh();
    assert.equal(info.lastSeq, seen.at(-1)!.seq);
    const replay = await session.events();
    assert.equal(replay.events[0]?.type, "session.started");
    await session.close();
  } finally {
    await kit.close();
  }
});

test("approvals pause the turn until the host responds", async () => {
  const kit = await connect();
  try {
    const session = await kit.sessions.create({ runtime: "fake" });
    const turn = session.prompt("[approve] run something");
    const tools: string[] = [];
    for await (const ev of turn) {
      if (ev.type === "approval.requested") {
        assert.equal(ev.data.kind, "command");
        assert.equal(ev.data.command, "echo hello");
        const pending = await kit.approvals.list(session.id);
        assert.equal(pending.length, 1);
        await kit.approvals.respond(ev.data.approvalId, "allow");
      }
      if (ev.type === "tool.completed") tools.push(ev.data.status);
    }
    assert.deepEqual(tools, ["completed"]);

    // Deny with a message.
    const denied = session.prompt("[approve]");
    for await (const ev of denied) {
      if (ev.type === "approval.requested") await kit.approvals.respond(ev.data.approvalId, { outcome: "deny", message: "no thanks" });
      if (ev.type === "tool.completed") assert.equal(ev.data.error, "no thanks");
    }
  } finally {
    await kit.close();
  }
});

test("AbortSignal cancels a running turn", async () => {
  const kit = await connect();
  try {
    const session = await kit.sessions.create({ runtime: "fake" });
    const ac = new AbortController();
    const turn = session.prompt("[slow] go", { signal: ac.signal });
    for await (const ev of turn) {
      if (ev.type === "message.delta") ac.abort();
    }
    assert.equal((await turn.done).status, "cancelled");
    // The session is reusable afterwards.
    assert.equal((await session.prompt("again").done).status, "completed");
  } finally {
    await kit.close();
  }
});

test("errors carry stable reasons", async () => {
  const kit = await connect();
  try {
    await assert.rejects(kit.sessions.create({ runtime: "nope" }), (e: unknown) => e instanceof HarnessKitError && e.reason === "runtime_not_found");
    await assert.rejects(kit.sessions.create({ runtime: "fake", options: { unsafe: true } }), (e: unknown) => e instanceof HarnessKitError && e.reason === "policy_violation");
    await assert.rejects(kit.sessions.get("ses_missing"), (e: unknown) => e instanceof HarnessKitError && e.reason === "session_not_found");

    const session = await kit.sessions.create({ runtime: "fake" });
    const slow = session.prompt("[slow]");
    await assert.rejects(session.prompt("concurrent").done, (e: unknown) => e instanceof HarnessKitError && e.reason === "session_busy");
    await slow.cancel();
    await slow.done;

    const failed = await session.prompt("[fail]").done;
    assert.equal(failed.status, "failed");
    assert.match(failed.error?.message ?? "", /failed as requested/);
  } finally {
    await kit.close();
  }
});

test("--allow-unsafe is reported and honoured", async () => {
  const kit = await connect(["--allow-unsafe"]);
  try {
    assert.equal(kit.server.capabilities.allowUnsafe, true);
    await kit.sessions.create({ runtime: "fake", options: { unsafe: true } });
  } finally {
    await kit.close();
  }
});

test("events dropped for a slow client are recovered transparently", async () => {
  // A one-event server queue and an instant producer force the server to
  // drop events for this client and announce them with stream.gap.
  const kit = await connect(["--event-buffer", "1", "--fake-delay", "0s"]);
  try {
    let gaps = 0;
    kit.onEvent((ev) => {
      if (ev.type === "stream.gap") gaps++;
    });
    const session = await kit.sessions.create({ runtime: "fake" });
    const words = Array.from({ length: 2000 }, (_, i) => `w${i}`).join(" ");
    let text = "";
    let last = 0;
    for await (const ev of session.prompt(words)) {
      assert.ok(ev.seq > last, "strictly increasing");
      last = ev.seq;
      if (ev.type === "message.delta") text += ev.data.text;
    }
    assert.equal(text, "Echo: " + words, "no text lost despite drops");
    // Contiguity check against the server's log for this turn.
    const { events } = await session.events();
    const turnEvents = events.filter((e) => e.type !== "session.started");
    assert.equal(turnEvents.at(-1)?.seq, last);
    console.log(`  (server reported ${gaps} gap(s); all recovered)`);
  } finally {
    await kit.close();
  }
});

test("sessions persist across sidecar restarts and can be restored", async () => {
  const store = mkdtempSync(join(tmpdir(), "hk-store-"));
  try {
    const first = await connect(["--store", store]);
    const s = await first.sessions.create({ runtime: "fake", cwd: binDir });
    await s.prompt("remember pineapple").done;
    const native = await first.runtimes.sessions("fake", { cwd: binDir });
    assert.equal(native[0]?.id, s.info.runtimeSessionId);
    await first.close();

    const second = await connect(["--store", store]);
    try {
      const listed = await second.sessions.list();
      assert.equal(listed.length, 1);
      assert.equal(listed[0]?.title, "remember pineapple");
      assert.equal(listed[0]?.turns, 1);
      assert.equal(listed[0]?.lastStatus, "completed");

      // Reading the durable transcript does not start the runtime.
      const saved = await second.sessions.events(s.id);
      assert.equal(saved.truncated, false);
      assert.ok(saved.events.some((ev) => ev.type === "turn.started" && ev.data.text === "remember pineapple"));
      assert.ok(saved.events.some((ev) => ev.type === "message.completed" && ev.data.text === "Echo: remember pineapple"));
      assert.equal((await second.sessions.get(s.id)).state, "closed");
      const page = await second.sessions.events(s.id, { afterSeq: saved.events[0]!.seq, limit: 2 });
      assert.deepEqual(page.events, saved.events.slice(1, 3));
      const beforeRestoreSeq = saved.events.at(-1)!.seq;
      const updates: HarnessEvent[] = [];
      const unsubscribe = second.onEvent((ev) => { if (ev.type === "session.updated" && ev.sessionId === s.id) updates.push(ev); });

      // A stored session can be renamed before it is restored.
      assert.equal((await second.sessions.rename(s.id, "  Fruit\n facts ")).title, "Fruit facts");
      await assert.rejects(second.sessions.rename(s.id, "  "), (e: unknown) => e instanceof HarnessKitError && e.reason === "invalid_params");

      const restored = await second.sessions.restore(s.id);
      assert.equal(restored.id, s.id);
      assert.equal(restored.info.title, "Fruit facts");
      assert.equal(restored.info.runtimeSessionId, s.info.runtimeSessionId);
      assert.equal((await restored.prompt("again").done).status, "completed");
      assert.equal((await restored.refresh()).turns, 2);
      assert.equal(restored.info.title, "Fruit facts", "a prompt never replaces a chosen title");
      assert.equal((await restored.rename("Pineapple")).title, "Pineapple");
      assert.equal((await second.sessions.get(s.id)).title, "Pineapple");
      const replay = await restored.events();
      assert.deepEqual(replay.events.slice(0, saved.events.length), saved.events, "restore preserves earlier events verbatim");
      assert.ok(replay.events.at(-1)!.seq > beforeRestoreSeq);
      assert.equal(replay.events.filter((ev) => ev.type === "turn.started").length, 2);
      assert.ok(replay.events.some((ev) => ev.type === "session.updated" && ev.data.title === "Fruit facts"));
      // Notifications and RPC responses travel independently: wait for the
      // rename notification rather than assuming it precedes its response.
      for (let i = 0; i < 100 && !updates.some((ev) => ev.type === "session.updated" && ev.data.title === "Pineapple"); i++) {
        await new Promise((resolve) => setTimeout(resolve, 5));
      }
      assert.ok(updates.some((ev) => ev.type === "session.updated" && ev.data.title === "Pineapple"));
      unsubscribe();

      await second.sessions.delete(s.id);
      await assert.rejects(second.sessions.events(s.id), (e: unknown) => e instanceof HarnessKitError && e.reason === "session_not_found");
      await assert.rejects(second.sessions.get(s.id), (e: unknown) => e instanceof HarnessKitError && e.reason === "session_not_found");
    } finally {
      await second.close();
    }
  } finally {
    rmSync(store, { recursive: true, force: true });
  }
});

test("per-session MCP servers reach the runtime", async () => {
  const kit = await connect();
  try {
    assert.equal(kit.server.capabilities.runtimes[0]?.capabilities.mcp, true);
    const seen: string[] = [];
    kit.onEvent((ev) => {
      if (ev.type === "runtime.status" && ev.data.status === "mcp") seen.push(ev.data.message ?? "");
    });
    const s = await kit.sessions.create({
      runtime: "fake",
      mcp: { inheritNative: false, mcpServers: { github: { command: "npx", args: ["-y", "srv"], env: { GITHUB_TOKEN: "${GITHUB_TOKEN}" } } } },
    });
    const { events } = await s.events();
    assert.ok(events.some((e) => e.type === "runtime.status" && e.data.message === "MCP servers: github"));
    await assert.rejects(
      kit.sessions.create({ runtime: "fake", mcp: { mcpServers: { "bad__name": { command: "x" } } } }),
      (e: unknown) => e instanceof HarnessKitError && e.reason === "invalid_params",
    );
  } finally {
    await kit.close();
  }
});

test("host tools: registered in TypeScript, called by the runtime", { timeout: 30_000 }, async (t) => {
  const kit = await connect();
  t.after(() => kit.close());
  assert.equal(kit.server.capabilities.tools, true);
  const calls: { args: unknown; sessionId: string; turnId?: string }[] = [];
  let abortedBy: unknown;
  let started!: () => void;
  let aborted!: () => void;
  const toolStarted = new Promise<void>((resolve) => { started = resolve; });
  const toolAborted = new Promise<void>((resolve) => { aborted = resolve; });
  await kit.tools.register([
    {
      name: "lookup",
      description: "Look up an order",
      inputSchema: { type: "object", properties: { id: { type: "string" } }, required: ["id"] },
      handler: (args: { id: string }, ctx) => {
        calls.push({ args, sessionId: ctx.sessionId, turnId: ctx.turnId });
        return { id: args.id, status: "shipped" };
      },
    },
    { name: "refund", approval: "ask", handler: () => "refunded" },
    { name: "explode", handler: () => { throw new Error("database offline"); } },
    {
      name: "wait",
      handler: (_args, ctx) => new Promise((_resolve, reject) => {
        ctx.signal.addEventListener("abort", () => {
          abortedBy = ctx.signal.reason;
          aborted();
          reject(new Error("aborted"));
        }, { once: true });
        started();
      }),
    },
  ]);
  await assert.rejects(kit.tools.register({ name: "lookup", handler: () => "x" }), /already registered/);
  assert.deepEqual((await kit.tools.list()).map((t) => t.name), ["lookup", "refund", "explode", "wait"]);

  const session = await kit.sessions.create({ runtime: "fake" });
  const outputs: string[] = [];
  const turn = session.prompt('[call lookup {"id":"A7"}] [call refund] [call explode]');
  for await (const ev of turn) {
    if (ev.type === "approval.requested") {
      assert.equal(ev.data.tool, "refund");
      await kit.approvals.respond(ev.data.approvalId, "allow");
    }
    if (ev.type === "tool.completed") outputs.push(`${ev.data.status}:${ev.data.output ?? ""}${ev.data.error ?? ""}`);
  }
  assert.deepEqual(outputs, ['completed:{"id":"A7","status":"shipped"}', "completed:refunded", "failed:database offline"]);
  assert.deepEqual(calls[0], { args: { id: "A7" }, sessionId: session.id, turnId: turn.id });

  // Cancelling the turn aborts the in-flight tool call in the app.
  const waiting = session.prompt("[call wait]");
  // Under load, cancelling after a fixed delay can precede the tool call.
  await toolStarted;
  await waiting.cancel();
  await toolAborted;
  assert.ok(abortedBy);
  assert.equal((await waiting.done).status, "cancelled");

  // Sessions can be limited to some tools, or have none.
  const limited = await kit.sessions.create({ runtime: "fake", hostTools: ["lookup"] });
  const r = await limited.prompt("[call refund]").done;
  assert.equal(r.status, "completed");
  const none = await kit.sessions.create({ runtime: "fake", disableHostTools: true });
  let failed = "";
  for await (const ev of none.prompt('[call lookup {"id":"1"}]')) if (ev.type === "tool.completed") failed = ev.data.error ?? "";
  assert.match(failed, /no host tools/);

  await kit.tools.unregister("explode");
  assert.equal((await kit.tools.list()).length, 3);
});

test("pending requests reject when the sidecar dies", async () => {
  const transport = stdio({ command: bin, args: ["serve", "--runtimes", "", "--fake-runtime", "--log-level", "error"] });
  const kit = await createHarnessKit({ transport });
  const session = await kit.sessions.create({ runtime: "fake" });
  const turn = session.prompt("[slow]");
  let closed = false;
  kit.onClose(() => (closed = true));
  transport.process.kill("SIGKILL");
  await assert.rejects(turn.done);
  assert.equal(closed, true);
  await assert.rejects(kit.runtimes.list());
});

test("configuration defaults, turn overrides and reset cross the full client/server path", async () => {
  const kit = await connect();
  try {
    const capability = kit.server.capabilities.runtimes[0]?.capabilities.configuration?.model;
    assert.equal(capability?.session, true);
    assert.equal(capability?.turn, true);
    assert.equal(capability?.live, false);
    const session = await kit.sessions.create({ runtime: "fake", model: "base", effort: "low" });
    assert.equal(session.info.config?.model, "base");
    const configured = await session.configure({ model: "future", effort: "high" });
    assert.equal(configured.config?.effort, "high");
    assert.equal(session.info.config?.model, "future");
    const first = session.prompt("one-off", { config: { model: "temporary", effort: "medium" } });
    let sawFirst = false;
    for await (const ev of first) {
      if (ev.type === "turn.started") {
        sawFirst = true;
        assert.equal(ev.data.config?.model, "temporary");
        assert.equal(ev.data.config?.effort, "medium");
      }
    }
    assert.ok(sawFirst);
    for await (const ev of session.prompt("defaults remain")) {
      if (ev.type === "turn.started") {
        assert.equal(ev.data.config?.model, "future");
        assert.equal(ev.data.config?.effort, "high");
      }
    }
    // Both calls are intentionally issued before either response. They must
    // take effect in wire order for this session.
    const applying = kit.sessions.configure(session.id, { model: "ordered" });
    for await (const ev of session.prompt("after configure")) {
      if (ev.type === "turn.started") assert.equal(ev.data.config?.model, "ordered");
    }
    await applying;
    const reset = await kit.sessions.configure(session.id, { reset: ["model", "effort"] });
    assert.equal(reset.config?.model, "base");
    assert.equal(reset.config?.effort, "low");
    for (const config of [null, { model: null }, { effort: null }, { modle: "typo" }, { model: "conflict", reset: ["model"] }]) {
      await assert.rejects(kit.request("session.configure", { sessionId: session.id, config }),
        (e: unknown) => e instanceof HarnessKitError && e.code === -32602 && e.reason === "invalid_params");
    }
    await assert.rejects(kit.request("session.prompt", { sessionId: session.id, input: [{ type: "text", text: "invalid null config" }], config: null }),
      (e: unknown) => e instanceof HarnessKitError && e.reason === "invalid_params");
    await assert.rejects(kit.request("session.configure", { sessionId: session.id }),
      (e: unknown) => e instanceof HarnessKitError && e.reason === "invalid_params");
    await assert.rejects(kit.sessions.configure(session.id, { reset: ["options.failStart"] }),
      (e: unknown) => e instanceof HarnessKitError && e.reason === "unsupported");
    await assert.rejects(kit.sessions.configure(session.id, { options: { unsafe: true } }),
      (e: unknown) => e instanceof HarnessKitError && e.reason === "policy_violation");
    const denied = session.prompt("must not start", { config: { options: { unsafe: true } } });
    await assert.rejects(denied.done,
      (e: unknown) => e instanceof HarnessKitError && e.reason === "policy_violation");
    const explicitFalse = await session.configure({ options: { unsafe: false } });
    assert.equal(explicitFalse.config?.options?.unsafe, false);
    assert.equal((await session.refresh()).config?.model, "base");
  } finally {
    await kit.close();
  }
});

test("one settings API configures startup, live policy, session defaults and turn input", async () => {
  const transport = stdio({ command: bin, args: ["serve", "--runtimes", "fake", "--log-level", "error"] });
  const kit = await createHarnessKit({
    transport,
    initialSettings: {
      session: { runtime: "fake", model: "host-model", effort: "low" },
      history: { maxPageEvents: 2 },
    },
  });
  try {
    assert.equal(kit.server.capabilities.settings, true);
    const initial = await kit.settings.get();
    assert.equal(initial.values.session.model, "host-model");
    assert.equal(initial.fields["memory.evictAfterIdleMs"]?.unit, "milliseconds");
    const changes: number[] = [];
    const unsubscribe = kit.settings.onChanged(snapshot => changes.push(snapshot.revision));
    const updated = await kit.settings.configure({
      patch: { history: { maxPageEvents: 3 }, streams: { maxQueueBytes: 4096 } },
      expectedRevision: initial.revision,
    });
    assert.equal(updated.values.history.maxPageEvents, 3);
    assert.ok(changes.includes(updated.revision));
    await assert.rejects(kit.settings.configure({ patch: { history: { maxPageEvents: 4 } }, expectedRevision: initial.revision }),
      (e: unknown) => e instanceof HarnessKitError && e.reason === "settings_conflict");
    await assert.rejects(kit.settings.configure({ patch: { process: { store: "new-directory" }, history: { maxPageEvents: 5 } } }),
      (e: unknown) => e instanceof HarnessKitError && e.reason === "restart_required");
    assert.equal((await kit.settings.get()).values.history.maxPageEvents, 3);

    const session = await kit.sessions.create({ settings: { session: { model: "created-model" } } });
    assert.equal(session.runtime, "fake");
    assert.equal(session.info.config?.model, "created-model");
    const scoped = await kit.settings.get({ sessionId: session.id });
    await kit.settings.configure({ target: { sessionId: session.id }, patch: { session: { model: "future-model" } }, expectedRevision: scoped.revision });
    for await (const ev of session.prompt("unified turn", { settings: { session: { model: "turn-model" } } })) {
      if (ev.type === "turn.started") {
        assert.equal(ev.data.config?.model, "turn-model");
        assert.equal(ev.data.config?.effort, "low");
      }
    }
    assert.equal((await session.refresh()).config?.model, "future-model");
    await assert.rejects(session.prompt("conflict", { settings: { session: { model: "one" } }, config: { model: "two" } }).done,
      (e: unknown) => e instanceof HarnessKitError && e.reason === "invalid_params");
    const page = await session.eventsPage();
    assert.equal(page.events.length, 3);
    assert.equal(page.hasMore, true);
    const next = await session.eventsPage({ afterSeq: page.nextSeq });
    assert.ok(next.events[0]!.seq > page.nextSeq);
    // Compatibility helper fetches bounded pages, preserving its complete-result contract.
    const all = await session.events();
    assert.equal(all.events.at(-1)?.seq, (await session.refresh()).lastSeq);
    assert.ok(all.events.length > page.events.length);
    assert.equal((await kit.settings.stats()).residentSessions, 1);
    unsubscribe();
  } finally {
    await kit.close();
  }
});

test("settings validation rejects unknown and null envelopes atomically", async () => {
  const kit = await connect();
  try {
    const before = await kit.settings.get();
    for (const params of [
      { patch: null },
      { patch: { memory: null } },
      { patch: { memory: { maxCahceBytes: 123 } } },
      { patch: { memory: { resumeOnPrompt: null } } },
      { patch: {}, expectedRevison: before.revision },
      { patch: {}, target: { session: "typo" } },
    ]) {
      await assert.rejects(kit.request("settings.configure", params),
        (e: unknown) => e instanceof HarnessKitError && e.reason === "invalid_params");
    }
    assert.equal((await kit.settings.get()).revision, before.revision);
    const session = await kit.sessions.create({ runtime: "fake" });
    for (const method of ["session.create", "session.prompt"]) {
      await assert.rejects(kit.request(method, { runtime: "fake", sessionId: session.id, input: [{ type: "text", text: "bad" }], settings: null }),
        (e: unknown) => e instanceof HarnessKitError && e.reason === "invalid_params");
    }
  } finally {
    await kit.close();
  }
});

test("stdio boots from the same settings document including process wiring", async () => {
  const store = mkdtempSync(join(tmpdir(), "hk-settings-boot-"));
  const kit = await createHarnessKit({
    transport: stdio({
      command: bin,
      settings: {
        process: { store, runtimes: ["fake"], fakeDelayMs: 0 },
        logging: { level: "error" },
        session: { runtime: "fake", model: "boot-default" },
        history: { maxPageEvents: 2 },
      },
    }),
  });
  try {
    const boot = await kit.settings.get();
    assert.equal(boot.values.process.store, store);
    assert.deepEqual(boot.values.process.runtimes, ["fake"]);
    assert.equal(boot.values.session.model, "boot-default");
    const session = await kit.sessions.create({});
    assert.equal(session.info.config?.model, "boot-default");
    await kit.settings.configure({ patch: { memory: { evictAfterIdleMs: 600_000, suspendAfterIdleMs: 600_000, resumeOnPrompt: true } } });
    await session.prompt("saved conversation").done;
    await kit.sessions.suspend(session.id);
    assert.equal((await session.refresh()).state, "suspended");
    assert.equal((await session.prompt("resume automatically").done).status, "completed");
  } finally {
    await kit.close();
    rmSync(store, { recursive: true, force: true });
  }
});

test("catalog filters, tags, archive and independent forks", async () => {
  const kit = await connect();
  try {
    const parent = await kit.sessions.create({ runtime: "fake", title: "Source" });
    await parent.prompt("inherited conversation").done;
    await kit.sessions.setTags(parent.id, [" Work ", "work"]);
    await kit.sessions.archive(parent.id);
    const archived = await kit.sessions.list({ archived: true, tags: ["work"], query: "source" });
    assert.equal(archived.length, 1);
    assert.equal(archived[0]?.metadata?.tags, '["work"]');
    const child = await kit.sessions.fork(parent.id, { title: "Branch", settings: { session: { model: "branch-model" } } });
    assert.equal(child.info.forkedFrom, parent.id);
    assert.equal(child.info.inheritedTurns, 1);
    assert.equal(child.info.model, "branch-model");
    assert.notEqual(child.info.runtimeSessionId, parent.info.runtimeSessionId);
    assert.ok(!child.info.archived);
    assert.ok((await child.events()).events.some((ev) => ev.type === "turn.started" && ev.data.text === "inherited conversation"));
    await child.prompt("branch turn").done;
    assert.equal((await parent.refresh()).turns, 1);
    assert.equal((await child.refresh()).turns, 2);
    await kit.sessions.archive(parent.id, false);
    await kit.sessions.setTags(parent.id, []);
    assert.equal((await kit.sessions.list({ archived: true })).length, 0);
  } finally { await kit.close(); }
});

test("budgets use shared settings, preserve totals and report observed stopping thresholds", async () => {
  const kit = await connect();
  try {
    await kit.settings.configure({ patch: { budget: { maxTurnTokens: 0 } } });
    const session = await kit.sessions.create({ runtime: "fake" });
    await assert.rejects(session.prompt("blocked").done, (err: unknown) => err instanceof HarnessKitError && err.reason === "budget_exceeded");
    const first = await session.prompt("hello", { settings: { budget: { maxTurnTokens: null } } }).done;
    assert.equal(first.status, "completed");
    const info = await session.refresh();
    assert.equal(info.usage?.totalTokens, first.usage?.totalTokens);
    const limit = info.usage!.totalTokens;
    await kit.settings.configure({ target: { sessionId: session.id }, patch: { budget: { maxSessionTokens: limit, maxTurnTokens: null } } });
    await assert.rejects(session.prompt("still blocked").done, (err: unknown) => err instanceof HarnessKitError && err.reason === "budget_exceeded");
    await kit.settings.configure({ target: { sessionId: session.id }, patch: { budget: { maxSessionTokens: null } } });
    const second = await session.prompt("again", { settings: { budget: { maxTurnTokens: 1 } } }).done;
    // Fake usage arrives with the result: already completed work stays completed.
    assert.equal(second.status, "completed");
    assert.equal(second.budget?.scope, "turn");
    assert.equal(second.budget?.metric, "tokens");
    const latest = await session.refresh();
    assert.equal(latest.usage?.totalTokens, limit + second.usage!.totalTokens!);
  } finally { await kit.close(); }
});
