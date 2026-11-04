// Package agent defines the AgentRuntime abstraction: an adapter that
// delegates execution to an existing, complete agent harness such as the
// Codex app-server or the Claude Code CLI.
//
// An agent runtime owns its own agent loop, tools, context window and model
// access. Harness Kit only drives it: it starts sessions, sends prompts,
// observes events and answers approval requests. Developers who want to
// build their own loop on raw model inference use the separate model
// package instead; the two abstractions are deliberately not conflated.
//
// The package is transport-neutral and does not start processes itself;
// adapters that spawn CLIs live in runtimes/*.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tomanagle/harness-kit/core/budget"
	"github.com/tomanagle/harness-kit/core/config"
	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/mcp"
	"github.com/tomanagle/harness-kit/core/permission"
)

// Runtime is implemented by agent runtime adapters.
type Runtime interface {
	// Info returns static metadata. It must not perform I/O.
	Info() Info
	// Discover probes the local machine: is the runtime installed, which
	// version, is it signed in. It must not start agent sessions or
	// consume model quota, and it must never read credential stores
	// directly; authentication status must come from the runtime's own
	// documented status command or API.
	Discover(ctx context.Context) (Discovery, error)
	// Start creates a new session, or resumes cfg.ResumeID. ctx bounds
	// startup only: the session (and any process it owns) must outlive it.
	// The Handler receives the session's events and approval requests for
	// the session's whole lifetime.
	Start(ctx context.Context, cfg SessionConfig, h Handler) (Session, error)
}

// Forker is an optional runtime extension that branches a complete native
// conversation without changing its source or running a synthetic prompt.
// Deferred implementations return an empty RuntimeSessionID until the first
// real prompt materializes the branch; advertise ForkDeferred in that case.
type Forker interface {
	Fork(ctx context.Context, sourceRuntimeID string, cfg SessionConfig, h Handler) (Session, error)
}

// Info is static runtime metadata.
type Info struct {
	Name         string       `json:"name"`
	DisplayName  string       `json:"displayName"`
	Description  string       `json:"description,omitempty"`
	Capabilities Capabilities `json:"capabilities"`
}

// Capabilities advertises optional behaviour.
type Capabilities struct {
	// Images: image input parts are accepted by the runtime.
	Images bool `json:"images"`
	// Configuration advertises accepted settings and their lifetimes.
	Configuration map[string]config.Support `json:"configuration,omitempty"`
	// Streaming: emits message.delta events while the agent writes.
	Streaming bool `json:"streaming"`
	// Approvals: can pause and ask the host to approve actions.
	Approvals bool `json:"approvals"`
	// Resume: sessions can be resumed by runtime session ID.
	Resume       bool `json:"resume"`
	Fork         bool `json:"fork"`
	ForkDeferred bool `json:"forkDeferred,omitempty"`
	// Cancel: an in-progress turn can be interrupted gracefully.
	Cancel bool `json:"cancel"`
	// Usage: reports consumed token usage, not just context-window occupancy.
	Usage bool `json:"usage"`
	// Cost: reports an estimated cost.
	Cost bool `json:"cost"`
	// ListSessions: implements SessionLister (lists the runtime's own
	// stored sessions, including ones started outside Harness Kit).
	ListSessions bool `json:"listSessions"`
	// MCP: honours SessionConfig.MCP (per-session MCP servers).
	MCP bool `json:"mcp"`
}

// AuthState is the authentication state reported by a runtime.
type AuthState string

const (
	AuthAuthenticated   AuthState = "authenticated"
	AuthUnauthenticated AuthState = "unauthenticated"
	AuthUnknown         AuthState = "unknown"
)

// AuthStatus describes how a runtime is signed in. It deliberately omits
// personal data such as email addresses.
type AuthStatus struct {
	State AuthState `json:"state"`
	// Method is the runtime's own name for the auth method, e.g.
	// "chatgpt", "apiKey", "claude.ai", "api_key".
	Method string `json:"method,omitempty"`
	// Plan is the subscription plan, when the runtime reports it.
	Plan string `json:"plan,omitempty"`
}

