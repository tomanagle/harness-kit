package tools

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"
)

// SchemaOf infers a JSON Schema from v's type (see SchemaFor).
func SchemaOf(v any) (json.RawMessage, error) {
	t := reflect.TypeOf(v)
	if t == nil {
		return json.RawMessage(`{"type":"object"}`), nil
	}
	return SchemaFor(t)
}

// SchemaFor infers a JSON Schema for an argument type, which must be a
// struct (or pointer to one) or a map with string keys.
//
// Struct fields use their `json` names; fields tagged `omitempty` or of
// pointer type are optional. Two extra tags are understood:
//
//	desc:"what this field means"
//	enum:"low,medium,high"
func SchemaFor(t reflect.Type) (json.RawMessage, error) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct && !(t.Kind() == reflect.Map && t.Key().Kind() == reflect.String) {
		return nil, fmt.Errorf("arguments must be a struct, got %s", t)
	}
	s, err := schemaFor(t, 0)
	if err != nil {
		return nil, err
	}
	return json.Marshal(s)
}

var (
	timeType = reflect.TypeOf(time.Time{})
	rawType  = reflect.TypeOf(json.RawMessage(nil))
)

func schemaFor(t reflect.Type, depth int) (map[string]any, error) {
	if depth > 16 {
		return nil, fmt.Errorf("type %s is too deeply nested", t)
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch {
	case t == timeType:
		return map[string]any{"type": "string", "format": "date-time"}, nil
	case t == rawType:
		return map[string]any{}, nil
	}
	switch t.Kind() {
	case reflect.String:
		return map[string]any{"type": "string"}, nil
	case reflect.Bool:
		return map[string]any{"type": "boolean"}, nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}, nil
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}, nil
	case reflect.Interface:
		return map[string]any{}, nil
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return map[string]any{"type": "string", "contentEncoding": "base64"}, nil
		}
		items, err := schemaFor(t.Elem(), depth+1)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "array", "items": items}, nil
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return nil, fmt.Errorf("map keys must be strings in %s", t)
		}
		vals, err := schemaFor(t.Elem(), depth+1)
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "object", "additionalProperties": vals}, nil
	case reflect.Struct:
		props := map[string]any{}
		var required []string
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "-" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			fs, err := schemaFor(f.Type, depth+1)
			if err != nil {
				return nil, err
			}
			if typ, ok := fs["type"].(string); ok && f.Type.Kind() == reflect.Pointer {
				fs["type"] = []string{typ, "null"} // pointers may be null
			}
			if d := f.Tag.Get("desc"); d != "" {
				fs["description"] = d
			}
			if e := f.Tag.Get("enum"); e != "" {
				fs["enum"] = strings.Split(e, ",")
			}
			props[name] = fs
			if !strings.Contains(opts, "omitempty") && f.Type.Kind() != reflect.Pointer {
				required = append(required, name)
			}
		}
		s := map[string]any{"type": "object", "properties": props}
		if len(required) > 0 {
			s["required"] = required
		}
		return s, nil
	}
	return nil, fmt.Errorf("unsupported type %s", t)
}
