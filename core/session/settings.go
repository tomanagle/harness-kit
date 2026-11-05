package session

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/config"
	"github.com/tomanagle/harness-kit/core/settings"
	"github.com/tomanagle/harness-kit/core/workspace"
)

// initSettings bridges constructor options into the unified settings document.
func (m *Manager) initSettings() {
	v := settings.Defaults()
	v.Process.AllowUnsafe = m.opts.AllowUnsafe
	for _, rt := range m.reg.List() {
		v.Process.Runtimes = append(v.Process.Runtimes, rt.Name)
	}
	if fs, ok := m.store.(interface{ Dir() string }); ok {
		v.Process.Store = fs.Dir()
	}
	v.Memory.EventLogSize = m.opts.EventLogSize
	v.Memory.MaxClosedSessions = m.opts.MaxClosedSessions
	v.Approvals.TimeoutMs = m.opts.ApprovalTimeout.Milliseconds()
	if m.opts.StreamBuffer > 0 {
		v.Streams.MaxQueueEvents = m.opts.StreamBuffer
	}
	if m.opts.LogLevel != nil {
		v.Logging.Level = strings.ToLower(m.opts.LogLevel.Level().String())
	}
	limits := m.opts.Logs.Config()
	v.Logging.MaxQueueRecords, v.Logging.MaxQueueBytes, v.Logging.MaxRecordBytes = limits.MaxQueueRecords, limits.MaxQueueBytes, limits.MaxRecordBytes
	m.settings, m.initialSettings = v.Clone(), v.Clone()
	m.settingsRevision = 1
	m.bus.Configure(v.Streams.MaxQueueEvents, v.Streams.MaxQueueBytes)
}

// InitializeSettings supplies the owning host's startup document. It is valid
// once, before sessions are created. Additional client connections must read
// the current settings instead of reapplying startup preferences.
func (m *Manager) InitializeSettings(ctx context.Context, v settings.Values) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := settings.Validate(v); err != nil {
		return err
	}
	copy, err := settings.Clone(v)
	if err != nil {
		return err
	}
	m.settingsChangeMu.Lock()
	defer m.settingsChangeMu.Unlock()
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	m.mu.Lock()
	used := len(m.sessions) > 0
	m.mu.Unlock()
	m.settingsMu.RLock()
	initialized := m.settingsInitialized
	m.settingsMu.RUnlock()
	if used || initialized {
		return fmt.Errorf("%w: startup settings have already been initialized", settings.ErrRestartRequired)
	}
	if err := m.validateResourceSettings(copy); err != nil {
		return err
	}
	if copy.Memory.Pinned {
		return fmt.Errorf("%w: memory.pinned requires a session target", settings.ErrScope)
	}
	if err := m.validateSessionDefaults(ctx, copy); err != nil {
		return err
	}
	if m.opts.SettingsStore != nil {
		if err := m.opts.SettingsStore.Save(ctx, copy.Clone()); err != nil {
			return fmt.Errorf("%w: %w", settings.ErrPersistence, err)
		}
	}
	m.opts.AllowUnsafe = copy.Process.AllowUnsafe
	if m.opts.LogLevel != nil {
		var level slog.Level
		_ = level.UnmarshalText([]byte(copy.Logging.Level))
		m.opts.LogLevel.Set(level)
	}
	m.configureLogs(copy.Logging)
	m.settingsMu.Lock()
	m.settings = copy.Clone()
	m.initialSettings = copy.Clone()
	m.settingsRevision++
	m.settingsInitialized = true
	m.publishSettingsLocked(m.globalSnapshotLocked())
	m.settingsMu.Unlock()
	m.bus.Configure(copy.Streams.MaxQueueEvents, copy.Streams.MaxQueueBytes)
	m.wakeResources()
	return nil
}

