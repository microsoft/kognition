// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/store"
)

type modelGetErrorStore struct {
	store.Store
	kind string
	name string
	err  error
}

func (s *modelGetErrorStore) Get(ctx context.Context, kind, namespace, name string) ([]byte, error) {
	if kind == s.kind && name == s.name {
		return nil, s.err
	}
	return s.Store.Get(ctx, kind, namespace, name)
}

// mockDriver implements cpi.Driver for testing. Validate checks endpoint/modelID;
// Probe always returns healthy.
type mockDriver struct {
	name string
}

func (d *mockDriver) Name() string { return d.name }

func (d *mockDriver) Validate(_ context.Context, model *v1alpha1.ModelSpec, _ *v1alpha1.ModelClassSpec) error {
	if model.Endpoint == "" {
		return fmt.Errorf("%s: endpoint is required", d.name)
	}
	parsed, err := url.Parse(model.Endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("%s: endpoint must be a valid URL with scheme and host", d.name)
	}
	if model.ModelID == "" {
		return fmt.Errorf("%s: modelID is required", d.name)
	}
	return nil
}

func (d *mockDriver) Probe(_ context.Context, _ *v1alpha1.ModelSpec, _ *cpi.Credentials) (*cpi.HealthResult, error) {
	return &cpi.HealthResult{Available: true, Reason: "ProbeOK", Message: "mock probe healthy"}, nil
}

func (d *mockDriver) Complete(_ context.Context, _ *cpi.CompletionRequest) (*cpi.CompletionResponse, error) {
	return nil, fmt.Errorf("not implemented in mock")
}

func (d *mockDriver) Embed(_ context.Context, _ *cpi.EmbedRequest) (*cpi.EmbedResponse, error) {
	return nil, fmt.Errorf("not implemented in mock")
}

func testRegistry() *cpi.Registry {
	reg := cpi.NewRegistry()
	reg.MustRegister(&mockDriver{name: "openai-compatible"})
	reg.MustRegister(&mockDriver{name: "foundry"})
	reg.MustRegister(&mockDriver{name: "anthropic"})
	reg.MustRegister(&mockDriver{name: "ollama"})
	reg.MustRegister(&mockDriver{name: "aws-bedrock"})
	return reg
}

func storeModel(t *testing.T, s *memStore, model v1alpha1.Model) {
	t.Helper()
	data, _ := json.Marshal(model)
	if err := s.Create(context.Background(), "Model", model.ObjectMeta.Namespace, model.ObjectMeta.Name, data); err != nil {
		t.Fatalf("store Model: %v", err)
	}
}

func getModel(t *testing.T, s *memStore, ns, name string) v1alpha1.Model {
	t.Helper()
	raw, err := s.Get(context.Background(), "Model", ns, name)
	if err != nil {
		t.Fatalf("get Model %s/%s: %v", ns, name, err)
	}
	var model v1alpha1.Model
	if err := json.Unmarshal(raw, &model); err != nil {
		t.Fatalf("unmarshal Model: %v", err)
	}
	return model
}

func storeSecretRefForModel(t *testing.T, s *memStore, ref v1alpha1.SecretRef) {
	t.Helper()
	data, _ := json.Marshal(ref)
	if err := s.Create(context.Background(), "SecretRef", ref.ObjectMeta.Namespace, ref.ObjectMeta.Name, data); err != nil {
		t.Fatalf("store SecretRef: %v", err)
	}
}

func reconcileModel(t *testing.T, r *ModelReconciler, ns, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), Request{Namespace: ns, Name: name, Kind: "Model"}); err != nil {
		t.Fatalf("reconcile Model: %v", err)
	}
}

func findModelCondition(model v1alpha1.Model, condType string) *v1alpha1.Condition {
	for i := range model.Status.Conditions {
		if model.Status.Conditions[i].Type == condType {
			return &model.Status.Conditions[i]
		}
	}
	return nil
}

func TestModelReconciler_WrappedNotFoundIsComplete(t *testing.T) {
	s := &modelGetErrorStore{
		Store: newMemStore(),
		kind:  modelKind,
		name:  "deleted",
		err:   fmt.Errorf("wrapped missing model: %w", store.ErrNotFound),
	}
	r := NewModelReconciler(s, testRegistry(), zaptest.NewLogger(t))

	result, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "deleted"})
	if err != nil || result != (Result{}) {
		t.Fatalf("result = %+v, error = %v, want completed not-found reconciliation", result, err)
	}
}

