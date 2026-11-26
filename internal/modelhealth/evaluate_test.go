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
			{Calls: 2, ConsecutiveSuccesses: 2, StreakStartedAt: base.Add(time.Second), LastSuccessAt: base.Add(2 * time.Second)},
		})
		if merged.ConsecutiveFaults != 0 || merged.ConsecutiveSuccesses != 2 {
			t.Fatalf("Merge() = %+v, want the fault streak ended and 2 successes", merged)
		}
	})
	t.Run("a later fault on another reporter ends the success streak", func(t *testing.T) {
		merged := Merge([]store.ModelCallHealth{
			{Calls: 3, ConsecutiveSuccesses: 3, LastSuccessAt: base},
			{Calls: 1, Faults: 1, ConsecutiveFaults: 1, StreakStartedAt: base.Add(time.Second), LastFaultReason: cpi.ReasonTimeout, LastFaultAt: base.Add(time.Second)},
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

// The reporters in these tests observed the outcomes below, in time order, and
// the merged streak must never be longer than the run that ended the sequence.
func TestMergeCountsOnlyTheOutcomesAfterTheLatestOppositeOutcome(t *testing.T) {
	base := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	at := func(seconds int) time.Time { return base.Add(time.Duration(seconds) * time.Second) }

	t.Run("successes of one reporter around a failure of another are not three in a row", func(t *testing.T) {
		// A: success t1, t2, t4; B: failure t3. Only t4 followed the failure.
		merged := Merge([]store.ModelCallHealth{
			{Calls: 3, ConsecutiveSuccesses: 3, StreakStartedAt: at(1), LastSuccessAt: at(4)},
			{Calls: 1, Faults: 1, ConsecutiveFaults: 1, StreakStartedAt: at(3), LastFaultReason: cpi.ReasonTimeout, LastFaultAt: at(3)},
		})
		if merged.ConsecutiveSuccesses != 1 || merged.ConsecutiveFaults != 0 {
			t.Fatalf("Merge() = %+v, want one success after the failure and no fault streak", merged)
		}
		if Evaluate(merged, true).Degraded != true {
			t.Fatal("a degraded target must not be healed by a single success that followed the failure")
		}
	})
	t.Run("failures of one reporter around a success of another are not three in a row", func(t *testing.T) {
		// A: failure t1, t3, t4; B: success t2. Only t3 and t4 followed the success.
		merged := Merge([]store.ModelCallHealth{
			{Calls: 3, Faults: 3, ConsecutiveFaults: 3, StreakStartedAt: at(1), LastFaultReason: cpi.ReasonTimeout, LastFaultAt: at(4)},
			{Calls: 1, ConsecutiveSuccesses: 1, StreakStartedAt: at(2), LastSuccessAt: at(2)},
		})
		if merged.ConsecutiveFaults != 1 || merged.ConsecutiveSuccesses != 0 {
			t.Fatalf("Merge() = %+v, want a fault streak that never exceeds the failures after the success", merged)
		}
		if Evaluate(merged, false).Degraded {
			t.Fatal("a target must not be degraded by failures from before another reporter's success")
		}
	})
	t.Run("a run that began after the other reporter's outcome counts in full", func(t *testing.T) {
		merged := Merge([]store.ModelCallHealth{
			{Calls: 3, Faults: 3, ConsecutiveFaults: 3, StreakStartedAt: at(5), LastFaultReason: cpi.ReasonTimeout, LastFaultAt: at(7)},
			{Calls: 1, ConsecutiveSuccesses: 1, StreakStartedAt: at(2), LastSuccessAt: at(2)},
		})
		if merged.ConsecutiveFaults != 3 || merged.ConsecutiveSuccesses != 0 {
			t.Fatalf("Merge() = %+v, want the whole fault run of 3", merged)
		}
	})
	t.Run("a run that ended before the other reporter's outcome does not count", func(t *testing.T) {
		merged := Merge([]store.ModelCallHealth{
			{Calls: 3, Faults: 3, ConsecutiveFaults: 3, StreakStartedAt: at(1), LastFaultReason: cpi.ReasonTimeout, LastFaultAt: at(3)},
			{Calls: 2, ConsecutiveSuccesses: 2, StreakStartedAt: at(4), LastSuccessAt: at(5)},
		})
		if merged.ConsecutiveFaults != 0 || merged.ConsecutiveSuccesses != 2 {
			t.Fatalf("Merge() = %+v, want only the later success run", merged)
		}
	})
	t.Run("an unknown start is a single outcome when the reporters interleave", func(t *testing.T) {
		merged := Merge([]store.ModelCallHealth{
			{Calls: 3, Faults: 3, ConsecutiveFaults: 3, LastFaultReason: cpi.ReasonTimeout, LastFaultAt: at(4)},
			{Calls: 1, ConsecutiveSuccesses: 1, LastSuccessAt: at(2)},
		})
		if merged.ConsecutiveFaults != 1 {
			t.Fatalf("Merge() = %+v, want a lower bound of one failure", merged)
		}
	})
	t.Run("a run starting exactly at the other reporter's outcome is a lower bound", func(t *testing.T) {
		merged := Merge([]store.ModelCallHealth{
			{Calls: 3, Faults: 3, ConsecutiveFaults: 3, StreakStartedAt: at(2), LastFaultReason: cpi.ReasonTimeout, LastFaultAt: at(4)},
			{Calls: 1, ConsecutiveSuccesses: 1, StreakStartedAt: at(2), LastSuccessAt: at(2)},
		})
		if merged.ConsecutiveFaults != 1 {
			t.Fatalf("Merge() = %+v, want one failure when the order of equal timestamps is unknown", merged)
		}
	})
	t.Run("a reporter's own earlier outcomes never shorten its own run", func(t *testing.T) {
		// One reporter: success at t1, then failures from the same timestamp on.
		merged := Merge([]store.ModelCallHealth{
			{Calls: 4, Faults: 3, ConsecutiveFaults: 3, StreakStartedAt: at(1), LastFaultReason: cpi.ReasonTimeout, LastFaultAt: at(4), LastSuccessAt: at(1)},
		})
		if merged.ConsecutiveFaults != 3 {
			t.Fatalf("Merge() = %+v, want the reporter's full run of 3", merged)
		}
	})
	t.Run("three reporters keep the bound of the latest opposite outcome", func(t *testing.T) {
		merged := Merge([]store.ModelCallHealth{
			{Calls: 2, Faults: 2, ConsecutiveFaults: 2, StreakStartedAt: at(6), LastFaultReason: cpi.ReasonTimeout, LastFaultAt: at(7)},
			{Calls: 2, Faults: 2, ConsecutiveFaults: 2, StreakStartedAt: at(3), LastFaultReason: cpi.ReasonTimeout, LastFaultAt: at(8)},
			{Calls: 1, ConsecutiveSuccesses: 1, StreakStartedAt: at(5), LastSuccessAt: at(5)},
		})
		if merged.ConsecutiveFaults != 3 {
			t.Fatalf("Merge() = %+v, want 2 (began after the success) + 1 (interleaved) failures", merged)
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

func TestEvaluateModelKeepsAccountsApart(t *testing.T) {
	targetsA := TargetsFor("anthropic", "https://api.example", "m", "account-a", true)
	targetsB := TargetsFor("anthropic", "https://api.example", "m", "account-b", true)
	rejected := map[string]Aggregate{
		// Account A's calls were rejected twice; account B's succeeded.
		targetsA.Credential:  {Calls: 2, Faults: 2, ConsecutiveFaults: 2, LastFaultReason: cpi.ReasonAuthenticationFailed},
		targetsB.Credential:  {Calls: 5, ConsecutiveSuccesses: 5},
		targetsA.Destination: {Calls: 5, ConsecutiveSuccesses: 5},
	}
	if verdict := EvaluateModel(rejected, targetsA, Verdict{}, false); !verdict.Degraded || verdict.Reason != cpi.ReasonAuthenticationFailed {
		t.Fatalf("account A = %+v, want it degraded for its rejected credentials", verdict)
	}
	if verdict := EvaluateModel(rejected, targetsB, Verdict{}, false); verdict.Degraded {
		t.Fatalf("account B = %+v, want it unaffected by another account's rejected credentials", verdict)
	}

	billing := map[string]Aggregate{
		targetsA.Credential: {Calls: 2, Faults: 2, ConsecutiveFaults: 2, LastFaultReason: cpi.ReasonQuotaOrBilling},
	}
	if verdict := EvaluateModel(billing, targetsA, Verdict{}, false); !verdict.Degraded || verdict.Reason != cpi.ReasonQuotaOrBilling {
		t.Fatalf("account A = %+v, want it degraded for its exhausted quota", verdict)
	}
	if verdict := EvaluateModel(billing, targetsB, Verdict{}, false); verdict.Degraded {
		t.Fatalf("account B = %+v, want it unaffected by another account's exhausted quota", verdict)
	}
}

func TestEvaluateModelSharesDestinationFailuresWithEveryAccount(t *testing.T) {
	targetsA := TargetsFor("anthropic", "https://api.example", "m", "account-a", true)
	targetsB := TargetsFor("anthropic", "https://api.example", "m", "account-b", true)
	down := map[string]Aggregate{targetsA.Destination: {Calls: 3, Faults: 3, ConsecutiveFaults: 3, LastFaultReason: cpi.ReasonTimeout}}
	for name, targets := range map[string]Targets{"a": targetsA, "b": targetsB} {
		if verdict := EvaluateModel(down, targets, Verdict{}, false); !verdict.Degraded || verdict.Reason != cpi.ReasonTimeout {
			t.Errorf("account %s = %+v, want it degraded by the destination's timeouts", name, verdict)
		}
	}
}

func TestEvaluateModelWithoutAKnownCredentialIgnoresCredentialFailures(t *testing.T) {
	unknown := TargetsFor("anthropic", "https://api.example", "m", "", false)
	known := TargetsFor("anthropic", "https://api.example", "m", "", true)
	aggregates := map[string]Aggregate{
		known.Credential: {Calls: 3, Faults: 3, ConsecutiveFaults: 3, LastFaultReason: cpi.ReasonAuthenticationFailed},
	}
	if verdict := EvaluateModel(aggregates, unknown, Verdict{}, false); verdict.Degraded {
		t.Fatalf("verdict = %+v, want a Model whose credential is unknown never blamed for an account's failures", verdict)
	}
	if verdict := EvaluateModel(aggregates, known, Verdict{}, false); !verdict.Degraded {
		t.Fatal("a Model that presents no credential shares the failures of calls that presented none")
	}
}

func TestEvaluateModelOnlyKeepsADegradationItsOwnEvidenceCaused(t *testing.T) {
	targets := TargetsFor("anthropic", "https://api.example", "m", "account-a", true)
	// A rejected credential degraded the Model. The destination has a failure that
	// is nowhere near enough on its own, and too few successes since.
	aggregates := map[string]Aggregate{
		targets.Destination: {Calls: 4, Faults: 1, ConsecutiveSuccesses: 1, LastFaultReason: cpi.ReasonTimeout},
		targets.Credential:  {Calls: 2, Faults: 2, ConsecutiveFaults: 2, LastFaultReason: cpi.ReasonAuthenticationFailed},
	}
	previous := Verdict{Degraded: true, Reason: cpi.ReasonAuthenticationFailed}
	if verdict := EvaluateModel(aggregates, targets, previous, true); verdict.Reason != cpi.ReasonAuthenticationFailed {
		t.Fatalf("verdict = %+v, want the credential's degradation kept with its own reason", verdict)
	}

	// The credential recovers; the destination's earlier failure must not keep the
	// Model degraded just because the Model was.
	recovered := map[string]Aggregate{
		targets.Destination: aggregates[targets.Destination],
		targets.Credential:  {Calls: 5, Faults: 2, ConsecutiveSuccesses: 3, LastFaultReason: cpi.ReasonAuthenticationFailed},
	}
	if verdict := EvaluateModel(recovered, targets, previous, true); verdict.Degraded {
		t.Fatalf("verdict = %+v, want a recovered credential to heal the Model", verdict)
	}

	// The converse: a destination degradation is not kept alive by the credential.
	destinationDown := Verdict{Degraded: true, Reason: cpi.ReasonTimeout}
	partial := map[string]Aggregate{
		targets.Destination: {Calls: 10, Faults: 3, ConsecutiveSuccesses: 3, LastFaultReason: cpi.ReasonTimeout},
		targets.Credential:  {Calls: 4, Faults: 1, ConsecutiveSuccesses: 1, LastFaultReason: cpi.ReasonAuthenticationFailed},
	}
	if verdict := EvaluateModel(partial, targets, destinationDown, true); verdict.Degraded {
		t.Fatalf("verdict = %+v, want the destination's recovery to heal the Model despite a stray credential failure", verdict)
	}
}

func TestEvaluateModelPrefersTheDestinationWhenBothAreDegraded(t *testing.T) {
	targets := TargetsFor("anthropic", "https://api.example", "m", "account-a", true)
	both := map[string]Aggregate{
		targets.Destination: {Calls: 3, Faults: 3, ConsecutiveFaults: 3, LastFaultReason: cpi.ReasonEndpointError},
		targets.Credential:  {Calls: 2, Faults: 2, ConsecutiveFaults: 2, LastFaultReason: cpi.ReasonAuthenticationFailed},
	}
	if verdict := EvaluateModel(both, targets, Verdict{}, false); verdict.Reason != cpi.ReasonEndpointError {
		t.Fatalf("verdict = %+v, want the destination-wide reason", verdict)
	}
}
