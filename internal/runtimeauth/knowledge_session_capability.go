// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeauth

import (
	"fmt"
	"slices"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/util/validation"
)

const KnowledgeSessionCapabilityAudience = "kognition-knowledge-gateway-session"

// KnowledgeSessionCapabilityBinding identifies the active workload session and
// exact KnowledgeBases it may query through Knowledge Gateway.
type KnowledgeSessionCapabilityBinding struct {
	Namespace         string
	AgentName         string
	AgentUID          string
	AgentIdentityName string
	AgentIdentityUID  string
	InstanceName      string
	InstanceUID       string
	SessionID         string
	KnowledgeBases    []string
}

type KnowledgeSessionCapabilityClaims struct {
	Namespace         string   `json:"namespace"`
	AgentName         string   `json:"agent_name"`
	AgentUID          string   `json:"agent_uid"`
	AgentIdentityName string   `json:"agent_identity_name"`
	AgentIdentityUID  string   `json:"agent_identity_uid"`
	InstanceName      string   `json:"instance_name"`
	InstanceUID       string   `json:"instance_uid"`
	SessionID         string   `json:"session_id"`
	KnowledgeBases    []string `json:"knowledge_bases"`
	jwt.RegisteredClaims
}

// KnowledgeSessionCapabilityIssuer signs capabilities consumed only by
// Knowledge Gateway. It accepts the previous key during rotation overlap.
type KnowledgeSessionCapabilityIssuer struct {
	keys *CapabilityKeyRing
	ttl  time.Duration
	now  func() time.Time
}

func NewKnowledgeSessionCapabilityIssuer(key []byte, ttl time.Duration) (*KnowledgeSessionCapabilityIssuer, error) {
	keys, err := NewCapabilityKeyRing(ActiveCapabilityKeyID, key, "", nil)
	if err != nil {
		return nil, err
	}
	return NewKnowledgeSessionCapabilityIssuerWithKeyRing(keys, ttl)
}

func NewKnowledgeSessionCapabilityIssuerWithKeyRing(keys *CapabilityKeyRing, ttl time.Duration) (*KnowledgeSessionCapabilityIssuer, error) {
	if keys == nil {
		return nil, fmt.Errorf("knowledge session capability key ring is required")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("knowledge session capability TTL must be positive")
	}
	return &KnowledgeSessionCapabilityIssuer{keys: keys, ttl: ttl, now: time.Now}, nil
}

func (i *KnowledgeSessionCapabilityIssuer) TTL() time.Duration {
	if i == nil {
		return 0
	}
	return i.ttl
}

func (i *KnowledgeSessionCapabilityIssuer) Issue(binding KnowledgeSessionCapabilityBinding) (string, error) {
	if err := validateKnowledgeSessionBinding(binding); err != nil {
		return "", err
	}
	now := i.now().UTC()
	claims := KnowledgeSessionCapabilityClaims{
		Namespace: binding.Namespace, AgentName: binding.AgentName, AgentUID: binding.AgentUID,
		AgentIdentityName: binding.AgentIdentityName, AgentIdentityUID: binding.AgentIdentityUID,
		InstanceName: binding.InstanceName, InstanceUID: binding.InstanceUID, SessionID: binding.SessionID,
		KnowledgeBases: append([]string(nil), binding.KnowledgeBases...),
		RegisteredClaims: jwt.RegisteredClaims{
			ID: uuid.NewString(), Issuer: memoryCapabilityIssuer, Audience: jwt.ClaimStrings{KnowledgeSessionCapabilityAudience},
			IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(i.ttl)),
		},
	}
	return i.keys.Sign(claims)
}

func (i *KnowledgeSessionCapabilityIssuer) VerifyClaims(token string) (*KnowledgeSessionCapabilityClaims, error) {
	keys, err := i.keys.VerificationKeys(token)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidCapability, err)
	}
	var lastErr error
	for _, key := range keys {
		claims := &KnowledgeSessionCapabilityClaims{}
		parsed, parseErr := jwt.ParseWithClaims(token, claims, func(token *jwt.Token) (any, error) {
			if token.Method != jwt.SigningMethodHS256 {
				return nil, fmt.Errorf("%w: unsupported signing algorithm", ErrInvalidCapability)
			}
			return key, nil
		}, jwt.WithAudience(KnowledgeSessionCapabilityAudience), jwt.WithIssuer(memoryCapabilityIssuer), jwt.WithExpirationRequired(), jwt.WithTimeFunc(i.now))
		if parseErr != nil || !parsed.Valid {
			lastErr = parseErr
			continue
		}
		if claims.ExpiresAt == nil || !claims.ExpiresAt.Time.After(i.now().UTC()) {
			return nil, fmt.Errorf("%w: expired", ErrInvalidCapability)
		}
		if validateErr := validateKnowledgeSessionBinding(knowledgeSessionBindingFromClaims(claims)); validateErr != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidCapability, validateErr)
		}
		return claims, nil
	}
	return nil, fmt.Errorf("%w: %w", ErrInvalidCapability, lastErr)
}

func knowledgeSessionBindingFromClaims(claims *KnowledgeSessionCapabilityClaims) KnowledgeSessionCapabilityBinding {
	return KnowledgeSessionCapabilityBinding{
		Namespace: claims.Namespace, AgentName: claims.AgentName, AgentUID: claims.AgentUID,
		AgentIdentityName: claims.AgentIdentityName, AgentIdentityUID: claims.AgentIdentityUID,
		InstanceName: claims.InstanceName, InstanceUID: claims.InstanceUID, SessionID: claims.SessionID,
		KnowledgeBases: append([]string(nil), claims.KnowledgeBases...),
	}
}

func validateKnowledgeSessionBinding(binding KnowledgeSessionCapabilityBinding) error {
	if binding.Namespace == "" || binding.AgentName == "" || binding.AgentUID == "" || binding.AgentIdentityName == "" || binding.AgentIdentityUID == "" || binding.InstanceName == "" || binding.InstanceUID == "" || binding.SessionID == "" || len(binding.KnowledgeBases) == 0 {
		return fmt.Errorf("runtime knowledge session capability requires every binding field")
	}
	if !slices.IsSorted(binding.KnowledgeBases) {
		return fmt.Errorf("runtime knowledge session capability requires sorted KnowledgeBase names")
	}
	for index, name := range binding.KnowledgeBases {
		if len(validation.IsDNS1123Subdomain(name)) != 0 || (index > 0 && name == binding.KnowledgeBases[index-1]) {
			return fmt.Errorf("runtime knowledge session capability contains an invalid KnowledgeBase name")
		}
	}
	return nil
}
