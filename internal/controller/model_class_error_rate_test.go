// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/modelhealth"
	"github.com/kognition-ai/kognition/internal/store"
)

const rateClassName = "rate-class"

// rateTarget is the health target of a Model that the class serves.
func rateTarget(modelID string) string {
	return modelhealth.TargetKey("openai", "https://api.example/v1", modelID)
}

// rateRecord is one target's summary: calls and faults in the window, and whether
// the latest call failed.
func rateRecord(target string, calls, faults int32, latestFailed bool) store.ModelCallHealth {
	now := time.Now()
	record := store.ModelCallHealth{Target: target, Calls: calls, Faults: faults, LastFaultReason: cpi.ReasonTimeout}
	if faults > 0 {
		record.LastFaultAt = now.Add(-time.Second)
	}
	if calls > faults {
		record.LastSuccessAt = now.Add(-time.Second)
	}
	if latestFailed {
		record.ConsecutiveFaults, record.LastFaultAt = 1, now
	} else {
		record.ConsecutiveSuccesses, record.LastSuccessAt = 1, now
	}
	return record
}

func newRateClass(threshold int32, probeType string) v1alpha1.ModelClass {
	return v1alpha1.ModelClass{
		TypeMeta:   v1alpha1.TypeMeta{Kind: "ModelClass", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: rateClassName, Namespace: "default", Finalizers: []string{modelClassFinalizer}},
		Spec: v1alpha1.ModelClassSpec{
			Provisioner: "openai",
			HealthProbe: &v1alpha1.HealthProbeSpec{
				Endpoint: "https://api.example/health", Type: probeType, ErrorRateThreshold: &threshold,
				FailureThreshold: 1, SuccessThreshold: 2, IntervalSeconds: 10,
			},
		},
		Status: v1alpha1.ModelClassStatus{Phase: v1alpha1.PhaseReady, Provisioned: true, Health: "Healthy"},
	}
}

func rateModel(name, namespace, class, modelID string) v1alpha1.Model {
	return v1alpha1.Model{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Model"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: name, Namespace: namespace},
		Spec:       v1alpha1.ModelSpec{ClassName: class, ModelID: modelID, Endpoint: "https://api.example/v1"},
	}
}

type rateFixture struct {
	t      *testing.T
	store  *callHealthStore
	driver *configurableMockDriver
	r      *ModelClassReconciler
}

func newRateFixture(t *testing.T, class v1alpha1.ModelClass, models ...v1alpha1.Model) *rateFixture {
	t.Helper()
	s := newCallHealthStore()
	driver := &configurableMockDriver{name: "openai-compatible", probeResult: &cpi.HealthResult{Available: true, Reason: "OK", Message: "ok"}}
	f := &rateFixture{t: t, store: s, driver: driver, r: NewModelClassReconciler(s, registryWith(driver), zaptest.NewLogger(t))}
	storeModelClass(t, s.memStore, class)
	for _, model := range models {
		storeModel(t, s.memStore, model)
	}
	return f
}

func (f *rateFixture) reconcile() v1alpha1.ModelClass {
	f.t.Helper()
	reconcileModelClass(f.t, f.r, "default", rateClassName)
	return getModelClass(f.t, f.store.memStore, "default", rateClassName)
}

func readyConditionOf(mc v1alpha1.ModelClass) *v1alpha1.Condition {
	for i := range mc.Status.Conditions {
		if mc.Status.Conditions[i].Type == "Ready" {
			return &mc.Status.Conditions[i]
		}
	}
	return nil
}

