// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cpi

import (
	"fmt"
	"slices"

	v1alpha1 "github.com/kognition-ai/kognition/api/v1alpha1"
)

const KnowledgeProtocolVersion = "kognition.knowledge/v1alpha1"

func knowledgeCapabilitiesFromWire(wire *KnowledgeProviderCapabilities) *v1alpha1.KnowledgeProviderCapabilities {
	if wire == nil {
		return nil
	}
	capabilities := &v1alpha1.KnowledgeProviderCapabilities{
		ProtocolVersion:    wire.ProtocolVersion,
		Operations:         slices.Clone(wire.Operations),
		ConfigurationKinds: make([]v1alpha1.KnowledgeConfigurationKind, len(wire.ConfigurationKinds)),
	}
	for index, kind := range wire.ConfigurationKinds {
		capabilities.ConfigurationKinds[index] = v1alpha1.KnowledgeConfigurationKind{APIGroup: kind.APIGroup, Kind: kind.Kind}
	}
	return capabilities
}

func (h *ProviderHost) ResolveKnowledgeProvider(name string, reference v1alpha1.KnowledgeBackendReference, requiredOperations []string) (*v1alpha1.ProviderEntry, error) {
	if name == "" || reference.APIGroup == "" || reference.Kind == "" || reference.Name == "" {
		return nil, fmt.Errorf("knowledge provider name and complete backend reference are required")
	}
	for _, operation := range requiredOperations {
		if !slices.Contains([]string{"keyword", "dense", "hybrid", "filter", "citations", "refresh", "delete", "retain", "archive"}, operation) {
			return nil, fmt.Errorf("unsupported knowledge operation %q", operation)
		}
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	var selected *v1alpha1.ProviderEntry
	for _, connection := range h.providers {
		connection.mu.RLock()
		entry := cloneProviderEntry(connection.Info)
		connection.mu.RUnlock()
		if entry.Name != name || entry.Dimension != "knowledge" {
			continue
		}
		if selected != nil {
			return nil, fmt.Errorf("knowledge provider %q has ambiguous registrations", name)
		}
		selected = &entry
	}
	if selected == nil || selected.HealthStatus != v1alpha1.ProviderHealthReady || selected.ConnectionState != v1alpha1.ProviderConnectionConnected {
		return nil, fmt.Errorf("knowledge provider %q is unavailable", name)
	}
	capabilities := selected.Knowledge
	if capabilities == nil || capabilities.ProtocolVersion != KnowledgeProtocolVersion {
		return nil, fmt.Errorf("knowledge provider %q does not support protocol %q", name, KnowledgeProtocolVersion)
	}
	kind := v1alpha1.KnowledgeConfigurationKind{APIGroup: reference.APIGroup, Kind: reference.Kind}
	if !slices.Contains(capabilities.ConfigurationKinds, kind) {
		return nil, fmt.Errorf("knowledge provider %q does not support configuration kind %s/%s", name, reference.APIGroup, reference.Kind)
	}
	for _, operation := range requiredOperations {
		if !slices.Contains(capabilities.Operations, operation) {
			return nil, fmt.Errorf("knowledge provider %q does not support operation %q", name, operation)
		}
	}
	return selected, nil
}

func cloneProviderEntry(entry v1alpha1.ProviderEntry) v1alpha1.ProviderEntry {
	entry.Capabilities = slices.Clone(entry.Capabilities)
	if entry.Knowledge != nil {
		capabilities := *entry.Knowledge
		capabilities.ConfigurationKinds = slices.Clone(capabilities.ConfigurationKinds)
		capabilities.Operations = slices.Clone(capabilities.Operations)
		entry.Knowledge = &capabilities
	}
	return entry
}
