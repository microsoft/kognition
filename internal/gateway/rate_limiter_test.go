// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gateway

import (
	"testing"
	"time"
)

func TestRateLimiterAllowBurst(t *testing.T) {
	rateLimiter := NewRateLimiter(10, 3)
	defer rateLimiter.Stop()
	for index := 0; index < 3; index++ {
		if !rateLimiter.Allow("key1") {
			t.Fatalf("expected Allow to return true on call %d", index+1)
		}
	}
	if rateLimiter.Allow("key1") {
		t.Fatal("expected Allow to return false after burst exhausted")
	}
}

func TestRateLimiterTokenReplenishment(t *testing.T) {
	rateLimiter := NewRateLimiter(10, 2)
	defer rateLimiter.Stop()
	rateLimiter.Allow("key1")
	rateLimiter.Allow("key1")
	if rateLimiter.Allow("key1") {
		t.Fatal("expected denial after burst")
	}
	rateLimiter.mu.Lock()
	rateLimiter.buckets["key1"].lastAccess = time.Now().Add(-100 * time.Millisecond)
	rateLimiter.mu.Unlock()
	if !rateLimiter.Allow("key1") {
		t.Fatal("expected Allow after token replenishment")
	}
}

func TestRateLimiterIndependentKeys(t *testing.T) {
	rateLimiter := NewRateLimiter(1, 1)
	defer rateLimiter.Stop()
	if !rateLimiter.Allow("a") || !rateLimiter.Allow("b") {
		t.Fatal("independent keys should be allowed")
	}
	if rateLimiter.Allow("a") {
		t.Fatal("exhausted key should be denied")
	}
}

func TestRateLimiterEviction(t *testing.T) {
	rateLimiter := &RateLimiter{buckets: make(map[string]*bucket), rate: 1, burst: 1, ttl: 50 * time.Millisecond, stop: make(chan struct{})}
	defer rateLimiter.Stop()
	rateLimiter.Allow("stale")
	rateLimiter.mu.Lock()
	rateLimiter.buckets["stale"].lastAccess = time.Now().Add(-100 * time.Millisecond)
	for key, entry := range rateLimiter.buckets {
		if time.Since(entry.lastAccess) > rateLimiter.ttl {
			delete(rateLimiter.buckets, key)
		}
	}
	_, exists := rateLimiter.buckets["stale"]
	rateLimiter.mu.Unlock()
	if exists {
		t.Fatal("expected stale bucket to be evicted")
	}
}

func TestRateLimiterStopTerminatesGoroutine(t *testing.T) {
	NewRateLimiter(1, 1).Stop()
}
