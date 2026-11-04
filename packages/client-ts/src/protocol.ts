// Wire types for the Harness Kit sidecar protocol (version 1).
// Mirrors github.com/tomanagle/harness-kit/rpc/protocol. See docs/protocol.md.

export const PROTOCOL_VERSION = 1;

export const ErrorCodes = {
  ParseError: -32700,
  InvalidRequest: -32600,
  MethodNotFound: -32601,
  InvalidParams: -32602,
  InternalError: -32603,
  ServerNotInitialized: -32002,
  RequestCancelled: -32800,
  SessionNotFound: -32001,
  RuntimeNotFound: -32003,
  RuntimeUnavailable: -32004,
  SessionBusy: -32005,
  SessionClosed: -32006,
  ApprovalNotFound: -32007,
  UnsupportedVersion: -32008,
  PolicyViolation: -32009,
  InvalidDecision: -32010,
  RuntimeError: -32011,
  Unsupported: -32012,
  NotResumable: -32013,
  SettingsConflict: -32014,
  RestartRequired: -32015,
  NewSessionRequired: -32016,
  EventTooLarge: -32017,
} as const;

export type ErrorReason =
  | "not_initialized"
  | "session_not_found"
  | "runtime_not_found"
  | "runtime_unavailable"
  | "session_busy"
  | "session_closed"
  | "approval_not_found"
  | "unsupported_version"
  | "policy_violation"
  | "invalid_decision"
  | "runtime_error"
  | "duplicate_turn"
  | "invalid_params"
  | "unsupported"
  | "not_resumable"
  | "duplicate_tool"
  | "settings_conflict"
  | "restart_required"
  | "new_session_required"
  | "event_too_large"
  | "fork_source_changed"
  | "budget_exceeded";

export interface Capabilities {
  images?: boolean;
  fork?: boolean;
  forkDeferred?: boolean;
  streaming: boolean;
  approvals: boolean;
  resume: boolean;
  cancel: boolean;
  usage: boolean;
  cost: boolean;
  /** The runtime can list its own stored sessions (runtimes.sessions). */
  listSessions: boolean;
  /** The runtime accepts per-session MCP servers (CreateSessionParams.mcp). */
  mcp: boolean;
  /** Per-setting configuration lifetimes; an absent entry is unsupported. */
  configuration?: Record<string, ConfigurationSupport>;
}

export interface ConfigurationSupport {
  startup: boolean;
  session: boolean;
  turn: boolean;
  live: boolean;
}

/** Complete model/effort/native settings for a session or accepted turn. */
export interface TurnConfig {
  model?: string;
  effort?: string;
  options?: Record<string, unknown>;
}

/**
 * Omitted fields inherit. Empty strings explicitly clear model/effort.
 * Native option values retain false, 0 and []. Reset names are model,
 * effort, or options.NAME; they remove this layer's override.
 */
export interface ConfigPatch {
  model?: string;
  effort?: string;
  options?: Record<string, unknown>;
  reset?: string[];
}

/** One MCP server in Harness Kit's portable format (see docs/mcp.md). */
export interface McpServer {
  /** "stdio" (default) or "http" (streamable HTTP). */
  type?: "stdio" | "http" | "streamable-http";
  command?: string;
  args?: string[];
  /** Values may reference the environment: "${TOKEN}" or "${TOKEN:-default}". */
  env?: Record<string, string>;
  cwd?: string;
  url?: string;
  headers?: Record<string, string>;
  /** Exact tool names. `allow` (if set) is the complete list of usable tools. */
  tools?: { allow?: string[]; deny?: string[] };
  /** "auto" (default): run tools without asking. "ask": request approval. */
  approval?: "auto" | "ask";
  /** Only load for these runtimes. */
  runtimes?: string[];
  startupTimeoutSec?: number;
  toolTimeoutSec?: number;
  /** Remove a server defined in a lower layer (user/project config). */
  disabled?: boolean;
}

/** One kit MCP server in an mcp.list result. */
export interface McpEntry {
  name: string;
  source: "user" | "project";
  /** false for servers from an untrusted project file. */
  loaded: boolean;
  note?: string;
  server: McpServer;
}

/** The MCP servers configured in a runtime itself. */
export interface NativeMcp {
  runtime: string;
  supported: boolean;
  servers: string[];
  error?: string;
}

export interface McpListResult {
  /** Whether the sidecar merges kit config files into sessions (serve --kit-mcp). */
  enabled: boolean;
  servers: McpEntry[];
  inheritNative: boolean;
  userFile?: string;
  projectFile?: string;
  projectTrusted: boolean;
  native: NativeMcp[];
}

export interface McpConfig {
  mcpServers?: Record<string, McpServer>;
  /** Also load the servers configured in the runtime itself. Default true. */
  inheritNative?: boolean;
}

