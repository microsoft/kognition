// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package modelhealth

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/store"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestTracker(clock *fakeClock) *Tracker {
	tracker := NewTracker()
	tracker.now = clock.Now
	return tracker
}

func success(model string) cpi.CallOutcome {
	return cpi.CallOutcome{Provider: "anthropic", Operation: cpi.OperationComplete, Model: model, Endpoint: "https://api.example"}
}

func fault(model, reason string) cpi.CallOutcome {
	outcome := success(model)
	outcome.Reason, outcome.ProviderFault = reason, true
	return outcome
}

func TestTargetKey(t *testing.T) {
	base := TargetKey("anthropic", "https://api.example", "claude")
	if len(base) != 64 || strings.Trim(base, "0123456789abcdef") != "" {
		t.Fatalf("TargetKey() = %q, want 64 lowercase hex characters", base)
	}
	if strings.Contains(base, "example") {
		t.Errorf("TargetKey() = %q leaks the endpoint", base)
	}

	same := []struct{ provider, endpoint, model string }{
		{"anthropic", "https://api.example/", "claude"},
		{" anthropic ", "  https://api.example//  ", " claude "},
	}
	for _, test := range same {
		if got := TargetKey(test.provider, test.endpoint, test.model); got != base {
			t.Errorf("TargetKey(%q, %q, %q) differs from the normalized key", test.provider, test.endpoint, test.model)
		}
	}
	if TargetKey("openai", "https://x", "m") != TargetKey("openai-compatible", "https://x", "m") ||
		TargetKey("azure-openai", "https://x", "m") != TargetKey("openai-compatible", "https://x", "m") {
		t.Error("provider aliases must share a key with their canonical driver")
	}

	different := []struct{ provider, endpoint, model string }{
		{"ollama", "https://api.example", "claude"},
		{"anthropic", "https://other.example", "claude"},
		{"anthropic", "https://api.example", "haiku"},
		{"anthropic", "", "claude"},
	}
	for _, test := range different {
		if TargetKey(test.provider, test.endpoint, test.model) == base {
			t.Errorf("TargetKey(%q, %q, %q) collides with the base key", test.provider, test.endpoint, test.model)
		}
	}
	if TargetKey("a", "bc", "d") == TargetKey("a", "b", "cd") {
		t.Error("field boundaries must be part of the key")
	}
}

func TestTrackerSummarizesOutcomesPerTarget(t *testing.T) {
	clock := newFakeClock()
	tracker := newTestTracker(clock)

	tracker.RecordCall(success("claude"))
	clock.Advance(time.Second)
	tracker.RecordCall(fault("claude", cpi.ReasonRateLimited))
	clock.Advance(time.Second)
	tracker.RecordCall(fault("claude", cpi.ReasonTimeout))
	tracker.RecordCall(success("haiku"))

	snapshots := tracker.Snapshot()
	if len(snapshots) != 2 {
		t.Fatalf("snapshots = %+v, want two targets", snapshots)
	}
	byTarget := map[string]store.ModelCallHealth{}
	for _, snapshot := range snapshots {
		byTarget[snapshot.Target] = snapshot
	}
	claude := byTarget[TargetKey("anthropic", "https://api.example", "claude")]
	if claude.Calls != 3 || claude.Faults != 2 || claude.ConsecutiveFaults != 2 || claude.ConsecutiveSuccesses != 0 || claude.LastFaultReason != cpi.ReasonTimeout {
		t.Errorf("claude = %+v, want 3 calls, 2 faults, a streak of 2 and the last reason Timeout", claude)
	}
	if !claude.LastFaultAt.After(claude.LastSuccessAt) || claude.LastSuccessAt.IsZero() {
		t.Errorf("claude times = fault %v, success %v, want the fault after the success", claude.LastFaultAt, claude.LastSuccessAt)
	}
	haiku := byTarget[TargetKey("anthropic", "https://api.example", "haiku")]
	if haiku.Calls != 1 || haiku.Faults != 0 || haiku.ConsecutiveSuccesses != 1 || !haiku.LastFaultAt.IsZero() || haiku.LastFaultReason != "" {
		t.Errorf("haiku = %+v, want one success and no fault", haiku)
	}
	if snapshots[0].Target > snapshots[1].Target {
		t.Error("Snapshot() must be ordered by target")
	}
}

