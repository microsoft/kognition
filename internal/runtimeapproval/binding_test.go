// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeapproval

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

func TestBindingCanonicalizationReplayAndRotation(t *testing.T) {
	keys, err := NewStaticBindingKeyStore([]BindingKey{
		{Version: 1, Algorithm: BindingAlgorithmV1, Domain: BindingDomainV1, Status: BindingKeyVerifyOnly, Material: []byte("0123456789abcdef0123456789abcdef")},
		{Version: 2, Algorithm: BindingAlgorithmV1, Domain: BindingDomainV1, Status: BindingKeyActive, Material: []byte("abcdef0123456789abcdef0123456789")},
	})
	if err != nil {
		t.Fatal(err)
	}
	action := bindingAction(`{"z":2,"a":{"y":true,"x":1}}`)
	binding, err := Bind(context.Background(), keys, action)
	if err != nil || binding.KeyVersion != 2 {
		t.Fatalf("Bind() = %#v, %v", binding, err)
	}
	const knownTag = "vE-x2FsQ_KO-dVv-3R3Y3azm6W1TqIhNbwGSa0JbgYU"
	if binding.Tag != knownTag {
		t.Fatalf("known binding vector = %q, want %q", binding.Tag, knownTag)
	}
	equivalent := bindingAction(" { \"a\" : { \"x\" : 1, \"y\" : true }, \"z\" : 2 } ")
	if err := Verify(context.Background(), keys, equivalent, binding); err != nil {
		t.Fatalf("semantically equivalent JSON did not verify: %v", err)
	}
	changes := map[string]func(*ActionEnvelope){
		"action":    func(changed *ActionEnvelope) { changed.Operation = "delete" },
		"reference": func(changed *ActionEnvelope) { changed.TargetRef.ResourceVersion++ },
		"payload":   func(changed *ActionEnvelope) { changed.Payload = json.RawMessage(`{"a":{"x":2,"y":true},"z":2}`) },
	}
	for name, change := range changes {
		t.Run("changed_"+name, func(t *testing.T) {
			changed := equivalent
			change(&changed)
			if err := Verify(context.Background(), keys, changed, binding); err == nil {
				t.Fatalf("changed %s verified", name)
			}
		})
	}

	ordered := bindingAction(`{"a":1}`)
	ordered.AuthorizationRefs = append(ordered.AuthorizationRefs, api.RuntimeApprovalReference{Kind: "ToolGrant", Scope: "Namespaced", Namespace: "team-a", Name: "grant-b", UID: "grant-b-uid", ResourceVersion: 2})
	reversed := ordered
	reversed.AuthorizationRefs = append([]api.RuntimeApprovalReference(nil), ordered.AuthorizationRefs...)
	reversed.AuthorizationRefs[0], reversed.AuthorizationRefs[1] = reversed.AuthorizationRefs[1], reversed.AuthorizationRefs[0]
	orderedBinding, err := Bind(context.Background(), keys, ordered)
	if err != nil {
		t.Fatal(err)
	}
	reversedBinding, err := Bind(context.Background(), keys, reversed)
	if err != nil {
		t.Fatal(err)
	}
	if orderedBinding != reversedBinding {
		t.Fatalf("authorization ordering changed binding: %#v != %#v", orderedBinding, reversedBinding)
	}
	binding.KeyVersion = 99
	if err := Verify(context.Background(), keys, action, binding); !errors.Is(err, ErrBindingKeyUnavailable) {
		t.Fatalf("missing key error = %v", err)
	}
}

