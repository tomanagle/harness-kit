package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tomanagle/harness-kit/agent"
	"github.com/tomanagle/harness-kit/budget"
	"github.com/tomanagle/harness-kit/event"
	"github.com/tomanagle/harness-kit/mcp"
	"github.com/tomanagle/harness-kit/permission"
	"github.com/tomanagle/harness-kit/settings"
	"github.com/tomanagle/harness-kit/workspace"
)

// Errors returned by the Manager. They can be matched with errors.Is.
var (
	ErrRuntimeNotFound = errors.New("session: runtime not registered")
	ErrBusy            = errors.New("session: a turn is already running")
	ErrClosed          = errors.New("session: closed")
	ErrDuplicateTurn   = errors.New("session: turn id already used")
	ErrUnsafe          = errors.New("session: configuration disables runtime safety checks")
	ErrNotResumable    = errors.New("session: nothing to resume (the runtime never started this session)")
	ErrInvalidTitle    = errors.New("session: invalid title")
)

// MaxTitleLength is the longest title, in characters, that Rename and
// CreateParams.Title accept.
const MaxTitleLength = 200

// Options configure a Manager. The zero value is usable.
type Options struct {
	// Workspace supplies optional native worktrees and turn snapshot support.
	// Nil leaves the portable manager usable without Git/process dependencies.
	Workspace workspace.Backend
	// LogLevel controls the host logger's live level. Supply the same LevelVar
	// used by the logger handler to enable logging.level updates.
	LogLevel *slog.LevelVar
	// ValidateConfig lets the host apply its own constraints at creation,
	// restore, session updates and turn admission (including resolved settings).
	// It must be safe for concurrent calls and must not call back into Manager.
	ValidateConfig func(context.Context, ConfigRequest) error
	// PrepareRestore explicitly chooses whether/how to reload external settings
	// on restore. Nil preserves saved configuration; Prepare remains creation-only.
	PrepareRestore func(context.Context, *CreateParams) ([]string, error)

	// Store persists session records. Defaults to a MemoryStore.
	Store Store
	// Policy decides approval requests before the host is asked.
	Policy permission.Policy
	// ApprovalTimeout bounds how long an approval may wait for the host.
	// When it elapses the request is denied. Zero waits indefinitely.
	ApprovalTimeout time.Duration
	// EventLogSize is the number of events retained per session for
	// replay in memory. An EventStore retains the full history on disk.
	// Defaults to 2048.
	EventLogSize int
	// MaxClosedSessions bounds how many closed sessions keep their event
	// logs in memory. Defaults to 64. Records remain in the Store.
	MaxClosedSessions int
	// StreamBuffer is the subscriber queue length used by Stream. Defaults
	// to event.DefaultBuffer. Slow consumers beyond it are recovered from
	// the session log.
	StreamBuffer int
	// Prepare, if set, can adjust a new session's configuration before the
	// runtime starts (for example to merge MCP servers from config files).
	// Returned warnings are recorded as runtime.status "warning" events.
	// It is not called by Restore, which reuses the stored configuration.
	Prepare func(ctx context.Context, p *CreateParams) (warnings []string, err error)
	// ToolServer, if set, serves host tools (and Claude Code's permission
	// prompts) to sessions whose runtime supports MCP. See package toolhost.
	ToolServer ToolServer
	// AllowUnsafe permits session configurations that a Guarded runtime
	// reports as disabling its sandbox or approvals.
	AllowUnsafe bool
	// Logger receives diagnostics. Defaults to discarding.
	Logger *slog.Logger
}

// ToolServer gives runtime sessions access to host tools by serving them as
// an MCP server (package toolhost implements it).
type ToolServer interface {
	// Attach binds a session and returns the MCP server the runtime should
	// launch to reach it. An Attachment with an empty Name means there is
	// nothing to serve.
	Attach(ctx context.Context, b ToolBinding) (Attachment, error)
}

// ToolBinding describes a session to a ToolServer.
type ToolBinding struct {
	SessionID string
	Runtime   string
	// Tools limits the session to these host tools; nil means all.
	Tools []string
	// ApprovalPrompt also serves a permission-prompt tool.
	ApprovalPrompt bool
	// Approver asks the host (policy, then approval.requested).
	Approver func(ctx context.Context, req permission.Request) (permission.Decision, error)
	// CurrentTurn returns the active turn ID ("" between turns).
	CurrentTurn func() string
	// CancelTurn interrupts the active turn (for "abort" decisions).
	CancelTurn func()
}

// Attachment is the result of ToolServer.Attach.
type Attachment struct {
	Name   string
	Server mcp.Server
	// PermissionTool names the tool in Server that answers permission
	// prompts, when the binding asked for one.
	PermissionTool string
	Detach         func()
}

// Manager owns sessions across all registered runtimes. It is safe for
// concurrent use.
type Manager struct {
	reg    *agent.Registry
	opts   Options
	store  Store
	broker *permission.Broker
	bus    *event.Bus
	log    *slog.Logger

	baseCtx    context.Context
	baseCancel context.CancelFunc

	// lifecycleMu serialises restore, rename and delete, including dormant sessions.
	lifecycleMu sync.Mutex
	mu          sync.Mutex
	sessions    map[string]*managed
	closedOrder []*managed
	turns       sync.WaitGroup

	settingsMu          sync.RWMutex
	settingsChangeMu    sync.Mutex
	settings            settings.Values
	initialSettings     settings.Values
	settingsRevision    uint64
	settingsSubscribers map[chan settings.Snapshot]struct{}
	settingsInitialized bool
	resourceWake        chan struct{}
	resourceDone        chan struct{}
}

