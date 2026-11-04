package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/tomanagle/harness-kit/agent"
	"github.com/tomanagle/harness-kit/event"
	"github.com/tomanagle/harness-kit/settings"
	"github.com/tomanagle/harness-kit/workspace"
)

func workspacePolicy(value workspace.Settings) workspace.Settings {
	if value == (workspace.Settings{}) {
		return workspace.Defaults()
	}
	return value
}

// WorkspaceSupported reports whether this host can create/capture workspaces.
// Saved turn diffs remain readable even without a native backend.
func (m *Manager) WorkspaceSupported() bool { return m.opts.Workspace != nil }

func (m *Manager) retainFailedWorkspace(info Info, cfg agent.SessionConfig, cause error) (Info, error) {
	info.State = StateFailed
	info.Cwd = cfg.Cwd
	info.ActiveTurnID = ""
	info.UpdatedAt = time.Now().UTC()
	info.Error = &event.ErrorInfo{Code: "workspace_start_failed", Message: cause.Error()}
	initial, _ := cfg.Clone()
	policy := workspacePolicy(info.WorkspaceSettings)
	record := Record{Info: info, Config: cfg, InitialConfig: &initial, InitialWorkspace: &policy, NativeDefaults: cfg.NativeDefaults, ForkSourceRuntimeID: cfg.ForkFrom}
	err := m.store.Put(context.Background(), record)
	message := fmt.Sprintf("owned worktree retained at %s for session %s", info.Workspace.Path, info.ID)
	if err != nil {
		return info, fmt.Errorf("%w (%s; recovery record could not be saved: %v)", cause, message, err)
	}
	return info, fmt.Errorf("%w (%s; use explicit workspace removal after reviewing it)", cause, message)
}
func (m *Manager) validateWorkspacePolicy(value workspace.Settings) error {
	if err := workspace.Validate(value); err != nil {
		return fmt.Errorf("%w: %v", settings.ErrInvalid, err)
	}
	if (value.Mode == "worktree" || value.CaptureDiff) && m.opts.Workspace == nil {
		return fmt.Errorf("%w: host has no workspace backend", agent.ErrUnsupported)
	}
	return nil
}

func (m *Manager) prepareWorkspace(ctx context.Context, info *Info, cfg *agent.SessionConfig) error {
	info.WorkspaceSettings = workspacePolicy(info.WorkspaceSettings)
	policy := info.WorkspaceSettings
	if err := m.validateWorkspacePolicy(policy); err != nil {
		return err
	}
	if info.Workspace == nil && policy.Mode != "worktree" && !policy.CaptureDiff {
		return nil
	}
	if m.opts.Workspace == nil {
		return fmt.Errorf("%w: restoring this workspace requires a backend", agent.ErrUnsupported)
	}
	if info.Workspace == nil && policy.Mode == "worktree" && cfg.ResumeID != "" {
		return fmt.Errorf("%w: importing a native resume ID into a new worktree is unsupported", agent.ErrUnsupported)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(policy.TimeoutMs)*time.Millisecond)
	defer cancel()
	binding, err := m.opts.Workspace.Prepare(ctx, info.ID, cfg.Cwd, policy, info.Workspace)
	if err != nil {
		return err
	}
	info.Workspace = &binding
	if binding.Owned {
		cfg.Cwd = binding.Path
	}
	return nil
}

func (ms *managed) turnWorkspace(patch settings.Patch) (workspace.Settings, error) {
	ms.mu.Lock()
	policy := workspacePolicy(ms.info.WorkspaceSettings)
	ms.mu.Unlock()
	if raw, ok := patch["workspace"]; ok {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
			return policy, settings.ErrInvalid
		}
		for name := range fields {
			field, ok := settings.Describe()["workspace."+name]
			if !ok || !slices.Contains(field.Scopes, "turn") {
				return policy, fmt.Errorf("%w: workspace.%s is not turn-configurable", settings.ErrScope, name)
			}
		}
		base := ms.m.currentSettings()
		base.Workspace = policy
		next, _, err := settings.Apply(base, settings.Patch{"workspace": raw}, nil, base)
		if err != nil {
			return policy, err
		}
		policy = next.Workspace
	}
	return policy, ms.m.validateWorkspacePolicy(policy)
}

func (ms *managed) captureWorkspace(ctx context.Context, t *Turn) *workspace.TurnChange {
	if !t.workspace.CaptureDiff {
		return nil
	}
	change := &workspace.TurnChange{SessionID: t.SessionID, TurnID: t.ID, MaxDiffBytes: t.workspace.MaxDiffBytes}
	ms.mu.Lock()
	info := ms.info
	cwd := ms.cfg.Cwd
	ms.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, time.Duration(t.workspace.TimeoutMs)*time.Millisecond)
	defer cancel()
	binding, err := ms.m.opts.Workspace.Prepare(ctx, info.ID, cwd, t.workspace, info.Workspace)
	if err != nil {
		change.Error = "before snapshot: " + err.Error()
		return change
	}
	change.Binding = binding
	if info.Workspace == nil {
		ms.emitMu.Lock()
		ms.mu.Lock()
		ms.info.Workspace = &binding
		ms.mu.Unlock()
		_ = ms.persist()
		ms.emitMu.Unlock()
	}
	before, err := ms.m.opts.Workspace.Capture(ctx, binding, t.ID, "before", t.workspace)
	if err != nil {
		change.Error = "before snapshot: " + err.Error()
	} else {
		change.Before = &before
	}
	return change
}

