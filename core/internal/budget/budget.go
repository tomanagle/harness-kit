// Package budget implements private observed-usage accounting. Public policy
// and event contracts live in config and event.
package budget

import (
	"math"

	"github.com/tomanagle/harness-kit/core/config"
	"github.com/tomanagle/harness-kit/core/event"
)

// Observation is either an absolute snapshot (default) or an incremental delta.
// Session snapshots cover the entire native conversation; turn snapshots cover
// the current turn. A nil metric means unknown, including context occupancy.
type Observation struct {
	Scope, Mode string
	Tokens      *int64
	CostUSD     *float64
}

// Metric stores a bounded accounting checkpoint. Snapshot high water marks
// prevent repeated or delayed cumulative updates from counting twice.
type Metric struct {
	Total          float64 `json:"total"`
	Native         float64 `json:"native"`
	NativeKnown    bool    `json:"nativeKnown"`
	Known          bool    `json:"known"`
	Incomplete     bool    `json:"incomplete"`
	Base           float64 `json:"base"`
	Turn           float64 `json:"turn"`
	SegmentOffset  float64 `json:"segmentOffset,omitempty"`
	TurnKnown      bool    `json:"turnKnown"`
	NativeStart    float64 `json:"nativeStart"`
	NativeObserved bool    `json:"nativeObserved"`
}

// Ledger is persisted with a session record and can be recovered by replaying
// subsequent events. It retains no event or turn-ID collection.
type Ledger struct {
	Tokens Metric `json:"tokens"`
	Cost   Metric `json:"cost"`
	Active bool   `json:"active"`
}

// New starts accounting. An external resume has no known native baseline:
// its first cumulative snapshot counts toward session limits, but cannot be
// attributed entirely to its first kit turn.
func New(externalResume bool) Ledger {
	return Ledger{Tokens: Metric{NativeKnown: !externalResume, Incomplete: externalResume}, Cost: Metric{NativeKnown: !externalResume, Incomplete: externalResume}}
}

func (l *Ledger) Begin() { l.Active = true; l.Tokens.begin(); l.Cost.begin() }
func (m *Metric) begin() {
	m.Base = m.Total
	m.Turn = 0
	m.SegmentOffset = 0
	m.TurnKnown = false
	m.NativeStart = m.Total
	m.NativeObserved = false
}
func (l *Ledger) Observe(o Observation) {
	if o.Mode != "" && o.Mode != "snapshot" && o.Mode != "delta" {
		return
	}
	if o.Scope != "" && o.Scope != "turn" && o.Scope != "session" {
		return
	}
	if o.Tokens != nil && *o.Tokens >= 0 {
		l.Tokens.observe(float64(*o.Tokens), o.Scope, o.Mode, l.Active)
	}
	if o.CostUSD != nil && *o.CostUSD >= 0 && !math.IsNaN(*o.CostUSD) && !math.IsInf(*o.CostUSD, 0) {
		l.Cost.observe(*o.CostUSD, o.Scope, o.Mode, l.Active)
	}
}
func (m *Metric) observe(value float64, scope, mode string, active bool) {
	m.Known = true
	if scope == "session" {
		if mode == "delta" {
			anchor := m.Base
			if !active {
				anchor = m.Total
			}
			m.Native = math.Max(m.Native, anchor) + value
			if !m.NativeKnown {
				m.NativeStart = m.Base
				m.NativeKnown = true
			}
		} else {
			if !m.NativeKnown {
				m.NativeStart = value
				m.NativeKnown = true
			}
			m.Native = math.Max(m.Native, value)
			m.Incomplete = false
		}
		m.NativeObserved = true
	} else if active {
		if mode == "delta" {
			m.Turn += value
		} else {
			m.Turn = math.Max(m.Turn, value)
		}
		m.TurnKnown = true
	}
	if active {
		m.Total = math.Max(m.Base+m.SegmentOffset+m.Turn, m.Native)
	} else {
		m.Total = math.Max(m.Total, m.Native)
	}
}
func (l *Ledger) End() {
	if !l.Active {
		return
	}
	if !l.Tokens.TurnKnown && !l.Tokens.NativeObserved {
		l.Tokens.Incomplete = true
	}
	if !l.Cost.TurnKnown && !l.Cost.NativeObserved {
		l.Cost.Incomplete = true
	}
	l.Active = false
}

// Interrupt closes an unfinished recovered turn. Further native work may have
// happened after its last saved update, so its totals remain lower bounds.
func (l *Ledger) Interrupt() {
	if !l.Active {
		return
	}
	l.End()
	l.Tokens.Incomplete = true
	l.Cost.Incomplete = true
}
func (l Ledger) Usage() event.BudgetUsage {
	u := event.BudgetUsage{TotalTokens: tokens(l.Tokens.Total), TokensComplete: !l.Tokens.Incomplete, CostComplete: !l.Cost.Incomplete}
	if l.Cost.Known {
		v := l.Cost.Total
		u.CostUSD = &v
	}
	return u
}
func tokens(n float64) int64 {
	if n >= math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(n)
}
func (m Metric) turnUsage() float64 {
	return math.Max(m.SegmentOffset+m.Turn, math.Max(0, m.Native-m.NativeStart))
}

// NextSegment starts another native invocation inside the same kit turn. Each
// invocation's turn snapshots begin at zero; session snapshots remain cumulative.
func (l *Ledger) NextSegment() {
	if !l.Active {
		return
	}
	if !l.Tokens.TurnKnown && !l.Tokens.NativeObserved {
		l.Tokens.Incomplete = true
	}
	if !l.Cost.TurnKnown && !l.Cost.NativeObserved {
		l.Cost.Incomplete = true
	}
	l.Tokens.SegmentOffset = l.Tokens.turnUsage()
	l.Cost.SegmentOffset = l.Cost.turnUsage()
	l.Tokens.Turn, l.Cost.Turn = 0, 0
	l.Tokens.TurnKnown, l.Cost.TurnKnown = false, false
	l.Tokens.NativeObserved, l.Cost.NativeObserved = false, false
}

// Check compares usage with limits. Admission uses zero turn usage and still
// rejects an explicit zero, even if a runtime cannot report that metric.
func (l Ledger) Check(p config.BudgetLimits, admission bool) *event.BudgetExceededData {
	turnTokens, turnCost := l.Tokens.turnUsage(), l.Cost.turnUsage()
	if admission {
		turnTokens, turnCost = 0, 0
	}
	checks := []struct {
		scope, metric string
		limit         *float64
		used          float64
	}{
		{"session", "tokens", asFloat(p.MaxSessionTokens), l.Tokens.Total},
		{"session", "costUsd", p.MaxSessionCostUSD, l.Cost.Total},
		{"turn", "tokens", asFloat(p.MaxTurnTokens), turnTokens},
		{"turn", "costUsd", p.MaxTurnCostUSD, turnCost},
	}
	for _, c := range checks {
		if c.limit != nil && c.used >= *c.limit {
			return &event.BudgetExceededData{Scope: c.scope, Metric: c.metric, Limit: *c.limit, Observed: c.used}
		}
	}
	return nil
}
func asFloat(n *int64) *float64 {
	if n == nil {
		return nil
	}
	v := float64(*n)
	return &v
}