// NewManager returns a Manager for the runtimes in reg.
func NewManager(reg *agent.Registry, opts Options) *Manager {
	if opts.Store == nil {
		opts.Store = NewMemoryStore()
	}
	if opts.EventLogSize <= 0 {
		opts.EventLogSize = 2048
	}
	if opts.MaxClosedSessions <= 0 {
		opts.MaxClosedSessions = 64
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Manager{
		reg:                 reg,
		opts:                opts,
		store:               opts.Store,
		broker:              permission.NewBroker(),
		bus:                 event.NewBus(),
		log:                 opts.Logger,
		baseCtx:             ctx,
		baseCancel:          cancel,
		sessions:            make(map[string]*managed),
		settingsSubscribers: make(map[chan settings.Snapshot]struct{}),
		resourceWake:        make(chan struct{}, 1),
		resourceDone:        make(chan struct{}),
	}
	m.initSettings()
	go m.resourceLoop()
	return m
}

// Registry returns the manager's runtime registry.
func (m *Manager) Registry() *agent.Registry { return m.reg }

// CreateParams configure a new session.
type CreateParams struct {
	workspace workspace.Settings
	// Settings uses the unified settings envelope: session values and
	// memory.pinned may be supplied at creation. Global resource policies
	// belong to ConfigureSettings.
	Settings settings.Patch
	pinned   bool
	budget   budget.Limits
	Runtime  string
	Config   agent.SessionConfig
	Metadata map[string]string
	// Title names the session. Empty means the start of the first prompt.
	Title string
}

// Create starts a session. ctx bounds runtime startup only; the session
// outlives it.
func (m *Manager) Create(ctx context.Context, p CreateParams) (Info, error) {
	if m.baseCtx.Err() != nil {
		return Info{}, fmt.Errorf("%w: manager is shut down", ErrClosed)
	}
	var err error
	p, err = m.createSettings(p)
	if err != nil {
		return Info{}, err
	}
	var warnings []string
	if m.opts.Prepare != nil {
		w, err := m.opts.Prepare(ctx, &p)
		if err != nil {
			return Info{}, err
		}
		warnings = w
	}
	name, err := cleanTitle(p.Title, true)
	if err != nil {
		return Info{}, err
	}
	var cloneErr error
	p.Config, cloneErr = p.Config.Clone()
	if cloneErr != nil {
		return Info{}, cloneErr
	}
	if p.Config.MCP != nil {
		if err := p.Config.MCP.Validate(); err != nil {
			return Info{}, err
		}
		if rt, ok := m.reg.Get(p.Runtime); ok && !rt.Info().Capabilities.MCP && len(p.Config.MCP.Servers) > 0 {
			warnings = append(warnings, fmt.Sprintf("runtime %s does not support MCP servers; %d ignored", p.Runtime, len(p.Config.MCP.Servers)))
		}
	}
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	if m.baseCtx.Err() != nil {
		return Info{}, ErrClosed
	}
	now := time.Now().UTC()
	return m.start(ctx, Info{
		WorkspaceSettings: p.workspace,
		ID:                NewID("ses_"),
		Budget:            p.budget.Clone(),
		Pinned:            p.pinned,
		Runtime:           p.Runtime,
		Cwd:               p.Config.Cwd,
		Model:             p.Config.Model,
		Metadata:          maps.Clone(p.Metadata),
		Title:             name,
		CreatedAt:         now,
	}, p.Config, warnings...)
}

// Rename sets a session's title. Titles are one line: runs of whitespace
// become single spaces. An empty title or one longer than MaxTitleLength
// returns ErrInvalidTitle. Any session in the store can be renamed, live
// or not, and a title set this way is never replaced by the automatic one.
// Renaming does not change UpdatedAt, which tracks activity.
func (m *Manager) Rename(ctx context.Context, id, newTitle string) (Info, error) {
	name, err := cleanTitle(newTitle, false)
	if err != nil {
		return Info{}, err
	}
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	if ms, ok := m.lookup(id); ok {
		return ms.rename(name)
	}
	rec, err := m.store.Get(ctx, id)
	if err != nil {
		return Info{}, err
	}
	rec.Info, err = m.storedInfo(ctx, rec.Info)
	if err != nil {
		return Info{}, err
	}
	// A dormant session can publish and persist an update without launching
	// a runtime or entering the live-session cache.
	ms, err := m.newManaged(ctx, rec.Info, rec.Config, true)
	if err != nil {
		return Info{}, err
	}
	return ms.rename(name)
}

// Restore brings a stored session back to life under the same Harness Kit
// ID, resuming the runtime-native session with its saved configuration.
// Persisted events remain readable and sequence numbers never restart.
func (m *Manager) Restore(ctx context.Context, id string) (Info, error) {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	return m.restoreLocked(ctx, id)
}

func (m *Manager) restoreLocked(ctx context.Context, id string) (Info, error) {
	if m.baseCtx.Err() != nil {
		return Info{}, ErrClosed
	}
	if ms, ok := m.lookup(id); ok {
		ms.emitMu.Lock()
		unsaved := ms.eventWriteFailed
		ms.emitMu.Unlock()
		if unsaved {
			return Info{}, fmt.Errorf("session: cannot restore while events are unsaved")
		}
		ms.mu.Lock()
		live := !ms.info.State.Terminal() && ms.info.State != StateClosing && ms.info.State != StateSuspended && ms.info.State != StateSuspending
		ms.mu.Unlock()
		if live {
			return ms.snapshot(), nil
		}
		// Failed sessions may still be releasing their runtime asynchronously.
		if err := ms.closeRuntime(ctx); err != nil {
			return Info{}, err
		}
	}
	rec, err := m.store.Get(ctx, id)
	if err != nil {
		return Info{}, err
	}
	rec.Info, err = m.storedInfo(ctx, rec.Info)
	if err != nil {
		return Info{}, err
	}
	if (rec.Info.RuntimeSessionID == "" || rec.Info.Turns == 0) && rec.Info.State != StateSuspended && !rec.Info.ForkPending {
		return Info{}, fmt.Errorf("%w: %s", ErrNotResumable, id)
	}
	cfg := rec.Config
	if rec.Info.Config != nil {
		cfg = cfg.WithSettings(*rec.Info.Config)
	}
	cfg.NativeDefaults = rec.NativeDefaults
	if rec.Info.ForkPending {
		cfg.ForkFrom = rec.ForkSourceRuntimeID
	}
	cfg.ResumeID = rec.Info.RuntimeSessionID
	if rec.Info.Turns == 0 {
		cfg.ResumeID = ""
	}
	info := rec.Info
	info.ActiveTurnID, info.Error = "", nil
	var warnings []string
	if m.opts.PrepareRestore != nil {
		params := CreateParams{Runtime: info.Runtime, Config: cfg, Metadata: maps.Clone(info.Metadata), Title: info.Title}
		warnings, err = m.opts.PrepareRestore(ctx, &params)
		if err != nil {
			return Info{}, err
		}
		if params.Runtime != info.Runtime || params.Config.ResumeID != cfg.ResumeID {
			return Info{}, fmt.Errorf("%w: restore hook cannot change runtime or resume ID", agent.ErrInvalidConfig)
		}
		cfg = params.Config
		info.Metadata = maps.Clone(params.Metadata)
		info.Title, err = cleanTitle(params.Title, true)
		if err != nil {
			return Info{}, err
		}
	}
	return m.start(ctx, info, cfg, warnings...)
}

// newManaged reconstructs only the bounded event tail. Durable stores can
// check duplicate turn IDs on disk, so their complete turn index stays cold.
func (m *Manager) newManaged(ctx context.Context, info Info, cfg agent.SessionConfig, existing bool) (*managed, error) {
	policy := m.memoryPolicy()
	if info.SettingsRevision == 0 {
		info.SettingsRevision = 1
	}
	log := event.NewLogWithHistory(policy.EventLogSize, nil, info.LastSeq)
	log.Configure(policy.EventLogSize, policy.MaxSessionCacheBytes)
	turnIDs := make(map[string]struct{})
	if info.Config != nil {
		cfg = cfg.WithSettings(*info.Config)
	}
	cfg, err := cfg.Clone()
	if err != nil {
		return nil, err
	}
	initial, err := cfg.Clone()
	if err != nil {
		return nil, err
	}
	ledger := budget.New(cfg.ResumeID != "")
	initialBudget := info.Budget.Clone()
	initialWorkspace := workspacePolicy(info.WorkspaceSettings)
	var accountingAfter uint64
	_, hasLookup := m.store.(TurnLookup)
	visit := func(ev event.Event) error {
		if ev.Seq > accountingAfter {
			replayBudgetEvent(&ledger, ev)
		}
		log.Restore(ev)
		if !hasLookup && ev.TurnID != "" {
			turnIDs[ev.TurnID] = struct{}{}
		}
		return nil
	}
	if existing {
		rec, err := m.store.Get(ctx, info.ID)
		if err != nil {
			return nil, err
		}
		if rec.Accounting != nil {
			ledger = *rec.Accounting
			accountingAfter = rec.Info.LastSeq
		}
		if rec.InitialBudget != nil {
			initialBudget = rec.InitialBudget.Clone()
		}
		if rec.InitialWorkspace != nil {
			initialWorkspace = *rec.InitialWorkspace
		}
		if rec.InitialConfig != nil {
			initial, err = rec.InitialConfig.Clone()
			if err != nil {
				return nil, err
			}
		}
		if cfg.NativeDefaults == nil {
			cfg.NativeDefaults = rec.NativeDefaults
		}
		if info.ForkPending {
			cfg.ForkFrom = rec.ForkSourceRuntimeID
		}
		if scanner, ok := m.store.(EventScanner); ok {
			if err := scanner.ScanEvents(ctx, info.ID, visit); err != nil {
				return nil, err
			}
		} else if store, ok := m.store.(EventStore); ok {
			// Compatibility for stores implementing the original interface: page by
			// event count so restore never requests all history in one allocation.
			var after uint64
			for {
				page, _, err := store.Events(ctx, info.ID, after, 128)
				if err != nil {
					return nil, err
				}
				if len(page) == 0 {
					break
				}
				for _, ev := range page {
					if ev.Seq <= after {
						continue
					}
					if err := visit(ev); err != nil {
						return nil, err
					}
				}
				next := page[len(page)-1].Seq
				if next <= after {
					return nil, fmt.Errorf("session: event store made no progress")
				}
				after = next
			}
		} else if old, ok := m.lookup(info.ID); ok {
			old.emitMu.Lock()
			history, _ := old.log.Since(0, 0)
			for _, ev := range history {
				_ = visit(ev)
			}
			old.mu.Lock()
			maps.Copy(turnIDs, old.turnIDs)
			old.mu.Unlock()
			old.emitMu.Unlock()
		}
	}
	selection, err := cfg.Settings()
	if err != nil {
		return nil, err
	}
	info.Config = &selection
	info.Model = selection.Model
	ledger.Interrupt()
	info.Usage = ledger.Usage()
	return &managed{m: m, cfg: cfg, initialCfg: initial, initialWorkspace: initialWorkspace, initialBudget: initialBudget, accounting: ledger, info: info, turnIDs: turnIDs, log: log, lastAccess: time.Now()}, nil
}

// Warnings returns the warning messages recorded for a session (from
// Options.Prepare and from the runtime adapter) that are still in its
// event log.
func (m *Manager) Warnings(sessionID string) []string {
	evs, _, err := m.Events(sessionID, 0, 0)
	if err != nil {
		return nil
	}
	var out []string
	for _, ev := range evs {
		if d, ok := ev.Data.(event.RuntimeStatusData); ok && d.Status == "warning" {
			out = append(out, d.Message)
		}
	}
	return out
}

// start registers a session, starts its runtime and announces it.
func (m *Manager) start(ctx context.Context, info Info, cfg agent.SessionConfig, warnings ...string) (result Info, startErr error) {
	rt, ok := m.reg.Get(info.Runtime)
	if !ok {
		return Info{}, fmt.Errorf("%w: %q", ErrRuntimeNotFound, info.Runtime)
	}
	cfg, err := cfg.Clone()
	if err != nil {
		return Info{}, err
	}
	op := ConfigCreate
	if info.State != "" {
		op = ConfigRestore
	}
	if cfg.MCP != nil {
		if err := cfg.MCP.Validate(); err != nil {
			return Info{}, err
		}
	}
	if err := validateBudgetPolicy(rt, info.Budget); err != nil {
		return Info{}, err
	}
	if err := m.validateConfig(ctx, rt, info.ID, op, cfg); err != nil {
		return Info{}, err
	}
	newWorkspace := info.Workspace == nil
	if err := m.prepareWorkspace(ctx, &info, &cfg); err != nil {
		return Info{}, err
	}
	if newWorkspace && info.Workspace != nil && info.Workspace.Owned {
		defer func() {
			if startErr != nil {
				result, startErr = m.retainFailedWorkspace(info, cfg, startErr)
			}
		}()
	}
	if err := m.validateConfig(ctx, rt, info.ID, op, cfg); err != nil {
		return Info{}, err
	}
	selected, err := cfg.Settings()
	if err != nil {
		return Info{}, err
	}
	info.Config = &selected
	info.Cwd = cfg.Cwd
	ms, err := m.newManaged(ctx, info, cfg, info.State != "")
	if err != nil {
		return Info{}, err
	}
	ms.rt = rt
	ms.info.State = StateStarting
	ms.info.UpdatedAt = time.Now().UTC()
	var previous *Record
	if info.State != "" {
		rec, err := m.store.Get(ctx, info.ID)
		if err != nil {
			return Info{}, err
		}
		previous = &rec
	}
	// Startup callbacks can emit events, so the record must exist first.
	if err := ms.persist(); err != nil {
		return Info{}, err
	}
	m.mu.Lock()
	replaced := m.sessions[info.ID]
	m.sessions[info.ID] = ms
	m.mu.Unlock()
	if replaced != nil {
		// The new instance owns the saved state (including its pin). Retaining
		// the retired object would retain another cache on every restore.
		m.forgetManaged(replaced)
	}

	// The runtime gets runCfg; the stored record keeps cfg, so bridge
	// details (socket, token) are never persisted.
	runCfg, err := ms.cfg.Clone()
	if err != nil {
		return Info{}, err
	}
	runCfg, warn := m.attachTools(ctx, ms, rt, runCfg)
	if warn != "" {
		warnings = append(warnings, warn)
	}
	var sess agent.Session
	if cfg.ForkFrom != "" {
		forker, ok := rt.(agent.Forker)
		if !ok {
			err = fmt.Errorf("%w: %s cannot fork", agent.ErrUnsupported, info.Runtime)
		} else {
			sess, err = forker.Fork(ctx, cfg.ForkFrom, runCfg, ms)
		}
	} else {
		sess, err = rt.Start(ctx, runCfg, ms)
	}
	if err == nil && cfg.ForkFrom != "" && sess.RuntimeSessionID() == cfg.ForkFrom {
		_ = sess.Close(context.Background())
		err = errors.New("runtime fork returned the source conversation ID")
	}
	if err != nil {
		if ms.detach != nil {
			ms.detach()
		}
		m.mu.Lock()
		if m.sessions[info.ID] == ms {
			delete(m.sessions, info.ID)
		}
		m.mu.Unlock()
		if info.State == "" {
			_ = m.store.Delete(context.Background(), info.ID)
		} else if previous != nil {
			previous.Info.LastSeq = ms.log.LastSeq()
			_ = m.store.Put(context.Background(), *previous)
		}
		return Info{}, fmt.Errorf("session: start %s: %w", info.Runtime, err)
	}

	ms.mu.Lock()
	ms.sess = sess
	ms.info.RuntimeSessionID = sess.RuntimeSessionID()
	if cfg.ForkFrom != "" && ms.info.RuntimeSessionID != "" {
		ms.cfg.ForkFrom = ""
		ms.info.ForkPending = false
	}
	ms.info.State = StateIdle
	ms.mu.Unlock()
	ms.emitMu.Lock()
	ms.persist()
	ms.emitLocked(event.Event{Type: event.SessionStarted, Data: event.SessionStartedData{
		Runtime:          info.Runtime,
		RuntimeSessionID: sess.RuntimeSessionID(),
		Resumed:          cfg.ResumeID != "",
		Model:            cfg.Model,
	}})
	ms.updatedLocked("")
	ms.persist()
	ms.emitMu.Unlock()
	for _, w := range warnings {
		ms.emit(event.Event{Type: event.RuntimeStatus, Data: event.RuntimeStatusData{Status: "warning", Message: w}})
	}
	m.wakeResources()
	return ms.snapshot(), nil
}

// attachTools binds the session to the ToolServer, if any, and returns the
// configuration to start the runtime with.
func (m *Manager) attachTools(ctx context.Context, ms *managed, rt agent.Runtime, cfg agent.SessionConfig) (agent.SessionConfig, string) {
	ts := m.opts.ToolServer
	if ts == nil || cfg.DisableHostTools || !rt.Info().Capabilities.MCP {
		return cfg, ""
	}
	prompt := false
	if pu, ok := rt.(agent.PermissionToolUser); ok {
		prompt = pu.UsesPermissionTool(cfg)
	}
	att, err := ts.Attach(ctx, ToolBinding{
		SessionID: ms.info.ID, Runtime: ms.info.Runtime, Tools: cfg.HostTools, ApprovalPrompt: prompt,
		Approver: ms.RequestApproval, CurrentTurn: ms.currentTurnID, CancelTurn: ms.cancelTurn,
	})
	if err != nil {
		return cfg, "host tools are unavailable for this session: " + err.Error()
	}
	if att.Name == "" {
		return cfg, ""
	}
	ms.detach = att.Detach
	run := cfg
	mc := mcp.Config{Servers: map[string]mcp.Server{}}
	if cfg.MCP != nil {
		mc.InheritNative = cfg.MCP.InheritNative
		maps.Copy(mc.Servers, cfg.MCP.Servers)
	}
	mc.Servers[att.Name] = att.Server
	run.MCP = &mc
	if att.PermissionTool != "" {
		run.PermissionTool = att.Name + "/" + att.PermissionTool
	}
	return run, ""
}

// Delete closes the session if it is live and removes its record from the
// store, including its event log. It never deletes the runtime's own transcript.
func (m *Manager) Delete(ctx context.Context, id string) error {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	if ms, ok := m.lookup(id); ok {
		if err := ms.close(ctx, "closed"); err != nil {
			return err
		}
		m.mu.Lock()
		if m.sessions[id] == ms {
			delete(m.sessions, id)
		}
		m.mu.Unlock()
	} else if _, err := m.store.Get(ctx, id); err != nil {
		return err
	}
	return m.store.Delete(ctx, id)
}

// Get returns a session's current info.
func (m *Manager) Get(ctx context.Context, id string) (Info, error) {
	if ms, ok := m.lookup(id); ok {
		return ms.snapshot(), nil
	}
	rec, err := m.store.Get(ctx, id)
	if err != nil {
		return Info{}, err
	}
	return m.storedInfo(ctx, rec.Info)
}

// dormantInfo distinguishes stored history from runtime instances in this manager.
func dormantInfo(info Info) Info {
	if info.SettingsRevision == 0 {
		info.SettingsRevision = 1
	}
	if !info.State.Terminal() && info.State != StateSuspended {
		info.State = StateClosed
	}
	info.ActiveTurnID = ""
	return info
}

// storedInfo reconciles updates synced just before a process stopped, before
// the corresponding record replacement. Old snapshots never override newer records.
func (m *Manager) storedInfo(ctx context.Context, info Info) (Info, error) {
	rec, err := m.store.Get(ctx, info.ID)
	if err != nil {
		return Info{}, err
	}
	ledger := budget.New(rec.Config.ResumeID != "")
	var ledgerAfter uint64
	if rec.Accounting != nil {
		ledger = *rec.Accounting
		ledgerAfter = rec.Info.LastSeq
	}
	after := info.LastSeq
	apply := func(ev event.Event) error {
		if ev.Seq > ledgerAfter {
			replayBudgetEvent(&ledger, ev)
		}
		if ev.Seq <= after {
			return nil
		}
		if d, ok := ev.Data.(event.SessionUpdatedData); ok {
			info.Title, info.State, info.ActiveTurnID = d.Title, State(d.State), d.ActiveTurnID
			info.RuntimeSessionID, info.Turns, info.LastStatus = d.RuntimeSessionID, d.Turns, d.LastStatus
			info.UpdatedAt, info.Error, info.Pinned = d.UpdatedAt, d.Error, d.Pinned
			if d.Budget != nil {
				info.Budget = d.Budget.Clone()
			}
			if d.Usage != nil {
				info.Usage = d.Usage.Clone()
			}
			info.Archived, info.ForkPending, info.Metadata = d.Archived, d.ForkPending, maps.Clone(d.Metadata)
			if d.SettingsRevision > 0 {
				info.SettingsRevision = max(info.SettingsRevision, d.SettingsRevision)
			}
			if d.Config != nil {
				cfg := d.Config.Clone()
				info.Config = &cfg
				info.Model = cfg.Model
			}
		}
		info.LastSeq = max(info.LastSeq, ev.Seq)
		return nil
	}
	if scanner, ok := m.store.(EventScanner); ok {
		if err := scanner.ScanEvents(ctx, info.ID, apply); err != nil {
			return Info{}, err
		}
	} else if store, ok := m.store.(EventStore); ok {
		cursor := min(after, ledgerAfter)
		for {
			evs, _, err := store.Events(ctx, info.ID, cursor, 128)
			if err != nil {
				return Info{}, err
			}
			if len(evs) == 0 {
				break
			}
			for _, ev := range evs {
				_ = apply(ev)
			}
			next := evs[len(evs)-1].Seq
			if next <= cursor {
				return Info{}, fmt.Errorf("session: event store made no progress")
			}
			cursor = next
		}
	}
	ledger.Interrupt()
	info.Usage = ledger.Usage()
	return dormantInfo(info), nil
}

// List returns all sessions known to the store, with live state for
// sessions held in memory.
func (m *Manager) List(ctx context.Context) ([]Info, error) {
	recs, err := m.store.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Info, 0, len(recs))
	for _, r := range recs {
		if ms, ok := m.lookup(r.Info.ID); ok {
			out = append(out, ms.snapshot())
		} else {
			out = append(out, dormantInfo(r.Info))
		}
	}
	return out, nil
}

