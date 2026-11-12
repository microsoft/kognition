// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeapproval

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

// Gate3State classifies the tagged outcome of a Gate 3 evaluation per
// ADR-0014. Transport clients use it to decide whether to issue a capability,
// return a durable pending response, or surface a terminal decision.
type Gate3State string

const (
	// Gate3StateReady means the caller may proceed to preparation and claim.
	// The initial decision was NotRequired or a human has approved this exact
	// bound action.
	Gate3StateReady Gate3State = "ready"
	// Gate3StatePending means no terminal decision has been made yet. The
	// tagged pending response must remain the same durable invocation and
	// RuntimeApproval on every retry.
	Gate3StatePending Gate3State = "pending"
	// Gate3StateRejected means a human explicitly denied this exact bound
	// action. Retries must not be transparently upgraded.
	Gate3StateRejected Gate3State = "rejected"
	// Gate3StateExpired means the decision expiry passed before approval.
	Gate3StateExpired Gate3State = "expired"
)

// Gate3Store is the minimum RuntimeApproval persistence surface Gate 3 needs.
// It is a strict subset of store.RuntimeApprovalStore, so the production
// store satisfies it directly.
type Gate3Store interface {
	CreateOrGetRuntimeApproval(ctx context.Context, approval api.RuntimeApproval, payload json.RawMessage, ttl time.Duration) (bool, api.RuntimeApproval, error)
	AcknowledgeRuntimeApprovalDecisionAudit(ctx context.Context, workspace, invocationID string, expectedResourceVersion int64, auditID string) (api.RuntimeApproval, error)
}

// Gate3Config configures a Gate3Coordinator.
type Gate3Config struct {
	Store    Gate3Store
	Resolver *Resolver
	Keys     BindingKeyStore
	TTL      time.Duration
	Now      func() time.Time
}

// Gate3Coordinator resolves live effective policy, creates or gets an
// exact-bound RuntimeApproval, delivers decision evidence, and returns the
// tagged state per ADR-0014 "Preparation, Claim, and Accounting Order"
// steps 3 and 4.
type Gate3Coordinator struct {
	store    Gate3Store
	resolver *Resolver
	keys     BindingKeyStore
	ttl      time.Duration
	now      func() time.Time
}

