# Configuration owned by the host

Harness Kit accepts configuration values; it does not require a settings UI,
profile system, environment-variable scheme, or configuration file format.
A host can source values from code, files, a database, CLI flags, or a user
interface. The optional CLI/MCP configuration loaders remain host conveniences.

## One settings document

The `settings` package is the shared configuration contract for Go hosts, the
sidecar and clients. The document has `process`, `logging`, `approvals`,
`memory`, `history`, `streams`, and `session` sections. The same validated
document initializes the sidecar and supplies runtime patches. The kit does
not write application preferences to a second settings store.

```json
{
  "process": { "store": "./sessions", "runtimes": ["codex", "claude"] },
  "logging": { "level": "info" },
  "approvals": { "timeoutMs": 0 },
  "memory": {
    "maxCacheBytes": 67108864,
    "maxSessionCacheBytes": 8388608,
    "maxCachedSessions": 20,
    "evictAfterIdleMs": 600000,
    "suspendAfterIdleMs": 600000,
    "resumeOnPrompt": true
  },
  "history": { "maxPageEvents": 200, "maxPageBytes": 1048576 },
  "streams": { "maxQueueEvents": 4096, "maxQueueBytes": 2097152 },
  "session": { "model": "", "effort": "" }
}
```

These are example policy values. Automatic eviction and suspension are off
unless the host supplies a timeout or cache budget. `process.store` selects
durable storage; the sidecar otherwise uses an in-memory store.

```sh
harness-kit serve --settings ./settings.json
```

Explicit command-line flags override file values. Files are read at startup;
changing a file does not silently reconfigure a running process. A settings UI
uses `settings.configure` to apply a change and saves accepted preferences in
its own storage for the next launch.

`settings.get` returns `values`, `revision`, and field metadata. Each field
describes its unit, valid minimum, nullability, scope and application lifetime.
`settings.configure` accepts `{patch, target?, reset?, expectedRevision?}` and
returns the new snapshot. `settings.updated` broadcasts changes to connected
clients separately from session transcripts. The Go manager exposes
`Settings`, `ConfigureSettings`, and `InitializeSettings` using the same types.

A global patch to `session` changes defaults for newly created sessions.
Supply `target: {sessionId}` to configure an existing session's future turns.
Session creation and prompting also accept a `settings` envelope, restricted
to fields supported at that lifetime. Existing `session.configure` and prompt
`config` remain available for compatibility.

Patches preserve omitted fields. An explicit `false`, zero, empty string or
empty list is a value. `null` disables nullable limits/timeouts; zero cache
bytes retains no cached events. Page byte/count limits must be positive, and
zero approval timeout waits indefinitely. `reset` lists complete dotted field
paths, such as `memory.maxCacheBytes` or `session.model`, to inherit the
appropriate parent again. Native option keys are patched individually, and
each supplied option value replaces the complete prior value.

Revision checks prevent stale settings windows from overwriting newer edits.
Subscribe to changes before fetching a snapshot, and ignore snapshots with
older revisions. A `resync` notification means the consumer fell behind;
refresh each displayed target with `settings.get`. Session settings revisions
change only with settings or pinning, so streaming tokens do not invalidate
an edit to the next turn's configuration. Invalid patches apply nothing. A mixed patch containing a
change requiring a process restart is rejected as a whole; the kit never
silently restarts itself. Reconnecting clients should read the existing
settings, not reapply startup preferences. Only the owner explicitly supplies
initial settings.

In Node/Electron, the owning transport accepts the same settings object at
spawn time, including process wiring. A connected client can then apply live
patches through the single settings method:

```ts
const kit = await createHarnessKit({
  transport: stdio({ command: "harness-kit", settings: savedSettings }),
});

const current = await kit.settings.get();
const accepted = await kit.settings.configure({
  expectedRevision: current.revision,
  patch: { memory: { evictAfterIdleMs: 300_000 } },
});
await preferences.save(accepted.values); // your application's storage

await kit.settings.configure({
  target: { sessionId },
  patch: { session: { effort: "high" }, memory: { pinned: true } },
});

// The same envelope supplies one-turn overrides without changing defaults.
for await (const event of session.prompt("Quick summary", {
  settings: { session: { effort: "low" } },
})) {
  // Consume events.
}
```

