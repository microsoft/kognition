// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	"github.com/kognition-ai/kognition/internal/observability"
)

type pagedListClient struct {
	client.Client
	pages [][]corev1.ConfigMap
	calls int
}

type limitingListClient struct{ client.Client }

func (c *limitingListClient) List(ctx context.Context, list client.ObjectList, options ...client.ListOption) error {
	if err := c.Client.List(ctx, list, options...); err != nil {
		return err
	}
	configMaps := list.(*corev1.ConfigMapList)
	parsed := (&client.ListOptions{}).ApplyOptions(options)
	start := 0
	if parsed.Continue != "" {
		var err error
		start, err = strconv.Atoi(parsed.Continue)
		if err != nil {
			return err
		}
	}
	if start > len(configMaps.Items) {
		return fmt.Errorf("invalid continuation %d", start)
	}
	end := len(configMaps.Items)
	if parsed.Limit > 0 && end-start > int(parsed.Limit) {
		end = start + int(parsed.Limit)
	}
	if end < len(configMaps.Items) {
		configMaps.Continue = strconv.Itoa(end)
	} else {
		configMaps.Continue = ""
	}
	configMaps.Items = configMaps.Items[start:end]
	return nil
}

func (c *pagedListClient) List(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
	configMaps := list.(*corev1.ConfigMapList)
	if c.calls >= len(c.pages) {
		return nil
	}
	configMaps.Items = append(configMaps.Items, c.pages[c.calls]...)
	c.calls++
	if c.calls < len(c.pages) {
		configMaps.Continue = fmt.Sprintf("page-%d", c.calls)
	}
	return nil
}

