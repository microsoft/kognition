package runtimeapproval

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
	authenticationv1 "k8s.io/api/authentication/v1"
)

func TestNativeWorkflowSubmissionReceiptPreservesExactAuthenticatedEvidence(t *testing.T) {
	action, keys, now := executorActionFixture(t)
	receipt := NativeWorkflowSubmissionReceipt{AdmissionUID: "admission-request-uid", Run: api.RuntimeApprovalReference{Kind: "WorkflowRun", Scope: "Namespaced", Namespace: action.Workflow.Namespace, Name: "investigation", UID: "persisted-resource-uid"},
		Workflow: action.Workflow, ExecutionBinding: action.ExecutionBinding, Executor: action.Executor, WorkflowRevisionDigest: action.WorkflowRevisionDigest,
		RequestDigest: strings.Repeat("a", 64), IssuedAt: now, ExpiresAt: now.Add(NativeWorkflowSubmissionTTL),
		Submitter: authenticationv1.UserInfo{Username: "actual-submitter", UID: "actual-submitter-uid", Groups: []string{"workflow-users"}, Extra: map[string]authenticationv1.ExtraValue{"authentication.kubernetes.io/pod-uid": {"submitter-pod-uid"}}}}
	sealed, err := SealNativeWorkflowSubmissionReceipt(context.Background(), keys, receipt, now)
	if err != nil {
		t.Fatal(err)
	}
	if sealed.Run.UID != "persisted-resource-uid" || sealed.AdmissionUID != "admission-request-uid" || sealed.Submitter.UID != "actual-submitter-uid" || sealed.Binding.Domain == WorkflowExecutorInitiationDomain {
		t.Fatalf("submission replaced actual evidence or reused initiation domain: %+v", sealed)
	}
	if err := VerifyNativeWorkflowSubmissionReceipt(context.Background(), keys, sealed, now.Add(30*time.Second)); err != nil {
		t.Fatalf("healthy receipt failed verification: %v", err)
	}
	for name, change := range map[string]func(*NativeWorkflowSubmissionReceipt){
		"recreated run":              func(receipt *NativeWorkflowSubmissionReceipt) { receipt.Run.UID = "recreated-resource-uid" },
		"copied name":                func(receipt *NativeWorkflowSubmissionReceipt) { receipt.Run.Name = "copied-run" },
		"admission UID substitution": func(receipt *NativeWorkflowSubmissionReceipt) { receipt.Run.UID = receipt.AdmissionUID },
		"source UID":                 func(receipt *NativeWorkflowSubmissionReceipt) { receipt.Submitter.UID = "forged-source-uid" },
		"source groups":              func(receipt *NativeWorkflowSubmissionReceipt) { receipt.Submitter.Groups = []string{"administrators"} },
		"source extras": func(receipt *NativeWorkflowSubmissionReceipt) {
			receipt.Submitter.Extra = map[string]authenticationv1.ExtraValue{"authentication.kubernetes.io/pod-uid": {"other-pod-uid"}}
		},
		"workflow revision": func(receipt *NativeWorkflowSubmissionReceipt) { receipt.Workflow.ResourceVersion++ },
		"binding revision":  func(receipt *NativeWorkflowSubmissionReceipt) { receipt.ExecutionBinding.ResourceVersion++ },
		"payload":           func(receipt *NativeWorkflowSubmissionReceipt) { receipt.RequestDigest = strings.Repeat("b", 64) },
		"executor":          func(receipt *NativeWorkflowSubmissionReceipt) { receipt.Executor.Subject = "other-executor" },
		"initiation domain": func(receipt *NativeWorkflowSubmissionReceipt) {
			receipt.Binding.Domain = WorkflowExecutorInitiationDomain
		},
		"missing resource UID": func(receipt *NativeWorkflowSubmissionReceipt) { receipt.Run.UID = "" },
		"unbounded lifetime":   func(receipt *NativeWorkflowSubmissionReceipt) { receipt.ExpiresAt = now.Add(2 * time.Minute) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := sealed
			changed.Submitter = *sealed.Submitter.DeepCopy()
			change(&changed)
			if err := VerifyNativeWorkflowSubmissionReceipt(context.Background(), keys, changed, now); err == nil {
				t.Fatal("edited or copied evidence passed verification")
			}
		})
	}
	if err := VerifyNativeWorkflowSubmissionReceipt(context.Background(), keys, sealed, sealed.ExpiresAt); err == nil {
		t.Fatal("receipt remained valid at expiry")
	}
	receipt.Submitter.Groups[0] = "changed-after-seal"
	if err := VerifyNativeWorkflowSubmissionReceipt(context.Background(), keys, sealed, now); err != nil {
		t.Fatalf("caller mutation modified sealed evidence: %v", err)
	}
}

