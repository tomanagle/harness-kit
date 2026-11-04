// Package harnesskit is the standalone Go client for Harness Kit's JSON-RPC
// protocol. Wire types and RPC methods are generated from the shared contract.
// Connect uses an application-supplied byte stream; Spawn starts a local sidecar.
package harnesskit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/tomanagle/harness-kit/packages/client-go/internal/jsonrpc"
	"github.com/tomanagle/harness-kit/packages/client-go/internal/process"
)

// ErrClosed identifies calls made after the connection has closed.
var ErrClosed = jsonrpc.ErrClosed

// Options configures the connection, callbacks and initial settings.
type Options struct {
	ClientInfo ClientInfo
	// OnEvent, OnSettings and OnLog run in wire order on the read goroutine. They must
	// return promptly. Queue work elsewhere before making blocking RPC calls.
	OnEvent    func(HarnessEvent)
	OnSettings func(SettingsSnapshot)
	// OnLog must be installed before LogsSubscribe; delivery can begin before
	// that method's response arrives.
	OnLog func(LogNotification)
	// OnError reports malformed notifications and other non-fatal wire errors.
	OnError func(error)
	// Settings is the startup document for Spawn. Connect rejects boot settings
	// because an attached host already exists; use InitialSettings there.
	Settings        SettingsPatch
	InitialSettings SettingsPatch
	// CloseTransport overrides transport cleanup. Otherwise Connect takes
	// ownership of readers/writers implementing io.Closer. For non-closable
	// streams, provide this function to interrupt blocked reads and writes.
	CloseTransport func() error
	// MaxMessageBytes bounds inbound JSON lines. Zero uses the 32 MiB default.
	MaxMessageBytes int
}

// Error is a server error. Data retains the complete JSON error data, including
// fields unknown to this client version. Reason is extracted for convenience.
type Error struct {
	Code    int
	Message string
	Reason  string
	Data    json.RawMessage
}

func (e *Error) Error() string {
	return fmt.Sprintf("harness-kit: %s (code %d, %s)", e.Message, e.Code, e.Reason)
}

// ToolFunc handles a reverse tool.call request. Its context is cancelled when
// the host cancels the request or the connection closes. Errors become tool
// error results, so the agent can observe them.
type ToolFunc func(context.Context, ToolCallParams) (ToolResult, error)

// Client supports concurrent RPC calls. Close releases its transport and, only
// for Spawn, shuts down the owned sidecar. Attached hosts are never shut down
// implicitly.
type Client struct {
	rpc         *jsonrpc.Conn
	info        InitializeResult
	transport   func() error
	toolMu      sync.Mutex
	toolOps     sync.Mutex
	tools       map[string]ToolFunc
	cmd         *exec.Cmd
	processDone chan error
	closeOnce   sync.Once
	closeErr    error
}

