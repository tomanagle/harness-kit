// Package server exposes a session.Manager over the Harness Kit JSON-RPC
// protocol. It is transport-agnostic: Serve takes any reader/writer pair,
// typically the process's stdin and stdout.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/internal/jsonrpc"
	"github.com/tomanagle/harness-kit/core/mcp"
	"github.com/tomanagle/harness-kit/core/permission"
	"github.com/tomanagle/harness-kit/core/rpc/protocol"
	"github.com/tomanagle/harness-kit/core/session"
	"github.com/tomanagle/harness-kit/core/settings"
	"github.com/tomanagle/harness-kit/core/tools"
	"github.com/tomanagle/harness-kit/core/workspace"
)

// Options configure a Server.
type Options struct {
	// Name and Version are reported in initialize. Defaults: "harness-kit", "dev".
	Name    string
	Version string
	// EventBuffer is the per-connection event queue length. When a client
	// reads too slowly, events are dropped for it and a stream.gap event
	// tells it which to fetch with session.events. Defaults to 4096.
	EventBuffer int
	// AllowUnsafe is reported to clients; enforcement is the Manager's
	// (session.Options.AllowUnsafe).
	AllowUnsafe bool
	// EventLogSize is reported to clients (session.Options.EventLogSize).
	EventLogSize int
	// ShutdownTimeout bounds the shutdown method. Defaults to 10s.
	ShutdownTimeout time.Duration
	// MCP, if set, is the resolver for kit MCP config files, reported by
	// mcp.list (use the same one as the session Prepare hook).
	MCP *mcp.Resolver
	// Tools, if set, lets clients register host tools (tools.register).
	// It should be the registry served to sessions (toolhost.Options).
	Tools  *tools.Registry
	Logger *slog.Logger
}

// Server serves the protocol for one Manager.
type Server struct {
	mgr  *session.Manager
	opts Options
	log  *slog.Logger
}

// New returns a Server.
func New(mgr *session.Manager, opts Options) *Server {
	// Legacy constructor aliases feed the same live settings controller.
	// A connection subscribes using the current policy, so later updates can
	// both lower and raise its queue limit.
	legacy := settings.Patch{}
	if opts.EventBuffer > 0 {
		legacy["streams"], _ = json.Marshal(map[string]int{"maxQueueEvents": opts.EventBuffer})
	}
	if opts.ShutdownTimeout > 0 {
		legacy["process"], _ = json.Marshal(map[string]int64{"shutdownTimeoutMs": max(1, opts.ShutdownTimeout.Milliseconds())})
	}
	if len(legacy) > 0 {
		_, _ = mgr.ConfigureSettings(context.Background(), settings.ConfigureParams{Patch: legacy})
	}
	if opts.Name == "" {
		opts.Name = "harness-kit"
	}
	if opts.Version == "" {
		opts.Version = "dev"
	}
	if opts.EventBuffer <= 0 {
		opts.EventBuffer = event.DefaultBuffer
	}
	if opts.ShutdownTimeout <= 0 {
		opts.ShutdownTimeout = 10 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Server{mgr: mgr, opts: opts, log: opts.Logger}
}

// Serve runs one client connection until r reaches EOF. It does not shut
// the Manager down; the caller decides whether sessions outlive the
// connection.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	c := &conn{srv: s}
	c.rpc = jsonrpc.NewConn(r, w, c, jsonrpc.Options{
		CancelMethod: protocol.NotificationCancelRequest,
		OrderKey:     orderKey,
		// initialize completes before any later request is read, so clients
		// may pipeline requests without awaiting its response.
		Inline:  func(r *jsonrpc.Request) bool { return r.Method == protocol.MethodInitialize },
		OnError: func(err error) { s.log.Warn("rpc", "err", err) },
	})
	err := c.rpc.Run(ctx)
	c.stopEvents()
	c.dropTools()
	return err
}

type conn struct {
	srv         *Server
	rpc         *jsonrpc.Conn
	initialized atomic.Bool

	mu           sync.Mutex
	sub          *event.Subscription
	stopSettings func()
	wg           sync.WaitGroup
	tools        []string // names this client registered
}