func executorActionFixture(t *testing.T) (WorkflowExecutorAction, *StaticBindingKeyStore, time.Time) {
	t.Helper()
	keys, err := NewStaticBindingKeyStore([]BindingKey{{Version: 1, Algorithm: BindingAlgorithmV1, Domain: BindingDomainV1, Status: BindingKeyActive, Material: []byte("0123456789abcdef0123456789abcdef")}})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	ref := func(kind, namespace, name, uid string) api.RuntimeApprovalReference {
		return api.RuntimeApprovalReference{Kind: kind, Scope: "Namespaced", Namespace: namespace, Name: name, UID: uid, ResourceVersion: 1}
	}
	return WorkflowExecutorAction{
		Executor:  api.ControlPlaneRoleSubject{Method: "mtls", Issuer: "executor-ca", Subject: "executor"},
		Initiator: api.ControlPlaneRoleSubject{Method: "oidc", Issuer: "https://issuer.example", Subject: "initiator"},
		Workflow:  ref("Workflow", "planning", "investigate", "workflow-uid"), WorkflowRevisionDigest: strings.Repeat("a", 64),
		ExecutionBinding: ref("WorkflowExecutionBinding", "planning", "approved", "binding-uid"),
		Target:           ref("Agent", "workers", "analyst", "agent-uid"), Service: ref("AgentService", "workers", "analyst", "service-uid"),
		Activation: store.WorkflowActivationKey{Workspace: "planning", RunUID: "run-uid", Step: "team", ActivationID: "visit-1", Attempt: 1},
		OwnerEpoch: 1, BindingDigest: strings.Repeat("a", 64), InvocationID: "initiate-1", Operation: WorkflowExecutorInitiate, ExpiresAt: now.Add(time.Hour), Payload: json.RawMessage(`{"goal":"bounded analysis","input":{"a":1,"b":2}}`),
	}, keys, now
}

func TestWorkflowExecutorPrincipalConfigurationFailsClosed(t *testing.T) {
	if principals, err := WorkflowExecutorPrincipalsFromJSON(""); err != nil || len(principals) != 0 {
		t.Fatalf("unconfigured execution: %+v %v", principals, err)
	}
	valid := `[{"method":"oidc","issuer":"https://issuer.example","subject":"executor"}]`
	if principals, err := WorkflowExecutorPrincipalsFromJSON(valid); err != nil || len(principals) != 1 || principals[0].Subject != "executor" {
		t.Fatalf("explicit principal: %+v %v", principals, err)
	}
	for _, input := range []string{`null`, `[]`, `[{"method":"token","issuer":"shared","subject":"executor"}]`, `[{"method":"oidc","method":"mtls","issuer":"issuer","subject":"executor"}]`, `[{"method":"oidc","issuer":"issuer","subject":"executor","groups":["admins"]}]`, `[{"method":"oidc","issuer":"issuer","subject":"executor"},{"method":"oidc","issuer":"issuer","subject":"executor"}]`} {
		if _, err := WorkflowExecutorPrincipalsFromJSON(input); err == nil {
			t.Fatalf("untrusted principal configuration accepted: %s", input)
		}
	}
}

