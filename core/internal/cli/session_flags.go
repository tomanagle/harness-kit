package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/rpc/protocol"
	"github.com/tomanagle/harness-kit/core/settings"
)

type sessionFlags struct {
	backendFlags
	runtime            string
	cwd                string
	model              string
	effort             string
	turnConfig         string
	sessionSettingsRaw string
	turnSettingsRaw    string
	sessionSettings    settings.Patch
	turnSettings       settings.Patch
	turnPatch          *agent.ConfigPatch
	resume             string
	instructions       string
	title              string
	options            string
	approve            string
	out                outputFlags
	raw                bool
	mcpFile            string
	noNativeMCP        bool
	images             []string
}

func (sf *sessionFlags) register(fs *flag.FlagSet) {
	sf.backendFlags.register(fs)
	fs.StringVar(&sf.sessionSettingsRaw, "session-settings", "", "shared settings JSON for creation or a restored session")
	fs.StringVar(&sf.turnSettingsRaw, "turn-settings", "", "shared settings JSON for this turn")
	fs.Func("image", "attach a local image to the first prompt (repeat for multiple images)", func(path string) error {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		sf.images = append(sf.images, absolute)
		return nil
	})
	fs.StringVar(&sf.runtime, "runtime", "codex", "runtime to use: codex, claude or fake")
	fs.StringVar(&sf.cwd, "cwd", ".", "working directory for the agent")
	fs.StringVar(&sf.model, "model", "", "model, in the runtime's own naming")
	fs.StringVar(&sf.effort, "effort", "", "default reasoning effort, in the runtime's own naming")
	fs.StringVar(&sf.turnConfig, "turn-config", "", `configuration overrides for each prompt as JSON, e.g. '{"model":"model-name","effort":"high"}'`)
	fs.StringVar(&sf.resume, "resume", "", "session to continue: a Harness Kit ID (ses_..., from `harness-kit sessions`) or a runtime-native ID with -runtime")
	fs.StringVar(&sf.instructions, "instructions", "", "extra instructions appended to the runtime's system prompt")
	fs.StringVar(&sf.title, "title", "", "name the session (default: the start of the first prompt); with -resume ses_..., renames it")
	fs.StringVar(&sf.options, "options", "", `runtime-specific options as JSON, e.g. '{"permissionMode":"acceptEdits"}' (see docs/runtimes.md)`)
	fs.StringVar(&sf.approve, "approve", "", "how to answer approval requests: ask (default when interactive), allow, or deny")
	sf.out.register(fs, "text (readable reply), json (one result object per turn), stream-json (every event as a JSON line)")
	fs.BoolVar(&sf.raw, "raw", false, "also emit unmapped native runtime events (runtime.raw)")
	fs.StringVar(&sf.mcpFile, "mcp", "", "add the MCP servers in this file (harness-kit.json format) for this session")
	fs.BoolVar(&sf.noNativeMCP, "no-native-mcp", false, "don't load the MCP servers configured in the runtime itself")
}

// prepare finalises flags after parsing: the requested runtime is always
// registered, unless --runtimes was set explicitly.
func (sf *sessionFlags) prepare(fs *flag.FlagSet) error {
	explicit := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "runtimes" {
			explicit = true
		}
	})
	if !explicit {
		sf.runtimes = sf.runtime
		if isSessionID(sf.resume) {
			sf.runtimes = "codex,claude,fake" // the stored session names its runtime
		}
	}
	if isSessionID(sf.resume) && !sf.persistent() {
		return fmt.Errorf("-resume %s needs a session store (--store)", sf.resume)
	}
	switch sf.approve {
	case "":
		sf.approve = "ask"
	case "ask", "allow", "deny":
	default:
		return fmt.Errorf("--approve must be ask, allow or deny")
	}
	if err := sf.out.resolve("text", "json", "stream-json"); err != nil {
		return err
	}
	if sf.options != "" && !json.Valid([]byte(sf.options)) {
		return fmt.Errorf("--options is not valid JSON")
	}
	if sf.turnSettingsRaw != "" && sf.turnConfig != "" {
		return errors.New("--turn-settings and --turn-config cannot both be supplied")
	}
	for _, item := range []struct {
		raw    string
		target *settings.Patch
	}{{sf.sessionSettingsRaw, &sf.sessionSettings}, {sf.turnSettingsRaw, &sf.turnSettings}} {
		if item.raw != "" {
			if err := json.Unmarshal([]byte(item.raw), item.target); err != nil {
				return err
			}
		}
	}
	if sf.turnConfig != "" {
		patch, err := parseConfigPatch(sf.turnConfig)
		if err != nil {
			return fmt.Errorf("--turn-config: %w", err)
		}
		sf.turnPatch = &patch
	}
	abs, err := filepath.Abs(sf.cwd)
	if err != nil {
		return err
	}
	sf.cwd = abs
	return nil
}

func (sf *sessionFlags) createParams() (protocol.SessionCreateParams, error) {
	extra, err := sessionMCP(sf.mcpFile, sf.noNativeMCP)
	if err != nil {
		return protocol.SessionCreateParams{}, err
	}
	p := protocol.SessionCreateParams{
		Settings: sf.sessionSettings,
		MCP:      extra,
		Runtime:  sf.runtime, Cwd: sf.cwd, Model: sf.model, Effort: sf.effort, ResumeID: sf.resume,
		Instructions: sf.instructions, RawEvents: sf.raw, Title: sf.title,
		Metadata: map[string]string{"client": "harness-kit-cli"},
	}
	if sf.options != "" {
		p.Options = json.RawMessage(sf.options)
	}
	return p, nil
}

// parseConfigPatch requires an object so null cannot silently become a no-op.
func parseConfigPatch(raw string) (agent.ConfigPatch, error) {
	var patch agent.ConfigPatch
	if !strings.HasPrefix(strings.TrimSpace(raw), "{") {
		return patch, errors.New("configuration must be a JSON object")
	}
	if err := json.Unmarshal([]byte(raw), &patch); err != nil {
		return patch, fmt.Errorf("invalid configuration JSON: %w", err)
	}
	return patch, nil
}
