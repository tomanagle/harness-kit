package server_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/internal/rpcclient"
	"github.com/tomanagle/harness-kit/core/rpc/protocol"
)

func configString(s string) *string { return &s }

func TestConfigurationOverRPC(t *testing.T) {
	h := start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cap := h.c.Info().Capabilities.Runtimes[0].Capabilities.Configuration["model"]
	if !cap.Startup || !cap.Session || !cap.Turn || cap.Live {
		t.Fatalf("model capabilities: %+v", cap)
	}
	info, err := h.c.CreateSession(ctx, protocol.SessionCreateParams{Runtime: "fake", Model: "base", Effort: "low"})
	if err != nil {
		t.Fatal(err)
	}
	if info.Config == nil || info.Config.Model != "base" || info.Config.Effort != "low" {
		t.Fatalf("startup defaults: %+v", info)
	}
	_, err = h.c.Prompt(ctx, protocol.SessionPromptParams{SessionID: info.ID, Input: agent.Text("[slow] first"),
		Config: &agent.ConfigPatch{Model: configString("one-turn")}})
	if err != nil {
		t.Fatal(err)
	}
	first := h.waitFor(t, event.TurnStarted).Data.(event.TurnStartedData)
	if first.Config == nil || first.Config.Model != "one-turn" || first.Config.Effort != "low" {
		t.Fatalf("first turn: %+v", first.Config)
	}
	updated, err := h.c.ConfigureSession(ctx, info.ID, agent.ConfigPatch{Model: configString("future"), Effort: configString("high")})
	if err != nil || updated.Config == nil || updated.Config.Model != "future" || updated.ActiveTurnID == "" {
		t.Fatalf("configure during turn: %+v %v", updated, err)
	}
	if err := h.c.Cancel(ctx, info.ID); err != nil {
		t.Fatal(err)
	}
	h.waitFor(t, event.TurnCompleted)
	_, err = h.c.Prompt(ctx, protocol.SessionPromptParams{SessionID: info.ID, Input: agent.Text("next")})
	if err != nil {
		t.Fatal(err)
	}
	next := h.waitFor(t, event.TurnStarted).Data.(event.TurnStartedData)
	if next.Config == nil || next.Config.Model != "future" || next.Config.Effort != "high" {
		t.Fatalf("future defaults: %+v", next.Config)
	}
	h.waitFor(t, event.TurnCompleted)
	reset, err := h.c.ConfigureSession(ctx, info.ID, agent.ConfigPatch{Reset: []string{"model", "effort"}})
	if err != nil || reset.Config == nil || reset.Config.Model != "base" || reset.Config.Effort != "low" {
		t.Fatalf("reset defaults: %+v %v", reset, err)
	}
	var rpcError *client.Error
	_, err = h.c.ConfigureSession(ctx, info.ID, agent.ConfigPatch{Reset: []string{"cwd"}})
	if !errors.As(err, &rpcError) || rpcError.Reason != protocol.ReasonInvalidParams {
		t.Fatalf("invalid configuration error: %v", err)
	}
	_, err = h.c.ConfigureSession(ctx, info.ID, agent.ConfigPatch{Reset: []string{"options.failStart"}})
	if !errors.As(err, &rpcError) || rpcError.Reason != protocol.ReasonUnsupported {
		t.Fatalf("startup-only configuration error: %v", err)
	}
	got, err := h.c.GetSession(ctx, info.ID)
	if err != nil || got.Config.Model != "base" {
		t.Fatalf("failed update changed defaults: %+v %v", got, err)
	}
	replay, err := h.c.Events(ctx, protocol.SessionEventsParams{SessionID: info.ID})
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range replay.Events {
		if d, ok := ev.Data.(event.TurnStartedData); ok && ev.TurnID == updated.ActiveTurnID && d.Config.Model != "one-turn" {
			t.Fatalf("accepted turn snapshot was mutated: %+v", d.Config)
		}
	}
}
