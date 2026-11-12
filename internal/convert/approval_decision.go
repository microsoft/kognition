// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package convert

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

func ApprovalDecisionToK8s(in *api.ApprovalDecision) *k8s.ApprovalDecision {
	if in == nil {
		return nil
	}
	out := &k8s.ApprovalDecision{
		TypeMeta: metav1.TypeMeta{
			Kind:       "ApprovalDecision",
			APIVersion: "kognition.dev/v1alpha1",
		},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
		Spec: k8s.ApprovalDecisionSpec{
			WorkflowRunRef: localRefToK8s(in.Spec.WorkflowRunRef.Name),
			Step:           in.Spec.Step,
			Iteration:      in.Spec.Iteration,
			Decision:       in.Spec.Decision,
			Approver:       in.Spec.Approver,
			Reason:         in.Spec.Reason,
		},
		Status: k8s.ApprovalDecisionStatus{
			Observed: in.Status.Observed,
		},
	}
	if in.Status.ObservedAt != nil {
		t := metav1.NewTime(*in.Status.ObservedAt)
		out.Status.ObservedAt = &t
	}
	return out
}

func ApprovalDecisionFromK8s(in *k8s.ApprovalDecision) *api.ApprovalDecision {
	if in == nil {
		return nil
	}
	out := &api.ApprovalDecision{
		TypeMeta: api.TypeMeta{
			Kind:       in.Kind,
			APIVersion: in.APIVersion,
		},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec: api.ApprovalDecisionSpec{
			WorkflowRunRef: api.LocalObjectReference{Name: in.Spec.WorkflowRunRef.Name},
			Step:           in.Spec.Step,
			Iteration:      in.Spec.Iteration,
			Decision:       in.Spec.Decision,
			Approver:       in.Spec.Approver,
			Reason:         in.Spec.Reason,
		},
		Status: api.ApprovalDecisionStatus{
			Observed: in.Status.Observed,
		},
	}
	if in.Status.ObservedAt != nil {
		t := in.Status.ObservedAt.Time
		out.Status.ObservedAt = &t
	}
	return out
}
