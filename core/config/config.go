// Package config defines explicit configuration values and patches. It does not
// read files, environment variables or profiles; those sources belong to hosts.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrInvalid identifies malformed configuration or patches.
var ErrInvalid = errors.New("config: invalid configuration")

// Settings is a complete selection for a turn. Empty Model/Effort delegate
// selection to the runtime. Options use the adapter's native names. Values are
// replaced as a whole: there is no implicit recursive merge of native options.
type Settings struct {
	Model   string                     `json:"model,omitempty"`
	Effort  string                     `json:"effort,omitempty"`
	Options map[string]json.RawMessage `json:"options,omitempty"`
}

// Support describes where a setting can be supplied. Session changes apply to
// future turns, even when submitted during a turn. Live requires an explicit
// active-turn API; a setting without Live never changes a running turn.
type Support struct {
	Startup bool `json:"startup"`
	Session bool `json:"session"`
	Turn    bool `json:"turn"`
	Live    bool `json:"live"`
}

// Patch changes only supplied fields. Empty strings explicitly clear portable
// selections; false, zero, null and empty lists remain native option values.
// Reset names a field to inherit again: model, effort or options.NAME.
// Session patches inherit creation settings; turn patches inherit current
// session settings. A field cannot be both set and reset in the same patch.
type Patch struct {
	Model   *string                    `json:"model,omitempty"`
	Effort  *string                    `json:"effort,omitempty"`
	Options map[string]json.RawMessage `json:"options,omitempty"`
	Reset   []string                   `json:"reset,omitempty"`
}

// UnmarshalJSON rejects ambiguous null portable fields and unknown fields.
func (p *Patch) UnmarshalJSON(data []byte) error {
	type plain Patch
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if fields == nil {
		return fmt.Errorf("%w: patch must be an object", ErrInvalid)
	}
	for _, key := range []string{"model", "effort", "options", "reset"} {
		if v, ok := fields[key]; ok && bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return fmt.Errorf("%w: %s cannot be null; use reset to inherit", ErrInvalid, key)
		}
	}
	var value plain
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&value); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("%w: trailing data", ErrInvalid)
	}
	*p = Patch(value)
	_, err := p.Fields()
	return err
}

// Clone returns independent option values suitable for an immutable snapshot.
func (s Settings) Clone() Settings {
	if s.Options != nil {
		opts := make(map[string]json.RawMessage, len(s.Options))
		for k, v := range s.Options {
			opts[k] = bytes.Clone(v)
		}
		s.Options = opts
	}
	return s
}

// Fields returns the touched setting names, rejecting conflicts and bad values.
func (p Patch) Fields() ([]string, error) {
	seen := map[string]bool{}
	var fields []string
	add := func(name string) error {
		if seen[name] {
			return fmt.Errorf("%w: %s is both set/reset or repeated", ErrInvalid, name)
		}
		if name != "model" && name != "effort" && (!strings.HasPrefix(name, "options.") || len(name) == len("options.")) {
			return fmt.Errorf("%w: unknown reset field %q", ErrInvalid, name)
		}
		seen[name] = true
		fields = append(fields, name)
		return nil
	}
	if p.Model != nil {
		_ = add("model")
	}
	if p.Effort != nil {
		_ = add("effort")
	}
	for name, value := range p.Options {
		if name == "" || !json.Valid(value) {
			return nil, fmt.Errorf("%w: invalid option %q", ErrInvalid, name)
		}
		if err := add("options." + name); err != nil {
			return nil, err
		}
	}
	for _, name := range p.Reset {
		if err := add(name); err != nil {
			return nil, err
		}
	}
	return fields, nil
}

// Apply makes an independent snapshot of base with the patch applied.
func Apply(base Settings, p Patch, inherited Settings) (Settings, error) {
	if _, err := p.Fields(); err != nil {
		return Settings{}, err
	}
	out := base.Clone()
	if out.Options == nil {
		out.Options = map[string]json.RawMessage{}
	}
	for _, field := range p.Reset {
		switch field {
		case "model":
			out.Model = inherited.Model
		case "effort":
			out.Effort = inherited.Effort
		default:
			key := strings.TrimPrefix(field, "options.")
			if v, ok := inherited.Options[key]; ok {
				out.Options[key] = bytes.Clone(v)
			} else {
				delete(out.Options, key)
			}
		}
	}
	if p.Model != nil {
		out.Model = *p.Model
	}
	if p.Effort != nil {
		out.Effort = *p.Effort
	}
	for key, value := range p.Options {
		out.Options[key] = bytes.Clone(value)
	}
	return out, nil
}
