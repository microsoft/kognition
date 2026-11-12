// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// KnowledgeBindingRecord permanently preserves the ownership locator for a
// provider backend retained after its KnowledgeBase is deleted.
//
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Workspace",type=string,JSONPath=`.spec.knowledgeBaseNamespace`
// +kubebuilder:printcolumn:name="KnowledgeBase",type=string,JSONPath=`.spec.knowledgeBaseName`
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.binding.providerName`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:validation:XValidation:rule="self.spec == oldSelf.spec",message="KnowledgeBindingRecord spec is immutable"
// +kubebuilder:validation:XValidation:rule="self.spec.reclaimPolicy == 'Retain' && has(self.spec.binding.reclaimPolicy) && self.spec.binding.reclaimPolicy == 'Retain'",message="KnowledgeBindingRecord reclaim policies must be Retain"
type KnowledgeBindingRecord struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              KnowledgeBindingRecordSpec `json:"spec"`
}

type KnowledgeBindingRecordSpec struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	KnowledgeBaseNamespace string `json:"knowledgeBaseNamespace"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	KnowledgeBaseName string `json:"knowledgeBaseName"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	KnowledgeBaseUID string `json:"knowledgeBaseUID"`
	// +kubebuilder:validation:Enum=Retain
	ReclaimPolicy ReclaimPolicy           `json:"reclaimPolicy"`
	Binding       KnowledgeBackendBinding `json:"binding"`
}

// +kubebuilder:object:root=true
type KnowledgeBindingRecordList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KnowledgeBindingRecord `json:"items"`
}

func init() {
	SchemeBuilder.Register(&KnowledgeBindingRecord{}, &KnowledgeBindingRecordList{})
}
