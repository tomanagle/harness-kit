package settings

import (
	"fmt"
	"strings"
)

// ACPRuntime configures a named external ACP agent. Process wiring applies at
// startup; model and native session selections use the session settings group.
// Omitted limits use the adapter defaults. Set process.runtimes to select which
// configured agents are registered. The process.acp map is replaced as a whole.
type ACPRuntime struct {
	Command                string            `json:"command"`
	Args                   []string          `json:"args,omitempty"`
	Env                    map[string]string `json:"env,omitempty"`
	StartTimeoutMs         *int64            `json:"startTimeoutMs,omitempty"`
	CancelTimeoutMs        *int64            `json:"cancelTimeoutMs,omitempty"`
	MaxMessageBytes        *int              `json:"maxMessageBytes,omitempty"`
	MaxImageBytes          *int64            `json:"maxImageBytes,omitempty"`
	MaxRetainedOutputBytes *int              `json:"maxRetainedOutputBytes,omitempty"`
}

func validateACP(agents map[string]ACPRuntime) error {
	for name, agent := range agents {
		if name == "" || name != strings.TrimSpace(name) || name == "codex" || name == "claude" || name == "fake" {
			return fmt.Errorf("%w: invalid or reserved ACP runtime name %q", ErrInvalid, name)
		}
		if strings.TrimSpace(agent.Command) == "" {
			return fmt.Errorf("%w: process.acp.%s.command is required", ErrInvalid, name)
		}
		for field, value := range map[string]*int64{"startTimeoutMs": agent.StartTimeoutMs, "cancelTimeoutMs": agent.CancelTimeoutMs} {
			if value != nil && (*value < 1 || *value > 9223372036854) {
				return fmt.Errorf("%w: process.acp.%s.%s must be a positive duration in milliseconds", ErrInvalid, name, field)
			}
		}
		for field, value := range map[string]*int{"maxMessageBytes": agent.MaxMessageBytes, "maxRetainedOutputBytes": agent.MaxRetainedOutputBytes} {
			if value != nil && (*value < 1 || int64(*value) > 2147483647) {
				return fmt.Errorf("%w: process.acp.%s.%s must be in [1, 2147483647]", ErrInvalid, name, field)
			}
		}
		if agent.MaxImageBytes != nil && (*agent.MaxImageBytes < 1 || *agent.MaxImageBytes > 9007199254740991) {
			return fmt.Errorf("%w: process.acp.%s.maxImageBytes must be a positive safe integer", ErrInvalid, name)
		}
	}
	return nil
}
