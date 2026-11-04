package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/tomanagle/harness-kit/core/event"
)

// EventCopier is an optional streaming optimization for branching a transcript.
// The destination must be new. It copies events through the inclusive cursor,
// remapping SessionID and preserving all other event fields.
type EventCopier interface {
	CopyEvents(ctx context.Context, source, destination string, through uint64) error
}

// CopyEvents writes a complete branch with bounded memory. The destination is
// installed atomically, so an interrupted fork cannot expose half a transcript.
func (s *FileStore) CopyEvents(ctx context.Context, source, destination string, through uint64) error {
	if _, err := s.path(destination); err != nil {
		return err
	}
	if source == destination {
		return fmt.Errorf("session: fork destination must differ from source")
	}
	f, err := os.CreateTemp(s.dir, ".fork-*.jsonl")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	err = s.ScanEvents(ctx, source, func(ev event.Event) error {
		if ev.Seq > through {
			return nil
		}
		ev.SessionID = destination
		return json.NewEncoder(f).Encode(ev)
	})
	if err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err = os.Stat(s.eventPath(destination)); !os.IsNotExist(err) {
		return fmt.Errorf("session: fork destination already exists")
	}
	return os.Rename(f.Name(), s.eventPath(destination))
}
