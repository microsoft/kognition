// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Provisioner",type=string,JSONPath=`.spec.provisioner`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ModelClass defines a class of LLM providers (cluster-scoped).
type ModelClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ModelClassSpec   `json:"spec,omitempty"`
	Status            ModelClassStatus `json:"status,omitempty"`
}

// ModelClassSpec defines the provisioner configuration for model access.
type ModelClassSpec struct {
	// Provisioner is the CPI provider name (e.g., "openai", "azure-openai", "anthropic").
	Provisioner string `json:"provisioner"`

	// Parameters are provider-specific configuration.
	// +optional
	Parameters map[string]string `json:"parameters,omitempty"`

	// LatencyClass categorises expected response latency ("low", "medium", "high").
	// +optional
	LatencyClass string `json:"latencyClass,omitempty"`

	// CostTier indicates relative cost ("low", "medium", "high", "premium").
	// +optional
	CostTier string `json:"costTier,omitempty"`

	// ContextWindow is the maximum token context window.
	// +optional
	ContextWindow int64 `json:"contextWindow,omitempty"`

	// Modalities lists supported modalities (e.g., ["text", "image", "audio"]).
	// +optional
	Modalities []string `json:"modalities,omitempty"`

	// HealthProbe configures the model health probe.
	// +optional
	HealthProbe *ModelHealthProbeSpec `json:"healthProbe,omitempty"`

	// VersionPin pins the model to a specific version.
	// +optional
	VersionPin        string                         `json:"versionPin,omitempty"`
	VersionCandidates map[string]ModelVersionBinding `json:"versionCandidates,omitempty"`
}

type ModelVersionBinding struct {
	Namespace               string `json:"namespace"`
	ModelName               string `json:"modelName"`
	ModelUID                string `json:"modelUID"`
	ModelResourceVersion    int64  `json:"modelResourceVersion"`
	SpecDigest              string `json:"specDigest"`
	InvocationProfileUID    string `json:"invocationProfileUID,omitempty"`
	InvocationProfileDigest string `json:"invocationProfileDigest,omitempty"`
}

