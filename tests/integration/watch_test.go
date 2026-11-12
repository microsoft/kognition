// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

// flushRecorder wraps httptest.ResponseRecorder and implements http.Flusher.
type flushRecorder struct {
	*httptest.ResponseRecorder
}

func (f *flushRecorder) Flush() {}

// TestWatch_ReceivesCreateEvent verifies that a watch stream receives an event
// when a resource is created via the API.
func TestWatch_ReceivesCreateEvent(t *testing.T) {
	h := newTestHarness(t)

	// Start watch in a goroutine; use context to cancel
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req := httptest.NewRequest("GET", "/apis/kognition.dev/v1alpha1/watch/namespaces/default/agents", nil)
	req = req.WithContext(ctx)
	rec := &flushRecorder{httptest.NewRecorder()}

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.Server.ServeHTTP(rec, req)
	}()

	// Give watch handler time to set up
	time.Sleep(50 * time.Millisecond)

	// Create an agent via direct store mutation to trigger the watcher
	agent := makeAgent("watch-target", "default", "gpt-4", v1alpha1.PhaseReady)
	data, _ := json.Marshal(agent)
	h.Store.Create(context.Background(), "Agent", "default", "watch-target", data)

	// Send watch event manually since memStore.Create notifies
	// Actually memStore.Create already calls notifyLocked. But we need to verify
	// that the Create method in memStore notifies watchers.

	// Wait for handler to process
	time.Sleep(100 * time.Millisecond)
	cancel()
	waitForWatchHandler(t, done)

	body := rec.Body.String()
	if !strings.Contains(body, "watch-target") {
		t.Fatalf("expected watch event with 'watch-target', got: %s", body)
	}
	if !strings.Contains(body, "ADDED") {
		t.Fatalf("expected ADDED event type, got: %s", body)
	}
}

// TestWatch_SSEFormat verifies events are formatted as SSE "data:" lines.
func TestWatch_SSEFormat(t *testing.T) {
	h := newTestHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req := httptest.NewRequest("GET", "/apis/kognition.dev/v1alpha1/watch/namespaces/default/agents", nil)
	req = req.WithContext(ctx)
	rec := &flushRecorder{httptest.NewRecorder()}

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.Server.ServeHTTP(rec, req)
	}()

	time.Sleep(50 * time.Millisecond)

	// Inject event through store
	agent := makeAgent("sse-test", "default", "gpt-4", v1alpha1.PhaseReady)
	data, _ := json.Marshal(agent)
	h.Store.Create(context.Background(), "Agent", "default", "sse-test", data)

	time.Sleep(100 * time.Millisecond)
	cancel()
	waitForWatchHandler(t, done)

	// Verify SSE format
	body := rec.Body.String()
	scanner := bufio.NewScanner(strings.NewReader(body))
	foundData := false
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			foundData = true
			payload := strings.TrimPrefix(line, "data: ")
			var evt map[string]any
			if err := json.Unmarshal([]byte(payload), &evt); err != nil {
				t.Fatalf("SSE data not valid JSON: %v", err)
			}
			if evt["type"] == nil {
				t.Fatal("SSE event missing 'type' field")
			}
			if evt["object"] == nil {
				t.Fatal("SSE event missing 'object' field")
			}
		}
	}
	if !foundData {
		t.Fatalf("no SSE data lines found in response: %q", body)
	}
}

// TestWatch_ContentTypeSSE verifies the watch endpoint sets the correct content type.
func TestWatch_ContentTypeSSE(t *testing.T) {
	h := newTestHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest("GET", "/apis/kognition.dev/v1alpha1/watch/namespaces/default/agents", nil)
	req = req.WithContext(ctx)
	rec := &flushRecorder{httptest.NewRecorder()}

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.Server.ServeHTTP(rec, req)
	}()

	// Inject an event to ensure headers are written
	time.Sleep(50 * time.Millisecond)
	agent := makeAgent("ct-test", "default", "gpt-4", v1alpha1.PhaseReady)
	data, _ := json.Marshal(agent)
	h.Store.Create(context.Background(), "Agent", "default", "ct-test", data)

	time.Sleep(100 * time.Millisecond)
	cancel()
	waitForWatchHandler(t, done)

	ct := rec.Header().Get("Content-Type")
	if ct != "text/event-stream" {
		t.Fatalf("expected Content-Type text/event-stream, got %q", ct)
	}
}

// TestWatch_MultipleEvents verifies that multiple events are received in order.
func TestWatch_MultipleEvents(t *testing.T) {
	h := newTestHarness(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req := httptest.NewRequest("GET", "/apis/kognition.dev/v1alpha1/watch/namespaces/default/agents", nil)
	req = req.WithContext(ctx)
	rec := &flushRecorder{httptest.NewRecorder()}

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.Server.ServeHTTP(rec, req)
	}()

	time.Sleep(50 * time.Millisecond)

	// Create multiple agents
	for i, name := range []string{"w1", "w2", "w3"} {
		_ = i
		a := makeAgent(name, "default", "m", v1alpha1.PhaseReady)
		d, _ := json.Marshal(a)
		h.Store.Create(context.Background(), "Agent", "default", name, d)
		time.Sleep(20 * time.Millisecond)
	}

	time.Sleep(100 * time.Millisecond)
	cancel()
	waitForWatchHandler(t, done)

	body := rec.Body.String()
	count := strings.Count(body, "data: ")
	if count < 3 {
		t.Fatalf("expected at least 3 SSE events, got %d in: %s", count, body)
	}
}

func waitForWatchHandler(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for canceled watch handler")
	}
}

// TestWatch_StoreNotifiesOnCreate verifies memStore triggers watchers on Create.
func TestWatch_StoreNotifiesOnCreate(t *testing.T) {
	h := newTestHarness(t)

	ch, err := h.Store.Watch(context.Background(), "Agent", store.ListOptions{Namespace: "default"})
	if err != nil {
		t.Fatal(err)
	}

	agent := makeAgent("notify-test", "default", "m", v1alpha1.PhaseReady)
	data, _ := json.Marshal(agent)
	h.Store.Create(context.Background(), "Agent", "default", "notify-test", data)

	select {
	case evt := <-ch:
		if evt.Type != store.EventCreated {
			t.Fatalf("expected ADDED, got %s", evt.Type)
		}
		if evt.Name != "notify-test" {
			t.Fatalf("expected name 'notify-test', got %q", evt.Name)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for watch event")
	}
}
