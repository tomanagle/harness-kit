package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseAndValidate(t *testing.T) {
	good := `{"mcpServers": {
		"github": {"command": "npx", "args": ["-y", "srv"], "env": {"TOKEN": "${GH_TOKEN}"}, "tools": {"allow": ["search"]}, "approval": "ask"},
		"docs": {"type": "http", "url": "https://mcp.example.com/mcp", "headers": {"Authorization": "Bearer ${DOCS:-none}"}},
		"alias": {"type": "streamable-http", "url": "http://localhost:3000/mcp"}
	}, "inheritNative": false}`
	c, err := Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Servers) != 3 || c.Inherit() || !c.Servers["docs"].IsHTTP() || !c.Servers["alias"].IsHTTP() || c.Servers["github"].EffectiveApproval() != ApprovalAsk {
		t.Fatalf("parsed: %+v", c)
	}
	if c.Servers["docs"].EffectiveApproval() != ApprovalAuto {
		t.Fatal("approval defaults to auto")
	}

	bad := map[string]string{
		"unknown field":     `{"mcpServers": {"a": {"command": "x", "comand": "y"}}}`,
		"no command":        `{"mcpServers": {"a": {}}}`,
		"http without url":  `{"mcpServers": {"a": {"type": "http"}}}`,
		"stdio with url":    `{"mcpServers": {"a": {"command": "x", "url": "https://x"}}}`,
		"http with command": `{"mcpServers": {"a": {"type": "http", "url": "https://x", "command": "y"}}}`,
		"sse":               `{"mcpServers": {"a": {"type": "sse", "url": "https://x"}}}`,
		"bad scheme":        `{"mcpServers": {"a": {"type": "http", "url": "ftp://x"}}}`,
		"double underscore": `{"mcpServers": {"a__b": {"command": "x"}}}`,
		"dot in name":       `{"mcpServers": {"a.b": {"command": "x"}}}`,
		"bad approval":      `{"mcpServers": {"a": {"command": "x", "approval": "always"}}}`,
		"glob tool":         `{"mcpServers": {"a": {"command": "x", "tools": {"allow": ["get_*"]}}}}`,
		"malformed ref":     `{"mcpServers": {"a": {"command": "x", "env": {"K": "${OOPS"}}}}`,
	}
	for name, src := range bad {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if c, err := Parse(nil); err != nil || !c.IsZero() {
		t.Fatalf("empty: %+v %v", c, err)
	}
}

