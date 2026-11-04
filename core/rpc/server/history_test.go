package server_test

import (
	"context"
	"encoding/json"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/internal/rpcclient"
	"github.com/tomanagle/harness-kit/core/rpc/protocol"
	"github.com/tomanagle/harness-kit/core/rpc/server"
	"github.com/tomanagle/harness-kit/core/runtimes/fake"
	"github.com/tomanagle/harness-kit/core/session"
)

// connectHistoryClient attaches to an existing server without shutting its
// manager down on disconnect, as two dashboard windows would.
func connectHistoryClient(t *testing.T, srv *server.Server) *harness {
	t.Helper()
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(context.Background(), sr, sw) }()
	h := &harness{events: make(chan event.Event, 1024)}
	var err error
	h.c, err = client.Connect(context.Background(), cr, cw, client.Options{
		OnEvent: func(ev event.Event) { h.events <- ev },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cw.Close()
		cr.Close()
		sw.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("server did not stop after disconnect")
		}
		sr.Close()
	})
	return h
}

func waitForUpdate(t *testing.T, h *harness, match func(event.SessionUpdatedData) bool) event.Event {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case ev := <-h.events:
			if d, ok := ev.Data.(event.SessionUpdatedData); ok && match(d) {
				return ev
			}
		case <-timer.C:
			t.Fatal("timed out waiting for session.updated")
		}
	}
}

