package session_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/hooks"
	"github.com/tomanagle/harness-kit/core/runtimes/fake"
	"github.com/tomanagle/harness-kit/core/session"
	"github.com/tomanagle/harness-kit/core/settings"
)

type preferenceStore struct {
	mu    sync.Mutex
	value settings.Values
	fail  bool
}

func (s *preferenceStore) Load(context.Context) (settings.Values, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.value.Clone(), nil
}
func (s *preferenceStore) Save(_ context.Context, v settings.Values) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errors.New("disk unavailable")
	}
	s.value = v.Clone()
	return nil
}
func TestSettingsSaveFailureIsAtomic(t *testing.T) {
	ctx := context.Background()
	store := &preferenceStore{value: settings.Defaults()}
	m := session.NewManager(agent.NewRegistry(fake.New()), session.Options{SettingsStore: store})
	defer m.Shutdown(ctx)
	if err := m.InitializeSettings(ctx, settings.Defaults()); err != nil {
		t.Fatal(err)
	}
	before, _ := m.Settings(ctx, settings.Target{})
	store.fail = true
	if _, err := m.ConfigureSettings(ctx, settings.ConfigureParams{Patch: resourcePatch(t, `{"logging":{"level":"debug"}}`)}); !errors.Is(err, settings.ErrPersistence) {
		t.Fatal(err)
	}
	after, _ := m.Settings(ctx, settings.Target{})
	if after.Revision != before.Revision || after.Values.Logging.Level != before.Values.Logging.Level {
		t.Fatal("failed save changed runtime settings")
	}
	store.fail = false
	if _, err := m.ConfigureSettings(ctx, settings.ConfigureParams{Patch: resourcePatch(t, `{"logging":{"level":"warn"}}`)}); err != nil {
		t.Fatal(err)
	}
	saved, _ := store.Load(ctx)
	if saved.Logging.Level != "warn" {
		t.Fatal("accepted settings were not saved")
	}
}
func TestHookPreparationCancellationAndFirstTurn(t *testing.T) {
	ctx := context.Background()
	m := newManager(t, session.Options{})
	info := create(t, m, agent.SessionConfig{})
	entered := make(chan struct{})
	var calls []bool
	err := m.Hooks().Register("host", hooks.Spec{ID: "prepare", Name: hooks.BeforeTurn}, hooks.HandlerFunc(func(ctx context.Context, r hooks.Request) (hooks.Response, error) {
		// Reads and configuration writes must not deadlock on a lifecycle lock.
		if _, err := m.Settings(ctx, settings.Target{}); err != nil {
			return hooks.Response{}, err
		}
		calls = append(calls, r.IsFirstTurn)
		if len(calls) == 1 {
			close(entered)
			<-ctx.Done()
			return hooks.Response{}, ctx.Err()
		}
		input := append(r.Input, agent.InputPart{Type: "text", Text: "extra context"})
		return hooks.Response{Input: &input}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	rejected := make(chan error, 1)
	go func() {
		_, err := m.Prompt(ctx, info.ID, session.PromptParams{TurnID: "retry", Input: agent.Text("hello")})
		rejected <- err
	}()
	<-entered
	if _, err := m.Prompt(ctx, info.ID, session.PromptParams{Input: agent.Text("race")}); !errors.Is(err, session.ErrBusy) {
		t.Fatalf("race admitted: %v", err)
	}
	if err := m.Cancel(info.ID); err != nil {
		t.Fatal(err)
	}
	if err := <-rejected; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	before, _ := m.Get(ctx, info.ID)
	if before.Turns != 0 {
		t.Fatal("failed preparation consumed a turn")
	}
	for _, id := range []string{"retry", "second"} {
		turn, err := m.Prompt(ctx, info.ID, session.PromptParams{TurnID: id, Input: agent.Text("hello")})
		if err != nil {
			t.Fatal(err)
		}
		<-turn.Done()
	}
	if len(calls) != 3 || !calls[0] || !calls[1] || calls[2] {
		t.Fatalf("first turn flags: %v", calls)
	}
}
func TestAfterHookDoesNotChangeResultOrHoldSessionLock(t *testing.T) {
	ctx := context.Background()
	m := newManager(t, session.Options{})
	info := create(t, m, agent.SessionConfig{})
	done := make(chan error, 1)
	_ = m.Hooks().Register("host", hooks.Spec{ID: "after", Name: hooks.AfterTurn}, hooks.HandlerFunc(func(ctx context.Context, r hooks.Request) (hooks.Response, error) {
		_, err := m.Rename(ctx, r.Session.ID, "Renamed by hook")
		done <- err
		return hooks.Response{}, errors.New("after failure")
	}))
	turn, err := m.Prompt(ctx, info.ID, session.PromptParams{Input: agent.Text("hi")})
	if err != nil {
		t.Fatal(err)
	}
	<-turn.Done()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("after hook deadlocked")
	}
	if turn.Result().Status != "completed" {
		t.Fatal("after hook rewrote completed result")
	}
}
