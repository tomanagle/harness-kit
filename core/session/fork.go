package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"time"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/settings"
)

var ErrForkSourceChanged = errors.New("session: pending fork source has advanced; create a new fork")

// ForkParams branch the complete conversation. Arbitrary turn cutoffs are not
// portable across runtimes. Settings use the same envelope as session creation.
type ForkParams struct {
	Title    string         `json:"title,omitempty"`
	Settings settings.Patch `json:"settings,omitempty"`
}

// Fork copies the observable transcript and branches native conversation state.
// The parent remains unchanged. With a deferred runtime (Claude), ForkPending
// stays true and RuntimeSessionID empty until the first actual prompt. That
// prompt is rejected if the parent has since advanced; no synthetic turn runs.
func (m *Manager) Fork(ctx context.Context, id string, p ForkParams) (Info, error) {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	if m.baseCtx.Err() != nil {
		return Info{}, ErrClosed
	}
	parent, err := m.Get(ctx, id)
	if err != nil {
		return Info{}, err
	}
	if parent.ActiveTurnID != "" || parent.State == StateStarting || parent.State == StateClosing || parent.State == StateSuspending {
		return Info{}, ErrBusy
	}
	if parent.RuntimeSessionID == "" || parent.Turns == 0 || parent.ForkPending {
		return Info{}, ErrNotResumable
	}
	rt, ok := m.reg.Get(parent.Runtime)
	if !ok {
		return Info{}, ErrRuntimeNotFound
	}
	if _, ok := rt.(agent.Forker); !ok || !rt.Info().Capabilities.Fork {
		return Info{}, fmt.Errorf("%w: %s cannot fork", agent.ErrUnsupported, parent.Runtime)
	}
	if live, ok := m.lookup(id); ok {
		live.emitMu.Lock()
		unsaved := live.eventWriteFailed
		if !unsaved {
			err = live.persist()
		}
		live.emitMu.Unlock()
		if unsaved {
			return Info{}, errors.New("session: cannot fork while events are unsaved")
		}
		if err != nil {
			return Info{}, err
		}
	}
	rec, err := m.store.Get(ctx, id)
	if err != nil {
		return Info{}, err
	}
	if _, live := m.lookup(id); !live {
		// Get may have recovered events newer than the record checkpoint. The
		// child's checkpoint must include that same history before advancing its
		// LastSeq, or restoring the branch would skip the recovered usage.
		recovered, err := m.newManaged(ctx, parent, rec.Config, true)
		if err != nil {
			return Info{}, err
		}
		ledger := recovered.accounting
		rec.Accounting = &ledger
		parent.LastSeq = recovered.log.LastSeq()
		parent.Usage = ledger.Usage()
	}
	cfg := rec.Config
	if parent.Config != nil {
		cfg = cfg.WithSettings(*parent.Config)
	}
	cfg.NativeDefaults = rec.NativeDefaults
	cfg.ResumeID = ""
	values := m.currentSettings()
	values.Session.Runtime, values.Session.SessionConfig = parent.Runtime, cfg
	values.Workspace = workspacePolicy(parent.WorkspaceSettings)
	if parent.Workspace != nil && parent.Workspace.Owned {
		raw, explicit := p.Settings["workspace"]
		var fields map[string]json.RawMessage
		if explicit {
			_ = json.Unmarshal(raw, &fields)
		}
		_, pathSet := fields["path"]
		_, rootSet := fields["root"]
		if !explicit || (!pathSet && !rootSet) {
			return Info{}, fmt.Errorf("%w: forking an owned worktree requires an explicit new workspace path or root", settings.ErrScope)
		}
		values.Workspace.Path, values.Workspace.Root = "", ""
	}
	values.Memory.Pinned = false
	values.Budget = parent.Budget.Clone()
	for group, raw := range p.Settings {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
			return Info{}, fmt.Errorf("%w: %s must be an object", settings.ErrInvalid, group)
		}
		for field := range fields {
			if group == "workspace" {
				continue
			}
			if group == "budget" {
				continue
			}
			if group == "memory" && field == "pinned" {
				continue
			}
			if group == "session" && field != "runtime" && field != "resumeId" {
				continue
			}
			return Info{}, fmt.Errorf("%w: %s.%s cannot be changed during fork", settings.ErrScope, group, field)
		}
	}
	next := values
	if len(p.Settings) > 0 {
		next, _, err = settings.Apply(values, p.Settings, nil, values)
		if err != nil {
			return Info{}, err
		}
	}
	cfg = next.Session.SessionConfig
	if parent.Workspace != nil && parent.Workspace.Owned && next.Workspace.Mode != "worktree" {
		return Info{}, fmt.Errorf("%w: an owned-worktree fork needs worktree mode", settings.ErrScope)
	}
	if err := m.validateWorkspacePolicy(next.Workspace); err != nil {
		return Info{}, err
	}
	cfg.NativeDefaults, cfg.ForkFrom = rec.NativeDefaults, parent.RuntimeSessionID
	name := p.Title
	if name == "" {
		name = parent.Title
	}
	name, err = cleanTitle(name, true)
	if err != nil {
		return Info{}, err
	}
	now := time.Now().UTC()
	child := Info{Budget: next.Budget.Clone(), Usage: parent.Usage.Clone(), ID: NewID("ses_"), Runtime: parent.Runtime, State: StateSuspended,
		WorkspaceSettings: next.Workspace,
		Cwd:               cfg.Cwd, Model: cfg.Model, Title: name, Metadata: maps.Clone(parent.Metadata), Pinned: next.Memory.Pinned,
		CreatedAt: now, UpdatedAt: now, Turns: parent.Turns, LastStatus: parent.LastStatus,
		LastSeq: parent.LastSeq, ForkedFrom: id, ForkedAtSeq: parent.LastSeq,
		InheritedTurns: parent.Turns, ForkPending: true}
	initial, err := cfg.Clone()
	if err != nil {
		return Info{}, err
	}
	initialBudget := next.Budget.Clone()
	initialWorkspace := next.Workspace
	childRecord := Record{InitialWorkspace: &initialWorkspace, Accounting: rec.Accounting, InitialBudget: &initialBudget, Info: child, Config: cfg, InitialConfig: &initial, NativeDefaults: rec.NativeDefaults, ForkSourceRuntimeID: cfg.ForkFrom}
	_, atomicCopy := m.store.(EventCopier)
	if !atomicCopy {
		if err = m.store.Put(ctx, childRecord); err != nil {
			return Info{}, err
		}
	}
	cleanup := func() {
		if retained, err := m.store.Get(context.Background(), child.ID); err == nil && retained.Info.Workspace != nil && retained.Info.Workspace.Owned {
			return
		}
		m.mu.Lock()
		delete(m.sessions, child.ID)
		m.mu.Unlock()
		_ = m.store.Delete(context.Background(), child.ID)
	}
	if dst, ok := m.store.(EventStore); ok {
		copyEvent := func(ev event.Event) error {
			if ev.Seq > parent.LastSeq {
				return nil
			}
			ev.SessionID = child.ID
			return dst.AppendEvent(ctx, ev)
		}
		if copier, ok := m.store.(EventCopier); ok {
			err = copier.CopyEvents(ctx, id, child.ID, parent.LastSeq)
		} else {
			var cursor uint64
			for err == nil {
				var page []event.Event
				page, _, err = dst.Events(ctx, id, cursor, 128)
				if err != nil || len(page) == 0 {
					break
				}
				for _, ev := range page {
					if err = copyEvent(ev); err != nil {
						break
					}
				}
				next := page[len(page)-1].Seq
				if next <= cursor {
					err = errors.New("session: fork event scan made no progress")
					break
				}
				cursor = next
			}
		}
	} else if source, ok := m.lookup(id); ok {
		// Non-durable hosts inherit the same bounded replay window as their parent.
		seed, e := m.newManaged(ctx, child, cfg, false)
		if e != nil {
			err = e
		} else {
			source.emitMu.Lock()
			history, _ := source.log.Since(0, 0)
			for _, ev := range history {
				ev.SessionID = child.ID
				seed.log.Restore(ev)
			}
			source.mu.Lock()
			maps.Copy(seed.turnIDs, source.turnIDs)
			source.mu.Unlock()
			source.emitMu.Unlock()
			m.mu.Lock()
			m.sessions[child.ID] = seed
			m.mu.Unlock()
		}
	}
	if err != nil {
		cleanup()
		return Info{}, err
	}
	if atomicCopy {
		if err = m.store.Put(ctx, childRecord); err != nil {
			cleanup()
			return Info{}, err
		}
	}
	result, err := m.start(ctx, child, cfg)
	if err != nil {
		cleanup()
	}
	return result, err
}

// Called with lifecycleMu held. While a deferred fork is materializing, its
// source cannot accept another turn; otherwise the native branch could include
// a prompt absent from the child's copied transcript.
func (m *Manager) checkForkAdmission(ctx context.Context, ms *managed) error {
	info := ms.snapshot()
	if info.ForkPending {
		parent, err := m.Get(ctx, info.ForkedFrom)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrForkSourceChanged, err)
		}
		if parent.Turns != info.InheritedTurns {
			return ErrForkSourceChanged
		}
		if parent.ActiveTurnID != "" {
			return ErrBusy
		}
	}
	m.mu.Lock()
	all := make([]*managed, 0, len(m.sessions))
	for _, child := range m.sessions {
		all = append(all, child)
	}
	m.mu.Unlock()
	for _, child := range all {
		other := child.snapshot()
		if other.ForkedFrom == info.ID && other.ForkPending && other.ActiveTurnID != "" {
			return ErrBusy
		}
	}
	return nil
}
