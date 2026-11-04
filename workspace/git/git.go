// Package git implements workspace.Backend using a native Git executable.
package git

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tomanagle/harness-kit/process"
	"github.com/tomanagle/harness-kit/workspace"
)

// Backend uses Git for detached worktrees and snapshots. No branch is created
// or deleted. Operations on one Backend are serialized to protect ownership.
type Backend struct {
	Command string
	mu      sync.Mutex
}

func New(command string) *Backend {
	if command == "" {
		command = "git"
	}
	return &Backend{Command: command}
}

func (b *Backend) run(ctx context.Context, dir string, env []string, input io.Reader, limit int64, args ...string) ([]byte, bool, error) {
	path, err := process.LookPath(b.Command, process.LookOptions{})
	if err != nil {
		return nil, false, err
	}
	all := []string{"-C", dir, "-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false", "-c", "core.quotepath=true"}
	all = append(all, args...)
	cmd := exec.CommandContext(ctx, path, all...)
	process.Configure(cmd)
	cmd.Cancel = func() error { return process.Kill(cmd) }
	cmd.WaitDelay = 2 * time.Second
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "GIT_") {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	cmd.Env = append(cmd.Env, env...)
	cmd.Stdin = input
	out := &limitedBuffer{limit: limit}
	stderr := process.NewTail(4096)
	cmd.Stdout = out
	cmd.Stderr = stderr
	if err = cmd.Run(); err != nil {
		return nil, out.truncated, fmt.Errorf("git %s: %w%s", args[0], err, stderr.Suffix())
	}
	return out.data.Bytes(), out.truncated, nil
}

type limitedBuffer struct {
	data      bytes.Buffer
	limit     int64
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := b.limit - int64(b.data.Len())
	if int64(n) > remaining {
		b.truncated = true
		if remaining > 0 {
			_, _ = b.data.Write(p[:remaining])
		}
	} else {
		_, _ = b.data.Write(p)
	}
	return n, nil
}
func (b *Backend) text(ctx context.Context, dir string, args ...string) (string, error) {
	data, truncated, err := b.run(ctx, dir, nil, nil, 1<<20, args...)
	if truncated {
		return "", workspace.ErrLimit
	}
	return strings.TrimSpace(string(data)), err
}

func (b *Backend) identity(ctx context.Context, path string) (workspace.Binding, error) {
	root, err := b.text(ctx, path, "rev-parse", "--show-toplevel")
	if err != nil {
		return workspace.Binding{}, err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return workspace.Binding{}, err
	}
	gitdir, err := b.text(ctx, root, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return workspace.Binding{}, err
	}
	common, err := b.text(ctx, root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return workspace.Binding{}, err
	}
	gitdir, err = filepath.EvalSymlinks(gitdir)
	if err != nil {
		return workspace.Binding{}, err
	}
	common, err = filepath.EvalSymlinks(common)
	if err != nil {
		return workspace.Binding{}, err
	}
	return workspace.Binding{Kind: "git", Repo: root, Path: root, GitDir: gitdir, CommonDir: common}, nil
}

