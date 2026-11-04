package codex

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/internal/jsonrpc"
)

// CanCompact reports the native manual-compaction extension. Older app-servers
// that reject the method are remembered for this session.
func (s *Session) CanCompact() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !s.compactUnsupported
}

// Compact requests Codex's native compaction operation, not a user prompt. The
// RPC acknowledgement only means accepted: retain the active slot until its
// native turn completes, or cancellation is confirmed.
func (s *Session) Compact(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-s.exited:
		return s.dead()
	default:
	}
	at := &activeTurn{ctx: ctx, done: make(chan turnOutcome, 1), started: make(chan struct{})}
	s.mu.Lock()
	if s.compactUnsupported {
		s.mu.Unlock()
		return agent.ErrUnsupported
	}
	if s.active != nil {
		s.mu.Unlock()
		return fmt.Errorf("codex: an operation is already active")
	}
	s.active = at
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.active == at {
			s.active = nil
		}
		s.mu.Unlock()
	}()

	startCtx, cancel := context.WithTimeout(ctx, s.rt.startTimeout)
	err := s.conn.Call(startCtx, "thread/compact/start", map[string]any{"threadId": s.threadID}, nil)
	cancel()
	if err != nil {
		var re *jsonrpc.Error
		if errors.As(err, &re) {
			if re.Code == jsonrpc.CodeMethodNotFound {
				s.mu.Lock()
				s.compactUnsupported = true
				s.mu.Unlock()
				return fmt.Errorf("%w: %v", agent.ErrUnsupported, err)
			}
			return fmt.Errorf("codex: thread/compact/start: %w", err)
		}
		// A lost/timed-out acknowledgement leaves acceptance uncertain. Stop
		// the native operation before allowing another prompt on this thread.
		return s.interruptCompaction(at, err)
	}
	select {
	case out := <-at.done:
		return compactionOutcome(out)
	case <-s.exited:
		return s.dead()
	case <-ctx.Done():
		return s.interruptCompaction(at, ctx.Err())
	}
}

func compactionOutcome(out turnOutcome) error {
	switch out.status {
	case "completed":
		return nil
	case "interrupted":
		return context.Canceled
	default:
		message := "native compaction failed"
		if out.err != nil && out.err.Message != "" {
			message = out.err.Message
		}
		return fmt.Errorf("codex: %s", message)
	}
}

func (s *Session) interruptCompaction(at *activeTurn, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	completed := func(out turnOutcome) error {
		if out.status == "completed" {
			return nil // Native completion beat cancellation.
		}
		return cause
	}
	select {
	case out := <-at.done:
		return completed(out)
	case <-s.exited:
		return errors.Join(cause, s.dead())
	case <-at.started:
	case <-ctx.Done():
		_ = s.Close(context.Background())
		return errors.Join(cause, agent.ErrSessionDead)
	}
	s.mu.Lock()
	id := at.codexID
	s.mu.Unlock()
	_ = s.conn.Call(ctx, "turn/interrupt", map[string]any{"threadId": s.threadID, "turnId": id}, nil)
	select {
	case out := <-at.done:
		return completed(out)
	case <-s.exited:
		return errors.Join(cause, s.dead())
	case <-ctx.Done():
		// An unconfirmed background compaction must not race a future prompt.
		_ = s.Close(context.Background())
		return errors.Join(cause, agent.ErrSessionDead)
	}
}

// Invalidate the last request's occupancy when compaction starts. A subsequent
// native token update can replace it with the compacted request's count. Legacy
// runtimes only send the completion notification, so invalidate there when no
// item lifecycle was observed. A missing count is not a reported zero.
func (s *Session) compactionBoundary(turnID string, start bool) {
	s.mu.Lock()
	unknown := start || s.active == nil || !s.active.compactionSeen
	if start && s.active != nil {
		s.active.compactionSeen = true
	}
	var window *int64
	if s.windowTokens != nil {
		value := *s.windowTokens
		window = &value
	}
	s.mu.Unlock()
	if start {
		// Let the host capture its last known occupancy as the lifecycle's
		// Before snapshot before invalidating that observation.
		s.emit(turnID, event.RuntimeStatus, event.RuntimeStatusData{Status: "compacting"})
	}
	if unknown {
		s.emit(turnID, event.ContextUsage, event.ContextUsageData{WindowTokens: window, Basis: "unknown", Stale: true})
	}
}

var _ agent.Compactor = (*Session)(nil)
