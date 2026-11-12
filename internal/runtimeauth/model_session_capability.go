// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeauth

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/modelinvocation"
)

const ModelSessionCapabilityAudience = "kognition-model-gateway-session"
const ModelProfileSessionCapabilityAudience = "kognition-model-gateway-profile-session-v1"

// ModelSessionCapabilityBinding identifies the active workload session and
// exact Model revision allowed to request inference through Model Gateway.
type ModelSessionCapabilityBinding struct {
	Namespace                string
	AgentName                string
	AgentIdentityName        string
	InstanceName             string
	InstanceUID              string
	SessionID                string
	WorkflowName             string
	StepName                 string
	Solution                 string
	SolutionRev              int64
	ModelName                string
	ModelResourceVersion     int64
	CandidateBinding         *v1alpha1.ModelVersionBinding
	InvocationProfileBinding *v1alpha1.ModelInvocationProfileBinding
}

type ModelSessionCapabilityClaims struct {
	Namespace                string                                  `json:"namespace"`
	AgentName                string                                  `json:"agent_name"`
	AgentIdentityName        string                                  `json:"agent_identity_name"`
	InstanceName             string                                  `json:"instance_name"`
	InstanceUID              string                                  `json:"instance_uid"`
	SessionID                string                                  `json:"session_id"`
	WorkflowName             string                                  `json:"workflow_name,omitempty"`
	StepName                 string                                  `json:"step_name,omitempty"`
	Solution                 string                                  `json:"solution,omitempty"`
	SolutionRev              int64                                   `json:"solution_revision,omitempty"`
	ModelName                string                                  `json:"model_name"`
	ModelResourceVersion     int64                                   `json:"model_resource_version"`
	CandidateBinding         *v1alpha1.ModelVersionBinding           `json:"candidate_binding,omitempty"`
	InvocationProfileBinding *v1alpha1.ModelInvocationProfileBinding `json:"invocation_profile_binding,omitempty"`
	jwt.RegisteredClaims
}

// ModelSessionCapabilityIssuer signs capabilities consumed only by Model
// Gateway. It accepts the previous key during configured rotation overlap.
type ModelSessionCapabilityIssuer struct {
	keys *CapabilityKeyRing
	ttl  time.Duration
	now  func() time.Time
}

func NewModelSessionCapabilityIssuer(key []byte, ttl time.Duration) (*ModelSessionCapabilityIssuer, error) {
	keys, err := NewCapabilityKeyRing(ActiveCapabilityKeyID, key, "", nil)
	if err != nil {
		return nil, err
	}
	return NewModelSessionCapabilityIssuerWithKeyRing(keys, ttl)
}

func NewModelSessionCapabilityIssuerWithKeyRing(keys *CapabilityKeyRing, ttl time.Duration) (*ModelSessionCapabilityIssuer, error) {
	if keys == nil {
		return nil, fmt.Errorf("model session capability key ring is required")
	}
	if ttl <= 0 {
		return nil, fmt.Errorf("model session capability TTL must be positive")
	}
	return &ModelSessionCapabilityIssuer{keys: keys, ttl: ttl, now: time.Now}, nil
}

// TTL returns the maximum lifetime of a Model session capability.
func (i *ModelSessionCapabilityIssuer) TTL() time.Duration {
	if i == nil {
		return 0
	}
	return i.ttl
}

func (i *ModelSessionCapabilityIssuer) Issue(binding ModelSessionCapabilityBinding) (string, error) {
	if err := validateModelSessionBinding(binding); err != nil {
		return "", err
	}
	now := i.now().UTC()
	audience := ModelSessionCapabilityAudience
	if binding.InvocationProfileBinding != nil {
		audience = ModelProfileSessionCapabilityAudience
	}
	claims := ModelSessionCapabilityClaims{
		Namespace: binding.Namespace, AgentName: binding.AgentName, AgentIdentityName: binding.AgentIdentityName,
		InstanceName: binding.InstanceName, InstanceUID: binding.InstanceUID, SessionID: binding.SessionID,
		WorkflowName: binding.WorkflowName, StepName: binding.StepName,
		Solution: binding.Solution, SolutionRev: binding.SolutionRev,
		ModelName: binding.ModelName, ModelResourceVersion: binding.ModelResourceVersion,
		CandidateBinding:         binding.CandidateBinding,
		InvocationProfileBinding: binding.InvocationProfileBinding,
		RegisteredClaims: jwt.RegisteredClaims{
			ID:     uuid.NewString(),
			Issuer: memoryCapabilityIssuer, Audience: jwt.ClaimStrings{audience},
			IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(i.ttl)),
		},
	}
	return i.keys.Sign(claims)
}

