// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cost

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	"github.com/kognition-ai/kognition/internal/observability"
)

type injectUsageEventClient struct {
	client.Client
	once      sync.Once
	namespace string
	event     observability.CostRecord
	err       error
}

func (c *injectUsageEventClient) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, ok := object.(*k8s.CognitiveQuota); ok && key.Namespace == c.event.Workspace {
		c.once.Do(func() {
			payload, err := json.Marshal([]observability.CostRecord{c.event})
			if err != nil {
				c.err = err
				return
			}
			if err := c.Client.Create(ctx, usageEventConfigMap(c.namespace, c.event, payload)); err != nil {
				c.err = err
				return
			}
			var quota k8s.CognitiveQuota
			if err := c.Client.Get(ctx, key, &quota); err != nil {
				c.err = err
				return
			}
			quota.Status.CostUsage = &k8s.QuotaCostUsage{TotalCost: 100, ModelTierSeconds: map[string]int64{"premium": 2}, ModelTierMilliseconds: map[string]int64{"premium": 2000}}
			c.err = c.Client.Status().Update(ctx, &quota)
		})
		if c.err != nil {
			return c.err
		}
	}
	return c.Client.Get(ctx, key, object, options...)
}

func newTestStore(t *testing.T) *ConfigMapStore {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := coordinationv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := k8s.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	store, err := NewConfigMapStore(fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&k8s.CognitiveQuota{}).Build(), "kognition-system")
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestConfigMapStoreAppliesUsageEventAndRepairsProjectionIdempotently(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	quota := &k8s.CognitiveQuota{
		TypeMeta:   metav1.TypeMeta{APIVersion: "kognition.dev/v1alpha1", Kind: "CognitiveQuota"},
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "team-a"},
		Spec: k8s.CognitiveQuotaSpec{
			Hard:        map[string]int64{},
			TokenBudget: &k8s.TokenBudget{MaxTotalTokens: 1000},
			CostBudget:  &k8s.CostBudget{MaxCostPerPeriod: 1000, MaxModelTierSeconds: map[string]int64{"premium": 60}},
		},
		Status: k8s.CognitiveQuotaStatus{LastResetTime: "2026-08-25T13:00:00Z"},
	}
	if err := store.client.Create(ctx, quota); err != nil {
		t.Fatal(err)
	}
	event := observability.CostRecord{
		ID: "usage-1", Timestamp: time.Date(2026, 8, 25, 14, 0, 0, 0, time.UTC),
		Workspace: "team-a", AgentIdentity: "spiffe://planner", SessionID: "session-1", ReservationID: "agentinstance:instance-1",
		OperationType: observability.CostOpModelInference, Purpose: observability.UsagePurposeReasoningProbe, ModelName: "gpt", ModelProvider: "openai-compatible", ModelClass: "frontier-v1", ModelClassVersion: "2026-08-01", ModelCostTier: "premium",
		PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12, UsageEstimated: true, PricingConfigured: true,
		InputCostMicrousdPerMillionTokens: 2500000, OutputCostMicrousdPerMillionTokens: 10000000,
		CostUnits: 45, ProviderDurationMillis: 1250, TraceID: "trace-1", SpanID: "span-1",
	}
	applied, err := store.ApplyUsageEvent(ctx, event)
	if err != nil || !applied {
		t.Fatalf("first ApplyUsageEvent() = (%t, %v), want (true, nil)", applied, err)
	}
	applied, err = store.ApplyUsageEvent(ctx, event)
	if err != nil || applied {
		t.Fatalf("retry ApplyUsageEvent() = (%t, %v), want (false, nil)", applied, err)
	}
	historical := event
	historical.ID, historical.TraceID, historical.SpanID = "usage-before-reset", "trace-before-reset", "span-before-reset"
	historical.Timestamp = time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	historical.CostUnits = 100
	if applied, err = store.ApplyUsageEvent(ctx, historical); err != nil || !applied {
		t.Fatalf("historical ApplyUsageEvent() = (%t, %v), want (true, nil)", applied, err)
	}

	if err := store.client.Get(ctx, client.ObjectKeyFromObject(quota), quota); err != nil {
		t.Fatal(err)
	}
	if quota.Status.CostUsage == nil || quota.Status.CostUsage.TotalCost != 45 || quota.Status.CostUsage.ModelTierSeconds["premium"] != 2 {
		t.Fatalf("cost usage = %#v, want 45 micro-USD and 2 premium seconds", quota.Status.CostUsage)
	}
	if quota.Status.TokenUsage != nil {
		t.Fatalf("token reservation projection was double charged: %#v", quota.Status.TokenUsage)
	}
	var records corev1.ConfigMapList
	if err := store.client.List(ctx, &records, client.InNamespace("kognition-system"), client.MatchingLabels{managedLabel: "true"}); err != nil {
		t.Fatal(err)
	}
	if len(records.Items) != 2 {
		t.Fatalf("immutable usage event ConfigMaps = %d, want 2", len(records.Items))
	}
	for index := range records.Items {
		stored, err := decodeRecords(&records.Items[index])
		if err != nil || len(stored) != 1 || stored[0].Purpose != observability.UsagePurposeReasoningProbe || !stored[0].UsageEstimated {
			t.Fatalf("persisted usage classification = (%+v, %v), want estimated %q", stored, err, observability.UsagePurposeReasoningProbe)
		}
	}
}

