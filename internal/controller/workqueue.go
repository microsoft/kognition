// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"math"
	"math/rand/v2"
	"sync"
	"time"
)

// WorkQueue is a rate-limited work queue with deduplication and exponential backoff.
type WorkQueue struct {
	mu         sync.Mutex
	cond       *sync.Cond
	queue      []string
	queued     map[string]struct{} // keys currently in queue
	processing map[string]struct{} // keys being processed
	dirty      map[string]struct{} // keys added while being processed
	failures   map[string]int      // failure count per key
	delayed    map[string]*time.Timer
	shutdown   bool

	baseDelay  time.Duration
	maxDelay   time.Duration
	maxRetries int
}

// NewWorkQueue creates a work queue with the given backoff parameters.
// baseDelay: initial delay after first failure (e.g. 1s)
// maxDelay: maximum backoff cap (e.g. 5m)
// maxRetries: maximum number of retries before dropping the item (0 = unlimited)
func NewWorkQueue(baseDelay, maxDelay time.Duration, maxRetries int) *WorkQueue {
	q := &WorkQueue{
		queued:     make(map[string]struct{}),
		processing: make(map[string]struct{}),
		dirty:      make(map[string]struct{}),
		failures:   make(map[string]int),
		delayed:    make(map[string]*time.Timer),
		baseDelay:  baseDelay,
		maxDelay:   maxDelay,
		maxRetries: maxRetries,
	}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// Add enqueues a key for processing. If the key is already queued, this is a no-op.
// If the key is currently being processed, it will be re-queued after Done is called.
func (q *WorkQueue) Add(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.cancelDelayedLocked(key)
	q.addLocked(key)
}

func (q *WorkQueue) addLocked(key string) {

	if q.shutdown {
		return
	}

	// If currently being processed, mark dirty so it's re-queued after Done.
	if _, ok := q.processing[key]; ok {
		q.dirty[key] = struct{}{}
		return
	}

	// If already queued, no-op.
	if _, ok := q.queued[key]; ok {
		return
	}

	q.queue = append(q.queue, key)
	q.queued[key] = struct{}{}
	q.cond.Signal()
}

// AddAfter schedules one keyed delayed enqueue, replacing any prior deadline.
func (q *WorkQueue) AddAfter(key string, delay time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.shutdown {
		return
	}
	q.cancelDelayedLocked(key)
	var timer *time.Timer
	timer = time.AfterFunc(delay, func() {
		q.mu.Lock()
		defer q.mu.Unlock()
		if q.delayed[key] != timer {
			return
		}
		delete(q.delayed, key)
		q.addLocked(key)
	})
	q.delayed[key] = timer
}

func (q *WorkQueue) cancelDelayedLocked(key string) {
	if timer := q.delayed[key]; timer != nil {
		timer.Stop()
		delete(q.delayed, key)
	}
}

// Get blocks until an item is available, then returns it.
// The caller must call Done(key) when processing is complete.
// Returns "", false if the queue has been shut down and is empty.
func (q *WorkQueue) Get() (string, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()

	for len(q.queue) == 0 && !q.shutdown {
		q.cond.Wait()
	}

	if len(q.queue) == 0 && q.shutdown {
		return "", false
	}

	key := q.queue[0]
	q.queue = q.queue[1:]
	delete(q.queued, key)
	q.processing[key] = struct{}{}
	return key, true
}

// Done marks an item as finished processing.
// If the item was re-added while being processed (dirty), it is re-queued.
func (q *WorkQueue) Done(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()

	delete(q.processing, key)

	if _, ok := q.dirty[key]; ok {
		delete(q.dirty, key)
		q.queue = append(q.queue, key)
		q.queued[key] = struct{}{}
		q.cond.Signal()
	}
}

// Requeue re-adds a key with exponential backoff delay.
// Returns false if max retries have been exceeded (item is dropped).
func (q *WorkQueue) Requeue(key string) bool {
	q.mu.Lock()

	q.failures[key]++
	failures := q.failures[key]

	if q.maxRetries > 0 && failures > q.maxRetries {
		delete(q.failures, key)
		q.mu.Unlock()
		return false
	}

	delay := q.backoffDelay(failures)
	q.mu.Unlock()

	q.AddAfter(key, delay)
	return true
}

// Forget resets the failure count for a key, typically called after a successful reconciliation.
func (q *WorkQueue) Forget(key string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.failures, key)
}

// NumRequeues returns the number of times a key has been requeued.
func (q *WorkQueue) NumRequeues(key string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.failures[key]
}

// Len returns the number of items in the queue (not including items being processed).
func (q *WorkQueue) Len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.queue)
}

// ShutDown signals the queue to stop accepting new items and unblocks Get.
func (q *WorkQueue) ShutDown() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.shutdown = true
	for key, timer := range q.delayed {
		timer.Stop()
		delete(q.delayed, key)
	}
	q.cond.Broadcast()
}

// ShuttingDown returns true if ShutDown has been called.
func (q *WorkQueue) ShuttingDown() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.shutdown
}

// backoffDelay calculates exponential backoff with jitter.
// delay = baseDelay * 2^(failures-1) * (1 + jitter), capped at maxDelay.
func (q *WorkQueue) backoffDelay(failures int) time.Duration {
	exp := math.Pow(2, float64(failures-1))
	delay := float64(q.baseDelay) * exp

	// Add jitter: ±25% of the calculated delay.
	jitter := delay * 0.25 * (rand.Float64()*2 - 1)
	delay += jitter

	if delay > float64(q.maxDelay) {
		delay = float64(q.maxDelay)
	}
	if delay < float64(q.baseDelay) {
		delay = float64(q.baseDelay)
	}

	return time.Duration(delay)
}
