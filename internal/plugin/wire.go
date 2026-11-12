// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package plugin

// wire.go defines the JSON-encoded wire types for the Plugin gRPC service.
// gRPC supports custom codecs; we use JSON encoding over the gRPC transport
// to avoid requiring protoc-generated code in the MVP. When protoc code
// generation is added (US-0065), these types will be replaced by generated stubs.

import (
	"encoding/json"

	"google.golang.org/grpc/encoding"
)

func init() {
	encoding.RegisterCodec(JSONCodec{})
}

// JSONCodec implements grpc encoding.Codec using JSON serialization.
// This allows gRPC services to use plain Go structs without proto generation.
type JSONCodec struct{}

func (JSONCodec) Marshal(v interface{}) ([]byte, error)      { return json.Marshal(v) }
func (JSONCodec) Unmarshal(data []byte, v interface{}) error { return json.Unmarshal(data, v) }
func (JSONCodec) Name() string                               { return "json" }

// --- Request/Response wire types (mirror plugin.proto) ---

// Empty is a zero-field message for RPCs that take no parameters.
type Empty struct{}

// GetPluginInfoResponse mirrors plugin.proto GetPluginInfoResponse.
type GetPluginInfoResponse struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	Vendor      string `json:"vendor"`
}

// GetPluginCapabilitiesResponse mirrors plugin.proto.
type GetPluginCapabilitiesResponse struct {
	Capabilities []PluginCapabilityWire `json:"capabilities"`
}

// PluginCapabilityWire mirrors the proto PluginCapability message.
type PluginCapabilityWire struct {
	Type int `json:"type"`
}

// PluginProbeResponse mirrors plugin.proto PluginProbeResponse.
type PluginProbeResponse struct {
	Ready bool `json:"ready"`
}

// NotifyRegisteredRequest mirrors plugin.proto NotifyRegisteredRequest.
type NotifyRegisteredRequest struct {
	HostVersion string `json:"host_version"`
}

// NotifyRegisteredResponse mirrors plugin.proto NotifyRegisteredResponse.
type NotifyRegisteredResponse struct{}

// capabilityTypeFromWire converts wire integer to CapabilityType.
func capabilityTypeFromWire(wire int) CapabilityType {
	switch wire {
	case 1:
		return CapabilityRuntime
	case 2:
		return CapabilityMemory
	case 3:
		return CapabilityTool
	case 4:
		return CapabilityModel
	case 5:
		return CapabilityKnowledge
	default:
		return CapabilityUnspecified
	}
}
