// Package protocol defines the Harness Kit sidecar protocol: JSON-RPC 2.0
// messages, one per line, over the sidecar's stdin/stdout. It is shared by
// the Go server and legacy Go client. The standalone Go and TypeScript client
// wire contracts are generated from these types and the referenced domain types.
//
// See docs/protocol.md for the normative description.
package protocol

//go:generate go run ../../cmd/protocolgen

import (
	"encoding/json"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/mcp"
	"github.com/tomanagle/harness-kit/core/permission"
	"github.com/tomanagle/harness-kit/core/session"
	"github.com/tomanagle/harness-kit/core/settings"
	"github.com/tomanagle/harness-kit/core/workspace"
)

// Version is the protocol version implemented by this package. It is a
// single integer incremented only on breaking changes; additive changes
// (new methods, new optional fields, new event types) keep the version.
const Version = 1

// Method names.
const (
	MethodInitialize        = "initialize"
	MethodShutdown          = "shutdown"
	MethodSettingsGet       = "settings.get"
	MethodSettingsConfigure = "settings.configure"
	MethodSettingsStats     = "settings.stats"
	MethodSessionEventsPage = "session.eventsPage"
	MethodSessionEvict      = "session.evict"
	MethodSessionSuspend    = "session.suspend"
	MethodSessionPin        = "session.pin"
	MethodRuntimeList       = "runtime.list"
	MethodRuntimeDiscover   = "runtime.discover"
	MethodRuntimeSessions   = "runtime.sessions"
	MethodSessionCreate     = "session.create"
	MethodSessionGet        = "session.get"
	MethodSessionDiff       = "session.diff"
	MethodWorkspaceRemove   = "workspace.remove"
	MethodSessionList       = "session.list"
	MethodSessionPrompt     = "session.prompt"
	MethodSessionCancel     = "session.cancel"
	MethodSessionClose      = "session.close"
	MethodSessionEvents     = "session.events"
	MethodSessionRestore    = "session.restore"
	MethodSessionDelete     = "session.delete"
	MethodSessionRename     = "session.rename"
	MethodSessionFork       = "session.fork"
	MethodSessionArchive    = "session.archive"
	MethodSessionTags       = "session.tags"
	MethodSessionConfigure  = "session.configure"
	MethodApprovalRespond   = "approval.respond"
	MethodApprovalList      = "approval.list"
	MethodToolsRegister     = "tools.register"
	MethodToolsUnregister   = "tools.unregister"
	MethodToolsList         = "tools.list"
	MethodMCPList           = "mcp.list"

	// RequestToolCall (server -> client request) runs a tool the client
	// registered. Params: ToolCallParams; result: tools.Result. The server
	// sends $/cancelRequest if the call is abandoned (e.g. the turn was
	// cancelled).
	RequestToolCall = "tool.call"

	// NotificationEvent (server -> client) carries one event.Event.
	NotificationEvent           = "event"
	NotificationSettingsUpdated = "settings.updated"
	// NotificationCancelRequest (client -> server) cancels an in-flight
	// request: {"id": <request id>}.
	NotificationCancelRequest = "$/cancelRequest"
)

// Error codes. JSON-RPC reserves -32768..-32000; codes -32099..-32000 are
// for implementation-defined server errors. Every Harness Kit error also
// carries data.reason, a stable string clients should switch on.
const (
	CodeServerNotInitialized = -32002
	CodeRequestCancelled     = -32800

	CodeSessionNotFound    = -32001
	CodeRuntimeNotFound    = -32003
	CodeRuntimeUnavailable = -32004
	CodeSessionBusy        = -32005
	CodeSessionClosed      = -32006
	CodeApprovalNotFound   = -32007
	CodeUnsupportedVersion = -32008
	CodePolicyViolation    = -32009
	CodeInvalidDecision    = -32010
	CodeRuntimeError       = -32011
	CodeUnsupported        = -32012
	CodeNotResumable       = -32013
	CodeSettingsConflict   = -32014
	CodeRestartRequired    = -32015
	CodeNewSessionRequired = -32016
	CodeEventTooLarge      = -32017
	CodeForkSourceChanged  = -32018
	CodeBudgetExceeded     = -32019
)

// Reason strings for ErrorData.Reason.
const (
	ReasonNotInitialized     = "not_initialized"
	ReasonSessionNotFound    = "session_not_found"
	ReasonRuntimeNotFound    = "runtime_not_found"
	ReasonRuntimeUnavailable = "runtime_unavailable"
	ReasonSessionBusy        = "session_busy"
	ReasonSessionClosed      = "session_closed"
	ReasonApprovalNotFound   = "approval_not_found"
	ReasonUnsupportedVersion = "unsupported_version"
	ReasonPolicyViolation    = "policy_violation"
	ReasonInvalidDecision    = "invalid_decision"
	ReasonRuntimeError       = "runtime_error"
	ReasonDuplicateTurn      = "duplicate_turn"
	ReasonInvalidParams      = "invalid_params"
	ReasonUnsupported        = "unsupported"
	ReasonDuplicateTool      = "duplicate_tool"
	ReasonNotResumable       = "not_resumable"
	ReasonSettingsConflict   = "settings_conflict"
	ReasonRestartRequired    = "restart_required"
	ReasonNewSessionRequired = "new_session_required"
	ReasonEventTooLarge      = "event_too_large"
	ReasonForkSourceChanged  = "fork_source_changed"
	ReasonBudgetExceeded     = "budget_exceeded"
)

