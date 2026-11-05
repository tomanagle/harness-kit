# Harness Kit

An unopinionated, language- and framework-agnostic kit for building AI harnesses.

Build a desktop app, CLI, editor integration, or other
product that runs coding agents for your users. Harness Kit provides the runtime
adapters, session lifecycle, saved conversations, event streams, approvals, host
tools and resource controls behind that product.

You choose the interface, workflow, models and defaults. Settings can come from
code, a file, a database, CLI flags or your own settings screen. The kit accepts
one settings object at startup and exposes the same configuration surface at
runtime.

## What the kit handles for you

| You need                            | The kit provides                                                                                                           |
| ----------------------------------- | -------------------------------------------------------------------------------------------------------------------------- |
| Run different agents                | Adapters for Codex, Claude Code and configured ACP agents, plus an offline fake runtime                                    |
| Manage processes and conversations  | Session creation, cancellation, close, suspend, restore and native conversation forks                                      |
| Show what an agent is doing         | Typed streams for messages, tool calls, approvals, usage and lifecycle changes                                             |
| Show host diagnostics               | Structured Go `slog` records streamed to subscribed clients, with live level controls and bounded queues                   |
| Reopen a conversation               | Durable session records and JSONL transcripts, paginated replay and transcript search                                      |
| Build a session browser             | Titles, tags, metadata, archive filters and updates for connected clients                                                  |
| Build a settings screen             | One startup/runtime settings object, field metadata, scope/lifetime information, revisions and change notifications        |
| Control resource usage              | Bounded event caches and queues, idle eviction, runtime suspension, pinning and resource statistics                        |
| Let users approve actions           | Normalized approval requests, supported decisions and recovery of pending approvals                                        |
| Give agents your app's capabilities | Host tool registration, argument validation, timeouts, cancellation and an MCP bridge                                      |
| Connect MCP servers                 | Portable server definitions translated into each supported runtime's configuration                                         |
| Send images                         | A shared text/image input format translated by image-capable adapters                                                      |
| Track usage and spending            | Normalized usage, durable accounting and configurable token/cost stopping thresholds                                       |
| Manage the context window           | Consistent current-window observations, native compaction, optional pre-turn thresholds, and explicit unknown/stale values |
| Isolate coding work                 | Optional Git worktrees and saved diffs between turn boundaries                                                             |
| Build source control                | Status, working/staged diffs, branches, history, staging, commits, fetch, pull and push                                    |
| Integrate with your application     | Schema-generated TypeScript and Go clients, a local stdio sidecar, and an embeddable Go SDK                                |
| Build your own agent loop           | Go model-provider interfaces, optional OpenAI/Anthropic adapters, and caller-controlled context management                 |

Features vary by runtime. Capabilities describe what each adapter supports;
the kit does not assume every agent offers the same models, permissions, usage
reporting or resume behavior.

## The kit and the server

There are two product components in this repository:

| Component                                                  | Location                                              | Status                                     |
| ---------------------------------------------------------- | ----------------------------------------------------- | ------------------------------------------ |
| Kit for building harness applications                      | [`core/`](core/README.md) and [`packages/`](packages) | Implemented and usable independently       |
| Deployable server for coordinating sessions across devices | [`server/`](server/README.md)                         | Planned placeholder; no implementation yet |

The existing `harness-kit serve` command is a **local stdio sidecar**. Your app
can ship with it and work without a Harness Kit server. Cross-device sharing,
accounts and coordination belong to the future server and your product design.
Runnable applications live in root [`examples/`](examples).

## Start with TypeScript

The examples below target the **current checkout**. Build the client and sidecar
from the same revision; newer APIs such as Git are not in the published npm
`@tomanagle/harness-kit@0.2.0` release.

Requires Go 1.24+ and Node.js 22.18+ for the sidecar and Node transport:

```sh
git clone https://github.com/tomanagle/harness-kit.git
cd harness-kit
make build client-ts
```

In your application's directory, install the built local client:

```sh
npm install /absolute/path/to/harness-kit/packages/client-ts
```

