// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package externaltransport

import (
	"sync"
	"time"
)

// Limiter bounds the tasks in flight to each ExternalAgent from this process. A
// reservation is keyed by dispatch, so reserving again is idempotent, and it
// expires after its ttl so a lost release cannot hold a slot forever. The bound
// is per controller process: it is not shared across replicas or restarts.
type Limiter struct {
	mu    sync.Mutex
	slots map[string]map[string]time.Time
}

// NewLimiter returns an empty Limiter.
func NewLimiter() *Limiter { return &Limiter{slots: map[string]map[string]time.Time{}} }

// Reserve takes a slot for dispatch under key when fewer than max are live.
func (l *Limiter) Reserve(key, dispatch string, max int32, now time.Time, ttl time.Duration) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	held := l.slots[key]
	if held == nil {
		held = map[string]time.Time{}
		l.slots[key] = held
	}
	for id, expires := range held {
		if !now.Before(expires) {
			delete(held, id)
		}
	}
	if _, ok := held[dispatch]; ok {
		held[dispatch] = now.Add(ttl)
		return true
	}
	if int32(len(held)) >= max {
		return false
	}
	held[dispatch] = now.Add(ttl)
	return true
}

// Release frees the slot of dispatch.
func (l *Limiter) Release(key, dispatch string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if held := l.slots[key]; held != nil {
		delete(held, dispatch)
		if len(held) == 0 {
			delete(l.slots, key)
		}
	}
}
