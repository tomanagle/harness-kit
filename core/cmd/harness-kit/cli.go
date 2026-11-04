package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/budget"
	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/permission"
	"github.com/tomanagle/harness-kit/core/rpc/client"
	"github.com/tomanagle/harness-kit/core/rpc/protocol"
	"github.com/tomanagle/harness-kit/core/session"
	"github.com/tomanagle/harness-kit/core/settings"
	"github.com/tomanagle/harness-kit/core/toolhost"
	"github.com/tomanagle/harness-kit/core/workspace"
	gitworkspace "github.com/tomanagle/harness-kit/core/workspace/git"
)

// exitCode is returned by commands that want a specific process exit code.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

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
	host := toolhost.New(toolhost.Options{Version: buildVersion()})
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
		ClientInfo: protocol.ClientInfo{Name: "harness-kit-cli", Version: buildVersion()},
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
func (r *rpcBackend) Close() error               { return r.c.Close() }

// parseArgs parses flags that may be interspersed with positional
// arguments ("prompt hello -runtime fake"). Everything after "--" is
// positional.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var tail []string
	for i, a := range args {
		if a == "--" {
			args, tail = args[:i], args[i+1:]
			break
		}
	}
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return append(pos, tail...), nil
		}
		pos, args = append(pos, args[0]), args[1:]
	}
}

func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// outputFlags implements --output (alias -o) and the --json shorthand.
type outputFlags struct {
	format string
	json   bool
}

func (o *outputFlags) register(fs *flag.FlagSet, formats string) {
	fs.StringVar(&o.format, "output", "text", "output format: "+formats)
	fs.StringVar(&o.format, "o", "text", "shorthand for --output")
	fs.BoolVar(&o.json, "json", false, "shorthand for --output json")
}

func (o *outputFlags) resolve(allowed ...string) error {
	if o.json {
		if o.format != "text" && o.format != "json" {
			return fmt.Errorf("--json conflicts with --output %s", o.format)
		}
		o.format = "json"
	}
	for _, a := range allowed {
		if o.format == a {
			return nil
		}
	}
	return fmt.Errorf("--output must be one of: %s", strings.Join(allowed, ", "))
}

// ---- runtimes ------------------------------------------------------------

