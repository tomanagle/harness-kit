package claude

import "github.com/tomanagle/harness-kit/core/config"

func describeSupport(fields map[string]config.Support) map[string]config.Support {
	descriptors := map[string]config.Descriptor{
		"model":                  {Type: "string", AllowCustom: true, Source: "declared", Choices: config.Choices("sonnet", "opus", "haiku", "fable", "best", "opusplan"), Description: "CLI aliases and custom model IDs. Availability depends on account, gateway and managed policies."},
		"effort":                 {Type: "string", Source: "declared", Choices: config.Choices("low", "medium", "high", "xhigh", "max"), Description: "Requested effort. Native model support, environment and organization caps can constrain the applied level."},
		"options.permissionMode": {Type: "string", Source: "declared", Choices: config.Choices("default", "acceptEdits", "auto", "plan", "dontAsk", "bypassPermissions"), Description: "Native permission mode. Bypass requires host allowUnsafe; auto availability depends on the runtime and account."},
	}
	for name, descriptor := range descriptors {
		support := fields[name]
		support.Descriptor = descriptor
		fields[name] = support
	}
	return fields
}
