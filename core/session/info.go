package session

import (
	"time"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/config"
	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/workspace"
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
	StateCompacting State = "compacting"
	StateClosing    State = "closing"
	StateClosed     State = "closed"
	StateFailed     State = "failed"
)

// Terminal reports whether no further turns are possible.
func (s State) Terminal() bool { return s == StateClosed || s == StateFailed }

// Info describes a session.
type Info struct {
	Context           *event.ContextUsageData   `json:"context,omitempty"`
	Compaction        config.CompactionSettings `json:"compaction"`
	Workspace         *workspace.Binding        `json:"workspace,omitempty"`
	WorkspaceSettings workspace.Settings        `json:"workspaceSettings"`
	Budget            config.BudgetLimits       `json:"budget"`
	Usage             event.BudgetUsage         `json:"usage"`
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