// Policy snapshots borrow immutable values. Settings updates replace the
// entire document; internal readers never mutate its pointer-valued limits.
func (m *Manager) memoryPolicy() settings.Memory {
	m.settingsMu.RLock()
	defer m.settingsMu.RUnlock()
	return m.settings.Memory
}
func (m *Manager) historyPolicy() settings.History {
	m.settingsMu.RLock()
	defer m.settingsMu.RUnlock()
	return m.settings.History
}

func (m *Manager) currentSettings() settings.Values {
	m.settingsMu.RLock()
	defer m.settingsMu.RUnlock()
	return m.settings.Clone()
}

func (m *Manager) globalSnapshotLocked() settings.Snapshot {
	fields := settings.Describe()
	for name, f := range fields {
		if !slices.Contains(f.Scopes, "global") {
			f.Writable = false
			fields[name] = f
		}
	}
	snapshot := settings.Snapshot{Revision: m.settingsRevision, Values: m.settings.Clone(), Fields: fields}
	if rt, ok := m.reg.Get(snapshot.Values.Session.Runtime); ok {
		applyRuntimeFields(&snapshot, rt.Info(), false)
	}
	return snapshot
}

// Settings returns effective values and descriptions for the global host or a
// selected session. Session revisions change only for configuration/pinning,
// allowing a settings UI to save while unrelated transcript events stream.
func (m *Manager) Settings(ctx context.Context, target settings.Target) (settings.Snapshot, error) {
	if err := ctx.Err(); err != nil {
		return settings.Snapshot{}, err
	}
	m.lifecycleMu.Lock()
	snapshot, err := m.settingsSnapshotLocked(ctx, target)
	m.lifecycleMu.Unlock()
	if err != nil {
		return snapshot, err
	}
	if target.Runtime != "" {
		rt, _ := m.reg.Get(target.Runtime)
		if describer, ok := rt.(agent.ConfigurationDescriber); ok {
			cfg := agent.SessionConfig{Model: target.Model, Cwd: snapshot.Values.Session.Cwd}
			descriptors, err := describer.DescribeConfiguration(ctx, cfg)
			if err != nil {
				snapshot.Warnings = append(snapshot.Warnings, err.Error())
			} else {
				for name, descriptor := range descriptors {
					field := snapshot.Fields["session."+name]
					describeField(&field, descriptor)
					snapshot.Fields["session."+name] = field
				}
			}
		}
	}
	return snapshot, ctx.Err()
}