func (i *ModelSessionCapabilityIssuer) VerifyClaims(token string) (*ModelSessionCapabilityClaims, error) {
	keys, err := i.keys.VerificationKeys(token)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidCapability, err)
	}
	var lastErr error
	for _, key := range keys {
		claims := &ModelSessionCapabilityClaims{}
		parsed, parseErr := jwt.ParseWithClaims(token, claims, func(token *jwt.Token) (any, error) {
			if token.Method != jwt.SigningMethodHS256 {
				return nil, fmt.Errorf("%w: unsupported signing algorithm", ErrInvalidCapability)
			}
			return key, nil
		}, jwt.WithAudience(ModelSessionCapabilityAudience, ModelProfileSessionCapabilityAudience), jwt.WithIssuer(memoryCapabilityIssuer), jwt.WithExpirationRequired(), jwt.WithTimeFunc(i.now))
		if parseErr != nil || !parsed.Valid {
			lastErr = parseErr
			continue
		}
		if claims.ExpiresAt == nil || !claims.ExpiresAt.Time.After(i.now().UTC()) {
			return nil, fmt.Errorf("%w: expired", ErrInvalidCapability)
		}
		audience := ModelSessionCapabilityAudience
		if claims.InvocationProfileBinding != nil {
			audience = ModelProfileSessionCapabilityAudience
		}
		if len(claims.Audience) != 1 || claims.Audience[0] != audience {
			return nil, fmt.Errorf("%w: invocation profile audience does not match its binding", ErrInvalidCapability)
		}
		if validateErr := validateModelSessionBinding(modelSessionBindingFromClaims(claims)); validateErr != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidCapability, validateErr)
		}
		return claims, nil
	}
	return nil, fmt.Errorf("%w: %w", ErrInvalidCapability, lastErr)
}

func modelSessionBindingFromClaims(claims *ModelSessionCapabilityClaims) ModelSessionCapabilityBinding {
	return ModelSessionCapabilityBinding{
		Namespace: claims.Namespace, AgentName: claims.AgentName, AgentIdentityName: claims.AgentIdentityName,
		InstanceName: claims.InstanceName, InstanceUID: claims.InstanceUID, SessionID: claims.SessionID,
		WorkflowName: claims.WorkflowName, StepName: claims.StepName,
		Solution: claims.Solution, SolutionRev: claims.SolutionRev,
		ModelName: claims.ModelName, ModelResourceVersion: claims.ModelResourceVersion,
		CandidateBinding:         claims.CandidateBinding,
		InvocationProfileBinding: claims.InvocationProfileBinding,
	}
}

func validateModelSessionBinding(binding ModelSessionCapabilityBinding) error {
	if !modelinvocation.ValidBinding(binding.InvocationProfileBinding) {
		return fmt.Errorf("runtime model invocation profile binding is invalid")
	}
	if binding.Namespace == "" || binding.AgentName == "" || binding.AgentIdentityName == "" || binding.InstanceName == "" || binding.InstanceUID == "" || binding.SessionID == "" || binding.ModelName == "" || binding.ModelResourceVersion <= 0 {
		return fmt.Errorf("runtime model session capability requires every binding field")
	}
	if candidate := binding.CandidateBinding; candidate != nil && (candidate.Namespace != binding.Namespace || candidate.ModelName == "" || candidate.ModelUID == "" || candidate.ModelResourceVersion <= 0 || !modelinvocation.ValidDigest(candidate.SpecDigest)) {
		return fmt.Errorf("runtime model capability candidate binding is invalid")
	}
	return nil
}
