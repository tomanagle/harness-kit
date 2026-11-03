// Package acp drives arbitrary Agent Client Protocol v1 agents over stdio.
// It has no preferred agent, filesystem implementation, or shell policy.
package acp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tomanagle/harness-kit/agent"
	"github.com/tomanagle/harness-kit/config"
	"github.com/tomanagle/harness-kit/event"
	"github.com/tomanagle/harness-kit/internal/jsonrpc"
	"github.com/tomanagle/harness-kit/mcp"
	"github.com/tomanagle/harness-kit/process"
)

const ProtocolVersion = 1

// ClientInfo identifies the embedding application to the agent.
type ClientInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

type Option func(*Runtime)

func WithName(name string) Option    { return func(r *Runtime) { r.name = name } }
func WithArgs(args ...string) Option { return func(r *Runtime) { r.args = slices.Clone(args) } }

// WithEnv overlays the inherited environment. ${VAR} references are expanded
// from the parent environment when each process starts, never during storage.
func WithEnv(env map[string]string) Option     { return func(r *Runtime) { r.env = maps.Clone(env) } }
func WithStartTimeout(d time.Duration) Option  { return func(r *Runtime) { r.startTimeout = d } }
func WithCancelTimeout(d time.Duration) Option { return func(r *Runtime) { r.cancelTimeout = d } }
func WithClientInfo(info ClientInfo) Option    { return func(r *Runtime) { r.clientInfo = info } }
func WithFileSystem(fs FileSystem) Option      { return func(r *Runtime) { r.fs = fs } }
func WithTerminal(host TerminalHost) Option    { return func(r *Runtime) { r.terminal = host } }

// WithMaxMessageSize bounds inbound JSON lines. Default: 32 MiB.
func WithMaxMessageSize(n int) Option { return func(r *Runtime) { r.maxMessageSize = n } }

// WithMaxImageBytes bounds each image before base64 encoding. Default: 16 MiB.
func WithMaxImageBytes(n int64) Option { return func(r *Runtime) { r.maxImageBytes = n } }

// WithMaxRetainedOutput bounds each completed text/tool snapshot in memory.
// Streaming deltas remain complete. Default: 8 MiB per item.
func WithMaxRetainedOutput(n int) Option { return func(r *Runtime) { r.maxRetainedOutput = n } }

// Runtime launches a separate ACP process for every live session.
type Runtime struct {
	command, name                     string
	args                              []string
	env                               map[string]string
	startTimeout, cancelTimeout       time.Duration
	clientInfo                        ClientInfo
	fs                                FileSystem
	terminal                          TerminalHost
	maxMessageSize, maxRetainedOutput int
	maxImageBytes                     int64
	mu                                sync.RWMutex
	capabilities                      capabilities
}

func New(command string, options ...Option) *Runtime {
	r := &Runtime{command: command, name: "acp", startTimeout: 30 * time.Second, cancelTimeout: 5 * time.Second,
		clientInfo: ClientInfo{Name: "harness-kit", Version: "0.1.0"}, maxMessageSize: 32 << 20, maxImageBytes: 16 << 20, maxRetainedOutput: 8 << 20}
	for _, option := range options {
		option(r)
	}
	return r
}

func (r *Runtime) Info() agent.Info {
	r.mu.RLock()
	caps := r.capabilities
	r.mu.RUnlock()
	return agent.Info{Name: r.name, DisplayName: r.name, Description: "Agent Client Protocol v1 over stdio",
		Capabilities: agent.Capabilities{Streaming: true, Approvals: true, Cancel: true, MCP: true, Images: caps.Prompt.Image,
			Resume: caps.LoadSession || caps.session("resume"), Usage: true, Cost: true,
			Configuration: map[string]config.Support{
				"model": {Startup: true, Session: true, Turn: true}, "effort": {Startup: true, Session: true, Turn: true},
				"options.config":     {Startup: true, Session: true, Turn: true},
				"options.authMethod": {Startup: true}, "options.modelConfigId": {Startup: true}, "options.effortConfigId": {Startup: true},
			}}}
}

