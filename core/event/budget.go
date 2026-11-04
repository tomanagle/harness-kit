package event

// BudgetUsage contains observed totals. Incomplete totals are lower bounds; nil cost
// means no cost estimate was supplied. Reported costs are estimates, not bills.
type BudgetUsage struct {
	TotalTokens    int64    `json:"totalTokens"`
	CostUSD        *float64 `json:"costUsd,omitempty"`
	TokensComplete bool     `json:"tokensComplete"`
	CostComplete   bool     `json:"costComplete"`
}

func (u BudgetUsage) Clone() BudgetUsage {
	if u.CostUSD != nil {
		v := *u.CostUSD
		u.CostUSD = &v
	}
	return u
}

// BudgetExceededData identifies the observed threshold that stopped further work.
type BudgetExceededData struct {
	Scope    string  `json:"scope"`
	Metric   string  `json:"metric"`
	Limit    float64 `json:"limit"`
	Observed float64 `json:"observed"`
}