func (b *Backend) Prepare(ctx context.Context, id, cwd string, s workspace.Settings, existing *workspace.Binding) (workspace.Binding, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := workspace.Validate(s); err != nil {
		return workspace.Binding{}, err
	}
	if id == "" {
		return workspace.Binding{}, fmt.Errorf("%w: empty session ID", workspace.ErrInvalid)
	}
	if existing != nil {
		if existing.SessionID != id {
			return workspace.Binding{}, workspace.ErrNotOwned
		}
		current, err := b.identity(ctx, existing.Path)
		if err != nil {
			return workspace.Binding{}, fmt.Errorf("workspace missing or changed: %w", err)
		}
		if current.Path != existing.Path || current.GitDir != existing.GitDir || current.CommonDir != existing.CommonDir {
			return workspace.Binding{}, fmt.Errorf("%w: repository identity changed", workspace.ErrNotOwned)
		}
		if existing.Owned {
			if err = b.verifyOwner(*existing); err != nil {
				return workspace.Binding{}, err
			}
		}
		return *existing, nil
	}
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return workspace.Binding{}, err
		}
	}
	if s.Mode != "worktree" {
		binding, err := b.identity(ctx, cwd)
		binding.SessionID = id
		return binding, err
	}
	repo := s.Repo
	if repo == "" {
		repo = cwd
	}
	if !filepath.IsAbs(repo) {
		return workspace.Binding{}, fmt.Errorf("%w: repo must be absolute", workspace.ErrInvalid)
	}
	source, err := b.identity(ctx, repo)
	if err != nil {
		return workspace.Binding{}, err
	}
	base := s.Base
	if base == "" {
		base = "HEAD"
	}
	commit, err := b.text(ctx, source.Path, "rev-parse", "--verify", "--end-of-options", base+"^{commit}")
	if err != nil {
		return workspace.Binding{}, err
	}
	target := s.Path
	if target == "" {
		if !filepath.IsAbs(s.Root) {
			return workspace.Binding{}, fmt.Errorf("%w: root must be absolute", workspace.ErrInvalid)
		}
		target = filepath.Join(s.Root, safeName(id))
	}
	if !filepath.IsAbs(target) {
		return workspace.Binding{}, fmt.Errorf("%w: path must be absolute", workspace.ErrInvalid)
	}
	if _, err = os.Lstat(target); !os.IsNotExist(err) {
		return workspace.Binding{}, fmt.Errorf("%w: target must not already exist: %s", workspace.ErrInvalid, target)
	}
	if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		return workspace.Binding{}, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(target))
	if err != nil {
		return workspace.Binding{}, err
	}
	target = filepath.Join(parent, filepath.Base(target))
	// Worktree checkout must not run repository-provided post-checkout hooks.
	hooks, err := os.MkdirTemp("", "harness-kit-hooks-")
	if err != nil {
		return workspace.Binding{}, err
	}
	defer os.RemoveAll(hooks)
	if _, err = b.text(ctx, source.Path, "-c", "core.hooksPath="+hooks, "worktree", "add", "--detach", "--", target, commit); err != nil {
		return workspace.Binding{}, fmt.Errorf("create worktree at %s (inspect this path if Git was interrupted): %w", target, err)
	}
	binding, err := b.identity(ctx, target)
	if err != nil {
		return workspace.Binding{}, fmt.Errorf("worktree retained at %s but identity could not be verified: %w", target, err)
	}
	binding.SessionID, binding.Repo, binding.BaseCommit, binding.Owned = id, source.Path, commit, true
	data, _ := json.Marshal(binding)
	owner := ownerPath(binding)
	file, err := os.OpenFile(owner, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return workspace.Binding{}, fmt.Errorf("worktree created at %s but ownership could not be saved: %w", target, err)
	}
	_, err = file.Write(data)
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return workspace.Binding{}, fmt.Errorf("worktree retained at %s but ownership could not be saved: %w", target, err)
	}
	return binding, nil
}

func safeName(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:16])
}
func ownerPath(binding workspace.Binding) string {
	return filepath.Join(binding.GitDir, "harness-kit-owner.json")
}
func (b *Backend) verifyOwner(binding workspace.Binding) error {
	if !binding.Owned || binding.SessionID == "" || binding.GitDir == binding.CommonDir {
		return workspace.ErrNotOwned
	}
	data, err := os.ReadFile(ownerPath(binding))
	if err != nil {
		return fmt.Errorf("%w: %v", workspace.ErrNotOwned, err)
	}
	var owner workspace.Binding
	if json.Unmarshal(data, &owner) != nil || owner != binding {
		return workspace.ErrNotOwned
	}
	return nil
}

func (b *Backend) Remove(ctx context.Context, binding workspace.Binding) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	current, err := b.identity(ctx, binding.Path)
	if err != nil {
		return err
	}
	if current.GitDir != binding.GitDir || current.CommonDir != binding.CommonDir || current.Path != binding.Path {
		return workspace.ErrNotOwned
	}
	if err = b.verifyOwner(binding); err != nil {
		return err
	}
	head, err := b.text(ctx, binding.Path, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return err
	}
	if head != binding.BaseCommit {
		return fmt.Errorf("%w: worktree HEAD advanced from its creation commit; preserve those commits before manual cleanup", workspace.ErrDirty)
	}
	// Include ignored files: they may be valuable even though Git would allow
	// removing them with a clean tracked index.
	status, truncated, err := b.run(ctx, binding.Path, nil, nil, 1<<20, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignored")
	if err != nil {
		return err
	}
	if truncated || len(status) > 0 {
		return workspace.ErrDirty
	}
	_, err = b.text(ctx, binding.CommonDir, "worktree", "remove", "--", binding.Path)
	return err
}

