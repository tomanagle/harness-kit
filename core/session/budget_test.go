package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/runtimes/fake"
	"github.com/tomanagle/harness-kit/core/session"
	"github.com/tomanagle/harness-kit/core/settings"
)

type meterRuntime struct {
	script func(context.Context, agent.Handler, agent.Turn) (agent.TurnResult, error)
}

func (r meterRuntime) Info() agent.Info {
	return agent.Info{Name: "meter", Capabilities: agent.Capabilities{Resume: true, Usage: true, Cost: true}}
}
func (r meterRuntime) Discover(context.Context) (agent.Discovery, error) {
	return agent.Discovery{}, nil
}
func (r meterRuntime) Start(_ context.Context, cfg agent.SessionConfig, h agent.Handler) (agent.Session, error) {
	return &meterSession{runtime: r, h: h}, nil
}

type meterSession struct {
	runtime meterRuntime
	h       agent.Handler
}

func (s *meterSession) RuntimeSessionID() string    { return "native-meter" }
func (s *meterSession) Close(context.Context) error { return nil }
func (s *meterSession) Prompt(ctx context.Context, t agent.Turn) (agent.TurnResult, error) {
	return s.runtime.script(ctx, s.h, t)
}
func meter(t *testing.T, store session.Store, script func(context.Context, agent.Handler, agent.Turn) (agent.TurnResult, error)) *session.Manager {
	t.Helper()
	m := session.NewManager(agent.NewRegistry(meterRuntime{script}), session.Options{Store: store})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = m.Shutdown(ctx)
	})
	return m
}
func patchBudget(raw string) settings.Patch { return settings.Patch{"budget": json.RawMessage(raw)} }
func meterSessionInfo(t *testing.T, m *session.Manager, raw string) session.Info {
	t.Helper()
	p := session.CreateParams{Runtime: "meter"}
	if raw != "" {
		p.Settings = patchBudget(raw)
	}
	info, err := m.Create(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	return info
}
func startMeterTurn(t *testing.T, m *session.Manager, id string, p session.PromptParams) *session.Turn {
	t.Helper()
	turn, err := m.Prompt(context.Background(), id, p)
	if err != nil {
		t.Fatal(err)
	}
	return turn
}
func waitMeterTurn(t *testing.T, turn *session.Turn) agent.TurnResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, err := turn.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestBudgetsCancelOnObservedUsageAndBlockAdmission(t *testing.T) {
	m := meter(t, nil, func(ctx context.Context, h agent.Handler, _ agent.Turn) (agent.TurnResult, error) {
		u := event.UsageData{Scope: "turn", TotalTokens: 4}
		h.Emit(event.Event{Type: event.Usage, Data: u})
		h.Emit(event.Event{Type: event.Usage, Data: u})
		<-ctx.Done()
		return agent.TurnResult{Usage: &u}, ctx.Err()
	})
	p := meterSessionInfo(t, m, `{"maxSessionTokens":4}`)
	res := waitMeterTurn(t, startMeterTurn(t, m, p.ID, session.PromptParams{Input: agent.Text("go")}))
	if res.Status != event.TurnStatusCancelled || res.Budget == nil || res.Budget.Observed != 4 {
		t.Fatal(res)
	}
	info, _ := m.Get(context.Background(), p.ID)
	if info.Usage.TotalTokens != 4 || info.Usage.CostUSD != nil || info.Usage.CostComplete {
		t.Fatal(info.Usage)
	}
	if _, err := m.Prompt(context.Background(), p.ID, session.PromptParams{}); !errors.Is(err, session.ErrBudgetExceeded) {
		t.Fatal(err)
	}
	all, _, _ := m.Events(p.ID, 0, 0)
	count := 0
	for _, ev := range all {
		if ev.Type == event.BudgetExceeded {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("budget event count %d", count)
	}
}

func TestLiveBudgetUpdatesCancelInheritedLimits(t *testing.T) {
	observed := make(chan struct{})
	var once sync.Once
	m := meter(t, nil, func(ctx context.Context, h agent.Handler, _ agent.Turn) (agent.TurnResult, error) {
		h.Emit(event.Event{Type: event.Usage, Data: event.UsageData{Scope: "turn", TotalTokens: 7}})
		once.Do(func() { close(observed) })
		<-ctx.Done()
		return agent.TurnResult{}, ctx.Err()
	})
	p := meterSessionInfo(t, m, "")
	turn := startMeterTurn(t, m, p.ID, session.PromptParams{})
	select {
	case <-observed:
	case <-time.After(time.Second):
		t.Fatal("no usage")
	}
	if _, err := m.ConfigureSettings(context.Background(), settings.ConfigureParams{Target: settings.Target{SessionID: p.ID}, Patch: patchBudget(`{"maxTurnTokens":7}`)}); err != nil {
		t.Fatal(err)
	}
	if res := waitMeterTurn(t, turn); res.Budget == nil || res.Budget.Scope != "turn" {
		t.Fatal(res)
	}
	state, err := m.Settings(context.Background(), settings.Target{SessionID: p.ID})
	if err != nil || state.Values.Budget.MaxTurnTokens == nil || state.Fields["budget.maxTurnTokens"].When != "live" {
		t.Fatal(state, err)
	}
}

func TestBudgetTurnOverridesZeroNullAndContextOccupancy(t *testing.T) {
	m := meter(t, nil, func(_ context.Context, h agent.Handler, _ agent.Turn) (agent.TurnResult, error) {
		h.Emit(event.Event{Type: event.Usage, Data: event.UsageData{Scope: "session", ContextWindow: 1000000}})
		return agent.TurnResult{Status: event.TurnStatusCompleted}, nil
	})
	if _, err := m.ConfigureSettings(context.Background(), settings.ConfigureParams{Patch: patchBudget(`{"maxTurnTokens":0}`)}); err != nil {
		t.Fatal(err)
	}
	p := meterSessionInfo(t, m, "")
	if _, err := m.Prompt(context.Background(), p.ID, session.PromptParams{}); !errors.Is(err, session.ErrBudgetExceeded) {
		t.Fatal(err)
	}
	res := waitMeterTurn(t, startMeterTurn(t, m, p.ID, session.PromptParams{Settings: patchBudget(`{"maxTurnTokens":null}`)}))
	if res.Status != event.TurnStatusCompleted || res.Budget != nil {
		t.Fatal(res)
	}
	info, _ := m.Get(context.Background(), p.ID)
	if info.Usage.TotalTokens != 0 || info.Usage.TokensComplete || info.Usage.CostUSD != nil {
		t.Fatal(info.Usage)
	}
	if _, err := m.Prompt(context.Background(), p.ID, session.PromptParams{Settings: patchBudget(`{"maxSessionTokens":100}`)}); !errors.Is(err, settings.ErrScope) {
		t.Fatal(err)
	}
	if _, err := m.Prompt(context.Background(), p.ID, session.PromptParams{}); !errors.Is(err, session.ErrBudgetExceeded) {
		t.Fatal("turn override leaked", err)
	}
}

func TestBudgetAccountingPersistsAcrossRestoreAndLegacyReplay(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "checkpoint", true: "replay"}[legacy], func(t *testing.T) {
			ctx := context.Background()
			store, err := session.NewFileStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			script := func(total int64) func(context.Context, agent.Handler, agent.Turn) (agent.TurnResult, error) {
				return func(_ context.Context, h agent.Handler, _ agent.Turn) (agent.TurnResult, error) {
					cost := float64(total) / 100
					h.Emit(event.Event{Type: event.Usage, Data: event.UsageData{Scope: "session", TotalTokens: total, CostUSD: &cost}})
					u := event.UsageData{Scope: "turn", TotalTokens: 10}
					if total == 100 {
						u.TotalTokens = 100
					}
					return agent.TurnResult{Status: event.TurnStatusCompleted, Usage: &u}, nil
				}
			}
			m := meter(t, store, script(100))
			p := meterSessionInfo(t, m, `{"maxSessionTokens":110,"maxSessionCostUsd":1.1}`)
			waitMeterTurn(t, startMeterTurn(t, m, p.ID, session.PromptParams{}))
			if err = m.Shutdown(ctx); err != nil {
				t.Fatal(err)
			}
			if legacy {
				rec, err := store.Get(ctx, p.ID)
				if err != nil {
					t.Fatal(err)
				}
				rec.Accounting = nil
				if err = store.Put(ctx, rec); err != nil {
					t.Fatal(err)
				}
			}
			m2 := meter(t, store, script(110))
			restored, err := m2.Restore(ctx, p.ID)
			if err != nil || restored.Usage.TotalTokens != 100 {
				t.Fatal(restored, err)
			}
			res := waitMeterTurn(t, startMeterTurn(t, m2, p.ID, session.PromptParams{}))
			if res.Budget == nil {
				t.Fatal(res)
			}
			info, _ := m2.Get(ctx, p.ID)
			if info.Usage.TotalTokens != 110 || info.Usage.CostUSD == nil || *info.Usage.CostUSD != 1.1 {
				t.Fatal(info.Usage)
			}
			if _, err = m2.Prompt(ctx, p.ID, session.PromptParams{}); !errors.Is(err, session.ErrBudgetExceeded) {
				t.Fatal(err)
			}
			if _, err = m2.ConfigureSettings(ctx, settings.ConfigureParams{Target: settings.Target{SessionID: p.ID}, Patch: patchBudget(`{"maxSessionTokens":null,"maxSessionCostUsd":null}`)}); err != nil {
				t.Fatal(err)
			}
			settingsSnapshot, _ := m2.Settings(ctx, settings.Target{SessionID: p.ID})
			if settingsSnapshot.Values.Budget.MaxSessionTokens != nil {
				t.Fatal(settingsSnapshot)
			}
		})
	}
}

