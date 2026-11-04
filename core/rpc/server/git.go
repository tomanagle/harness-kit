package server

import (
	"context"
	"errors"

	"github.com/tomanagle/harness-kit/core/internal/jsonrpc"
	"github.com/tomanagle/harness-kit/core/rpc/protocol"
	"github.com/tomanagle/harness-kit/core/session"
	"github.com/tomanagle/harness-kit/core/workspace"
)

func (c *conn) dispatchGit(ctx context.Context, req *jsonrpc.Request) (result any, err error) {
	defer func() {
		if err == nil {
			return
		}
		var re *jsonrpc.Error
		if errors.As(err, &re) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, session.ErrNotFound) || errors.Is(err, session.ErrBusy) || errors.Is(err, session.ErrClosed) || errors.Is(err, workspace.ErrInvalid) || errors.Is(err, workspace.ErrUnsupported) || errors.Is(err, workspace.ErrReadOnly) || errors.Is(err, workspace.ErrRemoteDisabled) || errors.Is(err, workspace.ErrDirty) || errors.Is(err, workspace.ErrNotOwned) || errors.Is(err, workspace.ErrConflict) {
			return
		}
		err = rpcErr(protocol.CodeGitError, protocol.ReasonGitError, err.Error())
	}()
	p, err := sessionID(req)
	if err != nil {
		return nil, err
	}
	m := c.srv.mgr
	switch req.Method {
	case protocol.MethodGitStatus:
		if err := strictParams(req, &p); err != nil {
			return nil, err
		}
		return m.GitStatus(ctx, p.SessionID)
	case protocol.MethodGitBranches:
		if err := strictParams(req, &p); err != nil {
			return nil, err
		}
		return m.GitBranches(ctx, p.SessionID)
	case protocol.MethodGitDiff:
		var p protocol.GitDiffParams
		if err := strictParams(req, &p); err != nil {
			return nil, err
		}
		return m.GitDiff(ctx, p.SessionID, p.FileDiffParams)
	case protocol.MethodGitLog:
		var p protocol.GitLogParams
		if err := strictParams(req, &p); err != nil {
			return nil, err
		}
		return m.GitLog(ctx, p.SessionID, p.LogParams)
	case protocol.MethodGitStage, protocol.MethodGitUnstage:
		var p protocol.GitPathsParams
		if err := strictParams(req, &p); err != nil {
			return nil, err
		}
		if req.Method == protocol.MethodGitStage {
			return protocol.Empty{}, m.GitStage(ctx, p.SessionID, p.PathsParams)
		}
		return protocol.Empty{}, m.GitUnstage(ctx, p.SessionID, p.PathsParams)
	case protocol.MethodGitCommit:
		var p protocol.GitCommitParams
		if err := strictParams(req, &p); err != nil {
			return nil, err
		}
		return m.GitCommit(ctx, p.SessionID, p.CommitParams)
	case protocol.MethodGitCheckout:
		var p protocol.GitCheckoutParams
		if err := strictParams(req, &p); err != nil {
			return nil, err
		}
		return protocol.Empty{}, m.GitCheckout(ctx, p.SessionID, p.CheckoutParams)
	case protocol.MethodGitFetch, protocol.MethodGitPull, protocol.MethodGitPush:
		var p protocol.GitRemoteParams
		if err := strictParams(req, &p); err != nil {
			return nil, err
		}
		if req.Method == protocol.MethodGitFetch {
			return m.GitFetch(ctx, p.SessionID, p.RemoteParams)
		}
		if req.Method == protocol.MethodGitPull {
			return m.GitPull(ctx, p.SessionID, p.RemoteParams)
		}
		return m.GitPush(ctx, p.SessionID, p.RemoteParams)
	default:
		return nil, jsonrpc.NewError(jsonrpc.CodeMethodNotFound, "unknown Git method", nil)
	}
}
