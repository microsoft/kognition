// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package postgres_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/modelhealth"
	"github.com/kognition-ai/kognition/internal/store/postgres"
)

// TestModelClassErrorRateFollowsRealCallsThroughPostgres drives an
// error-rate-threshold ModelClass with the real tracker, reporter, SQL store and
// reconciler: failing calls to one of its Models degrade the class even though
// its active probe passes, and healed calls recover it.
func TestModelClassErrorRateFollowsRealCallsThroughPostgres(t *testing.T) {
	threshold := int32(50)
	ctx := context.Background()
	pool := postgres.SharedTestPool()
	backend := postgres.New(pool, zaptest.NewLogger(t))
	namespace := "class-rate-" + uuid.NewString()
	className := namespace
	reporterID := namespace + "/reporter"
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, "DELETE FROM resources WHERE namespace = $1 OR (kind = 'ModelClass' AND name = $2)", namespace, className)
		_, _ = pool.Exec(ctx, "DELETE FROM model_call_health WHERE reporter = $1", reporterID)
	})

	provider := &flakyProvider{}
	registry := cpi.NewRegistry()
	registry.MustRegister(provider)
	tracker := modelhealth.NewTracker()
	registry.Observe(tracker)
	reporter := modelhealth.NewReporter(tracker, backend, reporterID, zaptest.NewLogger(t))

	class := v1alpha1.ModelClass{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "ModelClass"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: className, Finalizers: []string{"kognition.dev/model-class-protection"}},
		Spec: v1alpha1.ModelClassSpec{
			Provisioner: "anthropic",
			HealthProbe: &v1alpha1.HealthProbeSpec{
				Endpoint: "https://api.example/" + namespace + "/health", Type: "error-rate-threshold",
				ErrorRateThreshold: &threshold, FailureThreshold: 1, SuccessThreshold: 1,
			},
		},
		Status: v1alpha1.ModelClassStatus{Phase: v1alpha1.PhaseReady, Provisioned: true, Health: "Healthy"},
	}
	model := v1alpha1.Model{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Model"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "member", Namespace: namespace},
		Spec:       v1alpha1.ModelSpec{ClassName: className, ModelID: "claude-sonnet", Endpoint: "https://api.example/" + namespace},
	}
	for kind, object := range map[string]struct {
		namespace, name string
		value           any
	}{
		"ModelClass": {"", className, class},
		"Model":      {namespace, "member", model},
	} {
		data, err := json.Marshal(object.value)
		if err != nil {
			t.Fatal(err)
		}
		if err := backend.Create(ctx, kind, object.namespace, object.name, data); err != nil {
			t.Fatal(err)
		}
	}

	reconciler := controller.NewModelClassReconciler(backend, registry, zaptest.NewLogger(t))
	reconcile := func() v1alpha1.ModelClass {
		t.Helper()
		if _, err := reconciler.Reconcile(ctx, controller.Request{Name: className, Kind: "ModelClass"}); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		raw, err := backend.Get(ctx, "ModelClass", "", className)
		if err != nil {
			t.Fatal(err)
		}
		var got v1alpha1.ModelClass
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	callAndPublish := func(calls int) {
		t.Helper()
		driver, _ := registry.Get("anthropic")
		for i := 0; i < calls; i++ {
			_, _ = driver.Complete(ctx, &cpi.CompletionRequest{Model: model.Spec.ModelID, Endpoint: model.Spec.Endpoint})
		}
		if err := reporter.Flush(ctx); err != nil {
			t.Fatalf("Flush() error: %v", err)
		}
	}

	if got := reconcile(); got.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("phase = %s, want Ready before any call failed", got.Status.Phase)
	}

	provider.setFailing(true)
	callAndPublish(6)
	got := reconcile()
	if got.Status.Phase != v1alpha1.PhaseDegraded {
		t.Fatalf("phase = %s, want Degraded by the real calls' error rate", got.Status.Phase)
	}
	var ready *v1alpha1.Condition
	for i := range got.Status.Conditions {
		if got.Status.Conditions[i].Type == "Ready" {
			ready = &got.Status.Conditions[i]
		}
	}
	if ready == nil || ready.Reason != "HighErrorRate" {
		t.Fatalf("Ready = %+v, want reason HighErrorRate", ready)
	}

	provider.setFailing(false)
	callAndPublish(6)
	if got := reconcile(); got.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("phase = %s, want Ready once the latest calls succeed", got.Status.Phase)
	}
}
