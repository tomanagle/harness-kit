// Package codex adapts the OpenAI Codex CLI to agent.Runtime through the
// official Codex app-server (`codex app-server`), a JSON-RPC protocol over
// stdio that OpenAI documents for rich client integrations such as the
// Codex IDE extension.
//
// Authentication is entirely Codex's own: the adapter never reads, stores
// or forwards credentials. It asks the app-server for the account state
// (account/read) and uses whatever sign-in the user completed with
// `codex login`. OpenAI's app-server documentation states that app-server
// authentication is permitted for local and open-source applications, but
// has never been permitted for commercial or hosted services, which should
// use Sign in with ChatGPT or API keys instead. See docs/runtimes.md.
package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tomanagle/harness-kit/agent"
	"github.com/tomanagle/harness-kit/config"
	"github.com/tomanagle/harness-kit/event"
	"github.com/tomanagle/harness-kit/internal/jsonrpc"
	"github.com/tomanagle/harness-kit/mcp"
	"github.com/tomanagle/harness-kit/permission"
	"github.com/tomanagle/harness-kit/process"
)

// Name is the runtime name.
const Name = "codex"

// TestedVersion is the newest Codex CLI version the adapter was verified
// against.
const TestedVersion = "0.166.0"

// Runtime is the Codex adapter.
type Runtime struct {
	path         string
	args         []string
	env          []string
	clientInfo   ClientInfo
	startTimeout time.Duration
}

// ClientInfo identifies the integration to the app-server (and to OpenAI's
// compliance logs). Integrations built on Harness Kit should set their own.
type ClientInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

// Option configures the adapter.
type Option func(*Runtime)

// WithPath sets the codex binary path (default: HARNESS_KIT_CODEX_PATH,
// then PATH and well-known install directories).
func WithPath(path string) Option { return func(r *Runtime) { r.path = path } }

// WithArgs appends arguments after `codex app-server`, e.g. -c overrides.
func WithArgs(args ...string) Option { return func(r *Runtime) { r.args = append(r.args, args...) } }

// WithEnv adds KEY=VALUE environment entries for the app-server process.
func WithEnv(kv ...string) Option { return func(r *Runtime) { r.env = append(r.env, kv...) } }

// WithClientInfo sets the clientInfo sent in initialize.
func WithClientInfo(ci ClientInfo) Option { return func(r *Runtime) { r.clientInfo = ci } }

// WithStartTimeout bounds process start and handshake (default 30s).
func WithStartTimeout(d time.Duration) Option { return func(r *Runtime) { r.startTimeout = d } }

