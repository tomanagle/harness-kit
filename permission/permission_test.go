package permission

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestGlob(t *testing.T) {
	cases := []struct {
		p, s string
		want bool
	}{
		{"*", "", true},
		{"*", "rm -rf /", true},
		{"git *", "git status", true},
		{"git *", "gitk", false},
		{"rm *", "rm -rf /tmp/x", true},
		{"*.go", "a/b/c.go", true},
		{"a?c", "abc", true},
		{"a?c", "ac", false},
		{"Bash", "Bash", true},
		{"Bash", "BashX", false},
		{"mcp__*__read", "mcp__github__read", true},
	}
	for _, c := range cases {
		if got := Glob(c.p, c.s); got != c.want {
			t.Errorf("Glob(%q, %q) = %v, want %v", c.p, c.s, got, c.want)
		}
	}
}

func TestRulesFirstMatchWins(t *testing.T) {
	rules := Rules{
		{Kind: KindCommand, Command: "rm *", Action: ActionDeny, Message: "no deletes"},
		{Kind: KindCommand, Command: "git *", Action: ActionAllow},
		{Kind: KindFileChange, Action: ActionAsk},
		{Action: ActionDeny},
	}
	ctx := context.Background()

	if d, ok := rules.Evaluate(ctx, Request{Kind: KindCommand, Command: "rm -rf x"}); !ok || d.Outcome != Deny || d.Message != "no deletes" {
		t.Fatalf("rm: %+v %v", d, ok)
	}
	if d, ok := rules.Evaluate(ctx, Request{Kind: KindCommand, Command: "git push"}); !ok || d.Outcome != Allow {
		t.Fatalf("git: %+v %v", d, ok)
	}
	if _, ok := rules.Evaluate(ctx, Request{Kind: KindFileChange}); ok {
		t.Fatal("file change should defer to host")
	}
	if d, ok := rules.Evaluate(ctx, Request{Kind: KindNetwork}); !ok || d.Outcome != Deny || d.Message == "" {
		t.Fatalf("catch-all: %+v %v", d, ok)
	}
}

func TestChain(t *testing.T) {
	never := PolicyFunc(func(context.Context, Request) (Decision, bool) { return Decision{}, false })
	allow := Rules{{Action: ActionAllow}}
	d, ok := Chain(nil, never, allow).Evaluate(context.Background(), Request{})
	if !ok || d.Outcome != Allow {
		t.Fatalf("%+v %v", d, ok)
	}
}

func TestBrokerResolve(t *testing.T) {
	b := NewBroker()
	notified := make(chan Request, 1)
	type result struct {
		req Request
		d   Decision
		err error
	}
	done := make(chan result, 1)
	go func() {
		req, d, err := b.Ask(context.Background(), Request{SessionID: "s1", Kind: KindCommand, Outcomes: []Outcome{Allow, Deny}}, 0, func(r Request) { notified <- r })
		done <- result{req, d, err}
	}()
	req := <-notified
	if req.ID == "" {
		t.Fatal("id not assigned")
	}
	if p := b.Pending("s1"); len(p) != 1 || p[0].ID != req.ID {
		t.Fatalf("pending: %+v", p)
	}
	if p := b.Pending("other"); len(p) != 0 {
		t.Fatalf("pending filter: %+v", p)
	}
	if err := b.Resolve(req.ID, Decision{Outcome: AllowSession}); !errors.Is(err, ErrUnsupportedOutcome) {
		t.Fatalf("unsupported outcome: %v", err)
	}
	if err := b.Resolve(req.ID, Decision{Outcome: "maybe"}); !errors.Is(err, ErrUnsupportedOutcome) {
		t.Fatalf("invalid outcome: %v", err)
	}
	if err := b.Resolve(req.ID, Decision{Outcome: Allow, Message: "ok"}); err != nil {
		t.Fatal(err)
	}
	r := <-done
	if r.err != nil || r.d.Outcome != Allow || r.d.Message != "ok" || r.req.ID != req.ID {
		t.Fatalf("%+v", r)
	}
	if err := b.Resolve(req.ID, Decision{Outcome: Allow}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double resolve: %v", err)
	}
	if len(b.Pending("")) != 0 {
		t.Fatal("pending not cleaned up")
	}
}

func TestBrokerCancelAndTimeout(t *testing.T) {
	b := NewBroker()
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, _, err := b.Ask(ctx, Request{}, 0, func(Request) { cancel() })
		errc <- err
	}()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}

	_, _, err := b.Ask(context.Background(), Request{}, 10*time.Millisecond, nil)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("timeout: %v", err)
	}
	if len(b.Pending("")) != 0 {
		t.Fatal("pending not cleaned up")
	}
}
