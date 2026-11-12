// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeapproval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

const (
	ActionToolInvocation        = "toolInvocation"
	ActionMemoryWrite           = "memoryWrite"
	ActionExternalCommunication = "externalCommunication"

	SyntheticSupervisedUID = "kognition.dev/synthetic-supervised/v1"
)

var ErrPolicyUnavailable = errors.New("runtime autonomy policy unavailable")

// Resolution is the live effective trust/class evidence used in an action
// binding. A nil TrustProfile is the valid cold-start state.
type Resolution struct {
	Agent        api.Agent
	TrustProfile *api.TrustProfile
	Class        api.AutonomyClass
	Synthetic    bool
}

type TrustProfileIndexValidator interface {
	ValidateTrustProfileIndex(context.Context, string, api.Agent, *api.TrustProfile) error
}

// Resolver resolves the effective class from live store state and rejects
// stale Agent incarnations rather than inheriting a predecessor's profile.
type Resolver struct {
	store store.Store
}

func NewResolver(st store.Store) *Resolver { return &Resolver{store: st} }

func (r *Resolver) Resolve(ctx context.Context, namespace, agentName string) (Resolution, error) {
	if namespace == "" || agentName == "" {
		return Resolution{}, fmt.Errorf("%w: namespace and Agent name are required", ErrPolicyUnavailable)
	}
	rawAgent, err := r.store.Get(ctx, "Agent", namespace, agentName)
	if err != nil {
		return Resolution{}, fmt.Errorf("%w: get Agent: %w", ErrPolicyUnavailable, err)
	}
	var agent api.Agent
	if err := json.Unmarshal(rawAgent, &agent); err != nil || agent.UID == "" || agent.ResourceVersion <= 0 {
		return Resolution{}, fmt.Errorf("%w: Agent lacks stable identity", ErrPolicyUnavailable)
	}

	rawProfiles, err := r.store.List(ctx, "TrustProfile", store.ListOptions{Namespace: namespace})
	if err != nil {
		return Resolution{}, fmt.Errorf("%w: list TrustProfiles: %w", ErrPolicyUnavailable, err)
	}
	var matching []api.TrustProfile
	for _, raw := range rawProfiles {
		var profile api.TrustProfile
		if err := json.Unmarshal(raw, &profile); err != nil {
			return Resolution{}, fmt.Errorf("%w: invalid TrustProfile: %w", ErrPolicyUnavailable, err)
		}
		if profile.Spec.AgentRef.Name != agentName || profile.DeletionTimestamp != nil {
			continue
		}
		if profile.Spec.AgentIncarnationUID != agent.UID {
			return Resolution{}, fmt.Errorf("%w: TrustProfile %q belongs to a different Agent incarnation", ErrPolicyUnavailable, profile.Name)
		}
		if profile.UID == "" || profile.ResourceVersion <= 0 {
			return Resolution{}, fmt.Errorf("%w: TrustProfile %q lacks stable identity", ErrPolicyUnavailable, profile.Name)
		}
		matching = append(matching, profile)
	}
	if len(matching) > 1 {
		return Resolution{}, fmt.Errorf("%w: multiple live TrustProfiles match Agent %q", ErrPolicyUnavailable, agentName)
	}

	className := "supervised"
	var profile *api.TrustProfile
	if len(matching) == 1 {
		profile = &matching[0]
		if profile.Status.CurrentClass != "" {
			className = profile.Status.CurrentClass
		} else if profile.Spec.InitialClass != "" {
			className = profile.Spec.InitialClass
		}
	}
	if validator, ok := r.store.(TrustProfileIndexValidator); ok {
		if err := validator.ValidateTrustProfileIndex(ctx, namespace, agent, profile); err != nil {
			return Resolution{}, fmt.Errorf("%w: validate TrustProfileIndex: %w", ErrPolicyUnavailable, err)
		}
	}
	rawClass, err := r.store.Get(ctx, "AutonomyClass", "system", className)
	if err != nil {
		if profile == nil && className == "supervised" && errors.Is(err, store.ErrNotFound) {
			return Resolution{Agent: agent, Class: syntheticSupervisedClass(), Synthetic: true}, nil
		}
		return Resolution{}, fmt.Errorf("%w: get AutonomyClass %q: %w", ErrPolicyUnavailable, className, err)
	}
	var class api.AutonomyClass
	if err := json.Unmarshal(rawClass, &class); err != nil || class.UID == "" || class.ResourceVersion <= 0 {
		return Resolution{}, fmt.Errorf("%w: AutonomyClass %q lacks valid stable identity", ErrPolicyUnavailable, className)
	}
	class.Namespace = ""
	return Resolution{Agent: agent, TrustProfile: profile, Class: class}, nil
}

func syntheticSupervisedClass() api.AutonomyClass {
	// AutonomyClass is cluster-scoped (see api/k8s/v1alpha1/autonomy_types.go),
	// so the synthetic fallback MUST expose an empty Namespace. The resolver
	// looks the resource up under the "system" store bucket, but the object's
	// own metadata must carry no namespace so that Reference(..., "Cluster",
	// class) produces a ref that passes NormalizeAndValidateReferences (which
	// rejects cluster-scoped references that set namespace).
	return api.AutonomyClass{
		TypeMeta:   api.TypeMeta{APIVersion: api.APIVersion, Kind: "AutonomyClass"},
		ObjectMeta: api.ObjectMeta{Name: "supervised", UID: SyntheticSupervisedUID, ResourceVersion: 1},
		Spec: api.AutonomyClassSpec{RequiresApproval: true, ApprovalPolicy: &api.ApprovalPolicy{
			ToolInvocation: "always", MemoryWrite: "always", ExternalCommunication: "always",
		}},
	}
}

// EvaluateActionPolicy returns the immutable initial RuntimeApproval decision.
// Unknown policy and all currently unclassified high-risk actions fail closed
// to Pending. CostThreshold is intentionally not evaluated in this slice.
func EvaluateActionPolicy(class api.AutonomyClass, action string) (string, error) {
	if class.Spec.RequiresApproval {
		return api.RuntimeApprovalDecisionPending, nil
	}
	if class.Spec.ApprovalPolicy == nil {
		return api.RuntimeApprovalDecisionPending, nil
	}
	var value string
	switch action {
	case ActionToolInvocation:
		value = class.Spec.ApprovalPolicy.ToolInvocation
	case ActionMemoryWrite:
		value = class.Spec.ApprovalPolicy.MemoryWrite
	case ActionExternalCommunication:
		value = class.Spec.ApprovalPolicy.ExternalCommunication
	default:
		return "", fmt.Errorf("%w: unsupported protected action %q", ErrPolicyUnavailable, action)
	}
	switch value {
	case "never":
		return api.RuntimeApprovalDecisionNotRequired, nil
	case "always", "high-risk-only", "":
		return api.RuntimeApprovalDecisionPending, nil
	default:
		return api.RuntimeApprovalDecisionPending, nil
	}
}

func Reference(kind, scope string, object api.Object) api.RuntimeApprovalReference {
	metadata := object.GetObjectMeta()
	return api.RuntimeApprovalReference{
		Kind: kind, Scope: scope, Namespace: metadata.Namespace, Name: metadata.Name,
		UID: metadata.UID, ResourceVersion: metadata.ResourceVersion,
	}
}
