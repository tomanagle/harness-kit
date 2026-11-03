// Package claude adapts Anthropic's Claude Code CLI to agent.Runtime using
// the CLI's documented programmatic interface: `claude -p` with
// `--output-format stream-json`. Anthropic's Agent SDK documentation names
// this as the way to drive Claude Code from languages other than Python
// and TypeScript.
//
// Each turn runs one `claude -p` process. The first turn pins the session
// with --session-id; later turns continue it with --resume. Cancellation
// sends SIGINT (documented to end the turn cleanly), then kills the
// process tree after a grace period.
//
// Authentication is Claude Code's own. The adapter runs the unmodified
// binary as installed, never reads or forwards credentials, and offers no
// login flow; it only reports `claude auth status`. Anthropic's terms
// require that products built for other people use API-key (Claude
// Console or cloud provider) authentication rather than Claude.ai
// subscription login. See docs/runtimes.md.
//
// Interactive approvals use Claude Code's documented --permission-prompt-tool:
// when the session manager has a ToolServer (package toolhost), each
// session's permission prompts go to Harness Kit's MCP bridge and become
// approval.requested events. Without one, anything that would prompt is
// denied (--permission-prompts none) and reported as runtime.status.
package claude

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tomanagle/harness-kit/agent"
	"github.com/tomanagle/harness-kit/config"
	"github.com/tomanagle/harness-kit/event"
	"github.com/tomanagle/harness-kit/mcp"
	"github.com/tomanagle/harness-kit/process"
)

// Name is the runtime name.
const Name = "claude"

// TestedVersion is the newest Claude Code version the adapter was verified
// against.
const TestedVersion = "2.1.316"

// permissionPromptsVersion is the first version supporting
// --permission-prompts.
const permissionPromptsVersion = "2.1.259"

// Runtime is the Claude Code adapter.
type Runtime struct {
	path  string
	env   []string
	args  []string
	grace time.Duration

	versionMu sync.Mutex
	versions  map[string]string
}

// Option configures the adapter.
type Option func(*Runtime)

// WithPath sets the claude binary path (default: HARNESS_KIT_CLAUDE_PATH,
// then PATH and well-known install directories).
func WithPath(path string) Option { return func(r *Runtime) { r.path = path } }

// WithEnv adds KEY=VALUE environment entries for claude processes, e.g.
// ANTHROPIC_API_KEY for products that must use API-key authentication.
func WithEnv(kv ...string) Option { return func(r *Runtime) { r.env = append(r.env, kv...) } }

// WithArgs appends trusted extra CLI flags to every invocation. These are
// set by the embedding Go program, never by RPC clients.
func WithArgs(args ...string) Option { return func(r *Runtime) { r.args = append(r.args, args...) } }

// WithInterruptGrace sets how long to wait after SIGINT before killing
// (default 5s).
func WithInterruptGrace(d time.Duration) Option { return func(r *Runtime) { r.grace = d } }

// New returns a Claude Code adapter.
func New(opts ...Option) *Runtime {
	r := &Runtime{grace: 5 * time.Second, versions: make(map[string]string)}
	for _, o := range opts {
		o(r)
	}
	return r
}

// Options are per-session options (agent.SessionConfig.Options).
type Options struct {
	// PermissionMode: "default"/"manual", "acceptEdits", "plan", "auto",
	// "dontAsk" or "bypassPermissions" (requires AllowUnsafe).
	PermissionMode string `json:"permissionMode,omitempty"`
	// AllowedTools run without prompting, in Claude Code permission-rule
	// syntax, e.g. "Read", "Bash(git diff *)".
	AllowedTools []string `json:"allowedTools,omitempty"`
	// DisallowedTools are deny rules.
	DisallowedTools []string `json:"disallowedTools,omitempty"`
	// Tools restricts the built-in tool set; an empty list disables all
	// built-in tools. Nil keeps the default set.
	Tools *[]string `json:"tools,omitempty"`
	// AddDirs grants tool access to additional directories.
	AddDirs []string `json:"addDirs,omitempty"`
	// Bare skips hooks, plugins, MCP servers, CLAUDE.md and auto memory
	// for reproducible runs. Bare mode authenticates only with
	// ANTHROPIC_API_KEY or an apiKeyHelper, never with a Claude.ai login.
	Bare bool `json:"bare,omitempty"`
	// MaxTurns limits agentic turns per prompt.
	MaxTurns int `json:"maxTurns,omitempty"`
	// MaxBudgetUSD stops a prompt when estimated spend reaches it.
	MaxBudgetUSD float64 `json:"maxBudgetUsd,omitempty"`
	// MaxImageBytes optionally bounds each input image before encoding. Nil
	// imposes no kit limit; zero refuses image bytes. Native limits still apply.
	MaxImageBytes *int64 `json:"maxImageBytes,omitempty"`
	// Effort: "low", "medium", "high", "xhigh" or "max".
	Effort string `json:"effort,omitempty"`
	// ForkOnResume continues a resumed session under a new session ID.
	ForkOnResume bool `json:"forkOnResume,omitempty"`
	// DisablePartialMessages turns off token-level streaming.
	DisablePartialMessages bool `json:"disablePartialMessages,omitempty"`
}