// dropTools removes the client's tools when it disconnects.
func (c *conn) dropTools() {
	c.mu.Lock()
	names := c.tools
	c.tools = nil
	c.mu.Unlock()
	if reg := c.srv.opts.Tools; reg != nil && len(names) > 0 {
		reg.Unregister(names...)
	}
}

// forward returns a handler that runs the tool in the client.
func (c *conn) forward(name string) tools.Handler {
	return func(ctx context.Context, call tools.Call) (tools.Result, error) {
		var res tools.Result
		err := c.rpc.Call(ctx, protocol.RequestToolCall, protocol.ToolCallParams{
			SessionID: call.SessionID, TurnID: call.TurnID, Name: name, Arguments: call.Arguments,
		}, &res)
		if err != nil {
			return tools.Result{}, fmt.Errorf("the host application could not run %s: %v", name, err)
		}
		return res, nil
	}
}

func (c *conn) startEvents() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sub != nil {
		return
	}
	c.sub = c.srv.mgr.Subscribe(nil, 0)
	sub := c.sub
	changes, stop := c.srv.mgr.SubscribeSettings()
	c.stopSettings = stop
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		for snapshot := range changes {
			if err := c.rpc.Notify(context.Background(), protocol.NotificationSettingsUpdated, snapshot); err != nil {
				if errors.Is(err, jsonrpc.ErrClosed) {
					return
				}
				c.srv.log.Warn("send settings", "err", err)
			}
		}
	}()
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		for ev := range sub.C {
			if err := c.rpc.Notify(context.Background(), protocol.NotificationEvent, ev); err != nil {
				if errors.Is(err, jsonrpc.ErrClosed) {
					return
				}
				c.srv.log.Warn("send event", "err", err)
			}
		}
	}()
}

func (c *conn) stopEvents() {
	c.mu.Lock()
	sub := c.sub
	stop := c.stopSettings
	c.mu.Unlock()
	if stop != nil {
		stop()
	}
	if sub != nil {
		sub.Close()
	}
	c.wg.Wait()
}

func rpcErr(code int, reason, msg string) *jsonrpc.Error {
	return jsonrpc.NewError(code, msg, protocol.ErrorData{Reason: reason})
}

func invalidParams(msg string) *jsonrpc.Error {
	return rpcErr(jsonrpc.CodeInvalidParams, protocol.ReasonInvalidParams, msg)
}