// PromptParams describe one turn.
type PromptParams struct {
	// Settings applies the unified session selection for this prompt only.
	// It cannot be supplied together with the legacy Config field.
	Settings settings.Patch
	// TurnID lets the caller choose the turn ID (it must be unique within
	// the session). Empty generates one.
	TurnID string
	Input  []agent.InputPart
	// Config changes only this turn. Omitted values inherit session defaults.
	Config *agent.ConfigPatch
}

// Turn is a handle on a running turn.
type Turn struct {
	workspace workspace.Settings
	ID        string
	SessionID string

	cancel     context.CancelFunc
	done       chan struct{}
	result     agent.TurnResult
	budget     turnBudget
	budgetStop *budget.Exceeded
}

// Done is closed when the turn has finished and turn.completed was emitted.
func (t *Turn) Done() <-chan struct{} { return t.done }

// Result returns the turn's result. Only valid after Done is closed.
func (t *Turn) Result() agent.TurnResult { return t.result }

// Wait blocks until the turn finishes or ctx ends.
func (t *Turn) Wait(ctx context.Context) (agent.TurnResult, error) {
	select {
	case <-t.done:
		return t.result, nil
	case <-ctx.Done():
		return agent.TurnResult{}, ctx.Err()
	}
}

// Prompt starts a turn and returns immediately. Its events, starting with
// turn.started and ending with turn.completed, are emitted to subscribers.
// The turn is not bound to ctx; use Cancel to stop it.
func (m *Manager) Prompt(requestCtx context.Context, sessionID string, p PromptParams) (*Turn, error) {
	if p.Settings != nil {
		if p.Config != nil {
			return nil, fmt.Errorf("%w: settings and config cannot both be supplied", agent.ErrInvalidConfig)
		}
		patch, err := promptSettings(p.Settings)
		if err != nil {
			return nil, err
		}
		p.Config = &patch
	}
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	ms, ok := m.lookup(sessionID)
	if m.memoryPolicy().ResumeOnPrompt {
		info, err := m.Get(requestCtx, sessionID)
		if err != nil {
			return nil, err
		}
		if info.State == StateSuspended {
			if _, err := m.restoreLocked(requestCtx, sessionID); err != nil {
				return nil, err
			}
			ms, ok = m.lookup(sessionID)
		}
	}
	if !ok {
		return nil, ErrNotFound
	}
	if err := m.checkForkAdmission(requestCtx, ms); err != nil {
		return nil, err
	}
	ms.emitMu.Lock()
	defer ms.emitMu.Unlock()
	ms.mu.Lock()
	switch {
	case ms.info.State.Terminal() || ms.info.State == StateClosing || ms.info.State == StateSuspended:
		ms.mu.Unlock()
		return nil, ErrClosed
	case ms.info.State != StateIdle:
		ms.mu.Unlock()
		return nil, ErrBusy
	}
	ms.mu.Unlock()
	budgetSelection, budgetPolicy, err := ms.prepareBudget(p.Settings)
	if err != nil {
		return nil, err
	}
	workspaceSettings, err := ms.turnWorkspace(p.Settings)
	if err != nil {
		return nil, err
	}
	settings, err := ms.prepareTurn(requestCtx, p.Config)
	if err != nil {
		return nil, err
	}
	input := slices.Clone(p.Input)
	if p.TurnID != "" {
		if lookup, ok := m.store.(TurnLookup); ok {
			used, err := lookup.HasTurn(requestCtx, sessionID, p.TurnID)
			if err != nil {
				return nil, err
			}
			if used {
				return nil, fmt.Errorf("%w: %q", ErrDuplicateTurn, p.TurnID)
			}
		}
	}
	ms.mu.Lock()
	turnID := p.TurnID
	if turnID == "" {
		turnID = NewID("turn_")
	}
	if _, dup := ms.turnIDs[turnID]; dup {
		ms.mu.Unlock()
		return nil, fmt.Errorf("%w: %q", ErrDuplicateTurn, turnID)
	}
	// Keep the current ID until its completion is durable. Historical IDs
	// are checked on disk rather than retained for the lifetime of the process.
	ms.turnIDs[turnID] = struct{}{}
	ctx, cancel := context.WithCancel(m.baseCtx)
	t := &Turn{ID: turnID, SessionID: sessionID, cancel: cancel, done: make(chan struct{}), budget: budgetSelection, workspace: workspaceSettings}
	ms.turn = t
	ms.accounting.Begin()
	ms.info.State = StateRunning
	ms.info.ActiveTurnID = turnID
	ms.info.Turns++
	if ms.info.Title == "" {
		ms.info.Title = title(agent.JoinText(input))
	}
	ms.info.UpdatedAt = time.Now().UTC()
	ms.mu.Unlock()

	snapshot := settings.Clone()
	ms.emitLocked(event.Event{Type: event.TurnStarted, TurnID: turnID, Data: event.TurnStartedData{Workspace: &workspaceSettings, Budget: &budgetPolicy, Text: agent.JoinText(input), Input: slices.Clone(input), Config: &snapshot}})
	ms.updatedLocked(turnID)
	ms.persist()
	m.turns.Add(1)
	go ms.runTurn(ctx, t, agent.Turn{ID: t.ID, Input: input, Config: &settings})
	return t, nil
}