func TestBindingWorkflowContextIsExactAndOptional(t *testing.T) {
	_, keys, _ := executorActionFixture(t)
	action := bindingAction(`{"a":1}`)
	action.WorkflowContext = &api.WorkflowActionContext{Workspace: "origin", RunUID: "run-uid", Step: "team",
		ActivationID: "visit-1", Attempt: 1, OwnerEpoch: 1, BindingDigest: strings.Repeat("a", 64)}
	binding, err := Bind(context.Background(), keys, action)
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*api.WorkflowActionContext){
		"workspace":  func(context *api.WorkflowActionContext) { context.Workspace = "foreign" },
		"run":        func(context *api.WorkflowActionContext) { context.RunUID = "other-run" },
		"step":       func(context *api.WorkflowActionContext) { context.Step = "other-step" },
		"activation": func(context *api.WorkflowActionContext) { context.ActivationID = "visit-2" },
		"attempt":    func(context *api.WorkflowActionContext) { context.Attempt++ },
		"epoch":      func(context *api.WorkflowActionContext) { context.OwnerEpoch++ },
		"binding":    func(context *api.WorkflowActionContext) { context.BindingDigest = strings.Repeat("b", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := action
			changed.WorkflowContext = cloneWorkflowActionContext(action.WorkflowContext)
			change(changed.WorkflowContext)
			if err := Verify(context.Background(), keys, changed, binding); err == nil {
				t.Fatalf("changed Workflow %s retained execution binding", name)
			}
		})
	}
	stripped := action
	stripped.WorkflowContext = nil
	if err := Verify(context.Background(), keys, stripped, binding); err == nil {
		t.Fatal("removing collaboration context retained action authority")
	}
	for _, invalid := range []api.WorkflowActionContext{
		{Workspace: "origin", RunUID: "run-uid", Step: "team", ActivationID: "visit-1", Attempt: 1, OwnerEpoch: 0, BindingDigest: strings.Repeat("a", 64)},
		{Workspace: "*", RunUID: "run-uid", Step: "team", ActivationID: "visit-1", Attempt: 1, OwnerEpoch: 1, BindingDigest: strings.Repeat("a", 64)},
		{Workspace: "origin", RunUID: "run-uid", Step: "team", ActivationID: "visit-1", Attempt: 1, OwnerEpoch: 1, BindingDigest: "*"},
	} {
		changed := action
		changed.WorkflowContext = &invalid
		if _, err := Bind(context.Background(), keys, changed); err == nil {
			t.Fatal("invalid Workflow context received an action binding")
		}
	}
}

func TestBindingKeyBoundaryRejectsMissingStaleRetiredAndInvalidMetadata(t *testing.T) {
	active, err := NewStaticBindingKeyStore([]BindingKey{{Version: 2, Algorithm: BindingAlgorithmV1, Domain: BindingDomainV1, Status: BindingKeyActive, Material: []byte("abcdef0123456789abcdef0123456789")}})
	if err != nil {
		t.Fatal(err)
	}
	action := bindingAction(`{"a":1}`)
	binding, err := Bind(context.Background(), active, action)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateNewBindingKey(context.Background(), nil, binding); !errors.Is(err, ErrBindingKeyUnavailable) {
		t.Fatalf("missing key error = %v", err)
	}
	stale := binding
	stale.KeyVersion = 1
	if err := ValidateNewBindingKey(context.Background(), active, stale); err == nil {
		t.Fatal("stale key accepted for new write")
	}
	retired := bindingKeyStoreStub{byVersionErr: ErrBindingKeyUnavailable}
	if err := ValidateRetainedBindingKey(context.Background(), retired, binding); !errors.Is(err, ErrBindingKeyUnavailable) {
		t.Fatalf("retired key error = %v", err)
	}
	invalid := binding
	invalid.Domain = "wrong"
	if err := ValidateRetainedBindingKey(context.Background(), active, invalid); err == nil {
		t.Fatal("mismatched retained key metadata was accepted")
	}
	verifyOnly := bindingKeyStoreStub{activeKey: BindingKey{Version: 2, Epoch: 1, Algorithm: BindingAlgorithmV1, Domain: BindingDomainV1, Status: BindingKeyVerifyOnly, Material: []byte("abcdef0123456789abcdef0123456789")}, byVersionKey: BindingKey{Version: 2, Epoch: 1, Algorithm: BindingAlgorithmV1, Domain: BindingDomainV1, Status: BindingKeyVerifyOnly, Material: []byte("abcdef0123456789abcdef0123456789")}}
	if _, err := Bind(context.Background(), verifyOnly, bindingAction(`{"a":1}`)); err == nil {
		t.Fatal("verify-only key accepted for new binding")
	}
	if err := ValidateRetainedBindingKey(context.Background(), verifyOnly, binding); err != nil {
		t.Fatalf("verify-only retained key rejected: %v", err)
	}
}

