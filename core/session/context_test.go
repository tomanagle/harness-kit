package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/session"
	"github.com/tomanagle/harness-kit/core/settings"
)

type contextRuntime struct {
	compact func(context.Context, agent.Handler) error
	prompt  func(context.Context, agent.Handler) error
	started chan struct{}
	observe func(agent.Handler)
	prompts atomic.Int32
}

func (r *contextRuntime) Info() agent.Info {
	return agent.Info{Name: "context-test", Capabilities: agent.Capabilities{Resume: true, Usage: true, Compaction: true, ContextUsage: true}}
}
func (r *contextRuntime) Discover(context.Context) (agent.Discovery, error) {
	return agent.Discovery{}, nil
}
func (r *contextRuntime) Start(_ context.Context, _ agent.SessionConfig, h agent.Handler) (agent.Session, error) {
	if r.observe != nil {
		r.observe(h)
	}
	used, window := int64(80), int64(100)
	h.Emit(event.Event{Type: event.ContextUsage, Data: event.ContextUsageData{UsedTokens: &used, WindowTokens: &window, Basis: "reported"}})
	return &contextSession{r: r, h: h}, nil
}

type contextSession struct {
	r *contextRuntime
	h agent.Handler
}

func (s *contextSession) RuntimeSessionID() string    { return "context-native" }
func (s *contextSession) Close(context.Context) error { return nil }
func (s *contextSession) CanCompact() bool            { return true }
func (s *contextSession) Compact(ctx context.Context) error {
	if s.r.started != nil {
		close(s.r.started)
	}
	return s.r.compact(ctx, s.h)
}
func (s *contextSession) Prompt(ctx context.Context, _ agent.Turn) (agent.TurnResult, error) {
	s.r.prompts.Add(1)
	if s.r.prompt != nil {
		return agent.TurnResult{}, s.r.prompt(ctx, s.h)
	}
	return agent.TurnResult{}, nil
}
func contextManager(t *testing.T, r *contextRuntime, store session.Store) *session.Manager {
	t.Helper()
	m := session.NewManager(agent.NewRegistry(r), session.Options{Store: store})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := m.Shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return m
}
func contextCreate(t *testing.T, m *session.Manager, p settings.Patch) session.Info {
	t.Helper()
	info, err := m.Create(context.Background(), session.CreateParams{Runtime: "context-test", Settings: p})
	if err != nil {
		t.Fatal(err)
	}
	return info
}
func contextEvents(t *testing.T, m *session.Manager, id string) []event.ContextCompactionData {
	t.Helper()
	events, _, err := m.Events(id, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []event.ContextCompactionData
	for _, ev := range events {
		if d, ok := ev.Data.(event.ContextCompactionData); ok {
			out = append(out, d)
		}
	}
	return out
}

func TestCompactionCancellationReservesSessionUntilAdapterStops(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	r := &contextRuntime{started: make(chan struct{}), compact: func(ctx context.Context, _ agent.Handler) error { <-ctx.Done(); <-release; return ctx.Err() }}
	store, err := session.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := contextManager(t, r, store)
	info := contextCreate(t, m, nil)
	request, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := m.Compact(request, info.ID); done <- err }()
	select {
	case <-r.started:
	case <-time.After(time.Second):
		t.Fatal("compact did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("request cancellation blocked")
	}
	state, err := m.Context(context.Background(), info.ID)
	if err != nil || state.State != session.StateCompacting || state.CanCompact || !state.Usage.Stale {
		t.Fatal(state, err)
	}
	if _, err = m.Prompt(context.Background(), info.ID, session.PromptParams{}); !errors.Is(err, session.ErrBusy) {
		t.Fatalf("prompt: %v", err)
	}
	if _, err = m.Compact(context.Background(), info.ID); !errors.Is(err, session.ErrBusy) {
		t.Fatalf("second compact: %v", err)
	}
	if err = m.Evict(context.Background(), info.ID); !errors.Is(err, session.ErrBusy) {
		t.Fatalf("evict: %v", err)
	}
	if err = m.Suspend(context.Background(), info.ID); !errors.Is(err, session.ErrBusy) {
		t.Fatalf("suspend: %v", err)
	}
	if _, err = m.Fork(context.Background(), info.ID, session.ForkParams{}); !errors.Is(err, session.ErrBusy) {
		t.Fatalf("fork: %v", err)
	}
	once.Do(func() { close(release) })
	deadline := time.Now().Add(time.Second)
	for {
		state, err = m.Context(context.Background(), info.ID)
		if err != nil {
			t.Fatal(err)
		}
		if state.State == session.StateIdle {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("session did not become idle")
		}
		time.Sleep(time.Millisecond)
	}
	if state.Usage.UsedTokens != nil || !state.Usage.Stale {
		t.Fatal(state)
	}
	events := contextEvents(t, m, info.ID)
	if len(events) != 2 || events[0].Status != "started" || events[1].Status != "cancelled" || events[0].OperationID != events[1].OperationID {
		t.Fatal(events)
	}
}

func TestContextAndCompactionPersistWithoutConversationTurns(t *testing.T) {
	store, err := session.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r := &contextRuntime{compact: func(_ context.Context, h agent.Handler) error {
		zero, window := int64(0), int64(100)
		h.Emit(event.Event{Type: event.ContextUsage, Data: event.ContextUsageData{UsedTokens: &zero, WindowTokens: &window, Basis: "reported"}})
		return nil
	}}
	m := contextManager(t, r, store)
	info := contextCreate(t, m, nil)
	checkpoint, err := store.Get(context.Background(), info.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := m.Compact(context.Background(), info.ID)
	if err != nil || got.Usage.UsedTokens == nil || *got.Usage.UsedTokens != 0 || got.Usage.Stale || !got.CanCompact {
		t.Fatal(got, err)
	}
	*got.Usage.UsedTokens = 900
	live, _ := m.Get(context.Background(), info.ID)
	if live.Turns != 0 || live.Context == nil || *live.Context.UsedTokens != 0 {
		t.Fatal(live)
	}
	events := contextEvents(t, m, info.ID)
	if len(events) != 2 || events[1].Status != "completed" || events[1].Reason != "manual" || *events[1].Before.UsedTokens != 80 || *events[1].After.UsedTokens != 0 {
		t.Fatal(events)
	}
	// Simulate a crash after JSONL sync but before the record checkpoint update.
	if err = store.Put(context.Background(), checkpoint); err != nil {
		t.Fatal(err)
	}
	reopened := contextManager(t, r, store)
	cached, err := reopened.Context(context.Background(), info.ID)
	if err != nil || cached.CanCompact || cached.Usage.UsedTokens == nil || *cached.Usage.UsedTokens != 0 || !cached.Usage.Stale {
		t.Fatal(cached, err)
	}
	replayed := contextEvents(t, reopened, info.ID)
	if len(replayed) != 2 || replayed[1].OperationID != events[1].OperationID || *replayed[1].After.UsedTokens != 0 {
		t.Fatal(replayed)
	}
}

func TestPerTurnCompactionPolicyDoesNotChangeSessionDefaults(t *testing.T) {
	m := newManager(t, session.Options{})
	info := create(t, m, agent.SessionConfig{Options: map[string]any{"contextTokens": 80, "contextWindowTokens": 100}})
	turn, err := m.Prompt(context.Background(), info.ID, session.PromptParams{Input: agent.Text("first"), Settings: settings.Patch{"compaction": json.RawMessage(`{"autoAtPercent":75}`)}})
	if err != nil {
		t.Fatal(err)
	}
	if res := waitMeterTurn(t, turn); res.Status != event.TurnStatusCompleted {
		t.Fatal(res)
	}
	collect(t, m, info.ID, "second")
	events := contextEvents(t, m, info.ID)
	if len(events) != 2 || events[0].Reason != "threshold" || events[1].Status != "completed" {
		t.Fatal(events)
	}
	snapshot, err := m.Settings(context.Background(), settings.Target{SessionID: info.ID})
	if err != nil || snapshot.Values.Compaction.AutoAtPercent != nil || snapshot.Fields["compaction.autoAtPercent"].When != "nextOperation" {
		t.Fatal(snapshot, err)
	}
	_, err = m.ConfigureSettings(context.Background(), settings.ConfigureParams{Target: settings.Target{SessionID: info.ID}, Patch: settings.Patch{"compaction": json.RawMessage(`{"autoAtTokens":70}`)}})
	if err != nil {
		t.Fatal(err)
	}
	collect(t, m, info.ID, "third")
	if events = contextEvents(t, m, info.ID); len(events) != 4 {
		t.Fatal(events)
	}
	for _, raw := range []string{`{"autoAtTokens":0}`, `{"autoAtPercent":101}`, `{"timeoutMs":0}`} {
		if _, err = m.ConfigureSettings(context.Background(), settings.ConfigureParams{Target: settings.Target{SessionID: info.ID}, Patch: settings.Patch{"compaction": json.RawMessage(raw)}}); !errors.Is(err, settings.ErrInvalid) {
			t.Fatalf("%s: %v", raw, err)
		}
	}
}

func TestCompactionBudgetStopsMaintenanceAndAutomaticPrompt(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		t.Run(map[bool]string{false: "manual", true: "threshold"}[automatic], func(t *testing.T) {
			r := &contextRuntime{compact: func(ctx context.Context, h agent.Handler) error {
				h.Emit(event.Event{Type: event.Usage, Data: event.UsageData{Scope: "turn", TotalTokens: 4}})
				<-ctx.Done()
				return ctx.Err()
			}}
			m := contextManager(t, r, nil)
			patch := settings.Patch{"budget": json.RawMessage(`{"maxSessionTokens":4}`)}
			if automatic {
				patch["compaction"] = json.RawMessage(`{"autoAtTokens":70}`)
			} else {
				patch["budget"] = json.RawMessage(`{"maxSessionTokens":4,"maxTurnTokens":0}`)
			}
			info := contextCreate(t, m, patch)
			if automatic {
				turn := startMeterTurn(t, m, info.ID, session.PromptParams{})
				result := waitMeterTurn(t, turn)
				if result.Status != event.TurnStatusCancelled || result.Budget == nil || r.prompts.Load() != 0 {
					t.Fatal(result, r.prompts.Load())
				}
			} else {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if _, err := m.Compact(ctx, info.ID); !errors.Is(err, session.ErrBudgetExceeded) {
					t.Fatal(err)
				}
			}
			state, _ := m.Get(context.Background(), info.ID)
			if state.Usage.TotalTokens != 4 || state.State != session.StateIdle {
				t.Fatal(state)
			}
			if _, err := m.Compact(context.Background(), info.ID); !errors.Is(err, session.ErrBudgetExceeded) {
				t.Fatal(err)
			}
		})
	}
}

func TestNativeCompactionHasNormalizedLifecycleAndFreshCounts(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		start, fresh bool
	}{
		{"completion-only", "compacted", false, false},
		{"fresh-zero", "compacted", true, true},
		{"failure", "compaction_failed", true, false},
		{"cancelled", "compaction_cancelled", true, false},
		{"missing-terminal", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &contextRuntime{prompt: func(_ context.Context, h agent.Handler) error {
				if tc.start {
					h.Emit(event.Event{Type: event.RuntimeStatus, Data: event.RuntimeStatusData{Status: "compacting"}})
				}
				if tc.fresh {
					n, w := int64(0), int64(100)
					h.Emit(event.Event{Type: event.ContextUsage, Data: event.ContextUsageData{UsedTokens: &n, WindowTokens: &w, Basis: "reported"}})
				}
				h.Emit(event.Event{Type: event.RuntimeStatus, Data: event.RuntimeStatusData{Status: tc.status}})
				return nil
			}}
			m := contextManager(t, r, nil)
			info := contextCreate(t, m, nil)
			collect(t, m, info.ID, "go")
			got, err := m.Context(context.Background(), info.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.fresh {
				if got.Usage.UsedTokens == nil || *got.Usage.UsedTokens != 0 || got.Usage.Stale {
					t.Fatal(got)
				}
			} else if got.Usage.UsedTokens != nil || !got.Usage.Stale {
				t.Fatal(got)
			}
			events := contextEvents(t, m, info.ID)
			expect := map[string]string{"compacted": "completed", "compaction_failed": "failed", "compaction_cancelled": "cancelled"}[tc.status]
			if tc.status == "" {
				expect = "failed"
			}
			if len(events) != 2 || events[0].Status != "started" || events[1].Status != expect || events[0].OperationID != events[1].OperationID {
				t.Fatal(events)
			}
		})
	}
}