// New returns a Codex adapter.
func New(opts ...Option) *Runtime {
	r := &Runtime{
		clientInfo:   ClientInfo{Name: "harness_kit", Title: "Harness Kit", Version: "0.1.0"},
		startTimeout: 30 * time.Second,
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Options are per-session options (agent.SessionConfig.Options).
type Options struct {
	// ApprovalPolicy: "untrusted", "on-request" or "never". Empty inherits Codex configuration.
	ApprovalPolicy string `json:"approvalPolicy,omitempty"`
	// Sandbox: "read-only", "workspace-write" or
	// "danger-full-access" (requires AllowUnsafe). Empty inherits Codex configuration.
	Sandbox string `json:"sandbox,omitempty"`
	// Effort overrides the reasoning effort, e.g. "low", "medium", "high".
	Effort string `json:"effort,omitempty"`
	// Ephemeral threads are not persisted by Codex and cannot be resumed.
	Ephemeral bool `json:"ephemeral,omitempty"`
	// Config passes thread configuration overrides through verbatim.
	// Overrides touching sandbox or approval settings require AllowUnsafe.
	Config map[string]any `json:"config,omitempty"`
}

func (o Options) validate() error {
	switch o.ApprovalPolicy {
	case "", "untrusted", "on-request", "never":
	default:
		return fmt.Errorf("codex: invalid approvalPolicy %q", o.ApprovalPolicy)
	}
	switch o.Sandbox {
	case "", "read-only", "workspace-write", "danger-full-access":
	default:
		return fmt.Errorf("codex: invalid sandbox %q", o.Sandbox)
	}
	return nil
}

var (
	_ agent.Runtime          = (*Runtime)(nil)
	_ agent.Configurable     = (*Runtime)(nil)
	_ agent.TurnPreparer     = (*Session)(nil)
	_ agent.DefaultsProvider = (*Session)(nil)
	_ agent.Guarded          = (*Runtime)(nil)
	_ agent.SessionLister    = (*Runtime)(nil)
	_ agent.NativeMCPLister  = (*Runtime)(nil)
)

// Info implements agent.Runtime.
func (r *Runtime) Info() agent.Info {
	return agent.Info{
		Name:        Name,
		DisplayName: "OpenAI Codex",
		Description: "Codex CLI via the official app-server protocol.",
		Capabilities: agent.Capabilities{
			Streaming: true, Approvals: true, Resume: true, Fork: true, Cancel: true, Usage: true, ListSessions: true, MCP: true, Images: true,
			Configuration: map[string]config.Support{
				"cwd": {Startup: true}, "instructions": {Startup: true}, "rawEvents": {Startup: true},
				"mcp": {Startup: true}, "hostTools": {Startup: true}, "disableHostTools": {Startup: true},
				"model":                  {Startup: true, Session: true, Turn: true},
				"effort":                 {Startup: true, Session: true, Turn: true},
				"options.approvalPolicy": {Startup: true},
				"options.sandbox":        {Startup: true},
				"options.ephemeral":      {Startup: true},
				"options.config":         {Startup: true},
			},
		},
	}
}

// Unsafe implements agent.Guarded.
func (r *Runtime) Unsafe(cfg agent.SessionConfig) string {
	var o Options
	if err := agent.DecodeOptions(cfg.Options, &o); err != nil {
		return ""
	}
	if o.Sandbox == "danger-full-access" {
		return "codex sandbox \"danger-full-access\" disables sandboxing"
	}
	for k := range o.Config {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "sandbox") || strings.Contains(lk, "approval") {
			return fmt.Sprintf("codex config override %q changes sandbox or approval settings", k)
		}
	}
	return ""
}

func (r *Runtime) lookPath() (string, error) {
	return process.LookPath("codex", process.LookOptions{Path: r.path, EnvVar: "HARNESS_KIT_CODEX_PATH"})
}

// Discover implements agent.Runtime.
func (r *Runtime) Discover(ctx context.Context) (agent.Discovery, error) {
	d := agent.Discovery{Runtime: Name, Capabilities: r.Info().Capabilities, Auth: agent.AuthStatus{State: agent.AuthUnknown}}
	path, err := r.lookPath()
	if err != nil {
		d.Problems = append(d.Problems, "Codex CLI not found. Install it (https://developers.openai.com/codex/cli) or set HARNESS_KIT_CODEX_PATH.")
		return d, nil
	}
	d.Installed, d.Path = true, path
	if v, err := process.Version(ctx, path, r.env); err == nil {
		d.Version = v
	} else {
		d.Problems = append(d.Problems, err.Error())
	}

	auth, err := r.probeAuth(ctx, path)
	if err != nil {
		d.Problems = append(d.Problems, "could not query Codex account status: "+err.Error())
	} else {
		d.Auth = auth
	}
	if d.Auth.State == agent.AuthUnauthenticated {
		d.Problems = append(d.Problems, "Codex is not signed in. Run `codex login` in a terminal.")
	}
	d.Ready = len(d.Problems) == 0
	d.Notes = append(d.Notes,
		"Uses the local Codex sign-in through the official app-server; Harness Kit never reads Codex credentials.",
		"OpenAI permits app-server authentication for local and open-source apps, not for commercial or hosted services (use Sign in with ChatGPT or API keys there).",
	)
	return d, nil
}

func (r *Runtime) probeAuth(ctx context.Context, path string) (agent.AuthStatus, error) {
	s, err := r.spawn(ctx, path, "", nil)
	if err != nil {
		return agent.AuthStatus{}, err
	}
	defer s.Close(context.Background())
	var res accountReadResult
	if err := s.conn.Call(ctx, "account/read", map[string]any{}, &res); err != nil {
		return agent.AuthStatus{}, err
	}
	return res.status(), nil
}

type accountReadResult struct {
	Account *struct {
		Type     string `json:"type"`
		PlanType string `json:"planType"`
	} `json:"account"`
	RequiresOpenaiAuth bool `json:"requiresOpenaiAuth"`
}

func (a accountReadResult) status() agent.AuthStatus {
	switch {
	case a.Account != nil:
		return agent.AuthStatus{State: agent.AuthAuthenticated, Method: a.Account.Type, Plan: a.Account.PlanType}
	case a.RequiresOpenaiAuth:
		return agent.AuthStatus{State: agent.AuthUnauthenticated}
	default:
		// A custom model provider that needs no OpenAI account.
		return agent.AuthStatus{State: agent.AuthAuthenticated, Method: "none"}
	}
}

// ListSessions implements agent.SessionLister with the app-server's
// thread/list, newest first. It includes threads started in the Codex CLI,
// IDE extension, `codex exec` and other app-server clients.
func (r *Runtime) ListSessions(ctx context.Context, opts agent.ListSessionsOptions) ([]agent.RuntimeSession, error) {
	path, err := r.lookPath()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrNotInstalled, err)
	}
	s, err := r.spawn(ctx, path, "", nil)
	if err != nil {
		return nil, err
	}
	defer s.Close(context.Background())
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	params := map[string]any{
		"limit":       limit,
		"sortKey":     "updated_at",
		"sourceKinds": []string{"cli", "vscode", "exec", "appServer"},
	}
	if opts.Cwd != "" {
		params["cwd"] = opts.Cwd
	}
	var res struct {
		Data []struct {
			ID        string          `json:"id"`
			Cwd       string          `json:"cwd"`
			Name      *string         `json:"name"`
			Preview   string          `json:"preview"`
			Model     *string         `json:"model"`
			Source    json.RawMessage `json:"source"`
			CreatedAt int64           `json:"createdAt"`
			UpdatedAt int64           `json:"updatedAt"`
		} `json:"data"`
	}
	if err := s.conn.Call(ctx, "thread/list", params, &res); err != nil {
		return nil, fmt.Errorf("codex: thread/list: %w", err)
	}
	out := make([]agent.RuntimeSession, 0, len(res.Data))
	for _, t := range res.Data {
		rs := agent.RuntimeSession{
			ID: t.ID, Cwd: t.Cwd, Title: oneLine(t.Preview),
			CreatedAt: time.Unix(t.CreatedAt, 0).UTC(), UpdatedAt: time.Unix(t.UpdatedAt, 0).UTC(),
		}
		if t.Name != nil && *t.Name != "" {
			rs.Title = oneLine(*t.Name)
		}
		if t.Model != nil {
			rs.Model = *t.Model
		}
		var src string
		if json.Unmarshal(t.Source, &src) == nil {
			rs.Source = src
		} else if len(t.Source) > 0 {
			rs.Source = "other"
		}
		out = append(out, rs)
	}
	return out, nil
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 100 {
		s = string(r[:99]) + "…"
	}
	return s
}

