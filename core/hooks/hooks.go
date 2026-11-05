// Package hooks supplies ordered, application-owned lifecycle extensions.
package hooks

import (
	"context"
	"errors"

	"github.com/tomanagle/harness-kit/core/agent"
)

var ErrFailed = errors.New("lifecycle hook failed")

type Name string

const (
	BeforeCreate Name = "session.beforeCreate"
	BeforeTurn   Name = "turn.before"
	AfterTurn    Name = "turn.after"
)

func (n Name) Valid() bool { return n == BeforeCreate || n == BeforeTurn || n == AfterTurn }

type Spec struct {
	ID   string `json:"id"`
	Name Name   `json:"name"`
	// Lower order runs first; ties are ordered by ID, independent of connection timing.
	Order     int    `json:"order,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
}

type Session struct {
	ID       string              `json:"sessionId"`
	Runtime  string              `json:"runtime"`
	Title    string              `json:"title"`
	Turns    int                 `json:"turns"`
	Metadata map[string]string   `json:"metadata,omitempty"`
	Config   agent.SessionConfig `json:"config"`
}

type Create struct {
	Runtime  string              `json:"runtime"`
	Title    string              `json:"title,omitempty"`
	Metadata map[string]string   `json:"metadata,omitempty"`
	Config   agent.SessionConfig `json:"config"`
}

type Request struct {
	// ConcurrencyLimit also bounds callbacks retained by a remote client after timeout.
	ConcurrencyLimit int                `json:"concurrencyLimit"`
	HookID           string             `json:"hookId"`
	InvocationID     string             `json:"invocationId"`
	Name             Name               `json:"name"`
	Session          *Session           `json:"session,omitempty"`
	Create           *Create            `json:"create,omitempty"`
	TurnID           string             `json:"turnId,omitempty"`
	IsFirstTurn      bool               `json:"isFirstTurn"`
	Input            []agent.InputPart  `json:"input,omitempty"`
	Config           *agent.ConfigPatch `json:"config,omitempty"`
	Result           *agent.TurnResult  `json:"result,omitempty"`
}

// Response is a patch. Omitted values preserve the previous handler's output.
// Before-create accepts Create; before-turn accepts Input and Config; after-turn
// is observational and must return an empty response.
type Response struct {
	Create *Create            `json:"create,omitempty"`
	Input  *[]agent.InputPart `json:"input,omitempty"`
	Config *agent.ConfigPatch `json:"config,omitempty"`
}

type Handler interface {
	Handle(context.Context, Request) (Response, error)
}

type HandlerFunc func(context.Context, Request) (Response, error)

func (f HandlerFunc) Handle(ctx context.Context, r Request) (Response, error) { return f(ctx, r) }

// Executor is injectable; implementations must preserve ordering, cancellation
// and ownership of request data. Go handlers must honor their context deadline.
type Executor interface {
	Run(context.Context, Request, Settings) (Request, []string, error)
}

type Registration interface {
	Executor
	Register(string, Spec, Handler) error
	Unregister(string, string) error
	DropOwner(string)
	List() []Spec
}