// ModelHealthProbeSpec defines how to probe model health.
type ModelHealthProbeSpec struct {
	// Endpoint is the health probe endpoint.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// Type is the probe type: "inference-ping" (default), "latency-threshold" or
	// "error-rate-threshold". Other values are treated as "inference-ping".
	// +optional
	Type string `json:"type,omitempty"`

	// IntervalSeconds is the probe interval.
	// +optional
	// +kubebuilder:default=60
	IntervalSeconds int32 `json:"intervalSeconds,omitempty"`

	// TimeoutSeconds is the probe timeout.
	// +optional
	// +kubebuilder:default=10
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`

	// LatencyThresholdMs is the latency threshold in milliseconds.
	// +optional
	LatencyThresholdMs int64 `json:"latencyThresholdMs,omitempty"`

	// ErrorRateThreshold is the error rate percentage (0-100) at which an
	// "error-rate-threshold" probe degrades the class. It is measured over the
	// provider failures of recent real model calls to the class's Models, needs at
	// least 5 calls and defaults to 50 when unset.
	// +optional
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	ErrorRateThreshold int32 `json:"errorRateThreshold,omitempty"`

	// FailureThreshold is the number of failures before marking unhealthy.
	// +optional
	// +kubebuilder:default=3
	FailureThreshold int32 `json:"failureThreshold,omitempty"`

	// SuccessThreshold is the number of successes before marking healthy again.
	// +optional
	// +kubebuilder:default=1
	SuccessThreshold int32 `json:"successThreshold,omitempty"`
}

// ModelClassStatus describes the observed state of a ModelClass.
type ModelClassStatus struct {
	// Phase is the current lifecycle phase.
	// +optional
	Phase string `json:"phase,omitempty"`

	// Conditions represent the latest available observations.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Provisioned indicates the model endpoint is reachable.
	// +optional
	Provisioned bool `json:"provisioned,omitempty"`

	// ActiveVersion is the currently active model version.
	// +optional
	ActiveVersion string               `json:"activeVersion,omitempty"`
	ActiveBinding *ModelVersionBinding `json:"activeBinding,omitempty"`

	// Health is the model health status ("Healthy", "Degraded", "Unknown").
	// +optional
	Health string `json:"health,omitempty"`

	// FallbackClass is the ModelClass to fall back to if this one is unhealthy.
	// +optional
	FallbackClass string `json:"fallbackClass,omitempty"`

	// LastProbeTime is the last time the health probe ran.
	// +optional
	LastProbeTime string `json:"lastProbeTime,omitempty"`

	// ConsecutiveFailures is the number of consecutive probe failures.
	// +optional
	ConsecutiveFailures int32 `json:"consecutiveFailures,omitempty"`

	// ConsecutiveSuccesses is the number of consecutive probe successes.
	// +optional
	ConsecutiveSuccesses int32 `json:"consecutiveSuccesses,omitempty"`
}

// +kubebuilder:object:root=true

// ModelClassList contains a list of ModelClasses.
type ModelClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelClass `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.provider`
// +kubebuilder:printcolumn:name="Class",type=string,JSONPath=`.spec.className`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Model declares an LLM or embedding model available for inference.
type Model struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ModelSpec   `json:"spec,omitempty"`
	Status            ModelStatus `json:"status,omitempty"`
}

// ModelSpec defines the desired state of a Model.
type ModelSpec struct {
	// Provider identifies the model backend provider.
	// Supported values include "openai-compatible", "azure-openai", and "foundry".
	// +optional
	Provider string `json:"provider,omitempty"`

	// ClassName references the ModelClass provisioner.
	ClassName string `json:"className"`

	// ModelID is the provider-specific model identifier (e.g., "gpt-4o", "claude-3.5-sonnet").
	ModelID string `json:"modelID"`

	// Endpoint is the provider endpoint used for health checks and inference routing.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// Capabilities describes supported workloads (chat, embedding, tool-calling, vision).
	// +optional
	Capabilities []string `json:"capabilities,omitempty"`

	// CredentialRef references the SecretRef containing API credentials.
	// +optional
	CredentialRef *LocalObjectReference `json:"credentialRef,omitempty"`

	// RateLimit is the provider call budget (requests per minute).
	// +optional
	RateLimit *int64 `json:"rateLimit,omitempty"`

	// Parameters are model-specific settings (temperature, max_tokens, apiVersion, etc.).
	// +optional
	Parameters map[string]string `json:"parameters,omitempty"`

	// Signature is a cosign-compatible supply-chain signature for verification.
	// +optional
	Signature string `json:"signature,omitempty"`

	// +optional
	InvocationProfileRef *ModelInvocationProfileReference `json:"invocationProfileRef,omitempty"`
	// +optional
	// +kubebuilder:validation:MaxLength=128
	InvocationContract string `json:"invocationContract,omitempty"`
}

type ModelInvocationProfileReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	SpecDigest string `json:"specDigest"`
}

// ModelStatus describes the observed state of a Model.
type ModelStatus struct {
	// Phase is the current lifecycle phase.
	// +optional
	Phase string `json:"phase,omitempty"`

	// Conditions represent the latest available observations.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Available indicates the model endpoint is reachable.
	// +optional
	Available bool `json:"available,omitempty"`

	// Latency is the last observed latency string (e.g., "120ms").
	// +optional
	Latency string `json:"latency,omitempty"`

	// LastCheck is the RFC3339 timestamp of the last health check.
	// +optional
	LastCheck string `json:"lastCheck,omitempty"`

	// Capabilities lists capabilities confirmed by the provider.
	// +optional
	Capabilities []string `json:"capabilities,omitempty"`
}

// +kubebuilder:object:root=true

// ModelList contains a list of Models.
type ModelList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Model `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.spec.agentRef.name`
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.spec.modelRef.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ModelGrant authorizes an agent to use a model.
type ModelGrant struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ModelGrantSpec `json:"spec"`
}

// ModelGrantSpec defines which agent can use which model.
type ModelGrantSpec struct {
	AgentRef LocalObjectReference `json:"agentRef"`
	ModelRef LocalObjectReference `json:"modelRef"`
}

// +kubebuilder:object:root=true

// ModelGrantList contains a list of ModelGrants.
type ModelGrantList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ModelGrant `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ModelClass{}, &ModelClassList{})
	SchemeBuilder.Register(&Model{}, &ModelList{})
	SchemeBuilder.Register(&ModelGrant{}, &ModelGrantList{})
}
