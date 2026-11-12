// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import "time"

// ProviderRegistry tracks registered CPI providers, their dimension,
// capabilities, health status, and connection state.
type ProviderRegistry struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       ProviderRegistrySpec   `json:"spec" yaml:"spec"`
	Status     ProviderRegistryStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

func (p *ProviderRegistry) GetTypeMeta() TypeMeta      { return p.TypeMeta }
func (p *ProviderRegistry) GetObjectMeta() *ObjectMeta { return &p.ObjectMeta }

// ProviderRegistrySpec defines the desired configuration for the registry.
type ProviderRegistrySpec struct {
	// SocketDir is the directory watched for CPI provider socket files.
	SocketDir string `json:"socketDir,omitempty" yaml:"socketDir,omitempty"`

	// HealthCheckInterval is how frequently to health-check providers.
	HealthCheckInterval string `json:"healthCheckInterval,omitempty" yaml:"healthCheckInterval,omitempty"`

	// HealthCheckTimeout is the maximum time to wait for a health response.
	HealthCheckTimeout string `json:"healthCheckTimeout,omitempty" yaml:"healthCheckTimeout,omitempty"`
}

// ProviderRegistryStatus describes the observed state of all registered providers.
type ProviderRegistryStatus struct {
	Phase      Phase           `json:"phase,omitempty" yaml:"phase,omitempty"`
	Providers  []ProviderEntry `json:"providers,omitempty" yaml:"providers,omitempty"`
	Conditions []Condition     `json:"conditions,omitempty" yaml:"conditions,omitempty"`
}

// ProviderEntry describes a single registered CPI provider.
type ProviderEntry struct {
	// Name is the provider name returned by IdentityRPC.GetProviderInfo.
	Name string `json:"name" yaml:"name"`

	// Version is the provider version.
	Version string `json:"version" yaml:"version"`

	// Dimension indicates the provider category: "tool", "memory", "model", or "knowledge".
	Dimension string `json:"dimension" yaml:"dimension"`

	// Capabilities lists provider-supported operations.
	Capabilities []string                       `json:"capabilities,omitempty" yaml:"capabilities,omitempty"`
	Knowledge    *KnowledgeProviderCapabilities `json:"knowledge,omitempty" yaml:"knowledge,omitempty"`

	// SocketPath is the provider's socket file.
	SocketPath string `json:"socketPath,omitempty" yaml:"socketPath,omitempty"`

	// ConnectionState is the current transport state (connected, connecting, disconnected).
	ConnectionState ProviderConnectionState `json:"connectionState" yaml:"connectionState"`

	// HealthStatus describes the provider's health.
	HealthStatus ProviderHealthStatus `json:"healthStatus" yaml:"healthStatus"`

	// RegisteredAt is when the provider first registered.
	RegisteredAt time.Time `json:"registeredAt,omitempty" yaml:"registeredAt,omitempty"`

	// LastHealthCheck is when the last health probe completed.
	LastHealthCheck time.Time `json:"lastHealthCheck,omitempty" yaml:"lastHealthCheck,omitempty"`

	// LastError records the most recent error from the provider.
	LastError string `json:"lastError,omitempty" yaml:"lastError,omitempty"`
}

type KnowledgeProviderCapabilities struct {
	ProtocolVersion    string                       `json:"protocolVersion" yaml:"protocolVersion"`
	ConfigurationKinds []KnowledgeConfigurationKind `json:"configurationKinds" yaml:"configurationKinds"`
	Operations         []string                     `json:"operations" yaml:"operations"`
}

type KnowledgeConfigurationKind struct {
	APIGroup string `json:"apiGroup" yaml:"apiGroup"`
	Kind     string `json:"kind" yaml:"kind"`
}

// ProviderConnectionState represents the transport-level connection state.
type ProviderConnectionState string

const (
	ProviderConnectionConnected    ProviderConnectionState = "Connected"
	ProviderConnectionConnecting   ProviderConnectionState = "Connecting"
	ProviderConnectionDisconnected ProviderConnectionState = "Disconnected"
)

// ProviderHealthStatus describes the health assessment of a provider.
type ProviderHealthStatus string

const (
	ProviderHealthReady       ProviderHealthStatus = "Ready"
	ProviderHealthUnavailable ProviderHealthStatus = "Unavailable"
	ProviderHealthUnknown     ProviderHealthStatus = "Unknown"
)
