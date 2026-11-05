// Package settings defines the configuration shared by hosts, the sidecar and
// clients. It owns no process-global configuration and performs no implicit I/O.
// Store and FileStore provide optional, explicitly selected persistence.
package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/config"
	"github.com/tomanagle/harness-kit/core/hooks"
	"github.com/tomanagle/harness-kit/core/workspace"
)

var (
	ErrInvalid            = config.ErrInvalid
	ErrConflict           = errors.New("settings: revision conflict")
	ErrRestartRequired    = errors.New("settings: sidecar restart required")
	ErrNewSessionRequired = errors.New("settings: new session required")
	ErrScope              = errors.New("settings: field is not supported in this scope")
)

// Values is the single settings document. Process describes host wiring and
// shutdown policy; Session supplies defaults for new sessions. Scoped snapshots replace
// Session with the selected session's settings. Optional limits use nil for no
// limit; a zero cache budget retains no cached events. These are data budgets,
// not limits on process RSS or memory owned by child processes.
type Values struct {
	Hooks      hooks.Settings            `json:"hooks"`
	Compaction config.CompactionSettings `json:"compaction"`
	Git        workspace.GitSettings     `json:"git"`
	Workspace  workspace.Settings        `json:"workspace"`
	Budget     config.BudgetLimits       `json:"budget"`
	Process    Process                   `json:"process"`
	Logging    Logging                   `json:"logging"`
	Approvals  Approvals                 `json:"approvals"`
	Memory     Memory                    `json:"memory"`
	History    History                   `json:"history"`
	Streams    Streams                   `json:"streams"`
	Session    Session                   `json:"session"`
}

type Process struct {
	// SettingsPath opts into host-owned JSON preference persistence. Empty is in-memory.
	SettingsPath      string                `json:"settingsPath"`
	ACP               map[string]ACPRuntime `json:"acp,omitempty"`
	Store             string                `json:"store"`
	Runtimes          []string              `json:"runtimes"`
	CodexPath         string                `json:"codexPath"`
	ClaudePath        string                `json:"claudePath"`
	GitPath           string                `json:"gitPath"`
	FakeDelayMs       int64                 `json:"fakeDelayMs"`
	KitMCP            bool                  `json:"kitMcp"`
	AllowUnsafe       bool                  `json:"allowUnsafe"`
	ShutdownTimeoutMs int64                 `json:"shutdownTimeoutMs"`
}
type Logging struct {
	Level           string `json:"level"`
	MaxQueueRecords int    `json:"maxQueueRecords"`
	MaxQueueBytes   int64  `json:"maxQueueBytes"`
	MaxRecordBytes  int    `json:"maxRecordBytes"`
}
type Approvals struct {
	TimeoutMs int64 `json:"timeoutMs"`
}
type Memory struct {
	// Pinned is session-scoped: automatic eviction and suspension skip it.
	Pinned               bool   `json:"pinned"`
	MaxCacheBytes        *int64 `json:"maxCacheBytes"`
	MaxSessionCacheBytes *int64 `json:"maxSessionCacheBytes"`
	MaxCachedSessions    *int   `json:"maxCachedSessions"`
	EvictAfterIdleMs     *int64 `json:"evictAfterIdleMs"`
	SuspendAfterIdleMs   *int64 `json:"suspendAfterIdleMs"`
	ResumeOnPrompt       bool   `json:"resumeOnPrompt"`
	EventLogSize         int    `json:"eventLogSize"`
	MaxClosedSessions    int    `json:"maxClosedSessions"`
}
type History struct {
	MaxPageEvents int    `json:"maxPageEvents"`
	MaxPageBytes  *int64 `json:"maxPageBytes"`
}
type Streams struct {
	MaxQueueEvents int    `json:"maxQueueEvents"`
	MaxQueueBytes  *int64 `json:"maxQueueBytes"`
}
type Session struct {
	Runtime string `json:"runtime,omitempty"`
	agent.SessionConfig
}

// Patch recursively patches settings groups. session.options patches native
// option keys, replacing each option's complete value (no recursive merge).
// Omitted fields are untouched. Use ConfigureParams.Reset to inherit defaults.
type Patch map[string]json.RawMessage

