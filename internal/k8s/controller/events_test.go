// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// ---------------------------------------------------------------------------
// TC-0274: Event emission with fingerprint dedupe — verify that the emitter
// records Warning events on Ready=False condition and suppresses duplicates
// when the fingerprint has not changed between reconciles.
// ---------------------------------------------------------------------------

func TestEventEmitter_EmitsWarningOnFailedPhase(t *testing.T) {
	// TC-0274: force a Warning condition, verify event is emitted with matching reason/message.
	phase := "Failed"
	conds := []interface{}{
		map[string]interface{}{
			"type":    "Ready",
			"status":  "False",
			"reason":  "InvalidCredentials",
			"message": "model endpoint returned 401 Unauthorized",
		},
	}

	reason, message, evType := summarize(phase, conds)
	if reason != "InvalidCredentials" {
		t.Errorf("reason = %q, want InvalidCredentials", reason)
	}
	if message != "model endpoint returned 401 Unauthorized" {
		t.Errorf("message = %q, want 'model endpoint returned 401 Unauthorized'", message)
	}
	if evType != corev1.EventTypeWarning {
		t.Errorf("evType = %q, want Warning", evType)
	}
}

func TestEventEmitter_FingerprintDedupe(t *testing.T) {
	// TC-0274: verify no duplicate event is emitted when fingerprint hasn't changed.
	recorder := events.NewFakeRecorder(10)

	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kognition.dev/v1alpha1",
		"kind":       "Model",
		"metadata": map[string]interface{}{
			"namespace":       "default",
			"name":            "bad-creds",
			"resourceVersion": "1",
		},
		"status": map[string]interface{}{
			"phase": "Failed",
			"conditions": []interface{}{
				map[string]interface{}{
					"type":    "Ready",
					"status":  "False",
					"reason":  "InvalidCredentials",
					"message": "model endpoint returned 401 Unauthorized",
				},
			},
		},
	}}

	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Model"},
		&unstructured.Unstructured{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "ModelList"},
		&unstructured.UnstructuredList{},
	)

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(obj).
		Build()

	emitter := &eventEmitter{
		client:   fc,
		recorder: recorder,
		kind:     "Model",
		lastSeen: make(map[types.NamespacedName]string),
	}

	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "bad-creds"}}

	// First emit should record an event.
	emitter.emit(ctx, req, nil)

	// Second emit with same state should be deduped (no new event).
	emitter.emit(ctx, req, nil)

	// Drain recorded events.
	close(recorder.Events)
	events := make([]string, 0)
	for ev := range recorder.Events {
		events = append(events, ev)
	}

	if len(events) != 1 {
		t.Fatalf("expected exactly 1 event (dedupe), got %d: %v", len(events), events)
	}
}

func TestEventEmitter_EmitsAgainOnStateChange(t *testing.T) {
	// TC-0274: verify event IS emitted when fingerprint changes.
	recorder := events.NewFakeRecorder(10)

	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Model"},
		&unstructured.Unstructured{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "ModelList"},
		&unstructured.UnstructuredList{},
	)

	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kognition.dev/v1alpha1",
		"kind":       "Model",
		"metadata": map[string]interface{}{
			"namespace":       "default",
			"name":            "model-1",
			"resourceVersion": "1",
		},
		"status": map[string]interface{}{
			"phase": "Failed",
			"conditions": []interface{}{
				map[string]interface{}{
					"type":    "Ready",
					"status":  "False",
					"reason":  "InvalidCredentials",
					"message": "401 Unauthorized",
				},
			},
		},
	}}

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(obj).
		Build()

	emitter := &eventEmitter{
		client:   fc,
		recorder: recorder,
		kind:     "Model",
		lastSeen: make(map[types.NamespacedName]string),
	}

	ctx := context.Background()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "model-1"}}

	// First emit.
	emitter.emit(ctx, req, nil)

	// Change the object's phase — simulates reconcile fixing the issue.
	obj2 := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kognition.dev/v1alpha1",
		"kind":       "Model",
		"metadata": map[string]interface{}{
			"namespace":       "default",
			"name":            "model-1",
			"resourceVersion": "2",
		},
		"status": map[string]interface{}{
			"phase": "Ready",
			"conditions": []interface{}{
				map[string]interface{}{
					"type":    "Ready",
					"status":  "True",
					"reason":  "Reconciled",
					"message": "model is ready",
				},
			},
		},
	}}
	fc2 := fake.NewClientBuilder().WithScheme(scheme).WithObjects(obj2).Build()
	emitter.client = fc2

	// Second emit should fire because fingerprint changed.
	emitter.emit(ctx, req, nil)

	close(recorder.Events)
	events := make([]string, 0)
	for ev := range recorder.Events {
		events = append(events, ev)
	}

	if len(events) != 2 {
		t.Fatalf("expected 2 events (state changed), got %d: %v", len(events), events)
	}
}

