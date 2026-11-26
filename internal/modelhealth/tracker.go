// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package modelhealth

import (
	"sort"
	"sync"
	"time"

	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/store"
)

const (
	// Window is how long a call outcome counts toward a target's health.
	Window = 5 * time.Minute

	maxEventsPerTarget = 32
	// maxTrackedTargets bounds the destinations and credentials tracked, which is
	// what one reporter may publish.
	maxTrackedTargets = store.MaxModelCallHealthTargets
)

type event struct {
	at     time.Time
	fault  bool
	reason string
}

// Tracker keeps a bounded, short window of recent call outcomes per target. It
// implements cpi.CallRecorder and is safe for concurrent use. Only outcomes that
// say something about the provider are kept: successes and provider failures.
// Invalid requests, cancellations and local errors are ignored.
//
// A call is attributed to what its outcome is about. A success counts for the
// destination and for the credential that made it. A failure of the credential,
// rejected credentials or exhausted quota or billing, counts only for that
// credential, so one account's failures cannot degrade the destination for
// another. Every other failure counts only for the destination.
type Tracker struct {
	mu      sync.Mutex
	targets map[string][]event
	now     func() time.Time
}

var _ cpi.CallRecorder = (*Tracker)(nil)

// NewTracker creates an empty tracker.
func NewTracker() *Tracker {
	return &Tracker{targets: make(map[string][]event), now: time.Now}
}

// RecordCall records one finished call.
func (t *Tracker) RecordCall(call cpi.CallOutcome) {
	fault := !call.Succeeded()
	if fault && !call.ProviderFault {
		return
	}
	entry := event{at: t.now().UTC(), fault: fault}
	if fault {
		entry.reason = cpi.NormalizeReason(call.Reason)
	}
	destination := TargetKey(call.Provider, call.Endpoint, call.Model)
	credential := CredentialKey(destination, call.Credential)

	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case !fault:
		t.appendLocked(destination, entry)
		t.appendLocked(credential, entry)
	case credentialScoped(entry.reason):
		t.appendLocked(credential, entry)
	default:
		t.appendLocked(destination, entry)
	}
}

func (t *Tracker) appendLocked(key string, entry event) {
	events, known := t.targets[key]
	if !known && len(t.targets) >= maxTrackedTargets {
		t.evictStalestLocked()
	}
	events = pruneEvents(events, entry.at)
	events = append(events, entry)
	if len(events) > maxEventsPerTarget {
		events = append([]event(nil), events[len(events)-maxEventsPerTarget:]...)
	}
	t.targets[key] = events
}

// Snapshot summarizes the targets that have outcomes inside the window, ordered
// by target. Targets whose outcomes have all aged out are forgotten.
func (t *Tracker) Snapshot() []store.ModelCallHealth {
	now := t.now().UTC()
	t.mu.Lock()
	defer t.mu.Unlock()

	snapshots := make([]store.ModelCallHealth, 0, len(t.targets))
	for key, events := range t.targets {
		events = pruneEvents(events, now)
		if len(events) == 0 {
			delete(t.targets, key)
			continue
		}
		t.targets[key] = events
		snapshots = append(snapshots, summarize(key, events))
	}
	sort.Slice(snapshots, func(i, j int) bool { return snapshots[i].Target < snapshots[j].Target })
	return snapshots
}

func (t *Tracker) evictStalestLocked() {
	var stalest string
	var stalestAt time.Time
	for key, events := range t.targets {
		last := events[len(events)-1].at
		if stalest == "" || last.Before(stalestAt) {
			stalest, stalestAt = key, last
		}
	}
	delete(t.targets, stalest)
}

func pruneEvents(events []event, now time.Time) []event {
	cutoff := now.Add(-Window)
	first := 0
	for first < len(events) && events[first].at.Before(cutoff) {
		first++
	}
	return events[first:]
}

func summarize(target string, events []event) store.ModelCallHealth {
	summary := store.ModelCallHealth{Target: target, Calls: int32(len(events))}
	for _, e := range events {
		if e.fault {
			summary.Faults++
			summary.LastFaultAt, summary.LastFaultReason = e.at, e.reason
		} else {
			summary.LastSuccessAt = e.at
		}
	}
	last := events[len(events)-1].fault
	run := 0
	for i := len(events) - 1; i >= 0 && events[i].fault == last; i-- {
		run++
		summary.StreakStartedAt = events[i].at
	}
	if last {
		summary.ConsecutiveFaults = int32(run)
	} else {
		summary.ConsecutiveSuccesses = int32(run)
	}
	return summary
}
