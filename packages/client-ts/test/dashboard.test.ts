// The dashboard is a classic script shared by Electron and Wails. Exercise
// replay races without a browser or new dependencies, using its small DOM surface.
import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { runInNewContext } from "node:vm";
import { makeApi } from "../../../examples/ui/api.js";

class Element {
  children: (Element | string)[] = [];
  listeners = new Map<string, Function[]>();
  dataset: Record<string, string> = {};
  id = "";
  className = "";
  hidden = false;
  disabled = false;
  value = "";
  parent?: Element;
  classList = { add: (name: string) => { this.className += " " + name; } };
  get textContent(): string { return this.children.map((c) => typeof c === "string" ? c : c.textContent).join(""); }
  set textContent(text: string) { this.children = [text]; }
  append(...nodes: (Element | string)[]) {
    for (const node of nodes) {
      if (typeof node !== "string") node.parent = this;
      this.children.push(node);
    }
  }
  appendChild(node: Element) { this.append(node); return node; }
  replaceChildren(...nodes: (Element | string)[]) { this.children = []; this.append(...nodes); }
  addEventListener(name: string, fn: Function) { this.listeners.set(name, [...(this.listeners.get(name) || []), fn]); }
  click() { for (const fn of this.listeners.get("click") || []) fn({ target: this }); }
  focus() {}
  select() {}
  after(node: Element) { this.parent?.children.splice(this.parent.children.indexOf(this) + 1, 0, node); node.parent = this.parent; }
  remove() { this.parent?.children.splice(this.parent.children.indexOf(this), 1); }
}
function all(root: Element): Element[] {
  return [root, ...root.children.flatMap((c) => typeof c === "string" ? [] : all(c))];
}
const flush = () => new Promise<void>((resolve) => setImmediate(resolve));
const stamp = new Date().toISOString();
const info = (id = "ses_a") => ({ sessionId: id, runtime: "fake", state: "closed", title: "Original", turns: 1, lastSeq: 5, updatedAt: stamp, createdAt: stamp });
const event = (seq: number, type: string, data: unknown, turnId = "turn_a", sessionId = "ses_a") => ({ seq, type, data, turnId, sessionId, time: stamp });
const transcript = [
  event(1, "turn.started", { text: "hello" }),
  event(2, "message.delta", { itemId: "reused", text: "hello" }),
  event(3, "message.completed", { itemId: "reused", text: "hello there" }),
  event(4, "turn.completed", { status: "completed" }),
];
async function dashboard(opts: Record<string, unknown> = {}) {
  const body = new Element();
  const document = { body, createElement: () => new Element(), querySelector: (selector: string) => all(body).find((e) => selector.startsWith("#") ? e.id === selector.slice(1) : e.className.split(" ").includes(selector.slice(1))) };
  let receive: (ev: unknown) => void = () => {};
  let lists = 0;
  const api = {
    overview: async () => ({ runtimes: [], tools: [], mcp: { enabled: true, servers: [] } }),
    listSessions: async () => { lists++; return [info()]; },
    openSession: async () => info(),
    history: async () => ({ events: transcript, truncated: false }),
    onEvent: (fn: typeof receive) => { receive = fn; },
    ...opts,
  };
  runInNewContext(readFileSync(new URL("../../../examples/ui/app.js", import.meta.url), "utf8"), {
    document, window: { harnessKit: api }, Option: Element, setTimeout, clearTimeout,
  });
  await flush();
  assert.equal(body.dataset.ready, "1");
  const get = (id: string) => document.querySelector("#" + id)!;
  const rows = (cls: string) => all(get("transcript")).filter((e) => e.className.split(" ").includes(cls));
  return { get, rows, receive: (ev: unknown) => receive(ev), lists: () => lists };
}

test("dashboard replays history once while overlapping live events are buffered", async () => {
  let resolveHistory!: (value: unknown) => void;
  const ui = await dashboard({ history: () => new Promise((resolve) => { resolveHistory = resolve; }) });
  (ui.get("sessions").children[0] as Element).click();
  await flush();
  ui.receive(transcript[2]);
  ui.receive(event(6, "turn.started", { text: "second" }, "turn_b"));
  ui.receive(event(7, "message.completed", { itemId: "reused", text: "second reply" }, "turn_b"));
  resolveHistory({ events: transcript, truncated: false });
  await flush();
  ui.receive(transcript[2]); // a notification delivered after its replay
  assert.deepEqual(ui.rows("user").map((e) => e.textContent), ["hello", "second"]);
  assert.deepEqual(ui.rows("agent").map((e) => e.textContent), ["hello there", "second reply"]);
  assert.doesNotMatch(ui.get("transcript").textContent, /no saved transcript/);
});