func TestConfigMapStoreRejectsInvalidAndConflictingUsageEvents(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	if _, err := store.ApplyUsageEvent(ctx, observability.CostRecord{ID: "tool-1", Workspace: "team-a", OperationType: observability.CostOpToolExecution}); err == nil {
		t.Fatal("non-model usage event was accepted")
	}
	if _, err := store.ApplyUsageEvent(ctx, observability.CostRecord{Workspace: "team-a", OperationType: observability.CostOpModelInference}); err == nil {
		t.Fatal("usage event without idempotency ID was accepted")
	}
	if _, err := store.ApplyUsageEvent(ctx, observability.CostRecord{ID: "inconsistent", Workspace: "team-a", OperationType: observability.CostOpModelInference, PromptTokens: 2, CompletionTokens: 1, TotalTokens: 1}); err == nil {
		t.Fatal("usage event with inconsistent total tokens was accepted")
	}
	if _, err := store.ApplyUsageEvent(ctx, observability.CostRecord{ID: "unknown-purpose", Workspace: "team-a", OperationType: observability.CostOpModelInference, Purpose: "unknown"}); err == nil {
		t.Fatal("usage event with unknown purpose was accepted")
	}

	event := observability.CostRecord{
		ID: "usage-conflict", Timestamp: time.Date(2026, 8, 25, 14, 0, 0, 0, time.UTC), Workspace: "team-a",
		OperationType: observability.CostOpModelInference, Purpose: observability.UsagePurposeReasoningProbe, CostUnits: 45, TraceID: "trace-conflict", SpanID: "span-conflict",
	}
	if applied, err := store.ApplyUsageEvent(ctx, event); err != nil || !applied {
		t.Fatalf("first ApplyUsageEvent() = (%t, %v), want (true, nil)", applied, err)
	}
	conflict := event
	conflict.Purpose = ""
	if _, err := store.ApplyUsageEvent(ctx, conflict); err == nil {
		t.Fatal("usage event ID reuse with different purpose was accepted")
	}
	conflict = event
	conflict.UsageEstimated = true
	if _, err := store.ApplyUsageEvent(ctx, conflict); err == nil {
		t.Fatal("usage event ID reuse with different estimated state was accepted")
	}
	total, err := store.GetTotalCost(ctx, "team-a", time.Time{}, time.Time{})
	if err != nil || total != 45 {
		t.Fatalf("cost after conflicting retry = (%d, %v), want (45, nil)", total, err)
	}
	deleted, err := store.DeleteCostRecordsBefore(ctx, "team-a", event.Timestamp.Add(time.Hour))
	if err != nil || deleted != 1 {
		t.Fatalf("usage event retention = (%d, %v), want (1, nil)", deleted, err)
	}
	if applied, err := store.ApplyUsageEvent(ctx, event); err != nil || applied {
		t.Fatalf("retry after retention = (%t, %v), want (false, nil)", applied, err)
	}
}

func TestConfigMapStoreRecoversOrRejectsPreCreatedUsageEvent(t *testing.T) {
	for _, test := range []struct {
		name        string
		storedUnits int64
		wantErr     bool
	}{
		{name: "matching event is completed", storedUnits: 45},
		{name: "different event payload is rejected", storedUnits: 90, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newTestStore(t)
			ctx := context.Background()
			quota := &k8s.CognitiveQuota{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "team-a"}, Spec: k8s.CognitiveQuotaSpec{Hard: map[string]int64{}, CostBudget: &k8s.CostBudget{MaxCostPerPeriod: 1000}}}
			if err := store.client.Create(ctx, quota); err != nil {
				t.Fatal(err)
			}
			event := observability.CostRecord{ID: "usage-precreated", Timestamp: time.Now().UTC(), Workspace: "team-a", OperationType: observability.CostOpModelInference, CostUnits: 45}
			stored := event
			stored.CostUnits = observability.CostUnit(test.storedUnits)
			payload, err := json.Marshal([]observability.CostRecord{stored})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.client.Create(ctx, usageEventConfigMap("kognition-system", event, payload)); err != nil {
				t.Fatal(err)
			}
			applied, err := store.ApplyUsageEvent(ctx, event)
			if test.wantErr {
				if err == nil || applied {
					t.Fatalf("pre-created mismatch = (%t, %v), want error", applied, err)
				}
				return
			}
			if err != nil || !applied {
				t.Fatalf("pre-created recovery = (%t, %v), want (true, nil)", applied, err)
			}
			fingerprint := fmt.Sprintf("%x", sha256.Sum256(payload))
			var receipt corev1.ConfigMap
			if err := store.client.Get(ctx, client.ObjectKeyFromObject(usageEventReceiptConfigMap("kognition-system", event, fingerprint)), &receipt); err != nil {
				t.Fatalf("load recovered usage receipt: %v", err)
			}
			if receipt.Data[fingerprintDataKey] != fingerprint {
				t.Fatalf("receipt fingerprint = %q, want %q", receipt.Data[fingerprintDataKey], fingerprint)
			}
			if applied, err := store.ApplyUsageEvent(ctx, event); err != nil || applied {
				t.Fatalf("recovered event retry = (%t, %v), want (false, nil)", applied, err)
			}
			if err := store.client.Get(ctx, client.ObjectKeyFromObject(quota), quota); err != nil {
				t.Fatal(err)
			}
			if quota.Status.CostUsage == nil || quota.Status.CostUsage.TotalCost != 45 {
				t.Fatalf("recovered usage = %#v", quota.Status.CostUsage)
			}
		})
	}
}

func TestConfigMapStoreProjectionCannotOverwriteConcurrentUsageEvent(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	quota := &k8s.CognitiveQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "team-a"},
		Spec:       k8s.CognitiveQuotaSpec{Hard: map[string]int64{}, CostBudget: &k8s.CostBudget{MaxCostPerPeriod: 1000}},
	}
	if err := store.client.Create(ctx, quota); err != nil {
		t.Fatal(err)
	}
	first := observability.CostRecord{
		ID: "usage-first", Timestamp: time.Date(2026, 8, 25, 14, 0, 0, 0, time.UTC), Workspace: "team-a",
		OperationType: observability.CostOpModelInference, ModelCostTier: "premium", CostUnits: 45, ProviderDurationMillis: 1000,
		TraceID: "trace-first", SpanID: "span-first",
	}
	concurrent := first
	concurrent.ID, concurrent.TraceID, concurrent.SpanID = "usage-concurrent", "trace-concurrent", "span-concurrent"
	concurrent.CostUnits, concurrent.ProviderDurationMillis = 100, 2000
	injector := &injectUsageEventClient{Client: store.client, namespace: "kognition-system", event: concurrent}
	store.client = injector

	if applied, err := store.ApplyUsageEvent(ctx, first); err != nil || !applied {
		t.Fatalf("ApplyUsageEvent() = (%t, %v), want (true, nil)", applied, err)
	}
	if err := store.client.Get(ctx, client.ObjectKeyFromObject(quota), quota); err != nil {
		t.Fatal(err)
	}
	if quota.Status.CostUsage == nil || quota.Status.CostUsage.TotalCost != 145 || quota.Status.CostUsage.ModelTierSeconds["premium"] != 3 {
		t.Fatalf("concurrent projection = %#v, want 145 micro-USD and 3 premium seconds", quota.Status.CostUsage)
	}
}

