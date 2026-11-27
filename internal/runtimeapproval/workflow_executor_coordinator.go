// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeapproval

import (
	"context"
	"fmt"
	"time"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

type WorkflowExecutorAuthorizer interface {
	AuthorizeWorkflowExecutorAction(context.Context, WorkflowExecutorAction) (string, error)
	AuthorizeWorkflowExecutorActivation(context.Context, WorkflowExecutorAction, store.WorkflowActivation) error
}

type WorkflowExecutorCoordinatorConfig struct {
	Store      Gate3Store
	Keys       BindingKeyStore
	Authorizer WorkflowExecutorAuthorizer
	Dispatch   *WorkflowDispatchGate
	Clock      func() (time.Time, error)
	TTL        time.Duration
}

type WorkflowExecutorCoordinator struct {
	config WorkflowExecutorCoordinatorConfig
}

func NewWorkflowExecutorCoordinator(config WorkflowExecutorCoordinatorConfig) (*WorkflowExecutorCoordinator, error) {
	if config.Store == nil || config.Keys == nil || config.Authorizer == nil || config.Dispatch == nil || config.Clock == nil ||
		config.TTL <= 0 || config.TTL > maxApprovalTTL {
		return nil, ErrWorkflowDispatchUnsupported
	}
	dispatchConfig := config.Dispatch.config
	previousEligibility := dispatchConfig.Eligibility
	dispatchConfig.Eligibility = func(ctx context.Context, approval api.RuntimeApproval) error {
		if err := previousEligibility(ctx, approval); err != nil {
			return err
		}
		if approval.Spec.ActorKind != api.RuntimeApprovalActorExecutor {
			return nil
		}
		action, err := WorkflowExecutorActionFromApproval(approval, nil)
		if err != nil {
			return err
		}
		decision, err := authorizeExecutorActivation(ctx, config, action)
		if err != nil {
			return err
		}
		switch decision {
		case api.RuntimeApprovalDecisionNotRequired:
			return nil
		case api.RuntimeApprovalDecisionPending:
			if approval.Status.DecisionState == api.RuntimeApprovalDecisionPending || approval.Status.DecisionState == api.RuntimeApprovalDecisionApproved {
				return nil
			}
		}
		return fmt.Errorf("current executor policy does not permit the retained decision")
	}
	dispatch, err := NewWorkflowDispatchGate(dispatchConfig)
	if err != nil {
		return nil, err
	}
	config.Dispatch = dispatch
	return &WorkflowExecutorCoordinator{config: config}, nil
}

func (coordinator *WorkflowExecutorCoordinator) Resolve(ctx context.Context, action WorkflowExecutorAction) (Gate3Result, error) {
	if coordinator == nil {
		return Gate3Result{}, ErrWorkflowDispatchUnsupported
	}
	now, err := coordinator.config.Clock()
	if err != nil {
		return Gate3Result{}, err
	}
	if err := ValidateWorkflowExecutorAction(action, now); err != nil {
		return Gate3Result{}, err
	}
	initialDecision, err := authorizeExecutorActivation(ctx, coordinator.config, action)
	if err != nil {
		return Gate3Result{}, err
	}
	proposed, err := BuildWorkflowExecutorApproval(ctx, coordinator.config.Keys, action, initialDecision, now)
	if err != nil {
		return Gate3Result{}, err
	}
	_, stored, err := coordinator.config.Store.CreateOrGetRuntimeApproval(ctx, proposed, action.Payload, coordinator.config.TTL)
	if err != nil {
		return Gate3Result{}, fmt.Errorf("persist executor action decision: %w", err)
	}
	result := Gate3Result{Approval: stored, ExpiresAt: stored.Spec.ExpiresAt}
	if stored.Spec.ActorKind != api.RuntimeApprovalActorExecutor || !SameImmutableBinding(stored, proposed) {
		return result, fmt.Errorf("executor action decision changed its immutable binding")
	}
	if err := AuthenticateRetainedBinding(ctx, coordinator.config.Keys, stored, action.Payload); err != nil {
		return result, err
	}
	if stored.Status.DecisionState == api.RuntimeApprovalDecisionPending || PermitsExecution(stored.Status.DecisionState) {
		if err := coordinator.config.Dispatch.Register(ctx, stored, action.Payload); err != nil {
			return result, err
		}
	}
	switch stored.Status.DecisionState {
	case api.RuntimeApprovalDecisionPending:
		result.State = Gate3StatePending
	case api.RuntimeApprovalDecisionApproved, api.RuntimeApprovalDecisionNotRequired:
		result.State = Gate3StateReady
	case api.RuntimeApprovalDecisionRejected:
		result.State = Gate3StateRejected
	case api.RuntimeApprovalDecisionExpired:
		result.State = Gate3StateExpired
	default:
		return result, fmt.Errorf("executor decision has unsupported state %q", stored.Status.DecisionState)
	}
	if IsTerminalDecision(stored.Status.DecisionState) && !stored.Status.DecisionEvidenceDelivered {
		if stored.Status.DecisionAudit == nil {
			return result, fmt.Errorf("executor decision lacks durable audit evidence")
		}
		acked, err := coordinator.config.Store.AcknowledgeRuntimeApprovalDecisionAudit(ctx, stored.Spec.Workspace, stored.Spec.InvocationID,
			stored.ResourceVersion, stored.Status.DecisionAudit.ID)
		if err != nil {
			result.State = ""
			return result, err
		}
		result.Approval = acked
	}
	return result, nil
}

func authorizeExecutorActivation(ctx context.Context, config WorkflowExecutorCoordinatorConfig, action WorkflowExecutorAction) (string, error) {
	decision, err := config.Authorizer.AuthorizeWorkflowExecutorAction(ctx, action)
	if err != nil {
		return "", err
	}
	activation, err := config.Dispatch.config.Activations.GetWorkflowActivation(ctx, action.Activation)
	if err != nil {
		return "", err
	}
	if err := config.Authorizer.AuthorizeWorkflowExecutorActivation(ctx, action, activation); err != nil {
		return "", err
	}
	return decision, nil
}

func (coordinator *WorkflowExecutorCoordinator) Dispatch(ctx context.Context, approval api.RuntimeApproval, payload []byte, target func(context.Context) error) (WorkflowDispatchResult, error) {
	if coordinator == nil || approval.Spec.ActorKind != api.RuntimeApprovalActorExecutor {
		return WorkflowDispatchResult{}, ErrWorkflowDispatchUnsupported
	}
	return coordinator.config.Dispatch.Dispatch(ctx, approval, payload, target)
}
