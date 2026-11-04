package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tomanagle/harness-kit/core/internal/process"
	"github.com/tomanagle/harness-kit/core/workspace"
)

// A repository lock also covers its linked worktrees, because they share refs
// and an object store. Entries disappear after the last waiter releases them.
type repositoryLock struct {
	gate  chan struct{}
	users int
}

// Resolve only discovers and validates identity; it never waits for a Git
// mutation or creates a worktree. Operations revalidate after taking their lock.
func (b *Backend) Resolve(ctx context.Context, id, cwd string, existing *workspace.Binding) (workspace.Binding, error) {
	if id == "" {
		return workspace.Binding{}, workspace.ErrInvalid
	}
	if existing != nil {
		if existing.SessionID != id {
			return workspace.Binding{}, workspace.ErrNotOwned
		}
		actual, err := b.identity(ctx, existing.Path)
		if err != nil {
			return workspace.Binding{}, err
		}
		if !sameIdentity(*existing, actual) {
			return workspace.Binding{}, workspace.ErrNotOwned
		}
		if existing.Owned {
			if err := b.verifyOwner(*existing); err != nil {
				return workspace.Binding{}, err
			}
		}
		return pinIdentity(*existing, actual), nil
	}
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return workspace.Binding{}, err
		}
	}
	actual, err := b.identity(ctx, cwd)
	actual.SessionID = id
	return actual, err
}

func (b *Backend) lockRepository(ctx context.Context, common string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Physical IDs also coalesce different path spellings on case-insensitive
	// filesystems. Git bindings carry these IDs across session persistence.
	identity, err := directoryIdentity(common)
	if err != nil {
		return nil, err
	}
	common = identity
	b.mu.Lock()
	if b.repositories == nil {
		b.repositories = map[string]*repositoryLock{}
	}
	lock := b.repositories[common]
	if lock == nil {
		lock = &repositoryLock{gate: make(chan struct{}, 1)}
		b.repositories[common] = lock
	}
	lock.users++
	b.mu.Unlock()
	drop := func() {
		b.mu.Lock()
		lock.users--
		if lock.users == 0 {
			delete(b.repositories, common)
		}
		b.mu.Unlock()
	}
	select {
	case lock.gate <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-lock.gate; drop() }) }, nil
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	}
}

func (b *Backend) action(ctx context.Context, binding workspace.Binding, policy workspace.GitSettings, write, remote bool) (context.Context, func(), error) {
	if err := workspace.ValidateGit(policy); err != nil {
		return nil, nil, err
	}
	if write && policy.ReadOnly {
		return nil, nil, workspace.ErrReadOnly
	}
	if remote && !policy.AllowRemote {
		return nil, nil, workspace.ErrRemoteDisabled
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(policy.TimeoutMs)*time.Millisecond)
	unlock, err := b.lockRepository(ctx, binding.CommonDir)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	finish := func() { unlock(); cancel() }
	actual, err := b.identity(ctx, binding.Path)
	if err == nil && (!sameIdentity(binding, actual) || binding.Kind != "git") {
		err = workspace.ErrNotOwned
	}
	if err == nil && binding.Owned {
		err = b.verifyOwner(binding)
	}
	if err != nil {
		finish()
		return nil, nil, err
	}
	return ctx, finish, nil
}

// Action commands retain author, signing, SSH and credential environment
// choices, but never let an inherited Git location/index override the binding.
func actionEnvironment() []string {
	var out []string
	blocked := map[string]bool{"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_COMMON_DIR": true, "GIT_INDEX_FILE": true, "GIT_OBJECT_DIRECTORY": true, "GIT_ALTERNATE_OBJECT_DIRECTORIES": true, "GIT_NAMESPACE": true, "GIT_PREFIX": true, "GIT_CEILING_DIRECTORIES": true, "GIT_DISCOVERY_ACROSS_FILESYSTEM": true, "GIT_TERMINAL_PROMPT": true, "GIT_OPTIONAL_LOCKS": true, "GIT_LITERAL_PATHSPECS": true, "GIT_GLOB_PATHSPECS": true, "GIT_NOGLOB_PATHSPECS": true, "GIT_ICASE_PATHSPECS": true}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !blocked[name] && !strings.HasPrefix(name, "GIT_CONFIG") {
			out = append(out, entry)
		}
	}
	return append(out, "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
}

