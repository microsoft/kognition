// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// TrustProfileIndex records the one selected TrustProfile for an Agent
// incarnation. The controller creates it only after the selected profile is
// durable and attaches an Agent owner reference for orphan collection.
//
// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Agent UID",type=string,JSONPath=`.spec.agentUID`
// +kubebuilder:printcolumn:name="Profile",type=string,JSONPath=`.spec.selectedProfileName`
// +kubebuilder:printcolumn:name="Profile UID",type=string,JSONPath=`.spec.selectedProfileUID`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.spec.agentUID == oldSelf.spec.agentUID",message="TrustProfileIndex agentUID is immutable"
type TrustProfileIndex struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              TrustProfileIndexSpec `json:"spec"`
}

type TrustProfileIndexSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	AgentUID string `json:"agentUID"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	SelectedProfileName string `json:"selectedProfileName"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	SelectedProfileUID string `json:"selectedProfileUID"`
}

// +kubebuilder:object:root=true
type TrustProfileIndexList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TrustProfileIndex `json:"items"`
}

func init() {
	SchemeBuilder.Register(&TrustProfileIndex{}, &TrustProfileIndexList{})
}