func runtimesCmd(args []string) error {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("runtimes "+sub, flag.ContinueOnError)
	var bf backendFlags
	bf.register(fs)
	var out outputFlags
	out.register(fs, "text, json")
	names, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if err := out.resolve("text", "json"); err != nil {
		return err
	}
	asJSON := out.format == "json"
	bf.store = "memory" // runtime commands keep no history
	ctx := context.Background()
	b, err := bf.open(ctx)
	if err != nil {
		return err
	}
	defer b.Close()

	switch sub {
	case "list":
		infos, err := b.Runtimes(ctx)
		if err != nil {
			return err
		}
		if asJSON {
			return printJSON(infos)
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tDISPLAY NAME\tSTREAMING\tAPPROVALS\tRESUME\tCANCEL\tCOST")
		for _, i := range infos {
			c := i.Capabilities
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", i.Name, i.DisplayName, yes(c.Streaming), yes(c.Approvals), yes(c.Resume), yes(c.Cancel), yes(c.Cost))
		}
		return tw.Flush()
	case "discover":
		ds, err := b.Discover(ctx, names)
		if err != nil {
			return err
		}
		if asJSON {
			return printJSON(ds)
		}
		printDiscovery(ds)
		return nil
	}
	return fmt.Errorf("unknown command \"runtimes %s\" (try list or discover)", sub)
}

func yes(b bool) string {
	if b {
		return "yes"
	}
	return "-"
}

// printJSON prints one JSON document (coloured by jq on a terminal).
func printJSON(v any) error {
	w, done := jsonWriter()
	defer done()
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func printDiscovery(ds []agent.Discovery) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "RUNTIME\tREADY\tVERSION\tAUTH\tPATH")
	for _, d := range ds {
		auth := string(d.Auth.State)
		if d.Auth.Method != "" {
			auth += " (" + d.Auth.Method
			if d.Auth.Plan != "" {
				auth += ", " + d.Auth.Plan
			}
			auth += ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", d.Runtime, yes(d.Ready), dash(d.Version), auth, dash(d.Path))
	}
	tw.Flush()
	for _, d := range ds {
		for _, p := range d.Problems {
			fmt.Printf("  %s: problem: %s\n", d.Runtime, p)
		}
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ---- prompt and chat -----------------------------------------------------

type sessionFlags struct {
	backendFlags
	runtime            string
	cwd                string
	model              string
	effort             string
	turnConfig         string
	sessionSettingsRaw string
	turnSettingsRaw    string
	sessionSettings    settings.Patch
	turnSettings       settings.Patch
	turnPatch          *agent.ConfigPatch
	resume             string
	instructions       string
	title              string
	options            string
	approve            string
	out                outputFlags
	raw                bool
	mcpFile            string
	noNativeMCP        bool
	images             []string
}

func (sf *sessionFlags) register(fs *flag.FlagSet) {
	sf.backendFlags.register(fs)
	fs.StringVar(&sf.sessionSettingsRaw, "session-settings", "", "shared settings JSON for creation or a restored session")
	fs.StringVar(&sf.turnSettingsRaw, "turn-settings", "", "shared settings JSON for this turn")
	fs.Func("image", "attach a local image to the first prompt (repeat for multiple images)", func(path string) error {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		sf.images = append(sf.images, absolute)
		return nil
	})
	fs.StringVar(&sf.runtime, "runtime", "codex", "runtime to use: codex, claude or fake")
	fs.StringVar(&sf.cwd, "cwd", ".", "working directory for the agent")
	fs.StringVar(&sf.model, "model", "", "model, in the runtime's own naming")
	fs.StringVar(&sf.effort, "effort", "", "default reasoning effort, in the runtime's own naming")
	fs.StringVar(&sf.turnConfig, "turn-config", "", `configuration overrides for each prompt as JSON, e.g. '{"model":"model-name","effort":"high"}'`)
	fs.StringVar(&sf.resume, "resume", "", "session to continue: a Harness Kit ID (ses_..., from `harness-kit sessions`) or a runtime-native ID with -runtime")
	fs.StringVar(&sf.instructions, "instructions", "", "extra instructions appended to the runtime's system prompt")
	fs.StringVar(&sf.title, "title", "", "name the session (default: the start of the first prompt); with -resume ses_..., renames it")
	fs.StringVar(&sf.options, "options", "", `runtime-specific options as JSON, e.g. '{"permissionMode":"acceptEdits"}' (see docs/runtimes.md)`)
	fs.StringVar(&sf.approve, "approve", "", "how to answer approval requests: ask (default when interactive), allow, or deny")
	sf.out.register(fs, "text (readable reply), json (one result object per turn), stream-json (every event as a JSON line)")
	fs.BoolVar(&sf.raw, "raw", false, "also emit unmapped native runtime events (runtime.raw)")
	fs.StringVar(&sf.mcpFile, "mcp", "", "add the MCP servers in this file (harness-kit.json format) for this session")
	fs.BoolVar(&sf.noNativeMCP, "no-native-mcp", false, "don't load the MCP servers configured in the runtime itself")
}

// prepare finalises flags after parsing: the requested runtime is always
// registered, unless --runtimes was set explicitly.
func (sf *sessionFlags) prepare(fs *flag.FlagSet) error {
	explicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "runtimes" {
			explicit = true
		}
	})
	if !explicit {
		sf.runtimes = sf.runtime
		if isSessionID(sf.resume) {
			sf.runtimes = "codex,claude,fake" // the stored session names its runtime
		}
	}
	if isSessionID(sf.resume) && !sf.persistent() {
		return fmt.Errorf("-resume %s needs a session store (--store)", sf.resume)
	}
	switch sf.approve {
	case "":
		sf.approve = "ask"
	case "ask", "allow", "deny":
	default:
		return fmt.Errorf("--approve must be ask, allow or deny")
	}
	if err := sf.out.resolve("text", "json", "stream-json"); err != nil {
		return err
	}
	if sf.options != "" && !json.Valid([]byte(sf.options)) {
		return fmt.Errorf("--options is not valid JSON")
	}
	if sf.turnSettingsRaw != "" && sf.turnConfig != "" {
		return errors.New("--turn-settings and --turn-config cannot both be supplied")
	}
	for _, item := range []struct {
		raw    string
		target *settings.Patch
	}{{sf.sessionSettingsRaw, &sf.sessionSettings}, {sf.turnSettingsRaw, &sf.turnSettings}} {
		if item.raw != "" {
			if err := json.Unmarshal([]byte(item.raw), item.target); err != nil {
				return err
			}
		}
	}
	if sf.turnConfig != "" {
		patch, err := parseConfigPatch(sf.turnConfig)
		if err != nil {
			return fmt.Errorf("--turn-config: %w", err)
		}
		sf.turnPatch = &patch
	}
	abs, err := filepath.Abs(sf.cwd)
	if err != nil {
		return err
	}
	sf.cwd = abs
	return nil
}

func (sf *sessionFlags) createParams() (protocol.SessionCreateParams, error) {
	extra, err := sessionMCP(sf.mcpFile, sf.noNativeMCP)
	if err != nil {
		return protocol.SessionCreateParams{}, err
	}
	p := protocol.SessionCreateParams{
		Settings: sf.sessionSettings,
		MCP:      extra,
		Runtime:  sf.runtime, Cwd: sf.cwd, Model: sf.model, Effort: sf.effort, ResumeID: sf.resume,
		Instructions: sf.instructions, RawEvents: sf.raw, Title: sf.title,
		Metadata: map[string]string{"client": "harness-kit-cli"},
	}
	if sf.options != "" {
		p.Options = json.RawMessage(sf.options)
	}
	return p, nil
}

// console serialises terminal input between the chat prompt and approval
// questions: one goroutine reads lines, whoever is asking consumes them.
type console struct {
	once  sync.Once
	lines chan string
}

func (c *console) next(cancel <-chan struct{}) (string, bool) {
	c.once.Do(func() {
		c.lines = make(chan string)
		go func() {
			sc := bufio.NewScanner(os.Stdin)
			for sc.Scan() {
				c.lines <- sc.Text()
			}
			close(c.lines)
		}()
	})
	select {
	case l, ok := <-c.lines:
		return strings.TrimSpace(l), ok
	case <-cancel:
		return "", false
	}
}

// turnResult is the --output json object.
type turnResult struct {
	Budget           *budget.Exceeded `json:"budget,omitempty"`
	SessionID        string           `json:"sessionId"`
	Runtime          string           `json:"runtime"`
	RuntimeSessionID string           `json:"runtimeSessionId,omitempty"`
	TurnID           string           `json:"turnId"`
	Status           event.TurnStatus `json:"status"`
	Text             string           `json:"text"`
	Usage            *event.UsageData `json:"usage,omitempty"`
	Error            *event.ErrorInfo `json:"error,omitempty"`
	DurationMs       int64            `json:"durationMs"`
}

// replyText assembles the agent's reply from message events, in order,
// ignoring subagent messages.
type replyText struct {
	order []string
	text  map[string]string
}

func (t *replyText) add(itemID, text string, replace bool) {
	if t.text == nil {
		t.text = map[string]string{}
	}
	if _, ok := t.text[itemID]; !ok {
		t.order = append(t.order, itemID)
	}
	if replace {
		t.text[itemID] = text
	} else {
		t.text[itemID] += text
	}
}

func (t *replyText) String() string {
	parts := make([]string, 0, len(t.order))
	for _, id := range t.order {
		if s := strings.TrimSpace(t.text[id]); s != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n\n")
}

type turnRunner struct {
	b        backend
	sf       *sessionFlags
	con      *console
	canAsk   bool
	sigs     chan os.Signal
	info     session.Info
	textOut  io.Writer
	closeOut func() // flushes textOut (stops jq)
}

// run executes one turn and returns its result.
func (r *turnRunner) run(input []agent.InputPart) (turnResult, error) {
	ctx := context.Background()
	for _, path := range r.sf.images {
		input = append(input, agent.InputPart{Type: "image", Path: path})
	}
	r.sf.images = nil
	start := time.Now()
	turnID := session.NewID("turn_")
	res := turnResult{SessionID: r.info.ID, Runtime: r.info.Runtime, TurnID: turnID}
	if err := r.b.Prompt(ctx, r.info.ID, turnID, input, r.sf.turnPatch, r.sf.turnSettings); err != nil {
		return res, err
	}
	var reply replyText
	streamAll := r.sf.out.format == "stream-json"
	done := make(chan struct{})
	defer close(done)

	var lastSeq uint64
	cancelled := false
	for {
		select {
		case <-r.sigs:
			if cancelled {
				fmt.Fprintln(os.Stderr, "\nforce quit")
				os.Exit(130)
			}
			cancelled = true
			fmt.Fprintln(os.Stderr, "\n[cancelling turn - press Ctrl-C again to force quit]")
			_ = r.b.Cancel(ctx, r.info.ID)
		case ev, ok := <-r.b.Stream():
			if !ok {
				return res, errors.New("connection closed")
			}
			if ev.SessionID != r.info.ID {
				continue
			}
			batch := []event.Event{ev}
			if ev.Type == event.StreamGap {
				missed, err := r.b.Events(ctx, r.info.ID, lastSeq)
				if err != nil {
					return res, err
				}
				batch = missed
			}
			for _, e := range batch {
				// stream-json also shows session-level events (session.started).
				mine := e.TurnID == turnID || (streamAll && e.TurnID == "")
				if !mine || e.Seq <= lastSeq {
					continue
				}
				lastSeq = e.Seq
				r.render(e)
				switch d := e.Data.(type) {
				case event.MessageDeltaData:
					if d.ParentCallID == "" {
						reply.add(d.ItemID, d.Text, false)
					}
				case event.MessageCompletedData:
					if d.ParentCallID == "" {
						reply.add(d.ItemID, d.Text, true)
					}
				case permission.Request:
					r.answer(d, done)
				case event.TurnCompletedData:
					res.Status, res.Usage, res.Error, res.Budget = d.Status, d.Usage, d.Error, d.Budget
					res.Text = reply.String()
					res.DurationMs = time.Since(start).Milliseconds()
					res.RuntimeSessionID = r.info.RuntimeSessionID
					if info, err := r.b.Get(ctx, r.info.ID); err == nil {
						res.RuntimeSessionID = info.RuntimeSessionID
					}
					return res, nil
				}
			}
		}
	}
}

func (r *turnRunner) answer(req permission.Request, turnDone <-chan struct{}) {
	respond := func(o permission.Outcome, msg string) {
		if !req.Supports(o) {
			o = permission.Deny
		}
		if err := r.b.Respond(context.Background(), req.ID, permission.Decision{Outcome: o, Message: msg}); err != nil {
			fmt.Fprintln(os.Stderr, "  respond:", err)
		}
	}
	mode := r.sf.approve
	if mode == "ask" && !r.canAsk {
		mode = "deny"
		fmt.Fprintln(os.Stderr, "  (no terminal to ask; denying - use --approve allow to auto-approve)")
	}
	switch mode {
	case "allow":
		fmt.Fprintln(os.Stderr, "  auto-approved (--approve allow)")
		respond(permission.Allow, "")
	case "deny":
		respond(permission.Deny, "denied by harness-kit CLI (--approve)")
	default:
		if r.sf.out.format != "text" { // text mode already showed the request
			fmt.Fprintf(os.Stderr, "\n⚠ %s wants to: %s\n", req.Runtime, req.Title)
		}
		go func() {
			fmt.Fprint(os.Stderr, "  allow? [y]es / [a]lways this session / [n]o / [q]uit turn: ")
			line, ok := r.con.next(turnDone)
			if !ok {
				return // turn ended (e.g. cancelled) before an answer
			}
			outcome := map[string]permission.Outcome{"y": permission.Allow, "yes": permission.Allow, "a": permission.AllowSession, "q": permission.Abort}[strings.ToLower(line)]
			if outcome == "" {
				outcome = permission.Deny
			}
			respond(outcome, "")
		}()
	}
}

func (r *turnRunner) render(ev event.Event) {
	switch r.sf.out.format {
	case "stream-json":
		b, _ := json.Marshal(ev)
		fmt.Fprintln(r.textOut, string(b))
		return
	case "json":
		return // one result object is printed when the turn ends
	}
	switch d := ev.Data.(type) {
	case event.MessageDeltaData:
		fmt.Fprint(r.textOut, d.Text)
	case event.ToolStartedData:
		fmt.Fprintf(os.Stderr, "\n· %s\n", firstNonEmpty(d.Title, d.Name))
	case event.ToolCompletedData:
		if d.Status != event.ToolStatusCompleted {
			fmt.Fprintf(os.Stderr, "· %s %s\n", d.Status, d.Error)
		}
	case permission.Request:
		fmt.Fprintf(os.Stderr, "\n⚠ %s wants to: %s\n", d.Runtime, d.Title)
		if d.Reason != "" {
			fmt.Fprintf(os.Stderr, "  reason: %s\n", d.Reason)
		}
	case event.ApprovalResolvedData:
		if d.Source != permission.SourceHost {
			fmt.Fprintf(os.Stderr, "· approval %s (%s)\n", d.Decision.Outcome, d.Source)
		}
	case event.RuntimeStatusData:
		if d.Status != "ready" {
			fmt.Fprintf(os.Stderr, "\n[%s] %s\n", d.Status, d.Message)
		}
	case event.ErrorData:
		fmt.Fprintf(os.Stderr, "\n[error] %s\n", d.Message)
	case event.TurnCompletedData:
		fmt.Fprintln(r.textOut)
		line := string(d.Status)
		if d.Usage != nil {
			line += fmt.Sprintf(" · %d in / %d out tokens", d.Usage.InputTokens, d.Usage.OutputTokens)
			if d.Usage.CostUSD != nil {
				line += fmt.Sprintf(" · ~$%.4f", *d.Usage.CostUSD)
			}
		}
		if d.Error != nil {
			line += " · " + d.Error.Message
		}
		fmt.Fprintln(os.Stderr, "—", line)
	}
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func startSession(args []string, name string) (*turnRunner, []string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	sf := &sessionFlags{}
	sf.register(fs)
	pos, err := parseArgs(fs, args)
	if err != nil {
		return nil, nil, err
	}
	if err := sf.prepare(fs); err != nil {
		return nil, nil, err
	}
	ctx := context.Background()
	b, err := sf.open(ctx)
	if err != nil {
		return nil, nil, err
	}
	var info session.Info
	restoring := isSessionID(sf.resume)
	if restoring {
		info, err = b.Restore(ctx, sf.resume)
		if errors.Is(err, session.ErrNotFound) || isReason(err, protocol.ReasonSessionNotFound) {
			err = fmt.Errorf("no session %s in %s (see harness-kit sessions)", sf.resume, sf.store)
		}
		if err == nil && sf.sessionSettings != nil {
			_, err = b.ConfigureSettings(ctx, settings.ConfigureParams{Target: settings.Target{SessionID: info.ID}, Patch: sf.sessionSettings})
			if err == nil {
				info, err = b.Get(ctx, info.ID)
			}
		}
		if err == nil && sf.title != "" {
			info, err = b.Rename(ctx, info.ID, sf.title)
		}
		if err == nil {
			var cfg agent.ConfigPatch
			changed := false
			fs.Visit(func(f *flag.Flag) {
				switch f.Name {
				case "model":
					cfg.Model = &sf.model
					changed = true
				case "effort":
					cfg.Effort = &sf.effort
					changed = true
				}
			})
			if changed {
				info, err = b.Configure(ctx, info.ID, cfg)
			}
		}
	} else {
		var p protocol.SessionCreateParams
		if p, err = sf.createParams(); err == nil {
			info, err = b.Create(ctx, p)
		}
	}
	if err != nil {
		b.Close()
		return nil, nil, err
	}
	mode := "in-process"
	if sf.rpc {
		mode = "via JSON-RPC sidecar"
	}
	if sf.out.format == "text" {
		if restoring {
			fmt.Fprintf(os.Stderr, "restored %s: %q, %d turn(s) so far (%s %s, %s)\n", info.ID, info.Title, info.Turns, info.Runtime, info.RuntimeSessionID, mode)
		} else {
			fmt.Fprintf(os.Stderr, "%s session %s (%s)\n", info.Runtime, info.RuntimeSessionID, mode)
		}
	}
	// Startup notes (MCP warnings, skipped servers) are session events.
	if evs, err := b.Events(ctx, info.ID, 0); err == nil && sf.out.format == "text" {
		for _, ev := range evs {
			if d, ok := ev.Data.(event.RuntimeStatusData); ok && (d.Status == "warning" || d.Status == "mcp") {
				fmt.Fprintf(os.Stderr, "[%s] %s\n", d.Status, d.Message)
			}
		}
	}
	r := &turnRunner{b: b, sf: sf, con: &console{}, info: info, textOut: os.Stdout, sigs: make(chan os.Signal, 1), closeOut: func() {}}
	if sf.out.format == "stream-json" || (name == "chat" && sf.out.format == "json") {
		r.textOut, r.closeOut = jsonWriter() // a stream of JSON lines
	}
	signal.Notify(r.sigs, os.Interrupt)
	return r, pos, nil
}

func (r *turnRunner) finish() {
	defer r.closeOut()
	signal.Stop(r.sigs)
	if info, err := r.b.Get(context.Background(), r.info.ID); err == nil && info.Turns > 0 && r.sf.out.format == "text" {
		if r.sf.persistent() {
			fmt.Fprintf(os.Stderr, "resume with: harness-kit prompt -resume %s \"...\"\n", info.ID)
		} else if info.RuntimeSessionID != "" {
			fmt.Fprintf(os.Stderr, "resume with: harness-kit prompt -runtime %s -resume %s \"...\"\n", info.Runtime, info.RuntimeSessionID)
		}
	}
	r.b.Close()
}

func promptCmd(args []string) error {
	r, words, err := startSession(args, "prompt")
	if err != nil {
		return err
	}
	defer r.finish()

	text := strings.Join(words, " ")
	r.canAsk = stdinIsTerminal()
	if text == "" && len(r.sf.images) == 0 {
		if r.canAsk {
			return errors.New(`usage: harness-kit prompt [flags] "your prompt" (or pipe the prompt on stdin)`)
		}
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		text, r.canAsk = strings.TrimSpace(string(b)), false
	}
	res, err := r.run(agent.Text(text))
	if err != nil {
		return err
	}
	if r.sf.out.format == "json" {
		if err := printJSON(res); err != nil {
			return err
		}
	}
	switch res.Status {
	case event.TurnStatusFailed:
		return exitCode(1)
	case event.TurnStatusCancelled:
		return exitCode(130)
	}
	return nil
}

func chatCmd(args []string) error {
	r, words, err := startSession(args, "chat")
	if err != nil {
		return err
	}
	defer r.finish()
	r.canAsk = stdinIsTerminal()
	turn := func(text string) error {
		res, err := r.run(agent.Text(text))
		if err == nil && r.sf.out.format == "json" {
			b, _ := json.Marshal(res) // one line per turn
			fmt.Fprintln(r.textOut, string(b))
		}
		return err
	}
	if len(words) > 0 {
		if err := turn(strings.Join(words, " ")); err != nil {
			return err
		}
	}
	if r.sf.out.format == "text" {
		fmt.Fprintln(os.Stderr, "type a message, /configure JSON to change defaults, /quit to exit; Ctrl-C cancels a running turn")
	}
	for {
		fmt.Fprint(os.Stderr, "\n> ")
		quit := make(chan struct{})
		lineC := make(chan string, 1)
		okC := make(chan bool, 1)
		go func() {
			l, ok := r.con.next(quit)
			lineC <- l
			okC <- ok
		}()
		select {
		case <-r.sigs:
			close(quit)
			fmt.Fprintln(os.Stderr)
			return nil
		case line := <-lineC:
			if !<-okC || line == "/quit" || line == "/exit" {
				return nil
			}
			if line == "" {
				continue
			}
			if line == "/configure" || strings.HasPrefix(line, "/configure ") {
				patch, err := parseConfigPatch(strings.TrimSpace(strings.TrimPrefix(line, "/configure")))
				if err == nil {
					var info session.Info
					info, err = r.b.Configure(context.Background(), r.info.ID, patch)
					if err == nil {
						r.info = info
					}
				}
				if err != nil {
					fmt.Fprintln(os.Stderr, "error:", err)
				} else {
					fmt.Fprintln(os.Stderr, "session defaults updated")
				}
				continue
			}
			if err := turn(line); err != nil {
				fmt.Fprintln(os.Stderr, "error:", err)
			}
		}
	}
}

// parseConfigPatch requires an object so null cannot silently become a no-op.
func parseConfigPatch(raw string) (agent.ConfigPatch, error) {
	var patch agent.ConfigPatch
	if !strings.HasPrefix(strings.TrimSpace(raw), "{") {
		return patch, errors.New("configuration must be a JSON object")
	}
	if err := json.Unmarshal([]byte(raw), &patch); err != nil {
		return patch, fmt.Errorf("invalid configuration JSON: %w", err)
	}
	return patch, nil
}

// ---- sessions ------------------------------------------------------------

func isSessionID(s string) bool { return strings.HasPrefix(s, "ses_") }

func sessionsCmd(args []string) error {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("sessions "+sub, flag.ContinueOnError)
	var bf backendFlags
	bf.register(fs)
	var out outputFlags
	out.register(fs, "text, json")
	runtimeName := fs.String("runtime", "", "only this runtime (required with --native)")
	cwd := fs.String("cwd", "", "only sessions in this working directory")
	limit := fs.Int("limit", 20, "maximum number of sessions (0 = all)")
	native := fs.Bool("native", false, "list the runtime's own stored sessions, including ones started outside Harness Kit")
	query := fs.String("query", "", "case-insensitive title substring")
	transcript := fs.Bool("search-transcript", false, "also search saved event contents")
	tags := fs.String("tags", "", "comma-separated tags that must all match")
	archived := fs.String("archived", "all", "archive filter: all, true, false")
	forkTitle := fs.String("title", "", "title for the fork (defaults to parent title)")
	forkSettingsRaw := fs.String("session-settings", "", "shared settings JSON for the fork, including workspace and budget overrides")
	diffBytes := fs.Int64("max-bytes", 0, "maximum bytes returned by sessions diff (0 keeps the recorded limit)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if err := out.resolve("text", "json"); err != nil {
		return err
	}
	var forkSettings settings.Patch
	if *forkSettingsRaw != "" {
		if sub != "fork" {
			return errors.New("--session-settings is only valid with sessions fork")
		}
		if err := json.Unmarshal([]byte(*forkSettingsRaw), &forkSettings); err != nil {
			return fmt.Errorf("--session-settings: %w", err)
		}
	}
	if *cwd != "" {
		if *cwd, err = filepath.Abs(*cwd); err != nil {
			return err
		}
	}
	explicit := false
	fs.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "runtimes" })
	if !explicit && *runtimeName != "" {
		bf.runtimes = *runtimeName
	}
	if *native {
		bf.store = "memory"
	}
	ctx := context.Background()
	b, err := bf.open(ctx)
	if err != nil {
		return err
	}
	defer b.Close()

	needID := func() (string, error) {
		if len(pos) != 1 {
			return "", fmt.Errorf("usage: harness-kit sessions %s <session-id>", sub)
		}
		return pos[0], nil
	}
	switch sub {
	case "diff":
		if len(pos) != 2 {
			return errors.New("usage: harness-kit sessions diff <session-id> <turn-id> [--max-bytes N]")
		}
		diff, err := b.Diff(ctx, pos[0], pos[1], workspace.DiffOptions{MaxBytes: *diffBytes})
		if err != nil {
			return err
		}
		if out.format == "json" {
			return printJSON(diff)
		}
		fmt.Fprint(os.Stdout, diff.WorkingTree)
		if diff.Index != "" {
			fmt.Fprintln(os.Stdout, "\nStaged changes:")
			fmt.Fprint(os.Stdout, diff.Index)
		}
		if !diff.Complete {
			return fmt.Errorf("incomplete turn diff: %s", diff.Error)
		}
		if diff.Truncated {
			fmt.Fprintln(os.Stderr, "diff truncated by the configured size limit")
		}
		if diff.EncodingLossy {
			fmt.Fprintln(os.Stderr, "some file content could not be displayed as text")
		}
		if diff.WorkingTree == "" && diff.Index == "" {
			fmt.Fprintln(os.Stdout, "No file changes during this turn.")
		}
		return nil
	case "remove-workspace":
		id, err := needID()
		if err != nil {
			return err
		}
		if err := b.RemoveWorkspace(ctx, id); err != nil {
			return err
		}
		if out.format == "json" {
			return printJSON(map[string]bool{"ok": true})
		}
		fmt.Fprintln(os.Stdout, "Removed the session's clean, owned worktree.")
		return nil
	case "list":
		if *native {
			return listNativeSessions(ctx, b, *runtimeName, *cwd, *limit, out.format == "json")
		}
		if !bf.persistent() {
			return errors.New("no session store (--store memory); use --native to ask a runtime for its sessions")
		}
		var archiveFilter *bool
		switch *archived {
		case "all":
		case "true", "false":
			value := *archived == "true"
			archiveFilter = &value
		default:
			return errors.New("--archived must be all, true or false")
		}
		var tagFilter []string
		if *tags != "" {
			tagFilter = strings.Split(*tags, ",")
		}
		return listSessions(ctx, b, session.ListOptions{Runtime: *runtimeName, Cwd: *cwd, Limit: *limit, Query: *query, SearchTranscript: *transcript, Tags: tagFilter, Archived: archiveFilter}, out.format == "json")
	case "show":
		id, err := needID()
		if err != nil {
			return err
		}
		info, err := b.Get(ctx, id)
		if err != nil {
			return err
		}
		if out.format == "json" {
			return printJSON(info)
		}
		effort := ""
		if info.Config != nil {
			effort = info.Config.Effort
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		for _, kv := range [][2]string{
			{"session", info.ID}, {"runtime", info.Runtime}, {"runtime session", dash(info.RuntimeSessionID)},
			{"archived", fmt.Sprint(info.Archived)}, {"tags", strings.Join(session.Tags(info), ", ")}, {"forked from", dash(info.ForkedFrom)}, {"fork pending", fmt.Sprint(info.ForkPending)},
			{"title", dash(info.Title)}, {"cwd", dash(info.Cwd)}, {"model", dash(info.Model)}, {"effort", dash(effort)},
			{"turns", fmt.Sprint(info.Turns)}, {"last status", dash(string(info.LastStatus))}, {"state", string(info.State)},
			{"created", info.CreatedAt.Local().Format(time.DateTime)}, {"updated", info.UpdatedAt.Local().Format(time.DateTime)},
		} {
			fmt.Fprintf(tw, "%s:\t%s\n", kv[0], kv[1])
		}
		tw.Flush()
		fmt.Printf("\nresume with: harness-kit prompt -resume %s \"...\"\n", info.ID)
		return nil
	case "archive", "unarchive", "fork", "tags":
		if len(pos) == 0 || (sub != "tags" && len(pos) != 1) {
			return fmt.Errorf("usage: harness-kit sessions %s <session-id> [tags...]", sub)
		}
		var info session.Info
		switch sub {
		case "archive", "unarchive":
			info, err = b.Archive(ctx, pos[0], sub == "archive")
		case "fork":
			info, err = b.Fork(ctx, pos[0], session.ForkParams{Title: *forkTitle, Settings: forkSettings})
		case "tags":
			info, err = b.SetTags(ctx, pos[0], pos[1:])
		}
		if err != nil {
			return err
		}
		if out.format == "json" {
			return printJSON(info)
		}
		fmt.Fprintln(os.Stdout, info.ID)
		return nil
	case "rename", "mv":
		if len(pos) < 2 {
			return errors.New(`usage: harness-kit sessions rename <session-id> "new title"`)
		}
		info, err := b.Rename(ctx, pos[0], strings.Join(pos[1:], " "))
		if err != nil {
			return err
		}
		if out.format == "json" {
			return printJSON(info)
		}
		fmt.Fprintf(os.Stderr, "renamed %s to %q\n", info.ID, info.Title)
		return nil
	case "settings":
		if len(pos) < 1 || len(pos) > 2 {
			return errors.New(`usage: harness-kit sessions settings <session-id> ['{"budget":{"maxSessionTokens":10000}}']`)
		}
		target := settings.Target{SessionID: pos[0]}
		var snapshot settings.Snapshot
		if len(pos) == 1 {
			snapshot, err = b.Settings(ctx, target)
		} else {
			var patch settings.Patch
			if err = json.Unmarshal([]byte(pos[1]), &patch); err != nil {
				return err
			}
			snapshot, err = b.ConfigureSettings(ctx, settings.ConfigureParams{Target: target, Patch: patch})
		}
		if err != nil {
			return err
		}
		return printJSON(snapshot)
	case "configure":
		if len(pos) != 2 {
			return errors.New(`usage: harness-kit sessions configure <session-id> '{"model":"model-name","effort":"high"}'`)
		}
		patch, err := parseConfigPatch(pos[1])
		if err != nil {
			return err
		}
		info, err := b.Configure(ctx, pos[0], patch)
		if err != nil {
			return err
		}
		if out.format == "json" {
			return printJSON(info)
		}
		fmt.Fprintf(os.Stderr, "updated defaults for %s\n", info.ID)
		return nil
	case "delete", "rm":
		id, err := needID()
		if err != nil {
			return err
		}
		if err := b.Delete(ctx, id); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "deleted %s (the runtime's own transcript is untouched)\n", id)
		return nil
	}
	return fmt.Errorf("unknown command \"sessions %s\" (try list, show, rename, configure, fork, archive, unarchive, tags or delete)", sub)
}

func listSessions(ctx context.Context, b backend, options session.ListOptions, asJSON bool) error {
	list, err := b.Query(ctx, options)
	if err != nil {
		return err
	}
	if asJSON {
		if list == nil {
			list = []session.Info{}
		}
		return printJSON(list)
	}
	if len(list) == 0 {
		fmt.Fprintln(os.Stderr, "no sessions yet; start one with: harness-kit prompt -runtime codex \"...\"")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION\tRUNTIME\tUPDATED\tTURNS\tLAST\tCWD\tTITLE")
	for _, s := range list {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n", s.ID, s.Runtime, ago(s.UpdatedAt), s.Turns, dash(string(s.LastStatus)), shortPath(s.Cwd), clip(s.Title, 50))
	}
	tw.Flush()
	fmt.Fprintln(os.Stderr, "\ncontinue one with: harness-kit prompt -resume <SESSION> \"...\"")
	return nil
}

func listNativeSessions(ctx context.Context, b backend, runtimeName, cwd string, limit int, asJSON bool) error {
	if runtimeName == "" {
		return errors.New("--native needs -runtime (e.g. -runtime codex)")
	}
	list, err := b.RuntimeSessions(ctx, runtimeName, agent.ListSessionsOptions{Cwd: cwd, Limit: limit})
	if errors.Is(err, agent.ErrUnsupported) || isUnsupported(err) {
		return fmt.Errorf("%s does not offer a way to list its sessions outside its Python/TypeScript SDKs; "+
			"`harness-kit sessions` shows the %s sessions you ran through Harness Kit", runtimeName, runtimeName)
	}
	if err != nil {
		return err
	}
	if asJSON {
		if list == nil {
			list = []agent.RuntimeSession{}
		}
		return printJSON(list)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tUPDATED\tSOURCE\tCWD\tTITLE")
	for _, s := range list {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", s.ID, ago(s.UpdatedAt), dash(s.Source), shortPath(s.Cwd), clip(s.Title, 50))
	}
	tw.Flush()
	fmt.Fprintf(os.Stderr, "\ncontinue one with: harness-kit prompt -runtime %s -resume <ID> \"...\"\n", runtimeName)
	return nil
}

func isUnsupported(err error) bool { return isReason(err, protocol.ReasonUnsupported) }

// isReason reports whether err is a sidecar error with the given reason.
func isReason(err error, reason string) bool {
	var ce *client.Error
	return errors.As(err, &ce) && ce.Reason == reason
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
	return t.Local().Format("2006-01-02")
}

func shortPath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && p != "" {
		if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.Join("~", rel)
		}
	}
	return dash(p)
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return dash(s)
}
