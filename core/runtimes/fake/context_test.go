package fake

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/permission"
)

type contextRecorder struct {
	mu     sync.Mutex
	events []event.Event
}

func (r *contextRecorder) Emit(e event.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func (r *contextRecorder) RequestApproval(context.Context, permission.Request) (permission.Decision, error) {
	return permission.Decision{Outcome: permission.Deny}, nil
}

func (r *contextRecorder) snapshots() []event.ContextUsageData {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []event.ContextUsageData
	for _, e := range r.events {
		if value, ok := e.Data.(event.ContextUsageData); ok {
			out = append(out, value)
		}
	}
	return out
}

func TestContextFixtureIsOptIn(t *testing.T) {
	h := &contextRecorder{}
	live, err := New().Start(context.Background(), agent.SessionConfig{}, h)
	if err != nil {
		t.Fatal(err)
	}
	s := live.(*Session)
	if s.CanCompact() || !errors.Is(s.Compact(context.Background()), agent.ErrUnsupported) {
		t.Fatal("default fake enabled compaction")
	}
	if len(h.events) != 0 {
		t.Fatalf("default startup emitted events: %+v", h.events)
	}
	result, err := s.Prompt(context.Background(), agent.Turn{ID: "turn", Input: []agent.InputPart{{Text: "hello"}}})
	if err != nil || result.Status != event.TurnStatusCompleted {
		t.Fatalf("default prompt: %+v, %v", result, err)
	}
	if len(h.snapshots()) != 0 {
		t.Fatal("default fake emitted context observations")
	}
}

func TestContextFixturePromptAndCompaction(t *testing.T) {
	h := &contextRecorder{}
	live, err := New().Start(context.Background(), agent.SessionConfig{Options: map[string]any{"contextTokens": 85000, "contextWindowTokens": 100000, "compactedTokens": 1200}}, h)
	if err != nil {
		t.Fatal(err)
	}
	s := live.(*Session)
	if !s.CanCompact() {
		t.Fatal("explicit fixture unavailable")
	}
	start := h.snapshots()
	if len(start) != 1 || start[0].UsedTokens == nil || *start[0].UsedTokens != 85000 || start[0].WindowTokens == nil || *start[0].WindowTokens != 100000 {
		t.Fatalf("startup context: %+v", start)
	}
	before := len(h.events)
	if err := s.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.events) != before+1 || h.events[before].Type != event.ContextUsage {
		t.Fatalf("compaction emitted conversational events: %+v", h.events[before:])
	}
	if _, err := s.Prompt(context.Background(), agent.Turn{ID: "turn", Input: []agent.InputPart{{Text: "hello"}}}); err != nil {
		t.Fatal(err)
	}
	got := h.snapshots()
	if len(got) != 3 || *got[1].UsedTokens != 1200 || *got[2].UsedTokens != 85000 || got[2].Basis != "reported" {
		t.Fatalf("fixture observations: %+v", got)
	}
	if last := h.events[len(h.events)-1]; last.Type != event.ContextUsage || last.TurnID != "turn" {
		t.Fatalf("prompt context attribution: %+v", last)
	}
}

func TestContextFixtureUnknownAndZeroAreDistinct(t *testing.T) {
	for _, options := range []map[string]any{{"contextWindowTokens": 100000}, {"contextTokens": 0}} {
		h := &contextRecorder{}
		live, err := New().Start(context.Background(), agent.SessionConfig{Options: options}, h)
		if err != nil {
			t.Fatal(err)
		}
		first := h.snapshots()[0]
		if _, exists := options["contextTokens"]; exists {
			if first.UsedTokens == nil || *first.UsedTokens != 0 || first.WindowTokens != nil {
				t.Fatalf("zero with unknown capacity: %+v", first)
			}
		} else if first.UsedTokens != nil || first.WindowTokens == nil {
			t.Fatalf("unknown with known capacity: %+v", first)
		}
		if err := live.(*Session).Compact(context.Background()); err != nil {
			t.Fatal(err)
		}
		after := h.snapshots()[1]
		if after.UsedTokens == nil || *after.UsedTokens != 0 {
			t.Fatalf("default compacted tokens: %+v", after)
		}
	}
}

func TestContextFixtureCompactionCancellation(t *testing.T) {
	h := &contextRecorder{}
	live, err := New().Start(context.Background(), agent.SessionConfig{Options: map[string]any{"contextTokens": 50000, "compactionDelayMs": 1000}}, h)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := live.(*Session).Compact(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancelled maintenance: %v", err)
	}
	if got := h.snapshots(); len(got) != 1 || *got[0].UsedTokens != 50000 {
		t.Fatalf("cancelled maintenance changed context: %+v", got)
	}
}

func TestContextFixtureValidation(t *testing.T) {
	for _, options := range []map[string]any{
		{"contextTokens": -1}, {"contextWindowTokens": 0}, {"contextWindowTokens": -1}, {"compactedTokens": -1}, {"compactionDelayMs": -1}, {"compactionDelayMs": 60001},
	} {
		cfg := agent.SessionConfig{Options: options}
		r := New()
		if err := r.ValidateConfig(cfg); !errors.Is(err, agent.ErrInvalidConfig) {
			t.Fatalf("validation accepted %v: %v", options, err)
		}
		if _, err := r.Start(context.Background(), cfg, &contextRecorder{}); !errors.Is(err, agent.ErrInvalidConfig) {
			t.Fatalf("start accepted %v: %v", options, err)
		}
	}
}
