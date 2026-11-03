module github.com/tomanagle/harness-kit/providers/openai

go 1.26.0

require (
	github.com/tomanagle/harness-kit v0.1.0
	github.com/openai/openai-go/v3 v3.93.0
)

require (
	github.com/coder/websocket v1.8.15 // indirect
	github.com/tidwall/gjson v1.20.0 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
)

replace github.com/tomanagle/harness-kit => ../..
