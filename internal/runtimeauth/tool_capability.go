// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeauth

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	// ToolCapabilityAudience identifies the Tool Gateway as the only intended
	// recipient of a runtime tool capability.
	ToolCapabilityAudience = "kognition-tool-gateway"
)

// ToolCapabilityBinding identifies one permitted tool invocation. Every field
// is required so a capability cannot be replayed for another workload, tool,
// verb, or invocation.
type ToolCapabilityBinding struct {
	Namespace           string
	AgentName           string
	AgentIdentityName   string
	InstanceName        string
	InstanceUID         string
	SessionID           string
	Solution            string
	SolutionRev         int64
	ToolName            string
	ToolResourceVersion int64
	Verb                string
	InvocationID        string
	InputDigest         string
}

// ToolCapabilityClaims carries the immutable binding for a Tool Gateway call.
type ToolCapabilityClaims struct {
	Namespace           string `json:"namespace"`
	AgentName           string `json:"agent_name"`
	AgentIdentityName   string `json:"agent_identity_name"`
	InstanceName        string `json:"instance_name"`
	InstanceUID         string `json:"instance_uid"`
	SessionID           string `json:"session_id"`
	Solution            string `json:"solution,omitempty"`
	SolutionRev         int64  `json:"solution_revision,omitempty"`
	ToolName            string `json:"tool_name"`
	ToolResourceVersion int64  `json:"tool_resource_version"`
	Verb                string `json:"verb"`
	InvocationID        string `json:"invocation_id"`
	InputDigest         string `json:"input_digest"`
	jwt.RegisteredClaims
}

// ToolCapabilityIssuer signs and verifies short-lived Tool Gateway
// capabilities. It is safe for concurrent use after construction.
type ToolCapabilityIssuer struct {
	keys *CapabilityKeyRing
	ttl  time.Duration
	now  func() time.Time
}

// NewToolCapabilityIssuer creates a Tool capability issuer. Tool capabilities
// use a distinct audience from memory capabilities even when a deployment
// chooses to provide the same HMAC key material.
func NewToolCapabilityIssuer(key []byte, ttl time.Duration) (*ToolCapabilityIssuer, error) {
	keys, err := NewCapabilityKeyRing(ActiveCapabilityKeyID, key, "", nil)
	if err != nil {
		return nil, err
	}
	return NewToolCapabilityIssuerWithKeyRing(keys, ttl)
}

// NewToolCapabilityIssuerWithKeyRing creates an issuer that signs with the
// active key and accepts the previous key for rotation overlap.
func NewToolCapabilityIssuerWithKeyRing(keys *CapabilityKeyRing, ttl time.Duration) (*ToolCapabilityIssuer, error) {
	if keys == nil {
		return nil, fmt.Errorf("tool capability key ring is required")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("tool capability TTL must be positive")
	}
	return &ToolCapabilityIssuer{keys: keys, ttl: ttl, now: time.Now}, nil
}

// Issue creates one capability for exactly one tool invocation.
func (i *ToolCapabilityIssuer) Issue(binding ToolCapabilityBinding) (string, error) {
	if err := validateToolBinding(binding); err != nil {
		return "", err
	}
	now := i.now().UTC()
	claims := ToolCapabilityClaims{
		Namespace:           binding.Namespace,
		AgentName:           binding.AgentName,
		AgentIdentityName:   binding.AgentIdentityName,
		InstanceName:        binding.InstanceName,
		InstanceUID:         binding.InstanceUID,
		SessionID:           binding.SessionID,
		Solution:            binding.Solution,
		SolutionRev:         binding.SolutionRev,
		ToolName:            binding.ToolName,
		ToolResourceVersion: binding.ToolResourceVersion,
		Verb:                binding.Verb,
		InvocationID:        binding.InvocationID,
		InputDigest:         binding.InputDigest,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        uuid.NewString(),
			Issuer:    memoryCapabilityIssuer,
			Audience:  jwt.ClaimStrings{ToolCapabilityAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(i.ttl)),
		},
	}
	return i.keys.Sign(claims)
}

// Verify verifies a capability and its exact expected invocation binding.
func (i *ToolCapabilityIssuer) Verify(token string, expected ToolCapabilityBinding) (*ToolCapabilityClaims, error) {
	claims, err := i.VerifyClaims(token)
	if err != nil {
		return nil, err
	}
	if expected != toolBindingFromClaims(claims) {
		return nil, ErrCapabilityBindingMismatch
	}
	return claims, nil
}

// VerifyClaims verifies signature, issuer, audience, expiry, and complete
// binding fields before returning the authorization claims.
func (i *ToolCapabilityIssuer) VerifyClaims(token string) (*ToolCapabilityClaims, error) {
	keys, err := i.keys.VerificationKeys(token)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidCapability, err)
	}
	var lastErr error
	for _, key := range keys {
		claims := &ToolCapabilityClaims{}
		parsed, parseErr := jwt.ParseWithClaims(token, claims, func(token *jwt.Token) (any, error) {
			if token.Method != jwt.SigningMethodHS256 {
				return nil, fmt.Errorf("%w: unsupported signing algorithm", ErrInvalidCapability)
			}
			return key, nil
		}, jwt.WithAudience(ToolCapabilityAudience), jwt.WithIssuer(memoryCapabilityIssuer), jwt.WithExpirationRequired(), jwt.WithTimeFunc(i.now))
		if parseErr != nil || !parsed.Valid {
			lastErr = parseErr
			continue
		}
		if claims.ExpiresAt == nil || !claims.ExpiresAt.Time.After(i.now().UTC()) {
			return nil, fmt.Errorf("%w: expired", ErrInvalidCapability)
		}
		if validateErr := validateToolBinding(toolBindingFromClaims(claims)); validateErr != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidCapability, validateErr)
		}
		return claims, nil
	}
	return nil, fmt.Errorf("%w: %w", ErrInvalidCapability, lastErr)
}

func toolBindingFromClaims(claims *ToolCapabilityClaims) ToolCapabilityBinding {
	return ToolCapabilityBinding{
		Namespace:           claims.Namespace,
		AgentName:           claims.AgentName,
		AgentIdentityName:   claims.AgentIdentityName,
		InstanceName:        claims.InstanceName,
		InstanceUID:         claims.InstanceUID,
		SessionID:           claims.SessionID,
		Solution:            claims.Solution,
		SolutionRev:         claims.SolutionRev,
		ToolName:            claims.ToolName,
		ToolResourceVersion: claims.ToolResourceVersion,
		Verb:                claims.Verb,
		InvocationID:        claims.InvocationID,
		InputDigest:         claims.InputDigest,
	}
}

func validateToolBinding(binding ToolCapabilityBinding) error {
	if binding.Namespace == "" || binding.AgentName == "" || binding.AgentIdentityName == "" ||
		binding.InstanceName == "" || binding.InstanceUID == "" || binding.SessionID == "" ||
		binding.ToolName == "" || binding.ToolResourceVersion <= 0 || binding.Verb == "" || binding.InvocationID == "" || binding.InputDigest == "" {
		return fmt.Errorf("runtime tool capability requires every binding field")
	}
	return nil
}