func (m *Manager) settingsSnapshotLocked(ctx context.Context, target settings.Target) (settings.Snapshot, error) {
	m.settingsMu.RLock()
	snapshot := m.globalSnapshotLocked()
	m.settingsMu.RUnlock()
	snapshot.Target = target
	if target.Runtime != "" || target.Model != "" {
		if target.Runtime == "" || target.SessionID != "" {
			return settings.Snapshot{}, fmt.Errorf("%w: runtime discovery requires runtime and no sessionId", settings.ErrScope)
		}
		rt, ok := m.reg.Get(target.Runtime)
		if !ok {
			return settings.Snapshot{}, fmt.Errorf("%w: %s", ErrRuntimeNotFound, target.Runtime)
		}
		applyRuntimeFields(&snapshot, rt.Info(), false)
		for name, field := range snapshot.Fields {
			field.Writable = false
			snapshot.Fields[name] = field
		}

		return snapshot, nil
	}
	if target.SessionID == "" {
		if rt, ok := m.reg.Get(snapshot.Values.Session.Runtime); ok {
			applyRuntimeFields(&snapshot, rt.Info(), false)
		}
		return snapshot, nil
	}
	var cfg agent.SessionConfig
	var info Info
	if ms, ok := m.lookup(target.SessionID); ok {
		ms.mu.Lock()
		var err error
		cfg, err = ms.cfg.Clone()
		ms.mu.Unlock()
		if err != nil {
			return settings.Snapshot{}, err
		}
		info = ms.snapshot()
	} else {
		rec, err := m.store.Get(ctx, target.SessionID)
		if err != nil {
			return settings.Snapshot{}, err
		}
		info, err = m.storedInfo(ctx, rec.Info)
		if err != nil {
			return settings.Snapshot{}, err
		}
		cfg = rec.Config
		if info.Config != nil {
			cfg = cfg.WithSettings(*info.Config)
		}
	}
	snapshot.Values.Session = settings.Session{Runtime: info.Runtime, SessionConfig: cfg}
	snapshot.Values.Memory.Pinned = info.Pinned
	snapshot.Values.Budget = info.Budget.Clone()
	snapshot.Values.Compaction = compactionPolicy(info.Compaction)
	snapshot.Values.Workspace = workspacePolicy(info.WorkspaceSettings)
	snapshot.Revision = max(uint64(1), info.SettingsRevision)
	rt, _ := m.reg.Get(info.Runtime)
	for path, f := range snapshot.Fields {
		f.Writable = path == "memory.pinned"
		if strings.HasPrefix(path, "compaction.") {
			f.When = "nextOperation"
			f.Writable = rt != nil && rt.Info().Capabilities.Compaction
			if !f.Writable {
				f.When = "unsupported"
			}
		}
		if strings.HasPrefix(path, "budget.") {
			f.Writable = true
			f.When = "live"
			if rt != nil {
				caps := rt.Info().Capabilities
				if strings.HasSuffix(path, "Tokens") && !caps.Usage || strings.HasSuffix(path, "CostUsd") && !caps.Cost {
					f.Writable = false
					f.When = "unsupported"
				}
			}
		}
		if strings.HasPrefix(path, "workspace.") {
			if slices.Contains(f.Scopes, "turn") {
				f.When = "nextTurn"
				f.Writable = m.opts.Workspace != nil
			}
		}
		if strings.HasPrefix(path, "session.") {
			f.When = "newSession"
			field := strings.TrimPrefix(path, "session.")
			if rt != nil {
				if support, ok := rt.Info().Capabilities.Configuration[field]; ok && support.Session {
					f.When = "nextTurn"
					f.Writable = true
				}
			}
		}
		snapshot.Fields[path] = f
	}
	if rt != nil {
		for path, support := range rt.Info().Capabilities.Configuration {
			if !strings.HasPrefix(path, "options.") {
				continue
			}
			f := settings.Field{Type: "native", When: "newSession", Scopes: []string{"session", "turn"}, Writable: support.Session}
			if support.Session {
				f.When = "nextTurn"
			}
			snapshot.Fields["session."+path] = f
		}
	}
	if rt != nil {
		applyRuntimeFields(&snapshot, rt.Info(), true)
	}
	return snapshot, nil
}

