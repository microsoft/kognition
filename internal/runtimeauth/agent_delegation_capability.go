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
	// AgentDelegationCapabilityAudience identifies the Agent Communication
	// Gateway as the only intended recipient of this capability.
	AgentDelegationCapabilityAudience = "kognition-agent-communication-gateway"
)

// AgentDelegationCapabilityBinding identifies one permitted delegation from an
// active source runtime session to a resolved target AgentService. Every field
// is required so a capability cannot be replayed for another source session,
// workspace, service, Agent, or invocation.
type AgentDelegationCapabilityBinding struct {
	Namespace          string
	AgentName          string
	AgentIdentityName  string
	InstanceName       string
	InstanceUID        string
	SessionID          string
	Solution           string
	SolutionRev        int64
	TargetNamespace    string
	TargetAgentService string
	TargetAgent        string
	InvocationID       string
	MessageDigest      string
}

// AgentDelegationCapabilityClaims carries the immutable binding for one Agent
// Communication Gateway delegation request.
type AgentDelegationCapabilityClaims struct {
	Namespace          string `json:"namespace"`
	AgentName          string `json:"agent_name"`
	AgentIdentityName  string `json:"agent_identity_name"`
	InstanceName       string `json:"instance_name"`
	InstanceUID        string `json:"instance_uid"`
	SessionID          string `json:"session_id"`
	Solution           string `json:"solution,omitempty"`
	SolutionRev        int64  `json:"solution_revision,omitempty"`
	TargetNamespace    string `json:"target_namespace"`
	TargetAgentService string `json:"target_agent_service"`
	TargetAgent        string `json:"target_agent"`
	InvocationID       string `json:"invocation_id"`
	MessageDigest      string `json:"message_digest"`
	jwt.RegisteredClaims
}

// AgentDelegationCapabilityIssuer signs and verifies short-lived Agent
// Communication Gateway capabilities. It is safe for concurrent use after
// construction.
type AgentDelegationCapabilityIssuer struct {
	keys *CapabilityKeyRing
	ttl  time.Duration
	now  func() time.Time
}

// NewAgentDelegationCapabilityIssuer creates an issuer using one active HMAC
// key. Deployments may use the same key material as other capability issuers,
// but the audience remains distinct to prevent confused-deputy use.
func NewAgentDelegationCapabilityIssuer(key []byte, ttl time.Duration) (*AgentDelegationCapabilityIssuer, error) {
	keys, err := NewCapabilityKeyRing(ActiveCapabilityKeyID, key, "", nil)
	if err != nil {
		return nil, err
	}
	return NewAgentDelegationCapabilityIssuerWithKeyRing(keys, ttl)
}

// NewAgentDelegationCapabilityIssuerWithKeyRing creates an issuer that signs
// with the active key and accepts the previous key for rotation overlap.
func NewAgentDelegationCapabilityIssuerWithKeyRing(keys *CapabilityKeyRing, ttl time.Duration) (*AgentDelegationCapabilityIssuer, error) {
	if keys == nil {
		return nil, fmt.Errorf("agent delegation capability key ring is required")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("agent delegation capability TTL must be positive")
	}
	return &AgentDelegationCapabilityIssuer{keys: keys, ttl: ttl, now: time.Now}, nil
}

// Issue creates one capability for exactly one resolved AgentService delegation.
func (i *AgentDelegationCapabilityIssuer) Issue(binding AgentDelegationCapabilityBinding) (string, error) {
	if err := validateAgentDelegationBinding(binding); err != nil {
		return "", err
	}
	now := i.now().UTC()
	claims := AgentDelegationCapabilityClaims{
		Namespace:          binding.Namespace,
		AgentName:          binding.AgentName,
		AgentIdentityName:  binding.AgentIdentityName,
		InstanceName:       binding.InstanceName,
		InstanceUID:        binding.InstanceUID,
		SessionID:          binding.SessionID,
		Solution:           binding.Solution,
		SolutionRev:        binding.SolutionRev,
		TargetNamespace:    binding.TargetNamespace,
		TargetAgentService: binding.TargetAgentService,
		TargetAgent:        binding.TargetAgent,
		InvocationID:       binding.InvocationID,
		MessageDigest:      binding.MessageDigest,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        uuid.NewString(),
			Issuer:    memoryCapabilityIssuer,
			Audience:  jwt.ClaimStrings{AgentDelegationCapabilityAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(i.ttl)),
		},
	}
	return i.keys.Sign(claims)
}

