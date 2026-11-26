// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/modelhealth"
	"github.com/kognition-ai/kognition/internal/store"
)

// DefaultModelHealthWatchInterval is how often the watcher compares passive
// health with the Models' status.
const DefaultModelHealthWatchInterval = 15 * time.Second

// DefaultModelHealthRequeueBackoff is how long the watcher waits before queuing a
// Model again for the same outcome. A Model whose reconciliation does not move its
// status to match the evidence, for example because its probe is slow or its store
// write fails, is not probed again on every check. A different outcome, such as a
// recovery, is queued at once.
const DefaultModelHealthRequeueBackoff = time.Minute

// ModelHealthWatcher notices when the passive health of a Model's target no
// longer matches the Model's status and queues the Model for reconciliation, so
// the status follows real traffic without re-probing every Model on a timer. It
// is a manager runnable and runs only while the controllers run.
type ModelHealthWatcher struct {
	store    store.Store
	health   store.ModelCallHealthStore
	enqueue  func(key string)
	interval time.Duration
	backoff  time.Duration
	now      func() time.Time
	logger   *zap.Logger

	// mayHaveDegraded lets an idle watcher skip listing Models. It starts true so
	// that a restarted controller still recovers Models marked by its predecessor.
	mayHaveDegraded bool
	// queued remembers the Models queued for an outcome their status does not
	// reflect yet, and when. It is only used by the goroutine that calls Check.
	queued map[string]queuedOutcome
}

// queuedOutcome is what a Model was queued for.
type queuedOutcome struct {
	degraded bool
	reason   string
	at       time.Time
}

// NewModelHealthWatcher creates a watcher. enqueue receives "namespace/name" keys
// of the Model controller's queue.
func NewModelHealthWatcher(st store.Store, health store.ModelCallHealthStore, enqueue func(key string), logger *zap.Logger) *ModelHealthWatcher {
	return &ModelHealthWatcher{
		store:           st,
		health:          health,
		enqueue:         enqueue,
		interval:        DefaultModelHealthWatchInterval,
		backoff:         DefaultModelHealthRequeueBackoff,
		now:             time.Now,
		logger:          logger.With(zap.String("runnable", "model-health-watcher")),
		mayHaveDegraded: true,
	}
}

// Start checks on every tick until ctx is canceled.
func (w *ModelHealthWatcher) Start(ctx context.Context) error {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := w.Check(ctx); err != nil {
				w.logger.Warn("check passive model health", zap.Error(err))
			}
		}
	}
}

// Check queues every Model whose passive degradation, its reason or its recovery
// is not yet reflected in its status.
//
// A Model that is unready for another reason, such as a failing probe, is not
// queued to become passively degraded: the passive verdict only applies to a Model
// whose probe passes, so reconciling it cannot change its status, and queuing it
// would re-probe it on every check. It is queued again once its status shows
// something the evidence can change, and a Model queued for an outcome is not
// queued for the same outcome again until the backoff has passed.
func (w *ModelHealthWatcher) Check(ctx context.Context) error {
	records, err := w.health.ListModelCallHealth(ctx, modelhealth.ReporterStaleAfter)
	if err != nil {
		return fmt.Errorf("read passive model health: %w", err)
	}
	if len(records) == 0 && !w.mayHaveDegraded {
		return nil
	}
	aggregates := modelhealth.MergeByTarget(records)

	raws, err := w.store.List(ctx, modelKind, store.ListOptions{})
	if err != nil {
		return fmt.Errorf("list models: %w", err)
	}
	classes := &modelClassCache{Store: w.store, cache: make(map[string]modelClassLookup)}
	now := w.now()
	pending := make(map[string]queuedOutcome)
	anyDegraded := false
	for _, raw := range raws {
		var model v1alpha1.Model
		if json.Unmarshal(raw, &model) != nil || model.Status.Phase == v1alpha1.PhaseTerminated {
			continue
		}
		previous, wasDegraded := modelPassiveDegradation(&model)
		provider, _, failure := resolveModelProvider(ctx, classes, &model)
		if failure != nil {
			// The Model's own reconciler reports an unresolvable provider.
			anyDegraded = anyDegraded || wasDegraded
			continue
		}
		targets := modelHealthTargets(ctx, classes, model.ObjectMeta.Namespace, provider, &model)
		next := modelhealth.EvaluateModel(aggregates, targets, previous, wasDegraded)
		anyDegraded = anyDegraded || next.Degraded || wasDegraded
		if next.Degraded == wasDegraded && (!next.Degraded || next.Reason == previous.Reason) {
			continue
		}
		if next.Degraded && !wasDegraded && modelHasActiveFailure(&model) {
			continue
		}
		key := model.ObjectMeta.Namespace + "/" + model.ObjectMeta.Name
		outcome := queuedOutcome{degraded: next.Degraded, reason: next.Reason, at: now}
		if earlier, queued := w.queued[key]; queued && earlier.degraded == outcome.degraded && earlier.reason == outcome.reason && now.Sub(earlier.at) < w.backoff {
			pending[key] = earlier
			continue
		}
		w.enqueue(key)
		pending[key] = outcome
	}
	w.queued = pending
	w.mayHaveDegraded = anyDegraded
	return nil
}

// modelClassCache answers ModelClass and SecretRef reads once per check, since
// many Models usually share a few classes and credentials.
type modelClassCache struct {
	store.Store
	cache map[string]modelClassLookup
}

type modelClassLookup struct {
	data []byte
	err  error
}

func (c *modelClassCache) Get(ctx context.Context, kind, namespace, name string) ([]byte, error) {
	if kind != "ModelClass" && kind != "SecretRef" {
		return c.Store.Get(ctx, kind, namespace, name)
	}
	key := kind + "/" + namespace + "/" + name
	if lookup, ok := c.cache[key]; ok {
		return lookup.data, lookup.err
	}
	data, err := c.Store.Get(ctx, kind, namespace, name)
	if err == nil || errors.Is(err, store.ErrNotFound) {
		c.cache[key] = modelClassLookup{data: data, err: err}
	}
	return data, err
}
