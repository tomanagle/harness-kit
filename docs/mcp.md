# MCP servers

Define an MCP server once and every runtime gets it. Harness Kit translates
one portable definition into each runtime's own **per-session** mechanism.
It never edits `~/.codex/config.toml`, `~/.claude.json` or a project's
`.mcp.json`.

```sh
harness-kit mcp add github --env GITHUB_TOKEN -- npx -y @modelcontextprotocol/server-github
harness-kit mcp test github        # starts it, lists its tools
harness-kit prompt -runtime codex  "Find open issues about login"
harness-kit prompt -runtime claude "Find open issues about login"   # same server
```

## Three layers

Each new session merges three layers. A later layer replaces an earlier
entry with the same name entirely; fields are not merged.

| Layer | Where | Typical use |
| --- | --- | --- |
| user | `~/Library/Application Support/harness-kit/config.json` (macOS), `~/.config/harness-kit/config.json` (Linux), `%AppData%\harness-kit\config.json` (Windows); override with `HARNESS_KIT_CONFIG` | your personal servers, everywhere |
| project | `harness-kit.json`, found by walking up from the session's working directory | servers a repository needs; commit it |
| session | `--mcp FILE` on the CLI, or `mcp` in `session.create` | servers an app adds in code |

Separately, the runtime's **own** servers (configured with `codex mcp add`,
`claude mcp add` and so on) still load unless the merged config sets
`"inheritNative": false` (`--no-native-mcp`). Turning them off can matter a
lot: one natively configured Codex server can add dozens of tools to every
session.

Use native configuration for servers that need OAuth. Claude Code cannot
run an OAuth flow in `-p` mode, and both runtimes keep OAuth tokens in
their own stores. Sign in once with `codex mcp login` or `claude mcp login`.

## Format

The format is the familiar `mcpServers` object, plus a few portable fields.
Unknown fields are rejected.

```json
{
  "mcpServers": {
    "github": {
      "command": "npx",
      "args": ["-y", "@modelcontextprotocol/server-github"],
      "env": { "GITHUB_TOKEN": "${GITHUB_TOKEN}" },
      "tools": { "allow": ["search_issues", "get_issue"] }
    },
    "docs": {
      "type": "http",
      "url": "https://mcp.example.com/mcp",
      "headers": { "Authorization": "Bearer ${DOCS_TOKEN}" },
      "approval": "ask",
      "runtimes": ["codex"]
    }
  },
  "inheritNative": true
}
```

| Field | Meaning |
| --- | --- |
| `type` | `stdio` (default) or `http` (streamable HTTP; `streamable-http` is an alias). Legacy SSE is not supported. |
| `command`, `args`, `env`, `cwd` | stdio servers |
| `url`, `headers` | http servers |
| `tools.allow` / `tools.deny` | exact tool names. With `allow`, only those tools are usable; `deny` tools are never usable. |
| `approval` | `auto` (default): run the allowed tools without asking. `ask`: request approval for each call. |
| `runtimes` | only load for these runtimes |
| `startupTimeoutSec`, `toolTimeoutSec` | timeouts (Codex; ignored by Claude Code) |
| `disabled` | remove a server defined in a lower layer |

Server names may contain letters, digits, `-` and single `_`, which keeps
them valid as Codex config keys and in Claude Code's `mcp__<server>__<tool>`
tool names.

### Secrets

Write `${VAR}` or `${VAR:-default}` in `command`, `args`, `env`, `cwd`, `url`
and `headers`. Values are resolved from the environment when a session
starts and are never written to Harness Kit's files or session records.
`harness-kit mcp add NAME --env TOKEN -- ...` stores `"TOKEN": "${TOKEN}"`,
and `--bearer-env VAR` stores `Authorization: Bearer ${VAR}`. A server whose
variable is unset is skipped with a warning; it does not fail the session.

## How each runtime gets the servers

Every mapping below was checked against the real CLIs (Codex 0.166.0,
Claude Code 2.1.316).