func (p *Patch) UnmarshalJSON(data []byte) error {
	type plain Patch
	var out plain
	if err := json.Unmarshal(data, &out); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if out == nil {
		return fmt.Errorf("%w: patch must be an object", ErrInvalid)
	}
	*p = Patch(out)
	return nil
}

type Target struct {
	SessionID string `json:"sessionId,omitempty"`
	// Runtime and Model select read-only provider discovery through settings.get.
	// They cannot be combined with SessionID or used by settings.configure.
	Runtime string `json:"runtime,omitempty"`
	Model   string `json:"model,omitempty"`
}
type ConfigureParams struct {
	Target           Target   `json:"target,omitempty"`
	Patch            Patch    `json:"patch"`
	Reset            []string `json:"reset,omitempty"`
	ExpectedRevision *uint64  `json:"expectedRevision,omitempty"`
}

// Field describes a field for settings interfaces. When describes the global
// lifetime; scoped snapshots can refine it using adapter capabilities.
type Field struct {
	Type        string          `json:"type"`
	Unit        string          `json:"unit,omitempty"`
	When        string          `json:"when"`
	Scopes      []string        `json:"scopes"`
	Nullable    bool            `json:"nullable,omitempty"`
	Minimum     *int64          `json:"minimum,omitempty"`
	Writable    bool            `json:"writable"`
	Description string          `json:"description,omitempty"`
	Choices     []config.Choice `json:"choices,omitempty"`
	AllowCustom bool            `json:"allowCustom,omitempty"`
	Source      string          `json:"source,omitempty"`
	Default     json.RawMessage `json:"default,omitempty"`
}
type Snapshot struct {
	Warnings []string         `json:"warnings,omitempty"`
	Target   Target           `json:"target,omitempty"`
	Revision uint64           `json:"revision"`
	Values   Values           `json:"values"`
	Fields   map[string]Field `json:"fields"`
	Deferred []string         `json:"deferred,omitempty"`
	// Resync means a slow observer missed one or more notifications, possibly
	// for another target. Refresh displayed targets with settings.get.
	Resync bool `json:"resync,omitempty"`
}

func Defaults() Values {
	pageBytes := int64(1024 * 1024)
	return Values{
		Hooks:      hooks.Defaults(),
		Compaction: config.CompactionDefaults(),
		Git:        workspace.GitDefaults(),
		Workspace:  workspace.Defaults(),
		Process:    Process{Store: "memory", Runtimes: []string{}, ShutdownTimeoutMs: 10000},
		Logging:    Logging{Level: "info", MaxQueueRecords: 256, MaxQueueBytes: 1048576, MaxRecordBytes: 65536},
		Memory:     Memory{EventLogSize: 2048, MaxClosedSessions: 64},
		History:    History{MaxPageEvents: 200, MaxPageBytes: &pageBytes},
		Streams:    Streams{MaxQueueEvents: 4096},
	}
}

