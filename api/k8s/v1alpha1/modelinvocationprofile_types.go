// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

// +kubebuilder:object:root=true
// +kubebuilder:printcolumn:name="Operation",type=string,JSONPath=`.spec.operation`
// +kubebuilder:validation:XValidation:rule="self.spec == oldSelf.spec",message="invocation profile spec is immutable; create a new revision"
type ModelInvocationProfile struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              api.ModelInvocationProfileSpec `json:"spec"`
}

// +kubebuilder:object:root=true
type ModelInvocationProfileList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelInvocationProfile `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ModelInvocationProfile{}, &ModelInvocationProfileList{})
}