// Stream starts a turn and yields its events in order, ending after
// turn.completed. Events dropped by a slow consumer are recovered from the
// session log transparently. Cancelling ctx cancels the turn (and the
// iterator keeps yielding until turn.completed); breaking out of the loop
// early stops observation but leaves the turn running.
func (m *Manager) Stream(ctx context.Context, sessionID string, p PromptParams) iter.Seq2[event.Event, error] {
	return func(yield func(event.Event, error) bool) {
		if p.TurnID == "" {
			p.TurnID = NewID("turn_")
		}
		sub := m.Subscribe(event.ForSession(sessionID), 0)
		defer sub.Close()
		t, err := m.Prompt(ctx, sessionID, p)
		if err != nil {
			yield(event.Event{}, err)
			return
		}
		stop := context.AfterFunc(ctx, func() { t.cancel() })
		defer stop()
		var lastSeq uint64
		deliver := func(ev event.Event) (cont, finished bool) {
			if ev.Seq <= lastSeq || ev.TurnID != t.ID {
				return true, false
			}
			lastSeq = ev.Seq
			if !yield(ev, nil) {
				return false, false
			}
			return true, ev.Type == event.TurnCompleted
		}
		for ev := range sub.C {
			if ev.Type == event.StreamGap {
				cursor := lastSeq
				for {
					page, err := m.EventsPage(context.Background(), sessionID, cursor, 0)
					if err != nil {
						yield(event.Event{}, err)
						return
					}
					for _, mev := range page.Events {
						if cont, fin := deliver(mev); !cont || fin {
							return
						}
					}
					if !page.HasMore || page.NextSeq <= cursor {
						break
					}
					cursor = page.NextSeq
				}
				continue
			}
			if cont, fin := deliver(ev); !cont || fin {
				return
			}
		}
	}
}

