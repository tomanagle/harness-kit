// Package client is a Go client for the Harness Kit sidecar protocol. Use
// it when a Go program (for example a Wails backend) talks to a separate
// harness-kit process. Go programs that can link Harness Kit directly
// should use the session package instead and skip the RPC hop.
package client

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

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/internal/jsonrpc"
	"github.com/tomanagle/harness-kit/core/permission"
	"github.com/tomanagle/harness-kit/core/process"
	"github.com/tomanagle/harness-kit/core/rpc/protocol"
	"github.com/tomanagle/harness-kit/core/session"
	"github.com/tomanagle/harness-kit/core/settings"
	"github.com/tomanagle/harness-kit/core/tools"
	"github.com/tomanagle/harness-kit/core/workspace"
)

// Options configure a Client.
type Options struct {
	// OnEvent receives every event notification, in order, on the
	// connection's read goroutine. It must not block for long; a slow
	// handler applies backpressure and the server will start dropping
	// events for this client (reported as stream.gap events).
	OnEvent func(event.Event)
	// OnSettings receives revisioned host/session settings notifications. It must not block.
	OnSettings func(settings.Snapshot)
	// Settings is the owning Spawn caller's boot document, including process wiring.
	// Connect rejects it because an existing host has already been constructed.
	Settings settings.Patch
	// InitialSettings applies live preferences before Connect returns. For process
	// wiring such as store or runtime paths, use Settings with Spawn instead.
	InitialSettings settings.Patch
	// ClientInfo is sent in initialize.
	ClientInfo protocol.ClientInfo
}

// Error is a protocol error returned by the server.
type Error struct {
	Code    int
	Message string
	Reason  string
}

func (e *Error) Error() string {
	return fmt.Sprintf("harness-kit: %s (code %d, %s)", e.Message, e.Code, e.Reason)
}

// ToolFunc runs a tool registered with RegisterTool. Errors become error
// results for the model.
type ToolFunc func(ctx context.Context, call protocol.ToolCallParams) (tools.Result, error)

// Client is a protocol client. Methods are safe for concurrent use.
type Client struct {
	rpc  *jsonrpc.Conn
	init protocol.InitializeResult

	toolMu sync.Mutex
	toolFn map[string]ToolFunc

	cmd     *exec.Cmd
	stdin   io.Closer
	runErr  chan error
	closeMu sync.Once
}