export interface RuntimeInfo {
  name: string;
  displayName: string;
  description?: string;
  capabilities: Capabilities;
}

export type AuthState = "authenticated" | "unauthenticated" | "unknown";

export interface Discovery {
  runtime: string;
  installed: boolean;
  path?: string;
  version?: string;
  auth: { state: AuthState; method?: string; plan?: string };
  ready: boolean;
  problems?: string[];
  notes?: string[];
  capabilities: Capabilities;
}

export interface ErrorInfo {
  code?: string;
  message: string;
  retryable?: boolean;
}

export type SessionState = "starting" | "idle" | "running" | "closing" | "closed" | "failed" | "suspending" | "suspended";

export interface SessionInfo {
	workspace?: WorkspaceBinding;
	workspaceSettings?: WorkspaceSettings;
  budget?: BudgetLimits;
  usage?: BudgetUsage;
  archived?: boolean;
  forkedFrom?: string;
  forkedAtSeq?: number;
  inheritedTurns?: number;
  forkPending?: boolean;
  /** Changes only when session settings or pinning change, independently of streamed events. */
  settingsRevision?: number;
  sessionId: string;
  runtime: string;
  runtimeSessionId?: string;
  state: SessionState;
  pinned?: boolean;
  activeTurnId?: string;
  cwd?: string;
  model?: string;
  metadata?: Record<string, string>;
  createdAt: string;
  updatedAt: string;
  lastSeq: number;
  error?: ErrorInfo;
  /** One-line name: the start of the first prompt, unless set at creation or with sessions.rename. */
  title?: string;
  /** Prompts sent in this session. */
  turns: number;
  /** Status of the most recent finished turn. */
  lastStatus?: TurnStatus;
  /** Current defaults used for subsequent turns. */
  config?: TurnConfig;
}

/** Mutable session fields carried by session.updated; absent optional fields are cleared. */
export interface SessionUpdatedData {
  budget?: BudgetLimits;
  usage?: BudgetUsage;
  archived?: boolean;
  metadata?: Record<string, string>;
  forkPending?: boolean;
  settingsRevision?: number;
  title: string;
  state: SessionState;
  pinned?: boolean;
  activeTurnId?: string;
  runtimeSessionId?: string;
  turns: number;
  lastStatus?: TurnStatus;
  updatedAt: string;
  error?: ErrorInfo;
  config?: TurnConfig;
}

/** A session stored by a runtime itself (e.g. a Codex thread). */
export interface RuntimeSession {
  /** Pass as `resumeId` to sessions.create. */
  id: string;
  cwd?: string;
  title?: string;
  model?: string;
  source?: string;
  createdAt: string;
  updatedAt: string;
}

export interface InputPart {
  type: "text" | "image";
  text?: string;
  /** Absolute local path, for images. */
  path?: string;
}

export type Outcome = "allow" | "allow_session" | "deny" | "abort";

export type ApprovalKind = "command" | "file_change" | "tool" | "network" | "permissions" | "other";

export interface ApprovalRequest {
  approvalId: string;
  sessionId?: string;
  turnId?: string;
  runtime?: string;
  kind: ApprovalKind;
  tool?: string;
  title: string;
  command?: string;
  cwd?: string;
  paths?: string[];
  reason?: string;
  input?: unknown;
  /** Outcomes the runtime supports; absent means all. */
  outcomes?: Outcome[];
}

export interface Decision {
  outcome: Outcome;
  message?: string;
  updatedInput?: unknown;
}

export type TurnStatus = "completed" | "cancelled" | "failed";
export type ToolKind = "command" | "file_change" | "file_read" | "search" | "web_search" | "mcp" | "subagent" | "other";
export type ToolStatus = "completed" | "failed" | "declined";

export interface UsageData {
  mode?: "snapshot" | "delta";
  tokensKnown?: boolean;
  scope: "turn" | "session";
  inputTokens?: number;
  outputTokens?: number;
  cachedInputTokens?: number;
  cacheWriteTokens?: number;
  reasoningTokens?: number;
  totalTokens?: number;
  contextWindow?: number;
  /** The runtime's own estimate, not a bill. */
  costUsd?: number;
}

export interface TurnCompletedData {
  budget?: BudgetExceeded;
  status: TurnStatus;
  error?: ErrorInfo;
  usage?: UsageData;
  runtimeTurnId?: string;
}

interface EventOf<T extends string, D> {
  seq: number;
  sessionId: string;
  turnId?: string;
  type: T;
  time: string;
  data: D;
}

/**
 * Every event a session can emit. Consumers must tolerate event types not
 * listed here (new types are added without a protocol version bump).
 */
