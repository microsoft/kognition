// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtimeapproval

import (
	"context"
	"fmt"
	"time"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/auth"
	"github.com/kognition-ai/kognition/internal/store"
)

func AuthenticatedWorkflowExecutor(ctx context.Context) (api.ControlPlaneRoleSubject, error) {
	if err := ctx.Err(); err != nil {
		return api.ControlPlaneRoleSubject{}, err
	}
	identity, ok := auth.IdentityFromContext(ctx)
	if !ok || identity == nil || identity.Method == "" || identity.Method == "anonymous" || identity.Method == "token" ||
		identity.Issuer == "" || identity.Subject == "" {
		return api.ControlPlaneRoleSubject{}, fmt.Errorf("authenticated Workflow executor identity is required")
	}
	return api.ControlPlaneRoleSubject{Method: identity.Method, Issuer: identity.Issuer, Subject: identity.Subject}, nil
}

type WorkflowExecutorTarget struct {
	Agent           api.RuntimeApprovalReference `json:"agent"`
	Service         api.RuntimeApprovalReference `json:"service"`
	InitialDecision string                       `json:"initialDecision"`
	Operation       string                       `json:"operation"`
}

type WorkflowExecutorLimits struct {
	MaxEffects              int   `json:"maxEffects"`
	MaxPayloadBytes         int   `json:"maxPayloadBytes"`
	MaxDurationSeconds      int64 `json:"maxDurationSeconds"`
	MaxMembers              int   `json:"maxMembers"`
	MaxTurns                int   `json:"maxTurns"`
	MaxMessages             int   `json:"maxMessages"`
	MaxDelegations          int   `json:"maxDelegations"`
	MaxHandoffDepth         int   `json:"maxHandoffDepth"`
	MaxParallelTasks        int   `json:"maxParallelTasks"`
	ContentRetentionSeconds int64 `json:"contentRetentionSeconds"`
	ReplayRetentionSeconds  int64 `json:"replayRetentionSeconds"`
}

type WorkflowExecutorAuthority struct {
	PolicyVersion          string                       `json:"policyVersion"`
	Reference              api.RuntimeApprovalReference `json:"reference"`
	Workflow               api.RuntimeApprovalReference `json:"workflow"`
	WorkflowRevisionDigest string                       `json:"workflowRevisionDigest"`
	Executor               api.ControlPlaneRoleSubject  `json:"executor"`
	Enabled                bool                         `json:"enabled"`
	NotBefore              time.Time                    `json:"notBefore"`
	ExpiresAt              time.Time                    `json:"expiresAt"`
	Targets                []WorkflowExecutorTarget     `json:"targets"`
	ControllerUIDs         []string                     `json:"controllerUIDs"`
	Limits                 WorkflowExecutorLimits       `json:"limits"`
	ApprovedBy             api.ControlPlaneRoleSubject  `json:"approvedBy"`
}

type WorkflowRunAcceptance struct {
	Workspace              string                       `json:"workspace"`
	RunUID                 string                       `json:"runUID"`
	Workflow               api.RuntimeApprovalReference `json:"workflow"`
	WorkflowRevisionDigest string                       `json:"workflowRevisionDigest"`
	ExecutionBinding       api.RuntimeApprovalReference `json:"executionBinding"`
	Executor               api.ControlPlaneRoleSubject  `json:"executor"`
	Initiator              api.ControlPlaneRoleSubject  `json:"initiator"`
	BindingDigest          string                       `json:"bindingDigest"`
	AcceptedAt             time.Time                    `json:"acceptedAt"`
	ExpiresAt              time.Time                    `json:"expiresAt"`
	Run                    api.RuntimeApprovalReference `json:"run"`
	RequestDigest          string                       `json:"requestDigest"`
}

type WorkflowExecutorAuthorityReader interface {
	GetWorkflowExecutorAuthority(context.Context, api.RuntimeApprovalReference) (WorkflowExecutorAuthority, error)
	GetWorkflowRunAcceptance(context.Context, string, string) (WorkflowRunAcceptance, error)
	AuthorizeWorkflowExecutorStep(context.Context, WorkflowExecutorAuthority, WorkflowExecutorAction) error
	AuthorizeWorkflowExecutorTarget(context.Context, WorkflowExecutorAction) error
}

type WorkflowExecutorAuthorityConfig struct {
	Reader               WorkflowExecutorAuthorityReader
	CurrentExecutor      func(context.Context) (api.ControlPlaneRoleSubject, error)
	CurrentControllerUID func(context.Context) (string, error)
	Clock                func() (time.Time, error)
}

type LiveWorkflowExecutorAuthorizer struct {
	config WorkflowExecutorAuthorityConfig
}

