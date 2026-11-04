package main

import (
	"reflect"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/config"
	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/logging"
	"github.com/tomanagle/harness-kit/core/mcp"
	"github.com/tomanagle/harness-kit/core/permission"
	"github.com/tomanagle/harness-kit/core/rpc/protocol"
	"github.com/tomanagle/harness-kit/core/session"
	"github.com/tomanagle/harness-kit/core/settings"
	"github.com/tomanagle/harness-kit/core/tools"
	"github.com/tomanagle/harness-kit/core/workspace"
)

// A method binds the server's wire name to its actual parameter/result types.
// Coverage checks reject new protocol method constants without a binding.
type methodSpec struct {
	Name           string
	Params, Result reflect.Type
}

func method[P, R any](name string) methodSpec {
	return methodSpec{name, reflect.TypeFor[P](), reflect.TypeFor[R]()}
}

func methods() []methodSpec {
	return []methodSpec{
		method[protocol.SessionIDParams, workspace.RepositoryStatus](protocol.MethodGitStatus),
		method[protocol.GitDiffParams, workspace.FileDiff](protocol.MethodGitDiff),
		method[protocol.SessionIDParams, workspace.BranchesResult](protocol.MethodGitBranches),
		method[protocol.GitLogParams, workspace.LogResult](protocol.MethodGitLog),
		method[protocol.GitPathsParams, protocol.Empty](protocol.MethodGitStage),
		method[protocol.GitPathsParams, protocol.Empty](protocol.MethodGitUnstage),
		method[protocol.GitCommitParams, workspace.CommitResult](protocol.MethodGitCommit),
		method[protocol.GitCheckoutParams, protocol.Empty](protocol.MethodGitCheckout),
		method[protocol.GitRemoteParams, workspace.RemoteResult](protocol.MethodGitFetch),
		method[protocol.GitRemoteParams, workspace.RemoteResult](protocol.MethodGitPull),
		method[protocol.GitRemoteParams, workspace.RemoteResult](protocol.MethodGitPush),
		method[protocol.InitializeParams, protocol.InitializeResult](protocol.MethodInitialize),
		method[any, protocol.Empty](protocol.MethodShutdown),
		method[protocol.SettingsGetParams, settings.Snapshot](protocol.MethodSettingsGet),
		method[settings.ConfigureParams, settings.Snapshot](protocol.MethodSettingsConfigure),
		method[any, session.ResourceStats](protocol.MethodSettingsStats),
		method[protocol.Empty, protocol.Empty](protocol.MethodLogsSubscribe),
		method[protocol.Empty, protocol.Empty](protocol.MethodLogsUnsubscribe),
		method[any, protocol.RuntimeListResult](protocol.MethodRuntimeList),
		method[protocol.RuntimeDiscoverParams, protocol.RuntimeDiscoverResult](protocol.MethodRuntimeDiscover),
		method[protocol.RuntimeSessionsParams, protocol.RuntimeSessionsResult](protocol.MethodRuntimeSessions),
		method[protocol.SessionCreateParams, protocol.SessionResult](protocol.MethodSessionCreate),
		method[protocol.SessionIDParams, protocol.SessionResult](protocol.MethodSessionGet),
		method[protocol.SessionIDParams, session.ContextInfo](protocol.MethodSessionContext),
		method[protocol.SessionIDParams, session.ContextInfo](protocol.MethodSessionCompact),
		method[protocol.SessionListParams, protocol.SessionListResult](protocol.MethodSessionList),
		method[protocol.SessionPromptParams, protocol.SessionPromptResult](protocol.MethodSessionPrompt),
		method[protocol.SessionIDParams, protocol.Empty](protocol.MethodSessionCancel),
		method[protocol.SessionIDParams, protocol.Empty](protocol.MethodSessionClose),
		method[protocol.SessionIDParams, protocol.SessionResult](protocol.MethodSessionRestore),
		method[protocol.SessionIDParams, protocol.Empty](protocol.MethodSessionDelete),
		method[protocol.SessionRenameParams, protocol.SessionResult](protocol.MethodSessionRename),
		method[protocol.SessionForkParams, protocol.SessionResult](protocol.MethodSessionFork),
		method[protocol.SessionArchiveParams, protocol.SessionResult](protocol.MethodSessionArchive),
		method[protocol.SessionTagsParams, protocol.SessionResult](protocol.MethodSessionTags),
		method[protocol.SessionConfigureParams, protocol.SessionResult](protocol.MethodSessionConfigure),
		method[protocol.SessionEventsParams, session.EventPage](protocol.MethodSessionEvents),
		method[protocol.SessionEventsParams, session.EventPage](protocol.MethodSessionEventsPage),
		method[protocol.SessionIDParams, protocol.Empty](protocol.MethodSessionEvict),
		method[protocol.SessionIDParams, protocol.Empty](protocol.MethodSessionSuspend),
		method[protocol.SessionPinParams, protocol.SessionResult](protocol.MethodSessionPin),
		method[protocol.SessionDiffParams, workspace.Diff](protocol.MethodSessionDiff),
		method[protocol.SessionIDParams, protocol.Empty](protocol.MethodWorkspaceRemove),
		method[protocol.ApprovalRespondParams, protocol.Empty](protocol.MethodApprovalRespond),
		method[protocol.ApprovalListParams, protocol.ApprovalListResult](protocol.MethodApprovalList),
		method[protocol.ToolsRegisterParams, protocol.Empty](protocol.MethodToolsRegister),
		method[protocol.ToolsUnregisterParams, protocol.Empty](protocol.MethodToolsUnregister),
		method[any, protocol.ToolsListResult](protocol.MethodToolsList),
		method[protocol.MCPListParams, protocol.MCPListResult](protocol.MethodMCPList),
	}
}

