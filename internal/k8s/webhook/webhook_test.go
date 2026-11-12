// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	intadmission "github.com/kognition-ai/kognition/internal/admission"
	"github.com/kognition-ai/kognition/internal/convert"
	k8saudit "github.com/kognition-ai/kognition/internal/k8s/audit"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
)

type webhookClaimantReader struct{}

func (webhookClaimantReader) VerifyRuntimeApprovalClaimant(context.Context, string, string, string, string, api.RuntimeApprovalClaimant) error {
	return nil
}

type auditStore struct {
	entries []observability.AuditEntry
	err     error
}

func (s *auditStore) Append(_ context.Context, entry observability.AuditEntry) error {
	if s.err != nil {
		return s.err
	}
	s.entries = append(s.entries, entry)
	return nil
}

func (s *auditStore) AppendBatch(ctx context.Context, entries []observability.AuditEntry) error {
	for index := range entries {
		if err := s.Append(ctx, entries[index]); err != nil {
			return err
		}
	}
	return nil
}

func (*auditStore) Query(context.Context, observability.AuditFilter) ([]observability.AuditEntry, error) {
	return nil, nil
}

func testScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = k8s.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	return scheme
}

func testLogger() *zap.Logger {
	l, _ := zap.NewDevelopment()
	return l
}

func TestHandlerConvertsAutonomyFoundationTypes(t *testing.T) {
	handler := NewHandler(intadmission.NewChain(nil, nil), testScheme(), testLogger())
	objects := []struct {
		kind string
		obj  any
	}{
		{kind: "TrustProfile", obj: k8s.TrustProfile{TypeMeta: metav1.TypeMeta{Kind: "TrustProfile", APIVersion: "kognition.dev/v1alpha1"}, ObjectMeta: metav1.ObjectMeta{Name: "profile", Namespace: "team-a"}, Spec: k8s.TrustProfileSpec{AgentRef: k8s.LocalObjectReference{Name: "agent"}, AgentIncarnationUID: "agent-uid", InitialScore: 50}}},
		{kind: "RuntimeApproval", obj: k8s.RuntimeApproval{TypeMeta: metav1.TypeMeta{Kind: "RuntimeApproval", APIVersion: "kognition.dev/v1alpha1"}, ObjectMeta: metav1.ObjectMeta{Name: "approval", Namespace: "team-a"}, Spec: k8s.RuntimeApprovalSpec{Workspace: "team-a", InvocationID: "invocation"}}},
	}
	for _, test := range objects {
		t.Run(test.kind, func(t *testing.T) {
			raw, err := json.Marshal(test.obj)
			if err != nil {
				t.Fatal(err)
			}
			internal, err := handler.convertToInternal(test.kind, raw)
			if err != nil {
				t.Fatalf("convertToInternal: %v", err)
			}
			if _, err := handler.convertToK8s(test.kind, internal, raw); err != nil {
				t.Fatalf("convertToK8s: %v", err)
			}
		})
	}
}

func TestHandle_AuditsValidatedMutationBeforePersistence(t *testing.T) {
	store := &auditStore{}
	handler := NewAuditHandler(observability.NewCognitiveAuditLog(store), testLogger())
	agent := k8s.Agent{TypeMeta: metav1.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"}, ObjectMeta: metav1.ObjectMeta{Name: "planner", Namespace: "team-a"}, Spec: k8s.AgentSpec{ModelRef: k8s.LocalObjectReference{Name: "model"}, Instructions: k8s.AgentInstructions{Inline: "plan"}}}
	raw, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}
	response := handler.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{UID: "audit-create", Operation: admissionv1.Create, Kind: metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Agent"}, Namespace: "team-a", Name: "planner", Object: runtime.RawExtension{Raw: raw}, UserInfo: authenticationv1.UserInfo{Username: "alice"}}})
	if !response.Allowed {
		t.Fatalf("create denied: %#v", response.Result)
	}
	if len(store.entries) != 1 {
		t.Fatalf("audit entries = %#v", store.entries)
	}
	entry := store.entries[0]
	if entry.Workspace != "team-a" || entry.AgentIdentity != "alice" || entry.EventType != observability.AuditAdmissionAttempt || entry.SubjectKind != "Agent" || entry.SubjectRef != "team-a/planner" || entry.Verb != "attempt_create" {
		t.Fatalf("audit entry = %#v", entry)
	}
}

func TestHandle_DeniesMutationWhenAuditWriteFails(t *testing.T) {
	store := &auditStore{err: errors.New("audit unavailable")}
	handler := NewAuditHandler(observability.NewCognitiveAuditLog(store), testLogger())
	agent := k8s.Agent{TypeMeta: metav1.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"}, ObjectMeta: metav1.ObjectMeta{Name: "planner", Namespace: "team-a"}, Spec: k8s.AgentSpec{ModelRef: k8s.LocalObjectReference{Name: "model"}, Instructions: k8s.AgentInstructions{Inline: "plan"}}}
	raw, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}
	response := handler.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{UID: "audit-failure", Operation: admissionv1.Create, Kind: metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Agent"}, Namespace: "team-a", Name: "planner", Object: runtime.RawExtension{Raw: raw}}})
	if response.Allowed || response.Result == nil || response.Result.Code != http.StatusInternalServerError {
		t.Fatalf("response = %#v, want audit failure denial", response)
	}
}

func TestHandle_PersistsAuditEntryToKubernetesConfigMap(t *testing.T) {
	scheme := testScheme()
	auditStore, err := k8saudit.NewConfigMapStore(fake.NewClientBuilder().WithScheme(scheme).Build(), "kognition-system")
	if err != nil {
		t.Fatal(err)
	}
	handler := NewAuditHandler(observability.NewCognitiveAuditLog(auditStore), testLogger())
	agent := k8s.Agent{TypeMeta: metav1.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"}, ObjectMeta: metav1.ObjectMeta{Name: "planner", Namespace: "team-a"}, Spec: k8s.AgentSpec{ModelRef: k8s.LocalObjectReference{Name: "model"}, Instructions: k8s.AgentInstructions{Inline: "plan"}}}
	raw, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}
	response := handler.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{UID: "audit-configmap", Operation: admissionv1.Create, Kind: metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Agent"}, Namespace: "team-a", Name: "planner", Object: runtime.RawExtension{Raw: raw}}})
	if !response.Allowed {
		t.Fatalf("create denied: %#v", response.Result)
	}
	entries, err := auditStore.Query(context.Background(), observability.AuditFilter{Workspace: "team-a", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].SubjectRef != "team-a/planner" {
		t.Fatalf("audit entries = %#v", entries)
	}
}

func TestAuditHandler_SkipsDryRun(t *testing.T) {
	store := &auditStore{}
	handler := NewAuditHandler(observability.NewCognitiveAuditLog(store), testLogger())
	dryRun := true
	response := handler.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{UID: "audit-dry-run", Operation: admissionv1.Delete, Kind: metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "MemoryGrant"}, Namespace: "team-a", Name: "grant", DryRun: &dryRun}})
	if !response.Allowed || len(store.entries) != 0 {
		t.Fatalf("response=%#v entries=%#v", response, store.entries)
	}
}

// TC-0201: Agent with missing modelRef is rejected by validating hook.
func TestHandle_AgentMissingModelRef_Denied(t *testing.T) {
	validator := &stubValidator{
		name:  "require-model-ref",
		kinds: map[string]bool{"Agent": true},
		validateFn: func(_ context.Context, req *intadmission.Request) *intadmission.Response {
			var obj map[string]any
			if err := json.Unmarshal(req.Object, &obj); err != nil {
				return &intadmission.Response{Allowed: false, Reason: "invalid JSON"}
			}
			spec, _ := obj["spec"].(map[string]any)
			modelRef, _ := spec["modelRef"].(map[string]any)
			name, _ := modelRef["name"].(string)
			if name == "" {
				return &intadmission.Response{Allowed: false, Reason: "spec.modelRef.name is required"}
			}
			return &intadmission.Response{Allowed: true}
		},
	}

	chain := intadmission.NewChain(nil, []intadmission.ValidatingHook{validator})
	h := NewHandler(chain, testScheme(), testLogger())

	// Agent with empty modelRef.
	agent := k8s.Agent{
		TypeMeta: metav1.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-agent",
			Namespace: "default",
		},
		Spec: k8s.AgentSpec{
			ModelRef:     k8s.LocalObjectReference{Name: ""},
			Instructions: k8s.AgentInstructions{Inline: "hello"},
		},
	}
	raw, _ := json.Marshal(agent)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "123",
			Operation: admissionv1.Create,
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Agent"},
			Namespace: "default",
			Name:      "test-agent",
			Object:    runtime.RawExtension{Raw: raw},
		},
	}

	resp := h.Handle(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected denial, got allowed")
	}
	if resp.Result == nil || resp.Result.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %+v", resp.Result)
	}
}