func TestTrackerStreaksResetOnTheOppositeOutcome(t *testing.T) {
	clock := newFakeClock()
	tracker := newTestTracker(clock)
	for _, outcome := range []cpi.CallOutcome{fault("m", cpi.ReasonTimeout), fault("m", cpi.ReasonTimeout), success("m"), success("m"), success("m")} {
		tracker.RecordCall(outcome)
		clock.Advance(time.Millisecond)
	}
	got := tracker.Snapshot()[0]
	if got.ConsecutiveFaults != 0 || got.ConsecutiveSuccesses != 3 || got.Faults != 2 || got.Calls != 5 {
		t.Fatalf("snapshot = %+v, want a streak of 3 successes after 2 faults", got)
	}
}

func TestTrackerIgnoresFailuresThatAreNotTheProvidersFault(t *testing.T) {
	tracker := newTestTracker(newFakeClock())
	notProvider := fault("m", cpi.ReasonInvalidRequest)
	notProvider.ProviderFault = false
	tracker.RecordCall(notProvider)
	canceled := fault("m", cpi.ReasonCanceled)
	canceled.ProviderFault = false
	tracker.RecordCall(canceled)
	if snapshots := tracker.Snapshot(); len(snapshots) != 0 {
		t.Fatalf("snapshots = %+v, want request failures ignored", snapshots)
	}
}

func TestTrackerBoundsReasonsToTheKnownSet(t *testing.T) {
	tracker := newTestTracker(newFakeClock())
	tracker.RecordCall(fault("m", "free text from a provider"))
	if got := tracker.Snapshot()[0].LastFaultReason; got != cpi.ReasonOther {
		t.Fatalf("LastFaultReason = %q, want %q", got, cpi.ReasonOther)
	}
}

func TestTrackerForgetsOutcomesOutsideTheWindow(t *testing.T) {
	clock := newFakeClock()
	tracker := newTestTracker(clock)
	tracker.RecordCall(fault("old", cpi.ReasonTimeout))
	clock.Advance(Window - time.Second)
	tracker.RecordCall(fault("recent", cpi.ReasonTimeout))
	tracker.RecordCall(success("old"))

	clock.Advance(2 * time.Second)
	snapshots := tracker.Snapshot()
	byTarget := map[string]store.ModelCallHealth{}
	for _, snapshot := range snapshots {
		byTarget[snapshot.Target] = snapshot
	}
	if old := byTarget[TargetKey("anthropic", "https://api.example", "old")]; old.Calls != 1 || old.Faults != 0 {
		t.Errorf("old = %+v, want only the success inside the window", old)
	}

	clock.Advance(Window)
	if snapshots := tracker.Snapshot(); len(snapshots) != 0 {
		t.Fatalf("snapshots = %+v, want everything aged out", snapshots)
	}
	if len(tracker.targets) != 0 {
		t.Errorf("tracker still holds %d aged-out targets", len(tracker.targets))
	}
}

func TestTrackerBoundsEventsAndTargets(t *testing.T) {
	clock := newFakeClock()
	tracker := newTestTracker(clock)
	for i := 0; i < 3*maxEventsPerTarget; i++ {
		tracker.RecordCall(fault("busy", cpi.ReasonTimeout))
		clock.Advance(time.Millisecond)
	}
	if got := tracker.Snapshot()[0]; got.Calls != maxEventsPerTarget || got.ConsecutiveFaults != maxEventsPerTarget {
		t.Fatalf("busy target = %+v, want the newest %d events kept", got, maxEventsPerTarget)
	}

	for i := 0; i < maxTrackedTargets+10; i++ {
		tracker.RecordCall(success(fmt.Sprintf("model-%d", i)))
		clock.Advance(time.Millisecond)
	}
	if len(tracker.targets) != maxTrackedTargets {
		t.Fatalf("tracker holds %d targets, want at most %d", len(tracker.targets), maxTrackedTargets)
	}
	if _, kept := tracker.targets[TargetKey("anthropic", "https://api.example", "busy")]; kept {
		t.Error("the stalest target must be evicted first")
	}
	if _, kept := tracker.targets[TargetKey("anthropic", "https://api.example", fmt.Sprintf("model-%d", maxTrackedTargets+9))]; !kept {
		t.Error("the newest target must be kept")
	}
}

func TestTrackerIsSafeForConcurrentUse(t *testing.T) {
	tracker := NewTracker()
	var wait sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for i := 0; i < 200; i++ {
				if i%3 == 0 {
					tracker.RecordCall(fault("m", cpi.ReasonRateLimited))
				} else {
					tracker.RecordCall(success("m"))
				}
				if i%50 == 0 {
					_ = tracker.Snapshot()
				}
			}
		}()
	}
	wait.Wait()
	if got := tracker.Snapshot(); len(got) != 1 || got[0].Calls != maxEventsPerTarget {
		t.Fatalf("snapshot = %+v, want one target holding the newest %d events", got, maxEventsPerTarget)
	}
}
