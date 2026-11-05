package codex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/event"
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
	if _, ok := start["approvalsReviewer"]; ok {
		t.Fatalf("adapter forced approvalsReviewer: %+v", start)
	}
}

func TestApprovalReviewerConfiguration(t *testing.T) {
	for _, reviewer := range []string{"user", "auto_review", "guardian_subagent"} {
		t.Run(reviewer, func(t *testing.T) {
			log := filepath.Join(t.TempDir(), "requests.jsonl")
			s := startSession(t, newFake(t, "HK_FAKE_CODEX_REQUEST_LOG="+log), agent.SessionConfig{
				Options: Options{ApprovalsReviewer: reviewer, ApprovalPolicy: "on-request", Sandbox: "workspace-write"},
			}, &recorder{})
			request := recordedRequests(t, log, "thread/start")[0]
			if request["approvalsReviewer"] != reviewer || request["approvalPolicy"] != "on-request" || request["sandbox"] != "workspace-write" {
				t.Fatalf("permission settings not forwarded: %+v", request)
			}
			cfg := s.initialTurnConfig()
			cfg.Options["approvalsReviewer"] = json.RawMessage(`""`)
			if _, err := s.PrepareTurn(context.Background(), cfg); !errors.Is(err, agent.ErrUnsupported) {
				t.Fatalf("unavailable native reviewer must require explicit selection: %v", err)
			}
		})
	}
	rt := New()
	for _, opts := range []Options{
		{ApprovalsReviewer: "invalid"},
		{ApprovalsReviewer: "user", Config: map[string]any{"approvals_reviewer": "auto_review"}},
	} {
		if err := rt.ValidateConfig(agent.SessionConfig{Options: opts}); !errors.Is(err, agent.ErrInvalidConfig) {
			t.Fatalf("invalid reviewer config accepted: %+v %v", opts, err)
		}
	}
	capability := rt.Info().Capabilities.Configuration["options.approvalsReviewer"]
	if !capability.Startup || !capability.Session || !capability.Turn {
		t.Fatalf("unexpected reviewer capabilities: %+v", capability)
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
	settings.Options["ephemeral"] = json.RawMessage(`true`)
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
	if !caps["model"].Turn || !caps["effort"].Session || caps["model"].Live || !caps["options.sandbox"].Session || !caps["options.sandbox"].Startup {
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

func TestAdditionalFoldersRemainScopedAndPreserveNativePolicies(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(t.TempDir(), "requests.jsonl")
	cfg := agent.SessionConfig{Cwd: t.TempDir(), Options: Options{AddDirs: []string{dir}}}
	if reason := New().Unsafe(cfg); reason != "" {
		t.Fatalf("scoped roots marked unsafe: %s", reason)
	}
	s := startSession(t, newFake(t, "HK_FAKE_CODEX_REQUEST_LOG="+log), cfg, &recorder{})
	if _, err := s.Prompt(context.Background(), agent.Turn{ID: "folders", Input: agent.Text("hello")}); err != nil {
		t.Fatal(err)
	}
	request := recordedRequests(t, log, "thread/start")[0]
	config := request["config"].(map[string]any)
	roots := config["sandbox_workspace_write.writable_roots"].([]any)
	if len(roots) != 1 || roots[0] != dir || request["cwd"] != cfg.Cwd {
		t.Fatalf("folders: %+v", request)
	}
	if request["sandbox"] != nil || request["approvalPolicy"] != nil {
		t.Fatalf("changed native policy: %+v", request)
	}
	if err := New().ValidateConfig(agent.SessionConfig{Options: Options{AddDirs: []string{"relative"}}}); !errors.Is(err, agent.ErrInvalidConfig) {
		t.Fatalf("relative root: %v", err)
	}
	if err := New().ValidateConfig(agent.SessionConfig{Options: Options{AddDirs: []string{filepath.Join(dir, "missing")}}}); !errors.Is(err, agent.ErrInvalidConfig) {
		t.Fatalf("missing root: %v", err)
	}
	if !New().Info().Capabilities.Configuration["options.addDirs"].Startup {
		t.Fatal("missing startup capability")
	}
}

func TestPermissionOverridesDoNotLeakBetweenTurns(t *testing.T) {
	log := filepath.Join(t.TempDir(), "permissions.jsonl")
	cwd := t.TempDir()
	s := startSession(t, newFake(t, "HK_FAKE_CODEX_REQUEST_LOG="+log), agent.SessionConfig{Cwd: cwd, Options: Options{Sandbox: "workspace-write", ApprovalPolicy: "on-request", ApprovalsReviewer: "user"}}, &recorder{})
	base := s.initialTurnConfig()
	full := base.Clone()
	full.Options["sandbox"] = json.RawMessage(`"danger-full-access"`)
	full.Options["approvalPolicy"] = json.RawMessage(`"never"`)
	auto := base.Clone()
	auto.Options["approvalsReviewer"] = json.RawMessage(`"auto_review"`)
	for _, selection := range []*agent.TurnConfig{&base, &full, &auto, nil} {
		if _, err := s.Prompt(context.Background(), agent.Turn{ID: "t", Input: agent.Text("hi"), Config: selection}); err != nil {
			t.Fatal(err)
		}
	}
	requests := recordedRequests(t, log, "turn/start")
	for i, want := range []string{"workspaceWrite", "dangerFullAccess", "workspaceWrite", "workspaceWrite"} {
		if requests[i]["sandboxPolicy"].(map[string]any)["type"] != want {
			t.Fatalf("turn %d: %+v", i, requests[i])
		}
	}
	if requests[1]["approvalPolicy"] != "never" || requests[2]["approvalPolicy"] != "on-request" || requests[2]["approvalsReviewer"] != "auto_review" || requests[3]["approvalsReviewer"] != "user" {
		t.Fatalf("permission override leaked: %+v", requests)
	}
}

func TestSettingsDiscoveryDoesNotCreateThread(t *testing.T) {
	log := filepath.Join(t.TempDir(), "discovery.jsonl")
	rt := newFake(t, "HK_FAKE_CODEX_REQUEST_LOG="+log)
	fields, err := rt.DescribeConfiguration(context.Background(), agent.SessionConfig{Model: "gpt-other"})
	if err != nil {
		t.Fatal(err)
	}
	if fields["model"].Source != "reported" || len(fields["model"].Choices) != 2 || string(fields["effort"].Default) != `"low"` {
		t.Fatalf("bad catalogue: %+v", fields)
	}
	if len(recordedRequests(t, log, "thread/start")) != 0 || len(recordedRequests(t, log, "turn/start")) != 0 {
		t.Fatal("discovery started an agent")
	}
}

func TestPreparedNativePermissionConfigCanBePreparedAgain(t *testing.T) {
	log := filepath.Join(t.TempDir(), "native-permissions.jsonl")
	s := startSession(t, newFake(t, "HK_FAKE_CODEX_REQUEST_LOG="+log), agent.SessionConfig{Options: Options{Config: map[string]any{"approvals_reviewer": "auto_review", "sandbox_mode": "workspace-write"}}}, &recorder{})
	prepared, err := s.PrepareTurn(context.Background(), s.initialTurnConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Prompt(context.Background(), agent.Turn{ID: "t", Input: agent.Text("hi"), Config: &prepared}); err != nil {
		t.Fatal(err)
	}
	request := recordedRequests(t, log, "turn/start")[0]
	if request["approvalsReviewer"] != "auto_review" || request["sandboxPolicy"].(map[string]any)["type"] != "workspaceWrite" {
		t.Fatalf("native permissions: %+v", request)
	}
}
