package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/mcp"
	mcpclient "github.com/tomanagle/harness-kit/core/mcp/client"
	"github.com/tomanagle/harness-kit/core/permission"
	"github.com/tomanagle/harness-kit/core/rpc/protocol"
	"github.com/tomanagle/harness-kit/core/runtimes/claude"
	"github.com/tomanagle/harness-kit/core/runtimes/codex"
	"github.com/tomanagle/harness-kit/core/runtimes/fake"
	"github.com/tomanagle/harness-kit/core/session"
	"github.com/tomanagle/harness-kit/core/toolhost"
	"github.com/tomanagle/harness-kit/core/tools"
	"github.com/tomanagle/harness-kit/core/workspace"
	gitworkspace "github.com/tomanagle/harness-kit/core/workspace/git"
)

// Direct links Harness Kit into the Wails backend: no sidecar process. It
// implements the dashboard API (../ui/api.js) as typed Wails bindings, and
// its demo host tools are Go functions served through toolhost (the app
// binary doubles as the MCP bridge; see main).
type Direct struct {
	ctx      context.Context
	mgr      *session.Manager
	sub      *event.Subscription
	host     *toolhost.Host
	reg      *tools.Registry
	resolver mcp.Resolver
	cwd      string
}

// NewDirect returns the in-process backend.
func NewDirect() *Direct {
	d := &Direct{reg: tools.NewRegistry()}
	d.cwd, _ = os.Getwd()
	registerDemoTools(d.reg)
	d.host = toolhost.New(toolhost.Options{Registry: d.reg})

	if user, err := mcp.UserConfigPath(); err == nil {
		trust, _ := mcp.TrustPath()
		d.resolver = mcp.Resolver{UserFile: user, Project: true, Trust: mcp.TrustStore{Path: trust}}
	}
	opts := session.Options{ToolServer: d.host, Prepare: d.prepare, Workspace: gitworkspace.New("")}
	if store, err := session.NewFileStore(storeDir()); err == nil {
		opts.Store = store
	}
	runtimes := agent.NewRegistry(codex.New(), claude.New(), fake.New(fake.WithMCPCaller(callMCP)))
	d.mgr = session.NewManager(runtimes, opts)
	return d
}

// prepare merges the user's and trusted project MCP config into sessions.
func (d *Direct) prepare(_ context.Context, p *session.CreateParams) ([]string, error) {
	var own mcp.Config
	if p.Config.MCP != nil {
		own = *p.Config.MCP
	}
	res, err := d.resolver.Resolve(p.Config.Cwd, p.Runtime, own)
	if err != nil {
		return nil, err
	}
	if c := res.Config; !c.IsZero() {
		p.Config.MCP = &c
	}
	return res.Warnings, nil
}

func (d *Direct) startup(ctx context.Context) {
	d.ctx = ctx
	d.sub = d.mgr.Subscribe(nil, 0)
	go func() {
		for ev := range d.sub.C {
			wailsruntime.EventsEmit(ctx, "harness-kit:event", ev)
		}
	}()
}

func (d *Direct) shutdown(ctx context.Context) {
	d.sub.Close()
	_ = d.mgr.Shutdown(ctx)
	d.host.Close()
}

// OverviewResult is what the dashboard's sidebar shows.
type OverviewResult struct {
	Git      bool                   `json:"git"`
	Runtimes []agent.Discovery      `json:"runtimes"`
	Tools    []protocol.ToolSpec    `json:"tools"`
	MCP      protocol.MCPListResult `json:"mcp"`
}

// Overview lists runtimes, host tools and MCP servers.
func (d *Direct) Overview() (OverviewResult, error) {
	var out OverviewResult
	out.Git = true // NewDirect explicitly installs the native Git adapter.
	var err error
	if out.Runtimes, err = d.mgr.Registry().Discover(d.ctx); err != nil {
		return out, err
	}
	for _, t := range d.reg.List() {
		out.Tools = append(out.Tools, protocol.ToolSpec{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema, Approval: t.Approval, ReadOnly: t.ReadOnly})
	}
	out.MCP.Native = []protocol.NativeMCP{}
	if ov, err := d.resolver.Overview(d.cwd); err == nil {
		out.MCP.Enabled, out.MCP.Overview = true, ov
	}
	if rt, ok := d.mgr.Registry().Get(codex.Name); ok {
		if l, ok := rt.(agent.NativeMCPLister); ok {
			n := protocol.NativeMCP{Runtime: codex.Name, Supported: true, Servers: []string{}}
			if names, err := l.NativeMCPServers(d.ctx, d.cwd); err != nil {
				n.Error = err.Error()
			} else {
				n.Servers = names
			}
			out.MCP.Native = append(out.MCP.Native, n)
		}
	}
	return out, nil
}

// ListSessions lists stored and live sessions.
func (d *Direct) ListSessions() ([]session.Info, error) { return d.mgr.List(d.ctx) }

// CreateSession starts a session in the app's working directory.
func (d *Direct) CreateSession(runtime string) (session.Info, error) {
	return d.mgr.Create(d.ctx, session.CreateParams{Runtime: runtime, Config: agent.SessionConfig{Cwd: d.cwd}})
}

