// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package convert

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

func ToolInvocationRunToK8s(in *api.ToolInvocationRun) *k8s.ToolInvocationRun {
	if in == nil {
		return nil
	}
	return &k8s.ToolInvocationRun{
		TypeMeta:   metav1.TypeMeta{Kind: "ToolInvocationRun", APIVersion: api.APIVersion},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
		Spec: k8s.ToolInvocationRunSpec{
			ToolName:            in.Spec.ToolName,
			ToolResourceVersion: in.Spec.ToolResourceVersion,
			InvocationID:        in.Spec.InvocationID,
			Input:               cloneJSONMap(in.Spec.Input),
		},
		Status: k8s.ToolInvocationRunStatus{
			Phase:          string(in.Status.Phase),
			Conditions:     conditionsToK8s(in.Status.Conditions),
			Output:         cloneJSONMap(in.Status.Output),
			ErrorCode:      in.Status.ErrorCode,
			ErrorMessage:   in.Status.ErrorMessage,
			StartedAt:      timeToMetav1(in.Status.StartedAt),
			CompletedAt:    timeToMetav1(in.Status.CompletedAt),
			ExecutionMode:  in.Status.ExecutionMode,
			DurationMillis: in.Status.DurationMillis,
		},
	}
}
func ToolInvocationRunFromK8s(in *k8s.ToolInvocationRun) *api.ToolInvocationRun {
	if in == nil {
		return nil
	}
	return &api.ToolInvocationRun{
		TypeMeta:   api.TypeMeta{Kind: in.Kind, APIVersion: in.APIVersion},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec: api.ToolInvocationRunSpec{
			ToolName:            in.Spec.ToolName,
			ToolResourceVersion: in.Spec.ToolResourceVersion,
			InvocationID:        in.Spec.InvocationID,
			Input:               cloneJSONMap(in.Spec.Input),
		},
		Status: api.ToolInvocationRunStatus{
			Phase:          api.Phase(in.Status.Phase),
			Conditions:     conditionsFromK8s(in.Status.Conditions),
			Output:         cloneJSONMap(in.Status.Output),
			ErrorCode:      in.Status.ErrorCode,
			ErrorMessage:   in.Status.ErrorMessage,
			StartedAt:      timeFromMetav1(in.Status.StartedAt),
			CompletedAt:    timeFromMetav1(in.Status.CompletedAt),
			ExecutionMode:  in.Status.ExecutionMode,
			DurationMillis: in.Status.DurationMillis,
		},
	}
}
