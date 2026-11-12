// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cpi

// wire.go defines the JSON-encoded wire types for the CPI gRPC services
// (Identity, Lifecycle, Extension). These mirror cpi.proto and are used
// with the JSON codec for gRPC transport without protoc code generation.

// --- Identity wire types ---

// GetProviderInfoRequest is the request for Identity.GetProviderInfo.
type GetProviderInfoRequest struct{}

// GetProviderInfoResponse is the response for Identity.GetProviderInfo.
type GetProviderInfoResponse struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	Dimension   string `json:"dimension"`
}

// GetProviderCapabilitiesRequest is the request for Identity.GetProviderCapabilities.
type GetProviderCapabilitiesRequest struct{}

// GetProviderCapabilitiesResponse is the response for Identity.GetProviderCapabilities.
type GetProviderCapabilitiesResponse struct {
	Capabilities []string                       `json:"capabilities"`
	Knowledge    *KnowledgeProviderCapabilities `json:"knowledge,omitempty"`
}

type KnowledgeProviderCapabilities struct {
	ProtocolVersion    string                       `json:"protocol_version"`
	ConfigurationKinds []KnowledgeConfigurationKind `json:"configuration_kinds"`
	Operations         []string                     `json:"operations"`
}

type KnowledgeConfigurationKind struct {
	APIGroup string `json:"api_group"`
	Kind     string `json:"kind"`
}

// --- Lifecycle wire types ---

// ProvisionRequest is the request for Lifecycle.Provision.
type ProvisionRequest struct {
	ResourceKind string            `json:"resource_kind"`
	ResourceName string            `json:"resource_name"`
	Namespace    string            `json:"namespace"`
	Parameters   map[string]string `json:"parameters"`
}

// ProvisionResponse is the response for Lifecycle.Provision.
type ProvisionResponse struct {
	ResourceID     string            `json:"resource_id"`
	ConnectionInfo map[string]string `json:"connection_info"`
}

// DeprovisionRequest is the request for Lifecycle.Deprovision.
type DeprovisionRequest struct {
	ResourceID string `json:"resource_id"`
}

// DeprovisionResponse is the response for Lifecycle.Deprovision.
type DeprovisionResponse struct{}

// UpdateRequest is the request for Lifecycle.Update.
type UpdateRequest struct {
	ResourceID string            `json:"resource_id"`
	Parameters map[string]string `json:"parameters"`
}

// UpdateResponse is the response for Lifecycle.Update.
type UpdateResponse struct{}

// ValidateRequest is the request for Lifecycle.Validate.
type ValidateRequest struct {
	ResourceKind string            `json:"resource_kind"`
	SpecJSON     string            `json:"spec_json"`
	Parameters   map[string]string `json:"parameters"`
}

// ValidateResponse is the response for Lifecycle.Validate.
type ValidateResponse struct {
	Valid  bool   `json:"valid"`
	Reason string `json:"reason"`
}

// HealthCheckRequest is the request for Lifecycle.HealthCheck.
type HealthCheckRequest struct{}

// HealthCheckResponse is the response for Lifecycle.HealthCheck.
type HealthCheckResponse struct {
	Ready   bool   `json:"ready"`
	Message string `json:"message"`
}

// --- Extension wire types ---

// ExtensionCallRequest is the request for Extension.Call.
type ExtensionCallRequest struct {
	Operation  string            `json:"operation"`
	Parameters map[string]string `json:"parameters"`
}

// ExtensionCallResponse is the response for Extension.Call.
type ExtensionCallResponse struct {
	Results map[string]string `json:"results"`
	Error   string            `json:"error"`
}

// ListExtensionOperationsRequest is the request for Extension.ListOperations.
type ListExtensionOperationsRequest struct{}

// ListExtensionOperationsResponse is the response for Extension.ListOperations.
type ListExtensionOperationsResponse struct {
	Operations []ExtensionOperationWire `json:"operations"`
}

// ExtensionOperationWire describes a single extension operation.
type ExtensionOperationWire struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}
