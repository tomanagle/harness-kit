package session

import (
	"context"
	"fmt"
	"time"

	"github.com/tomanagle/harness-kit/core/workspace"
)

func gitKey(binding workspace.Binding) string {
	if binding.GitDirIdentity != "" {
		return binding.GitDirIdentity
	}
	return binding.GitDir
}

type gitReservation struct {
	binding    workspace.Binding
	blockTurns bool
}

// Older saved bindings have canonical paths but no physical IDs. Preserve the
// checkout guard until their next Resolve upgrades the persisted binding.
func sameGitCheckout(a, b workspace.Binding) bool {
	if a.GitDirIdentity != "" && b.GitDirIdentity != "" {
		return a.GitDirIdentity == b.GitDirIdentity
	}
	return a.GitDir != "" && a.GitDir == b.GitDir
}

func (m *Manager) gitBusy(binding workspace.Binding, turn bool) bool {
	for _, reservation := range m.gitOperations {
		if sameGitCheckout(binding, reservation.binding) && (!turn || reservation.blockTurns) {
			return true
		}
	}
	return false
}

// HasSourceControl reports an optional host-supplied capability; no executable
// discovery or repository scan is performed until a Git operation is requested.
func (m *Manager) HasSourceControl() bool {
	_, ok := m.opts.Workspace.(workspace.SourceControl)
	return ok
}

// gitBinding must be called with lifecycleMu held. Lazy discovery associates
// ordinary sessions with the exact repository, without starting their runtime.
func (m *Manager) gitBinding(ctx context.Context, id string) (workspace.Binding, error) {
	info, err := m.Get(ctx, id)
	if err != nil {
		return workspace.Binding{}, err
	}
	backend := m.opts.Workspace.(workspace.SourceControl)
	binding, err := backend.Resolve(ctx, id, info.Cwd, info.Workspace)
	if err != nil {
		return workspace.Binding{}, err
	}
	if info.Workspace != nil && *info.Workspace == binding {
		return binding, nil
	}
	if ms, ok := m.lookup(id); ok {
		// Keep the accounting checkpoint and its transcript cursor consistent
		// when lazy binding happens while a runtime is emitting usage/events.
		ms.emitMu.Lock()
		defer ms.emitMu.Unlock()
		ms.mu.Lock()
		ms.info.Workspace = &binding
		ms.mu.Unlock()
		if err := ms.persist(); err != nil {
			ms.mu.Lock()
			ms.info.Workspace = info.Workspace
			ms.mu.Unlock()
			return workspace.Binding{}, err
		}
	} else {
		rec, err := m.store.Get(ctx, id)
		if err != nil {
			return workspace.Binding{}, err
		}
		rec.Info.Workspace = &binding
		if err := m.store.Put(ctx, rec); err != nil {
			return workspace.Binding{}, err
		}
	}
	return binding, nil
}

// checkGitPrompt runs during prompt admission. Reservations avoid holding the
// manager's lifecycle lock across slow Git commands in unrelated repositories.
func (m *Manager) checkGitPrompt(ctx context.Context, ms *managed) error {
	if len(m.gitOperations) == 0 {
		return nil
	}
	info := ms.snapshot()
	if info.Workspace != nil {
		if m.gitBusy(*info.Workspace, true) {
			return fmt.Errorf("%w: a Git operation is changing this checkout", ErrBusy)
		}
		return nil
	}
	// Resolve aliases/subdirectories as Git does; lexical cwd comparisons would
	// miss two sessions referring to the same checkout through symlinks.
	probe, cancel := context.WithTimeout(ctx, time.Duration(m.currentSettings().Git.TimeoutMs)*time.Millisecond)
	defer cancel()
	binding, err := m.opts.Workspace.(workspace.SourceControl).Resolve(probe, info.ID, info.Cwd, nil)
	if err != nil {
		if probe.Err() != nil {
			return probe.Err()
		}
		return nil
	}
	if m.gitBusy(binding, true) {
		return fmt.Errorf("%w: a Git operation is changing this checkout", ErrBusy)
	}
	return nil
}

