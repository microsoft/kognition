// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
)

func newMemoryFakeClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(objects...).
		WithStatusSubresource(&k8s.Memory{}, &k8s.MemoryRecord{}).
		Build()
}

func TestMemoryRecordReconcilerAdapterPersistsNormalizationAndStatus(t *testing.T) {
	record := &k8s.MemoryRecord{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: "record-a"},
		Spec: k8s.MemoryRecordSpec{
			MemoryRef: k8s.LocalObjectReference{Name: "conversation"},
			Content:   "Contact Ada at ada@example.com",
			Provenance: &k8s.MemoryProvenance{
				SourceAgentIdentity: "assistant",
				SessionID:           "session-a",
				ReasoningStepID:     "step-a",
			},
		},
	}
	k8sClient := newMemoryFakeClient(t, record)
	adapter := NewMemoryRecordReconcilerAdapter(NewStoreAdapter(k8sClient), testLogger(t))

	if _, err := adapter.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: record.Namespace, Name: record.Name},
	}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	var updated k8s.MemoryRecord
	if err := k8sClient.Get(context.Background(), client.ObjectKeyFromObject(record), &updated); err != nil {
		t.Fatalf("get updated MemoryRecord: %v", err)
	}
	if updated.Spec.Provenance == nil || updated.Spec.Provenance.Timestamp.IsZero() {
		t.Fatal("provenance timestamp was not persisted")
	}
	if !strings.Contains(updated.Spec.Content, "[REDACTED_EMAIL]") {
		t.Fatalf("content = %q, want redacted email", updated.Spec.Content)
	}
	if updated.Spec.Metadata["piiRedacted"] != "true" {
		t.Fatalf("metadata = %v, want piiRedacted=true", updated.Spec.Metadata)
	}
	if updated.Status.Phase != "Ready" {
		t.Fatalf("status.phase = %q, want Ready", updated.Status.Phase)
	}
	if len(updated.Status.Conditions) != 1 || updated.Status.Conditions[0].Status != metav1.ConditionTrue {
		t.Fatalf("status.conditions = %+v, want Ready=True", updated.Status.Conditions)
	}
}

func TestMemorySummarizationAdaptersPersistNativeResources(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		reconcile func(client.Client) error
	}{
		{
			name: "Memory event",
			reconcile: func(k8sClient client.Client) error {
				adapter := NewMemorySummarizationReconcilerAdapter(NewStoreAdapter(k8sClient), testLogger(t))
				_, err := adapter.Reconcile(context.Background(), ctrl.Request{
					NamespacedName: types.NamespacedName{Namespace: "tenant-a", Name: "conversation"},
				})
				return err
			},
		},
		{
			name: "MemoryRecord event",
			reconcile: func(k8sClient client.Client) error {
				adapter := NewMemoryRecordSummarizationReconcilerAdapter(NewStoreAdapter(k8sClient), testLogger(t))
				_, err := adapter.Reconcile(context.Background(), ctrl.Request{
					NamespacedName: types.NamespacedName{Namespace: "tenant-a", Name: "record-c"},
				})
				return err
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			objects := memorySummarizationFixture()
			k8sClient := newMemoryFakeClient(t, objects...)

			if err := testCase.reconcile(k8sClient); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}

			var memory k8s.Memory
			if err := k8sClient.Get(context.Background(), types.NamespacedName{Namespace: "tenant-a", Name: "conversation"}, &memory); err != nil {
				t.Fatalf("get Memory: %v", err)
			}
			if memory.Status.RecordCount != 1 {
				t.Fatalf("Memory status.recordCount = %d, want 1", memory.Status.RecordCount)
			}

			var records k8s.MemoryRecordList
			if err := k8sClient.List(context.Background(), &records, client.InNamespace("tenant-a")); err != nil {
				t.Fatalf("list MemoryRecords: %v", err)
			}
			activeRecords := 0
			summarizedRecords := 0
			for i := range records.Items {
				record := &records.Items[i]
				if record.Spec.MemoryRef.Name != memory.Name {
					continue
				}
				if record.Status.Phase == "Summarized" {
					summarizedRecords++
				} else {
					activeRecords++
					if record.Spec.Metadata["summary"] != "true" {
						t.Fatalf("active record %q is not the generated summary", record.Name)
					}
				}
			}
			if activeRecords != 1 || summarizedRecords != 3 {
				t.Fatalf("active records = %d, summarized records = %d; want 1 and 3", activeRecords, summarizedRecords)
			}
		})
	}
}

func memorySummarizationFixture() []client.Object {
	memoryClass := &k8s.MemoryClass{
		ObjectMeta: metav1.ObjectMeta{Name: "episodic"},
		Spec: k8s.MemoryClassSpec{
			BackendType: "pgvector",
			SummarizationConfig: &k8s.MemorySummarizationConfig{
				Enabled:          true,
				ThresholdRecords: 2,
			},
		},
	}
	claim := &k8s.MemoryClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: "conversation-claim"},
		Spec: k8s.MemoryClaimSpec{
			MemoryClassName: memoryClass.Name,
			Type:            k8s.MemoryTypeEpisodic,
		},
	}
	memory := &k8s.Memory{
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: "conversation"},
		Spec: k8s.MemorySpec{
			ClaimRef: k8s.LocalObjectReference{Name: claim.Name},
			Type:     k8s.MemoryTypeEpisodic,
		},
	}

	objects := []client.Object{memoryClass, claim, memory}
	baseTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for index, name := range []string{"record-a", "record-b", "record-c"} {
		objects = append(objects, &k8s.MemoryRecord{
			ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: name},
			Spec: k8s.MemoryRecordSpec{
				MemoryRef: k8s.LocalObjectReference{Name: memory.Name},
				Content:   "fact-" + name,
				Provenance: &k8s.MemoryProvenance{
					SourceAgentIdentity: "assistant",
					SessionID:           "session-a",
					ReasoningStepID:     "step-" + name,
					Timestamp:           metav1.NewTime(baseTime.Add(time.Duration(index) * time.Minute)),
				},
			},
		})
	}
	return objects
}
