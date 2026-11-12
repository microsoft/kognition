// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

type Solution struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SolutionSpec   `json:"spec"`
	Status            SolutionStatus `json:"status,omitempty"`
}

type SolutionSpec struct {
	Goal        string                `json:"goal"`
	Target      *SolutionTarget       `json:"target,omitempty"`
	Constraints []string              `json:"constraints,omitempty"`
	Preferences map[string]string     `json:"preferences,omitempty"`
	Seed        *int64                `json:"seed,omitempty"`
	ModelRef    *LocalObjectReference `json:"modelRef,omitempty"`
	Overrides   *SolutionOverrides    `json:"overrides,omitempty"`
	// +optional
	PromotionGates []SolutionPromotionGate `json:"promotionGates,omitempty"`
}

type SolutionPromotionGate struct {
	EvaluationRef LocalObjectReference `json:"evaluationRef"`
}

type SolutionTarget struct {
	Kind string               `json:"kind"`
	Ref  LocalObjectReference `json:"ref"`
}

type SolutionOverrides struct {
	TokenBudget *TokenBudget     `json:"tokenBudget,omitempty"`
	Hard        map[string]int64 `json:"hard,omitempty"`
}

type SolutionStatus struct {
	Phase              string              `json:"phase,omitempty"`
	Conditions         []metav1.Condition  `json:"conditions,omitempty"`
	ActiveRevision     int64               `json:"activeRevision,omitempty"`
	CandidateRevision  int64               `json:"candidateRevision,omitempty"`
	CompiledResources  []CompiledResource  `json:"compiledResources,omitempty"`
	GeneratedResources []GeneratedResource `json:"generatedResources,omitempty"`
}

type CompiledResource struct {
	Kind      string          `json:"kind"`
	Name      string          `json:"name"`
	Namespace string          `json:"namespace,omitempty"`
	Manifest  json.RawMessage `json:"manifest"`
}

type GeneratedResource struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// +kubebuilder:object:root=true

type SolutionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Solution `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Solution{}, &SolutionList{})
}
