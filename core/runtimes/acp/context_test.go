package acp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/event"
)

func contextObservations(h *captureHandler) []event.ContextUsageData {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []event.ContextUsageData
	for _, e := range h.events {
		if d, ok := e.Data.(event.ContextUsageData); ok {
			out = append(out, d)
		}
	}
	return out
}

func TestUsageIsCurrentContextIncludingZero(t *testing.T) {
	h := &captureHandler{}
	s := &Session{rt: &Runtime{}, h: h, id: "native", active: &turnState{}}
	s.update(json.RawMessage(`{"sessionId":"native","update":{"sessionUpdate":"usage_update","used":53000,"size":200000,"cost":{"amount":2.5,"currency":"USD"}}}`))
	s.update(json.RawMessage(`{"sessionId":"native","update":{"sessionUpdate":"usage_update","used":0,"size":200000}}`))
	s.update(json.RawMessage(`{"sessionId":"native","update":{"sessionUpdate":"usage_update","used":-1,"size":200000}}`))
	s.update(json.RawMessage(`{"sessionId":"native","update":{"sessionUpdate":"usage_update","size":200000}}`))
	got := contextObservations(h)
	if len(got) != 2 || got[0].UsedTokens == nil || *got[0].UsedTokens != 53000 || got[1].UsedTokens == nil || *got[1].UsedTokens != 0 || got[1].Basis != "reported" {
		t.Fatalf("observations: %+v", got)
	}
	for _, e := range h.events {
		if d, ok := e.Data.(event.UsageData); ok && d.TotalTokens != 0 {
			t.Fatalf("context counted as spend: %+v", d)
		}
	}
}

func TestIdleContextUpdatesExcludeSessionLoadReplay(t *testing.T) {
	h := &captureHandler{}
	s := &Session{rt: &Runtime{}, h: h, id: "native", replaying: true}
	usage := json.RawMessage(`{"sessionId":"native","update":{"sessionUpdate":"usage_update","used":50000,"size":200000}}`)
	compacted := json.RawMessage(`{"sessionId":"native","update":{"sessionUpdate":"compaction_update","compactionId":"cmp_1","status":"completed"}}`)
	s.update(usage)
	s.update(compacted)
	if len(contextObservations(h)) != 0 {
		t.Fatal("replayed native observations were appended")
	}
	s.replaying = false
	s.update(usage)
	s.update(compacted)
	s.update(usage)
	s.update(compacted) // terminal metadata patch must preserve the newer usage
	got := contextObservations(h)
	if len(got) != 3 || got[0].UsedTokens == nil || *got[0].UsedTokens != 50000 || got[1].UsedTokens != nil || got[2].UsedTokens == nil {
		t.Fatalf("idle context updates: %+v", got)
	}
}

func TestNativeCompactionStatusPrecedesContextInvalidation(t *testing.T) {
	h := &captureHandler{}
	s := &Session{rt: &Runtime{}, h: h, id: "native"}
	s.update(json.RawMessage(`{"sessionId":"native","update":{"sessionUpdate":"usage_update","used":50000,"size":200000}}`))
	s.update(json.RawMessage(`{"sessionId":"native","update":{"sessionUpdate":"compaction_update","compactionId":"cmp_1","status":"in_progress"}}`))
	s.update(json.RawMessage(`{"sessionId":"native","update":{"sessionUpdate":"compaction_update","compactionId":"cmp_1","status":"completed"}}`))
	var order []string
	for _, e := range h.events {
		switch value := e.Data.(type) {
		case event.ContextUsageData:
			if value.UsedTokens != nil {
				order = append(order, "known")
			} else {
				order = append(order, "unknown")
			}
		case event.RuntimeStatusData:
			if value.Status == "compacting" || value.Status == "compacted" {
				order = append(order, value.Status)
			}
		}
	}
	if len(order) != 4 || order[0] != "known" || order[1] != "compacting" || order[2] != "compacted" || order[3] != "unknown" {
		t.Fatalf("lifecycle lost its before observation: %v", order)
	}
}