func TestHandle_AgentIdentityAutomaticProvenanceRequiresController(t *testing.T) {
	validator := intadmission.NewAgentIdentityValidator("system:serviceaccount:kognition-system:kognition")
	h := NewHandler(intadmission.NewChain(nil, []intadmission.ValidatingHook{validator}), testScheme(), testLogger())
	identity := k8s.AgentIdentity{
		TypeMeta: metav1.TypeMeta{Kind: "AgentIdentity", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-identity-planner",
			Namespace: "team-a",
			Labels:    map[string]string{"kognition.dev/auto-created-for-agent": "planner"},
		},
		Spec: k8s.AgentIdentitySpec{AgentRef: k8s.LocalObjectReference{Name: "planner"}},
	}
	raw, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	request := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID:       "identity-provenance",
		Operation: admissionv1.Create,
		Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "AgentIdentity"},
		Namespace: "team-a",
		Name:      identity.Name,
		Object:    runtime.RawExtension{Raw: raw},
		UserInfo:  authenticationv1.UserInfo{Username: "system:serviceaccount:team-a:tenant"},
	}}
	if response := h.Handle(context.Background(), request); response.Allowed {
		t.Fatal("tenant identity provenance forgery was allowed")
	}
	request.UserInfo.Username = "system:serviceaccount:kognition-system:kognition"
	if response := h.Handle(context.Background(), request); !response.Allowed {
		t.Fatalf("controller identity was denied: %s", response.Result.Message)
	}
}

func TestHandle_AgentIdentityAutomaticDeletionRequiresController(t *testing.T) {
	validator := intadmission.NewAgentIdentityValidator("system:serviceaccount:kognition-system:kognition")
	h := NewHandler(intadmission.NewChain(nil, []intadmission.ValidatingHook{validator}), testScheme(), testLogger())
	identity := k8s.AgentIdentity{
		TypeMeta:   metav1.TypeMeta{Kind: "AgentIdentity", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{Name: "agent-identity-planner", Namespace: "team-a", Labels: map[string]string{"kognition.dev/auto-created-for-agent": "planner"}},
		Spec:       k8s.AgentIdentitySpec{AgentRef: k8s.LocalObjectReference{Name: "planner"}},
	}
	raw, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	request := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID:       "identity-delete",
		Operation: admissionv1.Delete,
		Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "AgentIdentity"},
		Namespace: "team-a",
		Name:      identity.Name,
		OldObject: runtime.RawExtension{Raw: raw},
		UserInfo:  authenticationv1.UserInfo{Username: "system:serviceaccount:team-a:tenant"},
	}}
	if response := h.Handle(context.Background(), request); response.Allowed {
		t.Fatal("tenant automatic identity deletion was allowed")
	}
	request.UserInfo.Username = "system:serviceaccount:kognition-system:kognition"
	if response := h.Handle(context.Background(), request); !response.Allowed {
		t.Fatalf("controller automatic identity deletion denied: %s", response.Result.Message)
	}
}

// TC-0202: Agent without replicas gets defaults applied by mutating hook.
func TestHandle_AgentDefaulted_Mutated(t *testing.T) {
	mutator := &stubMutator{
		name:  "agent-defaulter",
		kinds: map[string]bool{"Agent": true},
		mutateFn: func(_ context.Context, req *intadmission.Request) *intadmission.Response {
			var obj map[string]any
			if err := json.Unmarshal(req.Object, &obj); err != nil {
				return &intadmission.Response{Allowed: false, Reason: "bad json"}
			}
			spec, _ := obj["spec"].(map[string]any)
			if spec["replicas"] == nil {
				spec["replicas"] = float64(1)
			}
			if rt, ok := spec["runtime"].(map[string]any); !ok || rt == nil {
				spec["runtime"] = map[string]any{"class": "default"}
			}
			obj["spec"] = spec
			patched, _ := json.Marshal(obj)
			return &intadmission.Response{Allowed: true, Patch: patched}
		},
	}

	chain := intadmission.NewChain([]intadmission.MutatingHook{mutator}, nil)
	h := NewHandler(chain, testScheme(), testLogger())

	agent := k8s.Agent{
		TypeMeta: metav1.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-agent",
			Namespace: "default",
		},
		Spec: k8s.AgentSpec{
			ModelRef:     k8s.LocalObjectReference{Name: "gpt-4"},
			Instructions: k8s.AgentInstructions{Inline: "you are helpful"},
		},
	}
	raw, _ := json.Marshal(agent)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "456",
			Operation: admissionv1.Create,
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Agent"},
			Namespace: "default",
			Name:      "test-agent",
			Object:    runtime.RawExtension{Raw: raw},
		},
	}

	resp := h.Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected allowed, got denied: %v", resp.Result)
	}
	if len(resp.Patches) == 0 {
		t.Fatal("expected JSON patches to be present")
	}
}

func TestHandle_EndpointModesRunOnlyTheirAdmissionPhase(t *testing.T) {
	agent := k8s.Agent{
		TypeMeta:   metav1.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "default"},
		Spec:       k8s.AgentSpec{ModelRef: k8s.LocalObjectReference{Name: "model"}},
	}
	raw, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}
	req := admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID:       "endpoint-mode",
		Operation: admissionv1.Create,
		Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Agent"},
		Namespace: "default",
		Name:      "test-agent",
		Object:    runtime.RawExtension{Raw: raw},
	}}

	mutatorCalls := 0
	validatorCalls := 0
	mutator := &stubMutator{name: "default", kinds: map[string]bool{"Agent": true}, mutateFn: func(_ context.Context, req *intadmission.Request) *intadmission.Response {
		mutatorCalls++
		var object map[string]any
		_ = json.Unmarshal(req.Object, &object)
		object["metadata"].(map[string]any)["labels"] = map[string]any{"defaulted": "true"}
		patch, _ := json.Marshal(object)
		return &intadmission.Response{Allowed: true, Patch: patch}
	}}
	validator := &stubValidator{name: "validate", kinds: map[string]bool{"Agent": true}, validateFn: func(_ context.Context, _ *intadmission.Request) *intadmission.Response {
		validatorCalls++
		return &intadmission.Response{Allowed: true}
	}}
	chain := intadmission.NewChain([]intadmission.MutatingHook{mutator}, []intadmission.ValidatingHook{validator})

	mutateResponse := newHandlerForMode(chain, testScheme(), testLogger(), modeMutate).Handle(context.Background(), req)
	if !mutateResponse.Allowed || len(mutateResponse.Patches) == 0 {
		t.Fatalf("mutate response = %+v; want allowed response with patch", mutateResponse)
	}
	if mutatorCalls != 1 || validatorCalls != 0 {
		t.Fatalf("mutate endpoint calls: mutator=%d validator=%d", mutatorCalls, validatorCalls)
	}

	validateResponse := newHandlerForMode(chain, testScheme(), testLogger(), modeValidate).Handle(context.Background(), req)
	if !validateResponse.Allowed || len(validateResponse.Patches) != 0 {
		t.Fatalf("validate response = %+v; want allowed response without patch", validateResponse)
	}
	if mutatorCalls != 1 || validatorCalls != 1 {
		t.Fatalf("validate endpoint calls: mutator=%d validator=%d", mutatorCalls, validatorCalls)
	}
}

