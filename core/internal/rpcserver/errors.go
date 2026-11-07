package rpcserver

import (
	"context"
	"errors"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/hooks"
	"github.com/tomanagle/harness-kit/core/internal/jsonrpc"
	"github.com/tomanagle/harness-kit/core/permission"
	"github.com/tomanagle/harness-kit/core/rpc/protocol"
	"github.com/tomanagle/harness-kit/core/session"
	"github.com/tomanagle/harness-kit/core/settings"
	"github.com/tomanagle/harness-kit/core/workspace"
)

func rpcErr(code int, reason, msg string) *jsonrpc.Error {
	return jsonrpc.NewError(code, msg, protocol.ErrorData{Reason: reason})
}

func invalidParams(msg string) *jsonrpc.Error {
	return rpcErr(jsonrpc.CodeInvalidParams, protocol.ReasonInvalidParams, msg)
}

// toRPCError maps library errors to protocol errors.
func toRPCError(err error) error {
	var re *jsonrpc.Error
	switch {
	case errors.Is(err, workspace.ErrNotFound):
		return rpcErr(protocol.CodeSessionNotFound, "turn_diff_not_found", err.Error())
	case errors.Is(err, workspace.ErrUnsupported):
		return rpcErr(protocol.CodeUnsupported, protocol.ReasonUnsupported, err.Error())
	case errors.Is(err, workspace.ErrInvalid):
		return invalidParams(err.Error())
	case errors.Is(err, workspace.ErrConflict):
		return rpcErr(protocol.CodeGitConflict, protocol.ReasonGitConflict, err.Error())
	case errors.Is(err, workspace.ErrDirty) || errors.Is(err, workspace.ErrNotOwned) || errors.Is(err, workspace.ErrReadOnly) || errors.Is(err, workspace.ErrRemoteDisabled):
		return rpcErr(protocol.CodePolicyViolation, protocol.ReasonPolicyViolation, err.Error())
	case err == nil:
		return nil
	case errors.Is(err, hooks.ErrFailed):
		return rpcErr(protocol.CodeHookFailed, protocol.ReasonHookFailed, err.Error())
	case errors.Is(err, settings.ErrPersistence):
		return rpcErr(protocol.CodeSettingsPersistence, protocol.ReasonSettingsPersistence, err.Error())
	case errors.As(err, &re):
		return re
	case errors.Is(err, session.ErrNotFound):
		return rpcErr(protocol.CodeSessionNotFound, protocol.ReasonSessionNotFound, err.Error())
	case errors.Is(err, session.ErrRuntimeNotFound):
		return rpcErr(protocol.CodeRuntimeNotFound, protocol.ReasonRuntimeNotFound, err.Error())
	case errors.Is(err, session.ErrBusy):
		return rpcErr(protocol.CodeSessionBusy, protocol.ReasonSessionBusy, err.Error())
	case errors.Is(err, session.ErrClosed):
		return rpcErr(protocol.CodeSessionClosed, protocol.ReasonSessionClosed, err.Error())
	case errors.Is(err, session.ErrDuplicateTurn):
		return rpcErr(jsonrpc.CodeInvalidParams, protocol.ReasonDuplicateTurn, err.Error())
	case errors.Is(err, session.ErrUnsafe):
		return rpcErr(protocol.CodePolicyViolation, protocol.ReasonPolicyViolation, err.Error())
	case errors.Is(err, permission.ErrNotFound):
		return rpcErr(protocol.CodeApprovalNotFound, protocol.ReasonApprovalNotFound, err.Error())
	case errors.Is(err, permission.ErrUnsupportedOutcome):
		return rpcErr(protocol.CodeInvalidDecision, protocol.ReasonInvalidDecision, err.Error())
	case errors.Is(err, agent.ErrNotInstalled):
		return rpcErr(protocol.CodeRuntimeUnavailable, protocol.ReasonRuntimeUnavailable, err.Error())
	case errors.Is(err, agent.ErrUnsupported):
		return rpcErr(protocol.CodeUnsupported, protocol.ReasonUnsupported, err.Error())
	case errors.Is(err, session.ErrInvalidTitle), errors.Is(err, agent.ErrInvalidConfig):
		return rpcErr(jsonrpc.CodeInvalidParams, protocol.ReasonInvalidParams, err.Error())
	case errors.Is(err, settings.ErrConflict):
		return rpcErr(protocol.CodeSettingsConflict, protocol.ReasonSettingsConflict, err.Error())
	case errors.Is(err, settings.ErrRestartRequired):
		return rpcErr(protocol.CodeRestartRequired, protocol.ReasonRestartRequired, err.Error())
	case errors.Is(err, settings.ErrNewSessionRequired):
		return rpcErr(protocol.CodeNewSessionRequired, protocol.ReasonNewSessionRequired, err.Error())
	case errors.Is(err, settings.ErrScope):
		return rpcErr(protocol.CodeUnsupported, protocol.ReasonUnsupported, err.Error())
	case errors.Is(err, session.ErrEventTooLarge):
		return rpcErr(protocol.CodeEventTooLarge, protocol.ReasonEventTooLarge, err.Error())
	case errors.Is(err, session.ErrDurableHistoryUnavailable):
		return rpcErr(protocol.CodeDurableHistoryUnavailable, protocol.ReasonDurableHistoryUnavailable, err.Error())
	case errors.Is(err, session.ErrBudgetExceeded):
		return rpcErr(protocol.CodeBudgetExceeded, protocol.ReasonBudgetExceeded, err.Error())
	case errors.Is(err, session.ErrForkSourceChanged):
		return rpcErr(protocol.CodeForkSourceChanged, protocol.ReasonForkSourceChanged, err.Error())
	case errors.Is(err, session.ErrNotResumable):
		return rpcErr(protocol.CodeNotResumable, protocol.ReasonNotResumable, err.Error())
	case errors.Is(err, context.Canceled):
		return jsonrpc.NewError(protocol.CodeRequestCancelled, "request cancelled", nil)
	}
	return rpcErr(protocol.CodeRuntimeError, protocol.ReasonRuntimeError, err.Error())
}