// Connect initializes a protocol session over r/w.
func Connect(ctx context.Context, r io.Reader, w io.Writer, opts Options) (*Client, error) {
	if opts.Settings != nil {
		return nil, errors.New("harness-kit: boot settings require Spawn; use InitialSettings for live settings on an existing host")
	}
	c := &Client{runErr: make(chan error, 1), toolFn: map[string]ToolFunc{}}
	c.rpc = jsonrpc.NewConn(r, w, jsonrpc.HandlerFunc(func(ctx context.Context, req *jsonrpc.Request) (any, error) {
		switch {
		case req.Method == protocol.NotificationEvent && opts.OnEvent != nil:
			var ev event.Event
			if err := json.Unmarshal(req.Params, &ev); err == nil {
				opts.OnEvent(ev)
			}
			return nil, nil
		case req.Method == protocol.NotificationSettingsUpdated && opts.OnSettings != nil:
			var snapshot settings.Snapshot
			if err := json.Unmarshal(req.Params, &snapshot); err == nil {
				opts.OnSettings(snapshot)
			}
			return nil, nil
		case req.Method == protocol.RequestToolCall:
			var p protocol.ToolCallParams
			if err := req.UnmarshalParams(&p); err != nil {
				return nil, err
			}
			c.toolMu.Lock()
			fn := c.toolFn[p.Name]
			c.toolMu.Unlock()
			if fn == nil {
				return tools.ErrorResult("tool " + p.Name + " is not registered by this client"), nil
			}
			res, err := fn(ctx, p)
			if err != nil {
				return tools.ErrorResult(err.Error()), nil
			}
			if res.Content == nil {
				res.Content = []tools.Content{}
			}
			return res, nil
		}
		return nil, jsonrpc.NewError(jsonrpc.CodeMethodNotFound, "client does not handle "+req.Method, nil)
	}), jsonrpc.Options{CancelMethod: protocol.NotificationCancelRequest})
	go func() { c.runErr <- c.rpc.Run(context.Background()) }()

	if opts.ClientInfo.Name == "" {
		opts.ClientInfo = protocol.ClientInfo{Name: "harness-kit-go-client"}
	}
	if err := c.call(ctx, protocol.MethodInitialize, protocol.InitializeParams{
		ProtocolVersion: protocol.Version, ClientInfo: opts.ClientInfo,
	}, &c.init); err != nil {
		return nil, err
	}
	if opts.InitialSettings != nil {
		if _, err := c.ConfigureSettings(ctx, settings.ConfigureParams{Patch: opts.InitialSettings}); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// Spawn starts a harness-kit sidecar binary and connects to it. The
// sidecar's stderr is forwarded to stderr (os.Stderr if nil).
func Spawn(ctx context.Context, path string, args []string, stderr io.Writer, opts Options) (*Client, error) {
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
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("harness-kit: start sidecar: %w", err)
	}
	c, err := Connect(ctx, stdout, stdin, opts)
	if err != nil {
		stdin.Close()
		_ = process.Kill(cmd)
		_ = cmd.Wait()
		return nil, err
	}
	c.cmd, c.stdin = cmd, stdin
	return c, nil
}

// Info returns the server's initialize result.
func (c *Client) Info() protocol.InitializeResult { return c.init }

func (c *Client) call(ctx context.Context, method string, params, result any) error {
	err := c.rpc.Call(ctx, method, params, result)
	var re *jsonrpc.Error
	if errors.As(err, &re) {
		var data protocol.ErrorData
		_ = json.Unmarshal(re.Data, &data)
		return &Error{Code: re.Code, Message: re.Message, Reason: data.Reason}
	}
	return err
}

// Settings returns effective settings, their revision and UI field descriptors.
func (c *Client) Settings(ctx context.Context, target settings.Target) (settings.Snapshot, error) {
	var res settings.Snapshot
	err := c.call(ctx, protocol.MethodSettingsGet, protocol.SettingsGetParams{Target: target}, &res)
	return res, err
}

// ConfigureSettings applies one atomic patch without restarting the sidecar.
func (c *Client) ConfigureSettings(ctx context.Context, p settings.ConfigureParams) (settings.Snapshot, error) {
	var res settings.Snapshot
	err := c.call(ctx, protocol.MethodSettingsConfigure, p, &res)
	return res, err
}

// MemoryStats reports cache accounting and deferred resource reclamation.
func (c *Client) MemoryStats(ctx context.Context) (session.ResourceStats, error) {
	var res session.ResourceStats
	err := c.call(ctx, protocol.MethodSettingsStats, nil, &res)
	return res, err
}

// EvictSession releases cached transcript data after checking persistence.
func (c *Client) EvictSession(ctx context.Context, id string) error {
	return c.call(ctx, protocol.MethodSessionEvict, protocol.SessionIDParams{SessionID: id}, nil)
}

// SuspendSession releases an idle runtime without closing its conversation.
func (c *Client) SuspendSession(ctx context.Context, id string) error {
	return c.call(ctx, protocol.MethodSessionSuspend, protocol.SessionIDParams{SessionID: id}, nil)
}

// PinSession protects a session from automatic eviction/suspension.
func (c *Client) PinSession(ctx context.Context, id string, pinned bool) (session.Info, error) {
	var res protocol.SessionResult
	err := c.call(ctx, protocol.MethodSessionPin, protocol.SessionPinParams{SessionID: id, Pinned: pinned}, &res)
	return res.Session, err
}

// EventsPage reads one page bounded by the host's history settings.
func (c *Client) EventsPage(ctx context.Context, p protocol.SessionEventsParams) (session.EventPage, error) {
	var res session.EventPage
	err := c.call(ctx, protocol.MethodSessionEventsPage, p, &res)
	return res, err
}

// Runtimes lists registered runtimes.
func (c *Client) Runtimes(ctx context.Context) (protocol.RuntimeListResult, error) {
	var res protocol.RuntimeListResult
	return res, c.call(ctx, protocol.MethodRuntimeList, nil, &res)
}

// Discover probes runtimes (all if names is empty).
func (c *Client) Discover(ctx context.Context, names ...string) (protocol.RuntimeDiscoverResult, error) {
	var res protocol.RuntimeDiscoverResult
	return res, c.call(ctx, protocol.MethodRuntimeDiscover, protocol.RuntimeDiscoverParams{Runtimes: names}, &res)
}

// RuntimeSessions lists a runtime's own stored sessions.
func (c *Client) RuntimeSessions(ctx context.Context, p protocol.RuntimeSessionsParams) ([]agent.RuntimeSession, error) {
	var res protocol.RuntimeSessionsResult
	err := c.call(ctx, protocol.MethodRuntimeSessions, p, &res)
	return res.Sessions, err
}

// RestoreSession restarts a stored session under the same ID.
func (c *Client) RestoreSession(ctx context.Context, id string) (session.Info, error) {
	var res protocol.SessionResult
	err := c.call(ctx, protocol.MethodSessionRestore, protocol.SessionIDParams{SessionID: id}, &res)
	return res.Session, err
}

// DeleteSession closes a session and removes its stored record.
func (c *Client) DeleteSession(ctx context.Context, id string) error {
	return c.call(ctx, protocol.MethodSessionDelete, protocol.SessionIDParams{SessionID: id}, nil)
}

// RenameSession sets a session's title.
func (c *Client) RenameSession(ctx context.Context, id, title string) (session.Info, error) {
	var res protocol.SessionResult
	err := c.call(ctx, protocol.MethodSessionRename, protocol.SessionRenameParams{SessionID: id, Title: title}, &res)
	return res.Session, err
}

// ConfigureSession updates defaults for future turns without changing an
// already-running turn. Unsupported changes fail rather than restarting it.
func (c *Client) ConfigureSession(ctx context.Context, id string, cfg agent.ConfigPatch) (session.Info, error) {
	var res protocol.SessionResult
	err := c.call(ctx, protocol.MethodSessionConfigure, protocol.SessionConfigureParams{SessionID: id, Config: &cfg}, &res)
	return res.Session, err
}

// CreateSession starts a session.
func (c *Client) CreateSession(ctx context.Context, p protocol.SessionCreateParams) (session.Info, error) {
	var res protocol.SessionResult
	err := c.call(ctx, protocol.MethodSessionCreate, p, &res)
	return res.Session, err
}

// GetSession returns a session's info.
func (c *Client) GetSession(ctx context.Context, id string) (session.Info, error) {
	var res protocol.SessionResult
	err := c.call(ctx, protocol.MethodSessionGet, protocol.SessionIDParams{SessionID: id}, &res)
	return res.Session, err
}

func (c *Client) TurnDiff(ctx context.Context, p protocol.SessionDiffParams) (workspace.Diff, error) {
	var result workspace.Diff
	err := c.call(ctx, protocol.MethodSessionDiff, p, &result)
	return result, err
}
func (c *Client) RemoveWorkspace(ctx context.Context, id string) error {
	return c.call(ctx, protocol.MethodWorkspaceRemove, protocol.SessionIDParams{SessionID: id}, nil)
}

// QuerySessions filters stored conversations without starting runtimes.
func (c *Client) QuerySessions(ctx context.Context, p session.ListOptions) ([]session.Info, error) {
	var res protocol.SessionListResult
	err := c.call(ctx, protocol.MethodSessionList, p, &res)
	return res.Sessions, err
}
func (c *Client) ForkSession(ctx context.Context, id string, p session.ForkParams) (session.Info, error) {
	var res protocol.SessionResult
	err := c.call(ctx, protocol.MethodSessionFork, protocol.SessionForkParams{SessionID: id, ForkParams: p}, &res)
	return res.Session, err
}
func (c *Client) ArchiveSession(ctx context.Context, id string, archived bool) (session.Info, error) {
	var res protocol.SessionResult
	err := c.call(ctx, protocol.MethodSessionArchive, protocol.SessionArchiveParams{SessionID: id, Archived: archived}, &res)
	return res.Session, err
}
func (c *Client) SetSessionTags(ctx context.Context, id string, tags []string) (session.Info, error) {
	var res protocol.SessionResult
	err := c.call(ctx, protocol.MethodSessionTags, protocol.SessionTagsParams{SessionID: id, Tags: tags}, &res)
	return res.Session, err
}

// ListSessions lists sessions.
func (c *Client) ListSessions(ctx context.Context) ([]session.Info, error) {
	var res protocol.SessionListResult
	err := c.call(ctx, protocol.MethodSessionList, nil, &res)
	return res.Sessions, err
}

// Prompt starts a turn and returns its ID.
func (c *Client) Prompt(ctx context.Context, p protocol.SessionPromptParams) (string, error) {
	var res protocol.SessionPromptResult
	err := c.call(ctx, protocol.MethodSessionPrompt, p, &res)
	return res.TurnID, err
}

// Cancel interrupts a session's active turn.
func (c *Client) Cancel(ctx context.Context, sessionID string) error {
	return c.call(ctx, protocol.MethodSessionCancel, protocol.SessionIDParams{SessionID: sessionID}, nil)
}

// CloseSession closes a session.
func (c *Client) CloseSession(ctx context.Context, sessionID string) error {
	return c.call(ctx, protocol.MethodSessionClose, protocol.SessionIDParams{SessionID: sessionID}, nil)
}

// Events fetches retained events after afterSeq.
func (c *Client) Events(ctx context.Context, p protocol.SessionEventsParams) (protocol.SessionEventsResult, error) {
	var result protocol.SessionEventsResult
	cursor := p.AfterSeq
	for {
		request := p
		request.AfterSeq = cursor
		if p.Limit > 0 {
			request.Limit = p.Limit - len(result.Events)
			if request.Limit <= 0 {
				return result, nil
			}
		}
		var page protocol.SessionEventsResult
		if err := c.call(ctx, protocol.MethodSessionEvents, request, &page); err != nil {
			return result, err
		}
		result.Events = append(result.Events, page.Events...)
		result.Truncated = result.Truncated || page.Truncated
		result.NextSeq, result.HasMore = page.NextSeq, page.HasMore
		if !page.HasMore {
			return result, nil
		}
		if page.NextSeq <= cursor {
			return result, errors.New("harness-kit: history page did not advance")
		}
		cursor = page.NextSeq
	}
}

// RespondApproval answers an approval request.
func (c *Client) RespondApproval(ctx context.Context, approvalID string, d permission.Decision) error {
	return c.call(ctx, protocol.MethodApprovalRespond, protocol.ApprovalRespondParams{
		ApprovalID: approvalID, Outcome: d.Outcome, Message: d.Message, UpdatedInput: d.UpdatedInput,
	}, nil)
}

// PendingApprovals lists unresolved approvals.
func (c *Client) PendingApprovals(ctx context.Context, sessionID string) ([]permission.Request, error) {
	var res protocol.ApprovalListResult
	err := c.call(ctx, protocol.MethodApprovalList, protocol.ApprovalListParams{SessionID: sessionID}, &res)
	return res.Approvals, err
}

// RegisterTool registers a tool whose handler runs in this process.
// Register tools before creating the sessions that should use them.
func (c *Client) RegisterTool(ctx context.Context, spec protocol.ToolSpec, fn ToolFunc) error {
	c.toolMu.Lock()
	if _, exists := c.toolFn[spec.Name]; exists {
		c.toolMu.Unlock()
		return &Error{Code: jsonrpc.CodeInvalidParams, Message: "tool " + spec.Name + " is already registered", Reason: protocol.ReasonDuplicateTool}
	}
	c.toolFn[spec.Name] = fn // before registering, so an immediate call finds it
	c.toolMu.Unlock()
	err := c.call(ctx, protocol.MethodToolsRegister, protocol.ToolsRegisterParams{Tools: []protocol.ToolSpec{spec}}, nil)
	if err != nil {
		c.toolMu.Lock()
		delete(c.toolFn, spec.Name)
		c.toolMu.Unlock()
	}
	return err
}

// UnregisterTools removes tools this client registered.
func (c *Client) UnregisterTools(ctx context.Context, names ...string) error {
	err := c.call(ctx, protocol.MethodToolsUnregister, protocol.ToolsUnregisterParams{Names: names}, nil)
	c.toolMu.Lock()
	for _, n := range names {
		delete(c.toolFn, n)
	}
	c.toolMu.Unlock()
	return err
}

// MCPList describes the server's kit MCP configuration and, for the given
// runtimes, their natively configured servers.
func (c *Client) MCPList(ctx context.Context, p protocol.MCPListParams) (protocol.MCPListResult, error) {
	var res protocol.MCPListResult
	return res, c.call(ctx, protocol.MethodMCPList, p, &res)
}

// Tools lists the tools registered on the server.
func (c *Client) Tools(ctx context.Context) ([]protocol.ToolSpec, error) {
	var res protocol.ToolsListResult
	err := c.call(ctx, protocol.MethodToolsList, nil, &res)
	return res.Tools, err
}

// Shutdown asks the server to close all sessions.
func (c *Client) Shutdown(ctx context.Context) error {
	return c.call(ctx, protocol.MethodShutdown, nil, nil)
}

// Close shuts the server down (if it was spawned), closes the connection
// and waits for the sidecar to exit, killing it after a grace period.
func (c *Client) Close() error {
	var err error
	c.closeMu.Do(func() {
		if c.cmd == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = c.Shutdown(ctx)
		c.stdin.Close()
		done := make(chan error, 1)
		go func() { done <- c.cmd.Wait() }()
		select {
		case err = <-done:
		case <-time.After(5 * time.Second):
			_ = process.Kill(c.cmd)
			err = <-done
		}
	})
	return err
}
