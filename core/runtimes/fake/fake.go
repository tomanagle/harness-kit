// Package fake provides a deterministic agent runtime for tests, demos and
// client development. It needs no network, no credentials and no installed
// CLI, so the full Harness Kit stack (sessions, events, approvals, RPC,
// TypeScript client) can be exercised anywhere.
//
// The fake echoes prompts back as a streamed agent message. Directives in
// the prompt text trigger other behaviour:
//
//	[subagent] report a deterministic delegated task
//	[approve]  ask for approval to run `echo hello` before replying
//	[slow]     stream a long reply slowly (for cancellation tests)
//	[fail]     fail the turn
//	[crash]    fail the turn and mark the session dead
//	[raw]      emit a runtime.raw event (if the session enabled RawEvents)
//	[call NAME {"json":"args"}]
//	           call host tool NAME through the session's harness_kit MCP
//	           server (needs WithMCPCaller)
package fake

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/config"
	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/mcp"
	"github.com/tomanagle/harness-kit/core/permission"
)

// MCPCaller calls a tool on an MCP server. It is injected so this package
// never starts processes itself (see mcp/client for an implementation).
type MCPCaller func(ctx context.Context, server mcp.Server, tool string, args json.RawMessage) (text string, isError bool, err error)

// WithMCPCaller lets [call ...] directives reach MCP servers.
func WithMCPCaller(c MCPCaller) Option { return func(r *Runtime) { r.caller = c } }

// Runtime is the fake runtime.
type Runtime struct {
	name   string
	delay  time.Duration
	seq    atomic.Int64
	caller MCPCaller

	mu      sync.Mutex
	started []agent.RuntimeSession // sessions started by this instance
}

// Option configures the fake runtime.
type Option func(*Runtime)

// WithName overrides the runtime name (default "fake").
func WithName(name string) Option { return func(r *Runtime) { r.name = name } }

// WithDelay sets the pause between streamed words (default 0).
func WithDelay(d time.Duration) Option { return func(r *Runtime) { r.delay = d } }

