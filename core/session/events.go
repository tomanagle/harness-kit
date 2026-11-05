package session

import (
	"context"
	"maps"
	"slices"

	"github.com/tomanagle/harness-kit/core/event"
)

// Events returns events with Seq > afterSeq, including durable history for
// sessions that are not live. truncated reports unavailable events, not paging.
// This legacy API can read unlimited history; use EventsPage to apply the
// configured per-response count and byte budgets.
func (m *Manager) Events(sessionID string, afterSeq uint64, limit int) (events []event.Event, truncated bool, err error) {
	ms, live := m.lookup(sessionID)
	if live {
		ms.emitMu.Lock()
		defer ms.emitMu.Unlock()
	}
	if store, ok := m.store.(EventStore); ok {
		if !live || !ms.eventWriteFailed {
			return store.Events(context.Background(), sessionID, afterSeq, limit)
		}
		// A failed disk write must not also prevent live gap recovery. Merge
		// the bounded cache with disk, preserving sequence order and gaps.
		events, _, err = store.Events(context.Background(), sessionID, afterSeq, 0)
		if err != nil {
			return nil, false, err
		}
		cached, _ := ms.log.Since(afterSeq, 0)
		for _, ev := range ms.unsaved {
			if ev.Seq > afterSeq {
				cached = append(cached, ev)
			}
		}
		bySeq := make(map[uint64]event.Event, len(events)+len(cached))
		for _, ev := range append(events, cached...) {
			bySeq[ev.Seq] = ev
		}
		events = nil
		for _, ev := range bySeq {
			events = append(events, ev)
		}
		slices.SortFunc(events, func(a, b event.Event) int {
			if a.Seq < b.Seq {
				return -1
			}
			if a.Seq > b.Seq {
				return 1
			}
			return 0
		})
		if limit > 0 && len(events) > limit {
			events = events[:limit]
		}
		seq := afterSeq
		for _, ev := range events {
			if ev.Seq != seq+1 {
				truncated = true
			}
			seq = ev.Seq
		}
		if (limit <= 0 || len(events) < limit) && seq < ms.log.LastSeq() {
			truncated = true
		}
		return events, truncated, nil
	}
	if live {
		events, truncated = ms.log.Since(afterSeq, limit)
		return events, truncated, nil
	}
	rec, err := m.store.Get(context.Background(), sessionID)
	return nil, err == nil && afterSeq < rec.Info.LastSeq, err
}

// Subscribe receives events from all sessions matching filter. buffer <= 0
// uses event.DefaultBuffer. Close the subscription when done.
func (m *Manager) Subscribe(filter event.Filter, buffer int) *event.Subscription {
	return m.bus.Subscribe(filter, buffer)
}

// updatedLocked publishes a complete snapshot of the mutable session fields.
func (ms *managed) updatedLocked(turnID string) {
	info := ms.snapshot()
	ms.emitLocked(event.Event{Type: event.SessionUpdated, TurnID: turnID, Data: event.SessionUpdatedData{
		Context: info.Context, Compaction: &info.Compaction,
		Budget: &info.Budget, Usage: &info.Usage, Archived: info.Archived, Metadata: maps.Clone(info.Metadata), ForkPending: info.ForkPending,
		Config: info.Config, SettingsRevision: info.SettingsRevision, Pinned: info.Pinned, Title: info.Title, State: string(info.State), ActiveTurnID: info.ActiveTurnID,
		RuntimeSessionID: info.RuntimeSessionID, Turns: info.Turns,
		LastStatus: info.LastStatus, UpdatedAt: info.UpdatedAt, Error: info.Error,
	}})
}

// Emit implements agent.Handler.
func (ms *managed) Emit(ev event.Event) {
	if ev.TurnID == "" {
		ms.mu.Lock()
		if ms.turn != nil {
			ev.TurnID = ms.turn.ID
		}
		ms.mu.Unlock()
	}
	ms.emitMu.Lock()
	ms.emitLocked(ev)
	ms.observeNativeCompactionLocked(ev)
	if u, ok := ev.Data.(event.UsageData); ok {
		ms.observeUsageLocked(u, false)
		_ = ms.persist()
	}
	if ev.Type == event.ContextUsage || ev.Type == event.RuntimeStatus {
		_ = ms.persist()
	}
	ms.emitMu.Unlock()
}

func (ms *managed) emit(ev event.Event) {
	ms.emitMu.Lock()
	defer ms.emitMu.Unlock()
	ms.emitLocked(ev)
}

func (ms *managed) emitLocked(ev event.Event) {
	ms.applyContextEventLocked(&ev)
	ev.SessionID = ms.info.ID // immutable after creation
	ev.Seq = 0
	stored := ms.log.Append(ev)
	if store, ok := ms.m.store.(EventStore); ok {
		if err := store.AppendEvent(context.Background(), stored); err != nil {
			ms.unsaved = append(ms.unsaved, stored)
			ms.m.log.Error("persist event", "sessionId", stored.SessionID, "seq", stored.Seq, "err", err)
			ms.m.bus.Publish(stored)
			if !ms.eventWriteFailed {
				ms.eventWriteFailed = true
				warning := ms.log.Append(event.Event{SessionID: stored.SessionID, TurnID: stored.TurnID,
					Type: event.Error, Data: event.ErrorData{Code: "event_store_error", Message: "Session events could not be saved: " + err.Error()}})
				ms.unsaved = append(ms.unsaved, warning)
				ms.m.bus.Publish(warning)
			}
			return
		}
	}
	ms.m.bus.Publish(stored)
}