func TestModelClassReconciler_ErrorRateDegradesAClassWhoseRealCallsFail(t *testing.T) {
	f := newRateFixture(t, newRateClass(40, "error-rate-threshold"), rateModel("a", "default", rateClassName, "m1"))
	if got := f.reconcile(); got.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("precondition: a class without failing calls must stay Ready, got %s", got.Status.Phase)
	}

	f.store.set([]store.ModelCallHealth{rateRecord(rateTarget("m1"), 10, 4, true)}, nil)
	got := f.reconcile()
	if got.Status.Phase != v1alpha1.PhaseDegraded || got.Status.Health != "Degraded" {
		t.Fatalf("phase=%s health=%s, want a class degraded by a 40%% error rate at a 40%% threshold", got.Status.Phase, got.Status.Health)
	}
	condition := readyConditionOf(got)
	if condition == nil || condition.Status != "False" || condition.Reason != errorRateReason {
		t.Fatalf("Ready = %+v, want False with reason %s", condition, errorRateReason)
	}
	if want := "4 of 10 recent model calls failed (40%, threshold 40%)"; !strings.Contains(condition.Message, want) {
		t.Fatalf("message = %q, want it to contain %q", condition.Message, want)
	}
}

func TestModelClassReconciler_ErrorRateBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name         string
		threshold    int32
		calls        int32
		faults       int32
		latestFailed bool
		wantDegraded bool
	}{
		{"just below the threshold", 50, 10, 4, true, false},
		{"exactly at the threshold", 50, 10, 5, true, true},
		{"zero threshold degrades on one provider failure", 0, 5, 1, true, true},
		{"zero threshold still needs minimum evidence", 0, 4, 1, true, false},
		{"zero threshold does not degrade a healthy target", 0, 5, 0, false, false},
		{"zero threshold does not degrade a recovered target", 0, 5, 1, false, false},
		{"threshold of 100 needs every call to fail", 100, 10, 9, true, false},
		{"threshold of 100 with every call failing", 100, 10, 10, true, true},
		{"too few calls are no evidence", 10, 4, 4, true, false},
		{"minimum evidence is enough", 10, 5, 5, true, true},
		{"a recovered target does not degrade the class", 50, 10, 8, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRateFixture(t, newRateClass(tc.threshold, "error-rate-threshold"), rateModel("a", "default", rateClassName, "m1"))
			f.store.set([]store.ModelCallHealth{rateRecord(rateTarget("m1"), tc.calls, tc.faults, tc.latestFailed)}, nil)
			if degraded := f.reconcile().Status.Phase == v1alpha1.PhaseDegraded; degraded != tc.wantDegraded {
				t.Fatalf("degraded = %t, want %t", degraded, tc.wantDegraded)
			}
		})
	}
}

func TestModelClassReconciler_ErrorRateOmittedThresholdDefaultsToHalf(t *testing.T) {
	for _, tc := range []struct {
		faults       int32
		wantDegraded bool
	}{{1, false}, {4, false}, {5, true}} {
		t.Run(fmt.Sprint(tc.faults), func(t *testing.T) {
			class := newRateClass(0, "error-rate-threshold")
			class.Spec.HealthProbe.ErrorRateThreshold = nil
			f := newRateFixture(t, class, rateModel("a", "default", rateClassName, "m1"))
			f.store.set([]store.ModelCallHealth{rateRecord(rateTarget("m1"), 10, tc.faults, true)}, nil)
			got := f.reconcile()
			if degraded := got.Status.Phase == v1alpha1.PhaseDegraded; degraded != tc.wantDegraded {
				t.Fatalf("degraded = %t, want %t", degraded, tc.wantDegraded)
			}
			if tc.wantDegraded {
				condition := readyConditionOf(got)
				if condition == nil || !strings.Contains(condition.Message, "threshold 50%") {
					t.Fatalf("Ready = %+v, want the omitted threshold evaluated as 50%%", condition)
				}
			}
		})
	}
}

