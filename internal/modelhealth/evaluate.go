// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package modelhealth

import (
	"time"

	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/store"
)

// Policy. These are fixed defaults rather than API, so they can be tuned without
// a schema change once real traffic shows what they should be.
const (
	// consecutiveFaultsToDegrade is the run of provider failures that degrades a
	// target. Failures that cannot recover on their own, such as rejected
	// credentials or exhausted quota, degrade it sooner.
	consecutiveFaultsToDegrade = 3
	strictFaultsToDegrade      = 2
	// minCallsForRate and faultRateToDegrade degrade a target whose recent calls
	// fail at least half of the time, while the latest one failed.
	minCallsForRate    = 5
	faultRateToDegrade = 0.5
	// successesToRecover is the run of successes that ends a degradation before
	// the failures age out of the window.
	successesToRecover = 3

	// MessagePrefix starts the condition message of a passively degraded Model,
	// which is how the Model controller and its watcher recognize one.
	MessagePrefix = "passive health: "
)

// Aggregate combines the summaries of all reporters for one target.
type Aggregate struct {
	Calls                int
	Faults               int
	ConsecutiveFaults    int
	ConsecutiveSuccesses int
	LastFaultReason      string
}

// Merge combines the reporters' summaries of one target. A reporter's streak is
// only as long as the part that no other reporter's opposite outcome interrupted:
// it counts in full when it began after the latest opposite outcome seen by the
// other reporters, not at all when it ended before it, and as a single outcome,
// which is all that is certain, when the two interleave or its start is unknown.
// The merged streaks therefore never exceed the true ones.
func Merge(records []store.ModelCallHealth) Aggregate {
	var merged Aggregate
	var lastFaultAt time.Time
	for _, record := range records {
		merged.Calls += int(record.Calls)
		merged.Faults += int(record.Faults)
		if record.LastFaultAt.After(lastFaultAt) {
			lastFaultAt, merged.LastFaultReason = record.LastFaultAt, record.LastFaultReason
		}
	}
	for i, record := range records {
		if record.ConsecutiveFaults > 0 {
			merged.ConsecutiveFaults += uninterruptedRun(record.ConsecutiveFaults, record.StreakStartedAt, record.LastFaultAt,
				latestOtherOutcome(records, i, func(r store.ModelCallHealth) time.Time { return r.LastSuccessAt }))
		}
		if record.ConsecutiveSuccesses > 0 {
			merged.ConsecutiveSuccesses += uninterruptedRun(record.ConsecutiveSuccesses, record.StreakStartedAt, record.LastSuccessAt,
				latestOtherOutcome(records, i, func(r store.ModelCallHealth) time.Time { return r.LastFaultAt }))
		}
	}
	return merged
}

// latestOtherOutcome is the latest time, among the records other than records[skip],
// that outcome reports. Its own record is skipped because its outcomes of the
// opposite kind always precede its current run.
func latestOtherOutcome(records []store.ModelCallHealth, skip int, outcome func(store.ModelCallHealth) time.Time) time.Time {
	var latest time.Time
	for i, record := range records {
		if i != skip && outcome(record).After(latest) {
			latest = outcome(record)
		}
	}
	return latest
}

// uninterruptedRun is how many of a run of length, which began at started and
// ended at ended, followed the opposite outcome at interruptedAt.
func uninterruptedRun(length int32, started, ended, interruptedAt time.Time) int {
	switch {
	case interruptedAt.IsZero():
		return int(length)
	case !ended.After(interruptedAt):
		return 0
	case started.After(interruptedAt):
		return int(length)
	default:
		return 1
	}
}

// MergeByTarget groups records by target and merges each group.
func MergeByTarget(records []store.ModelCallHealth) map[string]Aggregate {
	grouped := make(map[string][]store.ModelCallHealth)
	for _, record := range records {
		grouped[record.Target] = append(grouped[record.Target], record)
	}
	merged := make(map[string]Aggregate, len(grouped))
	for target, group := range grouped {
		merged[target] = Merge(group)
	}
	return merged
}

// Verdict is the passive health of a target.
type Verdict struct {
	// Degraded reports that recent real calls to the target are failing.
	Degraded bool
	// Reason is the failure reason of the latest provider failure.
	Reason string
	// Message describes the degradation. It deliberately contains no counts or
	// times, so it stays stable while the failures continue.
	Message string
}

// Evaluate applies the policy to an aggregate. wasDegraded makes a degradation
// sticky until the target recovers, which prevents a target from flapping on a
// single success.
func Evaluate(aggregate Aggregate, wasDegraded bool) Verdict {
	if aggregate.Calls == 0 || aggregate.Faults == 0 {
		return Verdict{}
	}
	reason := cpi.NormalizeReason(aggregate.LastFaultReason)
	threshold := consecutiveFaultsToDegrade
	if reason == cpi.ReasonAuthenticationFailed || reason == cpi.ReasonQuotaOrBilling {
		threshold = strictFaultsToDegrade
	}
	degraded := aggregate.ConsecutiveFaults >= threshold ||
		(aggregate.Calls >= minCallsForRate && aggregate.ConsecutiveFaults > 0 &&
			float64(aggregate.Faults) >= faultRateToDegrade*float64(aggregate.Calls))
	if !degraded && wasDegraded {
		degraded = aggregate.ConsecutiveSuccesses < successesToRecover
	}
	if !degraded {
		return Verdict{}
	}
	return Verdict{Degraded: true, Reason: reason, Message: MessagePrefix + "recent model calls are failing with " + reason}
}

// EvaluateModel combines the destination's evidence, which concerns everyone who
// calls it, with the evidence about the Model's own credential. previous is the
// verdict the Model currently carries and wasDegraded whether it is one: each
// part only keeps a degradation it caused, which its reason tells.
func EvaluateModel(aggregates map[string]Aggregate, targets Targets, previous Verdict, wasDegraded bool) Verdict {
	caused := wasDegraded && credentialScoped(previous.Reason)
	if verdict := Evaluate(aggregates[targets.Destination], wasDegraded && !caused); verdict.Degraded {
		return verdict
	}
	if targets.Credential == "" {
		return Verdict{}
	}
	return Evaluate(aggregates[targets.Credential], caused)
}
