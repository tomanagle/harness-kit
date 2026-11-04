package budget

import (
	"testing"
)

func integer(n int64) *int64    { return &n }
func amount(n float64) *float64 { return &n }
func TestSnapshotsDeltasAndFinalTotalsDoNotDoubleCount(t *testing.T) {
	l := New(false)
	l.Begin()
	for _, n := range []int64{10, 10, 7, 15} {
		l.Observe(Observation{Scope: "session", Tokens: integer(n), CostUSD: amount(float64(n) / 100)})
	}
	l.Observe(Observation{Scope: "turn", Tokens: integer(15), CostUSD: amount(.15)})
	l.Observe(Observation{Scope: "turn", Tokens: integer(15), CostUSD: amount(.15)})
	l.End()
	if u := l.Usage(); u.TotalTokens != 15 || *u.CostUSD != .15 {
		t.Fatal(u)
	}
	l.Begin()
	l.Observe(Observation{Scope: "turn", Mode: "delta", Tokens: integer(2)})
	l.Observe(Observation{Scope: "turn", Mode: "delta", Tokens: integer(3)})
	l.Observe(Observation{Scope: "turn", Tokens: integer(5)})
	l.Observe(Observation{Scope: "session", Tokens: integer(20)})
	l.End()
	if u := l.Usage(); u.TotalTokens != 20 || u.CostComplete {
		t.Fatal(u)
	}
}
func TestMixedSessionAndTurnReportingAcrossTurns(t *testing.T) {
	l := New(false)
	l.Begin()
	l.Observe(Observation{Scope: "turn", Tokens: integer(10)})
	l.End()
	l.Begin()
	l.Observe(Observation{Scope: "session", Tokens: integer(20)})
	l.Observe(Observation{Scope: "turn", Tokens: integer(10)})
	l.End()
	l.Begin()
	l.Observe(Observation{Scope: "turn", Tokens: integer(10)})
	l.End()
	if l.Usage().TotalTokens != 30 {
		t.Fatal(l)
	}
}
func TestExternalResumeAndUnknownMetrics(t *testing.T) {
	l := New(true)
	l.Begin()
	l.Observe(Observation{Scope: "session", CostUSD: amount(100)})
	if hit := l.Check(Limits{MaxTurnCostUSD: amount(1)}, false); hit != nil {
		t.Fatal("attributed historical cost to turn", hit)
	}
	if hit := l.Check(Limits{MaxSessionCostUSD: amount(99)}, false); hit == nil {
		t.Fatal("ignored observed historical total")
	}
	l.End()
	if l.Usage().TokensComplete || !l.Usage().CostComplete {
		t.Fatal(l.Usage())
	}
	l.Begin()
	l.Observe(Observation{Scope: "session", CostUSD: amount(101)})
	if hit := l.Check(Limits{MaxTurnCostUSD: amount(1)}, false); hit == nil {
		t.Fatal("lost restored native baseline")
	}
	if hit := l.Check(Limits{MaxTurnTokens: integer(0)}, true); hit == nil {
		t.Fatal("zero must deny even unknown metrics")
	}
}

func TestExternalResumeStaysIncompleteWithoutNativeSnapshot(t *testing.T) {
	l := New(true)
	l.Begin()
	l.Observe(Observation{Scope: "turn", Tokens: integer(10), CostUSD: amount(1)})
	l.Observe(Observation{Scope: "session", Mode: "delta", Tokens: integer(10), CostUSD: amount(1)})
	l.End()
	if u := l.Usage(); u.TokensComplete || u.CostComplete {
		t.Fatal(u)
	}
	l.Begin()
	l.Observe(Observation{Scope: "session", Tokens: integer(100), CostUSD: amount(10)})
	l.End()
	if u := l.Usage(); !u.TokensComplete || !u.CostComplete {
		t.Fatal(u)
	}
}

func TestSessionDeltaOutsideTurnAddsToObservedTotal(t *testing.T) {
	l := New(false)
	l.Begin()
	l.Observe(Observation{Scope: "turn", Tokens: integer(10)})
	l.End()
	l.Observe(Observation{Scope: "session", Mode: "delta", Tokens: integer(2)})
	if l.Usage().TotalTokens != 12 {
		t.Fatal(l)
	}
}
