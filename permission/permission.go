// Package permission provides headless approval primitives: a request and
// decision vocabulary, composable policies, and a Broker that parks
// requests until a host (a UI, a CLI prompt, a test) resolves them.
//
// The package has no knowledge of runtimes or transports. Runtime adapters
// translate their native approval prompts into Requests; hosts answer them
// with Decisions.
package permission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Kind classifies what an approval request is about.
type Kind string

const (
	KindCommand    Kind = "command"     // run a shell command
	KindFileChange Kind = "file_change" // write, edit or delete files
	KindTool       Kind = "tool"        // call a (non-shell) tool
	KindNetwork    Kind = "network"     // network access
	KindPermission Kind = "permissions" // widen sandbox permissions
	KindOther      Kind = "other"
)

// Outcome is the host's answer to a request.
type Outcome string

const (
	// Allow permits this one action.
	Allow Outcome = "allow"
	// AllowSession permits this action and similar ones for the rest of the
	// session, where the runtime supports it.
	AllowSession Outcome = "allow_session"
	// Deny refuses the action; the agent is told and may continue.
	Deny Outcome = "deny"
	// Abort refuses the action and stops the current turn.
	Abort Outcome = "abort"
)

// Valid reports whether o is a known outcome.
func (o Outcome) Valid() bool {
	switch o {
	case Allow, AllowSession, Deny, Abort:
		return true
	}
	return false
}

// Allowed reports whether o permits the action.
func (o Outcome) Allowed() bool { return o == Allow || o == AllowSession }

// Request describes an action awaiting approval.
type Request struct {
	ID        string `json:"approvalId"`
	SessionID string `json:"sessionId,omitempty"`
	TurnID    string `json:"turnId,omitempty"`
	Runtime   string `json:"runtime,omitempty"`
	Kind      Kind   `json:"kind"`
	// Tool is the runtime's name for the tool, e.g. "Bash" or "commandExecution".
	Tool string `json:"tool,omitempty"`
	// Title is a one-line, human-readable summary suitable for a prompt.
	Title   string   `json:"title"`
	Command string   `json:"command,omitempty"`
	Cwd     string   `json:"cwd,omitempty"`
	Paths   []string `json:"paths,omitempty"`
	// Reason is the agent's explanation, when it gives one.
	Reason string `json:"reason,omitempty"`
	// Input is the raw, runtime-specific tool input.
	Input json.RawMessage `json:"input,omitempty"`
	// Outcomes lists the outcomes the runtime can honour. Empty means all.
	Outcomes []Outcome `json:"outcomes,omitempty"`
}

// Supports reports whether the request accepts outcome o.
func (r Request) Supports(o Outcome) bool {
	return len(r.Outcomes) == 0 || slices.Contains(r.Outcomes, o)
}

// Decision answers a Request.
type Decision struct {
	Outcome Outcome `json:"outcome"`
	// Message is passed back to the agent (e.g. why it was denied).
	Message string `json:"message,omitempty"`
	// UpdatedInput optionally replaces the tool input, for runtimes that
	// support editing a request before approving it.
	UpdatedInput json.RawMessage `json:"updatedInput,omitempty"`
}

// Source records who produced a decision.
type Source string

const (
	SourceHost      Source = "host"      // answered by the host application
	SourcePolicy    Source = "policy"    // decided by a Policy without asking
	SourceTimeout   Source = "timeout"   // nobody answered in time
	SourceCancelled Source = "cancelled" // the turn or session ended first
)

// Policy decides requests without asking a person. Evaluate returns
// decided=false to defer to the host.
type Policy interface {
	Evaluate(ctx context.Context, req Request) (d Decision, decided bool)
}

// PolicyFunc adapts a function to Policy.
type PolicyFunc func(ctx context.Context, req Request) (Decision, bool)

// Evaluate implements Policy.
func (f PolicyFunc) Evaluate(ctx context.Context, req Request) (Decision, bool) { return f(ctx, req) }

// Chain evaluates policies in order and returns the first decision.
func Chain(policies ...Policy) Policy {
	return PolicyFunc(func(ctx context.Context, req Request) (Decision, bool) {
		for _, p := range policies {
			if p == nil {
				continue
			}
			if d, ok := p.Evaluate(ctx, req); ok {
				return d, true
			}
		}
		return Decision{}, false
	})
}

// Action is what a Rule does when it matches.
type Action string

const (
	ActionAllow Action = "allow"
	ActionDeny  Action = "deny"
	ActionAsk   Action = "ask" // stop evaluating and ask the host
)

// Rule matches requests by kind, tool and command. Empty fields match
// anything. Tool and Command are glob patterns where '*' matches any run
// of characters (including '/') and '?' matches one character.
type Rule struct {
	Kind    Kind   `json:"kind,omitempty"`
	Tool    string `json:"tool,omitempty"`
	Command string `json:"command,omitempty"`
	Action  Action `json:"action"`
	Message string `json:"message,omitempty"`
}

