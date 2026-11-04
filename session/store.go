// Package session manages the lifecycle of agent runtime sessions: it gives
// every session a stable Harness Kit ID, enforces one active turn at a
// time, records an ordered event log, routes approval requests through
// policies and the permission broker, and persists session records to a
// replaceable Store. Stores implementing EventStore also retain the full
// event transcript across manager restarts.
//
// For delegated runtimes (Codex, Claude Code), runtime-native conversation
// state still lives in the runtime; the saved events are an observable
// transcript, and the runtime-native ID is needed to resume the agent.
package session

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tomanagle/harness-kit/agent"
	"github.com/tomanagle/harness-kit/budget"
	"github.com/tomanagle/harness-kit/event"
	"github.com/tomanagle/harness-kit/workspace"
)

// State is a session's lifecycle state.
//
//	starting -> idle <-> running
//	idle|running -> closing -> closed
//	starting|idle|running -> failed   (the runtime died)
type State string

const (
	StateStarting   State = "starting"
	StateIdle       State = "idle"
	StateSuspending State = "suspending"
	StateSuspended  State = "suspended"
	StateRunning    State = "running"
	StateClosing    State = "closing"
	StateClosed     State = "closed"
	StateFailed     State = "failed"
)

// Terminal reports whether no further turns are possible.
func (s State) Terminal() bool { return s == StateClosed || s == StateFailed }

// Info describes a session.
type Info struct {
	Workspace         *workspace.Binding `json:"workspace,omitempty"`
	WorkspaceSettings workspace.Settings `json:"workspaceSettings"`
	Budget            budget.Limits      `json:"budget"`
	Usage             budget.Usage       `json:"usage"`
	// Archived hides a conversation only when a caller requests that filter.
	// It is independent of runtime lifecycle and never cancels work.
	Archived       bool   `json:"archived,omitempty"`
	ForkedFrom     string `json:"forkedFrom,omitempty"`
	ForkedAtSeq    uint64 `json:"forkedAtSeq,omitempty"`
	InheritedTurns int    `json:"inheritedTurns,omitempty"`
	ForkPending    bool   `json:"forkPending,omitempty"`
	// SettingsRevision changes only when session selections or pinning change.
	SettingsRevision uint64 `json:"settingsRevision"`
	// Pinned prevents automatic cache eviction and runtime suspension.
	Pinned bool `json:"pinned,omitempty"`
	// Config is the current selection inherited by subsequent turns.
	Config           *agent.TurnConfig `json:"config,omitempty"`
	ID               string            `json:"sessionId"`
	Runtime          string            `json:"runtime"`
	RuntimeSessionID string            `json:"runtimeSessionId,omitempty"`
	State            State             `json:"state"`
	ActiveTurnID     string            `json:"activeTurnId,omitempty"`
	Cwd              string            `json:"cwd,omitempty"`
	Model            string            `json:"model,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
	CreatedAt        time.Time         `json:"createdAt"`
	UpdatedAt        time.Time         `json:"updatedAt"`
	LastSeq          uint64            `json:"lastSeq"`
	Error            *event.ErrorInfo  `json:"error,omitempty"`
	// Title is a one-line name: the start of the first prompt, unless it
	// was set at creation or with Manager.Rename.
	Title string `json:"title,omitempty"`
	// Turns counts the prompts sent in this session.
	Turns int `json:"turns"`
	// LastStatus is the status of the most recent finished turn.
	LastStatus event.TurnStatus `json:"lastStatus,omitempty"`
}

// Record is what a Store persists.
type Record struct {
	InitialWorkspace *workspace.Settings `json:"initialWorkspace,omitempty"`
	Accounting       *budget.Ledger      `json:"accounting,omitempty"`
	InitialBudget    *budget.Limits      `json:"initialBudget,omitempty"`
	// ForkSourceRuntimeID is retained until a lazy native fork materializes.
	ForkSourceRuntimeID string `json:"forkSourceRuntimeId,omitempty"`
	// InitialConfig is the immutable parent of session.configure patches.
	InitialConfig *agent.SessionConfig `json:"initialConfig,omitempty"`
	// NativeDefaults preserves native selections across resumed runtime threads.
	NativeDefaults *agent.TurnConfig   `json:"nativeDefaults,omitempty"`
	Info           Info                `json:"info"`
	Config         agent.SessionConfig `json:"config"`
}

// ErrNotFound is returned when a session does not exist.
var ErrNotFound = errors.New("session: not found")

// Store persists session records. Implementations must be safe for
// concurrent use. Get returns ErrNotFound for unknown IDs.
type Store interface {
	Put(ctx context.Context, rec Record) error
	Get(ctx context.Context, id string) (Record, error)
	List(ctx context.Context) ([]Record, error)
	Delete(ctx context.Context, id string) error
}

// EventStore is an optional Store extension for persistent transcripts.
// AppendEvent receives a fully assigned session ID, sequence number and time;
// events must be appended in increasing sequence order. Implementations must
// be safe for concurrent use. A session has only one active event writer.
// Events returns events with Seq > afterSeq (limit <= 0 means no limit).
// truncated reports missing events in the requested range, rather than a
// page limited by limit. Unknown session IDs return ErrNotFound.
type EventStore interface {
	AppendEvent(ctx context.Context, ev event.Event) error
	Events(ctx context.Context, id string, afterSeq uint64, limit int) ([]event.Event, bool, error)
}

// MemoryStore is an in-memory Store.
type MemoryStore struct {
	mu   sync.RWMutex
	recs map[string]Record
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore { return &MemoryStore{recs: make(map[string]Record)} }

// cloneRecord prevents callers from modifying the creation parent, native
// defaults or current selections through a store snapshot.
func cloneRecord(rec Record) (Record, error) {
	if rec.Info.Workspace != nil {
		v := *rec.Info.Workspace
		rec.Info.Workspace = &v
	}
	if rec.InitialWorkspace != nil {
		v := *rec.InitialWorkspace
		rec.InitialWorkspace = &v
	}
	rec.Info.Budget = rec.Info.Budget.Clone()
	rec.Info.Usage = rec.Info.Usage.Clone()
	if rec.Accounting != nil {
		v := *rec.Accounting
		rec.Accounting = &v
	}
	if rec.InitialBudget != nil {
		v := rec.InitialBudget.Clone()
		rec.InitialBudget = &v
	}
	rec.Info.Metadata = maps.Clone(rec.Info.Metadata)
	if rec.Info.Config != nil {
		v := rec.Info.Config.Clone()
		rec.Info.Config = &v
	}
	if rec.Info.Error != nil {
		v := *rec.Info.Error
		rec.Info.Error = &v
	}
	var err error
	rec.Config, err = rec.Config.Clone()
	if err != nil {
		return Record{}, err
	}
	if rec.InitialConfig != nil {
		v, err := rec.InitialConfig.Clone()
		if err != nil {
			return Record{}, err
		}
		rec.InitialConfig = &v
	}
	if rec.NativeDefaults != nil {
		v := rec.NativeDefaults.Clone()
		rec.NativeDefaults = &v
	}
	return rec, nil
}

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
