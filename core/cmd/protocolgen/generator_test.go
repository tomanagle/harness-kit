package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func checkout(t *testing.T) string {
	t.Helper()
	root, err := repoRoot()
	if err != nil {
		t.Skip("generator contract tests require the complete source checkout")
	}
	return root
}

func contract(t *testing.T) *Document {
	t.Helper()
	doc, err := buildSchema(checkout(t))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestProtocolSemantics(t *testing.T) {
	d := contract(t)
	// The empty request field and an explicitly false filter have different
	// meanings. Config clears use empty strings, not null.
	config := d.Defs["ConfigPatch"]
	if slices.Contains(config.Required, "model") || len(config.Properties["model"].AnyOf) > 0 || config.Properties["model"].GoType != "*string" {
		t.Fatal("config patch lost omission/explicit empty string semantics")
	}
	filter := d.Defs["ListSessionsOptions"].Properties["archived"]
	if filter.GoType != "*bool" || filter.Type != "boolean" {
		t.Fatal("archived false must remain distinct from omission")
	}
	required := d.Defs["SessionConfigureParams"]
	if !slices.Contains(required.Required, "config") || required.Properties["config"].Ref != "#/$defs/ConfigPatch" || len(required.Properties["config"].AnyOf) > 0 {
		t.Fatal("session.configure requires a non-null patch")
	}
	if slices.Contains(d.Defs["CreateSessionParams"].Required, "runtime") {
		t.Fatal("runtime may inherit host settings")
	}
	for _, name := range []string{"MemorySettings", "BudgetLimits"} {
		field := "maxCacheBytes"
		if name == "BudgetLimits" {
			field = "maxTurnTokens"
		}
		prop := d.Defs[name].Properties[field]
		if !slices.Contains(d.Defs[name].Required, field) || len(prop.AnyOf) != 2 || prop.AnyOf[1].Type != "null" {
			t.Fatalf("%s.%s must be a present nullable limit", name, field)
		}
		if len(d.Defs[name+"Patch"].Required) != 0 {
			t.Fatal("patch fields must be optional")
		}
	}
	if d.Defs["SettingsPatch"].Properties["memory"].Ref != "#/$defs/MemorySettingsPatch" {
		t.Fatal("settings groups must be derived from Values")
	}
	if d.Defs["ToolCallParams"].Properties["arguments"].Type != "" {
		t.Fatal("RawMessage must remain arbitrary JSON")
	}
	if d.Defs["SessionInfo"].Properties["createdAt"].Format != "date-time" {
		t.Fatal("time must be an RFC3339 string")
	}
	if d.Defs["SessionSettings"].Properties["cwd"] == nil || d.Defs["SessionForkParams"].Properties["title"] == nil {
		t.Fatal("embedded Go fields must flatten into the wire object")
	}
	if d.Methods["session.events"].Result.Ref != d.Methods["session.eventsPage"].Result.Ref {
		t.Fatal("both history methods return the same page type")
	}
	if d.Constants["CodeForkSourceChanged"] != int64(-32018) || d.Constants["CodeBudgetExceeded"] != int64(-32019) {
		t.Fatal("all protocol error codes must be included")
	}
}

func TestSchemaAloneReproducesClients(t *testing.T) {
	d := contract(t)
	before, err := render(d)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Document
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	after, err := render(&decoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("client generation depends on state outside the schema")
	}
	root := checkout(t)
	for path, content := range after {
		committed, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(content, committed) {
			t.Errorf("%s is stale; run make generate", path)
		}
	}
}

func TestEventEnvelopeChangesReachBothClients(t *testing.T) {
	d := contract(t)
	prop := &Schema{Type: "string", GoType: "string", GoName: "TraceID", GoJSONTag: "traceId,omitempty"}
	d.Defs["HarnessEvent"].Properties["traceId"] = prop
	for _, variant := range d.Defs["HarnessEvent"].OneOf {
		variant.Properties["traceId"] = prop
	}
	outputs, err := render(d)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(outputs["packages/client-ts/src/protocol.ts"], []byte(`"traceId"?: string`)) {
		t.Fatal("new event envelope fields did not reach TS union")
	}
	if !bytes.Contains(outputs["packages/client-go/protocol.gen.go"], []byte(`json:"traceId,omitempty"`)) {
		t.Fatal("new event envelope fields did not reach Go")
	}
	variants := d.Defs["HarnessEvent"].OneOf
	if len(variants) != len(d.Events)+1 || variants[len(variants)-1].Properties["type"].Not == nil {
		t.Fatal("schema must allow future event kinds without weakening known payloads")
	}
}

func TestSchemaReferencesResolve(t *testing.T) {
	d := contract(t)
	var visit func(*Schema)
	visit = func(s *Schema) {
		if s == nil {
			return
		}
		if s.Ref != "" && d.Defs[refName(s)] == nil {
			t.Errorf("unresolved ref %s", s.Ref)
		}
		for _, p := range s.Properties {
			visit(p)
		}
		for _, p := range s.AnyOf {
			visit(p)
		}
		for _, p := range s.OneOf {
			visit(p)
		}
		visit(s.Items)
		visit(s.AdditionalProperties)
		visit(s.Not)
	}
	for _, s := range d.Defs {
		visit(s)
	}
	for _, m := range d.Methods {
		visit(m.Params)
		visit(m.Result)
	}
	for _, m := range d.Requests {
		visit(m.Params)
		visit(m.Result)
	}
	for _, s := range d.Notifications {
		visit(s)
	}
}

func TestUnsupportedWireConstantsFailClosed(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"agent", "budget", "config", "event", "mcp", "permission", "rpc/protocol", "settings", "session", "tools", "workspace", "internal/jsonrpc"} {
		if err := os.MkdirAll(filepath.Join(root, "core", dir), 0755); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(root, "core/rpc/protocol/protocol.go")
	if err := os.WriteFile(path, []byte("package protocol\nconst MethodNew = \"session.\" + \"new\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := readSource(root)
	if err == nil || !strings.Contains(err.Error(), "MethodNew") {
		t.Fatalf("unsupported method constant silently lost: %v", err)
	}
}
