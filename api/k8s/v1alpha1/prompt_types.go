// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

type Prompt struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              PromptSpec   `json:"spec"`
	Status            PromptStatus `json:"status,omitempty"`
}

type PromptSpec struct {
	Content      string            `json:"content,omitempty"`
	Parameters   []PromptParameter `json:"parameters,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
	Instructions string            `json:"instructions,omitempty"`
}

type PromptParameter struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Default     string `json:"default,omitempty"`
}

type PromptStatus struct {
	Hash    string `json:"hash,omitempty"`
	Version int64  `json:"version,omitempty"`
}

// +kubebuilder:object:root=true

type PromptList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Prompt `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Prompt{}, &PromptList{})
}