func TestExplicitTurnBudgetSurvivesDefaultUpdateButSessionLimitStillApplies(t *testing.T) {
	ready := make(chan struct{})
	m := meter(t, nil, func(ctx context.Context, h agent.Handler, _ agent.Turn) (agent.TurnResult, error) {
		h.Emit(event.Event{Type: event.Usage, Data: event.UsageData{Scope: "turn", TotalTokens: 7}})
		close(ready)
		<-ctx.Done()
		return agent.TurnResult{}, ctx.Err()
	})
	p := meterSessionInfo(t, m, "")
	turn := startMeterTurn(t, m, p.ID, session.PromptParams{Settings: patchBudget(`{"maxTurnTokens":10}`)})
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("no usage")
	}
	for _, raw := range []string{`{"maxTurnTokens":1}`, `{"maxSessionTokens":7}`} {
		if _, err := m.ConfigureSettings(context.Background(), settings.ConfigureParams{Target: settings.Target{SessionID: p.ID}, Patch: patchBudget(raw)}); err != nil {
			t.Fatal(err)
		}
	}
	if res := waitMeterTurn(t, turn); res.Budget == nil || res.Budget.Scope != "session" {
		t.Fatal(res)
	}
}

func TestNormalizedTokenTotalsIncludeCacheExactlyOnce(t *testing.T) {
	for name, u := range map[string]event.UsageData{
		"claude": {Scope: "turn", InputTokens: 2, OutputTokens: 3, CachedInputTokens: 10, CacheWriteTokens: 1, TotalTokens: 16},
		"codex":  {Scope: "turn", InputTokens: 12, OutputTokens: 3, CachedInputTokens: 10, ReasoningTokens: 2, TotalTokens: 15},
	} {
		t.Run(name, func(t *testing.T) {
			m := meter(t, nil, func(_ context.Context, h agent.Handler, _ agent.Turn) (agent.TurnResult, error) {
				h.Emit(event.Event{Type: event.Usage, Data: u})
				return agent.TurnResult{Usage: &u}, nil
			})
			p := meterSessionInfo(t, m, "")
			waitMeterTurn(t, startMeterTurn(t, m, p.ID, session.PromptParams{}))
			info, _ := m.Get(context.Background(), p.ID)
			if info.Usage.TotalTokens != u.TotalTokens {
				t.Fatal(info.Usage)
			}
		})
	}
}