func (b *Backend) actionRun(ctx context.Context, dir string, policy workspace.GitSettings, input io.Reader, limit int64, args ...string) ([]byte, []byte, bool, error) {
	command, err := process.LookPath(b.Command, process.LookOptions{})
	if err != nil {
		return nil, nil, false, err
	}
	all := []string{"-C", dir, "--literal-pathspecs", "-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false", "-c", "color.ui=false"}
	if policy.Hooks == "disabled" {
		hooks, err := os.MkdirTemp("", "harness-kit-git-hooks-")
		if err != nil {
			return nil, nil, false, err
		}
		defer os.RemoveAll(hooks)
		all = append(all, "-c", "core.hooksPath="+hooks)
	}
	if policy.Signing != "inherit" {
		all = append(all, "-c", "commit.gpgSign="+strconv.FormatBool(policy.Signing == "enabled"))
	}
	all = append(all, args...)
	cmd := exec.CommandContext(ctx, command, all...)
	process.Configure(cmd)
	cmd.Cancel = func() error { return process.Kill(cmd) }
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = actionEnvironment()
	cmd.Stdin = input
	out, stderr := &limitedBuffer{limit: limit}, &limitedBuffer{limit: limit}
	cmd.Stdout, cmd.Stderr = out, stderr
	err = cmd.Run()
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		err = fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(redactURLs(string(stderr.data.Bytes()))))
	}
	return out.data.Bytes(), stderr.data.Bytes(), out.truncated || stderr.truncated, err
}

func (b *Backend) actionText(ctx context.Context, binding workspace.Binding, policy workspace.GitSettings, args ...string) (string, error) {
	out, _, cut, err := b.actionRun(ctx, binding.Path, policy, nil, policy.MaxOutputBytes, args...)
	if err != nil {
		return "", err
	}
	if cut {
		return "", workspace.ErrLimit
	}
	return strings.TrimSpace(string(out)), nil
}

func exitCode(err error, code int) bool {
	var e *exec.ExitError
	return errors.As(err, &e) && e.ExitCode() == code
}

func (b *Backend) head(ctx context.Context, binding workspace.Binding, policy workspace.GitSettings) (string, error) {
	head, err := b.actionText(ctx, binding, policy, "rev-parse", "--verify", "--quiet", "HEAD")
	if exitCode(err, 1) {
		return "", nil
	}
	return head, err
}

func (b *Backend) Status(ctx context.Context, binding workspace.Binding, policy workspace.GitSettings) (workspace.RepositoryStatus, error) {
	ctx, done, err := b.action(ctx, binding, policy, false, false)
	if err != nil {
		return workspace.RepositoryStatus{}, err
	}
	defer done()
	return b.status(ctx, binding, policy)
}

