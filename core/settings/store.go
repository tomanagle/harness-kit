package settings

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ErrNotFound means no preferences have been saved yet.
var ErrPersistence = errors.New("settings persistence failed")

var ErrNotFound = errors.New("settings: no saved preferences")

// Store persists the global document. Save must commit atomically, leave the
// prior document unchanged on failure, and must not call back into the manager.
// One host owns a store; cross-process coordination belongs to the implementation.
type Store interface {
	Load(context.Context) (Values, error)
	Save(context.Context, Values) error
}

// Load returns defaults only for a missing document. Corruption and I/O failures
// are returned to the host instead of silently discarding user preferences.
func Load(ctx context.Context, store Store, defaults Values) (Values, error) {
	if store == nil {
		return defaults.Clone(), nil
	}
	v, err := store.Load(ctx)
	if errors.Is(err, ErrNotFound) {
		return defaults.Clone(), nil
	}
	if err != nil {
		return Values{}, err
	}
	if err = Validate(v); err != nil {
		return Values{}, err
	}
	return v.Clone(), nil
}

// FileStore is an optional JSON implementation. It never chooses a directory.
// Files are replaced atomically with owner-only permissions. The format is the
// same settings document accepted at startup, not a session-store record.
type FileStore struct{ Path string }

func (s FileStore) Load(ctx context.Context) (Values, error) {
	if err := ctx.Err(); err != nil {
		return Values{}, err
	}
	f, err := os.Open(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return Values{}, ErrNotFound
	}
	if err != nil {
		return Values{}, err
	}
	defer f.Close()
	const maxBytes = 16 * 1024 * 1024
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return Values{}, err
	}
	if len(data) > maxBytes {
		return Values{}, fmt.Errorf("settings file exceeds 16 MiB")
	}
	v, err := Decode(bytes.NewReader(data), Defaults())
	if err != nil {
		return Values{}, err
	}
	return v, ctx.Err()
}
func (s FileStore) Save(ctx context.Context, v Values) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := Validate(v); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if len(raw) > 16*1024*1024 {
		return fmt.Errorf("settings file exceeds 16 MiB")
	}
	if err = os.MkdirAll(filepath.Dir(s.Path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.Path), ".settings-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if _, err = f.Write(raw); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return os.Rename(f.Name(), s.Path)
}
