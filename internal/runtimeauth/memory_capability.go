// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package runtimeauth defines authenticated capabilities issued by the
// control plane to runtime workloads.
package runtimeauth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

const (
	// MemoryCapabilityAudience identifies the memory policy gateway as the only
	// intended recipient of a runtime memory capability.
	MemoryCapabilityAudience = "kognition-memory-policy-gateway"

	memoryCapabilityIssuer = "kognition-runtime-dispatch"
	minimumHMACKeyBytes    = 32
)

var (
	// ErrInvalidCapability indicates that a capability is malformed, expired,
	// signed by a different key, or has unexpected registered claims.
	ErrInvalidCapability = errors.New("invalid runtime memory capability")

	// ErrCapabilityBindingMismatch indicates a valid capability was presented
	// for a different workload or memory resource.
	ErrCapabilityBindingMismatch = errors.New("runtime memory capability binding mismatch")
)

// MemoryCapabilityBinding identifies the workload and memory resource a
// capability is permitted to access.
type MemoryCapabilityBinding struct {
	Namespace         string
	AgentName         string
	AgentIdentityName string
	InstanceName      string
	InstanceUID       string
	SessionID         string
	VolumeHandle      string
}

// MemoryCapabilityClaims carries a runtime memory capability's immutable
// request binding.
type MemoryCapabilityClaims struct {
	Namespace         string `json:"namespace"`
	AgentName         string `json:"agent_name"`
	AgentIdentityName string `json:"agent_identity_name"`
	InstanceName      string `json:"instance_name"`
	InstanceUID       string `json:"instance_uid"`
	SessionID         string `json:"session_id"`
	VolumeHandle      string `json:"volume_handle"`
	jwt.RegisteredClaims
}

// MemoryCapabilityIssuer signs and verifies short-lived runtime memory
// capabilities. It is safe for concurrent use after construction.
type MemoryCapabilityIssuer struct {
	keys *CapabilityKeyRing
	ttl  time.Duration
	now  func() time.Time
}

// NewMemoryCapabilityIssuer creates an issuer for a controller and policy
// gateway that share a randomly generated HMAC key.
func NewMemoryCapabilityIssuer(key []byte, ttl time.Duration) (*MemoryCapabilityIssuer, error) {
	keys, err := NewCapabilityKeyRing(ActiveCapabilityKeyID, key, "", nil)
	if err != nil {
		return nil, err
	}
	return NewMemoryCapabilityIssuerWithKeyRing(keys, ttl)
}

// NewMemoryCapabilityIssuerWithKeyRing creates an issuer that signs with the
// active key and accepts the previous key for rotation overlap.
func NewMemoryCapabilityIssuerWithKeyRing(keys *CapabilityKeyRing, ttl time.Duration) (*MemoryCapabilityIssuer, error) {
	if keys == nil {
		return nil, errors.New("memory capability key ring is required")
	}
	if ttl <= 0 {
		return nil, errors.New("memory capability TTL must be positive")
	}
	return &MemoryCapabilityIssuer{
		keys: keys,
		ttl:  ttl,
		now:  time.Now,
	}, nil
}

// Issue creates a capability limited to exactly one runtime session and memory
// volume. Every binding field is required to prevent wildcard capabilities.
func (i *MemoryCapabilityIssuer) Issue(binding MemoryCapabilityBinding) (string, error) {
	if err := validateMemoryBinding(binding); err != nil {
		return "", err
	}
	now := i.now().UTC()
	claims := MemoryCapabilityClaims{
		Namespace:         binding.Namespace,
		AgentName:         binding.AgentName,
		AgentIdentityName: binding.AgentIdentityName,
		InstanceName:      binding.InstanceName,
		InstanceUID:       binding.InstanceUID,
		SessionID:         binding.SessionID,
		VolumeHandle:      binding.VolumeHandle,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:        uuid.NewString(),
			Issuer:    memoryCapabilityIssuer,
			Audience:  jwt.ClaimStrings{MemoryCapabilityAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(i.ttl)),
		},
	}
	return i.keys.Sign(claims)
}

// Verify rejects capabilities that are not valid for the expected workload and
// target volume. It returns the verified claims for gateway audit fields.
func (i *MemoryCapabilityIssuer) Verify(token string, expected MemoryCapabilityBinding) (*MemoryCapabilityClaims, error) {
	claims, err := i.VerifyClaims(token)
	if err != nil {
		return nil, err
	}
	if expected != memoryBindingFromClaims(claims) {
		return nil, ErrCapabilityBindingMismatch
	}
	return claims, nil
}

// VerifyClaims verifies signature and registered claims before returning the
// immutable workload binding for a policy gateway to authorize.
func (i *MemoryCapabilityIssuer) VerifyClaims(token string) (*MemoryCapabilityClaims, error) {
	keys, err := i.keys.VerificationKeys(token)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidCapability, err)
	}
	var lastErr error
	for _, key := range keys {
		claims := &MemoryCapabilityClaims{}
		parsed, parseErr := jwt.ParseWithClaims(token, claims, func(token *jwt.Token) (any, error) {
			if token.Method != jwt.SigningMethodHS256 {
				return nil, fmt.Errorf("%w: unsupported signing algorithm", ErrInvalidCapability)
			}
			return key, nil
		}, jwt.WithAudience(MemoryCapabilityAudience), jwt.WithIssuer(memoryCapabilityIssuer), jwt.WithExpirationRequired(), jwt.WithTimeFunc(i.now))
		if parseErr != nil || !parsed.Valid {
			lastErr = parseErr
			continue
		}
		if claims.ExpiresAt == nil || !claims.ExpiresAt.Time.After(i.now().UTC()) {
			return nil, fmt.Errorf("%w: expired", ErrInvalidCapability)
		}
		if validateErr := validateMemoryBinding(memoryBindingFromClaims(claims)); validateErr != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidCapability, validateErr)
		}
		return claims, nil
	}
	return nil, fmt.Errorf("%w: %w", ErrInvalidCapability, lastErr)
}

func memoryBindingFromClaims(claims *MemoryCapabilityClaims) MemoryCapabilityBinding {
	return MemoryCapabilityBinding{
		Namespace:         claims.Namespace,
		AgentName:         claims.AgentName,
		AgentIdentityName: claims.AgentIdentityName,
		InstanceName:      claims.InstanceName,
		InstanceUID:       claims.InstanceUID,
		SessionID:         claims.SessionID,
		VolumeHandle:      claims.VolumeHandle,
	}
}

func validateMemoryBinding(binding MemoryCapabilityBinding) error {
	if binding.Namespace == "" || binding.AgentName == "" || binding.AgentIdentityName == "" ||
		binding.InstanceName == "" || binding.InstanceUID == "" || binding.SessionID == "" || binding.VolumeHandle == "" {
		return errors.New("runtime memory capability requires every binding field")
	}
	return nil
}

func isZeroKey(key []byte) bool {
	for _, value := range key {
		if value != 0 {
			return false
		}
	}
	return true
}
