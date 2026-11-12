// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/admission"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/kognition-ai/kognition/internal/store"
)

type modelClassErrorStore struct {
	store.Store
	getErr  error
	listErr error
}

type candidateCanaryDriver struct {
	mockDriver
	response    string
	calls       int
	request     *cpi.CompletionRequest
	onComplete  func()
	providerErr error
}

func (driver *candidateCanaryDriver) CountInputTokens(*cpi.CompletionRequest) (int, error) {
	return 12, nil
}

type candidateReservationStore struct {
	*memStore
	reservationMu sync.Mutex
	reservations  map[string]store.TokenReservation
	dispositions  map[string]store.CanaryReservationDisposition
}

type candidateUsageStore struct {
	*recordingCPIUsageStore
	usageMu sync.Mutex
}

type candidateReplayLedger struct {
	store.ModelDispatchStore
	record store.ModelDispatchRecord
}

func (ledger *candidateReplayLedger) GetModelDispatch(context.Context, string, string) (store.ModelDispatchRecord, error) {
	return ledger.record, nil
}

func (storage *candidateUsageStore) ApplyUsageEvent(ctx context.Context, event observability.CostRecord) (bool, error) {
	storage.usageMu.Lock()
	defer storage.usageMu.Unlock()
	if storage.err != nil {
		return false, storage.err
	}
	for _, current := range storage.events {
		if current.ID != event.ID {
			continue
		}
		before, err := json.Marshal(current)
		if err != nil {
			return false, err
		}
		after, err := json.Marshal(event)
		if err != nil {
			return false, err
		}
		if !bytes.Equal(before, after) {
			return false, store.ErrConflict
		}
		return false, nil
	}
	return storage.recordingCPIUsageStore.ApplyUsageEvent(ctx, event)
}

func (storage *candidateReservationStore) CreateCanaryReservation(ctx context.Context, reservation store.TokenReservation) (bool, error) {
	storage.reservationMu.Lock()
	defer storage.reservationMu.Unlock()
	if _, exists := storage.dispositions[reservation.ID]; exists {
		return false, store.ErrConflict
	}
	if current, exists := storage.reservations[reservation.ID]; exists {
		if current != reservation {
			return false, store.ErrConflict
		}
		return false, nil
	}
	quota := NewCognitiveQuotaReconciler(storage.memStore, zap.NewNop())
	if err := quota.CheckTokenQuota(ctx, reservation.Namespace, reservation.ReservedInput, reservation.ReservedOutput); err != nil {
		return false, store.ErrQuotaExceeded
	}
	if err := quota.IncrementTokenUsage(ctx, reservation.Namespace, reservation.ReservedInput, reservation.ReservedOutput); err != nil {
		return false, err
	}
	storage.reservations[reservation.ID] = reservation
	return true, nil
}

func (storage *candidateReservationStore) SettleCanaryReservation(ctx context.Context, id string, input, output int64) (store.CanaryReservationDisposition, error) {
	storage.reservationMu.Lock()
	defer storage.reservationMu.Unlock()
	if disposition, exists := storage.dispositions[id]; exists {
		return disposition, nil
	}
	reservation, exists := storage.reservations[id]
	if !exists {
		return store.CanaryReservationDisposition{}, store.ErrNotFound
	}
	quota := NewCognitiveQuotaReconciler(storage.memStore, zap.NewNop())
	if err := quota.IncrementTokenUsage(ctx, reservation.Namespace, input-reservation.ReservedInput, output-reservation.ReservedOutput); err != nil {
		return store.CanaryReservationDisposition{}, err
	}
	disposition := store.CanaryReservationDisposition{ReservationID: id, State: store.CanaryReservationSettled, InputTokens: input, OutputTokens: output}
	delete(storage.reservations, id)
	storage.dispositions[id] = disposition
	return disposition, nil
}

func (storage *candidateReservationStore) ReleaseCanaryReservation(ctx context.Context, id string) (store.CanaryReservationDisposition, error) {
	disposition, err := storage.SettleCanaryReservation(ctx, id, 0, 0)
	if err != nil {
		return disposition, err
	}
	storage.reservationMu.Lock()
	defer storage.reservationMu.Unlock()
	disposition.State = store.CanaryReservationReleased
	storage.dispositions[id] = disposition
	return disposition, nil
}

func (driver *candidateCanaryDriver) Complete(ctx context.Context, request *cpi.CompletionRequest) (*cpi.CompletionResponse, error) {
	if _, bounded := ctx.Deadline(); !bounded {
		return nil, fmt.Errorf("canary deadline missing")
	}
	driver.calls++
	driver.request = request
	if driver.onComplete != nil {
		driver.onComplete()
	}
	if driver.providerErr != nil {
		return nil, driver.providerErr
	}
	return &cpi.CompletionResponse{Content: driver.response, Usage: cpi.Usage{PromptTokens: 12, CompletionTokens: 6, TotalTokens: 18, Reported: true}}, nil
}