func TestEventEmitter_ReconcileErrorTakesPrecedenceOverStatus(t *testing.T) {
	recorder := events.NewFakeRecorder(1)
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kognition.dev/v1alpha1",
		"kind":       "Model",
		"metadata": map[string]interface{}{
			"namespace": "default",
			"name":      "model-1",
		},
		"status": map[string]interface{}{"phase": "Ready"},
	}}
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Model"},
		&unstructured.Unstructured{},
	)
	emitter := &eventEmitter{
		client:   fake.NewClientBuilder().WithScheme(scheme).WithObjects(obj).Build(),
		recorder: recorder,
		kind:     "Model",
		lastSeen: make(map[types.NamespacedName]string),
	}

	emitter.emit(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "model-1"},
	}, fmt.Errorf("provider unavailable"))

	var event string
	select {
	case event = <-recorder.Events:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for reconcile error event")
	}
	if !strings.Contains(event, "Warning ReconcileError provider unavailable") {
		t.Fatalf("event = %q, want reconcile error warning", event)
	}
}

func TestEventEmitter_SkipsClusterScopedObjects(t *testing.T) {
	recorder := events.NewFakeRecorder(1)
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kognition.dev/v1alpha1",
		"kind":       "KnowledgeBaseClass",
		"metadata":   map[string]interface{}{"name": "pgvector-default"},
		"status":     map[string]interface{}{"phase": "Failed"},
	}}
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "KnowledgeBaseClass"},
		&unstructured.Unstructured{},
	)
	emitter := &eventEmitter{
		client:   fake.NewClientBuilder().WithScheme(scheme).WithObjects(obj).Build(),
		recorder: recorder,
		kind:     "KnowledgeBaseClass",
		lastSeen: make(map[types.NamespacedName]string),
	}

	emitter.emit(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "pgvector-default"},
	}, fmt.Errorf("provider unavailable"))

	select {
	case event := <-recorder.Events:
		t.Fatalf("unexpected event for cluster-scoped object: %q", event)
	default:
	}
}

// ---------------------------------------------------------------------------
// TC-0274: fingerprint unit tests
// ---------------------------------------------------------------------------

func TestFingerprint_DifferentPhasesProduceDifferentFingerprints(t *testing.T) {
	// TC-0274
	conds := []interface{}{
		map[string]interface{}{"type": "Ready", "status": "True", "reason": "Reconciled"},
	}

	fp1 := fingerprint(nil, "Ready", conds)
	fp2 := fingerprint(nil, "Failed", conds)

	if fp1 == fp2 {
		t.Errorf("fingerprints should differ for different phases: %q vs %q", fp1, fp2)
	}
}

func TestFingerprint_ErrorProducesDifferentFingerprint(t *testing.T) {
	// TC-0274
	fp1 := fingerprint(nil, "Ready", nil)
	fp2 := fingerprint(fmt.Errorf("something broke"), "Ready", nil)

	if fp1 == fp2 {
		t.Errorf("fingerprints should differ when reconcileErr is set: %q vs %q", fp1, fp2)
	}
}