// Cancel interrupts the session's active turn, if any. It returns
// immediately; the turn ends with status "cancelled" shortly after.
func (m *Manager) Cancel(sessionID string) error {
	ms, ok := m.lookup(sessionID)
	if !ok {
		return ErrNotFound
	}
	ms.mu.Lock()
	t := ms.turn
	ms.mu.Unlock()
	if t != nil {
		t.cancel()
	}
	return nil
}

// Close cancels any active turn, releases the runtime session and emits
// session.closed. Closing a closed session is a no-op.
func (m *Manager) Close(ctx context.Context, sessionID string) error {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	ms, ok := m.lookup(sessionID)
	if !ok {
		rec, err := m.store.Get(ctx, sessionID)
		if err != nil {
			return err
		}
		if rec.Info.State != StateSuspended {
			return nil
		}
		ms, err = m.newManaged(ctx, rec.Info, rec.Config, true)
		if err != nil {
			return err
		}
		m.mu.Lock()
		m.sessions[sessionID] = ms
		m.mu.Unlock()
	}
	return ms.close(ctx, "closed")
}

// Events returns events with Seq > afterSeq, including durable history for
// sessions that are not live. truncated reports unavailable events, not paging.
// This legacy API can read unlimited history; use EventsPage to apply the
// configured per-response count and byte budgets.
func (m *Manager) Events(sessionID string, afterSeq uint64, limit int) (events []event.Event, truncated bool, err error) {
	ms, live := m.lookup(sessionID)
	if live {
		ms.emitMu.Lock()
		defer ms.emitMu.Unlock()
	}
	if store, ok := m.store.(EventStore); ok {
		if !live || !ms.eventWriteFailed {
			return store.Events(context.Background(), sessionID, afterSeq, limit)
		}
		// A failed disk write must not also prevent live gap recovery. Merge
		// the bounded cache with disk, preserving sequence order and gaps.
		events, _, err = store.Events(context.Background(), sessionID, afterSeq, 0)
		if err != nil {
			return nil, false, err
		}
		cached, _ := ms.log.Since(afterSeq, 0)
		for _, ev := range ms.unsaved {
			if ev.Seq > afterSeq {
				cached = append(cached, ev)
			}
		}
		bySeq := make(map[uint64]event.Event, len(events)+len(cached))
		for _, ev := range append(events, cached...) {
			bySeq[ev.Seq] = ev
		}
		events = nil
		for _, ev := range bySeq {
			events = append(events, ev)
		}
		slices.SortFunc(events, func(a, b event.Event) int {
			if a.Seq < b.Seq {
				return -1
			}
			if a.Seq > b.Seq {
				return 1
			}
			return 0
		})
		if limit > 0 && len(events) > limit {
			events = events[:limit]
		}
		seq := afterSeq
		for _, ev := range events {
			if ev.Seq != seq+1 {
				truncated = true
			}
			seq = ev.Seq
		}
		if (limit <= 0 || len(events) < limit) && seq < ms.log.LastSeq() {
			truncated = true
		}
		return events, truncated, nil
	}
	if live {
		events, truncated = ms.log.Since(afterSeq, limit)
		return events, truncated, nil
	}
	rec, err := m.store.Get(context.Background(), sessionID)
	return nil, err == nil && afterSeq < rec.Info.LastSeq, err
}