var permissionModes = map[string]bool{
	"": true, "default": true, "manual": true, "acceptEdits": true, "plan": true,
	"auto": true, "dontAsk": true, "bypassPermissions": true,
}

var (
	_ agent.Runtime            = (*Runtime)(nil)
	_ agent.Guarded            = (*Runtime)(nil)
	_ agent.PermissionToolUser = (*Runtime)(nil)
	_ agent.Configurable       = (*Runtime)(nil)
	_ agent.TurnPreparer       = (*Session)(nil)
	_ agent.DefaultsProvider   = (*Session)(nil)
)

// UsesPermissionTool implements agent.PermissionToolUser: Claude Code
// sessions route permission prompts to an MCP tool when the host offers one.
func (r *Runtime) UsesPermissionTool(cfg agent.SessionConfig) bool { return !cfg.DisableHostTools }

// Info implements agent.Runtime.
func (r *Runtime) Info() agent.Info {
	return agent.Info{
		Name:        Name,
		DisplayName: "Claude Code",
		Description: "Anthropic Claude Code CLI in headless stream-json mode.",
		Capabilities: agent.Capabilities{
			Streaming: true, Approvals: true, Resume: true, Fork: true, ForkDeferred: true, Cancel: true, Usage: true, Cost: true, MCP: true, Images: true,
			Configuration: configurationSupport(),
		},
	}
}

// configurationSupport lists only settings with representable CLI semantics.
// Changing tool permissions or process wiring requires a new runtime session.
func configurationSupport() map[string]config.Support {
	startup := config.Support{Startup: true}
	next := config.Support{Startup: true, Session: true, Turn: true}
	return map[string]config.Support{
		"model": next, "effort": next,
		"cwd": startup, "instructions": startup, "rawEvents": startup,
		"mcp": startup, "hostTools": startup, "disableHostTools": startup,
		"options.permissionMode": startup, "options.allowedTools": startup,
		"options.disallowedTools": startup, "options.tools": startup,
		"options.addDirs": startup, "options.bare": startup,
		"options.forkOnResume": startup,
		"options.maxTurns":     next, "options.maxBudgetUsd": next,
		"options.disablePartialMessages": next,
		"options.maxImageBytes":          next,
	}
}

// ValidateConfig checks configuration without launching a CLI or reading its
// private settings. The same checks apply at creation and before each turn.
func (r *Runtime) ValidateConfig(cfg agent.SessionConfig) error {
	_, err := validatedOptions(cfg)
	if err != nil {
		return err
	}
	if cfg.Cwd != "" {
		if fi, err := os.Stat(cfg.Cwd); err != nil || !fi.IsDir() {
			return fmt.Errorf("%w: claude: cwd %q is not a directory", agent.ErrInvalidConfig, cfg.Cwd)
		}
	}
	return nil
}

func validatedOptions(cfg agent.SessionConfig) (Options, error) {
	var o Options
	if err := agent.DecodeOptions(cfg.Options, &o); err != nil {
		return o, fmt.Errorf("%w: %v", agent.ErrInvalidConfig, err)
	}
	if cfg.Effort != "" {
		o.Effort = cfg.Effort
	}
	if !permissionModes[o.PermissionMode] {
		return o, fmt.Errorf("%w: claude: invalid permissionMode %q", agent.ErrInvalidConfig, o.PermissionMode)
	}
	switch o.Effort {
	case "", "low", "medium", "high", "xhigh", "max":
	default:
		return o, fmt.Errorf("%w: claude: invalid effort %q", agent.ErrInvalidConfig, o.Effort)
	}
	if cfg.Model != "" && strings.TrimSpace(cfg.Model) == "" {
		return o, fmt.Errorf("%w: claude: model cannot be whitespace", agent.ErrInvalidConfig)
	}
	if o.MaxTurns < 0 || o.MaxBudgetUSD < 0 {
		return o, fmt.Errorf("%w: claude: maxTurns and maxBudgetUsd must be nonnegative", agent.ErrInvalidConfig)
	}
	if o.MaxImageBytes != nil && *o.MaxImageBytes < 0 {
		return o, fmt.Errorf("%w: claude: maxImageBytes must be nonnegative", agent.ErrInvalidConfig)
	}
	return o, nil
}