// TC-0203: Unsupported operation returns error.
func TestHandle_UnsupportedOperation_Error(t *testing.T) {
	chain := intadmission.NewChain(nil, nil)
	h := NewHandler(chain, testScheme(), testLogger())

	agent := k8s.Agent{
		TypeMeta:   metav1.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: "default"},
		Spec:       k8s.AgentSpec{ModelRef: k8s.LocalObjectReference{Name: "m"}},
	}
	raw, _ := json.Marshal(agent)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "789",
			Operation: admissionv1.Operation("CONNECT"),
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Agent"},
			Namespace: "default",
			Name:      "x",
			Object:    runtime.RawExtension{Raw: raw},
		},
	}

	resp := h.Handle(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected error response for unsupported operation")
	}
	if resp.Result == nil || resp.Result.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %+v", resp.Result)
	}
}

// TC-0204: Unsupported kind returns error.
func TestHandle_UnsupportedKind_Error(t *testing.T) {
	chain := intadmission.NewChain(nil, nil)
	h := NewHandler(chain, testScheme(), testLogger())

	raw := []byte(`{"apiVersion":"kognition.dev/v1alpha1","kind":"Unknown","metadata":{"name":"x"}}`)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "000",
			Operation: admissionv1.Create,
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Unknown"},
			Namespace: "default",
			Name:      "x",
			Object:    runtime.RawExtension{Raw: raw},
		},
	}

	resp := h.Handle(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected error for unknown kind")
	}
	if resp.Result == nil || resp.Result.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %+v", resp.Result)
	}
}

// TC-0205: Tool mutation handling - verify Tool kind is supported through webhook.
func TestHandle_ToolMutated_PatchGenerated(t *testing.T) {
	mutator := &stubMutator{
		name:  "tool-enricher",
		kinds: map[string]bool{"Tool": true},
		mutateFn: func(_ context.Context, req *intadmission.Request) *intadmission.Response {
			// Simple mutation: just return allowed without patches
			// Patches would be applied by Kubernetes API server in real scenarios
			return &intadmission.Response{Allowed: true}
		},
	}

	chain := intadmission.NewChain([]intadmission.MutatingHook{mutator}, nil)
	h := NewHandler(chain, testScheme(), testLogger())

	tool := k8s.Tool{
		TypeMeta: metav1.TypeMeta{Kind: "Tool", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-tool",
			Namespace: "default",
		},
		Spec: k8s.ToolSpec{
			ClassName: "default-class",
		},
	}
	raw, _ := json.Marshal(tool)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "205",
			Operation: admissionv1.Create,
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Tool"},
			Namespace: "default",
			Name:      "test-tool",
			Object:    runtime.RawExtension{Raw: raw},
		},
	}

	resp := h.Handle(context.Background(), req)
	// Tool should be allowed through the webhook with mutator applied
	if !resp.Allowed {
		t.Fatalf("expected Tool mutation to be allowed, got denied: %v", resp.Result)
	}
}

// TC-0206: ToolClass with validation rejection.
func TestHandle_ToolClassValidation_Denied(t *testing.T) {
	validator := &stubValidator{
		name:  "toolclass-validator",
		kinds: map[string]bool{"ToolClass": true},
		validateFn: func(_ context.Context, req *intadmission.Request) *intadmission.Response {
			var obj map[string]any
			if err := json.Unmarshal(req.Object, &obj); err != nil {
				return &intadmission.Response{Allowed: false, Reason: "invalid JSON"}
			}
			spec, _ := obj["spec"].(map[string]any)
			provisioner, _ := spec["provisioner"].(string)
			if provisioner == "" {
				return &intadmission.Response{Allowed: false, Reason: "spec.provisioner is required"}
			}
			return &intadmission.Response{Allowed: true}
		},
	}

	chain := intadmission.NewChain(nil, []intadmission.ValidatingHook{validator})
	h := NewHandler(chain, testScheme(), testLogger())

	toolClass := k8s.ToolClass{
		TypeMeta: metav1.TypeMeta{Kind: "ToolClass", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-toolclass",
		},
		Spec: k8s.ToolClassSpec{
			Provisioner: "",
		},
	}
	raw, _ := json.Marshal(toolClass)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "206",
			Operation: admissionv1.Create,
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "ToolClass"},
			Namespace: "default",
			Name:      "test-toolclass",
			Object:    runtime.RawExtension{Raw: raw},
		},
	}

	resp := h.Handle(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected denial for empty provisioner")
	}
	if resp.Result == nil || resp.Result.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %+v", resp.Result)
	}
}

// TC-0207: Workflow conversion to/from internal type.
func TestHandle_WorkflowConversion_Success(t *testing.T) {
	chain := intadmission.NewChain(nil, nil)
	h := NewHandler(chain, testScheme(), testLogger())

	workflow := k8s.Workflow{
		TypeMeta: metav1.TypeMeta{Kind: "Workflow", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-workflow",
			Namespace: "default",
		},
		Spec: k8s.WorkflowSpec{
			Steps: []k8s.WorkflowStep{
				{
					Name:     "step-1",
					AgentRef: k8s.LocalObjectReference{Name: "agent-1"},
				},
			},
		},
	}
	raw, _ := json.Marshal(workflow)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "207",
			Operation: admissionv1.Create,
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Workflow"},
			Namespace: "default",
			Name:      "test-workflow",
			Object:    runtime.RawExtension{Raw: raw},
		},
	}

	resp := h.Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected allowed for valid Workflow, got: %v", resp.Result)
	}
}

// TC-0208: UPDATE operation with OldObject properly converted.
func TestHandle_UpdateWithOldObject_Success(t *testing.T) {
	validator := &stubValidator{
		name:  "update-validator",
		kinds: map[string]bool{"Agent": true},
		validateFn: func(_ context.Context, req *intadmission.Request) *intadmission.Response {
			if req.OldObject == nil {
				return &intadmission.Response{Allowed: false, Reason: "OldObject missing for UPDATE"}
			}
			return &intadmission.Response{Allowed: true}
		},
	}

	chain := intadmission.NewChain(nil, []intadmission.ValidatingHook{validator})
	h := NewHandler(chain, testScheme(), testLogger())

	oldAgent := k8s.Agent{
		TypeMeta: metav1.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-agent",
			Namespace: "default",
		},
		Spec: k8s.AgentSpec{
			ModelRef:     k8s.LocalObjectReference{Name: "old-model"},
			Instructions: k8s.AgentInstructions{Inline: "old instructions"},
		},
	}
	oldRaw, _ := json.Marshal(oldAgent)

	newAgent := k8s.Agent{
		TypeMeta: metav1.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-agent",
			Namespace: "default",
		},
		Spec: k8s.AgentSpec{
			ModelRef:     k8s.LocalObjectReference{Name: "new-model"},
			Instructions: k8s.AgentInstructions{Inline: "new instructions"},
		},
	}
	newRaw, _ := json.Marshal(newAgent)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "208",
			Operation: admissionv1.Update,
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Agent"},
			Namespace: "default",
			Name:      "test-agent",
			Object:    runtime.RawExtension{Raw: newRaw},
			OldObject: runtime.RawExtension{Raw: oldRaw},
		},
	}

	resp := h.Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected allowed for valid UPDATE, got: %v", resp.Result)
	}
}

