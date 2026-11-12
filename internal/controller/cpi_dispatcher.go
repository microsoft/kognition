// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/modelgateway"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/store"
)

// cpiDispatcher implements RuntimeDispatcher by executing agent completions
// synchronously via the CPI driver registry.
type cpiDispatcher struct {
	store    store.Store
	registry *cpi.Registry
	logger   *zap.Logger
	core     *modelgateway.Core
	usage    observability.UsageEventWriter
	audit    *observability.CognitiveAuditLog

	mu      sync.RWMutex
	results map[string]cpiDispatchResult
}

type cpiDispatchResult struct {
	namespace   string
	operation   string
	response    *cpi.CompletionResponse
	completedAt time.Time
}

// ErrDispatchAccountingUncertain means provider execution completed but its
// usage could not be durably accounted. Retrying could repeat a paid call.
var ErrDispatchAccountingUncertain = errors.New("dispatch usage accounting uncertain")

const (
	modelDispatchStartedReason   = "ModelDispatchStarted"
	modelDispatchAccountedReason = "ModelDispatchAccounted"
	cpiDispatchResultTTL         = 24 * time.Hour
)

// SetModelGatewayCore enables the shared live Model authorization boundary
// before each standalone provider completion.
func (d *cpiDispatcher) SetModelGatewayCore(core *modelgateway.Core) {
	d.core = core
}

func (d *cpiDispatcher) SetAuditLog(audit *observability.CognitiveAuditLog) {
	d.audit = audit
}

// SetUsageEventWriter overrides the immutable usage writer used after provider
// execution. Processes use this to share durable economic instrumentation.
func (d *cpiDispatcher) SetUsageEventWriter(usage observability.UsageEventWriter) {
	d.usage = usage
}

// NewCPIDispatcher creates a RuntimeDispatcher backed by the CPI driver registry.
func NewCPIDispatcher(s store.Store, registry *cpi.Registry, logger *zap.Logger) *cpiDispatcher {
	dispatcher := &cpiDispatcher{
		store:    s,
		registry: registry,
		logger:   logger,
		results:  make(map[string]cpiDispatchResult),
	}
	if usage, ok := s.(observability.UsageEventWriter); ok {
		dispatcher.usage = usage
	}
	return dispatcher
}

