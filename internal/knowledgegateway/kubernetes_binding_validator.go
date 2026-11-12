// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package knowledgegateway

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
)

type KubernetesCapabilityBindingValidator struct {
	reader ctrlclient.Reader
}

func (validator *KubernetesCapabilityBindingValidator) QueryConstraints(ctx context.Context, claims *runtimeauth.KnowledgeSessionCapabilityClaims, knowledgeBase string) (*QueryConstraints, error) {
	agent := &k8s.Agent{}
	if err := validator.reader.Get(ctx, types.NamespacedName{Namespace: claims.Namespace, Name: claims.AgentName}, agent); err != nil {
		return nil, fmt.Errorf("read live query Agent: %w", err)
	}
	if string(agent.UID) != claims.AgentUID || !agent.DeletionTimestamp.IsZero() {
		return nil, ErrInactiveCapabilityBinding
	}
	for _, reference := range agent.Spec.KnowledgeBaseClaims {
		claim := &k8s.KnowledgeBaseClaim{}
		if err := validator.reader.Get(ctx, types.NamespacedName{Namespace: claims.Namespace, Name: reference.Name}, claim); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("read live query claim: %w", err)
		}
		if !claim.DeletionTimestamp.IsZero() || claim.Spec.KnowledgeBaseName != knowledgeBase ||
			claim.Status.Phase != string(api.PhaseBound) || claim.Status.ObservedGeneration != claim.Generation || claim.Status.BoundKnowledgeBase != knowledgeBase {
			continue
		}
		result := &QueryConstraints{Filters: make(map[string]string)}
		if retrieval := claim.Spec.Retrieval; retrieval != nil {
			result.Mode = retrieval.Mode
			if retrieval.TopK != nil {
				result.TopK = *retrieval.TopK
			}
			if retrieval.MinScore != "" {
				value, err := strconv.ParseFloat(retrieval.MinScore, 64)
				if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
					return nil, fmt.Errorf("claim minimum retrieval score is invalid")
				}
				result.MinScore = &value
			}
			if retrieval.HybridWeights != nil {
				return nil, fmt.Errorf("claim hybrid weight controls are unsupported by this gateway")
			}
			for key, value := range retrieval.Filter {
				if !strings.HasPrefix(key, "metadata.") {
					key = "metadata." + key
				}
				result.Filters[key] = value
			}
		}
		return result, nil
	}
	return nil, ErrInactiveCapabilityBinding
}

func NewKubernetesCapabilityBindingValidator(reader ctrlclient.Reader) (*KubernetesCapabilityBindingValidator, error) {
	if reader == nil {
		return nil, errors.New("Kubernetes capability binding validator requires a reader")
	}
	return &KubernetesCapabilityBindingValidator{reader: reader}, nil
}

func (validator *KubernetesCapabilityBindingValidator) Validate(ctx context.Context, claims *runtimeauth.KnowledgeSessionCapabilityClaims, knowledgeBase string) error {
	if claims == nil {
		return ErrInactiveCapabilityBinding
	}
	agentKey := types.NamespacedName{Namespace: claims.Namespace, Name: claims.AgentName}
	agent := &k8s.Agent{}
	if err := validator.reader.Get(ctx, agentKey, agent); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%w: Agent no longer exists", ErrInactiveCapabilityBinding)
		}
		return fmt.Errorf("read Agent %s: %w", agentKey, err)
	}
	if string(agent.UID) != claims.AgentUID || !agent.DeletionTimestamp.IsZero() {
		return fmt.Errorf("%w: Agent does not match the active session", ErrInactiveCapabilityBinding)
	}
	key := types.NamespacedName{Namespace: claims.Namespace, Name: claims.InstanceName}
	instance := &k8s.AgentInstance{}
	if err := validator.reader.Get(ctx, key, instance); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%w: AgentInstance no longer exists", ErrInactiveCapabilityBinding)
		}
		return fmt.Errorf("read AgentInstance %s: %w", key, err)
	}
	if string(instance.ObjectMeta.UID) != claims.InstanceUID ||
		!instance.DeletionTimestamp.IsZero() ||
		instance.Spec.AgentRef.Name != claims.AgentName ||
		instance.Status.Phase != string(api.PhaseRunning) ||
		instance.Status.SessionID != claims.SessionID {
		return fmt.Errorf("%w: AgentInstance does not match the active session", ErrInactiveCapabilityBinding)
	}

	identityKey := types.NamespacedName{Namespace: claims.Namespace, Name: claims.AgentIdentityName}
	identity := &k8s.AgentIdentity{}
	if err := validator.reader.Get(ctx, identityKey, identity); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%w: AgentIdentity no longer exists", ErrInactiveCapabilityBinding)
		}
		return fmt.Errorf("read AgentIdentity %s: %w", identityKey, err)
	}
	if string(identity.UID) != claims.AgentIdentityUID || !identity.DeletionTimestamp.IsZero() || identity.Status.Phase != string(api.PhaseReady) || identity.Spec.AgentRef.Name != claims.AgentName {
		return fmt.Errorf("%w: AgentIdentity does not match the active agent", ErrInactiveCapabilityBinding)
	}
	for _, reference := range agent.Spec.KnowledgeBaseClaims {
		claimKey := types.NamespacedName{Namespace: claims.Namespace, Name: reference.Name}
		claim := &k8s.KnowledgeBaseClaim{}
		if err := validator.reader.Get(ctx, claimKey, claim); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("read KnowledgeBaseClaim %s: %w", claimKey, err)
		}
		if claim.DeletionTimestamp.IsZero() && claim.Status.Phase == string(api.PhaseBound) &&
			claim.Status.ObservedGeneration == claim.Generation && claim.Spec.KnowledgeBaseName == knowledgeBase &&
			claim.Status.BoundKnowledgeBase == knowledgeBase {
			if claim.Spec.Usage != nil && (claim.Spec.Usage.MaxQueriesPerMinute != nil || claim.Spec.Usage.MaxTokensReturned != nil) {
				return fmt.Errorf("configured knowledge claim usage limits require a query quota enforcer")
			}
			return nil
		}
	}
	return fmt.Errorf("%w: Agent has no current bound claim for KnowledgeBase", ErrInactiveCapabilityBinding)
}