func TestConfigMapStoreConcurrentSameEventProjectsExactlyOnce(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	quota := &k8s.CognitiveQuota{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "team-a"}, Spec: k8s.CognitiveQuotaSpec{Hard: map[string]int64{}, CostBudget: &k8s.CostBudget{MaxCostPerPeriod: 1000}}}
	if err := store.client.Create(ctx, quota); err != nil {
		t.Fatal(err)
	}
	event := observability.CostRecord{ID: "usage-concurrent-same", Timestamp: time.Now().UTC(), Workspace: "team-a", OperationType: observability.CostOpModelInference, ModelCostTier: "premium", CostUnits: 45, ProviderDurationMillis: 1250}
	results := make(chan bool, 2)
	errors := make(chan error, 2)
	var wait sync.WaitGroup
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			applied, err := store.ApplyUsageEvent(ctx, event)
			results <- applied
			errors <- err
		}()
	}
	wait.Wait()
	close(results)
	close(errors)
	appliedCount := 0
	for applied := range results {
		if applied {
			appliedCount++
		}
	}
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	if appliedCount != 1 {
		t.Fatalf("newly applied results = %d, want 1", appliedCount)
	}
	if err := store.client.Get(ctx, client.ObjectKeyFromObject(quota), quota); err != nil {
		t.Fatal(err)
	}
	if quota.Status.CostUsage == nil || quota.Status.CostUsage.TotalCost != 45 || quota.Status.CostUsage.ModelTierSeconds["premium"] != 2 || quota.Status.CostUsage.ModelTierMilliseconds["premium"] != 1250 {
		t.Fatalf("concurrent same-event usage = %#v", quota.Status.CostUsage)
	}
}

func TestConfigMapStoreWaitsForDeadUsageLeaseThenPersists(t *testing.T) {
	store := newTestStore(t)
	workspace := "team-a"
	quota := &k8s.CognitiveQuota{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: workspace}, Spec: k8s.CognitiveQuotaSpec{Hard: map[string]int64{}, CostBudget: &k8s.CostBudget{MaxCostPerPeriod: 1000}}}
	if err := store.client.Create(context.Background(), quota); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(workspace))
	holder := "dead-holder"
	duration := int32(15)
	renewed := metav1.NewMicroTime(time.Now().UTC().Add(-9 * time.Second))
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kognition-system", Name: "kognition-usage-lock-" + fmt.Sprintf("%x", digest[:12])},
		Spec:       coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &duration, RenewTime: &renewed},
	}
	if err := store.client.Create(context.Background(), lease); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	event := observability.CostRecord{ID: "usage-after-dead-holder", Timestamp: time.Now().UTC(), Workspace: workspace, OperationType: observability.CostOpModelInference, CostUnits: 45}
	started := time.Now()
	applied, err := store.ApplyUsageEvent(ctx, event)
	if err != nil || !applied {
		t.Fatalf("ApplyUsageEvent() = (%t, %v), want recovery", applied, err)
	}
	if elapsed := time.Since(started); elapsed < 5*time.Second || elapsed >= 10*time.Second {
		t.Fatalf("dead-holder recovery took %s", elapsed)
	}
	if err := store.client.Get(context.Background(), client.ObjectKeyFromObject(quota), quota); err != nil {
		t.Fatal(err)
	}
	if quota.Status.CostUsage == nil || quota.Status.CostUsage.TotalCost != 45 {
		t.Fatalf("recovered usage = %#v", quota.Status.CostUsage)
	}
}

func TestConfigMapStoreLostUsageLeaseFencesProjection(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	quota := &k8s.CognitiveQuota{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "team-a"}, Spec: k8s.CognitiveQuotaSpec{Hard: map[string]int64{}, CostBudget: &k8s.CostBudget{MaxCostPerPeriod: 1000}}}
	if err := store.client.Create(ctx, quota); err != nil {
		t.Fatal(err)
	}
	lockCtx, release, err := store.acquireUsageLock(ctx, "team-a")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	identity := lockCtx.Value(usageLockContextKey{}).(usageLockIdentity)
	lease := &coordinationv1.Lease{}
	if err := store.client.Get(ctx, identity.key, lease); err != nil {
		t.Fatal(err)
	}
	intruder := "intruder"
	lease.Spec.HolderIdentity = &intruder
	if err := store.client.Update(ctx, lease); err != nil {
		t.Fatal(err)
	}
	event := observability.CostRecord{ID: "usage-fenced", Timestamp: time.Now().UTC(), Workspace: "team-a", OperationType: observability.CostOpModelInference, CostUnits: 45}
	if err := store.applyEventToQuotaProjection(lockCtx, client.ObjectKeyFromObject(quota), event); err == nil {
		t.Fatal("projection succeeded after usage Lease ownership was lost")
	}
	if err := store.client.Get(ctx, client.ObjectKeyFromObject(quota), quota); err != nil {
		t.Fatal(err)
	}
	if quota.Status.CostUsage != nil {
		t.Fatalf("fenced projection updated quota: %#v", quota.Status.CostUsage)
	}
}

func TestConfigMapStoreRejectsForgedUsageLedgerAndRepairsOnRetry(t *testing.T) {
	for _, test := range []struct {
		name   string
		record observability.CostRecord
	}{
		{name: "wrong workspace", record: observability.CostRecord{ID: "forged-workspace", Workspace: "team-b", OperationType: observability.CostOpModelInference, CostUnits: 100}},
		{name: "negative cost", record: observability.CostRecord{ID: "forged-negative", Workspace: "team-a", OperationType: observability.CostOpModelInference, CostUnits: -100}},
		{name: "unreceipted positive cost", record: observability.CostRecord{ID: "forged-positive", Workspace: "team-a", OperationType: observability.CostOpModelInference, CostUnits: 100}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newTestStore(t)
			ctx := context.Background()
			quota := &k8s.CognitiveQuota{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "team-a"}, Spec: k8s.CognitiveQuotaSpec{Hard: map[string]int64{}, CostBudget: &k8s.CostBudget{MaxCostPerPeriod: 1000}}}
			if err := store.client.Create(ctx, quota); err != nil {
				t.Fatal(err)
			}
			payload, err := json.Marshal([]observability.CostRecord{test.record})
			if err != nil {
				t.Fatal(err)
			}
			forged := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "kognition-cost-forged", Namespace: "kognition-system", Labels: map[string]string{managedLabel: "true", usageEventLabel: "true", workspaceHashLabel: workspaceHash("team-a")}}, BinaryData: map[string][]byte{recordsDataKey: payload}, Immutable: boolPtr(true)}
			if err := store.client.Create(ctx, forged); err != nil {
				t.Fatal(err)
			}
			event := observability.CostRecord{ID: "usage-valid", Timestamp: time.Now().UTC(), Workspace: "team-a", OperationType: observability.CostOpModelInference, CostUnits: 45}
			if applied, err := store.ApplyUsageEvent(ctx, event); err != nil || !applied {
				t.Fatalf("first incremental projection = (%t, %v), want (true, nil)", applied, err)
			}
			if _, err := store.ApplyUsageEvent(ctx, event); err == nil {
				t.Fatal("forged ledger payload did not fail retry repair")
			}
			if err := store.client.Delete(ctx, forged); err != nil {
				t.Fatal(err)
			}
			if applied, err := store.ApplyUsageEvent(ctx, event); err != nil || applied {
				t.Fatalf("repair retry = (%t, %v), want (false, nil)", applied, err)
			}
			if err := store.client.Get(ctx, client.ObjectKeyFromObject(quota), quota); err != nil {
				t.Fatal(err)
			}
			if quota.Status.CostUsage == nil || quota.Status.CostUsage.TotalCost != 45 {
				t.Fatalf("repaired usage = %#v", quota.Status.CostUsage)
			}
		})
	}
}

