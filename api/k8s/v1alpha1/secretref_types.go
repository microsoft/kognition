// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Backend",type=string,JSONPath=`.spec.backendType`
// +kubebuilder:printcolumn:name="Synced",type=boolean,JSONPath=`.status.synced`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// SecretRef references an external secret store entry that the controller
// resolves into runtime credentials for models, tools, and other consumers.
type SecretRef struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SecretRefSpec   `json:"spec,omitempty"`
	Status            SecretRefStatus `json:"status,omitempty"`
}

// SecretRefSpec defines the external secret reference.
type SecretRefSpec struct {
	// BackendType identifies the secret backend (e.g., "internal", "env", "vault", "k8s").
	// +optional
	BackendType string `json:"backendType,omitempty"`

	// BackendRef is the backend-specific reference/path.
	// +optional
	BackendRef string `json:"backendRef,omitempty"`

	// Key is the specific key within the backend reference.
	// +optional
	Key string `json:"key,omitempty"`

	// Provider is a legacy alias for BackendType.
	// +optional
	Provider string `json:"provider,omitempty"`

	// Path is a legacy alias for BackendRef.
	// +optional
	Path string `json:"path,omitempty"`

	// SecretKey is a legacy alias for Key.
	// +optional
	SecretKey string `json:"secretKey,omitempty"`
}

// SecretRefStatus describes the observed state of SecretRef synchronization.
type SecretRefStatus struct {
	// Phase is the lifecycle phase (Pending|Ready|Failed).
	// +optional
	Phase string `json:"phase,omitempty"`

	// Message is a human-readable message describing the current phase.
	// +optional
	Message string `json:"message,omitempty"`

	// Synced indicates the secret material is currently retrievable.
	// +optional
	Synced bool `json:"synced,omitempty"`

	// LastSyncTime is the RFC3339 timestamp of the last successful sync.
	// +optional
	LastSyncTime string `json:"lastSyncTime,omitempty"`

	// Conditions represent the latest available observations.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true

// SecretRefList contains a list of SecretRefs.
type SecretRefList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SecretRef `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SecretRef{}, &SecretRefList{})
}
