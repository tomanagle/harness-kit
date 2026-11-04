package session

import (
	"context"
	"testing"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/runtimes/fake"
)

func TestPinnedRestoreReleasesPreviousInstance(t *testing.T) {
	ctx := context.Background()
	store, err := NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(agent.NewRegistry(fake.New(fake.WithDelay(0))), Options{Store: store})
	t.Cleanup(func() { _ = m.Shutdown(ctx) })
	info, err := m.Create(ctx, CreateParams{Runtime: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := m.Prompt(ctx, info.ID, PromptParams{Input: agent.Text("save a resumable conversation")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := turn.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Pin(ctx, info.ID, true); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		if err := m.Close(ctx, info.ID); err != nil {
			t.Fatal(err)
		}
		restored, err := m.Restore(ctx, info.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !restored.Pinned {
			t.Fatal("restore lost pin")
		}
		m.mu.Lock()
		retired := len(m.closedOrder)
		m.mu.Unlock()
		if retired != 0 {
			t.Fatalf("restoring one pinned session retained %d obsolete instances", retired)
		}
	}
}
