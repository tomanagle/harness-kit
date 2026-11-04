package server_test

import (
	"context"
	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/rpc/protocol"
	"github.com/tomanagle/harness-kit/core/session"
	"testing"
)

func TestCatalogAndForkClients(t *testing.T) {
	h := start(t)
	ctx := context.Background()
	p, err := h.c.CreateSession(ctx, protocol.SessionCreateParams{Runtime: "fake", Title: "Source"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.c.Prompt(ctx, protocol.SessionPromptParams{SessionID: p.ID, Input: agent.Text("conversation")}); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, event.TurnCompleted)
	if _, err = h.c.SetSessionTags(ctx, p.ID, []string{"Work"}); err != nil {
		t.Fatal(err)
	}
	if _, err = h.c.ArchiveSession(ctx, p.ID, true); err != nil {
		t.Fatal(err)
	}
	archived := true
	list, err := h.c.QuerySessions(ctx, session.ListOptions{Query: "source", Tags: []string{"work"}, Archived: &archived})
	if err != nil || len(list) != 1 {
		t.Fatal(list, err)
	}
	child, err := h.c.ForkSession(ctx, p.ID, session.ForkParams{Title: "Branch"})
	if err != nil {
		t.Fatal(err)
	}
	if child.Archived || child.ForkedFrom != p.ID || child.RuntimeSessionID == p.RuntimeSessionID {
		t.Fatal(child)
	}
	history, err := h.c.Events(ctx, protocol.SessionEventsParams{SessionID: child.ID})
	if err != nil || len(history.Events) < 5 {
		t.Fatal(history, err)
	}
}
