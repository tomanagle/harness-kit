package settings

import (
	"encoding/json"
	"testing"
)

func TestBudgetSettingsValidateAndReset(t *testing.T) {
	base := Defaults()
	next, _, err := Apply(base, Patch{"budget": json.RawMessage(`{"maxSessionTokens":0,"maxTurnCostUsd":0.25}`)}, nil, base)
	if err != nil || next.Budget.MaxSessionTokens == nil || *next.Budget.MaxSessionTokens != 0 || next.Budget.MaxTurnCostUSD == nil || *next.Budget.MaxTurnCostUSD != .25 {
		t.Fatal(next.Budget, err)
	}
	reset, _, err := Apply(next, Patch{}, []string{"budget.maxTurnCostUsd"}, base)
	if err != nil || reset.Budget.MaxTurnCostUSD != nil {
		t.Fatal(reset.Budget, err)
	}
	for _, raw := range []string{`{"maxTurnTokens":-1}`, `{"maxTurnTokens":1.5}`, `{"maxTurnCostUsd":-0.01}`, `{"maxTurnCostUsd":"1"}`, `{"maxTurnCostUsd":1e999}`, `{"unknown":1}`} {
		if _, _, err := Apply(base, Patch{"budget": json.RawMessage(raw)}, nil, base); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