// Public names preserve the TypeScript API and disambiguate Go package names.
// Fields and their JSON names are always read from the server's real types.
func publicNames() map[reflect.Type]string {
	return map[reflect.Type]string{
		reflect.TypeFor[logging.Record]():               "LogRecord",
		reflect.TypeFor[logging.Notification]():         "LogNotification",
		reflect.TypeFor[logging.Source]():               "LogSource",
		reflect.TypeFor[logging.Gap]():                  "LogGap",
		reflect.TypeFor[workspace.FileStatus]():         "GitFileStatus",
		reflect.TypeFor[workspace.Remote]():             "GitRemote",
		reflect.TypeFor[workspace.RepositoryStatus]():   "GitStatus",
		reflect.TypeFor[workspace.FileDiff]():           "GitDiff",
		reflect.TypeFor[workspace.FileDiffParams]():     "GitDiffOptions",
		reflect.TypeFor[workspace.Branch]():             "GitBranch",
		reflect.TypeFor[workspace.BranchesResult]():     "GitBranchesResult",
		reflect.TypeFor[workspace.LogParams]():          "GitLogOptions",
		reflect.TypeFor[workspace.CommitInfo]():         "GitCommitInfo",
		reflect.TypeFor[workspace.LogResult]():          "GitLogResult",
		reflect.TypeFor[workspace.PathsParams]():        "GitPathsOptions",
		reflect.TypeFor[workspace.CommitParams]():       "GitCommitOptions",
		reflect.TypeFor[workspace.CommitResult]():       "GitCommitResult",
		reflect.TypeFor[workspace.CheckoutParams]():     "GitCheckoutOptions",
		reflect.TypeFor[workspace.RemoteParams]():       "GitRemoteOptions",
		reflect.TypeFor[workspace.RemoteResult]():       "GitRemoteResult",
		reflect.TypeFor[agent.Info]():                   "RuntimeInfo",
		reflect.TypeFor[agent.SessionConfig]():          "SessionConfig",
		reflect.TypeFor[config.Settings]():              "TurnConfig",
		reflect.TypeFor[config.Patch]():                 "ConfigPatch",
		reflect.TypeFor[config.Support]():               "ConfigurationSupport",
		reflect.TypeFor[session.Info]():                 "SessionInfo",
		reflect.TypeFor[session.ContextInfo]():          "SessionContext",
		reflect.TypeFor[session.State]():                "SessionState",
		reflect.TypeFor[session.ListOptions]():          "ListSessionsOptions",
		reflect.TypeFor[session.ForkParams]():           "ForkSessionOptions",
		reflect.TypeFor[protocol.SessionCreateParams](): "CreateSessionParams",
		reflect.TypeFor[mcp.Config]():                   "McpConfig",
		reflect.TypeFor[mcp.Server]():                   "McpServer",
		reflect.TypeFor[mcp.Entry]():                    "McpEntry",
		reflect.TypeFor[mcp.Approval]():                 "McpApproval",
		reflect.TypeFor[protocol.NativeMCP]():           "NativeMcp",
		reflect.TypeFor[protocol.MCPListParams]():       "McpListParams",
		reflect.TypeFor[protocol.MCPListResult]():       "McpListResult",
		reflect.TypeFor[permission.Request]():           "ApprovalRequest",
		reflect.TypeFor[permission.Kind]():              "ApprovalKind",
		reflect.TypeFor[permission.Source]():            "ApprovalSource",
		reflect.TypeFor[event.Event]():                  "HarnessEvent",
		reflect.TypeFor[event.Type]():                   "EventType",
		reflect.TypeFor[tools.Result]():                 "ToolResult",
		reflect.TypeFor[tools.Content]():                "ToolContent",
		reflect.TypeFor[settings.Values]():              "SettingsValues",
		reflect.TypeFor[settings.Patch]():               "SettingsPatch",
		reflect.TypeFor[settings.Target]():              "SettingsTarget",
		reflect.TypeFor[settings.ConfigureParams]():     "ConfigureSettingsParams",
		reflect.TypeFor[settings.Snapshot]():            "SettingsSnapshot",
		reflect.TypeFor[settings.Field]():               "SettingsField",
		reflect.TypeFor[settings.Session]():             "SessionSettings",
		reflect.TypeFor[settings.Process]():             "ProcessSettings",
		reflect.TypeFor[settings.Logging]():             "LoggingSettings",
		reflect.TypeFor[settings.Approvals]():           "ApprovalSettings",
		reflect.TypeFor[settings.Memory]():              "MemorySettings",
		reflect.TypeFor[settings.History]():             "HistorySettings",
		reflect.TypeFor[settings.Streams]():             "StreamSettings",
		reflect.TypeFor[settings.ACPRuntime]():          "AcpRuntimeSettings",
		reflect.TypeFor[config.BudgetLimits]():          "BudgetLimits",
		reflect.TypeFor[event.BudgetUsage]():            "BudgetUsage",
		reflect.TypeFor[event.BudgetExceededData]():     "BudgetExceeded",
		reflect.TypeFor[workspace.Settings]():           "WorkspaceSettings",
		reflect.TypeFor[workspace.Binding]():            "WorkspaceBinding",
		reflect.TypeFor[workspace.Snapshot]():           "WorkspaceSnapshot",
		reflect.TypeFor[workspace.Diff]():               "WorkspaceDiff",
	}
}
