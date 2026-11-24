// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package convert

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

func RuntimeApprovalToK8s(in *api.RuntimeApproval) *k8s.RuntimeApproval {
	if in == nil {
		return nil
	}
	out := &k8s.RuntimeApproval{
		TypeMeta:   metav1.TypeMeta{Kind: "RuntimeApproval", APIVersion: api.APIVersion},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
		Spec: k8s.RuntimeApprovalSpec{
			ActorKind: in.Spec.ActorKind,
			Workspace: in.Spec.Workspace, InvocationID: in.Spec.InvocationID,
			InitialDecision: in.Spec.InitialDecision,
			Plane:           in.Spec.Plane, Operation: in.Spec.Operation,
			AgentRef:         runtimeApprovalReferenceToK8s(in.Spec.AgentRef),
			AgentIdentityRef: runtimeApprovalReferenceToK8s(in.Spec.AgentIdentityRef),
			AgentInstanceRef: runtimeApprovalReferenceToK8s(in.Spec.AgentInstanceRef),
			SessionRef:       in.Spec.SessionRef,
			TargetRef:        runtimeApprovalReferenceToK8s(in.Spec.TargetRef),
			AutonomyClassRef: runtimeApprovalReferenceToK8s(in.Spec.AutonomyClassRef),
			Binding:          k8s.RuntimeActionBinding(in.Spec.Binding),
			CreatedAt:        metav1.NewTime(in.Spec.CreatedAt), ExpiresAt: metav1.NewTime(in.Spec.ExpiresAt),
		},
		Status: runtimeApprovalStatusToK8s(in.Status),
	}
	if in.Spec.ExecutorAction != nil {
		reference := in.Spec.ExecutorAction
		out.Spec.ExecutorAction = &k8s.WorkflowExecutorActionReference{
			Executor: k8s.WorkflowExecutorPrincipal(reference.Executor), Initiator: k8s.WorkflowExecutorPrincipal(reference.Initiator),
			Workflow: k8s.RuntimeApprovalReference(reference.Workflow), WorkflowRevisionDigest: reference.WorkflowRevisionDigest,
			ExecutionBinding: k8s.RuntimeApprovalReference(reference.ExecutionBinding), Target: k8s.RuntimeApprovalReference(reference.Target),
			Service: k8s.RuntimeApprovalReference(reference.Service), Context: k8s.WorkflowActionContext(reference.Context),
			ExpiresAt: metav1.NewTime(reference.ExpiresAt.UTC()),
		}
	}
	if in.Spec.WorkflowContext != nil {
		value := k8s.WorkflowActionContext(*in.Spec.WorkflowContext)
		out.Spec.WorkflowContext = &value
	}
	if in.Spec.TrustProfileRef != nil {
		value := runtimeApprovalReferenceToK8s(*in.Spec.TrustProfileRef)
		out.Spec.TrustProfileRef = &value
	}
	if in.Spec.AuthorizationRefs != nil {
		out.Spec.AuthorizationRefs = make([]k8s.RuntimeApprovalReference, len(in.Spec.AuthorizationRefs))
		for index := range in.Spec.AuthorizationRefs {
			out.Spec.AuthorizationRefs[index] = runtimeApprovalReferenceToK8s(in.Spec.AuthorizationRefs[index])
		}
	}
	return out
}