// ValidateConfig checks adapter-specific values without starting a process.
func (r *Runtime) ValidateConfig(cfg agent.SessionConfig) error {
	var o Options
	if err := agent.DecodeOptions(cfg.Options, &o); err != nil {
		return fmt.Errorf("%w: %v", agent.ErrInvalidConfig, err)
	}
	if err := o.validate(); err != nil {
		return fmt.Errorf("%w: %v", agent.ErrInvalidConfig, err)
	}
	for native, portable := range map[string]string{"model": "model", "model_reasoning_effort": "effort"} {
		if _, exists := o.Config[native]; exists {
			return fmt.Errorf("%w: codex options.config.%s conflicts with portable configuration; use %s", agent.ErrInvalidConfig, native, portable)
		}
	}
	for name, value := range map[string]string{"model": cfg.Model, "effort": cfg.Effort, "options.effort": o.Effort} {
		if value != "" && strings.TrimSpace(value) == "" {
			return fmt.Errorf("%w: codex: %s must not contain only whitespace", agent.ErrInvalidConfig, name)
		}
	}
	if cfg.Cwd != "" {
		if fi, err := os.Stat(cfg.Cwd); err != nil || !fi.IsDir() {
			return fmt.Errorf("%w: codex: cwd %q is not a directory", agent.ErrInvalidConfig, cfg.Cwd)
		}
	}
	return nil
}

// Start implements agent.Runtime.
func (r *Runtime) Start(ctx context.Context, cfg agent.SessionConfig, h agent.Handler) (agent.Session, error) {
	if err := r.ValidateConfig(cfg); err != nil {
		return nil, err
	}
	var o Options
	_ = agent.DecodeOptions(cfg.Options, &o)
	path, err := r.lookPath()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrNotInstalled, err)
	}

	s, err := r.spawn(ctx, path, cfg.Cwd, h)
	if err != nil {
		return nil, err
	}
	s.cfg, s.opts = cfg, o
	// Resolve native defaults before applying this session's overrides. These
	// reads do not run a model or mutate Codex's configuration files.
	if cfg.NativeDefaults != nil {
		s.nativeModel, s.nativeEffort = cfg.NativeDefaults.Model, cfg.NativeDefaults.Effort
		_ = json.Unmarshal(cfg.NativeDefaults.Options["modelEfforts"], &s.modelEfforts)
		_ = json.Unmarshal(cfg.NativeDefaults.Options["supportedEfforts"], &s.supportedEfforts)
	} else {
		s.captureDefaults(ctx)
	}

	params := map[string]any{}
	if o.ApprovalPolicy != "" {
		params["approvalPolicy"] = o.ApprovalPolicy
	}
	if o.Sandbox != "" {
		params["sandbox"] = o.Sandbox
	}
	if cfg.Cwd != "" {
		params["cwd"] = cfg.Cwd
	}
	if cfg.Model != "" {
		params["model"] = cfg.Model
	}
	if cfg.Instructions != "" {
		params["developerInstructions"] = cfg.Instructions
	}
	threadConfig := map[string]any{}
	for k, v := range o.Config {
		threadConfig[k] = v
	}
	if cfg.MCP != nil {
		servers, warnings := r.mcpServers(ctx, s, cfg)
		for _, w := range warnings {
			h.Emit(event.Event{Type: event.RuntimeStatus, Data: event.RuntimeStatusData{Status: "warning", Message: w}})
		}
		if len(servers) > 0 {
			merged, _ := threadConfig["mcp_servers"].(map[string]any)
			if merged == nil {
				merged = map[string]any{}
			}
			for k, v := range servers {
				merged[k] = v
			}
			threadConfig["mcp_servers"] = merged
		}
	}
	if len(threadConfig) > 0 {
		params["config"] = threadConfig
	}
	method := "thread/start"
	if cfg.ForkFrom != "" {
		method = "thread/fork"
		params["threadId"] = cfg.ForkFrom
		params["excludeTurns"] = true
	} else if cfg.ResumeID != "" {
		method = "thread/resume"
		params["threadId"] = cfg.ResumeID
		params["excludeTurns"] = true
	} else if o.Ephemeral {
		params["ephemeral"] = true
	}

	sctx, cancel := context.WithTimeout(ctx, r.startTimeout)
	defer cancel()
	var res struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
		Model           string  `json:"model"`
		ReasoningEffort *string `json:"reasoningEffort"`
	}
	if err := s.conn.Call(sctx, method, params, &res); err != nil {
		s.Close(context.Background())
		return nil, fmt.Errorf("codex: %s: %w%s", method, err, s.stderr.Suffix())
	}
	if res.Thread.ID == "" {
		s.Close(context.Background())
		return nil, fmt.Errorf("codex: %s returned no thread id", method)
	}
	s.threadID = res.Thread.ID
	// Older servers may not expose config/read. A new thread's resolved
	// defaults are still authoritative when no corresponding override was sent.
	if cfg.NativeDefaults == nil && cfg.ResumeID == "" && cfg.Model == "" && o.Config["model"] == nil && s.nativeModel == "" {
		s.nativeModel = res.Model
	}
	if cfg.NativeDefaults == nil && cfg.ResumeID == "" && o.Config["model_reasoning_effort"] == nil && s.nativeEffort == "" && res.ReasoningEffort != nil {
		// A thread's resolved effort is specific to its selected model.
		// Only config/read can establish a global native effort selection.
		if s.modelEfforts == nil {
			s.modelEfforts = make(map[string]string)
		}
		if res.Model != "" && s.modelEfforts[res.Model] == "" {
			s.modelEfforts[res.Model] = *res.ReasoningEffort
		}
	}
	if res.Model != "" && h != nil {
		detail, _ := json.Marshal(map[string]string{"model": res.Model})
		h.Emit(event.Event{Type: event.RuntimeStatus, Data: event.RuntimeStatusData{Status: "ready", Message: "model " + res.Model, Detail: detail}})
	}
	return s, nil
}