// Unsafe implements agent.Guarded.
func (r *Runtime) Unsafe(cfg agent.SessionConfig) string {
	var o Options
	if err := agent.DecodeOptions(cfg.Options, &o); err != nil {
		return ""
	}
	if o.PermissionMode == "bypassPermissions" {
		return "claude permissionMode \"bypassPermissions\" skips all permission checks"
	}
	return ""
}

func (r *Runtime) lookPath() (string, error) {
	return process.LookPath("claude", process.LookOptions{Path: r.path, EnvVar: "HARNESS_KIT_CLAUDE_PATH"})
}

func (r *Runtime) version(ctx context.Context, path string) (string, error) {
	r.versionMu.Lock()
	defer r.versionMu.Unlock()
	if v, ok := r.versions[path]; ok {
		return v, nil
	}
	v, err := process.Version(ctx, path, r.env)
	if err != nil {
		return "", err
	}
	r.versions[path] = v
	return v, nil
}

func (r *Runtime) command(path string, args ...string) *exec.Cmd {
	cmd := exec.Command(path, args...)
	process.Configure(cmd)
	if len(r.env) > 0 {
		cmd.Env = append(os.Environ(), r.env...)
	}
	return cmd
}

// Discover implements agent.Runtime.
func (r *Runtime) Discover(ctx context.Context) (agent.Discovery, error) {
	d := agent.Discovery{Runtime: Name, Capabilities: r.Info().Capabilities, Auth: agent.AuthStatus{State: agent.AuthUnknown}}
	path, err := r.lookPath()
	if err != nil {
		d.Problems = append(d.Problems, "Claude Code CLI not found. Install it (https://code.claude.com/docs/en/setup) or set HARNESS_KIT_CLAUDE_PATH.")
		return d, nil
	}
	d.Installed, d.Path = true, path
	if v, err := r.version(ctx, path); err == nil {
		d.Version = v
		if process.CompareVersions(v, permissionPromptsVersion) < 0 {
			d.Notes = append(d.Notes, "Claude Code older than "+permissionPromptsVersion+": update for reliable non-interactive permission handling.")
		}
	} else {
		d.Problems = append(d.Problems, err.Error())
	}

	auth, err := r.authStatus(ctx, path)
	if err != nil {
		d.Problems = append(d.Problems, "could not query `claude auth status`: "+err.Error())
	} else {
		d.Auth = auth
	}
	if d.Auth.State == agent.AuthUnauthenticated {
		d.Problems = append(d.Problems, "Claude Code is not signed in. Run `claude auth login`, or configure ANTHROPIC_API_KEY.")
	}
	d.Ready = len(d.Problems) == 0
	d.Notes = append(d.Notes,
		"Runs the unmodified Claude Code CLI with the user's own authentication; Harness Kit never reads Claude credentials.",
		"Anthropic requires products built for other people to use API-key authentication (Claude Console or a cloud provider), not Claude.ai subscription login.",
		"Interactive approvals go through Harness Kit's MCP permission bridge (on in the CLI and sidecar; Go hosts set session.Options.ToolServer). Without it, prompts are auto-denied.",
	)
	if d.Auth.Method == "claude.ai" || d.Auth.Method == "oauth_token" {
		d.Notes = append(d.Notes, "Signed in with a Claude subscription: usage draws on that plan's limits and is subject to Anthropic's consumer terms for the user's own, individual use.")
	}
	return d, nil
}

