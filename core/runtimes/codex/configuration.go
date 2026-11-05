package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/tomanagle/harness-kit/core/agent"
)

// captureDefaults snapshots native configuration once, before thread overrides.
// Missing optional introspection does not prevent startup: an operation that
// needs an unavailable default fails explicitly in PrepareTurn instead.
func (s *Session) captureDefaults(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, s.rt.startTimeout)
	defer cancel()
	var native struct {
		Config struct {
			Sandbox   string          `json:"sandbox_mode"`
			Approval  string          `json:"approval_policy"`
			Reviewer  string          `json:"approvals_reviewer"`
			Workspace workspacePolicy `json:"sandbox_workspace_write"`
			Model     string          `json:"model"`
			Effort    string          `json:"model_reasoning_effort"`
		} `json:"config"`
	}
	params := map[string]any{"includeLayers": false}
	if s.cfg.Cwd != "" {
		params["cwd"] = s.cfg.Cwd
	}
	if err := s.conn.Call(ctx, "config/read", params, &native); err == nil {
		s.nativeModel, s.nativeEffort = native.Config.Model, native.Config.Effort
		s.nativeSandbox, s.nativeApproval, s.nativeReviewer, s.nativeWorkspace = native.Config.Sandbox, native.Config.Approval, native.Config.Reviewer, native.Config.Workspace
	}
	s.modelEfforts = make(map[string]string)
	s.supportedEfforts = make(map[string][]string)
	cursor := ""
	seen := make(map[string]bool)
	for {
		params := map[string]any{"limit": 100, "includeHidden": true}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var models struct {
			Data []struct {
				ID        string `json:"id"`
				Model     string `json:"model"`
				Default   bool   `json:"isDefault"`
				Effort    string `json:"defaultReasoningEffort"`
				Supported []struct {
					Effort string `json:"reasoningEffort"`
				} `json:"supportedReasoningEfforts"`
			} `json:"data"`
			NextCursor *string `json:"nextCursor"`
		}
		if err := s.conn.Call(ctx, "model/list", params, &models); err != nil {
			return
		}
		for _, model := range models.Data {
			var supported []string
			for _, effort := range model.Supported {
				supported = append(supported, effort.Effort)
			}
			if len(supported) > 0 {
				s.supportedEfforts[model.Model] = supported
				s.supportedEfforts[model.ID] = supported
			}
			if model.Model != "" {
				s.modelEfforts[model.Model] = model.Effort
			}
			if model.ID != "" {
				s.modelEfforts[model.ID] = model.Effort
			}
			if s.nativeModel == "" && model.Default {
				s.nativeModel = model.Model
			}
		}
		if models.NextCursor == nil || *models.NextCursor == "" || seen[*models.NextCursor] {
			return
		}
		cursor = *models.NextCursor
		seen[cursor] = true
	}
}

func (s *Session) initialTurnConfig() agent.TurnConfig {
	settings := agent.TurnConfig{Model: s.cfg.Model, Effort: s.cfg.Effort}
	if settings.Effort == "" {
		settings.Effort = s.opts.Effort
	}
	encoded, _ := json.Marshal(s.opts)
	_ = json.Unmarshal(encoded, &settings.Options)
	delete(settings.Options, "effort")
	return settings
}

// PrepareTurn validates and materializes model/effort without running a model.
// Empty selections inherit the captured native defaults. Codex treats omitted
// model/effort as sticky, so an unresolved default cannot safely be omitted.
func (s *Session) PrepareTurn(ctx context.Context, settings agent.TurnConfig) (agent.TurnConfig, error) {
	settings = settings.Clone()
	if err := ctx.Err(); err != nil {
		return agent.TurnConfig{}, err
	}
	if _, legacy := settings.Options["effort"]; legacy {
		return agent.TurnConfig{}, fmt.Errorf("%w: use the portable effort field for turn configuration", agent.ErrInvalidConfig)
	}
	cfg := s.cfg
	cfg.Model, cfg.Effort, cfg.Options = settings.Model, settings.Effort, settings.Options
	if err := s.rt.ValidateConfig(cfg); err != nil {
		return agent.TurnConfig{}, err
	}
	var options Options
	_ = agent.DecodeOptions(settings.Options, &options)
	baseline := s.opts
	options.Effort, baseline.Effort = "", ""
	options.Sandbox, baseline.Sandbox = "", ""
	options.ApprovalPolicy, baseline.ApprovalPolicy = "", ""
	options.ApprovalsReviewer, baseline.ApprovalsReviewer = "", ""
	actual, _ := json.Marshal(options)
	initial, _ := json.Marshal(baseline)
	if string(actual) != string(initial) {
		return agent.TurnConfig{}, fmt.Errorf("%w: codex native options require a new session", agent.ErrUnsupported)
	}
	if settings.Model == "" {
		settings.Model = s.nativeModel
	}
	if settings.Model == "" {
		return agent.TurnConfig{}, fmt.Errorf("%w: codex did not report its native default model; set model explicitly", agent.ErrUnsupported)
	}
	if settings.Effort == "" {
		settings.Effort = s.nativeEffort
		if settings.Effort == "" {
			settings.Effort = s.modelEfforts[settings.Model]
		}
	}
	if settings.Effort == "" {
		return agent.TurnConfig{}, fmt.Errorf("%w: codex did not report a default effort for model %q; set effort explicitly", agent.ErrUnsupported, settings.Model)
	}
	if supported := s.supportedEfforts[settings.Model]; len(supported) > 0 && !slices.Contains(supported, settings.Effort) {
		return agent.TurnConfig{}, fmt.Errorf("%w: codex model %q does not advertise effort %q", agent.ErrInvalidConfig, settings.Model, settings.Effort)
	}
	var permissions Options
	_ = agent.DecodeOptions(settings.Options, &permissions)
	if err := s.resolvePermissions(&settings, permissions); err != nil {
		return agent.TurnConfig{}, err
	}
	return settings, nil
}

// NativeDefaults returns the immutable baseline needed to resume without
// inheriting a sticky per-turn override from Codex's saved thread.
func (s *Session) NativeDefaults() agent.TurnConfig {
	models, _ := json.Marshal(s.modelEfforts)
	supported, _ := json.Marshal(s.supportedEfforts)
	sandbox, _ := json.Marshal(s.nativeSandbox)
	approval, _ := json.Marshal(s.nativeApproval)
	reviewer, _ := json.Marshal(s.nativeReviewer)
	workspace, _ := json.Marshal(s.nativeWorkspace)
	s.mu.Lock()
	changed, _ := json.Marshal(s.permissionOverrideUsed)
	s.mu.Unlock()
	return agent.TurnConfig{Model: s.nativeModel, Effort: s.nativeEffort,
		Options: map[string]json.RawMessage{"modelEfforts": models, "supportedEfforts": supported, "sandbox": sandbox, "approvalPolicy": approval, "approvalsReviewer": reviewer, "workspacePolicy": workspace, "permissionOverrides": changed}}
}
