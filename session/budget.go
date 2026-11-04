package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"

	"github.com/tomanagle/harness-kit/agent"
	"github.com/tomanagle/harness-kit/budget"
	"github.com/tomanagle/harness-kit/event"
	"github.com/tomanagle/harness-kit/settings"
)

var ErrBudgetExceeded = errors.New("session: observed usage budget exhausted")

type turnBudget struct {
	tokensSet, costSet bool
	limits             budget.Limits
}

func (t turnBudget) effective(base budget.Limits) budget.Limits {
	if t.tokensSet {
		base.MaxTurnTokens = t.limits.MaxTurnTokens
	}
	if t.costSet {
		base.MaxTurnCostUSD = t.limits.MaxTurnCostUSD
	}
	return base
}

func (ms *managed) prepareBudget(p settings.Patch) (turnBudget, budget.Limits, error) {
	ms.mu.Lock()
	policy := ms.info.Budget.Clone()
	ledger := ms.accounting
	ms.mu.Unlock()
	selection := turnBudget{}
	if raw, ok := p["budget"]; ok {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
			return selection, policy, fmt.Errorf("%w: budget must be an object", settings.ErrInvalid)
		}
		for key := range fields {
			switch key {
			case "maxTurnTokens":
				selection.tokensSet = true
			case "maxTurnCostUsd":
				selection.costSet = true
			default:
				return selection, policy, fmt.Errorf("%w: budget.%s is not turn-configurable", settings.ErrScope, key)
			}
		}
		v := settings.Defaults()
		v.Budget = policy
		next, _, err := settings.Apply(v, settings.Patch{"budget": raw}, nil, v)
		if err != nil {
			return selection, policy, err
		}
		selection.limits = next.Budget
		policy = next.Budget
	}
	if err := validateBudgetPolicy(ms.rt, policy); err != nil {
		return selection, policy, err
	}
	if hit := ledger.Check(policy, true); hit != nil {
		return selection, policy, fmt.Errorf("%w: %s %s observed %g, limit %g", ErrBudgetExceeded, hit.Scope, hit.Metric, hit.Observed, hit.Limit)
	}
	return selection, policy, nil
}

func observation(u event.UsageData, terminal bool) budget.Observation {
	o := budget.Observation{Scope: u.Scope, Mode: u.Mode, CostUSD: u.CostUSD}
	if terminal {
		o.Mode = "snapshot"
	}
	// Cached/reasoning counts may already be included in input/output. Adapters
	// should supply TotalTokens when they know a different counting convention.
	n := u.TotalTokens
	if n == 0 && u.InputTokens >= 0 && u.OutputTokens >= 0 && u.InputTokens <= math.MaxInt64-u.OutputTokens {
		n = u.InputTokens + u.OutputTokens
	}
	if n > 0 || u.TokensKnown {
		o.Tokens = &n
	}
	return o
}

func replayBudgetEvent(l *budget.Ledger, ev event.Event) {
	switch d := ev.Data.(type) {
	case event.TurnStartedData:
		l.Begin()
	case event.UsageData:
		l.Observe(observation(d, false))
	case event.TurnCompletedData:
		if d.Usage != nil {
			l.Observe(observation(*d.Usage, true))
		}
		l.End()
	}
}

// observeUsageLocked and checkBudgetLocked run with emitMu held. They do not
// wait for runtime shutdown; cancellation follows the adapter's existing path.
func (ms *managed) observeUsageLocked(u event.UsageData, terminal bool) {
	ms.mu.Lock()
	ms.accounting.Observe(observation(u, terminal))
	ms.info.Usage = ms.accounting.Usage()
	ms.mu.Unlock()
	ms.checkBudgetLocked()
}
func (ms *managed) checkBudgetLocked() {
	ms.mu.Lock()
	t := ms.turn
	if t == nil || t.budgetStop != nil {
		ms.mu.Unlock()
		return
	}
	hit := ms.accounting.Check(t.budget.effective(ms.info.Budget), false)
	if hit == nil {
		ms.mu.Unlock()
		return
	}
	t.budgetStop = hit
	ms.mu.Unlock()
	t.cancel()
	ms.emitLocked(event.Event{Type: event.BudgetExceeded, TurnID: t.ID, Data: *hit})
}

// BudgetUsage returns a durable session's observed accounting without starting
// a runtime. The same values are included in Info and session.updated.
func (m *Manager) BudgetUsage(ctx context.Context, id string) (budget.Usage, error) {
	info, err := m.Get(ctx, id)
	return info.Usage.Clone(), err
}

func validateBudgetPolicy(rt agent.Runtime, p budget.Limits) error {
	caps := rt.Info().Capabilities
	if !caps.Usage && ((p.MaxSessionTokens != nil && *p.MaxSessionTokens > 0) || (p.MaxTurnTokens != nil && *p.MaxTurnTokens > 0)) {
		return fmt.Errorf("%w: %s does not report consumed token usage for budget enforcement", agent.ErrUnsupported, rt.Info().Name)
	}
	if !caps.Cost && ((p.MaxSessionCostUSD != nil && *p.MaxSessionCostUSD > 0) || (p.MaxTurnCostUSD != nil && *p.MaxTurnCostUSD > 0)) {
		return fmt.Errorf("%w: %s does not report cost estimates for budget enforcement", agent.ErrUnsupported, rt.Info().Name)
	}
	return nil
}