func TestModelClassSignedCandidateCanaryPromotionAndRollback(test *testing.T) {
	for _, scenario := range []struct {
		name          string
		accepted      bool
		quotaResource string
	}{
		{name: "accepted-true", accepted: true},
		{name: "accepted-false"},
		{name: "cost-exhausted", accepted: true, quotaResource: "cost"},
		{name: "tier-exhausted", accepted: true, quotaResource: "tier"},
		{name: "tokens-exhausted", accepted: true, quotaResource: "tokens"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			accepted := scenario.accepted
			storage := &candidateReservationStore{memStore: newMemStore(), reservations: map[string]store.TokenReservation{}, dispositions: map[string]store.CanaryReservationDisposition{}}
			key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				test.Fatal(err)
			}
			public, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
			if err != nil {
				test.Fatal(err)
			}
			trust, err := json.Marshal(map[string]map[string]string{"tenant": {"release": string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: public}))}})
			if err != nil {
				test.Fatal(err)
			}
			test.Setenv("KOGNITION_ARTIFACT_SIGNING_KEYS_JSON", string(trust))
			test.Setenv("KOGNITION_ARTIFACT_SIGNING_KEYS_FILE", "")
			candidate := v1alpha1.Model{ObjectMeta: v1alpha1.ObjectMeta{Name: "candidate", Namespace: "tenant", UID: "candidate-uid", ResourceVersion: 7}, Spec: v1alpha1.ModelSpec{ClassName: "class", Provider: "openai-compatible", ModelID: "candidate-model-id", Endpoint: "https://candidate.example/v1"}}
			encoded, err := json.Marshal(candidate)
			if err != nil {
				test.Fatal(err)
			}
			payload, err := admission.ArtifactSignaturePayload("Model", "tenant", "candidate", encoded)
			if err != nil {
				test.Fatal(err)
			}
			hash := sha256.Sum256(payload)
			signed, err := ecdsa.SignASN1(rand.Reader, key, hash[:])
			if err != nil {
				test.Fatal(err)
			}
			envelope, err := json.Marshal(map[string]interface{}{"version": 1, "keyID": "release", "signature": base64.StdEncoding.EncodeToString(signed)})
			if err != nil {
				test.Fatal(err)
			}
			candidate.Spec.Signature = string(envelope)
			encoded, err = json.Marshal(candidate)
			if err != nil {
				test.Fatal(err)
			}
			if err := storage.Create(context.Background(), "Model", "tenant", "candidate", encoded); err != nil {
				test.Fatal(err)
			}
			stored, err := storage.Get(context.Background(), "Model", "tenant", "candidate")
			if err != nil || json.Unmarshal(stored, &candidate) != nil {
				test.Fatal("stored candidate unavailable")
			}
			digest, err := runtimeauth.ModelConfigurationDigest(&candidate)
			if err != nil {
				test.Fatal(err)
			}
			binding := v1alpha1.ModelVersionBinding{Namespace: "tenant", ModelName: "candidate", ModelUID: "candidate-uid", ModelResourceVersion: candidate.ObjectMeta.ResourceVersion, SpecDigest: digest}
			previous := v1alpha1.ModelVersionBinding{Namespace: "tenant", ModelName: "previous", ModelUID: "previous-uid", ModelResourceVersion: 4, SpecDigest: "old-digest"}
			class := v1alpha1.ModelClass{ObjectMeta: v1alpha1.ObjectMeta{Name: "class", Finalizers: []string{modelClassFinalizer}}, Spec: v1alpha1.ModelClassSpec{Provisioner: "openai-compatible", VersionPin: "candidate", VersionCandidates: map[string]v1alpha1.ModelVersionBinding{"candidate": binding}}, Status: v1alpha1.ModelClassStatus{Phase: v1alpha1.PhaseReady, ActiveVersion: "previous", ActiveBinding: &previous}}
			if scenario.quotaResource != "" {
				quota := v1alpha1.CognitiveQuota{ObjectMeta: v1alpha1.ObjectMeta{Name: "candidate-limit", Namespace: "tenant"}, Spec: v1alpha1.CognitiveQuotaSpec{CostBudget: &v1alpha1.CostBudget{}}, Status: v1alpha1.CognitiveQuotaStatus{CostUsage: &v1alpha1.QuotaCostUsage{}}}
				if scenario.quotaResource == "cost" {
					quota.Spec.CostBudget.MaxCostPerPeriod = 10
					quota.Status.CostUsage.TotalCost = 10
				} else if scenario.quotaResource == "tokens" {
					quota.Spec.TokenBudget = &v1alpha1.TokenBudget{MaxOutputTokens: 16}
					quota.Status.TokenUsage = &v1alpha1.QuotaTokenUsage{OutputTokens: 16, TotalTokens: 16}
				} else {
					class.Spec.CostTier = "premium"
					quota.Spec.CostBudget.MaxModelTierSeconds = map[string]int64{"premium": 10}
					quota.Status.CostUsage.ModelTierSeconds = map[string]int64{"premium": 10}
				}
				createQuota(test, storage.memStore, quota)
			}
			storeModelClass(test, storage.memStore, class)
			driver := &candidateCanaryDriver{mockDriver: mockDriver{name: "openai-compatible"}, response: "KOGNITION_MODEL_CANARY_BAD"}
			if accepted {
				driver.response = "KOGNITION_MODEL_CANARY_OK"
			}
			registry := cpi.NewRegistry()
			registry.MustRegister(driver)
			reconciler := NewModelClassReconciler(storage, registry, zaptest.NewLogger(test))
			accounting := &candidateUsageStore{recordingCPIUsageStore: &recordingCPIUsageStore{memStore: storage.memStore}}
			audit := observability.NewCognitiveAuditLog(&memoryGrantAuditStore{})
			reconciler.SetCanaryAccounting(storage, accounting, audit)
			if scenario.quotaResource == "" {
				driver.onComplete = func() {
					inFlight := getModelClass(test, storage.memStore, "", "class")
					overlapping := NewModelClassReconciler(storage, registry, zaptest.NewLogger(test))
					overlapping.SetCanaryAccounting(storage, accounting, audit)
					if err := overlapping.validateCandidate(context.Background(), &inFlight); !errors.Is(err, ErrDispatchAccountingUncertain) {
						test.Fatalf("overlapping claimant was not fenced: %v", err)
					}
				}
			}
			reconcileModelClass(test, reconciler, "", "class")
			driver.onComplete = nil
			observed := getModelClass(test, storage.memStore, "", "class")
			if scenario.quotaResource != "" {
				if driver.calls != 0 || len(accounting.events) != 0 {
					test.Fatal("exhausted quota allowed a paid canary or usage charge")
				}
				if observed.Status.ActiveVersion != "previous" || observed.Status.ActiveBinding == nil || *observed.Status.ActiveBinding != previous || observed.Status.Phase != v1alpha1.PhaseDegraded {
					test.Fatalf("quota denial replaced the previous serving binding: %+v", observed.Status)
				}
				restarted := NewModelClassReconciler(storage, registry, zaptest.NewLogger(test))
				restarted.SetCanaryAccounting(storage, accounting, audit)
				reconcileModelClass(test, restarted, "", "class")
				if driver.calls != 0 || len(accounting.events) != 0 {
					test.Fatal("restart bypassed exhausted candidate quota")
				}
				if scenario.quotaResource == "tokens" {
					body, err := storage.Get(context.Background(), "CognitiveQuota", "tenant", "candidate-limit")
					if err != nil {
						test.Fatal(err)
					}
					var quota v1alpha1.CognitiveQuota
					if err := json.Unmarshal(body, &quota); err != nil {
						test.Fatal(err)
					}
					quota.Spec.TokenBudget.MaxOutputTokens = 64
					body, err = json.Marshal(quota)
					if err != nil {
						test.Fatal(err)
					}
					if err := storage.Update(context.Background(), "CognitiveQuota", "tenant", quota.Name, body, quota.ResourceVersion); err != nil {
						test.Fatal(err)
					}
					current := getModelClass(test, storage.memStore, "", "class")
					current.Spec.VersionPin = "candidate"
					body, err = json.Marshal(current)
					if err != nil {
						test.Fatal(err)
					}
					if err := storage.Update(context.Background(), "ModelClass", "", "class", body, current.ResourceVersion); err != nil {
						test.Fatal(err)
					}
					reconcileModelClass(test, restarted, "", "class")
					if driver.calls != 0 || len(storage.reservations) != 0 {
						test.Fatal("restored quota replayed the same denied candidate claim")
					}
				}
				return
			}
			if driver.calls != 1 || driver.request.Model != "candidate-model-id" || driver.request.Endpoint != "https://candidate.example/v1" || driver.request.Parameters["max_tokens"] != "16" || len(driver.request.Tools) != 0 {
				test.Fatal("promotion did not probe the exact bounded candidate")
			}
			if accepted {
				if observed.Status.ActiveVersion != "candidate" || observed.Status.ActiveBinding == nil || *observed.Status.ActiveBinding != binding || observed.Status.Phase != v1alpha1.PhaseReady {
					test.Fatalf("candidate was not promoted: %+v", observed.Status)
				}
			} else if observed.Status.ActiveVersion != "previous" || observed.Status.ActiveBinding == nil || *observed.Status.ActiveBinding != previous || observed.Status.Phase != v1alpha1.PhaseDegraded {
				test.Fatalf("failed candidate did not preserve serving binding: %+v", observed.Status)
			}
			if len(accounting.events) != 1 || accounting.events[0].CompletionTokens != 6 || accounting.events[0].Workspace != "tenant" {
				test.Fatal("actual candidate canary usage was not accounted exactly once")
			}
			if len(storage.reservations) != 0 || len(storage.dispositions) != 1 {
				test.Fatal("known candidate usage did not settle its protected reservation")
			}
			for _, disposition := range storage.dispositions {
				if disposition.State != store.CanaryReservationSettled || disposition.InputTokens != 12 || disposition.OutputTokens != 6 {
					test.Fatal("candidate reservation settlement did not match actual usage")
				}
			}
			operation := accounting.events[0].ID
			corruptedUsage := accounting.events[0]
			corruptedUsage.SessionID = "unrelated-session"
			corrupted, err := json.Marshal(candidateCanaryResult{Version: 2, Passed: accepted, Usage: &corruptedUsage})
			if err != nil {
				test.Fatal(err)
			}
			for _, response := range []json.RawMessage{nil, json.RawMessage(`{"version":3,"passed":true}`), json.RawMessage(`{"version":2,"passed":true}`), json.RawMessage(`null`), json.RawMessage(`{`), json.RawMessage(bytes.Repeat([]byte(" "), 4097)), corrupted} {
				replay := NewModelClassReconciler(storage, registry, zaptest.NewLogger(test))
				replay.SetCanaryAccounting(&candidateReplayLedger{ModelDispatchStore: storage, record: store.ModelDispatchRecord{SessionID: operation, Response: response}}, accounting, audit)
				if err := replay.validateCandidate(context.Background(), &class); !errors.Is(err, ErrDispatchAccountingUncertain) || driver.calls != 1 || len(accounting.events) != 1 {
					test.Fatalf("invalid retained result was replayed or accounted: %v", err)
				}
			}
			legacy, err := json.Marshal(candidateCanaryResult{Version: 1, Passed: accepted})
			if err != nil {
				test.Fatal(err)
			}
			legacyReplay := NewModelClassReconciler(storage, registry, zaptest.NewLogger(test))
			legacyReplay.SetCanaryAccounting(&candidateReplayLedger{ModelDispatchStore: storage, record: store.ModelDispatchRecord{SessionID: operation, Response: legacy}}, accounting, audit)
			legacyErr := legacyReplay.validateCandidate(context.Background(), &class)
			if (legacyErr == nil) != accepted || errors.Is(legacyErr, ErrDispatchAccountingUncertain) || driver.calls != 1 || len(accounting.events) != 1 {
				test.Fatalf("legacy terminal compatibility changed: %v", legacyErr)
			}
			restarted := NewModelClassReconciler(storage, registry, zaptest.NewLogger(test))
			restarted.SetCanaryAccounting(storage, accounting, audit)
			reconcileModelClass(test, restarted, "", "class")
			if driver.calls != 1 || len(accounting.events) != 1 {
				test.Fatal("restart repeated a paid candidate canary")
			}
			blocked := NewModelClassReconciler(storage, registry, zaptest.NewLogger(test))
			if err := blocked.validateCandidate(context.Background(), &class); err == nil || driver.calls != 1 {
				test.Fatal("missing accounting dependencies allowed a canary call")
			}
			class.ObjectMeta.UID = "replacement-class-incarnation"
			accounting.err = errors.New("usage unavailable")
			uncertain := NewModelClassReconciler(storage, registry, zaptest.NewLogger(test))
			uncertain.SetCanaryAccounting(storage, accounting, audit)
			if err := uncertain.validateCandidate(context.Background(), &class); !errors.Is(err, ErrDispatchAccountingUncertain) {
				test.Fatalf("accounting failure was not fenced: %v", err)
			}
			if err := uncertain.validateCandidate(context.Background(), &class); !errors.Is(err, ErrDispatchAccountingUncertain) || driver.calls != 2 {
				test.Fatal("uncertain claimed execution was retried")
			}
			accounting.err = nil
			recovery := NewModelClassReconciler(storage, registry, zaptest.NewLogger(test))
			recovery.SetCanaryAccounting(storage, accounting, audit)
			recoveryErr := recovery.validateCandidate(context.Background(), &class)
			if (recoveryErr == nil) != accepted || errors.Is(recoveryErr, ErrDispatchAccountingUncertain) || driver.calls != 2 {
				test.Fatalf("known provider outcome did not recover without inference replay: calls=%d error=%v", driver.calls, recoveryErr)
			}
			if len(storage.reservations) != 0 || len(accounting.events) != 2 {
				test.Fatal("recovery did not settle and account the known outcome exactly once")
			}
			class.ObjectMeta.UID = "unknown-provider-incarnation"
			driver.providerErr = errors.New("provider outcome unknown")
			if err := recovery.validateCandidate(context.Background(), &class); !errors.Is(err, ErrDispatchAccountingUncertain) {
				test.Fatalf("unknown provider outcome was not fenced: %v", err)
			}
			driver.providerErr = nil
			if err := recovery.validateCandidate(context.Background(), &class); !errors.Is(err, ErrDispatchAccountingUncertain) || driver.calls != 3 || len(storage.reservations) != 1 || len(accounting.events) != 2 {
				test.Fatal("unknown outcome replayed inference or released protected quota")
			}
			rawClass, err := storage.Get(context.Background(), "ModelClass", "", "class")
			if err != nil || json.Unmarshal(rawClass, &class) != nil {
				test.Fatal("class unavailable for concurrent edit")
			}
			class.Spec.VersionPin = "concurrent-candidate"
			class.Spec.VersionCandidates["concurrent-candidate"] = binding
			class.Status.ActiveVersion, class.Status.ActiveBinding = "previous", &previous
			encodedClass, err := json.Marshal(class)
			if err != nil || storage.Update(context.Background(), "ModelClass", "", "class", encodedClass, class.ObjectMeta.ResourceVersion) != nil {
				test.Fatal("failed to configure concurrent candidate")
			}
			driver.response = "KOGNITION_MODEL_CANARY_OK"
			driver.onComplete = func() {
				current := getModelClass(test, storage.memStore, "", "class")
				current.Spec.VersionPin = "newer-desired-version"
				data, err := json.Marshal(current)
				if err != nil || storage.Update(context.Background(), "ModelClass", "", "class", data, current.ObjectMeta.ResourceVersion) != nil {
					test.Fatal("concurrent class update failed")
				}
			}
			concurrent := NewModelClassReconciler(storage, registry, zaptest.NewLogger(test))
			concurrent.SetCanaryAccounting(storage, accounting, audit)
			if _, err := concurrent.Reconcile(context.Background(), Request{Name: "class"}); !errors.Is(err, store.ErrConflict) {
				test.Fatalf("stale class promotion was not rejected: %v", err)
			}
			final := getModelClass(test, storage.memStore, "", "class")
			if final.Spec.VersionPin != "newer-desired-version" || final.Status.ActiveVersion != "previous" {
				test.Fatal("stale canary overwrote current class state")
			}
		})
	}
}

