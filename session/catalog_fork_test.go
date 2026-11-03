package session_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/tomanagle/harness-kit/agent"
	"github.com/tomanagle/harness-kit/event"
	"github.com/tomanagle/harness-kit/runtimes/fake"
	"github.com/tomanagle/harness-kit/session"
	"github.com/tomanagle/harness-kit/settings"
)

func TestCatalogPersistsAndSearchesColdTranscript(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := newManager(t, session.Options{Store: store})
	p := create(t, m, agent.SessionConfig{})
	collect(t, m, p.ID, "a distinctive searchable phrase")
	if _, err = m.Rename(ctx, p.ID, "Catalog item"); err != nil {
		t.Fatal(err)
	}
	if _, err = m.SetTags(ctx, p.ID, []string{" WORK  ", "work", "Alpha"}); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Archive(ctx, p.ID, true); err != nil {
		t.Fatal(err)
	}
	if err = m.Close(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	cold := newManager(t, session.Options{Store: store})
	archived := true
	got, err := cold.Query(ctx, session.ListOptions{Query: "SEARCHABLE PHRASE", SearchTranscript: true, Tags: []string{"work"}, Archived: &archived})
	if err != nil || len(got) != 1 {
		t.Fatalf("query: %+v %v", got, err)
	}
	if !reflect.DeepEqual(session.Tags(got[0]), []string{"alpha", "work"}) || got[0].Metadata["k"] != "v" {
		t.Fatalf("metadata: %+v", got[0])
	}
	if got, err = cold.Query(ctx, session.ListOptions{Query: "searchable"}); err != nil || len(got) != 0 {
		t.Fatal(got, err)
	}
	if _, err = cold.SetTags(ctx, p.ID, nil); err != nil {
		t.Fatal(err)
	}
	info, err := cold.Archive(ctx, p.ID, false)
	if err != nil || info.Archived || len(session.Tags(info)) != 0 || info.Metadata["k"] != "v" {
		t.Fatal(info, err)
	}
	page, err := cold.EventsPage(ctx, p.ID, info.LastSeq-1, 1)
	if err != nil || len(page.Events) != 1 {
		t.Fatal(page, err)
	}
	data := page.Events[0].Data.(event.SessionUpdatedData)
	if data.Archived || data.Metadata["k"] != "v" {
		t.Fatalf("snapshot: %+v", data)
	}
}

func TestForkPreservesParentAndInheritedTranscript(t *testing.T) {
	for _, durable := range []bool{false, true} {
		t.Run(map[bool]string{false: "memory", true: "disk"}[durable], func(t *testing.T) {
			ctx := context.Background()
			var store session.Store = session.NewMemoryStore()
			if durable {
				var err error
				store, err = session.NewFileStore(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
			}
			m := newManager(t, session.Options{Store: store})
			p := create(t, m, agent.SessionConfig{Model: "parent"})
			collect(t, m, p.ID, "parent message")
			before, _ := m.Get(ctx, p.ID)
			parentEvents, _, _ := m.Events(p.ID, 0, 0)
			child, err := m.Fork(ctx, p.ID, session.ForkParams{Title: "Branch", Settings: settings.Patch{"session": []byte(`{"model":"child"}`)}})
			if err != nil {
				t.Fatal(err)
			}
			if child.ID == p.ID || child.RuntimeSessionID == p.RuntimeSessionID || child.ForkPending || child.ForkedFrom != p.ID || child.InheritedTurns != 1 || child.Model != "child" {
				t.Fatalf("fork %+v", child)
			}
			history, _, err := m.Events(child.ID, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			if len(history) <= len(parentEvents) {
				t.Fatalf("missing inherited transcript: %d <= %d", len(history), len(parentEvents))
			}
			for i, ev := range parentEvents {
				copy := history[i]
				copy.SessionID = ev.SessionID
				if !reflect.DeepEqual(copy, ev) {
					t.Fatalf("event %d changed", i)
				}
			}
			collect(t, m, child.ID, "child message")
			after, _ := m.Get(ctx, p.ID)
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("parent changed: before %+v after %+v", before, after)
			}
			if _, err = m.Prompt(ctx, child.ID, session.PromptParams{TurnID: parentEvents[2].TurnID, Input: agent.Text("duplicate")}); !errors.Is(err, session.ErrDuplicateTurn) {
				t.Fatalf("inherited turn id accepted: %v", err)
			}
			if durable {
				if err = m.Close(ctx, child.ID); err != nil {
					t.Fatal(err)
				}
				cold := newManager(t, session.Options{Store: store})
				restored, err := cold.Restore(ctx, child.ID)
				if err != nil || restored.ForkPending || restored.ForkedFrom != p.ID || restored.Turns != 2 {
					t.Fatal(restored, err)
				}
			}
		})
	}
}

type deferredRuntime struct{ *fake.Runtime }

func (r deferredRuntime) Info() agent.Info {
	info := r.Runtime.Info()
	info.Capabilities.ForkDeferred = true
	return info
}
func (r deferredRuntime) Fork(ctx context.Context, source string, cfg agent.SessionConfig, h agent.Handler) (agent.Session, error) {
	s, err := r.Runtime.Fork(ctx, source, cfg, h)
	return &deferredSession{Session: s}, err
}

type deferredSession struct {
	agent.Session
	ready bool
}

func (s *deferredSession) RuntimeSessionID() string {
	if !s.ready {
		return ""
	}
	return s.Session.RuntimeSessionID()
}
func (s *deferredSession) Prompt(ctx context.Context, turn agent.Turn) (agent.TurnResult, error) {
	res, err := s.Session.Prompt(ctx, turn)
	s.ready = true
	return res, err
}

func TestDeferredForkRestoresAndRejectsAdvancedSource(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rt := deferredRuntime{fake.New()}
	m := session.NewManager(agent.NewRegistry(rt), session.Options{Store: store})
	t.Cleanup(func() { _ = m.Shutdown(ctx) })
	p := create(t, m, agent.SessionConfig{})
	collect(t, m, p.ID, "parent")
	child, err := m.Fork(ctx, p.ID, session.ForkParams{})
	if err != nil || !child.ForkPending || child.RuntimeSessionID != "" {
		t.Fatal(child, err)
	}
	if err = m.Close(ctx, child.ID); err != nil {
		t.Fatal(err)
	}
	child, err = m.Restore(ctx, child.ID)
	if err != nil || !child.ForkPending {
		t.Fatal(child, err)
	}
	collect(t, m, child.ID, "branch")
	child, _ = m.Get(ctx, child.ID)
	if child.ForkPending || child.RuntimeSessionID == "" {
		t.Fatal(child)
	}
	stale, err := m.Fork(ctx, p.ID, session.ForkParams{})
	if err != nil {
		t.Fatal(err)
	}
	collect(t, m, p.ID, "advanced")
	if _, err = m.Prompt(ctx, stale.ID, session.PromptParams{Input: agent.Text("must reject")}); !errors.Is(err, session.ErrForkSourceChanged) {
		t.Fatalf("advanced source: %v", err)
	}
}