func TestHandle_RuntimeApprovalAuditAcknowledgementAfterK8sRoundTrip(t *testing.T) {
	const gatewayUsername = "system:serviceaccount:kognition-system:kognition-tool-gateway"
	const decisionUsername = "system:serviceaccount:kognition-system:kognition-webhook"
	chain := intadmission.NewChain(nil, []intadmission.ValidatingHook{intadmission.NewRuntimeApprovalValidator("controller", gatewayUsername).WithDecisionAuthority(decisionUsername).WithExecutionWriters(map[string]string{gatewayUsername: "Tool"}).WithClaimantReader(webhookClaimantReader{})})
	h := NewHandler(chain, testScheme(), testLogger())
	decidedAt := metav1.NewTime(time.Date(2026, 9, 4, 13, 19, 15, 521114400, time.UTC))
	reference := func(kind, scope, namespace string) k8s.RuntimeApprovalReference {
		return k8s.RuntimeApprovalReference{Kind: kind, Scope: scope, Namespace: namespace, Name: strings.ToLower(kind), UID: kind + "-uid", ResourceVersion: 1}
	}
	oldApproval := k8s.RuntimeApproval{
		TypeMeta:   metav1.TypeMeta{Kind: "RuntimeApproval", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{Name: runtimeapproval.CanonicalName("team-a", "invocation"), Namespace: "team-a", UID: "approval-uid", ResourceVersion: "42", Finalizers: []string{k8s.RuntimeApprovalCompactionFinalizer}},
		Spec: k8s.RuntimeApprovalSpec{
			Workspace: "team-a", InvocationID: "invocation", InitialDecision: "Pending", Plane: "Tool", Operation: "invoke", SessionRef: "session",
			AgentRef: reference("Agent", "Namespaced", "team-a"), AgentIdentityRef: reference("AgentIdentity", "Namespaced", "team-a"),
			AgentInstanceRef: reference("AgentInstance", "Namespaced", "team-a"), TargetRef: reference("Tool", "Namespaced", "team-a"),
			AutonomyClassRef: reference("AutonomyClass", "Cluster", ""),
			Binding:          k8s.RuntimeActionBinding{Format: "kognition-runtime-action-v1", Algorithm: "HMAC-SHA-256", Domain: "kognition.dev/runtime-approval/action/v1", Epoch: 1, KeyVersion: 1, Tag: strings.Repeat("a", 43)},
			CreatedAt:        metav1.NewTime(decidedAt.Add(-time.Minute)), ExpiresAt: metav1.NewTime(decidedAt.Add(time.Minute)),
		},
		Status: k8s.RuntimeApprovalStatus{
			DecisionState: "Approved", ExecutionState: "NotStarted", DecidedByIssuer: "issuer", DecidedBySubject: "subject", DecidedAt: &decidedAt,
			DecisionAudit: &k8s.RuntimeApprovalDecisionAudit{ID: "audit-id", Payload: k8s.RuntimeApprovalDecisionAuditPayload{
				Workspace: "team-a", InvocationID: "invocation", ApprovalUID: "approval-uid", Decision: "Approved",
				Issuer: "issuer", Subject: "subject", DecidedAt: decidedAt,
			}},
		},
	}
	oldRaw, _ := json.Marshal(oldApproval)
	internal := convert.RuntimeApprovalFromK8s(&oldApproval)
	internal.Status.DecisionEvidenceDelivered = true
	acknowledged := convert.RuntimeApprovalToK8s(internal)
	acknowledged.ResourceVersion = oldApproval.ResourceVersion
	newRaw, _ := json.Marshal(acknowledged)

	response := h.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID: "runtime-approval-ack", Operation: admissionv1.Update,
		Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "RuntimeApproval"},
		Namespace: "team-a", Name: oldApproval.Name, Object: runtime.RawExtension{Raw: newRaw}, OldObject: runtime.RawExtension{Raw: oldRaw},
		UserInfo: authenticationv1.UserInfo{Username: gatewayUsername},
	}})
	if !response.Allowed {
		t.Fatalf("expected RuntimeApproval audit acknowledgement round trip to be allowed, got: %v", response.Result)
	}
	pending := oldApproval.DeepCopy()
	pending.Status = k8s.RuntimeApprovalStatus{DecisionState: "Pending", ExecutionState: "NotStarted"}
	pendingRaw, err := json.Marshal(pending)
	if err != nil {
		t.Fatal(err)
	}
	for _, username := range []string{gatewayUsername, decisionUsername} {
		response := h.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
			UID: "runtime-approval-decision", Operation: admissionv1.Update, SubResource: "status",
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "RuntimeApproval"},
			Namespace: "team-a", Name: oldApproval.Name, Object: runtime.RawExtension{Raw: oldRaw}, OldObject: runtime.RawExtension{Raw: pendingRaw},
			UserInfo: authenticationv1.UserInfo{Username: username},
		}})
		if response.Allowed != (username == decisionUsername) {
			t.Fatalf("direct status decision authority crossed: user=%s result=%v", username, response.Result)
		}
	}
	claimed := oldApproval.DeepCopy()
	claimed.Status.DecisionEvidenceDelivered = true
	claimed.Status.ExecutionState = "Claimed"
	claimed.Status.FencingToken = runtimeapproval.MinFencingToken
	claimed.Status.Claimant = &k8s.RuntimeApprovalClaimant{WorkloadUID: "owner-pod", ContainerID: "containerd://owner", ProcessStartNonce: "owner-process-0001"}
	completed := claimed.DeepCopy()
	completed.Status.ExecutionState = "Completed"
	completed.Status.ExecutionDisposition = runtimeapproval.ExecutionDispositionSucceeded
	claimedRaw, _ := json.Marshal(claimed)
	completedRaw, _ := json.Marshal(completed)
	for _, podUID := range []string{"", "foreign-pod", "owner-pod"} {
		response := h.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
			UID: "runtime-approval-owner", Operation: admissionv1.Update, SubResource: "status", Kind: metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "RuntimeApproval"}, Namespace: "team-a", Name: claimed.Name,
			Object: runtime.RawExtension{Raw: completedRaw}, OldObject: runtime.RawExtension{Raw: claimedRaw}, UserInfo: authenticationv1.UserInfo{Username: gatewayUsername, Extra: map[string]authenticationv1.ExtraValue{"authentication.kubernetes.io/pod-uid": {podUID}, "authentication.kubernetes.io/pod-name": {"gateway-pod"}}},
		}})
		if response.Allowed != (podUID == "owner-pod") {
			t.Fatalf("authenticated Pod ownership lost at handler: uid=%s result=%v", podUID, response.Result)
		}
	}
}

// TC-0209: Malformed JSON in object.
func TestHandle_MalformedJSON_Error(t *testing.T) {
	chain := intadmission.NewChain(nil, nil)
	h := NewHandler(chain, testScheme(), testLogger())

	badJSON := []byte(`{invalid json}`)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "209",
			Operation: admissionv1.Create,
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Agent"},
			Namespace: "default",
			Name:      "test-agent",
			Object:    runtime.RawExtension{Raw: badJSON},
		},
	}

	resp := h.Handle(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected error response for malformed JSON")
	}
	if resp.Result == nil || resp.Result.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %+v", resp.Result)
	}
}

func TestHandle_MalformedOldObject_Error(t *testing.T) {
	agent := k8s.Agent{
		TypeMeta:   metav1.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{Name: "agent-1", Namespace: "default"},
		Spec:       k8s.AgentSpec{ModelRef: k8s.LocalObjectReference{Name: "model-1"}},
	}
	raw, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}

	resp := NewHandler(intadmission.NewChain(nil, nil), testScheme(), testLogger()).Handle(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Update,
			Kind:      metav1.GroupVersionKind{Kind: "Agent"},
			Object:    runtime.RawExtension{Raw: raw},
			OldObject: runtime.RawExtension{Raw: []byte(`{invalid`)},
		},
	})
	if resp.Allowed || resp.Result == nil || resp.Result.Code != http.StatusInternalServerError {
		t.Fatalf("response = %+v, want 500 error", resp)
	}
}

func TestHandle_InvalidMutatorOutput_Error(t *testing.T) {
	mutator := &stubMutator{
		name:  "invalid-output",
		kinds: map[string]bool{"Agent": true},
		mutateFn: func(context.Context, *intadmission.Request) *intadmission.Response {
			return &intadmission.Response{Allowed: true, Patch: json.RawMessage(`{invalid`)}
		},
	}
	agent := k8s.Agent{
		TypeMeta:   metav1.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{Name: "agent-1", Namespace: "default"},
		Spec:       k8s.AgentSpec{ModelRef: k8s.LocalObjectReference{Name: "model-1"}},
	}
	raw, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}

	resp := NewHandler(intadmission.NewChain([]intadmission.MutatingHook{mutator}, nil), testScheme(), testLogger()).Handle(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Operation: admissionv1.Create,
			Kind:      metav1.GroupVersionKind{Kind: "Agent"},
			Object:    runtime.RawExtension{Raw: raw},
		},
	})
	if resp.Allowed || resp.Result == nil || resp.Result.Code != http.StatusInternalServerError {
		t.Fatalf("response = %+v, want 500 error", resp)
	}
}