func TestNativeIdleCompactionProtectsSessionAndCloseEndsLifecycle(t *testing.T) {
	var handler agent.Handler
	r := &contextRuntime{observe: func(h agent.Handler) { handler = h }}
	store, err := session.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := contextManager(t, r, store)
	info := contextCreate(t, m, nil)
	handler.Emit(event.Event{Type: event.RuntimeStatus, Data: event.RuntimeStatusData{Status: "compacting"}})
	state, err := m.Context(context.Background(), info.ID)
	if err != nil || state.State != session.StateCompacting || state.CanCompact {
		t.Fatal(state, err)
	}
	if _, err = m.Prompt(context.Background(), info.ID, session.PromptParams{}); !errors.Is(err, session.ErrBusy) {
		t.Fatal(err)
	}
	if err = m.Evict(context.Background(), info.ID); !errors.Is(err, session.ErrBusy) {
		t.Fatal(err)
	}
	if err = m.Cancel(info.ID); !errors.Is(err, agent.ErrUnsupported) {
		t.Fatal(err)
	}
	if err = m.Close(context.Background(), info.ID); err != nil {
		t.Fatal(err)
	}
	events := contextEvents(t, m, info.ID)
	if len(events) != 2 || events[1].Status != "cancelled" {
		t.Fatal(events)
	}
	if err = m.Evict(context.Background(), info.ID); err != nil {
		t.Fatal(err)
	}
}