func TestBindingKeyIdentifiersAndRetainedCardinalityBoundaries(t *testing.T) {
	const largeIdentifier = int64(^uint64(0) >> 1)
	keys := make([]BindingKey, maxBindingKeys)
	for index := range keys {
		keys[index] = BindingKey{
			Version:   int64(index + 1),
			Epoch:     1,
			Algorithm: BindingAlgorithmV1,
			Domain:    BindingDomainV1,
			Status:    BindingKeyVerifyOnly,
			Material:  []byte("0123456789abcdef0123456789abcdef"),
		}
	}
	keys[len(keys)-1].Version = largeIdentifier
	keys[len(keys)-1].Status = BindingKeyActive

	store, err := NewStaticBindingKeyStore(keys)
	if err != nil {
		t.Fatalf("16 retained keys with a large positive identifier were rejected: %v", err)
	}
	active, err := store.Active(context.Background())
	if err != nil || active.Version != largeIdentifier {
		t.Fatalf("Active() = %#v, %v; want identifier %d", active, err, largeIdentifier)
	}

	seventeenth := BindingKey{
		Version:   maxBindingKeys,
		Epoch:     1,
		Algorithm: BindingAlgorithmV1,
		Domain:    BindingDomainV1,
		Status:    BindingKeyVerifyOnly,
		Material:  []byte("abcdef0123456789abcdef0123456789"),
	}
	if _, err := NewStaticBindingKeyStore(append(keys, seventeenth)); err == nil {
		t.Fatal("17 retained keys were accepted")
	}
}

