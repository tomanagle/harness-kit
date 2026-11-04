package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"github.com/tomanagle/harness-kit/core/config"
	"github.com/tomanagle/harness-kit/core/mcp"
)

// TurnConfig is a full per-turn snapshot, including adapter-native options.
type TurnConfig = config.Settings

// ConfigPatch changes session defaults or one turn without mutating its parent.
type ConfigPatch = config.Patch

// ErrInvalidConfig identifies invalid configuration values.
var ErrInvalidConfig = config.ErrInvalid

// Configurable validates configuration without starting processes or turns.
// Managers use the same validation at create, restore, configure and prompt.
type Configurable interface{ ValidateConfig(SessionConfig) error }

// TurnPreparer validates and resolves selections against the running runtime.
// It must not start a turn or change session defaults. Empty model/effort must
// resolve to native defaults without inheriting a previous turn's overrides;
// return ErrUnsupported when the adapter cannot guarantee that behavior.
type TurnPreparer interface {
	PrepareTurn(context.Context, TurnConfig) (TurnConfig, error)
}

// DefaultsProvider exposes captured native defaults for durable resume.
// Return an independent snapshot; the manager may call this concurrently.
type DefaultsProvider interface{ NativeDefaults() TurnConfig }

// Settings extracts an independent portable/native configuration selection.
// Legacy options.effort is accepted at startup; portable Effort takes precedence.
func (cfg SessionConfig) Settings() (TurnConfig, error) {
	out := TurnConfig{Model: cfg.Model, Effort: cfg.Effort}
	if cfg.Options != nil {
		var raw []byte
		switch v := cfg.Options.(type) {
		case json.RawMessage:
			raw = v
		case []byte:
			raw = v
		case string:
			raw = []byte(v)
		default:
			var err error
			raw, err = json.Marshal(v)
			if err != nil {
				return out, fmt.Errorf("%w: %v", ErrInvalidConfig, err)
			}
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &out.Options); err != nil {
				return out, fmt.Errorf("%w: options must be an object: %v", ErrInvalidConfig, err)
			}
		}
	}
	if value, ok := out.Options["effort"]; ok {
		if out.Effort == "" {
			if err := json.Unmarshal(value, &out.Effort); err != nil {
				return out, fmt.Errorf("%w: effort must be a string", ErrInvalidConfig)
			}
		}
		delete(out.Options, "effort")
	}
	return out.Clone(), nil
}

// WithSettings replaces the turn-configurable selection while retaining wiring.
func (cfg SessionConfig) WithSettings(settings TurnConfig) SessionConfig {
	settings = settings.Clone()
	cfg.Model, cfg.Effort, cfg.Options = settings.Model, settings.Effort, settings.Options
	return cfg
}

// Clone snapshots caller-owned maps, slices and adapter options. PermissionTool
// is internal wiring and is preserved without serializing it.
func (cfg SessionConfig) Clone() (SessionConfig, error) {
	settings, err := cfg.Settings()
	if err != nil {
		return SessionConfig{}, err
	}
	out := cfg.WithSettings(settings)
	out.HostTools = slices.Clone(cfg.HostTools)
	if cfg.NativeDefaults != nil {
		defaults := cfg.NativeDefaults.Clone()
		out.NativeDefaults = &defaults
	}
	if cfg.MCP != nil {
		raw, err := json.Marshal(cfg.MCP)
		if err != nil {
			return SessionConfig{}, err
		}
		out.MCP = new(mcp.Config)
		if err := json.Unmarshal(raw, out.MCP); err != nil {
			return SessionConfig{}, err
		}
	}
	return out, nil
}