func TestModelReconciler_WrappedDependencyNotFound(t *testing.T) {
	for _, test := range []struct {
		name        string
		missingKind string
		model       v1alpha1.Model
		wantReason  string
	}{
		{
			name:        "model class",
			missingKind: modelClassKind,
			model: v1alpha1.Model{
				ObjectMeta: v1alpha1.ObjectMeta{Name: "missing-class", Namespace: "default"},
				Spec:       v1alpha1.ModelSpec{ClassName: "dependency", ModelID: "model", Endpoint: "https://models.example.test"},
			},
			wantReason: "ModelClassMissing",
		},
		{
			name:        "credential",
			missingKind: "SecretRef",
			model: v1alpha1.Model{
				ObjectMeta: v1alpha1.ObjectMeta{Name: "missing-credential", Namespace: "default"},
				Spec: v1alpha1.ModelSpec{
					Provider:      "openai-compatible",
					ModelID:       "model",
					Endpoint:      "https://models.example.test",
					CredentialRef: &v1alpha1.LocalObjectReference{Name: "dependency"},
				},
			},
			wantReason: "CredentialMissing",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			memoryStore := newMemStore()
			storeModel(t, memoryStore, test.model)
			s := &modelGetErrorStore{
				Store: memoryStore,
				kind:  test.missingKind,
				name:  "dependency",
				err:   fmt.Errorf("wrapped missing dependency: %w", store.ErrNotFound),
			}
			r := NewModelReconciler(s, testRegistry(), zaptest.NewLogger(t))

			reconcileModel(t, r, "default", test.model.ObjectMeta.Name)

			condition := findModelCondition(getModel(t, memoryStore, "default", test.model.ObjectMeta.Name), "Ready")
			if condition == nil || condition.Reason != test.wantReason {
				t.Fatalf("Ready condition = %+v, want reason %q", condition, test.wantReason)
			}
		})
	}
}

func newInternalSecretRef(ns, name, key, value string) v1alpha1.SecretRef {
	payload, _ := json.Marshal(map[string]string{key: value})
	return v1alpha1.SecretRef{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "SecretRef"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: name, Namespace: ns, Annotations: map[string]string{internalSecretDataAnnotation: string(payload)}},
		Spec: v1alpha1.SecretRefSpec{
			BackendType: "internal",
			BackendRef:  "secret/" + name,
			Key:         key,
		},
	}
}

// TC-0056: Create a Model with valid credentials; verify status shows available=true and capabilities are reported.
func TestModelReconciler_TC0056_ValidCredentialMarksAvailable(t *testing.T) {
	s := newMemStore()
	r := NewModelReconciler(s, testRegistry(), zaptest.NewLogger(t))

	storeSecretRefForModel(t, s, newInternalSecretRef("default", "openai-credentials", "api-key", "token-openai"))

	model := v1alpha1.Model{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Model"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "gpt-4o", Namespace: "default"},
		Spec: v1alpha1.ModelSpec{
			Provider:      "openai-compatible",
			ModelID:       "gpt-4o",
			Endpoint:      "https://model-gateway.internal/v1",
			Capabilities:  []string{"chat", "tool-calling"},
			CredentialRef: &v1alpha1.LocalObjectReference{Name: "openai-credentials"},
		},
	}
	storeModel(t, s, model)

	reconcileModel(t, r, "default", "gpt-4o")

	got := getModel(t, s, "default", "gpt-4o")
	if !got.Status.Available {
		t.Fatal("expected model to be available")
	}
	if got.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("phase = %q, want Ready", got.Status.Phase)
	}
	if len(got.Status.Capabilities) != 2 || got.Status.Capabilities[0] != "chat" || got.Status.Capabilities[1] != "tool-calling" {
		t.Fatalf("capabilities = %v, want [chat tool-calling]", got.Status.Capabilities)
	}
	if got.Status.LastCheck == "" {
		t.Fatal("expected LastCheck to be set")
	}
	if _, err := time.Parse(time.RFC3339, got.Status.LastCheck); err != nil {
		t.Fatalf("LastCheck is not RFC3339: %v", err)
	}
	cond := findModelCondition(got, "Ready")
	if cond == nil || cond.Status != "True" {
		t.Fatalf("expected Ready=True condition, got %+v", cond)
	}
}