| | Codex (`thread/start` → `config.mcp_servers`) | Claude Code (`claude -p`) |
| --- | --- | --- |
| servers | `mcp_servers.<name>` for that thread only | `--mcp-config '{"mcpServers": …}'` |
| `${VAR}` | expanded by Harness Kit; sent only over the app-server pipe, not written to `~/.codex` | passed through unexpanded; Claude Code expands it, so secrets never appear on the command line |
| `tools.allow` / `deny` | `enabled_tools` / `disabled_tools` | `--allowedTools mcp__<name>__<tool>` / `--disallowedTools` |
| `approval: auto` | `default_tools_approval_mode = "approve"` | `--allowedTools mcp__<name>__*` |
| `approval: ask` | `"prompt"`. Codex asks through an MCP elicitation, which Harness Kit turns into `approval.requested` | no allow rule, so Claude Code prompts, and the prompt reaches the host through the permission bridge ([tools.md](tools.md)). Without a tool host the tools are denied, with a warning. |
| `inheritNative: false` | each native server (from `config/read`) set to `enabled = false` for the thread | `--strict-mcp-config` |
| name clash with a native server | kit server skipped with a warning (Codex would merge the two definitions) | Claude Code's precedence applies |
| `cwd` | supported | ignored (warning) |

MCP tool calls show up as ordinary `tool.started` / `tool.completed` events
with `kind: "mcp"`.

## Project files and trust

A cloned repository's `harness-kit.json` could launch any command. It could
also send your tokens elsewhere, for example with an `http` server whose
header references `${GITHUB_TOKEN}`. Project servers therefore load only
after you trust the file:

```sh
harness-kit mcp list           # shows project servers as "untrusted"
harness-kit mcp trust          # prints what the file defines, then trusts it
```

Trust is tied to the file's exact content (a SHA-256 hash in `trust.json`
next to your user config). Any change, such as a pulled commit, needs
trusting again. Until then those servers are skipped and the session shows
a warning. `harness-kit mcp add --project` updates the trust record itself,
since you wrote the change. `inheritNative: false` from an untrusted file is
still honoured, because it can only remove servers.

Note that Claude Code itself loads a project's `.mcp.json` *without asking*
in `-p` mode. Use `--no-native-mcp` (`--strict-mcp-config`) in repositories
you don't trust.

## CLI

```sh
harness-kit mcp [list] [--cwd DIR] [--json]       # kit servers: source, type, tools, trust status
harness-kit mcp list --native -runtime codex      # servers configured in Codex itself
harness-kit mcp add NAME [--project] -- COMMAND [ARGS...]
      [--env KEY[=VALUE]]... [--allow a,b] [--deny c] [--approval auto|ask] [--runtime codex]...
harness-kit mcp add NAME [--project] --url URL [--header Name=Value]... [--bearer-env VAR]
harness-kit mcp remove NAME [--project]
harness-kit mcp trust | untrust [--cwd DIR]
harness-kit mcp test [NAME...] [--json]           # connect, list tools, report failures (exit 1)

harness-kit prompt|chat ... [--mcp FILE] [--no-native-mcp] [--no-kit-mcp]
```

## Apps and the sidecar

- `session.create` accepts `"mcp": {"mcpServers": {…}, "inheritNative": false}`.
  This is what an app uses to give a session its own tools.
- `harness-kit serve --kit-mcp` also merges the user file and a trusted
  project file into every session. It is off by default, because a sidecar
  embedded in an app should not pick up the end user's personal config
  unless the app wants that. The CLI's `--rpc` mode turns it on.
- Runtimes advertise `capabilities.mcp`. Sessions on runtimes without it
  get a warning that the servers were ignored.
- In Go, set `agent.SessionConfig.MCP`. To merge files the way the CLI does,
  use `mcp.Resolver` in a `session.Options.Prepare` hook.

## Limitations and next steps

- `approval: ask` on Claude Code needs the tool host (present in the CLI
  and sidecar). The same bridge serves host tools ([tools.md](tools.md)).
- Tool filters take exact names, not globs, because Codex's
  `enabled_tools` does.
- The kit does not manage OAuth logins; use the runtime's native config for
  those servers.
- Startup status (which servers connected) is not yet normalised into
  events. `harness-kit mcp test` checks a server directly.