type unmeteredRuntime struct{ meterRuntime }

func (r unmeteredRuntime) Info() agent.Info { return agent.Info{Name: "meter"} }
func TestUnavailableBudgetMetricsAreRejectedExplicitly(t *testing.T) {
	ctx := context.Background()
	m := session.NewManager(agent.NewRegistry(unmeteredRuntime{}), session.Options{})
	t.Cleanup(func() { _ = m.Shutdown(ctx) })
	for _, raw := range []string{`{"maxSessionTokens":1}`, `{"maxTurnCostUsd":1}`} {
		if _, err := m.Create(ctx, session.CreateParams{Runtime: "meter", Settings: patchBudget(raw)}); !errors.Is(err, agent.ErrUnsupported) {
			t.Fatal(err)
		}
	}
	p := meterSessionInfo(t, m, `{"maxSessionCostUsd":0}`)
	snapshot, err := m.Settings(ctx, settings.Target{SessionID: p.ID})
	if err != nil || snapshot.Fields["budget.maxTurnTokens"].Writable || snapshot.Fields["budget.maxSessionCostUsd"].When != "unsupported" {
		t.Fatal(snapshot, err)
	}
	if _, err = m.ConfigureSettings(ctx, settings.ConfigureParams{Target: settings.Target{SessionID: p.ID}, Patch: patchBudget(`{"maxSessionCostUsd":1}`)}); !errors.Is(err, agent.ErrUnsupported) {
		t.Fatal(err)
	}
	if _, err = m.Prompt(ctx, p.ID, session.PromptParams{Settings: patchBudget(`{"maxTurnTokens":1}`)}); !errors.Is(err, agent.ErrUnsupported) {
		t.Fatal(err)
	}
	// Global defaults remain runtime-agnostic, including hosts exposing several runtimes.
	if _, err = m.ConfigureSettings(ctx, settings.ConfigureParams{Patch: patchBudget(`{"maxSessionCostUsd":1}`)}); err != nil {
		t.Fatal(err)
	}
}

