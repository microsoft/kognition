// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gateway

import (
	"sync"
	"time"
)

// RateLimiter implements per-key token bucket rate limiting for gateway goal submissions.
// Stale entries are evicted after the configured TTL to prevent unbounded memory growth.
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64
	burst   int
	ttl     time.Duration
	stop    chan struct{}
}

type bucket struct {
	tokens     float64
	lastAccess time.Time
}

// NewRateLimiter creates a RateLimiter allowing rate requests per second with the given burst.
// Stale entries are evicted every 60 seconds if unused for 5 minutes.
func NewRateLimiter(rate float64, burst int) *RateLimiter {
	rateLimiter := &RateLimiter{
		buckets: make(map[string]*bucket),
		rate:    rate,
		burst:   burst,
		ttl:     5 * time.Minute,
		stop:    make(chan struct{}),
	}
	go rateLimiter.evictLoop()
	return rateLimiter
}

// Stop terminates the background eviction goroutine.
func (rl *RateLimiter) Stop() {
	close(rl.stop)
}

func (rl *RateLimiter) evictLoop() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-rl.stop:
			return
		case now := <-ticker.C:
			rl.mu.Lock()
			for key, entry := range rl.buckets {
				if now.Sub(entry.lastAccess) > rl.ttl {
					delete(rl.buckets, key)
				}
			}
			rl.mu.Unlock()
		}
	}
}

// Allow checks whether a request with the given key is allowed.
func (rl *RateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	entry, ok := rl.buckets[key]
	if !ok {
		rl.buckets[key] = &bucket{tokens: float64(rl.burst) - 1, lastAccess: now}
		return true
	}

	elapsed := now.Sub(entry.lastAccess).Seconds()
	entry.tokens += elapsed * rl.rate
	if entry.tokens > float64(rl.burst) {
		entry.tokens = float64(rl.burst)
	}
	entry.lastAccess = now
	if entry.tokens < 1 {
		return false
	}
	entry.tokens--
	return true
}