func TestModelClassReconciler_ErrorRateCountsOnlyTheClassesOwnTargets(t *testing.T) {
	terminated := rateModel("gone", "default", rateClassName, "gone-model")
	terminated.Status.Phase = v1alpha1.PhaseTerminated
	f := newRateFixture(t, newRateClass(50, "error-rate-threshold"),
		rateModel("a", "tenant-a", rateClassName, "m1"),
		rateModel("a-copy", "tenant-b", rateClassName, "m1"),
		rateModel("other", "default", "another-class", "other-model"),
		terminated,
	)
	bad := func(target string) store.ModelCallHealth { return rateRecord(target, 10, 10, true) }

	// Failures of a Model of another class, of a terminated Model and of the same
	// model on a different provider are not the class's.
	f.store.set([]store.ModelCallHealth{
		bad(rateTarget("other-model")),
		bad(rateTarget("gone-model")),
		bad(modelhealth.TargetKey("anthropic", "https://api.example/v1", "m1")),
	}, nil)
	if got := f.reconcile(); got.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("phase = %s, want Ready: only foreign targets are failing", got.Status.Phase)
	}

	// Two Models in different namespaces sharing one target count its calls once.
	f.store.set([]store.ModelCallHealth{rateRecord(rateTarget("m1"), 10, 5, true)}, nil)
	got := f.reconcile()
	if got.Status.Phase != v1alpha1.PhaseDegraded {
		t.Fatalf("phase = %s, want Degraded by the shared target", got.Status.Phase)
	}
	if message := readyConditionOf(got).Message; !strings.Contains(message, "5 of 10") {
		t.Fatalf("message = %q, want the shared target counted once", message)
	}
}

func TestModelClassReconciler_ErrorRateSumsTargets(t *testing.T) {
	f := newRateFixture(t, newRateClass(50, "error-rate-threshold"),
		rateModel("a", "default", rateClassName, "m1"),
		rateModel("b", "default", rateClassName, "m2"),
	)
	// Neither target reaches the minimum evidence alone, together they do and half
	// of the calls failed.
	f.store.set([]store.ModelCallHealth{
		rateRecord(rateTarget("m1"), 3, 3, true),
		rateRecord(rateTarget("m2"), 3, 0, false),
	}, nil)
	got := f.reconcile()
	if got.Status.Phase != v1alpha1.PhaseDegraded {
		t.Fatalf("phase = %s, want Degraded by the class's combined error rate", got.Status.Phase)
	}
	if message := readyConditionOf(got).Message; !strings.Contains(message, "3 of 6") {
		t.Fatalf("message = %q, want the targets combined", message)
	}
}

func TestModelClassReconciler_ErrorRateRecoversThroughTheStabilisationWindow(t *testing.T) {
	f := newRateFixture(t, newRateClass(50, "error-rate-threshold"), rateModel("a", "default", rateClassName, "m1"))
	f.store.set([]store.ModelCallHealth{rateRecord(rateTarget("m1"), 10, 8, true)}, nil)
	if got := f.reconcile(); got.Status.Phase != v1alpha1.PhaseDegraded {
		t.Fatalf("phase = %s, want Degraded", got.Status.Phase)
	}

	// The failing calls are still inside the window, but the latest call worked.
	f.store.set([]store.ModelCallHealth{rateRecord(rateTarget("m1"), 12, 8, false)}, nil)
	if got := f.reconcile(); got.Status.Phase != v1alpha1.PhaseDegraded || got.Status.ConsecutiveSuccesses != 1 {
		t.Fatalf("phase=%s successes=%d, want still Degraded after one clean probe of two", got.Status.Phase, got.Status.ConsecutiveSuccesses)
	}
	got := f.reconcile()
	if got.Status.Phase != v1alpha1.PhaseReady || got.Status.Health != "Healthy" {
		t.Fatalf("phase=%s health=%s, want Ready after two clean probes", got.Status.Phase, got.Status.Health)
	}

	// A failure while recovering restarts the window.
	f.store.set([]store.ModelCallHealth{rateRecord(rateTarget("m1"), 14, 10, true)}, nil)
	if got := f.reconcile(); got.Status.Phase != v1alpha1.PhaseDegraded {
		t.Fatalf("phase = %s, want Degraded again", got.Status.Phase)
	}
	f.store.set([]store.ModelCallHealth{rateRecord(rateTarget("m1"), 14, 10, false)}, nil)
	f.reconcile()
	f.store.set([]store.ModelCallHealth{rateRecord(rateTarget("m1"), 14, 10, true)}, nil)
	if got := f.reconcile(); got.Status.Phase != v1alpha1.PhaseDegraded || got.Status.ConsecutiveSuccesses != 0 {
		t.Fatalf("phase=%s successes=%d, want the stabilisation window reset by a failure", got.Status.Phase, got.Status.ConsecutiveSuccesses)
	}
}

