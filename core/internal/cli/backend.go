package cli

import (
	"context"
	"flag"
	"os"
	"time"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/internal/rpcclient"
	"github.com/tomanagle/harness-kit/core/permission"
	"github.com/tomanagle/harness-kit/core/rpc/protocol"
	"github.com/tomanagle/harness-kit/core/session"
	"github.com/tomanagle/harness-kit/core/settings"
	"github.com/tomanagle/harness-kit/core/toolhost"
	"github.com/tomanagle/harness-kit/core/workspace"
	gitworkspace "github.com/tomanagle/harness-kit/core/workspace/git"
)

// backend is what the CLI commands run against: the library in-process, or
// a harness-kit sidecar over JSON-RPC (--rpc). Both expose the same
// operations, so every command exercises either path.
type backend interface {
	Runtimes(ctx context.Context) ([]agent.Info, error)
	Discover(ctx context.Context, names []string) ([]agent.Discovery, error)
	Create(ctx context.Context, p protocol.SessionCreateParams) (session.Info, error)
	Get(ctx context.Context, sessionID string) (session.Info, error)
	Prompt(ctx context.Context, sessionID, turnID string, input []agent.InputPart, cfg *agent.ConfigPatch, selection settings.Patch) error
	Cancel(ctx context.Context, sessionID string) error
	Respond(ctx context.Context, approvalID string, d permission.Decision) error
	Events(ctx context.Context, sessionID string, afterSeq uint64) ([]event.Event, error)
	List(ctx context.Context) ([]session.Info, error)
	Query(ctx context.Context, options session.ListOptions) ([]session.Info, error)
	Diff(ctx context.Context, sessionID, turnID string, options workspace.DiffOptions) (workspace.Diff, error)
	RemoveWorkspace(ctx context.Context, sessionID string) error
	Fork(ctx context.Context, id string, p session.ForkParams) (session.Info, error)
	Archive(ctx context.Context, id string, archived bool) (session.Info, error)
	SetTags(ctx context.Context, id string, tags []string) (session.Info, error)
	Restore(ctx context.Context, sessionID string) (session.Info, error)
	Delete(ctx context.Context, sessionID string) error
	Rename(ctx context.Context, sessionID, title string) (session.Info, error)
	Configure(ctx context.Context, sessionID string, cfg agent.ConfigPatch) (session.Info, error)
	Settings(context.Context, settings.Target) (settings.Snapshot, error)
	ConfigureSettings(context.Context, settings.ConfigureParams) (settings.Snapshot, error)
	RuntimeSessions(ctx context.Context, runtime string, opts agent.ListSessionsOptions) ([]agent.RuntimeSession, error)
	// Stream delivers every event, from creation of the backend onwards.
	Stream() <-chan event.Event
	Close() error
}

type backendFlags struct {
	runtimeFlags
	rpc         bool
	allowUnsafe bool
	store       string
	noKitMCP    bool
}

func (bf *backendFlags) register(fs *flag.FlagSet) {
	bf.runtimeFlags.register(fs)
	fs.BoolVar(&bf.rpc, "rpc", false, "run through a harness-kit sidecar over JSON-RPC instead of in-process")
	fs.BoolVar(&bf.allowUnsafe, "allow-unsafe", false, "accept options that disable runtime sandboxes or approvals")
	def, _ := session.DefaultStoreDir()
	fs.StringVar(&bf.store, "store", def, `session history directory ("memory" keeps nothing; env HARNESS_KIT_STORE)`)
	fs.BoolVar(&bf.noKitMCP, "no-kit-mcp", false, "ignore the MCP servers in your user and project harness-kit config")
}

// openStore returns the store selected by --store: "memory" (or empty),
// "default" (the CLI's own history directory), or a directory.
func openStore(dir string) (session.Store, error) {
	switch dir {
	case "", "memory":
		return session.NewMemoryStore(), nil
	case "default":
		d, err := session.DefaultStoreDir()
		if err != nil {
			return nil, err
		}
		dir = d
	}
	return session.NewFileStore(dir)
}

