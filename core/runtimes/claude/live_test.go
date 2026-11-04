package claude

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/mcp"
)

// TestLive runs against the real, locally installed and signed-in Claude
// Code CLI. It consumes a small amount of the user's usage, so it only runs
// with HARNESS_KIT_LIVE=1. Set HARNESS_KIT_LIVE_CLAUDE_MODEL to choose the
// model (default "haiku").
func TestLive(t *testing.T) {
	if os.Getenv("HARNESS_KIT_LIVE") != "1" {
		t.Skip("set HARNESS_KIT_LIVE=1 to run against the real Claude Code CLI")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	rt := New()
	d, err := rt.Discover(ctx)
	if err != nil || !d.Ready {
		t.Skipf("claude not ready: %+v %v", d, err)
	}
	t.Logf("claude %s, auth %s/%s", d.Version, d.Auth.Method, d.Auth.Plan)

	model := os.Getenv("HARNESS_KIT_LIVE_CLAUDE_MODEL")
	if model == "" {
		model = "haiku"
	}
	noTools := []string{}
	inheritMCP := false
	rec := &recorder{}
	s, err := rt.Start(ctx, agent.SessionConfig{Cwd: t.TempDir(), Model: model, MCP: &mcp.Config{InheritNative: &inheritMCP}, Options: Options{Tools: &noTools, MaxTurns: 2, MaxBudgetUSD: .5}}, rec)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	reply := func(turnID string) string {
		var b strings.Builder
		for _, e := range rec.ofType(event.MessageDelta) {
			if e.TurnID == turnID {
				b.WriteString(e.Data.(event.MessageDeltaData).Text)
			}
		}
		return b.String()
	}

	res, err := s.Prompt(ctx, agent.Turn{ID: "t1", Input: agent.Text("Reply with exactly the word PINEAPPLE and nothing else.")})
	if err != nil || res.Status != event.TurnStatusCompleted {
		t.Fatalf("turn: %+v %v", res, err)
	}
	if !strings.Contains(strings.ToUpper(reply("t1")), "PINEAPPLE") {
		t.Fatalf("unexpected reply %q", reply("t1"))
	}
	t.Logf("reply %q usage %+v cost %v", reply("t1"), res.Usage, res.Usage.CostUSD)

	res, err = s.Prompt(ctx, agent.Turn{ID: "t2", Input: agent.Text("What single word did you reply with last time? Answer with just that word.")})
	if err != nil || res.Status != event.TurnStatusCompleted {
		t.Fatalf("resumed turn: %+v %v", res, err)
	}
	if !strings.Contains(strings.ToUpper(reply("t2")), "PINEAPPLE") {
		t.Fatalf("resume lost context: %q", reply("t2"))
	}
	for _, ev := range rec.ofType(event.Usage) {
		usage := ev.Data.(event.UsageData)
		if usage.CostUSD != nil {
			t.Logf("turn=%s scope=%s cost_usd=%.8f", ev.TurnID, usage.Scope, *usage.CostUSD)
		}
	}
}
