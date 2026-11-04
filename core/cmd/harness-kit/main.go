// Command harness-kit is both the Harness Kit sidecar and a command-line
// tool for driving agent runtimes.
//
//	harness-kit runtimes [list]        registered runtimes and capabilities
//	harness-kit runtimes discover      probe installation and sign-in state
//	harness-kit prompt [flags] "text"  run one turn and stream the reply
//	harness-kit chat [flags]           interactive session
//	harness-kit serve [flags]          serve JSON-RPC on stdin/stdout (sidecar)
//	harness-kit version
//
// CLI commands run in-process by default; with --rpc they spawn this binary
// as a sidecar and go through the JSON-RPC protocol instead.
//
// In serve mode stdout carries only protocol messages and diagnostics go to
// stderr. The sidecar exits when stdin closes (for example because the
// parent died), after closing every session and its runtime processes.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/logging"
	"github.com/tomanagle/harness-kit/core/mcp"
	mcpclient "github.com/tomanagle/harness-kit/core/mcp/client"
	"github.com/tomanagle/harness-kit/core/rpc/server"
	"github.com/tomanagle/harness-kit/core/runtimes/acp"
	"github.com/tomanagle/harness-kit/core/runtimes/claude"
	"github.com/tomanagle/harness-kit/core/runtimes/codex"
	"github.com/tomanagle/harness-kit/core/runtimes/fake"
	"github.com/tomanagle/harness-kit/core/session"
	"github.com/tomanagle/harness-kit/core/settings"
	"github.com/tomanagle/harness-kit/core/toolhost"
	"github.com/tomanagle/harness-kit/core/tools"
	gitworkspace "github.com/tomanagle/harness-kit/core/workspace/git"
)

// callMCPTool lets the fake runtime call tools (its [call] directive).
func callMCPTool(ctx context.Context, s mcp.Server, tool string, args json.RawMessage) (string, bool, error) {
	sess, err := mcpclient.Connect(ctx, s, mcpclient.Options{})
	if err != nil {
		return "", false, err
	}
	defer sess.Close()
	res, err := sess.CallTool(ctx, tool, args)
	return res.Text(), res.IsError, err
}

// version is set with -ldflags "-X main.version=v0.1.0".
var version = ""

func buildVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

func main() {
	// When a runtime launches us as the MCP bridge to the tool host, relay
	// and exit before anything else.
	toolhost.ServeBridgeIfRequested()
	args := os.Args[1:]
	switch {
	case len(args) == 0 && stdinIsTerminal():
		usage(os.Stdout) // a person ran it
		return
	case len(args) == 0:
		args = []string{"serve"} // spawned as a sidecar with piped stdin
	case args[0] == "-h" || args[0] == "--help":
		args[0] = "help"
	case args[0] == "--version":
		args[0] = "version"
	case strings.HasPrefix(args[0], "-"):
		args = append([]string{"serve"}, args...) // `harness-kit --fake-runtime`
	}

	var err error
	switch cmd, rest := args[0], args[1:]; cmd {
	case "serve":
		err = serve(rest)
	case "runtimes":
		err = runtimesCmd(rest)
	case "discover":
		err = runtimesCmd(append([]string{"discover"}, rest...))
	case "prompt":
		err = promptCmd(rest)
	case "chat":
		err = chatCmd(rest)
	case "sessions", "session":
		err = sessionsCmd(rest)
	case "mcp":
		err = mcpCmd(rest)
	case "mcp-bridge":
		err = errors.New("mcp-bridge is started by agent runtimes to reach Harness Kit's tool host; it is not run by hand")
	case "version":
		fmt.Println("harness-kit", buildVersion())
	case "help":
		usage(os.Stdout)
	default:
		fmt.Fprintf(os.Stderr, "harness-kit: unknown command %q\n\n", cmd)
		usage(os.Stderr)
		os.Exit(2)
	}
	var code exitCode
	switch {
	case errors.As(err, &code):
		os.Exit(int(code))
	case errors.Is(err, flag.ErrHelp):
		os.Exit(0)
	case err != nil:
		fmt.Fprintln(os.Stderr, "harness-kit:", err)
		os.Exit(1)
	}
}

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

type runtimeFlags struct {
	acp        map[string]settings.ACPRuntime
	runtimes   string
	fake       bool
	fakeDelay  time.Duration
	codexPath  string
	claudePath string
	gitPath    string
}

