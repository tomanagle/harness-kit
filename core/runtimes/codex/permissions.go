package codex

import (
	"encoding/json"
	"fmt"
	"github.com/tomanagle/harness-kit/core/agent"
	"maps"
	"slices"
)

type workspacePolicy struct {
	WritableRoots       []string `json:"writable_roots"`
	NetworkAccess       bool     `json:"network_access"`
	ExcludeSlashTmp     bool     `json:"exclude_slash_tmp"`
	ExcludeTmpdirEnvVar bool     `json:"exclude_tmpdir_env_var"`
}

func (s *Session) resolvePermissions(settings *agent.TurnConfig, options Options) error {
	s.mu.Lock()
	changed := maps.Clone(s.permissionOverrideUsed)
	s.mu.Unlock()
	for key, pair := range map[string][2]string{
		"sandbox":           {options.Sandbox, s.nativeSandbox},
		"approvalPolicy":    {options.ApprovalPolicy, s.nativeApproval},
		"approvalsReviewer": {options.ApprovalsReviewer, s.nativeReviewer},
	} {
		value := pair[0]
		if value == "" {
			nativeKey := map[string]string{"sandbox": "sandbox_mode", "approvalPolicy": "approval_policy", "approvalsReviewer": "approvals_reviewer"}[key]
			if configured, ok := options.Config[nativeKey].(string); ok && configured != "" {
				// Preserve the caller's native override without adding a conflicting
				// portable option when PrepareTurn is called again by Prompt.
				continue
			}
		}
		if value == "" {
			value = pair[1]
		}
		if value == "" {
			if changed[key] {
				return fmt.Errorf("%w: codex did not report native %s; choose an explicit value", agent.ErrUnsupported, key)
			}
			continue
		}
		if settings.Options == nil {
			settings.Options = make(map[string]json.RawMessage)
		}
		settings.Options[key], _ = json.Marshal(value)
	}
	return nil
}

func (s *Session) permissionParams(settings agent.TurnConfig) map[string]any {
	var options Options
	_ = agent.DecodeOptions(settings.Options, &options)
	for key, destination := range map[string]*string{
		"sandbox_mode":       &options.Sandbox,
		"approval_policy":    &options.ApprovalPolicy,
		"approvals_reviewer": &options.ApprovalsReviewer,
	} {
		if *destination == "" {
			*destination, _ = options.Config[key].(string)
		}
	}
	params := map[string]any{}
	if options.ApprovalPolicy != "" {
		params["approvalPolicy"] = options.ApprovalPolicy
	}
	if options.ApprovalsReviewer != "" {
		params["approvalsReviewer"] = options.ApprovalsReviewer
	}
	switch options.Sandbox {
	case "danger-full-access":
		params["sandboxPolicy"] = map[string]any{"type": "dangerFullAccess"}
	case "read-only":
		params["sandboxPolicy"] = map[string]any{"type": "readOnly"}
	case "workspace-write":
		workspace := s.nativeWorkspace
		if v, ok := options.Config["sandbox_workspace_write"]; ok {
			raw, _ := json.Marshal(v)
			_ = json.Unmarshal(raw, &workspace)
		}
		var raw []byte
		overrides := map[string]any{}
		for _, key := range []string{"writable_roots", "network_access", "exclude_slash_tmp", "exclude_tmpdir_env_var"} {
			if v, ok := options.Config["sandbox_workspace_write."+key]; ok {
				overrides[key] = v
			}
		}
		raw, _ = json.Marshal(overrides)
		_ = json.Unmarshal(raw, &workspace)
		roots := slices.Clone(workspace.WritableRoots)
		if len(options.AddDirs) > 0 {
			roots = slices.Clone(options.AddDirs)
		}
		if s.cfg.Cwd != "" && !slices.Contains(roots, s.cfg.Cwd) {
			roots = append(roots, s.cfg.Cwd)
		}
		if roots == nil {
			roots = []string{}
		}
		params["sandboxPolicy"] = map[string]any{"type": "workspaceWrite", "writableRoots": roots, "networkAccess": workspace.NetworkAccess, "excludeSlashTmp": workspace.ExcludeSlashTmp, "excludeTmpdirEnvVar": workspace.ExcludeTmpdirEnvVar}
	}
	if options.Sandbox != "" && options.Sandbox == s.initialSandboxMode && len(s.initialSandbox) > 0 {
		params["sandboxPolicy"] = json.RawMessage(s.initialSandbox)
	}
	return params
}