func TestFingerprint_SameInputsProduceSameFingerprint(t *testing.T) {
	// TC-0274
	conds := []interface{}{
		map[string]interface{}{"type": "Ready", "status": "True", "reason": "Reconciled"},
	}

	fp1 := fingerprint(nil, "Ready", conds)
	fp2 := fingerprint(nil, "Ready", conds)

	if fp1 != fp2 {
		t.Errorf("fingerprints should match for same inputs: %q vs %q", fp1, fp2)
	}
}

func TestSummarize_ReadyPhaseEmitsNormalEvent(t *testing.T) {
	// TC-0274
	conds := []interface{}{
		map[string]interface{}{"type": "Ready", "status": "True", "reason": "Reconciled", "message": "all good"},
	}

	reason, message, evType := summarize("Ready", conds)
	if evType != corev1.EventTypeNormal {
		t.Errorf("evType = %q, want Normal", evType)
	}
	if reason != "Reconciled" {
		t.Errorf("reason = %q, want Reconciled", reason)
	}
	if message != "all good" {
		t.Errorf("message = %q, want 'all good'", message)
	}
}

func TestSummarize_PendingPhaseEmitsNothing(t *testing.T) {
	// TC-0274
	reason, _, _ := summarize("Pending", nil)
	if reason != "" {
		t.Errorf("expected empty reason for Pending phase, got %q", reason)
	}
}

func TestSummarize_FailedPhaseEmitsWarning(t *testing.T) {
	// TC-0274
	conds := []interface{}{
		map[string]interface{}{"type": "Ready", "status": "False", "reason": "InvalidCredentials", "message": "401"},
	}

	reason, message, evType := summarize("Failed", conds)
	if evType != corev1.EventTypeWarning {
		t.Errorf("evType = %q, want Warning", evType)
	}
	if reason != "InvalidCredentials" {
		t.Errorf("reason = %q, want InvalidCredentials", reason)
	}
	if message != "401" {
		t.Errorf("message = %q, want '401'", message)
	}
}

func TestSummarize_SelectsConditionMatchingCurrentPhase(t *testing.T) {
	tests := []struct {
		name        string
		phase       string
		conditions  []interface{}
		wantReason  string
		wantMessage string
		wantType    string
	}{
		{
			name:  "recovery ignores stale failure",
			phase: "Ready",
			conditions: []interface{}{
				map[string]interface{}{"type": "Ready", "status": "False", "reason": "ProbeFailed", "message": "connection refused"},
				map[string]interface{}{"type": "Ready", "status": "True", "reason": "Recovered", "message": "probe succeeded"},
			},
			wantReason:  "Recovered",
			wantMessage: "probe succeeded",
			wantType:    corev1.EventTypeNormal,
		},
		{
			name:  "failure ignores stale success",
			phase: "Failed",
			conditions: []interface{}{
				map[string]interface{}{"type": "Ready", "status": "True", "reason": "Reconciled", "message": "resource was ready"},
				map[string]interface{}{"type": "Ready", "status": "False", "reason": "CredentialsRejected", "message": "authentication failed"},
			},
			wantReason:  "CredentialsRejected",
			wantMessage: "authentication failed",
			wantType:    corev1.EventTypeWarning,
		},
		{
			name:  "matching condition without details uses defaults",
			phase: "Ready",
			conditions: []interface{}{
				map[string]interface{}{"type": "Ready", "status": "True"},
			},
			wantReason:  "Reconciled",
			wantMessage: "resource is ready",
			wantType:    corev1.EventTypeNormal,
		},
		{
			name:  "no matching status uses failure defaults",
			phase: "Failed",
			conditions: []interface{}{
				map[string]interface{}{"type": "Ready", "status": "True", "reason": "StaleSuccess", "message": "resource was ready"},
			},
			wantReason:  "Failed",
			wantMessage: "resource reconciliation failed",
			wantType:    corev1.EventTypeWarning,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, message, eventType := summarize(tt.phase, tt.conditions)
			if reason != tt.wantReason || message != tt.wantMessage || eventType != tt.wantType {
				t.Fatalf("summarize() = (%q, %q, %q), want (%q, %q, %q)", reason, message, eventType, tt.wantReason, tt.wantMessage, tt.wantType)
			}
		})
	}
}
