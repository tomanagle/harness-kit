package contractgen

import (
	"encoding/json"
	"fmt"
	"reflect"
)

func tsType(t reflect.Type) (string, error) {
	if t == reflect.TypeFor[json.RawMessage]() {
		return "unknown", nil
	}
	switch t.Kind() {
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return "", fmt.Errorf("unsupported wire map key %s", t.Key())
		}
		element, err := tsType(t.Elem())
		if err != nil {
			return "", err
		}
		return "Record<string, " + element + ">", nil
	case reflect.String:
		return "string", nil
	case reflect.Bool:
		return "boolean", nil
	case reflect.Int, reflect.Int64:
		return "number", nil
	case reflect.Slice:
		element, err := tsType(t.Elem())
		if err != nil {
			return "", err
		}
		return element + "[]", nil
	case reflect.Struct:
		return t.Name(), nil
	}
	return "", fmt.Errorf("unsupported wire type %s", t)
}

func schema(t reflect.Type) (map[string]any, error) {
	if t == reflect.TypeFor[json.RawMessage]() {
		return map[string]any{}, nil
	}
	switch t.Kind() {
	case reflect.Map:
		if t.Key().Kind() != reflect.String {
			return nil, fmt.Errorf("unsupported wire map key %s", t.Key())
		}
		value, err := schema(t.Elem())
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "object", "additionalProperties": value}, nil
	case reflect.String:
		return map[string]any{"type": "string"}, nil
	case reflect.Bool:
		return map[string]any{"type": "boolean"}, nil
	case reflect.Int, reflect.Int64:
		return map[string]any{"type": "integer", "minimum": 0, "maximum": 9007199254740991}, nil
	case reflect.Slice:
		items, err := schema(t.Elem())
		if err != nil {
			return nil, err
		}
		return map[string]any{"type": "array", "items": items}, nil
	case reflect.Struct:
		return map[string]any{"$ref": "#/$defs/" + t.Name()}, nil
	}
	return nil, fmt.Errorf("unsupported wire type %s", t)
}
