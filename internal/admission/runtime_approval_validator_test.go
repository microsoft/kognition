// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	k8sapi "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
)

func TestRuntimeApprovalValidatorProtectsDedicatedLifecycle(t *testing.T) {
	const controllerUsername = "system:serviceaccount:kognition-system:kognition-k8s-controller"
	const gatewayUsername = "system:serviceaccount:kognition-system:kognition-tool-gateway"
	validator := NewRuntimeApprovalValidator(controllerUsername, gatewayUsername).WithDecisionAuthority(controllerUsername).WithExecutionWriters(map[string]string{gatewayUsername: api.RuntimeApprovalPlaneTool})
	current := admissionRuntimeApproval()
	encode := func(value api.RuntimeApproval) []byte { data, _ := json.Marshal(value); return data }
	controller := UserInfo{Username: controllerUsername}
	if response := validator.Validate(context.Background(), &Request{Operation: OperationCreate, Kind: "RuntimeApproval", Namespace: "team-a", Object: encode(current), UserInfo: controller}); !response.Allowed {
		t.Fatalf("valid initial create denied: %s", response.Reason)
	}
	if response := validator.Validate(context.Background(), &Request{Operation: OperationCreate, Kind: "RuntimeApproval", Namespace: "team-a", Object: encode(current), UserInfo: UserInfo{Username: gatewayUsername}}); !response.Allowed {
		t.Fatalf("configured gateway create denied: %s", response.Reason)
	}
	if response := validator.Validate(context.Background(), &Request{Operation: OperationCreate, Kind: "RuntimeApproval", Namespace: "team-a", Object: encode(current), UserInfo: UserInfo{Username: "system:serviceaccount:kognition-system:unknown"}}); response.Allowed {
		t.Fatal("unconfigured service account create was admitted")
	}
	forged := current
	forged.Status.PreparationToken = "forged"
	if response := validator.Validate(context.Background(), &Request{Operation: OperationCreate, Kind: "RuntimeApproval", Namespace: "team-a", Object: encode(forged), UserInfo: controller}); response.Allowed || !strings.Contains(response.Reason, "caller-owned") {
		t.Fatalf("forged create response = %#v", response)
	}
	mutated := current
	mutated.Spec.Operation = "changed"
	if response := validator.Validate(context.Background(), &Request{Operation: OperationUpdate, Kind: "RuntimeApproval", Namespace: "team-a", Object: encode(mutated), OldObject: encode(current)}); response.Allowed {
		t.Fatal("spec mutation was admitted")
	}
	terminal := current
	terminal.Status.DecisionState = api.RuntimeApprovalDecisionApproved
	terminal.Status.DecidedByIssuer = "issuer"
	terminal.Status.DecidedBySubject = "subject"
	terminal.Status.DecidedAt = newTimePointer()
	runtimeapproval.PopulateDecisionAudit(&terminal)
	withoutAudit := terminal
	withoutAudit.Status.DecisionAudit = nil
	if response := validator.Validate(context.Background(), &Request{Operation: OperationUpdate, Kind: "RuntimeApproval", Namespace: "team-a", Object: encode(withoutAudit), OldObject: encode(current), UserInfo: controller}); response.Allowed {
		t.Fatal("terminal decision without atomic audit was admitted")
	}
	if response := validator.Validate(context.Background(), &Request{Operation: OperationUpdate, Kind: "RuntimeApproval", Namespace: "team-a", Object: encode(terminal), OldObject: encode(current), UserInfo: UserInfo{Username: "mallory"}}); response.Allowed || !strings.Contains(response.Reason, "trusted lifecycle writers") {
		t.Fatalf("unauthorized terminal decision response = %#v", response)
	}
	if response := validator.Validate(context.Background(), &Request{Operation: OperationUpdate, Kind: "RuntimeApproval", Namespace: "team-a", Object: encode(terminal), OldObject: encode(current), UserInfo: UserInfo{Username: gatewayUsername}}); response.Allowed {
		t.Fatal("lifecycle gateway forged a human terminal decision")
	}
	if response := validator.Validate(context.Background(), &Request{Operation: OperationUpdate, Kind: "RuntimeApproval", Namespace: "team-a", Object: encode(terminal), OldObject: encode(current), UserInfo: controller}); !response.Allowed {
		t.Fatalf("authorized terminal decision denied: %s", response.Reason)
	}
	withoutFinalizer := current
	withoutFinalizer.Finalizers = nil
	if response := validator.Validate(context.Background(), &Request{Operation: OperationUpdate, Kind: "RuntimeApproval", Namespace: "team-a", Object: encode(withoutFinalizer), OldObject: encode(current), UserInfo: UserInfo{Username: gatewayUsername}}); response.Allowed {
		t.Fatal("gateway RuntimeApproval finalizer removal was admitted")
	}
	changed := terminal
	changed.Status.DecisionState = api.RuntimeApprovalDecisionRejected
	if response := validator.Validate(context.Background(), &Request{Operation: OperationUpdate, Kind: "RuntimeApproval", Namespace: "team-a", Object: encode(changed), OldObject: encode(terminal)}); response.Allowed {
		t.Fatal("terminal decision mutation was admitted")
	}
	terminal.Status.ExecutionState = api.RuntimeApprovalExecutionCompleted
	terminal.Status.DecisionEvidenceDelivered = true
	terminal.Finalizers = nil
	if response := validator.Validate(context.Background(), &Request{Operation: OperationDelete, Kind: "RuntimeApproval", Namespace: "team-a", OldObject: encode(terminal), UserInfo: controller}); !response.Allowed {
		t.Fatalf("compacted terminal RuntimeApproval delete denied: %s", response.Reason)
	}
	terminal.Finalizers = []string{k8sapi.RuntimeApprovalCompactionFinalizer}
	if response := validator.Validate(context.Background(), &Request{Operation: OperationDelete, Kind: "RuntimeApproval", Namespace: "team-a", OldObject: encode(terminal), UserInfo: controller}); response.Allowed {
		t.Fatal("RuntimeApproval delete with compaction finalizer was admitted")
	}
	terminal.Finalizers = nil
	if response := validator.Validate(context.Background(), &Request{Operation: OperationDelete, Kind: "RuntimeApproval", Namespace: "team-a", OldObject: encode(terminal), UserInfo: UserInfo{Username: gatewayUsername}}); response.Allowed {
		t.Fatal("gateway RuntimeApproval delete was admitted")
	}
	if response := validator.Validate(context.Background(), &Request{Operation: OperationDelete, Kind: "RuntimeApproval", Namespace: "team-a", OldObject: encode(terminal), UserInfo: UserInfo{Username: "mallory"}}); response.Allowed {
		t.Fatal("non-controller RuntimeApproval delete was admitted")
	}
	pending := current
	pending.Finalizers = nil
	if response := validator.Validate(context.Background(), &Request{Operation: OperationDelete, Kind: "RuntimeApproval", Namespace: "team-a", OldObject: encode(pending), UserInfo: controller}); response.Allowed {
		t.Fatal("non-terminal RuntimeApproval delete was admitted")
	}
}

