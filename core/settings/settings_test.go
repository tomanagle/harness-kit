package settings

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func patch(t *testing.T, raw string) Patch {
	t.Helper()
	var p Patch
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestACPProcessSettingsValidation(t *testing.T) {
	base := Defaults()
	got, paths, err := Apply(base, patch(t, `{"process":{"acp":{"custom":{"command":"agent-cli","args":["--acp"],"env":{"API_KEY":"${API_KEY}"},"startTimeoutMs":1000,"maxMessageBytes":1048576}}}}`), nil, base)
	if err != nil || len(paths) != 1 || paths[0] != "process.acp" {
		t.Fatalf("ACP settings: %+v %v %v", got, paths, err)
	}
	if Describe()["process.acp"].When != "restart" || Describe()["process.acp"].Writable {
		t.Fatal("ACP process wiring must advertise restart timing")
	}
	copy := got.Clone()
	copy.Process.ACP["custom"].Env["API_KEY"] = "changed"
	if got.Process.ACP["custom"].Env["API_KEY"] != "${API_KEY}" {
		t.Fatal("ACP environment must be cloned without expanding references")
	}
	for _, raw := range []string{
		`{"process":{"acp":{"codex":{"command":"replacement"}}}}`,
		`{"process":{"acp":{"custom":{}}}}`,
		`{"process":{"acp":{"custom":{"command":"cli","startTimeoutMs":0}}}}`,
		`{"process":{"acp":{"custom":{"command":"cli","cancelTimeoutMs":9223372036855}}}}`,
		`{"process":{"acp":{"custom":{"command":"cli","maxMessageBytes":-1}}}}`,
		`{"process":{"acp":{"custom":{"command":"cli","maxImageBytes":0}}}}`,
		`{"process":{"acp":{"custom":{"command":"cli","unknown":true}}}}`,
	} {
		if _, _, err := Apply(base, patch(t, raw), nil, base); !errors.Is(err, ErrInvalid) {
			t.Fatalf("invalid ACP settings accepted: %s (%v)", raw, err)
		}
	}
}

func TestApplyPreservesExplicitValuesAndNativeObjects(t *testing.T) {
	base := Defaults()
	base.Memory.ResumeOnPrompt = true
	base.Session.Options = map[string]any{"native": map[string]any{"a": 1, "b": 2}, "other": true}
	got, paths, err := Apply(base, patch(t, `{"memory":{"resumeOnPrompt":false,"maxCacheBytes":0,"evictAfterIdleMs":null},"session":{"options":{"native":{"a":3},"other":false},"hostTools":[]}}`), nil, Defaults())
	if err != nil {
		t.Fatal(err)
	}
	if got.Memory.ResumeOnPrompt || got.Memory.MaxCacheBytes == nil || *got.Memory.MaxCacheBytes != 0 || got.Memory.EvictAfterIdleMs != nil {
		t.Fatalf("explicit values lost: %+v", got.Memory)
	}
	if got.Memory.EventLogSize != base.Memory.EventLogSize {
		t.Fatal("omitted field changed")
	}
	if len(paths) != 6 {
		t.Fatalf("paths: %v", paths)
	}
	selected, err := got.Session.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if string(selected.Options["native"]) != `{"a":3}` || string(selected.Options["other"]) != "false" {
		t.Fatalf("native option values recursively merged or lost: %s", selected.Options)
	}
	if got.Session.HostTools == nil {
		t.Fatal("explicit empty list lost")
	}
	if base.Session.Options.(map[string]any)["other"] != true {
		t.Fatal("mutated caller settings")
	}
}

func TestResetInheritsStartupAndRemovesAbsentOption(t *testing.T) {
	initial := Defaults()
	initial.Session.Model = "startup-model"
	base := initial.Clone()
	base.Session.Model = "changed"
	base.Session.Options = map[string]any{"temporary": 1}
	got, _, err := Apply(base, Patch{}, []string{"session.model", "session.options.temporary"}, initial)
	if err != nil {
		t.Fatal(err)
	}
	selected, _ := got.Session.Settings()
	if got.Session.Model != "startup-model" || len(selected.Options) != 0 {
		t.Fatalf("bad reset: %+v", got.Session)
	}
}

func TestInvalidPatchIsRejectedWithoutMutation(t *testing.T) {
	cases := []struct {
		raw   string
		reset []string
	}{
		{`{"memory":{"maxCacheByets":1}}`, nil},
		{`{"memory":null}`, nil},
		{`{"memory":{"resumeOnPrompt":null}}`, nil},
		{`{"memory":{"maxCacheBytes":-1}}`, nil},
		{`{"memory":{"evictAfterIdleMs":9223372036855}}`, nil},
		{`{"history":{"maxPageEvents":0}}`, nil},
		{`{"history":{"maxPageBytes":0}}`, nil},
		{`{"logging":{"level":"bogus"}}`, nil},
		{`{"session":{"options":null}}`, nil},
		{`{"session":{"permissionTool":"internal"}}`, nil},
		{`{"memory":{"maxCacheBytes":1}}`, []string{"memory.maxCacheBytes"}},
		{`{"session":{"options":{"x":1}}}`, []string{"session.options"}},
		{`{}`, []string{"session.options", "session.options.x"}},
		{`{}`, []string{"madeup.value"}},
	}
	for _, tc := range cases {
		t.Run(tc.raw+strings.Join(tc.reset, ","), func(t *testing.T) {
			base := Defaults()
			before, _ := json.Marshal(base)
			_, _, err := Apply(base, patch(t, tc.raw), tc.reset, base)
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("expected invalid, got %v", err)
			}
			after, _ := json.Marshal(base)
			if string(before) != string(after) {
				t.Fatal("invalid patch mutated base")
			}
		})
	}
}

func TestDecodeAndDescription(t *testing.T) {
	v, err := Decode(strings.NewReader(`{"memory":{"evictAfterIdleMs":600000},"session":{"model":"test"}}`), Defaults())
	if err != nil {
		t.Fatal(err)
	}
	if *v.Memory.EvictAfterIdleMs != 600000 || v.Session.Model != "test" || v.Streams.MaxQueueEvents != 4096 {
		t.Fatalf("bad document %+v", v)
	}
	if _, err := Decode(strings.NewReader(`{} {}`), Defaults()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("trailing data accepted: %v", err)
	}
	if _, err := Decode(strings.NewReader(`null`), Defaults()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("null accepted: %v", err)
	}
	fields := Describe()
	if fields["process.store"].When != "restart" || fields["process.store"].Writable {
		t.Fatal("startup field advertised live")
	}
	if fields["memory.maxCacheBytes"].Minimum == nil || *fields["memory.maxCacheBytes"].Minimum != 0 {
		t.Fatal("missing range metadata")
	}
}

func TestCloneRejectsInvalidNativeOptions(t *testing.T) {
	v := Defaults()
	v.Session.Options = func() {}
	if _, err := Clone(v); !errors.Is(err, ErrInvalid) {
		t.Fatalf("wanted invalid config, got %v", err)
	}
}
