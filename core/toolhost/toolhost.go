// Package toolhost serves a tools.Registry to runtime sessions (Codex,
// Claude Code) as an MCP server, and answers Claude Code's permission
// prompts, without opening a network port.
//
// How it fits together:
//
//	runtime ──stdio──► bridge process ──Unix socket──► Host (this process)
//	                    (harness-kit, relay only)        MCP server: tools/list, tools/call,
//	                                                     approvals, permission prompts
//
// The Host listens on a socket inside a private (0700) temporary
// directory. Each session gets a random token; the bridge presents it, so a
// connection reaches exactly one session's tools and approval flow. The
// bridge is any executable that calls ServeBridgeIfRequested at the start
// of main (the harness-kit binary does). Tool handlers and approvals run
// in the host process, where the registry and the session manager live.
package toolhost

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tomanagle/harness-kit/core/internal/jsonrpc"
	"github.com/tomanagle/harness-kit/core/mcp"
	"github.com/tomanagle/harness-kit/core/permission"
	"github.com/tomanagle/harness-kit/core/session"
	"github.com/tomanagle/harness-kit/core/tools"
)

// ServerName is the MCP server name sessions see.
const ServerName = mcp.ReservedName

// PermissionTool answers Claude Code permission prompts
// (--permission-prompt-tool). Claude Code hides it from the model.
const PermissionTool = "approval_prompt"

const (
	envSocket = "HARNESS_KIT_BRIDGE_SOCKET"
	envToken  = "HARNESS_KIT_BRIDGE_TOKEN"
)

// ToolRegistry supplies a host's tool catalog and execution backend. It may be
// backed by tools.Registry, a remote service, or another application registry.
// Implementations must be safe for concurrent use. Call owns argument
// validation, execution limits, and cancellation; the Host enforces each
// session's tool allowlist and the approval metadata returned by Get.
type ToolRegistry interface {
	Get(string) (tools.Tool, bool)
	List() []tools.Tool
	Call(context.Context, tools.Call) (tools.Result, error)
}

// ToolChangeNotifier is an optional ToolRegistry capability. OnChange returns
// an unsubscribe function; callbacks notify connected agents to reload tools.
// Static catalogs need only implement ToolRegistry.
type ToolChangeNotifier interface {
	OnChange(func()) func()
}

var _ ToolRegistry = (*tools.Registry)(nil)
var _ ToolChangeNotifier = (*tools.Registry)(nil)

// Options configure a Host.
type Options struct {
	// Registry holds the host tools. Nil means none (the Host then only
	// answers permission prompts).
	Registry ToolRegistry
	// BridgeCommand starts the bridge: an executable that calls
	// ServeBridgeIfRequested first thing in main. Defaults to this
	// executable.
	BridgeCommand []string
	// Version is reported to runtimes.
	Version string
	Logger  *slog.Logger
}

// Host serves tools to sessions. It implements session.ToolServer.
type Host struct {
	opts Options
	log  *slog.Logger

	mu       sync.Mutex
	ln       net.Listener
	dir      string
	sock     string
	bindings map[string]*binding // token -> binding
	closed   bool
	stopList func()
}

var _ session.ToolServer = (*Host)(nil)

// New returns a Host. The socket is created on the first Attach.
func New(opts Options) *Host {
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opts.Version == "" {
		opts.Version = "dev"
	}
	h := &Host{opts: opts, log: opts.Logger, bindings: map[string]*binding{}}
	if changes, ok := opts.Registry.(ToolChangeNotifier); ok {
		h.stopList = changes.OnChange(h.notifyListChanged)
	}
	return h
}

type binding struct {
	session.ToolBinding
	ctx    context.Context
	cancel context.CancelFunc

	mu    sync.Mutex
	conns map[*jsonrpc.Conn]net.Conn
}

func (b *binding) allows(name string) bool { return b.Tools == nil || slices.Contains(b.Tools, name) }