func RuntimeApprovalFromK8s(in *k8s.RuntimeApproval) *api.RuntimeApproval {
	if in == nil {
		return nil
	}
	out := &api.RuntimeApproval{
		TypeMeta:   api.TypeMeta{Kind: in.Kind, APIVersion: in.APIVersion},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec: api.RuntimeApprovalSpec{
			ActorKind: in.Spec.ActorKind,
			Workspace: in.Spec.Workspace, InvocationID: in.Spec.InvocationID,
			InitialDecision: in.Spec.InitialDecision,
			Plane:           in.Spec.Plane, Operation: in.Spec.Operation,
			AgentRef:         runtimeApprovalReferenceFromK8s(in.Spec.AgentRef),
			AgentIdentityRef: runtimeApprovalReferenceFromK8s(in.Spec.AgentIdentityRef),
			AgentInstanceRef: runtimeApprovalReferenceFromK8s(in.Spec.AgentInstanceRef),
			SessionRef:       in.Spec.SessionRef,
			TargetRef:        runtimeApprovalReferenceFromK8s(in.Spec.TargetRef),
			AutonomyClassRef: runtimeApprovalReferenceFromK8s(in.Spec.AutonomyClassRef),
			Binding:          api.RuntimeActionBinding(in.Spec.Binding),
			CreatedAt:        in.Spec.CreatedAt.Time, ExpiresAt: in.Spec.ExpiresAt.Time,
		},
		Status: runtimeApprovalStatusFromK8s(in.Status),
	}
	if in.Spec.ExecutorAction != nil {
		reference := in.Spec.ExecutorAction
		out.Spec.ExecutorAction = &api.WorkflowExecutorActionReference{
			Executor: api.ControlPlaneRoleSubject(reference.Executor), Initiator: api.ControlPlaneRoleSubject(reference.Initiator),
			Workflow: api.RuntimeApprovalReference(reference.Workflow), WorkflowRevisionDigest: reference.WorkflowRevisionDigest,
			ExecutionBinding: api.RuntimeApprovalReference(reference.ExecutionBinding), Target: api.RuntimeApprovalReference(reference.Target),
			Service: api.RuntimeApprovalReference(reference.Service), Context: api.WorkflowActionContext(reference.Context), ExpiresAt: reference.ExpiresAt.Time.UTC(),
		}
	}
	if in.Spec.WorkflowContext != nil {
		value := api.WorkflowActionContext(*in.Spec.WorkflowContext)
		out.Spec.WorkflowContext = &value
	}
	if in.Spec.TrustProfileRef != nil {
		value := runtimeApprovalReferenceFromK8s(*in.Spec.TrustProfileRef)
		out.Spec.TrustProfileRef = &value
	}
	if in.Spec.AuthorizationRefs != nil {
		out.Spec.AuthorizationRefs = make([]api.RuntimeApprovalReference, len(in.Spec.AuthorizationRefs))
		for index := range in.Spec.AuthorizationRefs {
			out.Spec.AuthorizationRefs[index] = runtimeApprovalReferenceFromK8s(in.Spec.AuthorizationRefs[index])
		}
	}
	return out
}

func runtimeApprovalReferenceToK8s(in api.RuntimeApprovalReference) k8s.RuntimeApprovalReference {
	return k8s.RuntimeApprovalReference(in)
}

func runtimeApprovalReferenceFromK8s(in k8s.RuntimeApprovalReference) api.RuntimeApprovalReference {
	return api.RuntimeApprovalReference(in)
}