func (d *cpiDispatcher) Dispatch(ctx context.Context, instance *v1alpha1.AgentInstance) (string, error) {
	d.pruneResults(time.Now().UTC())
	namespace := instance.ObjectMeta.Namespace
	sessionID := uuid.New().String()
	durable, hasDurable := d.store.(store.ModelDispatchStore)
	operationKey := agentInstanceReservationID(instance)
	if hasDurable && instance.Spec.SessionContext["input"] != "" {
		record, err := durable.GetModelDispatch(ctx, namespace, operationKey)
		if err == nil {
			if len(record.Response) == 0 || (instance.Status.SessionID != "" && instance.Status.SessionID != record.SessionID) {
				return "", ErrDispatchAccountingUncertain
			}
			var response *cpi.CompletionResponse
			if err := json.Unmarshal(record.Response, &response); err != nil || response == nil || !response.Usage.Reported {
				return "", fmt.Errorf("%w: invalid stored completion", ErrDispatchAccountingUncertain)
			}
			return record.SessionID, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return "", fmt.Errorf("load model dispatch: %w", err)
		}
	}

	// Resolve Agent → Model → ModelClass chain.
	agent, err := ResolveExecutionAgent(ctx, d.store, namespace, instance)
	if err != nil {
		return "", fmt.Errorf("load agent %s: %w", instance.Spec.AgentRef.Name, err)
	}
	if agent.Spec.Runtime != nil {
		switch class := strings.TrimSpace(agent.Spec.Runtime.Class); class {
		case "", "default", "native":
		default:
			return "", fmt.Errorf("CPI dispatcher does not support runtime class %q", class)
		}
	}

	var model *v1alpha1.Model
	var modelClass *v1alpha1.ModelClass
	if d.core != nil {
		authorized, err := d.core.AuthorizeExecution(ctx, namespace, instance.Spec.AgentRef.Name, agent.Spec.ModelRef.Name)
		if err != nil {
			decision := modelgateway.StandaloneDecisionInput{Agent: agent, Workspace: namespace, SessionID: sessionID, InstanceName: instance.ObjectMeta.Name, InstanceUID: instance.ObjectMeta.UID, ModelName: agent.Spec.ModelRef.Name}
			if auditErr := modelgateway.RecordStandaloneDecision(ctx, d.audit, decision, modelgateway.DeniedOutcome(err), modelgateway.DecisionReasonCode(err), "blocked", false, true); auditErr != nil {
				return "", fmt.Errorf("persist denied model evidence: %w", auditErr)
			}
			return "", fmt.Errorf("authorize model execution: %w", err)
		}
		model = authorized.Model
		modelClass = authorized.ModelClass
		if modelClass == nil {
			return "", fmt.Errorf("authorized model %s has no ModelClass", model.ObjectMeta.Name)
		}
	} else {
		model, err = getStoredResource[v1alpha1.Model](ctx, d.store, "Model", namespace, agent.Spec.ModelRef.Name)
		if err != nil {
			return "", fmt.Errorf("load model %s: %w", agent.Spec.ModelRef.Name, err)
		}
		modelClass, err = getStoredResource[v1alpha1.ModelClass](ctx, d.store, "ModelClass", "", model.Spec.ClassName)
		if err != nil {
			return "", fmt.Errorf("load model class %s: %w", model.Spec.ClassName, err)
		}
	}

	// Get the CPI driver for this provisioner.
	driver, ok := d.registry.Get(modelClass.Spec.Provisioner)
	if !ok {
		return "", fmt.Errorf("no CPI driver registered for provisioner %q", modelClass.Spec.Provisioner)
	}

	// Resolve credentials from SecretRef.
	var credentials *cpi.Credentials
	if model.Spec.CredentialRef != nil && model.Spec.CredentialRef.Name != "" {
		creds, err := d.resolveCredentials(ctx, namespace, model.Spec.CredentialRef.Name)
		if err != nil {
			return "", fmt.Errorf("resolve credentials: %w", err)
		}
		credentials = creds
	}

	// Build messages from agent instructions and user input.
	var messages []cpi.Message
	if agent.Spec.Instructions.Inline != "" {
		messages = append(messages, cpi.Message{
			Role:    "system",
			Content: agent.Spec.Instructions.Inline,
		})
	}
	if input := instance.Spec.SessionContext["input"]; input != "" {
		messages = append(messages, cpi.Message{
			Role:    "user",
			Content: input,
		})
	}

	req := &cpi.CompletionRequest{
		Model:      model.Spec.ModelID,
		Endpoint:   model.Spec.Endpoint,
		Creds:      credentials,
		Messages:   messages,
		Parameters: model.Spec.Parameters,
	}

	if instance.Spec.SessionContext["input"] == "" {
		d.logger.Info("dispatch skipping completion: empty input",
			zap.String("sessionID", sessionID),
			zap.String("agent", instance.Spec.AgentRef.Name),
		)
		return sessionID, nil
	}
	if uncertain, err := d.hasUnfinishedModelDispatch(ctx, instance, operationKey); err != nil {
		return "", fmt.Errorf("inspect model dispatch marker: %w", err)
	} else if uncertain {
		return "", fmt.Errorf("%w: prior provider execution has no safely replayable result", ErrDispatchAccountingUncertain)
	}
	modelDecision := modelgateway.StandaloneDecisionInput{Agent: agent, Workspace: namespace, SessionID: sessionID, InstanceName: instance.ObjectMeta.Name, InstanceUID: instance.ObjectMeta.UID, ModelName: agent.Spec.ModelRef.Name, Model: model}
	if err := modelgateway.RecordStandaloneDecision(ctx, d.audit, modelDecision, "allowed", "authorized", "not_started", false, true); err != nil {
		return "", fmt.Errorf("persist model authorization evidence: %w", err)
	}
	if hasDurable {
		claimed, err := durable.ClaimModelDispatch(ctx, namespace, operationKey, sessionID)
		if err != nil {
			return "", fmt.Errorf("claim model dispatch: %w", err)
		}
		if !claimed {
			return "", ErrDispatchAccountingUncertain
		}
	}
	if err := d.appendDispatchMarker(ctx, instance, modelDispatchStartedReason, operationKey); err != nil {
		return "", fmt.Errorf("persist model dispatch marker: %w", err)
	}

	startedAt := time.Now().UTC()
	resp, err := driver.Complete(ctx, req)
	if err != nil {
		if auditErr := modelgateway.RecordStandaloneDecision(ctx, d.audit, modelDecision, "failed", "target_failed", "failed", true, false); auditErr != nil {
			d.logger.Error("persist failed model evidence", zap.Error(auditErr))
		}
		d.logger.Error("CPI completion outcome is uncertain", zap.String("agent", instance.Spec.AgentRef.Name), zap.String("model", model.Spec.ModelID), zap.Error(err))
		return "", fmt.Errorf("%w: provider completion outcome is unknown", ErrDispatchAccountingUncertain)
	}
	if resp == nil || !resp.Usage.Reported {
		if auditErr := modelgateway.RecordStandaloneDecision(ctx, d.audit, modelDecision, "failed", "provider_usage_missing", "failed", true, false); auditErr != nil {
			d.logger.Error("persist missing-usage model evidence", zap.Error(auditErr))
		}
		d.logger.Error("model provider response omitted token usage", zap.String("agent", instance.Spec.AgentRef.Name), zap.String("model", model.Spec.ModelID))
		return "", fmt.Errorf("%w: provider omitted usage", ErrDispatchAccountingUncertain)
	}
	if err := d.recordUsageEvent(ctx, instance, agent, model, modelClass, sessionID, resp.Usage, startedAt, time.Now().UTC()); err != nil {
		d.logger.Error("persist model usage", zap.String("agent", instance.Spec.AgentRef.Name), zap.String("model", model.Spec.ModelID), zap.Error(err))
		return "", fmt.Errorf("%w: %w", ErrDispatchAccountingUncertain, err)
	}
	if err := d.appendDispatchMarker(ctx, instance, modelDispatchAccountedReason, operationKey); err != nil {
		return "", fmt.Errorf("%w: usage persisted but completion marker failed: %w", ErrDispatchAccountingUncertain, err)
	}
	if err := modelgateway.RecordStandaloneDecision(ctx, d.audit, modelDecision, "completed", "target_completed", "completed", true, false); err != nil {
		return "", fmt.Errorf("persist completed model evidence: %w", err)
	}
	if hasDurable {
		response, err := json.Marshal(resp)
		if err != nil {
			return "", fmt.Errorf("%w: encode completion: %w", ErrDispatchAccountingUncertain, err)
		}
		if err := durable.CompleteModelDispatch(ctx, namespace, operationKey, sessionID, response); err != nil {
			return "", fmt.Errorf("%w: persist completion: %w", ErrDispatchAccountingUncertain, err)
		}
	}

	d.mu.Lock()
	d.results[sessionID] = cpiDispatchResult{namespace: namespace, operation: operationKey, response: resp, completedAt: time.Now().UTC()}
	d.mu.Unlock()

	d.logger.Info("dispatch completed",
		zap.String("sessionID", sessionID),
		zap.String("agent", instance.Spec.AgentRef.Name),
		zap.String("model", model.Spec.ModelID),
	)

	return sessionID, nil
}

