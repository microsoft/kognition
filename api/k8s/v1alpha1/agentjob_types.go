// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Succeeded",type=integer,JSONPath=`.status.succeeded`
// +kubebuilder:printcolumn:name="Failed",type=integer,JSONPath=`.status.failed`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AgentJob represents a one-shot agent execution.
type AgentJob struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AgentJobSpec   `json:"spec,omitempty"`
	Status            AgentJobStatus `json:"status,omitempty"`
}

// AgentJobSpec defines a one-shot agent task.
type AgentJobSpec struct {
	// AgentRef references an existing Agent resource.
	// +optional
	AgentRef *LocalObjectReference `json:"agentRef,omitempty"`

	// AgentRevision optionally selects an immutable revision of AgentRef for
	// execution while preserving AgentRef as the authorization identity.
	// +optional
	AgentRevision *int64 `json:"agentRevision,omitempty"`

	// AgentSpec allows inline definition of a one-shot agent workload.
	// +optional
	AgentSpec *AgentSpec `json:"agentSpec,omitempty"`

	// CompletionPolicy defines when the job is considered complete.
	// +optional
	// +kubebuilder:validation:Enum=Success;Any
	CompletionPolicy string `json:"completionPolicy,omitempty"`

	// BackoffLimit is the number of retry attempts before marking the job failed.
	// +optional
	BackoffLimit *int32 `json:"backoffLimit,omitempty"`

	// Input is the one-shot task input payload.
	// +optional
	Input string `json:"input,omitempty"`

	// MemoryClaimRef binds this AgentJob to an existing MemoryClaim.
	// +optional
	MemoryClaimRef *LocalObjectReference `json:"memoryClaimRef,omitempty"`
}

// AgentJobStatus describes the observed state of an AgentJob.
type AgentJobStatus struct {
	// Phase is the current lifecycle phase.
	// +optional
	Phase string `json:"phase,omitempty"`

	// Conditions represent the latest available observations.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Succeeded is the number of successful one-shot executions.
	// +optional
	Succeeded int32 `json:"succeeded,omitempty"`

	// Failed is the number of failed one-shot executions.
	// +optional
	Failed int32 `json:"failed,omitempty"`

	// StartTime records when the selected execution attempt started.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime records when the job entered a terminal phase.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// TokenUsage reports token consumption from the successful execution.
	// +optional
	TokenUsage *TokenUsage `json:"tokenUsage,omitempty"`

	// ToolCalls is the number of tool calls made by the successful execution.
	// +optional
	ToolCalls int32 `json:"toolCalls,omitempty"`

	// Output is the final output of a successful execution.
	// +optional
	Output string `json:"output,omitempty"`
}

// +kubebuilder:object:root=true

// AgentJobList contains a list of AgentJob.
type AgentJobList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentJob `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentJob{}, &AgentJobList{})
}