// New returns a fake runtime.
func New(opts ...Option) *Runtime {
	r := &Runtime{name: "fake"}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Options are the fake's per-session options.
type Options struct {
	// FailStart makes Start fail.
	FailStart bool `json:"failStart,omitempty"`
	// Unsafe makes the configuration report itself as unsafe (see
	// agent.Guarded), for testing host guards.
	Unsafe bool `json:"unsafe,omitempty"`
	// ContextTokens and ContextWindowTokens enable deterministic context
	// observations when either is supplied. Omitted counts remain unknown.
	// ContextTokens is reported initially and after each successful prompt.
	ContextTokens       *int64 `json:"contextTokens,omitempty"`
	ContextWindowTokens *int64 `json:"contextWindowTokens,omitempty"`
	// CompactedTokens is reported after manual compaction (default zero).
	CompactedTokens *int64 `json:"compactedTokens,omitempty"`
	// CompactionDelayMs delays maintenance, allowing cancellation tests.
	// Accepted range: 0–60000 milliseconds.
	CompactionDelayMs int64 `json:"compactionDelayMs,omitempty"`
}

var (
	_ agent.Runtime       = (*Runtime)(nil)
	_ agent.Guarded       = (*Runtime)(nil)
	_ agent.SessionLister = (*Runtime)(nil)
	_ agent.Compactor     = (*Session)(nil)
)

// Info implements agent.Runtime.
func (r *Runtime) Info() agent.Info {
	return agent.Info{
		Name:        r.name,
		DisplayName: "Fake Runtime",
		Description: "Deterministic echo runtime for tests and demos.",
		Capabilities: agent.Capabilities{
			Configuration: map[string]config.Support{
				"model": {Startup: true, Session: true, Turn: true}, "effort": {Startup: true, Session: true, Turn: true},
				"options.unsafe": {Startup: true, Session: true, Turn: true}, "options.failStart": {Startup: true},
				"options.contextTokens": {Startup: true}, "options.contextWindowTokens": {Startup: true},
				"options.compactedTokens": {Startup: true}, "options.compactionDelayMs": {Startup: true},
				"cwd": {Startup: true}, "instructions": {Startup: true}, "mcp": {Startup: true}, "hostTools": {Startup: true}, "rawEvents": {Startup: true},
			},
			Streaming: true, Approvals: true, Resume: true, Fork: true, Cancel: true, Usage: true, Cost: true, ListSessions: true, MCP: true, ContextUsage: true, Compaction: true,
		},
	}
}

// Discover implements agent.Runtime.
func (r *Runtime) Discover(context.Context) (agent.Discovery, error) {
	return agent.Discovery{
		Runtime:      r.name,
		Installed:    true,
		Version:      "1.0.0",
		Auth:         agent.AuthStatus{State: agent.AuthAuthenticated, Method: "none"},
		Ready:        true,
		Notes:        []string{"Deterministic test runtime; makes no network calls."},
		Capabilities: r.Info().Capabilities,
	}, nil
}

// Unsafe implements agent.Guarded.
func (r *Runtime) Unsafe(cfg agent.SessionConfig) string {
	var o Options
	if err := agent.DecodeOptions(cfg.Options, &o); err == nil && o.Unsafe {
		return "fake option unsafe=true"
	}
	return ""
}

// ValidateConfig validates the fake runtime without running it.
func (r *Runtime) ValidateConfig(cfg agent.SessionConfig) error {
	var opts Options
	if err := agent.DecodeOptions(cfg.Options, &opts); err != nil {
		return fmt.Errorf("%w: %v", agent.ErrInvalidConfig, err)
	}
	if opts.ContextTokens != nil && *opts.ContextTokens < 0 {
		return fmt.Errorf("%w: fake contextTokens must be non-negative", agent.ErrInvalidConfig)
	}
	if opts.ContextWindowTokens != nil && *opts.ContextWindowTokens <= 0 {
		return fmt.Errorf("%w: fake contextWindowTokens must be positive", agent.ErrInvalidConfig)
	}
	if opts.CompactedTokens != nil && *opts.CompactedTokens < 0 {
		return fmt.Errorf("%w: fake compactedTokens must be non-negative", agent.ErrInvalidConfig)
	}
	if opts.CompactionDelayMs < 0 || opts.CompactionDelayMs > 60000 {
		return fmt.Errorf("%w: fake compactionDelayMs must be between 0 and 60000", agent.ErrInvalidConfig)
	}
	return nil
}

// Start implements agent.Runtime.
func (r *Runtime) Start(_ context.Context, cfg agent.SessionConfig, h agent.Handler) (agent.Session, error) {
	if err := r.ValidateConfig(cfg); err != nil {
		return nil, err
	}
	var o Options
	if err := agent.DecodeOptions(cfg.Options, &o); err != nil {
		return nil, err
	}
	if o.FailStart {
		return nil, errors.New("fake: start failed as requested")
	}
	id := cfg.ResumeID
	if id == "" {
		id = fmt.Sprintf("fake-%d", r.seq.Add(1))
		if cfg.ForkFrom != "" {
			id = "fake-fork-" + rand.Text()
		}
		now := time.Now().UTC()
		r.mu.Lock()
		r.started = append(r.started, agent.RuntimeSession{ID: id, Cwd: cfg.Cwd, Title: "fake session", Source: "fake", CreatedAt: now, UpdatedAt: now})
		r.mu.Unlock()
	}
	if cfg.MCP != nil && h != nil {
		// The fake runs no MCP servers; it reports what it was given so
		// hosts can test their MCP plumbing.
		detail, _ := json.Marshal(map[string]any{"servers": cfg.MCP.Names(), "inheritNative": cfg.MCP.Inherit()})
		h.Emit(event.Event{Type: event.RuntimeStatus, Data: event.RuntimeStatusData{
			Status: "mcp", Message: "MCP servers: " + strings.Join(cfg.MCP.Names(), ", "), Detail: detail,
		}})
	}
	s := &Session{rt: r, id: id, cfg: cfg, h: h, opts: o}
	s.emitContext("", o.ContextTokens)
	return s, nil
}

// Fork creates an independent deterministic echo session.
func (r *Runtime) Fork(ctx context.Context, source string, cfg agent.SessionConfig, h agent.Handler) (agent.Session, error) {
	if source == "" {
		return nil, fmt.Errorf("%w: fork source is empty", agent.ErrInvalidConfig)
	}
	cfg.ResumeID, cfg.ForkFrom = "", source
	return r.Start(ctx, cfg, h)
}

// ListSessions implements agent.SessionLister: the sessions this instance
// started, newest first.
func (r *Runtime) ListSessions(_ context.Context, opts agent.ListSessionsOptions) ([]agent.RuntimeSession, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []agent.RuntimeSession
	for i := len(r.started) - 1; i >= 0; i-- {
		s := r.started[i]
		if opts.Cwd != "" && s.Cwd != opts.Cwd {
			continue
		}
		out = append(out, s)
		if opts.Limit > 0 && len(out) == opts.Limit {
			break
		}
	}
	return out, nil
}

// Session is a fake session.
type Session struct {
	rt    *Runtime
	id    string
	cfg   agent.SessionConfig
	h     agent.Handler
	turns int
	opts  Options
}

// RuntimeSessionID implements agent.Session.
func (s *Session) RuntimeSessionID() string { return s.id }

// Close implements agent.Session.
func (s *Session) Close(context.Context) error { return nil }

// CanCompact is enabled only by explicitly configured context fixtures.
func (s *Session) CanCompact() bool {
	return s.opts.ContextTokens != nil || s.opts.ContextWindowTokens != nil
}

// Compact performs deterministic, cancellable maintenance without a prompt.
func (s *Session) Compact(ctx context.Context) error {
	if !s.CanCompact() {
		return fmt.Errorf("%w: fake context fixtures are not enabled", agent.ErrUnsupported)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.opts.CompactionDelayMs > 0 {
		timer := time.NewTimer(time.Duration(s.opts.CompactionDelayMs) * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	used := int64(0)
	if s.opts.CompactedTokens != nil {
		used = *s.opts.CompactedTokens
	}
	s.emitContext("", &used)
	return nil
}

func (s *Session) emitContext(turnID string, used *int64) {
	if !s.CanCompact() || s.h == nil {
		return
	}
	snapshot := event.ContextUsageData{UsedTokens: used, WindowTokens: s.opts.ContextWindowTokens, Basis: "reported"}.Clone()
	s.h.Emit(event.Event{Type: event.ContextUsage, TurnID: turnID, Data: snapshot})
}

// Prompt implements agent.Session.
func (s *Session) Prompt(ctx context.Context, turn agent.Turn) (agent.TurnResult, error) {
	s.turns++
	text := agent.JoinText(turn.Input)
	emit := func(t event.Type, data any) { s.h.Emit(event.Event{Type: t, TurnID: turn.ID, Data: data}) }

	switch {
	case strings.Contains(text, "[crash]"):
		return agent.TurnResult{}, fmt.Errorf("fake: crashed as requested: %w", agent.ErrSessionDead)
	case strings.Contains(text, "[fail]"):
		return agent.TurnResult{}, errors.New("fake: turn failed as requested")
	}

	if strings.Contains(text, "[subagent]") {
		child := event.SubagentUpdatedData{ID: "subagent-" + turn.ID, CallID: "delegate-" + turn.ID, Title: "Review implementation", Prompt: "Review the implementation and report findings.", Status: event.SubagentRunning}
		emit(event.SubagentUpdated, child)
		defer func() {
			child.Status = event.SubagentCompleted
			child.Output = "Review complete. No issues found."
			if ctx.Err() != nil {
				child.Status = event.SubagentCancelled
				child.Output = ""
			}
			emit(event.SubagentUpdated, child)
		}()
	}
	if strings.Contains(text, "[raw]") && s.cfg.RawEvents {
		emit(event.RuntimeRaw, event.RawData{Runtime: s.rt.name, Kind: "fake/raw", Payload: json.RawMessage(`{"hello":"raw"}`)})
	}

	if strings.Contains(text, "[approve]") {
		callID := fmt.Sprintf("call_%d", s.turns)
		input := json.RawMessage(`{"command":"echo hello"}`)
		d, err := s.h.RequestApproval(ctx, permission.Request{
			TurnID:   turn.ID,
			Kind:     permission.KindCommand,
			Tool:     "shell",
			Title:    "Run `echo hello`",
			Command:  "echo hello",
			Cwd:      s.cfg.Cwd,
			Reason:   "The prompt asked for an approval round-trip.",
			Input:    input,
			Outcomes: []permission.Outcome{permission.Allow, permission.AllowSession, permission.Deny, permission.Abort},
		})
		if err != nil || d.Outcome == permission.Abort {
			return agent.TurnResult{Status: event.TurnStatusCancelled}, nil
		}
		emit(event.ToolStarted, event.ToolStartedData{CallID: callID, Name: "shell", Kind: event.ToolKindCommand, Title: "echo hello", Input: input})
		if d.Outcome.Allowed() {
			zero := 0
			emit(event.ToolOutput, event.ToolOutputData{CallID: callID, Output: "hello\n"})
			emit(event.ToolCompleted, event.ToolCompletedData{CallID: callID, Name: "shell", Status: event.ToolStatusCompleted, Output: "hello\n", ExitCode: &zero})
		} else {
			emit(event.ToolCompleted, event.ToolCompletedData{CallID: callID, Name: "shell", Status: event.ToolStatusDeclined, Error: d.Message})
		}
	}

	reply := "Echo: " + strings.TrimSpace(stripDirectives(text))
	for i, m := range callDirective.FindAllStringSubmatch(text, -1) {
		out, cancelled := s.callTool(ctx, turn.ID, fmt.Sprintf("call_%d_%d", s.turns, i), m[1], m[2])
		if cancelled {
			return agent.TurnResult{Status: event.TurnStatusCancelled}, nil
		}
		reply += " | " + m[1] + ": " + out
	}
	delay := s.rt.delay
	if strings.Contains(text, "[slow]") {
		reply += strings.Repeat(" and on", 60)
		if delay < 50*time.Millisecond {
			delay = 50 * time.Millisecond
		}
	}

	itemID := fmt.Sprintf("msg_%d", s.turns)
	words := strings.SplitAfter(reply, " ")
	var sent strings.Builder
	for _, w := range words {
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-ctx.Done():
			}
		}
		if ctx.Err() != nil {
			if sent.Len() > 0 {
				emit(event.MessageCompleted, event.MessageCompletedData{ItemID: itemID, Role: "assistant", Text: sent.String()})
			}
			return agent.TurnResult{Status: event.TurnStatusCancelled}, nil
		}
		sent.WriteString(w)
		emit(event.MessageDelta, event.MessageDeltaData{ItemID: itemID, Text: w})
	}
	emit(event.MessageCompleted, event.MessageCompletedData{ItemID: itemID, Role: "assistant", Text: reply})

	cost := 0.0
	usage := &event.UsageData{
		Scope:        "turn",
		InputTokens:  int64(len(strings.Fields(text))),
		OutputTokens: int64(len(words)),
		CostUSD:      &cost,
	}
	usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	emit(event.Usage, *usage)
	s.emitContext(turn.ID, s.opts.ContextTokens)
	return agent.TurnResult{Status: event.TurnStatusCompleted, Usage: usage, RuntimeTurnID: fmt.Sprintf("%s-turn-%d", s.id, s.turns)}, nil
}

var callDirective = regexp.MustCompile(`\[call ([A-Za-z0-9_-]+)\s*(\{.*?\})?\]`)

// callTool runs a [call] directive through the harness_kit MCP server.
func (s *Session) callTool(ctx context.Context, turnID, callID, name, args string) (string, bool) {
	if args == "" {
		args = "{}"
	}
	emit := func(t event.Type, data any) { s.h.Emit(event.Event{Type: t, TurnID: turnID, Data: data}) }
	emit(event.ToolStarted, event.ToolStartedData{CallID: callID, Name: name, Kind: event.ToolKindMCP, Title: mcp.ReservedName + "/" + name, Input: json.RawMessage(args)})
	var srv mcp.Server
	var ok bool
	if s.cfg.MCP != nil {
		srv, ok = s.cfg.MCP.Servers[mcp.ReservedName]
	}
	if !ok || s.rt.caller == nil {
		emit(event.ToolCompleted, event.ToolCompletedData{CallID: callID, Name: name, Status: event.ToolStatusFailed, Error: "no host tools in this session"})
		return "unavailable", false
	}
	text, isErr, err := s.rt.caller(ctx, srv, name, json.RawMessage(args))
	if ctx.Err() != nil {
		return "", true
	}
	d := event.ToolCompletedData{CallID: callID, Name: name, Status: event.ToolStatusCompleted, Output: text}
	if err != nil {
		text, isErr = err.Error(), true
	}
	if isErr {
		d.Status, d.Error, d.Output = event.ToolStatusFailed, text, ""
	}
	emit(event.ToolCompleted, d)
	return text, false
}

func stripDirectives(s string) string {
	s = callDirective.ReplaceAllString(s, "")
	for _, d := range []string{"[subagent]", "[approve]", "[slow]", "[fail]", "[crash]", "[raw]"} {
		s = strings.ReplaceAll(s, d, "")
	}
	return strings.Join(strings.Fields(s), " ")
}