func (b *Backend) status(ctx context.Context, binding workspace.Binding, policy workspace.GitSettings) (workspace.RepositoryStatus, error) {
	out := workspace.RepositoryStatus{Root: binding.Path, Files: []workspace.FileStatus{}, Remotes: []workspace.Remote{}}
	data, _, cut, err := b.actionRun(ctx, binding.Path, policy, nil, policy.MaxOutputBytes, "status", "--porcelain=v2", "--branch", "--ahead-behind", "-z", "--untracked-files=all")
	if err != nil {
		return out, err
	}
	out.Truncated = cut
	parts := bytes.Split(data, []byte{0})
	if len(parts) > 0 && len(parts[len(parts)-1]) != 0 {
		parts = parts[:len(parts)-1]
	}
	for i := 0; i < len(parts); i++ {
		record := string(parts[i])
		if record == "" {
			continue
		}
		if strings.HasPrefix(record, "# ") {
			key, value, _ := strings.Cut(strings.TrimPrefix(record, "# "), " ")
			switch key {
			case "branch.oid":
				out.Unborn = value == "(initial)"
				if !out.Unborn {
					out.Head = value
				}
			case "branch.head":
				out.Detached = value == "(detached)"
				if !out.Detached {
					out.Branch = value
				}
			case "branch.upstream":
				out.Upstream = value
			case "branch.ab":
				_, _ = fmt.Sscanf(value, "+%d -%d", &out.Ahead, &out.Behind)
			}
			continue
		}
		file := workspace.FileStatus{}
		switch record[0] {
		case '?':
			if len(record) < 3 {
				return out, fmt.Errorf("invalid Git status record")
			}
			file = workspace.FileStatus{Path: record[2:], Index: "?", Worktree: "?", Untracked: true}
		case '1', '2', 'u':
			n := 9
			if record[0] == '2' {
				n = 10
			}
			if record[0] == 'u' {
				n = 11
			}
			fields := strings.SplitN(record, " ", n)
			if len(fields) != n || len(fields[1]) != 2 {
				return out, fmt.Errorf("invalid Git status record")
			}
			file = workspace.FileStatus{Path: fields[n-1], Index: fields[1][:1], Worktree: fields[1][1:], Conflicted: record[0] == 'u'}
			if file.Index == "." {
				file.Index = " "
			}
			if file.Worktree == "." {
				file.Worktree = " "
			}
			if record[0] == '2' {
				i++
				if i >= len(parts) || len(parts[i]) == 0 {
					out.Truncated = true
					continue
				}
				file.OriginalPath = string(parts[i])
			}
		default:
			continue
		}
		if len(out.Files) >= policy.MaxFiles {
			out.Truncated = true
			continue
		}
		out.Files = append(out.Files, file)
	}
	data, _, cut, err = b.actionRun(ctx, binding.Path, policy, nil, policy.MaxOutputBytes, "remote", "-v")
	if err != nil {
		return out, err
	}
	out.Truncated = out.Truncated || cut
	byName := map[string]int{}
	lines := strings.Split(string(data), "\n")
	if cut && len(lines) > 0 {
		lines = lines[:len(lines)-1]
	}
	for _, line := range lines {
		name, value, ok := strings.Cut(line, "\t")
		if !ok {
			continue
		}
		value, kind, ok := strings.Cut(value, " (")
		if !ok {
			continue
		}
		index, found := byName[name]
		if !found {
			if len(out.Remotes) >= policy.MaxFiles {
				out.Truncated = true
				continue
			}
			index = len(out.Remotes)
			byName[name] = index
			out.Remotes = append(out.Remotes, workspace.Remote{Name: name})
		}
		if kind == "fetch)" {
			out.Remotes[index].FetchURL = redactURL(value)
		} else if kind == "push)" {
			out.Remotes[index].PushURL = redactURL(value)
		}
	}
	return out, nil
}

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err == nil && u.Scheme != "" && u.User != nil {
		u.User = nil
		return u.String()
	}
	return raw
}
func redactURLs(text string) string {
	for _, word := range strings.Fields(text) {
		trimmed := strings.Trim(word, "'\"()[]")
		if clean := redactURL(trimmed); clean != trimmed {
			text = strings.ReplaceAll(text, trimmed, clean)
		}
	}
	return text
}

func boundedText(data []byte, limit int64) (string, bool, bool) {
	lossy := !utf8.Valid(data)
	s := strings.ToValidUTF8(string(data), "�")
	cut := int64(len(s)) > limit
	if cut {
		n := int(limit)
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		s = s[:n]
	}
	return s, cut, lossy
}

func (b *Backend) WorkingDiff(ctx context.Context, binding workspace.Binding, p workspace.FileDiffParams, policy workspace.GitSettings) (workspace.FileDiff, error) {
	ctx, done, err := b.action(ctx, binding, policy, false, false)
	if err != nil {
		return workspace.FileDiff{}, err
	}
	defer done()
	if p.MaxBytes < 0 {
		return workspace.FileDiff{}, workspace.ErrInvalid
	}
	limit := effectiveLimit(policy.MaxOutputBytes, p.MaxBytes)
	if p.Path != "" {
		if err := literalPaths(binding, []string{p.Path}, policy.MaxFiles); err != nil {
			return workspace.FileDiff{}, err
		}
	}
	args := []string{"diff", "--no-ext-diff", "--no-textconv", "--no-color"}
	if p.Staged {
		args = append(args, "--cached")
	}
	args = append(args, "--")
	if p.Path != "" {
		args = append(args, p.Path)
	}
	data, _, cut, err := b.actionRun(ctx, binding.Path, policy, nil, limit, args...)
	if err != nil {
		return workspace.FileDiff{}, err
	}
	if !p.Staged && !cut {
		args = []string{"ls-files", "--others", "--exclude-standard", "-z", "--"}
		if p.Path != "" {
			args = append(args, p.Path)
		}
		untracked, _, listCut, e := b.actionRun(ctx, binding.Path, policy, nil, policy.MaxOutputBytes, args...)
		if e != nil {
			return workspace.FileDiff{}, e
		}
		cut = cut || listCut
		paths := bytes.Split(untracked, []byte{0})
		if len(paths) > 0 && len(paths[len(paths)-1]) != 0 {
			paths = paths[:len(paths)-1]
		}
		count := 0
		for _, raw := range paths {
			if len(raw) == 0 {
				continue
			}
			count++
			if count > policy.MaxFiles || int64(len(data)) >= limit {
				cut = true
				break
			}
			path := string(raw)
			if e := literalPaths(binding, []string{path}, 1); e != nil {
				return workspace.FileDiff{}, e
			}
			patch, _, patchCut, e := b.actionRun(ctx, binding.Path, policy, nil, limit-int64(len(data)), "diff", "--no-index", "--no-ext-diff", "--no-textconv", "--no-color", "--", os.DevNull, path)
			if e != nil && !exitCode(e, 1) {
				return workspace.FileDiff{}, e
			}
			data = append(data, patch...)
			cut = cut || patchCut
		}
	}
	patch, displayCut, lossy := boundedText(data, limit)
	return workspace.FileDiff{Patch: patch, Truncated: cut || displayCut, EncodingLossy: lossy}, nil
}