// ConfigureSettings is the single mutation path for host resource policy and
// session settings. Validation and revision checks precede any change.
func (m *Manager) ConfigureSettings(ctx context.Context, p settings.ConfigureParams) (settings.Snapshot, error) {
	if p.Target.Runtime != "" || p.Target.Model != "" {
		return settings.Snapshot{}, fmt.Errorf("%w: runtime/model targets are read-only discovery", settings.ErrScope)
	}
	if err := ctx.Err(); err != nil {
		return settings.Snapshot{}, err
	}
	m.settingsChangeMu.Lock()
	defer m.settingsChangeMu.Unlock()
	if p.Target.SessionID != "" {
		return m.configureSessionSettings(ctx, p)
	}
	m.settingsMu.RLock()
	base, initial, revision := m.settings.Clone(), m.initialSettings.Clone(), m.settingsRevision
	m.settingsMu.RUnlock()
	if p.ExpectedRevision != nil && *p.ExpectedRevision != revision {
		return settings.Snapshot{}, settings.ErrConflict
	}
	next, paths, err := settings.Apply(base, p.Patch, p.Reset, initial)
	if err != nil {
		return settings.Snapshot{}, err
	}
	for _, path := range paths {
		if sameSetting(base, next, path) {
			continue
		}
		if path == "memory.pinned" {
			return settings.Snapshot{}, fmt.Errorf("%w: %s requires a session target", settings.ErrScope, path)
		}
		if field, ok := settings.Describe()[path]; ok && field.When == "restart" {
			return settings.Snapshot{}, fmt.Errorf("%w: %s", settings.ErrRestartRequired, path)
		}
	}
	if reflect.DeepEqual(base, next) {
		m.settingsMu.RLock()
		snapshot := m.globalSnapshotLocked()
		m.settingsMu.RUnlock()
		return snapshot, nil
	}
	if err := m.validateResourceSettings(next); err != nil {
		return settings.Snapshot{}, err
	}
	if err := m.validateSessionDefaults(ctx, next); err != nil {
		return settings.Snapshot{}, err
	}
	if m.opts.SettingsStore != nil {
		if err := m.opts.SettingsStore.Save(ctx, next.Clone()); err != nil {
			return settings.Snapshot{}, fmt.Errorf("%w: %w", settings.ErrPersistence, err)
		}
	}
	m.settingsMu.Lock()
	m.settings = next.Clone()
	m.settingsRevision++
	snapshot := m.globalSnapshotLocked()
	m.publishSettingsLocked(snapshot)
	m.settingsMu.Unlock()
	if m.opts.LogLevel != nil {
		var level slog.Level
		_ = level.UnmarshalText([]byte(next.Logging.Level))
		m.opts.LogLevel.Set(level)
	}
	m.configureLogs(next.Logging)
	m.bus.Configure(next.Streams.MaxQueueEvents, next.Streams.MaxQueueBytes)
	// Cleanup is serialized with prompt admission. Busy/pinned/unsaved sessions
	// stay resident, and the snapshot reports why any budget is still exceeded.
	snapshot.Deferred = m.enforceResources(ctx)
	return snapshot, nil
}

func (m *Manager) validateResourceSettings(v settings.Values) error {
	if v.Memory.EvictAfterIdleMs != nil || v.Memory.SuspendAfterIdleMs != nil || v.Memory.MaxCacheBytes != nil || v.Memory.MaxSessionCacheBytes != nil || v.Memory.MaxCachedSessions != nil {
		if _, ok := m.store.(EventStore); !ok {
			return fmt.Errorf("%w: memory eviction policies require a persistent EventStore", agent.ErrUnsupported)
		}
	}
	return nil
}

func (m *Manager) validateSessionDefaults(ctx context.Context, v settings.Values) error {
	if err := m.validateWorkspacePolicy(v.Workspace); err != nil {
		return err
	}
	if v.Session.Cwd != "" && !filepath.IsAbs(v.Session.Cwd) {
		return fmt.Errorf("%w: session.cwd must be absolute", settings.ErrInvalid)
	}
	if v.Session.Runtime == "" {
		return nil
	}
	rt, ok := m.reg.Get(v.Session.Runtime)
	if !ok {
		return fmt.Errorf("%w: %s", ErrRuntimeNotFound, v.Session.Runtime)
	}
	if err := validateCompaction(rt, v.Compaction); err != nil {
		return err
	}
	return m.validateConfig(ctx, rt, "", ConfigCreate, v.Session.SessionConfig)
}

// SubscribeSettings delivers complete snapshots with revisions. Slow observers
// receive the newest state with Resync set, and refresh their observed scopes
// with Settings. The cancel
// function is safe to call repeatedly.
func (m *Manager) SubscribeSettings() (<-chan settings.Snapshot, func()) {
	ch := make(chan settings.Snapshot, 1)
	m.settingsMu.Lock()
	m.settingsSubscribers[ch] = struct{}{}
	m.settingsMu.Unlock()
	return ch, func() {
		m.settingsMu.Lock()
		if _, ok := m.settingsSubscribers[ch]; ok {
			delete(m.settingsSubscribers, ch)
			close(ch)
		}
		m.settingsMu.Unlock()
	}
}