func TestModelClassReconciler_ErrorRateKeepsTheClassWhenEvidenceCannotBeRead(t *testing.T) {
	for _, tc := range []struct {
		name  string
		phase v1alpha1.Phase
		want  v1alpha1.Phase
	}{
		{"a Ready class is not degraded by an outage", v1alpha1.PhaseReady, v1alpha1.PhaseReady},
		{"a Degraded class is not healed by an outage", v1alpha1.PhaseDegraded, v1alpha1.PhaseDegraded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			class := newRateClass(50, "error-rate-threshold")
			class.Status.Phase = tc.phase
			class.Status.Conditions = []v1alpha1.Condition{{Type: "Ready", Status: "False", Reason: errorRateReason, Message: "previous failure"}}
			if tc.phase == v1alpha1.PhaseReady {
				class.Status.Conditions[0] = v1alpha1.Condition{Type: "Ready", Status: "True", Reason: "Provisioned", Message: "previous health"}
			}
			f := newRateFixture(t, class, rateModel("a", "default", rateClassName, "m1"))
			f.store.set(nil, errors.New("store down"))
			// Two probes could recover a degraded class if the outage counted as health.
			f.reconcile()
			got := f.reconcile()
			if got.Status.Phase != tc.want {
				t.Fatalf("phase = %s, want %s", got.Status.Phase, tc.want)
			}
			condition := readyConditionOf(got)
			if tc.phase == v1alpha1.PhaseDegraded {
				if condition == nil || condition.Status != "False" || condition.Reason != "ErrorRateUnavailable" || condition.Message != "error-rate-threshold: recent model call health could not be read" {
					t.Fatalf("Ready = %+v, want False with the current outage reason and message", condition)
				}
				if got.Status.ConsecutiveSuccesses != 0 {
					t.Fatalf("successes = %d, want recovery blocked by the outage", got.Status.ConsecutiveSuccesses)
				}
			} else if condition == nil || condition.Status != "True" || condition.Reason != "Provisioned" || condition.Message != "previous health" {
				t.Fatalf("Ready = %+v, want the Ready class's condition unchanged by an outage", condition)
			}
		})
	}
}

// listFailingStore fails the Model listing that finds a class's targets.
type listFailingStore struct {
	*callHealthStore
	err error
}

func (s *listFailingStore) List(ctx context.Context, kind string, opts store.ListOptions) ([][]byte, error) {
	if kind == modelKind {
		return nil, s.err
	}
	return s.callHealthStore.List(ctx, kind, opts)
}

func TestModelClassReconciler_ErrorRateKeepsTheClassWhenModelsCannotBeListed(t *testing.T) {
	for _, phase := range []v1alpha1.Phase{v1alpha1.PhaseReady, v1alpha1.PhaseDegraded} {
		t.Run(string(phase), func(t *testing.T) {
			class := newRateClass(50, "error-rate-threshold")
			class.Status.Phase = phase
			class.Status.Conditions = []v1alpha1.Condition{{Type: "Ready", Status: "False", Reason: "ProbeFailed", Message: "previous failure"}}
			base := newCallHealthStore()
			storeModelClass(t, base.memStore, class)
			driver := &configurableMockDriver{name: "openai-compatible", probeResult: &cpi.HealthResult{Available: true}}
			r := NewModelClassReconciler(&listFailingStore{callHealthStore: base, err: errors.New("list failed")}, registryWith(driver), zaptest.NewLogger(t))
			reconcileModelClass(t, r, "default", rateClassName)
			reconcileModelClass(t, r, "default", rateClassName)
			got := getModelClass(t, base.memStore, "default", rateClassName)
			if got.Status.Phase != phase {
				t.Fatalf("phase = %s, want %s kept", got.Status.Phase, phase)
			}
			if phase == v1alpha1.PhaseDegraded {
				condition := readyConditionOf(got)
				if condition == nil || condition.Status != "False" || condition.Reason != "ErrorRateUnavailable" || condition.Message != "error-rate-threshold: recent model call health could not be read" {
					t.Fatalf("Ready = %+v, want False with the Model listing outage reason and message", condition)
				}
			}
		})
	}
}

