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
  const gitPanel = Object.assign(el("details", "git-panel"), { id: "git-panel", hidden: true });
  const gitSummary = Object.assign(el("summary", "", "Source control"), { id: "git-summary" });
  const gitToolbar = el("div", "git-toolbar");
  const gitRefresh = Object.assign(el("button", "", "Refresh"), { id: "git-refresh", type: "button" });
  const gitHistory = Object.assign(el("button", "", "Recent commits"), { id: "git-history", type: "button" });
  const gitRoot = Object.assign(el("span", "meta mono"), { id: "git-root" });
  gitToolbar.append(gitRefresh, gitHistory, gitRoot);
  const gitMessage = Object.assign(el("p", "status"), { id: "git-message" });
  const gitFiles = Object.assign(el("ul", "git-files list"), { id: "git-files" });
  const gitBranchRow = el("div", "git-toolbar");
  const gitBranches = Object.assign(el("select"), { id: "git-branch", title: "Local branch" });
  const gitSwitch = Object.assign(el("button", "", "Switch branch"), { id: "git-switch", type: "button" });
  const gitNewBranch = Object.assign(el("input"), { id: "git-new-branch", placeholder: "New branch name", autocomplete: "off" });
  const gitCreateBranch = Object.assign(el("button", "", "Create branch"), { id: "git-create-branch", type: "button" });
  gitBranchRow.append(gitBranches, gitSwitch, gitNewBranch, gitCreateBranch);
  const gitCommitForm = Object.assign(el("form", "git-toolbar"), { id: "git-commit-form" });
  const gitCommitMessage = Object.assign(el("input"), { id: "git-commit-message", placeholder: "Commit message (staged changes only)", autocomplete: "off" });
  const gitCommit = Object.assign(el("button", "primary", "Commit staged"), { id: "git-commit", type: "submit" });
  gitCommitForm.append(gitCommitMessage, gitCommit);
  const gitRemoteRow = el("div", "git-toolbar");
  const gitRemote = Object.assign(el("select"), { id: "git-remote", title: "Configured Git remote" });
  const gitRemoteBranch = Object.assign(el("input"), { id: "git-remote-branch", placeholder: "Remote branch (optional)", autocomplete: "off" });
  const gitUpstreamLabel = el("label", "meta git-checkbox");
  const gitUpstream = Object.assign(el("input"), { id: "git-set-upstream", type: "checkbox" });
  gitUpstreamLabel.append(gitUpstream, "Set upstream on push");
  const gitFetch = Object.assign(el("button", "", "Fetch"), { id: "git-fetch", type: "button" });
  const gitPull = Object.assign(el("button", "", "Pull (fast-forward)"), { id: "git-pull", type: "button" });
  const gitPush = Object.assign(el("button", "", "Push"), { id: "git-push", type: "button" });
  gitRemoteRow.append(gitRemote, gitRemoteBranch, gitUpstreamLabel, gitFetch, gitPull, gitPush);
  const gitDiffTitle = Object.assign(el("p", "meta"), { id: "git-diff-title" });
  const gitDiff = Object.assign(el("pre", "diff-output"), { id: "git-diff" });
  const gitDiffNote = Object.assign(el("p", "status"), { id: "git-diff-note" });
  gitPanel.append(gitSummary, gitToolbar, gitMessage, gitFiles, gitBranchRow, gitCommitForm, gitRemoteRow, gitDiffTitle, gitDiffNote, gitDiff);
  main.append(header, approval, gitPanel, transcript, composer);
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
  let gitStatus = null;
  let gitAvailable = false;
  let gitLoading = false;
  let gitAction = null;
  let gitVersion = 0;
  let gitDiffVersion = 0;
  let gitFileActions = [];

  function setBusy(b) {
    busy = b;
    cancelBtn.disabled = !b;
    sendBtn.disabled = b || !current;
    promptInput.disabled = !current;
    updateGitControls();
  }

  // ---- sidebar --------------------------------------------------------------
  async function refreshOverview() {
    const { runtimes, tools, mcp, git } = await hk.overview();
    gitAvailable = git !== false && typeof hk.gitStatus === "function";
    gitPanel.hidden = !gitAvailable || !current;

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

  // ---- source control -------------------------------------------------------
  // All mutations are explicit user actions. This example chooses to disable
  // them while a turn runs; the host independently enforces its settings.git.
  function updateGitControls() {
    const unavailable = !current || !gitStatus || gitLoading || Boolean(gitAction);
    const writesDisabled = unavailable || busy;
    gitRefresh.disabled = !current || gitLoading || Boolean(gitAction);
    gitHistory.disabled = unavailable || gitStatus?.unborn;
    gitSwitch.disabled = writesDisabled || !gitBranches.value;
    gitCreateBranch.disabled = writesDisabled;
    gitCommit.disabled = writesDisabled || !(gitStatus?.files || []).some((f) => hasIndexChange(f));
    gitBranches.disabled = gitNewBranch.disabled = gitCommitMessage.disabled = writesDisabled;
    const remoteDisabled = writesDisabled || !(gitStatus?.remotes || []).length;
    gitFetch.disabled = gitPull.disabled = gitPush.disabled = remoteDisabled;
    gitRemote.disabled = gitRemoteBranch.disabled = gitUpstream.disabled = remoteDisabled;
    for (const { button, write } of gitFileActions) button.disabled = write ? writesDisabled : unavailable;
  }

  function hasIndexChange(file) { return Boolean(file.index && ![" ", ".", "?"].includes(file.index)); }
  function hasWorktreeChange(file) { return Boolean(file.untracked || (file.worktree && ![" ", "."].includes(file.worktree))); }
  function gitNote(text, error = false) {
    gitMessage.textContent = text;
    gitMessage.className = "status" + (error ? " error" : "");
  }
  function resetGit() {
    gitVersion++;
    gitDiffVersion++;
    gitStatus = null;
    gitLoading = false;
    gitFileActions = [];
    gitPanel.hidden = !gitAvailable;
    gitFiles.replaceChildren();
    gitBranches.replaceChildren();
    gitRemote.replaceChildren();
    gitRoot.textContent = gitDiff.textContent = gitDiffTitle.textContent = gitDiffNote.textContent = "";
    gitCommitMessage.value = gitNewBranch.value = gitRemoteBranch.value = "";
    gitUpstream.checked = false;
    gitSummary.textContent = "Source control";
    gitNote("Loading repository…");
    updateGitControls();
  }

  async function refreshGit() {
    if (!current || !gitAvailable) return false;
    const id = current.sessionId, version = ++gitVersion;
    gitLoading = true;
    updateGitControls();
    try {
      const [status, branches] = await Promise.all([hk.gitStatus(id), hk.gitBranches(id)]);
      if (version !== gitVersion || current?.sessionId !== id) return false;
      gitStatus = status;
      gitRoot.textContent = status.root;
      gitSummary.textContent = `Source control · ${status.branch || (status.detached ? "detached HEAD" : "unborn branch")} · ${status.files.length} changed file(s)`;
      const upstream = status.upstream ? ` · ${status.upstream}: ${status.ahead} ahead, ${status.behind} behind` : "";
      gitNote(`${status.unborn ? "No commits yet" : status.head?.slice(0, 10) || "Repository"}${upstream}${status.truncated ? " · File list truncated by host limits" : ""}${branches.truncated ? " · Branch list truncated" : ""}`);
      gitBranches.replaceChildren();
      if (!status.branch) gitBranches.append(Object.assign(el("option", "", "Select a local branch"), { value: "" }));
      for (const branch of branches.branches.filter((b) => !b.remote)) {
        gitBranches.append(Object.assign(el("option", "", branch.name), { value: branch.name }));
      }
      gitBranches.value = status.branch || "";
      const remote = gitRemote.value;
      gitRemote.replaceChildren(Object.assign(el("option", "", "Configured upstream"), { value: "" }));
      for (const entry of status.remotes) gitRemote.append(Object.assign(el("option", "", entry.name), { value: entry.name }));
      gitRemote.value = status.remotes.some((r) => r.name === remote) ? remote : "";
      drawGitFiles(status.files);
      return true;
    } catch (error) {
      if (version !== gitVersion || current?.sessionId !== id) return false;
      gitStatus = null;
      gitSummary.textContent = "Source control · unavailable";
      gitFiles.replaceChildren();
      gitFileActions = [];
      gitNote(String(error?.message ?? error), true);
      return false;
    } finally {
      if (version === gitVersion) {
        gitLoading = false;
        updateGitControls();
      }
    }
  }

  function drawGitFiles(files) {
    gitFiles.replaceChildren();
    gitFileActions = [];
    if (!files.length) gitFiles.append(el("li", "meta", "Working tree clean"));
    for (const file of files) {
      const row = el("li", "git-file");
      row.dataset.path = file.path;
      row.append(el("span", "mono git-file-name", file.originalPath ? `${file.originalPath} → ${file.path}` : file.path));
      row.append(badge(file.conflicted ? "conflict" : file.untracked ? "untracked" : `${file.index || " "}${file.worktree || " "}`));
      const paths = [...new Set([file.originalPath, file.path].filter(Boolean))];
      const action = (name, label, write, fn) => {
        const button = Object.assign(el("button", `git-${name}`, label), { type: "button" });
        button.dataset.path = file.path;
        button.addEventListener("click", () => { if (!button.disabled) fn(); });
        gitFileActions.push({ button, write });
        row.append(button);
      };
      if (hasWorktreeChange(file)) {
        action("view-working", "Working diff", false, () => showGitDiff(file.path, false));
        action("stage", "Stage", true, () => runGit("Stage", (id) => hk.gitStage(id, paths)));
      }
      if (hasIndexChange(file)) {
        action("view-staged", "Staged diff", false, () => showGitDiff(file.path, true));
        action("unstage", "Unstage", true, () => runGit("Unstage", (id) => hk.gitUnstage(id, paths)));
      }
      gitFiles.append(row);
    }
  }

  async function showGitDiff(path, staged) {
    if (!current || gitLoading || gitAction) return;
    const id = current.sessionId, version = ++gitDiffVersion;
    gitDiffTitle.textContent = `${staged ? "Staged" : "Working"} · ${path}`;
    gitDiff.textContent = "Loading…";
    gitDiffNote.textContent = "";
    try {
      const diff = await hk.gitDiff(id, { path, staged, maxBytes: 131072 });
      if (version !== gitDiffVersion || current?.sessionId !== id) return;
      gitDiff.textContent = diff.patch || "No text changes to display.";
      gitDiffNote.textContent = [diff.truncated ? "Diff truncated by the configured size limit." : "", diff.encodingLossy ? "Some bytes could not be displayed as text." : ""].filter(Boolean).join(" ");
    } catch (error) {
      if (version !== gitDiffVersion || current?.sessionId !== id) return;
      gitDiff.textContent = "";
      gitDiffNote.textContent = String(error?.message ?? error);
    }
  }

  async function showGitHistory() {
    if (!current || gitHistory.disabled) return;
    const id = current.sessionId, version = ++gitDiffVersion;
    gitDiffTitle.textContent = "Recent commits";
    gitDiff.textContent = "Loading…";
    gitDiffNote.textContent = "";
    try {
      const result = await hk.gitLog(id, { limit: 20 });
      if (version !== gitDiffVersion || current?.sessionId !== id) return;
      gitDiff.textContent = result.commits.map((commit) => `${commit.id.slice(0, 10)} ${commit.subject}\n  ${commit.authorName} · ${commit.time}`).join("\n");
      gitDiffNote.textContent = result.truncated ? "More commits are available; this view is bounded." : "";
    } catch (error) {
      if (version !== gitDiffVersion || current?.sessionId !== id) return;
      gitDiff.textContent = "";
      gitDiffNote.textContent = String(error?.message ?? error);
    }
  }

  async function runGit(label, operation, success) {
    if (!current || !gitStatus || gitLoading || gitAction || busy) return;
    const id = current.sessionId;
    const action = { id };
    gitAction = action;
    gitDiffVersion++;
    gitNote(`${label}…`);
    updateGitControls();
    try {
      const result = await operation(id);
      if (current?.sessionId !== id) return;
      success?.();
      gitDiff.textContent = gitDiffTitle.textContent = gitDiffNote.textContent = "";
      const refreshed = await refreshGit();
      if (current?.sessionId !== id) return;
      const note = result?.commit ? `Committed ${result.commit.slice(0, 10)}.` : result?.output || `${label} complete.`;
      gitNote(`${note}${result?.truncated ? " (Output truncated.)" : ""}${refreshed ? "" : " Repository refresh failed: " + gitMessage.textContent}`, !refreshed);
    } catch (error) {
      if (current?.sessionId === id) gitNote(String(error?.message ?? error), true);
    } finally {
      if (gitAction === action) gitAction = null;
      updateGitControls();
    }
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
    resetGit();
    void refreshGit();
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
      case "turn.started": {
        const prompt = el("div", "bubble user", d.text);
        for (const part of Array.isArray(d.input) ? d.input : []) {
          if (part.type === "image") prompt.append(el("div", "attachment", `Image: ${part.path || "attachment"}`));
        }
        transcript.append(prompt);
        if (!current.title && d.text) {
          current.title = d.text.split(/\s+/).join(" ").slice(0, 80);
          $("#session-title").textContent = current.title;
        }
        setBusy(true);
        break;
      }
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
      case "workspace.diff": {
        const changes = el("details", "workspace-diff");
        changes.append(el("summary", "", "Changes during this turn"));
        if (d.error || !d.complete) {
          changes.append(el("p", "status error", d.error || "The change snapshot is incomplete."));
        }
        if (d.truncated) changes.append(el("p", "status", "Diff truncated by the configured size limit."));
        if (d.encodingLossy) changes.append(el("p", "status", "Some file content could not be displayed as text."));
        for (const [label, patch] of [["Working files", d.workingTree], ["Staged changes", d.index]]) {
          if (typeof patch !== "string" || !patch) continue;
          changes.append(el("p", "meta", label), el("pre", "diff-output", patch));
        }
        if (d.complete && !d.workingTree && !d.index) {
          changes.append(el("p", "status", "No file changes during this turn."));
        }
        transcript.append(changes);
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
  gitRefresh.addEventListener("click", () => { if (!gitRefresh.disabled) void refreshGit(); });
  gitHistory.addEventListener("click", () => void showGitHistory());
  gitBranches.addEventListener("change", updateGitControls);
  gitSwitch.addEventListener("click", () => {
    if (!gitSwitch.disabled && gitBranches.value) void runGit("Switch branch", (id) => hk.gitCheckout(id, { branch: gitBranches.value }));
  });
  gitCreateBranch.addEventListener("click", () => {
    if (gitCreateBranch.disabled) return;
    const branch = gitNewBranch.value.trim();
    if (!branch) { gitNote("Enter a branch name.", true); return; }
    void runGit("Create branch", (id) => hk.gitCheckout(id, { branch, create: true }), () => { if (gitNewBranch.value.trim() === branch) gitNewBranch.value = ""; });
  });
  gitCommitForm.addEventListener("submit", (event) => {
    event.preventDefault();
    if (gitCommit.disabled) return;
    const message = gitCommitMessage.value;
    if (!message.trim()) { gitNote("Enter a commit message.", true); return; }
    void runGit("Commit", (id) => hk.gitCommit(id, message), () => { if (gitCommitMessage.value === message) gitCommitMessage.value = ""; });
  });
  const remoteOptions = () => ({ ...(gitRemote.value ? { remote: gitRemote.value } : {}), ...(gitRemoteBranch.value.trim() ? { branch: gitRemoteBranch.value.trim() } : {}) });
  gitFetch.addEventListener("click", () => { if (!gitFetch.disabled) void runGit("Fetch", (id) => hk.gitFetch(id, remoteOptions())); });
  gitPull.addEventListener("click", () => { if (!gitPull.disabled) void runGit("Pull", (id) => hk.gitPull(id, remoteOptions())); });
  gitPush.addEventListener("click", () => { if (!gitPush.disabled) void runGit("Push", (id) => hk.gitPush(id, { ...remoteOptions(), ...(gitUpstream.checked ? { setUpstream: true } : {}) })); });
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
        if (ev.type === "turn.completed") void refreshGit();
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
