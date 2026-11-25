// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

// ModelClass defines a class of model provisioner.
type ModelClass struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       ModelClassSpec   `json:"spec" yaml:"spec"`
	Status     ModelClassStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

func (m *ModelClass) GetTypeMeta() TypeMeta      { return m.TypeMeta }
func (m *ModelClass) GetObjectMeta() *ObjectMeta { return &m.ObjectMeta }

// ModelClassSpec defines the provisioner for a class of models.
type ModelClassSpec struct {
	// Provisioner is the CPI provider name (e.g., "openai", "anthropic", "ollama").
	Provisioner string `json:"provisioner" yaml:"provisioner"`

	// Parameters are provisioner-specific configuration. Cost-aware ModelClasses
	// may set cost.inputMicrousdPerMillionTokens and
	// cost.outputMicrousdPerMillionTokens to emit normalized micro-USD cost tags.
	Parameters map[string]string `json:"parameters,omitempty" yaml:"parameters,omitempty"`

	// LatencyClass classifies the expected latency tier (e.g., "low", "medium", "high").
	LatencyClass string `json:"latencyClass,omitempty" yaml:"latencyClass,omitempty"`

	// CostTier classifies the cost tier (e.g., "free", "standard", "premium").
	CostTier string `json:"costTier,omitempty" yaml:"costTier,omitempty"`

	// ContextWindow is the maximum context window size in tokens.
	ContextWindow int64 `json:"contextWindow,omitempty" yaml:"contextWindow,omitempty"`

	// Modalities lists supported modalities (e.g., "text", "vision", "audio").
	Modalities []string `json:"modalities,omitempty" yaml:"modalities,omitempty"`

	// HealthProbe defines the health check endpoint for the provisioner.
	HealthProbe *HealthProbeSpec `json:"healthProbe,omitempty" yaml:"healthProbe,omitempty"`

	// VersionPin pins the model class to a specific version.
	VersionPin        string                         `json:"versionPin,omitempty" yaml:"versionPin,omitempty"`
	VersionCandidates map[string]ModelVersionBinding `json:"versionCandidates,omitempty" yaml:"versionCandidates,omitempty"`
}

type ModelVersionBinding struct {
	Namespace               string `json:"namespace" yaml:"namespace"`
	ModelName               string `json:"modelName" yaml:"modelName"`
	ModelUID                string `json:"modelUID" yaml:"modelUID"`
	ModelResourceVersion    int64  `json:"modelResourceVersion" yaml:"modelResourceVersion"`
	SpecDigest              string `json:"specDigest" yaml:"specDigest"`
	InvocationProfileUID    string `json:"invocationProfileUID,omitempty" yaml:"invocationProfileUID,omitempty"`
	InvocationProfileDigest string `json:"invocationProfileDigest,omitempty" yaml:"invocationProfileDigest,omitempty"`
}

// HealthProbeSpec defines health probe configuration for a model provisioner.
type HealthProbeSpec struct {
	// Endpoint is the URL to probe for health.
	Endpoint string `json:"endpoint" yaml:"endpoint"`

	// IntervalSeconds is the probe interval in seconds.
	IntervalSeconds int32 `json:"intervalSeconds,omitempty" yaml:"intervalSeconds,omitempty"`

	// TimeoutSeconds is the probe timeout in seconds.
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty" yaml:"timeoutSeconds,omitempty"`

	// Type is the probe type: "inference-ping", "latency-threshold", or "error-rate-threshold".
	// Defaults to "inference-ping" if unset.
	Type string `json:"type,omitempty" yaml:"type,omitempty"`

	// LatencyThresholdMs is the maximum acceptable latency in milliseconds.
	// Used when Type is "latency-threshold".
	LatencyThresholdMs int64 `json:"latencyThresholdMs,omitempty" yaml:"latencyThresholdMs,omitempty"`

	// ErrorRateThreshold is the error rate percentage (0-100) at which an
	// "error-rate-threshold" probe degrades the class. It is measured over the
	// provider failures of recent real model calls to the class's Models, needs at
	// least 5 calls and defaults to 50 when unset.
	ErrorRateThreshold int32 `json:"errorRateThreshold,omitempty" yaml:"errorRateThreshold,omitempty"`

	// FailureThreshold is the number of consecutive probe failures before marking Degraded.
	// Defaults to 3.
	FailureThreshold int32 `json:"failureThreshold,omitempty" yaml:"failureThreshold,omitempty"`

	// SuccessThreshold is the number of consecutive successes required for recovery (stabilisation window).
	// Defaults to 3.
	SuccessThreshold int32 `json:"successThreshold,omitempty" yaml:"successThreshold,omitempty"`
}