func TestConfigMapStoreRebuildsLegacyModelRecordsWithReceiptBoundEvents(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	quota := &k8s.CognitiveQuota{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "team-a"}, Spec: k8s.CognitiveQuotaSpec{Hard: map[string]int64{}, CostBudget: &k8s.CostBudget{MaxCostPerPeriod: 1000}}}
	if err := store.client.Create(ctx, quota); err != nil {
		t.Fatal(err)
	}
	legacy := observability.CostRecord{Timestamp: time.Now().UTC(), Workspace: "team-a", OperationType: observability.CostOpModelInference, CostUnits: 100, TraceID: "legacy-trace", SpanID: "legacy-span"}
	legacyPayload, err := json.Marshal([]observability.CostRecord{legacy})
	if err != nil {
		t.Fatal(err)
	}
	legacyConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: configMapPrefix + recordName(legacy), Namespace: "kognition-system", Labels: map[string]string{managedLabel: "true", workspaceHashLabel: workspaceHash(legacy.Workspace)}},
		BinaryData: map[string][]byte{recordsDataKey: legacyPayload},
		Immutable:  boolPtr(true),
	}
	if err := store.client.Create(ctx, legacyConfigMap); err != nil {
		t.Fatal(err)
	}
	event := observability.CostRecord{ID: "usage-current", Timestamp: time.Now().UTC(), Workspace: "team-a", OperationType: observability.CostOpModelInference, CostUnits: 45}
	if applied, err := store.ApplyUsageEvent(ctx, event); err != nil || !applied {
		t.Fatalf("first ApplyUsageEvent() = (%t, %v), want (true, nil)", applied, err)
	}
	if applied, err := store.ApplyUsageEvent(ctx, event); err != nil || applied {
		t.Fatalf("repair retry = (%t, %v), want (false, nil)", applied, err)
	}
	if err := store.client.Get(ctx, client.ObjectKeyFromObject(quota), quota); err != nil {
		t.Fatal(err)
	}
	if quota.Status.CostUsage == nil || quota.Status.CostUsage.TotalCost != 145 {
		t.Fatalf("repaired usage = %#v, want legacy and current cost", quota.Status.CostUsage)
	}
}

func TestConfigMapStoreRejectsInvalidLegacyModelRecords(t *testing.T) {
	for _, test := range []struct {
		name      string
		record    observability.CostRecord
		immutable *bool
	}{
		{name: "post-T-0111 ID", record: observability.CostRecord{ID: "new-event", Workspace: "team-a", OperationType: observability.CostOpModelInference, TraceID: "trace", SpanID: "span"}, immutable: boolPtr(true)},
		{name: "mutable legacy record", record: observability.CostRecord{Workspace: "team-a", OperationType: observability.CostOpModelInference, TraceID: "trace", SpanID: "span"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newTestStore(t)
			payload, err := json.Marshal([]observability.CostRecord{test.record})
			if err != nil {
				t.Fatal(err)
			}
			configMap := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: configMapPrefix + recordName(test.record), Namespace: "kognition-system", Labels: map[string]string{managedLabel: "true", workspaceHashLabel: workspaceHash("team-a")}},
				BinaryData: map[string][]byte{recordsDataKey: payload},
				Immutable:  test.immutable,
			}
			if err := store.client.Create(context.Background(), configMap); err != nil {
				t.Fatal(err)
			}
			if _, err := store.workspaceRecords(context.Background(), "team-a"); err == nil {
				t.Fatal("invalid legacy model record was accepted")
			}
		})
	}
}

func TestConfigMapStoreDoesNotBackChargeUsageBeforeQuotaCreation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	createdAt := time.Date(2026, 8, 25, 14, 0, 0, 0, time.UTC)
	quota := &k8s.CognitiveQuota{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "team-a", CreationTimestamp: metav1.NewTime(createdAt)}, Spec: k8s.CognitiveQuotaSpec{Hard: map[string]int64{}, CostBudget: &k8s.CostBudget{MaxCostPerPeriod: 1000}}}
	if err := store.client.Create(ctx, quota); err != nil {
		t.Fatal(err)
	}
	historical := observability.CostRecord{ID: "usage-historical", Timestamp: createdAt.Add(-time.Hour), Workspace: "team-a", OperationType: observability.CostOpModelInference, CostUnits: 100}
	current := observability.CostRecord{ID: "usage-current", Timestamp: createdAt.Add(time.Hour), Workspace: "team-a", OperationType: observability.CostOpModelInference, CostUnits: 45}
	if _, err := store.ApplyUsageEvent(ctx, historical); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyUsageEvent(ctx, current); err != nil {
		t.Fatal(err)
	}
	if err := store.client.Get(ctx, client.ObjectKeyFromObject(quota), quota); err != nil {
		t.Fatal(err)
	}
	if quota.Status.CostUsage == nil || quota.Status.CostUsage.TotalCost != 45 {
		t.Fatalf("cost usage = %#v, want only post-creation cost", quota.Status.CostUsage)
	}
}