func (r *Runtime) Discover(ctx context.Context) (agent.Discovery, error) {
	d := agent.Discovery{Runtime: r.name, Auth: agent.AuthStatus{State: agent.AuthUnknown}, Capabilities: r.Info().Capabilities}
	path, err := process.LookPath(r.command, process.LookOptions{})
	if err != nil {
		d.Problems = []string{err.Error()}
		return d, nil
	}
	d.Installed, d.Path = true, path
	s, err := r.launch(ctx, agent.SessionConfig{}, nil)
	if err != nil {
		d.Problems = []string{err.Error()}
		return d, nil
	}
	defer s.Close(context.Background())
	d.Version = s.init.AgentInfo.Version
	d.Ready = true
	d.Capabilities = r.Info().Capabilities
	d.Notes = []string{"ACP initialization does not report authentication state; authentication is checked when a session starts."}
	return d, nil
}

func (r *Runtime) ValidateConfig(cfg agent.SessionConfig) error {
	if cfg.Instructions != "" {
		return fmt.Errorf("%w: ACP v1 has no portable system-instructions field", agent.ErrUnsupported)
	}
	var opts Options
	if err := agent.DecodeOptions(cfg.Options, &opts); err != nil {
		return fmt.Errorf("%w: %v", agent.ErrInvalidConfig, err)
	}
	for id, value := range opts.Config {
		if id == "" || !validValue(value) {
			return fmt.Errorf("%w: ACP config values must be strings or booleans", agent.ErrInvalidConfig)
		}
	}
	return nil
}

func (r *Runtime) Start(ctx context.Context, cfg agent.SessionConfig, h agent.Handler) (agent.Session, error) {
	if err := r.ValidateConfig(cfg); err != nil {
		return nil, err
	}
	if h == nil {
		return nil, fmt.Errorf("%w: ACP requires a handler", agent.ErrInvalidConfig)
	}
	ctx, cancel := context.WithTimeout(ctx, r.startTimeout)
	defer cancel()
	s, err := r.launch(ctx, cfg, h)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = s.Close(context.Background())
		}
	}()
	_ = agent.DecodeOptions(cfg.Options, &s.opts)
	if s.opts.AuthMethod != "" {
		found := false
		for _, method := range s.init.AuthMethods {
			if method.ID == s.opts.AuthMethod {
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("%w: agent did not advertise auth method %q", agent.ErrUnsupported, s.opts.AuthMethod)
		}
		if err = s.rpc(ctx, "authenticate", map[string]any{"methodId": s.opts.AuthMethod}, nil); err != nil {
			return nil, err
		}
	}
	servers, err := s.mcpServers(cfg.MCP)
	if err != nil {
		return nil, err
	}
	method := "session/new"
	params := map[string]any{"cwd": s.cwd, "mcpServers": servers}
	if cfg.ResumeID != "" {
		params["sessionId"] = cfg.ResumeID
		s.mu.Lock()
		s.id = cfg.ResumeID
		s.mu.Unlock()
		switch {
		case s.init.AgentCapabilities.session("resume"):
			method = "session/resume"
		case s.init.AgentCapabilities.LoadSession:
			method = "session/load"
		default:
			return nil, fmt.Errorf("%w: agent does not support session resume or load", agent.ErrUnsupported)
		}
	}
	var response sessionResponse
	if err = s.rpc(ctx, method, params, &response); err != nil {
		return nil, fmt.Errorf("acp %s: %w", method, err)
	}
	if method == "session/new" {
		s.mu.Lock()
		s.id = response.SessionID
		s.mu.Unlock()
	}
	if s.id == "" {
		return nil, fmt.Errorf("acp: session/new returned no sessionId")
	}
	s.replaceConfig(response.ConfigOptions)
	s.captureDefaults(cfg.NativeDefaults)
	selected, err := s.PrepareTurn(ctx, s.initialConfig())
	if err != nil {
		return nil, err
	}
	if err = s.applyConfig(ctx, selected); err != nil {
		return nil, err
	}
	ok = true
	return s, nil
}

