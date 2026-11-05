package session

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"time"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/config"
	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/hooks"
	"github.com/tomanagle/harness-kit/core/settings"
	"github.com/tomanagle/harness-kit/core/workspace"
)

// PromptParams describe one turn.
type PromptParams struct {
	// Settings applies the unified session selection for this prompt only.
	// It cannot be supplied together with the legacy Config field.
	Settings settings.Patch
	// TurnID lets the caller choose the turn ID (it must be unique within
	// the session). Empty generates one.
	TurnID string
	Input  []agent.InputPart
	// Config changes only this turn. Omitted values inherit session defaults.
	Config *agent.ConfigPatch
}

// Turn is a handle on a running turn.
type Turn struct {
	compactionPolicy config.CompactionSettings
	workspace        workspace.Settings
	ID               string
	SessionID        string

	cancel     context.CancelFunc
	done       chan struct{}
	result     agent.TurnResult
	budget     turnBudget
	budgetStop *event.BudgetExceededData
}

// Done is closed when the turn has finished and turn.completed was emitted.
func (t *Turn) Done() <-chan struct{} { return t.done }

// Result returns the turn's result. Only valid after Done is closed.
func (t *Turn) Result() agent.TurnResult { return t.result }

// Wait blocks until the turn finishes or ctx ends.
func (t *Turn) Wait(ctx context.Context) (agent.TurnResult, error) {
	select {
	case <-t.done:
		return t.result, nil
	case <-ctx.Done():
		return agent.TurnResult{}, ctx.Err()
	}
}

// Prompt starts a turn and returns immediately. Its events, starting with
// turn.started and ending with turn.completed, are emitted to subscribers.
// The turn is not bound to ctx; use Cancel to stop it.
func (m *Manager) prompt(requestCtx context.Context, sessionID string, p PromptParams) (*Turn, error) {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	ms, ok := m.lookup(sessionID)
	if m.memoryPolicy().ResumeOnPrompt {
		info, err := m.Get(requestCtx, sessionID)
		if err != nil {
			return nil, err
		}
		if info.State == StateSuspended {
			if _, err := m.restoreLocked(requestCtx, sessionID); err != nil {
				return nil, err
			}
			ms, ok = m.lookup(sessionID)
		}
	}
	if !ok {
		return nil, ErrNotFound
	}
	if err := m.checkForkAdmission(requestCtx, ms); err != nil {
		return nil, err
	}
	if err := m.checkGitPrompt(requestCtx, ms); err != nil {
		return nil, err
	}
	ms.emitMu.Lock()
	defer ms.emitMu.Unlock()
	ms.mu.Lock()
	switch {
	case ms.info.State.Terminal() || ms.info.State == StateClosing || ms.info.State == StateSuspended:
		ms.mu.Unlock()
		return nil, ErrClosed
	case ms.info.State != StateIdle:
		ms.mu.Unlock()
		return nil, ErrBusy
	}
	ms.mu.Unlock()
	budgetSelection, budgetPolicy, err := ms.prepareBudget(p.Settings)
	if err != nil {
		return nil, err
	}
	workspaceSettings, err := ms.turnWorkspace(p.Settings)
	if err != nil {
		return nil, err
	}
	compactionSettings, err := ms.turnCompaction(p.Settings)
	if err != nil {
		return nil, err
	}
	settings, err := ms.prepareTurn(requestCtx, p.Config)
	if err != nil {
		return nil, err
	}
	input := slices.Clone(p.Input)
	if p.TurnID != "" {
		if lookup, ok := m.store.(TurnLookup); ok {
			used, err := lookup.HasTurn(requestCtx, sessionID, p.TurnID)
			if err != nil {
				return nil, err
			}
			if used {
				return nil, fmt.Errorf("%w: %q", ErrDuplicateTurn, p.TurnID)
			}
		}
	}
	ms.mu.Lock()
	turnID := p.TurnID
	if turnID == "" {
		turnID = NewID("turn_")
	}
	if _, dup := ms.turnIDs[turnID]; dup {
		ms.mu.Unlock()
		return nil, fmt.Errorf("%w: %q", ErrDuplicateTurn, turnID)
	}
	// Keep the current ID until its completion is durable. Historical IDs
	// are checked on disk rather than retained for the lifetime of the process.
	ms.turnIDs[turnID] = struct{}{}
	ctx, cancel := context.WithCancel(m.baseCtx)
	t := &Turn{ID: turnID, SessionID: sessionID, cancel: cancel, done: make(chan struct{}), budget: budgetSelection, workspace: workspaceSettings, compactionPolicy: compactionSettings}
	ms.turn = t
	ms.accounting.Begin()
	ms.info.State = StateRunning
	ms.info.ActiveTurnID = turnID
	ms.info.Turns++
	if ms.info.Title == "" {
		ms.info.Title = title(agent.JoinText(input))
	}
	ms.info.UpdatedAt = time.Now().UTC()
	ms.mu.Unlock()

	snapshot := settings.Clone()
	ms.emitLocked(event.Event{Type: event.TurnStarted, TurnID: turnID, Data: event.TurnStartedData{Workspace: &workspaceSettings, Budget: &budgetPolicy, Text: agent.JoinText(input), Input: slices.Clone(input), Config: &snapshot}})
	ms.updatedLocked(turnID)
	ms.persist()
	m.hookMu.Lock()
	delete(m.hookAdmissions, sessionID)
	m.hookMu.Unlock()
	m.turns.Add(1)
	go ms.runTurn(ctx, t, agent.Turn{ID: t.ID, Input: input, Config: &settings})
	return t, nil
}