// Verify verifies a capability and its exact expected delegation binding.
func (i *AgentDelegationCapabilityIssuer) Verify(token string, expected AgentDelegationCapabilityBinding) (*AgentDelegationCapabilityClaims, error) {
	claims, err := i.VerifyClaims(token)
	if err != nil {
		return nil, err
	}
	if expected != agentDelegationBindingFromClaims(claims) {
		return nil, ErrCapabilityBindingMismatch
	}
	return claims, nil
}

// VerifyClaims verifies signature, issuer, audience, expiry, and complete
// binding fields before returning the delegation claims.
func (i *AgentDelegationCapabilityIssuer) VerifyClaims(token string) (*AgentDelegationCapabilityClaims, error) {
	keys, err := i.keys.VerificationKeys(token)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidCapability, err)
	}
	var lastErr error
	for _, key := range keys {
		claims := &AgentDelegationCapabilityClaims{}
		parsed, parseErr := jwt.ParseWithClaims(token, claims, func(token *jwt.Token) (any, error) {
			if token.Method != jwt.SigningMethodHS256 {
				return nil, fmt.Errorf("%w: unsupported signing algorithm", ErrInvalidCapability)
			}
			return key, nil
		}, jwt.WithAudience(AgentDelegationCapabilityAudience), jwt.WithIssuer(memoryCapabilityIssuer), jwt.WithExpirationRequired(), jwt.WithTimeFunc(i.now))
		if parseErr != nil || !parsed.Valid {
			lastErr = parseErr
			continue
		}
		if claims.ExpiresAt == nil || !claims.ExpiresAt.Time.After(i.now().UTC()) {
			return nil, fmt.Errorf("%w: expired", ErrInvalidCapability)
		}
		if validateErr := validateAgentDelegationBinding(agentDelegationBindingFromClaims(claims)); validateErr != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidCapability, validateErr)
		}
		return claims, nil
	}
	return nil, fmt.Errorf("%w: %w", ErrInvalidCapability, lastErr)
}

func agentDelegationBindingFromClaims(claims *AgentDelegationCapabilityClaims) AgentDelegationCapabilityBinding {
	return AgentDelegationCapabilityBinding{
		Namespace:          claims.Namespace,
		AgentName:          claims.AgentName,
		AgentIdentityName:  claims.AgentIdentityName,
		InstanceName:       claims.InstanceName,
		InstanceUID:        claims.InstanceUID,
		SessionID:          claims.SessionID,
		Solution:           claims.Solution,
		SolutionRev:        claims.SolutionRev,
		TargetNamespace:    claims.TargetNamespace,
		TargetAgentService: claims.TargetAgentService,
		TargetAgent:        claims.TargetAgent,
		InvocationID:       claims.InvocationID,
		MessageDigest:      claims.MessageDigest,
	}
}

func validateAgentDelegationBinding(binding AgentDelegationCapabilityBinding) error {
	if binding.Namespace == "" || binding.AgentName == "" || binding.AgentIdentityName == "" ||
		binding.InstanceName == "" || binding.InstanceUID == "" || binding.SessionID == "" ||
		binding.TargetNamespace == "" || binding.TargetAgentService == "" || binding.TargetAgent == "" || binding.InvocationID == "" || binding.MessageDigest == "" {
		return fmt.Errorf("agent delegation capability requires every binding field")
	}
	return nil
}
