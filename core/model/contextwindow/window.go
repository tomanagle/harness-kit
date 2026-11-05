// Package contextwindow manages caller-owned inference history. It does not
// compact delegated Codex/Claude sessions or choose a tokenizer/model.
package contextwindow

import (
	"errors"
	"sync"

	"github.com/tomanagle/harness-kit/core/model"
)

// Snapshot is portable JSON state; applications decide where and when to save it.
type Snapshot struct {
	Version  int             `json:"version"`
	Revision uint64          `json:"revision"`
	Messages []model.Message `json:"messages"`
}

type Window struct {
	mu       sync.Mutex
	messages []model.Message
	revision uint64
}

func New(messages ...model.Message) (*Window, error) {
	w := &Window{}
	if err := w.Append(messages...); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *Window) Append(messages ...model.Message) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	all := append(append([]model.Message(nil), w.messages...), messages...)
	if err := model.ValidateMessages(all); err != nil {
		return err
	}
	cloned, err := model.CloneMessages(all)
	if err != nil {
		return err
	}
	w.messages = cloned
	w.revision++
	return nil
}

func (w *Window) Snapshot() Snapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	messages, _ := model.CloneMessages(w.messages)
	return Snapshot{Version: 1, Revision: w.revision, Messages: messages}
}

func Restore(s Snapshot) (*Window, error) {
	if s.Version != 1 {
		return nil, errors.New("contextwindow: unsupported snapshot version")
	}
	if err := model.ValidateMessages(s.Messages); err != nil {
		return nil, err
	}
	messages, err := model.CloneMessages(s.Messages)
	if err != nil {
		return nil, err
	}
	return &Window{messages: messages, revision: s.Revision}, nil
}

// Replace commits prepared/compacted history only if nobody appended since the
// caller's snapshot. It never silently loses a concurrently submitted prompt.
func (w *Window) Replace(expected uint64, messages []model.Message) error {
	if err := model.ValidateMessages(messages); err != nil {
		return err
	}
	copy, err := model.CloneMessages(messages)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.revision != expected {
		return ErrConflict
	}
	w.messages = copy
	w.revision++
	return nil
}