// Attach implements session.ToolServer.
func (h *Host) Attach(_ context.Context, sb session.ToolBinding) (session.Attachment, error) {
	if !sb.ApprovalPrompt && len(h.toolsFor(sb.Tools)) == 0 {
		return session.Attachment{}, nil // nothing to serve: don't start a bridge
	}
	bridge, err := h.bridgeCommand()
	if err != nil {
		return session.Attachment{}, err
	}
	sock, err := h.listen()
	if err != nil {
		return session.Attachment{}, err
	}
	token, err := newToken()
	if err != nil {
		return session.Attachment{}, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	b := &binding{ToolBinding: sb, ctx: ctx, cancel: cancel, conns: map[*jsonrpc.Conn]net.Conn{}}
	h.mu.Lock()
	h.bindings[token] = b
	h.mu.Unlock()

	att := session.Attachment{
		Name: ServerName,
		Server: mcp.Server{
			Command: bridge[0], Args: bridge[1:],
			Env:               map[string]string{envSocket: sock, envToken: token},
			Approval:          mcp.ApprovalAuto, // Harness Kit enforces tool approvals itself
			StartupTimeoutSec: 20,
		},
		Detach: func() { h.detach(token) },
	}
	if sb.ApprovalPrompt {
		att.PermissionTool = PermissionTool
	}
	return att, nil
}

func (h *Host) detach(token string) {
	h.mu.Lock()
	b := h.bindings[token]
	delete(h.bindings, token)
	h.mu.Unlock()
	if b == nil {
		return
	}
	b.cancel()
	b.mu.Lock()
	for _, nc := range b.conns {
		nc.Close()
	}
	b.mu.Unlock()
}

// Close stops listening and removes the socket directory.
func (h *Host) Close() error {
	h.mu.Lock()
	h.closed = true
	ln, dir := h.ln, h.dir
	tokens := make([]string, 0, len(h.bindings))
	for t := range h.bindings {
		tokens = append(tokens, t)
	}
	h.mu.Unlock()
	for _, t := range tokens {
		h.detach(t)
	}
	if h.stopList != nil {
		h.stopList()
	}
	var err error
	if ln != nil {
		err = ln.Close()
	}
	if dir != "" {
		os.RemoveAll(dir)
	}
	return err
}

func (h *Host) bridgeCommand() ([]string, error) {
	if len(h.opts.BridgeCommand) > 0 {
		return h.opts.BridgeCommand, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("toolhost: locate bridge executable: %w", err)
	}
	return []string{exe, "mcp-bridge"}, nil
}

func (h *Host) listen() (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return "", errors.New("toolhost: closed")
	}
	if h.ln != nil {
		return h.sock, nil
	}
	dir, err := os.MkdirTemp("", "hk-")
	if err != nil {
		return "", err
	}
	sock := filepath.Join(dir, "tools.sock")
	if len(sock) > 100 && runtime.GOOS != "windows" { // sun_path limit
		os.RemoveAll(dir)
		if dir, err = os.MkdirTemp("/tmp", "hk-"); err != nil {
			return "", err
		}
		sock = filepath.Join(dir, "tools.sock")
	}
	os.Chmod(dir, 0o700)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("toolhost: listen: %w", err)
	}
	h.ln, h.dir, h.sock = ln, dir, sock
	go h.accept(ln)
	return sock, nil
}

func (h *Host) accept(ln net.Listener) {
	for {
		nc, err := ln.Accept()
		if err != nil {
			return
		}
		go h.serve(nc)
	}
}

func (h *Host) serve(nc net.Conn) {
	defer nc.Close()
	br := bufio.NewReader(nc)
	nc.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := br.ReadBytes('\n')
	nc.SetReadDeadline(time.Time{})
	if err != nil {
		return
	}
	var hello struct {
		Token string `json:"token"`
	}
	json.Unmarshal(line, &hello)
	h.mu.Lock()
	b := h.bindings[hello.Token]
	h.mu.Unlock()
	if b == nil || hello.Token == "" {
		h.log.Warn("toolhost: rejected bridge connection with an unknown token")
		return
	}
	sc := &serverConn{h: h, b: b}
	sc.rpc = jsonrpc.NewConn(br, nc, sc, jsonrpc.Options{
		OnError: func(err error) { h.log.Debug("toolhost", "err", err) },
	})
	b.mu.Lock()
	b.conns[sc.rpc] = nc
	b.mu.Unlock()
	sc.rpc.Run(b.ctx)
	b.mu.Lock()
	delete(b.conns, sc.rpc)
	b.mu.Unlock()
}