// RenameSession sets a session's title.
func (d *Direct) RenameSession(id, title string) (session.Info, error) {
	return d.mgr.Rename(d.ctx, id, title)
}

// OpenSession reads a session without starting its runtime.
func (d *Direct) OpenSession(id string) (session.Info, error) { return d.mgr.Get(d.ctx, id) }

// Context reads the same cached observation as the sidecar without starting a runtime.
func (d *Direct) Context(id string) (session.ContextInfo, error) { return d.mgr.Context(d.ctx, id) }

// Compact waits for native completion; Cancel remains available during this call.
func (d *Direct) Compact(id string) (session.ContextInfo, error) { return d.mgr.Compact(d.ctx, id) }

// Git bindings keep repository selection tied to the session. The example
// exposes explicit actions; application-specific automation belongs to its host.
func (d *Direct) GitStatus(id string) (workspace.RepositoryStatus, error) {
	return d.mgr.GitStatus(d.ctx, id)
}
func (d *Direct) GitDiff(id string, p workspace.FileDiffParams) (workspace.FileDiff, error) {
	return d.mgr.GitDiff(d.ctx, id, p)
}
func (d *Direct) GitBranches(id string) (workspace.BranchesResult, error) {
	return d.mgr.GitBranches(d.ctx, id)
}
func (d *Direct) GitLog(id string, p workspace.LogParams) (workspace.LogResult, error) {
	return d.mgr.GitLog(d.ctx, id, p)
}
func (d *Direct) GitStage(id string, p workspace.PathsParams) error {
	return d.mgr.GitStage(d.ctx, id, p)
}
func (d *Direct) GitUnstage(id string, p workspace.PathsParams) error {
	return d.mgr.GitUnstage(d.ctx, id, p)
}
func (d *Direct) GitCommit(id string, p workspace.CommitParams) (workspace.CommitResult, error) {
	return d.mgr.GitCommit(d.ctx, id, p)
}
func (d *Direct) GitCheckout(id string, p workspace.CheckoutParams) error {
	return d.mgr.GitCheckout(d.ctx, id, p)
}
func (d *Direct) GitFetch(id string, p workspace.RemoteParams) (workspace.RemoteResult, error) {
	return d.mgr.GitFetch(d.ctx, id, p)
}
func (d *Direct) GitPull(id string, p workspace.RemoteParams) (workspace.RemoteResult, error) {
	return d.mgr.GitPull(d.ctx, id, p)
}
func (d *Direct) GitPush(id string, p workspace.RemoteParams) (workspace.RemoteResult, error) {
	return d.mgr.GitPush(d.ctx, id, p)
}

// History returns the session's retained events.
func (d *Direct) History(id string) (protocol.SessionEventsResult, error) {
	evs, truncated, err := d.mgr.Events(id, 0, 0)
	if evs == nil {
		evs = []event.Event{}
	}
	return protocol.SessionEventsResult{Events: evs, Truncated: truncated}, err
}

// Prompt starts a turn and returns its ID.
func (d *Direct) Prompt(sessionID, text string) (string, error) {
	if _, err := d.mgr.Restore(d.ctx, sessionID); err != nil {
		return "", err
	}
	t, err := d.mgr.Prompt(d.ctx, sessionID, session.PromptParams{Input: agent.Text(text)})
	if err != nil {
		return "", err
	}
	return t.ID, nil
}

// Cancel interrupts the active turn.
func (d *Direct) Cancel(sessionID string) error { return d.mgr.Cancel(sessionID) }

// RespondApproval answers an approval request.
func (d *Direct) RespondApproval(approvalID, outcome string) error {
	return d.mgr.RespondApproval(approvalID, permission.Decision{Outcome: permission.Outcome(outcome)})
}

// registerDemoTools mirrors ../ui/demo-tools.js, as Go functions.
func registerDemoTools(reg *tools.Registry) {
	orders := map[string]string{"A7": "shipped", "B2": "processing", "C9": "delivered"}
	type orderID struct {
		ID string `json:"id" desc:"Order ID"`
	}
	tools.Add(reg, "order_status", "Look up the status of an order by ID (try A7, B2 or C9)",
		func(_ context.Context, in orderID) (map[string]string, error) {
			status, ok := orders[in.ID]
			if !ok {
				status = "unknown"
			}
			return map[string]string{"id": in.ID, "status": status}, nil
		}, tools.ReadOnly())
	tools.Add(reg, "refund", "Refund an order",
		func(_ context.Context, in orderID) (string, error) {
			if _, ok := orders[in.ID]; !ok {
				return "", errors.New("no order " + in.ID)
			}
			orders[in.ID] = "refunded"
			return fmt.Sprintf("Order %s refunded", in.ID), nil
		}, tools.WithApproval(mcp.ApprovalAsk))
}

// callMCP lets the fake runtime call host tools (its [call] directive).
func callMCP(ctx context.Context, s mcp.Server, tool string, args json.RawMessage) (string, bool, error) {
	sess, err := mcpclient.Connect(ctx, s, mcpclient.Options{})
	if err != nil {
		return "", false, err
	}
	defer sess.Close()
	res, err := sess.CallTool(ctx, tool, args)
	return res.Text(), res.IsError, err
}