// Stream starts a turn and yields its events in order, ending after
// turn.completed. Events dropped by a slow consumer are recovered from the
// session log transparently. Cancelling ctx cancels the turn (and the
// iterator keeps yielding until turn.completed); breaking out of the loop
// early stops observation but leaves the turn running.
func (m *Manager) Stream(ctx context.Context, sessionID string, p PromptParams) iter.Seq2[event.Event, error] {
	return func(yield func(event.Event, error) bool) {
		if p.TurnID == "" {
			p.TurnID = NewID("turn_")
		}
		sub := m.Subscribe(event.ForSession(sessionID), 0)
		defer sub.Close()
		t, err := m.Prompt(ctx, sessionID, p)
		if err != nil {
			yield(event.Event{}, err)
			return
		}
		stop := context.AfterFunc(ctx, func() { t.cancel() })
		defer stop()
		var lastSeq uint64
		deliver := func(ev event.Event) (cont, finished bool) {
			if ev.Seq <= lastSeq || ev.TurnID != t.ID {
				return true, false
			}
			lastSeq = ev.Seq
			if !yield(ev, nil) {
				return false, false
			}
			return true, ev.Type == event.TurnCompleted
		}
		for ev := range sub.C {
			if ev.Type == event.StreamGap {
				cursor := lastSeq
				for {
					page, err := m.EventsPage(context.Background(), sessionID, cursor, 0)
					if err != nil {
						yield(event.Event{}, err)
						return
					}
					for _, mev := range page.Events {
						if cont, fin := deliver(mev); !cont || fin {
							return
						}
					}
					if !page.HasMore || page.NextSeq <= cursor {
						break
					}
					cursor = page.NextSeq
				}
				continue
			}
			if cont, fin := deliver(ev); !cont || fin {
				return
			}
		}
	}
}