// Fork branches persisted native history using thread/fork, without inference.
func (r *Runtime) Fork(ctx context.Context, source string, cfg agent.SessionConfig, h agent.Handler) (agent.Session, error) {
	if source == "" {
		return nil, fmt.Errorf("%w: fork source is empty", agent.ErrInvalidConfig)
	}
	cfg.ResumeID, cfg.ForkFrom = "", source
	return r.Start(ctx, cfg, h)
}

// NativeMCPServers lists the MCP servers configured in Codex itself, as
// seen from cwd (config/read), without connecting to them.
func (r *Runtime) NativeMCPServers(ctx context.Context, cwd string) ([]string, error) {
	path, err := r.lookPath()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrNotInstalled, err)
	}
	s, err := r.spawn(ctx, path, "", nil)
	if err != nil {
		return nil, err
	}
	defer s.Close(context.Background())
	var res struct {
		Config struct {
			MCPServers map[string]json.RawMessage `json:"mcp_servers"`
		} `json:"config"`
	}
	params := map[string]any{}
	if cwd != "" {
		params["cwd"] = cwd
	}
	if err := s.conn.Call(ctx, "config/read", params, &res); err != nil {
		return nil, fmt.Errorf("codex: config/read: %w", err)
	}
	names := make([]string, 0, len(res.Config.MCPServers))
	for n := range res.Config.MCPServers {
		names = append(names, n)
	}
	slices.Sort(names)
	return names, nil
}

// mcpServers translates the session's MCP servers into Codex thread config
// (mcp_servers.<name>), verified to apply per thread without touching
// ~/.codex/config.toml. ${VAR} references are resolved here; the expanded
// values travel only over the app-server pipe.
func (r *Runtime) mcpServers(ctx context.Context, s *Session, cfg agent.SessionConfig) (map[string]any, []string) {
	var warnings []string
	native := map[string]bool{}
	var res struct {
		Config struct {
			MCPServers map[string]json.RawMessage `json:"mcp_servers"`
		} `json:"config"`
	}
	params := map[string]any{}
	if cfg.Cwd != "" {
		params["cwd"] = cfg.Cwd
	}
	if err := s.conn.Call(ctx, "config/read", params, &res); err == nil {
		for name := range res.Config.MCPServers {
			native[name] = true
		}
	} else if !cfg.MCP.Inherit() {
		warnings = append(warnings, "could not read Codex's own MCP servers to disable them: "+err.Error())
	}

	lookup := mcp.LookupWith(os.LookupEnv, r.env)
	out := map[string]any{}
	for _, name := range cfg.MCP.Names() {
		srv := cfg.MCP.Servers[name]
		if native[name] {
			warnings = append(warnings, fmt.Sprintf("skipped MCP server %q: Codex already has a server with that name (rename one of them)", name))
			continue
		}
		e, err := srv.Expanded(lookup)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("skipped MCP server %q: %v", name, err))
			continue
		}
		t := map[string]any{}
		if e.IsHTTP() {
			t["url"] = e.URL
			if len(e.Headers) > 0 {
				t["http_headers"] = e.Headers
			}
		} else {
			t["command"] = e.Command
			if len(e.Args) > 0 {
				t["args"] = e.Args
			}
			if len(e.Env) > 0 {
				t["env"] = e.Env
			}
			if e.Cwd != "" {
				t["cwd"] = e.Cwd
			}
		}
		if len(e.Tools.Allow) > 0 {
			t["enabled_tools"] = e.Tools.Allow
		}
		if len(e.Tools.Deny) > 0 {
			t["disabled_tools"] = e.Tools.Deny
		}
		// Verified against codex 0.166: "approve" runs tools without asking;
		// "prompt" asks through an MCP elicitation (handled in onServerRequest).
		if e.EffectiveApproval() == mcp.ApprovalAsk {
			t["default_tools_approval_mode"] = "prompt"
		} else {
			t["default_tools_approval_mode"] = "approve"
		}
		if e.StartupTimeoutSec > 0 {
			t["startup_timeout_sec"] = e.StartupTimeoutSec
		}
		if e.ToolTimeoutSec > 0 {
			t["tool_timeout_sec"] = e.ToolTimeoutSec
		}
		out[name] = t
	}
	if !cfg.MCP.Inherit() {
		for name := range native {
			if _, kit := cfg.MCP.Servers[name]; !kit {
				out[name] = map[string]any{"enabled": false}
			}
		}
	}
	return out, warnings
}

// spawn starts `codex app-server` and completes the initialize handshake.
func (r *Runtime) spawn(ctx context.Context, path, cwd string, h agent.Handler) (*Session, error) {
	cmd := exec.Command(path, append([]string{"app-server"}, r.args...)...)
	process.Configure(cmd)
	cmd.Dir = cwd
	if len(r.env) > 0 {
		cmd.Env = append(os.Environ(), r.env...)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	s := &Session{
		rt:     r,
		h:      h,
		cmd:    cmd,
		stdin:  stdin,
		exited: make(chan struct{}),
		stderr: process.NewTail(16 << 10),
		items:  make(map[string][]string),
	}
	cmd.Stderr = s.stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("codex: start app-server: %w", err)
	}
	go func() {
		s.waitErr = cmd.Wait()
		close(s.exited)
	}()
	s.conn = jsonrpc.NewConn(stdout, stdin, s, jsonrpc.Options{OmitVersion: true})
	go s.conn.Run(context.Background())

	hctx, cancel := context.WithTimeout(ctx, r.startTimeout)
	defer cancel()
	err = s.conn.Call(hctx, "initialize", map[string]any{"clientInfo": r.clientInfo}, nil)
	if err == nil {
		err = s.conn.Notify(hctx, "initialized", nil)
	}
	if err != nil {
		s.Close(context.Background())
		return nil, fmt.Errorf("codex: app-server handshake: %w%s", err, s.stderr.Suffix())
	}
	return s, nil
}

