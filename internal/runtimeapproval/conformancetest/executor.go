// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package conformancetest

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

func ExecutorApprovalProposal(t *testing.T, keys runtimeapproval.BindingKeyStore, invocation string) (api.RuntimeApproval, json.RawMessage) {
	t.Helper()
	now := time.Now().UTC()
	reference := func(kind, name, uid string) api.RuntimeApprovalReference {
		return api.RuntimeApprovalReference{Kind: kind, Scope: "Namespaced", Namespace: "team-a", Name: name, UID: uid, ResourceVersion: 1}
	}
	payload := json.RawMessage(`{"goal":"bounded investigation"}`)
	action := runtimeapproval.WorkflowExecutorAction{
		Executor:  api.ControlPlaneRoleSubject{Method: "mtls", Issuer: "executor-ca", Subject: "workflow-executor"},
		Initiator: api.ControlPlaneRoleSubject{Method: "oidc", Issuer: "https://issuer.example", Subject: "submitter"},
		Workflow:  reference("Workflow", "investigate", "workflow-uid"), WorkflowRevisionDigest: strings.Repeat("a", 64),
		ExecutionBinding: reference("WorkflowExecutionBinding", "approved", "binding-uid"),
		Target:           reference("Agent", "analyst", "agent-uid"), Service: reference("AgentService", "analyst", "service-uid"),
		Activation: store.WorkflowActivationKey{Workspace: "team-a", RunUID: "run-uid", Step: "team", ActivationID: "visit-1", Attempt: 1},
		OwnerEpoch: 1, BindingDigest: strings.Repeat("b", 64), InvocationID: invocation, Operation: runtimeapproval.WorkflowExecutorInitiate,
		ExpiresAt: now.Add(15 * time.Minute), Payload: payload,
	}
	approval, err := runtimeapproval.BuildWorkflowExecutorApproval(context.Background(), keys, action, api.RuntimeApprovalDecisionPending, now)
	if err != nil {
		t.Fatal(err)
	}
	return approval, payload
}

func RunExecutorApprovalContract(t *testing.T, storage store.RuntimeApprovalExecutionStore, keys runtimeapproval.BindingKeyStore) (api.RuntimeApproval, json.RawMessage, api.RuntimeApproval) {
	t.Helper()
	ctx := context.Background()
	proposed, payload := ExecutorApprovalProposal(t, keys, "executor-contract")
	created, approval, err := storage.CreateOrGetRuntimeApproval(ctx, proposed, payload, time.Hour)
	if err != nil || !created {
		t.Fatalf("create executor approval: created=%v error=%v", created, err)
	}
	if approval.Spec.ActorKind != api.RuntimeApprovalActorExecutor || approval.Spec.AgentRef != (api.RuntimeApprovalReference{}) ||
		approval.Spec.AgentIdentityRef != (api.RuntimeApprovalReference{}) || approval.Spec.AgentInstanceRef != (api.RuntimeApprovalReference{}) || approval.Spec.SessionRef != "" {
		t.Fatal("executor approval fabricated an Agent identity or session")
	}
	if !reflect.DeepEqual(approval.Spec.ExecutorAction, proposed.Spec.ExecutorAction) || approval.Spec.ExpiresAt.After(proposed.Spec.ExecutorAction.ExpiresAt) {
		t.Fatal("executor identity changed or approval extended the authority deadline")
	}
	created, replay, err := storage.CreateOrGetRuntimeApproval(ctx, proposed, payload, time.Hour)
	if err != nil || created || replay.UID != approval.UID {
		t.Fatalf("executor replay created a second owner: created=%v error=%v", created, err)
	}
	changed := proposed
	changedReference := *proposed.Spec.ExecutorAction
	changedReference.Executor.Subject = "replacement-executor"
	changed.Spec.ExecutorAction = &changedReference
	if _, _, err := storage.CreateOrGetRuntimeApproval(ctx, changed, payload, time.Hour); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("changed executor gained the old decision: %v", err)
	}
	approved, err := storage.DecideRuntimeApproval(ctx, approval.Spec.Workspace, approval.Spec.InvocationID, store.RuntimeApprovalDecision{
		Decision: api.RuntimeApprovalDecisionApproved, ExpectedResourceVersion: approval.ResourceVersion, Issuer: "reviewer-issuer", Subject: "reviewer"})
	if err != nil || approved.Status.DecisionAudit == nil {
		t.Fatalf("executor exact-action decision missing durable evidence: %v", err)
	}
	approved, err = storage.AcknowledgeRuntimeApprovalDecisionAudit(ctx, approved.Spec.Workspace, approved.Spec.InvocationID,
		approved.ResourceVersion, approved.Status.DecisionAudit.ID)
	if err != nil {
		t.Fatal(err)
	}
	token, err := runtimeapproval.GeneratePreparationToken()
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := storage.PrepareRuntimeApproval(ctx, approved.Spec.Workspace, approved.Spec.InvocationID, approved.ResourceVersion,
		token, time.Now().UTC().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	claimTime := time.Now().UTC()
	claimant := api.RuntimeApprovalClaimant{WorkloadUID: "executor-gateway-uid", ProcessStartNonce: "executor-process-0001", ClaimedAt: claimTime, LastHeartbeat: claimTime}
	claimed, err := storage.ClaimRuntimeApprovalExecution(ctx, prepared.Spec.Workspace, prepared.Spec.InvocationID,
		prepared.ResourceVersion, token, claimant)
	if err != nil || claimed.Status.ExecutionState != api.RuntimeApprovalExecutionClaimed || claimed.Status.Claimant == nil {
		t.Fatalf("executor claim failed: %v", err)
	}
	otherClaimant := claimant
	otherClaimant.WorkloadUID, otherClaimant.ProcessStartNonce = "other-gateway", "executor-process-0002"
	if _, err := storage.ClaimRuntimeApprovalExecution(ctx, prepared.Spec.Workspace, prepared.Spec.InvocationID,
		prepared.ResourceVersion, token, otherClaimant); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("second process acquired the exact executor effect: %v", err)
	}
	terminal, err := storage.FinalizeRuntimeApprovalExecution(ctx, claimed.Spec.Workspace, claimed.Spec.InvocationID,
		claimed.ResourceVersion, claimed.Status.FencingToken, runtimeapproval.ExecutionDispositionSucceeded)
	if err != nil || terminal.Status.ExecutionState != api.RuntimeApprovalExecutionCompleted {
		t.Fatalf("executor exact owner could not settle: %v", err)
	}
	created, replay, err = storage.CreateOrGetRuntimeApproval(ctx, proposed, payload, time.Hour)
	if err != nil || created || replay.Status.ExecutionState != api.RuntimeApprovalExecutionCompleted {
		t.Fatalf("settled executor action reopened: created=%v error=%v", created, err)
	}
	return proposed, payload, terminal
}
