package claude

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/event"
)

func TestContextUsesLatestMainRequestNotCumulativeSpend(t *testing.T) {
	rec := &recorder{}
	r := &turnReader{s: &Session{h: rec}, turnID: "turn"}
	r.read(strings.NewReader(`{"type":"assistant","message":{"id":"main1","model":"main-model","usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":200,"cache_creation_input_tokens":30}}}
{"type":"assistant","parent_tool_use_id":"tool","message":{"id":"sub","model":"other-model","usage":{"input_tokens":9000,"output_tokens":500}}}
{"type":"assistant","message":{"id":"main2","model":"main-model","usage":{"input_tokens":40,"output_tokens":10,"cache_read_input_tokens":300,"cache_creation_input_tokens":5}}}
{"type":"result","subtype":"success","usage":{"input_tokens":1000000},"modelUsage":{"main-model":{"inputTokens":2000000,"contextWindow":200000},"other-model":{"contextWindow":1000000}}}
`))
	got := rec.ofType(event.ContextUsage)
	if len(got) != 3 {
		t.Fatalf("context observations = %d, want 3", len(got))
	}
	last := got[2].Data.(event.ContextUsageData)
	if last.UsedTokens == nil || *last.UsedTokens != 355 || last.WindowTokens == nil || *last.WindowTokens != 200000 || last.Basis != "last_request" {
		t.Fatalf("last context: %+v", last)
	}
}

func TestContextCommandAndCompactionBoundary(t *testing.T) {
	rec := &recorder{}
	r := &turnReader{s: &Session{h: rec}}
	r.read(strings.NewReader(`{"type":"assistant","context_usage":{"model":"main","total_tokens":0,"raw_max_tokens":100000}}
{"type":"result","subtype":"success","modelUsage":{"main":{"contextWindow":200000}}}
{"type":"system","subtype":"compact_boundary","compact_metadata":{"pre_tokens":100000}}
`))
	got := rec.ofType(event.ContextUsage)
	if len(got) != 2 {
		t.Fatalf("observations: %+v", got)
	}
	before, after := got[0].Data.(event.ContextUsageData), got[1].Data.(event.ContextUsageData)
	if before.UsedTokens == nil || *before.UsedTokens != 0 || before.WindowTokens == nil || *before.WindowTokens != 100000 || before.Basis != "reported" {
		t.Fatalf("reported effective window: %+v", before)
	}
	if after.UsedTokens != nil || after.Basis != "unknown" {
		t.Fatalf("compaction retained stale occupancy: %+v", after)
	}
}

func TestNativeCompactionStatusesExcludeSubagents(t *testing.T) {
	rec := &recorder{}
	r := &turnReader{s: &Session{h: rec}}
	r.read(strings.NewReader(`{"type":"system","subtype":"status","status":"compacting","parent_tool_use_id":"subagent"}
{"type":"system","subtype":"compact_boundary","agent_id":"subagent"}
{"type":"system","subtype":"status","status":"compacting"}
{"type":"system","subtype":"compact_boundary"}
`))
	statuses := rec.ofType(event.RuntimeStatus)
	if len(statuses) != 2 || statuses[0].Data.(event.RuntimeStatusData).Status != "compacting" || statuses[1].Data.(event.RuntimeStatusData).Status != "compacted" || len(rec.ofType(event.ContextUsage)) != 1 {
		t.Fatalf("main-session lifecycle: %+v", statuses)
	}
}

func TestNativeCompactionStatusPrecedesContextInvalidation(t *testing.T) {
	rec := &recorder{}
	r := &turnReader{s: &Session{h: rec}}
	r.read(strings.NewReader(`{"type":"assistant","message":{"id":"main","usage":{"input_tokens":50000}}}
{"type":"system","subtype":"status","status":"compacting"}
{"type":"system","subtype":"compact_boundary"}
`))
	var order []string
	for _, e := range rec.events {
		switch value := e.Data.(type) {
		case event.ContextUsageData:
			if value.UsedTokens != nil {
				order = append(order, "known")
			} else {
				order = append(order, "unknown")
			}
		case event.RuntimeStatusData:
			order = append(order, value.Status)
		}
	}
	if len(order) != 4 || order[0] != "known" || order[1] != "compacting" || order[2] != "compacted" || order[3] != "unknown" {
		t.Fatalf("lifecycle lost its before observation: %v", order)
	}
}

func TestCompactNativeConfirmation(t *testing.T) {
	for _, mode := range []string{"", "missing-command", "missing-boundary", "failed", "bad-exit"} {
		t.Run(mode, func(t *testing.T) {
			rt, args := newFake(t, TestedVersion, "HK_FAKE_CLAUDE_COMPACT="+mode)
			rec := &recorder{}
			live, err := rt.Start(context.Background(), agent.SessionConfig{ResumeID: "saved-session"}, rec)
			if err != nil {
				t.Fatal(err)
			}
			defer live.Close(context.Background())
			s := live.(*Session)
			if !s.CanCompact() {
				t.Fatal("resumed current CLI cannot compact")
			}
			err = s.Compact(context.Background())
			if (err != nil) != (mode != "") {
				t.Fatalf("mode %q: %v", mode, err)
			}
			calls := invocations(t, args)
			if len(calls) != 1 {
				t.Fatalf("calls: %v", calls)
			}
			if resume, _ := flag(calls[0], "--resume"); resume != "saved-session" {
				t.Fatalf("missing resume: %v", calls[0])
			}
			if len(rec.ofType(event.MessageCompleted)) != 0 {
				t.Fatal("maintenance appeared as a conversation turn")
			}
		})
	}
}

func TestCompactUnavailableAndCancellation(t *testing.T) {
	for _, cfg := range []struct{ version, resume string }{{"2.1.267", "saved"}, {TestedVersion, ""}} {
		rt, _ := newFake(t, cfg.version)
		live, err := rt.Start(context.Background(), agent.SessionConfig{ResumeID: cfg.resume}, &recorder{})
		if err != nil {
			t.Fatal(err)
		}
		s := live.(*Session)
		if s.CanCompact() || !errors.Is(s.Compact(context.Background()), agent.ErrUnsupported) {
			t.Fatal("unsupported compaction was accepted")
		}
		live.Close(context.Background())
	}
	rt, _ := newFake(t, TestedVersion, "HK_FAKE_CLAUDE_COMPACT=wait")
	live, err := rt.Start(context.Background(), agent.SessionConfig{ResumeID: "saved"}, &recorder{})
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := live.(*Session).Compact(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation: %v", err)
	}
}