func (r *Runtime) authStatus(ctx context.Context, path string) (agent.AuthStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := r.command(path, "auth", "status", "--json")
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Start(); err != nil {
		return agent.AuthStatus{}, err
	}
	stop := context.AfterFunc(ctx, func() { _ = process.Kill(cmd) })
	defer stop()
	runErr := cmd.Wait() // exit status 1 means "not logged in"
	var st struct {
		LoggedIn         bool   `json:"loggedIn"`
		AuthMethod       string `json:"authMethod"`
		SubscriptionType string `json:"subscriptionType"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &st); err != nil {
		if runErr != nil {
			return agent.AuthStatus{}, runErr
		}
		return agent.AuthStatus{}, fmt.Errorf("unexpected output: %w", err)
	}
	if !st.LoggedIn || st.AuthMethod == "none" {
		return agent.AuthStatus{State: agent.AuthUnauthenticated, Method: st.AuthMethod}, nil
	}
	return agent.AuthStatus{State: agent.AuthAuthenticated, Method: st.AuthMethod, Plan: st.SubscriptionType}, nil
}

// Start implements agent.Runtime. No process runs until the first prompt.
func (r *Runtime) Start(ctx context.Context, cfg agent.SessionConfig, h agent.Handler) (agent.Session, error) {
	if err := r.ValidateConfig(cfg); err != nil {
		return nil, err
	}
	o, _ := validatedOptions(cfg)
	path, err := r.lookPath()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrNotInstalled, err)
	}
	version, err := r.version(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("claude: %w", err)
	}
	s := &Session{rt: r, path: path, version: version, cfg: cfg, opts: o, h: h}
	if cfg.NativeDefaults != nil {
		s.nativeModel = cfg.NativeDefaults.Model
	}
	// A legacy resumed transcript may contain a temporary model selection.
	// Without a saved native baseline, inheriting it would be ambiguous.
	if (cfg.ResumeID != "" || cfg.ForkFrom != "") && s.nativeModel == "" {
		s.modelOverrideUsed = true
	}
	if cfg.MCP != nil {
		warnings := s.prepareMCP()
		for _, w := range warnings {
			if h != nil {
				h.Emit(event.Event{Type: event.RuntimeStatus, Data: event.RuntimeStatusData{Status: "warning", Message: w}})
			}
		}
	}
	if cfg.ForkFrom != "" {
		s.forkSource = cfg.ForkFrom
	} else if cfg.ResumeID != "" {
		s.id, s.started, s.resumed = cfg.ResumeID, true, true
	} else {
		s.id = newUUID()
	}
	return s, nil
}

// Fork prepares --resume/--fork-session. Claude creates the new native ID on
// the first real prompt, so RuntimeSessionID remains empty until then.
func (r *Runtime) Fork(ctx context.Context, source string, cfg agent.SessionConfig, h agent.Handler) (agent.Session, error) {
	if source == "" {
		return nil, fmt.Errorf("%w: fork source is empty", agent.ErrInvalidConfig)
	}
	cfg.ResumeID, cfg.ForkFrom = "", source
	return r.Start(ctx, cfg, h)
}

// Session is a Claude Code conversation.
type Session struct {
	rt      *Runtime
	path    string
	version string
	cfg     agent.SessionConfig
	opts    Options
	h       agent.Handler

	mu         sync.Mutex
	id         string
	forkSource string
	started    bool    // the session exists on disk; later turns use --resume
	resumed    bool    // created from ResumeID and not yet forked
	lastCost   float64 // cumulative cost reported by the previous turn
	costBase   bool    // lastCost is a valid baseline
	// Native model selection is learned only from a turn that inherited it.
	// Reusing it prevents a one-turn --model from becoming the resume default.
	nativeModel       string
	modelOverrideUsed bool

	// MCP flags computed once at Start.
	mcpConfig   string   // --mcp-config file (0600, removed on Close)
	mcpStrict   bool     // --strict-mcp-config
	mcpAllowed  []string // added to --allowedTools
	mcpDisallow []string // added to --disallowedTools
}

// prepareMCP translates the session's MCP servers into Claude Code flags.
// ${VAR} references are passed through unexpanded, because Claude Code
// expands them itself in command, args, env, url and headers: secrets never
// appear on the command line. They are checked here so a missing variable
// skips the server with a warning instead of failing obscurely.
func (s *Session) prepareMCP() []string {
	var warnings []string
	lookup := mcp.LookupWith(os.LookupEnv, s.rt.env)
	servers := map[string]any{}
	for _, name := range s.cfg.MCP.Names() {
		srv := s.cfg.MCP.Servers[name]
		if _, err := srv.Expanded(lookup); err != nil {
			warnings = append(warnings, fmt.Sprintf("skipped MCP server %q: %v", name, err))
			continue
		}
		entry := map[string]any{}
		if srv.IsHTTP() {
			entry["type"] = "http"
			entry["url"] = srv.URL
			if len(srv.Headers) > 0 {
				entry["headers"] = srv.Headers
			}
		} else {
			entry["command"] = srv.Command
			if len(srv.Args) > 0 {
				entry["args"] = srv.Args
			}
			if len(srv.Env) > 0 {
				entry["env"] = srv.Env
			}
			if srv.Cwd != "" {
				warnings = append(warnings, fmt.Sprintf("MCP server %q: cwd is not supported by Claude Code and was ignored", name))
			}
		}
		servers[name] = entry

		prefix := "mcp__" + name + "__"
		for _, t := range srv.Tools.Deny {
			s.mcpDisallow = append(s.mcpDisallow, prefix+t)
		}
		switch {
		case srv.EffectiveApproval() == mcp.ApprovalAsk:
			// No allow rule: Claude Code prompts, and the prompt reaches the
			// host through the permission bridge. Without the bridge it can
			// only deny.
			if s.cfg.PermissionTool == "" {
				warnings = append(warnings, fmt.Sprintf("MCP server %q: approval \"ask\" needs Harness Kit's permission bridge; without it its tools will be denied", name))
			}
		case len(srv.Tools.Allow) > 0:
			for _, t := range srv.Tools.Allow {
				s.mcpAllowed = append(s.mcpAllowed, prefix+t)
			}
		default:
			s.mcpAllowed = append(s.mcpAllowed, prefix+"*")
		}
	}
	if len(servers) > 0 {
		// A private file, not an argument: the config can hold the bridge
		// token, and arguments are visible to other users in `ps`.
		b, _ := json.Marshal(map[string]any{"mcpServers": servers})
		f, err := os.CreateTemp("", "hk-claude-mcp-*.json")
		if err == nil {
			_, err = f.Write(b)
			f.Close()
		}
		if err != nil {
			warnings = append(warnings, "could not write the MCP config file; MCP servers are unavailable: "+err.Error())
		} else {
			s.mcpConfig = f.Name()
		}
	}
	s.mcpStrict = !s.cfg.MCP.Inherit()
	return warnings
}

// RuntimeSessionID implements agent.Session.
func (s *Session) RuntimeSessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id
}

// Close implements agent.Session. Turns own their processes; Close only
// removes the session's MCP config file.
func (s *Session) Close(context.Context) error {
	if s.mcpConfig != "" {
		os.Remove(s.mcpConfig)
	}
	return nil
}

func (s *Session) buildArgs(cfg agent.SessionConfig, o Options) []string {
	s.mu.Lock()
	id, started, fork := s.id, s.started, s.resumed && o.ForkOnResume
	if s.forkSource != "" {
		id, started, fork = s.forkSource, true, true
	}
	s.mu.Unlock()
	args := []string{"-p", "--output-format", "stream-json", "--verbose"}
	if !o.DisablePartialMessages {
		args = append(args, "--include-partial-messages")
	}
	if started {
		args = append(args, "--resume", id)
		if fork {
			args = append(args, "--fork-session")
		}
	} else {
		args = append(args, "--session-id", id)
	}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.Instructions != "" {
		args = append(args, "--append-system-prompt", cfg.Instructions)
	}
	if o.PermissionMode != "" {
		args = append(args, "--permission-mode", o.PermissionMode)
	}
	if s.mcpConfig != "" {
		args = append(args, "--mcp-config", s.mcpConfig)
	}
	if s.mcpStrict {
		args = append(args, "--strict-mcp-config")
	}
	if allowed := append(slices.Clone(o.AllowedTools), s.mcpAllowed...); len(allowed) > 0 {
		args = append(args, "--allowedTools", strings.Join(allowed, ","))
	}
	if denied := append(slices.Clone(o.DisallowedTools), s.mcpDisallow...); len(denied) > 0 {
		args = append(args, "--disallowedTools", strings.Join(denied, ","))
	}
	if o.Tools != nil {
		args = append(args, "--tools", strings.Join(*o.Tools, ","))
	}
	for _, d := range o.AddDirs {
		args = append(args, "--add-dir", d)
	}
	if o.Bare {
		args = append(args, "--bare")
	}
	if o.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(o.MaxTurns))
	}
	if o.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(o.MaxBudgetUSD, 'f', -1, 64))
	}
	if o.Effort != "" {
		args = append(args, "--effort", o.Effort)
	}
	if server, tool, ok := strings.Cut(cfg.PermissionTool, "/"); ok {
		args = append(args, "--permission-prompt-tool", "mcp__"+server+"__"+tool)
	} else if process.CompareVersions(s.version, permissionPromptsVersion) >= 0 {
		args = append(args, "--permission-prompts", "none")
	}
	return append(args, s.rt.args...)
}

// NativeDefaults returns the inherited model learned from an unoverridden turn.
// Hosts persist it separately from their requested session defaults.
func (s *Session) NativeDefaults() agent.TurnConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return agent.TurnConfig{Model: s.nativeModel}
}

// PrepareTurn freezes one turn's settings. It never changes session defaults.
// An omitted native model is learned from the first inherited launch, then
// passed explicitly on later turns so a temporary model switch cannot leak.
func (s *Session) PrepareTurn(ctx context.Context, in agent.TurnConfig) (agent.TurnConfig, error) {
	if err := ctx.Err(); err != nil {
		return agent.TurnConfig{}, err
	}
	out := in.Clone()
	cfg := s.cfg.WithSettings(out)
	if err := s.rt.ValidateConfig(cfg); err != nil {
		return agent.TurnConfig{}, err
	}
	o, _ := validatedOptions(cfg)
	// These settings bind the session's MCP bridge or permission context.
	// Even direct adapter callers must not silently modify startup-only fields.
	base := s.opts
	o.Effort, base.Effort = "", ""
	o.MaxTurns, base.MaxTurns = 0, 0
	o.MaxBudgetUSD, base.MaxBudgetUSD = 0, 0
	o.MaxImageBytes, base.MaxImageBytes = nil, nil
	o.DisablePartialMessages, base.DisablePartialMessages = false, false
	if !reflect.DeepEqual(o, base) {
		return agent.TurnConfig{}, fmt.Errorf("%w: claude: startup-only options changed", agent.ErrUnsupported)
	}
	if selected, ok := mcp.LookupWith(os.LookupEnv, s.rt.env)("CLAUDE_CODE_EFFORT_LEVEL"); ok && selected != "" && out.Effort != "" && selected != out.Effort {
		return agent.TurnConfig{}, fmt.Errorf("%w: claude: CLAUDE_CODE_EFFORT_LEVEL overrides the requested effort; change the host environment or omit effort", agent.ErrUnsupported)
	}
	if out.Model == "" {
		s.mu.Lock()
		native, changed := s.nativeModel, s.modelOverrideUsed
		s.mu.Unlock()
		if native != "" {
			out.Model = native
		} else if changed {
			return agent.TurnConfig{}, fmt.Errorf("%w: claude: inherited model is unknown after an explicit model turn; select a model or the explicit default model", agent.ErrUnsupported)
		}
	}
	return out, nil
}

// Prompt implements agent.Session.
func (s *Session) Prompt(ctx context.Context, turn agent.Turn) (agent.TurnResult, error) {
	settings, err := s.cfg.Settings()
	if err != nil {
		return agent.TurnResult{}, err
	}
	if turn.Config != nil {
		settings, err = s.PrepareTurn(ctx, *turn.Config)
		if err != nil {
			return agent.TurnResult{}, err
		}
	}
	cfg := s.cfg.WithSettings(settings)
	o, _ := validatedOptions(cfg)
	input, streamInput, err := encodeInput(ctx, turn.Input, cfg.Cwd, o.MaxImageBytes)
	if err != nil {
		return agent.TurnResult{}, err
	}
	args := s.buildArgs(cfg, o)
	if streamInput {
		args = append(args, "--input-format", "stream-json")
	}
	cmd := s.rt.command(s.path, args...)
	cmd.Dir = cfg.Cwd
	// The prompt goes through stdin: no argv length limits and no risk of
	// a prompt being parsed as a flag.
	cmd.Stdin = bytes.NewReader(input)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return agent.TurnResult{}, err
	}
	stderr := process.NewTail(16 << 10)
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return agent.TurnResult{}, fmt.Errorf("claude: start: %w", err)
	}

	s.mu.Lock()
	if cfg.Model != "" {
		s.modelOverrideUsed = true
	}
	s.mu.Unlock()
	waitDone := make(chan struct{})
	var waitErr error
	parsed := make(chan struct{})
	tr := &turnReader{s: s, turnID: turn.ID, inheritModel: cfg.Model == ""}
	go func() {
		defer close(parsed)
		tr.read(stdout)
	}()
	go func() {
		<-parsed // drain stdout before Wait closes the pipe
		waitErr = cmd.Wait()
		close(waitDone)
	}()

	cancelled := false
	select {
	case <-waitDone:
	case <-ctx.Done():
		cancelled = true
		process.Stop(cmd, waitDone, s.rt.grace)
		<-waitDone
	}

	if cancelled {
		r := tr.result
		r.Status, r.Error = event.TurnStatusCancelled, nil
		return r, nil
	}
	if !tr.gotResult {
		msg := "claude exited without a result"
		if waitErr != nil {
			msg = "claude exited: " + waitErr.Error()
		}
		return agent.TurnResult{}, errors.New(msg + stderr.Suffix())
	}
	return tr.result, nil
}

// turnReader maps one process's stream-json output to events.
type turnReader struct {
	s            *Session
	turnID       string
	inheritModel bool

	curMsg    string
	curIndex  int
	streamed  map[string]bool // message ids seen via stream events
	result    agent.TurnResult
	gotResult bool
}

func (t *turnReader) emit(typ event.Type, data any) {
	if t.s.h != nil {
		t.s.h.Emit(event.Event{Type: typ, TurnID: t.turnID, Data: data})
	}
}

func (t *turnReader) raw(kind string, line []byte) {
	if t.s.cfg.RawEvents {
		t.emit(event.RuntimeRaw, event.RawData{Runtime: Name, Kind: kind, Payload: append(json.RawMessage(nil), line...)})
	}
}

func (t *turnReader) read(r io.Reader) {
	t.streamed = make(map[string]bool)
	br := bufio.NewReaderSize(r, 256<<10)
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			t.handle(bytes.TrimSpace(line))
		}
		if err != nil {
			return
		}
	}
}

type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

type streamLine struct {
	Type            string  `json:"type"`
	Subtype         string  `json:"subtype"`
	SessionID       string  `json:"session_id"`
	ParentToolUseID *string `json:"parent_tool_use_id"`
	Model           string  `json:"model"`
	PermissionMode  string  `json:"permissionMode"`
	Message         struct {
		ID      string          `json:"id"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	Event struct {
		Type    string `json:"type"`
		Index   int    `json:"index"`
		Message struct {
			ID string `json:"id"`
		} `json:"message"`
		Delta struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Thinking string `json:"thinking"`
		} `json:"delta"`
	} `json:"event"`
	// result
	IsError      bool     `json:"is_error"`
	Result       string   `json:"result"`
	TotalCostUSD *float64 `json:"total_cost_usd"`
	Usage        struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
	Errors []string `json:"errors"`
	// system/api_retry
	Error         string `json:"error"`
	Attempt       int    `json:"attempt"`
	RateLimitInfo struct {
		Status string `json:"status"`
	} `json:"rate_limit_info"`
}