func (b *Backend) Capture(ctx context.Context, binding workspace.Binding, turnID, phase string, s workspace.Settings) (workspace.Snapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := workspace.Validate(s); err != nil {
		return workspace.Snapshot{}, err
	}
	if phase != "before" && phase != "after" {
		return workspace.Snapshot{}, workspace.ErrInvalid
	}
	current, err := b.identity(ctx, binding.Path)
	if err != nil {
		return workspace.Snapshot{}, err
	}
	if current.GitDir != binding.GitDir || current.CommonDir != binding.CommonDir {
		return workspace.Snapshot{}, workspace.ErrNotOwned
	}
	// A NUL-delimited listing preserves tabs and newlines in filenames.
	listing, truncated, err := b.run(ctx, binding.Path, nil, nil, int64(s.MaxFiles)*4096, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return workspace.Snapshot{}, err
	}
	if truncated {
		return workspace.Snapshot{}, workspace.ErrLimit
	}
	paths := strings.Split(string(listing), "\x00")
	sort.Strings(paths)
	temp, err := os.MkdirTemp("", "harness-kit-index-")
	if err != nil {
		return workspace.Snapshot{}, err
	}
	defer os.RemoveAll(temp)
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(temp, "index")}
	if _, _, err = b.run(ctx, binding.Path, env, nil, 4096, "read-tree", "--empty"); err != nil {
		return workspace.Snapshot{}, err
	}
	var entries bytes.Buffer
	snapshot := workspace.Snapshot{CapturedAt: time.Now().UTC()}
	previous := ""
	for _, path := range paths {
		if path == "" || path == previous {
			continue
		}
		previous = path
		if filepath.IsAbs(path) || path == ".." || strings.HasPrefix(path, "../") {
			return workspace.Snapshot{}, workspace.ErrInvalid
		}
		full := filepath.Join(binding.Path, filepath.FromSlash(path))
		info, e := os.Lstat(full)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return workspace.Snapshot{}, e
		}
		if info.IsDir() {
			return workspace.Snapshot{}, fmt.Errorf("%w: submodules and nested repositories are not snapshotted: %s", workspace.ErrUnsupported, path)
		}
		snapshot.Files++
		if snapshot.Files > s.MaxFiles {
			return workspace.Snapshot{}, workspace.ErrLimit
		}
		mode := "100644"
		var data []byte
		if info.Mode()&os.ModeSymlink != 0 {
			mode = "120000"
			target, e := os.Readlink(full)
			if e != nil {
				return workspace.Snapshot{}, e
			}
			data = []byte(target)
		} else {
			if !info.Mode().IsRegular() {
				return workspace.Snapshot{}, fmt.Errorf("%w: special file %s", workspace.ErrUnsupported, path)
			}
			if info.Mode()&0111 != 0 {
				mode = "100755"
			}
			if info.Size() > s.MaxSnapshotBytes-snapshot.Bytes {
				return workspace.Snapshot{}, workspace.ErrLimit
			}
			file, e := os.Open(full)
			if e != nil {
				return workspace.Snapshot{}, e
			}
			data, e = io.ReadAll(io.LimitReader(file, s.MaxSnapshotBytes-snapshot.Bytes+1))
			_ = file.Close()
			if e != nil {
				return workspace.Snapshot{}, e
			}
		}
		snapshot.Bytes += int64(len(data))
		if snapshot.Bytes > s.MaxSnapshotBytes {
			return workspace.Snapshot{}, workspace.ErrLimit
		}
		hash, cut, e := b.run(ctx, binding.Path, nil, bytes.NewReader(data), 256, "hash-object", "-w", "--stdin", "--no-filters")
		if e != nil {
			return workspace.Snapshot{}, e
		}
		if cut {
			return workspace.Snapshot{}, workspace.ErrLimit
		}
		fmt.Fprintf(&entries, "%s %s\t%s\x00", mode, strings.TrimSpace(string(hash)), path)
	}
	if _, _, err = b.run(ctx, binding.Path, env, &entries, 4096, "update-index", "-z", "--index-info"); err != nil {
		return workspace.Snapshot{}, err
	}
	data, cut, err := b.run(ctx, binding.Path, env, nil, 256, "write-tree")
	if err != nil {
		return workspace.Snapshot{}, err
	}
	if cut {
		return workspace.Snapshot{}, workspace.ErrLimit
	}
	snapshot.Tree = strings.TrimSpace(string(data))
	// Rebuild staged entries in a second private index. write-tree on the
	// user's index can update its cache-tree extension, even for a read view.
	staged, cut, err := b.run(ctx, binding.Path, nil, nil, int64(s.MaxFiles)*4096, "ls-files", "--stage", "-z")
	if err != nil {
		return workspace.Snapshot{}, err
	}
	if cut {
		return workspace.Snapshot{}, workspace.ErrLimit
	}
	stagedFiles := 0
	for _, entry := range bytes.Split(staged, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		stagedFiles++
		if stagedFiles > s.MaxFiles {
			return workspace.Snapshot{}, workspace.ErrLimit
		}
		meta, _, _ := bytes.Cut(entry, []byte{'\t'})
		fields := strings.Fields(string(meta))
		if len(fields) != 3 || fields[2] != "0" {
			return workspace.Snapshot{}, fmt.Errorf("%w: unmerged index entries", workspace.ErrUnsupported)
		}
	}
	indexEnv := []string{"GIT_INDEX_FILE=" + filepath.Join(temp, "staged-index")}
	if _, _, err = b.run(ctx, binding.Path, indexEnv, nil, 4096, "read-tree", "--empty"); err != nil {
		return workspace.Snapshot{}, err
	}
	if _, _, err = b.run(ctx, binding.Path, indexEnv, bytes.NewReader(staged), 4096, "update-index", "-z", "--index-info"); err != nil {
		return workspace.Snapshot{}, err
	}
	indexTree, _, err := b.run(ctx, binding.Path, indexEnv, nil, 256, "write-tree")
	if err != nil {
		return workspace.Snapshot{}, err
	}
	snapshot.Index = strings.TrimSpace(string(indexTree))
	snapshot.Head, _ = b.text(ctx, binding.Path, "rev-parse", "--verify", "HEAD")
	// Retain content across ordinary Git garbage collection. The reference
	// namespace is separate from branches and does not alter HEAD or the index.
	ref := "refs/harness-kit/" + safeName(binding.SessionID) + "/" + safeName(turnID) + "/" + phase
	for suffix, hash := range map[string]string{"worktree": snapshot.Tree, "index": snapshot.Index} {
		if _, err = b.text(ctx, binding.Path, "update-ref", ref+"/"+suffix, hash); err != nil {
			return workspace.Snapshot{}, err
		}
	}
	return snapshot, nil
}