func (s *modelClassErrorStore) Get(ctx context.Context, kind, namespace, name string) ([]byte, error) {
	if s.getErr != nil {
		return nil, s.getErr
	}
	return s.Store.Get(ctx, kind, namespace, name)
}

func (s *modelClassErrorStore) List(ctx context.Context, kind string, opts store.ListOptions) ([][]byte, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.Store.List(ctx, kind, opts)
}

func storeModelClass(t *testing.T, s *memStore, mc v1alpha1.ModelClass) {
	t.Helper()
	data, err := json.Marshal(mc)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create(context.Background(), modelClassKind, mc.ObjectMeta.Namespace, mc.ObjectMeta.Name, data); err != nil {
		t.Fatal(err)
	}
}

func getModelClass(t *testing.T, s *memStore, ns, name string) v1alpha1.ModelClass {
	t.Helper()
	raw, err := s.Get(context.Background(), modelClassKind, ns, name)
	if err != nil {
		t.Fatal(err)
	}
	var mc v1alpha1.ModelClass
	if err := json.Unmarshal(raw, &mc); err != nil {
		t.Fatal(err)
	}
	return mc
}

func reconcileModelClass(t *testing.T, r *ModelClassReconciler, ns, name string) Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), Request{Namespace: ns, Name: name})
	if err != nil {
		t.Fatalf("reconcile error: %v", err)
	}
	return res
}

// TC-0182: Create a ModelClass with provider and latencyClass; verify reconciler
// transitions through Registered → Validating → Ready.
// TC-0194: Register an openai-compatible driver; reconciler calls Validate and Probe, status→Ready.
func TestModelClassReconciler_ProvisionToReady(t *testing.T) {
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	reg := testRegistry()
	r := NewModelClassReconciler(s, reg, logger)

	mc := v1alpha1.ModelClass{
		TypeMeta: v1alpha1.TypeMeta{Kind: "ModelClass", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "gpt4-class",
			Namespace: "default",
		},
		Spec: v1alpha1.ModelClassSpec{
			Provisioner:  "openai",
			LatencyClass: "low",
			CostTier:     "premium",
			VersionPin:   "gpt-4-0613",
		},
	}
	storeModelClass(t, s, mc)

	// First reconcile: adds finalizer, phase → Registered
	reconcileModelClass(t, r, "default", "gpt4-class")
	got := getModelClass(t, s, "default", "gpt4-class")

	hasFinalizer := false
	for _, f := range got.ObjectMeta.Finalizers {
		if f == modelClassFinalizer {
			hasFinalizer = true
		}
	}
	if !hasFinalizer {
		t.Fatal("expected finalizer to be added")
	}
	if got.Status.Phase != v1alpha1.PhaseRegistered {
		t.Fatalf("expected phase Registered after 1st reconcile, got %s", got.Status.Phase)
	}

	// Second reconcile: validates config → phase Validating
	reconcileModelClass(t, r, "default", "gpt4-class")
	got = getModelClass(t, s, "default", "gpt4-class")
	if got.Status.Phase != v1alpha1.PhaseValidating {
		t.Fatalf("expected phase Validating after 2nd reconcile, got %s", got.Status.Phase)
	}

	// Third reconcile: checks CPI driver → phase Ready
	reconcileModelClass(t, r, "default", "gpt4-class")
	got = getModelClass(t, s, "default", "gpt4-class")

	if got.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("expected phase Ready after 3rd reconcile, got %s", got.Status.Phase)
	}
	if !got.Status.Provisioned {
		t.Fatal("expected Provisioned=true")
	}
	if got.Status.ActiveVersion != "gpt-4-0613" {
		t.Fatalf("expected ActiveVersion gpt-4-0613, got %s", got.Status.ActiveVersion)
	}
	if got.Status.Health != "Healthy" {
		t.Fatalf("expected Health Healthy, got %s", got.Status.Health)
	}
}

// TC-0224a: ModelClass with a driver not in the registry stays in Validating → Failed.
// TC-0195: Model referencing non-existent provisioner reports DriverNotFound.
func TestModelClassReconciler_DriverNotFound(t *testing.T) {
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	// Empty registry — no drivers registered
	reg := cpi.NewRegistry()
	r := NewModelClassReconciler(s, reg, logger)

	mc := v1alpha1.ModelClass{
		TypeMeta: v1alpha1.TypeMeta{Kind: "ModelClass", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "no-driver-class",
			Namespace: "default",
		},
		Spec: v1alpha1.ModelClassSpec{
			Provisioner: "openai",
			VersionPin:  "gpt-4-0613",
		},
	}
	storeModelClass(t, s, mc)

	// 1st reconcile: Registered
	reconcileModelClass(t, r, "default", "no-driver-class")
	// 2nd reconcile: Validating
	reconcileModelClass(t, r, "default", "no-driver-class")
	// 3rd reconcile: driver not found → Failed
	reconcileModelClass(t, r, "default", "no-driver-class")
	got := getModelClass(t, s, "default", "no-driver-class")

	if got.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("expected phase Failed when driver not found, got %s", got.Status.Phase)
	}
	if got.Status.Provisioned {
		t.Fatal("expected Provisioned=false")
	}
}