// toRPCError maps library errors to protocol errors.
func toRPCError(err error) error {
	var re *jsonrpc.Error
	switch {
	case errors.Is(err, workspace.ErrNotFound):
		return rpcErr(protocol.CodeSessionNotFound, "turn_diff_not_found", err.Error())
	case errors.Is(err, workspace.ErrUnsupported):
		return rpcErr(protocol.CodeUnsupported, protocol.ReasonUnsupported, err.Error())
	case errors.Is(err, workspace.ErrInvalid):
		return invalidParams(err.Error())
	case errors.Is(err, workspace.ErrConflict):
		return rpcErr(protocol.CodeGitConflict, protocol.ReasonGitConflict, err.Error())
	case errors.Is(err, workspace.ErrDirty) || errors.Is(err, workspace.ErrNotOwned) || errors.Is(err, workspace.ErrReadOnly) || errors.Is(err, workspace.ErrRemoteDisabled):
		return rpcErr(protocol.CodePolicyViolation, protocol.ReasonPolicyViolation, err.Error())
	case err == nil:
		return nil
	case errors.As(err, &re):
		return re
	case errors.Is(err, session.ErrNotFound):
		return rpcErr(protocol.CodeSessionNotFound, protocol.ReasonSessionNotFound, err.Error())
	case errors.Is(err, session.ErrRuntimeNotFound):
		return rpcErr(protocol.CodeRuntimeNotFound, protocol.ReasonRuntimeNotFound, err.Error())
	case errors.Is(err, session.ErrBusy):
		return rpcErr(protocol.CodeSessionBusy, protocol.ReasonSessionBusy, err.Error())
	case errors.Is(err, session.ErrClosed):
		return rpcErr(protocol.CodeSessionClosed, protocol.ReasonSessionClosed, err.Error())
	case errors.Is(err, session.ErrDuplicateTurn):
		return rpcErr(jsonrpc.CodeInvalidParams, protocol.ReasonDuplicateTurn, err.Error())
	case errors.Is(err, session.ErrUnsafe):
		return rpcErr(protocol.CodePolicyViolation, protocol.ReasonPolicyViolation, err.Error())
	case errors.Is(err, permission.ErrNotFound):
		return rpcErr(protocol.CodeApprovalNotFound, protocol.ReasonApprovalNotFound, err.Error())
	case errors.Is(err, permission.ErrUnsupportedOutcome):
		return rpcErr(protocol.CodeInvalidDecision, protocol.ReasonInvalidDecision, err.Error())
	case errors.Is(err, agent.ErrNotInstalled):
		return rpcErr(protocol.CodeRuntimeUnavailable, protocol.ReasonRuntimeUnavailable, err.Error())
	case errors.Is(err, agent.ErrUnsupported):
		return rpcErr(protocol.CodeUnsupported, protocol.ReasonUnsupported, err.Error())
	case errors.Is(err, session.ErrInvalidTitle), errors.Is(err, agent.ErrInvalidConfig):
		return rpcErr(jsonrpc.CodeInvalidParams, protocol.ReasonInvalidParams, err.Error())
	case errors.Is(err, settings.ErrConflict):
		return rpcErr(protocol.CodeSettingsConflict, protocol.ReasonSettingsConflict, err.Error())
	case errors.Is(err, settings.ErrRestartRequired):
		return rpcErr(protocol.CodeRestartRequired, protocol.ReasonRestartRequired, err.Error())
	case errors.Is(err, settings.ErrNewSessionRequired):
		return rpcErr(protocol.CodeNewSessionRequired, protocol.ReasonNewSessionRequired, err.Error())
	case errors.Is(err, settings.ErrScope):
		return rpcErr(protocol.CodeUnsupported, protocol.ReasonUnsupported, err.Error())
	case errors.Is(err, session.ErrEventTooLarge):
		return rpcErr(protocol.CodeEventTooLarge, protocol.ReasonEventTooLarge, err.Error())
	case errors.Is(err, session.ErrBudgetExceeded):
		return rpcErr(protocol.CodeBudgetExceeded, protocol.ReasonBudgetExceeded, err.Error())
	case errors.Is(err, session.ErrForkSourceChanged):
		return rpcErr(protocol.CodeForkSourceChanged, protocol.ReasonForkSourceChanged, err.Error())
	case errors.Is(err, session.ErrNotResumable):
		return rpcErr(protocol.CodeNotResumable, protocol.ReasonNotResumable, err.Error())
	case errors.Is(err, context.Canceled):
		return jsonrpc.NewError(protocol.CodeRequestCancelled, "request cancelled", nil)
	}
	return rpcErr(protocol.CodeRuntimeError, protocol.ReasonRuntimeError, err.Error())
}

// Handle implements jsonrpc.Handler.
func (c *conn) Handle(ctx context.Context, req *jsonrpc.Request) (any, error) {
	if req.IsNotification() {
		// No client notifications other than $/cancelRequest (handled by
		// jsonrpc) are defined; ignore unknown ones per JSON-RPC.
		return nil, nil
	}
	if req.Method == protocol.MethodInitialize {
		return c.initialize(req)
	}
	if !c.initialized.Load() {
		return nil, rpcErr(protocol.CodeServerNotInitialized, protocol.ReasonNotInitialized, "initialize must be called first")
	}
	res, err := c.dispatch(ctx, req)
	return res, toRPCError(err)
}

