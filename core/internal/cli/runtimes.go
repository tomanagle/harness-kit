package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/mcp"
	mcpclient "github.com/tomanagle/harness-kit/core/mcp/client"
	"github.com/tomanagle/harness-kit/core/runtimes/acp"
	"github.com/tomanagle/harness-kit/core/runtimes/claude"
	"github.com/tomanagle/harness-kit/core/runtimes/codex"
	"github.com/tomanagle/harness-kit/core/runtimes/fake"
	"github.com/tomanagle/harness-kit/core/settings"
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

type runtimeFlags struct {
	version    string
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
			rt = codex.New(codex.WithPath(rf.codexPath), codex.WithClientInfo(codex.ClientInfo{Name: "harness_kit", Title: "Harness Kit", Version: rf.version}))
		case claude.Name:
			rt = claude.New(claude.WithPath(rf.claudePath))
		case "fake":
			rt = fake.New(fake.WithDelay(rf.fakeDelay), fake.WithMCPCaller(callMCPTool))
		default:
			definition, ok := rf.acp[n]
			if !ok {
				return nil, fmt.Errorf("unknown runtime %q", n)
			}
			options := []acp.Option{acp.WithName(n), acp.WithArgs(definition.Args...), acp.WithEnv(definition.Env), acp.WithClientInfo(acp.ClientInfo{Name: "harness-kit", Version: rf.version})}
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
