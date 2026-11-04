package server_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/rpc/client"
	"github.com/tomanagle/harness-kit/core/rpc/protocol"
	"github.com/tomanagle/harness-kit/core/rpc/server"
	"github.com/tomanagle/harness-kit/core/settings"
)

func patchSettings(t *testing.T, input string) settings.Patch {
	t.Helper()
	var p settings.Patch
	if err := json.Unmarshal([]byte(input), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUnifiedSettingsOverRPC(t *testing.T) {
	h := start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if !h.c.Info().Capabilities.Settings {
		t.Fatal("settings capability missing")
	}
	initial, err := h.c.Settings(ctx, settings.Target{})
	if err != nil {
		t.Fatal(err)
	}
	if initial.Fields["memory.evictAfterIdleMs"].Unit != "milliseconds" {
		t.Fatalf("missing UI descriptors: %+v", initial.Fields)
	}
	updated, err := h.c.ConfigureSettings(ctx, settings.ConfigureParams{
		Patch:            patchSettings(t, `{"process":{"shutdownTimeoutMs":5000},"history":{"maxPageEvents":2},"session":{"runtime":"fake","model":"host-default"}}`),
		ExpectedRevision: &initial.Revision,
	})
	if err != nil || updated.Revision <= initial.Revision || updated.Values.History.MaxPageEvents != 2 || updated.Values.Process.ShutdownTimeoutMs != 5000 {
		t.Fatalf("configure: %+v %v", updated, err)
	}
	_, err = h.c.ConfigureSettings(ctx, settings.ConfigureParams{Patch: patchSettings(t, `{"history":{"maxPageEvents":3}}`), ExpectedRevision: &initial.Revision})
	var rpcError *client.Error
	if !errors.As(err, &rpcError) || rpcError.Reason != protocol.ReasonSettingsConflict {
		t.Fatalf("stale update: %v", err)
	}
	_, err = h.c.ConfigureSettings(ctx, settings.ConfigureParams{Patch: patchSettings(t, `{"history":{"maxPageEvents":0},"approvals":{"timeoutMs":7}}`)})
	if !errors.As(err, &rpcError) || rpcError.Reason != protocol.ReasonInvalidParams {
		t.Fatalf("invalid update: %v", err)
	}
	after, err := h.c.Settings(ctx, settings.Target{})
	if err != nil || after.Revision != updated.Revision || after.Values.Approvals.TimeoutMs != initial.Values.Approvals.TimeoutMs {
		t.Fatalf("non-atomic rejection: %+v %v", after, err)
	}
	_, err = h.c.ConfigureSettings(ctx, settings.ConfigureParams{Patch: patchSettings(t, `{"process":{"store":"another-store"},"history":{"maxPageEvents":8}}`)})
	if !errors.As(err, &rpcError) || rpcError.Reason != protocol.ReasonRestartRequired {
		t.Fatalf("boot field update: %v", err)
	}
	after, _ = h.c.Settings(ctx, settings.Target{})
	if after.Values.History.MaxPageEvents != 2 {
		t.Fatal("restart-required patch was partially applied")
	}

	info, err := h.c.CreateSession(ctx, protocol.SessionCreateParams{Settings: patchSettings(t, `{"session":{"effort":"low"}}`)})
	if err != nil || info.Config == nil || info.Config.Model != "host-default" || info.Runtime != "fake" {
		t.Fatalf("global defaults on create: %+v %v", info, err)
	}
	scoped, err := h.c.Settings(ctx, settings.Target{SessionID: info.ID})
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.c.ConfigureSettings(ctx, settings.ConfigureParams{Target: scoped.Target, Patch: patchSettings(t, `{"session":{"model":"session-default"}}`), ExpectedRevision: &scoped.Revision})
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.c.Prompt(ctx, protocol.SessionPromptParams{SessionID: info.ID, Input: agent.Text("hello settings"), Settings: patchSettings(t, `{"session":{"model":"one-turn"}}`)})
	if err != nil {
		t.Fatal(err)
	}
	started := h.waitFor(t, event.TurnStarted).Data.(event.TurnStartedData)
	if started.Config == nil || started.Config.Model != "one-turn" || started.Config.Effort != "low" {
		t.Fatalf("turn settings: %+v", started)
	}
	h.waitFor(t, event.TurnCompleted)
	got, err := h.c.GetSession(ctx, info.ID)
	if err != nil || got.Config.Model != "session-default" {
		t.Fatalf("one-turn update leaked: %+v %v", got, err)
	}
	page, err := h.c.EventsPage(ctx, protocol.SessionEventsParams{SessionID: info.ID})
	if err != nil || len(page.Events) != 2 || !page.HasMore || page.NextSeq != page.Events[1].Seq {
		t.Fatalf("bounded page: %+v %v", page, err)
	}
	next, err := h.c.EventsPage(ctx, protocol.SessionEventsParams{SessionID: info.ID, AfterSeq: page.NextSeq, Limit: 1})
	if err != nil || len(next.Events) != 1 || next.Events[0].Seq <= page.NextSeq {
		t.Fatalf("next page: %+v %v", next, err)
	}
}

func TestSettingsNotifyOtherConnections(t *testing.T) {
	h := start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- server.New(h.mgr, server.Options{}).Serve(ctx, sr, sw) }()
	notifications := make(chan settings.Snapshot, 10)
	other, err := client.Connect(ctx, cr, cw, client.Options{OnSettings: func(snapshot settings.Snapshot) { notifications <- snapshot }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cw.Close(); <-done; sw.Close() })
	before, err := other.Settings(ctx, settings.Target{})
	if err != nil {
		t.Fatal(err)
	}
	applied, err := h.c.ConfigureSettings(ctx, settings.ConfigureParams{Patch: patchSettings(t, `{"history":{"maxPageEvents":17}}`), ExpectedRevision: &before.Revision})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case update := <-notifications:
		if update.Revision != applied.Revision || update.Values.History.MaxPageEvents != 17 {
			t.Fatalf("notification: %+v", update)
		}
	case <-ctx.Done():
		t.Fatal("missing settings.updated notification")
	}
}
