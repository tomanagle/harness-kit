package protocol

import "github.com/tomanagle/harness-kit/core/workspace"

// Git methods accept a session ID. The host resolves its saved repository; a
// client cannot substitute an arbitrary filesystem root in a request.
type GitDiffParams struct {
	SessionID string `json:"sessionId"`
	workspace.FileDiffParams
}
type GitLogParams struct {
	SessionID string `json:"sessionId"`
	workspace.LogParams
}
type GitPathsParams struct {
	SessionID string `json:"sessionId"`
	workspace.PathsParams
}
type GitCommitParams struct {
	SessionID string `json:"sessionId"`
	workspace.CommitParams
}
type GitCheckoutParams struct {
	SessionID string `json:"sessionId"`
	workspace.CheckoutParams
}
type GitRemoteParams struct {
	SessionID string `json:"sessionId"`
	workspace.RemoteParams
}