func (b *Backend) Branches(ctx context.Context, binding workspace.Binding, policy workspace.GitSettings) (workspace.BranchesResult, error) {
	ctx, done, err := b.action(ctx, binding, policy, false, false)
	if err != nil {
		return workspace.BranchesResult{}, err
	}
	defer done()
	out := workspace.BranchesResult{Branches: []workspace.Branch{}}
	data, _, cut, err := b.actionRun(ctx, binding.Path, policy, nil, policy.MaxOutputBytes, "for-each-ref", "--format=%(refname)%00%(objectname)%00%(HEAD)%00%(upstream:short)%00%(worktreepath)%00%(symref)%00", "refs/heads", "refs/remotes")
	if err != nil {
		return out, err
	}
	out.Truncated = cut
	fields := strings.Split(string(data), "\x00")
	for i := 0; i+5 < len(fields); i += 6 {
		name := strings.TrimPrefix(fields[i], "\n")
		if fields[i+5] != "" {
			continue
		}
		if len(out.Branches) >= policy.MaxFiles {
			out.Truncated = true
			break
		}
		remote := strings.HasPrefix(name, "refs/remotes/")
		name = strings.TrimPrefix(strings.TrimPrefix(name, "refs/heads/"), "refs/remotes/")
		out.Branches = append(out.Branches, workspace.Branch{Name: name, Commit: fields[i+1], Current: fields[i+2] == "*", Upstream: fields[i+3], Worktree: fields[i+4], Remote: remote})
	}
	return out, nil
}

func (b *Backend) Log(ctx context.Context, binding workspace.Binding, p workspace.LogParams, policy workspace.GitSettings) (workspace.LogResult, error) {
	ctx, done, err := b.action(ctx, binding, policy, false, false)
	if err != nil {
		return workspace.LogResult{}, err
	}
	defer done()
	out := workspace.LogResult{Commits: []workspace.CommitInfo{}}
	if p.Limit < 0 {
		return out, workspace.ErrInvalid
	}
	limit := policy.MaxLogEntries
	if p.Limit > 0 && p.Limit < limit {
		limit = p.Limit
	}
	ref := p.Ref
	if ref == "" {
		ref, err = b.head(ctx, binding, policy)
		if err != nil {
			return out, err
		}
		if ref == "" {
			return out, nil
		}
	}
	commit, err := b.resolveCommit(ctx, binding, policy, ref)
	if err != nil {
		return out, err
	}
	data, _, cut, err := b.actionRun(ctx, binding.Path, policy, nil, policy.MaxOutputBytes, "log", "--no-show-signature", "--format=%H%x00%P%x00%s%x00%an%x00%ae%x00%aI%x00", "-n", strconv.Itoa(limit+1), commit, "--")
	if err != nil {
		return out, err
	}
	out.Truncated = cut
	fields := strings.Split(string(data), "\x00")
	for i := 0; i+5 < len(fields); i += 6 {
		id := strings.TrimPrefix(fields[i], "\n")
		if !validHash(id) {
			return out, fmt.Errorf("invalid Git log record")
		}
		if len(out.Commits) >= limit {
			out.Truncated = true
			break
		}
		parents := strings.Fields(fields[i+1])
		if parents == nil {
			parents = []string{}
		}
		out.Commits = append(out.Commits, workspace.CommitInfo{ID: id, Parents: parents, Subject: fields[i+2], AuthorName: fields[i+3], AuthorEmail: fields[i+4], Time: fields[i+5]})
	}
	return out, nil
}

