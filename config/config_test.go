package config

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestPatchInheritanceAndExplicitValues(t *testing.T) {
	original := Settings{Model: "initial", Effort: "low", Options: map[string]json.RawMessage{"enabled": []byte(`true`), "count": []byte(`5`), "tools": []byte(`["a"]`), "nested": []byte(`{"a":1}`)}}
	var patch Patch
	if err := json.Unmarshal([]byte(`{"model":"","effort":"high","options":{"enabled":false,"count":0,"tools":[],"nested":{},"nullable":null}}`), &patch); err != nil {
		t.Fatal(err)
	}
	got, err := Apply(original, patch, original)
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "" || got.Effort != "high" || string(got.Options["enabled"]) != "false" || string(got.Options["count"]) != "0" || string(got.Options["tools"]) != "[]" || string(got.Options["nested"]) != "{}" || string(got.Options["nullable"]) != "null" {
		t.Fatalf("explicit values lost: %+v", got)
	}
	reset, err := Apply(got, Patch{Reset: []string{"model", "effort", "options.enabled", "options.nullable"}}, original)
	if err != nil {
		t.Fatal(err)
	}
	if reset.Model != "initial" || reset.Effort != "low" || string(reset.Options["enabled"]) != "true" {
		t.Fatalf("reset lost parent: %+v", reset)
	}
	if _, ok := reset.Options["nullable"]; ok {
		t.Fatal("reset of missing parent option should remove selection")
	}
	got.Options["nested"][0] = 'x'
	if string(original.Options["nested"]) != `{"a":1}` {
		t.Fatal("Apply mutated parent")
	}
}

func TestPatchRejectsAmbiguousInputs(t *testing.T) {
	for _, input := range []string{`null`, `[]`, `{"model":null}`, `{"effort":null}`, `{"options":null}`, `{"reset":null}`, `{"modle":"x"}`, `{"model":"x","reset":["model"]}`, `{"reset":["model","model"]}`, `{"reset":["cwd"]}`, `{"options":{"x":1},"reset":["options.x"]}`} {
		var patch Patch
		if err := json.Unmarshal([]byte(input), &patch); !errors.Is(err, ErrInvalid) {
			t.Errorf("accepted %s: %v", input, err)
		}
	}
	if _, err := (Patch{Options: map[string]json.RawMessage{"bad": []byte("not json")}}).Fields(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid Go patch: %v", err)
	}
}
