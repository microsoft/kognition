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

// destinationKey is the destination of the calls the helpers above make to model.
func destinationKey(model string) string {
	return TargetKey("anthropic", "https://api.example", model)
}

// stream returns the tracker's summary of the stream with key.
func stream(t *testing.T, tracker *Tracker, key string) store.ModelCallHealth {
	t.Helper()
	for _, snapshot := range tracker.Snapshot() {
		if snapshot.Target == key {
			return snapshot
		}
	}
	t.Fatalf("the tracker has no stream %s, it has %+v", key, tracker.Snapshot())
	return store.ModelCallHealth{}
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

	// Each model has a destination stream, and its successes also feed the stream
	// of the credential that made them.
	snapshots := tracker.Snapshot()
	if len(snapshots) != 4 {
		t.Fatalf("snapshots = %+v, want a destination and a credential stream for each of two models", snapshots)
	}
	claude := stream(t, tracker, destinationKey("claude"))
	if claude.Calls != 3 || claude.Faults != 2 || claude.ConsecutiveFaults != 2 || claude.ConsecutiveSuccesses != 0 || claude.LastFaultReason != cpi.ReasonTimeout {
		t.Errorf("claude = %+v, want 3 calls, 2 faults, a streak of 2 and the last reason Timeout", claude)
	}
	if !claude.LastFaultAt.After(claude.LastSuccessAt) || claude.LastSuccessAt.IsZero() {
		t.Errorf("claude times = fault %v, success %v, want the fault after the success", claude.LastFaultAt, claude.LastSuccessAt)
	}
	haiku := stream(t, tracker, destinationKey("haiku"))
	if haiku.Calls != 1 || haiku.Faults != 0 || haiku.ConsecutiveSuccesses != 1 || !haiku.LastFaultAt.IsZero() || haiku.LastFaultReason != "" {
		t.Errorf("haiku = %+v, want one success and no fault", haiku)
	}
	credential := stream(t, tracker, CredentialKey(destinationKey("claude"), ""))
	if credential.Calls != 1 || credential.Faults != 0 {
		t.Errorf("claude credential = %+v, want only its success: provider failures are not about the credential", credential)
	}
	for i := 1; i < len(snapshots); i++ {
		if snapshots[i-1].Target > snapshots[i].Target {
			t.Fatal("Snapshot() must be ordered by target")
		}
	}
}
func TestTrackerStreaksResetOnTheOppositeOutcome(t *testing.T) {
	clock := newFakeClock()
	tracker := newTestTracker(clock)
	for _, outcome := range []cpi.CallOutcome{fault("m", cpi.ReasonTimeout), fault("m", cpi.ReasonTimeout), success("m"), success("m"), success("m")} {
		tracker.RecordCall(outcome)
		clock.Advance(time.Millisecond)
	}
	got := stream(t, tracker, destinationKey("m"))
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
	if got := stream(t, tracker, destinationKey("m")).LastFaultReason; got != cpi.ReasonOther {
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
	if got := stream(t, tracker, destinationKey("busy")); got.Calls != maxEventsPerTarget || got.ConsecutiveFaults != maxEventsPerTarget {
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
	got := tracker.Snapshot()
	if len(got) != 2 {
		t.Fatalf("snapshot = %+v, want the destination and the credential stream", got)
	}
	for _, snapshot := range got {
		if snapshot.Calls != maxEventsPerTarget {
			t.Fatalf("stream = %+v, want the newest %d events", snapshot, maxEventsPerTarget)
		}
	}
}

func TestTrackerMergesRetainedSamplesRatherThanReplicaTrafficVolumes(t *testing.T) {
	clock := newFakeClock()
	busy, failing := newTestTracker(clock), newTestTracker(clock)
	for range 320 {
		busy.RecordCall(success("shared"))
		clock.Advance(time.Millisecond)
	}
	for range 32 {
		failing.RecordCall(fault("shared", cpi.ReasonTimeout))
		clock.Advance(time.Millisecond)
	}
	aggregate := Merge([]store.ModelCallHealth{
		stream(t, busy, destinationKey("shared")),
		stream(t, failing, destinationKey("shared")),
	})
	if aggregate.Calls != 64 || aggregate.Faults != 32 || aggregate.ConsecutiveFaults != 32 {
		t.Fatalf("aggregate = %+v, want 32 retained successes and 32 subsequent faults", aggregate)
	}
	if verdict := Evaluate(aggregate, false); !verdict.Degraded {
		t.Fatalf("verdict = %+v, want the retained sample to degrade despite the lower population rate", verdict)
	}
	clock.Advance(Window + time.Millisecond)
	if len(busy.Snapshot()) != 0 || len(failing.Snapshot()) != 0 {
		t.Fatal("retained outcomes must still expire by age")
	}
}

func TestTrackerReportsWhenTheCurrentRunBegan(t *testing.T) {
	clock := newFakeClock()
	tracker := newTestTracker(clock)
	start := clock.Now()
	tracker.RecordCall(success("m"))
	clock.Advance(time.Second)
	tracker.RecordCall(fault("m", cpi.ReasonTimeout))
	clock.Advance(time.Second)
	tracker.RecordCall(fault("m", cpi.ReasonTimeout))
	clock.Advance(time.Second)
	tracker.RecordCall(fault("m", cpi.ReasonTimeout))

	got := stream(t, tracker, destinationKey("m"))
	if got.ConsecutiveFaults != 3 || !got.StreakStartedAt.Equal(start.Add(time.Second)) {
		t.Fatalf("snapshot = %+v, want a run of 3 faults that began one second in", got)
	}

	clock.Advance(time.Second)
	tracker.RecordCall(success("m"))
	got = stream(t, tracker, destinationKey("m"))
	if got.ConsecutiveSuccesses != 1 || got.ConsecutiveFaults != 0 || !got.StreakStartedAt.Equal(start.Add(4*time.Second)) {
		t.Fatalf("snapshot = %+v, want a new run of 1 success that began with it", got)
	}
}

func TestTrackerRunStartFollowsTheWindow(t *testing.T) {
	clock := newFakeClock()
	tracker := newTestTracker(clock)
	tracker.RecordCall(fault("m", cpi.ReasonTimeout))
	clock.Advance(Window - time.Second)
	tracker.RecordCall(fault("m", cpi.ReasonTimeout))
	secondAt := clock.Now()
	clock.Advance(2 * time.Second)

	got := stream(t, tracker, destinationKey("m"))
	if got.ConsecutiveFaults != 1 || !got.StreakStartedAt.Equal(secondAt) {
		t.Fatalf("snapshot = %+v, want the run to start at the oldest outcome still in the window", got)
	}
}

// withCredential is a call made with the credential whose fingerprint is name.
func withCredential(outcome cpi.CallOutcome, name string) cpi.CallOutcome {
	outcome.Credential = name
	return outcome
}

func TestTrackerAttributesCredentialFailuresToTheCredentialOnly(t *testing.T) {
	for _, reason := range []string{cpi.ReasonAuthenticationFailed, cpi.ReasonQuotaOrBilling} {
		t.Run(reason, func(t *testing.T) {
			tracker := newTestTracker(newFakeClock())
			tracker.RecordCall(withCredential(fault("m", reason), "account-a"))
			tracker.RecordCall(withCredential(fault("m", reason), "account-a"))

			credential := stream(t, tracker, CredentialKey(destinationKey("m"), "account-a"))
			if credential.Faults != 2 || credential.ConsecutiveFaults != 2 || credential.LastFaultReason != reason {
				t.Fatalf("credential stream = %+v, want both failures", credential)
			}
			for _, snapshot := range tracker.Snapshot() {
				if snapshot.Target == destinationKey("m") {
					t.Fatalf("destination stream = %+v, want no evidence from one account's %s", snapshot, reason)
				}
			}
		})
	}
}

func TestTrackerAttributesDestinationFailuresToTheDestinationOnly(t *testing.T) {
	tracker := newTestTracker(newFakeClock())
	for _, reason := range []string{cpi.ReasonTimeout, cpi.ReasonUnreachable, cpi.ReasonEndpointError, cpi.ReasonRateLimited} {
		tracker.RecordCall(withCredential(fault("m", reason), "account-a"))
	}
	if destination := stream(t, tracker, destinationKey("m")); destination.Faults != 4 || destination.ConsecutiveFaults != 4 {
		t.Fatalf("destination stream = %+v, want all four failures", destination)
	}
	for _, snapshot := range tracker.Snapshot() {
		if snapshot.Target == CredentialKey(destinationKey("m"), "account-a") {
			t.Fatalf("credential stream = %+v, want it untouched by failures of the destination", snapshot)
		}
	}
}

func TestTrackerKeepsAccountsApart(t *testing.T) {
	tracker := newTestTracker(newFakeClock())
	tracker.RecordCall(withCredential(fault("m", cpi.ReasonAuthenticationFailed), "account-a"))
	tracker.RecordCall(withCredential(success("m"), "account-b"))
	tracker.RecordCall(withCredential(fault("m", cpi.ReasonAuthenticationFailed), "account-a"))
	tracker.RecordCall(withCredential(success("m"), "account-b"))

	a := stream(t, tracker, CredentialKey(destinationKey("m"), "account-a"))
	b := stream(t, tracker, CredentialKey(destinationKey("m"), "account-b"))
	if a.ConsecutiveFaults != 2 || a.Calls != 2 || b.ConsecutiveSuccesses != 2 || b.Faults != 0 {
		t.Fatalf("account a = %+v, account b = %+v, want a's two failures and b's two successes kept apart", a, b)
	}
	if destination := stream(t, tracker, destinationKey("m")); destination.Faults != 0 || destination.Calls != 2 {
		t.Fatalf("destination = %+v, want only b's two successes", destination)
	}
}

func TestCredentialKey(t *testing.T) {
	destination := destinationKey("m")
	base := CredentialKey(destination, "account-a")
	if len(base) != 64 || strings.Trim(base, "0123456789abcdef") != "" || strings.Contains(base, "account") {
		t.Fatalf("CredentialKey() = %q, want 64 lowercase hex characters that reveal nothing", base)
	}
	if CredentialKey(destination, "account-a") != base {
		t.Error("CredentialKey() must be stable")
	}
	for name, other := range map[string]string{
		"another account":     CredentialKey(destination, "account-b"),
		"no credential":       CredentialKey(destination, ""),
		"another destination": CredentialKey(destinationKey("n"), "account-a"),
		"the destination key": destination,
	} {
		if other == base {
			t.Errorf("CredentialKey() collides with %s", name)
		}
	}
	if CredentialKey("ab", "c") == CredentialKey("a", "bc") {
		t.Error("field boundaries must be part of the key")
	}
}

func TestCredentialScopedReasons(t *testing.T) {
	for reason, want := range map[string]bool{
		cpi.ReasonAuthenticationFailed: true,
		cpi.ReasonQuotaOrBilling:       true,
		cpi.ReasonRateLimited:          false,
		cpi.ReasonTimeout:              false,
		cpi.ReasonUnreachable:          false,
		cpi.ReasonEndpointError:        false,
		cpi.ReasonInvalidRequest:       false,
		"free text":                    false,
		"":                             false,
	} {
		if got := credentialScoped(reason); got != want {
			t.Errorf("credentialScoped(%q) = %t, want %t", reason, got, want)
		}
	}
}

func TestTargetsFor(t *testing.T) {
	known := TargetsFor("anthropic", "https://api.example", "m", "account-a", true)
	if known.Destination != destinationKey("m") || known.Credential != CredentialKey(destinationKey("m"), "account-a") {
		t.Fatalf("TargetsFor() = %+v, want the destination and the account's key", known)
	}
	noCredential := TargetsFor("anthropic", "https://api.example", "m", "", true)
	if noCredential.Credential != CredentialKey(destinationKey("m"), "") || noCredential.Credential == "" {
		t.Fatalf("TargetsFor() = %+v, want a known empty credential to have its own key", noCredential)
	}
	unknown := TargetsFor("anthropic", "https://api.example", "m", "ignored", false)
	if unknown.Credential != "" || unknown.Destination != destinationKey("m") {
		t.Fatalf("TargetsFor() = %+v, want no credential key when the credential is not known", unknown)
	}
	if !known.Contains(known.Destination) || !known.Contains(known.Credential) || known.Contains("other") {
		t.Error("Contains() must accept exactly the destination and credential keys")
	}
	if unknown.Contains("") {
		t.Error("an unknown credential must not match the empty key")
	}
}
