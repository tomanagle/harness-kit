package session

import (
	"context"
)

// Rename sets a session's title. Titles are one line: runs of whitespace
// become single spaces. An empty title or one longer than MaxTitleLength
// returns ErrInvalidTitle. Any session in the store can be renamed, live
// or not, and a title set this way is never replaced by the automatic one.
// Renaming does not change UpdatedAt, which tracks activity.
func (m *Manager) Rename(ctx context.Context, id, newTitle string) (Info, error) {
	name, err := cleanTitle(newTitle, false)
	if err != nil {
		return Info{}, err
	}
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	if ms, ok := m.lookup(id); ok {
		return ms.rename(name)
	}
	rec, err := m.store.Get(ctx, id)
	if err != nil {
		return Info{}, err
	}
	rec.Info, err = m.storedInfo(ctx, rec.Info)
	if err != nil {
		return Info{}, err
	}
	// A dormant session can publish and persist an update without launching
	// a runtime or entering the live-session cache.
	ms, err := m.newManaged(ctx, rec.Info, rec.Config, true)
	if err != nil {
		return Info{}, err
	}
	return ms.rename(name)
}

// Delete closes the session if it is live and removes its record from the
// store, including its event log. It never deletes the runtime's own transcript.
func (m *Manager) Delete(ctx context.Context, id string) error {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	if ms, ok := m.lookup(id); ok {
		if err := ms.close(ctx, "closed"); err != nil {
			return err
		}
		m.mu.Lock()
		if m.sessions[id] == ms {
			delete(m.sessions, id)
		}
		m.mu.Unlock()
	} else if _, err := m.store.Get(ctx, id); err != nil {
		return err
	}
	return m.store.Delete(ctx, id)
}

// Get returns a session's current info.
func (m *Manager) Get(ctx context.Context, id string) (Info, error) {
	if ms, ok := m.lookup(id); ok {
		return ms.snapshot(), nil
	}
	rec, err := m.store.Get(ctx, id)
	if err != nil {
		return Info{}, err
	}
	return m.storedInfo(ctx, rec.Info)
}

// List returns all sessions known to the store, with live state for
// sessions held in memory.
func (m *Manager) List(ctx context.Context) ([]Info, error) {
	recs, err := m.store.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Info, 0, len(recs))
	for _, r := range recs {
		if ms, ok := m.lookup(r.Info.ID); ok {
			out = append(out, ms.snapshot())
		} else {
			out = append(out, dormantInfo(r.Info))
		}
	}
	return out, nil
}
