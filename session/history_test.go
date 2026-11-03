package session_test

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tomanagle/harness-kit/agent"
	"github.com/tomanagle/harness-kit/event"
	"github.com/tomanagle/harness-kit/permission"
	"github.com/tomanagle/harness-kit/session"
)

func historyStore(t *testing.T, dir string) *session.FileStore {
	t.Helper()
	st, err := session.NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestHistorySurvivesMemoryEvictionAndRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dir := t.TempDir()
	m1 := newManager(t, session.Options{Store: historyStore(t, dir), EventLogSize: 2})
	info := create(t, m1, agent.SessionConfig{})
	sub := m1.Subscribe(event.ForSession(info.ID), 0)
	defer sub.Close()
	input := append(agent.Text("[approve] remembered conversation"), agent.InputPart{Type: "image", Path: "diagram.png"})
	turn, err := m1.Prompt(ctx, info.ID, session.PromptParams{TurnID: "first-turn", Input: input})
	if err != nil {
		t.Fatal(err)
	}
	approved := false
	for !approved {
		select {
		case ev := <-sub.C:
			if req, ok := ev.Data.(permission.Request); ok {
				if err := m1.RespondApproval(req.ID, permission.Decision{Outcome: permission.Allow}); err != nil {
					t.Fatal(err)
				}
				approved = true
			}
		case <-ctx.Done():
			t.Fatal("approval event never arrived")
		}
	}
	if res, err := turn.Wait(ctx); err != nil || res.Status != event.TurnStatusCompleted {
		t.Fatalf("turn: %+v %v", res, err)
	}
	if err := m1.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	want, truncated, err := m1.Events(info.ID, 0, 0)
	if err != nil || truncated || len(want) < 10 {
		t.Fatalf("full history with two-event cache: count=%d truncated=%v err=%v", len(want), truncated, err)
	}

	m2 := newManager(t, session.Options{Store: historyStore(t, dir), EventLogSize: 2})
	// Reading history must not require restoring (or starting) a runtime.
	got, truncated, err := m2.Events(info.ID, 0, 0)
	if err != nil || truncated || !reflect.DeepEqual(got, want) {
		t.Fatalf("history changed after restart: count=%d want=%d truncated=%v err=%v", len(got), len(want), truncated, err)
	}
	var prompt, reply, request, resolved, tool bool
	for i, ev := range got {
		if ev.Seq != uint64(i+1) {
			t.Fatalf("history sequence gap: index=%d seq=%d", i, ev.Seq)
		}
		switch d := ev.Data.(type) {
		case event.TurnStartedData:
			prompt = d.Text == "[approve] remembered conversation" && reflect.DeepEqual(d.Input, input)
		case event.MessageCompletedData:
			reply = d.Text == "Echo: remembered conversation"
		case permission.Request:
			request = d.Command == "echo hello" && d.TurnID == turn.ID
		case event.ApprovalResolvedData:
			resolved = d.Decision.Outcome == permission.Allow && d.Source == permission.SourceHost
		case event.ToolCompletedData:
			tool = d.Status == event.ToolStatusCompleted && d.Output == "hello\n" && d.ExitCode != nil && *d.ExitCode == 0
		}
	}
	if !prompt || !reply || !request || !resolved || !tool {
		t.Fatalf("typed replay incomplete: prompt=%v reply=%v request=%v resolved=%v tool=%v", prompt, reply, request, resolved, tool)
	}
	page, truncated, err := m2.Events(info.ID, got[2].Seq, 3)
	if err != nil || truncated || !reflect.DeepEqual(page, got[3:6]) {
		t.Fatalf("history pagination: %+v truncated=%v err=%v", page, truncated, err)
	}
	if _, err := m2.Restore(ctx, info.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := m2.Prompt(ctx, info.ID, session.PromptParams{TurnID: turn.ID, Input: agent.Text("duplicate")}); !errors.Is(err, session.ErrDuplicateTurn) {
		t.Fatalf("reused historical turn ID was accepted: %v", err)
	}
	newEvents, truncated, err := m2.Events(info.ID, got[len(got)-1].Seq, 0)
	if err != nil || truncated || len(newEvents) == 0 || newEvents[0].Seq != got[len(got)-1].Seq+1 {
		t.Fatalf("restored sequence: %+v truncated=%v err=%v", newEvents, truncated, err)
	}
}

func TestLegacyHistoryPreservesSequenceAndReportsMissingTranscript(t *testing.T) {
	ctx := context.Background()
	st := historyStore(t, t.TempDir())
	rec := session.Record{Info: session.Info{ID: "ses_legacy", Runtime: "fake", RuntimeSessionID: "fake-legacy",
		State: session.StateClosed, Turns: 3, LastSeq: 42, Title: "Legacy history"}}
	if err := st.Put(ctx, rec); err != nil {
		t.Fatal(err)
	}
	m := newManager(t, session.Options{Store: st, EventLogSize: 2})
	if got, truncated, err := m.Events(rec.Info.ID, 0, 0); err != nil || !truncated || len(got) != 0 {
		t.Fatalf("legacy transcript: %+v truncated=%v err=%v", got, truncated, err)
	}
	if _, err := m.Restore(ctx, rec.Info.ID); err != nil {
		t.Fatal(err)
	}
	got, truncated, err := m.Events(rec.Info.ID, 42, 0)
	if err != nil || truncated || len(got) == 0 || got[0].Seq != 43 {
		t.Fatalf("legacy continuation: %+v truncated=%v err=%v", got, truncated, err)
	}
	if _, truncated, err := m.Events(rec.Info.ID, 0, 0); err != nil || !truncated {
		t.Fatalf("missing legacy prefix was forgotten: truncated=%v err=%v", truncated, err)
	}
}

func TestHistoryRecoversSnapshotAheadOfRecord(t *testing.T) {
	ctx := context.Background()
	st := historyStore(t, t.TempDir())
	rec := session.Record{Info: session.Info{ID: "ses_recover", Runtime: "fake", State: session.StateRunning,
		ActiveTurnID: "stale-turn", Title: "Old title", LastSeq: 2}}
	if err := st.Put(ctx, rec); err != nil {
		t.Fatal(err)
	}
	updated := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, ev := range []event.Event{
		{SessionID: rec.Info.ID, Seq: 3, Type: event.SessionUpdated, Time: updated, Data: event.SessionUpdatedData{
			Title: "Recovered title", State: string(session.StateIdle), RuntimeSessionID: "fake-recovered",
			Turns: 2, LastStatus: event.TurnStatusCompleted, UpdatedAt: updated}},
		{SessionID: rec.Info.ID, Seq: 4, Type: event.RuntimeStatus, Time: updated, Data: event.RuntimeStatusData{Status: "ready"}},
	} {
		if err := st.AppendEvent(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	m := newManager(t, session.Options{Store: st, EventLogSize: 1})
	got, err := m.Get(ctx, rec.Info.ID)
	if err != nil || got.Title != "Recovered title" || got.LastSeq != 4 || got.RuntimeSessionID != "fake-recovered" || got.Turns != 2 || got.LastStatus != event.TurnStatusCompleted || !got.UpdatedAt.Equal(updated) {
		t.Fatalf("recovered record: %+v %v", got, err)
	}
	if got.State != session.StateClosed || got.ActiveTurnID != "" {
		t.Fatalf("dormant recovered state: %+v", got)
	}
	// The old record did not contain a resumable runtime ID or any turns.
	// Restore must use the snapshot to recover both and continue past seq 4.
	if _, err := m.Restore(ctx, rec.Info.ID); err != nil {
		t.Fatal(err)
	}
	evs, truncated, err := m.Events(rec.Info.ID, 4, 0)
	if err != nil || truncated || len(evs) == 0 || evs[0].Seq != 5 {
		t.Fatalf("recovered continuation: %+v truncated=%v err=%v", evs, truncated, err)
	}
}

func TestDormantHistoryDoesNotReportAnActiveRuntime(t *testing.T) {
	ctx := context.Background()
	st := historyStore(t, t.TempDir())
	for _, state := range []session.State{session.StateStarting, session.StateIdle, session.StateRunning, session.StateClosing, session.StateClosed, session.StateFailed} {
		id := "ses_" + string(state)
		if err := st.Put(ctx, session.Record{Info: session.Info{ID: id, State: state, ActiveTurnID: "stale-turn"}}); err != nil {
			t.Fatal(err)
		}
	}
	m := newManager(t, session.Options{Store: st})
	list, err := m.List(ctx)
	if err != nil || len(list) != 6 {
		t.Fatalf("list: %+v %v", list, err)
	}
	for _, listed := range list {
		want := session.StateClosed
		if listed.ID == "ses_failed" {
			want = session.StateFailed
		}
		got, err := m.Get(ctx, listed.ID)
		if err != nil || got.State != want || listed.State != want || got.ActiveTurnID != "" || listed.ActiveTurnID != "" {
			t.Fatalf("dormant state: get=%+v list=%+v err=%v", got, listed, err)
		}
	}
}

type failOneEventStore struct {
	*session.FileStore
	failed atomic.Bool
}

func (s *failOneEventStore) AppendEvent(ctx context.Context, ev event.Event) error {
	if ev.Type == event.MessageDelta && !s.failed.Swap(true) {
		return errors.New("simulated disk write failure")
	}
	return s.FileStore.AppendEvent(ctx, ev)
}

func TestHistoryWriteFailureIsVisibleAndRecoverableWhileLive(t *testing.T) {
	st := &failOneEventStore{FileStore: historyStore(t, t.TempDir())}
	m := newManager(t, session.Options{Store: st, EventLogSize: 64})
	info := create(t, m, agent.SessionConfig{})
	streamed := collect(t, m, info.ID, "keep the visible conversation")
	var warnings int
	for _, ev := range streamed {
		if d, ok := ev.Data.(event.ErrorData); ok && d.Code == "event_store_error" {
			warnings++
		}
	}
	if warnings != 1 {
		t.Fatalf("expected one visible persistence warning, got %d", warnings)
	}
	got, truncated, err := m.Events(info.ID, 0, 0)
	if err != nil || truncated {
		t.Fatalf("merged live history: truncated=%v err=%v", truncated, err)
	}
	for i, ev := range got {
		if ev.Seq != uint64(i+1) {
			t.Fatalf("live cache failed to repair missing disk event: %+v", got)
		}
	}
	if _, truncated, err := st.Events(context.Background(), info.ID, 0, 0); err != nil || !truncated {
		t.Fatalf("disk gap must remain visible after failed write: truncated=%v err=%v", truncated, err)
	}
}
