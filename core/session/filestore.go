package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
)

// FileStore persists each session record as a JSON file, <dir>/<id>.json.
// Records are replaced atomically. Session events are appended to
// <dir>/<id>.events.jsonl and synced before AppendEvent returns. Different
// sessions may share a directory, but each session must have only one active
// writer across FileStore instances and processes. Files can contain prompts
// and tool output, so they are created with mode 0600 inside a 0700 directory.
type FileStore struct {
	dir  string
	mu   sync.Mutex
	logs map[string]eventFileState
	// Windows cannot reliably replace/delete a record while a reader has it
	// open. Keep record handles separate from the transcript lock: event reads
	// already hold mu when calling Get. When both are needed, lock mu first.
	recordsMu sync.RWMutex
}

var _ Store = (*FileStore)(nil)

var _ EventStore = (*FileStore)(nil)

// NewFileStore opens (creating if needed) a FileStore in dir.
func NewFileStore(dir string) (*FileStore, error) {
	if dir == "" {
		return nil, errors.New("session: empty store directory")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("session: create store: %w", err)
	}
	return &FileStore{dir: dir, logs: make(map[string]eventFileState)}, nil
}

// DefaultStoreDir is the per-user session directory used by the CLI:
// <user config dir>/harness-kit/sessions, overridable with HARNESS_KIT_STORE.
func DefaultStoreDir() (string, error) {
	if d := os.Getenv("HARNESS_KIT_STORE"); d != "" {
		return d, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "harness-kit", "sessions"), nil
}

// Dir returns the store directory.
func (s *FileStore) Dir() string { return s.dir }

var validStoreID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func (s *FileStore) path(id string) (string, error) {
	if !validStoreID.MatchString(id) {
		return "", fmt.Errorf("session: invalid session id %q", id)
	}
	return filepath.Join(s.dir, id+".json"), nil
}

// Put implements Store.
func (s *FileStore) Put(_ context.Context, rec Record) error {
	p, err := s.path(rec.Info.ID)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, ".tmp-*.json")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(append(b, '\n'))
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	s.recordsMu.Lock()
	err = os.Rename(tmp.Name(), p)
	s.recordsMu.Unlock()
	if err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// Get implements Store.
func (s *FileStore) Get(_ context.Context, id string) (Record, error) {
	p, err := s.path(id)
	if err != nil {
		return Record{}, ErrNotFound
	}
	s.recordsMu.RLock()
	b, err := os.ReadFile(p)
	s.recordsMu.RUnlock()
	if errors.Is(err, fs.ErrNotExist) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, err
	}
	var rec Record
	if err := json.Unmarshal(b, &rec); err != nil {
		return Record{}, fmt.Errorf("session: corrupt record %s: %w", p, err)
	}
	return rec, nil
}

// List implements Store, ordered by creation time. Unreadable or corrupt
// files are skipped.
func (s *FileStore) List(ctx context.Context) ([]Record, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") {
			continue
		}
		rec, err := s.Get(ctx, strings.TrimSuffix(name, ".json"))
		if err != nil {
			continue
		}
		out = append(out, rec)
	}
	slices.SortFunc(out, func(a, b Record) int {
		if c := a.Info.CreatedAt.Compare(b.Info.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.Info.ID, b.Info.ID)
	})
	return out, nil
}

// Delete removes the record and its event transcript. Deleting a missing
// record is not an error.
func (s *FileStore) Delete(_ context.Context, id string) error {
	p, err := s.path(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordsMu.Lock()
	defer s.recordsMu.Unlock()
	var errs []error
	for _, name := range []string{p, s.eventPath(id)} {
		if err := os.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	delete(s.logs, id)
	return errors.Join(errs...)
}