// Subscribe receives events from all sessions matching filter. buffer <= 0
// uses event.DefaultBuffer. Close the subscription when done.
func (m *Manager) Subscribe(filter event.Filter, buffer int) *event.Subscription {
	return m.bus.Subscribe(filter, buffer)
}

// RespondApproval resolves a pending approval request.
func (m *Manager) RespondApproval(approvalID string, d permission.Decision) error {
	return m.broker.Resolve(approvalID, d)
}

// PendingApprovals lists unresolved approval requests (all sessions if
// sessionID is empty).
func (m *Manager) PendingApprovals(sessionID string) []permission.Request {
	return m.broker.Pending(sessionID)
}

// Shutdown cancels all turns and closes all sessions.
func (m *Manager) Shutdown(ctx context.Context) error {
	m.baseCancel()
	select {
	case <-m.resourceDone:
	case <-ctx.Done():
		return ctx.Err()
	}
	m.mu.Lock()
	all := make([]*managed, 0, len(m.sessions))
	for _, ms := range m.sessions {
		all = append(all, ms)
	}
	m.mu.Unlock()
	var wg sync.WaitGroup
	errs := make([]error, len(all))
	for i, ms := range all {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.lifecycleMu.Lock()
			defer m.lifecycleMu.Unlock()
			errs[i] = ms.close(ctx, "shutdown")
		}()
	}
	wg.Wait()
	done := make(chan struct{})
	go func() { m.turns.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		errs = append(errs, ctx.Err())
	}
	return errors.Join(errs...)
}

func (m *Manager) lookup(id string) (*managed, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms, ok := m.sessions[id]
	return ms, ok
}

func (m *Manager) retire(ms *managed) {
	m.mu.Lock()
	m.closedOrder = append(m.closedOrder, ms)
	m.mu.Unlock()
	m.trimClosed(m.memoryPolicy().MaxClosedSessions)
}

