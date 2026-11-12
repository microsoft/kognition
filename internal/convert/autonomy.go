// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package convert

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

// AutonomyClassToK8s converts an internal AutonomyClass to its Kubernetes representation.
func AutonomyClassToK8s(in *api.AutonomyClass) *k8s.AutonomyClass {
	out := &k8s.AutonomyClass{
		TypeMeta: metav1.TypeMeta{
			Kind:       "AutonomyClass",
			APIVersion: "kognition.dev/v1alpha1",
		},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
		Spec: k8s.AutonomyClassSpec{
			Level:            in.Spec.Level,
			RequiresApproval: in.Spec.RequiresApproval,
			MaxCostPerAction: in.Spec.MaxCostPerAction,
			AllowedTools:     cloneStrings(in.Spec.AllowedTools),
		},
	}
	if in.Spec.ApprovalPolicy != nil {
		out.Spec.ApprovalPolicy = &k8s.ApprovalPolicy{
			ToolInvocation:        in.Spec.ApprovalPolicy.ToolInvocation,
			MemoryWrite:           in.Spec.ApprovalPolicy.MemoryWrite,
			ExternalCommunication: in.Spec.ApprovalPolicy.ExternalCommunication,
			CostThreshold:         in.Spec.ApprovalPolicy.CostThreshold,
		}
	}
	if in.Spec.Constraints != nil {
		out.Spec.Constraints = &k8s.AutonomyConstraints{
			MaxConcurrentActions:    in.Spec.Constraints.MaxConcurrentActions,
			MaxTokenBudgetPerAction: in.Spec.Constraints.MaxTokenBudgetPerAction,
		}
	}
	return out
}

// AutonomyClassFromK8s converts a Kubernetes AutonomyClass to its internal representation.
func AutonomyClassFromK8s(in *k8s.AutonomyClass) *api.AutonomyClass {
	out := &api.AutonomyClass{
		TypeMeta: api.TypeMeta{
			Kind:       in.Kind,
			APIVersion: in.APIVersion,
		},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec: api.AutonomyClassSpec{
			Level:            in.Spec.Level,
			RequiresApproval: in.Spec.RequiresApproval,
			MaxCostPerAction: in.Spec.MaxCostPerAction,
			AllowedTools:     cloneStrings(in.Spec.AllowedTools),
		},
	}
	if in.Spec.ApprovalPolicy != nil {
		out.Spec.ApprovalPolicy = &api.ApprovalPolicy{
			ToolInvocation:        in.Spec.ApprovalPolicy.ToolInvocation,
			MemoryWrite:           in.Spec.ApprovalPolicy.MemoryWrite,
			ExternalCommunication: in.Spec.ApprovalPolicy.ExternalCommunication,
			CostThreshold:         in.Spec.ApprovalPolicy.CostThreshold,
		}
	}
	if in.Spec.Constraints != nil {
		out.Spec.Constraints = &api.AutonomyConstraints{
			MaxConcurrentActions:    in.Spec.Constraints.MaxConcurrentActions,
			MaxTokenBudgetPerAction: in.Spec.Constraints.MaxTokenBudgetPerAction,
		}
	}
	return out
}

// AutonomyReviewToK8s converts an internal AutonomyReview to its Kubernetes representation.
func AutonomyReviewToK8s(in *api.AutonomyReview) *k8s.AutonomyReview {
	return &k8s.AutonomyReview{
		TypeMeta:   metav1.TypeMeta{Kind: "AutonomyReview", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
		Spec: k8s.AutonomyReviewSpec{
			AgentRef:          k8s.LocalObjectReference{Name: in.Spec.AgentRef.Name},
			ActionDescription: in.Spec.ActionDescription,
			SessionID:         in.Spec.SessionID,
			CurrentClass:      in.Spec.CurrentClass,
			RequestedClass:    in.Spec.RequestedClass,
			TrustProfileRef:   in.Spec.TrustProfileRef,
			Evidence:          in.Spec.Evidence,
		},
		Status: k8s.AutonomyReviewStatus{
			Decision:  in.Status.Decision,
			DecidedBy: in.Status.DecidedBy, Reason: in.Status.Reason,
			DecidedAt: in.Status.DecidedAt, ExpiresAt: in.Status.ExpiresAt,
		},
	}
}

// AutonomyReviewFromK8s converts a Kubernetes AutonomyReview to its internal representation.
func AutonomyReviewFromK8s(in *k8s.AutonomyReview) *api.AutonomyReview {
	return &api.AutonomyReview{
		TypeMeta:   api.TypeMeta{Kind: in.Kind, APIVersion: in.APIVersion},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec: api.AutonomyReviewSpec{
			AgentRef:          api.LocalObjectReference{Name: in.Spec.AgentRef.Name},
			ActionDescription: in.Spec.ActionDescription,
			SessionID:         in.Spec.SessionID,
			CurrentClass:      in.Spec.CurrentClass,
			RequestedClass:    in.Spec.RequestedClass,
			TrustProfileRef:   in.Spec.TrustProfileRef,
			Evidence:          in.Spec.Evidence,
		},
		Status: api.AutonomyReviewStatus{
			Decision:  in.Status.Decision,
			DecidedBy: in.Status.DecidedBy, Reason: in.Status.Reason,
			DecidedAt: in.Status.DecidedAt, ExpiresAt: in.Status.ExpiresAt,
		},
	}
}

func cloneStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}