func TestRuntimeApprovalValidatorRejectsGatewayHumanDecisionForgery(t *testing.T) {
	controller := "system:serviceaccount:kognition-system:kognition-k8s-controller"
	for _, component := range []string{"tool-gateway", "memory-policy-gateway", "agent-communication-gateway"} {
		t.Run(component, func(t *testing.T) {
			gateway := "system:serviceaccount:kognition-system:kognition-" + component
			validator := NewRuntimeApprovalValidator(controller, gateway)
			pending := admissionRuntimeApproval()
			forged := pending
			forged.Status.DecisionState = api.RuntimeApprovalDecisionApproved
			forged.Status.DecidedByIssuer = "kubernetes.io/tokenreview"
			forged.Status.DecidedBySubject = "human-approver"
			forged.Status.DecidedAt = newTimePointer()
			runtimeapproval.PopulateDecisionAudit(&forged)
			oldObject, err := json.Marshal(pending)
			if err != nil {
				t.Fatal(err)
			}
			object, err := json.Marshal(forged)
			if err != nil {
				t.Fatal(err)
			}
			response := validator.Validate(context.Background(), &Request{
				Operation: OperationUpdate, Kind: "RuntimeApproval", Namespace: pending.Namespace,
				OldObject: oldObject, Object: object, UserInfo: UserInfo{Username: gateway},
			})
			if response.Allowed {
				t.Fatal("gateway forged a human decision through direct status mutation")
			}
		})
	}
}

