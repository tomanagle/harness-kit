package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sync"
	"time"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/config"
	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/internal/budget"
	"github.com/tomanagle/harness-kit/core/workspace"
)

// managed is one session held by the Manager. It implements agent.Handler.
type managed struct {
	m                 *Manager
	rt                agent.Runtime
	cfg               agent.SessionConfig
	initialCfg        agent.SessionConfig
	initialBudget     config.BudgetLimits
	initialCompaction config.CompactionSettings
	initialWorkspace  workspace.Settings
	accounting        budget.Ledger
	log               *event.Log

	// emitMu serialises Append+Publish so subscribers observe events in
	// sequence order. Lock order: emitMu before mu.
	emitMu           sync.Mutex
	eventWriteFailed bool          // guarded by emitMu; enables disk/cache merging
	unsaved          []event.Event // failed durable writes; never evicted automatically
	lastAccess       time.Time     // guarded by emitMu; transcript reads refresh cache residency
	// persistMu serialises store writes (lock order: persistMu before mu).
	persistMu sync.Mutex

	mu                 sync.Mutex
	info               Info
	recordWriteFailed  bool // guarded by mu; prevents losing an unsaved record
	sess               agent.Session
	turn               *Turn
	compaction         *compactionOperation
	nativeCompaction   *compactionOperation
	contextObservation uint64
	turnIDs            map[string]struct{}
	closeOnce          sync.Once
	closeErr           error
	detach             func() // revokes the tool bridge binding
}

func (ms *managed) currentTurnID() string {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if ms.turn != nil {
		return ms.turn.ID
	}
	return ""
}

func (ms *managed) cancelTurn() {
	ms.mu.Lock()
	t := ms.turn
	c := ms.compaction
	ms.mu.Unlock()
	if t != nil {
		t.cancel()
	}
	if c != nil {
		c.cancel()
	}
}

func (ms *managed) snapshot() Info {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	info := ms.info
	info.Compaction = compactionPolicy(info.Compaction)
	if info.Context != nil {
		v := info.Context.Clone()
		info.Context = &v
	}
	if info.Workspace != nil {
		v := *info.Workspace
		info.Workspace = &v
	}
	info.Budget = info.Budget.Clone()
	info.Usage = info.Usage.Clone()
	info.Metadata = maps.Clone(info.Metadata)
	if info.Config != nil {
		selection := info.Config.Clone()
		info.Config = &selection
	}
	info.LastSeq = ms.log.LastSeq()
	return info
}

// rename sets the title and publishes a persisted snapshot.
func (ms *managed) rename(name string) (Info, error) {
	ms.emitMu.Lock()
	defer ms.emitMu.Unlock()
	ms.mu.Lock()
	old := ms.info.Title
	ms.info.Title = name
	ms.mu.Unlock()
	if err := ms.persist(); err != nil {
		ms.mu.Lock()
		ms.info.Title = old
		ms.mu.Unlock()
		return Info{}, err
	}
	ms.updatedLocked("")
	err := ms.persist()
	return ms.snapshot(), err
}

// persist serialises writes and snapshots so older state cannot overwrite newer.
func (ms *managed) persist() error {
	ms.persistMu.Lock()
	defer ms.persistMu.Unlock()
	ms.mu.Lock()
	cfg, cloneErr := ms.cfg.Clone()
	initial, initialErr := ms.initialCfg.Clone()
	sess := ms.sess
	ledger := ms.accounting
	initialBudget := ms.initialBudget.Clone()
	ms.mu.Unlock()
	if err := errors.Join(cloneErr, initialErr); err != nil {
		return err
	}
	defaults := cfg.NativeDefaults
	if provider, ok := sess.(agent.DefaultsProvider); ok {
		selection := provider.NativeDefaults().Clone()
		defaults = &selection
	}
	initialWorkspace := ms.initialWorkspace
	initialCompaction := ms.initialCompaction.Clone()
	checkpoint, err := json.Marshal(ledger)
	if err != nil {
		return fmt.Errorf("session: encode accounting checkpoint: %w", err)
	}
	rec := Record{Accounting: checkpoint, InitialBudget: &initialBudget, InitialWorkspace: &initialWorkspace, InitialCompaction: &initialCompaction, Info: ms.snapshot(), Config: cfg, InitialConfig: &initial, NativeDefaults: defaults, ForkSourceRuntimeID: cfg.ForkFrom}
	err = ms.m.store.Put(context.Background(), rec)
	ms.mu.Lock()
	ms.recordWriteFailed = err != nil
	ms.mu.Unlock()
	if err != nil {
		ms.m.log.Warn("persist session", "sessionId", rec.Info.ID, "err", err)
	}
	return err
}

func (ms *managed) closeRuntime(ctx context.Context) error {
	ms.closeOnce.Do(func() {
		if ms.sess != nil {
			ms.closeErr = ms.sess.Close(ctx)
		}
		if ms.detach != nil {
			ms.detach()
		}
	})
	return ms.closeErr
}

func (ms *managed) close(ctx context.Context, reason string) error {
	ms.emitMu.Lock()
	ms.mu.Lock()
	if ms.info.State.Terminal() || ms.info.State == StateClosing {
		ms.mu.Unlock()
		ms.emitMu.Unlock()
		return nil
	}
	ms.info.State = StateClosing
	t := ms.turn
	c := ms.compaction
	ms.mu.Unlock()
	ms.updatedLocked("")
	ms.emitMu.Unlock()

	if t != nil {
		t.cancel()
		select {
		case <-t.done:
		case <-ctx.Done():
		}
	}
	if c != nil {
		c.cancel()
		select {
		case <-c.done:
		case <-ctx.Done():
		}
	}
	err := ms.closeRuntime(ctx)

	ms.emitMu.Lock()
	ms.mu.Lock()
	nativePending := ms.nativeCompaction != nil
	ms.mu.Unlock()
	if nativePending {
		ms.observeNativeCompactionLocked(event.Event{Type: event.RuntimeStatus, Data: event.RuntimeStatusData{Status: "compaction_cancelled", Message: "runtime closed during native compaction"}})
	}
	ms.mu.Lock()
	ms.info.State = StateClosed
	ms.info.UpdatedAt = time.Now().UTC()
	ms.markContextStaleLocked()
	ms.mu.Unlock()
	ms.updatedLocked("")
	ms.emitLocked(event.Event{Type: event.SessionClosed, Data: event.SessionClosedData{Reason: reason}})
	ms.persist()
	ms.emitMu.Unlock()
	ms.m.retire(ms)
	ms.m.log.Info("session closed", "sessionId", ms.info.ID, "reason", reason)
	return err
}