export type HarnessEvent =
	| EventOf<"workspace.diff", WorkspaceDiff>
  | EventOf<"session.started", { runtime: string; runtimeSessionId?: string; resumed?: boolean; model?: string }>
  | EventOf<"session.updated", SessionUpdatedData>
  | EventOf<"session.closed", { reason?: string; error?: ErrorInfo }>
  | EventOf<"turn.started", { text?: string; input?: InputPart[]; config?: TurnConfig; budget?: BudgetLimits; workspace?: WorkspaceSettings }>
  | EventOf<"turn.completed", TurnCompletedData>
  | EventOf<"message.delta", { itemId: string; text: string; parentCallId?: string }>
  | EventOf<"message.completed", { itemId: string; role: string; text: string; parentCallId?: string }>
  | EventOf<"reasoning.delta", { itemId: string; text: string }>
  | EventOf<"reasoning.completed", { itemId: string; text: string }>
  | EventOf<"tool.started", { callId: string; name: string; kind: ToolKind; title?: string; input?: unknown; parentCallId?: string }>
  | EventOf<"tool.output", { callId: string; output: string }>
  | EventOf<"tool.completed", { callId: string; name?: string; status: ToolStatus; output?: string; exitCode?: number; error?: string }>
  | EventOf<"approval.requested", ApprovalRequest>
  | EventOf<"approval.resolved", { approvalId: string; decision: Decision; source: "host" | "policy" | "timeout" | "cancelled"; request?: ApprovalRequest }>
  | EventOf<"usage", UsageData>
  | EventOf<"budget.exceeded", BudgetExceeded>
  | EventOf<"runtime.status", { status: string; message?: string; detail?: unknown }>
  | EventOf<"error", ErrorInfo>
  | EventOf<"runtime.raw", { runtime: string; kind: string; payload: unknown }>
  | EventOf<"stream.gap", { fromSeq: number; toSeq: number }>;

export type EventType = HarnessEvent["type"];
export type EventData<T extends EventType> = Extract<HarnessEvent, { type: T }>["data"];

export interface InitializeResult {
  protocolVersion: number;
  serverInfo: { name: string; version: string };
  capabilities: { runtimes: RuntimeInfo[]; allowUnsafe: boolean; eventLogSize: number; tools: boolean; settings?: boolean; workspace?: boolean };
}

/** A tool registered by the client; its handler runs in the client. */
export interface ToolSpec {
  /** Letters, digits, "_" and "-", up to 64 characters. */
  name: string;
  description?: string;
  /** JSON Schema for the arguments object. Arguments are validated before your handler runs. */
  inputSchema?: Record<string, unknown>;
  /** "ask": Harness Kit requests approval (approval.requested) before each call. */
  approval?: "auto" | "ask";
  readOnly?: boolean;
  timeoutMs?: number;
}

/** A tool result, in MCP's CallToolResult shape. */
export interface ToolResult {
  content: { type: "text"; text: string }[];
  structuredContent?: unknown;
  isError?: boolean;
}

export interface ToolCallParams {
  sessionId: string;
  turnId?: string;
  name: string;
  arguments: unknown;
}

export interface CreateSessionParams {
  /** Unified settings; the session section overrides host defaults. */
  settings?: SettingsPatch;
  runtime?: string;
  /** Absolute working directory for the agent. */
  cwd?: string;
  model?: string;
  effort?: string;
  /** Runtime-native session ID to resume (Codex thread ID, Claude session ID). */
  resumeId?: string;
  instructions?: string;
  rawEvents?: boolean;
  /** Runtime-specific options; see docs/runtimes.md. */
  options?: Record<string, unknown>;
  metadata?: Record<string, string>;
  /** Names the session (one line, up to 200 characters); default: the start of the first prompt. */
  title?: string;
  /** Host tools (registered with kit.tools.register) this session may use; default all. */
  hostTools?: string[];
  /** Give the session no host tools and no Harness Kit bridge. */
  disableHostTools?: boolean;
  /**
   * MCP servers for this session, translated for whichever runtime runs it.
   * A sidecar started with --kit-mcp also merges the user's and trusted
   * project harness-kit.json servers.
   */
  mcp?: McpConfig;
}


/** Settings shared by startup, host updates, session updates and turn input. */
export interface SettingsValues {
	workspace: WorkspaceSettings;
  budget: BudgetLimits;
  process: {
    store: string;
    runtimes: string[];
    /** Restart-only full-map replacement. Numeric overrides must be positive. */
    acp?: Record<string, AcpRuntimeSettings>;
    codexPath: string;
    claudePath: string;
    gitPath: string;
    fakeDelayMs: number;
    shutdownTimeoutMs: number;
    kitMcp: boolean;
    allowUnsafe: boolean;
  };
  logging: { level: "debug" | "info" | "warn" | "error" };
  approvals: { timeoutMs: number };
  memory: {
    maxCacheBytes: number | null;
    maxSessionCacheBytes: number | null;
    maxCachedSessions: number | null;
    evictAfterIdleMs: number | null;
    suspendAfterIdleMs: number | null;
    resumeOnPrompt: boolean;
    eventLogSize: number;
    maxClosedSessions: number;
    /** Only writable for a session target. */
    pinned: boolean;
  };
  history: { maxPageEvents: number; maxPageBytes: number | null };
  streams: { maxQueueEvents: number; maxQueueBytes: number | null };
  session: SessionSettings;
}