func (h *Host) notifyListChanged() {
	h.mu.Lock()
	var conns []*jsonrpc.Conn
	for _, b := range h.bindings {
		b.mu.Lock()
		for c := range b.conns {
			conns = append(conns, c)
		}
		b.mu.Unlock()
	}
	h.mu.Unlock()
	for _, c := range conns {
		_ = c.Notify(context.Background(), "notifications/tools/list_changed", nil)
	}
}

func (h *Host) toolsFor(names []string) []tools.Tool {
	if h.opts.Registry == nil {
		return nil
	}
	all := slices.Clone(h.opts.Registry.List())
	if names == nil {
		return all
	}
	return slices.DeleteFunc(all, func(t tools.Tool) bool { return !slices.Contains(names, t.Name) })
}

func newToken() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// serverConn is one runtime's MCP connection.
type serverConn struct {
	h   *Host
	b   *binding
	rpc *jsonrpc.Conn
}

type mcpTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Annotations map[string]any  `json:"annotations,omitempty"`
}

var permissionSchema = json.RawMessage(`{"type":"object","properties":{"tool_name":{"type":"string"},"input":{"type":"object"},"tool_use_id":{"type":"string"}},"required":["tool_name","input"]}`)

// Handle implements jsonrpc.Handler.
func (c *serverConn) Handle(ctx context.Context, req *jsonrpc.Request) (any, error) {
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		req.UnmarshalParams(&p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = "2025-06-18"
		}
		return map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": true}},
			"serverInfo":      map[string]any{"name": "harness-kit", "version": c.h.opts.Version},
			"instructions":    "Tools provided by the host application.",
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		list := []mcpTool{}
		for _, t := range c.h.toolsFor(c.b.Tools) {
			mt := mcpTool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema}
			if t.ReadOnly {
				mt.Annotations = map[string]any{"readOnlyHint": true}
			}
			list = append(list, mt)
		}
		if c.b.ApprovalPrompt {
			list = append(list, mcpTool{Name: PermissionTool, Description: "Answers the runtime's permission prompts on behalf of the host application.", InputSchema: permissionSchema})
		}
		return map[string]any{"tools": list}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := req.UnmarshalParams(&p); err != nil {
			return nil, err
		}
		if p.Name == PermissionTool && c.b.ApprovalPrompt {
			return c.permissionPrompt(ctx, p.Arguments), nil
		}
		return c.callTool(ctx, p.Name, p.Arguments), nil
	}
	if req.IsNotification() {
		return nil, nil
	}
	return nil, jsonrpc.NewError(jsonrpc.CodeMethodNotFound, "method not found: "+req.Method, nil)
}

func (c *serverConn) callTool(ctx context.Context, name string, args json.RawMessage) tools.Result {
	if c.h.opts.Registry == nil || !c.b.allows(name) {
		return tools.ErrorResult("unknown tool " + name)
	}
	t, ok := c.h.opts.Registry.Get(name)
	if !ok {
		return tools.ErrorResult("unknown tool " + name)
	}
	if t.Approval == mcp.ApprovalAsk {
		title := "Use " + name
		if t.Description != "" {
			title += ": " + t.Description
		}
		d, err := c.b.Approver(ctx, permission.Request{
			Kind: permission.KindTool, Tool: name, Title: title, Input: args,
			Outcomes: []permission.Outcome{permission.Allow, permission.Deny, permission.Abort},
		})
		switch {
		case err != nil:
			return tools.ErrorResult("not run: the approval request ended without a decision")
		case d.Outcome == permission.Abort:
			c.b.CancelTurn()
			return tools.ErrorResult("not run: the user stopped this turn")
		case !d.Outcome.Allowed():
			msg := "not run: the user denied this tool call"
			if d.Message != "" {
				msg += ": " + d.Message
			}
			return tools.ErrorResult(msg)
		}
		if len(d.UpdatedInput) > 0 {
			args = d.UpdatedInput
		}
	}
	res, err := c.h.opts.Registry.Call(ctx, tools.Call{SessionID: c.b.SessionID, TurnID: c.b.CurrentTurn(), Name: name, Arguments: args})
	if err != nil {
		return tools.ErrorResult(err.Error())
	}
	return res
}

