package workspace

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
)

var (
	ErrReadOnly       = errors.New("workspace: Git writes disabled")
	ErrRemoteDisabled = errors.New("workspace: Git remote operations disabled")
	ErrConflict       = errors.New("workspace: Git operation conflicts with repository state")
)

// GitSettings is the git group of the shared settings document. Each operation
// captures the current host policy. These settings never choose a UI workflow.
type GitSettings struct {
	ReadOnly        bool  `json:"readOnly"`
	AllowRemote     bool  `json:"allowRemote"`
	AllowDuringTurn bool  `json:"allowDuringTurn"`
	TimeoutMs       int64 `json:"timeoutMs"`
	MaxOutputBytes  int64 `json:"maxOutputBytes"`
	MaxFiles        int   `json:"maxFiles"`
	MaxLogEntries   int   `json:"maxLogEntries"`
	// Hooks and Signing are "inherit" or "disabled"; signing also accepts "enabled".
	Hooks   string `json:"hooks"`
	Signing string `json:"signing"`
}

func GitDefaults() GitSettings {
	return GitSettings{AllowRemote: true, TimeoutMs: 30000, MaxOutputBytes: 1 << 20, MaxFiles: 10000, MaxLogEntries: 100, Hooks: "inherit", Signing: "inherit"}
}

func ValidateGit(s GitSettings) error {
	if s.TimeoutMs < 1 || s.MaxOutputBytes < 1 || s.MaxFiles < 1 || s.MaxLogEntries < 1 {
		return fmt.Errorf("%w: Git limits must be positive", ErrInvalid)
	}
	if s.TimeoutMs > math.MaxInt64/int64(time.Millisecond) {
		return fmt.Errorf("%w: Git timeout is too large", ErrInvalid)
	}
	if s.MaxFiles == math.MaxInt || s.MaxLogEntries == math.MaxInt {
		return fmt.Errorf("%w: Git record limits are too large", ErrInvalid)
	}
	if s.Hooks != "inherit" && s.Hooks != "disabled" {
		return fmt.Errorf("%w: git.hooks must be inherit or disabled", ErrInvalid)
	}
	if s.Signing != "inherit" && s.Signing != "disabled" && s.Signing != "enabled" {
		return fmt.Errorf("%w: git.signing must be inherit, disabled or enabled", ErrInvalid)
	}
	return nil
}

// FileStatus uses Git's porcelain status letters. Paths are repository-relative
// literal paths, not pathspec patterns. OriginalPath is present for renames.
type FileStatus struct {
	Path         string `json:"path"`
	OriginalPath string `json:"originalPath,omitempty"`
	Index        string `json:"index"`
	Worktree     string `json:"worktree"`
	Untracked    bool   `json:"untracked,omitempty"`
	Conflicted   bool   `json:"conflicted,omitempty"`
}

type Remote struct {
	Name     string `json:"name"`
	FetchURL string `json:"fetchUrl,omitempty"`
	PushURL  string `json:"pushUrl,omitempty"`
}

type RepositoryStatus struct {
	Root      string       `json:"root"`
	Head      string       `json:"head,omitempty"`
	Branch    string       `json:"branch,omitempty"`
	Detached  bool         `json:"detached"`
	Unborn    bool         `json:"unborn"`
	Upstream  string       `json:"upstream,omitempty"`
	Ahead     int          `json:"ahead"`
	Behind    int          `json:"behind"`
	Files     []FileStatus `json:"files"`
	Remotes   []Remote     `json:"remotes"`
	Truncated bool         `json:"truncated"`
}

type FileDiffParams struct {
	Path     string `json:"path,omitempty"`
	Staged   bool   `json:"staged,omitempty"`
	MaxBytes int64  `json:"maxBytes,omitempty"`
}
type FileDiff struct {
	Patch         string `json:"patch"`
	Truncated     bool   `json:"truncated"`
	EncodingLossy bool   `json:"encodingLossy,omitempty"`
}

type Branch struct {
	Name     string `json:"name"`
	Commit   string `json:"commit"`
	Current  bool   `json:"current"`
	Remote   bool   `json:"remote"`
	Upstream string `json:"upstream,omitempty"`
	Worktree string `json:"worktree,omitempty"`
}
type BranchesResult struct {
	Branches  []Branch `json:"branches"`
	Truncated bool     `json:"truncated"`
}
type LogParams struct {
	Ref   string `json:"ref,omitempty"`
	Limit int    `json:"limit,omitempty"`
}
type CommitInfo struct {
	ID          string   `json:"id"`
	Parents     []string `json:"parents"`
	Subject     string   `json:"subject"`
	AuthorName  string   `json:"authorName"`
	AuthorEmail string   `json:"authorEmail"`
	Time        string   `json:"time"`
}
type LogResult struct {
	Commits   []CommitInfo `json:"commits"`
	Truncated bool         `json:"truncated"`
}
type PathsParams struct {
	Paths []string `json:"paths"`
}
type CommitParams struct {
	Message string `json:"message"`
}
type CommitResult struct {
	Commit string `json:"commit"`
}
type CheckoutParams struct {
	Branch string `json:"branch"`
	Create bool   `json:"create,omitempty"`
	Base   string `json:"base,omitempty"`
}

// RemoteParams chooses a configured remote by name. Empty fields use Git's
// configured upstream. Push never forces and Pull only fast-forwards.
type RemoteParams struct {
	Remote      string `json:"remote,omitempty"`
	Branch      string `json:"branch,omitempty"`
	SetUpstream bool   `json:"setUpstream,omitempty"`
}
type RemoteResult struct {
	Output    string `json:"output"`
	Truncated bool   `json:"truncated"`
}

// SourceControl is an optional capability of a Backend. It operates on an
// already resolved binding and must revalidate its repository identity.
// Implementations bound output, honor cancellation and serialize conflicting
// operations. Mutation policy is enforced by both the manager and adapter.
type SourceControl interface {
	// Resolve only inspects identity and ownership. It must not create a
	// worktree or wait for a long-running mutation; operations revalidate it.
	Resolve(context.Context, string, string, *Binding) (Binding, error)
	Status(context.Context, Binding, GitSettings) (RepositoryStatus, error)
	WorkingDiff(context.Context, Binding, FileDiffParams, GitSettings) (FileDiff, error)
	Branches(context.Context, Binding, GitSettings) (BranchesResult, error)
	Log(context.Context, Binding, LogParams, GitSettings) (LogResult, error)
	Stage(context.Context, Binding, PathsParams, GitSettings) error
	Unstage(context.Context, Binding, PathsParams, GitSettings) error
	Commit(context.Context, Binding, CommitParams, GitSettings) (CommitResult, error)
	Checkout(context.Context, Binding, CheckoutParams, GitSettings) error
	Fetch(context.Context, Binding, RemoteParams, GitSettings) (RemoteResult, error)
	Pull(context.Context, Binding, RemoteParams, GitSettings) (RemoteResult, error)
	Push(context.Context, Binding, RemoteParams, GitSettings) (RemoteResult, error)
}
