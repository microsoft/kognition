// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/runtime"
	"github.com/kognition-ai/kognition/internal/store"
)

// WorkflowToolInvoker executes a tool step synchronously and returns its output.
// It is the controller-side counterpart to the agent runtime's ToolDispatcher
// used by tool-typed workflow steps that have no AgentJob.
type WorkflowToolInvoker interface {
	InvokeTool(ctx context.Context, namespace, principal, toolName string, input map[string]any) (map[string]any, error)
}

type defaultWorkflowToolInvoker struct {
	store      store.Store
	grants     ToolAccessAuthorizer
	quota      ToolCallQuotaEnforcer
	dispatcher *runtime.ToolDispatcher
	logger     *zap.Logger
	tracer     *observability.Tracer
	collector  *observability.Collector
}

// ToolCallQuotaEnforcer reserves one tool call against workspace cost quota.
type ToolCallQuotaEnforcer interface {
	ReserveToolCall(context.Context, string) error
}

// WorkflowToolInvokerOption configures workflow tool execution.
type WorkflowToolInvokerOption func(*defaultWorkflowToolInvoker)

// WithWorkflowToolCallQuota enables tool-call accounting before dispatch.
func WithWorkflowToolCallQuota(quota ToolCallQuotaEnforcer) WorkflowToolInvokerOption {
	return func(invoker *defaultWorkflowToolInvoker) {
		invoker.quota = quota
	}
}

// WithWorkflowToolTracing emits Kognition semantic spans for workflow tool calls.
func WithWorkflowToolTracing(tracer *observability.Tracer, collector *observability.Collector) WorkflowToolInvokerOption {
	return func(invoker *defaultWorkflowToolInvoker) {
		invoker.tracer = tracer
		invoker.collector = collector
	}
}

// ToolAccessAuthorizer decides whether a principal may invoke a tool.
type ToolAccessAuthorizer interface {
	EvaluateToolAccess(context.Context, string, string, string, v1alpha1.ToolGrantVerb) error
}

type toolGrantInvocationAuthorizer interface {
	EvaluateToolInvocationDecision(context.Context, string, string, string, map[string]any) (ToolGrantDecision, error)
}

type policyInvocationAuthorizer interface {
	AuthorizeToolInvocation(context.Context, string, string, string, map[string]any) (ToolAuthorizationDecision, error)
}

// NewDefaultWorkflowToolInvoker returns an invoker that enforces ToolGrants and
// dispatches to the supplied runtime.ToolDispatcher.
func NewDefaultWorkflowToolInvoker(s store.Store, grants ToolAccessAuthorizer, dispatcher *runtime.ToolDispatcher, logger *zap.Logger, opts ...WorkflowToolInvokerOption) WorkflowToolInvoker {
	invoker := &defaultWorkflowToolInvoker{
		store:      s,
		grants:     grants,
		dispatcher: dispatcher,
		logger:     logger.With(zap.String("component", "workflow-tool-invoker")),
	}
	for _, opt := range opts {
		opt(invoker)
	}
	return invoker
}

func (i *defaultWorkflowToolInvoker) InvokeTool(ctx context.Context, namespace, principal, toolName string, input map[string]any) (map[string]any, error) {
	raw, err := i.store.Get(ctx, "Tool", namespace, toolName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, i.logAndReturn(namespace, principal, toolName, fmt.Errorf("tool %q not found in namespace %q", toolName, namespace))
		}
		return nil, i.logAndReturn(namespace, principal, toolName, fmt.Errorf("get tool: %w", err))
	}
	var tool v1alpha1.Tool
	if err := json.Unmarshal(raw, &tool); err != nil {
		return nil, i.logAndReturn(namespace, principal, toolName, fmt.Errorf("unmarshal tool: %w", err))
	}
	span := i.startToolSpan(ctx, namespace, principal, toolName, input, i.workflowAttribution(ctx, namespace, principal))
	var status observability.StatusCode
	var statusMsg string
	defer func(start time.Time) {
		if span == nil {
			return
		}
		span.Attributes[observability.AttrToolLatencyMs] = time.Since(start).Milliseconds()
		i.endToolSpan(ctx, span, status, statusMsg)
	}(time.Now())
	if i.grants != nil {
		var authorizationErr error
		switch authorizer := i.grants.(type) {
		case policyInvocationAuthorizer:
			_, authorizationErr = authorizer.AuthorizeToolInvocation(ctx, namespace, principal, toolName, input)
		case toolGrantInvocationAuthorizer:
			_, authorizationErr = authorizer.EvaluateToolInvocationDecision(ctx, namespace, principal, toolName, input)
		default:
			authorizationErr = errors.New("workflow tool invoker requires invocation-aware authorization")
		}
		if authorizationErr != nil {
			status = observability.StatusError
			statusMsg = authorizationErr.Error()
			setSpanAttribute(span, observability.AttrErrorMessage, authorizationErr.Error())
			return nil, i.logAndReturn(namespace, principal, toolName, authorizationErr)
		}
	}
	if i.quota != nil {
		if err := i.quota.ReserveToolCall(ctx, namespace); err != nil {
			wrappedErr := fmt.Errorf("reserve tool-call quota: %w", err)
			status = observability.StatusError
			statusMsg = wrappedErr.Error()
			setSpanAttribute(span, observability.AttrErrorMessage, wrappedErr.Error())
			return nil, i.logAndReturn(namespace, principal, toolName, wrappedErr)
		}
	}
	res, err := i.dispatcher.Execute(ctx, runtime.ToolExecutionRequest{Tool: tool, Input: input})
	if err != nil {
		status = observability.StatusError
		statusMsg = err.Error()
		setSpanAttribute(span, observability.AttrErrorMessage, err.Error())
		return nil, i.logAndReturn(namespace, principal, toolName, err)
	}
	status = observability.StatusOK
	setSpanAttribute(span, observability.AttrToolSandboxTier, res.ExecutionMode)
	setSpanAttribute(span, observability.AttrToolResult, res.Output)
	if res.Duration > 0 {
		setSpanAttribute(span, observability.AttrToolLatencyMs, res.Duration.Milliseconds())
	}
	return res.Output, nil
}

