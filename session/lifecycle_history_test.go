package session_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tomanagle/harness-kit/agent"
	"github.com/tomanagle/harness-kit/event"
	"github.com/tomanagle/harness-kit/runtimes/fake"
	"github.com/tomanagle/harness-kit/session"
)

type delayedCloseRuntime struct {
	*fake.Runtime
	starts  atomic.Int32
	closing chan struct{}
	release chan struct{}
}

func (r *delayedCloseRuntime) Start(ctx context.Context, cfg agent.SessionConfig, h agent.Handler) (agent.Session, error) {
	s, err := r.Runtime.Start(ctx, cfg, h)
	if err != nil {
		return nil, err
	}
	if r.starts.Add(1) == 1 {
		return &delayedCloseSession{Session: s, closing: r.closing, release: r.release}, nil
	}
	return s, nil
}

type delayedCloseSession struct {
	agent.Session
	closing chan struct{}
	release chan struct{}
}

func (s *delayedCloseSession) Close(ctx context.Context) error {
	close(s.closing)
	select {
	case <-s.release:
		return s.Session.Close(ctx)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestRestoreWaitsForPreviousRuntimeClose(t *testing.T) {
	for _, crash := range []bool{false, true} {
		name := "explicit-close"
		if crash {
			name = "failed-runtime"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			st, err := session.NewFileStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			rt := &delayedCloseRuntime{Runtime: fake.New(), closing: make(chan struct{}), release: make(chan struct{})}
			mgr := session.NewManager(agent.NewRegistry(rt), session.Options{Store: st})
			t.Cleanup(func() { mgr.Shutdown(context.Background()) })
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(rt.release) }) }
			t.Cleanup(release)
			info := create(t, mgr, agent.SessionConfig{})
			prompt := "hello"
			if crash {
				prompt = "[crash]"
			}
			turn, err := mgr.Prompt(ctx, info.ID, session.PromptParams{TurnID: "first-turn", Input: agent.Text(prompt)})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := turn.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			closed := make(chan error, 1)
			if !crash {
				go func() { closed <- mgr.Close(ctx, info.ID) }()
			}
			select {
			case <-rt.closing:
			case <-ctx.Done():
				t.Fatal("previous runtime did not enter Close")
			}
			restored := make(chan error, 1)
			go func() { _, err := mgr.Restore(ctx, info.ID); restored <- err }()
			select {
			case err := <-restored:
				t.Fatalf("Restore returned before the previous runtime closed: %v", err)
			case <-time.After(50 * time.Millisecond):
			}
			release()
			if !crash {
				select {
				case err := <-closed:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			select {
			case err := <-restored:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			rec, err := st.Get(ctx, info.ID)
			if err != nil || rec.Info.State != session.StateIdle || rt.starts.Load() != 2 {
				t.Fatalf("restored record was overwritten by old lifecycle: %+v, starts=%d, err=%v", rec.Info, rt.starts.Load(), err)
			}
			evs, truncated, err := mgr.Events(info.ID, 0, 0)
			if err != nil || truncated {
				t.Fatalf("history: truncated=%v err=%v", truncated, err)
			}
			var closes, resumes int
			for i, ev := range evs {
				if ev.Seq != uint64(i+1) {
					t.Fatalf("noncontiguous history at %d: %+v", i, ev)
				}
				if ev.Type == event.SessionClosed {
					closes++
					if resumes > 0 {
						t.Fatal("old session.closed arrived after session restore")
					}
				}
				if d, ok := ev.Data.(event.SessionStartedData); ok && d.Resumed {
					resumes++
				}
				if d, ok := ev.Data.(event.ErrorData); ok && d.Code == "event_store_error" {
					t.Fatalf("conflicting event writers: %+v", d)
				}
			}
			if closes != 1 || resumes != 1 {
				t.Fatalf("expected one close then one restore: closes=%d restores=%d", closes, resumes)
			}
		})
	}
}