// Matches reports whether the rule applies to req.
func (r Rule) Matches(req Request) bool {
	if r.Kind != "" && r.Kind != req.Kind {
		return false
	}
	if r.Tool != "" && !Glob(r.Tool, req.Tool) {
		return false
	}
	if r.Command != "" && !Glob(r.Command, req.Command) {
		return false
	}
	return true
}

// Rules is an ordered rule list; the first matching rule wins. It
// implements Policy.
type Rules []Rule

// Evaluate implements Policy.
func (rs Rules) Evaluate(_ context.Context, req Request) (Decision, bool) {
	for _, r := range rs {
		if !r.Matches(req) {
			continue
		}
		switch r.Action {
		case ActionAllow:
			return Decision{Outcome: Allow, Message: r.Message}, true
		case ActionDeny:
			msg := r.Message
			if msg == "" {
				msg = "denied by policy"
			}
			return Decision{Outcome: Deny, Message: msg}, true
		default:
			return Decision{}, false
		}
	}
	return Decision{}, false
}

// Glob matches s against pattern, where '*' matches any (possibly empty)
// sequence and '?' matches exactly one character.
func Glob(pattern, s string) bool {
	p, str := []rune(pattern), []rune(s)
	pi, si := 0, 0
	star, match := -1, 0
	for si < len(str) {
		switch {
		case pi < len(p) && (p[pi] == '?' || p[pi] == str[si]):
			pi++
			si++
		case pi < len(p) && p[pi] == '*':
			star, match = pi, si
			pi++
		case star >= 0:
			pi = star + 1
			match++
			si = match
		default:
			return false
		}
	}
	for pi < len(p) && p[pi] == '*' {
		pi++
	}
	return pi == len(p)
}

// Errors returned by the Broker.
var (
	ErrNotFound           = errors.New("permission: approval not found")
	ErrUnsupportedOutcome = errors.New("permission: outcome not supported by this request")
	ErrTimeout            = errors.New("permission: approval timed out")
)

// Broker parks approval requests until they are resolved. It is safe for
// concurrent use.
type Broker struct {
	mu      sync.Mutex
	pending map[string]*pendingRequest
	seq     atomic.Uint64
	prefix  string
}

type pendingRequest struct {
	req  Request
	resp chan Decision
}

// NewBroker returns an empty Broker.
func NewBroker() *Broker {
	return &Broker{pending: make(map[string]*pendingRequest), prefix: "apr_"}
}

// Ask registers req, invokes notify (so a host can be told), then blocks
// until Resolve is called, ctx is done, or timeout elapses (timeout <= 0
// waits indefinitely). An empty req.ID is assigned. The returned Request
// carries the final ID.
func (b *Broker) Ask(ctx context.Context, req Request, timeout time.Duration, notify func(Request)) (Request, Decision, error) {
	if req.ID == "" {
		req.ID = fmt.Sprintf("%s%d", b.prefix, b.seq.Add(1))
	}
	p := &pendingRequest{req: req, resp: make(chan Decision, 1)}
	b.mu.Lock()
	if _, dup := b.pending[req.ID]; dup {
		b.mu.Unlock()
		return req, Decision{}, fmt.Errorf("permission: duplicate approval id %q", req.ID)
	}
	b.pending[req.ID] = p
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.pending, req.ID)
		b.mu.Unlock()
	}()

	if notify != nil {
		notify(req)
	}

	var timer <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timer = t.C
	}
	select {
	case d := <-p.resp:
		return req, d, nil
	case <-ctx.Done():
		return req, Decision{}, ctx.Err()
	case <-timer:
		return req, Decision{}, ErrTimeout
	}
}

// Resolve answers a pending request.
func (b *Broker) Resolve(id string, d Decision) error {
	if !d.Outcome.Valid() {
		return fmt.Errorf("%w: %q", ErrUnsupportedOutcome, d.Outcome)
	}
	b.mu.Lock()
	p, ok := b.pending[id]
	if ok {
		if !p.req.Supports(d.Outcome) {
			b.mu.Unlock()
			return fmt.Errorf("%w: %q (supported: %s)", ErrUnsupportedOutcome, d.Outcome, joinOutcomes(p.req.Outcomes))
		}
		delete(b.pending, id)
	}
	b.mu.Unlock()
	if !ok {
		return ErrNotFound
	}
	p.resp <- d
	return nil
}

// Pending returns the requests awaiting a decision, optionally filtered by
// session ID.
func (b *Broker) Pending(sessionID string) []Request {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Request, 0, len(b.pending))
	for _, p := range b.pending {
		if sessionID == "" || p.req.SessionID == sessionID {
			out = append(out, p.req)
		}
	}
	slices.SortFunc(out, func(a, b Request) int { return strings.Compare(a.ID, b.ID) })
	return out
}

func joinOutcomes(os []Outcome) string {
	s := make([]string, len(os))
	for i, o := range os {
		s[i] = string(o)
	}
	return strings.Join(s, ", ")
}
