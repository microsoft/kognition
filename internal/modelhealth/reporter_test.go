// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package modelhealth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/store"
)

type fakeSink struct {
	mu      sync.Mutex
	calls   [][]store.ModelCallHealth
	reports []string
	err     error
}

func (s *fakeSink) ReplaceModelCallHealth(_ context.Context, reporter string, records []store.ModelCallHealth) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.calls = append(s.calls, records)
	s.reports = append(s.reports, reporter)
	return nil
}

func (s *fakeSink) ListModelCallHealth(context.Context, time.Duration) ([]store.ModelCallHealth, error) {
	return nil, nil
}

func (s *fakeSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *fakeSink) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func newTestReporter(clock *fakeClock) (*Reporter, *Tracker, *fakeSink) {
	tracker := newTestTracker(clock)
	sink := &fakeSink{}
	reporter := NewReporter(tracker, sink, "reporter-1", zap.NewNop())
	reporter.now = clock.Now
	return reporter, tracker, sink
}

func TestReporterPublishesChangesAndSkipsUnchangedSummaries(t *testing.T) {
	clock := newFakeClock()
	reporter, tracker, sink := newTestReporter(clock)
	ctx := context.Background()

	if err := reporter.Flush(ctx); err != nil || sink.count() != 0 {
		t.Fatalf("an idle reporter must not touch the store: err %v, writes %d", err, sink.count())
	}

	tracker.RecordCall(fault("m", cpi.ReasonRateLimited))
	if err := reporter.Flush(ctx); err != nil || sink.count() != 1 {
		t.Fatalf("first publish: err %v, writes %d, want 1", err, sink.count())
	}
	if sink.reports[0] != "reporter-1" || len(sink.calls[0]) != 1 || sink.calls[0][0].Faults != 1 {
		t.Fatalf("published %q %+v, want reporter-1's fault summary", sink.reports[0], sink.calls[0])
	}

	clock.Advance(DefaultReportInterval)
	if err := reporter.Flush(ctx); err != nil || sink.count() != 1 {
		t.Fatalf("an unchanged summary within the heartbeat must not be republished: err %v, writes %d", err, sink.count())
	}

	tracker.RecordCall(fault("m", cpi.ReasonRateLimited))
	if err := reporter.Flush(ctx); err != nil || sink.count() != 2 {
		t.Fatalf("a changed summary must be published: err %v, writes %d", err, sink.count())
	}
}

func TestReporterHeartbeatKeepsUnchangedSummariesFresh(t *testing.T) {
	clock := newFakeClock()
	reporter, tracker, sink := newTestReporter(clock)
	tracker.RecordCall(fault("m", cpi.ReasonRateLimited))
	if err := reporter.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	clock.Advance(reportHeartbeat - time.Second)
	if err := reporter.Flush(context.Background()); err != nil || sink.count() != 1 {
		t.Fatalf("before the heartbeat: err %v, writes %d, want 1", err, sink.count())
	}
	clock.Advance(2 * time.Second)
	if err := reporter.Flush(context.Background()); err != nil || sink.count() != 2 {
		t.Fatalf("at the heartbeat: err %v, writes %d, want 2", err, sink.count())
	}
	if ReporterStaleAfter <= reportHeartbeat {
		t.Fatal("readers must tolerate more than one missed heartbeat")
	}
}

func TestReporterClearsItsRecordsWhenOutcomesAgeOut(t *testing.T) {
	clock := newFakeClock()
	reporter, tracker, sink := newTestReporter(clock)
	ctx := context.Background()
	tracker.RecordCall(fault("m", cpi.ReasonRateLimited))
	if err := reporter.Flush(ctx); err != nil {
		t.Fatal(err)
	}

	clock.Advance(Window + time.Second)
	if err := reporter.Flush(ctx); err != nil || sink.count() != 2 || len(sink.calls[1]) != 0 {
		t.Fatalf("aged-out outcomes must be cleared from the store: err %v, writes %d", err, sink.count())
	}
	if err := reporter.Flush(ctx); err != nil || sink.count() != 2 {
		t.Fatalf("an empty reporter that already cleared must go quiet: err %v, writes %d", err, sink.count())
	}
}

