# Implementation plan

Status as of the initial MVP. ✅ done · 🔜 next · 📋 planned.

## Phase 1: Architecture ✅

- ✅ Research official Codex and Claude Code integration surfaces and usage
  policies ([runtimes.md](runtimes.md))
- ✅ Package boundaries and interfaces; `AgentRuntime` vs `ModelProvider`
  split ([architecture.md](architecture.md))
- ✅ JSON-RPC sidecar protocol v1 ([protocol.md](protocol.md))
- ✅ Native / RPC / WASM sharing one core: core packages are process-free,
  enforced by `make wasm-check`

## Phase 2: Working core ✅

- ✅ `agent`: runtime interface, registry, concurrent discovery
- ✅ `event`: typed events, per-session log with replay, non-blocking bus
  with gap markers
- ✅ `permission`: requests, decisions, rule policies, broker
- ✅ `session`: manager, lifecycle states, one turn at a time, `Stream`
  iterator, approval routing, unsafe-option guard, in-memory store
- ✅ `process`: binary lookup including GUI-app install dirs, process
  groups, interrupt then kill
- ✅ `internal/jsonrpc`: bidirectional NDJSON JSON-RPC, request
  cancellation, per-key ordering
- ✅ `rpc/server`, `rpc/client`, `cmd/harness-kit` (sidecar and CLI:
  `runtimes`, `prompt`, `chat`, `sessions`; `--rpc` runs any command
  through the sidecar)
- ✅ Session history: `session.FileStore`, restore under the same ID,
  runtime-native listing (`agent.SessionLister`; Codex `thread/list`)
- ✅ `@harness-kit/kit` (TypeScript), with automatic gap recovery
- ✅ Portable session forks, archive/unarchive, title/transcript search and metadata tags
- ✅ Host-owned configuration: startup settings, future-turn defaults,
  per-turn overrides, explicit inheritance/reset, capability discovery,
  saved configuration snapshots and Go/RPC/TypeScript/CLI parity
- ✅ `runtimes/fake`

## Phase 3: Runtime integrations ✅

- ✅ Codex via app-server: streaming, approvals, interrupt, resume, usage,
  auth status
- ✅ Claude Code via `claude -p` stream-json: streaming, resume and fork,
  SIGINT cancel, usage and cost, auth status, permission options
- ✅ Claude Code interactive approvals via `--permission-prompt-tool`,
  served by the tool bridge (verified live)
- 📋 Shared Codex app-server connection across sessions (optimisation)
- ✅ Claude image input via `--input-format stream-json`, with configurable byte caps

## Phase 4: Examples ✅

- ✅ Go CLI linking the library directly (`examples/cli`)
- ✅ Node.js sidecar client, the Electron main-process code path
  (`examples/node`)
- ✅ Electron app with a typed, mediated IPC bridge (`examples/electron`;
  syntax-checked, not launched in CI)
- ✅ Wails app: sidecar mode with the same TS client, plus direct in-process
  mode (`examples/wails`; builds, not launched in CI)

## Phase 5: Additional primitives 📋

Each is an independent package; none is required by the others.

1. **`tools`** ✅: registry, schema inference from Go types, validation,
   timeouts, concurrency limit; served to Codex and Claude Code by
   `toolhost` (MCP over a private local socket); RPC registration with
   `tool.call` for TypeScript and Go clients (verified live on both
   runtimes). Built dependency-free rather than on the MCP SDK.
   📋 Reference tools for your own loop (Go-only, `os.Root`-confined).
2. **`model`** ✅: `Provider` for raw inference (build-your-own loop), with
   API-key adapters over the official Anthropic and OpenAI Go SDKs in
   separate modules, so the core stays dependency-free.
3. **`contextwindow`** ✅: history, token budgets, pluggable compaction
   strategies, snapshots. Applies to the build-your-own track only.
4. **`session` stores**: ✅ `FileStore` (JSON files) and `Manager.Restore`;
   ✅ JSONL event transcripts, replay after restart, stable sequence numbers,
   and `session.updated` notifications for title and lifecycle changes.
   📋 SQLite store and dedicated replay/debugging tools.
5. **`mcp`**: ✅ portable per-session MCP config for every runtime (user,
   project with trust, and session layers; Codex and Claude translation;
   `harness-kit mcp` CLI; a minimal client for `mcp test`). Fixed Codex MCP
   tool approvals, which arrive as elicitations. ✅ Kit MCP servers with
   `approval: ask` on Claude Code (through the permission bridge).
   📋 Normalised MCP startup status events.
6. **`skills`**: discovery and loading.
7. **Budgets and routing**: token or cost budgets enforced from `usage`
   events; model routing for the build-your-own track.
8. **Subagents**: modelling `parentCallId` trees; Codex multi-thread
   events.
9. **ACP adapter** ✅: stable v1 agents over stdio, negotiated capabilities,
   configurable startup, per-turn settings, resume, approvals and images.
10. **WASM host shim**: a `syscall/js` entry point exposing the session
    manager, with runtimes implemented in JavaScript.
11. **Remote execution environments**: runtimes whose process lives
    elsewhere (container, VM), reusing the same adapter contract.

## Open questions for the maintainers

- Published identities: `github.com/tomanagle/harness-kit` and
  `@harness-kit/kit`. See [Releases](releases.md) for npm trusted publishing.
- Minimum supported runtime versions: currently "tested with" rather than
  enforced, apart from Claude Code's `--permission-prompts` (2.1.259 and
  later).