func newTestStore(t *testing.T) *ConfigMapStore {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := k8s.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	store, err := NewConfigMapStore(fake.NewClientBuilder().WithScheme(scheme).Build(), "kognition-system")
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestConfigMapStore_AppendsImmutableBatchAndQueriesFilters(t *testing.T) {
	store := newTestStore(t)
	first := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	entries := []observability.AuditEntry{
		{ID: "b", Timestamp: first.Add(time.Minute), Workspace: "team-a", EventType: observability.AuditMemoryOp, Verb: "store"},
		{ID: "a", Timestamp: first, Workspace: "team-a", EventType: observability.AuditSpecChange, Verb: "create"},
	}
	if err := store.AppendBatch(context.Background(), entries); err != nil {
		t.Fatal(err)
	}

	queried, err := store.Query(context.Background(), observability.AuditFilter{Workspace: "team-a", Since: first, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(queried) != 2 || queried[0].ID != "a" || queried[1].ID != "b" {
		t.Fatalf("entries = %#v", queried)
	}
	filtered, err := store.Query(context.Background(), observability.AuditFilter{Workspace: "team-a", EventType: observability.AuditMemoryOp, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].Verb != "store" {
		t.Fatalf("filtered = %#v", filtered)
	}

	var configMaps corev1.ConfigMapList
	if err := store.client.List(context.Background(), &configMaps); err != nil {
		t.Fatal(err)
	}
	if len(configMaps.Items) != 1 || configMaps.Items[0].Immutable == nil || !*configMaps.Items[0].Immutable {
		t.Fatalf("ConfigMaps = %#v, want one immutable audit ConfigMap", configMaps.Items)
	}
}

func TestConfigMapStore_AppendIsIdempotentByEntryID(t *testing.T) {
	store := newTestStore(t)
	entry := observability.AuditEntry{ID: "stable-entry", Workspace: "team-a", EventType: observability.AuditGrantChange, Verb: "revoke"}
	if err := store.Append(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(context.Background(), entry); err != nil {
		t.Fatalf("duplicate append: %v", err)
	}
	var configMaps corev1.ConfigMapList
	if err := store.client.List(context.Background(), &configMaps); err != nil {
		t.Fatal(err)
	}
	if len(configMaps.Items) != 1 {
		t.Fatalf("ConfigMaps = %d, want one idempotent audit batch", len(configMaps.Items))
	}
}

func TestConfigMapStore_AppendIsIdempotentWithRegeneratedTimestamp(t *testing.T) {
	store := newTestStore(t)
	entry := observability.AuditEntry{ID: "stable-entry", Timestamp: time.Date(2026, 9, 10, 18, 0, 0, 0, time.UTC), Workspace: "team-a", EventType: observability.AuditGrantChange, Verb: "revoke"}
	if err := store.Append(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	entry.Timestamp = entry.Timestamp.Add(time.Second)
	if err := store.Append(context.Background(), entry); err != nil {
		t.Fatalf("duplicate append with regenerated timestamp: %v", err)
	}
}

func TestConfigMapStore_RejectsDuplicateIDWithDifferentCanonicalPayload(t *testing.T) {
	store := newTestStore(t)
	entry := observability.AuditEntry{ID: "stable-entry", Workspace: "team-a", EventType: observability.AuditReasoningStep, Details: json.RawMessage(`{"outcome":"succeeded"}`)}
	if err := store.Append(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	entry.Details = json.RawMessage(`{"outcome":"failed"}`)
	if err := store.Append(context.Background(), entry); err == nil || !strings.Contains(err.Error(), "different content") {
		t.Fatalf("conflicting retry error = %v, want different content", err)
	}
}

func TestConfigMapStore_RejectsIncompleteEntry(t *testing.T) {
	store := newTestStore(t)
	if err := store.Append(context.Background(), observability.AuditEntry{Workspace: "team-a"}); err == nil {
		t.Fatal("Append() error = nil, want validation error")
	}
}

func TestConfigMapStore_RejectsMixedWorkspaceBatch(t *testing.T) {
	store := newTestStore(t)
	err := store.AppendBatch(context.Background(), []observability.AuditEntry{
		{Workspace: "team-a", EventType: observability.AuditSpecChange},
		{Workspace: "team-b", EventType: observability.AuditSpecChange},
	})
	if err == nil {
		t.Fatal("AppendBatch() error = nil, want mixed-workspace rejection")
	}
}

func TestConfigMapStore_RejectsOversizedBatch(t *testing.T) {
	store := newTestStore(t)
	err := store.Append(context.Background(), observability.AuditEntry{
		Workspace: "team-a",
		EventType: observability.AuditSpecChange,
		Details:   []byte(`{"payload":"` + strings.Repeat("x", maxPayloadBytes) + `"}`),
	})
	if err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("Append() error = %v, want payload limit error", err)
	}
}

func TestConfigMapStore_QuerySeparatesWorkspacesAndHonorsLimit(t *testing.T) {
	store := newTestStore(t)
	first := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	for _, entry := range []observability.AuditEntry{
		{ID: "a", Timestamp: first, Workspace: "team-a", EventType: observability.AuditSpecChange},
		{ID: "b", Timestamp: first.Add(time.Minute), Workspace: "team-a", EventType: observability.AuditSpecChange},
		{ID: "c", Timestamp: first, Workspace: "team-b", EventType: observability.AuditSpecChange},
	} {
		if err := store.Append(context.Background(), entry); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := store.Query(context.Background(), observability.AuditFilter{Workspace: "team-a", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != "a" {
		t.Fatalf("team-a entries = %#v", entries)
	}
}

func TestConfigMapStore_QueryFiltersAgentSessionAndTimeRange(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	first := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	for _, entry := range []observability.AuditEntry{
		{ID: "before", Timestamp: first.Add(-time.Minute), Workspace: "team-a", AgentIdentity: "spiffe://planner", SessionID: "session-a", EventType: observability.AuditMemoryOp},
		{ID: "match", Timestamp: first, Workspace: "team-a", AgentIdentity: "spiffe://planner", SessionID: "session-a", EventType: observability.AuditMemoryOp},
		{ID: "wrong-session", Timestamp: first, Workspace: "team-a", AgentIdentity: "spiffe://planner", SessionID: "session-b", EventType: observability.AuditMemoryOp},
		{ID: "wrong-agent", Timestamp: first, Workspace: "team-a", AgentIdentity: "spiffe://writer", SessionID: "session-a", EventType: observability.AuditMemoryOp},
		{ID: "after", Timestamp: first.Add(time.Minute), Workspace: "team-a", AgentIdentity: "spiffe://planner", SessionID: "session-a", EventType: observability.AuditMemoryOp},
	} {
		if err := store.Append(ctx, entry); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := store.Query(ctx, observability.AuditFilter{
		Workspace: "team-a", AgentIdentity: "spiffe://planner", SessionID: "session-a", EventType: observability.AuditMemoryOp,
		Since: first, Until: first, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != "match" {
		t.Fatalf("filtered entries = %#v", entries)
	}
	if _, err := store.Query(ctx, observability.AuditFilter{Workspace: "team-a"}); err == nil {
		t.Fatal("query without a positive limit was accepted")
	}
}

func TestConfigMapStore_QueryFindsIdentityBeyondWorkspacePageLimit(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	for index := range queryPageSize*maxQueryPages + 1 {
		identity := "other-agent"
		if index == queryPageSize*maxQueryPages {
			identity = "target-agent"
		}
		if err := store.Append(ctx, observability.AuditEntry{
			ID: fmt.Sprintf("entry-%04d", index), Workspace: "team-a", AgentIdentity: identity,
			Timestamp: time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC), EventType: observability.AuditToolCall,
		}); err != nil {
			t.Fatal(err)
		}
	}
	store.client = &limitingListClient{Client: store.client}
	entries, err := store.Query(ctx, observability.AuditFilter{Workspace: "team-a", AgentIdentity: "target-agent", Limit: 10})
	if err != nil || len(entries) != 1 || entries[0].ID != "entry-1000" {
		t.Fatalf("Query() = %#v, %v; want entry-1000", entries, err)
	}
}

func TestConfigMapStore_QueryBackfillsMixedIdentityBatch(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if err := store.AppendBatch(ctx, []observability.AuditEntry{
		{ID: "first", Workspace: "team-a", AgentIdentity: "agent-a", EventType: observability.AuditToolCall},
		{ID: "second", Workspace: "team-a", AgentIdentity: "agent-b", EventType: observability.AuditToolCall},
	}); err != nil {
		t.Fatal(err)
	}
	var batches corev1.ConfigMapList
	if err := store.client.List(ctx, &batches, client.MatchingLabels{managedLabel: "true"}); err != nil {
		t.Fatal(err)
	}
	legacy := &batches.Items[0]
	payload := append([]byte(nil), legacy.BinaryData[entriesDataKey]...)
	delete(legacy.Labels, agentIdentityLabel("agent-a"))
	delete(legacy.Labels, agentIdentityLabel("agent-b"))
	if err := store.client.Update(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	store.client = &limitingListClient{Client: store.client}
	entries, err := store.Query(ctx, observability.AuditFilter{Workspace: "team-a", AgentIdentity: "agent-b", Limit: 10})
	if err != nil || len(entries) != 1 || entries[0].ID != "second" {
		t.Fatalf("legacy Query() = %#v, %v", entries, err)
	}
	if err := store.client.Get(ctx, client.ObjectKeyFromObject(legacy), legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Labels[agentIdentityLabel("agent-a")] != "true" || legacy.Labels[agentIdentityLabel("agent-b")] != "true" || string(legacy.BinaryData[entriesDataKey]) != string(payload) || legacy.Immutable == nil || !*legacy.Immutable {
		t.Fatalf("backfilled batch lost identities or immutable payload: %#v", legacy)
	}
	marker := &corev1.ConfigMap{}
	if err := store.client.Get(ctx, client.ObjectKey{Namespace: store.namespace, Name: indexMarkerPrefix + workspaceHash("team-a")}, marker); err != nil {
		t.Fatal(err)
	}
	if marker.Labels[managedLabel] != "true" || string(marker.BinaryData[entriesDataKey]) != "[]" {
		t.Fatalf("index marker is not a valid reserved audit ConfigMap: %#v", marker)
	}
	all, err := store.Query(ctx, observability.AuditFilter{Limit: 10})
	if err != nil || len(all) != 2 {
		t.Fatalf("unfiltered Query() = %#v, %v", all, err)
	}
}

// TC-0307: ConfigMap trust-decision queries match the PostgreSQL dashboard contract.
func TestTrustDecisionQueryMatchesDashboardContract(t *testing.T) {
	store := newTestStore(t)
	now := time.Date(2026, 8, 27, 18, 0, 0, 0, time.UTC)
	for _, fixture := range []struct {
		id      string
		at      time.Time
		plane   string
		outcome string
	}{
		{id: "a", at: now.Add(-time.Hour), plane: "tool", outcome: "allowed"},
		{id: "b", at: now.Add(-time.Minute), plane: "tool", outcome: "allowed"},
		{id: "c", at: now.Add(-time.Minute), plane: "tool", outcome: "allowed"},
		{id: "memory", at: now, plane: "memory", outcome: "allowed"},
		{id: "model", at: now, plane: "model", outcome: "allowed"},
		{id: "communication", at: now, plane: "communication", outcome: "revoked"},
		{id: "denied", at: now, plane: "tool", outcome: "denied"},
	} {
		entry := configMapTrustDecisionEntry(t, fixture.id, fixture.at, fixture.plane, fixture.outcome)
		if err := store.Append(context.Background(), entry); err != nil {
			t.Fatal(err)
		}
	}
	for index := range 3 {
		details := json.RawMessage(`{"plane":"tool","outcome":"allowed","recordID":"continuing-record"}`)
		entry := observability.AuditEntry{ID: fmt.Sprintf("generic-memory-%d", index), Timestamp: now.Add(time.Duration(index) * time.Second), Workspace: "team-a", AgentIdentity: "agent-a", SessionID: "session-a", EventType: observability.AuditMemoryOp, Details: details}
		if err := store.Append(context.Background(), entry); err != nil {
			t.Fatal(err)
		}
	}
	result, err := observability.QueryTrustDecisions(context.Background(), store, observability.TrustDecisionQuery{Namespace: "team-a", AgentIdentity: "agent-a", SessionID: "session-a", Plane: "tool", Outcome: "allowed", Limit: 2}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Items) != 2 || result.Items[0].ID != "c" || result.Items[1].ID != "b" || !result.Truncated {
		t.Fatalf("result = %#v", result)
	}
	for _, fixture := range []struct{ plane, outcome, id string }{{"model", "allowed", "model"}, {"communication", "revoked", "communication"}} {
		result, err := observability.QueryTrustDecisions(context.Background(), store, observability.TrustDecisionQuery{Namespace: "team-a", Plane: fixture.plane, Outcome: fixture.outcome}, now)
		if err != nil || len(result.Items) != 1 || result.Items[0].ID != fixture.id {
			t.Fatalf("%s result=%#v error=%v", fixture.plane, result, err)
		}
	}
}

func configMapTrustDecisionEntry(t *testing.T, id string, timestamp time.Time, plane, outcome string) observability.AuditEntry {
	t.Helper()
	eventType, kind := observability.AuditToolCall, "Tool"
	switch plane {
	case "memory":
		eventType, kind = observability.AuditMemoryOp, "MemoryVolume"
	case "model":
		eventType, kind = observability.AuditModelCall, "Model"
	case "communication":
		eventType, kind = observability.AuditCommunication, "AgentService"
	}
	decision := observability.TrustDecisionV1{SchemaVersion: 1, ID: id, Timestamp: timestamp, Plane: plane, Outcome: outcome, ReasonCode: "authorized", Workspace: "team-a", Request: observability.TrustDecisionRequest{Operation: "invoke", TargetRef: observability.TrustDecisionResourceRef{Kind: kind, Name: "target"}}, Authorization: observability.TrustDecisionAuthorization{Identity: "allowed", Role: "allowed", Grant: "allowed", Policy: "allowed"}, Execution: observability.TrustDecisionExecution{Status: "not_started"}}
	details, err := json.Marshal(decision)
	if err != nil {
		t.Fatal(err)
	}
	return observability.AuditEntry{ID: id, Timestamp: timestamp, Workspace: "team-a", AgentIdentity: "agent-a", SessionID: "session-a", EventType: eventType, Details: details}
}

func TestConfigMapStore_QueryRejectsMalformedAndExcessivePages(t *testing.T) {
	t.Run("malformed payload", func(t *testing.T) {
		store := newTestStore(t)
		configMap := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Namespace: "kognition-system", Name: "malformed", Labels: map[string]string{managedLabel: "true"}},
			BinaryData: map[string][]byte{entriesDataKey: []byte("{")},
		}
		if err := store.client.Create(context.Background(), configMap); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Query(context.Background(), observability.AuditFilter{Limit: 1}); err == nil || !strings.Contains(err.Error(), "decode") {
			t.Fatalf("Query() error = %v, want decode error", err)
		}
	})

	t.Run("bounded pages", func(t *testing.T) {
		scheme := runtime.NewScheme()
		if err := corev1.AddToScheme(scheme); err != nil {
			t.Fatal(err)
		}
		pages := make([][]corev1.ConfigMap, maxQueryPages+1)
		for index := range pages {
			payload, err := json.Marshal([]observability.AuditEntry{{ID: fmt.Sprintf("entry-%d", index), Timestamp: time.Now().UTC(), Workspace: "team-a", EventType: observability.AuditSpecChange}})
			if err != nil {
				t.Fatal(err)
			}
			pages[index] = []corev1.ConfigMap{{ObjectMeta: metav1.ObjectMeta{Namespace: "kognition-system", Name: fmt.Sprintf("query-%d", index)}, BinaryData: map[string][]byte{entriesDataKey: payload}}}
		}
		store := &ConfigMapStore{client: &pagedListClient{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), pages: pages}, namespace: "kognition-system"}
		if _, err := store.Query(context.Background(), observability.AuditFilter{Workspace: "team-a", Limit: 100}); !errors.Is(err, ErrQueryPageLimit) {
			t.Fatalf("Query() error = %v, want page limit error", err)
		}
	})
}

func TestConfigMapStore_DeletesExpiredWorkspaceBatches(t *testing.T) {
	store := newTestStore(t)
	old := time.Now().UTC().Add(-2 * time.Hour)
	if err := store.Append(context.Background(), observability.AuditEntry{Timestamp: old, Workspace: "team-a", EventType: observability.AuditSpecChange}); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(context.Background(), observability.AuditEntry{Timestamp: time.Now().UTC(), Workspace: "team-b", EventType: observability.AuditSpecChange}); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.DeleteAuditLogsBefore(context.Background(), "team-a", time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("deleted = %d, want 1", deleted)
	}
	entries, err := store.Query(context.Background(), observability.AuditFilter{Workspace: "team-b", Limit: 10})
	if err != nil || len(entries) != 1 {
		t.Fatalf("team-b entries = %#v, err=%v", entries, err)
	}
}

func TestConfigMapStore_RetentionKeepsMixedAgeImmutableBatch(t *testing.T) {
	store := newTestStore(t)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	if err := store.AppendBatch(context.Background(), []observability.AuditEntry{
		{ID: "expired", Timestamp: now.Add(-2 * time.Hour), Workspace: "team-a", EventType: observability.AuditSpecChange},
		{ID: "current", Timestamp: now, Workspace: "team-a", EventType: observability.AuditSpecChange},
	}); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.DeleteAuditLogsBefore(context.Background(), "team-a", now.Add(-time.Hour))
	if err != nil || deleted != 0 {
		t.Fatalf("DeleteAuditLogsBefore() = (%d, %v), want (0, nil)", deleted, err)
	}
	entries, err := store.Query(context.Background(), observability.AuditFilter{Workspace: "team-a", Limit: 10})
	if err != nil || len(entries) != 2 {
		t.Fatalf("retained entries = %#v, err=%v", entries, err)
	}
}

func TestConfigMapStore_RetentionDoesNotDeleteHashLabelCollision(t *testing.T) {
	store := newTestStore(t)
	payload, err := json.Marshal([]observability.AuditEntry{{Timestamp: time.Now().UTC().Add(-2 * time.Hour), Workspace: "team-b", EventType: observability.AuditSpecChange}})
	if err != nil {
		t.Fatal(err)
	}
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kognition-system", Name: "collision", Labels: map[string]string{managedLabel: "true", workspaceHashLabel: workspaceHash("team-a")}},
		BinaryData: map[string][]byte{entriesDataKey: payload},
	}
	if err := store.client.Create(context.Background(), configMap); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.DeleteAuditLogsBefore(context.Background(), "team-a", time.Now().UTC().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Fatalf("deleted = %d, want 0", deleted)
	}
	if err := store.client.Get(context.Background(), client.ObjectKeyFromObject(configMap), &corev1.ConfigMap{}); err != nil {
		t.Fatalf("colliding workspace ConfigMap was deleted: %v", err)
	}
}

func TestConfigMapStore_RetentionBoundsPagedDeletion(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	pages := make([][]corev1.ConfigMap, maxRetentionPages+1)
	objects := make([]client.Object, 0, len(pages))
	for index := range pages {
		payload, err := json.Marshal([]observability.AuditEntry{{Timestamp: time.Now().UTC().Add(-2 * time.Hour), Workspace: "team-a", EventType: observability.AuditSpecChange}})
		if err != nil {
			t.Fatal(err)
		}
		configMap := corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "kognition-system", Name: fmt.Sprintf("audit-%d", index)}, BinaryData: map[string][]byte{entriesDataKey: payload}}
		pages[index] = []corev1.ConfigMap{configMap}
		objects = append(objects, configMap.DeepCopy())
	}
	store := &ConfigMapStore{client: &pagedListClient{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(), pages: pages}, namespace: "kognition-system"}
	deleted, err := store.DeleteAuditLogsBefore(context.Background(), "team-a", time.Now().UTC().Add(-time.Hour))
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("DeleteAuditLogsBefore() error = %v, want page limit error", err)
	}
	if deleted != maxRetentionPages {
		t.Fatalf("deleted = %d, want %d", deleted, maxRetentionPages)
	}
}

func TestRetentionRunnable_DeletesEntriesUsingWorkspaceAuditTTL(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := k8s.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	workspace := &k8s.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "workspace-policy", Namespace: "team-a"}, Spec: k8s.WorkspaceSpec{Retention: &k8s.RetentionPolicy{AuditLogTTL: "1h"}}}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(workspace).Build()
	store, err := NewConfigMapStore(client, "kognition-system")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	if err := store.Append(context.Background(), observability.AuditEntry{Timestamp: now.Add(-2 * time.Hour), Workspace: "team-a", EventType: observability.AuditSpecChange}); err != nil {
		t.Fatal(err)
	}
	runnable := NewRetentionRunnable(client, store, nil, time.Hour)
	runnable.now = func() time.Time { return now }
	runnable.sweep(context.Background())
	entries, err := store.Query(context.Background(), observability.AuditFilter{Workspace: "team-a", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("retained entries = %#v", entries)
	}
}
