package session

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/event"
)

// ResourceStats reports retained encoded event payloads, not process RSS.
// Busy/pinned sessions and failed durable writes may defer resource reclamation.
type ResourceStats struct {
	CachedBytes      int64    `json:"cachedBytes"`
	CachedEvents     int      `json:"cachedEvents"`
	CachedSessions   int      `json:"cachedSessions"`
	ResidentSessions int      `json:"residentSessions"`
	UnsavedEvents    int      `json:"unsavedEvents"`
	Deferred         []string `json:"deferred,omitempty"`
}

func (m *Manager) managedSessions() []*managed {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*managed, 0, len(m.sessions))
	for _, ms := range m.sessions {
		out = append(out, ms)
	}
	return out
}

// MemoryStats describes current cache occupancy and blocked reclamation.
func (m *Manager) MemoryStats() ResourceStats {
	var stats ResourceStats
	for _, ms := range m.managedSessions() {
		ms.emitMu.Lock()
		n, b := ms.log.Stats()
		stats.CachedEvents += n
		stats.CachedBytes += b
		if n > 0 {
			stats.CachedSessions++
		}
		stats.UnsavedEvents += len(ms.unsaved)
		ms.mu.Lock()
		state, pinned, recordFailed := ms.info.State, ms.info.Pinned, ms.recordWriteFailed
		ms.mu.Unlock()
		if state == StateIdle || state == StateRunning || state == StateCompacting || state == StateStarting || state == StateSuspending {
			stats.ResidentSessions++
		}
		if recordFailed {
			stats.Deferred = append(stats.Deferred, ms.info.ID+": unsaved record")
		}
		if ms.eventWriteFailed {
			stats.Deferred = append(stats.Deferred, ms.info.ID+": unsaved events")
		}
		if pinned {
			stats.Deferred = append(stats.Deferred, ms.info.ID+": pinned")
		}
		ms.emitMu.Unlock()
	}
	return stats
}

func (m *Manager) wakeResources() {
	select {
	case m.resourceWake <- struct{}{}:
	default:
	}
}
func (m *Manager) resourceLoop() {
	defer close(m.resourceDone)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-m.baseCtx.Done():
			return
		case <-ticker.C:
			p := m.memoryPolicy()
			if p.EvictAfterIdleMs == nil && p.SuspendAfterIdleMs == nil {
				continue
			}
		case <-m.resourceWake:
		}
		if m.baseCtx.Err() != nil {
			return
		}
		m.enforceResources(m.baseCtx)
	}
}

// Evict releases an idle session's transcript cache. It preserves its record,
// transcript, sequence numbers and runtime. A persistent event store is required.
func (m *Manager) Evict(ctx context.Context, id string) error {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	ms, ok := m.lookup(id)
	if !ok {
		_, err := m.store.Get(ctx, id)
		return err
	}
	return m.evictLocked(ms)
}

func (m *Manager) evictLocked(ms *managed) error {
	if _, ok := m.store.(EventStore); !ok {
		return fmt.Errorf("%w: cache eviction requires an EventStore", agent.ErrUnsupported)
	}
	ms.emitMu.Lock()
	defer ms.emitMu.Unlock()
	ms.mu.Lock()
	busy := m.hookAdmissionActive(ms.info.ID) || ms.turn != nil || ms.compaction != nil || ms.nativeCompaction != nil || ms.info.State == StateStarting || ms.info.State == StateClosing || ms.info.State == StateSuspending
	pin := ms.info.Pinned
	terminal := ms.info.State.Terminal()
	ms.mu.Unlock()
	if busy || pin {
		return ErrBusy
	}
	if ms.eventWriteFailed {
		return errors.New("session: cannot evict unsaved events")
	}
	if err := ms.persist(); err != nil {
		return err
	}
	ms.log.Clear()
	if terminal {
		m.forgetManaged(ms)
	}
	return nil
}

func (m *Manager) forgetManaged(ms *managed) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions[ms.info.ID] == ms {
		delete(m.sessions, ms.info.ID)
	}
	// Remove every reference, including retired objects replaced by Restore.
	kept := m.closedOrder[:0]
	for _, old := range m.closedOrder {
		if old != ms {
			kept = append(kept, old)
		}
	}
	for i := len(kept); i < len(m.closedOrder); i++ {
		m.closedOrder[i] = nil
	}
	m.closedOrder = kept
}

// Suspend releases an idle runtime and its cache after persisting its resume
// information. It never cancels a turn or an approval. Suspended sessions retain
// their ID and can be restored explicitly or by resumeOnPrompt.
func (m *Manager) Suspend(ctx context.Context, id string) error {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	return m.suspendLocked(ctx, id)
}