func (c *conn) initialize(req *jsonrpc.Request) (any, error) {
	var p protocol.InitializeParams
	if err := req.UnmarshalParams(&p); err != nil {
		return nil, err
	}
	if p.ProtocolVersion == 0 {
		return nil, invalidParams("protocolVersion is required")
	}
	if p.ProtocolVersion != protocol.Version {
		return nil, jsonrpc.NewError(protocol.CodeUnsupportedVersion, "unsupported protocol version",
			protocol.ErrorData{Reason: protocol.ReasonUnsupportedVersion, SupportedVersions: []int{protocol.Version}})
	}
	if !c.initialized.CompareAndSwap(false, true) {
		return nil, jsonrpc.NewError(jsonrpc.CodeInvalidRequest, "already initialized", nil)
	}
	c.srv.log.Info("client initialized", "client", p.ClientInfo.Name, "version", p.ClientInfo.Version)
	c.startEvents()
	snapshot, err := c.srv.mgr.Settings(context.Background(), settings.Target{})
	if err != nil {
		return nil, err
	}
	return protocol.InitializeResult{
		ProtocolVersion: protocol.Version,
		ServerInfo:      protocol.ServerInfo{Name: c.srv.opts.Name, Version: c.srv.opts.Version},
		Capabilities: protocol.ServerCapabilities{
			Git:          c.srv.mgr.HasSourceControl(),
			Runtimes:     c.srv.mgr.Registry().List(),
			AllowUnsafe:  snapshot.Values.Process.AllowUnsafe,
			EventLogSize: snapshot.Values.Memory.EventLogSize,
			Tools:        c.srv.opts.Tools != nil,
			Settings:     true,
			Workspace:    c.srv.mgr.WorkspaceSupported(),
		},
	}, nil
}