func TestAuthenticateBindingEnforcesMACAndRotationPolicy(t *testing.T) {
	versionOne := BindingKey{Version: 1, Algorithm: BindingAlgorithmV1, Domain: BindingDomainV1, Status: BindingKeyActive, Material: []byte("0123456789abcdef0123456789abcdef")}
	originalKeys, err := NewStaticBindingKeyStore([]BindingKey{versionOne})
	if err != nil {
		t.Fatal(err)
	}
	action := bindingAction(`{"secret":"transient"}`)
	binding, err := Bind(context.Background(), originalKeys, action)
	if err != nil {
		t.Fatal(err)
	}
	approval := approvalFromAction(action, binding)
	if err := AuthenticateNewBinding(context.Background(), originalKeys, approval, action.Payload); err != nil {
		t.Fatalf("active binding rejected: %v", err)
	}

	forged := approval
	forged.Spec.Binding.Tag = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if err := AuthenticateNewBinding(context.Background(), originalKeys, forged, action.Payload); err == nil {
		t.Fatal("forged tag accepted")
	}
	wrongMaterial, err := NewStaticBindingKeyStore([]BindingKey{{Version: 1, Algorithm: BindingAlgorithmV1, Domain: BindingDomainV1, Status: BindingKeyActive, Material: []byte("abcdef0123456789abcdef0123456789")}})
	if err != nil {
		t.Fatal(err)
	}
	if err := AuthenticateNewBinding(context.Background(), wrongMaterial, approval, action.Payload); err == nil {
		t.Fatal("tag signed by a different key was accepted")
	}

	versionOne.Status = BindingKeyVerifyOnly
	rotatedKeys, err := NewStaticBindingKeyStore([]BindingKey{
		versionOne,
		{Version: 2, Algorithm: BindingAlgorithmV1, Domain: BindingDomainV1, Status: BindingKeyActive, Material: []byte("abcdef0123456789abcdef0123456789")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := AuthenticateRetainedBinding(context.Background(), rotatedKeys, approval, action.Payload); err != nil {
		t.Fatalf("identical replay under retained verify-only key rejected: %v", err)
	}
	if err := AuthenticateNewBinding(context.Background(), rotatedKeys, approval, action.Payload); err == nil {
		t.Fatal("verify-only key accepted for a new write")
	}
}

func TestValidateInitialStatusRejectsEveryCallerOwnedField(t *testing.T) {
	now := time.Unix(1, 0).UTC()
	tests := map[string]func(*api.RuntimeApprovalStatus){
		"decision evidence delivered": func(status *api.RuntimeApprovalStatus) { status.DecisionEvidenceDelivered = true },
		"decision audit":              func(status *api.RuntimeApprovalStatus) { status.DecisionAudit = &api.RuntimeApprovalDecisionAudit{} },
		"decided issuer":              func(status *api.RuntimeApprovalStatus) { status.DecidedByIssuer = "issuer" },
		"decided subject":             func(status *api.RuntimeApprovalStatus) { status.DecidedBySubject = "subject" },
		"decided at":                  func(status *api.RuntimeApprovalStatus) { status.DecidedAt = &now },
		"execution disposition":       func(status *api.RuntimeApprovalStatus) { status.ExecutionDisposition = "allowed" },
		"preparation token":           func(status *api.RuntimeApprovalStatus) { status.PreparationToken = "token" },
		"preparation deadline":        func(status *api.RuntimeApprovalStatus) { status.PreparationDeadline = &now },
		"claimant":                    func(status *api.RuntimeApprovalStatus) { status.Claimant = &api.RuntimeApprovalClaimant{} },
		"fencing token":               func(status *api.RuntimeApprovalStatus) { status.FencingToken = 1 },
		"termination proof":           func(status *api.RuntimeApprovalStatus) { status.TerminationProofRef = "proof" },
		"reconciliation evidence":     func(status *api.RuntimeApprovalStatus) { status.ReconciliationEvidenceRef = "evidence" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			status := InitialStatus(api.RuntimeApprovalDecisionPending)
			mutate(&status)
			if err := ValidateInitialStatus(status); err == nil {
				t.Fatal("non-pristine initial status accepted")
			}
		})
	}
}

func TestNormalizeAndValidateReferencesOrdersAuthorizationAndRejectsInvalidScope(t *testing.T) {
	action := bindingAction(`{"a":1}`)
	approval := api.RuntimeApproval{Spec: api.RuntimeApprovalSpec{
		Workspace: action.Workspace, AgentRef: action.AgentRef, AgentIdentityRef: action.AgentIdentityRef,
		AgentInstanceRef: action.AgentInstanceRef, TargetRef: action.TargetRef,
		AutonomyClassRef: action.AutonomyClassRef,
		AuthorizationRefs: []api.RuntimeApprovalReference{
			{Kind: "ToolGrant", Scope: "Namespaced", Namespace: "team-a", Name: "z", UID: "z", ResourceVersion: 1},
			{Kind: "ToolGrant", Scope: "Namespaced", Namespace: "team-a", Name: "a", UID: "a", ResourceVersion: 1},
		},
	}}
	if err := NormalizeAndValidateReferences(&approval); err != nil {
		t.Fatal(err)
	}
	if approval.Spec.AuthorizationRefs[0].Name != "a" {
		t.Fatalf("authorization refs not normalized: %#v", approval.Spec.AuthorizationRefs)
	}
	approval.Spec.AuthorizationRefs = append(approval.Spec.AuthorizationRefs, approval.Spec.AuthorizationRefs[0])
	if err := NormalizeAndValidateReferences(&approval); err == nil {
		t.Fatal("duplicate authorization reference accepted")
	}
	approval.Spec.AuthorizationRefs = nil
	approval.Spec.AgentRef.Namespace = "other"
	if err := NormalizeAndValidateReferences(&approval); err == nil {
		t.Fatal("cross-workspace reference accepted")
	}
}

func TestNormalizeAndValidateReferencesChecksEveryReferenceIdentityField(t *testing.T) {
	action := bindingAction(`{"a":1}`)
	trustProfile := api.RuntimeApprovalReference{Kind: "TrustProfile", Scope: "Namespaced", Namespace: action.Workspace, Name: "default", UID: "profile-uid", ResourceVersion: 1}
	approval := approvalFromAction(action, api.RuntimeActionBinding{})
	approval.Spec.TrustProfileRef = &trustProfile
	references := map[string]func(*api.RuntimeApproval) *api.RuntimeApprovalReference{
		"agent":          func(value *api.RuntimeApproval) *api.RuntimeApprovalReference { return &value.Spec.AgentRef },
		"agent identity": func(value *api.RuntimeApproval) *api.RuntimeApprovalReference { return &value.Spec.AgentIdentityRef },
		"agent instance": func(value *api.RuntimeApproval) *api.RuntimeApprovalReference { return &value.Spec.AgentInstanceRef },
		"target":         func(value *api.RuntimeApproval) *api.RuntimeApprovalReference { return &value.Spec.TargetRef },
		"trust profile":  func(value *api.RuntimeApproval) *api.RuntimeApprovalReference { return value.Spec.TrustProfileRef },
		"autonomy class": func(value *api.RuntimeApproval) *api.RuntimeApprovalReference { return &value.Spec.AutonomyClassRef },
		"authorization": func(value *api.RuntimeApproval) *api.RuntimeApprovalReference {
			return &value.Spec.AuthorizationRefs[0]
		},
	}
	invalidFields := map[string]func(*api.RuntimeApprovalReference){
		"scope":            func(ref *api.RuntimeApprovalReference) { ref.Scope = "" },
		"UID":              func(ref *api.RuntimeApprovalReference) { ref.UID = "" },
		"resource version": func(ref *api.RuntimeApprovalReference) { ref.ResourceVersion = 0 },
	}
	for referenceName, selectReference := range references {
		for fieldName, invalidate := range invalidFields {
			t.Run(referenceName+"_"+fieldName, func(t *testing.T) {
				candidate := approval
				profileCopy := *approval.Spec.TrustProfileRef
				candidate.Spec.TrustProfileRef = &profileCopy
				candidate.Spec.AuthorizationRefs = append([]api.RuntimeApprovalReference(nil), approval.Spec.AuthorizationRefs...)
				invalidate(selectReference(&candidate))
				if err := NormalizeAndValidateReferences(&candidate); err == nil {
					t.Fatal("invalid reference accepted")
				}
			})
		}
	}
}

func TestBindingCrossWorkspaceCommunicationTarget(t *testing.T) {
	keys, err := NewStaticBindingKeyStore([]BindingKey{{Version: 1, Algorithm: BindingAlgorithmV1, Domain: BindingDomainV1, Status: BindingKeyActive, Material: []byte("0123456789abcdef0123456789abcdef")}})
	if err != nil {
		t.Fatal(err)
	}
	communication := func() ActionEnvelope {
		action := bindingAction(`{"message":"review this"}`)
		action.Plane = api.RuntimeApprovalPlaneCommunication
		action.Operation = "issue-capability"
		action.TargetRef.Kind = "AgentService"
		action.TargetRef.Namespace = "team-b"
		return action
	}
	action := communication()
	binding, err := Bind(context.Background(), keys, action)
	if err != nil {
		t.Fatalf("cross-workspace AgentService action binding rejected: %v", err)
	}
	approval := approvalFromAction(action, binding)
	if err := NormalizeAndValidateReferences(&approval); err != nil {
		t.Fatal("valid cross-workspace Communication approval rejected", err)
	}
	if err := AuthenticateNewBinding(context.Background(), keys, approval, action.Payload); err != nil {
		t.Fatal("cross-workspace Communication MAC did not authenticate", err)
	}
	drifted := action
	drifted.TargetRef.Namespace = "team-c"
	if err := Verify(context.Background(), keys, drifted, binding); err == nil {
		t.Fatal("changing the target workspace preserved the action MAC")
	}
	invalid := map[string]func(*ActionEnvelope){
		"tool target":          func(value *ActionEnvelope) { value.Plane = api.RuntimeApprovalPlaneTool },
		"memory target":        func(value *ActionEnvelope) { value.Plane = api.RuntimeApprovalPlaneMemory },
		"other target kind":    func(value *ActionEnvelope) { value.TargetRef.Kind = "Tool" },
		"empty target scope":   func(value *ActionEnvelope) { value.TargetRef.Scope = "" },
		"cluster target scope": func(value *ActionEnvelope) { value.TargetRef.Scope = "Cluster" },
		"empty namespace":      func(value *ActionEnvelope) { value.TargetRef.Namespace = "" },
		"missing target UID":   func(value *ActionEnvelope) { value.TargetRef.UID = "" },
		"missing target RV":    func(value *ActionEnvelope) { value.TargetRef.ResourceVersion = 0 },
		"foreign source agent": func(value *ActionEnvelope) { value.AgentRef.Namespace = "team-b" },
		"foreign identity":     func(value *ActionEnvelope) { value.AgentIdentityRef.Namespace = "team-b" },
		"foreign instance":     func(value *ActionEnvelope) { value.AgentInstanceRef.Namespace = "team-b" },
		"foreign authority":    func(value *ActionEnvelope) { value.AuthorizationRefs[0].Namespace = "team-b" },
	}
	for name, change := range invalid {
		t.Run(name, func(t *testing.T) {
			candidate := communication()
			change(&candidate)
			if _, err := Bind(context.Background(), keys, candidate); err == nil {
				t.Fatal("cross-workspace exception escaped the Communication AgentService target")
			}
		})
	}
}

type bindingKeyStoreStub struct {
	activeKey    BindingKey
	byVersionKey BindingKey
	byVersionErr error
}

func (s bindingKeyStoreStub) Active(context.Context) (BindingKey, error) {
	return s.activeKey, nil
}

func (s bindingKeyStoreStub) ByVersion(context.Context, int64) (BindingKey, error) {
	return s.byVersionKey, s.byVersionErr
}

func (s bindingKeyStoreStub) All(context.Context) ([]BindingKey, error) {
	return []BindingKey{s.activeKey}, nil
}

func TestSameImmutableBindingDoesNotMutateCallerAuthorizationRefs(t *testing.T) {
	refA := api.RuntimeApprovalReference{Kind: "ToolGrant", Scope: "Namespaced", Namespace: "team-a", Name: "a", UID: "a", ResourceVersion: 1}
	refZ := api.RuntimeApprovalReference{Kind: "ToolGrant", Scope: "Namespaced", Namespace: "team-a", Name: "z", UID: "z", ResourceVersion: 1}
	existing := api.RuntimeApproval{Spec: api.RuntimeApprovalSpec{Binding: api.RuntimeActionBinding{Tag: "tag"}, AuthorizationRefs: []api.RuntimeApprovalReference{refZ, refA}}}
	proposed := api.RuntimeApproval{Spec: api.RuntimeApprovalSpec{Binding: api.RuntimeActionBinding{Tag: "tag"}, AuthorizationRefs: []api.RuntimeApprovalReference{refA, refZ}}}

	if !SameImmutableBinding(existing, proposed) {
		t.Fatal("equivalent authorization ordering was not accepted")
	}
	if existing.Spec.AuthorizationRefs[0] != refZ || proposed.Spec.AuthorizationRefs[0] != refA {
		t.Fatalf("SameImmutableBinding mutated caller slices: existing=%#v proposed=%#v", existing.Spec.AuthorizationRefs, proposed.Spec.AuthorizationRefs)
	}
}

func TestInstanceConfigurationBindingPreservesLegacyAndAuthenticatesNewDigest(t *testing.T) {
	keys := newGate3Keys(t)
	legacyAction := bindingAction(`{"input":"x"}`)
	legacyTag, err := Bind(context.Background(), keys, legacyAction)
	if err != nil {
		t.Fatal(err)
	}
	legacy := approvalFromAction(legacyAction, legacyTag)
	changed := legacy
	changed.Spec.AgentInstanceRef.ResourceVersion++
	if SameImmutableBinding(legacy, changed) {
		t.Fatal("legacy references must still bind their exact resourceVersion")
	}
	newAction := legacyAction
	newAction.AgentInstanceRef.ConfigurationDigest = strings.Repeat("a", 64)
	newTag, err := Bind(context.Background(), keys, newAction)
	if err != nil {
		t.Fatal(err)
	}
	bound := approvalFromAction(newAction, newTag)
	changed = bound
	changed.Spec.AgentInstanceRef.ResourceVersion++
	if !SameImmutableBinding(bound, changed) {
		t.Fatal("a signed instance configuration digest must tolerate observed version drift")
	}
	if err := AuthenticateRetainedBinding(context.Background(), keys, changed, newAction.Payload); err != nil {
		t.Fatal(err)
	}
	changed.Spec.AgentInstanceRef.ConfigurationDigest = strings.Repeat("b", 64)
	if SameImmutableBinding(bound, changed) {
		t.Fatal("changed configuration digest reused the original binding")
	}
	if err := AuthenticateRetainedBinding(context.Background(), keys, changed, newAction.Payload); err == nil {
		t.Fatal("changed configuration digest retained a valid signature")
	}
	if SameImmutableBinding(legacy, bound) {
		t.Fatal("legacy records must not acquire configuration-digest semantics implicitly")
	}
	for _, digest := range []string{"short", strings.Repeat("G", 64), strings.Repeat("A", 64)} {
		invalid := newAction
		invalid.AgentInstanceRef.ConfigurationDigest = digest
		if _, err := Bind(context.Background(), keys, invalid); err == nil {
			t.Fatalf("invalid configuration digest %q was accepted", digest)
		}
	}
	invalid := newAction
	invalid.TargetRef.ConfigurationDigest = strings.Repeat("a", 64)
	if _, err := Bind(context.Background(), keys, invalid); err == nil {
		t.Fatal("target resource versions must not be replaced with configuration digests")
	}
}

func TestBindingRejectsDuplicateJSONKeysAndAuthorizationRefs(t *testing.T) {
	keys, _ := NewStaticBindingKeyStore([]BindingKey{{Version: 1, Algorithm: BindingAlgorithmV1, Domain: BindingDomainV1, Status: BindingKeyActive, Material: []byte("0123456789abcdef0123456789abcdef")}})
	action := bindingAction(`{"a":1,"a":2}`)
	if _, err := Bind(context.Background(), keys, action); err == nil {
		t.Fatal("duplicate payload key accepted")
	}
	action = bindingAction(`{"a":1}`)
	action.AuthorizationRefs = append(action.AuthorizationRefs, action.AuthorizationRefs[0])
	if _, err := Bind(context.Background(), keys, action); err == nil {
		t.Fatal("duplicate authorization reference accepted")
	}
}

func TestCanonicalizeJSONRejectsInvalidUTF8AndUnpairedSurrogates(t *testing.T) {
	tests := map[string][]byte{
		"invalid UTF-8":       {'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'},
		"lone high surrogate": []byte(`{"x":"\ud800"}`),
		"lone low surrogate":  []byte(`{"x":"\udc00"}`),
	}
	for name, payload := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := CanonicalizeJSON(payload); err == nil {
				t.Fatal("malformed Unicode was canonicalized")
			}
		})
	}
	if _, err := CanonicalizeJSON([]byte(`{"x":"\ud83d\ude00"}`)); err != nil {
		t.Fatalf("valid surrogate pair rejected: %v", err)
	}
}