func literalPaths(binding workspace.Binding, paths []string, maximum int) error {
	if len(paths) == 0 || len(paths) > maximum {
		return fmt.Errorf("%w: supply between 1 and %d literal paths", workspace.ErrInvalid, maximum)
	}
	for _, path := range paths {
		if path == "" || filepath.Clean(path) == "." || strings.ContainsRune(path, 0) || filepath.IsAbs(path) || filepath.VolumeName(path) != "" || (filepath.Separator == '\\' && strings.HasPrefix(path, "\\")) {
			return fmt.Errorf("%w: paths must be repository-relative files", workspace.ErrInvalid)
		}
		for _, part := range strings.Split(filepath.ToSlash(path), "/") {
			if part == ".." || strings.EqualFold(part, ".git") {
				return fmt.Errorf("%w: path escapes repository content", workspace.ErrInvalid)
			}
			if filepath.Separator == '\\' && (strings.Contains(part, ":") || strings.EqualFold(strings.TrimRight(part, ". "), ".git")) {
				return fmt.Errorf("%w: Windows path aliases repository metadata", workspace.ErrInvalid)
			}
		}
		full := filepath.Join(binding.Path, filepath.FromSlash(path))
		parent := filepath.Dir(full)
		for {
			real, err := filepath.EvalSymlinks(parent)
			if err == nil {
				rel, err := filepath.Rel(binding.Path, real)
				if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
					return fmt.Errorf("%w: symlink ancestor leaves repository", workspace.ErrInvalid)
				}
				break
			}
			if !os.IsNotExist(err) {
				return err
			}
			next := filepath.Dir(parent)
			if next == parent {
				return workspace.ErrInvalid
			}
			parent = next
		}
	}
	return nil
}

func (b *Backend) Stage(ctx context.Context, binding workspace.Binding, p workspace.PathsParams, policy workspace.GitSettings) error {
	ctx, done, err := b.action(ctx, binding, policy, true, false)
	if err != nil {
		return err
	}
	defer done()
	if err := literalPaths(binding, p.Paths, policy.MaxFiles); err != nil {
		return err
	}
	_, _, _, err = b.actionRun(ctx, binding.Path, policy, nil, policy.MaxOutputBytes, append([]string{"add", "--"}, p.Paths...)...)
	return err
}

func (b *Backend) Unstage(ctx context.Context, binding workspace.Binding, p workspace.PathsParams, policy workspace.GitSettings) error {
	ctx, done, err := b.action(ctx, binding, policy, true, false)
	if err != nil {
		return err
	}
	defer done()
	if err := literalPaths(binding, p.Paths, policy.MaxFiles); err != nil {
		return err
	}
	head, err := b.head(ctx, binding, policy)
	if err != nil {
		return err
	}
	args := []string{"reset", "--quiet", head, "--"}
	if head == "" {
		args = []string{"rm", "-r", "--cached", "--ignore-unmatch", "--"}
	}
	_, _, _, err = b.actionRun(ctx, binding.Path, policy, nil, policy.MaxOutputBytes, append(args, p.Paths...)...)
	return err
}

func (b *Backend) Commit(ctx context.Context, binding workspace.Binding, p workspace.CommitParams, policy workspace.GitSettings) (workspace.CommitResult, error) {
	ctx, done, err := b.action(ctx, binding, policy, true, false)
	if err != nil {
		return workspace.CommitResult{}, err
	}
	defer done()
	if strings.TrimSpace(p.Message) == "" || strings.ContainsRune(p.Message, 0) || len(p.Message) > 1<<20 {
		return workspace.CommitResult{}, fmt.Errorf("%w: provide a nonempty commit message of at most 1 MiB", workspace.ErrInvalid)
	}
	_, _, _, err = b.actionRun(ctx, binding.Path, policy, strings.NewReader(p.Message), policy.MaxOutputBytes, "commit", "--file=-")
	if err != nil {
		return workspace.CommitResult{}, err
	}
	head, err := b.head(ctx, binding, policy)
	return workspace.CommitResult{Commit: head}, err
}