func (c *conn) dispatch(ctx context.Context, req *jsonrpc.Request) (any, error) {
	if strings.HasPrefix(req.Method, "git.") {
		return c.dispatchGit(ctx, req)
	}
	mgr := c.srv.mgr
	switch req.Method {
	case protocol.MethodSettingsGet:
		var p protocol.SettingsGetParams
		if err := strictParams(req, &p); err != nil {
			return nil, err
		}
		return mgr.Settings(ctx, p.Target)
	case protocol.MethodSettingsConfigure:
		var p settings.ConfigureParams
		if err := strictParams(req, &p); err != nil {
			return nil, err
		}
		if p.Patch == nil {
			return nil, invalidParams("patch is required and must be an object")
		}
		return mgr.ConfigureSettings(ctx, p)
	case protocol.MethodSettingsStats:
		return mgr.MemoryStats(), nil
	case protocol.MethodSessionEvict, protocol.MethodSessionSuspend:
		p, err := sessionID(req)
		if err != nil {
			return nil, err
		}
		if req.Method == protocol.MethodSessionEvict {
			return protocol.Empty{}, mgr.Evict(ctx, p.SessionID)
		}
		return protocol.Empty{}, mgr.Suspend(ctx, p.SessionID)
	case protocol.MethodSessionPin:
		var p protocol.SessionPinParams
		if err := strictParams(req, &p); err != nil {
			return nil, err
		}
		if p.SessionID == "" {
			return nil, invalidParams("sessionId is required")
		}
		info, err := mgr.Pin(ctx, p.SessionID, p.Pinned)
		if err != nil {
			return nil, err
		}
		return protocol.SessionResult{Session: info}, nil
	case protocol.MethodShutdown:
		snapshot, err := mgr.Settings(context.Background(), settings.Target{})
		if err != nil {
			return nil, err
		}
		sctx, cancel := context.WithTimeout(context.Background(), time.Duration(snapshot.Values.Process.ShutdownTimeoutMs)*time.Millisecond)
		defer cancel()
		if err := mgr.Shutdown(sctx); err != nil {
			c.srv.log.Warn("shutdown", "err", err)
		}
		return protocol.Empty{}, nil

	case protocol.MethodRuntimeList:
		return protocol.RuntimeListResult{Runtimes: mgr.Registry().List()}, nil

	case protocol.MethodRuntimeDiscover:
		var p protocol.RuntimeDiscoverParams
		if err := req.UnmarshalParams(&p); err != nil {
			return nil, err
		}
		for _, name := range p.Runtimes {
			if _, ok := mgr.Registry().Get(name); !ok {
				return nil, rpcErr(protocol.CodeRuntimeNotFound, protocol.ReasonRuntimeNotFound, "unknown runtime "+name)
			}
		}
		ds, err := mgr.Registry().Discover(ctx, p.Runtimes...)
		if err != nil {
			return nil, err
		}
		return protocol.RuntimeDiscoverResult{Runtimes: ds}, nil

	case protocol.MethodRuntimeSessions:
		var p protocol.RuntimeSessionsParams
		if err := req.UnmarshalParams(&p); err != nil {
			return nil, err
		}
		if _, ok := mgr.Registry().Get(p.Runtime); !ok {
			return nil, rpcErr(protocol.CodeRuntimeNotFound, protocol.ReasonRuntimeNotFound, "unknown runtime "+p.Runtime)
		}
		list, err := mgr.Registry().ListSessions(ctx, p.Runtime, agent.ListSessionsOptions{Cwd: p.Cwd, Limit: p.Limit})
		if err != nil {
			return nil, err
		}
		if list == nil {
			list = []agent.RuntimeSession{}
		}
		return protocol.RuntimeSessionsResult{Sessions: list}, nil

	case protocol.MethodSessionRestore:
		p, err := sessionID(req)
		if err != nil {
			return nil, err
		}
		info, err := mgr.Restore(ctx, p.SessionID)
		if err != nil {
			return nil, err
		}
		return protocol.SessionResult{Session: info}, nil

	case protocol.MethodSessionDelete:
		p, err := sessionID(req)
		if err != nil {
			return nil, err
		}
		return protocol.Empty{}, mgr.Delete(ctx, p.SessionID)

	case protocol.MethodSessionDiff:
		var p protocol.SessionDiffParams
		if err := strictParams(req, &p); err != nil {
			return nil, err
		}
		if p.SessionID == "" || p.TurnID == "" {
			return nil, invalidParams("sessionId and turnId are required")
		}
		return mgr.TurnDiff(ctx, p.SessionID, p.TurnID, p.DiffOptions)
	case protocol.MethodWorkspaceRemove:
		p, err := sessionID(req)
		if err != nil {
			return nil, err
		}
		return protocol.Empty{}, mgr.RemoveWorkspace(ctx, p.SessionID)

	case protocol.MethodSessionRename:
		var p protocol.SessionRenameParams
		if err := req.UnmarshalParams(&p); err != nil {
			return nil, err
		}
		if p.SessionID == "" {
			return nil, invalidParams("sessionId is required")
		}
		info, err := mgr.Rename(ctx, p.SessionID, p.Title)
		if err != nil {
			return nil, err
		}
		return protocol.SessionResult{Session: info}, nil

	case protocol.MethodSessionFork:
		var p protocol.SessionForkParams
		if err := strictParams(req, &p); err != nil {
			return nil, err
		}
		if p.SessionID == "" {
			return nil, invalidParams("sessionId is required")
		}
		info, err := mgr.Fork(ctx, p.SessionID, p.ForkParams)
		if err != nil {
			return nil, err
		}
		return protocol.SessionResult{Session: info}, nil
	case protocol.MethodSessionArchive:
		var p protocol.SessionArchiveParams
		if err := strictParams(req, &p); err != nil {
			return nil, err
		}
		if p.SessionID == "" {
			return nil, invalidParams("sessionId is required")
		}
		info, err := mgr.Archive(ctx, p.SessionID, p.Archived)
		if err != nil {
			return nil, err
		}
		return protocol.SessionResult{Session: info}, nil
	case protocol.MethodSessionTags:
		var p protocol.SessionTagsParams
		if err := strictParams(req, &p); err != nil {
			return nil, err
		}
		if p.SessionID == "" {
			return nil, invalidParams("sessionId is required")
		}
		info, err := mgr.SetTags(ctx, p.SessionID, p.Tags)
		if err != nil {
			return nil, err
		}
		return protocol.SessionResult{Session: info}, nil

	case protocol.MethodSessionConfigure:
		var p protocol.SessionConfigureParams
		if err := req.UnmarshalParams(&p); err != nil {
			return nil, invalidParams(err.Error())
		}
		if p.SessionID == "" || p.Config == nil {
			return nil, invalidParams("sessionId and config are required")
		}
		info, err := mgr.Configure(ctx, p.SessionID, *p.Config)
		if err != nil {
			return nil, err
		}
		return protocol.SessionResult{Session: info}, nil

	case protocol.MethodSessionCreate:
		var p protocol.SessionCreateParams
		if err := req.UnmarshalParams(&p); err != nil {
			return nil, invalidParams(err.Error())
		}

		if p.Cwd != "" && !filepath.IsAbs(p.Cwd) {
			return nil, invalidParams("cwd must be an absolute path")
		}
		if p.MCP != nil {
			if err := p.MCP.Validate(); err != nil {
				return nil, invalidParams(err.Error())
			}
		}
		var opts any
		if len(p.Options) > 0 {
			opts = p.Options
		}
		info, err := mgr.Create(ctx, session.CreateParams{
			Runtime:  p.Runtime,
			Settings: p.Settings,
			Config: agent.SessionConfig{
				Cwd: p.Cwd, Model: p.Model, Effort: p.Effort, ResumeID: p.ResumeID,
				Instructions: p.Instructions, RawEvents: p.RawEvents, Options: opts, MCP: p.MCP,
				HostTools: p.HostTools, DisableHostTools: p.DisableHostTools,
			},
			Metadata: p.Metadata,
			Title:    p.Title,
		})
		if err != nil {
			return nil, err
		}
		return protocol.SessionResult{Session: info}, nil

	case protocol.MethodSessionGet:
		p, err := sessionID(req)
		if err != nil {
			return nil, err
		}
		info, err := mgr.Get(ctx, p.SessionID)
		if err != nil {
			return nil, err
		}
		return protocol.SessionResult{Session: info}, nil

	case protocol.MethodSessionList:
		var p protocol.SessionListParams
		if len(req.Params) != 0 && string(req.Params) != "null" {
			if err := strictParams(req, &p); err != nil {
				return nil, err
			}
		}
		list, err := mgr.Query(ctx, p)
		if err != nil {
			return nil, err
		}
		return protocol.SessionListResult{Sessions: list}, nil

	case protocol.MethodSessionPrompt:
		var raw struct {
			Config   json.RawMessage `json:"config"`
			Settings json.RawMessage `json:"settings"`
		}
		if err := req.UnmarshalParams(&raw); err != nil {
			return nil, invalidParams(err.Error())
		}
		if string(raw.Settings) == "null" {
			return nil, invalidParams("settings must be an object; omit it to inherit")
		}
		if string(raw.Config) == "null" {
			return nil, invalidParams("config must be an object; omit it to inherit")
		}
		var p protocol.SessionPromptParams
		if err := req.UnmarshalParams(&p); err != nil {
			return nil, invalidParams(err.Error())
		}
		if p.SessionID == "" {
			return nil, invalidParams("sessionId is required")
		}
		if len(p.Input) == 0 {
			return nil, invalidParams("input must not be empty")
		}
		for _, part := range p.Input {
			switch part.Type {
			case "text", "":
			case "image":
				if part.Path == "" || !filepath.IsAbs(part.Path) {
					return nil, invalidParams("image input requires an absolute path")
				}
			default:
				return nil, invalidParams("unsupported input type " + part.Type)
			}
		}
		t, err := mgr.Prompt(ctx, p.SessionID, session.PromptParams{TurnID: p.TurnID, Input: p.Input, Config: p.Config, Settings: p.Settings})
		if err != nil {
			return nil, err
		}
		return protocol.SessionPromptResult{TurnID: t.ID}, nil

	case protocol.MethodSessionCancel:
		p, err := sessionID(req)
		if err != nil {
			return nil, err
		}
		return protocol.Empty{}, mgr.Cancel(p.SessionID)

	case protocol.MethodSessionClose:
		p, err := sessionID(req)
		if err != nil {
			return nil, err
		}
		return protocol.Empty{}, mgr.Close(ctx, p.SessionID)

	case protocol.MethodSessionEvents, protocol.MethodSessionEventsPage:
		var p protocol.SessionEventsParams
		if err := req.UnmarshalParams(&p); err != nil {
			return nil, err
		}
		if p.SessionID == "" {
			return nil, invalidParams("sessionId is required")
		}
		if p.Limit < 0 {
			return nil, invalidParams("limit must be nonnegative")
		}
		page, err := mgr.EventsPage(ctx, p.SessionID, p.AfterSeq, p.Limit)
		if err != nil {
			return nil, err
		}
		if page.Events == nil {
			page.Events = []event.Event{}
		}
		return page, nil

	case protocol.MethodApprovalRespond:
		var p protocol.ApprovalRespondParams
		if err := req.UnmarshalParams(&p); err != nil {
			return nil, err
		}
		if p.ApprovalID == "" {
			return nil, invalidParams("approvalId is required")
		}
		if !p.Outcome.Valid() {
			return nil, rpcErr(protocol.CodeInvalidDecision, protocol.ReasonInvalidDecision, "outcome must be allow, allow_session, deny or abort")
		}
		return protocol.Empty{}, mgr.RespondApproval(p.ApprovalID, permission.Decision{
			Outcome: p.Outcome, Message: p.Message, UpdatedInput: p.UpdatedInput,
		})

	case protocol.MethodToolsRegister:
		reg := c.srv.opts.Tools
		if reg == nil {
			return nil, rpcErr(protocol.CodeUnsupported, protocol.ReasonUnsupported, "this server does not host tools")
		}
		var p protocol.ToolsRegisterParams
		if err := req.UnmarshalParams(&p); err != nil {
			return nil, err
		}
		var added []string
		for _, spec := range p.Tools {
			err := reg.Register(tools.Tool{
				Name: spec.Name, Description: spec.Description, InputSchema: spec.InputSchema,
				Approval: spec.Approval, ReadOnly: spec.ReadOnly,
				Timeout: time.Duration(spec.TimeoutMs) * time.Millisecond, Handler: c.forward(spec.Name),
			})
			if err != nil {
				reg.Unregister(added...) // all or nothing
				if errors.Is(err, tools.ErrExists) {
					return nil, rpcErr(jsonrpc.CodeInvalidParams, protocol.ReasonDuplicateTool, err.Error())
				}
				return nil, invalidParams(err.Error())
			}
			added = append(added, spec.Name)
		}
		c.mu.Lock()
		c.tools = append(c.tools, added...)
		c.mu.Unlock()
		return protocol.Empty{}, nil

	case protocol.MethodToolsUnregister:
		var p protocol.ToolsUnregisterParams
		if err := req.UnmarshalParams(&p); err != nil {
			return nil, err
		}
		c.mu.Lock()
		var mine []string
		for _, n := range p.Names {
			if slices.Contains(c.tools, n) {
				mine = append(mine, n)
			}
		}
		c.tools = slices.DeleteFunc(c.tools, func(n string) bool { return slices.Contains(mine, n) })
		c.mu.Unlock()
		if reg := c.srv.opts.Tools; reg != nil {
			reg.Unregister(mine...) // only the client's own tools
		}
		return protocol.Empty{}, nil

	case protocol.MethodToolsList:
		out := []protocol.ToolSpec{}
		if reg := c.srv.opts.Tools; reg != nil {
			for _, t := range reg.List() {
				out = append(out, protocol.ToolSpec{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema,
					Approval: t.Approval, ReadOnly: t.ReadOnly, TimeoutMs: int(t.Timeout / time.Millisecond)})
			}
		}
		return protocol.ToolsListResult{Tools: out}, nil

	case protocol.MethodMCPList:
		var p protocol.MCPListParams
		if err := req.UnmarshalParams(&p); err != nil {
			return nil, err
		}
		res := protocol.MCPListResult{Overview: mcp.Overview{Servers: []mcp.Entry{}, InheritNative: true}, Native: []protocol.NativeMCP{}}
		if r := c.srv.opts.MCP; r != nil {
			cwd := p.Cwd
			if cwd == "" {
				cwd, _ = os.Getwd()
			}
			ov, err := r.Overview(cwd)
			if err != nil {
				return nil, invalidParams(err.Error())
			}
			res.Enabled, res.Overview = true, ov
		}
		for _, name := range p.Runtimes {
			n := protocol.NativeMCP{Runtime: name, Servers: []string{}}
			rt, ok := mgr.Registry().Get(name)
			if !ok {
				return nil, rpcErr(protocol.CodeRuntimeNotFound, protocol.ReasonRuntimeNotFound, "unknown runtime "+name)
			}
			if l, ok := rt.(agent.NativeMCPLister); ok {
				n.Supported = true
				if names, err := l.NativeMCPServers(ctx, p.Cwd); err != nil {
					n.Error = err.Error()
				} else {
					n.Servers = names
				}
			}
			res.Native = append(res.Native, n)
		}
		return res, nil

	case protocol.MethodApprovalList:
		var p protocol.ApprovalListParams
		if err := req.UnmarshalParams(&p); err != nil {
			return nil, err
		}
		list := mgr.PendingApprovals(p.SessionID)
		if list == nil {
			list = []permission.Request{}
		}
		return protocol.ApprovalListResult{Approvals: list}, nil
	}
	return nil, jsonrpc.NewError(jsonrpc.CodeMethodNotFound, "method not found: "+req.Method, nil)
}

