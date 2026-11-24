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

// ModelHealthWatcher notices when the passive health of a Model's target no
// longer matches the Model's status and queues the Model for reconciliation, so
// the status follows real traffic without re-probing every Model on a timer. It
// is a manager runnable and runs only while the controllers run.
type ModelHealthWatcher struct {
	store    store.Store
	health   store.ModelCallHealthStore
	enqueue  func(key string)
	interval time.Duration
	logger   *zap.Logger

	// mayHaveDegraded lets an idle watcher skip listing Models. It starts true so
	// that a restarted controller still recovers Models marked by its predecessor.
	mayHaveDegraded bool
}

// NewModelHealthWatcher creates a watcher. enqueue receives "namespace/name" keys
// of the Model controller's queue.
func NewModelHealthWatcher(st store.Store, health store.ModelCallHealthStore, enqueue func(key string), logger *zap.Logger) *ModelHealthWatcher {
	return &ModelHealthWatcher{
		store:           st,
		health:          health,
		enqueue:         enqueue,
		interval:        DefaultModelHealthWatchInterval,
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
		target := modelhealth.TargetKey(provider, model.Spec.Endpoint, model.Spec.ModelID)
		next := modelhealth.Evaluate(aggregates[target], wasDegraded)
		anyDegraded = anyDegraded || next.Degraded || wasDegraded
		if next.Degraded != wasDegraded || (next.Degraded && next.Reason != previous.Reason) {
			w.enqueue(model.ObjectMeta.Namespace + "/" + model.ObjectMeta.Name)
		}
	}
	w.mayHaveDegraded = anyDegraded
	return nil
}

// modelClassCache answers ModelClass reads once per check, since many Models
// usually share a few classes.
type modelClassCache struct {
	store.Store
	cache map[string]modelClassLookup
}

type modelClassLookup struct {
	data []byte
	err  error
}

func (c *modelClassCache) Get(ctx context.Context, kind, namespace, name string) ([]byte, error) {
	if kind != "ModelClass" {
		return c.Store.Get(ctx, kind, namespace, name)
	}
	key := namespace + "/" + name
	if lookup, ok := c.cache[key]; ok {
		return lookup.data, lookup.err
	}
	data, err := c.Store.Get(ctx, kind, namespace, name)
	if err == nil || errors.Is(err, store.ErrNotFound) {
		c.cache[key] = modelClassLookup{data: data, err: err}
	}
	return data, err
}