func NewLiveWorkflowExecutorAuthorizer(config WorkflowExecutorAuthorityConfig) (*LiveWorkflowExecutorAuthorizer, error) {
	if config.Reader == nil || config.CurrentExecutor == nil || config.CurrentControllerUID == nil || config.Clock == nil {
		return nil, ErrWorkflowDispatchUnsupported
	}
	return &LiveWorkflowExecutorAuthorizer{config: config}, nil
}

func (authorizer *LiveWorkflowExecutorAuthorizer) ApprovedWorkflowExecutorLimits(ctx context.Context, action WorkflowExecutorAction) (WorkflowExecutorLimits, error) {
	if _, err := authorizer.AuthorizeWorkflowExecutorAction(ctx, action); err != nil {
		return WorkflowExecutorLimits{}, err
	}
	authority, err := authorizer.config.Reader.GetWorkflowExecutorAuthority(ctx, action.ExecutionBinding)
	if err != nil {
		return WorkflowExecutorLimits{}, err
	}
	if err := ValidateWorkflowExecutorAuthority(authority); err != nil {
		return WorkflowExecutorLimits{}, err
	}
	acceptance, err := authorizer.config.Reader.GetWorkflowRunAcceptance(ctx, action.Activation.Workspace, action.Activation.RunUID)
	if err != nil {
		return WorkflowExecutorLimits{}, err
	}
	digest, err := WorkflowRunExecutionScopeDigest(acceptance, authority)
	if err != nil || digest != action.BindingDigest || acceptance.BindingDigest != action.BindingDigest ||
		authority.Reference != action.ExecutionBinding || authority.Workflow != action.Workflow ||
		authority.WorkflowRevisionDigest != action.WorkflowRevisionDigest || authority.Executor != action.Executor || !authority.Enabled {
		return WorkflowExecutorLimits{}, fmt.Errorf("executor limits do not match the protected action scope")
	}
	if _, err := authorizer.AuthorizeWorkflowExecutorAction(ctx, action); err != nil {
		return WorkflowExecutorLimits{}, err
	}
	return authority.Limits, nil
}

func (authorizer *LiveWorkflowExecutorAuthorizer) AuthorizeWorkflowExecutorActivation(ctx context.Context, action WorkflowExecutorAction, activation store.WorkflowActivation) error {
	if authorizer == nil {
		return ErrWorkflowDispatchUnsupported
	}
	authority, err := authorizer.config.Reader.GetWorkflowExecutorAuthority(ctx, action.ExecutionBinding)
	if err != nil {
		return err
	}
	if err := ValidateWorkflowExecutorAuthority(authority); err != nil {
		return err
	}
	acceptance, err := authorizer.config.Reader.GetWorkflowRunAcceptance(ctx, action.Activation.Workspace, action.Activation.RunUID)
	if err != nil {
		return err
	}
	now, err := authorizer.config.Clock()
	if err != nil {
		return err
	}
	if err := ValidateWorkflowRunAcceptanceAuthority(acceptance, authority, now); err != nil {
		return err
	}
	if _, err := store.NewWorkflowActivation(activation.Spec, now); err != nil {
		return err
	}
	if !authority.Enabled || authority.Reference != action.ExecutionBinding || acceptance.BindingDigest != action.BindingDigest ||
		activation.Spec.Key != action.Activation || activation.Spec.BindingDigest != action.BindingDigest || activation.Mode != store.WorkflowGateActive || activation.Epoch != action.OwnerEpoch ||
		activation.Spec.MaxEffects > authority.Limits.MaxEffects || activation.Spec.MaxBytes > authority.Limits.MaxPayloadBytes ||
		activation.Spec.MaxHandoffs > authority.Limits.MaxDelegations || activation.Spec.ExpiresAt.After(acceptance.ExpiresAt) || action.ExpiresAt.After(activation.Spec.ExpiresAt) {
		return fmt.Errorf("activation exceeds its approved executor authority")
	}
	return ctx.Err()
}

func (authorizer *LiveWorkflowExecutorAuthorizer) AuthorizeWorkflowExecutorAction(ctx context.Context, action WorkflowExecutorAction) (string, error) {
	if authorizer == nil {
		return "", ErrWorkflowDispatchUnsupported
	}
	executor, err := authorizer.config.CurrentExecutor(ctx)
	if err != nil {
		return "", err
	}
	if executor != action.Executor {
		return "", fmt.Errorf("authenticated executor does not own this action")
	}
	controllerUID, err := authorizer.config.CurrentControllerUID(ctx)
	if err != nil {
		return "", err
	}
	return authorizer.authorizeWorkflowExecutorPolicy(ctx, action, executor, controllerUID)
}