// Session is a Codex thread served by a dedicated app-server process.
type Session struct {
	rt   *Runtime
	h    agent.Handler
	cfg  agent.SessionConfig
	opts Options

	cmd     *exec.Cmd
	stdin   io.WriteCloser
	conn    *jsonrpc.Conn
	exited  chan struct{}
	waitErr error
	stderr  *process.Tail

	threadID string
	// Captured once before session overrides; never changed by a turn.
	nativeModel      string
	nativeEffort     string
	modelEfforts     map[string]string
	supportedEfforts map[string][]string

	mu        sync.Mutex
	active    *activeTurn
	items     map[string][]string // fileChange item id -> paths
	lastTotal *tokenBreakdown
	closeOnce sync.Once
}

type activeTurn struct {
	ctx     context.Context
	id      string
	codexID string
	start   *tokenBreakdown
	done    chan turnOutcome
	once    sync.Once
}

type turnOutcome struct {
	codexID string
	status  string
	err     *turnError
}

type turnError struct {
	Message           string          `json:"message"`
	AdditionalDetails string          `json:"additionalDetails,omitempty"`
	CodexErrorInfo    json.RawMessage `json:"codexErrorInfo,omitempty"`
}

func (t *activeTurn) finish(o turnOutcome) { t.once.Do(func() { t.done <- o }) }

// RuntimeSessionID implements agent.Session (the Codex thread ID).
func (s *Session) RuntimeSessionID() string { return s.threadID }

func (s *Session) dead() error {
	msg := "codex app-server exited"
	if s.waitErr != nil {
		msg += ": " + s.waitErr.Error()
	}
	return fmt.Errorf("%s%s: %w", msg, s.stderr.Suffix(), agent.ErrSessionDead)
}

// Prompt implements agent.Session.
func (s *Session) Prompt(ctx context.Context, turn agent.Turn) (agent.TurnResult, error) {
	select {
	case <-s.exited:
		return agent.TurnResult{}, s.dead()
	default:
	}
	at := &activeTurn{ctx: ctx, id: turn.ID, done: make(chan turnOutcome, 1)}
	s.mu.Lock()
	if s.lastTotal != nil {
		cp := *s.lastTotal
		at.start = &cp
	}
	s.active = at
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.active = nil
		s.mu.Unlock()
	}()

	input := make([]map[string]any, 0, len(turn.Input))
	for _, p := range turn.Input {
		switch p.Type {
		case "image":
			input = append(input, map[string]any{"type": "localImage", "path": p.Path})
		default:
			input = append(input, map[string]any{"type": "text", "text": p.Text})
		}
	}
	settings := s.initialTurnConfig()
	if turn.Config != nil {
		settings = *turn.Config
	}
	settings, err := s.PrepareTurn(ctx, settings)
	if err != nil {
		return agent.TurnResult{}, err
	}
	// Codex turn overrides persist to subsequent turns. Send both resolved
	// fields every time, including after a one-turn override has finished.
	params := map[string]any{"threadId": s.threadID, "input": input,
		"model": settings.Model, "effort": settings.Effort}

	callCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	var res struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	err = s.conn.Call(callCtx, "turn/start", params, &res)
	cancel()
	if err != nil {
		select {
		case <-s.exited:
			return agent.TurnResult{}, s.dead()
		default:
		}
		return agent.TurnResult{}, fmt.Errorf("codex: turn/start: %w", err)
	}
	s.mu.Lock()
	if at.codexID == "" {
		at.codexID = res.Turn.ID
	}
	s.mu.Unlock()

	select {
	case out := <-at.done:
		return s.result(at, out), nil
	case <-s.exited:
		return agent.TurnResult{}, s.dead()
	case <-ctx.Done():
	}

	// Cancelled: interrupt gracefully and wait for Codex to confirm.
	ictx, icancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer icancel()
	_ = s.conn.Call(ictx, "turn/interrupt", map[string]any{"threadId": s.threadID, "turnId": res.Turn.ID}, nil)
	select {
	case out := <-at.done:
		r := s.result(at, out)
		if r.Status == event.TurnStatusCompleted {
			return r, nil // finished before the interrupt landed
		}
		r.Status, r.Error = event.TurnStatusCancelled, nil
		return r, nil
	case <-s.exited:
		return agent.TurnResult{Status: event.TurnStatusCancelled}, nil
	case <-ictx.Done():
		return agent.TurnResult{Status: event.TurnStatusCancelled, RuntimeTurnID: res.Turn.ID}, nil
	}
}

func (s *Session) result(at *activeTurn, out turnOutcome) agent.TurnResult {
	r := agent.TurnResult{RuntimeTurnID: out.codexID}
	switch out.status {
	case "completed":
		r.Status = event.TurnStatusCompleted
	case "interrupted":
		r.Status = event.TurnStatusCancelled
	default:
		r.Status = event.TurnStatusFailed
		msg := "codex turn failed"
		if out.err != nil && out.err.Message != "" {
			msg = out.err.Message
		}
		r.Error = &event.ErrorInfo{Code: "codex_turn_failed", Message: msg}
	}
	s.mu.Lock()
	if s.lastTotal != nil {
		u := s.lastTotal.sub(at.start).usage("turn", 0)
		r.Usage = &u
	}
	s.mu.Unlock()
	return r
}

// Close implements agent.Session: it closes the app-server's stdin (which
// makes it exit) and kills the process tree if it lingers.
func (s *Session) Close(context.Context) error {
	s.closeOnce.Do(func() {
		s.stdin.Close()
		select {
		case <-s.exited:
		case <-time.After(3 * time.Second):
			_ = process.Kill(s.cmd)
			<-s.exited
		}
	})
	return nil
}