func (b *Backend) validBranch(ctx context.Context, binding workspace.Binding, policy workspace.GitSettings, name string) error {
	if name == "" || len(name) > 1024 || strings.HasPrefix(name, "-") || strings.ContainsRune(name, 0) || strings.Contains(name, "@{") {
		return fmt.Errorf("%w: invalid branch", workspace.ErrInvalid)
	}
	_, err := b.actionText(ctx, binding, policy, "check-ref-format", "--branch", name)
	if err != nil {
		return fmt.Errorf("%w: invalid branch: %s", workspace.ErrInvalid, name)
	}
	return nil
}
func (b *Backend) resolveCommit(ctx context.Context, binding workspace.Binding, policy workspace.GitSettings, ref string) (string, error) {
	if len(ref) > 4096 || strings.ContainsRune(ref, 0) {
		return "", workspace.ErrInvalid
	}
	return b.actionText(ctx, binding, policy, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
}

func (b *Backend) clean(ctx context.Context, binding workspace.Binding, policy workspace.GitSettings) error {
	data, _, cut, err := b.actionRun(ctx, binding.Path, policy, nil, policy.MaxOutputBytes, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return err
	}
	if cut || len(data) > 0 {
		return workspace.ErrDirty
	}
	return nil
}

func (b *Backend) Checkout(ctx context.Context, binding workspace.Binding, p workspace.CheckoutParams, policy workspace.GitSettings) error {
	ctx, done, err := b.action(ctx, binding, policy, true, false)
	if err != nil {
		return err
	}
	defer done()
	if err := b.validBranch(ctx, binding, policy, p.Branch); err != nil {
		return err
	}
	if !p.Create && p.Base != "" {
		return fmt.Errorf("%w: base requires branch creation", workspace.ErrInvalid)
	}
	args := []string{"switch", "--no-overwrite-ignore"}
	if p.Create {
		head, err := b.head(ctx, binding, policy)
		if err != nil {
			return err
		}
		base := head
		if p.Base != "" {
			base, err = b.resolveCommit(ctx, binding, policy, p.Base)
			if err != nil {
				return err
			}
		}
		if base != head {
			if err := b.clean(ctx, binding, policy); err != nil {
				return err
			}
		}
		args = append(args, "-c", p.Branch)
		if base != "" {
			args = append(args, base)
		}
	} else {
		if err := b.clean(ctx, binding, policy); err != nil {
			return err
		}
		args = append(args, "--", p.Branch)
	}
	_, _, _, err = b.actionRun(ctx, binding.Path, policy, nil, policy.MaxOutputBytes, args...)
	return err
}

func (b *Backend) configuredRemote(ctx context.Context, binding workspace.Binding, policy workspace.GitSettings, p workspace.RemoteParams) (string, string, error) {
	data, err := b.actionText(ctx, binding, policy, "remote")
	if err != nil {
		return "", "", err
	}
	names := strings.Split(data, "\n")
	current, err := b.actionText(ctx, binding, policy, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil && !exitCode(err, 1) {
		return "", "", err
	}
	remote := p.Remote
	if remote == "" && current != "" {
		remote, err = b.actionText(ctx, binding, policy, "config", "--get", "branch."+current+".remote")
		if err != nil && !exitCode(err, 1) {
			return "", "", err
		}
	}
	if remote == "" {
		remote = "origin"
	}
	found := false
	for _, name := range names {
		if remote == name && name != "" {
			found = true
		}
	}
	if !found || strings.HasPrefix(remote, "-") {
		return "", "", fmt.Errorf("%w: choose a configured remote name", workspace.ErrInvalid)
	}
	branch := p.Branch
	if branch == "" && current != "" {
		upstreamRemote, e := b.actionText(ctx, binding, policy, "config", "--get", "branch."+current+".remote")
		if e != nil && !exitCode(e, 1) {
			return "", "", e
		}
		if upstreamRemote == remote {
			merge, e := b.actionText(ctx, binding, policy, "config", "--get", "branch."+current+".merge")
			if e != nil && !exitCode(e, 1) {
				return "", "", e
			}
			branch = strings.TrimPrefix(merge, "refs/heads/")
		}
		if branch == "" {
			branch = current
		}
	}
	if branch != "" {
		if err := b.validBranch(ctx, binding, policy, branch); err != nil {
			return "", "", err
		}
	}
	return remote, branch, nil
}

func (b *Backend) remoteAction(ctx context.Context, binding workspace.Binding, p workspace.RemoteParams, policy workspace.GitSettings, action string) (workspace.RemoteResult, error) {
	ctx, done, err := b.action(ctx, binding, policy, true, true)
	if err != nil {
		return workspace.RemoteResult{}, err
	}
	defer done()
	if p.SetUpstream && action != "push" {
		return workspace.RemoteResult{}, fmt.Errorf("%w: setUpstream only applies to push", workspace.ErrInvalid)
	}
	remote, branch, err := b.configuredRemote(ctx, binding, policy, p)
	if err != nil {
		return workspace.RemoteResult{}, err
	}
	args := []string{action}
	var fetched, fetchErrors []byte
	var fetchCut bool
	switch action {
	case "pull":
		if branch == "" {
			return workspace.RemoteResult{}, workspace.ErrInvalid
		}
		if err := b.clean(ctx, binding, policy); err != nil {
			return workspace.RemoteResult{}, err
		}
		if _, err := b.actionText(ctx, binding, policy, "symbolic-ref", "--quiet", "HEAD"); err != nil {
			return workspace.RemoteResult{}, fmt.Errorf("%w: pull requires a branch", workspace.ErrConflict)
		}
		// Pull does not expose merge's --no-overwrite-ignore switch. Split it
		// into fetch and fast-forward merge so ignored local files are retained
		// when the incoming commit starts tracking the same path.
		fetched, fetchErrors, fetchCut, err = b.actionRun(ctx, binding.Path, policy, nil, policy.MaxOutputBytes, "fetch", "--", remote, branch)
		if err != nil {
			return workspace.RemoteResult{}, err
		}
		args = []string{"merge", "--ff-only", "--no-overwrite-ignore", "--no-autostash", "--", "FETCH_HEAD"}
	case "push":
		if branch == "" {
			return workspace.RemoteResult{}, fmt.Errorf("%w: detached push needs an explicit destination branch", workspace.ErrInvalid)
		}
		if p.SetUpstream {
			if _, err := b.actionText(ctx, binding, policy, "symbolic-ref", "--quiet", "HEAD"); err != nil {
				return workspace.RemoteResult{}, fmt.Errorf("%w: setUpstream requires a branch", workspace.ErrInvalid)
			}
			args = append(args, "--set-upstream")
		}
		args = append(args, "--no-force", "--no-mirror", "--no-follow-tags", "--", remote, "HEAD:refs/heads/"+branch)
	case "fetch":
		args = append(args, "--", remote)
		if p.Branch != "" {
			args = append(args, branch)
		}
	}
	out, stderr, cut, err := b.actionRun(ctx, binding.Path, policy, nil, policy.MaxOutputBytes, args...)
	if err != nil {
		return workspace.RemoteResult{}, err
	}
	combined := &limitedBuffer{limit: policy.MaxOutputBytes}
	for _, data := range [][]byte{fetched, fetchErrors, out, stderr} {
		_, _ = combined.Write(data)
	}
	text, displayCut, _ := boundedText([]byte(redactURLs(combined.data.String())), policy.MaxOutputBytes)
	return workspace.RemoteResult{Output: text, Truncated: cut || fetchCut || combined.truncated || displayCut}, nil
}
func (b *Backend) Fetch(ctx context.Context, binding workspace.Binding, p workspace.RemoteParams, policy workspace.GitSettings) (workspace.RemoteResult, error) {
	return b.remoteAction(ctx, binding, p, policy, "fetch")
}
func (b *Backend) Pull(ctx context.Context, binding workspace.Binding, p workspace.RemoteParams, policy workspace.GitSettings) (workspace.RemoteResult, error) {
	return b.remoteAction(ctx, binding, p, policy, "pull")
}
func (b *Backend) Push(ctx context.Context, binding workspace.Binding, p workspace.RemoteParams, policy workspace.GitSettings) (workspace.RemoteResult, error) {
	return b.remoteAction(ctx, binding, p, policy, "push")
}

var _ workspace.SourceControl = (*Backend)(nil)