// TC-0184: Terminate a ModelClass; verify finalizer drains active sessions before deletion.
func TestModelClassReconciler_FinalizerDrainsModels(t *testing.T) {
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	reg := testRegistry()
	r := NewModelClassReconciler(s, reg, logger)

	now := time.Now()
	mc := v1alpha1.ModelClass{
		TypeMeta: v1alpha1.TypeMeta{Kind: "ModelClass", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:              "gpt4-class",
			Namespace:         "default",
			Finalizers:        []string{modelClassFinalizer},
			DeletionTimestamp: &now,
		},
		Spec: v1alpha1.ModelClassSpec{
			Provisioner: "openai",
		},
	}
	storeModelClass(t, s, mc)

	// Store an active model referencing this class
	model := v1alpha1.Model{
		TypeMeta: v1alpha1.TypeMeta{Kind: "Model", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "gpt4-turbo",
			Namespace: "default",
		},
		Spec: v1alpha1.ModelSpec{
			ClassName: "gpt4-class",
			Provider:  "openai",
			Endpoint:  "https://api.openai.com/v1",
		},
		Status: v1alpha1.ModelStatus{
			Phase: v1alpha1.PhaseReady,
		},
	}
	storeModel(t, s, model)

	// Reconcile — should requeue because model is still active
	res := reconcileModelClass(t, r, "default", "gpt4-class")
	if res.RequeueAfter == 0 {
		t.Fatal("expected requeue while draining active models")
	}

	got := getModelClass(t, s, "default", "gpt4-class")
	if got.Status.Phase != v1alpha1.PhaseTerminated {
		t.Fatalf("expected phase Terminated during drain, got %s", got.Status.Phase)
	}
	// Finalizer still present
	hasFinalizer := false
	for _, f := range got.ObjectMeta.Finalizers {
		if f == modelClassFinalizer {
			hasFinalizer = true
		}
	}
	if !hasFinalizer {
		t.Fatal("finalizer should remain while draining")
	}

	// Mark model as terminated
	currentModel := getModel(t, s, "default", "gpt4-turbo")
	currentModel.Status.Phase = v1alpha1.PhaseTerminated
	data, _ := json.Marshal(currentModel)
	_ = s.Update(context.Background(), "Model", "default", "gpt4-turbo", data, currentModel.ObjectMeta.ResourceVersion)

	// Reconcile again — should complete deletion (remove finalizer)
	res = reconcileModelClass(t, r, "default", "gpt4-class")
	if res.RequeueAfter != 0 {
		t.Fatal("expected no requeue after drain completes")
	}

	got = getModelClass(t, s, "default", "gpt4-class")
	for _, f := range got.ObjectMeta.Finalizers {
		if f == modelClassFinalizer {
			t.Fatal("finalizer should be removed after drain completes")
		}
	}
}

func TestModelClassReconciler_UnsupportedProvisioner(t *testing.T) {
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	reg := testRegistry()
	r := NewModelClassReconciler(s, reg, logger)

	mc := v1alpha1.ModelClass{
		TypeMeta: v1alpha1.TypeMeta{Kind: "ModelClass", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "bad-class",
			Namespace: "default",
		},
		Spec: v1alpha1.ModelClassSpec{
			Provisioner: "unsupported-provider",
		},
	}
	storeModelClass(t, s, mc)

	reconcileModelClass(t, r, "default", "bad-class")
	reconcileModelClass(t, r, "default", "bad-class")
	got := getModelClass(t, s, "default", "bad-class")

	if got.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("expected phase Failed, got %s", got.Status.Phase)
	}
	if got.Status.Provisioned {
		t.Fatal("expected Provisioned=false")
	}
}

// TC-0225a: Version pin change triggers canary promotion.
func TestModelClassReconciler_VersionPinChange(t *testing.T) {
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	reg := testRegistry()
	r := NewModelClassReconciler(s, reg, logger)

	mc := v1alpha1.ModelClass{
		TypeMeta: v1alpha1.TypeMeta{Kind: "ModelClass", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:       "pin-class",
			Namespace:  "default",
			Finalizers: []string{modelClassFinalizer},
		},
		Spec: v1alpha1.ModelClassSpec{
			Provisioner: "openai",
			VersionPin:  "gpt-4-0125-preview",
		},
		Status: v1alpha1.ModelClassStatus{
			Phase:         v1alpha1.PhaseReady,
			Provisioned:   true,
			ActiveVersion: "gpt-4-0613",
			Health:        "Healthy",
		},
	}
	storeModelClass(t, s, mc)

	// Reconcile detects drift between spec.VersionPin and status.ActiveVersion
	reconcileModelClass(t, r, "default", "pin-class")
	got := getModelClass(t, s, "default", "pin-class")

	if got.Status.ActiveVersion != "gpt-4-0125-preview" {
		t.Fatalf("expected ActiveVersion promoted to gpt-4-0125-preview, got %s", got.Status.ActiveVersion)
	}
	if got.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("expected phase Ready after pin change, got %s", got.Status.Phase)
	}
}

// TC-0225b: Version pin change when driver disappears triggers rollback to Degraded.
// TC-0196: Driver unavailable/invalid credentials; Probe returns Degraded with error condition.
func TestModelClassReconciler_VersionPinRollback(t *testing.T) {
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	// Empty registry — driver is NOT available for canary check
	reg := cpi.NewRegistry()
	r := NewModelClassReconciler(s, reg, logger)

	mc := v1alpha1.ModelClass{
		TypeMeta: v1alpha1.TypeMeta{Kind: "ModelClass", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:       "rollback-class",
			Namespace:  "default",
			Finalizers: []string{modelClassFinalizer},
		},
		Spec: v1alpha1.ModelClassSpec{
			Provisioner: "openai",
			VersionPin:  "gpt-4-0125-preview",
		},
		Status: v1alpha1.ModelClassStatus{
			Phase:         v1alpha1.PhaseReady,
			Provisioned:   true,
			ActiveVersion: "gpt-4-0613",
			Health:        "Healthy",
		},
	}
	storeModelClass(t, s, mc)

	// Reconcile detects drift but driver is unavailable → rollback
	res := reconcileModelClass(t, r, "default", "rollback-class")
	got := getModelClass(t, s, "default", "rollback-class")

	if got.Status.Phase != v1alpha1.PhaseDegraded {
		t.Fatalf("expected phase Degraded after failed canary, got %s", got.Status.Phase)
	}
	if got.Status.ActiveVersion != "gpt-4-0613" {
		t.Fatalf("expected ActiveVersion to remain gpt-4-0613 (rollback), got %s", got.Status.ActiveVersion)
	}
	if got.Status.Health != "Degraded" {
		t.Fatalf("expected Health Degraded, got %s", got.Status.Health)
	}
	if res.RequeueAfter != 30*time.Second {
		t.Fatalf("expected 30s requeue after rollback, got %v", res.RequeueAfter)
	}
}

// TC-0224c: Degraded stays degraded when driver is still unavailable.
func TestModelClassReconciler_DegradedStaysDegraded(t *testing.T) {
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	// Empty registry — driver NOT available
	reg := cpi.NewRegistry()
	r := NewModelClassReconciler(s, reg, logger)

	mc := v1alpha1.ModelClass{
		TypeMeta: v1alpha1.TypeMeta{Kind: "ModelClass", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:       "still-degraded",
			Namespace:  "default",
			Finalizers: []string{modelClassFinalizer},
		},
		Spec: v1alpha1.ModelClassSpec{
			Provisioner: "openai",
			VersionPin:  "gpt-4-0613",
		},
		Status: v1alpha1.ModelClassStatus{
			Phase:         v1alpha1.PhaseDegraded,
			Provisioned:   true,
			ActiveVersion: "gpt-4-0613",
			Health:        "Degraded",
		},
	}
	storeModelClass(t, s, mc)

	// Reconcile: driver still unavailable → stays degraded, requeue 30s
	res := reconcileModelClass(t, r, "default", "still-degraded")
	got := getModelClass(t, s, "default", "still-degraded")

	if got.Status.Phase != v1alpha1.PhaseDegraded {
		t.Fatalf("expected phase to remain Degraded, got %s", got.Status.Phase)
	}
	if res.RequeueAfter != 30*time.Second {
		t.Fatalf("expected 30s requeue, got %v", res.RequeueAfter)
	}
}

