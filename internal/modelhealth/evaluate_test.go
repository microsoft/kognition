// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package modelhealth

import (
	"strings"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/store"
)

func TestMergeSumsCountsAndFindsTheLatestFault(t *testing.T) {
	base := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	merged := Merge([]store.ModelCallHealth{
		{Target: "t", Calls: 4, Faults: 2, ConsecutiveFaults: 2, LastFaultReason: cpi.ReasonTimeout, LastFaultAt: base},
		{Target: "t", Calls: 6, Faults: 1, ConsecutiveFaults: 1, LastFaultReason: cpi.ReasonRateLimited, LastFaultAt: base.Add(time.Second)},
	})
	want := Aggregate{Calls: 10, Faults: 3, ConsecutiveFaults: 3, LastFaultReason: cpi.ReasonRateLimited}
	if merged != want {
		t.Fatalf("Merge() = %+v, want %+v", merged, want)
	}
	if got := Merge(nil); got != (Aggregate{}) {
		t.Fatalf("Merge(nil) = %+v, want the zero aggregate", got)
	}
}

func TestMergeStreaksOnlyCountWhenNoReporterSawTheOppositeOutcomeSince(t *testing.T) {
	base := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	t.Run("a later success on another reporter ends the fault streak", func(t *testing.T) {
		merged := Merge([]store.ModelCallHealth{
			{Calls: 3, Faults: 3, ConsecutiveFaults: 3, LastFaultReason: cpi.ReasonTimeout, LastFaultAt: base},
			{Calls: 2, ConsecutiveSuccesses: 2, LastSuccessAt: base.Add(time.Second)},
		})
		if merged.ConsecutiveFaults != 0 || merged.ConsecutiveSuccesses != 2 {
			t.Fatalf("Merge() = %+v, want the fault streak ended and 2 successes", merged)
		}
	})
	t.Run("a later fault on another reporter ends the success streak", func(t *testing.T) {
		merged := Merge([]store.ModelCallHealth{
			{Calls: 3, ConsecutiveSuccesses: 3, LastSuccessAt: base},
			{Calls: 1, Faults: 1, ConsecutiveFaults: 1, LastFaultReason: cpi.ReasonTimeout, LastFaultAt: base.Add(time.Second)},
		})
		if merged.ConsecutiveSuccesses != 0 || merged.ConsecutiveFaults != 1 {
			t.Fatalf("Merge() = %+v, want the success streak ended and 1 fault", merged)
		}
	})
	t.Run("concurrent streaks on different reporters add up", func(t *testing.T) {
		merged := Merge([]store.ModelCallHealth{
			{Calls: 2, Faults: 2, ConsecutiveFaults: 2, LastFaultReason: cpi.ReasonTimeout, LastFaultAt: base},
			{Calls: 2, Faults: 2, ConsecutiveFaults: 2, LastFaultReason: cpi.ReasonTimeout, LastFaultAt: base.Add(time.Second)},
		})
		if merged.ConsecutiveFaults != 4 {
			t.Fatalf("Merge() = %+v, want a combined streak of 4", merged)
		}
	})
}

func TestMergeByTargetGroupsRecords(t *testing.T) {
	merged := MergeByTarget([]store.ModelCallHealth{
		{Target: "a", Calls: 1}, {Target: "b", Calls: 2}, {Target: "a", Calls: 4},
	})
	if len(merged) != 2 || merged["a"].Calls != 5 || merged["b"].Calls != 2 {
		t.Fatalf("MergeByTarget() = %+v, want a=5 and b=2 calls", merged)
	}
}

