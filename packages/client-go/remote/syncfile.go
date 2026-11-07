package remote

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

type FileSyncOptions struct {
	MaxStateBytes   int64
	MaxSessionBytes int64
	MaxSessions     int
}

// FileSyncStore keeps events in 25-event chunks. One process/instance owns a
// directory. Inject another SyncStore for a transactional database or browser.
type FileSyncStore struct {
	directory string
	limits    FileSyncOptions
	mu        sync.Mutex
}
type cacheHead struct {
	CachedSession
	Bytes        int64
	PurgeThrough int64
}

func NewFileSyncStore(directory string, limits FileSyncOptions) (*FileSyncStore, error) {
	if directory == "" {
		return nil, errors.New("remote: sync directory required")
	}
	if limits.MaxStateBytes == 0 {
		limits.MaxStateBytes = 32 << 20
	}
	if limits.MaxSessionBytes == 0 {
		limits.MaxSessionBytes = 256 << 20
	}
	if limits.MaxSessions == 0 {
		limits.MaxSessions = 1000
	}
	if limits.MaxStateBytes < 1 || limits.MaxSessionBytes < 1 || limits.MaxSessions < 1 {
		return nil, errors.New("remote: invalid store limits")
	}
	return &FileSyncStore{directory: directory, limits: limits}, nil
}
func (s *FileSyncStore) path(parts ...any) string {
	b, _ := json.Marshal(parts)
	sum := sha256.Sum256(b)
	return filepath.Join(s.directory, hex.EncodeToString(sum[:])+".json")
}
func (s *FileSyncStore) read(path string, out any, limit int64) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if info.Size() > limit {
		return errors.New("remote: store capacity")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return errors.New("remote: store capacity")
	}
	return json.Unmarshal(data, out)
}
func (s *FileSyncStore) write(path string, value any, limit int64) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return errors.New("remote: store capacity")
	}
	if err = os.MkdirAll(s.directory, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(s.directory, ".sync-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		dir, e := os.Open(s.directory)
		if e != nil {
			return e
		}
		defer dir.Close()
		return dir.Sync()
	}
	return nil
}
func (s *FileSyncStore) Claim(ctx context.Context, id, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	var previous string
	p := s.path("destination", id)
	if err := s.read(p, &previous, s.limits.MaxStateBytes); err != nil {
		return err
	}
	if previous != "" && previous != key {
		return errors.New("remote: immutable destination")
	}
	return s.write(p, key, s.limits.MaxStateBytes)
}
func (s *FileSyncStore) Load(ctx context.Context, key string) (Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var c Checkpoint
	if err := ctx.Err(); err != nil {
		return c, err
	}
	err := s.read(s.path("state", key), &c, s.limits.MaxStateBytes)
	if err == nil && (c.Cursor < 0 || len(c.Bindings) > s.limits.MaxSessions) {
		err = errors.New("remote: invalid checkpoint")
	}
	return c, err
}
func (s *FileSyncStore) Save(ctx context.Context, key string, c Checkpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(c.Bindings) > s.limits.MaxSessions {
		return errors.New("remote: store capacity")
	}
	return s.write(s.path("state", key), c, s.limits.MaxStateBytes)
}
func (s *FileSyncStore) index(key string) ([]cacheHead, error) {
	var heads []cacheHead
	err := s.read(s.path("cache", key), &heads, s.limits.MaxStateBytes)
	return heads, err
}
func (s *FileSyncStore) List(ctx context.Context, key string) ([]CachedSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	heads, err := s.index(key)
	if err != nil {
		return nil, err
	}
	result := make([]CachedSession, 0, len(heads))
	for _, h := range heads {
		result = append(result, h.CachedSession)
	}
	return result, nil
}
func (s *FileSyncStore) Append(ctx context.Context, key string, change Change, events []Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	heads, err := s.index(key)
	if err != nil {
		return err
	}
	i := -1
	var previous cacheHead
	for n, h := range heads {
		if h.SessionID == change.SessionID {
			i = n
			previous = h
		}
	}
	if i < 0 {
		if len(heads) >= s.limits.MaxSessions {
			return errors.New("remote: store capacity")
		}
		i = len(heads)
		heads = append(heads, cacheHead{})
	}
	next := cacheHead{CachedSession: CachedSession{Change: change, CachedSeq: previous.CachedSeq}, Bytes: previous.Bytes}
	if change.Deleted || previous.Deleted {
		next.Deleted = true
		next.CachedSeq = 0
		next.Bytes = 0
		next.PurgeThrough = max(previous.PurgeThrough, previous.CachedSeq)
		heads[i] = next
		if err = s.write(s.path("cache", key), heads, s.limits.MaxStateBytes); err != nil {
			return err
		}
		for block := int64(0); block < (next.PurgeThrough+24)/25; block++ {
			if err = os.Remove(s.path("events", key, change.SessionID, block)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		heads[i].PurgeThrough = 0
		return s.write(s.path("cache", key), heads, s.limits.MaxStateBytes)
	}
	if len(events) > 100 {
		return errors.New("remote: invalid cache page")
	}
	for _, event := range events {
		next.CachedSeq++
		if event.Seq != next.CachedSeq {
			return errors.New("remote: history gap")
		}
		data, e := json.Marshal(event)
		if e != nil {
			return e
		}
		if len(data) > 66000 {
			return errors.New("remote: event too large")
		}
		next.Bytes += int64(len(data))
	}
	if next.Bytes > s.limits.MaxSessionBytes {
		return errors.New("remote: store capacity")
	}
	for offset := 0; offset < len(events); {
		start := events[offset].Seq
		block := (start - 1) / 25
		prefix := (start - 1) % 25
		var old []Event
		p := s.path("events", key, change.SessionID, block)
		if err = s.read(p, &old, 2<<20); err != nil {
			return err
		}
		if int64(len(old)) < prefix {
			return errors.New("remote: cache gap")
		}
		end := min(len(events), offset+int(25-prefix))
		chunk := append(old[:prefix], events[offset:end]...)
		if err = s.write(p, chunk, 2<<20); err != nil {
			return err
		}
		offset = end
	}
	if previous.Cursor > next.Cursor {
		next.Change = previous.Change
	}
	heads[i] = next
	return s.write(s.path("cache", key), heads, s.limits.MaxStateBytes)
}
func (s *FileSyncStore) Events(ctx context.Context, key, id string, after int64, limit int) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if after < 0 || limit < 1 || limit > 100 {
		return nil, errors.New("remote: invalid cache page")
	}
	heads, err := s.index(key)
	if err != nil {
		return nil, err
	}
	var head int64
	for _, h := range heads {
		if h.SessionID == id {
			head = h.CachedSeq
		}
	}
	out := []Event{}
	bytes := 0
	for seq := after + 1; seq <= head && len(out) < limit; {
		block := (seq - 1) / 25
		var chunk []Event
		if err = s.read(s.path("events", key, id, block), &chunk, 2<<20); err != nil {
			return nil, err
		}
		for seq <= head && (seq-1)/25 == block && len(out) < limit {
			index := (seq - 1) % 25
			if index >= int64(len(chunk)) || chunk[index].Seq != seq {
				return nil, fmt.Errorf("remote: cache gap")
			}
			event := chunk[index]
			data, e := json.Marshal(event)
			if e != nil {
				return nil, e
			}
			bytes += len(data)
			if bytes > 1<<20 && len(out) > 0 {
				return out, nil
			}
			out = append(out, event)
			seq++
		}
	}
	return out, nil
}