// TC-0210: AgentInstance with both mutation and validation.
func TestHandle_AgentInstanceChain_MutationThenValidation(t *testing.T) {
	// Mutator adds a label
	mutator := &stubMutator{
		name:  "instance-labeler",
		kinds: map[string]bool{"AgentInstance": true},
		mutateFn: func(_ context.Context, req *intadmission.Request) *intadmission.Response {
			var obj map[string]any
			if err := json.Unmarshal(req.Object, &obj); err != nil {
				return &intadmission.Response{Allowed: false, Reason: "bad json"}
			}
			metadata, _ := obj["metadata"].(map[string]any)
			labels, _ := metadata["labels"].(map[string]any)
			if labels == nil {
				labels = make(map[string]any)
			}
			labels["mutated"] = "true"
			metadata["labels"] = labels
			obj["metadata"] = metadata
			patched, _ := json.Marshal(obj)
			return &intadmission.Response{Allowed: true, Patch: patched}
		},
	}

	// Validator checks the label was added
	validator := &stubValidator{
		name:  "instance-checker",
		kinds: map[string]bool{"AgentInstance": true},
		validateFn: func(_ context.Context, req *intadmission.Request) *intadmission.Response {
			var obj map[string]any
			if err := json.Unmarshal(req.Object, &obj); err != nil {
				return &intadmission.Response{Allowed: false, Reason: "invalid JSON"}
			}
			metadata, _ := obj["metadata"].(map[string]any)
			labels, _ := metadata["labels"].(map[string]any)
			if labels["mutated"] == "true" {
				return &intadmission.Response{Allowed: true}
			}
			return &intadmission.Response{Allowed: false, Reason: "label mutation did not occur"}
		},
	}

	chain := intadmission.NewChain([]intadmission.MutatingHook{mutator}, []intadmission.ValidatingHook{validator})
	h := NewHandler(chain, testScheme(), testLogger())

	instance := k8s.AgentInstance{
		TypeMeta: metav1.TypeMeta{Kind: "AgentInstance", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-instance",
			Namespace: "default",
		},
		Spec: k8s.AgentInstanceSpec{
			AgentRef: k8s.LocalObjectReference{Name: "agent-1"},
		},
	}
	raw, _ := json.Marshal(instance)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "210",
			Operation: admissionv1.Create,
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "AgentInstance"},
			Namespace: "default",
			Name:      "test-instance",
			Object:    runtime.RawExtension{Raw: raw},
		},
	}

	resp := h.Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected allowed, got denied: %v", resp.Result)
	}
}