func TestModelClassReconciler_ErrorRateNeedsModelsToAttributeCallsTo(t *testing.T) {
	f := newRateFixture(t, newRateClass(10, "error-rate-threshold"))
	f.store.set([]store.ModelCallHealth{rateRecord(rateTarget("m1"), 10, 10, true)}, nil)
	if got := f.reconcile(); got.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("phase = %s, want Ready: a class without Models has no calls of its own", got.Status.Phase)
	}
	if f.store.lists != 0 {
		t.Fatalf("health was read %d times, want no read for a class without targets", f.store.lists)
	}
}

func TestModelClassReconciler_ErrorRateIsOnlyForTheErrorRateProbe(t *testing.T) {
	for _, probeType := range []string{"inference-ping", "latency-threshold", ""} {
		t.Run("type "+probeType, func(t *testing.T) {
			f := newRateFixture(t, newRateClass(10, probeType), rateModel("a", "default", rateClassName, "m1"))
			f.store.set([]store.ModelCallHealth{rateRecord(rateTarget("m1"), 10, 10, true)}, nil)
			if got := f.reconcile(); got.Status.Phase != v1alpha1.PhaseReady {
				t.Fatalf("phase = %s, want Ready: only the error-rate-threshold probe reads call health", got.Status.Phase)
			}
		})
	}
}

func TestModelClassReconciler_ErrorRateStillRequiresAnAvailableEndpoint(t *testing.T) {
	f := newRateFixture(t, newRateClass(50, "error-rate-threshold"), rateModel("a", "default", rateClassName, "m1"))
	f.driver.probeResult = &cpi.HealthResult{Available: false, Reason: "Down", Message: "provider down"}
	got := f.reconcile()
	if got.Status.Phase != v1alpha1.PhaseDegraded {
		t.Fatalf("phase = %s, want Degraded by the failing availability probe", got.Status.Phase)
	}
	if condition := readyConditionOf(got); condition.Reason != "ProbeFailed" {
		t.Fatalf("reason = %q, want the availability failure to take precedence", condition.Reason)
	}
}

func TestModelClassReconciler_ErrorRateWithoutSharedHealthChecksAvailabilityOnly(t *testing.T) {
	s := newMemStore()
	driver := &configurableMockDriver{name: "openai-compatible", probeResult: &cpi.HealthResult{Available: true}}
	r := NewModelClassReconciler(s, registryWith(driver), zaptest.NewLogger(t))
	if r.callHealth != nil {
		t.Fatal("a store without call health must not enable the error rate")
	}
	storeModelClass(t, s, newRateClass(1, "error-rate-threshold"))
	reconcileModelClass(t, r, "default", rateClassName)
	if got := getModelClass(t, s, "default", rateClassName); got.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("phase = %s, want Ready", got.Status.Phase)
	}

	shared := newCallHealthStore()
	if NewModelClassReconciler(shared, registryWith(driver), zaptest.NewLogger(t)).callHealth == nil {
		t.Fatal("a store that shares call health must enable the error rate")
	}
	r.SetCallHealth(shared)
	if r.callHealth == nil {
		t.Fatal("SetCallHealth() must enable the error rate")
	}
}