// Handle implements jsonrpc.Handler for messages from the app-server.
func (s *Session) Handle(ctx context.Context, req *jsonrpc.Request) (any, error) {
	if req.IsNotification() {
		s.onNotification(req.Method, req.Params)
		return nil, nil
	}
	return s.onServerRequest(ctx, req)
}

func (s *Session) emit(turnID string, t event.Type, data any) {
	if s.h != nil {
		s.h.Emit(event.Event{Type: t, TurnID: turnID, Data: data})
	}
}

func (s *Session) current() *activeTurn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

type tokenBreakdown struct {
	CachedInputTokens     int64 `json:"cachedInputTokens"`
	CacheWriteInputTokens int64 `json:"cacheWriteInputTokens"`
	InputTokens           int64 `json:"inputTokens"`
	OutputTokens          int64 `json:"outputTokens"`
	ReasoningOutputTokens int64 `json:"reasoningOutputTokens"`
	TotalTokens           int64 `json:"totalTokens"`
}

func (b tokenBreakdown) sub(o *tokenBreakdown) tokenBreakdown {
	if o == nil {
		return b
	}
	return tokenBreakdown{
		CachedInputTokens:     b.CachedInputTokens - o.CachedInputTokens,
		CacheWriteInputTokens: b.CacheWriteInputTokens - o.CacheWriteInputTokens,
		InputTokens:           b.InputTokens - o.InputTokens,
		OutputTokens:          b.OutputTokens - o.OutputTokens,
		ReasoningOutputTokens: b.ReasoningOutputTokens - o.ReasoningOutputTokens,
		TotalTokens:           b.TotalTokens - o.TotalTokens,
	}
}

func (b tokenBreakdown) usage(scope string, window int64) event.UsageData {
	return event.UsageData{
		Scope:             scope,
		InputTokens:       b.InputTokens,
		OutputTokens:      b.OutputTokens,
		CachedInputTokens: b.CachedInputTokens,
		CacheWriteTokens:  b.CacheWriteInputTokens,
		ReasoningTokens:   b.ReasoningOutputTokens,
		TotalTokens:       b.TotalTokens,
		ContextWindow:     window,
	}
}

type threadItem struct {
	Type             string          `json:"type"`
	ID               string          `json:"id"`
	Text             string          `json:"text"`
	Summary          []string        `json:"summary"`
	Content          []string        `json:"content"`
	Command          string          `json:"command"`
	Cwd              string          `json:"cwd"`
	Status           string          `json:"status"`
	AggregatedOutput *string         `json:"aggregatedOutput"`
	ExitCode         *int            `json:"exitCode"`
	Changes          []fileChange    `json:"changes"`
	Server           string          `json:"server"`
	Tool             string          `json:"tool"`
	Arguments        json.RawMessage `json:"arguments"`
	Result           json.RawMessage `json:"result"`
	Error            *struct {
		Message string `json:"message"`
	} `json:"error"`
	Query string `json:"query"`
}

type fileChange struct {
	Path string          `json:"path"`
	Kind json.RawMessage `json:"kind"`
	Diff string          `json:"diff"`
}

func toolStatus(s string) event.ToolStatus {
	switch s {
	case "failed":
		return event.ToolStatusFailed
	case "declined":
		return event.ToolStatusDeclined
	}
	return event.ToolStatusCompleted
}