func setSpanAttribute(span *observability.Span, key string, value interface{}) {
	if span == nil {
		return
	}
	span.Attributes[key] = value
}

func (i *defaultWorkflowToolInvoker) startToolSpan(ctx context.Context, namespace, principal, toolName string, input map[string]any, attribution workflowAttribution) *observability.Span {
	if i.tracer == nil {
		return nil
	}
	return i.tracer.StartSpan(ctx, observability.SpanNameToolCall, observability.SpanKindClient, map[string]interface{}{
		observability.AttrWorkspace:      namespace,
		observability.AttrAgentNamespace: namespace,
		observability.AttrAgentIdentity:  principal,
		observability.AttrSolution:       attribution.solution,
		observability.AttrSolutionRev:    attribution.solutionRev,
		observability.AttrToolName:       toolName,
		observability.AttrToolArgs:       input,
	})
}

type workflowAttribution struct {
	solution    string
	solutionRev int64
}

func (i *defaultWorkflowToolInvoker) workflowAttribution(ctx context.Context, namespace, principal string) workflowAttribution {
	workflowName, ok := strings.CutPrefix(principal, "workflow:")
	if !ok || workflowName == "" {
		return workflowAttribution{}
	}
	rawWorkflow, err := i.store.Get(ctx, "Workflow", namespace, workflowName)
	if err != nil {
		return workflowAttribution{}
	}
	var workflow v1alpha1.Workflow
	if json.Unmarshal(rawWorkflow, &workflow) != nil || workflow.ObjectMeta.Labels == nil {
		return workflowAttribution{}
	}
	solutionName := workflow.ObjectMeta.Labels["kognition.dev/solution"]
	if solutionName == "" {
		return workflowAttribution{}
	}
	rawSolution, err := i.store.Get(ctx, "Solution", namespace, solutionName)
	if err != nil {
		return workflowAttribution{solution: solutionName}
	}
	var solution v1alpha1.Solution
	if json.Unmarshal(rawSolution, &solution) != nil {
		return workflowAttribution{solution: solutionName}
	}
	return workflowAttribution{solution: solutionName, solutionRev: solution.Status.ActiveRevision}
}

func (i *defaultWorkflowToolInvoker) endToolSpan(ctx context.Context, span *observability.Span, status observability.StatusCode, statusMsg string) {
	if status == observability.StatusUnset {
		status = observability.StatusOK
	}
	if err := i.tracer.EndSpan(span, status, statusMsg); err != nil {
		i.logger.Warn("failed to end workflow tool span", zap.Error(err))
	}
	if i.collector == nil || span.Context.TraceFlags&0x01 == 0 {
		return
	}
	if err := i.collector.FlushSpans(ctx, []*observability.Span{span}); err != nil {
		i.logger.Warn("failed to export workflow tool span", zap.Error(err))
	}
}

// logAndReturn records the invocation failure without raw error content so it is
// observable even when the reconciler's event stream is a no-op (K8s mode).
func (i *defaultWorkflowToolInvoker) logAndReturn(namespace, principal, toolName string, err error) error {
	i.logger.Warn("tool invocation failed",
		zap.String("namespace", namespace),
		zap.String("principal", principal),
		zap.String("tool", toolName),
	)
	return err
}
