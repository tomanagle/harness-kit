package codex

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/tomanagle/harness-kit/agent"
	"github.com/tomanagle/harness-kit/event"
)

// TestLive runs against the real, locally installed and signed-in Codex
// CLI. It consumes a small amount of the user's Codex quota, so it only
// runs with HARNESS_KIT_LIVE=1.
func TestLive(t *testing.T) {
	if os.Getenv("HARNESS_KIT_LIVE") != "1" {
		t.Skip("set HARNESS_KIT_LIVE=1 to run against the real Codex CLI")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	rt := New()
	d, err := rt.Discover(ctx)
	if err != nil || !d.Ready {
		t.Skipf("codex not ready: %+v %v", d, err)
	}
	t.Logf("codex %s, auth %s/%s", d.Version, d.Auth.Method, d.Auth.Plan)

	cwd := t.TempDir()
	rec := &recorder{}
	opts := Options{Sandbox: "read-only", ApprovalPolicy: "on-request", Effort: "low"}
	s, err := rt.Start(ctx, agent.SessionConfig{Cwd: cwd, Options: opts}, rec)
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Prompt(ctx, agent.Turn{ID: "t1", Input: agent.Text("Reply with exactly the word PINEAPPLE and nothing else. Do not run any commands.")})
	if err != nil || res.Status != event.TurnStatusCompleted {
		t.Fatalf("turn: %+v %v", res, err)
	}
	var text strings.Builder
	for _, e := range rec.ofType(event.MessageDelta) {
		text.WriteString(e.Data.(event.MessageDeltaData).Text)
	}
	if !strings.Contains(strings.ToUpper(text.String()), "PINEAPPLE") {
		t.Fatalf("unexpected reply %q", text.String())
	}
	t.Logf("reply %q usage %+v", text.String(), res.Usage)
	thread := s.RuntimeSessionID()
	s.Close(ctx)

	// Resume the thread in a fresh app-server process.
	rec2 := &recorder{}
	s2, err := rt.Start(ctx, agent.SessionConfig{Cwd: cwd, ResumeID: thread, Options: opts}, rec2)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close(ctx)
	res, err = s2.Prompt(ctx, agent.Turn{ID: "t2", Input: agent.Text("What single word did you reply with last time? Answer with just that word.")})
	if err != nil || res.Status != event.TurnStatusCompleted {
		t.Fatalf("resumed turn: %+v %v", res, err)
	}
	var text2 strings.Builder
	for _, e := range rec2.ofType(event.MessageDelta) {
		text2.WriteString(e.Data.(event.MessageDeltaData).Text)
	}
	if !strings.Contains(strings.ToUpper(text2.String()), "PINEAPPLE") {
		t.Fatalf("resume lost context: %q", text2.String())
	}
}
