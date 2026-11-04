package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/session"
)

func textPtr(s string) *string { return &s }

func TestConfigurationSnapshotsAndHostPolicy(t *testing.T) {
	ctx := context.Background()
	m := newManager(t, session.Options{ValidateConfig: func(_ context.Context, req session.ConfigRequest) error {
		if req.Config.Model == "blocked" {
			return session.ErrUnsafe
		}
		return nil
	}})
	cfg := agent.SessionConfig{Model: "initial", Options: map[string]any{"unsafe": false}}
	info := create(t, m, cfg)
	cfg.Options.(map[string]any)["unsafe"] = true
	// Caller mutation after create cannot change session defaults.
	if _, err := m.Configure(ctx, info.ID, agent.ConfigPatch{Model: textPtr("blocked")}); !errors.Is(err, session.ErrUnsafe) {
		t.Fatalf("host policy: %v", err)
	}
	if _, err := m.Prompt(ctx, info.ID, session.PromptParams{Config: &agent.ConfigPatch{Options: map[string]json.RawMessage{"unsafe": []byte(`true`)}}}); !errors.Is(err, session.ErrUnsafe) {
		t.Fatalf("turn guard: %v", err)
	}
	if got, _ := m.Get(ctx, info.ID); got.Turns != 0 || got.LastSeq != info.LastSeq {
		t.Fatalf("rejected patches changed state: %+v", got)
	}
	patch := agent.ConfigPatch{Model: textPtr("once"), Effort: textPtr("high")}
	turn, err := m.Prompt(ctx, info.ID, session.PromptParams{Input: agent.Text("hello"), Config: &patch})
	if err != nil {
		t.Fatal(err)
	}
	*patch.Model = "mutated"
	if _, err := turn.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	evs, _, _ := m.Events(info.ID, 0, 0)
	for _, ev := range evs {
		if d, ok := ev.Data.(event.TurnStartedData); ok {
			if d.Config.Model != "once" || d.Config.Effort != "high" {
				t.Fatalf("turn patch mutation: %+v", d.Config)
			}
		}
	}
	next := collect(t, m, info.ID, "again")
	if d := next[0].Data.(event.TurnStartedData); d.Config.Model != "initial" || d.Config.Effort != "" {
		t.Fatalf("override leaked: %+v", d.Config)
	}
	got, _ := m.Get(ctx, info.ID)
	got.Config.Options["unsafe"][0] = 'x'
	if _, err := m.Prompt(ctx, info.ID, session.PromptParams{Input: agent.Text("snapshot stays isolated")}); err != nil {
		t.Fatalf("returned snapshot mutated manager: %v", err)
	}
}

func TestConfigurationRestoreAndExplicitPrepareHook(t *testing.T) {
	ctx := context.Background()
	store := historyStore(t, t.TempDir())
	m := newManager(t, session.Options{Store: store})
	info := create(t, m, agent.SessionConfig{Model: "creation", Effort: "low"})
	if _, err := m.Configure(ctx, info.ID, agent.ConfigPatch{Model: textPtr("changed"), Effort: textPtr("high")}); err != nil {
		t.Fatal(err)
	}
	collect(t, m, info.ID, "save")
	if err := m.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	creates, restores := 0, 0
	restoredCwd := t.TempDir()
	m2 := newManager(t, session.Options{Store: store, Prepare: func(context.Context, *session.CreateParams) ([]string, error) { creates++; return nil, nil }, PrepareRestore: func(_ context.Context, p *session.CreateParams) ([]string, error) {
		restores++
		p.Config.Effort = "medium"
		p.Config.Cwd = restoredCwd
		return []string{"restored settings"}, nil
	}})
	restored, err := m2.Restore(ctx, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if creates != 0 || restores != 1 || restored.Config.Model != "changed" || restored.Config.Effort != "medium" || restored.Cwd != restoredCwd {
		t.Fatalf("restore hooks/settings: %+v create=%d restore=%d", restored, creates, restores)
	}
	reset, err := m2.Configure(ctx, info.ID, agent.ConfigPatch{Reset: []string{"model", "effort"}})
	if err != nil {
		t.Fatal(err)
	}
	if reset.Config.Model != "creation" || reset.Config.Effort != "low" {
		t.Fatalf("lost creation parent: %+v", reset.Config)
	}
}

func TestMemoryStoreConfigurationOwnership(t *testing.T) {
	ctx := context.Background()
	store := session.NewMemoryStore()
	initial := agent.SessionConfig{Model: "initial", Options: map[string]any{"unsafe": false}}
	native := agent.TurnConfig{Model: "native", Options: map[string]json.RawMessage{"nested": []byte(`{"x":1}`)}}
	rec := session.Record{Info: session.Info{ID: "owned"}, Config: initial, InitialConfig: &initial, NativeDefaults: &native}
	if err := store.Put(ctx, rec); err != nil {
		t.Fatal(err)
	}
	initial.Model = "changed"
	initial.Options.(map[string]any)["unsafe"] = true
	native.Model = "changed"
	stored, err := store.Get(ctx, "owned")
	if err != nil {
		t.Fatal(err)
	}
	settings, _ := stored.Config.Settings()
	if stored.InitialConfig.Model != "initial" || stored.NativeDefaults.Model != "native" || string(settings.Options["unsafe"]) != "false" {
		t.Fatalf("caller mutated store: %+v", stored)
	}
	stored.InitialConfig.Model = "returned mutation"
	stored.NativeDefaults.Options["nested"][0] = 'x'
	list, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if list[0].InitialConfig.Model != "initial" || string(list[0].NativeDefaults.Options["nested"]) != `{"x":1}` {
		t.Fatalf("Get mutated store: %+v", list[0])
	}
	list[0].InitialConfig.Model = "list mutation"
	again, _ := store.Get(ctx, "owned")
	if again.InitialConfig.Model != "initial" {
		t.Fatal("List mutated store")
	}
}

func TestFailedRestoreConfigurationIsTransactional(t *testing.T) {
	ctx := context.Background()
	store := historyStore(t, t.TempDir())
	m := newManager(t, session.Options{Store: store})
	info := create(t, m, agent.SessionConfig{Model: "initial"})
	collect(t, m, info.ID, "save")
	m.Shutdown(ctx)
	m2 := newManager(t, session.Options{Store: store, PrepareRestore: func(_ context.Context, p *session.CreateParams) ([]string, error) {
		p.Config.Model = "failed-candidate"
		p.Config.Options = map[string]any{"failStart": true}
		return nil, nil
	}})
	if _, err := m2.Restore(ctx, info.ID); err == nil {
		t.Fatal("restore should fail")
	}
	rec, err := store.Get(ctx, info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Config.Model != "initial" || rec.Info.Config.Model != "initial" {
		t.Fatalf("failed restore changed saved defaults: %+v", rec)
	}
}

func TestCreationHookSuppliesTitleAndSettings(t *testing.T) {
	m := newManager(t, session.Options{Prepare: func(_ context.Context, p *session.CreateParams) ([]string, error) {
		p.Title = "Host\nchosen title"
		p.Config.Model = "host-model"
		return nil, nil
	}})
	info := create(t, m, agent.SessionConfig{})
	if info.Title != "Host chosen title" || info.Config.Model != "host-model" {
		t.Fatalf("host configuration lost: %+v", info)
	}
}
