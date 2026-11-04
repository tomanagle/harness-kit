// Package client is a minimal MCP client used to check that an MCP server
// starts and to list its tools (`harness-kit mcp test`). It speaks the
// stdio transport (newline-delimited JSON-RPC) and streamable HTTP (JSON or
// server-sent-event responses). It is native-only: stdio servers are child
// processes.
package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tomanagle/harness-kit/core/internal/jsonrpc"
	"github.com/tomanagle/harness-kit/core/internal/process"
	"github.com/tomanagle/harness-kit/core/mcp"
)

// ProtocolVersion is the MCP protocol version this client requests.
const ProtocolVersion = "2025-06-18"

// Tool is a tool advertised by a server.
type Tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// Result describes a successfully probed server.
type Result struct {
	ServerName      string `json:"serverName"`
	ServerVersion   string `json:"serverVersion,omitempty"`
	ProtocolVersion string `json:"protocolVersion"`
	Tools           []Tool `json:"tools"`
}

// Options configure Probe.
type Options struct {
	// Lookup resolves ${VAR} references (default os.LookupEnv).
	Lookup func(string) (string, bool)
	// Dir is the working directory for stdio servers without a cwd.
	Dir string
	// HTTPClient is used for http servers (default: 20s timeout).
	HTTPClient *http.Client
}

type caller interface {
	call(ctx context.Context, method string, params, result any) error
	notify(ctx context.Context, method string, params any) error
	close()
}

// Session is a connection to one MCP server.
type Session struct {
	c    caller
	Info Result // server name, version and protocol; Tools is empty until ListTools
}

// Connect starts or contacts s and performs the initialize handshake.
func Connect(ctx context.Context, s mcp.Server, opts Options) (*Session, error) {
	if opts.Lookup == nil {
		opts.Lookup = os.LookupEnv
	}
	e, err := s.Expanded(opts.Lookup)
	if err != nil {
		return nil, err
	}
	var c caller
	if e.IsHTTP() {
		hc := opts.HTTPClient
		if hc == nil {
			hc = &http.Client{Timeout: 20 * time.Second}
		}
		c = &httpCaller{url: e.URL, headers: e.Headers, client: hc}
	} else {
		sc, err := startStdio(e, opts.Dir)
		if err != nil {
			return nil, err
		}
		c = sc
	}
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "harness-kit", "version": "0.1.0"},
	}, &init); err != nil {
		c.close()
		return nil, fmt.Errorf("initialize: %w", err)
	}
	if hc, ok := c.(*httpCaller); ok {
		hc.protocol = init.ProtocolVersion
	}
	if err := c.notify(ctx, "notifications/initialized", nil); err != nil {
		c.close()
		return nil, fmt.Errorf("initialized: %w", err)
	}
	return &Session{c: c, Info: Result{ServerName: init.ServerInfo.Name, ServerVersion: init.ServerInfo.Version, ProtocolVersion: init.ProtocolVersion, Tools: []Tool{}}}, nil
}

// ListTools returns all of the server's tools.
func (s *Session) ListTools(ctx context.Context) ([]Tool, error) {
	all := []Tool{}
	cursor := ""
	for page := 0; page < 100; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var list struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := s.c.call(ctx, "tools/list", params, &list); err != nil {
			return all, fmt.Errorf("tools/list: %w", err)
		}
		all = append(all, list.Tools...)
		if list.NextCursor == "" {
			break
		}
		cursor = list.NextCursor
	}
	return all, nil
}

// CallResult is the result of CallTool.
type CallResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text,omitempty"`
	} `json:"content"`
	Structured json.RawMessage `json:"structuredContent,omitempty"`
	IsError    bool            `json:"isError,omitempty"`
}

// Text joins the result's text content.
func (r CallResult) Text() string {
	var b strings.Builder
	for _, c := range r.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

// CallTool calls a tool. ctx bounds the call (approvals may make it slow).
func (s *Session) CallTool(ctx context.Context, name string, args json.RawMessage) (CallResult, error) {
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	var res CallResult
	err := s.c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args}, &res)
	return res, err
}

// Close disconnects (and stops a stdio server).
func (s *Session) Close() { s.c.close() }

// Probe connects to s, lists all tools and disconnects.
func Probe(ctx context.Context, s mcp.Server, opts Options) (Result, error) {
	sess, err := Connect(ctx, s, opts)
	if err != nil {
		return Result{}, err
	}
	defer sess.Close()
	res := sess.Info
	tools, err := sess.ListTools(ctx)
	res.Tools = tools
	return res, err
}

// ---- stdio ---------------------------------------------------------------