Save this as `demo.ts` and run `node demo.ts`. Replace the binary path with your
checkout location. The fake runtime runs offline and needs no account or model
API key.

```ts
import { createHarnessKit } from "@tomanagle/harness-kit";
import { stdio } from "@tomanagle/harness-kit/node";

const kit = await createHarnessKit({
  transport: stdio({
    command: "/absolute/path/to/harness-kit/core/bin/harness-kit",
    settings: {
      process: {
        store: "./sessions",
        runtimes: ["fake", "codex", "claude"],
      },
    },
  }),
});

try {
  const session = await kit.sessions.create({
    title: "My first conversation",
    metadata: { project: "demo" },
    settings: { session: { runtime: "fake", cwd: process.cwd() } },
  });

  const turn = session.prompt("Hello from my harness!");
  for await (const event of turn) {
    if (event.type === "message.delta") process.stdout.write(event.data.text);
  }
  const result = await turn.done;
  console.log("\n", result.status, "Saved session:", session.id);
} finally {
  await kit.close();
}
```

For a real agent, install and authenticate its CLI, then select `"codex"` or
`"claude"`. The TypeScript package contains the client; the sidecar and selected
agent are separate executables. Add approval handling before real-agent prompts,
as shown below. `kit.close()` shuts down the host and all its sessions, so the
application that owns the sidecar should control its lifetime.

The following snippets are independent examples within an open connection's
lifetime. `kit` refers to the connected client and `session` to a session handle.

## Saved preferences and lifecycle hooks

Opt into automatic global preference persistence with
`settings.process.settingsPath`. The sidecar loads that JSON document at startup
and saves accepted `kit.settings.configure()` changes before applying them.
Session-specific settings already live in the session store. Embedded Go hosts
can inject a `settings.Store` backed by their own database or use `settings.FileStore`.
See [configuration and persistence](core/docs/configuration.md).

Add application behavior without wrapping every prompt:

```ts
const remove = await kit.hooks.register(
  "turn.before",
  async (context) => {
    if (!context.isFirstTurn) return;
    return {
      input: [...context.input, { type: "text", text: "Project context supplied by my app" }],
    };
  },
  { id: "project-context" },
);

await kit.settings.configure({
  patch: { hooks: { timeoutMs: 10_000, onError: "fail", required: ["project-context"] } },
});
```

Hooks support `session.beforeCreate`, `turn.before` and `turn.after`, with ordered
execution, cancellation, bounded concurrency and connection ownership. First-turn
status survives restore; a rejected preparation does not consume the first turn.
Policy lives in the shared settings object; handlers are registered when the app
starts. Both clients expose typed registration helpers. See [lifecycle hooks](core/docs/hooks.md)
for failure behavior, retries, after-turn completion and Go examples.

## Less client plumbing

Use `session.run()` when you only need the result; it retains no stream of deltas.
Use a bounded observer to build a conversation view, without implementing replay,
deduplication, message assembly or gap recovery yourself:

```ts
const conversation = session.observe({ maxEvents: 2000, maxBytes: 4 * 1024 * 1024 });
await conversation.ready;
console.log(conversation.getSnapshot()); // Messages, tools, subtasks, approvals, context.
const stop = conversation.subscribe((snapshot) => {
  console.log(snapshot.messages, snapshot.truncated);
});
await session.run("Explain this project", { config: { effort: "high" } });
stop();
conversation.close(); // Release the view when it is no longer needed.
```

The window reports truncation rather than pretending to contain all history.
`session.prompt()` retains at most 256 events / 1 MiB by default; overflow is an
explicit iterator error while `.done` still provides the result. Hosts can change
those limits or disable buffering. These bound retained event data, not process RSS.

A settings screen can discover model and effort choices before creating a session:

```ts
const catalog = await kit.settings.get({ runtime: "codex", model: "your-model" });
console.log(catalog.fields["session.effort"]);
// choices, allowCustom, source (reported / declared / unknown), scopes and timing.
```

