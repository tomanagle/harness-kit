package event

// ContextUsageData is the latest model-window observation, independent of
// cumulative token consumption. Nil counts are unknown; zero is a known zero.
// Basis is reported (native occupancy), last_request (the latest model request)
// or unknown. Stale observations must not be presented as current occupancy.
type ContextUsageData struct {
	UsedTokens   *int64 `json:"usedTokens"`
	WindowTokens *int64 `json:"windowTokens"`
	Basis        string `json:"basis"`
	Stale        bool   `json:"stale"`
}

func (v ContextUsageData) Clone() ContextUsageData {
	if v.UsedTokens != nil {
		n := *v.UsedTokens
		v.UsedTokens = &n
	}
	if v.WindowTokens != nil {
		n := *v.WindowTokens
		v.WindowTokens = &n
	}
	return v
}

// ContextCompactionData reports manual, threshold-triggered, and observed native
// compaction using one lifecycle. Runtime-specific details remain in runtime.status.
type ContextCompactionData struct {
	OperationID string            `json:"operationId"`
	Reason      string            `json:"reason"`
	Status      string            `json:"status"`
	Before      ContextUsageData  `json:"before"`
	After       *ContextUsageData `json:"after,omitempty"`
	Error       *ErrorInfo        `json:"error,omitempty"`
}