func TestCompactCommandAdvertisementIsSessionScopedAndReplaceable(t *testing.T) {
	s := &Session{rt: &Runtime{}}
	s.update(json.RawMessage(`{"sessionId":"first","update":{"sessionUpdate":"available_commands_update","availableCommands":[{"name":"compact"}]}}`))
	if s.CanCompact() {
		t.Fatal("session ID was not negotiated")
	}
	s.id = "second"
	if s.CanCompact() {
		t.Fatal("another session's command accepted")
	}
	s.id = "first"
	if !s.CanCompact() {
		t.Fatal("early command advertisement lost")
	}
	s.update(json.RawMessage(`{"sessionId":"first","update":{"sessionUpdate":"available_commands_update","availableCommands":[]}}`))
	if s.CanCompact() {
		t.Fatal("withdrawn command retained")
	}
}

func TestCompactUsesAdvertisedCommandAndWaitsForConfirmation(t *testing.T) {
	for _, mode := range []string{"compact", "compact-missing", "compact-failed", "compact-unknown", "compact-cancelled"} {
		t.Run(mode, func(t *testing.T) {
			h := &captureHandler{}
			s := startMock(t, mockRuntime(t, mode), agent.SessionConfig{}, h)
			if !s.CanCompact() {
				t.Fatal("advertised compact unavailable")
			}
			err := s.Compact(context.Background())
			if (err != nil) != (mode != "compact") {
				t.Fatalf("mode %s: %v", mode, err)
			}
			if mode == "compact-cancelled" && !errors.Is(err, context.Canceled) {
				t.Fatalf("native cancellation: %v", err)
			}
			if h.text() != "" {
				t.Fatal("maintenance became an assistant turn")
			}
			if mode == "compact" {
				observations := contextObservations(h)
				if len(observations) != 2 || observations[0].UsedTokens != nil || observations[1].UsedTokens == nil || *observations[1].UsedTokens != 1200 {
					t.Fatalf("post-compaction occupancy: %+v", observations)
				}
			}
		})
	}
	s := startMock(t, mockRuntime(t, "default"), agent.SessionConfig{}, &captureHandler{})
	if s.CanCompact() || !errors.Is(s.Compact(context.Background()), agent.ErrUnsupported) {
		t.Fatal("unadvertised compact accepted")
	}
}

func TestCompactWaitsForPromptResponseAndAcknowledgesCancellation(t *testing.T) {
	s := startMock(t, mockRuntime(t, "compact-wait"), agent.SessionConfig{}, &captureHandler{})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := s.Compact(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("completion notification prematurely finished operation: %v", err)
	}
	if err := s.live(); err != nil {
		t.Fatalf("acknowledged cancellation destroyed session: %v", err)
	}
}

type sessionReadingHandler struct {
	captureHandler
	read func()
}

func (h *sessionReadingHandler) Emit(ev event.Event) {
	h.read()
	h.captureHandler.Emit(ev)
}

func TestEventCallbacksCanReadSessionWithoutDeadlocking(t *testing.T) {
	h := &sessionReadingHandler{}
	s := &Session{rt: &Runtime{maxRetainedOutput: 1024}, cfg: agent.SessionConfig{RawEvents: true}, h: h, id: "native", active: &turnState{id: "turn", tools: map[string]*toolState{}}}
	readErrors := make(chan error, 10)
	h.read = func() {
		_ = s.RuntimeSessionID()
		_ = s.CanCompact()
		_ = s.ConfigOptions()
		_ = s.NativeDefaults()
		if _, err := s.PrepareTurn(context.Background(), agent.TurnConfig{}); err != nil {
			readErrors <- err
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.update(json.RawMessage(`{"sessionId":"native","update":{"sessionUpdate":"usage_update","used":100,"size":200000}}`))
		s.raw("test", json.RawMessage(`{}`))
		s.lockEvents()
		s.active.itemID, s.active.kind, s.active.text = "message", "agent_message_chunk", "hello"
		s.finishTextLocked()
		s.unlockEvents()
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("adapter held its state mutex during a host event callback")
	}
	select {
	case err := <-readErrors:
		t.Fatalf("read in callback: %v", err)
	default:
	}
	if len(h.events) != 5 || h.events[0].Type != event.Usage || h.events[2].Type != event.ContextUsage || h.events[3].Type != event.RuntimeRaw || h.events[4].Type != event.MessageCompleted {
		t.Fatalf("event batch order changed: %+v", h.events)
	}
}
