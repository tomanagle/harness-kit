# Implementation plan

Implementation status for the standalone SDK in `core/` and root `packages/`.
✅ done · 🔜 next · 📋 planned. Go package paths below are relative to `core/`.
Example paths start at repository-root `examples/`; the TypeScript client lives
in root `packages/client-ts`.
The separate [server directory](../../server/README.md) is a placeholder for a
planned deployable product that coordinates sessions across devices; it has no
implementation yet. The kit is usable independently.

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
- ✅ `internal/process`: binary lookup including GUI-app install dirs, process
  groups, interrupt then kill
- ✅ `internal/jsonrpc`: bidirectional NDJSON JSON-RPC, request
  cancellation, per-key ordering
- ✅ `rpc/server`, `internal/rpcclient`, `cmd/harness-kit` (sidecar and CLI:
  `runtimes`, `prompt`, `chat`, `sessions`; `--rpc` runs any command
  through the sidecar)
- ✅ Session history: `session.FileStore`, restore under the same ID,
  runtime-native listing (`agent.SessionLister`; Codex `thread/list`)
- ✅ `@tomanagle/harness-kit` (TypeScript), with automatic gap recovery
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
3. **`model/contextwindow`** ✅: history, token budgets, pluggable compaction
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
7. **Budgets and routing**: ✅ observed token/cost budgets, durable accounting,
   live settings and per-turn limits. 📋 Model routing for the build-your-own track.
8. **Subagents**: modelling `parentCallId` trees; Codex multi-thread
   events.
9. **ACP adapter** ✅: stable v1 agents over stdio, negotiated capabilities,
   configurable startup, per-turn settings, resume, approvals and images.
10. **WASM host shim**: a `syscall/js` entry point exposing the session
    manager, with runtimes implemented in JavaScript.
11. **Remote execution environments**: runtimes whose process lives
    elsewhere (container, VM), reusing the same adapter contract.
12. **Worktrees and turn diffs** ✅: optional Git worktrees, before/after snapshots,
    bounded durable diffs, explicit safe cleanup and a reference dashboard view.
13. **Git operations** ✅: optional status, diffs, branches, history, staging,
    commits and remote operations, shared runtime settings, generated clients
    and desktop examples. Harnesses own their workflow and forge integrations.

## Release tooling

- ✅ Canonical GitHub/Go identity, initial commits and tagged platform binaries.
- ✅ npm publishing workflow with bootstrap token and GitHub OIDC support.
- ✅ First npm publish: [`@tomanagle/harness-kit@0.2.0`](https://www.npmjs.com/package/@tomanagle/harness-kit/v/0.2.0)
  is public. A clean registry install was verified against the released v0.2.0
  sidecar, including live settings and transcript replay after restart.
  See [Releases](releases.md) for subsequent publishing.

## Identities and compatibility

- Current source identities: Go `github.com/tomanagle/harness-kit/core` and
  npm `@tomanagle/harness-kit`. The new Go paths are not tagged yet; use a source
  checkout. Future tags use `core/v…` for core and `core/providers/{name}/v…` for
  provider modules. npm tags remain `npm-v…`.
- Historical root-module `v0.2.0` and npm `@tomanagle/harness-kit@0.2.0` remain
  available. This directory move does not publish a release. See
  [Releases](releases.md) for migration and npm trusted publishing.
- Minimum supported runtime versions: currently "tested with" rather than
  enforced, apart from Claude Code's `--permission-prompts` (2.1.259 and
  later).
