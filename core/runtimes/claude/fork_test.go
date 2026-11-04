package claude

import (
	"context"
	"github.com/tomanagle/harness-kit/core/agent"
	"testing"
)

func TestPortableForkDefersNativeIDUntilRealPrompt(t *testing.T) {
	rt, log := newFake(t, "2.1.316")
	s, err := rt.Fork(context.Background(), "parent", agent.SessionConfig{}, &recorder{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	if s.RuntimeSessionID() != "" {
		t.Fatal("pending fork must never expose the parent's native id")
	}
	if _, err = s.Prompt(context.Background(), agent.Turn{ID: "real", Input: agent.Text("hello")}); err != nil {
		t.Fatal(err)
	}
	if s.RuntimeSessionID() != "forked-parent" {
		t.Fatalf("native child id: %s", s.RuntimeSessionID())
	}
	calls := invocations(t, log)
	if len(calls) != 1 {
		t.Fatalf("fork ran synthetic turn: %+v", calls)
	}
}