test("dashboard applies remote rename and lifecycle updates without fetching sessions", async () => {
  const ui = await dashboard();
  (ui.get("sessions").children[0] as Element).click();
  await flush();
  const fetched = ui.lists();
  ui.receive(event(6, "session.updated", { title: "Remote title", state: "running", activeTurnId: "t", turns: 2, updatedAt: stamp }));
  assert.equal(ui.get("session-title").textContent, "Remote title");
  assert.match(ui.get("sessions").textContent, /Remote title.*running/);
  assert.equal(ui.get("send").disabled, true);
  ui.receive(event(7, "session.updated", { title: "Remote title", state: "idle", turns: 2, updatedAt: stamp }));
  assert.equal(ui.get("send").disabled, false, "omitted activeTurnId clears the running state");
  assert.equal(ui.lists(), fetched);
});

test("dashboard replays bounded turn diffs with their completeness status", async () => {
  const patch = "diff --git a/example.txt b/example.txt\n+<script>literal file content</script>\n";
  const diff = event(5, "workspace.diff", { complete: true, workingTree: patch, index: "+staged\n", truncated: true });
  const ui = await dashboard({ history: async () => ({ events: [...transcript, diff], truncated: false }) });
  (ui.get("sessions").children[0] as Element).click();
  await flush();
  ui.receive(diff); // overlapping live delivery must not duplicate the saved diff
  assert.equal(ui.rows("workspace-diff").length, 1);
  assert.deepEqual(ui.rows("diff-output").map((e) => e.textContent), [patch, "+staged\n"]);
  assert.match(ui.rows("workspace-diff")[0]!.textContent, /truncated by the configured size limit/);
  ui.receive(event(6, "workspace.diff", { complete: false, error: "snapshot limit exceeded" }, "turn_b"));
  assert.match(ui.rows("workspace-diff")[1]!.textContent, /snapshot limit exceeded/);
});

test("dashboard retains image references in saved prompts", async () => {
  const ui = await dashboard({ history: async () => ({ events: [event(1, "turn.started", { input: [{ type: "image", path: "diagram.png" }] })], truncated: false }) });
  (ui.get("sessions").children[0] as Element).click();
  await flush();
  assert.equal(ui.rows("user")[0]!.textContent, "Image: diagram.png");
});

test("a slower session open cannot replace the more recently selected session", async () => {
  let finishA!: (value: unknown) => void;
  const ui = await dashboard({
    listSessions: async () => [info("ses_a"), info("ses_b")],
    openSession: (id: string) => id === "ses_a" ? new Promise((resolve) => { finishA = resolve; }) : Promise.resolve(info(id)),
    history: async () => ({ events: [], truncated: false }),
  });
  (ui.get("sessions").children[0] as Element).click();
  (ui.get("sessions").children[1] as Element).click();
  await flush();
  finishA(info("ses_a"));
  await flush();
  assert.match(ui.get("session-meta").textContent, /ses_b/);
});

test("dashboard marks only the missing part of legacy history", async () => {
  const ui = await dashboard({ openSession: async () => ({ ...info(), turns: 3 }) });
  (ui.get("sessions").children[0] as Element).click();
  await flush();
  assert.match(ui.get("transcript").textContent, /2 earlier turn\(s\) have no saved transcript/);
  assert.equal(ui.rows("agent").length, 1);
});

test("dashboard API reads history without restoring; prompts restore lazily", async () => {
  let restores = 0;
  const api = makeApi({
    sessions: {
      get: async () => info(),
      events: async () => ({ events: transcript, truncated: true }),
      restore: async () => { restores++; return { prompt: () => ({ id: "t", done: Promise.resolve() }) }; },
    },
  });
  assert.equal((await api.openSession("ses_a")).state, "closed");
  assert.equal((await api.history("ses_a")).truncated, true);
  assert.equal(restores, 0);
  assert.equal(await api.prompt("ses_a", "continue"), "t");
  assert.equal(restores, 1);
});
