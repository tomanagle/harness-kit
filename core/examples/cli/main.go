// Command cli is a minimal terminal harness built directly on the Harness
// Kit Go packages, with no sidecar and no RPC. It streams the agent's reply
// and asks for approvals on the terminal.
//
//	go run ./examples/cli -runtime codex "Explain this codebase"
//	go run ./examples/cli -runtime claude -model sonnet "What does main.go do?"
//	go run ./examples/cli -runtime fake "[approve] hello"
//
// Omit the prompt to chat interactively; resume with -resume <runtime id>.
//
// This is sample code showing how to build your own harness on the
// library. For a ready-made CLI, use `harness-kit prompt` and
// `harness-kit chat` (cmd/harness-kit).
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/permission"
	"github.com/tomanagle/harness-kit/core/runtimes/claude"
	"github.com/tomanagle/harness-kit/core/runtimes/codex"
	"github.com/tomanagle/harness-kit/core/runtimes/fake"
	"github.com/tomanagle/harness-kit/core/session"
)

func main() {
	rtName := flag.String("runtime", "codex", "runtime: codex, claude or fake")
	model := flag.String("model", "", "model override")
	cwd := flag.String("cwd", ".", "working directory for the agent")
	resume := flag.String("resume", "", "runtime session id to resume")
	flag.Parse()

	dir, err := absDir(*cwd)
	if err != nil {
		fatal(err)
	}

	// 1. Registry: the runtimes this harness offers.
	registry := agent.NewRegistry(codex.New(), claude.New(), fake.New())

	ctx := context.Background()
	ds, err := registry.Discover(ctx, *rtName)
	if err != nil {
		fatal(err)
	}
	if d := ds[0]; !d.Ready {
		fatal(fmt.Errorf("%s is not ready: %s", d.Runtime, strings.Join(d.Problems, "; ")))
	}

	// 2. Sessions, with a policy that auto-approves read-only git commands
	// and asks the terminal about everything else.
	mgr := session.NewManager(registry, session.Options{
		Policy: permission.Rules{
			{Kind: permission.KindCommand, Command: "git status*", Action: permission.ActionAllow},
			{Kind: permission.KindCommand, Command: "git diff*", Action: permission.ActionAllow},
			{Kind: permission.KindCommand, Command: "git log*", Action: permission.ActionAllow},
		},
	})
	defer mgr.Shutdown(context.Background())

	info, err := mgr.Create(ctx, session.CreateParams{
		Runtime: *rtName,
		Config:  agent.SessionConfig{Cwd: dir, Model: *model, ResumeID: *resume},
	})
	if err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "session %s (%s %s)\n", info.ID, info.Runtime, info.RuntimeSessionID)

	stdin := bufio.NewReader(os.Stdin)
	var stdinMu sync.Mutex
	readLine := func(prompt string) (string, bool) {
		stdinMu.Lock()
		defer stdinMu.Unlock()
		fmt.Fprint(os.Stderr, prompt)
		line, err := stdin.ReadString('\n')
		return strings.TrimSpace(line), err == nil
	}

	// 3. Approvals: a separate subscription answers approval.requested.
	approvals := mgr.Subscribe(func(e event.Event) bool { return e.Type == event.ApprovalRequested }, 0)
	defer approvals.Close()
	go func() {
		for ev := range approvals.C {
			req := ev.Data.(permission.Request)
			fmt.Fprintf(os.Stderr, "\n\n⚠ %s wants to: %s\n", req.Runtime, req.Title)
			if req.Reason != "" {
				fmt.Fprintf(os.Stderr, "  reason: %s\n", req.Reason)
			}
			answer, _ := readLine("  allow? [y]es / [a]lways / [n]o / [q]uit turn: ")
			outcome := map[string]permission.Outcome{"y": permission.Allow, "a": permission.AllowSession, "q": permission.Abort}[strings.ToLower(answer)]
			if outcome == "" || !req.Supports(outcome) {
				outcome = permission.Deny
			}
			if err := mgr.RespondApproval(req.ID, permission.Decision{Outcome: outcome}); err != nil {
				fmt.Fprintln(os.Stderr, "  respond:", err)
			}
		}
	}()

	run := func(prompt string) {
		// Ctrl-C cancels the turn rather than the program.
		tctx, stop := signal.NotifyContext(ctx, os.Interrupt)
		defer stop()
		for ev, err := range mgr.Stream(tctx, info.ID, session.PromptParams{Input: agent.Text(prompt)}) {
			if err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
				return
			}
			render(ev)
		}
	}

	if prompt := strings.Join(flag.Args(), " "); prompt != "" {
		run(prompt)
		return
	}
	for {
		line, ok := readLine("\n> ")
		if !ok || line == "/quit" {
			return
		}
		if line != "" {
			run(line)
		}
	}
}

func render(ev event.Event) {
	switch d := ev.Data.(type) {
	case event.MessageDeltaData:
		fmt.Print(d.Text)
	case event.ToolStartedData:
		fmt.Fprintf(os.Stderr, "\n· %s\n", firstNonEmpty(d.Title, d.Name))
	case event.ToolCompletedData:
		if d.Status != event.ToolStatusCompleted {
			fmt.Fprintf(os.Stderr, "· %s %s\n", d.Status, d.Error)
		}
	case event.RuntimeStatusData:
		if d.Status != "ready" {
			fmt.Fprintf(os.Stderr, "\n[%s] %s\n", d.Status, d.Message)
		}
	case event.ErrorData:
		fmt.Fprintf(os.Stderr, "\n[error] %s\n", d.Message)
	case event.TurnCompletedData:
		fmt.Println()
		line := string(d.Status)
		if d.Usage != nil {
			line += fmt.Sprintf(" · %d in / %d out tokens", d.Usage.InputTokens, d.Usage.OutputTokens)
			if d.Usage.CostUSD != nil {
				line += fmt.Sprintf(" · ~$%.4f", *d.Usage.CostUSD)
			}
		}
		if d.Error != nil {
			line += " · " + d.Error.Message
		}
		fmt.Fprintln(os.Stderr, "—", line)
	}
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func absDir(p string) (string, error) {
	fi, err := os.Stat(p)
	if err != nil || !fi.IsDir() {
		return "", fmt.Errorf("%s is not a directory", p)
	}
	if p == "." {
		return os.Getwd()
	}
	return p, nil
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "cli:", err)
	os.Exit(1)
}
