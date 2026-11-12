// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// +kubebuilder:object:root=true

type CrossNamespaceGrant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              CrossNamespaceGrantSpec `json:"spec"`
}

type CrossNamespaceGrantSpec struct {
	From      string          `json:"from"`
	To        string          `json:"to"`
	Resources []GrantResource `json:"resources"`
}

type GrantResource struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// +kubebuilder:object:root=true

type CrossNamespaceGrantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []CrossNamespaceGrant `json:"items"`
}

func init() {
	SchemeBuilder.Register(&CrossNamespaceGrant{}, &CrossNamespaceGrantList{})
}