func TestExpand(t *testing.T) {
	env := map[string]string{"A": "1", "EMPTY": ""}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	cases := map[string]string{
		"plain":                 "plain",
		"${A}":                  "1",
		"Bearer ${A}":           "Bearer 1",
		"${MISSING:-dflt}":      "dflt",
		"${EMPTY:-dflt}":        "",
		"${A}-${MISSING:-x}/$5": "1-x/$5",
	}
	for in, want := range cases {
		if got, err := Expand(in, lookup); err != nil || got != want {
			t.Errorf("Expand(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := Expand("${MISSING}", lookup); err == nil || !strings.Contains(err.Error(), "MISSING") {
		t.Fatalf("missing var: %v", err)
	}
	if _, err := Expand("${bad", lookup); err == nil {
		t.Fatal("malformed reference should fail")
	}

	s := Server{Command: "${A}/bin", Args: []string{"--x=${A}"}, Env: map[string]string{"K": "${A}"}}
	e, err := s.Expanded(LookupWith(lookup, []string{"A=override"}))
	if err != nil || e.Command != "override/bin" || e.Args[0] != "--x=override" || e.Env["K"] != "override" || s.Env["K"] != "${A}" {
		t.Fatalf("expanded: %+v %v (original %+v)", e, err, s)
	}
}

func TestMergeAndRuntimeFilter(t *testing.T) {
	f := false
	user := Config{Servers: map[string]Server{"a": {Command: "user-a"}, "b": {Command: "user-b"}, "c": {Command: "c", Runtimes: []string{"claude"}}}}
	project := Config{Servers: map[string]Server{"a": {Command: "project-a"}, "b": {Disabled: true}}, InheritNative: &f}
	session := Config{Servers: map[string]Server{"d": {Command: "d"}}}
	m := Merge(user, project, session)
	if m.Servers["a"].Command != "project-a" || len(m.Servers) != 3 || m.Inherit() {
		t.Fatalf("merge: %+v", m)
	}
	if _, ok := m.Servers["b"]; ok {
		t.Fatal("disabled entry must remove the lower definition")
	}
	codex := m.ForRuntime("codex")
	if _, ok := codex.Servers["c"]; ok || len(codex.Servers) != 2 || codex.Inherit() {
		t.Fatalf("for codex: %+v", codex)
	}
	if _, ok := m.ForRuntime("claude").Servers["c"]; !ok {
		t.Fatal("claude-scoped server missing for claude")
	}
}

func TestResolveWithTrust(t *testing.T) {
	dir := t.TempDir()
	userFile := filepath.Join(dir, "config", "config.json")
	if _, err := SaveFile(userFile, Config{Servers: map[string]Server{"shared": {Command: "user"}, "u": {Command: "u"}}}); err != nil {
		t.Fatal(err)
	}
	proj := filepath.Join(dir, "repo")
	sub := filepath.Join(proj, "pkg", "deep")
	os.MkdirAll(sub, 0o755)
	projFile := filepath.Join(proj, ProjectFileName)
	content, err := SaveFile(projFile, Config{Servers: map[string]Server{"shared": {Command: "project"}, "p": {Command: "p"}}})
	if err != nil {
		t.Fatal(err)
	}
	trust := TrustStore{Path: filepath.Join(dir, "config", "trust.json")}
	r := Resolver{UserFile: userFile, Project: true, Trust: trust}

	// Untrusted: project servers skipped (from a subdirectory, too).
	res, err := r.Resolve(sub, "codex", Config{})
	if err != nil {
		t.Fatal(err)
	}
	if res.ProjectFile != projFile || res.ProjectTrusted || len(res.Warnings) != 1 || res.Config.Servers["shared"].Command != "user" {
		t.Fatalf("untrusted: %+v", res)
	}
	if _, ok := res.Config.Servers["p"]; ok {
		t.Fatal("untrusted project server loaded")
	}

	// Trusted: project wins over user, session wins over both.
	if err := trust.Trust(projFile, content); err != nil {
		t.Fatal(err)
	}
	res, _ = r.Resolve(sub, "codex", Config{Servers: map[string]Server{"s": {Command: "s"}}})
	if !res.ProjectTrusted || res.Config.Servers["shared"].Command != "project" || res.Config.Servers["shared"].Source != "project" ||
		res.Config.Servers["s"].Source != "session" || res.Config.Servers["u"].Source != "user" {
		t.Fatalf("trusted: %+v", res)
	}

	// Editing the file revokes trust.
	os.WriteFile(projFile, append(content, ' '), 0o600)
	if res, _ := r.Resolve(proj, "codex", Config{}); res.ProjectTrusted {
		t.Fatal("changed file must need re-trust")
	}
	trust.Untrust(projFile)

	// Malformed files are errors, not silent skips.
	os.WriteFile(userFile, []byte(`{"mcpServers": {"x": {}}}`), 0o600)
	if _, err := r.Resolve(proj, "codex", Config{}); err == nil {
		t.Fatal("malformed user config should fail")
	}
	// Without a trust store nothing is trusted, and a missing user file is fine.
	res, err = Resolver{UserFile: filepath.Join(dir, "nope.json"), Project: true}.Resolve(proj, "codex", Config{})
	if err != nil || res.ProjectTrusted {
		t.Fatalf("no trust store: %+v %v", res, err)
	}
}

func TestSaveFileStripsSource(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	b, err := SaveFile(p, Config{Servers: map[string]Server{"a": {Command: "x", Source: "user"}}})
	if err != nil || strings.Contains(string(b), "source") {
		t.Fatalf("%s %v", b, err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
}