// permissionPrompt implements Claude Code's --permission-prompt-tool
// contract (verified against Claude Code 2.1.316): arguments
// {tool_name, input, tool_use_id}; the result is text holding
// {"behavior":"allow","updatedInput":{...}} or
// {"behavior":"deny","message":"..."}.
func (c *serverConn) permissionPrompt(ctx context.Context, args json.RawMessage) tools.Result {
	var p struct {
		ToolName  string          `json:"tool_name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
	}
	json.Unmarshal(args, &p)
	if len(p.Input) == 0 {
		p.Input = json.RawMessage(`{}`)
	}
	req := classify(p.ToolName, p.Input)
	req.Outcomes = []permission.Outcome{permission.Allow, permission.Deny, permission.Abort}
	d, err := c.b.Approver(ctx, req)
	reply := func(v map[string]any) tools.Result {
		b, _ := json.Marshal(v)
		return tools.TextResult(string(b))
	}
	switch {
	case err != nil:
		return reply(map[string]any{"behavior": "deny", "message": "No decision was made (the request was cancelled)."})
	case d.Outcome == permission.Abort:
		c.b.CancelTurn()
		return reply(map[string]any{"behavior": "deny", "message": "The user stopped this turn."})
	case d.Outcome.Allowed():
		input := p.Input
		if len(d.UpdatedInput) > 0 {
			input = d.UpdatedInput
		}
		return reply(map[string]any{"behavior": "allow", "updatedInput": input})
	}
	msg := d.Message
	if msg == "" {
		msg = "The user denied this action."
	}
	return reply(map[string]any{"behavior": "deny", "message": msg})
}

// classify describes a Claude Code tool call as a permission request.
func classify(tool string, input json.RawMessage) permission.Request {
	var in map[string]any
	json.Unmarshal(input, &in)
	str := func(k string) string { s, _ := in[k].(string); return s }
	r := permission.Request{Kind: permission.KindTool, Tool: tool, Input: input, Title: "Use " + tool, Reason: str("description")}
	switch {
	case tool == "Bash" || tool == "PowerShell":
		r.Kind, r.Command = permission.KindCommand, str("command")
		r.Title = "Run `" + r.Command + "`"
	case tool == "Edit" || tool == "MultiEdit" || tool == "Write" || tool == "NotebookEdit":
		path := firstNonEmpty(str("file_path"), str("notebook_path"))
		r.Kind, r.Paths = permission.KindFileChange, []string{path}
		r.Title = tool + " " + path
	case tool == "WebFetch" || tool == "WebSearch":
		r.Kind = permission.KindNetwork
		r.Title = tool + " " + firstNonEmpty(str("url"), str("query"))
	case strings.HasPrefix(tool, "mcp__"):
		parts := strings.SplitN(strings.TrimPrefix(tool, "mcp__"), "__", 2)
		if len(parts) == 2 {
			r.Title = "Use " + parts[1] + " from MCP server " + parts[0]
		}
	}
	return r
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// ServeBridgeIfRequested turns this process into a bridge (relaying
// stdin/stdout to the Host's socket) and exits, if it was started as one.
// Call it first thing in main of any executable used as BridgeCommand.
func ServeBridgeIfRequested() {
	sock := os.Getenv(envSocket)
	if sock == "" {
		return
	}
	if err := RunBridge(sock, os.Getenv(envToken), os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "harness-kit mcp-bridge:", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// RunBridge connects to socket, presents token, and relays in/out until
// either side closes.
func RunBridge(socket, token string, in io.Reader, out io.Writer) error {
	nc, err := net.DialTimeout("unix", socket, 10*time.Second)
	if err != nil {
		return fmt.Errorf("connect to host: %w", err)
	}
	defer nc.Close()
	hello, _ := json.Marshal(map[string]string{"token": token})
	if _, err := nc.Write(append(hello, '\n')); err != nil {
		return err
	}
	go func() {
		io.Copy(nc, in)
		if uc, ok := nc.(*net.UnixConn); ok {
			uc.CloseWrite() // the runtime closed stdin: let the host finish
		}
	}()
	_, err = io.Copy(out, nc)
	return err
}