// NewGate3Coordinator returns a validated Gate3Coordinator.
func NewGate3Coordinator(config Gate3Config) (*Gate3Coordinator, error) {
	if config.Store == nil {
		return nil, errors.New("Gate3Coordinator requires a RuntimeApproval store")
	}
	if config.Resolver == nil {
		return nil, errors.New("Gate3Coordinator requires a Resolver")
	}
	if config.Keys == nil {
		return nil, errors.New("Gate3Coordinator requires a BindingKeyStore")
	}
	if config.TTL <= 0 || config.TTL > maxApprovalTTL {
		return nil, fmt.Errorf("Gate3Coordinator TTL must be positive and at most %s", maxApprovalTTL)
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Gate3Coordinator{store: config.Store, resolver: config.Resolver, keys: config.Keys, ttl: config.TTL, now: config.Now}, nil
}

// Gate3Request identifies the exact action bound to a RuntimeApproval.
// Callers provide the plane-specific target reference and the canonical
// action payload so the same coordinator serves tool, memory, and
// communication planes.
type Gate3Request struct {
	Workspace         string
	Namespace         string
	InvocationID      string
	Plane             string
	Operation         string
	Action            string
	AgentName         string
	AgentIdentity     api.AgentIdentity
	AgentInstance     api.AgentInstance
	SessionID         string
	Target            api.RuntimeApprovalReference
	AuthorizationRefs []api.RuntimeApprovalReference
	CanonicalPayload  json.RawMessage
}

// Gate3Result is the tagged outcome of a Gate 3 evaluation.
type Gate3Result struct {
	State     Gate3State
	Approval  api.RuntimeApproval
	ExpiresAt time.Time
}

// Resolve resolves the live effective policy, creates or gets the exact
// RuntimeApproval, delivers decision evidence when a terminal decision is
// known, and returns the tagged outcome. It performs no quota reservation,
// preparation, or claim: those are ADR-0014 steps 5 through 10 owned by
// the invocation path.
func (g *Gate3Coordinator) Resolve(ctx context.Context, request Gate3Request) (Gate3Result, error) {
	if err := g.validateRequest(request); err != nil {
		return Gate3Result{}, err
	}
	resolution, err := g.resolver.Resolve(ctx, request.Namespace, request.AgentName)
	if err != nil {
		return Gate3Result{}, err
	}
	initialDecision, err := EvaluateActionPolicy(resolution.Class, request.Action)
	if err != nil {
		return Gate3Result{}, err
	}
	proposed, err := g.buildProposedApproval(ctx, request, resolution, initialDecision)
	if err != nil {
		return Gate3Result{}, err
	}
	_, stored, err := g.store.CreateOrGetRuntimeApproval(ctx, proposed, request.CanonicalPayload, g.ttl)
	if err != nil {
		return Gate3Result{}, fmt.Errorf("create or get RuntimeApproval: %w", err)
	}
	result := Gate3Result{Approval: stored, ExpiresAt: stored.Spec.ExpiresAt}
	switch stored.Status.DecisionState {
	case api.RuntimeApprovalDecisionApproved, api.RuntimeApprovalDecisionNotRequired:
		acked, ackErr := g.acknowledgeDecision(ctx, stored)
		if ackErr != nil {
			return result, ackErr
		}
		result.Approval = acked
		result.State = Gate3StateReady
	case api.RuntimeApprovalDecisionPending:
		result.State = Gate3StatePending
	case api.RuntimeApprovalDecisionRejected:
		acked, ackErr := g.acknowledgeDecision(ctx, stored)
		if ackErr != nil {
			return result, ackErr
		}
		result.Approval = acked
		result.State = Gate3StateRejected
	case api.RuntimeApprovalDecisionExpired:
		acked, ackErr := g.acknowledgeDecision(ctx, stored)
		if ackErr != nil {
			return result, ackErr
		}
		result.Approval = acked
		result.State = Gate3StateExpired
	default:
		return result, fmt.Errorf("Gate3Coordinator: RuntimeApproval has unknown decision state %q", stored.Status.DecisionState)
	}
	return result, nil
}

func (g *Gate3Coordinator) validateRequest(request Gate3Request) error {
	switch {
	case request.Workspace == "":
		return errors.New("Gate3Coordinator: workspace is required")
	case request.InvocationID == "":
		return errors.New("Gate3Coordinator: invocation ID is required")
	case request.Plane == "":
		return errors.New("Gate3Coordinator: plane is required")
	case request.Operation == "":
		return errors.New("Gate3Coordinator: operation is required")
	case request.Action == "":
		return errors.New("Gate3Coordinator: action is required")
	case request.AgentName == "":
		return errors.New("Gate3Coordinator: agent name is required")
	case request.SessionID == "":
		return errors.New("Gate3Coordinator: session ID is required")
	case request.AgentInstance.Status.SessionID != "" && request.AgentInstance.Status.SessionID != request.SessionID:
		return errors.New("Gate3Coordinator: AgentInstance session differs from the caller session")
	case request.Target.Kind == "" || request.Target.Name == "" || request.Target.UID == "":
		return errors.New("Gate3Coordinator: target reference must identify a stable resource incarnation")
	}
	return nil
}

func (g *Gate3Coordinator) buildProposedApproval(ctx context.Context, request Gate3Request, resolution Resolution, initialDecision string) (api.RuntimeApproval, error) {
	identity := request.AgentIdentity
	instance := request.AgentInstance
	instanceReference := Reference("AgentInstance", "Namespaced", &instance)
	configuration, err := json.Marshal(struct {
		Spec              api.AgentInstanceSpec `json:"spec"`
		Generation        int64                 `json:"generation"`
		Labels            map[string]string     `json:"labels,omitempty"`
		Annotations       map[string]string     `json:"annotations,omitempty"`
		OwnerReferences   []api.OwnerReference  `json:"ownerReferences,omitempty"`
		DeletionTimestamp *time.Time            `json:"deletionTimestamp,omitempty"`
		SessionID         string                `json:"sessionID,omitempty"`
	}{instance.Spec, instance.Generation, instance.Labels, instance.Annotations, instance.OwnerReferences,
		instance.DeletionTimestamp, request.SessionID})
	if err != nil {
		return api.RuntimeApproval{}, fmt.Errorf("bind AgentInstance configuration: %w", err)
	}
	digest := sha256.Sum256(configuration)
	instanceReference.ConfigurationDigest = hex.EncodeToString(digest[:])
	approval := api.RuntimeApproval{
		ObjectMeta: api.ObjectMeta{
			Name:      CanonicalName(request.Workspace, request.InvocationID),
			Namespace: request.Namespace,
		},
		Spec: api.RuntimeApprovalSpec{
			Workspace:         request.Workspace,
			InvocationID:      request.InvocationID,
			InitialDecision:   initialDecision,
			Plane:             request.Plane,
			Operation:         request.Operation,
			AgentRef:          Reference("Agent", "Namespaced", &resolution.Agent),
			AgentIdentityRef:  Reference("AgentIdentity", "Namespaced", &identity),
			AgentInstanceRef:  instanceReference,
			SessionRef:        request.SessionID,
			TargetRef:         request.Target,
			AutonomyClassRef:  Reference("AutonomyClass", "Cluster", &resolution.Class),
			AuthorizationRefs: request.AuthorizationRefs,
		},
		Status: InitialStatus(initialDecision),
	}
	if resolution.TrustProfile != nil {
		ref := Reference("TrustProfile", "Namespaced", resolution.TrustProfile)
		approval.Spec.TrustProfileRef = &ref
	}
	binding, err := Bind(ctx, g.keys, ActionEnvelopeFromApproval(approval, request.CanonicalPayload))
	if err != nil {
		return api.RuntimeApproval{}, fmt.Errorf("Gate3Coordinator: bind action: %w", err)
	}
	approval.Spec.Binding = binding
	return approval, nil
}

func (g *Gate3Coordinator) acknowledgeDecision(ctx context.Context, stored api.RuntimeApproval) (api.RuntimeApproval, error) {
	if stored.Status.DecisionEvidenceDelivered {
		return stored, nil
	}
	if stored.Status.DecisionAudit == nil {
		return stored, fmt.Errorf("Gate3Coordinator: RuntimeApproval decision %q lacks audit evidence", stored.Status.DecisionState)
	}
	acked, err := g.store.AcknowledgeRuntimeApprovalDecisionAudit(ctx, stored.Spec.Workspace, stored.Spec.InvocationID, stored.ResourceVersion, stored.Status.DecisionAudit.ID)
	if err != nil {
		return stored, fmt.Errorf("acknowledge RuntimeApproval decision audit: %w", err)
	}
	return acked, nil
}