func Describe() map[string]Field {
	out := map[string]Field{}
	add := func(path, typ, unit, when string, nullable bool, min *int64, scopes ...string) {
		out[path] = Field{Type: typ, Unit: unit, When: when, Nullable: nullable, Minimum: min, Writable: when != "restart", Scopes: scopes}
	}
	zero, one := int64(0), int64(1)
	add("hooks.maxConcurrent", "integer", "handlers", "nextOperation", false, &one, "global")
	add("hooks.enabled", "boolean", "", "nextOperation", false, nil, "global")
	add("hooks.timeoutMs", "integer", "milliseconds", "nextOperation", false, &one, "global")
	add("hooks.onError", "string", "", "nextOperation", false, nil, "global")
	add("hooks.required", "array", "", "nextOperation", false, nil, "global")
	add("compaction.autoAtTokens", "integer", "tokens", "newSession", true, &one, "global", "session", "turn")
	add("compaction.autoAtPercent", "number", "percent", "newSession", true, &zero, "global", "session", "turn")
	add("compaction.timeoutMs", "integer", "milliseconds", "newSession", false, &one, "global", "session", "turn")
	for _, p := range []string{"readOnly", "allowRemote", "allowDuringTurn"} {
		add("git."+p, "boolean", "", "nextOperation", false, nil, "global")
	}
	for _, p := range []string{"hooks", "signing"} {
		add("git."+p, "string", "", "nextOperation", false, nil, "global")
	}
	add("git.timeoutMs", "integer", "milliseconds", "nextOperation", false, &one, "global")
	add("git.maxOutputBytes", "integer", "bytes", "nextOperation", false, &one, "global")
	add("git.maxFiles", "integer", "files", "nextOperation", false, &one, "global")
	add("git.maxLogEntries", "integer", "commits", "nextOperation", false, &one, "global")
	for _, p := range []string{"mode", "repo", "base", "path", "root"} {
		add("workspace."+p, "string", "", "newSession", false, nil, "global", "session")
	}
	add("workspace.captureDiff", "boolean", "", "newSession", false, nil, "global", "session", "turn")
	for _, p := range []string{"maxDiffBytes", "maxSnapshotBytes"} {
		add("workspace."+p, "integer", "bytes", "newSession", false, &one, "global", "session", "turn")
	}
	add("workspace.maxFiles", "integer", "files", "newSession", false, &one, "global", "session", "turn")
	add("workspace.timeoutMs", "integer", "milliseconds", "newSession", false, &one, "global", "session", "turn")
	for _, p := range []string{"store", "codexPath", "claudePath", "gitPath", "settingsPath"} {
		add("process."+p, "string", "", "restart", false, nil, "global")
	}
	add("process.runtimes", "array", "", "restart", false, nil, "global")
	add("process.acp", "object", "", "restart", false, nil, "global")
	add("process.fakeDelayMs", "integer", "milliseconds", "restart", false, &zero, "global")
	add("process.shutdownTimeoutMs", "integer", "milliseconds", "nextShutdown", false, &one, "global")
	for _, p := range []string{"kitMcp", "allowUnsafe"} {
		add("process."+p, "boolean", "", "restart", false, nil, "global")
	}
	add("logging.level", "string", "", "live", false, nil, "global")
	add("logging.maxQueueRecords", "integer", "records", "live", false, &one, "global")
	add("logging.maxQueueBytes", "integer", "bytes", "live", false, &one, "global")
	add("logging.maxRecordBytes", "integer", "bytes", "live", false, &one, "global")
	add("approvals.timeoutMs", "integer", "milliseconds", "nextApproval", false, &zero, "global")
	for _, p := range []string{"maxCacheBytes", "maxSessionCacheBytes"} {
		add("memory."+p, "integer", "bytes", "live", true, &zero, "global")
	}
	add("memory.maxCachedSessions", "integer", "sessions", "live", true, &zero, "global")
	add("memory.pinned", "boolean", "", "live", false, nil, "session")
	for _, p := range []string{"evictAfterIdleMs", "suspendAfterIdleMs"} {
		add("memory."+p, "integer", "milliseconds", "live", true, &zero, "global")
	}
	add("memory.resumeOnPrompt", "boolean", "", "live", false, nil, "global")
	add("memory.eventLogSize", "integer", "events", "live", false, &zero, "global")
	add("memory.maxClosedSessions", "integer", "sessions", "live", false, &zero, "global")
	add("history.maxPageEvents", "integer", "events", "nextRequest", false, &one, "global")
	add("history.maxPageBytes", "integer", "bytes", "nextRequest", true, &one, "global")
	add("streams.maxQueueEvents", "integer", "events", "live", false, &one, "global")
	add("streams.maxQueueBytes", "integer", "bytes", "live", true, &zero, "global")
	for _, p := range []string{"runtime", "cwd", "resumeId", "instructions"} {
		add("session."+p, "string", "", "newSession", false, nil, "global", "session")
	}
	for _, p := range []string{"model", "effort"} {
		add("session."+p, "string", "", "newSession", false, nil, "global", "session", "turn")
	}
	for _, p := range []string{"rawEvents", "disableHostTools"} {
		add("session."+p, "boolean", "", "newSession", false, nil, "global", "session")
	}
	add("session.mcp", "object", "", "newSession", true, nil, "global", "session")
	add("session.hostTools", "array", "", "newSession", true, nil, "global", "session")
	add("session.options", "object", "", "newSession", false, nil, "global", "session", "turn")
	for _, field := range []string{"maxSessionTokens", "maxTurnTokens"} {
		scopes := []string{"global", "session"}
		if field == "maxTurnTokens" {
			scopes = append(scopes, "turn")
		}
		add("budget."+field, "integer", "tokens", "newSession", true, &zero, scopes...)
	}
	for _, field := range []string{"maxSessionCostUsd", "maxTurnCostUsd"} {
		scopes := []string{"global", "session"}
		if field == "maxTurnCostUsd" {
			scopes = append(scopes, "turn")
		}
		add("budget."+field, "number", "USD", "newSession", true, &zero, scopes...)
	}
	return out
}

