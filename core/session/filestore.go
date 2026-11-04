package session

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/tomanagle/harness-kit/core/event"
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

type eventFileState struct {
	lastSeq uint64
	size    int64
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

// eventPath is used only after path has validated id.
func (s *FileStore) eventPath(id string) string {
	return filepath.Join(s.dir, id+".events.jsonl")
}

// AppendEvent persists one ordered event. An incomplete trailing line left
// by an interrupted write is discarded before appending. Complete malformed
// lines are errors, so an existing transcript is never silently rewritten.
func (s *FileStore) AppendEvent(ctx context.Context, ev event.Event) error {
	if _, err := s.path(ev.SessionID); err != nil {
		return err
	}
	if ev.Seq == 0 || ev.Type == "" || ev.Type == event.StreamGap {
		return fmt.Errorf("session: invalid stored event %q at sequence %d", ev.Type, ev.Seq)
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("session: encode event: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	// Windows append-only handles cannot truncate an interrupted final record.
	// One writer per session is required; our mutex serializes repair and seek.
	f, err := os.OpenFile(s.eventPath(ev.SessionID), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	state, cached := s.logs[ev.SessionID]
	if !cached || state.size != fi.Size() {
		_, _, state, err = readEventFile(ctx, f, ev.SessionID, 0, -1)
		if err != nil {
			return err
		}
	}
	if ev.Seq <= state.lastSeq {
		return fmt.Errorf("session: event sequence %d does not follow %d for %s", ev.Seq, state.lastSeq, ev.SessionID)
	}
	if state.size != fi.Size() {
		if err := f.Truncate(state.size); err != nil {
			return err
		}
	}
	if _, err := f.Seek(state.size, io.SeekStart); err != nil {
		return err
	}
	b = append(b, '\n')
	n, err := f.Write(b)
	if err != nil {
		return err
	}
	if n != len(b) {
		return io.ErrShortWrite
	}
	if err := f.Sync(); err != nil {
		return err
	}
	s.logs[ev.SessionID] = eventFileState{lastSeq: ev.Seq, size: state.size + int64(n)}
	return nil
}

// Events reads the durable transcript, including events no longer in the
// manager's bounded memory log. A missing transcript is valid for old records.
func (s *FileStore) Events(ctx context.Context, id string, afterSeq uint64, limit int) ([]event.Event, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	rec, err := s.Get(ctx, id)
	if err != nil {
		return nil, false, err
	}
	f, err := os.Open(s.eventPath(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, afterSeq < rec.Info.LastSeq, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	// -1 is an internal readEventFile mode that scans without retaining any
	// events. All nonpositive public limits mean unlimited.
	if limit < 0 {
		limit = 0
	}
	evs, truncated, state, err := readEventFile(ctx, f, id, afterSeq, limit)
	if err != nil {
		return nil, false, err
	}
	if rec.Info.LastSeq > state.lastSeq && rec.Info.LastSeq > afterSeq {
		truncated = true
	}
	return evs, truncated, nil
}

// readEventFile scans complete newline-terminated records without a line
// length limit. state.size is the end of the last complete record, allowing
// AppendEvent to repair an incomplete tail. A limit of -1 scans only.
func readEventFile(ctx context.Context, r io.Reader, id string, afterSeq uint64, limit int) ([]event.Event, bool, eventFileState, error) {
	reader := bufio.NewReader(r)
	var evs []event.Event
	var state eventFileState
	var truncated bool
	for {
		if err := ctx.Err(); err != nil {
			return nil, false, state, err
		}
		line, err := reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			return evs, truncated, state, nil
		}
		if err != nil {
			return nil, false, state, err
		}
		var ev event.Event
		if err := json.Unmarshal(line, &ev); err != nil {
			return nil, false, state, fmt.Errorf("session: corrupt events for %s at byte %d: %w", id, state.size, err)
		}
		if ev.SessionID != id || ev.Seq <= state.lastSeq || ev.Type == "" || ev.Type == event.StreamGap {
			return nil, false, state, fmt.Errorf("session: invalid event for %s at byte %d", id, state.size)
		}
		if ev.Seq > afterSeq {
			previous := max(afterSeq, state.lastSeq)
			if ev.Seq-previous > 1 {
				truncated = true
			}
			if limit == 0 || limit > 0 && len(evs) < limit {
				evs = append(evs, ev)
			}
		}
		state.lastSeq = ev.Seq
		state.size += int64(len(line))
	}
}

var _ EventPageStore = (*FileStore)(nil)
var _ EventScanner = (*FileStore)(nil)
var _ TurnLookup = (*FileStore)(nil)

var errStopEventScan = errors.New("session: stop event scan")

// EventsPage returns a count- and byte-bounded page without loading the rest of
// the transcript. Reading old cursors scans earlier records, retaining only the
// requested page and the current decoded event. Large events are atomic: callers
// receive ErrEventTooLarge if the next event cannot fit by itself.
func (s *FileStore) EventsPage(ctx context.Context, id string, afterSeq uint64, limit int, maxBytes *int64) (EventPage, error) {
	page := EventPage{Events: []event.Event{}, NextSeq: afterSeq}
	if maxBytes != nil && *maxBytes < 0 {
		return page, fmt.Errorf("session: page byte limit cannot be negative")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return page, err
	}
	rec, err := s.Get(ctx, id)
	if err != nil {
		return page, err
	}
	f, err := os.Open(s.eventPath(id))
	if errors.Is(err, fs.ErrNotExist) {
		page.Truncated = afterSeq < rec.Info.LastSeq
		return page, nil
	}
	if err != nil {
		return page, err
	}
	defer f.Close()
	var retained int64
	state, err := scanEventFile(ctx, f, id, func(ev event.Event, size int64) error {
		if ev.Seq <= afterSeq {
			return nil
		}
		if limit > 0 && len(page.Events) == limit || maxBytes != nil && size > *maxBytes-retained {
			if len(page.Events) == 0 {
				return fmt.Errorf("%w: sequence %d needs %d bytes, limit %d", ErrEventTooLarge, ev.Seq, size, *maxBytes)
			}
			page.HasMore = true
			return errStopEventScan
		}
		if ev.Seq-page.NextSeq > 1 {
			page.Truncated = true
		}
		page.Events = append(page.Events, ev)
		page.NextSeq = ev.Seq
		retained += size
		return nil
	})
	if errors.Is(err, errStopEventScan) {
		return page, nil
	}
	if err != nil {
		return EventPage{Events: []event.Event{}, NextSeq: afterSeq}, err
	}
	if rec.Info.LastSeq > state.lastSeq && rec.Info.LastSeq > afterSeq {
		page.Truncated = true
	}
	return page, nil
}

// ScanEvents visits the durable transcript one event at a time.
func (s *FileStore) ScanEvents(ctx context.Context, id string, visit func(event.Event) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := s.Get(ctx, id); err != nil {
		return err
	}
	f, err := os.Open(s.eventPath(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = scanEventFile(ctx, f, id, func(ev event.Event, _ int64) error { return visit(ev) })
	return err
}

// HasTurn checks a turn ID on disk instead of retaining all IDs in memory.
func (s *FileStore) HasTurn(ctx context.Context, id, turnID string) (bool, error) {
	found := false
	err := s.ScanEvents(ctx, id, func(ev event.Event) error {
		if ev.TurnID == turnID && turnID != "" {
			found = true
			return errStopEventScan
		}
		return nil
	})
	if errors.Is(err, errStopEventScan) {
		err = nil
	}
	return found, err
}

// scanEventFile validates complete newline-delimited records and invokes visit
// before proceeding. state.size excludes a partial final record, matching the
// append recovery rules. At most one decoded event is held by the scanner.
func scanEventFile(ctx context.Context, r io.Reader, id string, visit func(event.Event, int64) error) (eventFileState, error) {
	reader := bufio.NewReader(r)
	var state eventFileState
	for {
		if err := ctx.Err(); err != nil {
			return state, err
		}
		line, err := reader.ReadBytes('\n')
		if errors.Is(err, io.EOF) {
			return state, nil
		}
		if err != nil {
			return state, err
		}
		var ev event.Event
		if err := json.Unmarshal(line, &ev); err != nil {
			return state, fmt.Errorf("session: corrupt events for %s at byte %d: %w", id, state.size, err)
		}
		if ev.SessionID != id || ev.Seq <= state.lastSeq || ev.Type == "" || ev.Type == event.StreamGap {
			return state, fmt.Errorf("session: invalid event for %s at byte %d", id, state.size)
		}
		state.lastSeq = ev.Seq
		state.size += int64(len(line))
		if err := visit(ev, int64(len(line)-1)); err != nil {
			return state, err
		}
	}
}