func (s *Session) onNotification(method string, params json.RawMessage) {
	var common struct {
		ThreadID string `json:"threadId"`
		TurnID   string `json:"turnId"`
	}
	_ = json.Unmarshal(params, &common)
	if common.ThreadID != "" && s.threadID != "" && common.ThreadID != s.threadID {
		s.raw("", method, params) // another thread, e.g. a Codex subagent
		return
	}
	at := s.current()
	turnID := ""
	if at != nil {
		turnID = at.id
	}

	switch method {
	case "item/agentMessage/delta":
		var p struct{ ItemID, Delta string }
		_ = json.Unmarshal(params, &p)
		s.emit(turnID, event.MessageDelta, event.MessageDeltaData{ItemID: p.ItemID, Text: p.Delta})
	case "item/reasoning/summaryTextDelta", "item/reasoning/textDelta":
		var p struct{ ItemID, Delta string }
		_ = json.Unmarshal(params, &p)
		s.emit(turnID, event.ReasoningDelta, event.ReasoningDeltaData{ItemID: p.ItemID, Text: p.Delta})
	case "item/commandExecution/outputDelta", "item/fileChange/outputDelta":
		var p struct{ ItemID, Delta string }
		_ = json.Unmarshal(params, &p)
		s.emit(turnID, event.ToolOutput, event.ToolOutputData{CallID: p.ItemID, Output: p.Delta})
	case "item/started":
		var p struct{ Item threadItem }
		if json.Unmarshal(params, &p) == nil {
			s.itemStarted(turnID, p.Item)
		}
	case "item/completed":
		var p struct{ Item threadItem }
		if json.Unmarshal(params, &p) == nil {
			s.itemCompleted(turnID, p.Item)
		}
	case "thread/tokenUsage/updated":
		var p struct {
			TokenUsage struct {
				Total              tokenBreakdown `json:"total"`
				ModelContextWindow int64          `json:"modelContextWindow"`
			} `json:"tokenUsage"`
		}
		if json.Unmarshal(params, &p) == nil {
			s.mu.Lock()
			total := p.TokenUsage.Total
			s.lastTotal = &total
			s.mu.Unlock()
			s.emit(turnID, event.Usage, total.usage("session", p.TokenUsage.ModelContextWindow))
		}
	case "turn/started":
		var p struct {
			Turn struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		if json.Unmarshal(params, &p) == nil && at != nil {
			s.mu.Lock()
			if at.codexID == "" {
				at.codexID = p.Turn.ID
			}
			s.mu.Unlock()
		}
	case "turn/completed":
		var p struct {
			Turn struct {
				ID     string     `json:"id"`
				Status string     `json:"status"`
				Error  *turnError `json:"error"`
			} `json:"turn"`
		}
		if json.Unmarshal(params, &p) == nil && at != nil {
			s.mu.Lock()
			match := at.codexID == "" || at.codexID == p.Turn.ID
			s.mu.Unlock()
			if match {
				at.finish(turnOutcome{codexID: p.Turn.ID, status: p.Turn.Status, err: p.Turn.Error})
			}
		}
	case "error":
		var p struct {
			Error     turnError `json:"error"`
			WillRetry bool      `json:"willRetry"`
		}
		_ = json.Unmarshal(params, &p)
		if p.WillRetry {
			s.emit(turnID, event.RuntimeStatus, event.RuntimeStatusData{Status: "retrying", Message: p.Error.Message})
		} else {
			s.emit(turnID, event.Error, event.ErrorData{Code: "codex_error", Message: p.Error.Message})
		}
	case "warning", "configWarning", "deprecationNotice", "guardianWarning":
		var p struct {
			Message string `json:"message"`
			Summary string `json:"summary"`
		}
		_ = json.Unmarshal(params, &p)
		msg := p.Message
		if msg == "" {
			msg = p.Summary
		}
		s.emit(turnID, event.RuntimeStatus, event.RuntimeStatusData{Status: "warning", Message: msg, Detail: params})
	case "model/rerouted":
		s.emit(turnID, event.RuntimeStatus, event.RuntimeStatusData{Status: "model_rerouted", Detail: params})
	case "thread/compacted":
		s.emit(turnID, event.RuntimeStatus, event.RuntimeStatusData{Status: "compacted"})
	default:
		s.raw(turnID, method, params)
	}
}

func (s *Session) raw(turnID, method string, params json.RawMessage) {
	if s.cfg.RawEvents {
		s.emit(turnID, event.RuntimeRaw, event.RawData{Runtime: Name, Kind: method, Payload: params})
	}
}

func (s *Session) itemStarted(turnID string, it threadItem) {
	switch it.Type {
	case "commandExecution":
		input, _ := json.Marshal(map[string]string{"command": it.Command, "cwd": it.Cwd})
		s.emit(turnID, event.ToolStarted, event.ToolStartedData{CallID: it.ID, Name: "command", Kind: event.ToolKindCommand, Title: it.Command, Input: input})
	case "fileChange":
		paths := make([]string, 0, len(it.Changes))
		for _, c := range it.Changes {
			paths = append(paths, c.Path)
		}
		s.mu.Lock()
		s.items[it.ID] = paths
		s.mu.Unlock()
		input, _ := json.Marshal(it.Changes)
		s.emit(turnID, event.ToolStarted, event.ToolStartedData{CallID: it.ID, Name: "file_change", Kind: event.ToolKindFileChange, Title: "Edit " + strings.Join(paths, ", "), Input: input})
	case "mcpToolCall":
		s.emit(turnID, event.ToolStarted, event.ToolStartedData{CallID: it.ID, Name: it.Server + "/" + it.Tool, Kind: event.ToolKindMCP, Title: it.Server + "/" + it.Tool, Input: it.Arguments})
	case "dynamicToolCall":
		s.emit(turnID, event.ToolStarted, event.ToolStartedData{CallID: it.ID, Name: it.Tool, Kind: event.ToolKindOther, Title: it.Tool, Input: it.Arguments})
	case "webSearch":
		s.emit(turnID, event.ToolStarted, event.ToolStartedData{CallID: it.ID, Name: "web_search", Kind: event.ToolKindWebSearch, Title: it.Query})
	case "collabAgentToolCall":
		s.emit(turnID, event.ToolStarted, event.ToolStartedData{CallID: it.ID, Name: it.Tool, Kind: event.ToolKindSubagent, Title: it.Tool})
	}
}

func (s *Session) itemCompleted(turnID string, it threadItem) {
	switch it.Type {
	case "agentMessage":
		s.emit(turnID, event.MessageCompleted, event.MessageCompletedData{ItemID: it.ID, Role: "assistant", Text: it.Text})
	case "reasoning":
		text := strings.Join(it.Summary, "\n")
		if text == "" {
			text = strings.Join(it.Content, "\n")
		}
		if text != "" {
			s.emit(turnID, event.ReasoningCompleted, event.ReasoningCompletedData{ItemID: it.ID, Text: text})
		}
	case "commandExecution":
		d := event.ToolCompletedData{CallID: it.ID, Name: "command", Status: toolStatus(it.Status), ExitCode: it.ExitCode}
		if it.AggregatedOutput != nil {
			d.Output = *it.AggregatedOutput
		}
		s.emit(turnID, event.ToolCompleted, d)
	case "fileChange":
		s.mu.Lock()
		delete(s.items, it.ID)
		s.mu.Unlock()
		s.emit(turnID, event.ToolCompleted, event.ToolCompletedData{CallID: it.ID, Name: "file_change", Status: toolStatus(it.Status)})
	case "mcpToolCall", "dynamicToolCall", "collabAgentToolCall":
		d := event.ToolCompletedData{CallID: it.ID, Name: it.Tool, Status: toolStatus(it.Status)}
		if it.Error != nil {
			d.Error = it.Error.Message
		}
		if len(it.Result) > 0 && string(it.Result) != "null" {
			d.Output = string(it.Result)
		}
		s.emit(turnID, event.ToolCompleted, d)
	case "webSearch":
		s.emit(turnID, event.ToolCompleted, event.ToolCompletedData{CallID: it.ID, Name: "web_search", Status: event.ToolStatusCompleted})
	case "contextCompaction":
		s.emit(turnID, event.RuntimeStatus, event.RuntimeStatusData{Status: "compacted"})
	}
}

var allOutcomes = []permission.Outcome{permission.Allow, permission.AllowSession, permission.Deny, permission.Abort}

func codexDecision(o permission.Outcome) string {
	switch o {
	case permission.Allow:
		return "accept"
	case permission.AllowSession:
		return "acceptForSession"
	case permission.Deny:
		return "decline"
	}
	return "cancel"
}

// ask routes an approval to the host, bounded by the active turn's context.
func (s *Session) ask(ctx context.Context, req permission.Request) (permission.Decision, error) {
	at := s.current()
	if at == nil || s.h == nil {
		return permission.Decision{Outcome: permission.Deny, Message: "no active turn"}, nil
	}
	req.TurnID = at.id
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(at.ctx, cancel)
	defer stop()
	return s.h.RequestApproval(ctx, req)
}

func (s *Session) onServerRequest(ctx context.Context, req *jsonrpc.Request) (any, error) {
	switch req.Method {
	case "item/commandExecution/requestApproval":
		var p struct {
			ItemID  string  `json:"itemId"`
			Command *string `json:"command"`
			Cwd     *string `json:"cwd"`
			Reason  *string `json:"reason"`
		}
		_ = json.Unmarshal(req.Params, &p)
		r := permission.Request{Kind: permission.KindCommand, Tool: "commandExecution", Input: req.Params, Outcomes: allOutcomes}
		if p.Command != nil {
			r.Command = *p.Command
			r.Title = "Run `" + *p.Command + "`"
		} else {
			r.Title = "Run a command"
		}
		if p.Cwd != nil {
			r.Cwd = *p.Cwd
		}
		if p.Reason != nil {
			r.Reason = *p.Reason
		}
		d, err := s.ask(ctx, r)
		if err != nil {
			return map[string]any{"decision": "cancel"}, nil
		}
		return map[string]any{"decision": codexDecision(d.Outcome)}, nil

	case "item/fileChange/requestApproval":
		var p struct {
			ItemID string  `json:"itemId"`
			Reason *string `json:"reason"`
		}
		_ = json.Unmarshal(req.Params, &p)
		s.mu.Lock()
		paths := s.items[p.ItemID]
		s.mu.Unlock()
		r := permission.Request{Kind: permission.KindFileChange, Tool: "fileChange", Paths: paths, Input: req.Params, Outcomes: allOutcomes, Title: "Apply file changes"}
		if len(paths) > 0 {
			r.Title = "Edit " + strings.Join(paths, ", ")
		}
		if p.Reason != nil {
			r.Reason = *p.Reason
		}
		d, err := s.ask(ctx, r)
		if err != nil {
			return map[string]any{"decision": "cancel"}, nil
		}
		return map[string]any{"decision": codexDecision(d.Outcome)}, nil

	case "item/permissions/requestApproval":
		var p struct {
			Permissions json.RawMessage `json:"permissions"`
			Reason      *string         `json:"reason"`
			Cwd         string          `json:"cwd"`
		}
		_ = json.Unmarshal(req.Params, &p)
		r := permission.Request{
			Kind: permission.KindPermission, Tool: "permissions", Title: "Grant additional sandbox permissions",
			Cwd: p.Cwd, Input: p.Permissions,
			Outcomes: []permission.Outcome{permission.Allow, permission.AllowSession, permission.Deny},
		}
		if p.Reason != nil {
			r.Reason = *p.Reason
		}
		d, err := s.ask(ctx, r)
		if err != nil || !d.Outcome.Allowed() || len(p.Permissions) == 0 {
			return map[string]any{"permissions": map[string]any{}}, nil
		}
		scope := "turn"
		if d.Outcome == permission.AllowSession {
			scope = "session"
		}
		return map[string]any{"permissions": p.Permissions, "scope": scope}, nil

	case "mcpServer/elicitation/request":
		// Codex asks for MCP tool approval through an elicitation tagged
		// _meta.codex_approval_kind = "mcp_tool_call". Other elicitations
		// (forms a server wants filled in) are declined: Harness Kit has no
		// form channel yet.
		var p struct {
			ServerName string `json:"serverName"`
			Message    string `json:"message"`
			Meta       struct {
				Kind        string          `json:"codex_approval_kind"`
				Description string          `json:"tool_description"`
				Params      json.RawMessage `json:"tool_params"`
			} `json:"_meta"`
		}
		_ = json.Unmarshal(req.Params, &p)
		if p.Meta.Kind != "mcp_tool_call" {
			return map[string]any{"action": "decline"}, nil
		}
		r := permission.Request{
			Kind: permission.KindTool, Tool: "mcp/" + p.ServerName, Title: p.Message, Reason: p.Meta.Description,
			Input: p.Meta.Params, Outcomes: []permission.Outcome{permission.Allow, permission.Deny, permission.Abort},
		}
		if r.Title == "" {
			r.Title = "Use a tool from MCP server " + p.ServerName
		}
		d, err := s.ask(ctx, r)
		switch {
		case err != nil || d.Outcome == permission.Abort:
			return map[string]any{"action": "cancel"}, nil
		case d.Outcome.Allowed():
			return map[string]any{"action": "accept", "content": map[string]any{}}, nil
		}
		return map[string]any{"action": "decline"}, nil
	case "item/tool/requestUserInput":
		return map[string]any{"answers": map[string]any{}}, nil
	case "applyPatchApproval", "execCommandApproval":
		return map[string]any{"decision": "denied"}, nil
	}
	// item/tool/call, account/chatgptAuthTokens/refresh, attestation/generate
	// and anything newer: Harness Kit does not provide these.
	return nil, jsonrpc.NewError(jsonrpc.CodeMethodNotFound, "not supported by harness-kit: "+req.Method, nil)
}