func (m *Manager) publishSettingsLocked(snapshot settings.Snapshot) {
	for ch := range m.settingsSubscribers {
		// Clone every recipient: Go subscribers own the document they receive.
		copy := cloneSettingsSnapshot(snapshot)
		select {
		case ch <- copy:
		default:
			select {
			case <-ch:
			default:
			}
			copy.Resync = true // A different target may have occupied the slot.
			ch <- copy
		}
	}
}

func cloneSettingsSnapshot(s settings.Snapshot) settings.Snapshot {
	s.Values = s.Values.Clone()
	fields := make(map[string]settings.Field, len(s.Fields))
	for k, v := range s.Fields {
		v.Scopes = slices.Clone(v.Scopes)
		v.Choices = slices.Clone(v.Choices)
		v.Default = slices.Clone(v.Default)
		if v.Minimum != nil {
			n := *v.Minimum
			v.Minimum = &n
		}
		fields[k] = v
	}
	s.Fields = fields
	s.Deferred = slices.Clone(s.Deferred)
	s.Warnings = slices.Clone(s.Warnings)
	return s
}

func (m *Manager) publishSessionSettingsLocked(ctx context.Context, id string) {
	snapshot, err := m.settingsSnapshotLocked(ctx, settings.Target{SessionID: id})
	if err != nil {
		return
	}
	m.settingsMu.Lock()
	m.publishSettingsLocked(snapshot)
	m.settingsMu.Unlock()
}

func (m *Manager) createSettings(p CreateParams) (CreateParams, error) {
	base := m.currentSettings()
	raw, err := json.Marshal(p.Config)
	if err != nil {
		return p, err
	}
	var legacy map[string]json.RawMessage
	if err = json.Unmarshal(raw, &legacy); err != nil {
		return p, err
	}
	if p.Runtime != "" {
		legacy["runtime"], _ = json.Marshal(p.Runtime)
	}
	if p.Settings != nil {
		for group, raw := range p.Settings {
			if group == "memory" {
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
					return p, fmt.Errorf("%w: memory must be an object", settings.ErrInvalid)
				}
				for field := range fields {
					if field != "pinned" {
						return p, fmt.Errorf("%w: memory.%s requires global scope", settings.ErrScope, field)
					}
				}
			} else if group != "session" && group != "workspace" && group != "budget" && group != "compaction" {
				return p, fmt.Errorf("%w: %s cannot be set during session creation", settings.ErrScope, group)
			}
		}
		var incoming map[string]json.RawMessage
		if raw, ok := p.Settings["session"]; ok {
			if err := json.Unmarshal(raw, &incoming); err != nil {
				return p, fmt.Errorf("%w: session must be an object", settings.ErrInvalid)
			}
		}
		for key := range incoming {
			if _, exists := legacy[key]; exists {
				return p, fmt.Errorf("%w: session.%s supplied through both settings and legacy fields", settings.ErrInvalid, key)
			}
		}
	}
	if len(legacy) > 0 {
		raw, _ = json.Marshal(legacy)
		base, _, err = settings.Apply(base, settings.Patch{"session": raw}, nil, base)
		if err != nil {
			return p, err
		}
	}
	if p.Settings != nil {
		base, _, err = settings.Apply(base, p.Settings, nil, base)
		if err != nil {
			return p, err
		}
	}
	if base.Session.Cwd != "" && !filepath.IsAbs(base.Session.Cwd) {
		return p, fmt.Errorf("%w: session.cwd must be absolute", settings.ErrInvalid)
	}
	p.Runtime = base.Session.Runtime
	p.Config = base.Session.SessionConfig
	p.pinned = base.Memory.Pinned
	p.budget = base.Budget.Clone()
	p.compaction = base.Compaction.Clone()
	p.workspace = base.Workspace
	return p, nil
}

