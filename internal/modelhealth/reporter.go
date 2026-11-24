// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package modelhealth

import (
	"context"
	"os"
	"slices"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/internal/store"
)

const (
	// DefaultReportInterval is how often a Reporter looks for something to publish.
	DefaultReportInterval = 15 * time.Second

	// reportHeartbeat bounds how long an unchanged summary goes unpublished, so
	// readers keep treating the reporter as alive.
	reportHeartbeat = 45 * time.Second

	// ReporterStaleAfter is how old a published record may be before readers
	// ignore it. It must be comfortably above reportHeartbeat.
	ReporterStaleAfter = 2 * time.Minute

	finalReportTimeout = 5 * time.Second
)

// hostname is replaceable in tests.
var hostname = os.Hostname

// NewReporterID returns an identifier that is unique to this process run.
func NewReporterID() string {
	host, _ := hostname()
	if host == "" {
		host = "unknown"
	}
	return host + "/" + uuid.NewString()
}

// Reporter periodically publishes a Tracker's summaries to a store so that
// controllers in other processes can read them. Publishing is never on the
// request path: outcomes are recorded in memory and flushed in the background.
type Reporter struct {
	tracker  *Tracker
	sink     store.ModelCallHealthStore
	id       string
	interval time.Duration
	logger   *zap.Logger
	now      func() time.Time

	// published and publishedAt are only touched by the goroutine running Start
	// (or by a single caller of Flush).
	published   []store.ModelCallHealth
	publishedAt time.Time
}

// NewReporter creates a reporter that publishes tracker's summaries as id.
func NewReporter(tracker *Tracker, sink store.ModelCallHealthStore, id string, logger *zap.Logger) *Reporter {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Reporter{tracker: tracker, sink: sink, id: id, interval: DefaultReportInterval, logger: logger.Named("model-call-health"), now: time.Now}
}

// Start publishes until ctx is canceled, then makes a final attempt so that
// outcomes recorded since the last tick are not lost. A failed publish is logged
// and retried on the next tick.
func (r *Reporter) Start(ctx context.Context) error {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			finalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalReportTimeout)
			if err := r.Flush(finalCtx); err != nil {
				r.logger.Warn("final model call health publish failed", zap.Error(err))
			}
			cancel()
			return nil
		case <-ticker.C:
			if err := r.Flush(ctx); err != nil {
				r.logger.Warn("model call health publish failed", zap.Error(err))
			}
		}
	}
}

// Flush publishes the tracker's current summaries unless nothing changed since the
// last publish and the heartbeat is not yet due. A reporter with nothing to
// publish and nothing published does not touch the store.
func (r *Reporter) Flush(ctx context.Context) error {
	snapshots := r.tracker.Snapshot()
	if len(snapshots) == 0 && len(r.published) == 0 {
		return nil
	}
	now := r.now()
	if slices.EqualFunc(snapshots, r.published, sameSummary) && now.Sub(r.publishedAt) < reportHeartbeat {
		return nil
	}
	if err := r.sink.ReplaceModelCallHealth(ctx, r.id, snapshots); err != nil {
		return err
	}
	r.published, r.publishedAt = snapshots, now
	return nil
}

func sameSummary(a, b store.ModelCallHealth) bool {
	return a.Target == b.Target && a.Calls == b.Calls && a.Faults == b.Faults &&
		a.ConsecutiveFaults == b.ConsecutiveFaults && a.ConsecutiveSuccesses == b.ConsecutiveSuccesses &&
		a.LastFaultReason == b.LastFaultReason && a.LastFaultAt.Equal(b.LastFaultAt) && a.LastSuccessAt.Equal(b.LastSuccessAt)
}
