// Package workspace defines portable workspace settings and snapshot primitives.
// It performs no filesystem or process operations; hosts inject a Backend.
package workspace

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrUnsupported = errors.New("workspace: backend unavailable")
	ErrInvalid     = errors.New("workspace: invalid configuration")
	ErrNotOwned    = errors.New("workspace: worktree is not owned by this session")
	ErrDirty       = errors.New("workspace: worktree has changes")
	ErrLimit       = errors.New("workspace: snapshot limit exceeded")
	ErrNotFound    = errors.New("workspace: turn diff not found")
)

// Settings is the workspace group in the kit's single settings document.
// Location fields apply to new sessions; capture policy applies to future turns.
type Settings struct {
	Mode             string `json:"mode"` // "existing" (default) or "worktree"
	Repo             string `json:"repo"`
	Base             string `json:"base"`
	Path             string `json:"path"`
	Root             string `json:"root"`
	CaptureDiff      bool   `json:"captureDiff"`
	MaxDiffBytes     int64  `json:"maxDiffBytes"`
	MaxSnapshotBytes int64  `json:"maxSnapshotBytes"`
	MaxFiles         int    `json:"maxFiles"`
	TimeoutMs        int64  `json:"timeoutMs"`
}

func Defaults() Settings {
	return Settings{Mode: "existing", MaxDiffBytes: 256 << 10, MaxSnapshotBytes: 64 << 20, MaxFiles: 10000, TimeoutMs: 30000}
}

func Validate(s Settings) error {
	if s.Mode != "" && s.Mode != "existing" && s.Mode != "worktree" {
		return fmt.Errorf("%w: mode must be existing or worktree", ErrInvalid)
	}
	if s.MaxDiffBytes < 1 || s.MaxSnapshotBytes < 1 || s.MaxFiles < 1 || s.TimeoutMs < 1 {
		return fmt.Errorf("%w: workspace limits must be positive", ErrInvalid)
	}
	if s.Mode == "worktree" && s.Path == "" && s.Root == "" {
		return fmt.Errorf("%w: worktree mode needs an explicit path or root", ErrInvalid)
	}
	if s.Path != "" && s.Root != "" {
		return fmt.Errorf("%w: choose path or root, not both", ErrInvalid)
	}
	if s.Mode != "worktree" && (s.Repo != "" || s.Base != "" || s.Path != "" || s.Root != "") {
		return fmt.Errorf("%w: repo, base, path and root require worktree mode", ErrInvalid)
	}
	return nil
}

// SameLocation reports whether applying b can preserve the running workspace.
func SameLocation(a, b Settings) bool {
	return a.Mode == b.Mode && a.Repo == b.Repo && a.Base == b.Base && a.Path == b.Path && a.Root == b.Root
}

// Binding is durable identity data. Owned means the backend created a dedicated
// worktree for SessionID; it is not permission to delete dirty files.
type Binding struct {
	Kind       string `json:"kind"`
	SessionID  string `json:"sessionId"`
	Repo       string `json:"repo"`
	Path       string `json:"path"`
	GitDir     string `json:"gitDir"`
	CommonDir  string `json:"commonDir"`
	BaseCommit string `json:"baseCommit,omitempty"`
	Owned      bool   `json:"owned"`
}

// Snapshot references durable content objects in the repository. Capture does
// not change the user's index, HEAD or worktree contents.
type Snapshot struct {
	Tree       string    `json:"tree"`
	Index      string    `json:"index"`
	Head       string    `json:"head,omitempty"`
	CapturedAt time.Time `json:"capturedAt"`
	Files      int       `json:"files"`
	Bytes      int64     `json:"bytes"`
}

// TurnChange is stored as a workspace.diff event, including failed captures.
// Complete describes the two boundary snapshots, not attribution to one agent.
type TurnChange struct {
	SessionID    string    `json:"sessionId"`
	TurnID       string    `json:"turnId"`
	Binding      Binding   `json:"binding"`
	Before       *Snapshot `json:"before,omitempty"`
	After        *Snapshot `json:"after,omitempty"`
	Complete     bool      `json:"complete"`
	Error        string    `json:"error,omitempty"`
	MaxDiffBytes int64     `json:"maxDiffBytes"`
}

type DiffOptions struct {
	MaxBytes int64 `json:"maxBytes,omitempty"`
}

// Diff compares content at turn boundaries. WorkingTree includes nonignored
// untracked files; Index separately shows changes to staged content.
type Diff struct {
	TurnChange
	WorkingTree   string `json:"workingTree"`
	Index         string `json:"index"`
	Truncated     bool   `json:"truncated"`
	EncodingLossy bool   `json:"encodingLossy,omitempty"`
}

type Backend interface {
	// Prepare creates a workspace or validates the exact existing binding.
	// Restoring must never silently recreate a missing worktree.
	Prepare(context.Context, string, string, Settings, *Binding) (Binding, error)
	Capture(context.Context, Binding, string, string, Settings) (Snapshot, error)
	Diff(context.Context, TurnChange, DiffOptions) (Diff, error)
	// Remove requires verifiable ownership and a clean worktree. No force mode.
	Remove(context.Context, Binding) error
}

// SnapshotReleaser removes temporary retention references after the bounded
// diff has been saved. It must never delete working files or user branches.
type SnapshotReleaser interface {
	ReleaseSnapshots(context.Context, Binding, string) error
}

// BoundDiff makes a saved result safe for a tighter request limit. The limit
// counts UTF-8 patch bytes across both views, excluding metadata/JSON escaping.
func BoundDiff(diff Diff, limit int64) Diff {
	if limit < 0 {
		limit = 0
	}
	for _, value := range []*string{&diff.WorkingTree, &diff.Index} {
		if !utf8.ValidString(*value) {
			*value = strings.ToValidUTF8(*value, "�")
			diff.EncodingLossy = true
		}
		if int64(len(*value)) > limit {
			n := int(limit)
			for n > 0 && !utf8.RuneStart((*value)[n]) {
				n--
			}
			*value = strings.Clone((*value)[:n])
			diff.Truncated = true
		}
		limit -= int64(len(*value))
	}
	return diff
}
