// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	"fmt"
	"math"
)

const MaxInvocationCompletionTokens int64 = 1<<31 - 1

// ModelInvocationProfile declares portable invocation intent, not a provider dialect.
type ModelInvocationProfile struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       ModelInvocationProfileSpec `json:"spec" yaml:"spec"`
}

func (p *ModelInvocationProfile) GetTypeMeta() TypeMeta      { return p.TypeMeta }
func (p *ModelInvocationProfile) GetObjectMeta() *ObjectMeta { return &p.ObjectMeta }

type ModelInvocationProfileSpec struct {
	// +kubebuilder:validation:Enum=chat;embedding
	Operation string `json:"operation" yaml:"operation"`
	// +optional
	Defaults InvocationDefaults `json:"defaults,omitempty" yaml:"defaults,omitempty"`
	// +optional
	Requirements InvocationRequirements `json:"requirements,omitempty" yaml:"requirements,omitempty"`
	// +optional
	Limits InvocationLimits `json:"limits,omitempty" yaml:"limits,omitempty"`
	// +optional
	ParameterHandling InvocationParameterHandling `json:"parameterHandling,omitempty" yaml:"parameterHandling,omitempty"`
}

type InvocationDefaults struct {
	// MaxCompletionTokens bounds one model completion, including counted reasoning.
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=2147483647
	MaxCompletionTokens *int64 `json:"maxCompletionTokens,omitempty" yaml:"maxCompletionTokens,omitempty"`
	// Omitted Temperature means provider-default sampling.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=2
	Temperature *float64 `json:"temperature,omitempty" yaml:"temperature,omitempty"`
	// +optional
	// +kubebuilder:validation:Enum=providerDefault;disabled;required
	Reasoning string `json:"reasoning,omitempty" yaml:"reasoning,omitempty"`
	// +optional
	// +kubebuilder:validation:Minimum=1
	Dimensions *int64 `json:"dimensions,omitempty" yaml:"dimensions,omitempty"`
}

type InvocationRequirements struct {
	// +optional
	// +kubebuilder:validation:Enum=optional;required;forbidden
	ToolCalling string `json:"toolCalling,omitempty" yaml:"toolCalling,omitempty"`
	// +optional
	// +kubebuilder:validation:Enum=optional;required;forbidden
	StructuredOutput string `json:"structuredOutput,omitempty" yaml:"structuredOutput,omitempty"`
	// +optional
	// +kubebuilder:validation:Enum=disabled;required
	Reasoning string `json:"reasoning,omitempty" yaml:"reasoning,omitempty"`
}

type InvocationLimits struct {
	// +optional
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=2147483647
	MaxCompletionTokens *int64 `json:"maxCompletionTokens,omitempty" yaml:"maxCompletionTokens,omitempty"`
}

type InvocationParameterHandling struct {
	// +optional
	// +kubebuilder:validation:Enum=Reject
	Unsupported string `json:"unsupported,omitempty" yaml:"unsupported,omitempty"`
	// +optional
	// +kubebuilder:validation:Enum=Reject
	Unknown string `json:"unknown,omitempty" yaml:"unknown,omitempty"`
}

type ModelInvocationProfileBinding struct {
	Name       string `json:"name" yaml:"name"`
	UID        string `json:"uid" yaml:"uid"`
	SpecDigest string `json:"specDigest" yaml:"specDigest"`
	Contract   string `json:"contract" yaml:"contract"`
}

type ModelInvocationProfileReference struct {
	Name string `json:"name" yaml:"name"`
	// SpecDigest pins reviewed intent independently of profile-name reuse.
	SpecDigest string `json:"specDigest" yaml:"specDigest"`
}

func (s ModelInvocationProfileSpec) Validate() error {
	if s.Operation != "chat" && s.Operation != "embedding" {
		return fmt.Errorf("invocation profile operation must be chat or embedding")
	}
	for _, mode := range []string{s.ParameterHandling.Unknown, s.ParameterHandling.Unsupported} {
		if mode != "" && mode != "Reject" {
			return fmt.Errorf("invocation profile parameter handling must be Reject")
		}
	}
	for _, requirement := range []string{s.Requirements.ToolCalling, s.Requirements.StructuredOutput} {
		if requirement != "" && requirement != "optional" && requirement != "required" && requirement != "forbidden" {
			return fmt.Errorf("invocation feature requirements must be optional, required or forbidden")
		}
	}
	if mode := s.Defaults.Reasoning; mode != "" && mode != "providerDefault" && mode != "disabled" && mode != "required" {
		return fmt.Errorf("invocation default reasoning must be providerDefault, disabled or required")
	}
	if mode := s.Requirements.Reasoning; mode != "" && mode != "disabled" && mode != "required" {
		return fmt.Errorf("invocation required reasoning must be disabled or required")
	}
	if s.Defaults.Reasoning != "" && s.Defaults.Reasoning != "providerDefault" && s.Requirements.Reasoning != "" && s.Defaults.Reasoning != s.Requirements.Reasoning {
		return fmt.Errorf("invocation reasoning default conflicts with its requirement")
	}
	if s.Operation == "embedding" {
		if s.Defaults.MaxCompletionTokens != nil || s.Defaults.Temperature != nil || s.Defaults.Reasoning != "" ||
			s.Limits.MaxCompletionTokens != nil || s.Requirements != (InvocationRequirements{}) {
			return fmt.Errorf("embedding invocation profiles cannot contain chat settings")
		}
		if s.Defaults.Dimensions != nil && (*s.Defaults.Dimensions < 1 || *s.Defaults.Dimensions > 65536) {
			return fmt.Errorf("embedding dimensions must be between 1 and 65536")
		}
		return nil
	}
	if s.Defaults.Dimensions != nil {
		return fmt.Errorf("chat invocation profiles cannot set embedding dimensions")
	}
	if s.Defaults.MaxCompletionTokens == nil || *s.Defaults.MaxCompletionTokens < 1 || *s.Defaults.MaxCompletionTokens > MaxInvocationCompletionTokens {
		return fmt.Errorf("chat invocation profiles require maxCompletionTokens between 1 and %d", MaxInvocationCompletionTokens)
	}
	if s.Limits.MaxCompletionTokens != nil && (*s.Limits.MaxCompletionTokens < 1 || *s.Limits.MaxCompletionTokens > MaxInvocationCompletionTokens || *s.Defaults.MaxCompletionTokens > *s.Limits.MaxCompletionTokens) {
		return fmt.Errorf("invocation completion default exceeds its positive hard limit")
	}
	if s.Defaults.Temperature != nil && (math.IsNaN(*s.Defaults.Temperature) || math.IsInf(*s.Defaults.Temperature, 0) || *s.Defaults.Temperature < 0 || *s.Defaults.Temperature > 2) {
		return fmt.Errorf("invocation temperature must be finite and between 0 and 2")
	}
	return nil
}

func (s *ModelInvocationProfileSpec) DeepCopyInto(out *ModelInvocationProfileSpec) {
	*out = *s
	if s.Defaults.MaxCompletionTokens != nil {
		value := *s.Defaults.MaxCompletionTokens
		out.Defaults.MaxCompletionTokens = &value
	}
	if s.Defaults.Temperature != nil {
		value := *s.Defaults.Temperature
		out.Defaults.Temperature = &value
	}
	if s.Defaults.Dimensions != nil {
		value := *s.Defaults.Dimensions
		out.Defaults.Dimensions = &value
	}
	if s.Limits.MaxCompletionTokens != nil {
		value := *s.Limits.MaxCompletionTokens
		out.Limits.MaxCompletionTokens = &value
	}
}
