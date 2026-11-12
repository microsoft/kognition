// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Tool",type=string,JSONPath=`.spec.toolName`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type ToolInvocationRun struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ToolInvocationRunSpec   `json:"spec,omitempty"`
	Status            ToolInvocationRunStatus `json:"status,omitempty"`
}

type ToolInvocationRunSpec struct {
	ToolName            string         `json:"toolName"`
	ToolResourceVersion int64          `json:"toolResourceVersion"`
	InvocationID        string         `json:"invocationID"`
	Input               map[string]any `json:"input,omitempty"`
}

type ToolInvocationRunStatus struct {
	Phase          string             `json:"phase,omitempty"`
	Conditions     []metav1.Condition `json:"conditions,omitempty"`
	Output         map[string]any     `json:"output,omitempty"`
	ErrorCode      string             `json:"errorCode,omitempty"`
	ErrorMessage   string             `json:"errorMessage,omitempty"`
	StartedAt      *metav1.Time       `json:"startedAt,omitempty"`
	CompletedAt    *metav1.Time       `json:"completedAt,omitempty"`
	ExecutionMode  string             `json:"executionMode,omitempty"`
	DurationMillis int64              `json:"durationMillis,omitempty"`
}

// +kubebuilder:object:root=true
type ToolInvocationRunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ToolInvocationRun `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ToolInvocationRun{}, &ToolInvocationRunList{})
}

func (in *ToolInvocationRun) DeepCopyInto(out *ToolInvocationRun) {
	*out = *in
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	if in.Spec.Input != nil {
		out.Spec.Input = runtime.DeepCopyJSON(in.Spec.Input)
	}
	if in.Status.Conditions != nil {
		out.Status.Conditions = make([]metav1.Condition, len(in.Status.Conditions))
		copy(out.Status.Conditions, in.Status.Conditions)
	}
	if in.Status.Output != nil {
		out.Status.Output = runtime.DeepCopyJSON(in.Status.Output)
	}
	if in.Status.StartedAt != nil {
		out.Status.StartedAt = in.Status.StartedAt.DeepCopy()
	}
	if in.Status.CompletedAt != nil {
		out.Status.CompletedAt = in.Status.CompletedAt.DeepCopy()
	}
}

func (in *ToolInvocationRun) DeepCopy() *ToolInvocationRun {
	if in == nil {
		return nil
	}
	out := new(ToolInvocationRun)
	in.DeepCopyInto(out)
	return out
}

func (in *ToolInvocationRun) DeepCopyObject() runtime.Object {
	if copied := in.DeepCopy(); copied != nil {
		return copied
	}
	return nil
}

func (in *ToolInvocationRunList) DeepCopyInto(out *ToolInvocationRunList) {
	*out = *in
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]ToolInvocationRun, len(in.Items))
		for index := range in.Items {
			in.Items[index].DeepCopyInto(&out.Items[index])
		}
	}
}

func (in *ToolInvocationRunList) DeepCopy() *ToolInvocationRunList {
	if in == nil {
		return nil
	}
	out := new(ToolInvocationRunList)
	in.DeepCopyInto(out)
	return out
}

func (in *ToolInvocationRunList) DeepCopyObject() runtime.Object {
	if copied := in.DeepCopy(); copied != nil {
		return copied
	}
	return nil
}
