package hooks_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/hooks"
)

func TestOrderingOwnershipAndIsolation(t *testing.T) {
	r := hooks.NewRegistry()
	var order []string
	for _, id := range []string{"b", "a"} {
		id := id
		err := r.Register("owner", hooks.Spec{ID: id, Name: hooks.BeforeTurn}, hooks.HandlerFunc(func(ctx context.Context, request hooks.Request) (hooks.Response, error) {
			order = append(order, id)
			input := append(request.Input, agent.InputPart{Type: "text", Text: id})
			return hooks.Response{Input: &input}, nil
		}))
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Unregister("another", "a"); err == nil {
		t.Fatal("another owner unregistered a hook")
	}
	request := hooks.Request{Name: hooks.BeforeTurn, InvocationID: "s:t", Input: agent.Text("original")}
	got, _, err := r.Run(context.Background(), request, hooks.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"a", "b"}) || len(got.Input) != 3 || len(request.Input) != 1 {
		t.Fatalf("order=%v input=%v", order, got.Input)
	}
	r.DropOwner("owner")
	policy := hooks.Defaults()
	policy.Required = []string{"a"}
	policy.OnError = "continue"
	if _, _, err = r.Run(context.Background(), request, policy); !errors.Is(err, hooks.ErrFailed) {
		t.Fatalf("missing required hook ignored: %v", err)
	}
}
func TestDeadlineBoundsUncooperativeHandlersAndPanic(t *testing.T) {
	r := hooks.NewRegistry()
	release := make(chan struct{})
	defer close(release)
	if err := r.Register("owner", hooks.Spec{ID: "blocked", Name: hooks.BeforeTurn}, hooks.HandlerFunc(func(context.Context, hooks.Request) (hooks.Response, error) { <-release; return hooks.Response{}, nil })); err != nil {
		t.Fatal(err)
	}
	policy := hooks.Defaults()
	policy.TimeoutMs = 10
	policy.MaxConcurrent = 1
	start := time.Now()
	request := hooks.Request{Name: hooks.BeforeTurn}
	if _, _, err := r.Run(context.Background(), request, policy); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("deadline did not bound handler")
	}
	if _, _, err := r.Run(context.Background(), request, policy); err == nil {
		t.Fatal("unbounded timed-out callbacks")
	}
	r2 := hooks.NewRegistry()
	_ = r2.Register("owner", hooks.Spec{ID: "panic", Name: hooks.BeforeTurn}, hooks.HandlerFunc(func(context.Context, hooks.Request) (hooks.Response, error) { panic("boom") }))
	policy.OnError = "continue"
	if _, warnings, err := r2.Run(context.Background(), request, policy); err != nil || len(warnings) != 1 {
		t.Fatalf("panic policy: %v %v", warnings, err)
	}
}
