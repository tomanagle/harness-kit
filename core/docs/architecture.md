# Architecture

This document reviews the original proposal, then describes the
architecture as built: package boundaries, public interfaces, how the
native, RPC and WebAssembly hosts share one core, and the trade-offs behind
each decision. The protocol is specified in [protocol.md](protocol.md);
runtime integrations and their usage policies are in
[runtimes.md](runtimes.md).

This document covers the standalone SDK: Go packages and local runtime support
in `core/`, with clients in root `packages/`. Source paths below are relative to
`core/` unless stated otherwise; runnable applications live in root `examples/`,
with independent Go modules for CLI, model and Wails examples. The core module is
`github.com/tomanagle/harness-kit/core`.
The existing local stdio sidecar belongs to this SDK. The separate
[`server/`](../../server/README.md) directory is a placeholder for future
cross-device session coordination, with no implementation or dependency on it
required by kit applications.

## 1. Review of the proposal

The proposal is sound in its central idea: headless, composable primitives
that sit underneath many different harnesses, with existing harnesses
(Codex, Claude Code) treated as **runtimes** rather than model providers.
These are the issues that changed the design.

### 1.1 Two tracks, not seven peers

The proposal lists runtime registry, tools, context manager, sessions,
permissions and events as equal primitives. They are not equally
applicable:

| Primitive | Delegated runtime (Codex, Claude Code) | Your own agent loop |
| --- | --- | --- |
| Sessions, events, permissions | yes | yes |
| Runtime registry | yes | n/a |
| Tool registry | only via MCP (see 1.2) | yes |
| Context manager | **no**: the runtime owns its context window | yes |
| ModelProvider | **no**: the runtime owns model access | yes |

A context manager cannot compact a Codex conversation, and a tool registry
cannot inject a Go function into Claude Code's tool loop directly. Harness
Kit therefore has a **delegated track** (`agent.Runtime`) and a
**build-your-own track** (`model.Provider` + tools + `contextwindow`).
Sessions, events and permissions are shared by both. This is the
`AgentRuntime` / `ModelProvider` split the proposal asks for, followed
through to the other primitives.

### 1.2 Host tools reach delegated runtimes through MCP

Both Codex and Claude Code consume MCP servers. The interoperable way to
give a delegated runtime your tools is to serve the tool registry as an MCP
server, not to invent a Harness Kit tool protocol. The `tools` registry is
served by `toolhost`, using a dependency-free MCP implementation. The same mechanism is the
documented path for Claude Code approvals (`--permission-prompt-tool`).

### 1.3 Approvals are asymmetric across runtimes

The Codex app-server sends approvals as JSON-RPC requests on the same
connection, and Harness Kit bridges them fully. Claude Code exposes
interactive approvals to non-SDK hosts only through an MCP permission-prompt
tool. The Python and TypeScript Agent SDKs use an internal control channel
that is not a documented public protocol. Harness Kit implements interactive
Claude approvals through the MCP permission-prompt bridge. Without a tool host,
anything that would prompt is denied and reported. Each runtime advertises
`capabilities.approvals` so hosts can adapt.

### 1.4 Subscription authentication is a policy question

