// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package postgres

import (
	"context"
	"fmt"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/auth"
	"github.com/kognition-ai/kognition/internal/controlplaneauth"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

func (s *Store) AuthorizeWorkflowExecutorTarget(ctx context.Context, action runtimeapproval.WorkflowExecutorAction) error {
	executor, err := runtimeapproval.AuthenticatedWorkflowExecutor(ctx)
	if err != nil {
		return err
	}
	if executor != action.Executor {
		return controlplaneauth.ErrForbidden
	}
	return runtimeapproval.AuthorizeWorkflowExecutorTarget(ctx, s, action,
		func(ctx context.Context, action runtimeapproval.WorkflowExecutorAction) error {
			for _, reference := range []api.RuntimeApprovalReference{action.Target, action.Service} {
				if err := runtimeapproval.AuthorizeWorkflowExecutorReference(ctx, s, action.Activation.Workspace, reference); err != nil {
					return err
				}
			}
			identity, _ := auth.IdentityFromContext(ctx)
			return controlplaneauth.New(s).AuthorizeAgentInvocation(ctx, identity, action.Target.Namespace, action.Target.Name, action.Target.UID)
		})
}

func (s *Store) GetWorkflowExecutorResource(ctx context.Context, kind, namespace, name string) ([]byte, error) {
	if kind != "Agent" && kind != "AgentService" && kind != "Workflow" {
		return nil, fmt.Errorf("unsupported workflow executor target kind %q", kind)
	}
	return s.Get(ctx, kind, namespace, name)
}

func (s *Store) AuthorizeWorkflowExecutorStep(ctx context.Context, authority runtimeapproval.WorkflowExecutorAuthority, action runtimeapproval.WorkflowExecutorAction) error {
	return runtimeapproval.AuthorizeWorkflowExecutorStep(ctx, s, authority, action, s.AuthorizeWorkflowExecutorTarget)
}

func (s *Store) ListWorkflowExecutorResources(ctx context.Context, kind, namespace string) ([][]byte, error) {
	if namespace == "" || kind != "AgentService" && kind != "CrossNamespaceGrant" {
		return nil, runtimeapproval.ErrWorkflowDispatchUnsupported
	}
	result, err := s.List(ctx, kind, store.ListOptions{Namespace: namespace, Limit: 257})
	if err != nil {
		return nil, err
	}
	if len(result) > 256 {
		return nil, fmt.Errorf("workflow executor resource discovery exceeds its finite bound")
	}
	return result, nil
}
