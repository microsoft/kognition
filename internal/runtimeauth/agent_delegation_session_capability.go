// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeauth

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const AgentDelegationSessionCapabilityAudience = "kognition-agent-communication-gateway-session"

// AgentDelegationSessionCapabilityBinding identifies one active source runtime
// session. It is non-executable and may only be exchanged at the Agent
// Communication Gateway for an exact target-bound delegation capability.
type AgentDelegationSessionCapabilityBinding struct {
	Namespace         string
	AgentName         string
	AgentIdentityName string
	InstanceName      string
	InstanceUID       string
	SessionID         string
	Solution          string
	SolutionRev       int64
}

// AgentDelegationSessionCapabilityClaims carries the immutable source-session
// binding used by the Agent Communication Gateway capability exchange.
type AgentDelegationSessionCapabilityClaims struct {
	Namespace         string `json:"namespace"`
	AgentName         string `json:"agent_name"`
	AgentIdentityName string `json:"agent_identity_name"`
	InstanceName      string `json:"instance_name"`
	InstanceUID       string `json:"instance_uid"`
	SessionID         string `json:"session_id"`
	Solution          string `json:"solution,omitempty"`
	SolutionRev       int64  `json:"solution_revision,omitempty"`
	jwt.RegisteredClaims
}

// AgentDelegationSessionCapabilityIssuer signs and verifies source runtime
// session capabilities for the Agent Communication Gateway exchange endpoint.
type AgentDelegationSessionCapabilityIssuer struct {
	keys *CapabilityKeyRing
	ttl  time.Duration
	now  func() time.Time
}

func NewAgentDelegationSessionCapabilityIssuer(key []byte, ttl time.Duration) (*AgentDelegationSessionCapabilityIssuer, error) {
	keys, err := NewCapabilityKeyRing(ActiveCapabilityKeyID, key, "", nil)
	if err != nil {
		return nil, err
	}
	return NewAgentDelegationSessionCapabilityIssuerWithKeyRing(keys, ttl)
}

func NewAgentDelegationSessionCapabilityIssuerWithKeyRing(keys *CapabilityKeyRing, ttl time.Duration) (*AgentDelegationSessionCapabilityIssuer, error) {
	if keys == nil {
		return nil, fmt.Errorf("agent delegation session capability key ring is required")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("agent delegation session capability TTL must be positive")
	}
	return &AgentDelegationSessionCapabilityIssuer{keys: keys, ttl: ttl, now: time.Now}, nil
}

// TTL returns the lifetime assigned to each source-session capability.
func (i *AgentDelegationSessionCapabilityIssuer) TTL() time.Duration {
	return i.ttl
}

func (i *AgentDelegationSessionCapabilityIssuer) Issue(binding AgentDelegationSessionCapabilityBinding) (string, error) {
	if err := validateAgentDelegationSessionBinding(binding); err != nil {
		return "", err
	}
	now := i.now().UTC()
	claims := AgentDelegationSessionCapabilityClaims{
		Namespace: binding.Namespace, AgentName: binding.AgentName, AgentIdentityName: binding.AgentIdentityName,
		InstanceName: binding.InstanceName, InstanceUID: binding.InstanceUID, SessionID: binding.SessionID,
		Solution: binding.Solution, SolutionRev: binding.SolutionRev,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        uuid.NewString(),
			Issuer:    memoryCapabilityIssuer,
			Audience:  jwt.ClaimStrings{AgentDelegationSessionCapabilityAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(i.ttl)),
		},
	}
	return i.keys.Sign(claims)
}

func (i *AgentDelegationSessionCapabilityIssuer) VerifyClaims(token string) (*AgentDelegationSessionCapabilityClaims, error) {
	keys, err := i.keys.VerificationKeys(token)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidCapability, err)
	}
	var lastErr error
	for _, key := range keys {
		claims := &AgentDelegationSessionCapabilityClaims{}
		parsed, parseErr := jwt.ParseWithClaims(token, claims, func(token *jwt.Token) (any, error) {
			if token.Method != jwt.SigningMethodHS256 {
				return nil, fmt.Errorf("%w: unsupported signing algorithm", ErrInvalidCapability)
			}
			return key, nil
		}, jwt.WithAudience(AgentDelegationSessionCapabilityAudience), jwt.WithIssuer(memoryCapabilityIssuer), jwt.WithExpirationRequired(), jwt.WithTimeFunc(i.now))
		if parseErr != nil || !parsed.Valid {
			lastErr = parseErr
			continue
		}
		if claims.ExpiresAt == nil || !claims.ExpiresAt.Time.After(i.now().UTC()) {
			return nil, fmt.Errorf("%w: expired", ErrInvalidCapability)
		}
		if validateErr := validateAgentDelegationSessionBinding(agentDelegationSessionBindingFromClaims(claims)); validateErr != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidCapability, validateErr)
		}
		return claims, nil
	}
	return nil, fmt.Errorf("%w: %w", ErrInvalidCapability, lastErr)
}

func agentDelegationSessionBindingFromClaims(claims *AgentDelegationSessionCapabilityClaims) AgentDelegationSessionCapabilityBinding {
	return AgentDelegationSessionCapabilityBinding{
		Namespace: claims.Namespace, AgentName: claims.AgentName, AgentIdentityName: claims.AgentIdentityName,
		InstanceName: claims.InstanceName, InstanceUID: claims.InstanceUID, SessionID: claims.SessionID,
		Solution: claims.Solution, SolutionRev: claims.SolutionRev,
	}
}

func validateAgentDelegationSessionBinding(binding AgentDelegationSessionCapabilityBinding) error {
	if binding.Namespace == "" || binding.AgentName == "" || binding.AgentIdentityName == "" || binding.InstanceName == "" || binding.InstanceUID == "" || binding.SessionID == "" {
		return fmt.Errorf("agent delegation session capability requires every source binding field")
	}
	return nil
}
