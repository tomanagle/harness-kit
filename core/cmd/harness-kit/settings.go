package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/tomanagle/harness-kit/core/settings"
)

// All startup sources feed the same settings validator as live RPC updates.
// Legacy flags are explicit overrides; absent flags never shadow file values.
func loadServeSettings(path string, flags *flag.FlagSet, flagValues settings.Values) (settings.Values, error) {
	values := flagValues
	if path != "" {
		f, err := os.Open(path)
		if err != nil {
			return settings.Values{}, fmt.Errorf("read settings: %w", err)
		}
		defer f.Close()
		values, err = settings.Decode(f, values)
		if err != nil {
			return settings.Values{}, fmt.Errorf("read settings: %w", err)
		}
	}
	return overrideServeFlags(flags, values, flagValues)
}

func overrideServeFlags(flags *flag.FlagSet, values, flagValues settings.Values) (settings.Values, error) {
	paths := map[string]string{
		"store": "process.store", "runtimes": "process.runtimes", "fake-runtime": "process.runtimes",
		"codex-path": "process.codexPath", "claude-path": "process.claudePath", "fake-delay": "process.fakeDelayMs",
		"git-path": "process.gitPath",
		"kit-mcp":  "process.kitMcp", "allow-unsafe": "process.allowUnsafe", "log-level": "logging.level",
		"approval-timeout": "approvals.timeoutMs", "event-log-size": "memory.eventLogSize", "event-buffer": "streams.maxQueueEvents",
	}
	raw, err := json.Marshal(flagValues)
	if err != nil {
		return settings.Values{}, err
	}
	var sections map[string]map[string]json.RawMessage
	if err := json.Unmarshal(raw, &sections); err != nil {
		return settings.Values{}, err
	}
	updates := map[string]map[string]json.RawMessage{}
	flags.Visit(func(f *flag.Flag) {
		path, ok := paths[f.Name]
		if !ok {
			return
		}
		var section, field string
		for i, c := range path {
			if c == '.' {
				section, field = path[:i], path[i+1:]
				break
			}
		}
		if updates[section] == nil {
			updates[section] = map[string]json.RawMessage{}
		}
		updates[section][field] = sections[section][field]
	})
	patch := settings.Patch{}
	for section, fields := range updates {
		patch[section], _ = json.Marshal(fields)
	}
	values, _, err = settings.Apply(values, patch, nil, flagValues)
	return values, err
}