func TestColdBudgetReadsRecoverUsageAfterRecordCheckpoint(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := meter(t, store, func(context.Context, agent.Handler, agent.Turn) (agent.TurnResult, error) {
		return agent.TurnResult{Usage: &event.UsageData{Scope: "turn", TotalTokens: 10}}, nil
	})
	p := meterSessionInfo(t, m, "")
	waitMeterTurn(t, startMeterTurn(t, m, p.ID, session.PromptParams{}))
	if err = m.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	rec, err := store.Get(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i, ev := range []event.Event{{Type: event.TurnStarted, Data: event.TurnStartedData{Text: "interrupted"}}, {Type: event.Usage, Data: event.UsageData{Scope: "turn", TotalTokens: 7}}} {
		ev.SessionID = p.ID
		ev.TurnID = "interrupted"
		ev.Seq = rec.Info.LastSeq + uint64(i) + 1
		ev.Time = time.Now().UTC()
		if err = store.AppendEvent(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
	cold := meter(t, store, nil)
	u, err := cold.BudgetUsage(ctx, p.ID)
	if err != nil || u.TotalTokens != 17 || u.TokensComplete {
		t.Fatal(u, err)
	}
	info, err := cold.Get(ctx, p.ID)
	if err != nil || info.Usage.TotalTokens != 17 {
		t.Fatal(info, err)
	}
}

func TestColdForkIncludesRecoveredAccountingCheckpoint(t *testing.T) {
	ctx := context.Background()
	store, err := session.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := newManager(t, session.Options{Store: store})
	p := create(t, m, agent.SessionConfig{})
	collect(t, m, p.ID, "parent")
	if err = m.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	rec, err := store.Get(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	observed := rec.Info.Usage.TotalTokens + 20
	ev := event.Event{SessionID: p.ID, Seq: rec.Info.LastSeq + 1, Time: time.Now().UTC(), Type: event.Usage, Data: event.UsageData{Scope: "session", TotalTokens: observed}}
	if err = store.AppendEvent(ctx, ev); err != nil {
		t.Fatal(err)
	}
	cold := session.NewManager(agent.NewRegistry(fake.New()), session.Options{Store: store})
	t.Cleanup(func() { _ = cold.Shutdown(ctx) })
	child, err := cold.Fork(ctx, p.ID, session.ForkParams{})
	if err != nil || child.Usage.TotalTokens != observed {
		t.Fatal(child.Usage, err)
	}
	if err = cold.Close(ctx, child.ID); err != nil {
		t.Fatal(err)
	}
	restored, err := cold.Restore(ctx, child.ID)
	if err != nil || restored.Usage.TotalTokens != observed {
		t.Fatal(restored.Usage, err)
	}
}