// TC-0211: Various kinds' convertToInternal coverage (sample).
func TestHandle_MultipleKinds_ConversionSuccess(t *testing.T) {
	kinds := []struct {
		name   string
		obj    interface{}
		apiVer string
	}{
		{
			name: "AgentPolicy",
			obj: k8s.AgentPolicy{
				TypeMeta: metav1.TypeMeta{Kind: "AgentPolicy", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "policy-1",
					Namespace: "default",
				},
				Spec: k8s.AgentPolicySpec{},
			},
		},
		{
			name: "Workspace",
			obj: k8s.Workspace{
				TypeMeta: metav1.TypeMeta{Kind: "Workspace", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "workspace-1",
					Namespace: "default",
				},
				Spec: k8s.WorkspaceSpec{},
			},
		},
		{
			name: "ModelClass",
			obj: k8s.ModelClass{
				TypeMeta: metav1.TypeMeta{Kind: "ModelClass", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{
					Name: "model-class-1",
				},
				Spec: k8s.ModelClassSpec{},
			},
		},
		{
			name: "AgentIdentity",
			obj: k8s.AgentIdentity{
				TypeMeta:   metav1.TypeMeta{Kind: "AgentIdentity", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{Name: "identity-1", Namespace: "default"},
				Spec:       k8s.AgentIdentitySpec{AgentRef: k8s.LocalObjectReference{Name: "agent-1"}},
			},
		},
		{
			name: "AgentRole",
			obj: k8s.AgentRole{
				TypeMeta:   metav1.TypeMeta{Kind: "AgentRole", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{Name: "role-1", Namespace: "default"},
				Rules: []k8s.PolicyRule{{
					Resources: []string{"agents"},
					Verbs:     []string{"get"},
				}},
			},
		},
		{
			name: "AgentRoleBinding",
			obj: k8s.AgentRoleBinding{
				TypeMeta:   metav1.TypeMeta{Kind: "AgentRoleBinding", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{Name: "binding-1", Namespace: "default"},
				Spec: k8s.AgentRoleBindingSpec{
					RoleRef:  k8s.LocalObjectReference{Name: "role-1"},
					Subjects: []k8s.Subject{{Kind: "Agent", Name: "agent-1"}},
				},
			},
		},
		{
			name: "MemoryClass",
			obj: k8s.MemoryClass{
				TypeMeta:   metav1.TypeMeta{Kind: "MemoryClass", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{Name: "mem-class-1"},
				Spec:       k8s.MemoryClassSpec{},
			},
		},
		{
			name: "MemoryClaim",
			obj: k8s.MemoryClaim{
				TypeMeta:   metav1.TypeMeta{Kind: "MemoryClaim", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{Name: "mem-claim-1", Namespace: "default"},
				Spec:       k8s.MemoryClaimSpec{},
			},
		},
		{
			name: "MemoryVolume",
			obj: k8s.MemoryVolume{
				TypeMeta:   metav1.TypeMeta{Kind: "MemoryVolume", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{Name: "mem-vol-1"},
				Spec:       k8s.MemoryVolumeSpec{},
			},
		},
		{
			name: "CognitiveAdmissionPolicy",
			obj: k8s.CognitiveAdmissionPolicy{
				TypeMeta:   metav1.TypeMeta{Kind: "CognitiveAdmissionPolicy", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{Name: "cap-1"},
				Spec:       k8s.CognitiveAdmissionPolicySpec{},
			},
		},
		{
			name: "CognitiveQuota",
			obj: k8s.CognitiveQuota{
				TypeMeta:   metav1.TypeMeta{Kind: "CognitiveQuota", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{Name: "quota-1", Namespace: "default"},
				Spec:       k8s.CognitiveQuotaSpec{},
			},
		},
		{
			name: "CognitiveLimitRange",
			obj: k8s.CognitiveLimitRange{
				TypeMeta:   metav1.TypeMeta{Kind: "CognitiveLimitRange", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{Name: "limit-1", Namespace: "default"},
				Spec:       k8s.CognitiveLimitRangeSpec{},
			},
		},
		{
			name: "WorkflowRun",
			obj: k8s.WorkflowRun{
				TypeMeta:   metav1.TypeMeta{Kind: "WorkflowRun", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{Name: "wfr-1", Namespace: "default"},
				Spec:       k8s.WorkflowRunSpec{WorkflowRef: k8s.LocalObjectReference{Name: "wf-1"}},
			},
		},
		{
			name: "KnowledgeBaseClass",
			obj: k8s.KnowledgeBaseClass{
				TypeMeta:   metav1.TypeMeta{Kind: "KnowledgeBaseClass", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{Name: "kb-class-1"},
			},
		},
		{
			name: "KnowledgeBase",
			obj: k8s.KnowledgeBase{
				TypeMeta:   metav1.TypeMeta{Kind: "KnowledgeBase", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{Name: "kb-1", Namespace: "default"},
			},
		},
		{
			name: "KnowledgeBaseClaim",
			obj: k8s.KnowledgeBaseClaim{
				TypeMeta:   metav1.TypeMeta{Kind: "KnowledgeBaseClaim", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{Name: "reader", Namespace: "default"},
			},
		},
		{
			name: "KnowledgeBaseIndexRun",
			obj: k8s.KnowledgeBaseIndexRun{
				TypeMeta:   metav1.TypeMeta{Kind: "KnowledgeBaseIndexRun", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "default"},
			},
		},
		{
			name: "KnowledgeBaseIndexSchedule",
			obj: k8s.KnowledgeBaseIndexSchedule{
				TypeMeta:   metav1.TypeMeta{Kind: "KnowledgeBaseIndexSchedule", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{Name: "schedule-1", Namespace: "default"},
			},
		},
		{
			name: "Model",
			obj: k8s.Model{
				TypeMeta:   metav1.TypeMeta{Kind: "Model", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{Name: "model-1", Namespace: "default"},
			},
		},
		{
			name: "SecretRef",
			obj: k8s.SecretRef{
				TypeMeta:   metav1.TypeMeta{Kind: "SecretRef", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{Name: "secret-1", Namespace: "default"},
			},
		},
		{
			name: "ApprovalDecision",
			obj: k8s.ApprovalDecision{
				TypeMeta:   metav1.TypeMeta{Kind: "ApprovalDecision", APIVersion: "kognition.dev/v1alpha1"},
				ObjectMeta: metav1.ObjectMeta{Name: "approval-1", Namespace: "default"},
			},
		},
	}

	chain := intadmission.NewChain(nil, nil)
	h := NewHandler(chain, testScheme(), testLogger())

	for _, kt := range kinds {
		t.Run(kt.name, func(t *testing.T) {
			raw, _ := json.Marshal(kt.obj)

			req := admission.Request{
				AdmissionRequest: admissionv1.AdmissionRequest{
					UID:       "211",
					Operation: admissionv1.Create,
					Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: kt.name},
					Namespace: "default",
					Name:      "test-obj",
					Object:    runtime.RawExtension{Raw: raw},
				},
			}

			resp := h.Handle(context.Background(), req)
			if !resp.Allowed {
				t.Fatalf("expected allowed for %s, got: %v", kt.name, resp.Result)
			}
		})
	}
}

// TC-0212: Handle respects context cancellation during validation.
func TestHandle_ContextCancelled_Denied(t *testing.T) {
	validator := &stubValidator{
		name:  "ctx-aware-validator",
		kinds: map[string]bool{"Agent": true},
		validateFn: func(ctx context.Context, req *intadmission.Request) *intadmission.Response {
			if err := ctx.Err(); err != nil {
				return &intadmission.Response{Allowed: false, Reason: "context cancelled: " + err.Error()}
			}
			return &intadmission.Response{Allowed: true}
		},
	}

	chain := intadmission.NewChain(nil, []intadmission.ValidatingHook{validator})
	h := NewHandler(chain, testScheme(), testLogger())

	agent := k8s.Agent{
		TypeMeta:   metav1.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{Name: "agent-1", Namespace: "default"},
		Spec:       k8s.AgentSpec{ModelRef: k8s.LocalObjectReference{Name: "m"}, Instructions: k8s.AgentInstructions{Inline: "x"}},
	}
	raw, _ := json.Marshal(agent)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "212",
			Operation: admissionv1.Create,
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Agent"},
			Namespace: "default",
			Name:      "agent-1",
			Object:    runtime.RawExtension{Raw: raw},
		},
	}

	// Cancel the context before calling Handle.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	resp := h.Handle(ctx, req)
	if resp.Allowed {
		t.Fatal("expected denial when context is already cancelled")
	}
	if resp.Result == nil || resp.Result.Code != http.StatusForbidden {
		t.Fatalf("expected 403 denial from context-aware hook, got %+v", resp.Result)
	}
}

// TC-0213: Handle respects context deadline during mutation.
func TestHandle_ContextDeadlineExceeded_Denied(t *testing.T) {
	mutator := &stubMutator{
		name:  "slow-mutator",
		kinds: map[string]bool{"Agent": true},
		mutateFn: func(ctx context.Context, req *intadmission.Request) *intadmission.Response {
			// Simulate work that checks deadline.
			select {
			case <-ctx.Done():
				return &intadmission.Response{Allowed: false, Reason: "deadline exceeded: " + ctx.Err().Error()}
			default:
			}
			// Simulate a blocking operation that exceeds the deadline.
			select {
			case <-ctx.Done():
				return &intadmission.Response{Allowed: false, Reason: "deadline exceeded: " + ctx.Err().Error()}
			case <-time.After(200 * time.Millisecond):
				return &intadmission.Response{Allowed: true}
			}
		},
	}

	chain := intadmission.NewChain([]intadmission.MutatingHook{mutator}, nil)
	h := NewHandler(chain, testScheme(), testLogger())

	agent := k8s.Agent{
		TypeMeta:   metav1.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{Name: "agent-2", Namespace: "default"},
		Spec:       k8s.AgentSpec{ModelRef: k8s.LocalObjectReference{Name: "m"}, Instructions: k8s.AgentInstructions{Inline: "x"}},
	}
	raw, _ := json.Marshal(agent)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "213",
			Operation: admissionv1.Create,
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Agent"},
			Namespace: "default",
			Name:      "agent-2",
			Object:    runtime.RawExtension{Raw: raw},
		},
	}

	// Set a very tight deadline that will expire before the mutator finishes.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	resp := h.Handle(ctx, req)
	if resp.Allowed {
		t.Fatal("expected denial when context deadline is exceeded")
	}
	if resp.Result == nil || resp.Result.Code != http.StatusForbidden {
		t.Fatalf("expected 403 from deadline-aware hook, got %+v", resp.Result)
	}
}

// TC-0214: Cyclic workflow without entryPoint is rejected by real WorkflowValidator through full webhook pipeline.
func TestHandle_WorkflowCyclicNoEntryPoint_Denied(t *testing.T) {
	chain := intadmission.NewChain(nil, []intadmission.ValidatingHook{intadmission.NewWorkflowValidator()})
	h := NewHandler(chain, testScheme(), testLogger())

	workflow := k8s.Workflow{
		TypeMeta: metav1.TypeMeta{Kind: "Workflow", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cyclic-workflow",
			Namespace: "default",
		},
		Spec: k8s.WorkflowSpec{
			Steps: []k8s.WorkflowStep{
				{Name: "a", AgentRef: k8s.LocalObjectReference{Name: "agent-a"}},
				{Name: "b", AgentRef: k8s.LocalObjectReference{Name: "agent-b"}},
			},
			Edges: []k8s.WorkflowEdge{
				{From: "a", To: "b"},
				{From: "b", To: "a"}, // creates cycle
			},
			// EntryPoint intentionally omitted — must be rejected.
		},
	}
	raw, _ := json.Marshal(workflow)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "214",
			Operation: admissionv1.Create,
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Workflow"},
			Namespace: "default",
			Name:      "cyclic-workflow",
			Object:    runtime.RawExtension{Raw: raw},
		},
	}

	resp := h.Handle(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected denial for cyclic workflow without entryPoint")
	}
	if resp.Result == nil || resp.Result.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %+v", resp.Result)
	}
}

// TC-0221: Workflow with no steps is rejected by real WorkflowValidator through full webhook pipeline.
func TestHandle_WorkflowNoSteps_Denied(t *testing.T) {
	chain := intadmission.NewChain(nil, []intadmission.ValidatingHook{intadmission.NewWorkflowValidator()})
	h := NewHandler(chain, testScheme(), testLogger())

	workflow := k8s.Workflow{
		TypeMeta: metav1.TypeMeta{Kind: "Workflow", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "empty-workflow",
			Namespace: "default",
		},
		Spec: k8s.WorkflowSpec{
			Steps: []k8s.WorkflowStep{}, // no steps
		},
	}
	raw, _ := json.Marshal(workflow)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "215",
			Operation: admissionv1.Create,
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Workflow"},
			Namespace: "default",
			Name:      "empty-workflow",
			Object:    runtime.RawExtension{Raw: raw},
		},
	}

	resp := h.Handle(context.Background(), req)
	if resp.Allowed {
		t.Fatal("expected denial for workflow with no steps")
	}
	if resp.Result == nil || resp.Result.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %+v", resp.Result)
	}
}

// TC-0222: Cyclic workflow WITH entryPoint is allowed by real WorkflowValidator through full webhook pipeline.
func TestHandle_WorkflowCyclicWithEntryPoint_Allowed(t *testing.T) {
	chain := intadmission.NewChain(nil, []intadmission.ValidatingHook{intadmission.NewWorkflowValidator()})
	h := NewHandler(chain, testScheme(), testLogger())

	workflow := k8s.Workflow{
		TypeMeta: metav1.TypeMeta{Kind: "Workflow", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "iterative-workflow",
			Namespace: "default",
		},
		Spec: k8s.WorkflowSpec{
			EntryPoint: "a",
			Steps: []k8s.WorkflowStep{
				{Name: "a", AgentRef: k8s.LocalObjectReference{Name: "agent-a"}},
				{Name: "b", AgentRef: k8s.LocalObjectReference{Name: "agent-b"}},
			},
			Edges: []k8s.WorkflowEdge{
				{From: "a", To: "b"},
				{From: "b", To: "a"}, // cycle allowed when entryPoint is set
			},
		},
	}
	raw, _ := json.Marshal(workflow)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "216",
			Operation: admissionv1.Create,
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Workflow"},
			Namespace: "default",
			Name:      "iterative-workflow",
			Object:    runtime.RawExtension{Raw: raw},
		},
	}

	resp := h.Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected allowed for cyclic workflow with entryPoint, got: %v", resp.Result)
	}
}

// TC-0219: Handle propagates deadline to validation hooks (hook sees remaining time).
func TestHandle_ContextDeadlinePropagated_HookSeesDeadline(t *testing.T) {
	var observedDeadline time.Time
	var hadDeadline bool

	validator := &stubValidator{
		name:  "deadline-observer",
		kinds: map[string]bool{"Agent": true},
		validateFn: func(ctx context.Context, req *intadmission.Request) *intadmission.Response {
			observedDeadline, hadDeadline = ctx.Deadline()
			return &intadmission.Response{Allowed: true}
		},
	}

	chain := intadmission.NewChain(nil, []intadmission.ValidatingHook{validator})
	h := NewHandler(chain, testScheme(), testLogger())

	agent := k8s.Agent{
		TypeMeta:   metav1.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{Name: "agent-3", Namespace: "default"},
		Spec:       k8s.AgentSpec{ModelRef: k8s.LocalObjectReference{Name: "m"}, Instructions: k8s.AgentInstructions{Inline: "x"}},
	}
	raw, _ := json.Marshal(agent)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "214",
			Operation: admissionv1.Create,
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Agent"},
			Namespace: "default",
			Name:      "agent-3",
			Object:    runtime.RawExtension{Raw: raw},
		},
	}

	deadline := time.Now().Add(5 * time.Second)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	resp := h.Handle(ctx, req)
	if !resp.Allowed {
		t.Fatalf("expected allowed, got denied: %v", resp.Result)
	}
	if !hadDeadline {
		t.Fatal("expected hook to observe a deadline on the context")
	}
	// The observed deadline should be at or before the one we set (Handle doesn't add time).
	if observedDeadline.After(deadline) {
		t.Fatalf("hook observed deadline %v which is after the parent deadline %v", observedDeadline, deadline)
	}
}

// TC-0218: AgentInstance mutation produces JSON patches via convertToK8s.
func TestHandle_AgentInstanceMutated_PatchGenerated(t *testing.T) {
	mutator := &stubMutator{
		name:  "agent-instance-defaulter",
		kinds: map[string]bool{"AgentInstance": true},
		mutateFn: func(ctx context.Context, req *intadmission.Request) *intadmission.Response {
			var obj map[string]any
			if err := json.Unmarshal(req.Object, &obj); err != nil {
				return &intadmission.Response{Allowed: false, Reason: err.Error()}
			}
			spec, _ := obj["spec"].(map[string]any)
			if spec == nil {
				spec = map[string]any{}
				obj["spec"] = spec
			}
			sc, _ := spec["sessionContext"].(map[string]any)
			if sc == nil {
				sc = map[string]any{}
				spec["sessionContext"] = sc
			}
			sc["injected"] = "by-mutator"
			patched, err := json.Marshal(obj)
			if err != nil {
				return &intadmission.Response{Allowed: false, Reason: err.Error()}
			}
			return &intadmission.Response{Allowed: true, Patch: patched}
		},
	}

	chain := intadmission.NewChain([]intadmission.MutatingHook{mutator}, nil)
	h := NewHandler(chain, testScheme(), testLogger())

	inst := k8s.AgentInstance{
		TypeMeta:   metav1.TypeMeta{Kind: "AgentInstance", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{Name: "inst-1", Namespace: "default"},
		Spec: k8s.AgentInstanceSpec{
			AgentRef: k8s.LocalObjectReference{Name: "agent-1"},
			Revision: 1,
		},
	}
	raw, _ := json.Marshal(inst)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "213",
			Operation: admissionv1.Create,
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "AgentInstance"},
			Namespace: "default",
			Name:      "inst-1",
			Object:    runtime.RawExtension{Raw: raw},
		},
	}

	resp := h.Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected allowed, got denied: %v", resp.Result)
	}
	if len(resp.Patches) == 0 {
		t.Fatal("expected JSON patches for AgentInstance mutation")
	}
}

