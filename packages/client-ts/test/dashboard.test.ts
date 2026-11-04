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
  checked = false;
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
  fire(name: string) { for (const fn of this.listeners.get(name) || []) fn({ target: this, preventDefault() {} }); }
  click() { if (!this.disabled) this.fire("click"); }
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

const gitStatus = (root = "/repo", files: unknown[] = []) => ({ root, head: "a".repeat(40), branch: "main", detached: false, unborn: false, ahead: 0, behind: 0, files, remotes: [{ name: "origin" }], truncated: false });
const gitBranches = async () => ({ branches: [{ name: "main", current: true }, { name: "topic" }], truncated: false });
const gitButton = (ui: Awaited<ReturnType<typeof dashboard>>, name: string) => all(ui.get("git-files")).find((e) => e.className.split(" ").includes("git-" + name))!;
const plain = (value: unknown) => JSON.parse(JSON.stringify(value));

test("source control stages exact selected paths and commits only after explicit actions", async () => {
  const calls: unknown[] = [];
  const path = "new <script> literal\nfile.txt";
  const status = gitStatus("/repo", [{ path, originalPath: "old name.txt", index: "R", worktree: "M" }]);
  const ui = await dashboard({
    gitStatus: async () => status, gitBranches,
    gitStage: async (...args: unknown[]) => { calls.push(["stage", ...args]); },
    gitUnstage: async (...args: unknown[]) => { calls.push(["unstage", ...args]); },
    gitCommit: async (...args: unknown[]) => { calls.push(["commit", ...args]); return { commit: "b".repeat(40) }; },
    gitPush: async (...args: unknown[]) => { calls.push(["push", ...args]); return { output: "Pushed", truncated: false }; },
  });
  (ui.get("sessions").children[0] as Element).click();
  await flush();
  assert.equal(ui.get("git-panel").hidden, false);
  assert.match(ui.get("git-files").textContent, /<script> literal/);
  assert.deepEqual(calls, [], "viewing a session must not stage, commit or push");
  gitButton(ui, "stage").click();
  await flush();
  assert.deepEqual(plain(calls), [["stage", "ses_a", ["old name.txt", path]]]);
  ui.get("git-commit-message").value = "Keep the caller's message";
  ui.get("git-commit-form").fire("submit");
  await flush();
  assert.deepEqual(plain(calls), [["stage", "ses_a", ["old name.txt", path]], ["commit", "ses_a", "Keep the caller's message"]]);
  assert.equal(ui.get("git-commit-message").value, "");
  assert.match(ui.get("git-message").textContent, /Committed bbbbbbbbbb/);
  ui.get("git-remote").value = "origin";
  ui.get("git-remote-branch").value = "topic";
  ui.get("git-set-upstream").checked = true;
  ui.get("git-push").click();
  await flush();
  assert.deepEqual(plain(calls).at(-1), ["push", "ses_a", { remote: "origin", branch: "topic", setUpstream: true }]);
});

test("source control displays bounded staged diffs as literal text", async () => {
  let request: unknown;
  const patch = "diff --git a/file b/file\n+<img src=x onerror=bad()>";
  const ui = await dashboard({
    gitStatus: async () => gitStatus("/repo", [{ path: "file", index: "M", worktree: " " }]), gitBranches,
    gitDiff: async (...args: unknown[]) => { request = args; return { patch, truncated: true, encodingLossy: true }; },
  });
  (ui.get("sessions").children[0] as Element).click();
  await flush();
  gitButton(ui, "view-staged").click();
  await flush();
  assert.deepEqual(plain(request), ["ses_a", { path: "file", staged: true, maxBytes: 131072 }]);
  assert.equal(ui.get("git-diff").textContent, patch);
  assert.match(ui.get("git-diff-note").textContent, /truncated.*bytes could not/);
});

