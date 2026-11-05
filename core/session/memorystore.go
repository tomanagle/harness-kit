package session

import (
	"context"
	"slices"
	"strings"
	"sync"
)

// MemoryStore is an in-memory Store.
type MemoryStore struct {
	mu   sync.RWMutex
	recs map[string]Record
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore { return &MemoryStore{recs: make(map[string]Record)} }

// Put implements Store.
func (s *MemoryStore) Put(_ context.Context, rec Record) error {
	rec, err := cloneRecord(rec)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.recs[rec.Info.ID] = rec
	s.mu.Unlock()
	return nil
}

// Get implements Store.
func (s *MemoryStore) Get(_ context.Context, id string) (Record, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.recs[id]
	if !ok {
		return Record{}, ErrNotFound
	}
	return cloneRecord(rec)
}

// List implements Store, ordered by creation time.
func (s *MemoryStore) List(_ context.Context) ([]Record, error) {
	s.mu.RLock()
	out := make([]Record, 0, len(s.recs))
	for _, r := range s.recs {
		copy, err := cloneRecord(r)
		if err != nil {
			s.mu.RUnlock()
			return nil, err
		}
		out = append(out, copy)
	}
	s.mu.RUnlock()
	slices.SortFunc(out, func(a, b Record) int {
		if c := a.Info.CreatedAt.Compare(b.Info.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.Info.ID, b.Info.ID)
	})
	return out, nil
}

// Delete implements Store.
func (s *MemoryStore) Delete(_ context.Context, id string) error {
	s.mu.Lock()
	delete(s.recs, id)
	s.mu.Unlock()
	return nil
}