`createHarnessKit({initialSettings})` applies an explicit live patch to an
already constructed host before returning the client; it cannot change that
host's process wiring. Use the transport's `settings` option for the complete
boot document. Go `client.Spawn` similarly accepts `Options.Settings`; direct
Go hosts construct their adapters/store and call `InitializeSettings` before
creating sessions. Functions and transport objects remain host-owned wiring.

## Resource policy

Cache eviction releases cached event data. Runtime suspension releases an idle
agent's resources and preserves its saved conversation for later resume. The
two deadlines are independent, and reading saved transcript pages does not
start the agent. Idle time measures activity, not the age of the conversation.
`memory.pinned` is session-scoped and exempts that session from automatic
eviction and suspension.

Policy changes take effect without restarting the sidecar. Running turns,
pending approvals and data that could not be persisted are protected. A budget
can remain exceeded while protected resources are in use; resource statistics
report blocked cleanup. Lowering a limit does not cancel work. Resume depends
on the adapter's native saved history and resume support.

Cache and queue byte accounting measures encoded event data. It does not
promise an exact Go heap or process RSS ceiling, and excludes child-process
memory. Replaying history uses pages bounded by both events and encoded bytes;
an event too large for a page returns an explicit error without advancing the
cursor. The host can raise the page limit to retrieve it. Restoring a session
scans its history while retaining a bounded tail.

Startup wiring such as the storage backend/directory, registered runtimes,
executable paths and unsafe-mode policy requires a restart or a separately
managed migration. Session working directory, instructions and MCP/tool setup
require a new runtime session. These lifetimes are exposed in settings metadata
so applications can explain the effect of a change before applying it.

## Lifetimes

The Go host selects adapters, storage, transports, approval policies and
validation callbacks when constructing a manager. The TypeScript client connects
to that host; connecting a client does not give it control over host wiring.

At session creation, `agent.SessionConfig` / `session.create` selects the working
directory, model, effort, instructions, native options and tool/MCP setup. The
manager snapshots these values as the creation configuration. Mutating the
caller's original maps afterward does not change the session.

`Manager.Configure` / `session.configure` changes defaults for subsequent turns.
It works while a turn runs and on stored sessions without launching their runtime.
The active turn retains its accepted configuration. A per-prompt `config` patch
changes that prompt only. Neither operation restarts, cancels or steers an active
turn. Current adapters do not advertise live configuration changes; cancellation
continues to use the separate cancel API.

Only settings advertised for the requested lifetime can be changed. Working
directory, instructions, MCP setup and permission/tool context currently require
a new runtime session. Hosts decide when to create or restore one.

## Inheritance and patches

Configuration has three layers: creation settings, current session defaults, and
one turn's overrides. `config.Settings` is a full selection; `config.Patch` is an
explicit patch. Go aliases are `agent.TurnConfig` and `agent.ConfigPatch`.

- An omitted field leaves the parent selection unchanged.
- A supplied `model` or `effort` replaces it. An empty string requests native
  default selection, resolved by the adapter when possible.
- `options` sets individual adapter-native option keys. Each value replaces the
  whole previous value, including objects and arrays. There is no recursive merge.
  `false`, `0`, `[]`, `{}` and native `null` remain explicit JSON values; the
  adapter validates whether they are meaningful for that option.
- `reset: ["model", "effort", "options.maxTurns"]` inherits those fields again.
  For `session.configure`, the parent is the original creation configuration.
  For a prompt patch, the parent is the current session configuration. Resetting
  a native option absent from the parent removes its selection.
- A field cannot be both set and reset in one patch. Portable `model: null` and
  `effort: null`, null `options`/`reset`, unknown patch fields and a null patch are
  rejected. Use `reset` to inherit; use an empty string to request native defaults.

Legacy startup `options.effort` is normalized into portable `effort`. A nonempty
portable startup effort wins if both are supplied. Runtime patches use portable
`effort`; `options.effort` is not a separately configurable field.

```ts
const session = await kit.sessions.create({
  runtime: "claude",
  model: "sonnet",
  effort: "low",
});

// Changes defaults for future prompts, including after restart.
await session.configure({ effort: "high" });

// Uses medium for this prompt, then returns to the session's high default.
for await (const event of session.prompt("Review this change", {
  config: { effort: "medium", options: { maxTurns: 3 } },
})) {
  // Render events or handle approvals as your application chooses.
}

// Restore the creation-time low effort selection.
await session.configure({ reset: ["effort"] });
```