func promptSettings(p settings.Patch) (agent.ConfigPatch, error) {
	for group := range p {
		if group == "workspace" || group == "compaction" {
			continue
		}
		if group != "session" && group != "budget" {
			return agent.ConfigPatch{}, fmt.Errorf("%w: %s cannot be set for one turn", settings.ErrScope, group)
		}
	}
	raw, ok := p["session"]
	if !ok {
		return agent.ConfigPatch{}, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return agent.ConfigPatch{}, fmt.Errorf("%w: session must be an object", settings.ErrInvalid)
	}
	for key := range fields {
		if key != "model" && key != "effort" && key != "options" {
			return agent.ConfigPatch{}, fmt.Errorf("%w: session.%s is not turn-configurable", settings.ErrScope, key)
		}
	}
	var patch agent.ConfigPatch
	if err := json.Unmarshal(raw, &patch); err != nil {
		return patch, err
	}
	return patch, nil
}

func (m *Manager) configureSessionSettings(ctx context.Context, p settings.ConfigureParams) (settings.Snapshot, error) {
	m.lifecycleMu.Lock()
	defer m.lifecycleMu.Unlock()
	current, err := m.settingsSnapshotLocked(ctx, p.Target)
	if err != nil {
		return settings.Snapshot{}, err
	}
	if p.ExpectedRevision != nil && *p.ExpectedRevision != current.Revision {
		return settings.Snapshot{}, settings.ErrConflict
	}
	rec, err := m.store.Get(ctx, p.Target.SessionID)
	if err != nil {
		return settings.Snapshot{}, err
	}
	initial := current.Values.Clone()
	if rec.InitialWorkspace != nil {
		initial.Workspace = *rec.InitialWorkspace
	}
	if rec.InitialConfig != nil {
		initial.Session.SessionConfig = *rec.InitialConfig
	}
	initial.Memory.Pinned = false
	if rec.InitialBudget != nil {
		initial.Budget = rec.InitialBudget.Clone()
	}
	initial.Compaction = compactionPolicy(config.CompactionSettings{})
	if rec.InitialCompaction != nil {
		initial.Compaction = compactionPolicy(*rec.InitialCompaction)
	}
	next, paths, err := settings.Apply(current.Values, p.Patch, p.Reset, initial)
	if err != nil {
		return settings.Snapshot{}, err
	}
	if reflect.DeepEqual(current.Values, next) {
		return current, nil
	}
	changes := sessionChanges{}
	patch := agent.ConfigPatch{}
	for _, path := range paths {
		if sameSetting(current.Values, next, path) {
			continue
		}
		switch {
		case strings.HasPrefix(path, "compaction."):
			v := next.Compaction.Clone()
			changes.compaction = &v
		case path == "memory.pinned":
			v := next.Memory.Pinned
			changes.pin = &v
		case strings.HasPrefix(path, "budget."):
			v := next.Budget.Clone()
			changes.budget = &v
		case strings.HasPrefix(path, "workspace."):
			if !workspace.SameLocation(current.Values.Workspace, next.Workspace) {
				return settings.Snapshot{}, fmt.Errorf("%w: %s", settings.ErrNewSessionRequired, path)
			}
			if err := m.validateWorkspacePolicy(next.Workspace); err != nil {
				return settings.Snapshot{}, err
			}
			v := next.Workspace
			changes.workspace = &v
		case path == "session.model":
			v := next.Session.Model
			patch.Model = &v
		case path == "session.effort":
			v := next.Session.Effort
			patch.Effort = &v
		case strings.HasPrefix(path, "session.options."):
			selected, e := next.Session.SessionConfig.Settings()
			if e != nil {
				return settings.Snapshot{}, e
			}
			name := strings.TrimPrefix(path, "session.options.")
			if value, exists := selected.Options[name]; exists {
				if patch.Options == nil {
					patch.Options = map[string]json.RawMessage{}
				}
				patch.Options[name] = value
			} else {
				patch.Reset = append(patch.Reset, "options."+name)
			}
		case path == "session.options":
			before, e := current.Values.Session.SessionConfig.Settings()
			if e != nil {
				return settings.Snapshot{}, e
			}
			after, e := next.Session.SessionConfig.Settings()
			if e != nil {
				return settings.Snapshot{}, e
			}
			patch.Options = after.Options
			for key := range before.Options {
				if _, ok := after.Options[key]; !ok {
					patch.Reset = append(patch.Reset, "options."+key)
				}
			}
		case strings.HasPrefix(path, "session."):
			return settings.Snapshot{}, fmt.Errorf("%w: %s", settings.ErrNewSessionRequired, path)
		default:
			return settings.Snapshot{}, fmt.Errorf("%w: %s requires global scope", settings.ErrScope, path)
		}
	}
	if _, err := m.configureLocked(ctx, p.Target.SessionID, patch, changes); err != nil {
		return settings.Snapshot{}, err
	}
	snapshot, err := m.settingsSnapshotLocked(ctx, p.Target)
	if err == nil {
		m.settingsMu.Lock()
		m.publishSettingsLocked(snapshot)
		m.settingsMu.Unlock()
	}
	m.wakeResources()
	return snapshot, err
}

