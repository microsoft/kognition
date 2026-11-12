// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/observability"
)

type recordingCostStore struct {
	workspace string
	before    time.Time
	deleted   int64
}

func (*recordingCostStore) RecordCost(context.Context, observability.CostRecord) error { return nil }
func (*recordingCostStore) RecordCostBatch(context.Context, []observability.CostRecord) error {
	return nil
}
func (*recordingCostStore) QueryCosts(context.Context, observability.CostQuery) (*observability.CostQueryResult, error) {
	return &observability.CostQueryResult{}, nil
}
func (*recordingCostStore) GetTotalCost(context.Context, string, time.Time, time.Time) (observability.CostUnit, error) {
	return 0, nil
}
func (s *recordingCostStore) DeleteCostRecordsBefore(_ context.Context, workspace string, before time.Time) (int64, error) {
	s.workspace, s.before = workspace, before
	return s.deleted, nil
}

func TestCostRetentionReconcilerDeletesExpiredWorkspaceRecords(t *testing.T) {
	memory := newMemStore()
	workspace := v1alpha1.Workspace{ObjectMeta: v1alpha1.ObjectMeta{Name: "retention-policy", Namespace: "team-a"}, Spec: v1alpha1.WorkspaceSpec{Retention: &v1alpha1.RetentionPolicy{CostRecordTTL: v1alpha1.Duration{Duration: time.Hour}}}}
	data, err := json.Marshal(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := memory.Create(context.Background(), "Workspace", "team-a", "retention-policy", data); err != nil {
		t.Fatal(err)
	}
	costs := &recordingCostStore{deleted: 2}
	reconciler := NewCostRetentionReconciler(memory, costs, zaptest.NewLogger(t))
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	reconciler.now = func() time.Time { return now }
	result, err := reconciler.Reconcile(context.Background(), Request{Kind: "Workspace", Namespace: "team-a", Name: "retention-policy"})
	if err != nil {
		t.Fatal(err)
	}
	if costs.workspace != "team-a" || !costs.before.Equal(now.Add(-time.Hour)) || result.RequeueAfter != defaultCostRetentionInterval {
		t.Fatalf("workspace=%q before=%s result=%#v", costs.workspace, costs.before, result)
	}
}