// Failed state re-validates on each reconcile so a previously-failed
// ModelClass auto-recovers once the underlying problem (missing CPI
// driver, transient registry gap) is resolved. Stays Failed when the
// configuration is still invalid.
func TestModelClassReconciler_FailedRevalidatesAndRecovers(t *testing.T) {
	logger := zaptest.NewLogger(t)
	reg := testRegistry()

	t.Run("recovers when config is valid", func(t *testing.T) {
		s := newMemStore()
		r := NewModelClassReconciler(s, reg, logger)

		mc := v1alpha1.ModelClass{
			TypeMeta: v1alpha1.TypeMeta{Kind: "ModelClass", APIVersion: "v1alpha1"},
			ObjectMeta: v1alpha1.ObjectMeta{
				Name:       "recoverable-class",
				Namespace:  "default",
				Finalizers: []string{modelClassFinalizer},
			},
			Spec:   v1alpha1.ModelClassSpec{Provisioner: "openai"},
			Status: v1alpha1.ModelClassStatus{Phase: v1alpha1.PhaseFailed, Provisioned: false},
		}
		storeModelClass(t, s, mc)

		// First reconcile: Failed → reconcileRegistered → advances to Validating.
		reconcileModelClass(t, r, "default", "recoverable-class")
		// Second reconcile: Validating → Ready.
		reconcileModelClass(t, r, "default", "recoverable-class")

		got := getModelClass(t, s, "default", "recoverable-class")
		if got.Status.Phase != v1alpha1.PhaseReady {
			t.Fatalf("expected phase Ready after recovery, got %s", got.Status.Phase)
		}
	})

	t.Run("stays failed when config is invalid", func(t *testing.T) {
		s := newMemStore()
		r := NewModelClassReconciler(s, reg, logger)

		mc := v1alpha1.ModelClass{
			TypeMeta: v1alpha1.TypeMeta{Kind: "ModelClass", APIVersion: "v1alpha1"},
			ObjectMeta: v1alpha1.ObjectMeta{
				Name:       "still-failed-class",
				Namespace:  "default",
				Finalizers: []string{modelClassFinalizer},
			},
			Spec:   v1alpha1.ModelClassSpec{Provisioner: "nonexistent-provider"},
			Status: v1alpha1.ModelClassStatus{Phase: v1alpha1.PhaseFailed, Provisioned: false},
		}
		storeModelClass(t, s, mc)

		reconcileModelClass(t, r, "default", "still-failed-class")
		got := getModelClass(t, s, "default", "still-failed-class")
		if got.Status.Phase != v1alpha1.PhaseFailed {
			t.Fatalf("expected phase to remain Failed for invalid config, got %s", got.Status.Phase)
		}
	})
}

func TestModelClassReconciler_ValidateConfig(t *testing.T) {
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	reg := testRegistry()
	r := NewModelClassReconciler(s, reg, logger)

	tests := []struct {
		name         string
		spec         v1alpha1.ModelClassSpec
		expectFailed bool
		expectReason string
	}{
		{
			name:         "empty provisioner",
			spec:         v1alpha1.ModelClassSpec{},
			expectFailed: true,
			expectReason: "ProvisionerMissing",
		},
		{
			name: "invalid latency class",
			spec: v1alpha1.ModelClassSpec{
				Provisioner:  "openai",
				LatencyClass: "extreme",
			},
			expectFailed: true,
			expectReason: "LatencyClassInvalid",
		},
		{
			name: "empty health probe endpoint",
			spec: v1alpha1.ModelClassSpec{
				Provisioner: "openai",
				HealthProbe: &v1alpha1.HealthProbeSpec{Endpoint: ""},
			},
			expectFailed: true,
			expectReason: "HealthProbeInvalid",
		},
		{
			name: "invalid health probe URL",
			spec: v1alpha1.ModelClassSpec{
				Provisioner: "openai",
				HealthProbe: &v1alpha1.HealthProbeSpec{Endpoint: "not-a-url"},
			},
			expectFailed: true,
			expectReason: "HealthProbeInvalid",
		},
		{
			name: "valid health probe URL",
			spec: v1alpha1.ModelClassSpec{
				Provisioner: "openai",
				HealthProbe: &v1alpha1.HealthProbeSpec{Endpoint: "https://health.example.com/check"},
			},
			expectFailed: false,
		},
		{
			name: "incomplete pricing",
			spec: v1alpha1.ModelClassSpec{
				Provisioner: "openai",
				Parameters:  map[string]string{"cost.inputMicrousdPerMillionTokens": "2500000"},
			},
			expectFailed: true,
			expectReason: "PricingInvalid",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			localStore := newMemStore()
			localR := NewModelClassReconciler(localStore, reg, logger)

			mc := v1alpha1.ModelClass{
				TypeMeta: v1alpha1.TypeMeta{Kind: "ModelClass", APIVersion: "v1alpha1"},
				ObjectMeta: v1alpha1.ObjectMeta{
					Name:      "validate-" + tc.name,
					Namespace: "default",
				},
				Spec: tc.spec,
			}
			storeModelClass(t, localStore, mc)

			// 1st reconcile: adds finalizer → Registered
			reconcileModelClass(t, localR, "default", mc.ObjectMeta.Name)
			// 2nd reconcile: validates config
			reconcileModelClass(t, localR, "default", mc.ObjectMeta.Name)

			got := getModelClass(t, localStore, "default", mc.ObjectMeta.Name)
			if tc.expectFailed {
				if got.Status.Phase != v1alpha1.PhaseFailed {
					t.Fatalf("expected phase Failed, got %s", got.Status.Phase)
				}
				// Verify reason is in conditions
				foundReason := false
				for _, c := range got.Status.Conditions {
					if c.Reason == tc.expectReason {
						foundReason = true
					}
				}
				if !foundReason {
					t.Fatalf("expected condition reason %q in conditions", tc.expectReason)
				}
			} else {
				if got.Status.Phase == v1alpha1.PhaseFailed {
					t.Fatalf("expected config to pass validation, got Failed")
				}
			}
			_ = r // suppress unused
		})
	}
}

// Test that reconciling a non-existent resource is a no-op.
func TestModelClassReconciler_NotFound(t *testing.T) {
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	reg := testRegistry()
	r := NewModelClassReconciler(&modelClassErrorStore{
		Store:  s,
		getErr: fmt.Errorf("wrapped missing model class: %w", store.ErrNotFound),
	}, reg, logger)

	res, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "does-not-exist"})
	if err != nil {
		t.Fatalf("expected no error for missing resource, got: %v", err)
	}
	if res.Requeue || res.RequeueAfter != 0 {
		t.Fatal("expected empty result for missing resource")
	}
}

func TestModelClassReconciler_WrappedNotFoundCompletesDrain(t *testing.T) {
	s := newMemStore()
	now := time.Now()
	storeModelClass(t, s, v1alpha1.ModelClass{
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:              "deleting-class",
			Namespace:         "default",
			Finalizers:        []string{modelClassFinalizer},
			DeletionTimestamp: &now,
		},
	})
	r := NewModelClassReconciler(&modelClassErrorStore{
		Store:   s,
		listErr: fmt.Errorf("wrapped empty model list: %w", store.ErrNotFound),
	}, testRegistry(), zaptest.NewLogger(t))

	result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "deleting-class"})
	if err != nil || result != (Result{}) {
		t.Fatalf("result = %+v, error = %v, want completed drain", result, err)
	}
	got := getModelClass(t, s, "default", "deleting-class")
	if len(got.ObjectMeta.Finalizers) != 0 || got.Status.Phase != v1alpha1.PhaseTerminated {
		t.Fatalf("model class = %+v, want terminated with no finalizers", got)
	}
}

// Test that VersionPin="" results in empty ActiveVersion after reaching Ready.
func TestModelClassReconciler_NoVersionPin(t *testing.T) {
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	reg := testRegistry()
	r := NewModelClassReconciler(s, reg, logger)

	mc := v1alpha1.ModelClass{
		TypeMeta: v1alpha1.TypeMeta{Kind: "ModelClass", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      "no-pin-class",
			Namespace: "default",
		},
		Spec: v1alpha1.ModelClassSpec{
			Provisioner: "anthropic",
		},
	}
	storeModelClass(t, s, mc)

	// Three reconciles: → Registered → Validating → Ready
	reconcileModelClass(t, r, "default", "no-pin-class")
	reconcileModelClass(t, r, "default", "no-pin-class")
	reconcileModelClass(t, r, "default", "no-pin-class")

	got := getModelClass(t, s, "default", "no-pin-class")
	if got.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("expected phase Ready, got %s", got.Status.Phase)
	}
	if got.Status.ActiveVersion != "" {
		t.Fatalf("expected empty ActiveVersion when no VersionPin, got %s", got.Status.ActiveVersion)
	}
	if got.Status.Health != "Healthy" {
		t.Fatalf("expected Health Healthy, got %s", got.Status.Health)
	}
}