```go
high := "high"
_, err := mgr.Configure(ctx, id, agent.ConfigPatch{Effort: &high})
turn, err := mgr.Prompt(ctx, id, session.PromptParams{
    Input: agent.Text("Review this change"),
    Config: &agent.ConfigPatch{Model: &chosenModel},
})
```

## Capability discovery and errors

`runtime.list` and `runtime.discover` include
`capabilities.configuration`, a map keyed by `model`, `effort`, or native paths
such as `options.maxTurns`. Each entry reports `startup`, `session`, `turn` and
`live` booleans. A missing or false capability is not supported for that operation.
Capability entries describe the adapter surface, not every model or installed
CLI version; validation or the runtime can still reject an unavailable value.

Codex supports model and effort changes for session defaults and individual
turns. Its native options are startup-only. Claude also supports per-turn and
session defaults for `options.maxTurns`, `options.maxBudgetUsd` and
`options.disablePartialMessages`; other native settings remain startup-only.

Malformed patches and invalid adapter values return `invalid_params` over RPC
(`agent.ErrInvalidConfig` in Go). Unsupported lifetimes or unavailable native
defaults return `unsupported` (`agent.ErrUnsupported`). Host permission guards
also apply to each configuration path. Rejected turn configuration creates no
turn, increments no turn count, and consumes no caller-chosen turn ID.

## Native defaults and honest snapshots

The adapters retain native configuration unless the host supplies a selection.
In particular, Codex no longer injects `workspace-write` and `on-request` for
omitted sandbox/approval options. A host requiring those values should set them
explicitly at creation. `AllowUnsafe` guards explicit requests to disable safety;
it does not inspect or override the runtime's independently configured defaults.

Codex retains model/effort values in its native thread. The adapter resolves
native defaults, sends complete model/effort selections on each turn, and saves
its captured defaults separately for resume. Effort validation uses the model's
advertised vocabulary when available. For custom models with no reported default,
select effort explicitly. Use portable model/effort fields instead of duplicating
them inside `options.config.model` or `options.config.model_reasoning_effort`.

Claude can restore the last model from its transcript. Its adapter learns the
native model from an initial inherited turn, retains that baseline and saves it
for resume. If the first prompt explicitly overrides the model before a baseline
is known, a later return to native selection cannot be guaranteed. The same
applies to older saved sessions without a captured baseline. Such a reset fails
explicitly; the host can select a model, including Claude's explicit `default`
alias. Ordinary omitted startup settings never force that alias.

Claude's environment can take precedence over effort flags. A conflicting
`CLAUDE_CODE_EFFORT_LEVEL` produces an explicit error; the adapter does not rewrite
the host environment. These native semantics follow the official
[Claude model configuration documentation](https://code.claude.com/docs/en/model-config).

`SessionInfo.config` and `session.updated.data.config` expose session selections.
`turn.started.data.config` records the accepted turn selection, including native
resolution available before admission. Some native choices are only reported
when execution starts; a still-empty field means delegated native selection,
not an invented model/effort value. Runtime status events preserve additional
native reports. Raw option values are included in saved configuration/events;
hosts should use environment references for secrets instead of literal values.

## Host hooks and restore

`session.Options.Prepare` runs on creation and can load/merge whatever external
sources the host chooses. `PrepareRestore` is a separate optional hook for loading
settings when a stored runtime is restarted. Leaving it nil reuses saved settings.
Restoring a session that is already live does not run either hook.

A restore hook cannot switch runtime identity or the resume ID. Hook changes are
validated before startup; failed startup restores the prior saved configuration.
Changing defaults on restore does not replace the original creation parent used
by `reset`. To use a new creation baseline, create a new session explicitly.

`Options.ValidateConfig` receives an independent `ConfigRequest` at create,
restore, configure and prompt admission. It can enforce a host's model allowlist,
limits or other application rules. Prompt validation can run again after native
resolution. Treat it as a concurrent, side-effect-free validator; it must not
reenter the same manager. Permission policy callbacks can likewise consult
host-owned current state without a prescribed settings store.

Custom adapters can implement `agent.Configurable` for shared validation,
`agent.TurnPreparer` for safe native default resolution, and
`agent.DefaultsProvider` for durable native baselines. Existing runtime interfaces
remain compatible. Advertise only implemented settings and read the immutable
`agent.Turn.Config` snapshot when supporting turn configuration.
