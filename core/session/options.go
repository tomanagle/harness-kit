package session

import (
	"context"
	"log/slog"
	"time"

	"github.com/tomanagle/harness-kit/core/hooks"
	"github.com/tomanagle/harness-kit/core/logging"
	"github.com/tomanagle/harness-kit/core/mcp"
	"github.com/tomanagle/harness-kit/core/permission"
	"github.com/tomanagle/harness-kit/core/settings"
	"github.com/tomanagle/harness-kit/core/workspace"
)

// Options configure a Manager. The zero value is usable.
type Options struct {
	// SettingsStore optionally saves accepted global changes before publishing.
	// Hosts load it before building their process/runtime dependencies.
	SettingsStore settings.Store
	// Hooks runs application lifecycle handlers. Nil uses an empty registry.
	Hooks hooks.Registration
	// Workspace supplies optional native worktrees and turn snapshot support.
	// Nil leaves the portable manager usable without Git/process dependencies.
	Workspace workspace.Backend
	// LogLevel optionally shares a level with host handlers. The manager creates
	// one when omitted; settings.logging.level always controls the UI stream.
	LogLevel *slog.LevelVar
	// Logs optionally shares a structured stream with other host components.
	// When supplied, its level/configuration take precedence over LogLevel.
	// The caller owns its lifetime; otherwise the manager owns the stream.
	Logs *logging.Stream
	// ValidateConfig lets the host apply its own constraints at creation,
	// restore, session updates and turn admission (including resolved settings).
	// It must be safe for concurrent calls and must not call back into Manager.
	ValidateConfig func(context.Context, ConfigRequest) error
	// PrepareRestore explicitly chooses whether/how to reload external settings
	// on restore. Nil preserves saved configuration; Prepare remains creation-only.
	PrepareRestore func(context.Context, *CreateParams) ([]string, error)

	// Store persists session records. Defaults to a MemoryStore.
	Store Store
	// Policy decides approval requests before the host is asked.
	Policy permission.Policy
	// ApprovalTimeout bounds how long an approval may wait for the host.
	// When it elapses the request is denied. Zero waits indefinitely.
	ApprovalTimeout time.Duration
	// EventLogSize is the number of events retained per session for
	// replay in memory. An EventStore retains the full history on disk.
	// Defaults to 2048.
	EventLogSize int
	// MaxClosedSessions bounds how many closed sessions keep their event
	// logs in memory. Defaults to 64. Records remain in the Store.
	MaxClosedSessions int
	// StreamBuffer is the subscriber queue length used by Stream. Defaults
	// to event.DefaultBuffer. Slow consumers beyond it are recovered from
	// the session log.
	StreamBuffer int
	// Prepare, if set, can adjust a new session's configuration before the
	// runtime starts (for example to merge MCP servers from config files).
	// Returned warnings are recorded as runtime.status "warning" events.
	// It is not called by Restore, which reuses the stored configuration.
	Prepare func(ctx context.Context, p *CreateParams) (warnings []string, err error)
	// ToolServer, if set, serves host tools (and Claude Code's permission
	// prompts) to sessions whose runtime supports MCP. See package toolhost.
	ToolServer ToolServer
	// AllowUnsafe permits session configurations that a Guarded runtime
	// reports as disabling its sandbox or approvals.
	AllowUnsafe bool
	// Logger receives diagnostics in addition to the UI stream. Its handler's
	// own filtering is respected. Nil leaves diagnostics in the stream only.
	Logger *slog.Logger
}

// ToolServer gives runtime sessions access to host tools by serving them as
// an MCP server (package toolhost implements it).
type ToolServer interface {
	// Attach binds a session and returns the MCP server the runtime should
	// launch to reach it. An Attachment with an empty Name means there is
	// nothing to serve.
	Attach(ctx context.Context, b ToolBinding) (Attachment, error)
}

// ToolBinding describes a session to a ToolServer.
type ToolBinding struct {
	SessionID string
	Runtime   string
	// Tools limits the session to these host tools; nil means all.
	Tools []string
	// ApprovalPrompt also serves a permission-prompt tool.
	ApprovalPrompt bool
	// Approver asks the host (policy, then approval.requested).
	Approver func(ctx context.Context, req permission.Request) (permission.Decision, error)
	// CurrentTurn returns the active turn ID ("" between turns).
	CurrentTurn func() string
	// CancelTurn interrupts the active turn (for "abort" decisions).
	CancelTurn func()
}

// Attachment is the result of ToolServer.Attach.
type Attachment struct {
	Name   string
	Server mcp.Server
	// PermissionTool names the tool in Server that answers permission
	// prompts, when the binding asked for one.
	PermissionTool string
	Detach         func()
}
