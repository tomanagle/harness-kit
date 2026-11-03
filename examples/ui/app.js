// Harness Kit example dashboard: runtimes, sessions, host tools, MCP
// servers, and a chat pane with tool calls and approvals.
//
// It talks only to `window.harnessKit` (see api.js), which the host
// provides: Electron's preload script, or wails-bridge.js. A classic script
// with no imports, so it loads the same way in both. All text is inserted
// with textContent; agent output is never interpreted as HTML.
(() => {
  "use strict";

  // ---- helpers --------------------------------------------------------------
  const $ = (sel) => document.querySelector(sel);
  function el(tag, cls, text) {
    const e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined && text !== null) e.textContent = String(text);
    return e;
  }
  function ago(iso) {
    const s = (Date.now() - new Date(iso).getTime()) / 1000;
    if (!isFinite(s)) return "";
    if (s < 60) return "just now";
    if (s < 3600) return `${Math.floor(s / 60)}m ago`;
    if (s < 86400) return `${Math.floor(s / 3600)}h ago`;
    return `${Math.floor(s / 86400)}d ago`;
  }
  function badge(text, kind) {
    return el("span", `badge ${kind || ""}`, text);
  }

  // ---- layout ---------------------------------------------------------------
  const sidebar = el("aside", "sidebar");
  sidebar.append(el("h1", "", "Harness Kit"));
  function section(title, id, extra) {
    const s = el("section");
    const h = el("h2", "", title);
    if (extra) h.append(extra);
    s.append(h, Object.assign(el("ul", "list"), { id }));
    sidebar.append(s);
    return s;
  }
  section("Runtimes", "runtimes");
  const newBox = el("span", "actions");
  const newRuntime = Object.assign(el("select"), { id: "new-runtime", title: "Runtime for a new session" });
  const newBtn = Object.assign(el("button", "", "New"), { id: "new-session" });
  newBox.append(newRuntime, newBtn);
  section("Sessions", "sessions", newBox);
  section("Host tools", "tools");
  const mcpSection = section("MCP servers", "mcp");
  mcpSection.append(Object.assign(el("p", "meta"), { id: "mcp-note" }));

  const main = el("main");
  const header = el("header", "session-header");
  const titleBox = el("div");
  titleBox.append(Object.assign(el("strong", "", "No session"), { id: "session-title" }), " ", Object.assign(el("span", "meta"), { id: "session-meta" }));
  const renameBtn = Object.assign(el("button", "", "Rename"), { id: "rename", disabled: true, title: "Rename this session (or double-click its title)" });
  const cancelBtn = Object.assign(el("button", "", "Cancel turn"), { id: "cancel", disabled: true });
  const headerActions = el("div", "actions");
  headerActions.append(renameBtn, cancelBtn);
  header.append(titleBox, headerActions);
  const approval = Object.assign(el("div", "approval"), { id: "approval", hidden: true });
  const approvalText = el("div", "approval-text");
  approval.append(approvalText);
  for (const [outcome, label] of [["allow", "Allow"], ["deny", "Deny"], ["abort", "Stop turn"]]) {
    const b = el("button", outcome === "allow" ? "primary" : "", label);
    b.dataset.outcome = outcome;
    approval.append(b);
  }
  const transcript = Object.assign(el("div", "transcript"), { id: "transcript" });
  transcript.append(el("p", "empty", "Create a session, or pick one from the list."));
  const composer = Object.assign(el("form", "composer"), { id: "composer" });
  const promptInput = Object.assign(el("input"), { id: "prompt", placeholder: "Message the agent…", disabled: true, autocomplete: "off" });
  const sendBtn = Object.assign(el("button", "primary", "Send"), { id: "send", type: "submit", disabled: true });
  composer.append(promptInput, sendBtn);
  main.append(header, approval, transcript, composer);
  document.body.append(sidebar, main);

  // ---- state ----------------------------------------------------------------
  let hk;
  let current = null; // SessionInfo
  let pending = null; // approval request shown in the bar
  let busy = false;
  const messages = new Map(); // turnId + itemId -> element
  const toolRows = new Map(); // turnId + callId -> element
  const sessions = new Map(); // sessionId -> most recent SessionInfo
  const updates = new Map(); // sessionId -> latest session.updated event
  let opening = null; // buffers live events while loading a transcript
  let openVersion = 0;
  let lastSeq = 0;

  function setBusy(b) {
    busy = b;
    cancelBtn.disabled = !b;
    sendBtn.disabled = b || !current;
    promptInput.disabled = !current;
  }

  // ---- sidebar --------------------------------------------------------------
  async function refreshOverview() {
    const { runtimes, tools, mcp } = await hk.overview();

    const rl = $("#runtimes");
    rl.replaceChildren();
    newRuntime.replaceChildren();
    for (const d of runtimes) {
      const li = el("li", d.ready ? "ok" : "off");
      li.append(el("span", `dot ${d.ready ? "on" : ""}`), el("span", "name", d.runtime), el("span", "meta", d.version || ""));
      const auth = d.auth?.method ? `${d.auth.state} (${d.auth.method}${d.auth.plan ? ", " + d.auth.plan : ""})` : d.auth?.state;
      li.title = [auth, ...(d.problems || [])].filter(Boolean).join("\n");
      rl.append(li);
      if (d.ready) newRuntime.append(new Option(d.runtime, d.runtime));
    }

    const tl = $("#tools");
    tl.replaceChildren();
    if (!tools.length) tl.append(el("li", "meta", "No host tools registered"));
    for (const t of tools) {
      const li = el("li");
      const top = el("div");
      top.append(el("span", "name mono", t.name));
      if (t.approval === "ask") top.append(badge("asks", "warn"));
      if (t.readOnly) top.append(badge("read-only"));
      li.append(top, el("div", "meta", t.description || ""));
      tl.append(li);
    }

    const ml = $("#mcp");
    const note = $("#mcp-note");
    ml.replaceChildren();
    const notes = [];
    if (mcp.error) {
      notes.push(mcp.error);
    } else {
      if (!mcp.enabled) notes.push("Kit MCP config is off (start the sidecar with --kit-mcp).");
      for (const e of mcp.servers) {
        const li = el("li", e.loaded ? "" : "off");
        const top = el("div");
        top.append(el("span", "name mono", e.name), badge(e.source), badge(e.server.type === "http" || e.server.type === "streamable-http" ? "http" : "stdio"));
        if (!e.loaded) top.append(badge("untrusted", "warn"));
        li.append(top, el("div", "meta mono", e.server.url || [e.server.command, ...(e.server.args || [])].join(" ")));
        if (e.note) li.title = e.note;
        ml.append(li);
      }
      if (mcp.enabled && !mcp.servers.length) ml.append(el("li", "meta", "No kit servers (harness-kit mcp add …)"));
      for (const n of mcp.native || []) {
        if (n.supported) notes.push(`${n.runtime}'s own servers: ${n.servers.length ? n.servers.join(", ") : "none"}${n.error ? " (" + n.error + ")" : ""}`);
      }
      notes.push(mcp.inheritNative ? "Runtimes also load their own MCP servers." : "Runtimes' own MCP servers are off.");
    }
    note.replaceChildren(...notes.map((n) => el("span", "", n)));
  }

  async function refreshSessions() {
    const list = await hk.listSessions();
    sessions.clear();
    for (const info of list) rememberSession(info);
    drawSessions();
  }

  function rememberSession(info) {
    const update = updates.get(info.sessionId);
    if (update && update.seq > info.lastSeq) info = updatedInfo(info, update);
    sessions.set(info.sessionId, info);
    return info;
  }

  function updatedInfo(info, ev) {
    // These are complete mutable fields, so omitted fields clear old state.
    const d = ev.data;
    return { ...info, ...d, pinned: Boolean(d.pinned), archived: Boolean(d.archived), forkPending: Boolean(d.forkPending), metadata: d.metadata, activeTurnId: d.activeTurnId, runtimeSessionId: d.runtimeSessionId,
      lastStatus: d.lastStatus, error: d.error, lastSeq: ev.seq };
  }

  function updateSession(ev) {
    const previous = updates.get(ev.sessionId);
    if (previous && ev.seq <= previous.seq) return;
    updates.set(ev.sessionId, ev);
    const info = sessions.get(ev.sessionId);
    if (info && ev.seq > info.lastSeq) sessions.set(ev.sessionId, updatedInfo(info, ev));
    if (current && current.sessionId === ev.sessionId && ev.seq > current.lastSeq) {
      current = updatedInfo(current, ev);
      showSessionState();
    }
    drawSessions();
  }

  function drawSessions() {
    const list = [...sessions.values()].sort((a, b) => (b.updatedAt || "").localeCompare(a.updatedAt || ""));
    const ul = $("#sessions");
    ul.replaceChildren();
    if (!list.length) ul.append(el("li", "meta", "No sessions yet"));
    for (const s of list) {
      const li = el("li", "session" + (current && s.sessionId === current.sessionId ? " active" : ""));
      li.dataset.id = s.sessionId;
      li.append(
        el("div", "name", s.title || "Untitled session"),
        el("div", "meta", `${s.runtime} · ${s.state} · ${s.turns || 0} turn${s.turns === 1 ? "" : "s"} · ${ago(s.updatedAt)}${s.lastStatus && s.lastStatus !== "completed" ? " · " + s.lastStatus : ""}`),
      );
      li.title = `${s.sessionId}\n${s.runtimeSessionId || ""}\n${s.cwd || ""}`;
      li.addEventListener("click", () => openSession(s.sessionId).catch(showError));
      ul.append(li);
    }
  }

  let refreshTimer;
  function refreshSessionsSoon() {
    clearTimeout(refreshTimer);
    refreshTimer = setTimeout(() => refreshSessions().catch(() => {}), 150);
  }

  // ---- sessions -------------------------------------------------------------
  function showSessionState() {
    $("#session-title").textContent = current.title || "New session";
    $("#session-meta").textContent = `${current.runtime} · ${current.sessionId} · ${current.state}`;
    setBusy(Boolean(current.activeTurnId));
    if (!current.activeTurnId) {
      pending = null;
      approval.hidden = true;
    }
  }

  function showSession(info) {
    current = rememberSession(info);
    lastSeq = 0;
    pending = null;
    approval.hidden = true;
    messages.clear();
    toolRows.clear();
    transcript.replaceChildren();
    renameBtn.disabled = false;
    showSessionState();
    promptInput.focus();
    drawSessions();
  }

  async function newSession() {
    const runtime = newRuntime.value;
    if (!runtime) return;
    const version = ++openVersion;
    opening = null;
    const info = await hk.createSession(runtime);
    if (version === openVersion) showSession(info);
  }

  async function openSession(id) {
    const load = { id, events: [], version: ++openVersion };
    opening = load;
    try {
      const info = await hk.openSession(id);
      if (load.version !== openVersion) return;
      showSession(info);
      let result = await hk.history(id);
      if (load.version !== openVersion) return;
      let history = Array.isArray(result) ? result : result.events;
      // A gap can arrive while the first read is in flight. Read again if
      // it extends beyond that snapshot; normal overlapping events dedupe below.
      while (load.events.some((ev) => ev.type === "stream.gap" && ev.data.toSeq > (history.at(-1)?.seq || 0))) {
        load.events = load.events.filter((ev) => ev.type !== "stream.gap");
        result = await hk.history(id);
        if (load.version !== openVersion) return;
        history = Array.isArray(result) ? result : result.events;
      }
      const events = [...history, ...load.events].filter((ev) => ev.type !== "stream.gap").sort((a, b) => a.seq - b.seq);
      const turns = new Set(events.filter((ev) => ev.type === "turn.started").map((ev) => ev.turnId));
      const missing = Math.max(0, (current.turns || 0) - turns.size);
      if (missing) {
        transcript.append(el("p", "status", `${missing} earlier turn(s) have no saved transcript in Harness Kit.`));
      } else if (result.truncated) {
        transcript.append(el("p", "status", "Some earlier events are unavailable."));
      }
      for (const ev of events) renderOrdered(ev);
      // The fetched snapshot is authoritative for older lifecycle/approval
      // events (e.g. an interrupted session left by a process crash).
      showSessionState();
    } finally {
      if (opening === load) opening = null;
    }
  }

  function renderOrdered(ev) {
    if (ev.seq <= lastSeq) return;
    lastSeq = ev.seq;
    render(ev);
  }

  // Swaps the header title for a text field: Enter or leaving the field
  // saves, Escape cancels.
  function startRename() {
    if (!current || $("#rename-input")) return;
    const id = current.sessionId;
    const was = current.title || "";
    const titleEl = $("#session-title");
    const input = Object.assign(el("input", "rename-input"), { id: "rename-input", value: was, maxLength: 200, placeholder: "Session title" });
    titleEl.hidden = true;
    titleEl.after(input);
    input.focus();
    input.select();
    let finished = false;
    const finish = async (save) => {
      if (finished) return;
      finished = true;
      const next = input.value.trim();
      input.remove();
      titleEl.hidden = false;
      if (!save || !next || next === was) return;
      try {
        const info = await hk.renameSession(id, next);
        rememberSession(info);
        if (current && current.sessionId === id && info.lastSeq >= current.lastSeq) {
          current = info;
          showSessionState();
        }
        drawSessions();
      } catch (e) {
        showError(e);
      }
    };
    input.addEventListener("keydown", (e) => {
      if (e.key === "Enter") finish(true);
      if (e.key === "Escape") finish(false);
    });
    input.addEventListener("blur", () => finish(true));
  }

  function showError(e) {
    transcript.append(el("p", "status error", String(e?.message ?? e)));
    transcript.scrollTop = transcript.scrollHeight;
  }

  // ---- transcript -----------------------------------------------------------
  function render(ev) {
    const d = ev.data || {};
    // Runtime item IDs are only guaranteed to be unique within a turn.
    const itemKey = `${ev.turnId || ""}:${d.itemId || ""}`;
    const callKey = `${ev.turnId || ""}:${d.callId || ""}`;
    switch (ev.type) {
      case "session.updated":
        updateSession(ev);
        break;
      case "turn.started":
        transcript.append(el("div", "bubble user", d.text));
        if (!current.title && d.text) {
          current.title = d.text.split(/\s+/).join(" ").slice(0, 80);
          $("#session-title").textContent = current.title;
        }
        setBusy(true);
        break;
      case "message.delta": {
        if (d.parentCallId) break;
        let m = messages.get(itemKey);
        if (!m) messages.set(itemKey, (m = transcript.appendChild(el("div", "bubble agent", ""))));
        m.textContent += d.text;
        break;
      }
      case "message.completed": {
        if (d.parentCallId) break;
        let m = messages.get(itemKey);
        if (!m) messages.set(itemKey, (m = transcript.appendChild(el("div", "bubble agent"))));
        m.textContent = d.text;
        break;
      }
      case "tool.started": {
        const row = el("div", "tool");
        row.append(el("span", "tool-name mono", d.title || d.name), el("span", "badge", d.kind));
        toolRows.set(callKey, row);
        transcript.append(row);
        break;
      }
      case "tool.completed": {
        const row = toolRows.get(callKey) || transcript.appendChild(el("div", "tool"));
        row.classList.add(d.status);
        row.append(el("span", "badge " + (d.status === "completed" ? "ok" : "warn"), d.status));
        const out = d.error || d.output;
        if (out) row.append(el("pre", "tool-output", out.length > 600 ? out.slice(0, 600) + "…" : out));
        break;
      }
      case "approval.requested":
        pending = d;
        approvalText.replaceChildren(el("strong", "", d.title), el("span", "meta", d.reason ? " " + d.reason : ""));
        approval.hidden = false;
        break;
      case "approval.resolved":
        if (pending && pending.approvalId === d.approvalId) {
          pending = null;
          approval.hidden = true;
        }
        transcript.append(el("p", "status", `${d.decision.outcome === "allow" || d.decision.outcome === "allow_session" ? "✓ approved" : "✗ " + d.decision.outcome}${d.source !== "host" ? " (" + d.source + ")" : ""}`));
        break;
      case "runtime.status":
        if (d.status === "warning" || d.status === "mcp" || d.status === "permission_denied" || d.status === "retrying") {
          transcript.append(el("p", "status", `${d.status}: ${d.message || ""}`));
        }
        break;
      case "error":
        transcript.append(el("p", "status error", d.message));
        break;
      case "turn.completed": {
        let line = d.status;
        if (d.usage) line += ` · ${d.usage.inputTokens || 0} in / ${d.usage.outputTokens || 0} out tokens`;
        if (d.usage?.costUsd != null) line += ` · ~$${d.usage.costUsd.toFixed(4)}`;
        if (d.error) line += ` · ${d.error.message}`;
        transcript.append(el("p", "status turn-end " + d.status, line));
        pending = null;
        approval.hidden = true;
        setBusy(false);
        break;
      }
      case "stream.gap":
        transcript.append(el("p", "status", "(some events were skipped)"));
        break;
    }
    transcript.scrollTop = transcript.scrollHeight;
  }

  // ---- wiring ---------------------------------------------------------------
  newBtn.addEventListener("click", () => newSession().catch(showError));
  renameBtn.addEventListener("click", startRename);
  $("#session-title").addEventListener("dblclick", startRename);
  composer.addEventListener("submit", async (e) => {
    e.preventDefault();
    const text = promptInput.value.trim();
    if (!text || !current || busy) return;
    promptInput.value = "";
    setBusy(true);
    try {
      await hk.prompt(current.sessionId, text);
    } catch (err) {
      setBusy(false);
      showError(err);
    }
  });
  cancelBtn.addEventListener("click", () => current && hk.cancel(current.sessionId).catch(showError));
  approval.addEventListener("click", (e) => {
    const outcome = e.target?.dataset?.outcome;
    if (!outcome || !pending) return;
    hk.approve(pending.approvalId, outcome).catch(showError);
  });

  async function init() {
    // The host may still be setting up (wails-bridge.js is a module).
    for (let i = 0; i < 250 && !window.harnessKitReady && !window.harnessKit; i++) await new Promise((r) => setTimeout(r, 20));
    hk = await (window.harnessKitReady || Promise.resolve(window.harnessKit));
    if (!hk) throw new Error("window.harnessKit is not available");
    hk.onEvent((ev) => {
      if (ev.type === "session.updated") updateSession(ev);
      if (ev.type === "session.started" && !sessions.has(ev.sessionId)) refreshSessionsSoon();
      if (opening && ev.sessionId === opening.id) {
        opening.events.push(ev);
        return;
      }
      if (!current || ev.sessionId !== current.sessionId) return;
      if (ev.type === "stream.gap") {
        openSession(ev.sessionId).catch(showError);
      } else {
        renderOrdered(ev);
      }
    });
    await Promise.all([refreshOverview(), refreshSessions()]);
    document.body.dataset.ready = "1";
  }
  init().catch((e) => {
    document.body.dataset.ready = "error";
    showError(e);
  });
})();
