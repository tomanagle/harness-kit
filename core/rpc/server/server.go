// Package server exposes a session.Manager over the Harness Kit JSON-RPC
// protocol. It is transport-agnostic: Serve takes any reader/writer pair,
// typically the process's stdin and stdout.
package server

import (
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/tomanagle/harness-kit/core/internal/rpcserver"
	"github.com/tomanagle/harness-kit/core/mcp"
	"github.com/tomanagle/harness-kit/core/session"
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
type Server struct{ impl *rpcserver.Server }

// New returns a Server.
func New(mgr *session.Manager, opts Options) *Server {
	return &Server{impl: rpcserver.New(mgr, rpcserver.Options{
		Name: opts.Name, Version: opts.Version,
		EventBuffer: opts.EventBuffer, AllowUnsafe: opts.AllowUnsafe,
		EventLogSize: opts.EventLogSize, ShutdownTimeout: opts.ShutdownTimeout,
		MCP: opts.MCP, Tools: opts.Tools, Logger: opts.Logger,
	})}
}

// Serve runs one client connection until r reaches EOF. It does not shut
// the Manager down; the caller decides whether sessions outlive the
// connection. The host owns transport cleanup and must unblock both reads and
// writes when cancelling or ending the connection. Queue bounds protect log
// producers, but cannot interrupt an arbitrary blocking io.Writer.
func (s *Server) Serve(ctx context.Context, r io.Reader, w io.Writer) error {
	return s.impl.Serve(ctx, r, w)
}