func (t *turnReader) handle(line []byte) {
	var m streamLine
	if err := json.Unmarshal(line, &m); err != nil {
		t.emit(event.RuntimeStatus, event.RuntimeStatusData{Status: "warning", Message: "unparseable output line from claude"})
		return
	}
	parent := ""
	if m.ParentToolUseID != nil {
		parent = *m.ParentToolUseID
	}
	switch m.Type {
	case "system":
		t.system(m, line)
	case "stream_event":
		t.streamEvent(m, parent)
	case "assistant":
		t.assistant(m, parent)
	case "user":
		t.user(m)
	case "result":
		t.resultLine(m)
	case "rate_limit_event":
		if m.RateLimitInfo.Status != "" && m.RateLimitInfo.Status != "allowed" {
			t.emit(event.RuntimeStatus, event.RuntimeStatusData{Status: "rate_limited", Message: m.RateLimitInfo.Status, Detail: append(json.RawMessage(nil), line...)})
		} else {
			t.raw(m.Type, line)
		}
	default:
		t.raw(m.Type, line)
	}
}

func (t *turnReader) system(m streamLine, line []byte) {
	switch m.Subtype {
	case "init":
		t.s.mu.Lock()
		if m.SessionID != "" {
			t.s.id = m.SessionID // differs from the requested ID after a fork
			t.s.forkSource = ""
		}
		if t.inheritModel && t.s.nativeModel == "" && m.Model != "" {
			t.s.nativeModel = m.Model
		}
		t.s.started, t.s.resumed = true, false
		t.s.mu.Unlock()
		detail, _ := json.Marshal(map[string]string{"model": m.Model, "permissionMode": m.PermissionMode})
		t.emit(event.RuntimeStatus, event.RuntimeStatusData{Status: "ready", Message: "model " + m.Model, Detail: detail})
	case "api_retry":
		t.emit(event.RuntimeStatus, event.RuntimeStatusData{Status: "retrying", Message: m.Error, Detail: append(json.RawMessage(nil), line...)})
	case "permission_denied":
		t.emit(event.RuntimeStatus, event.RuntimeStatusData{Status: "permission_denied", Message: "a tool call needing approval was denied", Detail: append(json.RawMessage(nil), line...)})
	case "compact_boundary":
		t.emit(event.RuntimeStatus, event.RuntimeStatusData{Status: "compacted"})
	default:
		t.raw("system/"+m.Subtype, line)
	}
}

