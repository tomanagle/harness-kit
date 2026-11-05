package codex

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/tomanagle/harness-kit/core/agent"
	"github.com/tomanagle/harness-kit/core/config"
)

func describeSupport(fields map[string]config.Support) map[string]config.Support {
	descriptors := map[string]config.Descriptor{
		"model":                     {Type: "string", AllowCustom: true, Source: "unknown", Description: "Model identifiers reported by the installed Codex runtime; custom gateways may accept other identifiers."},
		"effort":                    {Type: "string", AllowCustom: true, Source: "unknown", Description: "Reasoning effort supported by the selected model."},
		"options.sandbox":           {Type: "string", Source: "declared", Choices: config.Choices("read-only", "workspace-write", "danger-full-access"), Description: "Filesystem and network boundary. Full access requires the host allowUnsafe gate."},
		"options.approvalPolicy":    {Type: "string", Source: "declared", Choices: config.Choices("on-request", "never"), Description: "When Codex requests approval. Legacy untrusted configurations remain accepted for compatibility."},
		"options.approvalsReviewer": {Type: "string", Source: "declared", Choices: config.Choices("user", "auto_review"), Description: "Who reviews eligible approval requests."},
	}
	for name, descriptor := range descriptors {
		support := fields[name]
		support.Descriptor = descriptor
		fields[name] = support
	}
	return fields
}

// DescribeConfiguration queries config/model metadata without creating a thread.
func (r *Runtime) DescribeConfiguration(ctx context.Context, cfg agent.SessionConfig) (map[string]config.Descriptor, error) {
	path, err := r.lookPath()
	if err != nil {
		return nil, err
	}
	s, err := r.spawn(ctx, path, cfg.Cwd, nil)
	if err != nil {
		return nil, err
	}
	defer s.Close(context.Background())
	s.cfg = cfg
	s.captureDefaults(ctx)
	descriptors := map[string]config.Descriptor{}
	for name, support := range describeSupport(map[string]config.Support{}) {
		descriptors[name] = support.Descriptor
	}
	model := descriptors["model"]
	var ids []string
	for id := range s.modelEfforts {
		if id != "" {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	model.Choices = config.Choices(ids...)
	if len(ids) > 0 {
		model.Source = "reported"
	}
	if s.nativeModel != "" {
		model.Default, _ = json.Marshal(s.nativeModel)
	}
	descriptors["model"] = model
	selected := cfg.Model
	if selected == "" {
		selected = s.nativeModel
	}
	effort := descriptors["effort"]
	effort.Choices = config.Choices(s.supportedEfforts[selected]...)
	if len(effort.Choices) > 0 {
		effort.Source = "reported"
	}
	value := s.modelEfforts[selected]
	if cfg.Model == "" && s.nativeEffort != "" {
		value = s.nativeEffort
	}
	if value != "" {
		effort.Default, _ = json.Marshal(value)
	}
	descriptors["effort"] = effort
	return descriptors, ctx.Err()
}
