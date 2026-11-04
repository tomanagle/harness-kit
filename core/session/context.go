package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/config"
	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/settings"
)

// ContextInfo is a cached context observation. Reading it never starts a
// runtime or makes an inference request. CanCompact is live session availability.
type ContextInfo struct {
	SessionID  string                 `json:"sessionId"`
	State      State                  `json:"state"`
	Usage      event.ContextUsageData `json:"usage"`
	CanCompact bool                   `json:"canCompact"`
}

type compactionOperation struct {
	id, reason, turnID string
	cancel             context.CancelFunc
	done               chan struct{}
	before             event.ContextUsageData
	observation        uint64
	err                error
	budgetStop         bool
}

func compactionPolicy(s config.CompactionSettings) config.CompactionSettings {
	// Records predating the feature have no timeout or thresholds.
	if s.TimeoutMs == 0 {
		s.TimeoutMs = config.CompactionDefaults().TimeoutMs
	}
	return s.Clone()
}
func contextUsage(v *event.ContextUsageData) event.ContextUsageData {
	if v == nil {
		return event.ContextUsageData{Basis: "unknown"}
	}
	return v.Clone()
}

// markContextStaleLocked keeps cached terminal/suspended records consistent
// with their reloaded equivalents. The observation is retained as history.
func (ms *managed) markContextStaleLocked() {
	if ms.info.Context != nil {
		v := ms.info.Context.Clone()
		v.Stale = true
		ms.info.Context = &v
	}
}

func normalizeContext(v event.ContextUsageData) event.ContextUsageData {
	v = v.Clone()
	if v.UsedTokens != nil && *v.UsedTokens < 0 {
		v.UsedTokens = nil
	}
	if v.WindowTokens != nil && *v.WindowTokens <= 0 {
		v.WindowTokens = nil
	}
	switch v.Basis {
	case "reported", "last_request":
	default:
		v.Basis = "unknown"
	}
	if v.UsedTokens == nil {
		v.Basis = "unknown"
	}
	// No token count is manufactured from cumulative billable usage.
	if v.Basis == "unknown" {
		v.UsedTokens = nil
	}
	return v
}
func (m *Manager) Context(ctx context.Context, id string) (ContextInfo, error) {
	if err := ctx.Err(); err != nil {
		return ContextInfo{}, err
	}
	info, err := m.Get(ctx, id)
	if err != nil {
		return ContextInfo{}, err
	}
	out := ContextInfo{SessionID: id, State: info.State, Usage: contextUsage(info.Context)}
	if ms, ok := m.lookup(id); ok {
		ms.mu.Lock()
		sess := ms.sess
		ms.mu.Unlock()
		if c, ok := sess.(agent.Compactor); ok {
			out.CanCompact = c.CanCompact() && info.State == StateIdle && !info.ForkPending
		}
	}
	return out, nil
}

func validateCompaction(rt agent.Runtime, s config.CompactionSettings) error {
	if err := s.Validate(); err != nil {
		return fmt.Errorf("%w: %v", settings.ErrInvalid, err)
	}
	if (s.AutoAtTokens != nil || s.AutoAtPercent != nil) && !rt.Info().Capabilities.Compaction {
		return fmt.Errorf("%w: %s cannot request compaction", agent.ErrUnsupported, rt.Info().Name)
	}
	return nil
}
func (ms *managed) turnCompaction(p settings.Patch) (config.CompactionSettings, error) {
	ms.mu.Lock()
	policy := compactionPolicy(ms.info.Compaction)
	ms.mu.Unlock()
	if raw, ok := p["compaction"]; ok {
		v := settings.Defaults()
		v.Compaction = policy
		next, _, err := settings.Apply(v, settings.Patch{"compaction": raw}, nil, v)
		if err != nil {
			return policy, err
		}
		policy = next.Compaction
	}
	return policy, validateCompaction(ms.rt, policy)
}
func shouldCompact(v event.ContextUsageData, p config.CompactionSettings) bool {
	if v.Stale || v.UsedTokens == nil {
		return false
	}
	if p.AutoAtTokens != nil && *v.UsedTokens >= *p.AutoAtTokens {
		return true
	}
	return p.AutoAtPercent != nil && v.WindowTokens != nil && *v.WindowTokens > 0 &&
		float64(*v.UsedTokens)/float64(*v.WindowTokens)*100 >= *p.AutoAtPercent
}