func TestConfigMapStoreRoundsAggregateModelTierDurationOnce(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	quota := &k8s.CognitiveQuota{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "team-a"}, Spec: k8s.CognitiveQuotaSpec{Hard: map[string]int64{}, CostBudget: &k8s.CostBudget{MaxModelTierSeconds: map[string]int64{"premium": 60}}}}
	if err := store.client.Create(ctx, quota); err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= 2; index++ {
		event := observability.CostRecord{ID: fmt.Sprintf("usage-%d", index), Timestamp: time.Now().UTC(), Workspace: "team-a", OperationType: observability.CostOpModelInference, ModelCostTier: "premium", ProviderDurationMillis: 1}
		if _, err := store.ApplyUsageEvent(ctx, event); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.client.Get(ctx, client.ObjectKeyFromObject(quota), quota); err != nil {
		t.Fatal(err)
	}
	if quota.Status.CostUsage == nil || quota.Status.CostUsage.ModelTierSeconds["premium"] != 1 || quota.Status.CostUsage.ModelTierMilliseconds["premium"] != 2 {
		t.Fatalf("tier usage = %#v, want one aggregate second", quota.Status.CostUsage)
	}
}

func costRecord(workspace, agent, traceID, spanID string, timestamp time.Time, units int64) observability.CostRecord {
	return observability.CostRecord{
		Timestamp: timestamp, Workspace: workspace, AgentIdentity: agent, AgentNamespace: workspace,
		Solution: "research", SolutionRev: 2, SessionID: "session-1", OperationType: observability.CostOpModelInference,
		ModelName: "gpt", PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12, CostUnits: observability.CostUnit(units), TraceID: traceID, SpanID: spanID,
	}
}

func TestConfigMapStoreRecordsIdempotentlyAndAggregates(t *testing.T) {
	store := newTestStore(t)
	first := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	for _, record := range []observability.CostRecord{
		costRecord("team-a", "spiffe://planner", "trace-1", "span-1", first, 45),
		costRecord("team-a", "spiffe://writer", "trace-2", "span-2", first.Add(time.Hour), 50),
		costRecord("team-b", "spiffe://planner", "trace-3", "span-3", first, 7),
	} {
		if err := store.RecordCost(context.Background(), record); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.RecordCost(context.Background(), costRecord("team-a", "spiffe://planner", "trace-1", "span-1", first, 45)); err != nil {
		t.Fatal(err)
	}
	result, err := store.QueryCosts(context.Background(), observability.CostQuery{Workspace: "team-a", GroupBy: []observability.CostGroupBy{observability.GroupByAgent}})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalCount != 2 || len(result.Aggregations) != 2 || result.Aggregations[0].AgentIdentity != "spiffe://planner" || result.Aggregations[0].TotalCostUnits != 45 || result.Aggregations[1].AgentIdentity != "spiffe://writer" || result.Aggregations[1].TotalCostUnits != 50 {
		t.Fatalf("aggregations = %#v", result)
	}
	total, err := store.GetTotalCost(context.Background(), "team-a", time.Time{}, time.Time{})
	if err != nil || total != 95 {
		t.Fatalf("GetTotalCost() = (%d, %v), want (95, nil)", total, err)
	}
}

func TestConfigMapStoreValidatesConstructionAndPersistenceInputs(t *testing.T) {
	if _, err := NewConfigMapStore(nil, "kognition-system"); err == nil {
		t.Fatal("nil Kubernetes client was accepted")
	}
	if _, err := NewConfigMapStore(newTestStore(t).client, ""); err == nil {
		t.Fatal("empty Kubernetes namespace was accepted")
	}
	store := newTestStore(t)
	if _, err := NewConfigMapStore(store.client, "kognition-system"); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordCostBatch(context.Background(), nil); err != nil {
		t.Fatalf("empty batch = %v", err)
	}
	oversized := costRecord("team-a", "spiffe://planner", "trace-large", "span-large", time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC), 1)
	oversized.ToolName = strings.Repeat("x", maxPayloadBytes)
	if err := store.RecordCostBatch(context.Background(), []observability.CostRecord{oversized}); err == nil {
		t.Fatal("oversized cost batch was accepted")
	}
	if _, err := store.QueryCosts(context.Background(), observability.CostQuery{Workspace: "team-a", Limit: -1}); err == nil {
		t.Fatal("negative query limit was accepted")
	}
	if _, err := store.QueryCosts(context.Background(), observability.CostQuery{Workspace: "team-a", Offset: -1}); err == nil {
		t.Fatal("negative query offset was accepted")
	}
	total, err := store.GetTotalCost(context.Background(), "team-a", time.Time{}, time.Time{})
	if err != nil || total != 0 {
		t.Fatalf("empty total = (%d, %v), want (0, nil)", total, err)
	}
}

func TestConfigMapStoreSchemeRegistersCostDependencies(t *testing.T) {
	scheme, err := newConfigMapStoreScheme()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		kind string
		gvk  schema.GroupVersionKind
	}{
		{kind: "ConfigMap", gvk: corev1.SchemeGroupVersion.WithKind("ConfigMap")},
		{kind: "Lease", gvk: coordinationv1.SchemeGroupVersion.WithKind("Lease")},
		{kind: "CognitiveQuota", gvk: k8s.GroupVersion.WithKind("CognitiveQuota")},
		{kind: "Workspace", gvk: k8s.GroupVersion.WithKind("Workspace")},
	} {
		if _, err := scheme.New(test.gvk); err != nil {
			t.Fatalf("scheme does not register %s: %v", test.kind, err)
		}
	}
}

func TestConfigMapStoreRejectsMalformedPersistedRecords(t *testing.T) {
	store := newTestStore(t)
	malformed := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kognition-cost-malformed", Namespace: "kognition-system", Labels: map[string]string{managedLabel: "true", workspaceHashLabel: workspaceHash("team-a")}},
		BinaryData: map[string][]byte{recordsDataKey: []byte("not-json")},
		Immutable:  boolPtr(true),
	}
	if err := store.client.Create(context.Background(), malformed); err != nil {
		t.Fatal(err)
	}
	if _, err := store.QueryCosts(context.Background(), observability.CostQuery{Workspace: "team-a"}); err == nil {
		t.Fatal("malformed persisted record was accepted by query")
	}
	if _, err := store.DeleteCostRecordsBefore(context.Background(), "", time.Now()); err == nil {
		t.Fatal("retention accepted empty workspace")
	}
	if _, err := store.DeleteCostRecordsBefore(context.Background(), "team-a", time.Time{}); err == nil {
		t.Fatal("retention accepted zero cutoff")
	}
}

