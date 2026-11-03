package codex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tomanagle/harness-kit/agent"
	"github.com/tomanagle/harness-kit/event"
)

func recordedRequests(t *testing.T, path, method string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var req struct {
			Method string
			Params map[string]any
		}
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			t.Fatal(err)
		}
		if req.Method == method {
			out = append(out, req.Params)
		}
	}
	return out
}

func TestConfigurationOverridesDoNotLeakBetweenTurns(t *testing.T) {
	log := filepath.Join(t.TempDir(), "requests.jsonl")
	s := startSession(t, newFake(t, "HK_FAKE_CODEX_REQUEST_LOG="+log), agent.SessionConfig{Model: "gpt-other", Effort: "high"}, &recorder{})
	for _, settings := range []*agent.TurnConfig{
		{Model: "gpt-test", Effort: "low"},
		nil,                  // direct callers inherit the initial session selections
		{},                   // explicitly clear both selections to native defaults
		{Model: "gpt-other"}, // inherits this model's catalog default effort
	} {
		if res, err := s.Prompt(context.Background(), agent.Turn{ID: "turn", Input: agent.Text("hello"), Config: settings}); err != nil || res.Status != event.TurnStatusCompleted {
			t.Fatalf("prompt: %+v %v", res, err)
		}
	}
	requests := recordedRequests(t, log, "turn/start")
	want := [][2]string{{"gpt-test", "low"}, {"gpt-other", "high"}, {"gpt-test", "medium"}, {"gpt-other", "low"}}
	if len(requests) != len(want) {
		t.Fatalf("turn requests: %+v", requests)
	}
	for i, request := range requests {
		if request["model"] != want[i][0] || request["effort"] != want[i][1] {
			t.Fatalf("turn %d config: %+v", i, request)
		}
	}
	start := recordedRequests(t, log, "thread/start")[0]
	if _, ok := start["sandbox"]; ok {
		t.Fatalf("adapter forced sandbox: %+v", start)
	}
	if _, ok := start["approvalPolicy"]; ok {
		t.Fatalf("adapter forced approvalPolicy: %+v", start)
	}
}

func TestPrepareTurnRejectsUnavailableDefaultsAndStartupChanges(t *testing.T) {
	s := startSession(t, newFake(t, "HK_FAKE_CODEX_NO_DEFAULTS=1"), agent.SessionConfig{Model: "custom", Effort: "high", Options: Options{Sandbox: "read-only"}}, &recorder{})
	settings := s.initialTurnConfig()
	settings.Effort = ""
	if _, err := s.PrepareTurn(context.Background(), settings); !errors.Is(err, agent.ErrUnsupported) {
		t.Fatalf("unavailable effort default: %v", err)
	}
	settings = s.initialTurnConfig()
	settings.Model = ""
	if _, err := s.PrepareTurn(context.Background(), settings); !errors.Is(err, agent.ErrUnsupported) {
		t.Fatalf("unavailable model default: %v", err)
	}
	settings = s.initialTurnConfig()
	settings.Options["sandbox"] = json.RawMessage(`"workspace-write"`)
	if _, err := s.PrepareTurn(context.Background(), settings); !errors.Is(err, agent.ErrUnsupported) {
		t.Fatalf("startup-only option: %v", err)
	}
	settings = s.initialTurnConfig()
	if got, err := s.PrepareTurn(context.Background(), settings); err != nil || got.Model != "custom" || got.Effort != "high" {
		t.Fatalf("explicit settings require no defaults: %+v %v", got, err)
	}
}

func TestConfigurationValidationAndCapabilities(t *testing.T) {
	rt := New()
	for _, cfg := range []agent.SessionConfig{
		{Options: map[string]any{"sandbox": "invalid"}},
		{Options: map[string]any{"unknown": true}},
		{Options: map[string]any{"config": map[string]any{"model": "nested"}}},
		{Options: map[string]any{"config": map[string]any{"model_reasoning_effort": "high"}}},
		{Model: " "},
		{Effort: "\t"},
	} {
		if err := rt.ValidateConfig(cfg); !errors.Is(err, agent.ErrInvalidConfig) {
			t.Fatalf("config %#v: %v", cfg, err)
		}
	}
	caps := rt.Info().Capabilities.Configuration
	if !caps["model"].Turn || !caps["effort"].Session || caps["model"].Live || caps["options.sandbox"].Session || !caps["options.sandbox"].Startup {
		t.Fatalf("configuration capabilities: %+v", caps)
	}
}

func TestNativeDefaultsSurviveResumeAndCallerMutation(t *testing.T) {
	original := startSession(t, newFake(t), agent.SessionConfig{Model: "gpt-other", Effort: "high"}, &recorder{})
	saved := original.NativeDefaults()
	if saved.Model != "gpt-test" {
		t.Fatalf("native baseline: %+v", saved)
	}
	// A restored thread may currently hold a previous override. The manager's
	// captured native defaults must win over new runtime discovery results.
	restored := startSession(t, newFake(t, "HK_FAKE_CODEX_NO_DEFAULTS=1"), agent.SessionConfig{
		ResumeID: "thr_existing", Model: "gpt-other", Effort: "high", NativeDefaults: &saved,
	}, &recorder{})
	saved.Options["modelEfforts"] = json.RawMessage(`{"gpt-test":"mutated"}`)
	got, err := restored.PrepareTurn(context.Background(), agent.TurnConfig{})
	if err != nil || got.Model != "gpt-test" || got.Effort != "medium" {
		t.Fatalf("restored native baseline: %+v %v", got, err)
	}
	other, err := restored.PrepareTurn(context.Background(), agent.TurnConfig{Model: "gpt-other"})
	if err != nil || other.Effort != "low" {
		t.Fatalf("restored model-specific default: %+v %v", other, err)
	}
}

func TestResolvedThreadEffortIsNotAGlobalDefault(t *testing.T) {
	s := startSession(t, newFake(t, "HK_FAKE_CODEX_RESOLVED_EFFORT=low"), agent.SessionConfig{Model: "gpt-other"}, &recorder{})
	got, err := s.PrepareTurn(context.Background(), agent.TurnConfig{})
	if err != nil || got.Model != "gpt-test" || got.Effort != "medium" {
		t.Fatalf("thread effort leaked into native model default: %+v %v", got, err)
	}
	s.supportedEfforts = map[string][]string{"gpt-test": {"low", "medium"}}
	if _, err := s.PrepareTurn(context.Background(), agent.TurnConfig{Model: "gpt-test", Effort: "unlimited"}); !errors.Is(err, agent.ErrInvalidConfig) {
		t.Fatalf("unadvertised effort: %v", err)
	}
}
