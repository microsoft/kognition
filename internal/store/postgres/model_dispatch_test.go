// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/internal/store"
)

func TestModelDispatchAtomicClaimAndImmutableCompletion(t *testing.T) {
	ctx := context.Background()
	backend := New(testPool, zaptest.NewLogger(t))
	namespace := "dispatch-" + uuid.NewString()
	t.Cleanup(func() {
		if _, err := testPool.Exec(ctx, "DELETE FROM model_dispatches WHERE namespace = $1", namespace); err != nil {
			t.Error(err)
		}
	})
	var winners atomic.Int32
	var wait sync.WaitGroup
	for contender := 0; contender < 12; contender++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			claimed, err := backend.ClaimModelDispatch(ctx, namespace, "operation", uuid.NewString())
			if err != nil {
				t.Error(err)
			}
			if claimed {
				winners.Add(1)
			}
		}()
	}
	wait.Wait()
	if winners.Load() != 1 {
		t.Fatalf("claim winners = %d", winners.Load())
	}
	record, err := backend.GetModelDispatch(ctx, namespace, "operation")
	if err != nil || len(record.Response) != 0 {
		t.Fatalf("pending record: %v", err)
	}
	for _, invalid := range []string{"null", " \nnull\t", "[]", "true", "\"text\"", "{"} {
		if err := backend.CompleteModelDispatch(ctx, namespace, "operation", record.SessionID, json.RawMessage(invalid)); err == nil {
			t.Fatalf("accepted non-object completion %q", invalid)
		}
	}
	response := json.RawMessage(`{"Content":"private output"}`)
	if err := backend.CompleteModelDispatch(ctx, namespace, "operation", "wrong-session", response); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("wrong session completion = %v", err)
	}
	if err := backend.CompleteModelDispatch(ctx, namespace, "operation", record.SessionID, response); err != nil {
		t.Fatal(err)
	}
	if err := backend.CompleteModelDispatch(ctx, namespace, "operation", record.SessionID, response); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("overwrite completion = %v", err)
	}
	restored, err := New(testPool, zaptest.NewLogger(t)).GetModelDispatch(ctx, namespace, "operation")
	if err != nil || restored.SessionID != record.SessionID || len(restored.Response) == 0 {
		t.Fatalf("restored completion: %v", err)
	}
	var decoded struct{ Content string }
	if err := json.Unmarshal(restored.Response, &decoded); err != nil || decoded.Content != "private output" {
		t.Fatalf("stored completion changed: %v", err)
	}
	if _, err := backend.GetModelDispatch(ctx, namespace+"-other", "operation"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cross-workspace lookup = %v", err)
	}
}