func TestConfigMapStoreBatchIsImmutableAndSupportsFilteredPagination(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	first := time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)
	records := []observability.CostRecord{
		costRecord("team-a", "spiffe://planner", "trace-1", "span-1", first, 45),
		costRecord("team-a", "spiffe://planner", "trace-2", "span-2", first.Add(time.Minute), 10),
		costRecord("team-a", "spiffe://writer", "trace-3", "span-3", first.Add(2*time.Minute), 50),
	}
	records[1].SolutionRev = 3
	records[2].Solution = "delivery"
	if err := store.RecordCostBatch(ctx, records); err != nil {
		t.Fatal(err)
	}

	var configMaps corev1.ConfigMapList
	if err := store.client.List(ctx, &configMaps, client.InNamespace("kognition-system"), client.MatchingLabels{managedLabel: "true", workspaceHashLabel: workspaceHash("team-a")}); err != nil {
		t.Fatal(err)
	}
	if len(configMaps.Items) != 1 || configMaps.Items[0].Immutable == nil || !*configMaps.Items[0].Immutable || len(configMaps.Items[0].BinaryData[recordsDataKey]) == 0 {
		t.Fatalf("persisted ConfigMaps = %#v", configMaps.Items)
	}

	revision := int64(2)
	filtered, err := store.QueryCosts(ctx, observability.CostQuery{
		Workspace: "team-a", Solution: "research", SolutionRev: &revision,
		Start: first, End: first, GroupBy: []observability.CostGroupBy{observability.GroupBySolution},
	})
	if err != nil {
		t.Fatal(err)
	}
	if filtered.TotalCount != 1 || len(filtered.Aggregations) != 1 || filtered.Aggregations[0].TotalCostUnits != 45 || filtered.Aggregations[0].TotalOperations != 1 {
		t.Fatalf("filtered aggregation = %#v", filtered)
	}

	paged, err := store.QueryCosts(ctx, observability.CostQuery{Workspace: "team-a", GroupBy: []observability.CostGroupBy{observability.GroupByAgent}, Limit: 1, Offset: 1})
	if err != nil {
		t.Fatal(err)
	}
	if paged.TotalCount != 2 || len(paged.Aggregations) != 1 || paged.Aggregations[0].AgentIdentity != "spiffe://writer" || paged.Aggregations[0].TotalCostUnits != 50 {
		t.Fatalf("paged aggregation = %#v", paged)
	}
}

func TestConfigMapStoreRejectsInvalidAndMixedWorkspaceBatchesAtomically(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	if err := store.RecordCost(ctx, observability.CostRecord{OperationType: observability.CostOpModelInference}); err == nil {
		t.Fatal("record without workspace was accepted")
	}
	if err := store.RecordCost(ctx, observability.CostRecord{Workspace: "team-a", OperationType: observability.CostOpModelInference, CostUnits: -1}); err == nil {
		t.Fatal("record with negative cost was accepted")
	}
	if err := store.RecordCostBatch(ctx, []observability.CostRecord{
		costRecord("team-a", "spiffe://planner", "trace-a", "span-a", now, 1),
		costRecord("team-b", "spiffe://writer", "trace-b", "span-b", now, 1),
	}); err == nil {
		t.Fatal("mixed-workspace batch was accepted")
	}
	result, err := store.QueryCosts(ctx, observability.CostQuery{Workspace: "team-a"})
	if err != nil {
		t.Fatal(err)
	}
	if result.TotalCount != 0 || len(result.Aggregations) != 0 {
		t.Fatalf("mixed-workspace batch was partially persisted: %#v", result)
	}
	if _, err := store.QueryCosts(ctx, observability.CostQuery{}); err == nil {
		t.Fatal("query without workspace was accepted")
	}
	if _, err := store.QueryCosts(ctx, observability.CostQuery{Workspace: "team-a", GroupBy: []observability.CostGroupBy{observability.GroupByAgent, observability.GroupByAgent}}); err == nil {
		t.Fatal("query with duplicate groups was accepted")
	}
}

func TestConfigMapStoreRetentionRespectsWorkspace(t *testing.T) {
	store := newTestStore(t)
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	if err := store.RecordCost(context.Background(), costRecord("team-a", "spiffe://planner", "trace-a", "span-a", now.Add(-2*time.Hour), 45)); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordCost(context.Background(), costRecord("team-b", "spiffe://planner", "trace-b", "span-b", now.Add(-2*time.Hour), 7)); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.DeleteCostRecordsBefore(context.Background(), "team-a", now.Add(-time.Hour))
	if err != nil || deleted != 1 {
		t.Fatalf("DeleteCostRecordsBefore() = (%d, %v), want (1, nil)", deleted, err)
	}
	result, err := store.QueryCosts(context.Background(), observability.CostQuery{Workspace: "team-b"})
	if err != nil || result.TotalCount != 1 {
		t.Fatalf("team-b costs = %#v, err=%v", result, err)
	}
}

func TestConfigMapStoreRetentionKeepsMixedAgeBatch(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	if err := store.RecordCostBatch(ctx, []observability.CostRecord{
		costRecord("team-a", "spiffe://planner", "trace-old", "span-old", now.Add(-2*time.Hour), 45),
		costRecord("team-a", "spiffe://writer", "trace-current", "span-current", now, 50),
	}); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.DeleteCostRecordsBefore(ctx, "team-a", now.Add(-time.Hour))
	if err != nil || deleted != 0 {
		t.Fatalf("DeleteCostRecordsBefore() = (%d, %v), want (0, nil)", deleted, err)
	}
	total, err := store.GetTotalCost(ctx, "team-a", time.Time{}, time.Time{})
	if err != nil || total != 95 {
		t.Fatalf("GetTotalCost() = (%d, %v), want (95, nil)", total, err)
	}
}

