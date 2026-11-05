// Package event defines Harness Kit's runtime-neutral event model and the
// in-process plumbing to record and fan out events.
//
// Every runtime adapter translates its native stream (Codex notifications,
// Claude stream-json lines, ...) into these events. Each event belongs to a
// session and carries a per-session, strictly increasing sequence number,
// which is what makes ordering, replay and gap recovery possible across any
// transport.
//
// Consumers must ignore event types they do not recognise: new types may be
// added in minor releases.
package event

import (
	"encoding/json"
	"time"

	"github.com/tomanagle/harness-kit/core/config"
	"github.com/tomanagle/harness-kit/core/permission"
	"github.com/tomanagle/harness-kit/core/workspace"
)

// Type identifies an event.
type Type string

const (
	SessionStarted     Type = "session.started"
	ContextUsage       Type = "context.usage"
	ContextCompaction  Type = "context.compaction"
	WorkspaceDiff      Type = "workspace.diff"
	SessionUpdated     Type = "session.updated"
	SessionClosed      Type = "session.closed"
	TurnStarted        Type = "turn.started"
	TurnCompleted      Type = "turn.completed"
	MessageDelta       Type = "message.delta"
	MessageCompleted   Type = "message.completed"
	ReasoningDelta     Type = "reasoning.delta"
	ReasoningCompleted Type = "reasoning.completed"
	ToolStarted        Type = "tool.started"
	ToolOutput         Type = "tool.output"
	ToolCompleted      Type = "tool.completed"
	SubagentUpdated    Type = "subagent.updated"
	ApprovalRequested  Type = "approval.requested"
	ApprovalResolved   Type = "approval.resolved"
	Usage              Type = "usage"
	BudgetExceeded     Type = "budget.exceeded"
	RuntimeStatus      Type = "runtime.status"
	Error              Type = "error"
	// RuntimeRaw carries an unmapped native runtime message. Only emitted
	// when a session opts in to raw events.
	RuntimeRaw Type = "runtime.raw"
	// StreamGap is a delivery marker, never stored in a session log: the
	// subscriber fell behind and events FromSeq..ToSeq of the session were
	// dropped for it. Recover them from the session's event log.
	StreamGap Type = "stream.gap"
)

// Event is the envelope for everything a session emits.
type Event struct {
	Seq       uint64    `json:"seq"`
	SessionID string    `json:"sessionId"`
	TurnID    string    `json:"turnId,omitempty"`
	Type      Type      `json:"type"`
	Time      time.Time `json:"time"`
	// Data holds the typed payload for Type (see the *Data types). After
	// decoding from JSON, known types decode to their payload struct and
	// unknown types to json.RawMessage.
	Data any `json:"data,omitempty"`
}

