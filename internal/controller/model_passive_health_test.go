// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/modelhealth"
	"github.com/kognition-ai/kognition/internal/store"
)

// callHealthStore adds passive model health to the in-memory test store.
type callHealthStore struct {
	*memStore
	mu      sync.Mutex
	records []store.ModelCallHealth
	err     error
	lists   int
}

func newCallHealthStore() *callHealthStore { return &callHealthStore{memStore: newMemStore()} }

func (s *callHealthStore) ReplaceModelCallHealth(context.Context, string, []store.ModelCallHealth) error {
	return nil
}

func (s *callHealthStore) ListModelCallHealth(context.Context, time.Duration) ([]store.ModelCallHealth, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lists++
	return append([]store.ModelCallHealth(nil), s.records...), s.err
}

func (s *callHealthStore) set(records []store.ModelCallHealth, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records, s.err = records, err
}

const (
	passiveEndpoint = "https://api.example/v1"
	passiveModelID  = "claude"
)

func passiveTarget(provider string) string {
	return modelhealth.TargetKey(provider, passiveEndpoint, passiveModelID)
}

func faultRecord(target, reason string) store.ModelCallHealth {
	now := time.Now()
	return store.ModelCallHealth{Target: target, Calls: 3, Faults: 3, ConsecutiveFaults: 3, LastFaultReason: reason, LastFaultAt: now}
}

func recoveryRecord(target string, successes int32) store.ModelCallHealth {
	now := time.Now()
	return store.ModelCallHealth{
		Target: target, Calls: 3 + successes, Faults: 3, ConsecutiveSuccesses: successes,
		LastFaultReason: cpi.ReasonTimeout, LastFaultAt: now.Add(-time.Minute), LastSuccessAt: now,
	}
}

func newPassiveModel(name, provider string) v1alpha1.Model {
	return v1alpha1.Model{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Model"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: name, Namespace: "default"},
		Spec:       v1alpha1.ModelSpec{Provider: provider, ModelID: passiveModelID, Endpoint: passiveEndpoint, Capabilities: []string{"chat"}},
	}
}

func readyCondition(t *testing.T, model v1alpha1.Model) *v1alpha1.Condition {
	t.Helper()
	condition := findModelCondition(model, "Ready")
	if condition == nil {
		t.Fatal("expected a Ready condition")
	}
	return condition
}

func TestModelReconciler_PassiveHealthDegradesAModelWhoseProbePasses(t *testing.T) {
	s := newCallHealthStore()
	r := NewModelReconciler(s, testRegistry(), zaptest.NewLogger(t))
	storeModel(t, s.memStore, newPassiveModel("claude", "anthropic"))

	reconcileModel(t, r, "default", "claude")
	if got := getModel(t, s.memStore, "default", "claude"); !got.Status.Available || got.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("precondition: a model without failing calls must be Ready, got available=%t phase=%q", got.Status.Available, got.Status.Phase)
	}

	s.set([]store.ModelCallHealth{faultRecord(passiveTarget("anthropic"), cpi.ReasonRateLimited)}, nil)
	reconcileModel(t, r, "default", "claude")

	got := getModel(t, s.memStore, "default", "claude")
	if got.Status.Available || got.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("available=%t phase=%q, want an unavailable Failed model", got.Status.Available, got.Status.Phase)
	}
	condition := readyCondition(t, got)
	if condition.Status != "False" || condition.Reason != cpi.ReasonRateLimited || !strings.HasPrefix(condition.Message, modelhealth.MessagePrefix) {
		t.Fatalf("Ready = %+v, want False with reason RateLimited and the passive health message", condition)
	}
	if len(got.Spec.Capabilities) != 1 || got.Spec.Capabilities[0] != "chat" {
		t.Errorf("capabilities = %v, want the spec untouched", got.Spec.Capabilities)
	}
}

func TestModelReconciler_PassiveHealthIgnoresEvidenceForOtherTargets(t *testing.T) {
	s := newCallHealthStore()
	r := NewModelReconciler(s, testRegistry(), zaptest.NewLogger(t))
	storeModel(t, s.memStore, newPassiveModel("claude", "anthropic"))
	s.set([]store.ModelCallHealth{
		faultRecord(modelhealth.TargetKey("anthropic", passiveEndpoint, "another-model"), cpi.ReasonRateLimited),
		faultRecord(modelhealth.TargetKey("anthropic", "https://other.example", passiveModelID), cpi.ReasonRateLimited),
		faultRecord(modelhealth.TargetKey("ollama", passiveEndpoint, passiveModelID), cpi.ReasonRateLimited),
	}, nil)

	reconcileModel(t, r, "default", "claude")
	if got := getModel(t, s.memStore, "default", "claude"); !got.Status.Available || got.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("available=%t phase=%q, want evidence about other targets ignored", got.Status.Available, got.Status.Phase)
	}
}