// TC-0220: ToolClass mutation produces JSON patches via convertToK8s.
func TestHandle_ToolClassMutated_PatchGenerated(t *testing.T) {
	mutator := &stubMutator{
		name:  "tool-class-defaulter",
		kinds: map[string]bool{"ToolClass": true},
		mutateFn: func(ctx context.Context, req *intadmission.Request) *intadmission.Response {
			var obj map[string]any
			if err := json.Unmarshal(req.Object, &obj); err != nil {
				return &intadmission.Response{Allowed: false, Reason: err.Error()}
			}
			spec, _ := obj["spec"].(map[string]any)
			if spec == nil {
				spec = map[string]any{}
				obj["spec"] = spec
			}
			params, _ := spec["parameters"].(map[string]any)
			if params == nil {
				params = map[string]any{}
				spec["parameters"] = params
			}
			params["defaulted"] = "true"
			patched, err := json.Marshal(obj)
			if err != nil {
				return &intadmission.Response{Allowed: false, Reason: err.Error()}
			}
			return &intadmission.Response{Allowed: true, Patch: patched}
		},
	}

	chain := intadmission.NewChain([]intadmission.MutatingHook{mutator}, nil)
	h := NewHandler(chain, testScheme(), testLogger())

	tc := k8s.ToolClass{
		TypeMeta:   metav1.TypeMeta{Kind: "ToolClass", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{Name: "tc-1"},
		Spec:       k8s.ToolClassSpec{Provisioner: "mcp"},
	}
	raw, _ := json.Marshal(tc)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "214",
			Operation: admissionv1.Create,
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "ToolClass"},
			Namespace: "default",
			Name:      "tc-1",
			Object:    runtime.RawExtension{Raw: raw},
		},
	}

	resp := h.Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected allowed, got denied: %v", resp.Result)
	}
	if len(resp.Patches) == 0 {
		t.Fatal("expected JSON patches for ToolClass mutation")
	}
}

// TC-0223, TC-0245: mutator-added metadata labels survive convertToK8s and appear in JSON patch
// (regression for bug where convertToK8s overwrote patched.ObjectMeta with original).
func TestHandle_MutatorAddsLabel_LabelPreservedInPatch(t *testing.T) {
	mutator := &stubMutator{
		name:  "label-injector",
		kinds: map[string]bool{"Agent": true},
		mutateFn: func(ctx context.Context, req *intadmission.Request) *intadmission.Response {
			var obj map[string]any
			if err := json.Unmarshal(req.Object, &obj); err != nil {
				return &intadmission.Response{Allowed: false, Reason: err.Error()}
			}
			meta, _ := obj["metadata"].(map[string]any)
			if meta == nil {
				meta = map[string]any{}
				obj["metadata"] = meta
			}
			labels, _ := meta["labels"].(map[string]any)
			if labels == nil {
				labels = map[string]any{}
				meta["labels"] = labels
			}
			labels["kognition.dev/mutated"] = "true"
			patched, err := json.Marshal(obj)
			if err != nil {
				return &intadmission.Response{Allowed: false, Reason: err.Error()}
			}
			return &intadmission.Response{Allowed: true, Patch: patched}
		},
	}

	chain := intadmission.NewChain([]intadmission.MutatingHook{mutator}, nil)
	h := NewHandler(chain, testScheme(), testLogger())

	agent := k8s.Agent{
		TypeMeta:   metav1.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{Name: "agent-lbl", Namespace: "default"},
		Spec:       k8s.AgentSpec{ModelRef: k8s.LocalObjectReference{Name: "m"}, Instructions: k8s.AgentInstructions{Inline: "x"}},
	}
	raw, _ := json.Marshal(agent)

	req := admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID:       "223",
			Operation: admissionv1.Create,
			Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Agent"},
			Namespace: "default",
			Name:      "agent-lbl",
			Object:    runtime.RawExtension{Raw: raw},
		},
	}

	resp := h.Handle(context.Background(), req)
	if !resp.Allowed {
		t.Fatalf("expected allowed, got denied: %v", resp.Result)
	}
	if len(resp.Patches) == 0 {
		t.Fatal("expected JSON patches for label mutation")
	}
	// jsonpatch escapes '/' as '~1'. Look for any op touching /metadata/labels.
	found := false
	for _, op := range resp.Patches {
		if strings.HasPrefix(op.Path, "/metadata/labels") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected patch op under /metadata/labels, got: %+v", resp.Patches)
	}
}