export interface SessionSettings extends TurnConfig {
  runtime?: string;
  cwd?: string;
  resumeId?: string;
  instructions?: string;
  rawEvents?: boolean;
  mcp?: McpConfig | null;
  hostTools?: string[] | null;
  disableHostTools?: boolean;
}

export interface AcpRuntimeSettings {
  command: string;
  args?: string[];
  env?: Record<string, string>;
  startTimeoutMs?: number;
  cancelTimeoutMs?: number;
  maxMessageBytes?: number;
  maxImageBytes?: number;
  maxRetainedOutputBytes?: number;
}

/** Location fields require a new session; capture policy applies next turn. */
export interface WorkspaceSettings {
  mode: "existing" | "worktree";
  repo: string;
  base: string;
  path: string;
  root: string;
  captureDiff: boolean;
  maxDiffBytes: number;
  maxSnapshotBytes: number;
  maxFiles: number;
  timeoutMs: number;
}
export interface WorkspaceBinding {
  kind: string;
  sessionId: string;
  repo: string;
  path: string;
  gitDir: string;
  commonDir: string;
  baseCommit?: string;
  owned: boolean;
}
export interface WorkspaceSnapshot {
  tree: string;
  index: string;
  head?: string;
  capturedAt: string;
  files: number;
  bytes: number;
}
/** Observed before/after changes, including concurrent external edits. */
export interface WorkspaceDiff {
  sessionId: string;
  turnId: string;
  binding: WorkspaceBinding;
  before?: WorkspaceSnapshot;
  after?: WorkspaceSnapshot;
  complete: boolean;
  error?: string;
  maxDiffBytes: number;
  /** Includes nonignored untracked files. */
  workingTree: string;
  /** Separate changes to staged content. */
  index: string;
  truncated: boolean;
  /** Invalid UTF-8 patch bytes were replaced for JSON display. */
  encodingLossy?: boolean;
}

/** Omitted fields remain unchanged; null disables explicitly nullable limits. */
export type SettingsPatch = {
  [K in keyof SettingsValues]?: Partial<SettingsValues[K]>;
};

export interface SettingsTarget {
  /** Omit for host settings/defaults. */
  sessionId?: string;
}

export interface ConfigureSettingsParams {
  target?: SettingsTarget;
  patch: SettingsPatch;
  /** Dotted field paths to restore from startup/session-creation settings. */
  reset?: string[];
  /** Reject stale writes from another settings window. */
  expectedRevision?: number;
}

export interface SettingsField {
  type: string;
  unit?: string;
  when: string;
  scopes: string[];
  nullable?: boolean;
  minimum?: number;
  writable: boolean;
}

export interface SettingsSnapshot {
  target?: SettingsTarget;
  revision: number;
  values: SettingsValues;
  fields: Record<string, SettingsField>;
  deferred?: string[];
  /** A slow observer missed changes to one or more targets. Refresh displayed settings. */
  resync?: boolean;
}

/** A bounded history page; truncated means missing data, not pagination. */
export interface EventPage {
  events: HarnessEvent[];
  nextSeq: number;
  hasMore: boolean;
  truncated: boolean;
}

export interface ResourceStats {
  cachedBytes: number;
  cachedEvents: number;
  unsavedEvents: number;
  cachedSessions: number;
  residentSessions: number;
  deferred?: string[];
}

/** Omitted archived preserves the complete catalog. Tags all match. */
export interface ListSessionsOptions {
 query?: string;
 searchTranscript?: boolean;
 tags?: string[];
 archived?: boolean;
 runtime?: string;
 cwd?: string;
 limit?: number;
}
export interface ForkSessionOptions {
 title?: string;
 settings?: SettingsPatch;
}

/** Null disables a limit; zero rejects further work. Limits are observed thresholds. */
export interface BudgetLimits {
  maxSessionTokens: number | null;
  maxSessionCostUsd: number | null;
  maxTurnTokens: number | null;
  maxTurnCostUsd: number | null;
}
export interface BudgetUsage {
  totalTokens: number;
  /** Runtime estimate; omitted means unavailable. */
  costUsd?: number;
  tokensComplete: boolean;
  costComplete: boolean;
}
export interface BudgetExceeded {
  scope: "session" | "turn";
  metric: "tokens" | "costUsd";
  limit: number;
  observed: number;
}