func TestReporterRetriesAfterAFailedPublish(t *testing.T) {
	clock := newFakeClock()
	reporter, tracker, sink := newTestReporter(clock)
	ctx := context.Background()
	tracker.RecordCall(fault("m", cpi.ReasonRateLimited))

	wantErr := errors.New("store unavailable")
	sink.setErr(wantErr)
	if err := reporter.Flush(ctx); !errors.Is(err, wantErr) {
		t.Fatalf("Flush() error = %v, want the store error", err)
	}
	sink.setErr(nil)
	if err := reporter.Flush(ctx); err != nil || sink.count() != 1 {
		t.Fatalf("the failed publish must be retried: err %v, writes %d, want 1", err, sink.count())
	}
}

func TestReporterStartPublishesPeriodicallyAndOnShutdown(t *testing.T) {
	clock := newFakeClock()
	reporter, tracker, sink := newTestReporter(clock)
	reporter.interval = 5 * time.Millisecond
	tracker.RecordCall(fault("m", cpi.ReasonRateLimited))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- reporter.Start(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for sink.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if sink.count() == 0 {
		t.Fatal("Start() never published")
	}

	// An outcome recorded just before shutdown still reaches the store.
	tracker.RecordCall(fault("m", cpi.ReasonRateLimited))
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Start() error = %v, want nil on shutdown", err)
	}
	sink.mu.Lock()
	last := sink.calls[len(sink.calls)-1]
	sink.mu.Unlock()
	if len(last) != 1 || last[0].Faults != 2 {
		t.Fatalf("last publish = %+v, want both faults including the one recorded before shutdown", last)
	}
}

func TestReporterStartSurvivesPublishFailures(t *testing.T) {
	clock := newFakeClock()
	reporter, tracker, sink := newTestReporter(clock)
	reporter.interval = 5 * time.Millisecond
	sink.setErr(errors.New("store unavailable"))
	tracker.RecordCall(fault("m", cpi.ReasonRateLimited))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- reporter.Start(ctx) }()
	time.Sleep(30 * time.Millisecond)
	sink.setErr(nil)

	deadline := time.Now().Add(5 * time.Second)
	for sink.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil || sink.count() == 0 {
		t.Fatalf("Start() = %v with %d writes, want it to recover once the store is back", err, sink.count())
	}
}

func TestNewReporterIDIsUniquePerCall(t *testing.T) {
	first, second := NewReporterID(), NewReporterID()
	if first == "" || first == second {
		t.Fatalf("NewReporterID() = %q and %q, want distinct non-empty identifiers", first, second)
	}

	original := hostname
	t.Cleanup(func() { hostname = original })
	hostname = func() (string, error) { return "", errors.New("no hostname") }
	if id := NewReporterID(); !strings.HasPrefix(id, "unknown/") {
		t.Fatalf("NewReporterID() = %q, want the unknown host fallback", id)
	}
}

func TestReporterStartLogsAndSurvivesAFailedFinalPublish(t *testing.T) {
	clock := newFakeClock()
	reporter, tracker, sink := newTestReporter(clock)
	reporter.interval = time.Hour
	tracker.RecordCall(fault("m", cpi.ReasonRateLimited))
	sink.setErr(errors.New("store unavailable"))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- reporter.Start(ctx) }()
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Start() error = %v, want nil even when the final publish fails", err)
	}
	if sink.count() != 0 {
		t.Fatalf("writes = %d, want none because every publish failed", sink.count())
	}
}

func TestNewReporterToleratesANilLogger(t *testing.T) {
	if reporter := NewReporter(NewTracker(), &fakeSink{}, "id", nil); reporter.logger == nil {
		t.Fatal("NewReporter() must default the logger")
	}
}
