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
	"encoding/json"
	"errors"
	"maps"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/config"
	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/workspace"
)

// Record is what a Store persists.
type Record struct {
	InitialCompaction *config.CompactionSettings `json:"initialCompaction,omitempty"`
	InitialWorkspace  *workspace.Settings        `json:"initialWorkspace,omitempty"`
	// Accounting is the kit's opaque checkpoint. Custom stores preserve these
	// bytes as JSON; accounting implementation types are intentionally private.
	Accounting    json.RawMessage      `json:"accounting,omitempty"`
	InitialBudget *config.BudgetLimits `json:"initialBudget,omitempty"`
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

// cloneRecord prevents callers from modifying the creation parent, native
// defaults or current selections through a store snapshot.
func cloneRecord(rec Record) (Record, error) {
	rec.Info.Compaction = compactionPolicy(rec.Info.Compaction)
	if rec.Info.Context != nil {
		v := rec.Info.Context.Clone()
		rec.Info.Context = &v
	}
	if rec.InitialCompaction != nil {
		v := rec.InitialCompaction.Clone()
		rec.InitialCompaction = &v
	}
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
		rec.Accounting = append(json.RawMessage(nil), rec.Accounting...)
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
