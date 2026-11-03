# Security model

Harness Kit drives agents that can run shell commands and edit files. Its
job is to make the safe configuration the easy one, and to keep the host
application in control.

## Trust boundaries

```text
 untrusted-ish            trusted                         trusted, sandboxed by the runtime
 ┌──────────────┐  IPC   ┌────────────────────┐  stdio   ┌─────────────┐  stdio  ┌────────────────────┐
 │ renderer /   │ ─────► │ Electron main /    │ ───────► │ harness-kit │ ──────► │ codex / claude CLI │
 │ webview      │        │ Wails Go backend   │          │  sidecar    │         │ + its tool calls   │
 └──────────────┘        └────────────────────┘          └─────────────┘         └────────────────────┘
```

- **The sidecar trusts whoever holds its stdin.** That is the process that
  spawned it. It opens no network port and has no other entry point.
- **Web content should not get raw protocol access.** The Electron example
  exposes a few typed IPC calls from the main process. The Wails example
  forwards raw lines but enforces a method allowlist in Go. Either way the
  host decides which operations the UI may perform: for example, a UI that
  can prompt but cannot choose the runtime options.
- **The protocol never exposes unrestricted shell or filesystem access.**
  There is no `exec`, `readFile` or similar method. The only way to cause
  side effects is to prompt an agent, whose actions go through the
  runtime's own sandbox and Harness Kit's approval flow.

## Safe defaults

- **Unsafe options are refused.** Options that switch off a runtime's
  sandbox or permission checks fail with `policy_violation`:
  - Codex `sandbox: "danger-full-access"`
  - Codex `config` overrides touching sandbox or approval settings
  - Claude Code `permissionMode: "bypassPermissions"`

  Opt in explicitly with `session.Options{AllowUnsafe: true}`, or
  `harness-kit --allow-unsafe`. Runtime adapters declare these checks
  through `agent.Guarded`.
- **Codex** sessions default to `sandbox: "workspace-write"` with
  `approvalPolicy: "on-request"`.
- **Claude Code** turns default to Claude Code's manual mode, with
  `--permission-prompts none`: anything that would need approval is denied
  until interactive approvals are bridged.
- **Approvals deny on failure.** A request nobody answers before the
  optional `ApprovalTimeout` is denied. If the turn or session ends first,
  it is aborted. A `Policy` can allow or deny matching requests without
  asking, and every policy decision is still emitted as
  `approval.resolved` (source `policy`) for auditing.
- **Unknown runtime options are rejected**, so a misspelled `sandbox` does
  not silently fall back to a default.

## MCP servers

- Harness Kit never writes the runtimes' MCP configuration. Kit servers are
  added per session only ([mcp.md](mcp.md)).
- **Project `harness-kit.json` files need trust.** A repository could
  otherwise launch arbitrary commands, or send `${TOKEN}` values to a URL it
  controls. Trust is tied to the file's content hash, so any change needs
  re-trusting. Untrusted servers are skipped with a warning.
- Secrets stay as `${VAR}` references in every file and session record. For
  Claude Code they are expanded by Claude Code itself, so they never appear
  in a process's argument list. For Codex they travel only over the
  app-server pipe; a check confirmed they are not written under `~/.codex`.
- The embedded sidecar does not read the end user's kit files unless it is
  started with `--kit-mcp`.

## Host tools and the bridge

- Host tools run in the host process (or the sidecar's client). Runtimes
  reach them through a relay process and a Unix socket in a private (0700)
  temporary directory. No network port is opened.
- Each session has its own random token. A connection presenting an unknown
  token is dropped, and closing a session revokes its token and closes its
  connections. Tokens are never written to session records, and for Claude
  Code they never appear on a command line.
- Arguments are validated against each tool's schema before a handler runs.
  `approval: "ask"` tools go through the normal approval flow and policies.
- The kit ships no shell or filesystem tools (see [tools.md](tools.md)).

## Credentials and personal data

- Harness Kit never reads `~/.codex`, the macOS Keychain, Claude Code's
  credential files, or any other credential store. Sign-in state comes from
  `codex app-server`'s `account/read` and from `claude auth status`.
- Discovery results omit email addresses and account IDs, reporting only
  the auth method and plan.
- No login flow is offered. Users sign in with the CLIs themselves. See
  [runtimes.md](runtimes.md#authentication-and-usage-policies) for why this
  matters for products.

## Things the host must still get right

- **Working directories.** Claude Code in `-p` mode loads a directory's
  hooks and MCP servers without a trust prompt, unless `bare` is set. Only
  start sessions in directories the user trusts.
- **Approval UIs.** Show `title`, `command`, `cwd`, `paths` and `reason`
  from the request. Treat `allow_session` as a meaningful escalation.
- **Packaging.** Ship the sidecar binary inside your signed app bundle and
  launch it by absolute path. Never launch a `harness-kit` found on `PATH`.
- **Raw events** (`rawEvents: true`) pass runtime payloads through
  unfiltered. Don't render them as HTML.

## Saved transcripts

- FileStore records and JSONL event transcripts are created with mode 0600
  in a directory created with mode 0700. Transcripts include prompts,
  assistant replies, tool inputs/outputs, approvals, usage, and raw runtime
  events when enabled. Treat the store as conversation data; it is not
  encrypted or automatically redacted.
- `session.delete` removes both the Harness Kit record and its event log.
  Runtime-native history is managed separately by the runtime.
- `--store memory` avoids writing Harness Kit transcripts to disk.

## Configuration updates

Adapter validation and the host's configuration validator run at create,
restore, default updates and turn admission. Explicit safety-disabling requests
still require the host's `AllowUnsafe` opt-in. Omitted settings inherit native
runtime configuration; that configuration is not inspected or overridden by
this guard. Hosts requiring particular sandbox or approval settings should set
them explicitly. Configuration snapshots can include native option values and
are saved with transcripts; avoid placing literal secrets in those values.