// Pin is a compatibility convenience for scoped memory.pinned configuration.
func (m *Manager) Pin(ctx context.Context, id string, pinned bool) (Info, error) {
	raw, _ := json.Marshal(map[string]bool{"pinned": pinned})
	_, err := m.ConfigureSettings(ctx, settings.ConfigureParams{Target: settings.Target{SessionID: id}, Patch: settings.Patch{"memory": raw}})
	if err != nil {
		return Info{}, err
	}
	return m.Get(ctx, id)
}

// ApprovalTimeout returns the current timeout used for new approval requests.
func (m *Manager) ApprovalTimeout() time.Duration {
	m.settingsMu.RLock()
	defer m.settingsMu.RUnlock()
	return time.Duration(m.settings.Approvals.TimeoutMs) * time.Millisecond
}

// sameSetting avoids treating an unchanged field in a complete UI document as
// a restart or new-session request. Native options remain indivisible values.
func sameSetting(a, b settings.Values, path string) bool {
	read := func(v settings.Values) json.RawMessage {
		raw, _ := json.Marshal(v)
		var doc map[string]json.RawMessage
		_ = json.Unmarshal(raw, &doc)
		parts := strings.SplitN(path, ".", 3)
		var group map[string]json.RawMessage
		_ = json.Unmarshal(doc[parts[0]], &group)
		if len(parts) == 2 {
			return group[parts[1]]
		}
		var opts map[string]json.RawMessage
		_ = json.Unmarshal(group[parts[1]], &opts)
		return opts[parts[2]]
	}
	var av, bv any
	_ = json.Unmarshal(read(a), &av)
	_ = json.Unmarshal(read(b), &bv)
	return reflect.DeepEqual(av, bv)
}

func describeField(field *settings.Field, descriptor config.Descriptor) {
	if descriptor.Type != "" {
		field.Type = descriptor.Type
	}
	field.Description, field.Source, field.AllowCustom = descriptor.Description, descriptor.Source, descriptor.AllowCustom
	field.Choices = slices.Clone(descriptor.Choices)
	field.Default = slices.Clone(descriptor.Default)
}
func applyRuntimeFields(snapshot *settings.Snapshot, info agent.Info, scoped bool) {
	// A scoped target may use a different runtime from the host defaults.
	for path := range snapshot.Fields {
		if strings.HasPrefix(path, "session.options.") {
			delete(snapshot.Fields, path)
		}
	}
	for name, support := range info.Capabilities.Configuration {
		path := "session." + name
		field := snapshot.Fields[path]
		if field.Type == "" {
			field.Type = "native"
		}
		field.When, field.Writable = "newSession", support.Startup
		field.Scopes = []string{"global"}
		if support.Session {
			field.Scopes = append(field.Scopes, "session")
		}
		if support.Turn {
			field.Scopes = append(field.Scopes, "turn")
		}
		if scoped {
			field.Writable = support.Session
			if support.Session {
				field.When = "nextTurn"
			}
		}
		describeField(&field, support.Descriptor)
		snapshot.Fields[path] = field
	}
}