// UnmarshalJSON decodes Data into the payload type registered for Type.
func (e *Event) UnmarshalJSON(b []byte) error {
	type plain Event
	var raw struct {
		plain
		Data json.RawMessage `json:"data,omitempty"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*e = Event(raw.plain)
	e.Data = nil
	if len(raw.Data) == 0 || string(raw.Data) == "null" {
		return nil
	}
	newData, ok := payloadTypes[e.Type]
	if !ok {
		e.Data = raw.Data
		return nil
	}
	v, err := newData(raw.Data)
	if err != nil {
		return err
	}
	e.Data = v
	return nil
}

func decodeAs[T any](b json.RawMessage) (any, error) {
	var v T
	err := json.Unmarshal(b, &v)
	return v, err
}

var payloadTypes = map[Type]func(json.RawMessage) (any, error){
	ContextUsage:       decodeAs[ContextUsageData],
	ContextCompaction:  decodeAs[ContextCompactionData],
	WorkspaceDiff:      decodeAs[workspace.Diff],
	SessionStarted:     decodeAs[SessionStartedData],
	SessionUpdated:     decodeAs[SessionUpdatedData],
	SessionClosed:      decodeAs[SessionClosedData],
	TurnStarted:        decodeAs[TurnStartedData],
	TurnCompleted:      decodeAs[TurnCompletedData],
	MessageDelta:       decodeAs[MessageDeltaData],
	MessageCompleted:   decodeAs[MessageCompletedData],
	ReasoningDelta:     decodeAs[ReasoningDeltaData],
	ReasoningCompleted: decodeAs[ReasoningCompletedData],
	ToolStarted:        decodeAs[ToolStartedData],
	ToolOutput:         decodeAs[ToolOutputData],
	ToolCompleted:      decodeAs[ToolCompletedData],
	SubagentUpdated:    decodeAs[SubagentUpdatedData],
	ApprovalRequested:  decodeAs[permission.Request],
	ApprovalResolved:   decodeAs[ApprovalResolvedData],
	Usage:              decodeAs[UsageData],
	BudgetExceeded:     decodeAs[BudgetExceededData],
	RuntimeStatus:      decodeAs[RuntimeStatusData],
	Error:              decodeAs[ErrorData],
	RuntimeRaw:         decodeAs[RawData],
	StreamGap:          decodeAs[StreamGapData],
}

// ErrorInfo describes a failure.
type ErrorInfo struct {
	// Code is a stable, machine-readable identifier (e.g. "runtime_exited").
	Code      string `json:"code,omitempty"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable,omitempty"`
}

// SessionStartedData is the payload of session.started.
type SessionStartedData struct {
	Runtime          string `json:"runtime"`
	RuntimeSessionID string `json:"runtimeSessionId,omitempty"`
	Resumed          bool   `json:"resumed,omitempty"`
	Model            string `json:"model,omitempty"`
}

// SessionUpdatedData is a snapshot of the mutable session fields. Optional
// fields that are absent have been cleared, rather than left unchanged.
// State uses the session lifecycle names (idle, running, closing, ...).
type SessionUpdatedData struct {
	Context     *ContextUsageData          `json:"context,omitempty"`
	Compaction  *config.CompactionSettings `json:"compaction,omitempty"`
	Budget      *config.BudgetLimits       `json:"budget,omitempty"`
	Usage       *BudgetUsage               `json:"usage,omitempty"`
	Archived    bool                       `json:"archived,omitempty"`
	Metadata    map[string]string          `json:"metadata,omitempty"`
	ForkPending bool                       `json:"forkPending,omitempty"`
	// SettingsRevision changes only when session settings or pinning change.
	SettingsRevision uint64 `json:"settingsRevision,omitempty"`
	Pinned           bool   `json:"pinned,omitempty"`
	// Config contains current session defaults, never a one-turn override.
	Config           *config.Settings `json:"config,omitempty"`
	Title            string           `json:"title"`
	State            string           `json:"state"`
	ActiveTurnID     string           `json:"activeTurnId,omitempty"`
	RuntimeSessionID string           `json:"runtimeSessionId,omitempty"`
	Turns            int              `json:"turns"`
	LastStatus       TurnStatus       `json:"lastStatus,omitempty"`
	UpdatedAt        time.Time        `json:"updatedAt"`
	Error            *ErrorInfo       `json:"error,omitempty"`
}

// SessionClosedData is the payload of session.closed.
type SessionClosedData struct {
	Reason string     `json:"reason,omitempty"` // "closed", "failed", "shutdown"
	Error  *ErrorInfo `json:"error,omitempty"`
}

// TurnStartedData is the payload of turn.started.
type TurnStartedData struct {
	Workspace *workspace.Settings  `json:"workspace,omitempty"`
	Budget    *config.BudgetLimits `json:"budget,omitempty"`
	// Config is the accepted effective turn selection after native resolution.
	Config *config.Settings `json:"config,omitempty"`
	// Text is the concatenated text of the prompt input.
	Text string `json:"text,omitempty"`
	// Input preserves content order and local image references for replay.
	// Image bytes are not copied into the transcript.
	Input []InputPart `json:"input,omitempty"`
}

// InputPart is a text part or a local image reference in a prompt.
type InputPart struct {
	Type string `json:"type"` // "text" or "image"
	Text string `json:"text,omitempty"`
	Path string `json:"path,omitempty"`
}

// TurnStatus is the final status of a turn.
type TurnStatus string

const (
	TurnStatusCompleted TurnStatus = "completed"
	TurnStatusCancelled TurnStatus = "cancelled"
	TurnStatusFailed    TurnStatus = "failed"
)

// TurnCompletedData is the payload of turn.completed. Exactly one
// turn.completed is emitted for every turn.started.
type TurnCompletedData struct {
	Budget        *BudgetExceededData `json:"budget,omitempty"`
	Status        TurnStatus          `json:"status"`
	Error         *ErrorInfo          `json:"error,omitempty"`
	Usage         *UsageData          `json:"usage,omitempty"`
	RuntimeTurnID string              `json:"runtimeTurnId,omitempty"`
}

// MessageDeltaData is an incremental chunk of an agent message.
type MessageDeltaData struct {
	ItemID string `json:"itemId"`
	Text   string `json:"text"`
	// ParentCallID links messages from subagents to the tool call that
	// spawned them.
	ParentCallID string `json:"parentCallId,omitempty"`
}

// MessageCompletedData is a complete agent message.
type MessageCompletedData struct {
	ItemID       string `json:"itemId"`
	Role         string `json:"role"` // "assistant"
	Text         string `json:"text"`
	ParentCallID string `json:"parentCallId,omitempty"`
}

// ReasoningDeltaData is an incremental chunk of reasoning text or summary.
type ReasoningDeltaData struct {
	ItemID string `json:"itemId"`
	Text   string `json:"text"`
}

// ReasoningCompletedData is a complete reasoning block.
type ReasoningCompletedData struct {
	ItemID string `json:"itemId"`
	Text   string `json:"text"`
}

// ToolKind classifies tool calls.
type ToolKind string

const (
	ToolKindCommand    ToolKind = "command"
	ToolKindFileChange ToolKind = "file_change"
	ToolKindFileRead   ToolKind = "file_read"
	ToolKindSearch     ToolKind = "search"
	ToolKindWebSearch  ToolKind = "web_search"
	ToolKindMCP        ToolKind = "mcp"
	ToolKindSubagent   ToolKind = "subagent"
	ToolKindOther      ToolKind = "other"
)

// ToolStartedData is the payload of tool.started.
type ToolStartedData struct {
	CallID       string          `json:"callId"`
	Name         string          `json:"name"`
	Kind         ToolKind        `json:"kind"`
	Title        string          `json:"title,omitempty"`
	Input        json.RawMessage `json:"input,omitempty"`
	ParentCallID string          `json:"parentCallId,omitempty"`
}

// ToolOutputData is incremental tool output (e.g. command stdout).
type ToolOutputData struct {
	CallID string `json:"callId"`
	Output string `json:"output"`
}

// ToolStatus is the final status of a tool call.
type ToolStatus string

const (
	ToolStatusCompleted ToolStatus = "completed"
	ToolStatusFailed    ToolStatus = "failed"
	ToolStatusDeclined  ToolStatus = "declined"
)

// ToolCompletedData is the payload of tool.completed.
type ToolCompletedData struct {
	CallID   string     `json:"callId"`
	Name     string     `json:"name,omitempty"`
	Status   ToolStatus `json:"status"`
	Output   string     `json:"output,omitempty"`
	ExitCode *int       `json:"exitCode,omitempty"`
	Error    string     `json:"error,omitempty"`
}

// SubagentStatus is the last observed lifecycle state of a delegated agent.
type SubagentStatus string

const (
	SubagentRunning   SubagentStatus = "running"
	SubagentCompleted SubagentStatus = "completed"
	SubagentFailed    SubagentStatus = "failed"
	SubagentCancelled SubagentStatus = "cancelled"
	SubagentUnknown   SubagentStatus = "unknown"
)

// SubagentUpdatedData reports delegated work independently of the spawning tool.
// A completed spawn/wait tool does not imply a completed agent. Updates are
// observations, not independently controllable Harness Kit sessions. Optional
// descriptive fields enrich earlier observations when present.
type SubagentUpdatedData struct {
	// ID is the runtime's child identifier, or the spawn call ID until known.
	// Consumers reconcile a provisional ID with a later ID using CallID.
	ID       string `json:"id"`
	CallID   string `json:"callId,omitempty"`
	ParentID string `json:"parentId,omitempty"`
	// RunID distinguishes resumed runs when the runtime supplies one.
	RunID      string         `json:"runId,omitempty"`
	Title      string         `json:"title,omitempty"`
	Prompt     string         `json:"prompt,omitempty"`
	Status     SubagentStatus `json:"status"`
	Output     string         `json:"output,omitempty"`
	Error      string         `json:"error,omitempty"`
	Background bool           `json:"background,omitempty"`
}

// ApprovalResolvedData is the payload of approval.resolved.
type ApprovalResolvedData struct {
	ApprovalID string              `json:"approvalId"`
	Decision   permission.Decision `json:"decision"`
	Source     permission.Source   `json:"source"`
	// Request is included when the request was decided without being
	// announced (Source == policy), so observers can audit it.
	Request *permission.Request `json:"request,omitempty"`
}

// UsageData reports token usage and, where the runtime provides it, cost.
type UsageData struct {
	// Mode is "snapshot" (default) or "delta". Snapshots are cumulative within
	// Scope and repeated snapshots are not additional usage. Deltas are increments.
	Mode string `json:"mode,omitempty"`
	// TokensKnown distinguishes a reported zero from unknown token accounting.
	// ContextWindow is never a token-usage measurement.
	TokensKnown bool `json:"tokensKnown,omitempty"`
	// Scope is "turn" (this turn only) or "session" (cumulative).
	Scope             string `json:"scope"`
	InputTokens       int64  `json:"inputTokens,omitempty"`
	OutputTokens      int64  `json:"outputTokens,omitempty"`
	CachedInputTokens int64  `json:"cachedInputTokens,omitempty"`
	CacheWriteTokens  int64  `json:"cacheWriteTokens,omitempty"`
	ReasoningTokens   int64  `json:"reasoningTokens,omitempty"`
	// TotalTokens is the normalized consumed-token total, including cache reads
	// and writes where they are separate. Cached/reasoning counts may be subsets
	// of input/output; consumers must not add those fields to TotalTokens.
	TotalTokens   int64 `json:"totalTokens,omitempty"`
	ContextWindow int64 `json:"contextWindow,omitempty"`
	// CostUSD is the runtime's own estimate, not a bill.
	CostUSD *float64 `json:"costUsd,omitempty"`
}

// RuntimeStatusData reports runtime-level conditions (retries, rate limits,
// warnings) that do not end the turn.
type RuntimeStatusData struct {
	Status  string          `json:"status"`
	Message string          `json:"message,omitempty"`
	Detail  json.RawMessage `json:"detail,omitempty"`
}

// ErrorData is a non-fatal error. Fatal turn errors are reported in
// turn.completed.
type ErrorData = ErrorInfo

// RawData wraps an unmapped native runtime message.
type RawData struct {
	Runtime string          `json:"runtime"`
	Kind    string          `json:"kind"`
	Payload json.RawMessage `json:"payload"`
}

// StreamGapData is the payload of stream.gap.
type StreamGapData struct {
	FromSeq uint64 `json:"fromSeq"`
	ToSeq   uint64 `json:"toSeq"`
}