// ErrorData is the "data" member of Harness Kit errors.
type ErrorData struct {
	Reason            string `json:"reason"`
	SupportedVersions []int  `json:"supportedVersions,omitempty"`
}

// ClientInfo identifies the client.
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// InitializeParams are the params of "initialize".
type InitializeParams struct {
	ProtocolVersion int        `json:"protocolVersion"`
	ClientInfo      ClientInfo `json:"clientInfo"`
}

// ServerInfo identifies the server.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ServerCapabilities describe what the server offers.
type ServerCapabilities struct {
	Workspace bool         `json:"workspace"`
	Runtimes  []agent.Info `json:"runtimes"`
	// AllowUnsafe reports whether the sidecar accepts session options that
	// disable runtime sandboxes or approvals.
	AllowUnsafe bool `json:"allowUnsafe"`
	// EventLogSize is the per-session replay window.
	EventLogSize int `json:"eventLogSize"`
	// Tools reports whether clients can register host tools.
	Tools bool `json:"tools"`
	// Settings advertises the unified live settings API.
	Settings bool `json:"settings"`
}

// SessionDiffParams reads a saved, bounded before/after turn diff.
type SessionDiffParams struct {
	SessionID string `json:"sessionId"`
	TurnID    string `json:"turnId"`
	workspace.DiffOptions
}

// InitializeResult is the result of "initialize".
type InitializeResult struct {
	ProtocolVersion int                `json:"protocolVersion"`
	ServerInfo      ServerInfo         `json:"serverInfo"`
	Capabilities    ServerCapabilities `json:"capabilities"`
}

// RuntimeListResult is the result of "runtime.list".
type RuntimeListResult struct {
	Runtimes []agent.Info `json:"runtimes"`
}

// RuntimeDiscoverParams are the params of "runtime.discover".
type RuntimeDiscoverParams struct {
	// Runtimes limits discovery to these names; empty means all.
	Runtimes []string `json:"runtimes,omitempty"`
}

// RuntimeDiscoverResult is the result of "runtime.discover".
type RuntimeDiscoverResult struct {
	Runtimes []agent.Discovery `json:"runtimes"`
}

// RuntimeSessionsParams are the params of "runtime.sessions".
type RuntimeSessionsParams struct {
	Runtime string `json:"runtime"`
	Cwd     string `json:"cwd,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

// RuntimeSessionsResult is the result of "runtime.sessions".
type RuntimeSessionsResult struct {
	Sessions []agent.RuntimeSession `json:"sessions"`
}

// SessionCreateParams are the params of "session.create".
type SessionCreateParams struct {
	// Settings uses the shared schema. Only the session section is accepted here.
	Settings     settings.Patch    `json:"settings,omitempty"`
	Runtime      string            `json:"runtime,omitempty"`
	Cwd          string            `json:"cwd,omitempty"`
	Model        string            `json:"model,omitempty"`
	Effort       string            `json:"effort,omitempty"`
	ResumeID     string            `json:"resumeId,omitempty"`
	Instructions string            `json:"instructions,omitempty"`
	RawEvents    bool              `json:"rawEvents,omitempty"`
	Options      json.RawMessage   `json:"options,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
	// Title names the session; empty means the start of the first prompt.
	Title string `json:"title,omitempty"`
	// MCP adds MCP servers for this session (portable format; see the mcp
	// package). A sidecar started with --kit-mcp also merges the user and
	// trusted project config files.
	MCP *mcp.Config `json:"mcp,omitempty"`
	// HostTools limits the session to these registered host tools (default
	// all); DisableHostTools gives it none, and no Harness Kit bridge.
	HostTools        []string `json:"hostTools,omitempty"`
	DisableHostTools bool     `json:"disableHostTools,omitempty"`
}

// SettingsGetParams select host settings or a session's settings.
type SettingsGetParams struct {
	Target settings.Target `json:"target,omitempty"`
}

// SessionPinParams protect an idle session from automatic resource eviction.
type SessionPinParams struct {
	SessionID string `json:"sessionId"`
	Pinned    bool   `json:"pinned"`
}

// SessionResult wraps a session.Info.
type SessionResult struct {
	Session session.Info `json:"session"`
}

// SessionIDParams identify a session.
type SessionIDParams struct {
	SessionID string `json:"sessionId"`
}