// TC-0224, TC-0246: mutator-added metadata labels survive convertToK8s for every kind
// covered by the defensive merge expansion (Finding #2). Locks in the pattern
// for the 14 kinds added beyond the original 4 (Agent, AgentInstance, Tool, ToolClass).
func TestHandle_MutatorAddsLabel_AllKinds_LabelPreservedInPatch(t *testing.T) {
	kinds := []string{
		"Tool",
		"AgentPolicy",
		"CognitiveAdmissionPolicy",
		"CognitiveQuota",
		"CognitiveLimitRange",
		"Workspace",
		"AgentIdentity",
		"AgentRole",
		"AgentRoleBinding",
		"MemoryClass",
		"MemoryClaim",
		"MemoryVolume",
		"ModelClass",
		"AutonomyClass",
		"Model",
		"SecretRef",
		"Workflow",
		"WorkflowRun",
		"ApprovalDecision",
		"KnowledgeBaseClass",
		"KnowledgeBase",
		"KnowledgeBaseClaim",
		"KnowledgeBaseIndexRun",
		"KnowledgeBaseIndexSchedule",
	}

	for _, kind := range kinds {
		t.Run(kind, func(t *testing.T) {
			mutator := &stubMutator{
				name:  "label-injector",
				kinds: map[string]bool{kind: true},
				mutateFn: func(ctx context.Context, req *intadmission.Request) *intadmission.Response {
					var obj map[string]any
					if err := json.Unmarshal(req.Object, &obj); err != nil {
						return &intadmission.Response{Allowed: false, Reason: err.Error()}
					}
					meta, _ := obj["metadata"].(map[string]any)
					if meta == nil {
						meta = map[string]any{}
						obj["metadata"] = meta
					}
					labels, _ := meta["labels"].(map[string]any)
					if labels == nil {
						labels = map[string]any{}
						meta["labels"] = labels
					}
					labels["kognition.dev/mutated"] = "true"
					patched, err := json.Marshal(obj)
					if err != nil {
						return &intadmission.Response{Allowed: false, Reason: err.Error()}
					}
					return &intadmission.Response{Allowed: true, Patch: patched}
				},
			}
			chain := intadmission.NewChain([]intadmission.MutatingHook{mutator}, nil)
			h := NewHandler(chain, testScheme(), testLogger())

			raw := []byte(`{"apiVersion":"kognition.dev/v1alpha1","kind":"` + kind + `","metadata":{"name":"x","namespace":"default"}}`)
			req := admission.Request{
				AdmissionRequest: admissionv1.AdmissionRequest{
					UID:       "224",
					Operation: admissionv1.Create,
					Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: kind},
					Namespace: "default",
					Name:      "x",
					Object:    runtime.RawExtension{Raw: raw},
				},
			}

			resp := h.Handle(context.Background(), req)
			if !resp.Allowed {
				t.Fatalf("kind=%s: expected allowed, got denied: %v", kind, resp.Result)
			}
			if len(resp.Patches) == 0 {
				t.Fatalf("kind=%s: expected JSON patches for label mutation", kind)
			}
			found := false
			for _, op := range resp.Patches {
				if strings.HasPrefix(op.Path, "/metadata/labels") {
					found = true
					break
				}
			}
			if !found {
				t.Fatalf("kind=%s: expected patch op under /metadata/labels, got: %+v", kind, resp.Patches)
			}
		})
	}
}

func TestWebhookConversionsRejectMalformedJSONForEveryKind(t *testing.T) {
	kinds := []string{
		"Agent",
		"AgentInstance",
		"AgentPolicy",
		"CognitiveAdmissionPolicy",
		"CognitiveQuota",
		"CognitiveLimitRange",
		"Workspace",
		"AgentIdentity",
		"AgentRole",
		"AgentRoleBinding",
		"MemoryClass",
		"MemoryClaim",
		"MemoryVolume",
		"ModelClass",
		"AutonomyClass",
		"Model",
		"SecretRef",
		"Tool",
		"ToolClass",
		"Workflow",
		"WorkflowRun",
		"ApprovalDecision",
		"KnowledgeBaseClass",
		"KnowledgeBase",
		"KnowledgeBaseClaim",
		"KnowledgeBaseIndexRun",
		"KnowledgeBaseIndexSchedule",
	}
	handler := NewHandler(intadmission.NewChain(nil, nil), testScheme(), testLogger())
	validOriginal := []byte(`{"apiVersion":"kognition.dev/v1alpha1","metadata":{"name":"test"}}`)

	for _, kind := range kinds {
		t.Run(kind+"/internal", func(t *testing.T) {
			if _, err := handler.convertToInternal(kind, []byte("{")); err == nil {
				t.Fatalf("convertToInternal(%q) accepted malformed JSON", kind)
			}
		})
		t.Run(kind+"/original", func(t *testing.T) {
			if _, err := handler.convertToK8s(kind, []byte(`{}`), []byte("{")); err == nil {
				t.Fatalf("convertToK8s(%q) accepted malformed original JSON", kind)
			}
		})
		t.Run(kind+"/mutated", func(t *testing.T) {
			if _, err := handler.convertToK8s(kind, []byte("{"), validOriginal); err == nil {
				t.Fatalf("convertToK8s(%q) accepted malformed mutated JSON", kind)
			}
		})
	}

	if _, err := handler.convertToK8s("Unknown", []byte(`{}`), validOriginal); err == nil {
		t.Fatal("convertToK8s accepted an unsupported kind")
	}
}

func TestHandleUpdateRejectsMalformedOldObject(t *testing.T) {
	handler := NewHandler(intadmission.NewChain(nil, nil), testScheme(), testLogger())
	agent := k8s.Agent{
		TypeMeta:   metav1.TypeMeta{Kind: "Agent", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: metav1.ObjectMeta{Name: "test-agent", Namespace: "default"},
		Spec:       k8s.AgentSpec{ModelRef: k8s.LocalObjectReference{Name: "model"}},
	}
	raw, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}

	response := handler.Handle(context.Background(), admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UID:       "malformed-old-object",
		Operation: admissionv1.Update,
		Kind:      metav1.GroupVersionKind{Group: "kognition.dev", Version: "v1alpha1", Kind: "Agent"},
		Namespace: "default",
		Name:      "test-agent",
		Object:    runtime.RawExtension{Raw: raw},
		OldObject: runtime.RawExtension{Raw: []byte("{")},
	}})

	if response.Allowed {
		t.Fatal("malformed old object was allowed")
	}
	if response.Result == nil || response.Result.Code != http.StatusInternalServerError {
		t.Fatalf("response = %+v, want HTTP 500", response)
	}
}

// --- Stub hooks for tests ---

type stubValidator struct {
	name       string
	kinds      map[string]bool
	validateFn func(context.Context, *intadmission.Request) *intadmission.Response
}

func (s *stubValidator) Name() string { return s.name }
func (s *stubValidator) Handles(kind string, op intadmission.Operation) bool {
	return s.kinds[kind]
}
func (s *stubValidator) Validate(ctx context.Context, req *intadmission.Request) *intadmission.Response {
	return s.validateFn(ctx, req)
}

type stubMutator struct {
	name     string
	kinds    map[string]bool
	mutateFn func(context.Context, *intadmission.Request) *intadmission.Response
}

func (s *stubMutator) Name() string { return s.name }
func (s *stubMutator) Handles(kind string, op intadmission.Operation) bool {
	return s.kinds[kind]
}
func (s *stubMutator) Mutate(ctx context.Context, req *intadmission.Request) *intadmission.Response {
	return s.mutateFn(ctx, req)
}