func (bf *backendFlags) persistent() bool { return bf.store != "" && bf.store != "memory" }

func (bf *backendFlags) open(ctx context.Context) (backend, error) {
	if bf.rpc {
		return newRPCBackend(ctx, bf)
	}
	return newDirectBackend(bf)
}

// directBackend links the library in-process.
type directBackend struct {
	mgr  *session.Manager
	sub  *event.Subscription
	host *toolhost.Host
}

func newDirectBackend(bf *backendFlags) (*directBackend, error) {
	reg, err := bf.registry()
	if err != nil {
		return nil, err
	}
	store, err := openStore(bf.store)
	if err != nil {
		return nil, err
	}
	// The CLI registers no host tools of its own, but the tool host still
	// bridges Claude Code's permission prompts to the terminal.
	host := toolhost.New(toolhost.Options{Version: bf.version})
	opts := session.Options{AllowUnsafe: bf.allowUnsafe, Store: store, ToolServer: host, Workspace: gitworkspace.New(bf.gitPath)}
	if !bf.noKitMCP {
		r, err := kitResolver()
		if err != nil {
			return nil, err
		}
		opts.Prepare = kitPrepare(r)
	}
	mgr := session.NewManager(reg, opts)
	return &directBackend{mgr: mgr, sub: mgr.Subscribe(nil, 0), host: host}, nil
}

func (d *directBackend) Settings(ctx context.Context, target settings.Target) (settings.Snapshot, error) {
	return d.mgr.Settings(ctx, target)
}

func (d *directBackend) ConfigureSettings(ctx context.Context, p settings.ConfigureParams) (settings.Snapshot, error) {
	return d.mgr.ConfigureSettings(ctx, p)
}

func (d *directBackend) Runtimes(context.Context) ([]agent.Info, error) {
	return d.mgr.Registry().List(), nil
}

func (d *directBackend) Discover(ctx context.Context, names []string) ([]agent.Discovery, error) {
	return d.mgr.Registry().Discover(ctx, names...)
}

func (d *directBackend) Create(ctx context.Context, p protocol.SessionCreateParams) (session.Info, error) {
	var opts any
	if len(p.Options) > 0 {
		opts = p.Options
	}
	return d.mgr.Create(ctx, session.CreateParams{
		Settings: p.Settings,
		Runtime:  p.Runtime,
		Config: agent.SessionConfig{
			Cwd: p.Cwd, Model: p.Model, Effort: p.Effort, ResumeID: p.ResumeID, Instructions: p.Instructions,
			RawEvents: p.RawEvents, Options: opts, MCP: p.MCP,
			HostTools: p.HostTools, DisableHostTools: p.DisableHostTools,
		},
		Metadata: p.Metadata,
		Title:    p.Title,
	})
}

func (d *directBackend) Get(ctx context.Context, id string) (session.Info, error) {
	return d.mgr.Get(ctx, id)
}

func (d *directBackend) Prompt(ctx context.Context, sid, turnID string, input []agent.InputPart, cfg *agent.ConfigPatch, selection settings.Patch) error {
	_, err := d.mgr.Prompt(ctx, sid, session.PromptParams{TurnID: turnID, Input: input, Config: cfg, Settings: selection})
	return err
}

func (d *directBackend) Cancel(_ context.Context, sid string) error { return d.mgr.Cancel(sid) }

func (d *directBackend) Respond(_ context.Context, id string, dec permission.Decision) error {
	return d.mgr.RespondApproval(id, dec)
}

func (d *directBackend) Events(_ context.Context, sid string, after uint64) ([]event.Event, error) {
	evs, _, err := d.mgr.Events(sid, after, 0)
	return evs, err
}

func (d *directBackend) Query(ctx context.Context, p session.ListOptions) ([]session.Info, error) {
	return d.mgr.Query(ctx, p)
}

