package tools

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
)

// Validate checks value against a JSON Schema. It implements the subset
// that tool schemas use in practice (type, properties, required,
// additionalProperties, items, enum, minLength/maxLength,
// minimum/maximum) and ignores other keywords.
func Validate(schema, value json.RawMessage) error {
	var s any
	if err := json.Unmarshal(schema, &s); err != nil {
		return fmt.Errorf("invalid schema: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(value))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return fmt.Errorf("arguments are not valid JSON: %w", err)
	}
	return check(s, v, "arguments")
}

func typeOf(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case json.Number:
		if f, err := x.Float64(); err == nil && f == math.Trunc(f) && !strings.ContainsAny(x.String(), ".eE") {
			return "integer"
		}
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "unknown"
}

func typeMatches(want, got string) bool {
	return want == got || (want == "number" && got == "integer")
}

func check(schema any, v any, path string) error {
	s, ok := schema.(map[string]any)
	if !ok {
		return nil // true / unsupported forms accept anything
	}
	got := typeOf(v)
	switch want := s["type"].(type) {
	case string:
		if !typeMatches(want, got) {
			return fmt.Errorf("%s: expected %s, got %s", path, want, got)
		}
	case []any:
		ok := false
		for _, w := range want {
			if ws, _ := w.(string); typeMatches(ws, got) {
				ok = true
			}
		}
		if !ok {
			return fmt.Errorf("%s: unexpected type %s", path, got)
		}
	}
	if enum, ok := s["enum"].([]any); ok {
		match := slices.ContainsFunc(enum, func(e any) bool { return fmt.Sprint(e) == fmt.Sprint(v) })
		if !match {
			return fmt.Errorf("%s: must be one of %v", path, enum)
		}
	}
	switch x := v.(type) {
	case string:
		n := len([]rune(x))
		if min, ok := num(s["minLength"]); ok && float64(n) < min {
			return fmt.Errorf("%s: shorter than %v characters", path, min)
		}
		if max, ok := num(s["maxLength"]); ok && float64(n) > max {
			return fmt.Errorf("%s: longer than %v characters", path, max)
		}
	case json.Number:
		f, _ := x.Float64()
		if min, ok := num(s["minimum"]); ok && f < min {
			return fmt.Errorf("%s: less than %v", path, min)
		}
		if max, ok := num(s["maximum"]); ok && f > max {
			return fmt.Errorf("%s: greater than %v", path, max)
		}
	case []any:
		for i, item := range x {
			if err := check(s["items"], item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case map[string]any:
		props, _ := s["properties"].(map[string]any)
		if req, ok := s["required"].([]any); ok {
			for _, r := range req {
				if name, _ := r.(string); name != "" {
					if _, present := x[name]; !present {
						return fmt.Errorf("%s.%s: required", path, name)
					}
				}
			}
		}
		for k, val := range x {
			if ps, ok := props[k]; ok {
				if err := check(ps, val, path+"."+k); err != nil {
					return err
				}
				continue
			}
			switch ap := s["additionalProperties"].(type) {
			case bool:
				if !ap {
					return fmt.Errorf("%s.%s: unexpected property", path, k)
				}
			case map[string]any:
				if err := check(ap, val, path+"."+k); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func num(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}