func (authorizer *LiveWorkflowExecutorAuthorizer) authorizeWorkflowExecutorPolicy(ctx context.Context, action WorkflowExecutorAction, executor api.ControlPlaneRoleSubject, controllerUID string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	now, err := authorizer.config.Clock()
	if err != nil {
		return "", err
	}
	if now.IsZero() || !action.ExpiresAt.After(now) || action.ExpiresAt.Sub(now) > maxApprovalTTL {
		return "", fmt.Errorf("executor action authority has expired or exceeds its maximum lifetime")
	}
	if err := validateExecutorActionIdentity(action); err != nil {
		return "", err
	}
	authority, err := authorizer.config.Reader.GetWorkflowExecutorAuthority(ctx, action.ExecutionBinding)
	if err != nil {
		return "", fmt.Errorf("read live executor authority: %w", err)
	}
	if err := ValidateWorkflowExecutorAuthority(authority); err != nil {
		return "", err
	}
	approvedController := false
	for _, approvedUID := range authority.ControllerUIDs {
		approvedController = approvedController || approvedUID == controllerUID
	}
	if controllerUID == "" || !approvedController || len(action.Payload) > authority.Limits.MaxPayloadBytes {
		return "", fmt.Errorf("executor controller incarnation or payload exceeds its approved authority")
	}
	if !authority.Enabled || authority.Reference != action.ExecutionBinding || authority.Workflow != action.Workflow ||
		authority.WorkflowRevisionDigest != action.WorkflowRevisionDigest || authority.Executor != executor ||
		authority.NotBefore.IsZero() || authority.NotBefore.After(now) || !authority.ExpiresAt.After(now) ||
		authority.ExpiresAt.Sub(authority.NotBefore) > maxApprovalTTL || action.ExpiresAt.After(authority.ExpiresAt) ||
		len(authority.Targets) == 0 || len(authority.Targets) > 8 {
		return "", fmt.Errorf("live executor authority does not match the accepted action")
	}
	acceptance, err := authorizer.config.Reader.GetWorkflowRunAcceptance(ctx, action.Activation.Workspace, action.Activation.RunUID)
	if err != nil {
		return "", fmt.Errorf("read protected run acceptance: %w", err)
	}
	if err := ValidateWorkflowRunAcceptanceAuthority(acceptance, authority, now); err != nil {
		return "", err
	}
	scopeDigest, err := WorkflowRunExecutionScopeDigest(acceptance, authority)
	if err != nil || acceptance.BindingDigest != scopeDigest {
		return "", fmt.Errorf("protected execution scope does not match its approved authority")
	}
	if acceptance.Workspace != action.Activation.Workspace || acceptance.RunUID != action.Activation.RunUID ||
		acceptance.Workflow != action.Workflow || acceptance.WorkflowRevisionDigest != action.WorkflowRevisionDigest ||
		acceptance.ExecutionBinding != action.ExecutionBinding || acceptance.Executor != executor || acceptance.Initiator != action.Initiator ||
		acceptance.BindingDigest != action.BindingDigest || acceptance.AcceptedAt.IsZero() || acceptance.AcceptedAt.After(now) ||
		acceptance.AcceptedAt.Before(authority.NotBefore) || !acceptance.ExpiresAt.After(now) ||
		acceptance.ExpiresAt.After(authority.ExpiresAt) || action.ExpiresAt.After(acceptance.ExpiresAt) {
		return "", fmt.Errorf("protected run acceptance does not authorize this action")
	}
	decision := ""
	for _, target := range authority.Targets {
		if target.InitialDecision != api.RuntimeApprovalDecisionPending && target.InitialDecision != api.RuntimeApprovalDecisionNotRequired {
			return "", fmt.Errorf("executor authority contains an unsupported initiation decision")
		}
		if target.Agent == action.Target && target.Service == action.Service && target.Operation == action.Operation {
			if decision != "" {
				return "", fmt.Errorf("executor authority contains an ambiguous target")
			}
			decision = target.InitialDecision
		}
	}
	if decision == "" {
		return "", fmt.Errorf("executor authority does not permit this exact target")
	}
	if err := authorizer.config.Reader.AuthorizeWorkflowExecutorStep(ctx, authority, action); err != nil {
		return "", fmt.Errorf("live collaboration member authorization: %w", err)
	}
	if err := authorizer.config.Reader.AuthorizeWorkflowExecutorTarget(ctx, action); err != nil {
		return "", fmt.Errorf("live executor target authorization: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	confirmedNow, err := authorizer.config.Clock()
	if err != nil {
		return "", err
	}
	if confirmedNow.IsZero() || confirmedNow.Before(now) || !action.ExpiresAt.After(confirmedNow) ||
		!authority.ExpiresAt.After(confirmedNow) || !acceptance.ExpiresAt.After(confirmedNow) {
		return "", fmt.Errorf("executor authority expired or its clock became invalid during live checks")
	}
	return decision, nil
}
