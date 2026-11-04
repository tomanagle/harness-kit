package claude

import (
	"math"
	"testing"

	"github.com/tomanagle/harness-kit/agent"
	"github.com/tomanagle/harness-kit/event"
)

func TestResultCostVersionAndForkBaselines(t *testing.T) {
	for _, tc := range []struct {
		name, version string
		cfg           agent.SessionConfig
		wantFirst     *float64
		wantSecond    float64
		sessionEvents int
	}{
		{"modern fresh", "2.1.317", agent.SessionConfig{}, ptrCost(.25), .15, 2},
		{"modern resume", "2.1.317", agent.SessionConfig{ResumeID: "parent"}, nil, .15, 2},
		{"modern fork", "2.1.317", agent.SessionConfig{ForkFrom: "parent"}, nil, .15, 2},
		{"old resume", "2.1.276", agent.SessionConfig{ResumeID: "parent"}, ptrCost(.25), .40, 0},
		{"old fork", "2.1.276", agent.SessionConfig{ForkFrom: "parent"}, ptrCost(.25), .40, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			s := &Session{version: tc.version, cfg: tc.cfg, h: rec}
			reader := &turnReader{s: s, turnID: "first"}
			reader.resultLine(streamLine{Subtype: "success", TotalCostUSD: ptrCost(.25)})
			got := reader.result.Usage.CostUSD
			if (got == nil) != (tc.wantFirst == nil) || (got != nil && math.Abs(*got-*tc.wantFirst) > 1e-9) {
				t.Fatalf("first turn cost=%v expected=%v", got, tc.wantFirst)
			}
			reader.turnID = "second"
			reader.resultLine(streamLine{Subtype: "success", TotalCostUSD: ptrCost(.40)})
			if got = reader.result.Usage.CostUSD; got == nil || math.Abs(*got-tc.wantSecond) > 1e-9 {
				t.Fatalf("second turn cost=%v expected=%v", got, tc.wantSecond)
			}
			count := 0
			for _, ev := range rec.ofType(event.Usage) {
				if ev.Data.(event.UsageData).Scope == "session" {
					count++
				}
			}
			if count != tc.sessionEvents {
				t.Fatalf("session events=%d expected=%d", count, tc.sessionEvents)
			}
		})
	}
}

func ptrCost(v float64) *float64 { return &v }