// TC-0057: Create a Model with invalid endpoint; verify status shows available=false with error description.
func TestModelReconciler_TC0057_InvalidEndpointMarksUnavailable(t *testing.T) {
	s := newMemStore()
	r := NewModelReconciler(s, testRegistry(), zaptest.NewLogger(t))

	model := v1alpha1.Model{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Model"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "bad-endpoint", Namespace: "default"},
		Spec: v1alpha1.ModelSpec{
			Provider: "openai-compatible",
			ModelID:  "gpt-4o-mini",
			Endpoint: "not-a-uri",
		},
	}
	storeModel(t, s, model)

	reconcileModel(t, r, "default", "bad-endpoint")

	got := getModel(t, s, "default", "bad-endpoint")
	if got.Status.Available {
		t.Fatal("expected model to be unavailable")
	}
	if got.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
	cond := findModelCondition(got, "Ready")
	if cond == nil {
		t.Fatal("expected Ready condition")
	}
	if cond.Reason != "ValidationFailed" {
		t.Fatalf("condition reason = %q, want ValidationFailed", cond.Reason)
	}
	if !strings.Contains(cond.Message, "valid URL") {
		t.Fatalf("condition message = %q, want endpoint error description", cond.Message)
	}
}

// TC-0058: Create two Models (different providers); reference each from separate Agents; verify both work independently.
func TestModelReconciler_TC0058_MultiProviderModelsWorkIndependently(t *testing.T) {
	s := newMemStore()
	r := NewModelReconciler(s, testRegistry(), zaptest.NewLogger(t))

	storeSecretRefForModel(t, s, newInternalSecretRef("default", "openai-cred", "api-key", "token-openai"))
	storeSecretRefForModel(t, s, newInternalSecretRef("default", "azure-cred", "api-key", "token-azure"))

	openAIModel := v1alpha1.Model{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Model"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "openai-model", Namespace: "default"},
		Spec: v1alpha1.ModelSpec{
			Provider:      "openai-compatible",
			ModelID:       "gpt-4o",
			Endpoint:      "https://openai-gateway.internal/v1",
			Capabilities:  []string{"chat"},
			CredentialRef: &v1alpha1.LocalObjectReference{Name: "openai-cred"},
		},
	}
	azureModel := v1alpha1.Model{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Model"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "azure-model", Namespace: "default"},
		Spec: v1alpha1.ModelSpec{
			Provider:      "azure-openai",
			ModelID:       "gpt-4o",
			Endpoint:      "https://my-foundry.openai.azure.com/openai/deployments/gpt-4o",
			Capabilities:  []string{"chat", "vision"},
			CredentialRef: &v1alpha1.LocalObjectReference{Name: "azure-cred"},
		},
	}

	storeModel(t, s, openAIModel)
	storeModel(t, s, azureModel)

	reconcileModel(t, r, "default", "openai-model")
	reconcileModel(t, r, "default", "azure-model")

	openAIResult := getModel(t, s, "default", "openai-model")
	if !openAIResult.Status.Available {
		t.Fatal("expected openai-model to be available")
	}
	azureResult := getModel(t, s, "default", "azure-model")
	if !azureResult.Status.Available {
		t.Fatal("expected azure-model to be available")
	}

	agentA := v1alpha1.Agent{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Agent"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "agent-openai", Namespace: "default"},
		Spec:       v1alpha1.AgentSpec{ModelRef: v1alpha1.LocalObjectReference{Name: "openai-model"}},
	}
	agentB := v1alpha1.Agent{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Agent"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "agent-azure", Namespace: "default"},
		Spec:       v1alpha1.AgentSpec{ModelRef: v1alpha1.LocalObjectReference{Name: "azure-model"}},
	}
	storeAgent(t, s, agentA)
	storeAgent(t, s, agentB)

	resolver := newStoreSecretResolver(s)
	openAISecrets, err := resolver.ResolveAgentRuntimeSecrets(context.Background(), "default", &v1alpha1.AgentInstance{Spec: v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "agent-openai"}}})
	if err != nil {
		t.Fatalf("resolve openai agent secrets: %v", err)
	}
	azureSecrets, err := resolver.ResolveAgentRuntimeSecrets(context.Background(), "default", &v1alpha1.AgentInstance{Spec: v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "agent-azure"}}})
	if err != nil {
		t.Fatalf("resolve azure agent secrets: %v", err)
	}

	if got := openAISecrets["model.api-key"]; got != "token-openai" {
		t.Fatalf("openai model secret = %q, want token-openai", got)
	}
	if got := azureSecrets["model.api-key"]; got != "token-azure" {
		t.Fatalf("azure model secret = %q, want token-azure", got)
	}
}