func (ms *managed) completeWorkspace(t *Turn, change *workspace.TurnChange) {
	if change == nil {
		return
	}
	if change.Before != nil {
		// A cancelled model turn still needs its final workspace boundary.
		ctx, cancel := context.WithTimeout(ms.m.baseCtx, time.Duration(t.workspace.TimeoutMs)*time.Millisecond)
		after, err := ms.m.opts.Workspace.Capture(ctx, change.Binding, t.ID, "after", t.workspace)
		cancel()
		if err != nil {
			change.Error = "after snapshot: " + err.Error()
		} else {
			change.After = &after
			change.Complete = true
		}
	}
	result := workspace.Diff{TurnChange: *change}
	if change.Complete {
		ctx, cancel := context.WithTimeout(ms.m.baseCtx, time.Duration(t.workspace.TimeoutMs)*time.Millisecond)
		diff, err := ms.m.opts.Workspace.Diff(ctx, *change, workspace.DiffOptions{})
		cancel()
		if err != nil {
			result.Complete = false
			result.Error = "diff: " + err.Error()
		} else {
			result = workspace.BoundDiff(diff, t.workspace.MaxDiffBytes)
		}
	}
	// Durable transcript storage retains bounded patches, so it does not need
	// the Git objects or a live workspace when a client requests the diff later.
	ms.emit(event.Event{Type: event.WorkspaceDiff, TurnID: t.ID, Data: result})
	if releaser, ok := ms.m.opts.Workspace.(workspace.SnapshotReleaser); ok && change.Binding.SessionID != "" {
		ctx, cancel := context.WithTimeout(ms.m.baseCtx, time.Duration(t.workspace.TimeoutMs)*time.Millisecond)
		err := releaser.ReleaseSnapshots(ctx, change.Binding, t.ID)
		cancel()
		if err != nil {
			ms.emit(event.Event{Type: event.RuntimeStatus, TurnID: t.ID, Data: event.RuntimeStatusData{Status: "workspace_cleanup_warning", Message: err.Error()}})
		}
	}
}

// TurnDiff retrieves a bounded diff between this turn's before/after snapshots.
// Boundary capture is observational: concurrent external edits are included.
// The event store retains snapshot identities; the backend retains their bytes.
func (m *Manager) TurnDiff(ctx context.Context, id, turnID string, options workspace.DiffOptions) (workspace.Diff, error) {
	if turnID == "" || options.MaxBytes < 0 {
		return workspace.Diff{}, settings.ErrInvalid
	}
	if _, err := m.Get(ctx, id); err != nil {
		return workspace.Diff{}, err
	}
	var change *workspace.Diff
	visit := func(ev event.Event) error {
		if ev.TurnID != turnID || ev.Type != event.WorkspaceDiff {
			return nil
		}
		var value workspace.Diff
		data, err := json.Marshal(ev.Data)
		if err != nil {
			return err
		}
		if err = json.Unmarshal(data, &value); err != nil {
			return err
		}
		// Copied transcript events belong to the original workspace. They must
		// never be mistaken for changes made in a newly forked worktree.
		if value.SessionID != id {
			return nil
		}
		change = &value
		return nil
	}
	if scanner, ok := m.store.(EventScanner); ok {
		if err := scanner.ScanEvents(ctx, id, visit); err != nil {
			return workspace.Diff{}, err
		}
	} else if store, ok := m.store.(EventStore); ok {
		var cursor uint64
		for {
			events, _, err := store.Events(ctx, id, cursor, 128)
			if err != nil {
				return workspace.Diff{}, err
			}
			if len(events) == 0 {
				break
			}
			for _, ev := range events {
				if err = visit(ev); err != nil {
					return workspace.Diff{}, err
				}
			}
			next := events[len(events)-1].Seq
			if next <= cursor {
				return workspace.Diff{}, errors.New("session: diff scan made no progress")
			}
			cursor = next
		}
	} else if ms, ok := m.lookup(id); ok {
		ms.emitMu.Lock()
		events, _ := ms.log.Since(0, 0)
		ms.emitMu.Unlock()
		for _, ev := range events {
			if err := visit(ev); err != nil {
				return workspace.Diff{}, err
			}
		}
	}
	if change == nil {
		return workspace.Diff{}, workspace.ErrNotFound
	}
	// Request limits can tighten, never expand, the recorded turn's limit.
	// Later global defaults do not change a stored result's retrieval policy.
	limit := change.MaxDiffBytes
	if options.MaxBytes > 0 && options.MaxBytes < limit {
		limit = options.MaxBytes
	}
	return workspace.BoundDiff(*change, limit), nil
}

// RemoveWorkspace explicitly removes an owned clean worktree. Close/suspend the
// runtime first. Session deletion and shutdown never remove workspaces.
func (m *Manager) RemoveWorkspace(ctx context.Context, id string) error {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	info, err := m.Get(ctx, id)
	if err != nil {
		return err
	}
	if info.State != StateSuspended && !info.State.Terminal() {
		return ErrBusy
	}
	if info.Workspace == nil || !info.Workspace.Owned {
		return workspace.ErrNotOwned
	}
	if m.opts.Workspace == nil {
		return agent.ErrUnsupported
	}
	policy := workspacePolicy(info.WorkspaceSettings)
	ctx, cancel := context.WithTimeout(ctx, time.Duration(policy.TimeoutMs)*time.Millisecond)
	defer cancel()
	return m.opts.Workspace.Remove(ctx, *info.Workspace)
}
