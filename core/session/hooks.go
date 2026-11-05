package session

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/hooks"
)

// Hooks is the shared registry. Registration is process-local; handlers are
// registered again when their host starts. Settings.Hooks supplies live policy.
func (m *Manager) Hooks() hooks.Registration { return m.opts.Hooks }
func (m *Manager) runHooks(ctx context.Context, request hooks.Request) (hooks.Request, []string, error) {
	result, warnings, err := m.opts.Hooks.Run(ctx, request, m.currentSettings().Hooks)
	for _, warning := range warnings {
		m.log.WarnContext(ctx, "lifecycle hook warning", "hook", request.Name, "turnId", request.TurnID, "message", warning)
	}
	return result, warnings, err
}
func (ms *managed) hookRequestLocked(turnID string, input []agent.InputPart, patch *agent.ConfigPatch) hooks.Request {
	cfg, _ := ms.cfg.Clone()
	return hooks.Request{Name: hooks.BeforeTurn, InvocationID: ms.info.ID + ":" + turnID + ":turn.before", TurnID: turnID, IsFirstTurn: ms.info.Turns == ms.info.InheritedTurns, Input: input, Config: patch, Session: &hooks.Session{ID: ms.info.ID, Runtime: ms.info.Runtime, Title: ms.info.Title, Turns: ms.info.Turns, Metadata: maps.Clone(ms.info.Metadata), Config: cfg}}
}
func (m *Manager) cancelHookAdmission(id string) bool {
	m.hookMu.Lock()
	cancel := m.hookAdmissions[id]
	m.hookMu.Unlock()
	if cancel != nil {
		cancel()
		return true
	}
	return false
}
func (m *Manager) hookAdmissionActive(id string) bool {
	m.hookMu.Lock()
	defer m.hookMu.Unlock()
	return m.hookAdmissions[id] != nil
}

// Prompt reserves preparation per session, then invokes application code with
// no manager lock held. Admission validates the returned input/settings again.
// Reentrant prompts on the same session return ErrBusy instead of deadlocking.
func (m *Manager) Prompt(ctx context.Context, id string, p PromptParams) (*Turn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if p.Settings != nil {
		if p.Config != nil {
			return nil, fmt.Errorf("%w: settings and config cannot both be supplied", agent.ErrInvalidConfig)
		}
		patch, err := promptSettings(p.Settings)
		if err != nil {
			return nil, err
		}
		p.Config = &patch
		p.Settings = maps.Clone(p.Settings)
		delete(p.Settings, "session")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(m.baseCtx, cancel)
	defer stop()
	m.hookMu.Lock()
	if m.hookAdmissions == nil {
		m.hookAdmissions = map[string]context.CancelFunc{}
	}
	if m.hookAdmissions[id] != nil {
		m.hookMu.Unlock()
		return nil, ErrBusy
	}
	m.hookAdmissions[id] = cancel
	m.hookMu.Unlock()
	admitted := false
	defer func() {
		if !admitted {
			m.hookMu.Lock()
			delete(m.hookAdmissions, id)
			m.hookMu.Unlock()
		}
	}()
	if p.TurnID == "" {
		p.TurnID = NewID("turn_")
	}
	info, err := m.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if info.State != StateIdle && !(info.State == StateSuspended && m.memoryPolicy().ResumeOnPrompt) {
		if info.State.Terminal() {
			return nil, ErrClosed
		}
		return nil, ErrBusy
	}
	if lookup, ok := m.store.(TurnLookup); ok {
		used, err := lookup.HasTurn(ctx, id, p.TurnID)
		if err != nil {
			return nil, err
		}
		if used {
			return nil, ErrDuplicateTurn
		}
	}
	var req hooks.Request
	if ms, ok := m.lookup(id); ok {
		ms.mu.Lock()
		_, duplicate := ms.turnIDs[p.TurnID]
		req = ms.hookRequestLocked(p.TurnID, p.Input, p.Config)
		ms.mu.Unlock()
		if duplicate {
			return nil, ErrDuplicateTurn
		}
	} else {
		rec, err := m.store.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		req = hooks.Request{Name: hooks.BeforeTurn, InvocationID: id + ":" + p.TurnID + ":turn.before", TurnID: p.TurnID, IsFirstTurn: info.Turns == info.InheritedTurns, Input: p.Input, Config: p.Config, Session: &hooks.Session{ID: id, Runtime: info.Runtime, Title: info.Title, Turns: info.Turns, Metadata: maps.Clone(info.Metadata), Config: rec.Config}}
	}
	req, _, err = m.runHooks(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("prepare prompt: %w", err)
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	// Own executor output without round-tripping nil Settings patches through
	// their deliberately strict JSON decoder.
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var owned hooks.Request
	if err = json.Unmarshal(raw, &owned); err != nil {
		return nil, err
	}
	p.Input, p.Config = owned.Input, owned.Config
	turn, err := m.prompt(ctx, id, p)
	admitted = err == nil
	return turn, err
}
