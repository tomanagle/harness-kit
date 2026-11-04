package codex

import (
	"context"
	"github.com/tomanagle/harness-kit/core/agent"
	"testing"
)

func TestForkUsesNativeThreadForkWithoutPrompt(t *testing.T) {
	s, err := newFake(t).Fork(context.Background(), "parent", agent.SessionConfig{}, &recorder{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	if s.RuntimeSessionID() != "forked-parent" {
		t.Fatalf("fork id: %q", s.RuntimeSessionID())
	}
}
