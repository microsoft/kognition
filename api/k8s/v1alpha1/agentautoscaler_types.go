// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.spec.agentRef.name`
// +kubebuilder:printcolumn:name="Current",type=integer,JSONPath=`.status.currentReplicas`
// +kubebuilder:printcolumn:name="Desired",type=integer,JSONPath=`.status.desiredReplicas`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

type AgentAutoscaler struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AgentAutoscalerSpec   `json:"spec,omitempty"`
	Status            AgentAutoscalerStatus `json:"status,omitempty"`
}

type AgentAutoscalerSpec struct {
	AgentRef        LocalObjectReference `json:"agentRef"`
	MinReplicas     int32                `json:"minReplicas"`
	MaxReplicas     int32                `json:"maxReplicas"`
	Metrics         []AutoscalerMetric   `json:"metrics"`
	ScaleUpPolicy   *ScalingPolicy       `json:"scaleUpPolicy,omitempty"`
	ScaleDownPolicy *ScalingPolicy       `json:"scaleDownPolicy,omitempty"`
}

type MetricType string

const (
	MetricQueueDepth MetricType = "QueueDepth"
	MetricP95Latency MetricType = "P95Latency"
)

type AutoscalerMetric struct {
	Type        MetricType `json:"type"`
	TargetValue int64      `json:"targetValue"`
}

type ScalingPolicy struct {
	StabilizationWindowSeconds int32 `json:"stabilizationWindowSeconds"`
}

type AgentAutoscalerStatus struct {
	CurrentReplicas int32              `json:"currentReplicas,omitempty"`
	DesiredReplicas int32              `json:"desiredReplicas,omitempty"`
	LastScaleTime   *metav1.Time       `json:"lastScaleTime,omitempty"`
	Conditions      []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true

type AgentAutoscalerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentAutoscaler `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentAutoscaler{}, &AgentAutoscalerList{})
}
