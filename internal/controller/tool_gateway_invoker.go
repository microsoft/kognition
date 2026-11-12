// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"

	"github.com/kognition-ai/kognition/internal/invocation"
	"github.com/kognition-ai/kognition/internal/runtime"
	"github.com/kognition-ai/kognition/internal/store"
	"github.com/kognition-ai/kognition/internal/toolgateway"
	"go.uber.org/zap"
)

type unavailableToolGatewayInvoker struct{ err error }

func (i unavailableToolGatewayInvoker) InvokeTool(context.Context, string, string, string, map[string]any) (map[string]any, error) {
	return nil, fmt.Errorf("Tool Gateway is unavailable: %w", i.err)
}

// NewUnavailableToolGatewayInvoker prevents a deployment wiring error from
// falling back to a direct tool executor that could bypass gateway guarantees.
func NewUnavailableToolGatewayInvoker(err error) WorkflowToolInvoker {
	return unavailableToolGatewayInvoker{err: err}
}

// NewToolGatewayInvoker builds the parity-preserving in-process Tool Gateway
// used by standalone chat and controller-side workflow tool steps. Kubernetes
// runtime pods use the HTTP server over the same Tool Gateway core.
func NewToolGatewayInvoker(st store.Store, policies *InvocationPolicyEngine, quota *QuotaEnforcer, dispatcher *runtime.ToolDispatcher, logger *zap.Logger, tracer invocation.Tracer, additionalOptions ...toolgateway.Option) (WorkflowToolInvoker, error) {
	options := []toolgateway.Option{toolgateway.WithLedger(toolgateway.NewStoreLedger(st))}
	if tracer != nil {
		options = append(options, toolgateway.WithInvocationTracer(tracer))
	}
	options = append(options, additionalOptions...)
	return toolgateway.New(st, NewToolGatewayAuthorizer(policies), quota, toolgateway.NewInvocationRunExecutor(st, dispatcher), logger, options...)
}