type stdioCaller struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	conn   *jsonrpc.Conn
	stderr *process.Tail
	exited chan struct{}
}

func startStdio(s mcp.Server, dir string) (*stdioCaller, error) {
	cmd := exec.Command(s.Command, s.Args...)
	process.Configure(cmd)
	cmd.Dir = dir
	if s.Cwd != "" {
		cmd.Dir = s.Cwd
	}
	cmd.Env = os.Environ()
	for k, v := range s.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	c := &stdioCaller{cmd: cmd, stdin: stdin, stderr: process.NewTail(8 << 10), exited: make(chan struct{})}
	cmd.Stderr = c.stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", s.Command, err)
	}
	go func() { cmd.Wait(); close(c.exited) }()
	// Servers may send requests (ping, roots/list); answer politely.
	c.conn = jsonrpc.NewConn(stdout, stdin, jsonrpc.HandlerFunc(func(_ context.Context, req *jsonrpc.Request) (any, error) {
		switch req.Method {
		case "ping":
			return map[string]any{}, nil
		case "roots/list":
			return map[string]any{"roots": []any{}}, nil
		}
		return nil, jsonrpc.NewError(jsonrpc.CodeMethodNotFound, "not supported", nil)
	}), jsonrpc.Options{})
	go c.conn.Run(context.Background())
	return c, nil
}

func (c *stdioCaller) call(ctx context.Context, method string, params, result any) error {
	errc := make(chan error, 1)
	go func() { errc <- c.conn.Call(ctx, method, params, result) }()
	select {
	case err := <-errc:
		if err != nil {
			return fmt.Errorf("%w%s", err, c.stderr.Suffix())
		}
		return nil
	case <-c.exited:
		return fmt.Errorf("server exited%s", c.stderr.Suffix())
	}
}

func (c *stdioCaller) notify(ctx context.Context, method string, params any) error {
	return c.conn.Notify(ctx, method, params)
}

func (c *stdioCaller) close() {
	c.stdin.Close()
	select {
	case <-c.exited:
	case <-time.After(2 * time.Second):
		_ = process.Kill(c.cmd)
		<-c.exited
	}
}

// ---- streamable HTTP -----------------------------------------------------

type httpCaller struct {
	url      string
	headers  map[string]string
	client   *http.Client
	session  string
	protocol string
	nextID   atomic.Int64
	mu       sync.Mutex
}

type rpcMessage struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *jsonrpc.Error  `json:"error"`
}

func (h *httpCaller) post(ctx context.Context, body any) (*http.Response, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	h.mu.Lock()
	if h.session != "" {
		req.Header.Set("Mcp-Session-Id", h.session)
	}
	if h.protocol != "" {
		req.Header.Set("MCP-Protocol-Version", h.protocol)
	}
	h.mu.Unlock()
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, err
	}
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		h.mu.Lock()
		h.session = sid
		h.mu.Unlock()
	}
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return resp, nil
}

func (h *httpCaller) call(ctx context.Context, method string, params, result any) error {
	id := h.nextID.Add(1)
	msg := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		msg["params"] = params
	}
	resp, err := h.post(ctx, msg)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	want := fmt.Sprint(id)
	var found *rpcMessage
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		found, err = readSSE(resp.Body, want)
	} else {
		var m rpcMessage
		err = json.NewDecoder(resp.Body).Decode(&m)
		found = &m
	}
	if err != nil {
		return err
	}
	if found.Error != nil {
		return found.Error
	}
	if result != nil {
		return json.Unmarshal(found.Result, result)
	}
	return nil
}

// readSSE reads server-sent events until the response with id arrives.
func readSSE(r io.Reader, id string) (*rpcMessage, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	var data strings.Builder
	flush := func() *rpcMessage {
		defer data.Reset()
		var m rpcMessage
		if data.Len() == 0 || json.Unmarshal([]byte(data.String()), &m) != nil || m.Method != "" {
			return nil
		}
		if strings.Trim(string(m.ID), `"`) == id {
			return &m
		}
		return nil
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if m := flush(); m != nil {
				return m, nil
			}
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if m := flush(); m != nil {
		return m, nil
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("event stream ended without a response")
}

func (h *httpCaller) notify(ctx context.Context, method string, params any) error {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	resp, err := h.post(ctx, msg)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

func (h *httpCaller) close() {
	h.mu.Lock()
	sid := h.session
	h.mu.Unlock()
	if sid == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, h.url, nil)
	if err != nil {
		return
	}
	req.Header.Set("Mcp-Session-Id", sid)
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	if resp, err := h.client.Do(req); err == nil {
		resp.Body.Close()
	}
}
