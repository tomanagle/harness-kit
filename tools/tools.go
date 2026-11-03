// Package tools is a registry of host-defined tools: functions in your
// application (create a ticket, query a database, open a UI panel) that
// agents can call.
//
// The same registered tool works on both Harness Kit tracks:
//
//   - Delegated runtimes (Codex, Claude Code) reach the registry through an
//     MCP server that Harness Kit hosts for each session (package toolhost).
//   - Your own agent loop calls Registry.Call directly.
//
// The package validates arguments against each tool's JSON Schema, applies
// timeouts and a concurrency limit, and turns panics and handler errors
// into error results the model can read. It never offers shell or
// filesystem tools of its own.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/tomanagle/harness-kit/mcp"
)

// DefaultTimeout bounds a tool call when the tool sets no Timeout.
const DefaultTimeout = 2 * time.Minute

// Tool is a callable tool.
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// InputSchema is a JSON Schema for the arguments object. Empty means
	// {"type":"object"}.
	InputSchema json.RawMessage `json:"inputSchema"`
	// Approval ask makes Harness Kit request the host's approval before
	// each call. Default auto.
	Approval mcp.Approval `json:"approval,omitempty"`
	// ReadOnly hints that the tool has no side effects.
	ReadOnly bool `json:"readOnly,omitempty"`
	// Timeout bounds one call (DefaultTimeout if zero).
	Timeout time.Duration `json:"-"`
	Handler Handler       `json:"-"`
}

// Handler runs a tool call. Returning an error produces an error result
// (visible to the model), not a protocol failure.
type Handler func(ctx context.Context, call Call) (Result, error)

