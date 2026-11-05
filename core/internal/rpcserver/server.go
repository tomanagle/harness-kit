// Package rpcserver implements the sidecar protocol behind rpc/server.
package rpcserver

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"time"

	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/internal/jsonrpc"
	"github.com/tomanagle/harness-kit/core/mcp"
	"github.com/tomanagle/harness-kit/core/rpc/protocol"
	"github.com/tomanagle/harness-kit/core/session"
	"github.com/tomanagle/harness-kit/core/settings"
	"github.com/tomanagle/harness-kit/core/tools"
)

// ToolRegistry is the tool inventory the RPC server consumes. Hosts can supply
// their own implementation without coupling the server to tools.Registry.
type ToolRegistry interface {
	Register(tools.Tool) error
	Unregister(...string)
	List() []tools.Tool
}

// MCPResolver supplies a host's MCP overview without prescribing its storage.
type MCPResolver interface {
	Overview(string) (mcp.Overview, error)
}

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
	MCP MCPResolver
	// Tools, if set, lets clients register host tools (tools.register).
	// It should be the registry served to sessions (toolhost.Options).
	Tools  ToolRegistry
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
		opts.Logger = mgr.Logger()
	}
	return &Server{mgr: mgr, opts: opts, log: opts.Logger}
}

// Serve runs one client connection until r reaches EOF. It does not shut
// the Manager down; the caller decides whether sessions outlive the
// connection. The host owns transport cleanup and must unblock both reads and
// writes when cancelling or ending the connection. Queue bounds protect log
// producers, but cannot interrupt an arbitrary blocking io.Writer.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	c := &conn{srv: s, hookOwner: session.NewID("connection_")}
	c.rpc = jsonrpc.NewConn(r, w, c, jsonrpc.Options{
		CancelMethod: protocol.NotificationCancelRequest,
		OrderKey:     c.orderKey,
		// initialize completes before any later request is read, so clients
		// may pipeline requests without awaiting its response.
		Inline:  func(r *jsonrpc.Request) bool { return r.Method == protocol.MethodInitialize },
		OnError: func(err error) { s.log.Warn("rpc", "err", err) },
	})
	err := c.rpc.Run(ctx)
	c.stopEvents()
	c.dropTools()
	s.mgr.Hooks().DropOwner(c.hookOwner)
	return err
}