func TestWorkflowExecutorBindingExactActionAndLegacyIsolation(t *testing.T) {
	action, keys, now := executorActionFixture(t)
	binding, err := BindWorkflowExecutorAction(context.Background(), keys, action, now)
	if err != nil {
		t.Fatal(err)
	}
	if binding.Format != WorkflowExecutorBindingFormat || binding.Domain != WorkflowExecutorBindingDomain {
		t.Fatalf("executor binding used legacy format or domain: %+v", binding)
	}
	if err := VerifyWorkflowExecutorAction(context.Background(), keys, action, binding); err != nil {
		t.Fatal(err)
	}
	changes := map[string]func(*WorkflowExecutorAction){
		"executor method":   func(action *WorkflowExecutorAction) { action.Executor.Method = "oidc" },
		"executor issuer":   func(action *WorkflowExecutorAction) { action.Executor.Issuer = "other-ca" },
		"executor subject":  func(action *WorkflowExecutorAction) { action.Executor.Subject = "other-executor" },
		"initiator method":  func(action *WorkflowExecutorAction) { action.Initiator.Method = "mtls" },
		"initiator issuer":  func(action *WorkflowExecutorAction) { action.Initiator.Issuer = "other-ca" },
		"initiator subject": func(action *WorkflowExecutorAction) { action.Initiator.Subject = "other-initiator" },
		"workflow name":     func(action *WorkflowExecutorAction) { action.Workflow.Name = "replacement" },
		"workflow uid":      func(action *WorkflowExecutorAction) { action.Workflow.UID = "replacement-uid" },
		"workflow version":  func(action *WorkflowExecutorAction) { action.Workflow.ResourceVersion++ },
		"workflow revision": func(action *WorkflowExecutorAction) { action.WorkflowRevisionDigest = strings.Repeat("b", 64) },
		"authority uid":     func(action *WorkflowExecutorAction) { action.ExecutionBinding.UID = "other-binding" },
		"authority version": func(action *WorkflowExecutorAction) { action.ExecutionBinding.ResourceVersion++ },
		"target uid":        func(action *WorkflowExecutorAction) { action.Target.UID = "replacement-agent" },
		"target version":    func(action *WorkflowExecutorAction) { action.Target.ResourceVersion++ },
		"service uid":       func(action *WorkflowExecutorAction) { action.Service.UID = "replacement-service" },
		"service version":   func(action *WorkflowExecutorAction) { action.Service.ResourceVersion++ },
		"target namespace": func(action *WorkflowExecutorAction) {
			action.Target.Namespace = "foreign"
			action.Service.Namespace = "foreign"
		},
		"run uid":    func(action *WorkflowExecutorAction) { action.Activation.RunUID = "other-run" },
		"step":       func(action *WorkflowExecutorAction) { action.Activation.Step = "other-step" },
		"activation": func(action *WorkflowExecutorAction) { action.Activation.ActivationID = "visit-2" },
		"attempt":    func(action *WorkflowExecutorAction) { action.Activation.Attempt++ },
		"epoch":      func(action *WorkflowExecutorAction) { action.OwnerEpoch++ },
		"invocation": func(action *WorkflowExecutorAction) { action.InvocationID = "initiate-2" },
		"expiry":     func(action *WorkflowExecutorAction) { action.ExpiresAt = action.ExpiresAt.Add(time.Minute) },
		"payload":    func(action *WorkflowExecutorAction) { action.Payload = json.RawMessage(`{"goal":"expanded analysis"}`) },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			changed := action
			change(&changed)
			if err := VerifyWorkflowExecutorAction(context.Background(), keys, changed, binding); err == nil {
				t.Fatalf("changed %s authenticated as the original executor action", name)
			}
		})
	}
	legacyAction := bindingAction(`{"a":1}`)
	legacyBinding, err := Bind(context.Background(), keys, legacyAction)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(context.Background(), keys, legacyAction, binding); err == nil {
		t.Fatal("executor binding entered legacy Agent action path")
	}
	if err := VerifyWorkflowExecutorAction(context.Background(), keys, action, legacyBinding); err == nil {
		t.Fatal("Agent binding entered executor action path")
	}
	if err := AuthenticateNewBinding(context.Background(), keys, approvalFromAction(legacyAction, binding), action.Payload); err == nil {
		t.Fatal("legacy Agent approval accepted an executor binding")
	}
	equivalent := action
	equivalent.Payload = json.RawMessage(` { "input": {"b":2,"a":1}, "goal":"bounded analysis" } `)
	equivalent.ExpiresAt = action.ExpiresAt.In(time.FixedZone("offset", 3600))
	if err := VerifyWorkflowExecutorAction(context.Background(), keys, equivalent, binding); err != nil {
		t.Fatalf("equivalent structured action did not verify: %v", err)
	}
	if err := ValidateWorkflowExecutorAction(action, action.ExpiresAt); err == nil {
		t.Fatal("expired action accepted for new execution")
	}
	if err := VerifyWorkflowExecutorAction(context.Background(), keys, action, binding); err != nil {
		t.Fatalf("retained binding could not authenticate evidence after execution expiry: %v", err)
	}
}

