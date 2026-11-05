package cli

import (
	"fmt"
	"io"
)

func usage(w io.Writer) {
	fmt.Fprint(w, `harness-kit drives local agent runtimes (Codex, Claude Code) and serves
them to other apps over JSON-RPC.

Usage:
  harness-kit runtimes [list]            list registered runtimes and capabilities
  harness-kit runtimes discover [names]  check installation, version and sign-in
  harness-kit prompt [flags] "text"      run one turn and stream the reply
  harness-kit chat [flags] ["text"]      interactive session
  harness-kit sessions [list]            sessions you ran through Harness Kit
  harness-kit sessions --native -runtime codex   the runtime's own stored sessions
  harness-kit sessions show|delete ID    inspect or forget one session
  harness-kit sessions rename ID "title" rename one session
  harness-kit sessions configure ID JSON update defaults for future turns
  harness-kit sessions settings ID [JSON] read or update shared session settings
  harness-kit sessions diff ID TURN      show the saved changes during a turn
  harness-kit sessions remove-workspace ID remove a clean, owned worktree
  harness-kit mcp [list]                 MCP servers Harness Kit adds to every runtime
  harness-kit mcp add NAME -- CMD ARGS   add one (or --url URL); --project for this repo
  harness-kit mcp test [NAME]            start servers and list their tools
  harness-kit mcp trust|remove           trust a project's servers / remove one
  harness-kit serve [flags]              JSON-RPC sidecar on stdin/stdout
  harness-kit version

Common flags (prompt, chat):
  -runtime codex|claude|fake   runtime to use (default codex)
  -cwd DIR                     working directory (default .)
  -model NAME                  session default model
  -effort LEVEL                session default reasoning effort
  -turn-config JSON            overrides for each prompt; defaults stay unchanged
  -title TEXT                  session name (default: the start of the first prompt)
  -resume ID                   continue a session: ses_... from "sessions", or a
                               runtime-native ID together with -runtime
  -options JSON                runtime options, e.g. '{"sandbox":"read-only"}'
  -approve ask|allow|deny      answer approvals (default ask)
  -rpc                         go through a sidecar over JSON-RPC
  -mcp FILE                    extra MCP servers for this session
  -no-native-mcp               don't load the runtime's own MCP servers
  -no-kit-mcp                  ignore your user/project harness-kit MCP config

Output (all commands):
  --output text                readable output (default)
  --output json, --json        one JSON document (prompt: the final result)
  --output stream-json         prompt/chat: every event as a JSON line

Examples:
  harness-kit runtimes discover --json
  harness-kit prompt -runtime fake "[approve] hello"
  harness-kit prompt -runtime codex -options '{"sandbox":"read-only"}' "Summarise this repo"
  harness-kit prompt -runtime claude -model haiku --json "What does main.go do?" | jq -r .text
  git diff | harness-kit prompt -runtime claude --output stream-json
  harness-kit chat -runtime codex
  harness-kit sessions
  harness-kit prompt -resume ses_0123abcd "and now add tests"
  harness-kit sessions rename ses_0123abcd "Add tests to the parser"
  harness-kit mcp add github --env GITHUB_TOKEN -- npx -y @modelcontextprotocol/server-github

Run "harness-kit <command> -h" for all flags.
`)
}