// SessionRenameParams are the params of "session.rename". The result is a
// SessionResult.
type SessionRenameParams struct {
	SessionID string `json:"sessionId"`
	Title     string `json:"title"`
}

// SessionListParams filters the catalog; omitted archived includes both states.
type SessionListParams = session.ListOptions

type SessionForkParams struct {
	SessionID string `json:"sessionId"`
	session.ForkParams
}
type SessionArchiveParams struct {
	SessionID string `json:"sessionId"`
	Archived  bool   `json:"archived"`
}
type SessionTagsParams struct {
	SessionID string   `json:"sessionId"`
	Tags      []string `json:"tags"`
}

// SessionConfigureParams update defaults for subsequent turns. Config is
// required; an empty object is a no-op. Omitted fields are unchanged.
type SessionConfigureParams struct {
	SessionID string             `json:"sessionId"`
	Config    *agent.ConfigPatch `json:"config"`
}

// SessionListResult is the result of "session.list".
type SessionListResult struct {
	Sessions []session.Info `json:"sessions"`
}

// SessionPromptParams are the params of "session.prompt".
type SessionPromptParams struct {
	SessionID string `json:"sessionId"`
	// TurnID is optional; clients should generate it (e.g. a UUID) so they
	// can correlate events that arrive before the response.
	TurnID   string             `json:"turnId,omitempty"`
	Input    []agent.InputPart  `json:"input"`
	Config   *agent.ConfigPatch `json:"config,omitempty"`
	Settings settings.Patch     `json:"settings,omitempty"`
}

// SessionPromptResult is the result of "session.prompt". The turn runs
// asynchronously; follow it through "event" notifications.
type SessionPromptResult struct {
	TurnID string `json:"turnId"`
}

// SessionEventsParams are the params of "session.events".
type SessionEventsParams struct {
	SessionID string `json:"sessionId"`
	AfterSeq  uint64 `json:"afterSeq,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

// SessionEventsResult is the result of "session.events".
type SessionEventsResult struct {
	Events []event.Event `json:"events"`
	// Truncated reports that events after AfterSeq were evicted from the
	// replay window and cannot be recovered.
	Truncated bool `json:"truncated"`
	// NextSeq is the cursor for the next bounded page.
	NextSeq uint64 `json:"nextSeq"`
	HasMore bool   `json:"hasMore"`
}

// ApprovalRespondParams are the params of "approval.respond".
type ApprovalRespondParams struct {
	ApprovalID   string             `json:"approvalId"`
	Outcome      permission.Outcome `json:"outcome"`
	Message      string             `json:"message,omitempty"`
	UpdatedInput json.RawMessage    `json:"updatedInput,omitempty"`
}

// ApprovalListParams are the params of "approval.list".
type ApprovalListParams struct {
	SessionID string `json:"sessionId,omitempty"`
}

// ApprovalListResult is the result of "approval.list".
type ApprovalListResult struct {
	Approvals []permission.Request `json:"approvals"`
}

// MCPListParams are the params of "mcp.list".
type MCPListParams struct {
	// Cwd is where to look for a project harness-kit.json (default: the
	// server's working directory).
	Cwd string `json:"cwd,omitempty"`
	// Runtimes also lists the servers configured natively in these runtimes,
	// where supported (Codex).
	Runtimes []string `json:"runtimes,omitempty"`
}

// NativeMCP lists one runtime's own MCP servers.
type NativeMCP struct {
	Runtime   string   `json:"runtime"`
	Supported bool     `json:"supported"`
	Servers   []string `json:"servers"`
	Error     string   `json:"error,omitempty"`
}

// MCPListResult is the result of "mcp.list".
type MCPListResult struct {
	// Enabled reports whether this server merges kit config files into
	// sessions (serve --kit-mcp). When false, Servers is empty.
	Enabled bool `json:"enabled"`
	mcp.Overview
	Native []NativeMCP `json:"native"`
}

// ToolSpec describes a client-registered tool.
type ToolSpec struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// InputSchema is a JSON Schema for the arguments object (default
	// {"type":"object"}). Arguments are validated before tool.call.
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
	// Approval "ask" requests the host's approval before each call.
	Approval  mcp.Approval `json:"approval,omitempty"`
	ReadOnly  bool         `json:"readOnly,omitempty"`
	TimeoutMs int          `json:"timeoutMs,omitempty"`
}

// ToolsRegisterParams are the params of "tools.register".
type ToolsRegisterParams struct {
	Tools []ToolSpec `json:"tools"`
}

// ToolsUnregisterParams are the params of "tools.unregister".
type ToolsUnregisterParams struct {
	Names []string `json:"names"`
}

// ToolsListResult is the result of "tools.list".
type ToolsListResult struct {
	Tools []ToolSpec `json:"tools"`
}

// ToolCallParams are the params of the "tool.call" request.
type ToolCallParams struct {
	SessionID string          `json:"sessionId"`
	TurnID    string          `json:"turnId,omitempty"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Empty is the result of methods that return nothing.
type Empty struct{}