func TestWorkflowExecutorApprovalTypedActorAndBinding(t *testing.T) {
	action, keys, now := executorActionFixture(t)
	approval, err := BuildWorkflowExecutorApproval(context.Background(), keys, action, api.RuntimeApprovalDecisionPending, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateCreate(approval, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := NormalizeAndValidateReferences(&approval); err != nil {
		t.Fatal(err)
	}
	if err := AuthenticateNewBinding(context.Background(), keys, approval, action.Payload); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(approval.Spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{`"agentRef"`, `"agentIdentityRef"`, `"agentInstanceRef"`, `"autonomyClassRef"`, `"sessionRef"`, `"payload"`} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("executor record contains fabricated Agent fields or payload: %s", encoded)
		}
	}
	for name, change := range map[string]func(*api.RuntimeApproval){
		"actor":         func(approval *api.RuntimeApproval) { approval.Spec.ActorKind = "" },
		"mixed Agent":   func(approval *api.RuntimeApproval) { approval.Spec.AgentRef = action.Target },
		"mixed session": func(approval *api.RuntimeApproval) { approval.Spec.SessionRef = "fabricated" },
		"plane":         func(approval *api.RuntimeApproval) { approval.Spec.Plane = api.RuntimeApprovalPlaneCommunication },
		"target":        func(approval *api.RuntimeApproval) { approval.Spec.TargetRef.UID = "replacement" },
		"changed context": func(approval *api.RuntimeApproval) {
			context := *approval.Spec.WorkflowContext
			context.OwnerEpoch++
			approval.Spec.WorkflowContext = &context
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := approval
			change(&changed)
			if err := AuthenticateNewBinding(context.Background(), keys, changed, action.Payload); err == nil {
				t.Fatalf("changed %s retained executor authority", name)
			}
		})
	}
	deadline, err := RuntimeApprovalDeadline(approval, now, 2*time.Hour)
	if err != nil || !deadline.Equal(action.ExpiresAt) {
		t.Fatalf("approval enlarged authority expiry: %v %v", deadline, err)
	}
	if _, err := RuntimeApprovalDeadline(approval, action.ExpiresAt, time.Minute); err == nil {
		t.Fatal("expired executor authority renewed")
	}
}

func TestWorkflowExecutorBindingRejectsInvalidScopeAndCredentials(t *testing.T) {
	action, keys, now := executorActionFixture(t)
	for name, change := range map[string]func(*WorkflowExecutorAction){
		"anonymous executor":      func(action *WorkflowExecutorAction) { action.Executor.Method = "anonymous" },
		"token executor":          func(action *WorkflowExecutorAction) { action.Executor.Method = "token" },
		"anonymous initiator":     func(action *WorkflowExecutorAction) { action.Initiator.Method = "anonymous" },
		"missing issuer":          func(action *WorkflowExecutorAction) { action.Executor.Issuer = "" },
		"missing subject":         func(action *WorkflowExecutorAction) { action.Initiator.Subject = "" },
		"principal controls":      func(action *WorkflowExecutorAction) { action.Executor.Subject = "executor\x00" },
		"principal invalid UTF-8": func(action *WorkflowExecutorAction) { action.Executor.Subject = "executor\xff" },
		"principal whitespace":    func(action *WorkflowExecutorAction) { action.Initiator.Subject = " initiator" },
		"principal bound":         func(action *WorkflowExecutorAction) { action.Executor.Subject = strings.Repeat("a", 513) },
		"operation":               func(action *WorkflowExecutorAction) { action.Operation = "delegate" },
		"no expiry":               func(action *WorkflowExecutorAction) { action.ExpiresAt = time.Time{} },
		"expired":                 func(action *WorkflowExecutorAction) { action.ExpiresAt = now },
		"unbounded expiry":        func(action *WorkflowExecutorAction) { action.ExpiresAt = now.Add(24*time.Hour + time.Second) },
		"run namespace":           func(action *WorkflowExecutorAction) { action.Activation.Workspace = "foreign" },
		"missing run uid":         func(action *WorkflowExecutorAction) { action.Activation.RunUID = "" },
		"attempt":                 func(action *WorkflowExecutorAction) { action.Activation.Attempt = 0 },
		"owner epoch":             func(action *WorkflowExecutorAction) { action.OwnerEpoch = 0 },
		"missing invocation":      func(action *WorkflowExecutorAction) { action.InvocationID = "" },
		"invocation bound":        func(action *WorkflowExecutorAction) { action.InvocationID = strings.Repeat("a", 129) },
		"missing revision":        func(action *WorkflowExecutorAction) { action.WorkflowRevisionDigest = "" },
		"noncanonical revision":   func(action *WorkflowExecutorAction) { action.WorkflowRevisionDigest = strings.Repeat("A", 64) },
		"unqualified target":      func(action *WorkflowExecutorAction) { action.Target.Namespace = "" },
		"service namespace":       func(action *WorkflowExecutorAction) { action.Service.Namespace = "foreign" },
		"missing target uid":      func(action *WorkflowExecutorAction) { action.Target.UID = "" },
		"target invalid UTF-8":    func(action *WorkflowExecutorAction) { action.Target.UID = "agent\xff" },
		"target wildcard":         func(action *WorkflowExecutorAction) { action.Target.Name = "*" },
		"target kind":             func(action *WorkflowExecutorAction) { action.Target.Kind = "Tool" },
		"target version":          func(action *WorkflowExecutorAction) { action.Target.ResourceVersion = 0 },
		"authority kind":          func(action *WorkflowExecutorAction) { action.ExecutionBinding.Kind = "ServiceAccount" },
		"cluster authority":       func(action *WorkflowExecutorAction) { action.ExecutionBinding.Scope = "Cluster" },
		"configuration digest":    func(action *WorkflowExecutorAction) { action.Workflow.ConfigurationDigest = strings.Repeat("a", 64) },
		"no payload":              func(action *WorkflowExecutorAction) { action.Payload = nil },
		"duplicate keys": func(action *WorkflowExecutorAction) {
			action.Payload = json.RawMessage(`{"goal":"safe","goal":"expanded"}`)
		},
		"trailing payload": func(action *WorkflowExecutorAction) { action.Payload = json.RawMessage(`{} {}`) },
		"payload bound": func(action *WorkflowExecutorAction) {
			action.Payload = json.RawMessage(strings.Repeat(" ", maxExecutorPayloadBytes+1))
		},
	} {
		t.Run(name, func(t *testing.T) {
			changed := action
			change(&changed)
			if _, err := BindWorkflowExecutorAction(context.Background(), keys, changed, now); err == nil {
				t.Fatalf("invalid %s received an executor binding", name)
			}
		})
	}
	if _, err := BindWorkflowExecutorAction(context.Background(), nil, action, now); !errors.Is(err, ErrBindingKeyUnavailable) {
		t.Fatalf("missing keys = %v", err)
	}
	if _, err := BindWorkflowExecutorAction(context.Background(), keys, action, time.Time{}); err == nil {
		t.Fatal("unhealthy clock accepted for new binding")
	}
}