func TestRuntimeApprovalValidatorBindsCrossWorkspaceCommunicationTarget(t *testing.T) {
	const controller = "system:serviceaccount:kognition-system:kognition-k8s-controller"
	const gateway = "system:serviceaccount:kognition-system:kognition-agent-communication-gateway"
	validator := NewRuntimeApprovalValidator(controller, gateway).WithExecutionWriters(map[string]string{gateway: api.RuntimeApprovalPlaneCommunication})
	approval := admissionRuntimeApproval()
	approval.Spec.Plane = api.RuntimeApprovalPlaneCommunication
	approval.Spec.Operation = "issue-capability"
	approval.Spec.TargetRef.Kind = "AgentService"
	approval.Spec.TargetRef.Namespace = "team-b"
	encode := func(value api.RuntimeApproval) []byte {
		t.Helper()
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	request := &Request{Operation: OperationCreate, Kind: "RuntimeApproval", Namespace: "team-a", Object: encode(approval), UserInfo: UserInfo{Username: gateway}}
	if response := validator.Validate(context.Background(), request); !response.Allowed {
		t.Fatalf("source-owned cross-workspace Communication approval denied: %s", response.Reason)
	}
	changed := approval
	changed.Spec.TargetRef.Namespace = "team-c"
	request.Operation, request.OldObject, request.Object = OperationUpdate, encode(approval), encode(changed)
	if response := validator.Validate(context.Background(), request); response.Allowed {
		t.Fatal("changing the approved target workspace was admitted")
	}
	changed = approval
	changed.Spec.AgentIdentityRef.Namespace = "team-b"
	request.Operation, request.OldObject, request.Object = OperationCreate, nil, encode(changed)
	if response := validator.Validate(context.Background(), request); response.Allowed {
		t.Fatal("remote target exception permitted a foreign source identity")
	}
}

func TestRuntimeApprovalValidatorRequiresExplicitHumanAuthority(t *testing.T) {
	controller := "system:serviceaccount:control:controller"
	decisionAuthority := "system:serviceaccount:control:resource-webhooks"
	gateway := "system:serviceaccount:control:tool-gateway"
	pending := admissionRuntimeApproval()
	terminal := pending
	terminal.Status.DecisionState = api.RuntimeApprovalDecisionApproved
	terminal.Status.DecidedByIssuer = "kubernetes.io/tokenreview"
	terminal.Status.DecidedBySubject = "human-approver"
	terminal.Status.DecidedAt = newTimePointer()
	runtimeapproval.PopulateDecisionAudit(&terminal)
	oldObject, _ := json.Marshal(pending)
	object, _ := json.Marshal(terminal)
	validator := NewRuntimeApprovalValidator(controller, gateway).WithExecutionWriters(map[string]string{gateway: api.RuntimeApprovalPlaneTool})
	poisoned := pending
	poisoned.Status.DecidedByIssuer = terminal.Status.DecidedByIssuer
	poisoned.Status.DecidedBySubject = terminal.Status.DecidedBySubject
	poisoned.Status.DecidedAt = terminal.Status.DecidedAt
	poisoned.Status.DecisionAudit = terminal.Status.DecisionAudit
	poisonedObject, _ := json.Marshal(poisoned)
	if validator.Validate(context.Background(), &Request{Operation: OperationUpdate, Kind: "RuntimeApproval", Namespace: pending.Namespace, OldObject: oldObject, Object: poisonedObject, UserInfo: UserInfo{Username: gateway}}).Allowed {
		t.Fatal("gateway poisoned Pending human decision evidence")
	}
	for _, username := range []string{controller, gateway, decisionAuthority, ""} {
		request := &Request{Operation: OperationUpdate, Kind: "RuntimeApproval", Namespace: pending.Namespace, OldObject: oldObject, Object: object, UserInfo: UserInfo{Username: username}}
		if validator.Validate(context.Background(), request).Allowed {
			t.Fatal("unconfigured human authority accepted", username)
		}
	}
	validator.WithDecisionAuthority(decisionAuthority)
	ordinaryClaim := terminal
	ordinaryClaim.Status.ExecutionState = api.RuntimeApprovalExecutionClaimed
	ordinaryClaim.Status.FencingToken = runtimeapproval.MinFencingToken
	ordinaryClaim.Status.Claimant = &api.RuntimeApprovalClaimant{WorkloadUID: "gateway-pod", ContainerID: "container-a", ProcessStartNonce: "gateway-process-a-0001"}
	ordinaryComplete := ordinaryClaim
	ordinaryComplete.Status.ExecutionState = api.RuntimeApprovalExecutionCompleted
	ordinaryComplete.Status.ExecutionDisposition = runtimeapproval.ExecutionDispositionSucceeded
	claimObject, _ := json.Marshal(ordinaryClaim)
	completeObject, _ := json.Marshal(ordinaryComplete)
	if validator.Validate(context.Background(), &Request{Operation: OperationUpdate, Kind: "RuntimeApproval", Namespace: pending.Namespace, OldObject: claimObject, Object: completeObject, UserInfo: UserInfo{Username: decisionAuthority}}).Allowed {
		t.Fatal("human authority performed an ordinary gateway finalization")
	}
	legacy := terminal
	legacy.Status.DecisionAudit = nil
	legacyObject, _ := json.Marshal(legacy)
	if validator.Validate(context.Background(), &Request{Operation: OperationUpdate, Kind: "RuntimeApproval", Namespace: pending.Namespace, OldObject: legacyObject, Object: object, UserInfo: UserInfo{Username: gateway}}).Allowed {
		t.Fatal("gateway fabricated legacy human decision evidence")
	}
	for _, username := range []string{controller, gateway, decisionAuthority} {
		request := &Request{Operation: OperationUpdate, Kind: "RuntimeApproval", Namespace: pending.Namespace, OldObject: oldObject, Object: object, UserInfo: UserInfo{Username: username}}
		if validator.Validate(context.Background(), request).Allowed != (username == decisionAuthority) {
			t.Fatal("human decision authority crossed", username)
		}
	}
	uncertain := terminal
	uncertain.Status.ExecutionState = api.RuntimeApprovalExecutionUncertain
	uncertain.Status.FencingToken = runtimeapproval.MinFencingToken
	uncertain.Status.Claimant = &api.RuntimeApprovalClaimant{WorkloadUID: "gateway-uid", ProcessStartNonce: "process-a"}
	reconciled := uncertain
	reconciled.Status.ExecutionState = api.RuntimeApprovalExecutionCompleted
	reconciled.Status.ExecutionDisposition = "ReconciledCompleted"
	reconciled.Status.ReconciliationEvidenceRef = "evidence-a"
	reconciled.Status.ReconciledByIssuer = "kubernetes.io/tokenreview"
	reconciled.Status.ReconciledBySubject = "human-operator"
	oldObject, _ = json.Marshal(uncertain)
	object, _ = json.Marshal(reconciled)
	for _, username := range []string{controller, gateway, decisionAuthority} {
		request := &Request{Operation: OperationUpdate, Kind: "RuntimeApproval", Namespace: pending.Namespace, OldObject: oldObject, Object: object, UserInfo: UserInfo{Username: username}}
		if validator.Validate(context.Background(), request).Allowed != (username == decisionAuthority) {
			t.Fatal("human reconciliation authority crossed", username)
		}
	}
	for _, disposition := range []string{"ReconciledCompleted", "ReconciledNotExecuted"} {
		forged := uncertain
		forged.Status.ExecutionDisposition = disposition
		object, _ = json.Marshal(forged)
		if validator.Validate(context.Background(), &Request{Operation: OperationUpdate, Kind: "RuntimeApproval", Namespace: pending.Namespace, OldObject: oldObject, Object: object, UserInfo: UserInfo{Username: gateway}}).Allowed {
			t.Fatal("gateway forged reconciliation disposition without actor evidence")
		}
	}
}

func TestRuntimeApprovalValidatorRejectsCrossPlaneExecutionWriter(t *testing.T) {
	controller := "system:serviceaccount:control:controller"
	toolGateway := "system:serviceaccount:control:tool-gateway"
	memoryGateway := "system:serviceaccount:control:memory-policy-gateway"
	validator := NewRuntimeApprovalValidator(controller, toolGateway, memoryGateway).WithExecutionWriters(map[string]string{toolGateway: api.RuntimeApprovalPlaneTool, memoryGateway: api.RuntimeApprovalPlaneMemory}).WithClaimantReader(admissionClaimantReader{})
	claimed := admissionRuntimeApproval()
	claimed.Spec.Plane = api.RuntimeApprovalPlaneMemory
	claimed.Spec.TargetRef.Kind = "MemoryClaim"
	claimed.Status.DecisionState = api.RuntimeApprovalDecisionApproved
	claimed.Status.DecidedByIssuer = "kubernetes.io/tokenreview"
	claimed.Status.DecidedBySubject = "human-approver"
	claimed.Status.DecidedAt = newTimePointer()
	claimed.Status.DecisionEvidenceDelivered = true
	runtimeapproval.PopulateDecisionAudit(&claimed)
	claimed.Status.ExecutionState = api.RuntimeApprovalExecutionClaimed
	claimed.Status.FencingToken = runtimeapproval.MinFencingToken
	claimed.Status.Claimant = &api.RuntimeApprovalClaimant{WorkloadUID: "memory-gateway-uid", ProcessStartNonce: "memory-process"}
	completed := claimed
	completed.Status.ExecutionState = api.RuntimeApprovalExecutionCompleted
	completed.Status.ExecutionDisposition = runtimeapproval.ExecutionDispositionSucceeded
	oldObject, err := json.Marshal(claimed)
	if err != nil {
		t.Fatal(err)
	}
	object, err := json.Marshal(completed)
	if err != nil {
		t.Fatal(err)
	}
	response := validator.Validate(context.Background(), &Request{Operation: OperationUpdate, Kind: "RuntimeApproval", Namespace: claimed.Namespace, OldObject: oldObject, Object: object, UserInfo: UserInfo{Username: toolGateway}})
	if response.Allowed {
		t.Fatal("Tool Gateway completed another plane's claimed execution")
	}
	request := &Request{Operation: OperationUpdate, Kind: "RuntimeApproval", Namespace: claimed.Namespace, OldObject: oldObject, Object: object, UserInfo: UserInfo{Username: memoryGateway, Extra: map[string][]string{"authentication.kubernetes.io/pod-uid": {"memory-gateway-uid"}, "authentication.kubernetes.io/pod-name": {"memory-pod"}}}}
	if response := validator.Validate(context.Background(), request); !response.Allowed {
		t.Fatal("matching Memory Gateway execution denied", response.Reason)
	}
}

func TestRuntimeApprovalValidatorRejectsOtherGatewayPodCompletion(t *testing.T) {
	gateway := "system:serviceaccount:control:tool-gateway"
	validator := NewRuntimeApprovalValidator("controller", gateway).WithExecutionWriters(map[string]string{gateway: api.RuntimeApprovalPlaneTool}).WithClaimantReader(admissionClaimantReader{})
	claimed := admissionRuntimeApproval()
	claimed.Status.DecisionState = api.RuntimeApprovalDecisionNotRequired
	claimed.Spec.InitialDecision = api.RuntimeApprovalDecisionNotRequired
	claimed.Status.DecidedAt = newTimePointer()
	runtimeapproval.PopulateDecisionAudit(&claimed)
	claimed.Status.DecisionEvidenceDelivered = true
	claimed.Status.ExecutionState = api.RuntimeApprovalExecutionClaimed
	claimed.Status.FencingToken = runtimeapproval.MinFencingToken
	claimed.Status.Claimant = &api.RuntimeApprovalClaimant{WorkloadUID: "owner-pod", ContainerID: "container-a", ProcessStartNonce: "process-a"}
	completed := claimed
	completed.Status.ExecutionState = api.RuntimeApprovalExecutionCompleted
	completed.Status.ExecutionDisposition = runtimeapproval.ExecutionDispositionSucceeded
	oldObject, _ := json.Marshal(claimed)
	object, _ := json.Marshal(completed)
	for _, uid := range []string{"", "other-pod"} {
		response := validator.Validate(context.Background(), &Request{Operation: OperationUpdate, Kind: "RuntimeApproval", Namespace: claimed.Namespace, OldObject: oldObject, Object: object, UserInfo: UserInfo{Username: gateway, Extra: map[string][]string{"authentication.kubernetes.io/pod-uid": {uid}}}})
		if response.Allowed {
			t.Fatal("unbound or foreign gateway Pod completed another claim", uid)
		}
	}
	response := validator.Validate(context.Background(), &Request{Operation: OperationUpdate, Kind: "RuntimeApproval", Namespace: claimed.Namespace, OldObject: oldObject, Object: object, UserInfo: UserInfo{Username: gateway, Extra: map[string][]string{"authentication.kubernetes.io/pod-uid": {"owner-pod"}, "authentication.kubernetes.io/pod-name": {"owner-pod"}}}})
	if !response.Allowed {
		t.Fatal("authenticated owning Pod completion denied", response.Reason)
	}
	owner := UserInfo{Username: gateway, Extra: map[string][]string{"authentication.kubernetes.io/pod-uid": {"owner-pod"}, "authentication.kubernetes.io/pod-name": {"owner-pod"}}}
	for _, mutation := range []string{"claimant-pod", "container", "nonce", "fence", "skip-claim", "uncertain-finalize", "terminal-rewrite", "missing-reader", "duplicate-pod", "claimed-preparation", "erase-proof"} {
		t.Run(mutation, func(t *testing.T) {
			before, after := claimed, completed
			claim := *after.Status.Claimant
			after.Status.Claimant = &claim
			writer := owner
			candidateValidator := NewRuntimeApprovalValidator("controller", gateway).WithExecutionWriters(map[string]string{gateway: api.RuntimeApprovalPlaneTool}).WithClaimantReader(admissionClaimantReader{})
			switch mutation {
			case "claimant-pod":
				after.Status.Claimant.WorkloadUID = "other-pod"
			case "container":
				after.Status.Claimant.ContainerID = "other-container"
			case "nonce":
				after.Status.Claimant.ProcessStartNonce = "other-process"
			case "fence":
				after.Status.FencingToken++
			case "skip-claim":
				before.Status.ExecutionState = api.RuntimeApprovalExecutionNotStarted
				before.Status.Claimant = nil
				before.Status.FencingToken = 0
			case "uncertain-finalize":
				before.Status.ExecutionState = api.RuntimeApprovalExecutionUncertain
			case "terminal-rewrite":
				before = completed
				after.Status.ExecutionState = api.RuntimeApprovalExecutionFailed
				after.Status.ExecutionDisposition = runtimeapproval.ExecutionDispositionFailed
			case "missing-reader":
				candidateValidator.WithClaimantReader(nil)
			case "duplicate-pod":
				writer.Extra = map[string][]string{"authentication.kubernetes.io/pod-uid": {"owner-pod", "owner-pod"}, "authentication.kubernetes.io/pod-name": {"owner-pod"}}
			case "claimed-preparation":
				after.Status.PreparationToken = strings.Repeat("a", 43)
			case "erase-proof":
				before.Status.TerminationProofRef = "retained-proof"
				after.Status.TerminationProofRef = ""
			}
			oldObject, _ := json.Marshal(before)
			object, _ := json.Marshal(after)
			if candidateValidator.Validate(context.Background(), &Request{Operation: OperationUpdate, Kind: "RuntimeApproval", Namespace: claimed.Namespace, OldObject: oldObject, Object: object, UserInfo: writer}).Allowed {
				t.Fatal("forged execution ownership or ordering admitted")
			}
		})
	}
}

func TestRuntimeApprovalValidatorRejectsControllerOrdinaryFinalization(t *testing.T) {
	controller := "system:serviceaccount:control:controller"
	validator := NewRuntimeApprovalValidator(controller)
	claimed := admissionRuntimeApproval()
	claimed.Spec.InitialDecision = api.RuntimeApprovalDecisionNotRequired
	claimed.Status.DecisionState = api.RuntimeApprovalDecisionNotRequired
	claimed.Status.DecidedAt = newTimePointer()
	runtimeapproval.PopulateDecisionAudit(&claimed)
	claimed.Status.DecisionEvidenceDelivered = true
	claimed.Status.ExecutionState = api.RuntimeApprovalExecutionClaimed
	claimed.Status.FencingToken = runtimeapproval.MinFencingToken
	claimed.Status.Claimant = &api.RuntimeApprovalClaimant{WorkloadUID: "gateway-pod", ContainerID: "containerd://gateway", ProcessStartNonce: "gateway-process-0001"}
	completed := claimed
	completed.Status.ExecutionState = api.RuntimeApprovalExecutionCompleted
	completed.Status.ExecutionDisposition = runtimeapproval.ExecutionDispositionSucceeded
	before, _ := json.Marshal(claimed)
	after, _ := json.Marshal(completed)
	if validator.Validate(context.Background(), &Request{Operation: OperationUpdate, Kind: "RuntimeApproval", Namespace: claimed.Namespace, OldObject: before, Object: after, UserInfo: UserInfo{Username: controller}}).Allowed {
		t.Fatal("controller fabricated ordinary completion of a retained gateway claim")
	}
	uncertain := claimed
	uncertain.Status.ExecutionState = api.RuntimeApprovalExecutionUncertain
	uncertain.Status.TerminationProofRef = "termination-proof-canonical"
	uncertainObject, _ := json.Marshal(uncertain)
	request := &Request{Operation: OperationUpdate, Kind: "RuntimeApproval", Namespace: claimed.Namespace, OldObject: before, Object: uncertainObject, UserInfo: UserInfo{Username: controller}}
	if validator.Validate(context.Background(), request).Allowed {
		t.Fatal("controller recovery admitted without independent proof reader")
	}
	for _, reader := range []admissionTerminationReader{{err: errors.New("claimant is live")}, {err: errors.New("Pod evidence unavailable")}, {proof: "different-proof"}} {
		if validator.WithTerminationReader(reader).Validate(context.Background(), request).Allowed {
			t.Fatal("controller forged or unavailable termination evidence admitted")
		}
	}
	validator.WithTerminationReader(admissionTerminationReader{proof: "termination-proof-canonical"})
	if response := validator.Validate(context.Background(), request); !response.Allowed {
		t.Fatal("independently proven uncertainty denied", response.Reason)
	}
	for _, mutation := range []string{"claimant", "fence", "preparation", "disposition"} {
		forged := uncertain
		claimant := *forged.Status.Claimant
		forged.Status.Claimant = &claimant
		switch mutation {
		case "claimant":
			forged.Status.Claimant.ContainerID = "other"
		case "fence":
			forged.Status.FencingToken++
		case "preparation":
			forged.Status.PreparationToken = strings.Repeat("a", 43)
		case "disposition":
			forged.Status.ExecutionDisposition = runtimeapproval.ExecutionDispositionSucceeded
		}
		forgedObject, _ := json.Marshal(forged)
		request.Object = forgedObject
		if validator.Validate(context.Background(), request).Allowed {
			t.Fatal("controller recovery rewrote retained execution facts", mutation)
		}
	}
}

func TestRuntimeApprovalValidatorControllerPreclaimAndInlineHumanAuthority(t *testing.T) {
	controller := "system:serviceaccount:control:controller"
	validator := NewRuntimeApprovalValidator(controller)
	prepared := admissionRuntimeApproval()
	prepared.Spec.InitialDecision = api.RuntimeApprovalDecisionNotRequired
	prepared.Status.DecisionState = api.RuntimeApprovalDecisionNotRequired
	prepared.Status.DecidedAt = newTimePointer()
	runtimeapproval.PopulateDecisionAudit(&prepared)
	prepared.Status.DecisionEvidenceDelivered = true
	prepared.Status.PreparationToken = strings.Repeat("a", 43)
	deadline := time.Now().UTC().Add(time.Minute)
	prepared.Status.PreparationDeadline = &deadline
	claimed := prepared
	claimed.Status.ExecutionState = api.RuntimeApprovalExecutionClaimed
	claimed.Status.PreparationToken = ""
	claimed.Status.PreparationDeadline = nil
	claimed.Status.FencingToken = runtimeapproval.MinFencingToken
	claimed.Status.Claimant = &api.RuntimeApprovalClaimant{WorkloadUID: "gateway-pod", ContainerID: "containerd://gateway", ProcessStartNonce: "gateway-process-0001"}
	before, _ := json.Marshal(prepared)
	after, _ := json.Marshal(claimed)
	t.Run("controller-cannot-claim", func(t *testing.T) {
		if validator.Validate(context.Background(), &Request{Operation: OperationUpdate, Kind: "RuntimeApproval", Namespace: prepared.Namespace, OldObject: before, Object: after, UserInfo: UserInfo{Username: controller}}).Allowed {
			t.Fatal("controller manufactured a gateway execution claim")
		}
	})
	for _, expired := range []bool{false, true} {
		preparation := prepared
		if expired {
			past := time.Now().UTC().Add(-time.Minute)
			preparation.Status.PreparationDeadline = &past
		}
		reaped := preparation
		reaped.Status.ExecutionState = api.RuntimeApprovalExecutionFailed
		reaped.Status.ExecutionDisposition = runtimeapproval.ExecutionDispositionAbandonedPreparation
		reaped.Status.PreparationToken = ""
		reaped.Status.PreparationDeadline = nil
		oldObject, _ := json.Marshal(preparation)
		object, _ := json.Marshal(reaped)
		if response := validator.Validate(context.Background(), &Request{Operation: OperationUpdate, Kind: "RuntimeApproval", Namespace: prepared.Namespace, OldObject: oldObject, Object: object, UserInfo: UserInfo{Username: controller}}); response.Allowed != expired {
			t.Fatal("controller preparation reaper boundary differs", expired, response.Reason)
		}
	}
	ordinary := claimed
	ordinary.Status.ExecutionState = api.RuntimeApprovalExecutionCompleted
	ordinary.Status.ExecutionDisposition = runtimeapproval.ExecutionDispositionSucceeded
	oldClaim, _ := json.Marshal(claimed)
	ordinaryObject, _ := json.Marshal(ordinary)
	if validator.WithDecisionAuthority(controller).Validate(context.Background(), &Request{Operation: OperationUpdate, Kind: "RuntimeApproval", Namespace: prepared.Namespace, OldObject: oldClaim, Object: ordinaryObject, UserInfo: UserInfo{Username: controller}}).Allowed {
		t.Fatal("inline human configuration revived ordinary finalization")
	}
	uncertain := claimed
	uncertain.Status.ExecutionState = api.RuntimeApprovalExecutionUncertain
	uncertain.Status.TerminationProofRef = "proof"
	reconciled := uncertain
	reconciled.Status.ExecutionState = api.RuntimeApprovalExecutionCompleted
	reconciled.Status.ExecutionDisposition = runtimeapproval.ExecutionDispositionReconciledCompleted
	reconciled.Status.ReconciledByIssuer = "kubernetes.io/tokenreview"
	reconciled.Status.ReconciledBySubject = "human"
	reconciled.Status.ReconciliationEvidenceRef = "evidence"
	before, _ = json.Marshal(uncertain)
	after, _ = json.Marshal(reconciled)
	t.Run("explicit-inline-human-reconciliation", func(t *testing.T) {
		if response := validator.WithDecisionAuthority(controller).Validate(context.Background(), &Request{Operation: OperationUpdate, Kind: "RuntimeApproval", Namespace: prepared.Namespace, OldObject: before, Object: after, UserInfo: UserInfo{Username: controller}}); !response.Allowed {
			t.Fatal("explicit inline human authority reconciliation denied", response.Reason)
		}
	})
}

func TestRuntimeApprovalValidatorRetainsGatewayExecutionLifecycle(t *testing.T) {
	controller := "system:serviceaccount:control:controller"
	gateway := "system:serviceaccount:control:tool-gateway"
	validator := NewRuntimeApprovalValidator(controller, gateway).WithDecisionAuthority("system:serviceaccount:control:resource-webhooks").WithExecutionWriters(map[string]string{gateway: api.RuntimeApprovalPlaneTool}).WithClaimantReader(admissionClaimantReader{})
	approved := admissionRuntimeApproval()
	automatic := approved
	automatic.Spec.InitialDecision = api.RuntimeApprovalDecisionNotRequired
	automatic.Status = runtimeapproval.InitialStatus(api.RuntimeApprovalDecisionNotRequired)
	automatic.Status.DecidedAt = newTimePointer()
	runtimeapproval.PopulateDecisionAudit(&automatic)
	created := automatic
	created.Status = api.RuntimeApprovalStatus{}
	createdObject, err := json.Marshal(created)
	if err != nil {
		t.Fatal(err)
	}
	automaticObject, err := json.Marshal(automatic)
	if err != nil {
		t.Fatal(err)
	}
	if response := validator.Validate(context.Background(), &Request{Operation: OperationCreate, Kind: "RuntimeApproval", Namespace: automatic.Namespace, Object: createdObject, UserInfo: UserInfo{Username: gateway}}); !response.Allowed {
		t.Fatal("automatic NotRequired creation denied", response.Reason)
	}
	if response := validator.Validate(context.Background(), &Request{Operation: OperationUpdate, Kind: "RuntimeApproval", Namespace: automatic.Namespace, OldObject: createdObject, Object: automaticObject, UserInfo: UserInfo{Username: gateway}}); !response.Allowed {
		t.Fatal("automatic NotRequired evidence denied", response.Reason)
	}
	approved.Status.DecisionState = api.RuntimeApprovalDecisionApproved
	approved.Status.DecidedByIssuer = "kubernetes.io/tokenreview"
	approved.Status.DecidedBySubject = "human-approver"
	approved.Status.DecidedAt = newTimePointer()
	approved.Spec.ExpiresAt = time.Now().UTC().Add(time.Hour)
	runtimeapproval.PopulateDecisionAudit(&approved)
	acknowledged := approved
	acknowledged.Status.DecisionEvidenceDelivered = true
	claimed := acknowledged
	preparationDeadline := time.Now().UTC().Add(time.Minute)
	prepared := acknowledged
	prepared.Status.PreparationToken = strings.Repeat("a", 43)
	prepared.Status.PreparationDeadline = &preparationDeadline
	claimed.Status.ExecutionState = api.RuntimeApprovalExecutionClaimed
	claimed.Status.FencingToken = runtimeapproval.MinFencingToken
	claimed.Status.Claimant = &api.RuntimeApprovalClaimant{WorkloadUID: "gateway-uid", ContainerID: "container-a", ProcessStartNonce: "gateway-process-a-0001", ClaimedAt: time.Now().UTC(), LastHeartbeat: time.Now().UTC()}
	completed := claimed
	completed.Status.ExecutionState = api.RuntimeApprovalExecutionCompleted
	completed.Status.ExecutionDisposition = runtimeapproval.ExecutionDispositionSucceeded
	for _, mutation := range []string{"missing-preparation", "missing-evidence", "expired-preparation", "expired-approval", "premature-claimant", "gateway-recovery"} {
		t.Run(mutation, func(t *testing.T) {
			before, after := prepared, claimed
			switch mutation {
			case "missing-preparation":
				before.Status.PreparationToken = ""
			case "missing-evidence":
				before.Status.DecisionEvidenceDelivered = false
			case "expired-preparation":
				expiredDeadline := time.Now().UTC().Add(-time.Minute)
				before.Status.PreparationDeadline = &expiredDeadline
			case "expired-approval":
				before.Spec.ExpiresAt = time.Now().UTC().Add(-time.Minute)
				after.Spec = before.Spec
			case "premature-claimant":
				before = acknowledged
				after = prepared
				after.Status.Claimant = claimed.Status.Claimant
			case "gateway-recovery":
				before = claimed
				after = claimed
				after.Status.ExecutionState = api.RuntimeApprovalExecutionUncertain
				after.Status.TerminationProofRef = "unverified"
			}
			oldObject, _ := json.Marshal(before)
			object, _ := json.Marshal(after)
			request := &Request{Operation: OperationUpdate, Kind: "RuntimeApproval", Namespace: approved.Namespace, OldObject: oldObject, Object: object, UserInfo: UserInfo{Username: gateway, Extra: map[string][]string{"authentication.kubernetes.io/pod-uid": {"gateway-uid"}, "authentication.kubernetes.io/pod-name": {"gateway-pod"}}}}
			if validator.Validate(context.Background(), request).Allowed {
				t.Fatal("invalid gateway claim or recovery admitted")
			}
		})
	}
	for _, transition := range []struct{ before, after api.RuntimeApproval }{{approved, acknowledged}, {acknowledged, prepared}, {prepared, claimed}, {claimed, completed}} {
		oldObject, _ := json.Marshal(transition.before)
		object, _ := json.Marshal(transition.after)
		response := validator.Validate(context.Background(), &Request{Operation: OperationUpdate, Kind: "RuntimeApproval", Namespace: approved.Namespace, OldObject: oldObject, Object: object, UserInfo: UserInfo{Username: gateway, Extra: map[string][]string{"authentication.kubernetes.io/pod-uid": {"gateway-uid"}, "authentication.kubernetes.io/pod-name": {"gateway-pod"}}}})
		if !response.Allowed {
			t.Fatal("legitimate gateway execution lifecycle denied", response.Reason)
		}
	}
	expired := prepared
	expired.Spec.ExpiresAt = time.Now().UTC().Add(-time.Minute)
	denied := expired
	denied.Status.ExecutionState = api.RuntimeApprovalExecutionFailed
	denied.Status.ExecutionDisposition = runtimeapproval.ExecutionDispositionApprovalExpiredBeforeClaim
	denied.Status.PreparationToken = ""
	denied.Status.PreparationDeadline = nil
	oldObject, _ := json.Marshal(expired)
	object, _ := json.Marshal(denied)
	if response := validator.Validate(context.Background(), &Request{Operation: OperationUpdate, Kind: "RuntimeApproval", Namespace: expired.Namespace, OldObject: oldObject, Object: object, UserInfo: UserInfo{Username: gateway}}); !response.Allowed {
		t.Fatal("expired prepared claim denial blocked", response.Reason)
	}
}

func TestRuntimeApprovalValidatorRejectsNoncanonicalCreateAndMissingControllerIdentity(t *testing.T) {
	current := admissionRuntimeApproval()
	encode := func(value api.RuntimeApproval) []byte { data, _ := json.Marshal(value); return data }
	controller := UserInfo{Username: "system:serviceaccount:kognition-system:kognition-k8s-controller"}
	validator := NewRuntimeApprovalValidator(controller.Username)

	wrongName := current
	wrongName.Name = "caller-selected"
	if response := validator.Validate(context.Background(), &Request{Operation: OperationCreate, Kind: "RuntimeApproval", Namespace: "team-a", Object: encode(wrongName), UserInfo: controller}); response.Allowed {
		t.Fatal("noncanonical RuntimeApproval name was admitted")
	}

	unordered := current
	unordered.Spec.AuthorizationRefs = []api.RuntimeApprovalReference{
		{Kind: "ToolGrant", Scope: "Namespaced", Namespace: "team-a", Name: "z", UID: "z", ResourceVersion: 1},
		{Kind: "ToolGrant", Scope: "Namespaced", Namespace: "team-a", Name: "a", UID: "a", ResourceVersion: 1},
	}
	if response := validator.Validate(context.Background(), &Request{Operation: OperationCreate, Kind: "RuntimeApproval", Namespace: "team-a", Object: encode(unordered), UserInfo: controller}); response.Allowed {
		t.Fatal("noncanonical RuntimeApproval reference ordering was admitted")
	}
	blank := current
	blank.Status = api.RuntimeApprovalStatus{}
	if response := validator.Validate(context.Background(), &Request{Operation: OperationCreate, Kind: "RuntimeApproval", Namespace: "team-a", Object: encode(blank), UserInfo: UserInfo{Username: "mallory"}}); response.Allowed {
		t.Fatal("non-controller blank-status RuntimeApproval create was admitted")
	}

	forged := current
	forged.Status.PreparationToken = "forged"
	if response := NewRuntimeApprovalValidator().Validate(context.Background(), &Request{Operation: OperationCreate, Kind: "RuntimeApproval", Namespace: "team-a", Object: encode(forged), UserInfo: controller}); response.Allowed {
		t.Fatal("configured-empty controller identity authorized a status write")
	}
}

func TestRuntimeApprovalValidatorRejectsTerminalActorAndEvidenceDrift(t *testing.T) {
	const controllerUsername = "system:serviceaccount:kognition-system:kognition-k8s-controller"
	validator := NewRuntimeApprovalValidator(controllerUsername)
	encode := func(value api.RuntimeApproval) []byte { data, _ := json.Marshal(value); return data }
	controller := UserInfo{Username: controllerUsername}
	pending := admissionRuntimeApproval()
	terminal := pending
	terminal.Status.DecisionState = api.RuntimeApprovalDecisionApproved
	terminal.Status.DecidedByIssuer = "issuer"
	terminal.Status.DecidedBySubject = "subject"
	terminal.Status.DecidedAt = newTimePointer()
	runtimeapproval.PopulateDecisionAudit(&terminal)

	changedActor := terminal
	changedActor.Status.DecidedBySubject = "other-subject"
	if response := validator.Validate(context.Background(), &Request{Operation: OperationUpdate, Kind: "RuntimeApproval", Namespace: "team-a", Object: encode(changedActor), OldObject: encode(terminal), UserInfo: controller}); response.Allowed {
		t.Fatal("terminal actor drift was admitted")
	}

	changedEvidence := terminal
	changedEvidence.Status.DecisionAudit = &api.RuntimeApprovalDecisionAudit{}
	*changedEvidence.Status.DecisionAudit = *terminal.Status.DecisionAudit
	changedEvidence.Status.DecisionAudit.Payload.Subject = "other-subject"
	if response := validator.Validate(context.Background(), &Request{Operation: OperationUpdate, Kind: "RuntimeApproval", Namespace: "team-a", Object: encode(changedEvidence), OldObject: encode(terminal), UserInfo: controller}); response.Allowed {
		t.Fatal("terminal audit evidence drift was admitted")
	}
}

func TestRuntimeApprovalValidatorAllowsAuditTimestampCanonicalizationOnAcknowledgement(t *testing.T) {
	const gatewayUsername = "system:serviceaccount:kognition-system:kognition-tool-gateway"
	validator := NewRuntimeApprovalValidator(gatewayUsername)
	encode := func(value api.RuntimeApproval) []byte { data, _ := json.Marshal(value); return data }
	terminal := admissionRuntimeApproval()
	terminal.Status.DecisionState = api.RuntimeApprovalDecisionApproved
	terminal.Status.DecidedByIssuer = "issuer"
	terminal.Status.DecidedBySubject = "subject"
	decidedAt := time.Date(2026, 9, 1, 0, 0, 0, 123456700, time.UTC)
	terminal.Status.DecidedAt = &decidedAt
	runtimeapproval.PopulateDecisionAudit(&terminal)

	acknowledged := terminal
	canonicalDecidedAt := decidedAt.Truncate(time.Second)
	acknowledged.Status.DecidedAt = &canonicalDecidedAt
	acknowledged.Status.DecisionAudit = &api.RuntimeApprovalDecisionAudit{}
	*acknowledged.Status.DecisionAudit = *terminal.Status.DecisionAudit
	acknowledged.Status.DecisionAudit.Payload.DecidedAt = canonicalDecidedAt
	acknowledged.Status.DecisionEvidenceDelivered = true

	response := validator.Validate(context.Background(), &Request{Operation: OperationUpdate, Kind: "RuntimeApproval", Namespace: "team-a", Object: encode(acknowledged), OldObject: encode(terminal), UserInfo: UserInfo{Username: gatewayUsername}})
	if !response.Allowed {
		t.Fatalf("audit acknowledgement timestamp canonicalization denied: %s", response.Reason)
	}

	drifted := acknowledged
	drifted.Status.DecisionAudit = &api.RuntimeApprovalDecisionAudit{}
	*drifted.Status.DecisionAudit = *acknowledged.Status.DecisionAudit
	drifted.Status.DecisionAudit.Payload.DecidedAt = canonicalDecidedAt.Add(time.Second)
	if response := validator.Validate(context.Background(), &Request{Operation: OperationUpdate, Kind: "RuntimeApproval", Namespace: "team-a", Object: encode(drifted), OldObject: encode(terminal), UserInfo: UserInfo{Username: gatewayUsername}}); response.Allowed {
		t.Fatal("audit timestamp drift was admitted")
	}
}

type admissionTerminationReader struct {
	proof string
	err   error
}

func (r admissionTerminationReader) RuntimeApprovalTerminationProof(context.Context, api.RuntimeApprovalClaimant) (string, error) {
	return r.proof, r.err
}

type admissionClaimantReader struct{}

func (admissionClaimantReader) VerifyRuntimeApprovalClaimant(context.Context, string, string, string, string, api.RuntimeApprovalClaimant) error {
	return nil
}

func admissionRuntimeApproval() api.RuntimeApproval {
	ref := func(kind, scope, namespace string) api.RuntimeApprovalReference {
		return api.RuntimeApprovalReference{Kind: kind, Scope: scope, Namespace: namespace, Name: strings.ToLower(kind), UID: kind + "-uid", ResourceVersion: 1}
	}
	return api.RuntimeApproval{ObjectMeta: api.ObjectMeta{Name: runtimeapproval.CanonicalName("team-a", "invocation"), Namespace: "team-a", UID: "approval-uid", Finalizers: []string{k8sapi.RuntimeApprovalCompactionFinalizer}}, Spec: api.RuntimeApprovalSpec{
		Workspace: "team-a", InvocationID: "invocation", InitialDecision: api.RuntimeApprovalDecisionPending, Plane: api.RuntimeApprovalPlaneTool, Operation: "invoke", SessionRef: "session",
		AgentRef: ref("Agent", "Namespaced", "team-a"), AgentIdentityRef: ref("AgentIdentity", "Namespaced", "team-a"), AgentInstanceRef: ref("AgentInstance", "Namespaced", "team-a"),
		TargetRef: ref("Tool", "Namespaced", "team-a"), AutonomyClassRef: ref("AutonomyClass", "Cluster", ""),
		Binding: api.RuntimeActionBinding{Epoch: 1},
	}, Status: runtimeapproval.InitialStatus(api.RuntimeApprovalDecisionPending)}
}

func newTimePointer() *time.Time { value := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC); return &value }