func TestConfigMapStoreRetentionVerifiesDecodedWorkspace(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	payload, err := json.Marshal([]observability.CostRecord{costRecord("team-b", "spiffe://writer", "trace-b", "span-b", now.Add(-2*time.Hour), 50)})
	if err != nil {
		t.Fatal(err)
	}
	forged := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapPrefix + "forged-workspace",
			Namespace: "kognition-system",
			Labels: map[string]string{
				managedLabel:       "true",
				workspaceHashLabel: workspaceHash("team-a"),
			},
		},
		BinaryData: map[string][]byte{recordsDataKey: payload},
		Immutable:  boolPtr(true),
	}
	if err := store.client.Create(ctx, forged); err != nil {
		t.Fatal(err)
	}

	deleted, err := store.DeleteCostRecordsBefore(ctx, "team-a", now.Add(-time.Hour))
	if err != nil || deleted != 0 {
		t.Fatalf("DeleteCostRecordsBefore() = (%d, %v), want (0, nil)", deleted, err)
	}
	var stored corev1.ConfigMap
	if err := store.client.Get(ctx, client.ObjectKeyFromObject(forged), &stored); err != nil {
		t.Fatalf("retention deleted a record from another workspace: %v", err)
	}
}

func TestRetentionRunnableDeletesCostRecordsUsingWorkspaceTTL(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := k8s.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	workspace := &k8s.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "policy", Namespace: "team-a"}, Spec: k8s.WorkspaceSpec{Retention: &k8s.RetentionPolicy{CostRecordTTL: "1h"}}}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(workspace).Build()
	store, err := NewConfigMapStore(client, "kognition-system")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	if err := store.RecordCost(context.Background(), costRecord("team-a", "spiffe://planner", "trace-1", "span-1", now.Add(-2*time.Hour), 45)); err != nil {
		t.Fatal(err)
	}
	runnable := NewRetentionRunnable(client, store, nil, time.Hour)
	runnable.now = func() time.Time { return now }
	runnable.sweep(context.Background())
	result, err := store.QueryCosts(context.Background(), observability.CostQuery{Workspace: "team-a"})
	if err != nil || result.TotalCount != 0 {
		t.Fatalf("retained costs = %#v, err=%v", result, err)
	}
}

func TestRetentionRunnableLifecycleAndWorkspaceFallback(t *testing.T) {
	runnable := NewRetentionRunnable(nil, nil, nil, 0)
	if runnable.interval != time.Hour || !runnable.NeedLeaderElection() {
		t.Fatalf("runnable defaults = %#v", runnable)
	}
	runnable.sweep(context.Background())
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runnable.Start(canceled); err != nil {
		t.Fatalf("Start() with canceled context = %v", err)
	}
	if workspaceNamespace(&k8s.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "fallback"}}) != "fallback" {
		t.Fatal("workspace name fallback was not used")
	}
	if workspaceNamespace(&k8s.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "ignored", Namespace: "team-a"}}) != "team-a" {
		t.Fatal("workspace namespace was not preferred")
	}
}

func TestConfigMapStorePeriodAndAggregationHelpers(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	base := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	noCost := &k8s.CognitiveQuota{ObjectMeta: metav1.ObjectMeta{Name: "no-cost", Namespace: "team-a"}, Spec: k8s.CognitiveQuotaSpec{Hard: map[string]int64{}}}
	newer := &k8s.CognitiveQuota{ObjectMeta: metav1.ObjectMeta{Name: "newer", Namespace: "team-a", CreationTimestamp: metav1.NewTime(base)}, Spec: k8s.CognitiveQuotaSpec{Hard: map[string]int64{}, CostBudget: &k8s.CostBudget{}}, Status: k8s.CognitiveQuotaStatus{LastResetTime: base.Add(time.Hour).Format(time.RFC3339Nano)}}
	for _, quota := range []*k8s.CognitiveQuota{noCost, newer} {
		if err := store.client.Create(ctx, quota); err != nil {
			t.Fatal(err)
		}
	}
	periodStart, err := store.activeCostPeriodStart(ctx, "team-a")
	if err != nil || !periodStart.Equal(base.Add(time.Hour)) {
		t.Fatalf("active period start = (%s, %v)", periodStart, err)
	}
	invalid := &k8s.CognitiveQuota{ObjectMeta: metav1.ObjectMeta{Name: "invalid", Namespace: "team-b"}, Spec: k8s.CognitiveQuotaSpec{Hard: map[string]int64{}, CostBudget: &k8s.CostBudget{}}, Status: k8s.CognitiveQuotaStatus{LastResetTime: "invalid"}}
	if err := store.client.Create(ctx, invalid); err != nil {
		t.Fatal(err)
	}
	if _, err := store.activeCostPeriodStart(ctx, "team-b"); err == nil {
		t.Fatal("invalid reset time was accepted")
	}

	record := observability.CostRecord{Workspace: "team-a", AgentIdentity: "spiffe://planner", Solution: "research", SolutionRev: 2, ModelName: "gpt", OperationType: observability.CostOpModelInference, CostUnits: 45, PromptTokens: 2, CompletionTokens: 1, Timestamp: base}
	groups := []observability.CostGroupBy{observability.GroupByWorkspace, observability.GroupByAgent, observability.GroupBySolution, observability.GroupByModel, observability.GroupByOperationType}
	_, aggregate := aggregateFor(record, groups)
	if aggregate.Workspace != "team-a" || aggregate.AgentIdentity != "spiffe://planner" || aggregate.Solution != "research" || aggregate.SolutionRev != 2 || aggregate.ModelName != "gpt" || aggregate.OperationType != string(observability.CostOpModelInference) {
		t.Fatalf("aggregate = %#v", aggregate)
	}
	if key := aggregateKey(aggregate, groups); key != "team-a\x00spiffe://planner\x00research\x002\x00gpt\x00model_inference" {
		t.Fatalf("aggregate key = %q", key)
	}
	firstEventName := usageEventRecordName("event")
	secondEventName := usageEventRecordName("event")
	otherEventName := usageEventRecordName("other")
	if firstEventName != secondEventName || firstEventName == otherEventName || recordName(observability.CostRecord{TraceID: "trace", SpanID: "span"}) == "" || recordName(observability.CostRecord{}) == "" {
		t.Fatal("record names were not deterministic or distinct")
	}
	if !leaseExpired(&coordinationv1.Lease{}, base) {
		t.Fatal("lease without renewal data was not expired")
	}
	seconds := int32(60)
	renewed := metav1.NewMicroTime(base)
	if leaseExpired(&coordinationv1.Lease{Spec: coordinationv1.LeaseSpec{LeaseDurationSeconds: &seconds, RenewTime: &renewed}}, base.Add(time.Second)) {
		t.Fatal("fresh lease was expired")
	}
}

