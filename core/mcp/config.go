// Package mcp holds Harness Kit's portable MCP server configuration: one
// definition that every runtime adapter translates into its own per-session
// mechanism (Codex thread config, Claude Code --mcp-config). Harness Kit
// never writes the runtimes' own configuration files; servers configured
// natively in a runtime keep loading unless a session opts out with
// InheritNative.
//
// The format is the familiar "mcpServers" JSON object, with a few portable
// extras (tool filters, approval, runtime scoping, timeouts):
//
//	{
//	  "mcpServers": {
//	    "github": {
//	      "command": "npx", "args": ["-y", "@modelcontextprotocol/server-github"],
//	      "env": {"GITHUB_TOKEN": "${GITHUB_TOKEN}"},
//	      "tools": {"allow": ["search_code"]}
//	    },
//	    "docs": {"type": "http", "url": "https://mcp.example.com/mcp",
//	             "headers": {"Authorization": "Bearer ${DOCS_TOKEN}"}}
//	  }
//	}
//
// Secrets are referenced as ${VAR} or ${VAR:-default} and resolved from the
// environment when a session starts; they are never written to disk.
package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Approval says whether a server's tools need the host's approval.
type Approval string

const (
	// ApprovalAuto runs the server's (allowed) tools without asking. Default.
	ApprovalAuto Approval = "auto"
	// ApprovalAsk asks the host before each tool call, where the runtime
	// supports it.
	ApprovalAsk Approval = "ask"
)

// Config is a set of MCP servers.
type Config struct {
	Servers map[string]Server `json:"mcpServers,omitempty"`
	// InheritNative controls whether servers configured in the runtime
	// itself also load. Nil means yes.
	InheritNative *bool `json:"inheritNative,omitempty"`
}

// Server is one MCP server: a local process (stdio) or a remote endpoint
// (streamable HTTP).
type Server struct {
	// Type is "stdio" (default) or "http" ("streamable-http" is accepted).
	Type    string            `json:"type,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// Tools filters the server's tools by exact name.
	Tools ToolFilter `json:"tools,omitzero"`
	// Approval defaults to ApprovalAuto.
	Approval Approval `json:"approval,omitempty"`
	// Runtimes limits the server to these runtimes; empty means all.
	Runtimes          []string `json:"runtimes,omitempty"`
	StartupTimeoutSec float64  `json:"startupTimeoutSec,omitempty"`
	ToolTimeoutSec    float64  `json:"toolTimeoutSec,omitempty"`
	// Disabled removes the server, including one defined in a lower layer.
	Disabled bool `json:"disabled,omitempty"`
	// Source is set by Resolve: "user", "project" or "session".
	Source string `json:"source,omitempty"`
}

// ToolFilter limits which tools of a server may be used.
type ToolFilter struct {
	// Allow, if non-empty, is the complete list of usable tools.
	Allow []string `json:"allow,omitempty"`
	// Deny lists tools that may never be used.
	Deny []string `json:"deny,omitempty"`
}

// IsHTTP reports whether s is a remote (streamable HTTP) server.
func (s Server) IsHTTP() bool { return s.Type == "http" || s.Type == "streamable-http" }

// EffectiveApproval returns s.Approval or the default.
func (s Server) EffectiveApproval() Approval {
	if s.Approval == "" {
		return ApprovalAuto
	}
	return s.Approval
}

// AppliesTo reports whether s should load for the named runtime.
func (s Server) AppliesTo(runtime string) bool {
	return !s.Disabled && (len(s.Runtimes) == 0 || slices.Contains(s.Runtimes, runtime))
}

// Inherit reports whether native servers load (InheritNative, default true).
func (c Config) Inherit() bool { return c.InheritNative == nil || *c.InheritNative }

// Names returns the server names in sorted order.
func (c Config) Names() []string {
	names := make([]string, 0, len(c.Servers))
	for n := range c.Servers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// IsZero reports whether c configures nothing.
func (c Config) IsZero() bool { return len(c.Servers) == 0 && c.InheritNative == nil }

// Server names are restricted so they are valid in every runtime: Codex
// config keys and Claude Code's mcp__<server>__<tool> tool names (which
// reserve "__").
var validName = regexp.MustCompile(`^[A-Za-z0-9-]+(_[A-Za-z0-9-]+)*$`)

// ReservedName is the server name Harness Kit uses for its own tool bridge.
const ReservedName = "harness_kit"

// ValidName reports whether name is usable as a server name.
func ValidName(name string) bool { return len(name) <= 64 && validName.MatchString(name) }

// Validate checks c for errors.
func (c Config) Validate() error {
	var errs []error
	for _, name := range c.Names() {
		if err := c.Servers[name].validate(name); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s Server) validate(name string) error {
	if !ValidName(name) {
		return fmt.Errorf("mcp server %q: names may contain letters, digits, '-' and single '_'", name)
	}
	if name == ReservedName {
		return fmt.Errorf("mcp server %q: the name is reserved for Harness Kit's own tools", name)
	}
	if s.Disabled {
		return nil // a tombstone needs no definition
	}
	switch s.Type {
	case "", "stdio":
		if s.Command == "" {
			return fmt.Errorf("mcp server %q: stdio servers need a command", name)
		}
		if s.URL != "" || len(s.Headers) > 0 {
			return fmt.Errorf("mcp server %q: url and headers need \"type\": \"http\"", name)
		}
	case "http", "streamable-http":
		if s.URL == "" {
			return fmt.Errorf("mcp server %q: http servers need a url", name)
		}
		if !strings.Contains(s.URL, "${") {
			u, err := url.Parse(s.URL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return fmt.Errorf("mcp server %q: invalid url %q", name, s.URL)
			}
		}
		if s.Command != "" || len(s.Args) > 0 || len(s.Env) > 0 || s.Cwd != "" {
			return fmt.Errorf("mcp server %q: command, args, env and cwd apply to stdio servers only", name)
		}
	case "sse":
		return fmt.Errorf("mcp server %q: the legacy SSE transport is not supported; use \"type\": \"http\" (streamable HTTP)", name)
	default:
		return fmt.Errorf("mcp server %q: unknown type %q (use stdio or http)", name, s.Type)
	}
	switch s.Approval {
	case "", ApprovalAuto, ApprovalAsk:
	default:
		return fmt.Errorf("mcp server %q: approval must be auto or ask", name)
	}
	for _, t := range append(slices.Clone(s.Tools.Allow), s.Tools.Deny...) {
		if t == "" || strings.ContainsAny(t, " *?") {
			return fmt.Errorf("mcp server %q: tool filters take exact tool names, got %q", name, t)
		}
	}
	for _, rt := range s.Runtimes {
		if rt == "" {
			return fmt.Errorf("mcp server %q: empty runtime name", name)
		}
	}
	if s.StartupTimeoutSec < 0 || s.ToolTimeoutSec < 0 {
		return fmt.Errorf("mcp server %q: timeouts must be positive", name)
	}
	for _, v := range s.refs() {
		if _, err := Expand(v, func(string) (string, bool) { return "x", true }); err != nil {
			return fmt.Errorf("mcp server %q: %w", name, err)
		}
	}
	return nil
}

// Parse decodes and validates a config. Unknown fields are rejected so
// typos surface.
func Parse(data []byte) (Config, error) {
	var c Config
	if len(bytes.TrimSpace(data)) == 0 {
		return c, nil
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, err
	}
	return c, c.Validate()
}

// Merge combines layers; later layers win. A server entry replaces a lower
// entry of the same name entirely (fields are not merged), and a Disabled
// entry removes it. The last non-nil InheritNative wins.
func Merge(layers ...Config) Config {
	out := Config{Servers: map[string]Server{}}
	for _, l := range layers {
		for name, s := range l.Servers {
			if s.Disabled {
				delete(out.Servers, name)
				continue
			}
			out.Servers[name] = s
		}
		if l.InheritNative != nil {
			v := *l.InheritNative
			out.InheritNative = &v
		}
	}
	return out
}

// ForRuntime returns the servers that apply to runtime.
func (c Config) ForRuntime(runtime string) Config {
	out := Config{InheritNative: c.InheritNative}
	for name, s := range c.Servers {
		if s.AppliesTo(runtime) {
			if out.Servers == nil {
				out.Servers = map[string]Server{}
			}
			out.Servers[name] = s
		}
	}
	return out
}

func (s Server) refs() []string {
	v := []string{s.Command, s.Cwd, s.URL}
	v = append(v, s.Args...)
	v = append(v, slices.Collect(maps.Values(s.Env))...)
	return append(v, slices.Collect(maps.Values(s.Headers))...)
}

var ref = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}`)

