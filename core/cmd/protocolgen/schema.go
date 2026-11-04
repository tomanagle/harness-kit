package main

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tomanagle/harness-kit/core/event"
	"github.com/tomanagle/harness-kit/core/rpc/protocol"
	"github.com/tomanagle/harness-kit/core/settings"
	"github.com/tomanagle/harness-kit/core/tools"
)

const module = "github.com/tomanagle/harness-kit/core/"

// Schema is the JSON Schema subset needed by the public protocol. Go naming
// extensions preserve idiomatic identifiers without changing JSON wire names.
type Schema struct {
	Ref                  string             `json:"$ref,omitempty"`
	Type                 string             `json:"type,omitempty"`
	Description          string             `json:"description,omitempty"`
	Format               string             `json:"format,omitempty"`
	Properties           map[string]*Schema `json:"properties,omitempty"`
	Required             []string           `json:"required,omitempty"`
	Items                *Schema            `json:"items,omitempty"`
	AdditionalProperties *Schema            `json:"additionalProperties,omitempty"`
	Enum                 []string           `json:"enum,omitempty"`
	Const                any                `json:"const,omitempty"`
	AnyOf                []*Schema          `json:"anyOf,omitempty"`
	OneOf                []*Schema          `json:"oneOf,omitempty"`
	Not                  *Schema            `json:"not,omitempty"`
	GoName               string             `json:"x-go-name,omitempty"`
	GoType               string             `json:"x-go-type,omitempty"`
	GoJSONTag            string             `json:"x-go-json-tag,omitempty"`
}

type Method struct {
	GoName string  `json:"x-go-name"`
	Params *Schema `json:"params,omitempty"`
	Result *Schema `json:"result"`
}

type Document struct {
	Schema        string             `json:"$schema"`
	ID            string             `json:"$id"`
	Description   string             `json:"description"`
	Defs          map[string]*Schema `json:"$defs"`
	Version       int                `json:"x-protocol-version"`
	Constants     map[string]any     `json:"x-constants"`
	Methods       map[string]Method  `json:"x-rpc-methods"`
	Requests      map[string]Method  `json:"x-server-requests"`
	Notifications map[string]*Schema `json:"x-notifications"`
	Events        map[string]*Schema `json:"x-event-payloads"`
}

type sourceConstant struct {
	Name, Type string
	Value      any
}
type sourceInfo struct {
	comments  map[string]string
	constants map[string][]sourceConstant
}

func readSource(root string) (*sourceInfo, error) {
	s := &sourceInfo{map[string]string{}, map[string][]sourceConstant{}}
	for _, dir := range []string{"agent", "config", "event", "mcp", "permission", "rpc/protocol", "settings", "session", "tools", "workspace", "internal/jsonrpc"} {
		files, err := os.ReadDir(filepath.Join(root, "core", dir))
		if err != nil {
			return nil, err
		}
		pkg := module + dir
		for _, file := range files {
			if !strings.HasSuffix(file.Name(), ".go") || strings.HasSuffix(file.Name(), "_test.go") {
				continue
			}
			f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, "core", dir, file.Name()), nil, parser.ParseComments)
			if err != nil {
				return nil, err
			}
			stringTypes := map[string]bool{}
			for _, declaration := range f.Decls {
				if group, ok := declaration.(*ast.GenDecl); ok {
					for _, spec := range group.Specs {
						if typ, ok := spec.(*ast.TypeSpec); ok {
							if underlying, ok := typ.Type.(*ast.Ident); ok && underlying.Name == "string" {
								stringTypes[typ.Name.Name] = true
							}
						}
					}
				}
			}
			for _, decl := range f.Decls {
				g, ok := decl.(*ast.GenDecl)
				if !ok {
					continue
				}
				var previousType string
				var previousValues []ast.Expr
				for _, spec := range g.Specs {
					switch v := spec.(type) {
					case *ast.TypeSpec:
						key := pkg + "." + v.Name.Name
						doc := v.Doc
						if doc == nil {
							doc = g.Doc
						}
						s.comments[key] = doc.Text()
						if st, ok := v.Type.(*ast.StructType); ok {
							for _, field := range st.Fields.List {
								for _, name := range field.Names {
									s.comments[key+"."+name.Name] = strings.TrimSpace(field.Doc.Text() + field.Comment.Text())
								}
							}
						}
					case *ast.ValueSpec:
						if g.Tok != token.CONST {
							continue
						}
						values := v.Values
						if len(values) > 0 {
							previousType = ""
							if ident, ok := v.Type.(*ast.Ident); ok {
								previousType = ident.Name
							}
							previousValues = values
						} else {
							values = previousValues
						}
						for i, name := range v.Names {
							if i >= len(values) {
								continue
							}
							value, ok := literal(values[i])
							if !ok {
								if stringTypes[previousType] || ((dir == "rpc/protocol" || dir == "internal/jsonrpc") && isWireConstant(name.Name)) {
									return nil, fmt.Errorf("cannot extract wire constant %s.%s: extend protocolgen to support its expression", dir, name.Name)
								}
								continue
							}
							s.constants[pkg] = append(s.constants[pkg], sourceConstant{name.Name, previousType, value})
						}
					}
				}
			}
		}
	}
	return s, nil
}