func (t *turnReader) itemID(msg string, idx int) string { return msg + "#" + strconv.Itoa(idx) }

func (t *turnReader) streamEvent(m streamLine, parent string) {
	ev := m.Event
	switch ev.Type {
	case "message_start":
		t.curMsg = ev.Message.ID
		t.streamed[t.curMsg] = true
	case "content_block_start":
		t.curIndex = ev.Index
	case "content_block_delta":
		switch ev.Delta.Type {
		case "text_delta":
			t.emit(event.MessageDelta, event.MessageDeltaData{ItemID: t.itemID(t.curMsg, ev.Index), Text: ev.Delta.Text, ParentCallID: parent})
		case "thinking_delta":
			t.emit(event.ReasoningDelta, event.ReasoningDeltaData{ItemID: t.itemID(t.curMsg, ev.Index), Text: ev.Delta.Thinking})
		}
	}
}

func (t *turnReader) assistant(m streamLine, parent string) {
	var blocks []contentBlock
	if json.Unmarshal(m.Message.Content, &blocks) != nil {
		return
	}
	for i, b := range blocks {
		idx := i
		// With partial messages Claude Code emits one assistant message per
		// finished content block; align its ID with the streamed block.
		if len(blocks) == 1 && t.streamed[m.Message.ID] && m.Message.ID == t.curMsg {
			idx = t.curIndex
		}
		switch b.Type {
		case "text":
			t.emit(event.MessageCompleted, event.MessageCompletedData{ItemID: t.itemID(m.Message.ID, idx), Role: "assistant", Text: b.Text, ParentCallID: parent})
		case "thinking":
			if b.Thinking != "" {
				t.emit(event.ReasoningCompleted, event.ReasoningCompletedData{ItemID: t.itemID(m.Message.ID, idx), Text: b.Thinking})
			}
		case "tool_use":
			t.emit(event.ToolStarted, event.ToolStartedData{
				CallID: b.ID, Name: b.Name, Kind: toolKind(b.Name), Title: toolTitle(b.Name, b.Input),
				Input: b.Input, ParentCallID: parent,
			})
		}
	}
}

