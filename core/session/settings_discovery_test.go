package session_test

import (
	"context"
	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/config"
	"github.com/tomanagle/harness-kit/core/runtimes/claude"
	"github.com/tomanagle/harness-kit/core/runtimes/fake"
	"github.com/tomanagle/harness-kit/core/session"
	"github.com/tomanagle/harness-kit/core/settings"
	"testing"
	"time"
)

type describedRuntime struct {
	*fake.Runtime
	entered, release chan struct{}
}

func (r *describedRuntime) DescribeConfiguration(ctx context.Context, _ agent.SessionConfig) (map[string]config.Descriptor, error) {
	close(r.entered)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-r.release:
	}
	return map[string]config.Descriptor{"model": {Type: "string", Choices: config.Choices("reported-model"), Source: "reported", AllowCustom: true}}, nil
}
func TestSettingsDiscoveryIsReadOnlyAndDoesNotHoldLifecycleLock(t *testing.T) {
	rt := &describedRuntime{Runtime: fake.New(), entered: make(chan struct{}), release: make(chan struct{})}
	m := session.NewManager(agent.NewRegistry(rt), session.Options{})
	defer m.Shutdown(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second*3)
	defer cancel()
	type result struct {
		snapshot settings.Snapshot
		err      error
	}
	done := make(chan result, 1)
	go func() { s, e := m.Settings(ctx, settings.Target{Runtime: "fake"}); done <- result{s, e} }()
	<-rt.entered
	if _, err := m.Create(ctx, session.CreateParams{Runtime: "fake"}); err != nil {
		t.Fatal(err)
	}
	close(rt.release)
	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}
	field := got.snapshot.Fields["session.model"]
	if field.Writable || field.Source != "reported" || len(field.Choices) != 1 || field.Choices[0].Value != "reported-model" {
		t.Fatalf("discovery field: %+v", field)
	}
	if _, err := m.ConfigureSettings(ctx, settings.ConfigureParams{Target: settings.Target{Runtime: "fake"}, Patch: resourcePatch(t, `{"session":{"model":"no"}}`)}); err == nil {
		t.Fatal("discovery target accepted mutation")
	}
	if _, err := m.Settings(ctx, settings.Target{Model: "model"}); err == nil {
		t.Fatal("model without runtime accepted")
	}
}

func TestSettingsDiscoveryDoesNotLeakOtherRuntimeFields(t *testing.T) {
	m := session.NewManager(agent.NewRegistry(fake.New(), claude.New()), session.Options{})
	defer m.Shutdown(context.Background())
	ctx := context.Background()
	if _, err := m.ConfigureSettings(ctx, settings.ConfigureParams{Patch: resourcePatch(t, `{"session":{"runtime":"claude"}}`)}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := m.Settings(ctx, settings.Target{Runtime: "fake"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := snapshot.Fields["session.options.permissionMode"]; ok {
		t.Fatal("Claude options leaked into fake runtime discovery")
	}
}