func (r *Runtime) launch(ctx context.Context, cfg agent.SessionConfig, h agent.Handler) (*Session, error) {
	if r.command == "" || r.name == "" || r.startTimeout <= 0 || r.cancelTimeout <= 0 || r.maxMessageSize <= 0 || r.maxImageBytes <= 0 || r.maxRetainedOutput <= 0 {
		return nil, fmt.Errorf("%w: ACP command, name, timeouts and buffer limits must be set", agent.ErrInvalidConfig)
	}
	ctx, cancel := context.WithTimeout(ctx, r.startTimeout)
	defer cancel()
	path, err := process.LookPath(r.command, process.LookOptions{})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", agent.ErrNotInstalled, err)
	}
	cwd := cfg.Cwd
	if cwd == "" {
		cwd, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	cwd, err = filepath.Abs(cwd)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(path, r.args...)
	cmd.Dir = cwd
	cmd.Env = os.Environ()
	for _, key := range slices.Sorted(maps.Keys(r.env)) {
		if key == "" || strings.ContainsAny(key, "=\x00") {
			return nil, fmt.Errorf("%w: invalid environment key", agent.ErrInvalidConfig)
		}
		value, e := mcp.Expand(r.env[key], os.LookupEnv)
		if e != nil {
			return nil, e
		}
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	process.Configure(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	// Own stdout's pipe so cmd.Wait cannot close it before the reader drains
	// the final response from an agent that exits immediately afterward.
	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	cmd.Stdout = stdoutWriter
	s := &Session{rt: r, cfg: cfg, h: h, cwd: cwd, cmd: cmd, stdin: stdin, stdout: stdout, exited: make(chan struct{}), readerDone: make(chan struct{}), requests: map[string]context.CancelFunc{}}
	s.life, s.stop = context.WithCancel(context.Background())
	cmd.Stderr = process.NewTail(16 << 10)
	s.conn = jsonrpc.NewConn(stdout, stdin, s, jsonrpc.Options{MaxMessageSize: r.maxMessageSize})
	if err = cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stdoutWriter.Close()
		s.stop()
		return nil, err
	}
	_ = stdoutWriter.Close()
	go func() { _ = s.conn.Run(s.life); close(s.readerDone) }()
	go func() { _ = cmd.Wait(); close(s.exited) }()
	clientCaps := map[string]any{"session": map[string]any{"configOptions": map[string]any{"boolean": map[string]any{}}}}
	if r.fs.ReadTextFile != nil || r.fs.WriteTextFile != nil {
		clientCaps["fs"] = map[string]bool{"readTextFile": r.fs.ReadTextFile != nil, "writeTextFile": r.fs.WriteTextFile != nil}
	}
	if r.terminal != nil {
		clientCaps["terminal"] = true
	}
	err = s.rpc(ctx, "initialize", map[string]any{"protocolVersion": ProtocolVersion, "clientInfo": r.clientInfo, "clientCapabilities": clientCaps}, &s.init)
	if err == nil && s.init.ProtocolVersion != ProtocolVersion {
		err = fmt.Errorf("%w: agent negotiated ACP v%d; adapter supports v%d", agent.ErrUnsupported, s.init.ProtocolVersion, ProtocolVersion)
	}
	if err != nil {
		_ = s.Close(context.Background())
		return nil, fmt.Errorf("acp initialize: %w", err)
	}
	r.mu.Lock()
	r.capabilities = s.init.AgentCapabilities
	r.mu.Unlock()
	return s, nil
}

// Session holds one process and one negotiated ACP session.
type Session struct {
	rt                 *Runtime
	cfg                agent.SessionConfig
	opts               Options
	h                  agent.Handler
	cwd, id            string
	cmd                *exec.Cmd
	stdin              io.WriteCloser
	stdout             io.ReadCloser
	conn               *jsonrpc.Conn
	life               context.Context
	stop               context.CancelFunc
	exited, readerDone chan struct{}
	closeOnce          sync.Once
	init               initializeResponse
	mu                 sync.Mutex
	active             *turnState
	configOptions      []ConfigOption
	native             agent.TurnConfig
	requests           map[string]context.CancelFunc
	turnMu             sync.Mutex
}

func (s *Session) RuntimeSessionID() string { s.mu.Lock(); defer s.mu.Unlock(); return s.id }

func (s *Session) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		if s.id != "" && s.init.AgentCapabilities.session("close") && s.live() == nil {
			closeCtx, cancel := context.WithTimeout(ctx, s.rt.cancelTimeout)
			_ = s.rpc(closeCtx, "session/close", map[string]any{"sessionId": s.id}, nil)
			cancel()
		}
		s.stop()
		_ = s.stdin.Close()
		timer := time.NewTimer(s.rt.cancelTimeout)
		defer timer.Stop()
		select {
		case <-s.exited:
		case <-ctx.Done():
			_ = process.Kill(s.cmd)
		case <-timer.C:
			_ = process.Kill(s.cmd)
		}
		_ = s.stdout.Close()
		if s.rt.terminal != nil && s.id != "" {
			releaseCtx, cancel := context.WithTimeout(ctx, s.rt.cancelTimeout)
			_ = s.rt.terminal.ReleaseSession(releaseCtx, s.id)
			cancel()
		}
		// Kill closes process pipes; do not wait on a host callback that ignores context.
	})
	return nil
}

