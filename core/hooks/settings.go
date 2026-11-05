package hooks

import (
	"fmt"
)

// FailurePolicy controls whether a failed callback stops its lifecycle phase.
type FailurePolicy string

const (
	Fail     FailurePolicy = "fail"
	Continue FailurePolicy = "continue"
)

// Settings belongs to the shared settings document. Required IDs must be
// registered while hooks are enabled. This prevents a disconnected owner from
// silently removing application-required preparation or validation.
type Settings struct {
	MaxConcurrent int           `json:"maxConcurrent"`
	Enabled       bool          `json:"enabled"`
	TimeoutMs     int64         `json:"timeoutMs"`
	OnError       FailurePolicy `json:"onError"`
	Required      []string      `json:"required"`
}

func Defaults() Settings {
	return Settings{MaxConcurrent: 32, Enabled: true, TimeoutMs: 30000, OnError: "fail", Required: []string{}}
}

func (s Settings) Validate() error {
	if s.MaxConcurrent < 1 {
		return fmt.Errorf("hooks.maxConcurrent must be positive")
	}
	if s.TimeoutMs < 1 || s.TimeoutMs > 9223372036854 {
		return fmt.Errorf("hooks.timeoutMs must be positive and fit a duration")
	}
	if s.OnError != "fail" && s.OnError != "continue" {
		return fmt.Errorf("hooks.onError must be fail or continue")
	}
	seen := map[string]bool{}
	for _, id := range s.Required {
		if id == "" || seen[id] {
			return fmt.Errorf("hooks.required must contain unique nonempty IDs")
		}
		seen[id] = true
	}
	return nil
}
