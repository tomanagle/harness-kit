package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/config"
	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/hooks"
	"github.com/tomanagle/harness-kit/core/internal/budget"
	"github.com/tomanagle/harness-kit/core/mcp"
	"github.com/tomanagle/harness-kit/core/settings"
	"github.com/tomanagle/harness-kit/core/workspace"
)

// CreateParams configure a new session.
type CreateParams struct {
	compaction config.CompactionSettings
	workspace  workspace.Settings
	// Settings uses the unified settings envelope: session values and
	// memory.pinned may be supplied at creation. Global resource policies
	// belong to ConfigureSettings.
	Settings settings.Patch
	pinned   bool
	budget   config.BudgetLimits
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
	hookRequest, warnings, err := m.runHooks(ctx, hooks.Request{Name: hooks.BeforeCreate, InvocationID: NewID("create_"), Create: &hooks.Create{Runtime: p.Runtime, Title: p.Title, Metadata: p.Metadata, Config: p.Config}})
	if err != nil {
		return Info{}, err
	}
	if hookRequest.Create != nil {
		p.Runtime, p.Title, p.Metadata, p.Config = hookRequest.Create.Runtime, hookRequest.Create.Title, hookRequest.Create.Metadata, hookRequest.Create.Config
	}
	if m.opts.Prepare != nil {
		w, err := m.opts.Prepare(ctx, &p)
		if err != nil {
			return Info{}, err
		}
		warnings = append(warnings, w...)
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
		Compaction:        p.compaction.Clone(),
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
	info.Compaction = compactionPolicy(info.Compaction)
	initialCompaction := info.Compaction.Clone()
	initialWorkspace := workspacePolicy(info.WorkspaceSettings)
	var accountingAfter uint64
	_, hasLookup := m.store.(TurnLookup)
	visit := func(ev event.Event) error {
		if ev.Seq > info.LastSeq {
			replayContextEvent(&info, ev)
		}
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
		if len(rec.Accounting) != 0 && strings.TrimSpace(string(rec.Accounting)) != "null" {
			if err := json.Unmarshal(rec.Accounting, &ledger); err != nil {
				return nil, fmt.Errorf("session: decode accounting checkpoint: %w", err)
			}
			accountingAfter = rec.Info.LastSeq
		}
		if rec.InitialBudget != nil {
			initialBudget = rec.InitialBudget.Clone()
		}
		if rec.InitialCompaction != nil {
			initialCompaction = compactionPolicy(*rec.InitialCompaction)
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
	return &managed{m: m, cfg: cfg, initialCfg: initial, initialWorkspace: initialWorkspace, initialCompaction: initialCompaction, initialBudget: initialBudget, accounting: ledger, info: info, turnIDs: turnIDs, log: log, lastAccess: time.Now()}, nil
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
	info.Compaction = compactionPolicy(info.Compaction)
	if err := validateCompaction(rt, info.Compaction); err != nil {
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
	if ms.nativeCompaction != nil {
		ms.info.State = StateCompacting
	}
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
	m.log.InfoContext(ctx, "session started", "sessionId", info.ID, "runtime", info.Runtime, "resumed", cfg.ResumeID != "")
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
