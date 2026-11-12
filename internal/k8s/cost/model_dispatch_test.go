// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cost

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/store"
)

func TestConfigMapModelDispatchClaimsAndReplay(test *testing.T) {
	storage := newTestStore(test)
	ctx := context.Background()
	if _, err := storage.GetModelDispatch(ctx, "tenant", "operation"); !errors.Is(err, store.ErrNotFound) {
		test.Fatal("missing claim was treated as completed")
	}
	claimed, err := storage.ClaimModelDispatch(ctx, "tenant", "operation", "session")
	if err != nil || !claimed {
		test.Fatalf("claim=%t error=%v", claimed, err)
	}
	if claimed, err := storage.ClaimModelDispatch(ctx, "tenant", "operation", "other-session"); err != nil || claimed {
		test.Fatal("duplicate operation was claimed again")
	}
	record, err := storage.GetModelDispatch(ctx, "tenant", "operation")
	if err != nil || record.SessionID != "session" || len(record.Response) != 0 {
		test.Fatal("unfinished claim did not preserve uncertain execution")
	}
	response := json.RawMessage(`{"version":1,"passed":true}`)
	if err := storage.CompleteModelDispatch(ctx, "tenant", "operation", "other-session", response); !errors.Is(err, store.ErrConflict) {
		test.Fatal("another session completed the claimed operation")
	}
	if err := storage.CompleteModelDispatch(ctx, "tenant", "operation", "session", response); err != nil {
		test.Fatal(err)
	}
	if err := storage.CompleteModelDispatch(ctx, "tenant", "operation", "session", response); err != nil {
		test.Fatal("identical terminal replay failed")
	}
	if err := storage.CompleteModelDispatch(ctx, "tenant", "operation", "session", json.RawMessage(`{"version":1,"passed":false}`)); !errors.Is(err, store.ErrConflict) {
		test.Fatal("terminal result was overwritten")
	}
	restarted, err := NewConfigMapStore(storage.client, storage.namespace)
	if err != nil {
		test.Fatal(err)
	}
	record, err = restarted.GetModelDispatch(ctx, "tenant", "operation")
	if err != nil || string(record.Response) != string(response) {
		test.Fatal("restart did not recover immutable terminal result")
	}
	if _, err := restarted.GetModelDispatch(ctx, "other-tenant", "operation"); !errors.Is(err, store.ErrNotFound) {
		test.Fatal("another workspace received this result")
	}
}

func TestConfigMapStoreAcceptsModelCanaryUsage(test *testing.T) {
	storage := newTestStore(test)
	event := observability.CostRecord{ID: "model-canary:operation", Workspace: "tenant", Purpose: observability.UsagePurposeModelCanary, OperationType: observability.CostOpModelInference, UsageReported: true, PromptTokens: 12, CompletionTokens: 6, TotalTokens: 18, Timestamp: time.Now().UTC()}
	if applied, err := storage.ApplyUsageEvent(context.Background(), event); err != nil || !applied {
		test.Fatalf("canary usage rejected: applied=%t error=%v", applied, err)
	}
	if applied, err := storage.ApplyUsageEvent(context.Background(), event); err != nil || applied {
		test.Fatal("duplicate canary usage was applied again")
	}
}