func (m *Manager) suspendLocked(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, ok := m.store.(EventStore); !ok {
		return fmt.Errorf("%w: suspension requires an EventStore", agent.ErrUnsupported)
	}
	ms, ok := m.lookup(id)
	if !ok {
		info, err := m.Get(ctx, id)
		if err != nil {
			return err
		}
		if info.State == StateSuspended {
			return nil
		}
		return ErrClosed
	}
	ms.emitMu.Lock()
	ms.mu.Lock()
	if ms.info.State == StateSuspended {
		ms.mu.Unlock()
		ms.emitMu.Unlock()
		return nil
	}
	if m.hookAdmissionActive(ms.info.ID) || ms.info.State != StateIdle || ms.turn != nil || ms.info.Pinned {
		ms.mu.Unlock()
		ms.emitMu.Unlock()
		return ErrBusy
	}
	if ms.info.Turns > 0 && (ms.rt == nil || !ms.rt.Info().Capabilities.Resume || ms.info.RuntimeSessionID == "") {
		ms.mu.Unlock()
		ms.emitMu.Unlock()
		return ErrNotResumable
	}
	if ms.eventWriteFailed {
		ms.mu.Unlock()
		ms.emitMu.Unlock()
		return errors.New("session: cannot suspend unsaved events")
	}
	// Save native defaults before releasing the adapter. Suspending is an
	// admission barrier; the activity timestamp is deliberately unchanged.
	previous := ms.info.State
	ms.info.State = StateSuspending
	ms.mu.Unlock()
	if err := ms.persist(); err != nil {
		ms.mu.Lock()
		ms.info.State = previous
		ms.mu.Unlock()
		ms.emitMu.Unlock()
		return err
	}
	ms.updatedLocked("")
	if ms.eventWriteFailed {
		ms.mu.Lock()
		ms.info.State = previous
		ms.mu.Unlock()
		_ = ms.persist()
		ms.emitMu.Unlock()
		return errors.New("session: cannot suspend unsaved events")
	}
	ms.emitMu.Unlock()
	closeErr := ms.closeRuntime(ctx)
	ms.emitMu.Lock()
	ms.mu.Lock()
	ms.info.State = StateSuspended
	ms.markContextStaleLocked()
	if closeErr != nil {
		ms.info.State = StateFailed
		ms.info.Error = &event.ErrorInfo{Code: "suspend_failed", Message: closeErr.Error()}
	}
	ms.mu.Unlock()
	ms.updatedLocked("")
	persistErr := ms.persist()
	writeFailed := ms.eventWriteFailed
	safe := closeErr == nil && persistErr == nil && !writeFailed
	if safe {
		ms.log.Clear()
	}
	ms.emitMu.Unlock()
	if safe {
		m.forgetManaged(ms)
	}
	m.publishSessionSettingsLocked(ctx, id)
	if writeFailed && persistErr == nil {
		persistErr = errors.New("session: suspension events could not be persisted")
	}
	return errors.Join(closeErr, persistErr)
}

