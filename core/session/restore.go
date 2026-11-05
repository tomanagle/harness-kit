package session

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/internal/budget"
)

// Restore brings a stored session back to life under the same Harness Kit
// ID, resuming the runtime-native session with its saved configuration.
// Persisted events remain readable and sequence numbers never restart.
func (m *Manager) Restore(ctx context.Context, id string) (Info, error) {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	return m.restoreLocked(ctx, id)
}

func (m *Manager) restoreLocked(ctx context.Context, id string) (Info, error) {
	if m.baseCtx.Err() != nil {
		return Info{}, ErrClosed
	}
	if ms, ok := m.lookup(id); ok {
		ms.emitMu.Lock()
		unsaved := ms.eventWriteFailed
		ms.emitMu.Unlock()
		if unsaved {
			return Info{}, fmt.Errorf("session: cannot restore while events are unsaved")
		}
		ms.mu.Lock()
		live := !ms.info.State.Terminal() && ms.info.State != StateClosing && ms.info.State != StateSuspended && ms.info.State != StateSuspending
		ms.mu.Unlock()
		if live {
			return ms.snapshot(), nil
		}
		// Failed sessions may still be releasing their runtime asynchronously.
		if err := ms.closeRuntime(ctx); err != nil {
			return Info{}, err
		}
	}
	rec, err := m.store.Get(ctx, id)
	if err != nil {
		return Info{}, err
	}
	rec.Info, err = m.storedInfo(ctx, rec.Info)
	if err != nil {
		return Info{}, err
	}
	if (rec.Info.RuntimeSessionID == "" || rec.Info.Turns == 0) && rec.Info.State != StateSuspended && !rec.Info.ForkPending {
		return Info{}, fmt.Errorf("%w: %s", ErrNotResumable, id)
	}
	cfg := rec.Config
	if rec.Info.Config != nil {
		cfg = cfg.WithSettings(*rec.Info.Config)
	}
	cfg.NativeDefaults = rec.NativeDefaults
	if rec.Info.ForkPending {
		cfg.ForkFrom = rec.ForkSourceRuntimeID
	}
	cfg.ResumeID = rec.Info.RuntimeSessionID
	if rec.Info.Turns == 0 {
		cfg.ResumeID = ""
	}
	info := rec.Info
	info.ActiveTurnID, info.Error = "", nil
	var warnings []string
	if m.opts.PrepareRestore != nil {
		params := CreateParams{Runtime: info.Runtime, Config: cfg, Metadata: maps.Clone(info.Metadata), Title: info.Title}
		warnings, err = m.opts.PrepareRestore(ctx, &params)
		if err != nil {
			return Info{}, err
		}
		if params.Runtime != info.Runtime || params.Config.ResumeID != cfg.ResumeID {
			return Info{}, fmt.Errorf("%w: restore hook cannot change runtime or resume ID", agent.ErrInvalidConfig)
		}
		cfg = params.Config
		info.Metadata = maps.Clone(params.Metadata)
		info.Title, err = cleanTitle(params.Title, true)
		if err != nil {
			return Info{}, err
		}
	}
	return m.start(ctx, info, cfg, warnings...)
}

// dormantInfo distinguishes stored history from runtime instances in this manager.
func dormantInfo(info Info) Info {
	info.Compaction = compactionPolicy(info.Compaction)
	if info.Context != nil {
		v := info.Context.Clone()
		v.Stale = true
		info.Context = &v
	}
	if info.SettingsRevision == 0 {
		info.SettingsRevision = 1
	}
	if !info.State.Terminal() && info.State != StateSuspended {
		info.State = StateClosed
	}
	info.ActiveTurnID = ""
	return info
}

// storedInfo reconciles updates synced just before a process stopped, before
// the corresponding record replacement. Old snapshots never override newer records.
func (m *Manager) storedInfo(ctx context.Context, info Info) (Info, error) {
	rec, err := m.store.Get(ctx, info.ID)
	if err != nil {
		return Info{}, err
	}
	ledger := budget.New(rec.Config.ResumeID != "")
	var ledgerAfter uint64
	if len(rec.Accounting) != 0 && strings.TrimSpace(string(rec.Accounting)) != "null" {
		if err := json.Unmarshal(rec.Accounting, &ledger); err != nil {
			return Info{}, fmt.Errorf("session: decode accounting checkpoint: %w", err)
		}
		ledgerAfter = rec.Info.LastSeq
	}
	after := info.LastSeq
	apply := func(ev event.Event) error {
		if ev.Seq > ledgerAfter {
			replayBudgetEvent(&ledger, ev)
		}
		if ev.Seq <= after {
			return nil
		}
		replayContextEvent(&info, ev)
		if d, ok := ev.Data.(event.SessionUpdatedData); ok {
			info.Title, info.State, info.ActiveTurnID = d.Title, State(d.State), d.ActiveTurnID
			info.RuntimeSessionID, info.Turns, info.LastStatus = d.RuntimeSessionID, d.Turns, d.LastStatus
			info.UpdatedAt, info.Error, info.Pinned = d.UpdatedAt, d.Error, d.Pinned
			if d.Budget != nil {
				info.Budget = d.Budget.Clone()
			}
			if d.Usage != nil {
				info.Usage = d.Usage.Clone()
			}
			info.Archived, info.ForkPending, info.Metadata = d.Archived, d.ForkPending, maps.Clone(d.Metadata)
			if d.SettingsRevision > 0 {
				info.SettingsRevision = max(info.SettingsRevision, d.SettingsRevision)
			}
			if d.Config != nil {
				cfg := d.Config.Clone()
				info.Config = &cfg
				info.Model = cfg.Model
			}
		}
		info.LastSeq = max(info.LastSeq, ev.Seq)
		return nil
	}
	if scanner, ok := m.store.(EventScanner); ok {
		if err := scanner.ScanEvents(ctx, info.ID, apply); err != nil {
			return Info{}, err
		}
	} else if store, ok := m.store.(EventStore); ok {
		cursor := min(after, ledgerAfter)
		for {
			evs, _, err := store.Events(ctx, info.ID, cursor, 128)
			if err != nil {
				return Info{}, err
			}
			if len(evs) == 0 {
				break
			}
			for _, ev := range evs {
				_ = apply(ev)
			}
			next := evs[len(evs)-1].Seq
			if next <= cursor {
				return Info{}, fmt.Errorf("session: event store made no progress")
			}
			cursor = next
		}
	}
	ledger.Interrupt()
	info.Usage = ledger.Usage()
	return dormantInfo(info), nil
}