// TC-0224b: Degraded → Ready recovery when driver becomes available.
func TestModelClassReconciler_DegradedRecovery(t *testing.T) {
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	reg := testRegistry()
	r := NewModelClassReconciler(s, reg, logger)

	mc := v1alpha1.ModelClass{
		TypeMeta: v1alpha1.TypeMeta{Kind: "ModelClass", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:       "degraded-class",
			Namespace:  "default",
			Finalizers: []string{modelClassFinalizer},
		},
		Spec: v1alpha1.ModelClassSpec{
			Provisioner: "openai",
			VersionPin:  "gpt-4-0613",
		},
		Status: v1alpha1.ModelClassStatus{
			Phase:         v1alpha1.PhaseDegraded,
			Provisioned:   true,
			ActiveVersion: "gpt-4-0613",
			Health:        "Degraded",
		},
	}
	storeModelClass(t, s, mc)

	// Reconcile: driver is available → recovers to Ready
	reconcileModelClass(t, r, "default", "degraded-class")
	got := getModelClass(t, s, "default", "degraded-class")

	if got.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("expected phase Ready after recovery, got %s", got.Status.Phase)
	}
	if got.Status.Health != "Healthy" {
		t.Fatalf("expected Health Healthy after recovery, got %s", got.Status.Health)
	}
}

// configurableMockDriver is a mock CPI driver that allows controlling probe responses.
type configurableMockDriver struct {
	name        string
	probeResult *cpi.HealthResult
	probeErr    error
}

func (d *configurableMockDriver) Name() string { return d.name }

func (d *configurableMockDriver) Validate(_ context.Context, _ *v1alpha1.ModelSpec, _ *v1alpha1.ModelClassSpec) error {
	return nil
}

func (d *configurableMockDriver) Probe(_ context.Context, _ *v1alpha1.ModelSpec, _ *cpi.Credentials) (*cpi.HealthResult, error) {
	return d.probeResult, d.probeErr
}

func (d *configurableMockDriver) Complete(_ context.Context, _ *cpi.CompletionRequest) (*cpi.CompletionResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

func (d *configurableMockDriver) Embed(_ context.Context, _ *cpi.EmbedRequest) (*cpi.EmbedResponse, error) {
	return nil, fmt.Errorf("not implemented")
}

func registryWith(driver cpi.Driver) *cpi.Registry {
	reg := cpi.NewRegistry()
	reg.MustRegister(driver)
	return reg
}

// TC-0185: Health probe failure degrades ModelClass after FailureThreshold consecutive failures.
func TestModelClassReconciler_TC0185_ProbeFailureDegrades(t *testing.T) {
	driver := &configurableMockDriver{
		name:        "openai-compatible",
		probeResult: &cpi.HealthResult{Available: false, Reason: "Timeout", Message: "endpoint timed out"},
	}
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	reg := registryWith(driver)
	r := NewModelClassReconciler(s, reg, logger)

	mc := v1alpha1.ModelClass{
		TypeMeta: v1alpha1.TypeMeta{Kind: "ModelClass", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:       "probe-fail-class",
			Namespace:  "default",
			Finalizers: []string{modelClassFinalizer},
		},
		Spec: v1alpha1.ModelClassSpec{
			Provisioner:  "openai",
			LatencyClass: "low",
			HealthProbe: &v1alpha1.HealthProbeSpec{
				Endpoint:         "https://api.example.com/health",
				IntervalSeconds:  10,
				TimeoutSeconds:   5,
				Type:             "inference-ping",
				FailureThreshold: 2,
			},
		},
		Status: v1alpha1.ModelClassStatus{
			Phase:       v1alpha1.PhaseReady,
			Provisioned: true,
			Health:      "Healthy",
		},
	}
	storeModelClass(t, s, mc)

	// First probe failure: ConsecutiveFailures = 1, still Ready
	res := reconcileModelClass(t, r, "default", "probe-fail-class")
	got := getModelClass(t, s, "default", "probe-fail-class")
	if got.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("after 1st failure: expected Ready, got %s", got.Status.Phase)
	}
	if got.Status.ConsecutiveFailures != 1 {
		t.Fatalf("after 1st failure: expected ConsecutiveFailures=1, got %d", got.Status.ConsecutiveFailures)
	}
	if res.RequeueAfter != 10*time.Second {
		t.Fatalf("expected requeue after 10s, got %v", res.RequeueAfter)
	}

	// Second probe failure: ConsecutiveFailures = 2, threshold met → Degraded
	res = reconcileModelClass(t, r, "default", "probe-fail-class")
	got = getModelClass(t, s, "default", "probe-fail-class")
	if got.Status.Phase != v1alpha1.PhaseDegraded {
		t.Fatalf("after 2nd failure: expected Degraded, got %s", got.Status.Phase)
	}
	if got.Status.Health != "Degraded" {
		t.Fatalf("expected Health=Degraded, got %s", got.Status.Health)
	}
	if got.Status.ConsecutiveFailures != 2 {
		t.Fatalf("expected ConsecutiveFailures=2, got %d", got.Status.ConsecutiveFailures)
	}
	if got.Status.LastProbeTime == "" {
		t.Fatal("expected LastProbeTime to be set")
	}
}

// TC-0185b: Latency threshold probe type detects high latency.
func TestModelClassReconciler_TC0185b_LatencyThresholdDegrades(t *testing.T) {
	driver := &configurableMockDriver{
		name:        "openai-compatible",
		probeResult: &cpi.HealthResult{Available: true, Latency: "500ms", Reason: "OK", Message: "healthy but slow"},
	}
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	reg := registryWith(driver)
	r := NewModelClassReconciler(s, reg, logger)

	mc := v1alpha1.ModelClass{
		TypeMeta: v1alpha1.TypeMeta{Kind: "ModelClass", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:       "latency-class",
			Namespace:  "default",
			Finalizers: []string{modelClassFinalizer},
		},
		Spec: v1alpha1.ModelClassSpec{
			Provisioner:  "openai",
			LatencyClass: "low",
			HealthProbe: &v1alpha1.HealthProbeSpec{
				Endpoint:           "https://api.example.com/health",
				IntervalSeconds:    10,
				Type:               "latency-threshold",
				LatencyThresholdMs: 200,
				FailureThreshold:   1,
			},
		},
		Status: v1alpha1.ModelClassStatus{
			Phase:       v1alpha1.PhaseReady,
			Provisioned: true,
			Health:      "Healthy",
		},
	}
	storeModelClass(t, s, mc)

	// Single probe: latency 500ms > threshold 200ms → Degraded immediately (threshold=1)
	reconcileModelClass(t, r, "default", "latency-class")
	got := getModelClass(t, s, "default", "latency-class")
	if got.Status.Phase != v1alpha1.PhaseDegraded {
		t.Fatalf("expected Degraded due to latency, got %s", got.Status.Phase)
	}
}

// TC-0186: Fallback routing sets FallbackClass to a same-tier Ready ModelClass.
func TestModelClassReconciler_TC0186_FallbackRouting(t *testing.T) {
	driver := &configurableMockDriver{
		name:        "openai-compatible",
		probeResult: &cpi.HealthResult{Available: false, Reason: "Down", Message: "provider down"},
	}
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	reg := registryWith(driver)
	r := NewModelClassReconciler(s, reg, logger)

	// Store a Ready fallback class in the same latency tier.
	fallback := v1alpha1.ModelClass{
		TypeMeta: v1alpha1.TypeMeta{Kind: "ModelClass", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:       "fallback-class",
			Namespace:  "default",
			Finalizers: []string{modelClassFinalizer},
		},
		Spec: v1alpha1.ModelClassSpec{
			Provisioner:  "openai",
			LatencyClass: "low",
		},
		Status: v1alpha1.ModelClassStatus{
			Phase:       v1alpha1.PhaseReady,
			Provisioned: true,
			Health:      "Healthy",
		},
	}
	storeModelClass(t, s, fallback)

	// Store the primary class that will degrade.
	mc := v1alpha1.ModelClass{
		TypeMeta: v1alpha1.TypeMeta{Kind: "ModelClass", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:       "primary-class",
			Namespace:  "default",
			Finalizers: []string{modelClassFinalizer},
		},
		Spec: v1alpha1.ModelClassSpec{
			Provisioner:  "openai",
			LatencyClass: "low",
			HealthProbe: &v1alpha1.HealthProbeSpec{
				Endpoint:         "https://api.example.com/health",
				IntervalSeconds:  10,
				Type:             "inference-ping",
				FailureThreshold: 1,
			},
		},
		Status: v1alpha1.ModelClassStatus{
			Phase:       v1alpha1.PhaseReady,
			Provisioned: true,
			Health:      "Healthy",
		},
	}
	storeModelClass(t, s, mc)

	// Single failure → Degraded with fallback routing.
	reconcileModelClass(t, r, "default", "primary-class")
	got := getModelClass(t, s, "default", "primary-class")

	if got.Status.Phase != v1alpha1.PhaseDegraded {
		t.Fatalf("expected Degraded, got %s", got.Status.Phase)
	}
	if got.Status.FallbackClass != "fallback-class" {
		t.Fatalf("expected FallbackClass=fallback-class, got %q", got.Status.FallbackClass)
	}
}