// Compact requests native compaction and waits for its completion. Only idle,
// live sessions are accepted; restore an inactive session explicitly first.
// The request context cancels the operation, while the session stays reserved
// until the adapter confirms completion or termination.
func (m *Manager) Compact(ctx context.Context, id string) (ContextInfo, error) {
	m.lifecycleMu.Lock()
	op, ms, runCtx, err := m.admitCompaction(ctx, id)
	m.lifecycleMu.Unlock()
	if err != nil {
		return ContextInfo{}, err
	}
	go func() { defer m.turns.Done(); ms.runCompaction(runCtx, op, true) }()
	select {
	case <-op.done:
		info, readErr := m.Context(context.Background(), id)
		return info, errors.Join(op.err, readErr)
	case <-ctx.Done():
		op.cancel()
		return ContextInfo{}, ctx.Err()
	}
}
func (m *Manager) admitCompaction(request context.Context, id string) (*compactionOperation, *managed, context.Context, error) {
	fail := func(err error) (*compactionOperation, *managed, context.Context, error) { return nil, nil, nil, err }
	if err := request.Err(); err != nil {
		return fail(err)
	}
	if m.baseCtx.Err() != nil {
		return fail(ErrClosed)
	}
	ms, ok := m.lookup(id)
	if !ok {
		if _, err := m.Get(request, id); err != nil {
			return fail(err)
		}
		return fail(ErrClosed)
	}
	if err := m.checkGitPrompt(request, ms); err != nil {
		return fail(err)
	}
	if err := m.checkForkAdmission(request, ms); err != nil {
		return fail(err)
	}
	// Runtime methods may synchronize with notifications delivered through Emit.
	// Never call them while holding emitMu.
	ms.mu.Lock()
	sess := ms.sess
	ms.mu.Unlock()
	c, capable := sess.(agent.Compactor)
	canCompact := capable && c.CanCompact()
	ms.emitMu.Lock()
	defer ms.emitMu.Unlock()
	ms.mu.Lock()
	state, pending, policy := ms.info.State, ms.info.ForkPending, compactionPolicy(ms.info.Compaction)
	ms.mu.Unlock()
	if state.Terminal() || state == StateSuspended {
		return fail(ErrClosed)
	}
	if state != StateIdle {
		return fail(ErrBusy)
	}
	if pending {
		return fail(ErrNotResumable)
	}
	if !canCompact {
		return fail(agent.ErrUnsupported)
	}
	ms.mu.Lock()
	limits := ms.info.Budget.Clone()
	limits.MaxTurnTokens, limits.MaxTurnCostUSD = nil, nil
	hit := ms.accounting.Check(limits, true)
	ms.mu.Unlock()
	if hit != nil {
		return fail(fmt.Errorf("%w: %s %s", ErrBudgetExceeded, hit.Scope, hit.Metric))
	}
	runCtx, cancel := context.WithTimeout(m.baseCtx, time.Duration(policy.TimeoutMs)*time.Millisecond)
	stop := context.AfterFunc(request, cancel)
	op := ms.startCompactionLocked("manual", "", func() { stop(); cancel() })
	ms.mu.Lock()
	ms.info.State = StateCompacting
	ms.info.UpdatedAt = time.Now().UTC()
	ms.mu.Unlock()
	ms.updatedLocked("")
	if err := ms.persist(); err != nil {
		op.cancel()
		ms.mu.Lock()
		ms.compaction = nil
		ms.info.State = StateIdle
		ms.accounting.End()
		ms.info.Usage = ms.accounting.Usage()
		ms.mu.Unlock()
		ms.emitLocked(event.Event{Type: event.ContextCompaction, Data: event.ContextCompactionData{OperationID: op.id, Reason: op.reason, Status: "failed", Before: op.before, Error: &event.ErrorInfo{Code: "persistence_error", Message: err.Error()}}})
		ms.updatedLocked("")
		_ = ms.persist()
		return fail(err)
	}
	m.turns.Add(1)
	return op, ms, runCtx, nil
}
func (ms *managed) startCompactionLocked(reason, turnID string, cancel context.CancelFunc) *compactionOperation {
	ms.mu.Lock()
	before := contextUsage(ms.info.Context)
	op := &compactionOperation{id: NewID("compact_"), reason: reason, turnID: turnID, cancel: cancel, done: make(chan struct{}), before: before}
	ms.compaction = op
	if reason == "manual" {
		ms.accounting.Begin()
	}
	ms.mu.Unlock()
	ms.emitLocked(event.Event{Type: event.ContextCompaction, TurnID: turnID, Data: event.ContextCompactionData{OperationID: op.id, Reason: reason, Status: "started", Before: before}})
	stale := before.Clone()
	stale.Stale = true
	ms.emitLocked(event.Event{Type: event.ContextUsage, TurnID: turnID, Data: stale})
	ms.mu.Lock()
	op.observation = ms.contextObservation
	ms.mu.Unlock()
	return op
}
func (ms *managed) runCompaction(ctx context.Context, op *compactionOperation, manual bool) {
	ms.m.log.DebugContext(ctx, "compaction started", "sessionId", ms.info.ID, "operationId", op.id, "reason", op.reason)
	c, ok := ms.sess.(agent.Compactor)
	var err error
	if ctx.Err() != nil {
		err = ctx.Err()
	} else if !ok || !c.CanCompact() {
		err = agent.ErrUnsupported
	} else {
		err = c.Compact(ctx)
	}
	ms.finishCompaction(ctx, op, manual, err)
}
func (ms *managed) finishCompaction(ctx context.Context, op *compactionOperation, manual bool, err error) {
	defer op.cancel()
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}
	ms.emitMu.Lock()
	ms.mu.Lock()
	after := contextUsage(ms.info.Context)
	if ms.contextObservation == op.observation {
		after.UsedTokens = nil
		after.Basis = "unknown"
		after.Stale = true
	}
	ms.compaction = nil
	if manual {
		ms.accounting.End()
		ms.info.Usage = ms.accounting.Usage()
	} else if err == nil {
		// Native maintenance and the following prompt report independent turn
		// snapshots. Keep their combined usage under the outer kit turn's budget.
		ms.accounting.NextSegment()
	}
	if op.budgetStop {
		err = errors.Join(err, ErrBudgetExceeded)
	}
	dead := errors.Is(err, agent.ErrSessionDead)
	if dead {
		ms.info.State = StateFailed
		ms.info.Error = &event.ErrorInfo{Code: "runtime_exited", Message: err.Error()}
		after.Stale = true
	} else if manual && ms.info.State == StateCompacting {
		ms.info.State = StateIdle
	}
	ms.info.UpdatedAt = time.Now().UTC()
	ms.mu.Unlock()
	ms.emitLocked(event.Event{Type: event.ContextUsage, TurnID: op.turnID, Data: after})
	status := "completed"
	var detail *event.ErrorInfo
	if err != nil {
		status = "failed"
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			status = "cancelled"
		}
		detail = &event.ErrorInfo{Code: "compaction_failed", Message: err.Error()}
	}
	ms.emitLocked(event.Event{Type: event.ContextCompaction, TurnID: op.turnID, Data: event.ContextCompactionData{OperationID: op.id, Reason: op.reason, Status: status, Before: op.before, After: &after, Error: detail}})
	ms.updatedLocked(op.turnID)
	if dead && manual {
		ms.emitLocked(event.Event{Type: event.SessionClosed, Data: event.SessionClosedData{Reason: "failed", Error: detail}})
	}
	persistErr := ms.persist()
	op.err = errors.Join(err, persistErr)
	ms.emitMu.Unlock()
	ms.m.log.Info("compaction finished", "sessionId", ms.info.ID, "operationId", op.id, "reason", op.reason, "status", status)
	close(op.done)
	ms.m.wakeResources()
	if dead && manual {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = ms.closeRuntime(ctx)
			ms.m.retire(ms)
		}()
	}
}
func (ms *managed) compactBeforeTurn(ctx context.Context, t *Turn) error {
	ms.emitMu.Lock()
	ms.mu.Lock()
	usage := contextUsage(ms.info.Context)
	ms.mu.Unlock()
	if !shouldCompact(usage, t.compactionPolicy) {
		ms.emitMu.Unlock()
		return nil
	}
	opCtx, cancel := context.WithTimeout(ctx, time.Duration(t.compactionPolicy.TimeoutMs)*time.Millisecond)
	op := ms.startCompactionLocked("threshold", t.ID, cancel)
	persistErr := ms.persist()
	ms.emitMu.Unlock()
	if persistErr != nil {
		ms.finishCompaction(opCtx, op, false, persistErr)
	} else {
		ms.runCompaction(opCtx, op, false)
	}
	return op.err
}

