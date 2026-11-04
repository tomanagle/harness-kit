package agent

import (
	"context"
	"testing"
	"time"
)

type deadlineRuntime struct{ observed chan time.Duration }

func (r deadlineRuntime) Info() Info { return Info{Name: "deadline"} }
func (r deadlineRuntime) Discover(ctx context.Context) (Discovery, error) {
	deadline, _ := ctx.Deadline()
	r.observed <- time.Until(deadline)
	return Discovery{}, nil
}
func (deadlineRuntime) Start(context.Context, SessionConfig, Handler) (Session, error) {
	return nil, ErrUnsupported
}

func TestRegistryDiscoveryTimeoutIsPerInstance(t *testing.T) {
	for _, timeout := range []time.Duration{time.Second, 3 * time.Second} {
		runtime := deadlineRuntime{observed: make(chan time.Duration, 1)}
		registry := NewRegistryWithOptions(RegistryOptions{DiscoverTimeout: timeout}, runtime)
		if _, err := registry.Discover(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := <-runtime.observed; got <= timeout-time.Second/2 || got > timeout {
			t.Fatalf("timeout=%v, got=%v", timeout, got)
		}
	}
}