// Connect initializes the protocol over newline-delimited JSON streams. ctx
// bounds initialization, not the connection lifetime; call Close when finished.
// On failure, owned streams are closed too. A reader that cannot be closed may
// remain blocked until its owner supplies EOF, even after Close fails all calls.
func Connect(ctx context.Context, r io.Reader, w io.Writer, opts Options) (*Client, error) {
	if opts.Settings != nil {
		return nil, errors.New("harness-kit: startup settings require Spawn; use InitialSettings when attaching")
	}
	if r == nil || w == nil {
		return nil, errors.New("harness-kit: reader and writer are required")
	}
	c := &Client{tools: make(map[string]ToolFunc), transport: opts.CloseTransport}
	if c.transport == nil {
		c.transport = func() error {
			// Both may be the same connection. Repeated Close is harmless for the
			// supported pipe/network streams; cleanup errors are not protocol errors.
			if closer, ok := r.(io.Closer); ok {
				_ = closer.Close()
			}
			if closer, ok := w.(io.Closer); ok {
				_ = closer.Close()
			}
			return nil
		}
	}
	report := func(err error) {
		if opts.OnError != nil {
			opts.OnError(err)
		}
	}
	c.rpc = jsonrpc.NewConn(r, w, jsonrpc.HandlerFunc(func(ctx context.Context, req *jsonrpc.Request) (any, error) {
		switch req.Method {
		case NotificationLog:
			var notification LogNotification
			if err := json.Unmarshal(req.Params, &notification); err != nil {
				report(fmt.Errorf("harness-kit: decode log notification: %w", err))
			} else if opts.OnLog != nil {
				opts.OnLog(notification)
			}
			return nil, nil
		case NotificationEvent:
			var ev HarnessEvent
			if err := json.Unmarshal(req.Params, &ev); err != nil {
				report(fmt.Errorf("harness-kit: decode event: %w", err))
			} else if opts.OnEvent != nil {
				opts.OnEvent(ev)
			}
			return nil, nil
		case NotificationSettingsUpdated:
			var snapshot SettingsSnapshot
			if err := json.Unmarshal(req.Params, &snapshot); err != nil {
				report(fmt.Errorf("harness-kit: decode settings notification: %w", err))
			} else if opts.OnSettings != nil {
				opts.OnSettings(snapshot)
			}
			return nil, nil
		case RequestToolCall:
			var p ToolCallParams
			if err := req.UnmarshalParams(&p); err != nil {
				return nil, err
			}
			c.toolMu.Lock()
			fn := c.tools[p.Name]
			c.toolMu.Unlock()
			if fn == nil {
				return ErrorResult("tool " + p.Name + " is not registered by this client"), nil
			}
			res, err := fn(ctx, p)
			if err != nil {
				return ErrorResult(err.Error()), nil
			}
			if res.Content == nil {
				res.Content = []ToolContent{}
			}
			return res, nil
		default:
			return nil, jsonrpc.NewError(jsonrpc.CodeMethodNotFound, "client does not handle "+req.Method, nil)
		}
	}), jsonrpc.Options{CancelMethod: NotificationCancelRequest, MaxMessageSize: opts.MaxMessageBytes, OnError: opts.OnError})
	go c.rpc.Run(context.Background())
	if opts.ClientInfo.Name == "" {
		opts.ClientInfo.Name = "harness-kit-go-client"
	}
	if err := c.Call(ctx, MethodInitialize, InitializeParams{ProtocolVersion: Version, ClientInfo: opts.ClientInfo}, &c.info); err != nil {
		_ = c.Close()
		return nil, err
	}
	if c.info.ProtocolVersion != Version {
		_ = c.Close()
		return nil, &Error{Code: CodeUnsupportedVersion, Reason: ReasonUnsupportedVersion, Message: fmt.Sprintf("server selected protocol version %d; client supports %d", c.info.ProtocolVersion, Version)}
	}
	if opts.InitialSettings != nil {
		var snapshot SettingsSnapshot
		if err := c.Call(ctx, MethodSettingsConfigure, ConfigureSettingsParams{Patch: opts.InitialSettings}, &snapshot); err != nil {
			_ = c.Close()
			return nil, err
		}
	}
	return c, nil
}

// Spawn starts a sidecar and initializes it. args are explicit (normally
// []string{"serve"}); the caller controls executable selection and environment.
// Startup Settings are added as --settings-json. stderr defaults to os.Stderr.
// The owned process tree is terminated if initialization fails or Close's grace
// period expires. ctx only bounds startup; cancel calls with their own contexts.
func Spawn(ctx context.Context, path string, args []string, stderr io.Writer, opts Options) (*Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.CloseTransport != nil {
		return nil, errors.New("harness-kit: CloseTransport is only supported by Connect")
	}
	if opts.Settings != nil {
		raw, err := json.Marshal(opts.Settings)
		if err != nil {
			return nil, fmt.Errorf("harness-kit: encode startup settings: %w", err)
		}
		args = append(append([]string(nil), args...), "--settings-json", string(raw))
		opts.Settings = nil
	}
	cmd := exec.Command(path, args...)
	process.Configure(cmd)
	if stderr == nil {
		stderr = os.Stderr
	}
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("harness-kit: start sidecar: %w", err)
	}
	c, err := Connect(ctx, stdout, stdin, opts)
	if err != nil {
		_ = process.Kill(cmd)
		_ = cmd.Wait()
		return nil, err
	}
	c.cmd = cmd
	c.processDone = make(chan error, 1)
	go func() {
		// StdoutPipe must be drained before Wait closes it.
		<-c.rpc.Done()
		c.processDone <- cmd.Wait()
	}()
	return c, nil
}

