// Package harnesskit is the root of Harness Kit: headless, composable
// primitives for building AI agent harnesses.
//
// Harness Kit is a set of independent packages rather than a framework:
//
//   - agent: the AgentRuntime abstraction (Runtime, Session, Registry) for
//     delegating to existing harnesses such as Codex and Claude Code
//   - session: session lifecycle, turns, event logs, approvals
//   - event: the runtime-neutral event model
//   - permission: approval requests, policies and the broker
//   - process: native process host (binary lookup, process trees)
//   - rpc/server, rpc/client, rpc/protocol: the JSON-RPC sidecar protocol
//   - runtimes/codex, runtimes/claude, runtimes/fake: runtime adapters
//
// The cmd/harness-kit binary serves the sidecar protocol over stdio for
// Electron, Wails and other hosts; packages/client-ts is its TypeScript
// client. See README.md and docs/.
package harnesskit