func (t *turnReader) user(m streamLine) {
	var blocks []contentBlock
	if json.Unmarshal(m.Message.Content, &blocks) != nil {
		return // plain-string user content (prompt echo or subagent prompt)
	}
	for _, b := range blocks {
		if b.Type != "tool_result" {
			continue
		}
		d := event.ToolCompletedData{CallID: b.ToolUseID, Status: event.ToolStatusCompleted, Output: resultText(b.Content)}
		if b.IsError {
			d.Status, d.Error = event.ToolStatusFailed, d.Output
		}
		t.emit(event.ToolCompleted, d)
	}
}

func (t *turnReader) resultLine(m streamLine) {
	t.gotResult = true
	usage := &event.UsageData{
		Scope:             "turn",
		InputTokens:       m.Usage.InputTokens,
		OutputTokens:      m.Usage.OutputTokens,
		CachedInputTokens: m.Usage.CacheReadInputTokens,
		CacheWriteTokens:  m.Usage.CacheCreationInputTokens,
	}
	usage.TotalTokens = usage.InputTokens + usage.OutputTokens + usage.CachedInputTokens + usage.CacheWriteTokens
	if m.TotalCostUSD != nil {
		// total_cost_usd is cumulative for the conversation; derive the
		// turn's share when a baseline is known.
		t.s.mu.Lock()
		// A fresh session starts from zero; a resumed one has no baseline
		// until its first turn reports a total.
		if t.s.costBase || t.s.cfg.ResumeID == "" {
			delta := *m.TotalCostUSD - t.s.lastCost
			if delta >= 0 {
				usage.CostUSD = &delta
			}
		}
		t.s.lastCost, t.s.costBase = *m.TotalCostUSD, true
		t.s.mu.Unlock()
		total := *m.TotalCostUSD
		t.emit(event.Usage, event.UsageData{Scope: "session", CostUSD: &total})
	}
	t.emit(event.Usage, *usage)
	t.result.Usage = usage

	if m.IsError || (m.Subtype != "" && m.Subtype != "success") {
		msg := m.Result
		if msg == "" && len(m.Errors) > 0 {
			msg = strings.Join(m.Errors, "; ")
		}
		if msg == "" {
			msg = "claude reported " + m.Subtype
		}
		code := "claude_" + m.Subtype
		if m.Subtype == "" || m.Subtype == "success" {
			code = "claude_error"
		}
		t.result.Status = event.TurnStatusFailed
		t.result.Error = &event.ErrorInfo{Code: code, Message: msg}
		return
	}
	t.result.Status = event.TurnStatusCompleted
}

func resultText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "text" {
				b.WriteString(p.Text)
			}
		}
		return b.String()
	}
	return string(raw)
}

func toolKind(name string) event.ToolKind {
	switch {
	case name == "Bash" || name == "PowerShell" || name == "BashOutput":
		return event.ToolKindCommand
	case name == "Edit" || name == "Write" || name == "MultiEdit" || name == "NotebookEdit":
		return event.ToolKindFileChange
	case name == "Read":
		return event.ToolKindFileRead
	case name == "Grep" || name == "Glob" || name == "LS":
		return event.ToolKindSearch
	case name == "WebSearch" || name == "WebFetch":
		return event.ToolKindWebSearch
	case name == "Task" || name == "Agent":
		return event.ToolKindSubagent
	case strings.HasPrefix(name, "mcp__"):
		return event.ToolKindMCP
	}
	return event.ToolKindOther
}

func toolTitle(name string, input json.RawMessage) string {
	var in map[string]any
	_ = json.Unmarshal(input, &in)
	for _, k := range []string{"command", "file_path", "pattern", "url", "query", "description"} {
		if v, ok := in[k].(string); ok && v != "" {
			return name + ": " + v
		}
	}
	return name
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