func TestModelReconciler_PassiveHealthMergesEvidenceFromAllReporters(t *testing.T) {
	s := newCallHealthStore()
	r := NewModelReconciler(s, testRegistry(), zaptest.NewLogger(t))
	storeModel(t, s.memStore, newPassiveModel("claude", "anthropic"))
	target := passiveTarget("anthropic")
	now := time.Now()
	one := store.ModelCallHealth{Target: target, Calls: 2, Faults: 2, ConsecutiveFaults: 2, LastFaultReason: cpi.ReasonEndpointError, LastFaultAt: now}
	s.set([]store.ModelCallHealth{one, one}, nil)

	reconcileModel(t, r, "default", "claude")
	if got := getModel(t, s.memStore, "default", "claude"); got.Status.Available {
		t.Fatal("two replicas that each saw two failures must degrade the model")
	}
}

func TestModelReconciler_PassiveHealthResolvesProviderAliases(t *testing.T) {
	s := newCallHealthStore()
	r := NewModelReconciler(s, testRegistry(), zaptest.NewLogger(t))
	storeModel(t, s.memStore, newPassiveModel("aliased", "azure-openai"))
	s.set([]store.ModelCallHealth{faultRecord(passiveTarget("openai-compatible"), cpi.ReasonTimeout)}, nil)

	reconcileModel(t, r, "default", "aliased")
	if got := getModel(t, s.memStore, "default", "aliased"); got.Status.Available {
		t.Fatal("an aliased provider must share health with its canonical driver")
	}
}

func TestModelReconciler_PassiveHealthUsesTheModelClassProvisioner(t *testing.T) {
	s := newCallHealthStore()
	r := NewModelReconciler(s, testRegistry(), zaptest.NewLogger(t))
	storeModelClass(t, s.memStore, v1alpha1.ModelClass{
		TypeMeta:   v1alpha1.TypeMeta{Kind: "ModelClass", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "claude-class"},
		Spec:       v1alpha1.ModelClassSpec{Provisioner: "anthropic"},
	})
	model := newPassiveModel("classed", "")
	model.Spec.ClassName = "claude-class"
	storeModel(t, s.memStore, model)
	s.set([]store.ModelCallHealth{faultRecord(passiveTarget("anthropic"), cpi.ReasonTimeout)}, nil)

	reconcileModel(t, r, "default", "classed")
	if got := getModel(t, s.memStore, "default", "classed"); got.Status.Available {
		t.Fatal("a model served through a ModelClass must share health with the class provisioner's target")
	}
}

func TestModelReconciler_ActiveProbeFailureTakesPrecedenceOverPassiveHealth(t *testing.T) {
	s := newCallHealthStore()
	driver := &configurableMockDriver{name: "anthropic", probeResult: &cpi.HealthResult{Available: false, Reason: "ProbeTimeout", Message: "probe timed out"}}
	r := NewModelReconciler(s, registryWith(driver), zaptest.NewLogger(t))
	storeModel(t, s.memStore, newPassiveModel("claude", "anthropic"))
	s.set([]store.ModelCallHealth{faultRecord(passiveTarget("anthropic"), cpi.ReasonRateLimited)}, nil)

	reconcileModel(t, r, "default", "claude")
	condition := readyCondition(t, getModel(t, s.memStore, "default", "claude"))
	if condition.Reason != "ProbeTimeout" || strings.HasPrefix(condition.Message, modelhealth.MessagePrefix) {
		t.Fatalf("Ready = %+v, want the active probe's failure", condition)
	}
}

func TestModelReconciler_PassiveDegradationIsStickyUntilEnoughSuccesses(t *testing.T) {
	s := newCallHealthStore()
	r := NewModelReconciler(s, testRegistry(), zaptest.NewLogger(t))
	storeModel(t, s.memStore, newPassiveModel("claude", "anthropic"))
	target := passiveTarget("anthropic")

	s.set([]store.ModelCallHealth{faultRecord(target, cpi.ReasonTimeout)}, nil)
	reconcileModel(t, r, "default", "claude")
	if getModel(t, s.memStore, "default", "claude").Status.Available {
		t.Fatal("precondition: the model must be degraded")
	}

	s.set([]store.ModelCallHealth{recoveryRecord(target, 2)}, nil)
	reconcileModel(t, r, "default", "claude")
	if getModel(t, s.memStore, "default", "claude").Status.Available {
		t.Fatal("two successes must not end the degradation")
	}

	s.set([]store.ModelCallHealth{recoveryRecord(target, 3)}, nil)
	reconcileModel(t, r, "default", "claude")
	got := getModel(t, s.memStore, "default", "claude")
	if !got.Status.Available || got.Status.Phase != v1alpha1.PhaseReady || strings.HasPrefix(readyCondition(t, got).Message, modelhealth.MessagePrefix) {
		t.Fatalf("available=%t phase=%q condition=%+v, want a recovered Ready model", got.Status.Available, got.Status.Phase, readyCondition(t, got))
	}

	// With no evidence left at all (the failures aged out), a degraded model also recovers.
	s.set([]store.ModelCallHealth{faultRecord(target, cpi.ReasonTimeout)}, nil)
	reconcileModel(t, r, "default", "claude")
	s.set(nil, nil)
	reconcileModel(t, r, "default", "claude")
	if !getModel(t, s.memStore, "default", "claude").Status.Available {
		t.Fatal("a model must recover once its failures leave the window")
	}
}