func isWireConstant(name string) bool {
	for _, prefix := range []string{"Code", "Reason", "Method", "Request", "Notification"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func literal(e ast.Expr) (any, bool) {
	switch v := e.(type) {
	case *ast.BasicLit:
		if v.Kind == token.STRING {
			s, err := strconv.Unquote(v.Value)
			return s, err == nil
		}
		if v.Kind == token.INT {
			n, err := strconv.ParseInt(v.Value, 0, 64)
			return n, err == nil
		}
	case *ast.UnaryExpr:
		if v.Op == token.SUB {
			x, ok := literal(v.X)
			if n, isInt := x.(int64); ok && isInt {
				return -n, true
			}
		}
	}
	return nil, false
}

type builder struct {
	doc    *Document
	source *sourceInfo
	names  map[reflect.Type]string
	owners map[string]reflect.Type
}

func buildSchema(root string) (*Document, error) {
	source, err := readSource(root)
	if err != nil {
		return nil, err
	}
	d := &Document{
		Schema:      "https://json-schema.org/draft/2020-12/schema",
		ID:          "https://github.com/tomanagle/harness-kit/blob/main/protocol/schema.json",
		Description: "Harness Kit JSON-RPC wire contract. Generated from core Go types; run make generate. x-rpc-methods binds parameter and result schemas; x-event-payloads binds discriminators to payload schemas.",
		Version:     protocol.Version, Defs: map[string]*Schema{}, Constants: map[string]any{},
		Methods: map[string]Method{}, Requests: map[string]Method{}, Notifications: map[string]*Schema{}, Events: map[string]*Schema{},
	}
	b := &builder{d, source, publicNames(), map[string]reflect.Type{}}
	// Visit stable public aliases even when only used by convenience APIs.
	var types []reflect.Type
	for t := range b.names {
		types = append(types, t)
	}
	slices.SortFunc(types, func(a, c reflect.Type) int { return strings.Compare(b.names[a], b.names[c]) })
	for _, t := range types {
		b.schema(t)
	}
	for _, m := range methods() {
		if _, exists := d.Methods[m.Name]; exists {
			return nil, fmt.Errorf("duplicate method %s", m.Name)
		}
		x := Method{GoName: methodName(m.Name), Result: b.schema(m.Result)}
		if m.Params.Kind() != reflect.Interface {
			x.Params = b.schema(m.Params)
		}
		d.Methods[m.Name] = x
	}
	for _, pkg := range []string{module + "internal/jsonrpc", module + "rpc/protocol"} {
		for _, c := range source.constants[pkg] {
			if strings.HasPrefix(c.Name, "Code") || strings.HasPrefix(c.Name, "Reason") || strings.HasPrefix(c.Name, "Method") || strings.HasPrefix(c.Name, "Notification") || strings.HasPrefix(c.Name, "Request") {
				d.Constants[c.Name] = c.Value
				if strings.HasPrefix(c.Name, "Method") {
					name, ok := c.Value.(string)
					if !ok {
						return nil, fmt.Errorf("non-string method %s", c.Name)
					}
					if _, ok := d.Methods[name]; !ok {
						return nil, fmt.Errorf("missing method binding for %s", c.Name)
					}
				}
			}
		}
	}
	// Reuse the event decoder's registry rather than a second payload map.
	for _, c := range source.constants[module+"event"] {
		if c.Type != "Type" {
			continue
		}
		name := c.Value.(string)
		var ev event.Event
		raw, _ := json.Marshal(map[string]any{"type": name, "data": map[string]any{}})
		if err := json.Unmarshal(raw, &ev); err != nil {
			return nil, fmt.Errorf("event %s: %w", name, err)
		}
		if _, unknown := ev.Data.(json.RawMessage); unknown || ev.Data == nil {
			return nil, fmt.Errorf("event %s has no registered payload", name)
		}
		d.Events[name] = b.schema(reflect.TypeOf(ev.Data))
	}
	d.Requests[protocol.RequestToolCall] = Method{"ToolCall", b.schema(reflect.TypeFor[protocol.ToolCallParams]()), b.schema(reflect.TypeFor[tools.Result]())}
	d.Notifications[protocol.NotificationEvent] = b.schema(reflect.TypeFor[event.Event]())
	d.Notifications[protocol.NotificationSettingsUpdated] = b.schema(reflect.TypeFor[settings.Snapshot]())
	d.Notifications[protocol.NotificationCancelRequest] = &Schema{Type: "object", Properties: map[string]*Schema{"id": {AnyOf: []*Schema{{Type: "integer"}, {Type: "string"}}}}, Required: []string{"id"}}
	var reasons []string
	for name, value := range d.Constants {
		if strings.HasPrefix(name, "Reason") {
			reasons = append(reasons, value.(string))
		}
	}
	slices.Sort(reasons)
	d.Defs["ErrorReason"] = &Schema{Type: "string", Enum: reasons, GoType: "string"}
	b.settingsPatch()
	b.eventUnion()
	return d, nil
}

func (b *builder) name(t reflect.Type) string {
	if name := b.names[t]; name != "" {
		return name
	}
	return t.Name()
}

func (b *builder) schema(t reflect.Type) *Schema {
	if t == reflect.TypeFor[json.RawMessage]() {
		return &Schema{GoType: "json.RawMessage"}
	}
	if t == reflect.TypeFor[time.Time]() {
		return &Schema{Type: "string", Format: "date-time", GoType: "time.Time"}
	}
	if t.Kind() == reflect.Pointer {
		s := *b.schema(t.Elem())
		s.GoType = "*" + b.goType(t.Elem())
		return &s
	}
	if t.Name() != "" && t.PkgPath() != "" {
		name := b.name(t)
		if owner, ok := b.owners[name]; ok && owner != t {
			panic(fmt.Sprintf("public type collision %s: %s and %s", name, owner, t))
		}
		if b.doc.Defs[name] == nil {
			b.owners[name] = t
			b.doc.Defs[name] = &Schema{} // recursion guard
			s := b.shape(t)
			s.Description = strings.TrimSpace(b.source.comments[t.PkgPath()+"."+t.Name()])
			for _, c := range b.source.constants[t.PkgPath()] {
				if c.Type == t.Name() {
					if value, ok := c.Value.(string); ok {
						s.Enum = append(s.Enum, value)
					}
				}
			}
			slices.Sort(s.Enum)
			b.doc.Defs[name] = s
		}
		return &Schema{Ref: "#/$defs/" + name, GoType: name}
	}
	return b.shape(t)
}

func (b *builder) goType(t reflect.Type) string {
	s := b.schema(t)
	if s.GoType != "" {
		return s.GoType
	}
	panic("missing Go type for " + t.String())
}

func (b *builder) shape(t reflect.Type) *Schema {
	s := &Schema{}
	switch t.Kind() {
	case reflect.Struct:
		s.Type = "object"
		s.Properties = map[string]*Schema{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			tag := f.Tag.Get("json")
			parts := strings.Split(tag, ",")
			if parts[0] == "-" {
				continue
			}
			if f.Anonymous && parts[0] == "" {
				ft := f.Type
				if ft.Kind() == reflect.Pointer {
					ft = ft.Elem()
				}
				if ft.Kind() != reflect.Struct {
					panic("unsupported embedded type " + ft.String())
				}
				embedded := b.shape(ft)
				for name, prop := range embedded.Properties {
					if _, ok := s.Properties[name]; ok {
						panic("ambiguous embedded field " + name)
					}
					s.Properties[name] = prop
				}
				s.Required = append(s.Required, embedded.Required...)
				continue
			}
			name := parts[0]
			if name == "" {
				name = f.Name
			}
			optional := slices.Contains(parts[1:], "omitempty") || slices.Contains(parts[1:], "omitzero")
			prop := *b.schema(f.Type)
			if f.Type.Kind() == reflect.Pointer && !optional {
				prop = Schema{AnyOf: []*Schema{b.schema(f.Type.Elem()), {Type: "null"}}, GoType: b.goType(f.Type)}
			}
			prop.GoName, prop.GoJSONTag = f.Name, tag
			prop.Description = strings.TrimSpace(b.source.comments[t.PkgPath()+"."+t.Name()+"."+f.Name])
			applyFieldConstraints(t, f, &prop)
			s.Properties[name] = &prop
			if !optional {
				s.Required = append(s.Required, name)
			}
		}
		slices.Sort(s.Required)
	case reflect.Slice, reflect.Array:
		s.Type = "array"
		s.Items = b.schema(t.Elem())
		s.GoType = "[]" + b.goType(t.Elem())
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			panic("non-string map key: " + t.String())
		}
		s.Type = "object"
		s.AdditionalProperties = b.schema(t.Elem())
		s.GoType = "map[string]" + b.goType(t.Elem())
	case reflect.Interface:
		s.GoType = "json.RawMessage"
	case reflect.String:
		s.Type = "string"
		s.GoType = "string"
	case reflect.Bool:
		s.Type = "boolean"
		s.GoType = "bool"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		s.Type = "integer"
		s.GoType = t.Kind().String()
	case reflect.Float32, reflect.Float64:
		s.Type = "number"
		s.GoType = t.Kind().String()
	default:
		panic("unsupported public wire type: " + t.String())
	}
	return s
}

// These fields use plain Go strings but have a narrower documented wire domain.
// Named enums are discovered directly from Go constants above.
func applyFieldConstraints(t reflect.Type, f reflect.StructField, p *Schema) {
	key := strings.TrimPrefix(t.PkgPath(), module) + "." + t.Name() + "." + f.Name
	choices := map[string][]string{
		"workspace.GitSettings.Hooks":   {"inherit", "disabled"},
		"workspace.GitSettings.Signing": {"inherit", "disabled", "enabled"},
		"event.InputPart.Type":          {"text", "image"}, "event.UsageData.Mode": {"snapshot", "delta"},
		"event.UsageData.Scope": {"turn", "session"}, "event.BudgetExceededData.Scope": {"session", "turn"},
		"event.BudgetExceededData.Metric": {"tokens", "costUsd"}, "workspace.Settings.Mode": {"existing", "worktree"},
		"settings.Logging.Level": {"debug", "info", "warn", "error"}, "mcp.Server.Type": {"stdio", "http", "streamable-http"},
		"tools.Content.Type": {"text"}, "mcp.Entry.Source": {"user", "project"},
	}
	if values := choices[key]; len(values) > 0 {
		p.Enum = values
	}
	if key == "event.SessionUpdatedData.State" {
		p.Ref = "#/$defs/SessionState"
		p.Type = ""
	}
	// This pointer detects a missing required field; null is rejected by the
	// RPC handler, unlike nullable resource and budget limits.
	if key == "rpc/protocol.SessionConfigureParams.Config" {
		p.AnyOf = nil
		p.Ref = "#/$defs/ConfigPatch"
	}
	// Native options are JSON objects; their values remain deliberately open.
	if key == "agent.SessionConfig.Options" || key == "rpc/protocol.SessionCreateParams.Options" || key == "rpc/protocol.ToolSpec.InputSchema" {
		p.Type = "object"
		p.AdditionalProperties = &Schema{}
	}
}

func (b *builder) settingsPatch() {
	// Settings use a raw map in Go to distinguish omitted/null/zero values.
	// Derive the typed partial groups from Values instead of duplicating fields.
	values := b.doc.Defs["SettingsValues"]
	patch := &Schema{Type: "object", Properties: map[string]*Schema{}, GoType: "map[string]json.RawMessage", Description: "Omitted fields remain unchanged. Null disables nullable limits; false and zero are explicit values."}
	for _, name := range sortedKeys(values.Properties) {
		original := b.doc.Defs[refName(values.Properties[name])]
		if original == nil {
			panic("settings group is not a named object: " + name)
		}
		group := *original
		group.Required = nil
		group.Properties = map[string]*Schema{}
		for field, prop := range original.Properties {
			copy := *prop
			group.Properties[field] = &copy
		}
		if name == "session" {
			for _, field := range []string{"mcp", "hostTools"} {
				p := group.Properties[field]
				group.Properties[field] = &Schema{AnyOf: []*Schema{p, {Type: "null"}}, GoType: p.GoType, GoName: p.GoName, GoJSONTag: p.GoJSONTag}
			}
		}
		groupName := refName(values.Properties[name]) + "Patch"
		group.GoType = "map[string]json.RawMessage" // Go preserves explicit patch presence.
		b.doc.Defs[groupName] = &group
		patch.Properties[name] = &Schema{Ref: "#/$defs/" + groupName}
	}
	b.doc.Defs["SettingsPatch"] = patch
}

func (b *builder) eventUnion() {
	s := b.doc.Defs["HarnessEvent"]
	// Unknown event kinds remain valid for forward compatibility. The known
	// payload branches below still enforce their own discriminator and shape.
	s.Properties["type"] = &Schema{Type: "string", GoName: "Type", GoType: "EventType", GoJSONTag: "type"}
	for _, name := range sortedKeys(b.doc.Events) {
		variant := &Schema{Type: "object", Properties: map[string]*Schema{}, Required: append([]string(nil), s.Required...)}
		for field, prop := range s.Properties {
			variant.Properties[field] = prop
		}
		variant.Properties["type"] = &Schema{Const: name}
		variant.Properties["data"] = b.doc.Events[name]
		variant.Required = append(variant.Required, "data")
		s.OneOf = append(s.OneOf, variant)
	}
	s.OneOf = append(s.OneOf, &Schema{Type: "object", Properties: map[string]*Schema{
		"type": {Type: "string", Not: &Schema{Enum: sortedKeys(b.doc.Events)}},
	}, Required: []string{"type"}})
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
func refName(s *Schema) string { return strings.TrimPrefix(s.Ref, "#/$defs/") }
func methodName(name string) string {
	parts := strings.Split(name, ".")
	for i, s := range parts {
		parts[i] = strings.ToUpper(s[:1]) + s[1:]
	}
	return strings.Join(parts, "")
}