func TestStoreSecretResolver_MalformedSecretDoesNotBlockValidBindings(t *testing.T) {
	s := newMemStore()
	storeAgent(t, s, v1alpha1.Agent{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "agent", Namespace: "default"},
		Spec: v1alpha1.AgentSpec{
			ModelRef: v1alpha1.LocalObjectReference{Name: "model"},
			Tools:    []v1alpha1.AgentToolRef{{Ref: v1alpha1.LocalObjectReference{Name: "search"}}},
		},
	})
	storeModel(t, s, v1alpha1.Model{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "model", Namespace: "default"},
		Spec: v1alpha1.ModelSpec{
			CredentialRef: &v1alpha1.LocalObjectReference{Name: "malformed-model-secret"},
		},
	})
	storeTool(t, s, v1alpha1.Tool{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "search", Namespace: "default"},
		Spec: v1alpha1.ToolSpec{
			SecretRefs: []v1alpha1.ToolSecretRef{{Name: "search-secret", Key: "API_KEY"}},
		},
	})
	storeSecretRefForModel(t, s, v1alpha1.SecretRef{
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:        "malformed-model-secret",
			Namespace:   "default",
			Annotations: map[string]string{internalSecretDataAnnotation: "not-json"},
		},
		Spec: v1alpha1.SecretRefSpec{BackendType: "internal", Key: "api-key"},
	})
	storeSecretRefForModel(t, s, newInternalSecretRef("default", "search-secret", "api-key", "search-token"))

	resolved, err := newStoreSecretResolver(s).ResolveAgentRuntimeSecrets(
		context.Background(),
		"default",
		&v1alpha1.AgentInstance{Spec: v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "agent"}}},
	)
	if err != nil {
		t.Fatalf("ResolveAgentRuntimeSecrets() error = %v", err)
	}
	if _, ok := resolved["model.api-key"]; ok {
		t.Fatalf("malformed model secret was resolved: %+v", resolved)
	}
	if got := resolved["tool.search.API_KEY"]; got != "search-token" {
		t.Fatalf("valid tool secret = %q, want search-token", got)
	}
}

func TestStoreSecretResolver_WrappedNotFoundIsOptional(t *testing.T) {
	for _, test := range []struct {
		name      string
		kind      string
		storeBase func(*memStore)
	}{
		{name: "missing agent", kind: "Agent"},
		{
			name: "missing model",
			kind: "Model",
			storeBase: func(s *memStore) {
				storeAgent(t, s, v1alpha1.Agent{
					ObjectMeta: v1alpha1.ObjectMeta{Name: "agent", Namespace: "default"},
					Spec:       v1alpha1.AgentSpec{ModelRef: v1alpha1.LocalObjectReference{Name: "model"}},
				})
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := newMemStore()
			if test.storeBase != nil {
				test.storeBase(base)
			}
			resolver := newStoreSecretResolver(&modelGetErrorStore{
				Store: base,
				kind:  test.kind,
				name:  strings.ToLower(test.kind),
				err:   fmt.Errorf("wrapped missing %s: %w", test.kind, store.ErrNotFound),
			})

			resolved, err := resolver.ResolveAgentRuntimeSecrets(
				context.Background(),
				"default",
				&v1alpha1.AgentInstance{Spec: v1alpha1.AgentInstanceSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "agent"}}},
			)
			if err != nil {
				t.Fatalf("ResolveAgentRuntimeSecrets() error = %v, want nil", err)
			}
			if len(resolved) != 0 {
				t.Fatalf("resolved secrets = %v, want none", resolved)
			}
		})
	}
}

