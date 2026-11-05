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
//   - settings, config: shared settings, persistence and value inheritance
//   - hooks, logging: lifecycle extensions and structured diagnostics
//   - model, model/contextwindow: inference contracts and caller-owned history
//   - tools, toolhost, mcp, mcp/client: host tools and MCP integration
//   - workspace, workspace/git: workspace contracts and the native Git backend
//   - rpc/server, rpc/protocol: transport-neutral JSON-RPC hosting and wire types
//   - runtimes/codex, runtimes/claude, runtimes/acp, runtimes/fake: adapters
//   - runtimes/transport: injectable native process contracts
//
// Implementation-only code lives under internal, including command wiring,
// protocol dispatch, accounting, process supervision and code generation.
// The cmd/harness-kit binary serves the protocol over stdio. Independent Go
// and TypeScript clients live in the repository's packages directory.
// See docs/packages.md for the public API boundary and dependency rules.
package harnesskit