func (b *Backend) Diff(ctx context.Context, change workspace.TurnChange, opts workspace.DiffOptions) (workspace.Diff, error) {
	out := workspace.Diff{TurnChange: change}
	if !change.Complete || change.Before == nil || change.After == nil {
		return out, nil
	}
	limit := change.MaxDiffBytes
	if opts.MaxBytes > 0 && opts.MaxBytes < limit {
		limit = opts.MaxBytes
	}
	if limit <= 0 {
		return out, workspace.ErrInvalid
	}
	for index, pair := range [][2]string{{change.Before.Tree, change.After.Tree}, {change.Before.Index, change.After.Index}} {
		for _, hash := range pair {
			if !validHash(hash) {
				return out, workspace.ErrInvalid
			}
		}
		if pair[0] == pair[1] {
			continue
		}
		data, truncated, err := b.run(ctx, change.Binding.CommonDir, nil, nil, limit, "diff", "--no-ext-diff", "--no-textconv", "--no-color", "--no-renames", "--binary", pair[0], pair[1], "--")
		if err != nil {
			return out, err
		}
		out.Truncated = out.Truncated || truncated
		if index == 0 {
			out.WorkingTree = string(data)
		} else {
			out.Index = string(data)
		}
		limit -= int64(len(data))
		if limit <= 0 {
			out.Truncated = true
			break
		}
	}
	return workspace.BoundDiff(out, min(change.MaxDiffBytes, effectiveLimit(change.MaxDiffBytes, opts.MaxBytes))), nil
}

func effectiveLimit(base, requested int64) int64 {
	if requested > 0 && requested < base {
		return requested
	}
	return base
}

// ReleaseSnapshots removes only this session/turn's private temporary refs.
// Git's ordinary garbage collection can reclaim unreachable blob/tree objects.
func (b *Backend) ReleaseSnapshots(ctx context.Context, binding workspace.Binding, turnID string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if binding.SessionID == "" || turnID == "" {
		return workspace.ErrInvalid
	}
	for _, phase := range []string{"before", "after"} {
		for _, kind := range []string{"worktree", "index"} {
			ref := "refs/harness-kit/" + safeName(binding.SessionID) + "/" + safeName(turnID) + "/" + phase + "/" + kind
			if _, err := b.text(ctx, binding.CommonDir, "update-ref", "-d", ref); err != nil {
				return err
			}
		}
	}
	return nil
}
func validHash(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

var _ workspace.Backend = (*Backend)(nil)