func TestEvaluateAppliesThePolicy(t *testing.T) {
	tests := []struct {
		name        string
		aggregate   Aggregate
		wasDegraded bool
		want        bool
		wantReason  string
	}{
		{"no calls", Aggregate{}, false, false, ""},
		{"no faults", Aggregate{Calls: 20, ConsecutiveSuccesses: 20}, false, false, ""},
		{"two consecutive faults are tolerated", Aggregate{Calls: 2, Faults: 2, ConsecutiveFaults: 2, LastFaultReason: cpi.ReasonRateLimited}, false, false, ""},
		{"three consecutive faults degrade", Aggregate{Calls: 3, Faults: 3, ConsecutiveFaults: 3, LastFaultReason: cpi.ReasonRateLimited}, false, true, cpi.ReasonRateLimited},
		{"rejected credentials degrade after two", Aggregate{Calls: 2, Faults: 2, ConsecutiveFaults: 2, LastFaultReason: cpi.ReasonAuthenticationFailed}, false, true, cpi.ReasonAuthenticationFailed},
		{"exhausted quota degrades after two", Aggregate{Calls: 2, Faults: 2, ConsecutiveFaults: 2, LastFaultReason: cpi.ReasonQuotaOrBilling}, false, true, cpi.ReasonQuotaOrBilling},
		{"one rejected credential is tolerated", Aggregate{Calls: 1, Faults: 1, ConsecutiveFaults: 1, LastFaultReason: cpi.ReasonAuthenticationFailed}, false, false, ""},
		{"half of enough calls failing degrades", Aggregate{Calls: 10, Faults: 5, ConsecutiveFaults: 1, LastFaultReason: cpi.ReasonEndpointError}, false, true, cpi.ReasonEndpointError},
		{"a failure rate recovered by the latest call does not degrade", Aggregate{Calls: 10, Faults: 6, ConsecutiveSuccesses: 1, LastFaultReason: cpi.ReasonEndpointError}, false, false, ""},
		{"too few calls for a rate", Aggregate{Calls: 4, Faults: 2, ConsecutiveFaults: 1, LastFaultReason: cpi.ReasonEndpointError}, false, false, ""},
		{"a low failure rate does not degrade", Aggregate{Calls: 10, Faults: 4, ConsecutiveFaults: 1, LastFaultReason: cpi.ReasonEndpointError}, false, false, ""},
		{"degradation is sticky until enough successes", Aggregate{Calls: 10, Faults: 3, ConsecutiveSuccesses: 2, LastFaultReason: cpi.ReasonTimeout}, true, true, cpi.ReasonTimeout},
		{"enough successes recover", Aggregate{Calls: 10, Faults: 3, ConsecutiveSuccesses: 3, LastFaultReason: cpi.ReasonTimeout}, true, false, ""},
		{"a degradation ends when the failures leave the window", Aggregate{Calls: 5, ConsecutiveSuccesses: 5}, true, false, ""},
		{"unknown reasons are reported as Other", Aggregate{Calls: 3, Faults: 3, ConsecutiveFaults: 3, LastFaultReason: "anything"}, false, true, cpi.ReasonOther},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			verdict := Evaluate(test.aggregate, test.wasDegraded)
			if verdict.Degraded != test.want || verdict.Reason != test.wantReason {
				t.Fatalf("Evaluate() = %+v, want degraded %t with reason %q", verdict, test.want, test.wantReason)
			}
			if verdict.Degraded && !strings.HasPrefix(verdict.Message, MessagePrefix) {
				t.Errorf("Message = %q, want the %q prefix", verdict.Message, MessagePrefix)
			}
			if !verdict.Degraded && (verdict.Message != "" || verdict.Reason != "") {
				t.Errorf("a healthy verdict must be empty: %+v", verdict)
			}
		})
	}
}

func TestEvaluateMessageIsStableWhileFailuresContinue(t *testing.T) {
	first := Evaluate(Aggregate{Calls: 3, Faults: 3, ConsecutiveFaults: 3, LastFaultReason: cpi.ReasonRateLimited}, false)
	later := Evaluate(Aggregate{Calls: 30, Faults: 29, ConsecutiveFaults: 25, LastFaultReason: cpi.ReasonRateLimited}, true)
	if first.Message != later.Message || first.Message == "" {
		t.Fatalf("messages differ while the failures continue: %q vs %q", first.Message, later.Message)
	}
}
