package config

// BudgetLimits are host policy. Nil disables a limit; zero admits no further work.
type BudgetLimits struct {
	MaxSessionTokens  *int64   `json:"maxSessionTokens"`
	MaxSessionCostUSD *float64 `json:"maxSessionCostUsd"`
	MaxTurnTokens     *int64   `json:"maxTurnTokens"`
	MaxTurnCostUSD    *float64 `json:"maxTurnCostUsd"`
}

func (l BudgetLimits) Clone() BudgetLimits {
	if l.MaxSessionTokens != nil {
		v := *l.MaxSessionTokens
		l.MaxSessionTokens = &v
	}
	if l.MaxSessionCostUSD != nil {
		v := *l.MaxSessionCostUSD
		l.MaxSessionCostUSD = &v
	}
	if l.MaxTurnTokens != nil {
		v := *l.MaxTurnTokens
		l.MaxTurnTokens = &v
	}
	if l.MaxTurnCostUSD != nil {
		v := *l.MaxTurnCostUSD
		l.MaxTurnCostUSD = &v
	}
	return l
}