func (rf *runtimeFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&rf.runtimes, "runtimes", "codex,claude", "comma-separated runtimes to enable (codex, claude, fake)")
	fs.BoolVar(&rf.fake, "fake-runtime", os.Getenv("HARNESS_KIT_FAKE_RUNTIME") == "1", "also enable the deterministic fake runtime (env HARNESS_KIT_FAKE_RUNTIME=1)")
	fs.DurationVar(&rf.fakeDelay, "fake-delay", 5*time.Millisecond, "delay between words streamed by the fake runtime")
	fs.StringVar(&rf.codexPath, "codex-path", "", "path to the codex binary (default: HARNESS_KIT_CODEX_PATH, PATH, well-known dirs)")
	fs.StringVar(&rf.claudePath, "claude-path", "", "path to the claude binary (default: HARNESS_KIT_CLAUDE_PATH, PATH, well-known dirs)")
	fs.StringVar(&rf.gitPath, "git-path", "", "path to git for optional workspace operations (default: PATH)")
}

func (rf *runtimeFlags) registry() (*agent.Registry, error) {
	reg := agent.NewRegistry()
	names := strings.Split(rf.runtimes, ",")
	if rf.fake {
		names = append(names, "fake")
	}
	seen := map[string]bool{}
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		var rt agent.Runtime
		switch n {
		case codex.Name:
			rt = codex.New(codex.WithPath(rf.codexPath), codex.WithClientInfo(codex.ClientInfo{Name: "harness_kit", Title: "Harness Kit", Version: buildVersion()}))
		case claude.Name:
			rt = claude.New(claude.WithPath(rf.claudePath))
		case "fake":
			rt = fake.New(fake.WithDelay(rf.fakeDelay), fake.WithMCPCaller(callMCPTool))
		default:
			definition, ok := rf.acp[n]
			if !ok {
				return nil, fmt.Errorf("unknown runtime %q", n)
			}
			options := []acp.Option{acp.WithName(n), acp.WithArgs(definition.Args...), acp.WithEnv(definition.Env), acp.WithClientInfo(acp.ClientInfo{Name: "harness-kit", Version: buildVersion()})}
			if definition.StartTimeoutMs != nil {
				options = append(options, acp.WithStartTimeout(time.Duration(*definition.StartTimeoutMs)*time.Millisecond))
			}
			if definition.CancelTimeoutMs != nil {
				options = append(options, acp.WithCancelTimeout(time.Duration(*definition.CancelTimeoutMs)*time.Millisecond))
			}
			if definition.MaxMessageBytes != nil {
				options = append(options, acp.WithMaxMessageSize(*definition.MaxMessageBytes))
			}
			if definition.MaxImageBytes != nil {
				options = append(options, acp.WithMaxImageBytes(*definition.MaxImageBytes))
			}
			if definition.MaxRetainedOutputBytes != nil {
				options = append(options, acp.WithMaxRetainedOutput(*definition.MaxRetainedOutputBytes))
			}
			rt = acp.New(definition.Command, options...)
		}
		if err := reg.Register(rt); err != nil {
			return nil, err
		}
	}
	return reg, nil
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	var rf runtimeFlags
	rf.register(fs)
	allowUnsafe := fs.Bool("allow-unsafe", false, "accept session options that disable runtime sandboxes or approvals")
	approvalTimeout := fs.Duration("approval-timeout", 0, "deny approvals not answered within this duration (0 = wait indefinitely)")
	logSize := fs.Int("event-log-size", 2048, "events retained per session for replay")
	eventBuffer := fs.Int("event-buffer", 4096, "events queued for a slow client before they are dropped (reported as stream.gap)")
	logLevel := fs.String("log-level", "info", "stderr log level: debug, info, warn, error")
	storeDir := fs.String("store", "", `persist session records in this directory; "default" shares the CLI's history (default: in memory)`)
	kitMCP := fs.Bool("kit-mcp", false, "merge MCP servers from the user config and trusted project harness-kit.json into every session")
	settingsFile := fs.String("settings", "", "initial settings JSON file (explicit flags override the file; live updates use settings.configure)")
	settingsJSON := fs.String("settings-json", "", "initial settings JSON document (used by the owning SDK transport)")
	fs.Bool("stdio", true, "serve on stdin/stdout (the only transport)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	initial := settings.Defaults()
	initial.Process = settings.Process{Store: *storeDir, Runtimes: strings.Split(rf.runtimes, ","), CodexPath: rf.codexPath, ClaudePath: rf.claudePath, GitPath: rf.gitPath, FakeDelayMs: rf.fakeDelay.Milliseconds(), KitMCP: *kitMCP, AllowUnsafe: *allowUnsafe, ShutdownTimeoutMs: 10000}
	if rf.fake {
		initial.Process.Runtimes = append(initial.Process.Runtimes, "fake")
	}
	initial.Logging.Level = *logLevel
	initial.Approvals.TimeoutMs = approvalTimeout.Milliseconds()
	initial.Memory.EventLogSize = *logSize
	initial.Streams.MaxQueueEvents = *eventBuffer
	if *settingsFile != "" && *settingsJSON != "" {
		return errors.New("use only one of --settings and --settings-json")
	}
	var err error
	if *settingsJSON != "" {
		flagValues := initial
		initial, err = settings.Decode(strings.NewReader(*settingsJSON), initial)
		if err == nil {
			initial, err = overrideServeFlags(fs, initial, flagValues)
		}
	} else {
		initial, err = loadServeSettings(*settingsFile, fs, initial)
	}
	if err != nil {
		return err
	}
	rf.runtimes, rf.fake = strings.Join(initial.Process.Runtimes, ","), false
	rf.acp = initial.Process.ACP
	rf.codexPath, rf.claudePath = initial.Process.CodexPath, initial.Process.ClaudePath
	rf.gitPath = initial.Process.GitPath
	rf.fakeDelay = time.Duration(initial.Process.FakeDelayMs) * time.Millisecond
	*storeDir, *kitMCP, *allowUnsafe = initial.Process.Store, initial.Process.KitMCP, initial.Process.AllowUnsafe
	*logSize, *eventBuffer = initial.Memory.EventLogSize, initial.Streams.MaxQueueEvents
	*approvalTimeout = time.Duration(initial.Approvals.TimeoutMs) * time.Millisecond
	var level slog.Level
	if err := level.UnmarshalText([]byte(initial.Logging.Level)); err != nil {
		return err
	}
	logLevelVar := new(slog.LevelVar)
	logLevelVar.Set(level)
	logs := logging.New(logLevelVar, slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: logLevelVar}), logging.Config{
		MaxQueueRecords: initial.Logging.MaxQueueRecords, MaxQueueBytes: initial.Logging.MaxQueueBytes, MaxRecordBytes: initial.Logging.MaxRecordBytes,
	})
	defer logs.Close()
	log := slog.New(logs)

	reg, err := rf.registry()
	if err != nil {
		return err
	}
	store, err := openStore(*storeDir)
	if err != nil {
		return err
	}
	// Host tools: apps register them over RPC (tools.register); runtimes
	// reach them, and Claude Code's permission prompts reach the client,
	// through the MCP bridge.
	registry := tools.NewRegistry()
	host := toolhost.New(toolhost.Options{Registry: registry, Version: buildVersion(), Logger: log})
	defer host.Close()
	var prepare func(context.Context, *session.CreateParams) ([]string, error)
	var resolver server.MCPResolver
	if *kitMCP {
		r, err := kitResolver()
		if err != nil {
			return err
		}
		prepare, resolver = kitPrepare(r), &r
	}
	mgr := session.NewManager(reg, session.Options{
		Workspace:       gitworkspace.New(rf.gitPath),
		ToolServer:      host,
		Prepare:         prepare,
		Store:           store,
		AllowUnsafe:     *allowUnsafe,
		ApprovalTimeout: *approvalTimeout,
		EventLogSize:    *logSize,
		Logger:          log,
		LogLevel:        logLevelVar,
		Logs:            logs,
	})
	if err := mgr.InitializeSettings(context.Background(), initial); err != nil {
		return err
	}
	srv := server.New(mgr, server.Options{
		MCP:          resolver,
		Tools:        registry,
		Version:      buildVersion(),
		EventBuffer:  *eventBuffer,
		AllowUnsafe:  *allowUnsafe,
		EventLogSize: *logSize,
		Logger:       log,
	})

	ignoreSIGPIPE()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigs
		log.Info("signal received; shutting down", "signal", sig.String())
		os.Stdin.Close() // ends Serve
	}()

	log.Info("harness-kit serving on stdio", "version", buildVersion(), "runtimes", rf.runtimes, "fake", rf.fake)
	serveErr := srv.Serve(context.Background(), os.Stdin, os.Stdout)
	if errors.Is(serveErr, os.ErrClosed) || errors.Is(serveErr, io.ErrClosedPipe) {
		serveErr = nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := mgr.Shutdown(ctx); err != nil {
		log.Warn("shutdown", "err", err)
	}
	log.Info("harness-kit exited")
	return serveErr
}