test("stale Git reads cannot replace another session's source-control panel", async () => {
  let finishStatus!: (value: unknown) => void;
  let finishDiff!: (value: unknown) => void;
  let slowStatus = true;
  const ui = await dashboard({
    listSessions: async () => [info("ses_a"), info("ses_b")],
    openSession: async (id: string) => info(id),
    gitStatus: (id: string) => id === "ses_a" && slowStatus ? new Promise((resolve) => { finishStatus = resolve; }) : Promise.resolve(gitStatus("/repo/" + id, [{ path: "file", index: "M", worktree: " " }])),
    gitBranches,
    gitDiff: () => new Promise((resolve) => { finishDiff = resolve; }),
  });
  (ui.get("sessions").children[0] as Element).click();
  await flush();
  (ui.get("sessions").children[1] as Element).click();
  await flush();
  finishStatus(gitStatus("/stale"));
  await flush();
  assert.equal(ui.get("git-root").textContent, "/repo/ses_b");
  gitButton(ui, "view-staged").click();
  slowStatus = false;
  (ui.get("sessions").children[0] as Element).click();
  await flush();
  finishDiff({ patch: "old session diff", truncated: false });
  await flush();
  assert.equal(ui.get("git-root").textContent, "/repo/ses_a");
  assert.equal(ui.get("git-diff").textContent, "");
});

test("Git failures remain local to the panel and writes pause during a turn", async () => {
  let available = false, commits = 0;
  const ui = await dashboard({
    gitStatus: async () => { if (!available) throw new Error("not a Git repository"); return gitStatus("/repo", [{ path: "file", index: "M", worktree: " " }]); },
    gitBranches,
    gitCommit: async () => { commits++; throw new Error("commit hook rejected the message"); },
  });
  (ui.get("sessions").children[0] as Element).click();
  await flush();
  assert.match(ui.get("git-message").textContent, /not a Git repository/);
  assert.doesNotMatch(ui.get("transcript").textContent, /not a Git repository/);
  assert.equal(ui.get("send").disabled, false);
  available = true;
  ui.get("git-refresh").click();
  await flush();
  ui.receive(event(6, "session.updated", { title: "Running", state: "running", activeTurnId: "t", turns: 2, updatedAt: stamp }));
  ui.get("git-commit-message").value = "Message retained on failure";
  ui.get("git-commit-form").fire("submit");
  await flush();
  assert.equal(commits, 0);
  ui.receive(event(7, "session.updated", { title: "Idle", state: "idle", turns: 2, updatedAt: stamp }));
  ui.get("git-commit-form").fire("submit");
  await flush();
  assert.equal(commits, 1);
  assert.match(ui.get("git-message").textContent, /hook rejected/);
  assert.equal(ui.get("git-commit-message").value, "Message retained on failure");
});

test("dashboard Git API forwards explicit operations without restoring the runtime", async () => {
  const calls: unknown[] = [];
  const api = makeApi({
    git: {
      status: async (...args: unknown[]) => { calls.push(["status", ...args]); return gitStatus(); },
      stage: async (...args: unknown[]) => { calls.push(["stage", ...args]); },
      commit: async (...args: unknown[]) => { calls.push(["commit", ...args]); },
      push: async (...args: unknown[]) => { calls.push(["push", ...args]); },
    },
    sessions: { restore: () => { throw new Error("Git actions must not start an agent"); } },
  });
  await api.gitStatus("ses_a");
  await api.gitStage("ses_a", ["literal [file].txt"]);
  await api.gitCommit("ses_a", "Chosen message");
  await api.gitPush("ses_a", { remote: "origin", branch: "topic" });
  assert.deepEqual(calls, [["status", "ses_a"], ["stage", "ses_a", ["literal [file].txt"]], ["commit", "ses_a", "Chosen message"], ["push", "ses_a", { remote: "origin", branch: "topic" }]]);
});

test("dashboard hides source control when the host does not advertise Git", async () => {
  let gitReads = 0;
  const ui = await dashboard({
    overview: async () => ({ runtimes: [], tools: [], mcp: { enabled: true, servers: [] }, git: false }),
    gitStatus: async () => { gitReads++; return gitStatus(); }, gitBranches,
  });
  (ui.get("sessions").children[0] as Element).click();
  await flush();
  assert.equal(ui.get("git-panel").hidden, true);
  assert.equal(gitReads, 0);
  assert.equal(ui.get("send").disabled, false);
});
