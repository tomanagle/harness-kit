package acp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/tomanagle/harness-kit/agent"
)

// Options contains native ACP selections. Config addresses the agent's exact
// option IDs; it works even when an agent omits optional UX categories.
// AuthMethod and the ID mappings are fixed for the lifetime of a session.
type Options struct {
	AuthMethod     string                     `json:"authMethod,omitempty"`
	ModelConfigID  string                     `json:"modelConfigId,omitempty"`
	EffortConfigID string                     `json:"effortConfigId,omitempty"`
	Config         map[string]json.RawMessage `json:"config,omitempty"`
}

// ConfigOption is a negotiated ACP configuration control. Options contains
// the native select choices (including groups), preserving agent extensions.
type ConfigOption struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Description  string            `json:"description,omitempty"`
	Category     string            `json:"category,omitempty"`
	Type         string            `json:"type"`
	CurrentValue json.RawMessage   `json:"currentValue"`
	Options      []json.RawMessage `json:"options,omitempty"`
}

// ConfigOptions returns a deep copy suitable for a host's settings interface.
func (s *Session) ConfigOptions() []ConfigOption {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, _ := json.Marshal(s.configOptions)
	var out []ConfigOption
	_ = json.Unmarshal(data, &out)
	return out
}

func (s *Session) replaceConfig(options []ConfigOption) {
	if options == nil {
		return
	}
	s.mu.Lock()
	s.configOptions = options
	s.mu.Unlock()
}

func (s *Session) captureDefaults(saved *agent.TurnConfig) {
	if saved != nil {
		s.native = saved.Clone()
		return
	}
	values := map[string]json.RawMessage{}
	for _, option := range s.ConfigOptions() {
		if option.Type == "select" || option.Type == "boolean" {
			values[option.ID] = bytes.Clone(option.CurrentValue)
		}
	}
	data, _ := json.Marshal(values)
	s.native = agent.TurnConfig{Options: map[string]json.RawMessage{"config": data}}
	modelID, effortID := s.selectionIDs()
	_ = json.Unmarshal(values[modelID], &s.native.Model)
	_ = json.Unmarshal(values[effortID], &s.native.Effort)
}

func (s *Session) NativeDefaults() agent.TurnConfig { return s.native.Clone() }

func (s *Session) initialConfig() agent.TurnConfig {
	data, _ := json.Marshal(s.opts)
	var options map[string]json.RawMessage
	_ = json.Unmarshal(data, &options)
	return agent.TurnConfig{Model: s.cfg.Model, Effort: s.cfg.Effort, Options: options}
}

func (s *Session) selectionIDs() (string, string) {
	model, effort := s.opts.ModelConfigID, s.opts.EffortConfigID
	for _, option := range s.ConfigOptions() {
		if model == "" && option.Category == "model" {
			model = option.ID
		}
		if effort == "" && option.Category == "thought_level" {
			effort = option.ID
		}
	}
	return model, effort
}

func (s *Session) PrepareTurn(ctx context.Context, selected agent.TurnConfig) (agent.TurnConfig, error) {
	if err := ctx.Err(); err != nil {
		return agent.TurnConfig{}, err
	}
	selected = selected.Clone()
	var opts Options
	if err := agent.DecodeOptions(selected.Options, &opts); err != nil {
		return agent.TurnConfig{}, fmt.Errorf("%w: %v", agent.ErrInvalidConfig, err)
	}
	if opts.AuthMethod != s.opts.AuthMethod || opts.ModelConfigID != s.opts.ModelConfigID || opts.EffortConfigID != s.opts.EffortConfigID {
		return agent.TurnConfig{}, fmt.Errorf("%w: ACP authentication and config ID mappings require a new session", agent.ErrUnsupported)
	}
	values := map[string]json.RawMessage{}
	_ = json.Unmarshal(s.native.Options["config"], &values)
	if values == nil {
		values = map[string]json.RawMessage{}
	}
	for id, value := range opts.Config {
		values[id] = bytes.Clone(value)
	}
	modelID, effortID := s.selectionIDs()
	if selected.Model != "" {
		if modelID == "" {
			return agent.TurnConfig{}, fmt.Errorf("%w: agent has no model config option; set modelConfigId if its category is absent", agent.ErrUnsupported)
		}
		values[modelID], _ = json.Marshal(selected.Model)
	} else if raw := values[modelID]; len(raw) > 0 {
		_ = json.Unmarshal(raw, &selected.Model)
	}
	if selected.Effort != "" {
		if effortID == "" {
			return agent.TurnConfig{}, fmt.Errorf("%w: agent has no reasoning effort config option; set effortConfigId if its category is absent", agent.ErrUnsupported)
		}
		values[effortID], _ = json.Marshal(selected.Effort)
	} else if raw := values[effortID]; len(raw) > 0 {
		_ = json.Unmarshal(raw, &selected.Effort)
	}
	options := s.ConfigOptions()
	modelChanges := false
	for _, option := range options {
		if option.ID == modelID && !equalValue(option.CurrentValue, values[modelID]) {
			modelChanges = true
		}
	}
	for id, value := range values {
		found := false
		for _, option := range options {
			if option.ID == id {
				found = true
				if err := validateOption(option, value, modelChanges && id != modelID); err != nil {
					return agent.TurnConfig{}, err
				}
				break
			}
		}
		if !found {
			return agent.TurnConfig{}, fmt.Errorf("%w: agent did not advertise config option %q", agent.ErrUnsupported, id)
		}
	}
	if selected.Options == nil {
		selected.Options = map[string]json.RawMessage{}
	}
	if len(values) > 0 {
		selected.Options["config"], _ = json.Marshal(values)
	}
	return selected, nil
}