// orderKey serialises the requests that change a session's state, so a
// client's "prompt, then cancel" or "prompt, prompt" is applied in the
// order it was sent. Git operations also use this queue, so pipelined stage
// and commit requests stay ordered. Discovery, approvals and non-Git reads run
// concurrently. Git commands are bounded by policy and request cancellation.
func orderKey(req *jsonrpc.Request) string {
	if strings.HasPrefix(req.Method, "git.") {
		var p protocol.SessionIDParams
		if json.Unmarshal(req.Params, &p) == nil && p.SessionID != "" {
			return "session:" + p.SessionID
		}
	}
	if req.Method == protocol.MethodSettingsConfigure {
		var p settings.ConfigureParams
		if json.Unmarshal(req.Params, &p) == nil && p.Target.SessionID != "" {
			return "session:" + p.Target.SessionID
		}
		return "settings"
	}
	switch req.Method {
	case protocol.MethodSessionPrompt, protocol.MethodSessionCancel, protocol.MethodSessionClose,
		protocol.MethodSessionRestore, protocol.MethodSessionDelete, protocol.MethodSessionRename, protocol.MethodSessionConfigure,
		protocol.MethodSessionEvict, protocol.MethodSessionSuspend, protocol.MethodSessionPin,
		protocol.MethodSessionFork, protocol.MethodSessionArchive, protocol.MethodSessionTags:
		var p protocol.SessionIDParams
		if json.Unmarshal(req.Params, &p) == nil && p.SessionID != "" {
			return "session:" + p.SessionID
		}
	}
	return ""
}

func sessionID(req *jsonrpc.Request) (protocol.SessionIDParams, error) {
	var p protocol.SessionIDParams
	if err := req.UnmarshalParams(&p); err != nil {
		return p, err
	}
	if p.SessionID == "" {
		return p, invalidParams("sessionId is required")
	}
	return p, nil
}

// strictParams rejects typoed setting-operation fields instead of ignoring them.
func strictParams(req *jsonrpc.Request, target any) error {
	if len(req.Params) == 0 {
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(req.Params), []byte("null")) {
		return invalidParams("params must be an object")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(req.Params, &fields); err != nil {
		return invalidParams("params must be an object")
	}
	for _, name := range []string{"target", "expectedRevision", "reset"} {
		if value, ok := fields[name]; ok && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return invalidParams(name + " cannot be null; omit it instead")
		}
	}
	dec := json.NewDecoder(bytes.NewReader(req.Params))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return invalidParams(err.Error())
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return invalidParams("trailing parameters")
	}
	return nil
}
