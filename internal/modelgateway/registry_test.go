// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package modelgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/modelinvocation"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
)

type registeredInvocationAdapter struct {
	calls int
}

func (a *registeredInvocationAdapter) Name() string { return "custom-adapter" }
func (a *registeredInvocationAdapter) Validate(context.Context, *api.ModelSpec, *api.ModelClassSpec) error {
	return nil
}
func (a *registeredInvocationAdapter) Probe(context.Context, *api.ModelSpec, *cpi.Credentials) (*cpi.HealthResult, error) {
	return &cpi.HealthResult{Available: true}, nil
}
func (a *registeredInvocationAdapter) InvocationContract(name string) (modelinvocation.Contract, error) {
	if name != "custom-chat-v1" {
		return modelinvocation.Contract{}, fmt.Errorf("unsupported contract")
	}
	return modelinvocation.Contract{Name: name, Operation: "chat", Protocol: "custom-chat", CompletionLimitParameter: "max_tokens"}, nil
}
func (a *registeredInvocationAdapter) Complete(_ context.Context, request *cpi.CompletionRequest) (*cpi.CompletionResponse, error) {
	request, err := cpi.PrepareCompletion(request, a)
	if err != nil {
		return nil, err
	}
	if request.Model != "opaque-deployment" || request.Parameters["max_tokens"] != "128" || request.Creds.Data["apiKey"] != "upstream-secret" {
		return nil, fmt.Errorf("invocation configuration or credentials were not preserved")
	}
	a.calls++
	return &cpi.CompletionResponse{Content: "registered", Usage: cpi.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2, Reported: true}}, nil
}
func (a *registeredInvocationAdapter) Embed(context.Context, *cpi.EmbedRequest) (*cpi.EmbedResponse, error) {
	return nil, fmt.Errorf("unsupported operation")
}

func TestGatewayAcceptsAdministratorRegisteredInvocationAdapter(t *testing.T) {
	_, storage, authorizer, issuer, binding, _ := fixture(t)
	registry := cpi.NewRegistry()
	adapter := &registeredInvocationAdapter{}
	registry.MustRegister(adapter)
	server, err := New(storage, issuer, authorizer, nil, zap.NewNop(), WithCPIRegistry(registry))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(storage, issuer, authorizer, nil, zap.NewNop(), WithCPIRegistry(nil)); err == nil {
		t.Fatal("missing administrator registry was silently replaced")
	}
	token := registeredAdapterCapability(t, storage, issuer, binding, adapter)
	response := httptest.NewRecorder()
	server.ServeHTTP(response, completionRequest(token))
	if response.Code != http.StatusOK || adapter.calls != 1 {
		t.Fatalf("registered adapter failed: status=%d body=%s calls=%d", response.Code, response.Body.String(), adapter.calls)
	}
}

type recordedCalls struct{ outcomes []cpi.CallOutcome }

func (r *recordedCalls) RecordCall(outcome cpi.CallOutcome) { r.outcomes = append(r.outcomes, outcome) }

func TestGatewayReportsProviderCallsToTheConfiguredRecorder(t *testing.T) {
	_, storage, authorizer, issuer, binding, _ := fixture(t)
	registry := cpi.NewRegistry()
	adapter := &registeredInvocationAdapter{}
	registry.MustRegister(adapter)
	recorder := &recordedCalls{}
	server, err := New(storage, issuer, authorizer, nil, zap.NewNop(), WithCPIRegistry(registry), WithCallRecorder(recorder))
	if err != nil {
		t.Fatal(err)
	}
	token := registeredAdapterCapability(t, storage, issuer, binding, adapter)

	response := httptest.NewRecorder()
	server.ServeHTTP(response, completionRequest(token))
	if response.Code != http.StatusOK || adapter.calls != 1 {
		t.Fatalf("registered adapter failed: status=%d body=%s calls=%d", response.Code, response.Body.String(), adapter.calls)
	}
	if len(recorder.outcomes) != 1 {
		t.Fatalf("recorded %d outcomes, want 1: %+v", len(recorder.outcomes), recorder.outcomes)
	}
	got := recorder.outcomes[0]
	if got.Provider != adapter.Name() || got.Operation != cpi.OperationComplete || got.Model != "opaque-deployment" || !got.Succeeded() {
		t.Fatalf("outcome = %+v, want a successful complete call to opaque-deployment through %s", got, adapter.Name())
	}
	if _, supported := mustDriver(t, registry, adapter.Name()).(cpi.InvocationContractProvider); !supported {
		t.Fatal("observing the registry hid the adapter's invocation contract support")
	}
}

func TestGatewayWithoutRecorderLeavesTheRegistryUnobserved(t *testing.T) {
	_, storage, authorizer, issuer, _, _ := fixture(t)
	registry := cpi.NewRegistry()
	adapter := &registeredInvocationAdapter{}
	registry.MustRegister(adapter)
	if _, err := New(storage, issuer, authorizer, nil, zap.NewNop(), WithCPIRegistry(registry)); err != nil {
		t.Fatal(err)
	}
	if got := mustDriver(t, registry, adapter.Name()); got != cpi.Driver(adapter) {
		t.Fatal("a gateway without a recorder must not wrap the registry's drivers")
	}
}

func mustDriver(t *testing.T, registry *cpi.Registry, name string) cpi.Driver {
	t.Helper()
	driver, ok := registry.Get(name)
	if !ok {
		t.Fatalf("driver %q is not registered", name)
	}
	return driver
}

// registeredAdapterCapability points the fixture Model at the registered adapter
// and returns a runtime capability for it.
func registeredAdapterCapability(t *testing.T, storage *memoryStore, issuer *runtimeauth.ModelSessionCapabilityIssuer, binding runtimeauth.ModelSessionCapabilityBinding, adapter *registeredInvocationAdapter) string {
	t.Helper()
	var model api.Model
	if err := json.Unmarshal(storage.data["Model/team-a/mock-model"], &model); err != nil {
		t.Fatal(err)
	}
	model.Spec.Provider, model.Spec.ModelID = adapter.Name(), "opaque-deployment"
	model.Spec.InvocationProfileRef, model.Spec.InvocationContract = &api.ModelInvocationProfileReference{Name: "generic-v1"}, "custom-chat-v1"
	maximum := int64(128)
	put(t, storage, "ModelInvocationProfile", "team-a", "generic-v1", api.ModelInvocationProfile{
		ObjectMeta: api.ObjectMeta{Name: "generic-v1", Namespace: "team-a", UID: "profile-uid"},
		Spec:       api.ModelInvocationProfileSpec{Operation: "chat", Defaults: api.InvocationDefaults{MaxCompletionTokens: &maximum}},
	})
	model.Spec.InvocationProfileRef.SpecDigest = invocationProfileDigest(t, api.ModelInvocationProfileSpec{Operation: "chat", Defaults: api.InvocationDefaults{MaxCompletionTokens: &maximum}})
	put(t, storage, "Model", "team-a", model.Name, model)
	invocation, err := modelinvocation.Load(context.Background(), storage, &model)
	if err != nil {
		t.Fatal(err)
	}
	binding.InvocationProfileBinding = &invocation.Binding
	token, err := issuer.Issue(binding)
	if err != nil {
		t.Fatal(err)
	}
	return token
}