// TC-0186b: No fallback when no same-tier Ready class exists.
func TestModelClassReconciler_TC0186b_NoFallbackAvailable(t *testing.T) {
	driver := &configurableMockDriver{
		name:        "openai-compatible",
		probeResult: &cpi.HealthResult{Available: false, Reason: "Down", Message: "provider down"},
	}
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	reg := registryWith(driver)
	r := NewModelClassReconciler(s, reg, logger)

	mc := v1alpha1.ModelClass{
		TypeMeta: v1alpha1.TypeMeta{Kind: "ModelClass", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:       "alone-class",
			Namespace:  "default",
			Finalizers: []string{modelClassFinalizer},
		},
		Spec: v1alpha1.ModelClassSpec{
			Provisioner:  "openai",
			LatencyClass: "high",
			HealthProbe: &v1alpha1.HealthProbeSpec{
				Endpoint:         "https://api.example.com/health",
				IntervalSeconds:  10,
				Type:             "inference-ping",
				FailureThreshold: 1,
			},
		},
		Status: v1alpha1.ModelClassStatus{
			Phase:       v1alpha1.PhaseReady,
			Provisioned: true,
			Health:      "Healthy",
		},
	}
	storeModelClass(t, s, mc)

	reconcileModelClass(t, r, "default", "alone-class")
	got := getModelClass(t, s, "default", "alone-class")

	if got.Status.Phase != v1alpha1.PhaseDegraded {
		t.Fatalf("expected Degraded, got %s", got.Status.Phase)
	}
	if got.Status.FallbackClass != "" {
		t.Fatalf("expected empty FallbackClass when no fallback available, got %q", got.Status.FallbackClass)
	}
}

// TC-0187: Recovery from Degraded after SuccessThreshold consecutive probe successes.
func TestModelClassReconciler_TC0187_StabilisationRecovery(t *testing.T) {
	driver := &configurableMockDriver{
		name:        "openai-compatible",
		probeResult: &cpi.HealthResult{Available: true, Latency: "30ms", Reason: "OK", Message: "recovered"},
	}
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	reg := registryWith(driver)
	r := NewModelClassReconciler(s, reg, logger)

	mc := v1alpha1.ModelClass{
		TypeMeta: v1alpha1.TypeMeta{Kind: "ModelClass", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:       "recovering-class",
			Namespace:  "default",
			Finalizers: []string{modelClassFinalizer},
		},
		Spec: v1alpha1.ModelClassSpec{
			Provisioner:  "openai",
			LatencyClass: "low",
			HealthProbe: &v1alpha1.HealthProbeSpec{
				Endpoint:         "https://api.example.com/health",
				IntervalSeconds:  10,
				Type:             "inference-ping",
				SuccessThreshold: 3,
				FailureThreshold: 2,
			},
		},
		Status: v1alpha1.ModelClassStatus{
			Phase:                v1alpha1.PhaseDegraded,
			Provisioned:          true,
			Health:               "Degraded",
			ConsecutiveFailures:  5,
			ConsecutiveSuccesses: 0,
			FallbackClass:        "some-fallback",
		},
	}
	storeModelClass(t, s, mc)

	// First success: ConsecutiveSuccesses=1, still Degraded
	res := reconcileModelClass(t, r, "default", "recovering-class")
	got := getModelClass(t, s, "default", "recovering-class")
	if got.Status.Phase != v1alpha1.PhaseDegraded {
		t.Fatalf("after 1st success: expected still Degraded, got %s", got.Status.Phase)
	}
	if got.Status.ConsecutiveSuccesses != 1 {
		t.Fatalf("after 1st success: expected ConsecutiveSuccesses=1, got %d", got.Status.ConsecutiveSuccesses)
	}
	if got.Status.ConsecutiveFailures != 0 {
		t.Fatalf("after 1st success: expected ConsecutiveFailures=0, got %d", got.Status.ConsecutiveFailures)
	}
	if res.RequeueAfter != 10*time.Second {
		t.Fatalf("expected requeue 10s, got %v", res.RequeueAfter)
	}

	// Second success: ConsecutiveSuccesses=2, still Degraded
	reconcileModelClass(t, r, "default", "recovering-class")
	got = getModelClass(t, s, "default", "recovering-class")
	if got.Status.Phase != v1alpha1.PhaseDegraded {
		t.Fatalf("after 2nd success: expected still Degraded, got %s", got.Status.Phase)
	}
	if got.Status.ConsecutiveSuccesses != 2 {
		t.Fatalf("after 2nd success: expected ConsecutiveSuccesses=2, got %d", got.Status.ConsecutiveSuccesses)
	}

	// Third success: ConsecutiveSuccesses=3 >= SuccessThreshold → Ready
	reconcileModelClass(t, r, "default", "recovering-class")
	got = getModelClass(t, s, "default", "recovering-class")
	if got.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("after 3rd success: expected Ready, got %s", got.Status.Phase)
	}
	if got.Status.Health != "Healthy" {
		t.Fatalf("expected Health=Healthy after recovery, got %s", got.Status.Health)
	}
	if got.Status.FallbackClass != "" {
		t.Fatalf("expected FallbackClass cleared after recovery, got %q", got.Status.FallbackClass)
	}
	if got.Status.ConsecutiveSuccesses != 0 {
		t.Fatalf("expected ConsecutiveSuccesses reset to 0, got %d", got.Status.ConsecutiveSuccesses)
	}
}

// TC-0187b: Recovery resets when a probe fails during stabilisation.
func TestModelClassReconciler_TC0187b_StabilisationResetOnFailure(t *testing.T) {
	driver := &configurableMockDriver{
		name:        "openai-compatible",
		probeResult: &cpi.HealthResult{Available: true, Reason: "OK", Message: "ok"},
	}
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	reg := registryWith(driver)
	r := NewModelClassReconciler(s, reg, logger)

	mc := v1alpha1.ModelClass{
		TypeMeta: v1alpha1.TypeMeta{Kind: "ModelClass", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:       "reset-class",
			Namespace:  "default",
			Finalizers: []string{modelClassFinalizer},
		},
		Spec: v1alpha1.ModelClassSpec{
			Provisioner:  "openai",
			LatencyClass: "low",
			HealthProbe: &v1alpha1.HealthProbeSpec{
				Endpoint:         "https://api.example.com/health",
				IntervalSeconds:  10,
				Type:             "inference-ping",
				SuccessThreshold: 3,
				FailureThreshold: 2,
			},
		},
		Status: v1alpha1.ModelClassStatus{
			Phase:       v1alpha1.PhaseDegraded,
			Provisioned: true,
			Health:      "Degraded",
		},
	}
	storeModelClass(t, s, mc)

	// First success → ConsecutiveSuccesses=1
	reconcileModelClass(t, r, "default", "reset-class")
	got := getModelClass(t, s, "default", "reset-class")
	if got.Status.ConsecutiveSuccesses != 1 {
		t.Fatalf("expected ConsecutiveSuccesses=1, got %d", got.Status.ConsecutiveSuccesses)
	}

	// Now flip to failing.
	driver.probeResult = &cpi.HealthResult{Available: false, Reason: "Down", Message: "down"}

	// Failure → ConsecutiveSuccesses reset to 0
	reconcileModelClass(t, r, "default", "reset-class")
	got = getModelClass(t, s, "default", "reset-class")
	if got.Status.Phase != v1alpha1.PhaseDegraded {
		t.Fatalf("expected still Degraded, got %s", got.Status.Phase)
	}
	if got.Status.ConsecutiveSuccesses != 0 {
		t.Fatalf("expected ConsecutiveSuccesses reset to 0 after failure, got %d", got.Status.ConsecutiveSuccesses)
	}
}

// TC-0185c: Probe success in Ready state resets ConsecutiveFailures and stays Ready.
func TestModelClassReconciler_ProbeSuccessStaysReady(t *testing.T) {
	driver := &configurableMockDriver{
		name:        "openai-compatible",
		probeResult: &cpi.HealthResult{Available: true, Latency: "25ms", Reason: "OK", Message: "healthy"},
	}
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	reg := registryWith(driver)
	r := NewModelClassReconciler(s, reg, logger)

	mc := v1alpha1.ModelClass{
		TypeMeta: v1alpha1.TypeMeta{Kind: "ModelClass", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:       "healthy-class",
			Namespace:  "default",
			Finalizers: []string{modelClassFinalizer},
		},
		Spec: v1alpha1.ModelClassSpec{
			Provisioner: "openai",
			HealthProbe: &v1alpha1.HealthProbeSpec{
				Endpoint:         "https://api.example.com/health",
				IntervalSeconds:  15,
				Type:             "inference-ping",
				FailureThreshold: 3,
			},
		},
		Status: v1alpha1.ModelClassStatus{
			Phase:               v1alpha1.PhaseReady,
			Provisioned:         true,
			Health:              "Healthy",
			ConsecutiveFailures: 2, // below threshold
		},
	}
	storeModelClass(t, s, mc)

	res := reconcileModelClass(t, r, "default", "healthy-class")
	got := getModelClass(t, s, "default", "healthy-class")

	if got.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("expected Ready, got %s", got.Status.Phase)
	}
	if got.Status.ConsecutiveFailures != 0 {
		t.Fatalf("expected ConsecutiveFailures reset to 0, got %d", got.Status.ConsecutiveFailures)
	}
	if got.Status.LastProbeTime == "" {
		t.Fatal("expected LastProbeTime to be set")
	}
	if res.RequeueAfter != 15*time.Second {
		t.Fatalf("expected requeue after 15s, got %v", res.RequeueAfter)
	}
}