// applyContextEventLocked normalizes snapshots before logging and updates the
// durable session view. It runs under emitMu, before assigning the event cursor.
func (ms *managed) applyContextEventLocked(ev *event.Event) {
	if value, ok := ev.Data.(event.ContextUsageData); ok && ev.Type == event.ContextUsage {
		value = normalizeContext(value)
		ev.Data = value.Clone()
		ms.mu.Lock()
		ms.info.Context = &value
		ms.contextObservation++
		ms.mu.Unlock()
	}
}

func replayContextEvent(info *Info, ev event.Event) {
	if v, ok := ev.Data.(event.ContextUsageData); ok && ev.Type == event.ContextUsage {
		v = normalizeContext(v)
		info.Context = &v
	}
	if v, ok := ev.Data.(event.SessionUpdatedData); ok {
		if v.Context != nil {
			c := normalizeContext(*v.Context)
			info.Context = &c
		}
		if v.Compaction != nil {
			info.Compaction = compactionPolicy(*v.Compaction)
		}
	}
}

// Observe native auto-compaction even if a runtime only reports completion.
// A synthetic start fills that notification gap without inventing token counts.
func (ms *managed) observeNativeCompactionLocked(ev event.Event) {
	status, ok := ev.Data.(event.RuntimeStatusData)
	if !ok || ev.Type != event.RuntimeStatus {
		return
	}
	terminal := ""
	switch status.Status {
	case "compacting":
	case "compacted":
		terminal = "completed"
	case "compaction_failed":
		terminal = "failed"
	case "compaction_cancelled":
		terminal = "cancelled"
	default:
		return
	}
	ms.mu.Lock()
	if ms.compaction != nil {
		ms.mu.Unlock()
		return
	}
	native := ms.nativeCompaction
	if native == nil {
		native = &compactionOperation{id: NewID("compact_"), reason: "native", turnID: ev.TurnID, before: contextUsage(ms.info.Context)}
		ms.nativeCompaction = native
	}
	fresh := native.observation == 0
	ms.mu.Unlock()
	if fresh {
		ms.mu.Lock()
		idle := ms.info.State == StateIdle
		if idle {
			ms.info.State = StateCompacting
		}
		ms.mu.Unlock()
		ms.emitLocked(event.Event{Type: event.ContextCompaction, TurnID: ev.TurnID, Data: event.ContextCompactionData{OperationID: native.id, Reason: "native", Status: "started", Before: native.before}})
		stale := native.before.Clone()
		stale.Stale = true
		ms.emitLocked(event.Event{Type: event.ContextUsage, TurnID: ev.TurnID, Data: stale})
		ms.mu.Lock()
		native.observation = ms.contextObservation
		ms.mu.Unlock()
		if idle {
			ms.updatedLocked(ev.TurnID)
		}
	}
	if terminal != "" {
		ms.mu.Lock()
		after := contextUsage(ms.info.Context)
		if ms.contextObservation == native.observation {
			after.UsedTokens = nil
			after.Basis = "unknown"
			after.Stale = true
		}
		ms.nativeCompaction = nil
		idle := ms.info.State == StateCompacting
		if idle {
			ms.info.State = StateIdle
			ms.info.UpdatedAt = time.Now().UTC()
		}
		ms.mu.Unlock()
		ms.emitLocked(event.Event{Type: event.ContextUsage, TurnID: ev.TurnID, Data: after})
		var detail *event.ErrorInfo
		if terminal != "completed" {
			detail = &event.ErrorInfo{Code: status.Status, Message: status.Message}
		}
		ms.emitLocked(event.Event{Type: event.ContextCompaction, TurnID: ev.TurnID, Data: event.ContextCompactionData{OperationID: native.id, Reason: "native", Status: terminal, Before: native.before, After: &after, Error: detail}})
		if idle {
			ms.updatedLocked(ev.TurnID)
		}
	}
}