Both providers allow a user's own, locally signed-in CLI to be driven
programmatically. Both restrict products that build on someone else's
subscription. The details are in
[runtimes.md](runtimes.md#authentication-and-usage-policies). Harness Kit
therefore never reads credential stores, never offers a login flow, and
reports sign-in state only through each CLI's documented status interface.
It also surfaces the policy notes in `runtime.discover` so host apps can
show them.

### 1.5 Naming collisions in the suggested layout

- `core/context` would shadow the standard library's `context` in every
  file that needs both.
- A package named `runtime` collides with Go's `runtime`. It also collides
  with Wails' `github.com/wailsapp/wails/v2/pkg/runtime`, which almost every
  Wails app imports.
- The `core/` prefix adds nothing: Go import paths are already namespaced.

The `AgentRuntime` abstraction therefore lives in package **`agent`**
(`agent.Runtime`, `agent.Registry`). The future `ModelProvider` lives in
**`model`** (`model.Provider`). Within `core/`, the package layout is flat.

### 1.6 What WebAssembly can and cannot do

Browsers and JavaScript hosts cannot spawn processes, so the Codex and
Claude adapters can never run in WebAssembly. What *can* be portable is
everything else: events, permissions, sessions, the protocol server, and
runtimes whose transport is supplied by the JS host. The design enforces
this today. The core packages never import `os/exec` or `process`, and
`make wasm-check` builds them for `wasip1` and `js/wasm` in CI.

### 1.7 Streaming over JSON-RPC

JSON-RPC has no streaming responses. `session.prompt` therefore returns as
soon as the turn starts, and the turn's events arrive as notifications
carrying a per-session sequence number. The client chooses the turn ID, so
it can correlate events that arrive before the response. Backpressure,
ordering, replay and cancellation are specified in
[protocol.md](protocol.md).

### 1.8 Smaller corrections

- `createHarnessKit` cannot be synchronous, because spawning the sidecar
  and handshaking is asynchronous. It is `await createHarnessKit({ transport })`.
  Process spawning lives in a separate `@tomanagle/harness-kit/node` entry
  point, so the core client stays usable in browsers, Electron renderers
  and Wails frontends.
- macOS apps launched from Finder or the Dock do not inherit the user's
  shell `PATH`. Without explicit handling, `codex` and `claude` are "not
  installed" inside Electron and Wails apps. Discovery checks environment
  overrides, `PATH`, then well-known install directories.
- For Wails, linking Harness Kit in-process is simpler and faster than a
  sidecar. Both are supported; the sidecar mode reuses the same TypeScript
  client as Electron.
- Concurrent request handling reorders "prompt, then cancel" from one
  client. Requests that change a session's state are serialised per
  session; reads, discovery and approvals stay concurrent.

### 1.9 Prior art and reuse

| Project | Decision |
| --- | --- |
| [Agent Client Protocol](https://agentclientprotocol.com) (ACP, JSON-RPC over stdio, editor ↔ agent) | Planned as a **generic runtime adapter**, so any ACP agent becomes a Harness Kit runtime. Not adopted as the sidecar protocol: ACP's client serves the agent (filesystem, terminal), and it has no runtime discovery, registry or event replay. |
| Codex TypeScript SDK, Claude Agent SDK (TS/Python) | Not usable from Go. Harness Kit drives the documented interfaces underneath them: the Codex app-server and `claude -p` stream-json. Anthropic's Agent SDK docs name the CLI as the route for other languages. |
| Official MCP Go SDK | To be used for MCP client support and for serving tools to runtimes. |
| `google/jsonschema-go` | Candidate for tool schema inference and validation. |
| `sourcegraph/jsonrpc2`, `vscode-jsonrpc` | Not used. NDJSON JSON-RPC is about 400 lines, keeps the core dependency-free, and matches the framing both runtimes use. `vscode-jsonrpc` uses LSP `Content-Length` framing. |
| Terminal scrapers (PTY wrappers around interactive CLIs), reading `~/.codex/auth.json` or the Claude keychain, spoofing a CLI's client identity | Rejected: unofficial and fragile, and in the credential cases a policy violation. |

## 2. Architecture

```text
 Electron (TS)    Wails frontend (TS)      Go CLI / Wails backend / services
      │                  │                              │
 @tomanagle/harness-kit ──┘ (same client, any Transport)   │ (import packages directly)
      │ JSON-RPC 2.0, NDJSON over stdio                 │
┌─────▼────────────────────────────────────────┐        │
│ cmd/harness-kit  (sidecar binary)            │        │
│ rpc/server ── rpc/protocol ── internal/jsonrpc        │
└─────┬────────────────────────────────────────┘        │
      │                                                  │
┌─────▼──────────────────────────────────────────────────▼─────┐
│ session   lifecycle · one turn at a time · event log · store  │
│ permission  requests · policies · broker                      │  portable core
│ event       envelope · typed payloads · log · bus             │  (builds for WASM)
│ agent       Runtime interface · Registry · Discovery          │
└─────┬─────────────────────────────────────────────────────────┘
      │ agent.Runtime implementations
┌─────▼──────────────┬──────────────────────┬──────────────────┐
│ runtimes/codex     │ runtimes/claude      │ runtimes/fake    │
│ codex app-server   │ claude -p stream-json│ deterministic    │
└─────┬──────────────┴──────────┬───────────┴──────────────────┘
      │ process (native only: LookPath, process groups, interrupt/kill)
```

Dependencies point downward only. `rpc/server` knows nothing about Codex,
and `session` knows nothing about JSON-RPC. The module has no third-party
dependencies.

### 2.1 Packages

| Package | Role |
| --- | --- |
| `config` | explicit settings, patches, inheritance and setting-lifetime capabilities |
| `agent` | `Runtime`, `Session`, `Handler` interfaces; `Registry`; `Discovery`; `DecodeOptions`; the `Guarded` safety hook |
| `event` | `Event` envelope, typed payloads, per-session `Log` (sequence numbers, replay), non-blocking `Bus` with gap markers |
| `permission` | `Request`/`Decision` vocabulary, `Policy` (`Rules`, `Chain`), `Broker` that parks requests until resolved |
| `session` | `Manager`: create/prompt/cancel/close, `Stream` iterator, approval routing, `Store` (`MemoryStore`, `FileStore`), `ToolServer` hook |
| `mcp` | Portable MCP server config: layers, merge, `${VAR}` expansion, project trust ([mcp.md](mcp.md)) |
| `tools` | Host tool registry: schemas, validation, execution ([tools.md](tools.md)) |
| `toolhost` | Serves the registry (and Claude Code permission prompts) to runtimes as MCP over a private Unix socket |
| `process` | Native-only: binary lookup incl. GUI-app install dirs, process groups, SIGINT then kill, `Tail` |
| `rpc/protocol` | Wire types, method names, error codes, protocol version |
| `rpc/server` | Serves a `session.Manager` over any `io.Reader`/`io.Writer` |
| `rpc/client` | Compatibility Go client using core-domain types |
| `runtimes/codex`, `runtimes/claude`, `runtimes/acp`, `runtimes/fake` | Adapters |
| `model`, `contextwindow` | Portable raw inference contract and caller-owned context management |
| `budget` | Observed token/cost accounting with durable checkpoints and explicit limits |
| `workspace`, `workspace/git` | Portable workspace contract and optional native Git worktree/diff backend |
| `providers/openai`, `providers/anthropic` | Separate modules using official provider SDKs |
| `cmd/harness-kit` | The sidecar binary |
| `../packages/client-ts` (repository-root clients) | `@tomanagle/harness-kit` (browser-safe) and `@tomanagle/harness-kit/node` (stdio transport) |
| `../packages/client-go` | Standalone Go sidecar client; generated wire types and methods, standard-library transport |
| `cmd/protocolgen`, `../protocol` | Extract the server contract into JSON Schema and generate both client contracts; CI rejects stale output |

The standalone clients use the same schema for requests, results, events,
settings and error constants. The generator reads the Go server's wire/domain
types and method registry, writes `protocol/schema.json`, then renders both
clients from it. This avoids maintaining a second TypeScript model by hand.
Generated files are committed so consumers do not need the generator or a core
checkout. The `rpc/client` package is retained for compatibility with existing
Go applications; new clients can import the independent module under
`packages/client-go`. See [protocol generation](../../protocol/README.md).

### 2.2 Public interfaces

The runtime contract (`agent`):

```go
type Runtime interface {
    Info() Info                                        // static: name, capabilities
    Discover(ctx context.Context) (Discovery, error)  // installed? version? signed in? No quota used.
    Start(ctx context.Context, cfg SessionConfig, h Handler) (Session, error)
}

type Session interface {
    RuntimeSessionID() string                                       // resume token (thread ID, session ID)
    Prompt(ctx context.Context, turn Turn) (TurnResult, error)      // blocks; cancel ctx to interrupt
    Close(ctx context.Context) error
}

type Handler interface {                                            // implemented by the host (session.Manager)
    Emit(ev event.Event)
    RequestApproval(ctx context.Context, req permission.Request) (permission.Decision, error)
}
```

Cancellation is ordinary `context.Context` cancellation. An adapter turns a
cancelled `Prompt` context into the runtime's graceful interrupt
(`turn/interrupt` for Codex, SIGINT for Claude Code). It falls back to
killing the process tree.

Using the library directly:

```go
registry := agent.NewRegistry(codex.New(), claude.New())
discovered, err := registry.Discover(ctx)

mgr := session.NewManager(registry, session.Options{
    Policy: permission.Rules{{Kind: permission.KindCommand, Command: "git status*", Action: permission.ActionAllow}},
})
info, err := mgr.Create(ctx, session.CreateParams{Runtime: "codex", Config: agent.SessionConfig{Cwd: dir}})

for ev, err := range mgr.Stream(ctx, info.ID, session.PromptParams{Input: agent.Text("Explain this codebase")}) {
    // ev.Data is a typed payload: event.MessageDeltaData, event.ToolStartedData, ...
}
```

Approvals arrive as `approval.requested` events and are answered with
`mgr.RespondApproval(id, permission.Decision{Outcome: permission.Allow})`.
A `Policy` decides requests before any host is asked.

The build-your-own-loop contract (`model`):

```go
// Provider is raw model inference. It never runs tools; the caller's loop does.
type Provider interface {
    Info() Info
    Stream(ctx context.Context, req Request) iter.Seq2[Event, error]
}
```

The API-key adapters use the official Anthropic and OpenAI Go SDKs in separate
modules. The core stays dependency-free. Applications own tool execution,
approvals and context policy; see [Models and context](models.md).

### 2.3 One core, three hosts

| Host | How it reaches the core | Status |
| --- | --- | --- |
| Go library (CLI, Wails backend, services) | imports `session`, `agent`, adapters | implemented |
| Sidecar (Electron, Wails, anything with stdio) | `cmd/harness-kit` = `rpc/server` around the same `session.Manager` | implemented |
| WebAssembly | the core packages compiled to WASM; runtimes provided by the JS host | core builds for `wasip1` and `js/wasm`; host shim not yet written |

The RPC server translates transport requests to core methods. Every behaviour
(ordering, approvals, replay, safety checks) lives in the core, so every
host gets the same semantics.

## 3. Trade-offs

**One Codex app-server process per session.** The app-server can host many
threads on one connection. A process per session isolates failures and
makes cleanup trivial, at the cost of some memory per session. A shared,
reference-counted connection is a later optimisation.

**One `claude -p` process per Claude turn.** A long-lived
`--input-format stream-json` process would save startup time per turn
(about 1–3 s, more with many plugins). Per-turn processes use only the most
documented surface: `--session-id`, `--resume`, SIGINT to end a turn.
Nothing can leak between turns, and a crash fails one turn, not the
session.

**NDJSON over LSP framing.** NDJSON is simpler in every language, matches
both runtimes, and is safe because JSON escapes newlines. The cost: one
message per line, with a configurable maximum line size.

**Drop and replay instead of blocking backpressure.** A slow client never
stalls a runtime, and memory stays bounded. Loss is always signalled
(`stream.gap`) and recoverable through bounded history pages. The TypeScript
client's high-level `Turn` handles replay automatically. Go callback consumers
handle the marker and request pages explicitly with `SessionEventsPage`; the
client exposes the protocol without imposing an event-processing loop.

**Runtime-specific options instead of a lowest common denominator.**
Codex sandboxes and Claude Code permission modes are not the same concept.
Only universally meaningful fields (`cwd`, `model`, `resumeId`,
`instructions`) are generic. Everything else is a runtime's typed
`Options`, and unknown fields are rejected so typos fail loudly.

**Normalised events plus opt-in raw passthrough.** Normalisation lets one UI
work across runtimes. `rawEvents: true` adds `runtime.raw` events for
anything unmapped, so nothing is lost.

**Unsafe configurations refused by default.** Options that disable
sandboxes or permission checks (`danger-full-access`, `bypassPermissions`,
and Codex config overrides touching them) are rejected unless the host opts
in (`session.Options.AllowUnsafe`, `--allow-unsafe`). This is safe by
default for sidecars that take requests from a UI.

**Replaceable session stores.** `session.Store` is a four-method interface;
the optional `session.EventStore` extension adds append/read for ordered
events. The library defaults to memory with a bounded replay log. The CLI
uses `FileStore`: atomic JSON records plus synced JSONL transcripts, with
one active writer per session. Events are saved before publication, and
sequence numbers survive restore. Custom record-only stores keep working.
Runtimes still own the conversation used for native resume.

**Own JSON-RPC implementation.** About 400 lines and a test suite, in
exchange for zero dependencies, full control over ordering keys and
cancellation, and one implementation shared by the server and the Codex
client.


## Configuration lifetimes

The dependency-free `config` package defines `Settings`, `Patch` and per-setting
capability metadata. It performs value inheritance without reading external
sources. `agent.Turn` carries a snapshot; session updates affect only future
snapshots. The manager preserves creation settings for reset, validates updates
through adapter and host hooks, and records session/turn selections in events.

Hosts choose configuration sources through explicit values and optional
`Prepare`, `PrepareRestore`, and `ValidateConfig` callbacks. Runtime-specific
settings remain available through typed adapters and JSON options. The Go, RPC,
TypeScript and CLI surfaces share the same lifecycle and error semantics.
See [Configuration](configuration.md) for inheritance, reset and native defaults.