// ModelClassStatus describes the observed state of a ModelClass.
type ModelClassStatus struct {
	// Phase is the current lifecycle phase.
	Phase Phase `json:"phase,omitempty" yaml:"phase,omitempty"`

	// Conditions describe the current state of the ModelClass.
	Conditions []Condition `json:"conditions,omitempty" yaml:"conditions,omitempty"`

	// Provisioned indicates whether the provisioner is successfully registered.
	Provisioned bool `json:"provisioned,omitempty" yaml:"provisioned,omitempty"`

	// ActiveVersion is the currently active model version.
	ActiveVersion string               `json:"activeVersion,omitempty" yaml:"activeVersion,omitempty"`
	ActiveBinding *ModelVersionBinding `json:"activeBinding,omitempty" yaml:"activeBinding,omitempty"`

	// Health is the observed health state ("Healthy", "Degraded", "Unknown").
	Health string `json:"health,omitempty" yaml:"health,omitempty"`

	// LastProbeTime is the RFC3339 timestamp of the last health probe execution.
	LastProbeTime string `json:"lastProbeTime,omitempty" yaml:"lastProbeTime,omitempty"`

	// ConsecutiveFailures tracks consecutive probe failures.
	ConsecutiveFailures int32 `json:"consecutiveFailures,omitempty" yaml:"consecutiveFailures,omitempty"`

	// ConsecutiveSuccesses tracks consecutive probe successes (for stabilisation window).
	ConsecutiveSuccesses int32 `json:"consecutiveSuccesses,omitempty" yaml:"consecutiveSuccesses,omitempty"`

	// FallbackClass is the name of the fallback ModelClass selected on degradation.
	FallbackClass string `json:"fallbackClass,omitempty" yaml:"fallbackClass,omitempty"`
}

// Model declares an LLM or embedding model available for inference.
type Model struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       ModelSpec   `json:"spec" yaml:"spec"`
	Status     ModelStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

func (m *Model) GetTypeMeta() TypeMeta      { return m.TypeMeta }
func (m *Model) GetObjectMeta() *ObjectMeta { return &m.ObjectMeta }

// ModelSpec defines the desired state of a Model.
type ModelSpec struct {
	// Provider identifies the model backend provider.
	// Supported values include "openai-compatible", "azure-openai", and "foundry".
	Provider string `json:"provider,omitempty" yaml:"provider,omitempty"`

	// ClassName references the ModelClass provisioner.
	ClassName string `json:"className" yaml:"className"`

	// ModelID is the provider-specific model identifier (e.g., "gpt-4o", "claude-3.5-sonnet").
	ModelID string `json:"modelID" yaml:"modelID"`

	// Endpoint is the provider endpoint used for health checks and inference routing.
	Endpoint string `json:"endpoint,omitempty" yaml:"endpoint,omitempty"`

	// Capabilities describes supported workloads (chat, embedding, tool-calling, vision).
	Capabilities []string `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`

	// CredentialRef references the secret containing API credentials.
	CredentialRef *LocalObjectReference `json:"credentialRef,omitempty" yaml:"credentialRef,omitempty"`

	// RateLimit is the provider call budget (requests per minute).
	RateLimit *int64 `json:"rateLimit,omitempty" yaml:"rateLimit,omitempty"`

	// Parameters are model-specific settings (temperature, max_tokens, etc.).
	Parameters map[string]string `json:"parameters,omitempty" yaml:"parameters,omitempty"`

	// InvocationProfileRef selects namespace-local portable invocation intent.
	InvocationProfileRef *ModelInvocationProfileReference `json:"invocationProfileRef,omitempty" yaml:"invocationProfileRef,omitempty"`
	// InvocationContract selects an explicit versioned contract implemented by CPI.
	InvocationContract string `json:"invocationContract,omitempty" yaml:"invocationContract,omitempty"`

	// Signature is a cosign-compatible supply-chain signature for verification.
	Signature string `json:"signature,omitempty" yaml:"signature,omitempty"`
}

// ModelStatus describes the observed state of a Model.
type ModelStatus struct {
	Phase        Phase       `json:"phase,omitempty" yaml:"phase,omitempty"`
	Conditions   []Condition `json:"conditions,omitempty" yaml:"conditions,omitempty"`
	Available    bool        `json:"available,omitempty" yaml:"available,omitempty"`
	Latency      string      `json:"latency,omitempty" yaml:"latency,omitempty"`
	LastCheck    string      `json:"lastCheck,omitempty" yaml:"lastCheck,omitempty"`
	Capabilities []string    `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
}

// ModelGrant authorizes an agent to use a model.
type ModelGrant struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       ModelGrantSpec `json:"spec" yaml:"spec"`
}

func (m *ModelGrant) GetTypeMeta() TypeMeta      { return m.TypeMeta }
func (m *ModelGrant) GetObjectMeta() *ObjectMeta { return &m.ObjectMeta }

// ModelGrantSpec defines which agent can use which model.
type ModelGrantSpec struct {
	AgentRef LocalObjectReference `json:"agentRef" yaml:"agentRef"`
	ModelRef LocalObjectReference `json:"modelRef" yaml:"modelRef"`
}