// Discovery is the result of probing a runtime.
type Discovery struct {
	Runtime   string     `json:"runtime"`
	Installed bool       `json:"installed"`
	Path      string     `json:"path,omitempty"`
	Version   string     `json:"version,omitempty"`
	Auth      AuthStatus `json:"auth"`
	// Ready is true when the runtime is installed, not known to be signed
	// out, and has no blocking problems.
	Ready bool `json:"ready"`
	// Problems are blocking issues (not installed, unsupported version...).
	Problems []string `json:"problems,omitempty"`
	// Notes are non-blocking information, such as usage-policy reminders.
	Notes        []string     `json:"notes,omitempty"`
	Capabilities Capabilities `json:"capabilities"`
}

// SessionConfig configures a runtime session.
type SessionConfig struct {
	// ForkFrom is internal pending native-fork state, never a user setting.
	ForkFrom string `json:"-"`
	// NativeDefaults is internal resume data supplied by the manager, not a user override.
	NativeDefaults *TurnConfig `json:"-"`
	// Cwd is the working directory the agent operates in.
	Cwd string `json:"cwd,omitempty"`
	// Model selects a model, in the runtime's own naming. Empty uses the
	// runtime's configured default.
	Model string `json:"model,omitempty"`
	// Effort selects reasoning effort in the runtime's own vocabulary.
	Effort string `json:"effort,omitempty"`
	// ResumeID is a runtime-native session ID to resume (a Codex thread
	// ID, a Claude Code session ID).
	ResumeID string `json:"resumeId,omitempty"`
	// Instructions are appended to the runtime's own system/developer
	// instructions where supported.
	Instructions string `json:"instructions,omitempty"`
	// RawEvents asks the adapter to emit runtime.raw events for native
	// messages it does not map.
	RawEvents bool `json:"rawEvents,omitempty"`
	// MCP lists MCP servers to add for this session, in Harness Kit's
	// portable format. ${VAR} references stay unexpanded here; adapters
	// resolve them when they start the runtime, so secrets are never
	// stored. Requires Capabilities.MCP.
	MCP *mcp.Config `json:"mcp,omitempty"`
	// HostTools selects which of the host's registered tools (see package
	// tools) this session may use; nil means all of them.
	HostTools []string `json:"hostTools,omitempty"`
	// DisableHostTools gives the session no host tools, and no Harness Kit
	// MCP bridge at all (which also disables Claude Code approvals).
	DisableHostTools bool `json:"disableHostTools,omitempty"`
	// PermissionTool is set by the session manager, never by callers: the
	// "<mcp server>/<tool>" that answers the runtime's permission prompts.
	PermissionTool string `json:"-"`
	// Options holds runtime-specific options: either the adapter's own
	// options struct, or JSON (json.RawMessage, []byte, map) that decodes
	// into it. See DecodeOptions.
	Options any `json:"options,omitempty"`
}

// Handler is the host side of a runtime session.
type Handler interface {
	// Emit delivers an event. The adapter sets Type, Data and optionally
	// TurnID; the host assigns SessionID, Seq and Time. Emit must not
	// block for long.
	Emit(ev event.Event)
	// RequestApproval blocks until the host decides, ctx is done, or the
	// request times out. A non-nil error means no decision was made; the
	// adapter must then treat the request as aborted.
	RequestApproval(ctx context.Context, req permission.Request) (permission.Decision, error)
}

// Session is a live runtime session.
type Session interface {
	// RuntimeSessionID is the runtime-native ID usable as ResumeID later.
	RuntimeSessionID() string
	// Prompt runs one turn to completion and blocks until it ends. The
	// adapter emits the turn's events through the Handler. Cancelling ctx
	// interrupts the turn: the adapter makes a best-effort graceful
	// interruption, then returns with TurnStatusCancelled.
	//
	// A returned error means the turn failed to run (wrap ErrSessionDead
	// if the session can no longer be used).
	Prompt(ctx context.Context, turn Turn) (TurnResult, error)
	// Close releases the session's resources (processes, pipes). It is
	// called at most once, never concurrently with Prompt.
	Close(ctx context.Context) error
}