func TestClosedContextRetainsCountsButIsStale(t *testing.T) {
	m := newManager(t, session.Options{})
	info := create(t, m, agent.SessionConfig{Options: map[string]any{"contextTokens": 80, "contextWindowTokens": 100}})
	if err := m.Close(context.Background(), info.ID); err != nil {
		t.Fatal(err)
	}
	state, err := m.Context(context.Background(), info.ID)
	if err != nil || state.Usage.UsedTokens == nil || *state.Usage.UsedTokens != 80 || !state.Usage.Stale {
		t.Fatal(state, err)
	}
}

func TestThresholdCompactionAndPromptShareBudgetAcrossNativeSnapshots(t *testing.T) {
	r := &contextRuntime{
		compact: func(_ context.Context, h agent.Handler) error {
			h.Emit(event.Event{Type: event.Usage, Data: event.UsageData{Scope: "turn", TotalTokens: 40}})
			return nil
		},
		prompt: func(ctx context.Context, h agent.Handler) error {
			h.Emit(event.Event{Type: event.Usage, Data: event.UsageData{Scope: "turn", TotalTokens: 30}})
			return ctx.Err()
		},
	}
	store, err := session.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := contextManager(t, r, store)
	info := contextCreate(t, m, settings.Patch{
		"budget":     json.RawMessage(`{"maxTurnTokens":60}`),
		"compaction": json.RawMessage(`{"autoAtTokens":70}`),
	})
	checkpoint, err := store.Get(context.Background(), info.ID)
	if err != nil {
		t.Fatal(err)
	}
	res := waitMeterTurn(t, startMeterTurn(t, m, info.ID, session.PromptParams{}))
	if res.Status != event.TurnStatusCancelled || res.Budget == nil || res.Budget.Observed != 70 {
		t.Fatal(res)
	}
	live, _ := m.Get(context.Background(), info.ID)
	if live.Usage.TotalTokens != 70 {
		t.Fatal(live.Usage)
	}
	if err = store.Put(context.Background(), checkpoint); err != nil {
		t.Fatal(err)
	}
	reopened := contextManager(t, r, store)
	saved, err := reopened.Get(context.Background(), info.ID)
	if err != nil || saved.Usage.TotalTokens != 70 {
		t.Fatal(saved.Usage, err)
	}
}

func TestCompactionSegmentsKeepUnknownUsageIncomplete(t *testing.T) {
	for _, maintenanceKnown := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown-maintenance", true: "unknown-prompt"}[maintenanceKnown], func(t *testing.T) {
			r := &contextRuntime{
				compact: func(_ context.Context, h agent.Handler) error {
					if maintenanceKnown {
						h.Emit(event.Event{Type: event.Usage, Data: event.UsageData{Scope: "turn", TotalTokens: 40}})
					}
					return nil
				},
				prompt: func(_ context.Context, h agent.Handler) error {
					if !maintenanceKnown {
						h.Emit(event.Event{Type: event.Usage, Data: event.UsageData{Scope: "turn", TotalTokens: 30}})
					}
					return nil
				},
			}
			m := contextManager(t, r, nil)
			info := contextCreate(t, m, settings.Patch{"compaction": json.RawMessage(`{"autoAtTokens":70}`)})
			waitMeterTurn(t, startMeterTurn(t, m, info.ID, session.PromptParams{}))
			state, err := m.Get(context.Background(), info.ID)
			if err != nil || state.Usage.TokensComplete || state.Usage.TotalTokens == 0 {
				t.Fatal(state.Usage, err)
			}
		})
	}
}