func TestModelReconciler_PassiveHealthKeepsThePreviousVerdictWhenTheStoreFails(t *testing.T) {
	s := newCallHealthStore()
	r := NewModelReconciler(s, testRegistry(), zaptest.NewLogger(t))
	storeModel(t, s.memStore, newPassiveModel("claude", "anthropic"))
	target := passiveTarget("anthropic")
	outage := errors.New("health store unavailable")

	s.set(nil, outage)
	reconcileModel(t, r, "default", "claude")
	if !getModel(t, s.memStore, "default", "claude").Status.Available {
		t.Fatal("a monitoring outage must not degrade a healthy model")
	}

	s.set([]store.ModelCallHealth{faultRecord(target, cpi.ReasonRateLimited)}, nil)
	reconcileModel(t, r, "default", "claude")
	s.set(nil, outage)
	reconcileModel(t, r, "default", "claude")
	got := getModel(t, s.memStore, "default", "claude")
	if got.Status.Available || readyCondition(t, got).Reason != cpi.ReasonRateLimited {
		t.Fatalf("available=%t condition=%+v, want the degraded verdict kept during the outage", got.Status.Available, readyCondition(t, got))
	}
}

func TestModelReconciler_PassiveDegradationDoesNotChurnTheStore(t *testing.T) {
	s := newCallHealthStore()
	r := NewModelReconciler(s, testRegistry(), zaptest.NewLogger(t))
	storeModel(t, s.memStore, newPassiveModel("claude", "anthropic"))
	target := passiveTarget("anthropic")
	s.set([]store.ModelCallHealth{faultRecord(target, cpi.ReasonRateLimited)}, nil)
	reconcileModel(t, r, "default", "claude")
	revision := getModel(t, s.memStore, "default", "claude").ObjectMeta.ResourceVersion

	// More failures change the counts but not the message, so nothing is written.
	worse := faultRecord(target, cpi.ReasonRateLimited)
	worse.Calls, worse.Faults, worse.ConsecutiveFaults = 40, 40, 40
	s.set([]store.ModelCallHealth{worse}, nil)
	reconcileModel(t, r, "default", "claude")
	if got := getModel(t, s.memStore, "default", "claude").ObjectMeta.ResourceVersion; got != revision {
		t.Fatalf("resourceVersion changed from %d to %d while the degradation continued", revision, got)
	}
}

func TestModelReconciler_SetCallHealthEnablesPassiveHealthForAnyStore(t *testing.T) {
	s := newMemStore()
	r := NewModelReconciler(s, testRegistry(), zaptest.NewLogger(t))
	storeModel(t, s, newPassiveModel("claude", "anthropic"))

	health := newCallHealthStore()
	health.set([]store.ModelCallHealth{faultRecord(passiveTarget("anthropic"), cpi.ReasonTimeout)}, nil)
	r.SetCallHealth(health)

	reconcileModel(t, r, "default", "claude")
	if got := getModel(t, s, "default", "claude"); got.Status.Available || readyCondition(t, got).Reason != cpi.ReasonTimeout {
		t.Fatalf("available=%t, want the model degraded by the health store set after construction", got.Status.Available)
	}
}

func TestModelReconciler_PassiveHealthIsOptionalForTheStore(t *testing.T) {
	r := NewModelReconciler(newMemStore(), testRegistry(), zaptest.NewLogger(t))
	if r.callHealth != nil {
		t.Fatal("a store without passive health support must not enable it")
	}
	if verdict := r.passiveVerdict(context.Background(), "default", "anthropic", &v1alpha1.Model{}); verdict.Degraded {
		t.Fatalf("passiveVerdict() = %+v, want healthy without a health store", verdict)
	}
}

func TestModelPassiveDegradationRecognizesOnlyPassiveReadyFalseConditions(t *testing.T) {
	passive := v1alpha1.Model{Status: v1alpha1.ModelStatus{Conditions: []v1alpha1.Condition{
		{Type: "Ready", Status: "False", Reason: cpi.ReasonTimeout, Message: modelhealth.MessagePrefix + "recent model calls are failing with Timeout"},
	}}}
	verdict, ok := modelPassiveDegradation(&passive)
	if !ok || verdict.Reason != cpi.ReasonTimeout || !verdict.Degraded {
		t.Fatalf("modelPassiveDegradation() = %+v, %t, want the passive verdict", verdict, ok)
	}

	for name, model := range map[string]v1alpha1.Model{
		"no condition": {},
		"active failure": {Status: v1alpha1.ModelStatus{Conditions: []v1alpha1.Condition{
			{Type: "Ready", Status: "False", Reason: "ProbeFailed", Message: "probe failed"},
		}}},
		"ready true": {Status: v1alpha1.ModelStatus{Conditions: []v1alpha1.Condition{
			{Type: "Ready", Status: "True", Reason: "ProbeOK", Message: modelhealth.MessagePrefix + "stale text"},
		}}},
	} {
		if _, ok := modelPassiveDegradation(&model); ok {
			t.Errorf("%s: reported as passively degraded", name)
		}
	}
}