func TestModelReconciler_DerivesProviderFromModelClass(t *testing.T) {
	s := newMemStore()
	r := NewModelReconciler(s, testRegistry(), zaptest.NewLogger(t))
	storeModelClass(t, s, v1alpha1.ModelClass{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "ModelClass"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "foundry-class"},
		Spec:       v1alpha1.ModelClassSpec{Provisioner: "foundry"},
	})
	storeModel(t, s, v1alpha1.Model{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Model"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "class-model", Namespace: "default"},
		Spec: v1alpha1.ModelSpec{
			ClassName: "foundry-class",
			ModelID:   "gpt-4o",
			Endpoint:  "https://models.example.test",
		},
	})

	reconcileModel(t, r, "default", "class-model")

	got := getModel(t, s, "default", "class-model")
	if !got.Status.Available || got.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("status = available %v, phase %q; want available Ready", got.Status.Available, got.Status.Phase)
	}
}

func TestModelReconciler_RejectsProviderConflictingWithModelClass(t *testing.T) {
	s := newMemStore()
	r := NewModelReconciler(s, testRegistry(), zaptest.NewLogger(t))
	storeModelClass(t, s, v1alpha1.ModelClass{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "ModelClass"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "foundry-class"},
		Spec:       v1alpha1.ModelClassSpec{Provisioner: "foundry"},
	})
	storeModel(t, s, v1alpha1.Model{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Model"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "conflicting-model", Namespace: "default"},
		Spec: v1alpha1.ModelSpec{
			ClassName: "foundry-class",
			Provider:  "anthropic",
			ModelID:   "claude",
			Endpoint:  "https://models.example.test",
		},
	})

	reconcileModel(t, r, "default", "conflicting-model")

	got := getModel(t, s, "default", "conflicting-model")
	if got.Status.Available || got.Status.Phase != v1alpha1.PhaseFailed {
		t.Fatalf("status = available %v, phase %q; want unavailable Failed", got.Status.Available, got.Status.Phase)
	}
	condition := findModelCondition(got, "Ready")
	if condition == nil || condition.Reason != "ProviderConflict" || !strings.Contains(condition.Message, "foundry-class") {
		t.Fatalf("Ready condition = %+v, want ProviderConflict naming the ModelClass", condition)
	}
}

// TestModelReconciler_StableStatusIsNoOp verifies that reconciling a model whose
// status has not changed (same Phase + Available) does not write to the store,
// preventing pg_notify loops.
func TestModelReconciler_StableStatusIsNoOp(t *testing.T) {
	s := newMemStore()
	r := NewModelReconciler(s, testRegistry(), zaptest.NewLogger(t))

	storeSecretRefForModel(t, s, newInternalSecretRef("default", "stable-cred", "api-key", "tok"))

	model := v1alpha1.Model{
		TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "Model"},
		ObjectMeta: v1alpha1.ObjectMeta{Name: "stable-model", Namespace: "default"},
		Spec: v1alpha1.ModelSpec{
			Provider:      "openai-compatible",
			ModelID:       "gpt-4o",
			Endpoint:      "https://model-gateway.internal/v1",
			Capabilities:  []string{"chat"},
			CredentialRef: &v1alpha1.LocalObjectReference{Name: "stable-cred"},
		},
	}
	storeModel(t, s, model)

	// First reconcile: transitions from zero-value to Available/Ready.
	reconcileModel(t, r, "default", "stable-model")

	got := getModel(t, s, "default", "stable-model")
	if !got.Status.Available || got.Status.Phase != v1alpha1.PhaseReady {
		t.Fatalf("precondition: expected Available+Ready, got available=%v phase=%q", got.Status.Available, got.Status.Phase)
	}
	rvAfterFirst := got.ObjectMeta.ResourceVersion

	// Second reconcile: status is identical, should NOT write to store.
	res, err := r.Reconcile(context.Background(), Request{Namespace: "default", Name: "stable-model", Kind: "Model"})
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if res.Requeue || res.RequeueAfter > 0 {
		t.Fatalf("expected no requeue, got %+v", res)
	}

	after := getModel(t, s, "default", "stable-model")
	if after.ObjectMeta.ResourceVersion != rvAfterFirst {
		t.Fatalf("expected no store write on stable reconcile; RV changed from %d to %d", rvAfterFirst, after.ObjectMeta.ResourceVersion)
	}
}