// trimClosed respects pins and unsaved recovery payloads. A target may be
// exceeded until those protected records become eligible for reclamation.
func (m *Manager) trimClosed(limit int) {
	m.mu.Lock()
	all := slices.Clone(m.closedOrder)
	count := len(all)
	m.mu.Unlock()
	for _, old := range all {
		if count <= limit {
			break
		}
		old.emitMu.Lock()
		old.mu.Lock()
		safe := !old.info.Pinned && !old.eventWriteFailed && !old.recordWriteFailed
		old.mu.Unlock()
		if safe {
			m.forgetManaged(old)
			count--
		}
		old.emitMu.Unlock()
	}
}

// cleanTitle normalises a title supplied by the caller.
func cleanTitle(t string, allowEmpty bool) (string, error) {
	t = strings.Join(strings.Fields(t), " ")
	switch {
	case t == "" && !allowEmpty:
		return "", fmt.Errorf("%w: title is empty", ErrInvalidTitle)
	case utf8.RuneCountInString(t) > MaxTitleLength:
		return "", fmt.Errorf("%w: title is longer than %d characters", ErrInvalidTitle, MaxTitleLength)
	}
	return t, nil
}

// title makes a one-line session title from a prompt.
func title(prompt string) string {
	t := strings.Join(strings.Fields(prompt), " ")
	if r := []rune(t); len(r) > 80 {
		t = string(r[:79]) + "…"
	}
	return t
}

// managed is one session held by the Manager. It implements agent.Handler.
type managed struct {
	m                *Manager
	rt               agent.Runtime
	cfg              agent.SessionConfig
	initialCfg       agent.SessionConfig
	initialBudget    budget.Limits
	initialWorkspace workspace.Settings
	accounting       budget.Ledger
	log              *event.Log

	// emitMu serialises Append+Publish so subscribers observe events in
	// sequence order. Lock order: emitMu before mu.
	emitMu           sync.Mutex
	eventWriteFailed bool          // guarded by emitMu; enables disk/cache merging
	unsaved          []event.Event // failed durable writes; never evicted automatically
	lastAccess       time.Time     // guarded by emitMu; transcript reads refresh cache residency
	// persistMu serialises store writes (lock order: persistMu before mu).
	persistMu sync.Mutex

	mu                sync.Mutex
	info              Info
	recordWriteFailed bool // guarded by mu; prevents losing an unsaved record
	sess              agent.Session
	turn              *Turn
	turnIDs           map[string]struct{}
	closeOnce         sync.Once
	closeErr          error
	detach            func() // revokes the tool bridge binding
}

func (ms *managed) currentTurnID() string {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	if ms.turn != nil {
		return ms.turn.ID
	}
	return ""
}

func (ms *managed) cancelTurn() {
	ms.mu.Lock()
	t := ms.turn
	ms.mu.Unlock()
	if t != nil {
		t.cancel()
	}
}

var _ agent.Handler = (*managed)(nil)

func (ms *managed) snapshot() Info {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	info := ms.info
	if info.Workspace != nil {
		v := *info.Workspace
		info.Workspace = &v
	}
	info.Budget = info.Budget.Clone()
	info.Usage = info.Usage.Clone()
	info.Metadata = maps.Clone(info.Metadata)
	if info.Config != nil {
		selection := info.Config.Clone()
		info.Config = &selection
	}
	info.LastSeq = ms.log.LastSeq()
	return info
}

// rename sets the title and publishes a persisted snapshot.
func (ms *managed) rename(name string) (Info, error) {
	ms.emitMu.Lock()
	defer ms.emitMu.Unlock()
	ms.mu.Lock()
	old := ms.info.Title
	ms.info.Title = name
	ms.mu.Unlock()
	if err := ms.persist(); err != nil {
		ms.mu.Lock()
		ms.info.Title = old
		ms.mu.Unlock()
		return Info{}, err
	}
	ms.updatedLocked("")
	err := ms.persist()
	return ms.snapshot(), err
}

// persist serialises writes and snapshots so older state cannot overwrite newer.
func (ms *managed) persist() error {
	ms.persistMu.Lock()
	defer ms.persistMu.Unlock()
	ms.mu.Lock()
	cfg, cloneErr := ms.cfg.Clone()
	initial, initialErr := ms.initialCfg.Clone()
	sess := ms.sess
	ledger := ms.accounting
	initialBudget := ms.initialBudget.Clone()
	ms.mu.Unlock()
	if err := errors.Join(cloneErr, initialErr); err != nil {
		return err
	}
	defaults := cfg.NativeDefaults
	if provider, ok := sess.(agent.DefaultsProvider); ok {
		selection := provider.NativeDefaults().Clone()
		defaults = &selection
	}
	initialWorkspace := ms.initialWorkspace
	rec := Record{Accounting: &ledger, InitialBudget: &initialBudget, InitialWorkspace: &initialWorkspace, Info: ms.snapshot(), Config: cfg, InitialConfig: &initial, NativeDefaults: defaults, ForkSourceRuntimeID: cfg.ForkFrom}
	err := ms.m.store.Put(context.Background(), rec)
	ms.mu.Lock()
	ms.recordWriteFailed = err != nil
	ms.mu.Unlock()
	if err != nil {
		ms.m.log.Warn("persist session", "session", rec.Info.ID, "err", err)
	}
	return err
}

// updatedLocked publishes a complete snapshot of the mutable session fields.
func (ms *managed) updatedLocked(turnID string) {
	info := ms.snapshot()
	ms.emitLocked(event.Event{Type: event.SessionUpdated, TurnID: turnID, Data: event.SessionUpdatedData{
		Budget: &info.Budget, Usage: &info.Usage, Archived: info.Archived, Metadata: maps.Clone(info.Metadata), ForkPending: info.ForkPending,
		Config: info.Config, SettingsRevision: info.SettingsRevision, Pinned: info.Pinned, Title: info.Title, State: string(info.State), ActiveTurnID: info.ActiveTurnID,
		RuntimeSessionID: info.RuntimeSessionID, Turns: info.Turns,
		LastStatus: info.LastStatus, UpdatedAt: info.UpdatedAt, Error: info.Error,
	}})
}

// Emit implements agent.Handler.
func (ms *managed) Emit(ev event.Event) {
	if ev.TurnID == "" {
		ms.mu.Lock()
		if ms.turn != nil {
			ev.TurnID = ms.turn.ID
		}
		ms.mu.Unlock()
	}
	ms.emitMu.Lock()
	ms.emitLocked(ev)
	if u, ok := ev.Data.(event.UsageData); ok {
		ms.observeUsageLocked(u, false)
		_ = ms.persist()
	}
	ms.emitMu.Unlock()
}

func (ms *managed) emit(ev event.Event) {
	ms.emitMu.Lock()
	defer ms.emitMu.Unlock()
	ms.emitLocked(ev)
}