func TestCanonicalizeJSONNormalizesEquivalentNumbers(t *testing.T) {
	vectors := []string{`{"value":1}`, `{"value":1.0}`, `{"value":1e0}`, `{"value":10e-1}`}
	var expected string
	for _, vector := range vectors {
		canonical, err := CanonicalizeJSON([]byte(vector))
		if err != nil {
			t.Fatalf("CanonicalizeJSON(%s): %v", vector, err)
		}
		if expected == "" {
			expected = string(canonical)
		} else if string(canonical) != expected {
			t.Fatalf("CanonicalizeJSON(%s) = %s, want %s", vector, canonical, expected)
		}
	}
	positive, err := CanonicalizeJSON([]byte(`{"value":0}`))
	if err != nil {
		t.Fatal(err)
	}
	negative, err := CanonicalizeJSON([]byte(`{"value":-0}`))
	if err != nil || string(negative) != string(positive) {
		t.Fatalf("signed zero = %s, %v; want %s", negative, err, positive)
	}
}

func TestCanonicalizeJSONPreservesArbitraryPrecision(t *testing.T) {
	canonical, err := CanonicalizeJSON([]byte(`{"value":123456789012345678901234567890.00}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != `{"value":12345678901234567890123456789e1}` {
		t.Fatalf("canonical = %s", canonical)
	}
	if _, err := CanonicalizeJSON([]byte(`{"value":1e999999999999}`)); err == nil {
		t.Fatal("oversized exponent was accepted")
	}
}

func TestCanonicalJSONSharedLanguageVectors(t *testing.T) {
	type acceptedVector struct {
		Name      string `json:"name"`
		Input     string `json:"input"`
		Canonical string `json:"canonical"`
		Tag       string `json:"tag"`
	}
	type rejectedVector struct {
		Name  string `json:"name"`
		Input string `json:"input"`
	}
	type actionVector struct {
		Name      string         `json:"name"`
		Action    ActionEnvelope `json:"action"`
		Canonical string         `json:"canonical"`
		Tag       string         `json:"tag"`
	}
	var fixture struct {
		Key      string           `json:"key"`
		Domain   string           `json:"domain"`
		Accepted []acceptedVector `json:"accepted"`
		Rejected []rejectedVector `json:"rejected"`
		Actions  []actionVector   `json:"actions"`
	}
	fixturePath := filepath.Join("..", "..", "tests", "runtimeapproval", "canonical_vectors.json")
	encoded, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, vector := range fixture.Accepted {
		t.Run(vector.Name, func(t *testing.T) {
			canonical, err := CanonicalizeJSON([]byte(vector.Input))
			if err != nil {
				t.Fatal(err)
			}
			if string(canonical) != vector.Canonical {
				t.Fatalf("canonical = %s, want %s", canonical, vector.Canonical)
			}
			mac := hmac.New(sha256.New, []byte(fixture.Key))
			_, _ = mac.Write([]byte(fixture.Domain))
			_, _ = mac.Write([]byte{0})
			_, _ = mac.Write(canonical)
			if tag := base64.RawURLEncoding.EncodeToString(mac.Sum(nil)); tag != vector.Tag {
				t.Fatalf("tag = %s, want %s", tag, vector.Tag)
			}
		})
	}
	for _, vector := range fixture.Rejected {
		t.Run(vector.Name, func(t *testing.T) {
			if _, err := CanonicalizeJSON([]byte(vector.Input)); err == nil {
				t.Fatal("invalid JSON was canonicalized")
			}
		})
	}
	for _, vector := range fixture.Actions {
		t.Run("action_"+vector.Name, func(t *testing.T) {
			canonical, err := canonicalAction(vector.Action)
			if err != nil {
				t.Fatal(err)
			}
			if string(canonical) != vector.Canonical {
				t.Fatalf("canonical action = %s, want %s", canonical, vector.Canonical)
			}
			mac := hmac.New(sha256.New, []byte(fixture.Key))
			_, _ = mac.Write([]byte(fixture.Domain))
			_, _ = mac.Write([]byte{0})
			_, _ = mac.Write(canonical)
			if tag := base64.RawURLEncoding.EncodeToString(mac.Sum(nil)); tag != vector.Tag {
				t.Fatalf("action tag = %s, want %s", tag, vector.Tag)
			}
		})
	}
}

func TestCanonicalNameIsDeterministicBoundedAndUnambiguous(t *testing.T) {
	first := CanonicalName("ab", "c")
	if first != CanonicalName("ab", "c") || first == CanonicalName("a", "bc") || len(first) != 55 {
		t.Fatalf("unexpected canonical names: %q and %q", first, CanonicalName("a", "bc"))
	}
}

func bindingAction(payload string) ActionEnvelope {
	ref := func(kind, name, uid string) api.RuntimeApprovalReference {
		return api.RuntimeApprovalReference{Kind: kind, Scope: "Namespaced", Namespace: "team-a", Name: name, UID: uid, ResourceVersion: 1}
	}
	return ActionEnvelope{
		Plane: api.RuntimeApprovalPlaneTool, Operation: "invoke", Workspace: "team-a", InvocationID: "invocation-a",
		AgentRef: ref("Agent", "agent-a", "agent-uid"), AgentIdentityRef: ref("AgentIdentity", "agent-a", "identity-uid"),
		AgentInstanceRef: ref("AgentInstance", "agent-a-1", "instance-uid"), SessionRef: "session-a",
		TargetRef: ref("Tool", "tool-a", "tool-uid"), AutonomyClassRef: api.RuntimeApprovalReference{Kind: "AutonomyClass", Scope: "Cluster", Name: "supervised", UID: "class-uid", ResourceVersion: 1},
		AuthorizationRefs: []api.RuntimeApprovalReference{ref("ToolGrant", "grant-a", "grant-uid")}, Payload: json.RawMessage(payload),
	}
}

func approvalFromAction(action ActionEnvelope, binding api.RuntimeActionBinding) api.RuntimeApproval {
	return api.RuntimeApproval{Spec: api.RuntimeApprovalSpec{
		Workspace: action.Workspace, InvocationID: action.InvocationID, Plane: action.Plane, Operation: action.Operation,
		AgentRef: action.AgentRef, AgentIdentityRef: action.AgentIdentityRef, AgentInstanceRef: action.AgentInstanceRef,
		SessionRef: action.SessionRef, TargetRef: action.TargetRef, TrustProfileRef: action.TrustProfileRef,
		AutonomyClassRef: action.AutonomyClassRef, AuthorizationRefs: append([]api.RuntimeApprovalReference(nil), action.AuthorizationRefs...),
		Binding: binding,
	}}
}