func TestModelClassReconciler_ValidatesTheErrorRateThreshold(t *testing.T) {
	r := NewModelClassReconciler(newMemStore(), cpi.NewRegistry(), zaptest.NewLogger(t))
	for _, tc := range []struct {
		threshold int32
		wantValid bool
	}{{-1, false}, {0, true}, {1, true}, {100, true}, {101, false}} {
		t.Run(fmt.Sprint(tc.threshold), func(t *testing.T) {
			class := newRateClass(tc.threshold, "error-rate-threshold")
			valid, reason, message := r.validateConfig(&class)
			if valid != tc.wantValid {
				t.Fatalf("valid = %t (%s: %s), want %t", valid, reason, message, tc.wantValid)
			}
			if !valid && (reason != "HealthProbeInvalid" || !strings.Contains(message, "errorRateThreshold")) {
				t.Fatalf("reason=%q message=%q, want HealthProbeInvalid naming errorRateThreshold", reason, message)
			}
		})
	}
	class := newRateClass(0, "error-rate-threshold")
	class.Spec.HealthProbe.ErrorRateThreshold = nil
	if valid, reason, message := r.validateConfig(&class); !valid {
		t.Fatalf("an omitted threshold must be valid: %s: %s", reason, message)
	}
}

func TestModelClassReconciler_ValidatesUpdatedHealthProbesBeforeEvaluation(t *testing.T) {
	for _, phase := range []v1alpha1.Phase{v1alpha1.PhaseReady, v1alpha1.PhaseDegraded, v1alpha1.PhaseValidating} {
		for _, tc := range []struct {
			name   string
			change func(*v1alpha1.HealthProbeSpec)
		}{
			{"negative threshold", func(probe *v1alpha1.HealthProbeSpec) { *probe.ErrorRateThreshold = -1 }},
			{"threshold above 100", func(probe *v1alpha1.HealthProbeSpec) { *probe.ErrorRateThreshold = 101 }},
			{"missing endpoint", func(probe *v1alpha1.HealthProbeSpec) { probe.Endpoint = "" }},
			{"invalid endpoint", func(probe *v1alpha1.HealthProbeSpec) { probe.Endpoint = "not a URI" }},
		} {
			t.Run(string(phase)+"/"+tc.name, func(t *testing.T) {
				class := newRateClass(50, "error-rate-threshold")
				class.Status.Phase = phase
				f := newRateFixture(t, class, rateModel("a", "default", rateClassName, "m1"))
				f.store.set([]store.ModelCallHealth{rateRecord(rateTarget("m1"), 5, 5, true)}, nil)

				class = getModelClass(t, f.store.memStore, "default", rateClassName)
				tc.change(class.Spec.HealthProbe)
				if err := f.r.updateModelClass(context.Background(), &class); err != nil {
					t.Fatal(err)
				}
				got := f.reconcile()
				condition := readyConditionOf(got)
				if got.Status.Phase != v1alpha1.PhaseFailed || got.Status.Provisioned || got.Status.Health != "" {
					t.Fatalf("status = %+v, want invalid configuration to fail before probing", got.Status)
				}
				if condition == nil || condition.Status != "False" || condition.Reason != "HealthProbeInvalid" {
					t.Fatalf("Ready = %+v, want False with HealthProbeInvalid", condition)
				}
				if f.store.lists != 0 || got.Status.LastProbeTime != "" {
					t.Fatal("invalid probe configuration must not be evaluated")
				}

				corrected := int32(0)
				got.Spec.HealthProbe.ErrorRateThreshold = &corrected
				got.Spec.HealthProbe.Endpoint = "https://api.example/health"
				if err := f.r.updateModelClass(context.Background(), &got); err != nil {
					t.Fatal(err)
				}
				if got := f.reconcile(); got.Status.Phase != v1alpha1.PhaseValidating {
					t.Fatalf("phase = %s, want corrected configuration to be revalidated", got.Status.Phase)
				}
				if got := f.reconcile(); got.Status.Phase != v1alpha1.PhaseReady {
					t.Fatalf("phase = %s, want Ready after correcting configuration", got.Status.Phase)
				}
				if got := f.reconcile(); got.Status.Phase != v1alpha1.PhaseDegraded {
					t.Fatalf("phase = %s, want the corrected zero threshold evaluated", got.Status.Phase)
				}
			})
		}
	}
}