func (m *Manager) beginGit(ctx context.Context, id string, write, remote bool) (workspace.SourceControl, workspace.Binding, workspace.GitSettings, context.Context, func(), error) {
	backend, ok := m.opts.Workspace.(workspace.SourceControl)
	policy := m.currentSettings().Git
	if !ok {
		return nil, workspace.Binding{}, policy, ctx, nil, workspace.ErrUnsupported
	}
	if write && policy.ReadOnly {
		return nil, workspace.Binding{}, policy, ctx, nil, workspace.ErrReadOnly
	}
	if remote && !policy.AllowRemote {
		return nil, workspace.Binding{}, policy, ctx, nil, workspace.ErrRemoteDisabled
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(policy.TimeoutMs)*time.Millisecond)
	stop := context.AfterFunc(m.baseCtx, cancel)
	cleanup := func() { stop(); cancel() }
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	fail := func(err error) (workspace.SourceControl, workspace.Binding, workspace.GitSettings, context.Context, func(), error) {
		cleanup()
		return nil, workspace.Binding{}, policy, ctx, nil, err
	}
	if m.baseCtx.Err() != nil {
		return fail(ErrClosed)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	binding, err := m.gitBinding(ctx, id)
	if err != nil {
		return fail(err)
	}
	if !write {
		return backend, binding, policy, ctx, cleanup, nil
	}
	if m.gitBusy(binding, false) {
		return fail(fmt.Errorf("%w: a Git operation is already changing this checkout", ErrBusy))
	}
	if !policy.AllowDuringTurn {
		m.mu.Lock()
		live := make([]*managed, 0, len(m.sessions))
		for _, ms := range m.sessions {
			live = append(live, ms)
		}
		m.mu.Unlock()
		for _, ms := range live {
			info := ms.snapshot()
			if info.State != StateRunning && info.State != StateCompacting && info.State != StateStarting && info.State != StateClosing && info.State != StateSuspending {
				continue
			}
			other := info.Workspace
			if other == nil {
				resolved, err := backend.Resolve(ctx, info.ID, info.Cwd, nil)
				if err != nil {
					if ctx.Err() != nil {
						return fail(ctx.Err())
					}
					continue
				}
				other = &resolved
			}
			if sameGitCheckout(*other, binding) {
				return fail(fmt.Errorf("%w: session %s is using this checkout", ErrBusy, info.ID))
			}
		}
	}
	if m.gitOperations == nil {
		m.gitOperations = map[string]gitReservation{}
	}
	m.gitOperations[gitKey(binding)] = gitReservation{binding: binding, blockTurns: !policy.AllowDuringTurn}
	done := func() {
		cleanup()
		m.lifecycleMu.Lock()
		delete(m.gitOperations, gitKey(binding))
		m.lifecycleMu.Unlock()
	}
	return backend, binding, policy, ctx, done, nil
}

func (m *Manager) GitStatus(ctx context.Context, id string) (workspace.RepositoryStatus, error) {
	b, w, s, ctx, done, err := m.beginGit(ctx, id, false, false)
	if err != nil {
		return workspace.RepositoryStatus{}, err
	}
	defer done()
	return b.Status(ctx, w, s)
}
func (m *Manager) GitDiff(ctx context.Context, id string, p workspace.FileDiffParams) (workspace.FileDiff, error) {
	b, w, s, ctx, done, err := m.beginGit(ctx, id, false, false)
	if err != nil {
		return workspace.FileDiff{}, err
	}
	defer done()
	return b.WorkingDiff(ctx, w, p, s)
}
func (m *Manager) GitBranches(ctx context.Context, id string) (workspace.BranchesResult, error) {
	b, w, s, ctx, done, err := m.beginGit(ctx, id, false, false)
	if err != nil {
		return workspace.BranchesResult{}, err
	}
	defer done()
	return b.Branches(ctx, w, s)
}
func (m *Manager) GitLog(ctx context.Context, id string, p workspace.LogParams) (workspace.LogResult, error) {
	b, w, s, ctx, done, err := m.beginGit(ctx, id, false, false)
	if err != nil {
		return workspace.LogResult{}, err
	}
	defer done()
	return b.Log(ctx, w, p, s)
}
func (m *Manager) GitStage(ctx context.Context, id string, p workspace.PathsParams) error {
	b, w, s, ctx, done, err := m.beginGit(ctx, id, true, false)
	if err != nil {
		return err
	}
	defer done()
	return b.Stage(ctx, w, p, s)
}
func (m *Manager) GitUnstage(ctx context.Context, id string, p workspace.PathsParams) error {
	b, w, s, ctx, done, err := m.beginGit(ctx, id, true, false)
	if err != nil {
		return err
	}
	defer done()
	return b.Unstage(ctx, w, p, s)
}
func (m *Manager) GitCommit(ctx context.Context, id string, p workspace.CommitParams) (workspace.CommitResult, error) {
	b, w, s, ctx, done, err := m.beginGit(ctx, id, true, false)
	if err != nil {
		return workspace.CommitResult{}, err
	}
	defer done()
	return b.Commit(ctx, w, p, s)
}
func (m *Manager) GitCheckout(ctx context.Context, id string, p workspace.CheckoutParams) error {
	b, w, s, ctx, done, err := m.beginGit(ctx, id, true, false)
	if err != nil {
		return err
	}
	defer done()
	return b.Checkout(ctx, w, p, s)
}
func (m *Manager) GitFetch(ctx context.Context, id string, p workspace.RemoteParams) (workspace.RemoteResult, error) {
	b, w, s, ctx, done, err := m.beginGit(ctx, id, true, true)
	if err != nil {
		return workspace.RemoteResult{}, err
	}
	defer done()
	return b.Fetch(ctx, w, p, s)
}
func (m *Manager) GitPull(ctx context.Context, id string, p workspace.RemoteParams) (workspace.RemoteResult, error) {
	b, w, s, ctx, done, err := m.beginGit(ctx, id, true, true)
	if err != nil {
		return workspace.RemoteResult{}, err
	}
	defer done()
	return b.Pull(ctx, w, p, s)
}
func (m *Manager) GitPush(ctx context.Context, id string, p workspace.RemoteParams) (workspace.RemoteResult, error) {
	b, w, s, ctx, done, err := m.beginGit(ctx, id, true, true)
	if err != nil {
		return workspace.RemoteResult{}, err
	}
	defer done()
	return b.Push(ctx, w, p, s)
}
