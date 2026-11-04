package session_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/runtimes/fake"
	"github.com/tomanagle/harness-kit/core/session"
)

func deferredCompactionManager(t *testing.T, store session.Store, cache int) *session.Manager {
	t.Helper()
	m := session.NewManager(agent.NewRegistry(deferredRuntime{fake.New()}), session.Options{Store: store, EventLogSize: cache})
	t.Cleanup(func() { _ = m.Shutdown(context.Background()) })
	return m
}

func deferredContextFork(t *testing.T, m *session.Manager) (session.Info, session.Info) {
	t.Helper()
	parent := create(t, m, agent.SessionConfig{Options: map[string]any{"contextTokens": 80, "contextWindowTokens": 100}})
	collect(t, m, parent.ID, "parent")
	child, err := m.Fork(context.Background(), parent.ID, session.ForkParams{})
	if err != nil || !child.ForkPending {
		t.Fatalf("deferred fork: %+v %v", child, err)
	}
	return parent, child
}

func TestDeferredForkRejectsParentCompactionWithoutNewTurn(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := deferredCompactionManager(t, store, 2)
	parent, child := deferredContextFork(t, m)
	before, err := m.Get(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Compact(ctx, parent.ID); err != nil {
		t.Fatal(err)
	}
	after, err := m.Get(ctx, parent.ID)
	if err != nil || after.Turns != before.Turns {
		t.Fatalf("maintenance added a turn: %+v %v", after, err)
	}
	if _, err := m.Prompt(ctx, child.ID, session.PromptParams{Input: agent.Text("branch")}); !errors.Is(err, session.ErrForkSourceChanged) {
		t.Fatalf("fork accepted changed native history: %v", err)
	}
}

type forkCompactionPages struct {
	*session.FileStore
	reads   atomic.Int32
	missing atomic.Bool
}

func (s *forkCompactionPages) EventsPage(ctx context.Context, id string, after uint64, limit int, maxBytes *int64) (session.EventPage, error) {
	s.reads.Add(1)
	if s.missing.Load() {
		return session.EventPage{NextSeq: after}, nil
	}
	return s.FileStore.EventsPage(ctx, id, after, min(limit, 2), maxBytes)
}

func TestDeferredForkAllowsRenamesAndSettingsAcrossHistoryPages(t *testing.T) {
	ctx := context.Background()
	file, err := session.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := &forkCompactionPages{FileStore: file}
	m := deferredCompactionManager(t, store, 2)
	parent, child := deferredContextFork(t, m)
	for i := range 6 {
		if _, err := m.Rename(ctx, parent.ID, fmt.Sprintf("parent %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.Configure(ctx, parent.ID, agent.ConfigPatch{Model: textPtr("another-model")}); err != nil {
		t.Fatal(err)
	}
	collect(t, m, child.ID, "branch")
	if store.reads.Load() < 3 {
		t.Fatalf("history was not read in bounded pages: %d", store.reads.Load())
	}
	info, err := m.Get(ctx, child.ID)
	if err != nil || info.ForkPending {
		t.Fatalf("unchanged native history rejected: %+v %v", info, err)
	}
}

func TestDeferredForkRejectsUnavailableParentHistory(t *testing.T) {
	for _, mode := range []string{"truncated", "missing"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			var store session.Store = session.NewMemoryStore()
			var pages *forkCompactionPages
			if mode == "missing" {
				file, err := session.NewFileStore(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				pages = &forkCompactionPages{FileStore: file}
				store = pages
			}
			m := deferredCompactionManager(t, store, 2)
			parent, child := deferredContextFork(t, m)
			for i := range 5 {
				if _, err := m.Rename(ctx, parent.ID, fmt.Sprintf("parent %d", i)); err != nil {
					t.Fatal(err)
				}
			}
			if pages != nil {
				pages.missing.Store(true)
			}
			if _, err := m.Prompt(ctx, child.ID, session.PromptParams{Input: agent.Text("branch")}); !errors.Is(err, session.ErrForkSourceChanged) {
				t.Fatalf("unverifiable parent history accepted: %v", err)
			}
		})
	}
}