Native Codex and Claude options have generated types. Supported permission,
model and effort changes apply at turn boundaries through the existing settings
API. Provider/account availability stays explicit; the app chooses its own presets.
Node/Electron hosts can use `prepareAttachments()` from the `/node` entry point
for bounded text/image file preparation with an injectable filesystem.
See the [TypeScript client guide](packages/client-ts/README.md#application-helpers-current-checkout)
and [Go client](packages/client-go/README.md) for these helpers and their limits.

## Runtime discovery, streaming and cancellation

The kit translates each runtime's protocol into a common event stream. Tool
activity, usage and completion retain typed payloads alongside message text.
The turn iterator handles stream-gap recovery from retained history; raw event
listeners can use paginated replay when they receive `stream.gap`.

```ts
const registered = await kit.runtimes.list();
const discovered = await kit.runtimes.discover({ runtimes: ["codex", "claude"] });
console.log(registered, discovered); // Capabilities, installation and auth status.

const controller = new AbortController();
const turn = session.prompt("Explain the architecture", {
  signal: controller.signal,
});
// Your Stop button can call controller.abort() or turn.cancel().
for await (const event of turn) {
  if (event.type === "message.delta") process.stdout.write(event.data.text);
  if (event.type === "tool.started") console.log("Tool:", event.data.name);
  if (event.type === "usage") console.log("Usage:", event.data);
}
console.log((await turn.done).status);
```

Images use the same prompt method on an image-capable runtime:

```ts
await session.prompt([
  { type: "text", text: "Explain the layout in this screenshot" },
  { type: "image", path: "/absolute/path/to/screenshot.png" },
]).done;
```

Image paths refer to files on the sidecar's machine. Transcripts retain the
references, not copies of the image files. See [runtime adapters](core/docs/runtimes.md).

A session's `cwd` selects its working folder. For work across multiple folders,
Codex and Claude accept `options: { addDirs: ["/absolute/extra/project"] }` at
session creation. Claude maps this to `--add-dir`; Codex selects those extra
workspace-write roots while retaining its native sandbox mode and approval
policy. These are startup options and are persisted when sessions are restored.

## Observe delegated agents

`subagent.updated` reports native delegated work with a child ID, spawning call ID (when known), optional parent link, assignment, status and result. These events are saved and replayed like other conversation events, so applications can project them into subtasks:

```ts
const unsubscribe = session.onEvent((event) => {
  if (event.type === "subagent.updated") {
    const child = event.data;
    // Upsert by child.id; reconcile a provisional ID using child.callId.
    console.log(child.id, child.title, child.status, child.output);
  }
});
```

Statuses are `running`, `completed`, `failed`, `cancelled` and `unknown`. A successful spawn or wait tool call is separate from the child's lifecycle. Codex reports collaboration snapshots; Claude reports Agent/Task calls and identified subagent task notifications, excluding shell/Monitor tasks. Missing outcomes stay unknown; parent completion is not evidence of child completion. This is observation of native agents, not a child-session control API. The mapping follows the [Codex app-server schema](https://github.com/openai/codex/blob/main/codex-rs/app-server-protocol/schema/json/v2/ThreadStartResponse.json) and [Claude task lifecycle messages](https://code.claude.com/docs/en/agent-sdk/typescript#sdktaskstartedmessage).

## One settings object, including your settings UI

Use `stdio({ settings })` for startup and `kit.settings.configure()` for updates.
Global session settings supply defaults for new sessions; a session target
changes an existing session, and prompt settings apply to one turn.

```ts
// Inspect scope, timing and current values before rendering settings controls.
const current = await kit.settings.get();
console.log(current.fields["memory.evictAfterIdleMs"]);

// Revision checking prevents one settings window overwriting newer changes.
await kit.settings.configure({
  expectedRevision: current.revision,
  patch: { memory: { evictAfterIdleMs: 10 * 60_000 } },
});

// Change this session's defaults for future turns.
await kit.settings.configure({
  target: { sessionId: session.id },
  patch: { session: { effort: "high" } },
});

// Override just one prompt, without changing those defaults.
await session.prompt("Give me a quick summary", {
  settings: { session: { effort: "low" } },
}).done;

// Use the same session field to select a supported model:
// patch: { session: { model: selectedModel } }
```

Subscribe with `kit.settings.onChanged()` before reading the initial snapshot;
use target revisions to ignore stale reads and refresh on `resync`. Updates
preserve omitted fields; `reset` restores inherited values. Your application
saves accepted preferences for its next launch.

Memory controls apply live, model/effort changes apply to future turns, and Git
policy applies to the next operation. Process wiring such as the store directory
or executable paths requires a restart; the kit reports that requirement and
rejects the patch without partially applying it. Field metadata lets your UI
explain these lifetimes. See [configuration](core/docs/configuration.md).

Permission presets belong to the harness UI. The kit exposes independent native
options and preserves provider defaults when they are omitted. For example,
create a Codex conversation with sandboxed automatic approval review:

```ts
const session = await kit.sessions.create({
  runtime: "codex",
  cwd: "/path/to/project",
  options: {
    sandbox: "workspace-write",
    approvalPolicy: "on-request",
    approvalsReviewer: "auto_review", // "user" routes approvals to your UI
  },
});
```

Codex separates sandbox boundaries, prompting policy and reviewer selection;
Claude exposes `options.permissionMode` (`default`, `acceptEdits`, `auto`,
`plan`, `dontAsk`, or `bypassPermissions`). These permission options are
startup-only; effort can change for subsequent turns. Full access requires
the host to opt into `process.allowUnsafe` and explicitly select Codex's
`danger-full-access` sandbox or Claude's `bypassPermissions` mode. Enabling
that host gate alone does not select a permission mode. Native account and
organization restrictions still apply. See the official
[Codex sandbox and reviewer settings](https://learn.chatgpt.com/docs/sandboxing)
and [Claude permission modes](https://code.claude.com/docs/en/permissions).

## Stream diagnostics to your UI

The core uses Go's built-in `log/slog`. Clients can subscribe to structured host
diagnostics independently of conversation events. You choose how to display or
store them; the kit does not retain a second log history.

```ts
await kit.settings.configure({
  patch: {
    logging: {
      level: "debug",
      maxQueueRecords: 256,
      maxQueueBytes: 1_048_576,
      maxRecordBytes: 65_536,
    },
  },
});

// Await subscription readiness before starting work you want to observe.
const stopLogs = await kit.logs.subscribe(({ record, dropped }) => {
  if (record) console.log(record.time, record.level, record.message, record.attrs);
  if (dropped) console.log(`${dropped.count} diagnostic records were dropped`);
});

await session.prompt("Explain the project").done;
await stopLogs();
```

Records preserve structured attributes, groups, source location when available,
and sequence numbers. Slow readers receive explicit loss markers; oversized
records are marked `truncated` or dropped if even their envelope cannot fit.
Go hosts can use `manager.Logger()` and `manager.Logs().Subscribe()`, or inject a
shared `logging.Stream` while keeping their own `slog.Handler`. No global logger
is replaced. See [logging](core/docs/logging.md) for Go examples and queue semantics.

## Saved conversations, replay and session organization

With a file store, the kit writes each session record and its events as they
arrive. You can reopen a transcript without starting an agent process. History
is paginated so your UI can load a conversation incrementally.

```ts
const page = await kit.sessions.eventsPage(session.id, {
  afterSeq: 0,
  limit: 100,
});
for (const event of page.events) console.log(event.type, event.data);
// If page.hasMore, request the next page with afterSeq: page.nextSeq.
// page.truncated reports a gap in retained history.

await session.close(); // Stops the runtime; keeps the saved conversation.
const resumed = await kit.sessions.restore(session.id);
// You can now send the next prompt through resumed.
```

`attach(id)` obtains a handle without restoring the runtime. `restore(id)` resumes
its native conversation under the same kit ID, including after a sidecar restart
when both kit storage and the runtime's own history are available.

```ts
await session.rename("Fix authentication");
await kit.sessions.setTags(session.id, ["backend", "review"]);
const matches = await kit.sessions.list({
  query: "authentication",
  searchTranscript: true,
  tags: ["backend"],
  archived: false,
  limit: 20,
});
console.log(matches);

await kit.sessions.archive(session.id); // Hide with an archive filter.
await kit.sessions.archive(session.id, false); // Unarchive.

const unsubscribe = kit.onEvent((event) => {
  if (event.type === "session.updated") {
    console.log("Update session UI:", event.sessionId, event.data);
  }
});
// Call unsubscribe() when the view unmounts.
```

Archiving affects catalog visibility; it does not stop or delete the session.
`Session.info` is the last fetched snapshot; use events or `session.refresh()`
for changes. Creation also accepts application-owned `metadata`.

Branch an idle conversation after a completed turn:

```ts
const alternate = await kit.sessions.fork(session.id, {
  title: "Try a different approach",
});
await alternate.prompt("Solve the same problem using a smaller change").done;
```

Forking uses the runtime's native support and branches the whole conversation.
Claude materializes its fork on the child's first prompt; prompt the child before
advancing its source. Worktree forks need their own explicit workspace root/path.
See [session behavior and protocol](core/docs/protocol.md).

## Memory controls and idle suspension

Disk persistence and memory retention are separate. With a durable store,
transcripts are already on disk before eviction. Evicting a cache releases event
data; suspending an idle runtime releases its process resources. These policies
measure inactivity, so an old conversation being used is not evicted just
because it was created a long time ago.

```ts
await kit.settings.configure({
  patch: {
    memory: {
      maxCacheBytes: 64 * 1024 * 1024,
      maxSessionCacheBytes: 8 * 1024 * 1024,
      maxCachedSessions: 20,
      evictAfterIdleMs: 10 * 60_000,
      suspendAfterIdleMs: 10 * 60_000,
      resumeOnPrompt: true,
    },
    history: { maxPageEvents: 200, maxPageBytes: 1024 * 1024 },
    streams: { maxQueueEvents: 4096, maxQueueBytes: 2 * 1024 * 1024 },
  },
});

// Exempt a session from automatic eviction and suspension.
await kit.settings.configure({
  target: { sessionId: session.id },
  patch: { memory: { pinned: true } },
});
console.log(await kit.settings.stats());
```

You can also explicitly call `sessions.evict(id)` or `sessions.suspend(id)`.
Saved pages remain readable; with `resumeOnPrompt`, the next prompt restores a
suspended session. Active turns, pending approvals and unsaved data are protected,
and statistics report deferred cleanup. Cache/queue byte limits measure encoded
event data, not exact process RSS or the agent's own memory. Use `eventsPage()`
to bound your application's history loading too; `events()` collects its result
in application memory. See [resource policy](core/docs/configuration.md#resource-policy).

## Approvals and application tools

The kit turns runtime-specific approval messages into one request format. Your
application chooses the approval UI and returns a supported outcome. Register
that handling before a prompt can request permission. A request's `outcomes`
restricts the available decisions; an omitted or empty list allows the common
`allow`, `allow_session`, `deny` and `abort` outcomes:

```ts
import type { ApprovalRequest, Outcome } from "@tomanagle/harness-kit";

async function promptWithApprovals(
  text: string,
  choose: (request: ApprovalRequest) => Promise<Outcome>,
) {
  for await (const event of session.prompt(text)) {
    if (event.type === "approval.requested") {
      // choose() is your UI; offer the choices in event.data.outcomes.
      const decision = await choose(event.data);
      await kit.approvals.respond(event.data.approvalId, decision);
    } else if (event.type === "message.delta") {
      process.stdout.write(event.data.text);
    }
  }
}

// Rebuild an approval panel after a UI reload.
const pending = await kit.approvals.list(session.id);
console.log(pending);
```

Register your own functions as tools before creating the sessions that use them.
Handlers execute in your application, and the kit handles argument validation,
result conversion, approval routing, timeouts and cancellation propagation.

```ts
const projects = new Map([["demo", { name: "Demo project", status: "active" }]]);
await kit.tools.register({
  name: "lookup_project",
  description: "Read project details from this application",
  inputSchema: {
    type: "object",
    properties: { id: { type: "string" } },
    required: ["id"],
  },
  readOnly: true,
  approval: "ask",
  timeoutMs: 10_000,
  handler: ({ id }: { id: string }, context) => {
    context.signal.throwIfAborted();
    return projects.get(id) ?? { error: "Project not found" };
  },
});

const withTools = await kit.sessions.create({
  settings: {
    session: { runtime: "codex", cwd: process.cwd(), hostTools: ["lookup_project"] },
  },
});
```

The MCP bridge makes those tools available to compatible runtimes without a
separate implementation for each CLI. See [host tools](core/docs/tools.md).

## MCP servers and additional agents

Pass MCP definitions in the same settings object. The adapter translates them
for the selected runtime; your app can control native-server inheritance and
whether a server's tools ask for approval.

```ts
const withMcp = await kit.sessions.create({
  settings: {
    session: {
      runtime: "claude",
      cwd: process.cwd(),
      mcp: {
        inheritNative: false,
        mcpServers: {
          docs: {
            type: "http",
            url: "https://mcp.example.com/mcp", // Your MCP server.
            headers: { Authorization: "Bearer ${DOCS_TOKEN}" },
            approval: "ask",
          },
        },
      },
    },
  },
});
```

Environment references are resolved when the runtime starts. Optional kit
user/project config loading is enabled through `process.kitMcp`; project configs
use the CLI's trust flow. See [MCP configuration](core/docs/mcp.md).

For an agent that speaks ACP, configure its command at sidecar startup and then
select it like any other runtime:

```ts
import type { SettingsPatch } from "@tomanagle/harness-kit";

const acpSettings = {
  process: {
    runtimes: ["my-agent"],
    acp: {
      "my-agent": {
        command: "/absolute/path/to/your-agent",
        args: ["--acp"], // Use this agent's documented ACP arguments.
      },
    },
  },
} satisfies SettingsPatch;
// Pass acpSettings to stdio({ command: sidecarPath, settings: acpSettings }).
// Then create a session with settings.session.runtime: "my-agent".
```

ACP capabilities are negotiated with the installed agent. The kit does not
install or authenticate it automatically. See [ACP support](core/docs/acp.md).

## Usage and budgets

The kit reconciles usage snapshots and persists accounting across restore. Set
session-wide and per-turn thresholds through the same settings API. This example
uses Claude, which reports both token usage and estimated USD cost:

```ts
const budgeted = await kit.sessions.create({
  settings: {
    session: { runtime: "claude", cwd: process.cwd() },
    budget: {
      maxSessionTokens: 100_000,
      maxSessionCostUsd: 5,
      maxTurnTokens: 20_000,
      maxTurnCostUsd: 1,
    },
  },
});

// Raise an existing session's limit without restarting its runtime.
await kit.settings.configure({
  target: { sessionId: budgeted.id },
  patch: { budget: { maxSessionCostUsd: 10 } },
});
console.log((await kit.sessions.get(budgeted.id)).usage);
```

Limits reject new work when exhausted and request cancellation when reported
usage reaches a threshold. They are **observed stopping thresholds**, not hard
billing caps: delayed usage reporting can overshoot. Unsupported metrics are
rejected, and unknown cost stays unknown. Codex does not report USD cost; ACP
context occupancy is not consumed-token usage. See [budgets](core/docs/budgets.md).

## Context windows and compaction

Context usage has one shape in live events, saved history and session reads.
Unknown token counts are `null`; zero is a real observation. The `basis` tells
you whether the runtime reported occupancy directly or the kit used the latest
model request's token count. `stale` means that observation is no longer current.
The kit never substitutes cumulative billed tokens for context-window usage.

```ts
import type { ContextUsageData } from "@tomanagle/harness-kit";

function showContext(usage: ContextUsageData) {
  const used = usage.usedTokens ?? "unknown";
  const capacity = usage.windowTokens ?? "unknown";
  console.log(`${used} / ${capacity} tokens`, usage.basis, { stale: usage.stale });
}

// Cached read: no runtime startup or model request.
showContext((await session.context()).usage);

const unsubscribe = session.onEvent((event) => {
  if (event.type === "context.usage") showContext(event.data);
  if (event.type === "context.compaction") {
    console.log(event.data.status, event.data.reason, event.data.error);
  }
});

// After the preceding turn finishes, offer an explicit Compact action.
const context = await session.context();
if (context.canCompact) {
  const abort = new AbortController(); // Connect your Cancel action to abort.abort().
  showContext((await session.compact({ signal: abort.signal })).usage);
}
unsubscribe();
```

`compact()` waits for the runtime to finish; it rejects busy or unsupported
sessions. Restore a suspended or stored session explicitly before compacting.
Compaction can make model requests and incur usage. The full saved transcript
remains available after the runtime summarizes its context.

Optional automatic thresholds use the same settings object as everything else:

```ts
await kit.settings.configure({
  target: { sessionId: session.id },
  patch: { compaction: { autoAtPercent: 80, autoAtTokens: null, timeoutMs: 120_000 } },
});

// Override for one turn; changes apply before that prompt reaches the runtime.
await session.prompt("Continue", {
  settings: { compaction: { autoAtPercent: 90 } },
}).done;
```

Either configured threshold triggers compaction before the next turn's prompt,
using the latest non-stale observation. `null` disables that kit threshold;
both are disabled by default. These settings do not disable the runtime's own
automatic compaction. Adapter capabilities describe potential support;
`context.canCompact` describes current session availability. See
[context usage and compaction](core/docs/context.md) for runtime differences,
replay semantics and cancellation.

## Git worktrees, turn diffs and source control

Give a session an isolated checkout and capture what changed between the start
and end of each turn:

```ts
const isolated = await kit.sessions.create({
  settings: {
    session: { runtime: "codex", cwd: "/absolute/path/to/repository" },
    workspace: {
      mode: "worktree",
      root: "/absolute/path/to/agent-worktrees",
      base: "HEAD",
      captureDiff: true,
    },
  },
});
const turn = isolated.prompt("Add tests for the parser");
await turn.done;
const diff = await kit.sessions.diff(isolated.id, turn.id, { maxBytes: 65536 });
console.log(diff.workingTree, diff.index, diff.complete, diff.truncated);
```

These are boundary observations, including concurrent external edits, rather
than proof of which actor changed each file. Saved diffs can be read without
restarting the runtime. Worktrees are never removed automatically: close or
suspend the runtime, then explicitly call `sessions.removeWorkspace(id)` when
it is clean and its commits are retained. See [worktrees](core/docs/workspaces.md).

The optional Git API also works with ordinary sessions in existing repositories:

```ts
if (kit.server.capabilities.git) {
  const status = await kit.git.status(session.id);
  const working = await kit.git.diff(session.id, { path: "src/parser.ts" });
  console.log(status.branch, status.files, working.patch);

  // Invoke these in response to your application's explicit user actions.
  await kit.git.checkout(session.id, { branch: "fix/parser", create: true });
  await kit.git.stage(session.id, ["src/parser.ts"]);
  const commit = await kit.git.commit(session.id, "Fix parser edge case");
  console.log(commit.commit);
  await kit.git.push(session.id, {
    remote: "origin",
    branch: "fix/parser",
    setUpstream: true,
  });
}

await kit.settings.configure({
  patch: { git: { readOnly: true, allowRemote: false, maxOutputBytes: 262144 } },
});
```

Unstage, branch listing, history, fetch and fast-forward pull use the same API.
Git policy updates apply to the next operation without a restart. The kit bounds
output, validates repository identity and coordinates writes with agent turns;
your harness owns branch naming, commit timing, approval UI and forge/PR workflows.
See [Git integration](core/docs/git.md).

## Embedding, clients and the direct-model track

Use the TypeScript client from Node or Electron's main process with `stdio()`.
For renderer/browser code or Wails, supply a transport over your host's message
bridge; the main client has no Node imports or runtime dependencies. The desktop
examples demonstrate those boundaries. Keep process spawning in the native host.

Go applications can embed the core manager directly or use the standalone
[Go client](packages/client-go/README.md). Both clients' wire types and RPC methods
are generated from one [protocol schema](protocol/README.md). Typed helpers sit
above those methods, and `kit.protocol` exposes their exact request/result shapes.
The [dependency-injection guide](core/docs/dependencies.md) describes the core's
replaceable runtime, storage, tools, logging, Git, and model boundaries.

For applications that own their inference loop, the Go-only
[`model.Provider` and `model/contextwindow`](core/docs/models.md) packages provide
streamed model calls, history, token estimation, compaction and snapshots.
OpenAI and Anthropic SDK adapters live in separate modules so the core stays
dependency-free. This track leaves agent orchestration to your application;
there is no TypeScript direct-model API in the sidecar today.

## Examples and repository layout

```sh
make example node RUNTIME=fake PROMPT="Hello"
make example tools RUNTIME=fake
make example electron
make example wails                    # HARNESS_KIT_MODE=direct embeds Go in-process
```

Electron and Wails share a dashboard with session history, streaming chat,
approvals, tools, saved turn diffs and source control. The examples are reference
applications you can adapt to your own product.

```text
core/                Go SDK, adapters, CLI/sidecar, optional providers and docs
  internal/          Private accounting, process supervision and RPC plumbing
packages/client-ts/  @tomanagle/harness-kit
packages/client-go/  Standalone Go sidecar client
protocol/            Generated schema and generation guide
examples/            CLI, model, Node, Electron, Wails and shared dashboard
server/              Future cross-device coordination server
```

See the [SDK guide](core/README.md), [package boundaries and layout rationale](core/docs/packages.md)
and [examples](examples/README.md) for details. The root `Makefile` and `hk` forward
to `core/`; for example, `./hk prompt --runtime fake "Hello"` runs the local CLI.

Repository tooling requires [Bun 1.4.4 or newer](https://bun.com/docs/installation)
and Go. Run `bun upgrade` if an existing Bun installation predates the native
formatter. CI uses Bun 1.4.4 and the pinned dependencies in `bun.lock`.

```sh
make setup             # Install repository tooling and the Lefthook pre-commit hook
make format            # Format Go and web/config/docs files throughout the repo
make fmt               # Alias for make format
make format-check      # Check formatting without writing files
make lint              # Formatting checks, Go vet and JavaScript/TypeScript lint
make hooks             # Reinstall Git hooks if needed
bun lint               # JavaScript/TypeScript lint only (Oxlint)
make test              # Offline core, clients, providers and Go examples
make generate          # Regenerate schema and both client contracts
make check-generated   # Detect stale generated files
make lint wasm-check cross
```

`make format` uses `gofmt` for Go and Bun's native `bun fmt` for TypeScript,
JavaScript, JSON, CSS, HTML, YAML, TOML and Markdown. `bun lint` runs Oxlint;
the released Bun version does not yet include a native linter. Generated
protocol files keep their generator's formatting, and lockfiles are excluded.

Git has no staging hook, so Lefthook formats staged files immediately before
a commit and restages only those files, then lints staged JavaScript/TypeScript.
It temporarily hides and restores unstaged hunks in partially staged files.
Unrelated changes remain outside the commit. If restoring a hunk conflicts
with formatting, the commit stops so you can review the changes.

The optional OpenAI provider and direct-model example require Go 1.26+.
Current core imports use `github.com/tomanagle/harness-kit/core`; the new module
paths have not been tagged yet. Historical root-module `v0.2.0` tags and npm
`@tomanagle/harness-kit@0.2.0` remain available. See
[release and migration details](core/docs/releases.md).

[MIT license](LICENSE).
