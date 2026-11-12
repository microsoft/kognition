// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Controller ties together an Informer, WorkQueue, and Reconciler for a single
// resource kind. It implements EventHandler so the Informer dispatches to it.
type Controller struct {
	name       string
	kind       string
	reconciler Reconciler
	queue      *WorkQueue
	informer   *Informer
	workers    int
	logger     *zap.Logger
}

// ControllerConfig holds the parameters for creating a Controller.
type ControllerConfig struct {
	// Name is a human-readable identifier for this controller (e.g., "agent-controller").
	Name string

	// Kind is the resource kind this controller reconciles (e.g., "Agent").
	Kind string

	// Reconciler implements the business logic.
	Reconciler Reconciler

	// Workers is the number of concurrent reconciliation goroutines (default 1).
	Workers int

	// BaseDelay is the initial backoff delay on failure (default 1s).
	BaseDelay time.Duration

	// MaxDelay is the maximum backoff delay (default 5m).
	MaxDelay time.Duration

	// MaxRetries is the maximum number of retries per key (0 = unlimited, default 10).
	MaxRetries int
}

func (c *ControllerConfig) setDefaults() {
	if c.Workers <= 0 {
		c.Workers = 1
	}
	if c.BaseDelay == 0 {
		c.BaseDelay = 1 * time.Second
	}
	if c.MaxDelay == 0 {
		c.MaxDelay = 5 * time.Minute
	}
	if c.MaxRetries == 0 {
		c.MaxRetries = 10
	}
}

// NewController creates a Controller with the given configuration and informer.
func NewController(cfg ControllerConfig, informer *Informer, logger *zap.Logger) *Controller {
	cfg.setDefaults()

	queue := NewWorkQueue(cfg.BaseDelay, cfg.MaxDelay, cfg.MaxRetries)

	c := &Controller{
		name:       cfg.Name,
		kind:       cfg.Kind,
		reconciler: cfg.Reconciler,
		queue:      queue,
		informer:   informer,
		workers:    cfg.Workers,
		logger:     logger.With(zap.String("controller", cfg.Name)),
	}

	// Register this controller as an event handler on the informer.
	informer.AddEventHandler(c)

	return c
}

// --- EventHandler implementation ---

func (c *Controller) OnAdd(key string)    { c.queue.Add(key) }
func (c *Controller) OnUpdate(key string) { c.queue.Add(key) }
func (c *Controller) OnDelete(key string) { c.queue.Add(key) }

// Run starts the informer and worker goroutines. Blocks until ctx is canceled.
func (c *Controller) Run(ctx context.Context) error {
	c.logger.Info("starting controller",
		zap.String("kind", c.kind),
		zap.Int("workers", c.workers),
	)

	// Start the informer in a goroutine.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := c.informer.Run(ctx); err != nil {
			c.logger.Error("informer error", zap.Error(err))
		}
	}()

	// Wait for the informer to complete its initial sync.
	if !c.waitForSync(ctx) {
		c.queue.ShutDown()
		wg.Wait()
		return ctx.Err()
	}

	c.logger.Info("informer synced, starting workers")

	// Start worker goroutines.
	for i := 0; i < c.workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			c.runWorker(ctx, id)
		}(i)
	}

	// Block until context is canceled.
	<-ctx.Done()
	c.logger.Info("shutting down controller")
	c.queue.ShutDown()
	wg.Wait()
	return nil
}

// waitForSync polls until the informer has synced or context is canceled.
func (c *Controller) waitForSync(ctx context.Context) bool {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		if c.informer.HasSynced() {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

// runWorker processes items from the queue until shutdown.
func (c *Controller) runWorker(ctx context.Context, id int) {
	c.logger.Debug("worker started", zap.Int("worker", id))
	for {
		key, ok := c.queue.Get()
		if !ok {
			c.logger.Debug("worker shutting down", zap.Int("worker", id))
			return
		}
		c.processItem(ctx, key)
	}
}

// processItem reconciles a single key and handles the result.
func (c *Controller) processItem(ctx context.Context, key string) {
	defer c.queue.Done(key)

	c.logger.Debug("processing item", zap.String("key", key))
	req := c.keyToRequest(key)
	result, err := c.reconciler.Reconcile(ctx, req)

	if err != nil {
		if ok := c.queue.Requeue(key); !ok {
			c.logger.Error("item dropped after max retries",
				zap.String("key", key),
				zap.Int("retries", c.queue.maxRetries),
			)
		} else {
			c.logger.Warn("reconciliation failed, will retry",
				zap.String("key", key),
				zap.Error(err),
				zap.Int("retries", c.queue.NumRequeues(key)),
			)
		}
		return
	}

	// Success: reset failure count.
	c.queue.Forget(key)

	if result.RequeueAfter > 0 {
		c.queue.AddAfter(key, result.RequeueAfter)
	} else if result.Requeue {
		c.queue.Add(key)
	}
}

// keyToRequest parses "namespace/name" or "name" back into a Request.
func (c *Controller) keyToRequest(key string) Request {
	parts := strings.SplitN(key, "/", 2)
	if len(parts) == 2 {
		return Request{Kind: c.kind, Namespace: parts[0], Name: parts[1]}
	}
	return Request{Kind: c.kind, Name: parts[0]}
}