// Turn is one prompt.
type Turn struct {
	// ID is Harness Kit's turn ID; adapters set it on events they emit.
	ID    string      `json:"turnId"`
	Input []InputPart `json:"input"`
	// Config is the immutable effective selection for this turn; nil uses startup settings.
	Config *TurnConfig `json:"config,omitempty"`
}

// InputPart is one part of a prompt.
type InputPart = event.InputPart

// Text builds a single text input.
func Text(s string) []InputPart { return []InputPart{{Type: "text", Text: s}} }

// JoinText concatenates the text parts of an input.
func JoinText(in []InputPart) string {
	var b strings.Builder
	for _, p := range in {
		if p.Type == "text" || p.Type == "" {
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// TurnResult is the outcome of Prompt.
type TurnResult struct {
	Budget        *budget.Exceeded `json:"budget,omitempty"`
	Status        event.TurnStatus `json:"status"`
	Error         *event.ErrorInfo `json:"error,omitempty"`
	Usage         *event.UsageData `json:"usage,omitempty"`
	RuntimeTurnID string           `json:"runtimeTurnId,omitempty"`
}

// SessionLister is implemented by runtimes that can list the sessions they
// have stored, through an official interface. This includes sessions that
// were started outside Harness Kit (for example in the runtime's own CLI).
type SessionLister interface {
	ListSessions(ctx context.Context, opts ListSessionsOptions) ([]RuntimeSession, error)
}

// ListSessionsOptions filter SessionLister results.
type ListSessionsOptions struct {
	// Cwd keeps only sessions whose working directory is exactly Cwd.
	Cwd string `json:"cwd,omitempty"`
	// Limit caps the number of results (newest first). Zero means a
	// runtime-chosen default.
	Limit int `json:"limit,omitempty"`
}

// RuntimeSession is a session stored by a runtime.
type RuntimeSession struct {
	// ID is usable as SessionConfig.ResumeID.
	ID        string    `json:"id"`
	Cwd       string    `json:"cwd,omitempty"`
	Title     string    `json:"title,omitempty"`
	Model     string    `json:"model,omitempty"`
	Source    string    `json:"source,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// NativeMCPLister is implemented by runtimes that can list the MCP servers
// configured in the runtime itself, without connecting to them.
type NativeMCPLister interface {
	NativeMCPServers(ctx context.Context, cwd string) ([]string, error)
}

// PermissionToolUser is implemented by runtimes that ask for approvals
// through an MCP tool (Claude Code's --permission-prompt-tool). The session
// manager then serves one through its ToolServer and sets
// SessionConfig.PermissionTool.
type PermissionToolUser interface {
	UsesPermissionTool(cfg SessionConfig) bool
}

// Guarded is implemented by runtimes whose options can switch off their
// sandbox or approval checks. Hosts use it to refuse such configurations
// unless explicitly allowed (for example when requests arrive over RPC).
type Guarded interface {
	// Unsafe returns a reason if cfg disables a safety mechanism, or "".
	Unsafe(cfg SessionConfig) string
}

// Sentinel errors.
var (
	// ErrNotInstalled: the runtime's binary could not be found.
	ErrNotInstalled = errors.New("agent: runtime not installed")
	// ErrSessionDead: the session's process or connection is gone.
	ErrSessionDead = errors.New("agent: session is no longer running")
	// ErrUnsupported: the runtime does not support the requested feature.
	ErrUnsupported = errors.New("agent: not supported by this runtime")
)

// DecodeOptions decodes SessionConfig.Options into dst, which must be a
// pointer to the adapter's options struct. src may be nil, a value or
// pointer of dst's type, json.RawMessage, []byte, string (JSON), or any
// JSON-marshalable value such as map[string]any. Unknown JSON fields are
// rejected so typos surface as errors.
func DecodeOptions(src any, dst any) error {
	if src == nil {
		return nil
	}
	var raw []byte
	switch v := src.(type) {
	case json.RawMessage:
		raw = v
	case []byte:
		raw = v
	case string:
		raw = []byte(v)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("agent: encode options: %w", err)
		}
		raw = b
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("agent: invalid runtime options: %w", err)
	}
	return nil
}
