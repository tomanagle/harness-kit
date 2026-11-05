package session

import (
	"context"
	"errors"

	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/permission"
)

// RespondApproval resolves a pending approval request.
func (m *Manager) RespondApproval(approvalID string, d permission.Decision) error {
	return m.broker.Resolve(approvalID, d)
}

// PendingApprovals lists unresolved approval requests (all sessions if
// sessionID is empty).
func (m *Manager) PendingApprovals(sessionID string) []permission.Request {
	return m.broker.Pending(sessionID)
}

// RequestApproval implements agent.Handler.
func (ms *managed) RequestApproval(ctx context.Context, req permission.Request) (permission.Decision, error) {
	ms.mu.Lock()
	req.SessionID = ms.info.ID
	req.Runtime = ms.info.Runtime
	if req.TurnID == "" && ms.turn != nil {
		req.TurnID = ms.turn.ID
	}
	ms.mu.Unlock()
	if req.ID == "" {
		req.ID = NewID("apr_")
	}

	resolved := func(d permission.Decision, src permission.Source, withReq bool) {
		data := event.ApprovalResolvedData{ApprovalID: req.ID, Decision: d, Source: src}
		if withReq {
			r := req
			data.Request = &r
		}
		ms.emit(event.Event{Type: event.ApprovalResolved, TurnID: req.TurnID, Data: data})
	}

	if p := ms.m.opts.Policy; p != nil {
		if d, ok := p.Evaluate(ctx, req); ok {
			resolved(d, permission.SourcePolicy, true)
			return d, nil
		}
	}

	_, d, err := ms.m.broker.Ask(ctx, req, ms.m.ApprovalTimeout(), func(r permission.Request) {
		ms.emit(event.Event{Type: event.ApprovalRequested, TurnID: r.TurnID, Data: r})
	})
	switch {
	case err == nil:
		resolved(d, permission.SourceHost, false)
		return d, nil
	case errors.Is(err, permission.ErrTimeout):
		d = permission.Decision{Outcome: permission.Deny, Message: "approval timed out"}
		resolved(d, permission.SourceTimeout, false)
		return d, nil
	default:
		resolved(permission.Decision{Outcome: permission.Abort}, permission.SourceCancelled, false)
		return permission.Decision{}, err
	}
}
