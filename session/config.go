package session

import (
	"context"
	"fmt"
	"reflect"

	"github.com/tomanagle/harness-kit/agent"
	"github.com/tomanagle/harness-kit/config"
)

// ConfigOperation identifies when the host is validating a configuration.
type ConfigOperation string

const (
	ConfigCreate    ConfigOperation = "create"
	ConfigRestore   ConfigOperation = "restore"
	ConfigConfigure ConfigOperation = "configure"
	ConfigPrompt    ConfigOperation = "prompt"
)

// ConfigRequest is passed to the host validator. Config is an independent copy;
// changing it does not modify accepted settings. Return an error to reject it.
type ConfigRequest struct {
	Operation ConfigOperation
	SessionID string
	Runtime   string
	Config    agent.SessionConfig
}

func (m *Manager) validateConfig(ctx context.Context, rt agent.Runtime, id string, op ConfigOperation, cfg agent.SessionConfig) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if validator, ok := rt.(agent.Configurable); ok {
		if err := validator.ValidateConfig(cfg); err != nil {
			return err
		}
	}
	if guarded, ok := rt.(agent.Guarded); ok && !m.opts.AllowUnsafe {
		if reason := guarded.Unsafe(cfg); reason != "" {
			return fmt.Errorf("%w: %s", ErrUnsafe, reason)
		}
	}
	if m.opts.ValidateConfig != nil {
		copy, err := cfg.Clone()
		if err != nil {
			return err
		}
		if err := m.opts.ValidateConfig(ctx, ConfigRequest{Operation: op, SessionID: id, Runtime: rt.Info().Name, Config: copy}); err != nil {
			return err
		}
	}
	return nil
}

func validatePatch(rt agent.Runtime, patch agent.ConfigPatch, turn bool) error {
	fields, err := patch.Fields()
	if err != nil {
		return err
	}
	support := rt.Info().Capabilities.Configuration
	for _, field := range fields {
		setting := support[field]
		allowed := setting.Session
		if turn {
			allowed = setting.Turn
		}
		if !allowed {
			return fmt.Errorf("%w: %s does not support %s changes for %s", agent.ErrUnsupported, rt.Info().Name, field, map[bool]string{true: "one turn", false: "session defaults"}[turn])
		}
	}
	return nil
}

// Configure updates defaults for subsequent turns. The active turn keeps its
// accepted snapshot. Stored sessions can be configured without starting a
// runtime. Reset fields inherit the original creation configuration.
func (m *Manager) Configure(ctx context.Context, id string, patch agent.ConfigPatch) (Info, error) {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	info, err := m.configureLocked(ctx, id, patch, nil)
	if err == nil {
		m.publishSessionSettingsLocked(ctx, id)
	}
	return info, err
}

func (m *Manager) configureLocked(ctx context.Context, id string, patch agent.ConfigPatch, pin *bool) (Info, error) {
	ms, ok := m.lookup(id)
	if !ok {
		rec, err := m.store.Get(ctx, id)
		if err != nil {
			return Info{}, err
		}
		rec.Info, err = m.storedInfo(ctx, rec.Info)
		if err != nil {
			return Info{}, err
		}
		ms, err = m.newManaged(ctx, rec.Info, rec.Config, true)
		if err != nil {
			return Info{}, err
		}
	}
	rt, ok := m.reg.Get(ms.info.Runtime)
	if !ok {
		return Info{}, fmt.Errorf("%w: %s", ErrRuntimeNotFound, ms.info.Runtime)
	}
	if err := validatePatch(rt, patch, false); err != nil {
		return Info{}, err
	}
	ms.emitMu.Lock()
	defer ms.emitMu.Unlock()
	ms.mu.Lock()
	if ms.info.State == StateStarting || ms.info.State == StateClosing || ms.info.State == StateSuspending {
		ms.mu.Unlock()
		return Info{}, ErrBusy
	}
	current, err := ms.cfg.Clone()
	initial := ms.initialCfg
	ms.mu.Unlock()
	if err != nil {
		return Info{}, err
	}
	base, err := current.Settings()
	if err != nil {
		return Info{}, err
	}
	inherited, err := initial.Settings()
	if err != nil {
		return Info{}, err
	}
	next, err := config.Apply(base, patch, inherited)
	if err != nil {
		return Info{}, err
	}
	candidate := current.WithSettings(next)
	if err := m.validateConfig(ctx, rt, id, ConfigConfigure, candidate); err != nil {
		return Info{}, err
	}
	if preparer, ok := ms.sess.(agent.TurnPreparer); ok {
		// Preflight only. Keep inherited selections in session defaults; native
		// resolution belongs to each accepted turn snapshot.
		if _, err := preparer.PrepareTurn(ctx, next.Clone()); err != nil {
			return Info{}, err
		}
	}
	ms.mu.Lock()
	if reflect.DeepEqual(base, next) && (pin == nil || *pin == ms.info.Pinned) {
		ms.mu.Unlock()
		return ms.snapshot(), nil
	}
	oldCfg, oldInfo := ms.cfg, ms.info
	ms.info.SettingsRevision = max(uint64(1), ms.info.SettingsRevision) + 1
	ms.cfg = candidate
	if pin != nil {
		ms.info.Pinned = *pin
	}
	selection := next.Clone()
	ms.info.Config = &selection
	ms.info.Model = next.Model
	ms.mu.Unlock()
	if err := ms.persist(); err != nil {
		ms.mu.Lock()
		ms.cfg, ms.info = oldCfg, oldInfo
		ms.mu.Unlock()
		return Info{}, err
	}
	ms.updatedLocked("")
	err = ms.persist()
	return ms.snapshot(), err
}

// prepareTurn produces separate caller-, runtime-, and event-owned snapshots.
// Called with emitMu held, before allocating a turn ID or changing state.
func (ms *managed) prepareTurn(ctx context.Context, patch *agent.ConfigPatch) (agent.TurnConfig, error) {
	ms.mu.Lock()
	cfg, err := ms.cfg.Clone()
	ms.mu.Unlock()
	if err != nil {
		return agent.TurnConfig{}, err
	}
	base, err := cfg.Settings()
	if err != nil {
		return agent.TurnConfig{}, err
	}
	selected := base.Clone()
	if patch != nil {
		if err := validatePatch(ms.rt, *patch, true); err != nil {
			return agent.TurnConfig{}, err
		}
		selected, err = config.Apply(base, *patch, base)
		if err != nil {
			return agent.TurnConfig{}, err
		}
	}
	if err := ms.m.validateConfig(ctx, ms.rt, ms.info.ID, ConfigPrompt, cfg.WithSettings(selected)); err != nil {
		return agent.TurnConfig{}, err
	}
	if preparer, ok := ms.sess.(agent.TurnPreparer); ok {
		selected, err = preparer.PrepareTurn(ctx, selected.Clone())
		if err != nil {
			return agent.TurnConfig{}, err
		}
		// Resolved selections must also satisfy host constraints.
		if err := ms.m.validateConfig(ctx, ms.rt, ms.info.ID, ConfigPrompt, cfg.WithSettings(selected)); err != nil {
			return agent.TurnConfig{}, err
		}
	}
	return selected.Clone(), nil
}