func (ms *managed) emitLocked(ev event.Event) {
	ev.SessionID = ms.info.ID // immutable after creation
	ev.Seq = 0
	stored := ms.log.Append(ev)
	if store, ok := ms.m.store.(EventStore); ok {
		if err := store.AppendEvent(context.Background(), stored); err != nil {
			ms.unsaved = append(ms.unsaved, stored)
			ms.m.log.Error("persist event", "session", stored.SessionID, "seq", stored.Seq, "err", err)
			ms.m.bus.Publish(stored)
			if !ms.eventWriteFailed {
				ms.eventWriteFailed = true
				warning := ms.log.Append(event.Event{SessionID: stored.SessionID, TurnID: stored.TurnID,
					Type: event.Error, Data: event.ErrorData{Code: "event_store_error", Message: "Session events could not be saved: " + err.Error()}})
				ms.unsaved = append(ms.unsaved, warning)
				ms.m.bus.Publish(warning)
			}
			return
		}
	}
	ms.m.bus.Publish(stored)
}

// RequestApproval implements agent.Handler.
func (ms *managed) RequestApproval(ctx context.Context, req permission.Request) (permission.Decision, error) {
	ms.mu.Lock()
	req.SessionID = ms.info.ID
	req.Runtime = ms.info.Runtime
	if req.TurnID == "" && ms.turn != nil {
		req.TurnID = ms.turn.ID
	}
	ms.mu.Unlock()
	if req.ID == "" {
		req.ID = NewID("apr_")
	}

	resolved := func(d permission.Decision, src permission.Source, withReq bool) {
		data := event.ApprovalResolvedData{ApprovalID: req.ID, Decision: d, Source: src}
		if withReq {
			r := req
			data.Request = &r
		}
		ms.emit(event.Event{Type: event.ApprovalResolved, TurnID: req.TurnID, Data: data})
	}

	if p := ms.m.opts.Policy; p != nil {
		if d, ok := p.Evaluate(ctx, req); ok {
			resolved(d, permission.SourcePolicy, true)
			return d, nil
		}
	}

	_, d, err := ms.m.broker.Ask(ctx, req, ms.m.ApprovalTimeout(), func(r permission.Request) {
		ms.emit(event.Event{Type: event.ApprovalRequested, TurnID: r.TurnID, Data: r})
	})
	switch {
	case err == nil:
		resolved(d, permission.SourceHost, false)
		return d, nil
	case errors.Is(err, permission.ErrTimeout):
		d = permission.Decision{Outcome: permission.Deny, Message: "approval timed out"}
		resolved(d, permission.SourceTimeout, false)
		return d, nil
	default:
		resolved(permission.Decision{Outcome: permission.Abort}, permission.SourceCancelled, false)
		return permission.Decision{}, err
	}
}

func (ms *managed) runTurn(ctx context.Context, t *Turn, input agent.Turn) {
	defer ms.m.turns.Done()
	defer t.cancel()

	workspaceChange := ms.captureWorkspace(ctx, t)
	res, err := ms.sess.Prompt(ctx, input)
	ms.completeWorkspace(t, workspaceChange)
	dead := errors.Is(err, agent.ErrSessionDead)
	switch {
	case err != nil && ctx.Err() != nil && !dead:
		res.Status = event.TurnStatusCancelled
	case err != nil:
		code := "runtime_error"
		if dead {
			code = "runtime_exited"
		}
		res.Status = event.TurnStatusFailed
		res.Error = &event.ErrorInfo{Code: code, Message: err.Error()}
	case res.Status == "":
		res.Status = event.TurnStatusCompleted
	}
	ms.emitMu.Lock()
	if res.Usage != nil {
		ms.observeUsageLocked(*res.Usage, true)
	}
	ms.mu.Lock()
	res.Budget = t.budgetStop
	t.result = res
	ms.accounting.End()
	ms.info.Usage = ms.accounting.Usage()
	ms.turn = nil
	ms.info.ActiveTurnID = ""
	ms.info.UpdatedAt = time.Now().UTC()
	if rid := ms.sess.RuntimeSessionID(); rid != "" {
		ms.info.RuntimeSessionID = rid
		if ms.cfg.ForkFrom != "" && rid != ms.cfg.ForkFrom {
			ms.cfg.ForkFrom = ""
			ms.info.ForkPending = false
		}
	}
	switch {
	case dead:
		ms.info.State = StateFailed
		ms.info.Error = res.Error
	case ms.info.State == StateRunning:
		ms.info.State = StateIdle
	}
	ms.info.LastStatus = res.Status
	ms.mu.Unlock()
	ms.updatedLocked(t.ID)
	ms.emitLocked(event.Event{Type: event.TurnCompleted, TurnID: t.ID, Data: event.TurnCompletedData{
		Status:        res.Status,
		Budget:        res.Budget,
		Error:         res.Error,
		Usage:         res.Usage,
		RuntimeTurnID: res.RuntimeTurnID,
	}})
	if dead {
		ms.emitLocked(event.Event{Type: event.SessionClosed, Data: event.SessionClosedData{Reason: "failed", Error: res.Error}})
	}
	ms.persist()
	if _, durableLookup := ms.m.store.(TurnLookup); durableLookup && !ms.eventWriteFailed {
		ms.mu.Lock()
		delete(ms.turnIDs, t.ID)
		ms.mu.Unlock()
	}
	ms.emitMu.Unlock()
	ms.m.wakeResources()
	close(t.done)

	if dead {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = ms.closeRuntime(ctx)
			ms.m.retire(ms)
		}()
	}
}

func (ms *managed) closeRuntime(ctx context.Context) error {
	ms.closeOnce.Do(func() {
		if ms.sess != nil {
			ms.closeErr = ms.sess.Close(ctx)
		}
		if ms.detach != nil {
			ms.detach()
		}
	})
	return ms.closeErr
}

func (ms *managed) close(ctx context.Context, reason string) error {
	ms.emitMu.Lock()
	ms.mu.Lock()
	if ms.info.State.Terminal() || ms.info.State == StateClosing {
		ms.mu.Unlock()
		ms.emitMu.Unlock()
		return nil
	}
	ms.info.State = StateClosing
	t := ms.turn
	ms.mu.Unlock()
	ms.updatedLocked("")
	ms.emitMu.Unlock()

	if t != nil {
		t.cancel()
		select {
		case <-t.done:
		case <-ctx.Done():
		}
	}
	err := ms.closeRuntime(ctx)

	ms.emitMu.Lock()
	ms.mu.Lock()
	ms.info.State = StateClosed
	ms.info.UpdatedAt = time.Now().UTC()
	ms.mu.Unlock()
	ms.updatedLocked("")
	ms.emitLocked(event.Event{Type: event.SessionClosed, Data: event.SessionClosedData{Reason: reason}})
	ms.persist()
	ms.emitMu.Unlock()
	ms.m.retire(ms)
	return err
}

// NewID returns prefix followed by 24 random hex characters.
func NewID(prefix string) string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return prefix + hex.EncodeToString(b[:])
}