func (ms *managed) runTurn(ctx context.Context, t *Turn, input agent.Turn) {
	defer ms.m.turns.Done()
	defer t.cancel()
	ms.m.log.DebugContext(ctx, "turn started", "sessionId", ms.info.ID, "turnId", t.ID)

	var res agent.TurnResult
	err := ms.compactBeforeTurn(ctx, t)
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		workspaceChange := ms.captureWorkspace(ctx, t)
		res, err = ms.sess.Prompt(ctx, input)
		ms.completeWorkspace(t, workspaceChange)
	}
	dead := errors.Is(err, agent.ErrSessionDead)
	switch {
	case err != nil && ctx.Err() != nil && !dead:
		res.Status = event.TurnStatusCancelled
	case err != nil:
		code := "runtime_error"
		if dead {
			code = "runtime_exited"
		}
		res.Status = event.TurnStatusFailed
		res.Error = &event.ErrorInfo{Code: code, Message: err.Error()}
	case res.Status == "":
		res.Status = event.TurnStatusCompleted
	}
	ms.emitMu.Lock()
	ms.mu.Lock()
	nativePending := ms.nativeCompaction != nil
	ms.mu.Unlock()
	if nativePending {
		status := "compaction_failed"
		if res.Status == event.TurnStatusCancelled {
			status = "compaction_cancelled"
		}
		ms.observeNativeCompactionLocked(event.Event{Type: event.RuntimeStatus, TurnID: t.ID, Data: event.RuntimeStatusData{Status: status, Message: "native turn ended without confirming compaction completion"}})
	}
	if res.Usage != nil {
		ms.observeUsageLocked(*res.Usage, true)
	}
	ms.mu.Lock()
	res.Budget = t.budgetStop
	t.result = res
	ms.accounting.End()
	ms.info.Usage = ms.accounting.Usage()
	ms.turn = nil
	ms.info.ActiveTurnID = ""
	ms.info.UpdatedAt = time.Now().UTC()
	if rid := ms.sess.RuntimeSessionID(); rid != "" {
		ms.info.RuntimeSessionID = rid
		if ms.cfg.ForkFrom != "" && rid != ms.cfg.ForkFrom {
			ms.cfg.ForkFrom = ""
			ms.info.ForkPending = false
		}
	}
	switch {
	case dead:
		ms.info.State = StateFailed
		ms.info.Error = res.Error
		ms.markContextStaleLocked()
	case ms.info.State == StateRunning:
		ms.info.State = StateIdle
	}
	ms.info.LastStatus = res.Status
	afterHook := ms.hookRequestLocked(t.ID, input.Input, nil)
	afterHook.Name = hooks.AfterTurn
	afterHook.InvocationID = ms.info.ID + ":" + t.ID + ":turn.after"
	afterHook.Result = &res
	afterHook.IsFirstTurn = ms.info.Turns == ms.info.InheritedTurns+1
	ms.mu.Unlock()
	ms.updatedLocked(t.ID)
	ms.emitLocked(event.Event{Type: event.TurnCompleted, TurnID: t.ID, Data: event.TurnCompletedData{
		Status:        res.Status,
		Budget:        res.Budget,
		Error:         res.Error,
		Usage:         res.Usage,
		RuntimeTurnID: res.RuntimeTurnID,
	}})
	if dead {
		ms.emitLocked(event.Event{Type: event.SessionClosed, Data: event.SessionClosedData{Reason: "failed", Error: res.Error}})
	}
	ms.persist()
	if _, durableLookup := ms.m.store.(TurnLookup); durableLookup && !ms.eventWriteFailed {
		ms.mu.Lock()
		delete(ms.turnIDs, t.ID)
		ms.mu.Unlock()
	}
	ms.emitMu.Unlock()
	ms.m.wakeResources()
	ms.m.log.Info("turn completed", "sessionId", ms.info.ID, "turnId", t.ID, "status", string(res.Status))
	close(t.done)
	_, _, hookErr := ms.m.runHooks(ms.m.baseCtx, afterHook)
	if hookErr != nil {
		ms.m.log.Warn("after-turn hook failed", "sessionId", ms.info.ID, "turnId", t.ID, "error", hookErr)
	}

	if dead {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = ms.closeRuntime(ctx)
			ms.m.retire(ms)
		}()
	}
}
