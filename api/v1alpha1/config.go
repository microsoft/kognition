// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	"fmt"
	"time"
)

var supportedSecretRefBackends = map[string]struct{}{
	"internal": {},
	"env":      {},
}

// Prompt stores reusable agent instructions.
type Prompt struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       PromptSpec   `json:"spec" yaml:"spec"`
	Status     PromptStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

func (p *Prompt) GetTypeMeta() TypeMeta      { return p.TypeMeta }
func (p *Prompt) GetObjectMeta() *ObjectMeta { return &p.ObjectMeta }

// PromptSpec defines the prompt content.
type PromptSpec struct {
	// Content is the canonical prompt body.
	Content string `json:"content,omitempty" yaml:"content,omitempty"`

	// Parameters defines placeholders configurable at render time.
	Parameters []PromptParameter `json:"parameters,omitempty" yaml:"parameters,omitempty"`

	// Metadata carries user-defined prompt metadata.
	Metadata map[string]string `json:"metadata,omitempty" yaml:"metadata,omitempty"`

	// Instructions is kept as a legacy alias for backward compatibility.
	Instructions string `json:"instructions,omitempty" yaml:"instructions,omitempty"`
}

// ResolvedContent returns the effective prompt body.
//
// Precedence is deterministic for mixed payloads:
// 1. spec.content
// 2. spec.instructions (legacy alias)
func (p PromptSpec) ResolvedContent() string {
	if p.Content != "" {
		return p.Content
	}
	return p.Instructions
}

// PromptParameter defines a named input variable for a prompt template.
type PromptParameter struct {
	Name        string `json:"name" yaml:"name"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
	Required    bool   `json:"required,omitempty" yaml:"required,omitempty"`
	Default     string `json:"default,omitempty" yaml:"default,omitempty"`
}

// PromptStatus describes the observed state of a Prompt.
type PromptStatus struct {
	Hash    string `json:"hash,omitempty" yaml:"hash,omitempty"`
	Version int64  `json:"version,omitempty" yaml:"version,omitempty"`
}

// SecretRef references an external secret store entry.
type SecretRef struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       SecretRefSpec   `json:"spec" yaml:"spec"`
	Status     SecretRefStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

func (s *SecretRef) GetTypeMeta() TypeMeta      { return s.TypeMeta }
func (s *SecretRef) GetObjectMeta() *ObjectMeta { return &s.ObjectMeta }

// SecretRefSpec defines the external secret reference.
type SecretRefSpec struct {
	// BackendType identifies the secret backend (e.g., "internal", "env", "vault").
	BackendType string `json:"backendType,omitempty" yaml:"backendType,omitempty"`

	// BackendRef is the backend-specific reference/path.
	BackendRef string `json:"backendRef,omitempty" yaml:"backendRef,omitempty"`

	// Key is the specific key within the backend reference.
	Key string `json:"key,omitempty" yaml:"key,omitempty"`

	// Provider is a legacy alias for BackendType.
	Provider string `json:"provider,omitempty" yaml:"provider,omitempty"`

	// Path is a legacy alias for BackendRef.
	Path string `json:"path,omitempty" yaml:"path,omitempty"`

	// LegacyKey is a compatibility alias for key.
	LegacyKey string `json:"secretKey,omitempty" yaml:"secretKey,omitempty"`
}

// ResolvedBackendType returns canonical backend type with legacy fallback.
func (s SecretRefSpec) ResolvedBackendType() string {
	if s.BackendType != "" {
		return s.BackendType
	}
	return s.Provider
}

// ResolvedBackendRef returns canonical backend reference with legacy fallback.
func (s SecretRefSpec) ResolvedBackendRef() string {
	if s.BackendRef != "" {
		return s.BackendRef
	}
	return s.Path
}

// ResolvedKey returns canonical key with legacy fallback.
func (s SecretRefSpec) ResolvedKey() string {
	if s.Key != "" {
		return s.Key
	}
	return s.LegacyKey
}

// Validate ensures required resolved SecretRef fields are present.
func (s SecretRefSpec) Validate() error {
	backendType := s.ResolvedBackendType()
	if backendType == "" {
		return fmt.Errorf("secretRef.spec.backendType is required")
	}
	if s.ResolvedBackendRef() == "" {
		return fmt.Errorf("secretRef.spec.backendRef is required")
	}
	if s.ResolvedKey() == "" {
		return fmt.Errorf("secretRef.spec.key is required")
	}
	if _, ok := supportedSecretRefBackends[backendType]; !ok {
		return fmt.Errorf("secretRef.spec.backendType %q is not supported", backendType)
	}
	return nil
}

// SecretRefStatus describes the observed state of SecretRef synchronization.
type SecretRefStatus struct {
	Phase        Phase       `json:"phase,omitempty" yaml:"phase,omitempty"`
	Message      string      `json:"message,omitempty" yaml:"message,omitempty"`
	Synced       bool        `json:"synced,omitempty" yaml:"synced,omitempty"`
	LastSyncTime *time.Time  `json:"lastSyncTime,omitempty" yaml:"lastSyncTime,omitempty"`
	Conditions   []Condition `json:"conditions,omitempty" yaml:"conditions,omitempty"`
}