func TestConfigMapStoreReplayUsesLegacyTraceSpanName(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	record := costRecord("team-a", "spiffe://planner", "legacy-trace", "legacy-span", time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC), 45)
	payload, err := json.Marshal([]observability.CostRecord{record})
	if err != nil {
		t.Fatal(err)
	}
	legacy := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      configMapPrefix + recordName(record),
			Namespace: "kognition-system",
			Labels: map[string]string{
				managedLabel:       "true",
				workspaceHashLabel: workspaceHash(record.Workspace),
			},
		},
		BinaryData: map[string][]byte{recordsDataKey: payload},
		Immutable:  boolPtr(true),
	}
	if err := store.client.Create(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordCost(ctx, record); err != nil {
		t.Fatalf("legacy replay failed: %v", err)
	}
	var configMaps corev1.ConfigMapList
	if err := store.client.List(ctx, &configMaps, client.InNamespace("kognition-system"), client.MatchingLabels{managedLabel: "true", workspaceHashLabel: workspaceHash(record.Workspace)}); err != nil {
		t.Fatal(err)
	}
	if len(configMaps.Items) != 1 {
		t.Fatalf("cost ConfigMaps = %d, want one legacy record", len(configMaps.Items))
	}
}

func TestConfigMapStoreRejectsModernModelEventsFromLegacyAPIs(t *testing.T) {
	store := newTestStore(t)
	event := observability.CostRecord{ID: "modern-event", Workspace: "team-a", OperationType: observability.CostOpModelInference}
	if err := store.RecordCost(context.Background(), event); err == nil {
		t.Fatal("RecordCost accepted a modern model usage event")
	}
	if err := store.RecordCostBatch(context.Background(), []observability.CostRecord{event}); err == nil {
		t.Fatal("RecordCostBatch accepted a modern model usage event")
	}
	if applied, err := store.ApplyUsageEvent(context.Background(), event); err != nil || !applied {
		t.Fatalf("ApplyUsageEvent() = (%t, %v), want (true, nil)", applied, err)
	}
	result, err := store.QueryCosts(context.Background(), observability.CostQuery{Workspace: "team-a"})
	if err != nil || result.TotalCount != 1 || result.Aggregations[0].TotalOperations != 1 {
		t.Fatalf("costs = %#v, err=%v; want one event", result, err)
	}
}

func TestRetentionRunnableSkipsInvalidAndUnconfiguredWorkspaces(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := k8s.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	workspaces := []client.Object{
		&k8s.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "no-retention", Namespace: "team-a"}},
		&k8s.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "invalid-retention", Namespace: "team-b"}, Spec: k8s.WorkspaceSpec{Retention: &k8s.RetentionPolicy{CostRecordTTL: "invalid"}}},
		&k8s.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "zero-retention", Namespace: "team-c"}, Spec: k8s.WorkspaceSpec{Retention: &k8s.RetentionPolicy{CostRecordTTL: "0s"}}},
	}
	kubernetesClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(workspaces...).Build()
	store, err := NewConfigMapStore(kubernetesClient, "kognition-system")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	for _, workspace := range []string{"team-a", "team-b", "team-c"} {
		if err := store.RecordCost(context.Background(), costRecord(workspace, "spiffe://planner", "trace-"+workspace, "span-"+workspace, now.Add(-2*time.Hour), 45)); err != nil {
			t.Fatal(err)
		}
	}
	NewRetentionRunnable(kubernetesClient, store, nil, time.Minute).sweep(context.Background())
	for _, workspace := range []string{"team-a", "team-b", "team-c"} {
		result, err := store.QueryCosts(context.Background(), observability.CostQuery{Workspace: workspace})
		if err != nil || result.TotalCount != 1 {
			t.Fatalf("workspace %s costs = %#v, err=%v; want retained record", workspace, result, err)
		}
	}
}

func TestConfigMapStoreUsageLockTakeoverAndReceiptValidation(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	workspace := "team-a"
	digest := sha256.Sum256([]byte(workspace))
	lockKey := client.ObjectKey{Namespace: "kognition-system", Name: "kognition-usage-lock-" + fmt.Sprintf("%x", digest[:12])}
	holder := "stale-holder"
	duration := int32(60)
	stale := metav1.NewMicroTime(time.Now().Add(-time.Hour))
	if err := store.client.Create(ctx, &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Namespace: lockKey.Namespace, Name: lockKey.Name}, Spec: coordinationv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &duration, RenewTime: &stale}}); err != nil {
		t.Fatal(err)
	}
	lockCtx, release, err := store.acquireUsageLock(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	identity := lockCtx.Value(usageLockContextKey{}).(usageLockIdentity)
	if err := store.renewUsageLock(lockCtx, identity.key, identity.holder); err != nil {
		t.Fatalf("renew acquired usage lock: %v", err)
	}
	if err := store.verifyUsageLock(lockCtx); err != nil {
		t.Fatalf("verify acquired usage lock: %v", err)
	}
	release()

	active := "active-holder"
	now := metav1.NewMicroTime(time.Now().UTC())
	lease := &coordinationv1.Lease{}
	if err := store.client.Get(ctx, lockKey, lease); err != nil {
		t.Fatal(err)
	}
	lease.Spec.HolderIdentity, lease.Spec.RenewTime, lease.Spec.LeaseDurationSeconds = &active, &now, &duration
	if err := store.client.Update(ctx, lease); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := store.acquireUsageLock(canceled, workspace); err == nil {
		t.Fatal("canceled waiter acquired active usage lock")
	}

	record := observability.CostRecord{ID: "receipt-valid", Workspace: workspace, OperationType: observability.CostOpModelInference, Timestamp: time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)}
	payload, err := json.Marshal([]observability.CostRecord{record})
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256(payload))
	eventConfigMap := usageEventConfigMap("kognition-system", record, payload)
	if err := store.client.Create(ctx, eventConfigMap); err != nil {
		t.Fatal(err)
	}
	if err := store.client.Create(ctx, usageEventReceiptConfigMap("kognition-system", record, fingerprint)); err != nil {
		t.Fatal(err)
	}
	if err := store.validateUsageEventReceipt(ctx, eventConfigMap, []observability.CostRecord{record}); err != nil {
		t.Fatalf("valid receipt rejected: %v", err)
	}
	if err := store.validateUsageEventReceipt(ctx, eventConfigMap, nil); err == nil {
		t.Fatal("empty usage event batch was accepted")
	}
}
