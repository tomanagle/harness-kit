package claude

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/tomanagle/harness-kit/core/agent"
)

func TestTurnSettingsAreIsolated(t *testing.T) {
	t.Setenv("CLAUDE_CODE_EFFORT_LEVEL", "")
	rt, argLog := newFake(t, "2.1.317")
	cfg := agent.SessionConfig{Model: "sonnet", Effort: "medium", Options: Options{
		MaxTurns: 3, MaxBudgetUSD: 0.5, DisablePartialMessages: true,
		AllowedTools: []string{"Read"},
	}}
	sess, err := rt.Start(context.Background(), cfg, &recorder{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close(context.Background())
	defaults, err := cfg.Settings()
	if err != nil {
		t.Fatal(err)
	}
	override := defaults.Clone()
	override.Model, override.Effort = "haiku", "low"
	override.Options["maxTurns"] = json.RawMessage(`0`)
	override.Options["maxBudgetUsd"] = json.RawMessage(`0`)
	override.Options["disablePartialMessages"] = json.RawMessage(`false`)
	for i, settings := range []agent.TurnConfig{defaults, override, defaults} {
		if _, err := sess.Prompt(context.Background(), agent.Turn{ID: string(rune('a' + i)), Input: agent.Text("hello"), Config: &settings}); err != nil {
			t.Fatal(err)
		}
	}
	calls := invocations(t, argLog)
	if len(calls) != 3 {
		t.Fatalf("calls: %v", calls)
	}
	for i, a := range calls {
		model, effort := "sonnet", "medium"
		if i == 1 {
			model, effort = "haiku", "low"
		}
		if got, _ := flag(a, "--model"); got != model {
			t.Errorf("turn %d model=%q want=%q", i, got, model)
		}
		if got, _ := flag(a, "--effort"); got != effort {
			t.Errorf("turn %d effort=%q want=%q", i, got, effort)
		}
		if got, _ := flag(a, "--allowedTools"); got != "Read" {
			t.Errorf("startup tools lost: %v", a)
		}
		if i == 1 {
			if slices.Contains(a, "--max-turns") || slices.Contains(a, "--max-budget-usd") || !slices.Contains(a, "--include-partial-messages") {
				t.Errorf("zero/false not applied: %v", a)
			}
		} else if v, _ := flag(a, "--max-turns"); v != "3" || slices.Contains(a, "--include-partial-messages") {
			t.Errorf("defaults changed: %v", a)
		}
	}
	if got, _ := cfg.Settings(); string(got.Options["maxTurns"]) != "3" {
		t.Fatal("caller configuration changed")
	}
}

func TestNativeModelSurvivesTurnOverrideAndRestore(t *testing.T) {
	rt, argLog := newFake(t, "2.1.317")
	raw, err := rt.Start(context.Background(), agent.SessionConfig{}, &recorder{})
	if err != nil {
		t.Fatal(err)
	}
	s := raw.(*Session)
	defer s.Close(context.Background())
	for i, settings := range []agent.TurnConfig{{}, {Model: "haiku"}, {}} {
		if _, err := s.Prompt(context.Background(), agent.Turn{ID: string(rune('a' + i)), Input: agent.Text("hello"), Config: &settings}); err != nil {
			t.Fatal(err)
		}
	}
	calls := invocations(t, argLog)
	if slices.Contains(calls[0], "--model") {
		t.Fatalf("initial native configuration was overridden: %v", calls[0])
	}
	if model, _ := flag(calls[2], "--model"); model != "claude-test" {
		t.Fatalf("model override leaked: %v", calls[2])
	}
	baseline := s.NativeDefaults()
	if baseline.Model != "claude-test" {
		t.Fatalf("native defaults: %+v", baseline)
	}
	resumed, err := rt.Start(context.Background(), agent.SessionConfig{ResumeID: s.RuntimeSessionID(), NativeDefaults: &baseline}, &recorder{})
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close(context.Background())
	empty := agent.TurnConfig{}
	if _, err := resumed.Prompt(context.Background(), agent.Turn{ID: "restored", Input: agent.Text("hello"), Config: &empty}); err != nil {
		t.Fatal(err)
	}
	calls = invocations(t, argLog)
	if model, _ := flag(calls[3], "--model"); model != "claude-test" {
		t.Fatalf("restored baseline lost: %v", calls[3])
	}
}

func TestUnknownNativeModelResetFailsBeforeProcess(t *testing.T) {
	rt, log := newFake(t, "2.1.317")
	sess, err := rt.Start(context.Background(), agent.SessionConfig{}, &recorder{})
	if err != nil {
		t.Fatal(err)
	}
	first := agent.TurnConfig{Model: "haiku"}
	if _, err := sess.Prompt(context.Background(), agent.Turn{ID: "explicit", Input: agent.Text("hello"), Config: &first}); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.(agent.TurnPreparer).PrepareTurn(context.Background(), agent.TurnConfig{}); !errors.Is(err, agent.ErrUnsupported) {
		t.Fatalf("unknown inherited model: %v", err)
	}
	if len(invocations(t, log)) != 1 {
		t.Fatal("preparation launched another process")
	}
	clear, err := sess.(agent.TurnPreparer).PrepareTurn(context.Background(), agent.TurnConfig{Model: "default"})
	if err != nil || clear.Model != "default" {
		t.Fatalf("explicit native reset: %+v %v", clear, err)
	}

	legacy, err := rt.Start(context.Background(), agent.SessionConfig{ResumeID: "old-transcript"}, &recorder{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.(agent.TurnPreparer).PrepareTurn(context.Background(), agent.TurnConfig{}); !errors.Is(err, agent.ErrUnsupported) {
		t.Fatalf("legacy baseline silently accepted: %v", err)
	}
}

func TestConfigurationValidationAndCapabilities(t *testing.T) {
	rt := New(WithPath(filepath.Join(t.TempDir(), "not-installed")))
	for _, cfg := range []agent.SessionConfig{
		{Options: map[string]any{"unknown": true}},
		{Effort: "unlimited"},
		{Options: Options{PermissionMode: "yolo"}},
		{Options: Options{MaxTurns: -1}},
		{Options: Options{MaxBudgetUSD: -1}},
		{Cwd: filepath.Join(t.TempDir(), "missing")},
	} {
		if err := rt.ValidateConfig(cfg); !errors.Is(err, agent.ErrInvalidConfig) {
			t.Fatalf("invalid config %+v: %v", cfg, err)
		}
	}
	if err := rt.ValidateConfig(agent.SessionConfig{Effort: "low", Options: Options{Effort: "invalid-legacy"}}); err != nil {
		t.Fatalf("portable effort must win: %v", err)
	}
	if err := rt.ValidateConfig(agent.SessionConfig{Options: Options{MaxTurns: 0, MaxBudgetUSD: 0}}); err != nil {
		t.Fatal(err)
	}
	caps := rt.Info().Capabilities.Configuration
	for _, key := range []string{"model", "effort", "options.maxTurns", "options.maxBudgetUsd", "options.disablePartialMessages"} {
		v := caps[key]
		if !v.Startup || !v.Session || !v.Turn || v.Live {
			t.Errorf("%s capability: %+v", key, v)
		}
	}
	if v := caps["options.permissionMode"]; !v.Startup || !v.Session || !v.Turn || v.Live {
		t.Errorf("permission mode mutability: %+v", v)
	}
}

func TestTurnPreparationRejectsStartupChangesAndEffortEnvironmentConflicts(t *testing.T) {
	rt, log := newFake(t, "2.1.317", "CLAUDE_CODE_EFFORT_LEVEL=high")
	raw, err := rt.Start(context.Background(), agent.SessionConfig{Model: "sonnet"}, &recorder{})
	if err != nil {
		t.Fatal(err)
	}
	s := raw.(agent.TurnPreparer)
	if _, err := s.PrepareTurn(context.Background(), agent.TurnConfig{Model: "sonnet", Effort: "low"}); !errors.Is(err, agent.ErrUnsupported) {
		t.Fatalf("effort environment conflict: %v", err)
	}
	if _, err := s.PrepareTurn(context.Background(), agent.TurnConfig{Model: "sonnet", Effort: "high"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PrepareTurn(context.Background(), agent.TurnConfig{Model: "sonnet", Options: map[string]json.RawMessage{"bare": json.RawMessage(`true`)}}); !errors.Is(err, agent.ErrUnsupported) {
		t.Fatalf("startup-only process change: %v", err)
	}
	if _, err := os.Stat(log); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preparation launched process: %v", err)
	}
}

func TestPermissionOverridesDoNotLeakBetweenTurns(t *testing.T) {
	rt, log := newFake(t, "2.1.317")
	cfg := agent.SessionConfig{Model: "sonnet", Options: Options{PermissionMode: "default"}}
	sess, err := rt.Start(context.Background(), cfg, &recorder{})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close(context.Background())
	base, _ := cfg.Settings()
	override := base.Clone()
	override.Options["permissionMode"] = json.RawMessage(`"acceptEdits"`)
	for _, selection := range []*agent.TurnConfig{&base, &override, nil} {
		if _, err := sess.Prompt(context.Background(), agent.Turn{ID: "t", Input: agent.Text("hello"), Config: selection}); err != nil {
			t.Fatal(err)
		}
	}
	args := invocations(t, log)
	for i, want := range []string{"default", "acceptEdits", "default"} {
		if got, _ := flag(args[i], "--permission-mode"); got != want {
			t.Fatalf("turn %d permissionMode=%q want=%q", i, got, want)
		}
	}
}