func TestWorkflowExecutorBindingRotationAndForgery(t *testing.T) {
	action, keys, now := executorActionFixture(t)
	binding, err := BindWorkflowExecutorAction(context.Background(), keys, action, now)
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*api.RuntimeActionBinding){
		"format":      func(binding *api.RuntimeActionBinding) { binding.Format = BindingFormatV1 },
		"domain":      func(binding *api.RuntimeActionBinding) { binding.Domain = BindingDomainV1 },
		"algorithm":   func(binding *api.RuntimeActionBinding) { binding.Algorithm = "none" },
		"epoch":       func(binding *api.RuntimeActionBinding) { binding.Epoch++ },
		"version":     func(binding *api.RuntimeActionBinding) { binding.KeyVersion = 2 },
		"tag":         func(binding *api.RuntimeActionBinding) { binding.Tag = strings.Repeat("A", 43) },
		"invalid tag": func(binding *api.RuntimeActionBinding) { binding.Tag = "invalid" },
	} {
		t.Run(name, func(t *testing.T) {
			forged := binding
			change(&forged)
			if err := VerifyWorkflowExecutorAction(context.Background(), keys, action, forged); err == nil {
				t.Fatalf("forged %s verified", name)
			}
		})
	}
	rotated, err := NewStaticBindingKeyStoreAtEpoch(2, []BindingKey{
		{Version: 1, Epoch: 1, Algorithm: BindingAlgorithmV1, Domain: BindingDomainV1, Status: BindingKeyVerifyOnly, Material: []byte("0123456789abcdef0123456789abcdef")},
		{Version: 2, Epoch: 2, Algorithm: BindingAlgorithmV1, Domain: BindingDomainV1, Status: BindingKeyActive, Material: []byte("abcdef0123456789abcdef0123456789")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyWorkflowExecutorAction(context.Background(), rotated, action, binding); err != nil {
		t.Fatalf("retained executor evidence failed after rotation: %v", err)
	}
	newBinding, err := BindWorkflowExecutorAction(context.Background(), rotated, action, now)
	if err != nil || newBinding.KeyVersion != 2 || newBinding.Epoch != 2 {
		t.Fatalf("new action did not use current key: %+v %v", newBinding, err)
	}
}
