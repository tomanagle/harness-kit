package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

type stubRuntime struct {
	name string
	d    Discovery
	err  error
}

func (s stubRuntime) Info() Info { return Info{Name: s.name, DisplayName: s.name} }
func (s stubRuntime) Discover(context.Context) (Discovery, error) {
	return s.d, s.err
}
func (s stubRuntime) Start(context.Context, SessionConfig, Handler) (Session, error) {
	return nil, ErrUnsupported
}

func TestRegistry(t *testing.T) {
	r := NewRegistry(
		stubRuntime{name: "a", d: Discovery{Installed: true, Ready: true, Auth: AuthStatus{State: AuthAuthenticated}}},
		stubRuntime{name: "b", err: errors.New("probe failed")},
	)
	if err := r.Register(stubRuntime{name: "a"}); err == nil {
		t.Fatal("duplicate registration should fail")
	}
	if err := r.Register(stubRuntime{}); err == nil {
		t.Fatal("empty name should fail")
	}
	list := r.List()
	if len(list) != 2 || list[0].Name != "a" || list[1].Name != "b" {
		t.Fatalf("list: %+v", list)
	}
	if _, ok := r.Get("a"); !ok {
		t.Fatal("get a")
	}

	ds, err := r.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ds[0].Runtime != "a" || !ds[0].Ready {
		t.Fatalf("a: %+v", ds[0])
	}
	if ds[1].Runtime != "b" || ds[1].Ready || len(ds[1].Problems) != 1 || ds[1].Auth.State != AuthUnknown {
		t.Fatalf("b: %+v", ds[1])
	}
	if _, err := r.Discover(context.Background(), "nope"); err == nil {
		t.Fatal("unknown runtime should fail")
	}
}

type opts struct {
	Mode  string   `json:"mode"`
	Tools []string `json:"tools"`
}

func TestDecodeOptions(t *testing.T) {
	var o opts
	if err := DecodeOptions(nil, &o); err != nil || o.Mode != "" {
		t.Fatal(err)
	}
	if err := DecodeOptions(json.RawMessage(`{"mode":"x","tools":["a"]}`), &o); err != nil || o.Mode != "x" || len(o.Tools) != 1 {
		t.Fatalf("%v %+v", err, o)
	}
	o = opts{}
	if err := DecodeOptions(map[string]any{"mode": "y"}, &o); err != nil || o.Mode != "y" {
		t.Fatalf("%v %+v", err, o)
	}
	o = opts{}
	if err := DecodeOptions(opts{Mode: "z"}, &o); err != nil || o.Mode != "z" {
		t.Fatalf("%v %+v", err, o)
	}
	if err := DecodeOptions(`{"mdoe":"typo"}`, &o); err == nil {
		t.Fatal("unknown field should be rejected")
	}
}

func TestJoinText(t *testing.T) {
	in := []InputPart{{Type: "text", Text: "a"}, {Type: "image", Path: "/x.png"}, {Text: "b"}}
	if got := JoinText(in); got != "a\n\nb" {
		t.Fatalf("%q", got)
	}
}