func (d *directBackend) Fork(ctx context.Context, id string, p session.ForkParams) (session.Info, error) {
	return d.mgr.Fork(ctx, id, p)
}

func (d *directBackend) Archive(ctx context.Context, id string, archived bool) (session.Info, error) {
	return d.mgr.Archive(ctx, id, archived)
}

func (d *directBackend) SetTags(ctx context.Context, id string, tags []string) (session.Info, error) {
	return d.mgr.SetTags(ctx, id, tags)
}

func (d *directBackend) List(ctx context.Context) ([]session.Info, error) { return d.mgr.List(ctx) }

func (d *directBackend) Restore(ctx context.Context, id string) (session.Info, error) {
	return d.mgr.Restore(ctx, id)
}

func (d *directBackend) Delete(ctx context.Context, id string) error { return d.mgr.Delete(ctx, id) }

func (d *directBackend) Diff(ctx context.Context, id, turnID string, options workspace.DiffOptions) (workspace.Diff, error) {
	return d.mgr.TurnDiff(ctx, id, turnID, options)
}

func (d *directBackend) RemoveWorkspace(ctx context.Context, id string) error {
	return d.mgr.RemoveWorkspace(ctx, id)
}

func (d *directBackend) Rename(ctx context.Context, id, title string) (session.Info, error) {
	return d.mgr.Rename(ctx, id, title)
}

func (d *directBackend) Configure(ctx context.Context, id string, cfg agent.ConfigPatch) (session.Info, error) {
	return d.mgr.Configure(ctx, id, cfg)
}

func (d *directBackend) RuntimeSessions(ctx context.Context, rt string, opts agent.ListSessionsOptions) ([]agent.RuntimeSession, error) {
	return d.mgr.Registry().ListSessions(ctx, rt, opts)
}

func (d *directBackend) Stream() <-chan event.Event { return d.sub.C }

func (d *directBackend) Close() error {
	d.sub.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	err := d.mgr.Shutdown(ctx)
	d.host.Close()
	return err
}

// rpcBackend spawns this binary as a sidecar and speaks the protocol to it.
type rpcBackend struct {
	c  *client.Client
	ch chan event.Event
}

func newRPCBackend(ctx context.Context, bf *backendFlags) (*rpcBackend, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	args := []string{"serve", "--log-level", "warn", "--runtimes", bf.runtimes, "--fake-delay", bf.fakeDelay.String()}
	if bf.fake {
		args = append(args, "--fake-runtime")
	}
	if bf.codexPath != "" {
		args = append(args, "--codex-path", bf.codexPath)
	}
	if bf.claudePath != "" {
		args = append(args, "--claude-path", bf.claudePath)
	}
	if bf.gitPath != "" {
		args = append(args, "--git-path", bf.gitPath)
	}
	if bf.allowUnsafe {
		args = append(args, "--allow-unsafe")
	}
	if bf.persistent() {
		args = append(args, "--store", bf.store)
	}
	if !bf.noKitMCP {
		args = append(args, "--kit-mcp")
	}
	b := &rpcBackend{ch: make(chan event.Event, event.DefaultBuffer)}
	c, err := client.Spawn(ctx, self, args, os.Stderr, client.Options{
		ClientInfo: protocol.ClientInfo{Name: "harness-kit-cli", Version: bf.version},
		OnEvent:    func(ev event.Event) { b.ch <- ev },
	})
	if err != nil {
		return nil, err
	}
	b.c = c
	return b, nil
}

func (r *rpcBackend) Settings(ctx context.Context, target settings.Target) (settings.Snapshot, error) {
	return r.c.Settings(ctx, target)
}

func (r *rpcBackend) ConfigureSettings(ctx context.Context, p settings.ConfigureParams) (settings.Snapshot, error) {
	return r.c.ConfigureSettings(ctx, p)
}