// Clone returns an independent document, including native option values.
func (v Values) Clone() Values {
	out, err := Clone(v)
	if err != nil {
		panic(err)
	}
	return out
}

// Clone validates JSON representability before copying caller-owned values.
func Clone(v Values) (Values, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return Values{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	var out Values
	if err := json.Unmarshal(b, &out); err != nil {
		return Values{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return out, nil
}

func object(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var out map[string]json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil || out == nil {
		return nil, fmt.Errorf("%w: expected an object", ErrInvalid)
	}
	return out, nil
}

func document(v Values) (map[string]json.RawMessage, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return object(b)
}

// Apply validates and snapshots a patch. It does not enforce target lifetimes;
// the owner does that atomically with publishing the accepted revision.
func Apply(base Values, patch Patch, reset []string, defaults Values) (Values, []string, error) {
	if patch == nil {
		return Values{}, nil, fmt.Errorf("%w: patch is required", ErrInvalid)
	}
	doc, err := document(base)
	if err != nil {
		return Values{}, nil, err
	}
	parent, err := document(defaults)
	if err != nil {
		return Values{}, nil, err
	}
	fields := Describe()
	changes := map[string]json.RawMessage{}
	for group, raw := range patch {
		if _, ok := doc[group]; !ok {
			return Values{}, nil, fmt.Errorf("%w: unknown settings group %s", ErrInvalid, group)
		}
		values, e := object(raw)
		if e != nil {
			return Values{}, nil, fmt.Errorf("%w: %s must be an object", ErrInvalid, group)
		}
		for key, value := range values {
			path := group + "." + key
			if _, ok := fields[path]; !ok {
				return Values{}, nil, fmt.Errorf("%w: unknown field %s", ErrInvalid, path)
			}
			if path == "session.options" {
				opts, e := object(value)
				if e != nil {
					return Values{}, nil, fmt.Errorf("%w: session.options must be an object", ErrInvalid)
				}
				for name, v := range opts {
					if name == "" {
						return Values{}, nil, fmt.Errorf("%w: empty option name", ErrInvalid)
					}
					changes[path+"."+name] = v
				}
			} else {
				changes[path] = value
			}
		}
	}
	seen := map[string]bool{}
	for _, path := range reset {
		if _, ok := fields[path]; !ok && !(strings.HasPrefix(path, "session.options.") && len(path) > len("session.options.")) {
			return Values{}, nil, fmt.Errorf("%w: unknown reset field %s", ErrInvalid, path)
		}
		if _, ok := changes[path]; ok || seen[path] {
			return Values{}, nil, fmt.Errorf("%w: %s is set/reset more than once", ErrInvalid, path)
		}
		if path == "session.options" {
			for p := range changes {
				if strings.HasPrefix(p, "session.options.") {
					return Values{}, nil, fmt.Errorf("%w: overlapping options reset", ErrInvalid)
				}
			}
		}
		for p := range seen {
			if path == "session.options" && strings.HasPrefix(p, "session.options.") || p == "session.options" && strings.HasPrefix(path, "session.options.") {
				return Values{}, nil, fmt.Errorf("%w: overlapping options reset", ErrInvalid)
			}
		}
		seen[path] = true
	}
	set := func(path string, value json.RawMessage, remove bool) error {
		parts := strings.SplitN(path, ".", 3)
		group, e := object(doc[parts[0]])
		if e != nil {
			return e
		}
		if len(parts) == 3 {
			opts := map[string]json.RawMessage{}
			if len(group[parts[1]]) > 0 && string(group[parts[1]]) != "null" {
				if err := json.Unmarshal(group[parts[1]], &opts); err != nil {
					return err
				}
			}
			if remove {
				delete(opts, parts[2])
			} else {
				opts[parts[2]] = bytes.Clone(value)
			}
			group[parts[1]], e = json.Marshal(opts)
		} else if remove {
			delete(group, parts[1])
		} else {
			group[parts[1]] = bytes.Clone(value)
		}
		if e != nil {
			return e
		}
		doc[parts[0]], e = json.Marshal(group)
		return e
	}
	paths := make([]string, 0, len(changes)+len(reset))
	for _, path := range reset {
		parts := strings.SplitN(path, ".", 3)
		group, _ := object(parent[parts[0]])
		value, exists := group[parts[1]]
		if len(parts) == 3 {
			opts := map[string]json.RawMessage{}
			_ = json.Unmarshal(value, &opts)
			value, exists = opts[parts[2]]
		}
		if err := set(path, value, !exists); err != nil {
			return Values{}, nil, err
		}
		paths = append(paths, path)
	}
	for path, value := range changes {
		if field, ok := fields[path]; ok && bytes.Equal(bytes.TrimSpace(value), []byte("null")) && !field.Nullable {
			return Values{}, nil, fmt.Errorf("%w: %s cannot be null", ErrInvalid, path)
		}
		if err := set(path, value, false); err != nil {
			return Values{}, nil, err
		}
		paths = append(paths, path)
	}
	b, err := json.Marshal(doc)
	if err != nil {
		return Values{}, nil, err
	}
	var out Values
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&out); err != nil {
		return Values{}, nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := Validate(out); err != nil {
		return Values{}, nil, err
	}
	slices.Sort(paths)
	return out, paths, nil
}

func Validate(v Values) error {
	if err := v.Hooks.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := v.Compaction.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := workspace.Validate(v.Workspace); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := workspace.ValidateGit(v.Git); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := validateACP(v.Process.ACP); err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	doc, _ := object(b)
	for path, field := range Describe() {
		parts := strings.SplitN(path, ".", 2)
		group, _ := object(doc[parts[0]])
		value, ok := group[parts[1]]
		if !ok || bytes.Equal(value, []byte("null")) {
			continue
		}
		if field.Type == "number" {
			var n float64
			if err := json.Unmarshal(value, &n); err != nil || n < 0 || n > 9007199254740991 {
				return fmt.Errorf("%w: %s must be a nonnegative finite number", ErrInvalid, path)
			}
		}
		if field.Type == "integer" {
			var n int64
			if err := json.Unmarshal(value, &n); err != nil {
				return fmt.Errorf("%w: %s must be an integer", ErrInvalid, path)
			}
			maxValue := int64(9007199254740991)
			if field.Unit == "milliseconds" {
				maxValue = 9223372036854
			}
			if field.Minimum != nil && n < *field.Minimum || n > maxValue {
				return fmt.Errorf("%w: %s is out of range", ErrInvalid, path)
			}
		}
	}
	switch v.Logging.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("%w: logging.level must be debug, info, warn or error", ErrInvalid)
	}
	if v.Session.MCP != nil {
		if err := v.Session.MCP.Validate(); err != nil {
			return fmt.Errorf("%w: %v", ErrInvalid, err)
		}
	}
	if _, err := v.Session.SessionConfig.Settings(); err != nil {
		return err
	}
	return nil
}

// Decode parses a settings document as a patch over the supplied defaults.
// Hosts may use it for a file, environment value or UI input without creating
// separate precedence or validation rules in the core.
func Decode(r io.Reader, defaults Values) (Values, error) {
	var patch Patch
	d := json.NewDecoder(r)
	if err := d.Decode(&patch); err != nil {
		return Values{}, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return Values{}, fmt.Errorf("%w: trailing settings data", ErrInvalid)
	}
	v, _, err := Apply(defaults, patch, nil, defaults)
	return v, err
}