// TestModelClassReconciler_ProbeDriverError verifies that a driver.Probe error is treated as a probe failure.
func TestModelClassReconciler_ProbeDriverError(t *testing.T) {
	driver := &configurableMockDriver{
		name:        "openai-compatible",
		probeResult: nil,
		probeErr:    fmt.Errorf("connection refused"),
	}
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	reg := registryWith(driver)
	r := NewModelClassReconciler(s, reg, logger)

	mc := v1alpha1.ModelClass{
		TypeMeta: v1alpha1.TypeMeta{Kind: "ModelClass", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:       "probe-error-class",
			Namespace:  "default",
			Finalizers: []string{modelClassFinalizer},
		},
		Spec: v1alpha1.ModelClassSpec{
			Provisioner:  "openai",
			LatencyClass: "low",
			HealthProbe: &v1alpha1.HealthProbeSpec{
				Endpoint:         "https://api.example.com/health",
				IntervalSeconds:  10,
				Type:             "inference-ping",
				FailureThreshold: 1,
			},
		},
		Status: v1alpha1.ModelClassStatus{
			Phase:       v1alpha1.PhaseReady,
			Provisioned: true,
			Health:      "Healthy",
		},
	}
	storeModelClass(t, s, mc)

	reconcileModelClass(t, r, "default", "probe-error-class")
	got := getModelClass(t, s, "default", "probe-error-class")

	if got.Status.Phase != v1alpha1.PhaseDegraded {
		t.Fatalf("expected Degraded on driver error, got %s", got.Status.Phase)
	}
	if got.Status.ConsecutiveFailures != 1 {
		t.Fatalf("expected ConsecutiveFailures=1, got %d", got.Status.ConsecutiveFailures)
	}
}

// TestModelClassReconciler_ProbeNilResult verifies that a nil probe result is treated as a failure.
func TestModelClassReconciler_ProbeNilResult(t *testing.T) {
	driver := &configurableMockDriver{
		name:        "openai-compatible",
		probeResult: nil,
		probeErr:    nil,
	}
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	reg := registryWith(driver)
	r := NewModelClassReconciler(s, reg, logger)

	mc := v1alpha1.ModelClass{
		TypeMeta: v1alpha1.TypeMeta{Kind: "ModelClass", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:       "nil-result-class",
			Namespace:  "default",
			Finalizers: []string{modelClassFinalizer},
		},
		Spec: v1alpha1.ModelClassSpec{
			Provisioner:  "openai",
			LatencyClass: "low",
			HealthProbe: &v1alpha1.HealthProbeSpec{
				Endpoint:         "https://api.example.com/health",
				IntervalSeconds:  10,
				Type:             "inference-ping",
				FailureThreshold: 1,
			},
		},
		Status: v1alpha1.ModelClassStatus{
			Phase:       v1alpha1.PhaseReady,
			Provisioned: true,
			Health:      "Healthy",
		},
	}
	storeModelClass(t, s, mc)

	reconcileModelClass(t, r, "default", "nil-result-class")
	got := getModelClass(t, s, "default", "nil-result-class")

	if got.Status.Phase != v1alpha1.PhaseDegraded {
		t.Fatalf("expected Degraded on nil result, got %s", got.Status.Phase)
	}
}

// TestModelClassReconciler_ErrorRateThresholdProbe verifies the error-rate-threshold probe type
// checks availability (rate-based evaluation is documented as a future enhancement).
func TestModelClassReconciler_ErrorRateThresholdProbe(t *testing.T) {
	driver := &configurableMockDriver{
		name:        "openai-compatible",
		probeResult: &cpi.HealthResult{Available: false, Reason: "HighErrorRate", Message: "error rate elevated"},
	}
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	reg := registryWith(driver)
	r := NewModelClassReconciler(s, reg, logger)

	mc := v1alpha1.ModelClass{
		TypeMeta: v1alpha1.TypeMeta{Kind: "ModelClass", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:       "error-rate-class",
			Namespace:  "default",
			Finalizers: []string{modelClassFinalizer},
		},
		Spec: v1alpha1.ModelClassSpec{
			Provisioner:  "openai",
			LatencyClass: "low",
			HealthProbe: &v1alpha1.HealthProbeSpec{
				Endpoint:           "https://api.example.com/health",
				IntervalSeconds:    10,
				Type:               "error-rate-threshold",
				ErrorRateThreshold: 5,
				FailureThreshold:   1,
			},
		},
		Status: v1alpha1.ModelClassStatus{
			Phase:       v1alpha1.PhaseReady,
			Provisioned: true,
			Health:      "Healthy",
		},
	}
	storeModelClass(t, s, mc)

	reconcileModelClass(t, r, "default", "error-rate-class")
	got := getModelClass(t, s, "default", "error-rate-class")

	if got.Status.Phase != v1alpha1.PhaseDegraded {
		t.Fatalf("expected Degraded when endpoint unavailable, got %s", got.Status.Phase)
	}
}

// TestModelClassReconciler_VersionPinRollbackResetsCounters verifies that version pin rollback
// to Degraded state resets ConsecutiveFailures and ConsecutiveSuccesses.
func TestModelClassReconciler_VersionPinRollbackResetsCounters(t *testing.T) {
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	// Empty registry — driver unavailable triggers rollback.
	reg := cpi.NewRegistry()
	r := NewModelClassReconciler(s, reg, logger)

	mc := v1alpha1.ModelClass{
		TypeMeta: v1alpha1.TypeMeta{Kind: "ModelClass", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:       "rollback-counters-class",
			Namespace:  "default",
			Finalizers: []string{modelClassFinalizer},
		},
		Spec: v1alpha1.ModelClassSpec{
			Provisioner:  "openai",
			LatencyClass: "low",
			VersionPin:   "v2",
		},
		Status: v1alpha1.ModelClassStatus{
			Phase:         v1alpha1.PhaseReady,
			Provisioned:   true,
			Health:        "Healthy",
			ActiveVersion: "v1",
		},
	}
	storeModelClass(t, s, mc)

	reconcileModelClass(t, r, "default", "rollback-counters-class")
	got := getModelClass(t, s, "default", "rollback-counters-class")

	if got.Status.Phase != v1alpha1.PhaseDegraded {
		t.Fatalf("expected Degraded, got %s", got.Status.Phase)
	}
	if got.Status.ConsecutiveFailures != 0 {
		t.Fatalf("expected ConsecutiveFailures=0 after rollback, got %d", got.Status.ConsecutiveFailures)
	}
	if got.Status.ConsecutiveSuccesses != 0 {
		t.Fatalf("expected ConsecutiveSuccesses=0 after rollback, got %d", got.Status.ConsecutiveSuccesses)
	}
}

// Test that an unknown phase triggers a reset to Registered and requeue.
func TestModelClassReconciler_UnknownPhaseResetsToRegistered(t *testing.T) {
	s := newMemStore()
	logger := zaptest.NewLogger(t)
	reg := testRegistry()
	r := NewModelClassReconciler(s, reg, logger)

	mc := v1alpha1.ModelClass{
		TypeMeta: v1alpha1.TypeMeta{Kind: "ModelClass", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:       "unknown-phase-class",
			Namespace:  "default",
			Finalizers: []string{modelClassFinalizer},
		},
		Spec: v1alpha1.ModelClassSpec{
			Provisioner: "openai",
		},
		Status: v1alpha1.ModelClassStatus{
			Phase: "SomeUnknownPhase",
		},
	}
	storeModelClass(t, s, mc)

	// Reconcile: unknown phase → resets to Registered, requeue
	res := reconcileModelClass(t, r, "default", "unknown-phase-class")
	if !res.Requeue {
		t.Fatal("expected Requeue=true after resetting unknown phase")
	}

	got := getModelClass(t, s, "default", "unknown-phase-class")
	if got.Status.Phase != v1alpha1.PhaseRegistered {
		t.Fatalf("expected phase Registered after reset, got %s", got.Status.Phase)
	}
}