func (s *Session) applyConfig(ctx context.Context, selected agent.TurnConfig) error {
	var opts Options
	_ = agent.DecodeOptions(selected.Options, &opts)
	ids := slices.Sorted(maps.Keys(opts.Config))
	modelID, _ := s.selectionIDs()
	if i := slices.Index(ids, modelID); i >= 0 {
		ids = append([]string{modelID}, append(slices.Clone(ids[:i]), ids[i+1:]...)...)
	}
	for _, id := range ids {
		var current *ConfigOption
		for _, option := range s.ConfigOptions() {
			if option.ID == id {
				value := option
				current = &value
				break
			}
		}
		if current == nil {
			return fmt.Errorf("%w: agent removed config option %q", agent.ErrUnsupported, id)
		}
		value := opts.Config[id]
		if err := validateOption(*current, value, false); err != nil {
			return err
		}
		if equalValue(current.CurrentValue, value) {
			continue
		}
		params := map[string]any{"sessionId": s.id, "configId": id, "value": value}
		if current.Type == "boolean" {
			params["type"] = "boolean"
		}
		var result sessionResponse
		if err := s.rpc(ctx, "session/set_config_option", params, &result); err != nil {
			// Cancellation can leave an unacknowledged mutation in flight. Closing
			// prevents a later turn from racing its eventual application.
			if ctx.Err() != nil {
				_ = s.Close(context.Background())
				return fmt.Errorf("%w: interrupted ACP configuration update", agent.ErrSessionDead)
			}
			return fmt.Errorf("acp set config %q: %w", id, err)
		}
		if result.ConfigOptions == nil {
			return fmt.Errorf("acp: set_config_option omitted configOptions")
		}
		s.replaceConfig(result.ConfigOptions)
		confirmed := false
		for _, option := range result.ConfigOptions {
			if option.ID == id && equalValue(option.CurrentValue, value) {
				confirmed = true
			}
		}
		if !confirmed {
			return fmt.Errorf("acp: agent did not apply config option %q", id)
		}
	}
	return nil
}

func validValue(value json.RawMessage) bool {
	var v any
	if json.Unmarshal(value, &v) != nil {
		return false
	}
	switch v.(type) {
	case string, bool:
		return true
	default:
		return false
	}
}
func equalValue(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	switch value := x.(type) {
	case string:
		other, ok := y.(string)
		return ok && value == other
	case bool:
		other, ok := y.(bool)
		return ok && value == other
	default:
		return false
	}
}

func validateOption(option ConfigOption, value json.RawMessage, allowDependent bool) error {
	var v any
	if json.Unmarshal(value, &v) != nil {
		return fmt.Errorf("%w: invalid ACP config value for %q", agent.ErrInvalidConfig, option.ID)
	}
	switch option.Type {
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("%w: config %q requires a boolean", agent.ErrInvalidConfig, option.ID)
		}
	case "select":
		str, ok := v.(string)
		if !ok {
			return fmt.Errorf("%w: config %q requires a string", agent.ErrInvalidConfig, option.ID)
		}
		if !allowDependent && !containsChoice(option.Options, str) {
			return fmt.Errorf("%w: config %q does not offer %q", agent.ErrInvalidConfig, option.ID, str)
		}
	default:
		return fmt.Errorf("%w: ACP config type %q", agent.ErrUnsupported, option.Type)
	}
	return nil
}

func containsChoice(choices []json.RawMessage, value string) bool {
	for _, raw := range choices {
		var choice struct {
			Value   *string           `json:"value"`
			Options []json.RawMessage `json:"options"`
		}
		if json.Unmarshal(raw, &choice) == nil && ((choice.Value != nil && *choice.Value == value) || containsChoice(choice.Options, value)) {
			return true
		}
	}
	return false
}
