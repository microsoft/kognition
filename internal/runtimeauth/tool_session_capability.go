// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeauth

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const ToolSessionCapabilityAudience = "kognition-tool-gateway-session"

// ToolSessionCapabilityBinding identifies one active agent runtime session. It
// is not executable: the Tool Gateway exchanges it for an exact Tool
// capability only after live authorization of the selected tool.
type ToolSessionCapabilityBinding struct {
	Namespace         string
	AgentName         string
	AgentIdentityName string
	InstanceName      string
	InstanceUID       string
	SessionID         string
	Solution          string
	SolutionRev       int64
}

type ToolSessionCapabilityClaims struct {
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

// ToolSessionCapabilityIssuer signs runtime session capabilities that can
// only be presented to the Tool Gateway capability-exchange endpoint.
type ToolSessionCapabilityIssuer struct {
	keys *CapabilityKeyRing
	ttl  time.Duration
	now  func() time.Time
}

func NewToolSessionCapabilityIssuer(key []byte, ttl time.Duration) (*ToolSessionCapabilityIssuer, error) {
	keys, err := NewCapabilityKeyRing(ActiveCapabilityKeyID, key, "", nil)
	if err != nil {
		return nil, err
	}
	return NewToolSessionCapabilityIssuerWithKeyRing(keys, ttl)
}

// NewToolSessionCapabilityIssuerWithKeyRing creates an issuer that signs with
// the active key and accepts the previous key for rotation overlap.
func NewToolSessionCapabilityIssuerWithKeyRing(keys *CapabilityKeyRing, ttl time.Duration) (*ToolSessionCapabilityIssuer, error) {
	if keys == nil {
		return nil, fmt.Errorf("tool session capability key ring is required")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("tool session capability TTL must be positive")
	}
	return &ToolSessionCapabilityIssuer{keys: keys, ttl: ttl, now: time.Now}, nil
}

func (i *ToolSessionCapabilityIssuer) Issue(binding ToolSessionCapabilityBinding) (string, error) {
	if err := validateToolSessionBinding(binding); err != nil {
		return "", err
	}
	now := i.now().UTC()
	claims := ToolSessionCapabilityClaims{
		Namespace: binding.Namespace, AgentName: binding.AgentName, AgentIdentityName: binding.AgentIdentityName,
		InstanceName: binding.InstanceName, InstanceUID: binding.InstanceUID, SessionID: binding.SessionID,
		Solution: binding.Solution, SolutionRev: binding.SolutionRev,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:     uuid.NewString(),
			Issuer: memoryCapabilityIssuer, Audience: jwt.ClaimStrings{ToolSessionCapabilityAudience},
			IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(i.ttl)),
		},
	}
	return i.keys.Sign(claims)
}

func (i *ToolSessionCapabilityIssuer) VerifyClaims(token string) (*ToolSessionCapabilityClaims, error) {
	keys, err := i.keys.VerificationKeys(token)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidCapability, err)
	}
	var lastErr error
	for _, key := range keys {
		claims := &ToolSessionCapabilityClaims{}
		parsed, parseErr := jwt.ParseWithClaims(token, claims, func(token *jwt.Token) (any, error) {
			if token.Method != jwt.SigningMethodHS256 {
				return nil, fmt.Errorf("%w: unsupported signing algorithm", ErrInvalidCapability)
			}
			return key, nil
		}, jwt.WithAudience(ToolSessionCapabilityAudience), jwt.WithIssuer(memoryCapabilityIssuer), jwt.WithExpirationRequired(), jwt.WithTimeFunc(i.now))
		if parseErr != nil || !parsed.Valid {
			lastErr = parseErr
			continue
		}
		if claims.ExpiresAt == nil || !claims.ExpiresAt.Time.After(i.now().UTC()) {
			return nil, fmt.Errorf("%w: expired", ErrInvalidCapability)
		}
		if validateErr := validateToolSessionBinding(toolSessionBindingFromClaims(claims)); validateErr != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidCapability, validateErr)
		}
		return claims, nil
	}
	return nil, fmt.Errorf("%w: %w", ErrInvalidCapability, lastErr)
}

func toolSessionBindingFromClaims(claims *ToolSessionCapabilityClaims) ToolSessionCapabilityBinding {
	return ToolSessionCapabilityBinding{Namespace: claims.Namespace, AgentName: claims.AgentName, AgentIdentityName: claims.AgentIdentityName, InstanceName: claims.InstanceName, InstanceUID: claims.InstanceUID, SessionID: claims.SessionID, Solution: claims.Solution, SolutionRev: claims.SolutionRev}
}

func validateToolSessionBinding(binding ToolSessionCapabilityBinding) error {
	if binding.Namespace == "" || binding.AgentName == "" || binding.AgentIdentityName == "" || binding.InstanceName == "" || binding.InstanceUID == "" || binding.SessionID == "" {
		return fmt.Errorf("runtime tool session capability requires every binding field")
	}
	return nil
}
