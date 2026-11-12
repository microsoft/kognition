// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"testing"
	"time"
)

func TestWorkQueueAddAfterReplacesDeadlineAndStopsOnShutdown(t *testing.T) {
	queue := NewWorkQueue(time.Millisecond, time.Second, 3)
	queue.AddAfter("default/profile", time.Hour)
	first := queue.delayed["default/profile"]
	if first == nil || len(queue.delayed) != 1 {
		t.Fatal("first delayed enqueue was not registered")
	}
	queue.AddAfter("default/profile", 2*time.Hour)
	second := queue.delayed["default/profile"]
	if second == nil || second == first || len(queue.delayed) != 1 {
		t.Fatal("delayed enqueue was not replaced by key")
	}
	queue.ShutDown()
	if len(queue.delayed) != 0 {
		t.Fatal("shutdown did not cancel delayed enqueues")
	}
	queue.AddAfter("default/profile", time.Millisecond)
	if len(queue.delayed) != 0 {
		t.Fatal("shutdown queue accepted a delayed enqueue")
	}
}