// Info is the negotiated server information.
func (c *Client) Info() InitializeResult { return c.info }

// Done closes when the transport ends or Close is called.
func (c *Client) Done() <-chan struct{} { return c.rpc.Done() }

// Err waits for Done and returns the transport error (nil for clean EOF).
func (c *Client) Err() error { return c.rpc.Err() }

// Call sends an RPC request. Prefer the generated typed methods; this escape
// hatch also supports additive methods introduced by newer hosts. A cancelled
// context sends $/cancelRequest. That cancels this RPC, not an already accepted
// session turn; use SessionCancel to stop a turn.
func (c *Client) Call(ctx context.Context, method string, params, result any) error {
	err := c.rpc.Call(ctx, method, params, result)
	var re *jsonrpc.Error
	if errors.As(err, &re) {
		var data struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(re.Data, &data)
		return &Error{Code: re.Code, Message: re.Message, Reason: data.Reason, Data: append(json.RawMessage(nil), re.Data...)}
	}
	return err
}

// RegisterTool makes a local callback available through tools.register. Install
// tools before creating the sessions that should use them.
func (c *Client) RegisterTool(ctx context.Context, spec ToolSpec, fn ToolFunc) error {
	if fn == nil {
		return errors.New("harness-kit: tool handler is required")
	}
	c.toolOps.Lock()
	defer c.toolOps.Unlock()
	c.toolMu.Lock()
	if _, exists := c.tools[spec.Name]; exists {
		c.toolMu.Unlock()
		return &Error{Code: jsonrpc.CodeInvalidParams, Reason: ReasonDuplicateTool, Message: "tool " + spec.Name + " is already registered"}
	}
	c.tools[spec.Name] = fn
	c.toolMu.Unlock()
	if err := c.Call(ctx, MethodToolsRegister, ToolsRegisterParams{Tools: []ToolSpec{spec}}, nil); err != nil {
		c.toolMu.Lock()
		delete(c.tools, spec.Name)
		c.toolMu.Unlock()
		return err
	}
	return nil
}

// UnregisterTools removes registered host tools and their callbacks. A failed
// request preserves callbacks because the server may still own registrations.
func (c *Client) UnregisterTools(ctx context.Context, names ...string) error {
	c.toolOps.Lock()
	defer c.toolOps.Unlock()
	if err := c.Call(ctx, MethodToolsUnregister, ToolsUnregisterParams{Names: names}, nil); err != nil {
		return err
	}
	c.toolMu.Lock()
	defer c.toolMu.Unlock()
	for _, name := range names {
		delete(c.tools, name)
	}
	return nil
}

// TextResult constructs a successful text tool result.
func TextResult(text string) ToolResult {
	return ToolResult{Content: []ToolContent{{Type: "text", Text: text}}}
}

// ErrorResult constructs an error visible to the agent.
func ErrorResult(message string) ToolResult {
	result := TextResult(message)
	result.IsError = true
	return result
}

// Close releases the connection, giving a spawned sidecar five seconds to shut
// down before terminating its process tree. It is safe to call more than once.
func (c *Client) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.CloseContext(ctx)
}

// CloseContext lets the caller choose the shutdown grace period. Attached
// transports are closed immediately, without shutting down their host.
func (c *Client) CloseContext(ctx context.Context) error {
	c.closeOnce.Do(func() {
		if c.cmd != nil {
			shutdown := make(chan error, 1)
			go func() { shutdown <- c.Call(ctx, MethodShutdown, nil, nil) }()
			select {
			case <-shutdown:
			case <-ctx.Done():
			}
		}
		c.rpc.Close()
		c.closeErr = c.transport()
		if c.cmd == nil {
			return
		}
		select {
		case err := <-c.processDone:
			c.closeErr = errors.Join(c.closeErr, err)
		case <-ctx.Done():
			_ = process.Kill(c.cmd)
			c.closeErr = errors.Join(c.closeErr, ctx.Err(), <-c.processDone)
		}
	})
	return c.closeErr
}
