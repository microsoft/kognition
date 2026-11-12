// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.spec.agentRef.name`
// +kubebuilder:printcolumn:name="CurrentModel",type=string,JSONPath=`.status.currentModelRef`
// +kubebuilder:printcolumn:name="DesiredModel",type=string,JSONPath=`.status.desiredModelRef`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ModelAutoscaler defines vertical model-tier scaling rules for an agent.
type ModelAutoscaler struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ModelAutoscalerSpec   `json:"spec,omitempty"`
	Status            ModelAutoscalerStatus `json:"status,omitempty"`
}

// ModelAutoscalerSpec defines fallback and recovery behavior for model-tier scaling.
type ModelAutoscalerSpec struct {
	// AgentRef references the Agent whose modelRef may be adjusted.
	AgentRef LocalObjectReference `json:"agentRef"`

	// PreferredModelRef is the primary Model the agent should use when healthy.
	PreferredModelRef LocalObjectReference `json:"preferredModelRef"`

	// StabilizationWindowSeconds is the minimum time between model switches.
	// +optional
	StabilizationWindowSeconds int32 `json:"stabilizationWindowSeconds,omitempty"`
}

// ModelAutoscalerStatus describes the current vertical scaling state.
type ModelAutoscalerStatus struct {
	// CurrentModelRef is the model currently configured on the target agent.
	// +optional
	CurrentModelRef string `json:"currentModelRef,omitempty"`

	// DesiredModelRef is the model selected by the autoscaler decision.
	// +optional
	DesiredModelRef string `json:"desiredModelRef,omitempty"`

	// LastScaleTime is the last time the autoscaler changed the model binding.
	// +optional
	LastScaleTime *metav1.Time `json:"lastScaleTime,omitempty"`

	// Conditions represent the latest available observations.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true

// ModelAutoscalerList contains a list of ModelAutoscalers.
type ModelAutoscalerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelAutoscaler `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ModelAutoscaler{}, &ModelAutoscalerList{})
}