func (m *Manager) enforceResources(ctx context.Context) []string {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	if ctx.Err() != nil {
		return nil
	}
	p := m.memoryPolicy()
	all := m.managedSessions()
	type candidate struct {
		ms       *managed
		idle     time.Time
		eligible bool
	}
	items := make([]candidate, 0, len(all))
	var total int64
	var count int
	var deferred []string
	for _, ms := range all {
		ms.emitMu.Lock()
		ms.mu.Lock()
		idle := maxTime(ms.info.UpdatedAt, ms.lastAccess)
		eligible := !m.hookAdmissionActive(ms.info.ID) && ms.turn == nil && ms.compaction == nil && ms.nativeCompaction == nil && ms.info.State != StateStarting && ms.info.State != StateClosing && ms.info.State != StateSuspending && !ms.info.Pinned && !ms.eventWriteFailed && !ms.recordWriteFailed
		state := ms.info.State
		pinned := ms.info.Pinned
		ms.mu.Unlock()
		// New count/byte limits apply when a session is eligible. Busy, pinned
		// and unsaved sessions defer shrinking their current cache.
		if eligible {
			ms.log.Configure(p.EventLogSize, p.MaxSessionCacheBytes)
		}
		n, b := ms.log.Stats()
		total += b
		if n > 0 {
			count++
		}
		items = append(items, candidate{ms: ms, idle: idle, eligible: eligible})
		if !eligible && n > 0 {
			reason := "active"
			if pinned {
				reason = "pinned"
			}
			if ms.eventWriteFailed {
				reason = "unsaved events"
			}
			deferred = append(deferred, ms.info.ID+": "+reason)
		}
		ms.emitMu.Unlock()
		// Runtime expiry uses turn activity, not transcript reads or polling.
		if p.SuspendAfterIdleMs != nil && eligible && state == StateIdle {
			ms.mu.Lock()
			inactive := time.Since(ms.info.UpdatedAt) >= time.Duration(*p.SuspendAfterIdleMs)*time.Millisecond
			ms.mu.Unlock()
			if inactive {
				if err := m.suspendLocked(ctx, ms.info.ID); err != nil {
					deferred = append(deferred, ms.info.ID+": "+err.Error())
				} else {
					total -= b
					if n > 0 {
						count--
					}
				}
			}
		}
	}
	slices.SortFunc(items, func(a, b candidate) int { return a.idle.Compare(b.idle) })
	for _, item := range items {
		ms := item.ms
		// Suspended sessions may already have been removed.
		if current, ok := m.lookup(ms.info.ID); !ok || current != ms {
			continue
		}
		n, b := ms.log.Stats()
		over := p.MaxCacheBytes != nil && total > *p.MaxCacheBytes || p.MaxCachedSessions != nil && count > *p.MaxCachedSessions
		expired := p.EvictAfterIdleMs != nil && time.Since(item.idle) >= time.Duration(*p.EvictAfterIdleMs)*time.Millisecond
		if n == 0 || !item.eligible || !over && !expired {
			continue
		}
		if err := m.evictLocked(ms); err != nil {
			deferred = append(deferred, ms.info.ID+": "+err.Error())
			continue
		}
		total -= b
		count--
	}
	// The closed-object allowance itself is live.
	m.trimClosed(p.MaxClosedSessions)

	return deferred
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// DurableEventsPage reads only persisted events for replication. Unsupported
// stores fail closed; callers must also reject truncated pages. This never
// restores a runtime or substitutes unsaved events from the UI cache.
func (m *Manager) DurableEventsPage(ctx context.Context, id string, after uint64, limit int) (EventPage, error) {
	if err := ctx.Err(); err != nil {
		return EventPage{}, err
	}
	store, ok := m.store.(DurableEventPageStore)
	if !ok {
		return EventPage{}, ErrDurableHistoryUnavailable
	}
	p := m.historyPolicy()
	if limit <= 0 || limit > p.MaxPageEvents {
		limit = p.MaxPageEvents
	}
	if ms, live := m.lookup(id); live {
		ms.emitMu.Lock()
		defer ms.emitMu.Unlock()
		// A failed Sync may leave readable but uncommitted bytes on disk.
		if ms.eventWriteFailed {
			return EventPage{}, ErrDurableHistoryUnavailable
		}
	}
	return store.DurableEventsPage(ctx, id, after, limit, p.MaxPageBytes)
}

// EventsPage reads a bounded transcript page. Transcript reads do not restore
// runtimes. Count and encoded-byte limits apply to each response independently.
func (m *Manager) EventsPage(ctx context.Context, id string, after uint64, limit int) (EventPage, error) {
	if err := ctx.Err(); err != nil {
		return EventPage{}, err
	}
	p := m.historyPolicy()
	if limit <= 0 || limit > p.MaxPageEvents {
		limit = p.MaxPageEvents
	}
	ms, live := m.lookup(id)
	if live {
		ms.emitMu.Lock()
		defer ms.emitMu.Unlock()
		ms.lastAccess = time.Now()
	}
	if store, ok := m.store.(EventPageStore); ok && (!live || !ms.eventWriteFailed) {
		return store.EventsPage(ctx, id, after, limit, p.MaxPageBytes)
	}
	// Fallback remains bounded by event count for older stores. A store with
	// arbitrarily large individual events should implement EventPageStore to
	// reject oversized lines before decoding them.
	var evs []event.Event
	var truncated bool
	if store, ok := m.store.(EventStore); ok {
		var err error
		evs, truncated, err = store.Events(ctx, id, after, limit+1)
		if err != nil {
			return EventPage{}, err
		}
	} else if live {
		evs, truncated = ms.log.Since(after, limit+1)
	} else {
		rec, err := m.store.Get(ctx, id)
		return EventPage{NextSeq: after, Truncated: err == nil && after < rec.Info.LastSeq}, err
	}
	if live && ms.eventWriteFailed {
		bySeq := map[uint64]event.Event{}
		for _, ev := range evs {
			bySeq[ev.Seq] = ev
		}
		cached, _ := ms.log.Since(after, limit+1)
		for _, part := range [][]event.Event{cached, ms.unsaved} {
			count := 0
			for _, ev := range part {
				if ev.Seq > after {
					bySeq[ev.Seq] = ev
					count++
					if count >= limit+1 {
						break
					}
				}
			}
		}
		evs = nil
		for _, ev := range bySeq {
			evs = append(evs, ev)
		}
		slices.SortFunc(evs, func(a, b event.Event) int {
			if a.Seq < b.Seq {
				return -1
			}
			if a.Seq > b.Seq {
				return 1
			}
			return 0
		})
	}
	page := EventPage{NextSeq: after, Truncated: truncated, Events: []event.Event{}}
	var bytes int64
	for _, ev := range evs {
		size := event.EncodedSize(ev)
		if len(page.Events) >= limit || p.MaxPageBytes != nil && size > *p.MaxPageBytes-bytes {
			if len(page.Events) == 0 {
				return EventPage{}, ErrEventTooLarge
			}
			page.HasMore = true
			break
		}
		if ev.Seq > page.NextSeq+1 {
			page.Truncated = true
		}
		page.Events = append(page.Events, ev)
		page.NextSeq = ev.Seq
		bytes += size
	}
	if live && page.NextSeq < ms.log.LastSeq() {
		if len(page.Events) == 0 {
			page.Truncated = true
			page.HasMore = false
		} else if page.HasMore || len(page.Events) >= limit {
			page.HasMore = true
		} else {
			page.Truncated = true
		}
	}
	return page, nil
}