func TestPersistedHistoryOverRPC(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	newManager := func(reg *agent.Registry) *session.Manager {
		t.Helper()
		st, err := session.NewFileStore(dir)
		if err != nil {
			t.Fatal(err)
		}
		mgr := session.NewManager(reg, session.Options{Store: st, EventLogSize: 2})
		t.Cleanup(func() { mgr.Shutdown(ctx) })
		return mgr
	}

	m1 := newManager(agent.NewRegistry(fake.New()))
	h1 := connectHistoryClient(t, server.New(m1, server.Options{}))
	info, err := h1.c.CreateSession(ctx, protocol.SessionCreateParams{Runtime: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h1.c.Prompt(ctx, protocol.SessionPromptParams{
		SessionID: info.ID, TurnID: "original-turn", Input: agent.Text("remember pineapple"),
	}); err != nil {
		t.Fatal(err)
	}
	h1.waitFor(t, event.TurnCompleted)
	if err := m1.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	original, err := h1.c.Events(ctx, protocol.SessionEventsParams{SessionID: info.ID})
	if err != nil || original.Truncated || len(original.Events) <= 2 {
		t.Fatalf("durable history should exceed the memory ring: %+v, %v", original, err)
	}
	var sawPrompt, sawReply bool
	for i, ev := range original.Events {
		if ev.Seq != uint64(i+1) {
			t.Fatalf("event %d has noncontiguous sequence %d", i, ev.Seq)
		}
		if d, ok := ev.Data.(event.TurnStartedData); ok && d.Text == "remember pineapple" {
			sawPrompt = true
		}
		if d, ok := ev.Data.(event.MessageCompletedData); ok && d.Text == "Echo: remember pineapple" {
			sawReply = true
		}
	}
	if !sawPrompt || !sawReply {
		t.Fatalf("history lost conversation: prompt=%v reply=%v", sawPrompt, sawReply)
	}
	lastSeq := original.Events[len(original.Events)-1].Seq

	// A fresh server can read and rename history even when the runtime is
	// unavailable; neither action may try to start a runtime session.
	m2 := newManager(agent.NewRegistry())
	h2 := connectHistoryClient(t, server.New(m2, server.Options{}))
	var replayed []event.Event
	var after uint64
	for {
		page, err := h2.c.Events(ctx, protocol.SessionEventsParams{SessionID: info.ID, AfterSeq: after, Limit: 3})
		if err != nil || page.Truncated || len(page.Events) > 3 {
			t.Fatalf("replay page after %d: %+v, %v", after, page, err)
		}
		if len(page.Events) == 0 {
			break
		}
		replayed = append(replayed, page.Events...)
		after = page.Events[len(page.Events)-1].Seq
	}
	if !reflect.DeepEqual(replayed, original.Events) {
		t.Fatal("fresh server replay differs from the original event stream")
	}
	renamed, err := h2.c.RenameSession(ctx, info.ID, "Saved conversation")
	if err != nil || renamed.State != session.StateClosed {
		t.Fatalf("rename dormant session: %+v, %v", renamed, err)
	}
	updated := waitForUpdate(t, h2, func(d event.SessionUpdatedData) bool { return d.Title == "Saved conversation" })
	if updated.SessionID != info.ID || updated.Seq != lastSeq+1 {
		t.Fatalf("dormant rename sequence: %+v", updated)
	}
	if err := m2.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	m3 := newManager(agent.NewRegistry(fake.New()))
	h3 := connectHistoryClient(t, server.New(m3, server.Options{}))
	resumed, err := h3.c.RestoreSession(ctx, info.ID)
	if err != nil || resumed.Title != "Saved conversation" || resumed.State != session.StateIdle {
		t.Fatalf("restore saved session: %+v, %v", resumed, err)
	}
	newEvents, err := h3.c.Events(ctx, protocol.SessionEventsParams{SessionID: info.ID, AfterSeq: updated.Seq})
	if err != nil || newEvents.Truncated || len(newEvents.Events) == 0 || newEvents.Events[0].Seq != updated.Seq+1 {
		t.Fatalf("restore must continue sequence: %+v, %v", newEvents, err)
	}
	if d, ok := newEvents.Events[0].Data.(event.SessionStartedData); !ok || !d.Resumed {
		t.Fatalf("restore did not announce a resumed session: %+v", newEvents.Events[0])
	}
	if err := h3.c.DeleteSession(ctx, info.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h3.c.Events(ctx, protocol.SessionEventsParams{SessionID: info.ID}); err == nil {
		t.Fatal("deleted history remains readable")
	}
}

func TestSessionUpdatesBroadcastOverRPC(t *testing.T) {
	ctx := context.Background()
	mgr := session.NewManager(agent.NewRegistry(fake.New()), session.Options{})
	t.Cleanup(func() { mgr.Shutdown(ctx) })
	srv := server.New(mgr, server.Options{})
	a := connectHistoryClient(t, srv)
	b := connectHistoryClient(t, srv)
	info, err := a.c.CreateSession(ctx, protocol.SessionCreateParams{Runtime: "fake", Title: "Original"})
	if err != nil {
		t.Fatal(err)
	}
	renamed, err := a.c.RenameSession(ctx, info.ID, "Shared title")
	if err != nil {
		t.Fatal(err)
	}
	match := func(d event.SessionUpdatedData) bool { return d.Title == "Shared title" }
	ea, eb := waitForUpdate(t, a, match), waitForUpdate(t, b, match)
	ja, _ := json.Marshal(ea)
	jb, _ := json.Marshal(eb)
	if string(ja) != string(jb) || ea.SessionID != info.ID || ea.Seq != renamed.LastSeq {
		t.Fatalf("clients received different rename snapshots: %s / %s", ja, jb)
	}
	d := eb.Data.(event.SessionUpdatedData)
	if d.State != string(session.StateIdle) || d.Turns != 0 || !d.UpdatedAt.Equal(info.UpdatedAt) {
		t.Fatalf("rename snapshot: %+v", d)
	}

	if _, err := a.c.Prompt(ctx, protocol.SessionPromptParams{SessionID: info.ID, TurnID: "observed-turn", Input: agent.Text("hello")}); err != nil {
		t.Fatal(err)
	}
	running := waitForUpdate(t, b, func(d event.SessionUpdatedData) bool { return d.State == string(session.StateRunning) })
	d = running.Data.(event.SessionUpdatedData)
	if d.Title != "Shared title" || d.ActiveTurnID != "observed-turn" || d.Turns != 1 {
		t.Fatalf("running snapshot: %+v", d)
	}
	idle := waitForUpdate(t, b, func(d event.SessionUpdatedData) bool {
		return d.State == string(session.StateIdle) && d.Turns == 1
	})
	d = idle.Data.(event.SessionUpdatedData)
	if idle.Seq <= running.Seq || d.ActiveTurnID != "" || d.LastStatus != event.TurnStatusCompleted {
		t.Fatalf("completed snapshot: %+v", idle)
	}
}
