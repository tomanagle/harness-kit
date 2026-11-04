package claude

import (
	"math"

	"github.com/tomanagle/harness-kit/core/event"
)

// Assistant message usage describes one request. Result usage/modelUsage are
// cumulative consumption and must never be interpreted as window occupancy.
// https://code.claude.com/docs/en/agent-sdk/typescript
type messageUsage struct {
	InputTokens              *int64 `json:"input_tokens"`
	OutputTokens             int64  `json:"output_tokens"`
	CacheReadInputTokens     int64  `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64  `json:"cache_creation_input_tokens"`
}

func (t *turnReader) observeContext(m streamLine) {
	if usage := m.ContextUsage; usage != nil {
		if usage.TotalTokens == nil || *usage.TotalTokens < 0 || usage.RawMaxTokens == nil || *usage.RawMaxTokens <= 0 {
			return
		}
		// /context reports Claude's current-context estimate and effective
		// window, which may be lower than the model's hard limit.
		t.contextModel = usage.Model
		t.contextUsage = &event.ContextUsageData{UsedTokens: usage.TotalTokens, WindowTokens: usage.RawMaxTokens, Basis: "reported"}
		t.emit(event.ContextUsage, *t.contextUsage)
		return
	}
	usage := m.Message.Usage
	if usage == nil || usage.InputTokens == nil {
		return
	}
	used := int64(0)
	for _, n := range []int64{*usage.InputTokens, usage.OutputTokens, usage.CacheReadInputTokens, usage.CacheCreationInputTokens} {
		if n < 0 || n > math.MaxInt64-used {
			return
		}
		used += n
	}
	t.contextModel = m.Message.Model
	t.contextUsage = &event.ContextUsageData{UsedTokens: &used, Basis: "last_request"}
	t.emit(event.ContextUsage, *t.contextUsage)
}
