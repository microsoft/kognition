// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

type AgentService struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AgentServiceSpec   `json:"spec,omitempty"`
	Status            AgentServiceStatus `json:"status,omitempty"`
}

type AgentServiceSpec struct {
	AgentRef              LocalObjectReference `json:"agentRef"`
	RevisionPin           *int64               `json:"revisionPin,omitempty"`
	LoadBalancingStrategy string               `json:"loadBalancingStrategy,omitempty"`
	Selector              map[string]string    `json:"selector,omitempty"`
	Port                  int32                `json:"port,omitempty"`
}

type AgentServiceStatus struct {
	Phase          string             `json:"phase,omitempty"`
	Endpoint       string             `json:"endpoint,omitempty"`
	ActiveRevision int64              `json:"activeRevision,omitempty"`
	ReadyInstances int32              `json:"readyInstances,omitempty"`
	Conditions     []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true

type AgentServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentService `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

type IntentGateway struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              IntentGatewaySpec   `json:"spec,omitempty"`
	Status            IntentGatewayStatus `json:"status,omitempty"`
}

type IntentGatewaySpec struct {
	Protocol   string                   `json:"protocol,omitempty"`
	AuthConfig *IntentGatewayAuthConfig `json:"authConfig,omitempty"`
	Routes     []IntentRoute            `json:"routes"`
}

type IntentGatewayAuthConfig struct {
	Mode            string                `json:"mode,omitempty"`
	APIKeySecretRef *LocalObjectReference `json:"apiKeySecretRef,omitempty"`
	Header          string                `json:"header,omitempty"`
}

type IntentRoute struct {
	Pattern    string               `json:"pattern"`
	TargetRef  LocalObjectReference `json:"targetRef"`
	TargetKind string               `json:"targetKind"`
}

type IntentGatewayStatus struct {
	Phase      string             `json:"phase,omitempty"`
	Endpoint   string             `json:"endpoint,omitempty"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true

type IntentGatewayList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []IntentGateway `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentService{}, &AgentServiceList{}, &IntentGateway{}, &IntentGatewayList{})
}