// Expand replaces ${NAME} and ${NAME:-default} in s using lookup. A
// variable that is unset and has no default is an error.
func Expand(s string, lookup func(string) (string, bool)) (string, error) {
	if !strings.Contains(s, "${") {
		return s, nil
	}
	var missing []string
	out := ref.ReplaceAllStringFunc(s, func(m string) string {
		g := ref.FindStringSubmatch(m)
		if v, ok := lookup(g[1]); ok {
			return v
		}
		if g[2] != "" {
			return g[3]
		}
		missing = append(missing, g[1])
		return ""
	})
	if strings.Contains(ref.ReplaceAllString(s, ""), "${") {
		return "", fmt.Errorf("malformed variable reference in %q (use ${NAME} or ${NAME:-default})", s)
	}
	if len(missing) > 0 {
		return "", fmt.Errorf("environment variable %s is not set", strings.Join(missing, ", "))
	}
	return out, nil
}

// Expanded returns s with every ${VAR} reference resolved.
func (s Server) Expanded(lookup func(string) (string, bool)) (Server, error) {
	var err error
	exp := func(v string) string {
		if err != nil {
			return v
		}
		var out string
		out, err = Expand(v, lookup)
		return out
	}
	o := s
	o.Command, o.Cwd, o.URL = exp(s.Command), exp(s.Cwd), exp(s.URL)
	o.Args = make([]string, len(s.Args))
	for i, a := range s.Args {
		o.Args[i] = exp(a)
	}
	if s.Env != nil {
		o.Env = make(map[string]string, len(s.Env))
		for k, v := range s.Env {
			o.Env[k] = exp(v)
		}
	}
	if s.Headers != nil {
		o.Headers = make(map[string]string, len(s.Headers))
		for k, v := range s.Headers {
			o.Headers[k] = exp(v)
		}
	}
	return o, err
}

// LookupWith returns an environment lookup over extra KEY=VALUE entries
// (which win) and then base.
func LookupWith(base func(string) (string, bool), extra []string) func(string) (string, bool) {
	m := map[string]string{}
	for _, kv := range extra {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	return func(k string) (string, bool) {
		if v, ok := m[k]; ok {
			return v, true
		}
		return base(k)
	}
}