func (r *rpcBackend) Runtimes(ctx context.Context) ([]agent.Info, error) {
	res, err := r.c.Runtimes(ctx)
	return res.Runtimes, err
}

func (r *rpcBackend) Diff(ctx context.Context, id, turnID string, options workspace.DiffOptions) (workspace.Diff, error) {
	return r.c.TurnDiff(ctx, protocol.SessionDiffParams{SessionID: id, TurnID: turnID, DiffOptions: options})
}

func (r *rpcBackend) RemoveWorkspace(ctx context.Context, id string) error {
	return r.c.RemoveWorkspace(ctx, id)
}

func (r *rpcBackend) Discover(ctx context.Context, names []string) ([]agent.Discovery, error) {
	res, err := r.c.Discover(ctx, names...)
	return res.Runtimes, err
}

func (r *rpcBackend) Create(ctx context.Context, p protocol.SessionCreateParams) (session.Info, error) {
	return r.c.CreateSession(ctx, p)
}

func (r *rpcBackend) Get(ctx context.Context, id string) (session.Info, error) {
	return r.c.GetSession(ctx, id)
}

func (r *rpcBackend) Prompt(ctx context.Context, sid, turnID string, input []agent.InputPart, cfg *agent.ConfigPatch, selection settings.Patch) error {
	_, err := r.c.Prompt(ctx, protocol.SessionPromptParams{SessionID: sid, TurnID: turnID, Input: input, Config: cfg, Settings: selection})
	return err
}

func (r *rpcBackend) Cancel(ctx context.Context, sid string) error { return r.c.Cancel(ctx, sid) }

func (r *rpcBackend) Respond(ctx context.Context, id string, d permission.Decision) error {
	return r.c.RespondApproval(ctx, id, d)
}

func (r *rpcBackend) Events(ctx context.Context, sid string, after uint64) ([]event.Event, error) {
	res, err := r.c.Events(ctx, protocol.SessionEventsParams{SessionID: sid, AfterSeq: after})
	return res.Events, err
}

func (r *rpcBackend) Query(ctx context.Context, p session.ListOptions) ([]session.Info, error) {
	return r.c.QuerySessions(ctx, p)
}

func (r *rpcBackend) Fork(ctx context.Context, id string, p session.ForkParams) (session.Info, error) {
	return r.c.ForkSession(ctx, id, p)
}

func (r *rpcBackend) Archive(ctx context.Context, id string, archived bool) (session.Info, error) {
	return r.c.ArchiveSession(ctx, id, archived)
}

func (r *rpcBackend) SetTags(ctx context.Context, id string, tags []string) (session.Info, error) {
	return r.c.SetSessionTags(ctx, id, tags)
}

func (r *rpcBackend) List(ctx context.Context) ([]session.Info, error) { return r.c.ListSessions(ctx) }

func (r *rpcBackend) Restore(ctx context.Context, id string) (session.Info, error) {
	return r.c.RestoreSession(ctx, id)
}

func (r *rpcBackend) Delete(ctx context.Context, id string) error { return r.c.DeleteSession(ctx, id) }

func (r *rpcBackend) Rename(ctx context.Context, id, title string) (session.Info, error) {
	return r.c.RenameSession(ctx, id, title)
}

func (r *rpcBackend) Configure(ctx context.Context, id string, cfg agent.ConfigPatch) (session.Info, error) {
	return r.c.ConfigureSession(ctx, id, cfg)
}

func (r *rpcBackend) RuntimeSessions(ctx context.Context, rt string, opts agent.ListSessionsOptions) ([]agent.RuntimeSession, error) {
	return r.c.RuntimeSessions(ctx, protocol.RuntimeSessionsParams{Runtime: rt, Cwd: opts.Cwd, Limit: opts.Limit})
}

func (r *rpcBackend) Stream() <-chan event.Event { return r.ch }

func (r *rpcBackend) Close() error { return r.c.Close() }