func (s *Session) Prompt(ctx context.Context, turn agent.Turn) (agent.TurnResult, error) {
	if !s.turnMu.TryLock() {
		return agent.TurnResult{}, fmt.Errorf("acp: a turn is already running")
	}
	defer s.turnMu.Unlock()
	if err := s.live(); err != nil {
		return agent.TurnResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return agent.TurnResult{Status: event.TurnStatusCancelled}, nil
	}
	selected := s.initialConfig()
	if turn.Config != nil {
		selected = turn.Config.Clone()
	}
	selected, err := s.PrepareTurn(ctx, selected)
	if err != nil {
		return agent.TurnResult{}, err
	}
	if err = s.applyConfig(ctx, selected); err != nil {
		return agent.TurnResult{}, err
	}
	content, err := s.content(turn.Input)
	if err != nil {
		return agent.TurnResult{}, err
	}
	turnCtx, turnCancel := context.WithCancel(ctx)
	defer turnCancel()
	state := &turnState{id: turn.ID, ctx: turnCtx, cancel: turnCancel, tools: map[string]*toolState{}}
	s.mu.Lock()
	s.active = state
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.finishTextLocked(); s.active = nil; s.mu.Unlock() }()
	callCtx, callCancel := context.WithCancel(s.life)
	defer callCancel()
	type outcome struct {
		result promptResponse
		err    error
	}
	finished := make(chan outcome, 1)
	go func() {
		var response promptResponse
		err := s.conn.Call(callCtx, "session/prompt", map[string]any{"sessionId": s.id, "prompt": content}, &response)
		finished <- outcome{response, err}
	}()
	var out outcome
	cancelled := false
	select {
	case out = <-finished:
	case <-turnCtx.Done():
		cancelled = true
		// Sending can block behind a large prompt to an agent that stopped
		// reading stdin. The cancellation deadline must also bound that write.
		sent := make(chan error, 1)
		go func() { sent <- s.conn.Notify(s.life, "session/cancel", map[string]any{"sessionId": s.id}) }()
		timer := time.NewTimer(s.rt.cancelTimeout)
		defer timer.Stop()
		// Wait for the notification write too: a delayed cancel must never
		// arrive after the caller starts the following prompt.
		for sent != nil || finished != nil {
			select {
			case out = <-finished:
				finished = nil
			case err = <-sent:
				sent = nil
				if err != nil {
					_ = s.Close(context.Background())
					return agent.TurnResult{Status: event.TurnStatusCancelled}, fmt.Errorf("%w: %v", agent.ErrSessionDead, err)
				}
			case <-timer.C:
				callCancel()
				_ = process.Kill(s.cmd)
				_ = s.Close(context.Background())
				return agent.TurnResult{Status: event.TurnStatusCancelled}, fmt.Errorf("%w: ACP agent did not acknowledge cancellation", agent.ErrSessionDead)
			}
		}
	}
	if out.err != nil {
		if s.live() != nil || errors.Is(out.err, jsonrpc.ErrClosed) {
			return agent.TurnResult{}, fmt.Errorf("%w: %v", agent.ErrSessionDead, out.err)
		}
		if cancelled {
			return agent.TurnResult{Status: event.TurnStatusCancelled}, nil
		}
		return agent.TurnResult{}, out.err
	}
	result := agent.TurnResult{Status: event.TurnStatusCompleted}
	switch out.result.StopReason {
	case "cancelled":
		result.Status = event.TurnStatusCancelled
	case "end_turn":
	case "max_tokens", "max_turn_requests", "refusal":
		result.Status = event.TurnStatusFailed
		result.Error = &event.ErrorInfo{Code: out.result.StopReason, Message: "ACP agent stopped: " + out.result.StopReason}
	default:
		return agent.TurnResult{}, fmt.Errorf("acp: unknown stop reason %q", out.result.StopReason)
	}
	if cancelled {
		result.Status = event.TurnStatusCancelled
	}
	return result, nil
}

func (s *Session) live() error {
	select {
	case <-s.life.Done():
		return agent.ErrSessionDead
	case <-s.conn.Done():
		return agent.ErrSessionDead
	default:
		return nil
	}
}