func runtimeApprovalStatusToK8s(in api.RuntimeApprovalStatus) k8s.RuntimeApprovalStatus {
	out := k8s.RuntimeApprovalStatus{
		DecisionState: in.DecisionState, DecisionEvidenceDelivered: in.DecisionEvidenceDelivered,
		DecidedByIssuer: in.DecidedByIssuer, DecidedBySubject: in.DecidedBySubject,
		ExecutionState: in.ExecutionState, ExecutionDisposition: in.ExecutionDisposition,
		PreparationToken: in.PreparationToken, FencingToken: in.FencingToken,
		TerminationProofRef: in.TerminationProofRef, ReconciliationEvidenceRef: in.ReconciliationEvidenceRef,
		ReconciledByIssuer: in.ReconciledByIssuer, ReconciledBySubject: in.ReconciledBySubject,
	}
	if in.DecisionAudit != nil {
		out.DecisionAudit = &k8s.RuntimeApprovalDecisionAudit{
			ID: in.DecisionAudit.ID,
			Payload: k8s.RuntimeApprovalDecisionAuditPayload{
				Workspace: in.DecisionAudit.Payload.Workspace, InvocationID: in.DecisionAudit.Payload.InvocationID,
				ApprovalUID: in.DecisionAudit.Payload.ApprovalUID, Decision: in.DecisionAudit.Payload.Decision,
				Issuer: in.DecisionAudit.Payload.Issuer, Subject: in.DecisionAudit.Payload.Subject,
				DecidedAt: metav1.NewTime(in.DecisionAudit.Payload.DecidedAt),
			},
		}
	}
	out.DecidedAt = timeToMeta(in.DecidedAt)
	out.PreparationDeadline = timeToMeta(in.PreparationDeadline)
	if in.Claimant != nil {
		out.Claimant = &k8s.RuntimeApprovalClaimant{
			WorkloadUID: in.Claimant.WorkloadUID, ContainerID: in.Claimant.ContainerID,
			ProcessStartNonce: in.Claimant.ProcessStartNonce,
			ClaimedAt:         metav1.NewTime(in.Claimant.ClaimedAt), LastHeartbeat: metav1.NewTime(in.Claimant.LastHeartbeat),
			LeaseDeadline: timeToMeta(in.Claimant.LeaseDeadline),
		}
	}
	return out
}

func runtimeApprovalStatusFromK8s(in k8s.RuntimeApprovalStatus) api.RuntimeApprovalStatus {
	out := api.RuntimeApprovalStatus{
		DecisionState: in.DecisionState, DecisionEvidenceDelivered: in.DecisionEvidenceDelivered,
		DecidedByIssuer: in.DecidedByIssuer, DecidedBySubject: in.DecidedBySubject,
		ExecutionState: in.ExecutionState, ExecutionDisposition: in.ExecutionDisposition,
		PreparationToken: in.PreparationToken, FencingToken: in.FencingToken,
		TerminationProofRef: in.TerminationProofRef, ReconciliationEvidenceRef: in.ReconciliationEvidenceRef,
		ReconciledByIssuer: in.ReconciledByIssuer, ReconciledBySubject: in.ReconciledBySubject,
	}
	if in.DecisionAudit != nil {
		out.DecisionAudit = &api.RuntimeApprovalDecisionAudit{
			ID: in.DecisionAudit.ID,
			Payload: api.RuntimeApprovalDecisionAuditPayload{
				Workspace: in.DecisionAudit.Payload.Workspace, InvocationID: in.DecisionAudit.Payload.InvocationID,
				ApprovalUID: in.DecisionAudit.Payload.ApprovalUID, Decision: in.DecisionAudit.Payload.Decision,
				Issuer: in.DecisionAudit.Payload.Issuer, Subject: in.DecisionAudit.Payload.Subject,
				DecidedAt: in.DecisionAudit.Payload.DecidedAt.Time,
			},
		}
	}
	out.DecidedAt = metaToTime(in.DecidedAt)
	out.PreparationDeadline = metaToTime(in.PreparationDeadline)
	if in.Claimant != nil {
		out.Claimant = &api.RuntimeApprovalClaimant{
			WorkloadUID: in.Claimant.WorkloadUID, ContainerID: in.Claimant.ContainerID,
			ProcessStartNonce: in.Claimant.ProcessStartNonce,
			ClaimedAt:         in.Claimant.ClaimedAt.Time, LastHeartbeat: in.Claimant.LastHeartbeat.Time,
			LeaseDeadline: metaToTime(in.Claimant.LeaseDeadline),
		}
	}
	return out
}

func timeToMeta(in *time.Time) *metav1.Time {
	if in == nil {
		return nil
	}
	value := metav1.NewTime(*in)
	return &value
}

func metaToTime(in *metav1.Time) *time.Time {
	if in == nil {
		return nil
	}
	value := in.Time
	return &value
}