// Call is one invocation.
type Call struct {
	SessionID string          `json:"sessionId,omitempty"`
	TurnID    string          `json:"turnId,omitempty"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// Result is a tool's output, in MCP's CallToolResult shape.
type Result struct {
	Content    []Content `json:"content"`
	Structured any       `json:"structuredContent,omitempty"`
	IsError    bool      `json:"isError,omitempty"`
}

// Content is one piece of tool output.
type Content struct {
	Type string `json:"type"` // "text"
	Text string `json:"text"`
}

// Text joins the text content of r.
func (r Result) Text() string {
	var s string
	for _, c := range r.Content {
		if c.Type == "text" {
			s += c.Text
		}
	}
	return s
}

// TextResult is a successful text result.
func TextResult(s string) Result { return Result{Content: []Content{{Type: "text", Text: s}}} }

// ErrorResult is a failed result carrying msg for the model.
func ErrorResult(msg string) Result {
	return Result{Content: []Content{{Type: "text", Text: msg}}, IsError: true}
}

type callKey struct{}

// CallFromContext returns the Call being handled, for handlers created
// with Func that want the session or turn ID.
func CallFromContext(ctx context.Context) (Call, bool) {
	c, ok := ctx.Value(callKey{}).(Call)
	return c, ok
}

// Errors.
var (
	ErrNotFound = errors.New("tools: no such tool")
	ErrExists   = errors.New("tools: a tool with that name is already registered")
)

var validName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Registry holds tools. It is safe for concurrent use.
type Registry struct {
	mu        sync.RWMutex
	tools     map[string]Tool
	order     []string
	listeners map[int]func()
	nextL     int
	sem       chan struct{}
}

// Option configures a Registry.
type Option func(*Registry)

// MaxConcurrent limits how many calls run at once across the registry.
func MaxConcurrent(n int) Option {
	return func(r *Registry) {
		if n > 0 {
			r.sem = make(chan struct{}, n)
		}
	}
}

// NewRegistry returns an empty registry.
func NewRegistry(opts ...Option) *Registry {
	r := &Registry{tools: map[string]Tool{}, listeners: map[int]func(){}}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Register adds a tool.
func (r *Registry) Register(t Tool) error {
	if !validName.MatchString(t.Name) {
		return fmt.Errorf("tools: invalid name %q (letters, digits, '_' and '-', up to 64)", t.Name)
	}
	if t.Handler == nil {
		return fmt.Errorf("tools: %s has no handler", t.Name)
	}
	switch t.Approval {
	case "", mcp.ApprovalAuto, mcp.ApprovalAsk:
	default:
		return fmt.Errorf("tools: %s: approval must be auto or ask", t.Name)
	}
	if len(t.InputSchema) == 0 {
		t.InputSchema = json.RawMessage(`{"type":"object"}`)
	}
	var schema map[string]any
	if err := json.Unmarshal(t.InputSchema, &schema); err != nil {
		return fmt.Errorf("tools: %s: input schema is not a JSON object: %w", t.Name, err)
	}
	if typ, ok := schema["type"]; ok && typ != "object" {
		return fmt.Errorf("tools: %s: input schema must describe an object", t.Name)
	}
	r.mu.Lock()
	if _, dup := r.tools[t.Name]; dup {
		r.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrExists, t.Name)
	}
	r.tools[t.Name] = t
	r.order = append(r.order, t.Name)
	r.mu.Unlock()
	r.changed()
	return nil
}

// Unregister removes tools by name; unknown names are ignored.
func (r *Registry) Unregister(names ...string) {
	r.mu.Lock()
	removed := false
	for _, n := range names {
		if _, ok := r.tools[n]; ok {
			delete(r.tools, n)
			r.order = slices.DeleteFunc(r.order, func(s string) bool { return s == n })
			removed = true
		}
	}
	r.mu.Unlock()
	if removed {
		r.changed()
	}
}

// Get returns a tool.
func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// List returns tools in registration order.
func (r *Registry) List() []Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Tool, 0, len(r.order))
	for _, n := range r.order {
		out = append(out, r.tools[n])
	}
	return out
}

// OnChange registers fn to run after tools are added or removed. It
// returns a function that removes the listener.
func (r *Registry) OnChange(fn func()) func() {
	r.mu.Lock()
	id := r.nextL
	r.nextL++
	r.listeners[id] = fn
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		delete(r.listeners, id)
		r.mu.Unlock()
	}
}

func (r *Registry) changed() {
	r.mu.RLock()
	fns := make([]func(), 0, len(r.listeners))
	for _, fn := range r.listeners {
		fns = append(fns, fn)
	}
	r.mu.RUnlock()
	for _, fn := range fns {
		fn()
	}
}

// Call validates the arguments and runs the tool. Unknown tools are an
// error; everything else (invalid arguments, handler errors, panics,
// timeouts) becomes an error Result the model can read.
func (r *Registry) Call(ctx context.Context, call Call) (Result, error) {
	t, ok := r.Get(call.Name)
	if !ok {
		return Result{}, fmt.Errorf("%w: %s", ErrNotFound, call.Name)
	}
	if len(call.Arguments) == 0 || string(call.Arguments) == "null" {
		call.Arguments = json.RawMessage(`{}`)
	}
	if err := Validate(t.InputSchema, call.Arguments); err != nil {
		return ErrorResult("invalid arguments: " + err.Error()), nil
	}
	if r.sem != nil {
		select {
		case r.sem <- struct{}{}:
			defer func() { <-r.sem }()
		case <-ctx.Done():
			return ErrorResult("cancelled"), nil
		}
	}
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(context.WithValue(ctx, callKey{}, call), timeout)
	defer cancel()

	type outcome struct {
		res Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- outcome{err: fmt.Errorf("tool panicked: %v", p)}
			}
		}()
		res, err := t.Handler(ctx, call)
		done <- outcome{res, err}
	}()
	select {
	case o := <-done:
		if o.err != nil {
			return ErrorResult(o.err.Error()), nil
		}
		if o.res.Content == nil {
			o.res.Content = []Content{}
		}
		return o.res, nil
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return ErrorResult(fmt.Sprintf("tool timed out after %s", timeout)), nil
		}
		return ErrorResult("cancelled"), nil
	}
}

// ToolOption configures a tool built with Func or Add.
type ToolOption func(*Tool)

// WithApproval sets the approval mode.
func WithApproval(a mcp.Approval) ToolOption { return func(t *Tool) { t.Approval = a } }

// ReadOnly marks the tool as side-effect free.
func ReadOnly() ToolOption { return func(t *Tool) { t.ReadOnly = true } }

// WithTimeout sets the per-call timeout.
func WithTimeout(d time.Duration) ToolOption { return func(t *Tool) { t.Timeout = d } }

// WithSchema overrides the inferred input schema.
func WithSchema(schema json.RawMessage) ToolOption { return func(t *Tool) { t.InputSchema = schema } }

// Func builds a Tool from a typed function. The input schema is inferred
// from In (a struct; see SchemaFor). The output becomes text: strings as
// is, a Result unchanged, anything else as JSON (also returned as
// structured content).
func Func[In, Out any](name, description string, fn func(context.Context, In) (Out, error), opts ...ToolOption) (Tool, error) {
	var zero In
	schema, err := SchemaOf(zero)
	if err != nil {
		return Tool{}, fmt.Errorf("tools: %s: %w", name, err)
	}
	t := Tool{Name: name, Description: description, InputSchema: schema}
	t.Handler = func(ctx context.Context, call Call) (Result, error) {
		var in In
		if err := json.Unmarshal(call.Arguments, &in); err != nil {
			return ErrorResult("invalid arguments: " + err.Error()), nil
		}
		out, err := fn(ctx, in)
		if err != nil {
			return Result{}, err
		}
		switch v := any(out).(type) {
		case string:
			return TextResult(v), nil
		case Result:
			return v, nil
		}
		b, err := json.Marshal(out)
		if err != nil {
			return Result{}, err
		}
		res := TextResult(string(b))
		if json.Valid(b) && len(b) > 0 && b[0] == '{' {
			res.Structured = json.RawMessage(b)
		}
		return res, nil
	}
	for _, o := range opts {
		o(&t)
	}
	return t, nil
}

// Add builds a tool with Func and registers it.
func Add[In, Out any](r *Registry, name, description string, fn func(context.Context, In) (Out, error), opts ...ToolOption) error {
	t, err := Func(name, description, fn, opts...)
	if err != nil {
		return err
	}
	return r.Register(t)
}