// rpc bounds both pipe writes and response waiting. On cancellation the
// caller closes the process; response unmarshalling stays on the caller's
// goroutine so late replies cannot mutate abandoned response values.
func (s *Session) rpc(ctx context.Context, method string, params, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	type reply struct {
		data json.RawMessage
		err  error
	}
	done := make(chan reply, 1)
	go func() {
		var data json.RawMessage
		err := s.conn.Call(ctx, method, params, &data)
		done <- reply{data, err}
	}()
	select {
	case response := <-done:
		if response.err != nil {
			return response.err
		}
		if result != nil && len(response.data) > 0 {
			return json.Unmarshal(response.data, result)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Session) content(parts []agent.InputPart) ([]any, error) {
	out := make([]any, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case "", "text":
			out = append(out, map[string]any{"type": "text", "text": part.Text})
		case "image":
			if !s.init.AgentCapabilities.Prompt.Image {
				return nil, fmt.Errorf("%w: ACP agent does not accept images", agent.ErrUnsupported)
			}
			path := part.Path
			if !filepath.IsAbs(path) {
				path = filepath.Join(s.cwd, path)
			}
			file, err := os.Open(path)
			if err != nil {
				return nil, err
			}
			data, err := io.ReadAll(io.LimitReader(file, s.rt.maxImageBytes+1))
			_ = file.Close()
			if err != nil {
				return nil, err
			}
			if int64(len(data)) > s.rt.maxImageBytes {
				return nil, fmt.Errorf("%w: image exceeds %d bytes", agent.ErrInvalidConfig, s.rt.maxImageBytes)
			}
			mime := http.DetectContentType(data)
			if !strings.HasPrefix(mime, "image/") {
				return nil, fmt.Errorf("%w: unsupported image format %s", agent.ErrInvalidConfig, mime)
			}
			out = append(out, map[string]any{"type": "image", "mimeType": mime, "data": base64.StdEncoding.EncodeToString(data)})
		default:
			return nil, fmt.Errorf("%w: input type %q", agent.ErrUnsupported, part.Type)
		}
	}
	return out, nil
}

func (s *Session) mcpServers(cfg *mcp.Config) ([]any, error) {
	out := []any{}
	if cfg == nil {
		return out, nil
	}
	if !cfg.Inherit() {
		return nil, fmt.Errorf("%w: ACP cannot disable native MCP servers", agent.ErrUnsupported)
	}
	for _, name := range cfg.Names() {
		server := cfg.Servers[name]
		if !server.AppliesTo(s.rt.name) {
			continue
		}
		if server.Cwd != "" || len(server.Tools.Allow) > 0 || len(server.Tools.Deny) > 0 || server.EffectiveApproval() == mcp.ApprovalAsk || server.StartupTimeoutSec != 0 || server.ToolTimeoutSec != 0 {
			return nil, fmt.Errorf("%w: ACP MCP server %q cannot apply cwd, tool filters, approval policy or timeouts", agent.ErrUnsupported, name)
		}
		var err error
		server, err = server.Expanded(mcp.LookupWith(os.LookupEnv, s.cmd.Env))
		if err != nil {
			return nil, err
		}
		if server.IsHTTP() {
			if !s.init.AgentCapabilities.MCP.HTTP {
				return nil, fmt.Errorf("%w: agent does not accept HTTP MCP servers", agent.ErrUnsupported)
			}
			out = append(out, map[string]any{"type": "http", "name": name, "url": server.URL, "headers": pairs(server.Headers)})
		} else if server.Type == "" || server.Type == "stdio" {
			args := server.Args
			if args == nil {
				args = []string{}
			}
			out = append(out, map[string]any{"name": name, "command": server.Command, "args": args, "env": pairs(server.Env)})
		} else {
			return nil, fmt.Errorf("%w: MCP transport %q", agent.ErrUnsupported, server.Type)
		}
	}
	return out, nil
}

type NamedValue struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func pairs(values map[string]string) []NamedValue {
	out := []NamedValue{}
	for _, key := range slices.Sorted(maps.Keys(values)) {
		out = append(out, NamedValue{key, values[key]})
	}
	return out
}

var _ agent.Runtime = (*Runtime)(nil)
var _ agent.Configurable = (*Runtime)(nil)
var _ agent.TurnPreparer = (*Session)(nil)
var _ agent.DefaultsProvider = (*Session)(nil)