func (d *cpiDispatcher) hasUnfinishedModelDispatch(ctx context.Context, instance *v1alpha1.AgentInstance, operationKey string) (bool, error) {
	events, err := d.store.ListEvents(ctx, "AgentInstance", instance.ObjectMeta.Namespace, instance.ObjectMeta.Name)
	if err != nil {
		return false, err
	}
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Message != operationKey {
			continue
		}
		switch events[index].Reason {
		case modelDispatchStartedReason, modelDispatchAccountedReason:
			return true, nil
		}
	}
	return false, nil
}

func (d *cpiDispatcher) pruneResults(now time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for sessionID, result := range d.results {
		if now.Sub(result.completedAt) >= cpiDispatchResultTTL {
			delete(d.results, sessionID)
		}
	}
}

func (d *cpiDispatcher) appendDispatchMarker(ctx context.Context, instance *v1alpha1.AgentInstance, reason, operationKey string) error {
	return d.store.AppendEvent(ctx, "AgentInstance", instance.ObjectMeta.Namespace, instance.ObjectMeta.Name, store.InstanceEvent{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Type:      "Normal",
		Reason:    reason,
		Message:   operationKey,
	})
}

func (d *cpiDispatcher) recordUsageEvent(ctx context.Context, instance *v1alpha1.AgentInstance, agent *v1alpha1.Agent, model *v1alpha1.Model, modelClass *v1alpha1.ModelClass, sessionID string, usage cpi.Usage, startedAt, completedAt time.Time) error {
	if d.usage == nil {
		return nil
	}
	pricing, err := observability.ResolveModelPricing(modelClass.Spec.Parameters)
	if err != nil {
		return err
	}
	cost, _, err := observability.ModelCostUnits(modelClass.Spec.Parameters, int64(usage.PromptTokens), int64(usage.CompletionTokens))
	if err != nil {
		return err
	}
	totalTokens := usage.TotalTokens
	if totalTokens == 0 {
		totalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	durationMillis := completedAt.Sub(startedAt).Milliseconds()
	if !completedAt.Before(startedAt) && durationMillis == 0 {
		durationMillis = 1
	}
	instanceKey := instance.ObjectMeta.UID
	if instanceKey == "" {
		instanceKey = instance.ObjectMeta.Namespace + "/" + instance.ObjectMeta.Name
	}
	eventID := "agentinstance:" + instanceKey + ":model"
	event := observability.CostRecord{
		ID: eventID, Workspace: instance.ObjectMeta.Namespace, Agent: agent.ObjectMeta.Name, AgentNamespace: instance.ObjectMeta.Namespace,
		Workflow: instance.ObjectMeta.Annotations[observability.AnnotationWorkflowName], Step: instance.ObjectMeta.Annotations[observability.AnnotationStepName],
		SessionID: sessionID, ReservationID: "agentinstance:" + instanceKey,
		OperationType: observability.CostOpModelInference, ModelName: model.Spec.ModelID, ModelProvider: modelClass.Spec.Provisioner,
		ModelClass: modelClass.ObjectMeta.Name, ModelClassVersion: modelClass.Spec.VersionPin, ModelCostTier: modelClass.Spec.CostTier,
		PromptTokens: int64(usage.PromptTokens), CompletionTokens: int64(usage.CompletionTokens), TotalTokens: int64(totalTokens), UsageReported: usage.Reported,
		PricingConfigured: pricing.Configured, InputCostMicrousdPerMillionTokens: pricing.InputMicrousdPerMillionTokens,
		OutputCostMicrousdPerMillionTokens: pricing.OutputMicrousdPerMillionTokens, CostUnits: cost,
		ProviderDurationMillis: durationMillis, Timestamp: completedAt,
	}
	identity, identityErr := getStoredResource[v1alpha1.AgentIdentity](ctx, d.store, "AgentIdentity", instance.ObjectMeta.Namespace, "agent-identity-"+agent.ObjectMeta.Name)
	if identityErr == nil {
		event.AgentIdentity = identity.Spec.SPIFFEID
	}
	if agent.ObjectMeta.Labels != nil {
		event.Solution = agent.ObjectMeta.Labels["kognition.dev/solution"]
		if event.Solution != "" {
			if solution, solutionErr := getStoredResource[v1alpha1.Solution](ctx, d.store, "Solution", instance.ObjectMeta.Namespace, event.Solution); solutionErr == nil {
				event.SolutionRev = solution.Status.ActiveRevision
			}
		}
	}
	_, err = d.usage.ApplyUsageEvent(ctx, event)
	return err
}

func (d *cpiDispatcher) Status(ctx context.Context, instance *v1alpha1.AgentInstance) (RuntimeStatus, error) {
	sessionID := instance.Status.SessionID
	if sessionID == "" {
		return RuntimeStatus{}, fmt.Errorf("instance has no session ID")
	}

	d.mu.RLock()
	result, ok := d.results[sessionID]
	d.mu.RUnlock()
	if ok && (result.namespace != instance.ObjectMeta.Namespace || result.operation != agentInstanceReservationID(instance)) {
		return RuntimeStatus{}, ErrDispatchAccountingUncertain
	}

	if !ok {
		if durable, supported := d.store.(store.ModelDispatchStore); supported && instance.Spec.SessionContext["input"] != "" {
			record, err := durable.GetModelDispatch(ctx, instance.ObjectMeta.Namespace, agentInstanceReservationID(instance))
			if err != nil {
				return RuntimeStatus{}, fmt.Errorf("load model completion: %w", err)
			}
			if record.SessionID != sessionID || len(record.Response) == 0 {
				return RuntimeStatus{}, ErrDispatchAccountingUncertain
			}
			if err := json.Unmarshal(record.Response, &result.response); err != nil || result.response == nil || !result.response.Usage.Reported {
				return RuntimeStatus{}, fmt.Errorf("%w: invalid stored completion", ErrDispatchAccountingUncertain)
			}
		} else {
			return RuntimeStatus{Alive: true}, nil
		}
	}

	resp := result.response

	status := RuntimeStatus{
		Alive:     true,
		Completed: true,
		Output:    resp.Content,
	}
	if resp.Usage.Reported {
		totalTokens := resp.Usage.TotalTokens
		if totalTokens == 0 {
			totalTokens = resp.Usage.PromptTokens + resp.Usage.CompletionTokens
		}
		status.TokenUsage = &v1alpha1.TokenUsage{
			PromptTokens:     int64(resp.Usage.PromptTokens),
			CompletionTokens: int64(resp.Usage.CompletionTokens),
			TotalTokens:      int64(totalTokens),
		}
	}

	return status, nil
}

// Terminate acknowledges quota termination after synchronous CPI execution has
// already returned an authoritative terminal result.
func (d *cpiDispatcher) Terminate(ctx context.Context, instance *v1alpha1.AgentInstance, _ string) error {
	status, err := d.Status(ctx, instance)
	if err != nil {
		return err
	}
	if !status.Completed {
		return fmt.Errorf("CPI session %q is not terminal", instance.Status.SessionID)
	}
	return nil
}

// resolveCredentials loads a SecretRef and extracts credential type and data.
func (d *cpiDispatcher) resolveCredentials(ctx context.Context, namespace, secretRefName string) (*cpi.Credentials, error) {
	secretRef, err := getStoredResource[v1alpha1.SecretRef](ctx, d.store, "SecretRef", namespace, secretRefName)
	if err != nil {
		return nil, fmt.Errorf("load secret ref %s: %w", secretRefName, err)
	}

	switch secretRef.Spec.ResolvedBackendType() {
	case "internal":
		if secretRef.ObjectMeta.Annotations == nil {
			return nil, fmt.Errorf("annotation %q is missing", internalSecretDataAnnotation)
		}
		raw := secretRef.ObjectMeta.Annotations[internalSecretDataAnnotation]
		if raw == "" {
			return nil, fmt.Errorf("annotation %q is missing", internalSecretDataAnnotation)
		}
		var values map[string]string
		if err := json.Unmarshal([]byte(raw), &values); err != nil {
			return nil, fmt.Errorf("parse annotation %q: %w", internalSecretDataAnnotation, err)
		}
		credType := values["type"]
		delete(values, "type")
		return &cpi.Credentials{Type: credType, Data: values}, nil

	case "env":
		// For env-backed secrets, resolve value and use key as data key.
		value, ok := os.LookupEnv(secretRef.Spec.ResolvedBackendRef())
		if !ok {
			return nil, fmt.Errorf("environment variable %q is not set", secretRef.Spec.ResolvedBackendRef())
		}
		key := secretRef.Spec.ResolvedKey()
		return &cpi.Credentials{
			Type: "api-key",
			Data: map[string]string{key: value},
		}, nil

	default:
		return nil, fmt.Errorf("secret backend %q is not supported for CPI dispatch", secretRef.Spec.ResolvedBackendType())
	}
}
