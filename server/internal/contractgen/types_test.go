package contractgen

import (
	"reflect"
	"testing"
)

func TestUnsupportedTypesReturnErrors(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeFor[map[int]string](), reflect.TypeFor[[]complex128](), reflect.TypeFor[chan int]()} {
		if _, err := tsType(typ); err == nil {
			t.Errorf("TypeScript accepted %s", typ)
		}
		if _, err := schema(typ); err == nil {
			t.Errorf("schema accepted %s", typ)
		}
	}
}

func TestStringMapsGenerateOpenObjects(t *testing.T) {
	typ := reflect.TypeFor[map[string]string]()
	ts, err := tsType(typ)
	if err != nil || ts != "Record<string, string>" {
		t.Fatalf("%s %v", ts, err)
	}
	got, err := schema(typ)
	if err != nil {
		t.Fatal(err)
	}
	expected := map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}}
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("%v", got)
	}
}
