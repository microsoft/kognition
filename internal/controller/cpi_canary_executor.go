// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"time"
	"unicode/utf8"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/modelgateway"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/store"
)

// ErrCanaryOutcomeUncertain prevents replay after provider execution may have begun.
var ErrCanaryOutcomeUncertain = errors.New("reasoning canary provider outcome is uncertain")

// ErrCanaryBindingChanged prevents execution when a prepared dependency changed.
var ErrCanaryBindingChanged = errors.New("reasoning canary immutable binding changed")

const maxCanaryOutcomeBytes = 4096

type resolvedCanaryRequest struct {
	driver     cpi.Driver
	completion *cpi.CompletionRequest
	agent      *v1alpha1.Agent
	prompt     *v1alpha1.Prompt
	model      *v1alpha1.Model
	modelClass *v1alpha1.ModelClass
	credential *v1alpha1.SecretRef
}

// CPICanaryExecutor runs isolated reasoning canaries through the CPI registry.
type CPICanaryExecutor struct {
	store        store.Store
	operations   store.CanaryOperationStore
	reservations store.CanaryReservationStore
	registry     *cpi.Registry
	core         *modelgateway.Core
	credentials  *cpiDispatcher
	usage        observability.UsageEventWriter
	audit        observability.AuditStore
	tracer       *observability.Tracer
	collector    *observability.Collector
}

// NewCPICanaryExecutor creates a replay-fenced CPI reasoning canary executor.
func NewCPICanaryExecutor(s store.Store, registry *cpi.Registry, core *modelgateway.Core) (*CPICanaryExecutor, error) {
	operations, ok := s.(store.CanaryOperationStore)
	if !ok {
		return nil, errors.New("store does not support durable canary operations")
	}
	reservations, ok := s.(store.CanaryReservationStore)
	if !ok {
		return nil, errors.New("store does not support durable canary reservations")
	}
	usage, ok := s.(observability.UsageEventWriter)
	if !ok {
		return nil, errors.New("store does not support immutable usage events")
	}
	audit, ok := s.(observability.AuditStore)
	if !ok {
		return nil, errors.New("store does not support durable audit events")
	}
	return &CPICanaryExecutor{
		store: s, operations: operations, reservations: reservations, registry: registry, core: core,
		credentials: NewCPIDispatcher(s, registry, zap.NewNop()), usage: usage, audit: audit,
	}, nil
}

// SetUsageEventWriter installs the immutable writer shared by canary usage and
// its credentials dispatcher.
func (e *CPICanaryExecutor) SetUsageEventWriter(usage observability.UsageEventWriter) {
	e.usage = usage
	e.credentials.SetUsageEventWriter(usage)
}

// SetTracing installs the semantic tracer used for ReasoningProbe canary spans.
func (e *CPICanaryExecutor) SetTracing(tracer *observability.Tracer, collector *observability.Collector) {
	e.tracer = tracer
	e.collector = collector
}

func (e *CPICanaryExecutor) ExecuteCanary(ctx context.Context, request CanaryRequest) (result CanaryResult, resultErr error) {
	if request.Instance == nil || request.OperationID == "" || request.RequestDigest == "" {
		return CanaryResult{}, errors.New("canary requires an instance, operation ID, and request digest")
	}
	if err := validateCanaryBounds(request); err != nil {
		return CanaryResult{OperationID: request.OperationID, Outcome: CanaryOutcomeExecutionFailed, ClosedReason: v1alpha1.CanaryFailureInvalidConfiguration}, err
	}
	ctx, cancel := context.WithTimeout(ctx, request.Timeout)
	defer cancel()
	existing, err := e.operations.GetCanaryOperation(ctx, request.OperationID)
	if err == nil {
		if existing.RequestDigest != request.RequestDigest {
			return CanaryResult{}, store.ErrConflict
		}
		if existing.Phase != store.CanaryClaimed {
			return e.resume(ctx, request, existing, 0)
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return CanaryResult{}, fmt.Errorf("get canary operation: %w", err)
	}
	resolved, err := e.resolveCanaryRequest(ctx, request)
	if err != nil {
		var denied *modelgateway.AuthorizationDeniedError
		if errors.As(err, &denied) {
			claimed, operation, claimErr := e.operations.ClaimCanaryOperation(ctx, store.CanaryOperation{
				OperationID: request.OperationID, RequestDigest: request.RequestDigest, Phase: store.CanaryClaimed,
			})
			if claimErr != nil {
				return CanaryResult{}, fmt.Errorf("claim authorization-denied canary operation: %w", claimErr)
			}
			if !claimed && operation.Phase != store.CanaryClaimed {
				return e.resume(ctx, request, operation, 0)
			}
			if claimed {
				var span *observability.Span
				span, ctx = e.startCanarySpan(ctx, request)
				defer func() { e.endCanarySpan(ctx, span, resultErr) }()
			}
			return e.finalizePreCallFailure(ctx, request, 0, v1alpha1.CanaryFailureAuthorizationDenied, !claimed)
		}
		return CanaryResult{OperationID: request.OperationID, Outcome: CanaryOutcomeExecutionFailed, ClosedReason: v1alpha1.CanaryFailureBindingChanged}, err
	}
	if err := validatePreparedCanary(request, resolved); err != nil {
		return CanaryResult{OperationID: request.OperationID, Outcome: CanaryOutcomeExecutionFailed, ClosedReason: v1alpha1.CanaryFailureBindingChanged}, err
	}
	counter, ok := resolved.driver.(cpi.ExactTokenCounter)
	if !ok {
		return CanaryResult{OperationID: request.OperationID, Outcome: CanaryOutcomeExecutionFailed, ClosedReason: v1alpha1.CanaryFailureInvalidConfiguration}, errors.New("CPI driver does not support exact input tokenization")
	}
	promptTokens, err := counter.CountInputTokens(resolved.completion)
	if err != nil {
		return CanaryResult{OperationID: request.OperationID, Outcome: CanaryOutcomeExecutionFailed, ClosedReason: v1alpha1.CanaryFailureInvalidConfiguration}, fmt.Errorf("count exact input tokens: %w", err)
	}
	if promptTokens > int(request.MaxInputTokens) {
		return CanaryResult{OperationID: request.OperationID, PromptTokens: int64(promptTokens), Outcome: CanaryOutcomeExecutionFailed, ClosedReason: v1alpha1.CanaryFailureInvalidConfiguration}, fmt.Errorf("exact input token count %d exceeds limit %d", promptTokens, request.MaxInputTokens)
	}

	claimed, operation, err := e.operations.ClaimCanaryOperation(ctx, store.CanaryOperation{
		OperationID: request.OperationID, RequestDigest: request.RequestDigest, Phase: store.CanaryClaimed,
	})
	if err != nil {
		return CanaryResult{}, fmt.Errorf("claim canary operation: %w", err)
	}
	if !claimed && operation.Phase != store.CanaryClaimed {
		return e.resume(ctx, request, operation, int64(promptTokens))
	}
	if claimed {
		var span *observability.Span
		span, ctx = e.startCanarySpan(ctx, request)
		defer func() { e.endCanarySpan(ctx, span, resultErr) }()
	}
	reservationID := canaryReservationID(request.OperationID)
	if _, err := e.reservations.CreateCanaryReservation(ctx, store.TokenReservation{
		ID: reservationID, Namespace: request.Instance.ObjectMeta.Namespace,
		ReservedInput: int64(promptTokens), ReservedOutput: int64(request.MaxOutputTokens),
	}); err != nil {
		reason := v1alpha1.CanaryFailureBudgetDenied
		if errors.Is(err, context.DeadlineExceeded) {
			reason = v1alpha1.CanaryFailureTimedOut
		}
		return e.finalizePreCallFailure(ctx, request, promptTokens, reason, false)
	}
	expectedDigest := sha256.Sum256([]byte(request.ExpectedOutcome))
	fallback := CanaryResult{
		OperationID: request.OperationID, ExpectedBytes: int32(len([]byte(request.ExpectedOutcome))),
		ExpectedSHA256: hex.EncodeToString(expectedDigest[:]), PromptTokens: int64(promptTokens),
		CompletionTokens: int64(request.MaxOutputTokens), TotalTokens: int64(promptTokens) + int64(request.MaxOutputTokens),
		Outcome: CanaryOutcomeExecutionFailed, ClosedReason: v1alpha1.CanaryFailureAccountingUncertain,
	}
	fallback = correlateCanaryResult(ctx, fallback)
	fallbackData, err := json.Marshal(fallback)
	if err != nil {
		return CanaryResult{}, fmt.Errorf("marshal uncertain canary result: %w", err)
	}
	fallbackTerminal, err := e.buildCanaryTerminalRecord(ctx, request, resolved, fallback, fallbackData, time.Now().UTC(), true)
	if err != nil {
		return CanaryResult{}, fmt.Errorf("build uncertain canary terminal record: %w", err)
	}
	started, operation, err := e.operations.TransitionCanaryOperation(ctx, request.OperationID, request.RequestDigest, store.CanaryClaimed, store.CanaryProviderStarted, fallbackTerminal)
	if err != nil {
		return CanaryResult{OperationID: request.OperationID, PromptTokens: int64(promptTokens), Outcome: CanaryOutcomeExecutionFailed, ClosedReason: v1alpha1.CanaryFailureAccountingUncertain}, fmt.Errorf("%w: persist provider fence: %w", ErrCanaryOutcomeUncertain, err)
	}
	if !started {
		return e.resume(ctx, request, operation, int64(promptTokens))
	}

	startedAt := time.Now().UTC()
	response, err := resolved.driver.Complete(ctx, resolved.completion)
	if err != nil {
		return correlateCanaryResult(ctx, CanaryResult{OperationID: request.OperationID, PromptTokens: int64(promptTokens), Outcome: CanaryOutcomeExecutionFailed, ClosedReason: v1alpha1.CanaryFailureAccountingUncertain}), fmt.Errorf("%w: %w", ErrCanaryOutcomeUncertain, err)
	}
	result, usageEstimated := evaluateCanaryResponse(request, response, promptTokens, startedAt, time.Now().UTC())
	result = correlateCanaryResult(ctx, result)
	resultData, err := json.Marshal(result)
	if err != nil {
		return CanaryResult{}, fmt.Errorf("marshal canary result: %w", err)
	}
	terminal, err := e.buildCanaryTerminalRecord(ctx, request, resolved, result, resultData, time.Now().UTC(), usageEstimated)
	if err != nil {
		return result, fmt.Errorf("build canary terminal record: %w", err)
	}
	transitioned, operation, err := e.operations.TransitionCanaryOperation(ctx, request.OperationID, request.RequestDigest, store.CanaryProviderStarted, store.CanaryProviderCompleted, terminal)
	if err != nil {
		return result, fmt.Errorf("%w: persist provider result: %w", ErrCanaryOutcomeUncertain, err)
	}
	if !transitioned {
		return result, fmt.Errorf("%w: persist provider result: transition rejected", ErrCanaryOutcomeUncertain)
	}
	return e.finalize(ctx, request, operation, result)
}

func (e *CPICanaryExecutor) PrepareCanary(ctx context.Context, request CanaryRequest) (CanaryRequest, error) {
	if err := validateCanaryConfiguration(request); err != nil {
		return CanaryRequest{}, err
	}
	if request.Timeout == 0 {
		request.Timeout = 60 * time.Second
	}
	if request.MaxInputTokens == 0 {
		request.MaxInputTokens = 4096
	}
	if request.MaxOutputTokens == 0 {
		request.MaxOutputTokens = 256
	}
	resolved, err := e.resolveCanaryRequest(ctx, request)
	if err != nil {
		return CanaryRequest{}, err
	}
	counter, ok := resolved.driver.(cpi.ExactTokenCounter)
	if !ok {
		return CanaryRequest{}, errors.New("CPI driver does not support exact input tokenization")
	}
	count, err := counter.CountInputTokens(resolved.completion)
	if err != nil {
		return CanaryRequest{}, fmt.Errorf("count exact input tokens: %w", err)
	}
	if count > int(request.MaxInputTokens) {
		return CanaryRequest{}, fmt.Errorf("exact input token count %d exceeds limit %d", count, request.MaxInputTokens)
	}
	revision := request.AgentRevision
	request.Instructions = resolved.completion.Messages[0].Content
	request.Bindings = canaryBindings(request.Instance.ObjectMeta.Namespace, revision, resolved)
	request.RequestDigest = canaryRequestDigest(request)
	return request, nil
}

func (e *CPICanaryExecutor) resolveCanaryRequest(ctx context.Context, request CanaryRequest) (*resolvedCanaryRequest, error) {
	instance := request.Instance
	agent, err := ResolveExecutionAgent(ctx, e.store, instance.ObjectMeta.Namespace, instance)
	if err != nil {
		return nil, fmt.Errorf("resolve immutable agent: %w", err)
	}
	instructions := agent.Spec.Instructions.Inline
	var prompt *v1alpha1.Prompt
	if agent.Spec.Instructions.Ref != nil {
		prompt, err = getStoredResource[v1alpha1.Prompt](ctx, e.store, "Prompt", instance.ObjectMeta.Namespace, agent.Spec.Instructions.Ref.Name)
		if err != nil {
			return nil, fmt.Errorf("resolve prompt binding: %w", err)
		}
		instructions = prompt.Spec.ResolvedContent()
	}
	var model *v1alpha1.Model
	var class *v1alpha1.ModelClass
	if e.core != nil {
		authorized, err := e.core.AuthorizeExecution(ctx, instance.ObjectMeta.Namespace, instance.Spec.AgentRef.Name, agent.Spec.ModelRef.Name)
		if err != nil {
			return nil, fmt.Errorf("authorize model execution: %w", err)
		}
		model, class = authorized.Model, authorized.ModelClass
	} else {
		model, err = getStoredResource[v1alpha1.Model](ctx, e.store, "Model", instance.ObjectMeta.Namespace, agent.Spec.ModelRef.Name)
		if err == nil {
			class, err = getStoredResource[v1alpha1.ModelClass](ctx, e.store, "ModelClass", "", model.Spec.ClassName)
		}
		if err != nil {
			return nil, fmt.Errorf("resolve model binding: %w", err)
		}
	}
	if class == nil {
		return nil, errors.New("authorized model has no ModelClass")
	}
	driver, ok := e.registry.Get(class.Spec.Provisioner)
	if !ok {
		return nil, fmt.Errorf("no CPI driver registered for provisioner %q", class.Spec.Provisioner)
	}
	var credentials *cpi.Credentials
	var credential *v1alpha1.SecretRef
	if model.Spec.CredentialRef != nil && model.Spec.CredentialRef.Name != "" {
		credential, err = getStoredResource[v1alpha1.SecretRef](ctx, e.store, "SecretRef", instance.ObjectMeta.Namespace, model.Spec.CredentialRef.Name)
		if err != nil {
			return nil, fmt.Errorf("resolve credential binding: %w", err)
		}
		credentials, err = e.credentials.resolveCredentials(ctx, instance.ObjectMeta.Namespace, model.Spec.CredentialRef.Name)
		if err != nil {
			return nil, err
		}
	}
	parameters := make(map[string]string, len(model.Spec.Parameters))
	for key, value := range model.Spec.Parameters {
		parameters[key] = value
	}
	if model.Spec.InvocationProfileRef == nil {
		parameters["temperature"] = "0"
	}
	parameters["max_tokens"] = strconv.FormatInt(int64(request.MaxOutputTokens), 10)
	completion := &cpi.CompletionRequest{
		Model: model.Spec.ModelID, Endpoint: model.Spec.Endpoint, Creds: credentials,
		Messages: []cpi.Message{{Role: "system", Content: instructions}, {Role: "user", Content: request.CanaryTask}}, Parameters: parameters,
	}
	completion, err = cpi.BindCompletion(ctx, e.store, driver, model, completion)
	if err != nil {
		return nil, fmt.Errorf("canary invocation configuration: %w", err)
	}
	return &resolvedCanaryRequest{
		driver: driver, agent: agent, prompt: prompt, model: model, modelClass: class, credential: credential,
		completion: completion,
	}, nil
}

func canaryBindings(namespace string, revision int64, resolved *resolvedCanaryRequest) []v1alpha1.CanaryResourceBinding {
	bindings := []v1alpha1.CanaryResourceBinding{
		resourceBinding("Agent", namespace, &resolved.agent.ObjectMeta, &revision),
		resourceBinding("Model", namespace, &resolved.model.ObjectMeta, nil),
		resourceBinding("ModelClass", "", &resolved.modelClass.ObjectMeta, nil),
	}
	if invocation := resolved.completion.Invocation; invocation != nil {
		bindings = append(bindings, v1alpha1.CanaryResourceBinding{
			Kind: "ModelInvocationProfile", Namespace: namespace, Name: invocation.Binding.Name,
			UID: invocation.Binding.UID, ResourceVersion: strconv.FormatInt(invocation.ResourceVersion, 10),
		})
	}
	if resolved.prompt != nil {
		bindings = append(bindings, resourceBinding("Prompt", namespace, &resolved.prompt.ObjectMeta, nil))
	}
	if resolved.credential != nil {
		bindings = append(bindings, resourceBinding("SecretRef", namespace, &resolved.credential.ObjectMeta, nil))
	}
	return bindings
}

func resourceBinding(kind, namespace string, metadata *v1alpha1.ObjectMeta, revision *int64) v1alpha1.CanaryResourceBinding {
	return v1alpha1.CanaryResourceBinding{
		Kind: kind, Namespace: namespace, Name: metadata.Name, UID: metadata.UID,
		ResourceVersion: strconv.FormatInt(metadata.ResourceVersion, 10), Revision: revision,
	}
}

func validatePreparedCanary(request CanaryRequest, resolved *resolvedCanaryRequest) error {
	revision := request.AgentRevision
	bindings := canaryBindings(request.Instance.ObjectMeta.Namespace, revision, resolved)
	if !reflect.DeepEqual(request.Bindings, bindings) {
		return fmt.Errorf("%w: resource identity differs from prepared bindings", ErrCanaryBindingChanged)
	}
	prepared := request
	prepared.Instructions = resolved.completion.Messages[0].Content
	prepared.Bindings = bindings
	if digest := canaryRequestDigest(prepared); digest != request.RequestDigest {
		return fmt.Errorf("%w: request digest differs from prepared digest", ErrCanaryBindingChanged)
	}
	return nil
}

func canaryRequestDigest(request CanaryRequest) string {
	payload := struct {
		OperationID   string                           `json:"operationID"`
		ScheduleSlot  string                           `json:"scheduleSlot"`
		InstanceUID   string                           `json:"instanceUID"`
		SessionID     string                           `json:"sessionID"`
		AgentRevision int64                            `json:"agentRevision"`
		Bindings      []v1alpha1.CanaryResourceBinding `json:"bindings"`
		Instructions  string                           `json:"instructions"`
		Task          string                           `json:"task"`
		Expected      string                           `json:"expected"`
		TimeoutNanos  int64                            `json:"timeoutNanos"`
		MaxInput      int32                            `json:"maxInput"`
		MaxOutput     int32                            `json:"maxOutput"`
	}{request.OperationID, request.ScheduleSlot.UTC().Format(time.RFC3339Nano), request.Instance.ObjectMeta.UID, request.SessionID, request.AgentRevision, request.Bindings, request.Instructions, request.CanaryTask, request.ExpectedOutcome, request.Timeout.Nanoseconds(), request.MaxInputTokens, request.MaxOutputTokens}
	data, _ := json.Marshal(payload)
	digest := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func (e *CPICanaryExecutor) resume(ctx context.Context, request CanaryRequest, operation store.CanaryOperation, promptTokens int64) (CanaryResult, error) {
	switch operation.Phase {
	case store.CanaryProviderStarted:
		var result CanaryResult
		if err := json.Unmarshal(operation.Result, &result); err != nil {
			return CanaryResult{}, fmt.Errorf("decode uncertain canary result: %w", err)
		}
		terminal := &store.CanaryTerminalRecord{
			Result: operation.Result, UsageEvent: operation.UsageEvent, AuditEvent: operation.AuditEvent,
			ReservationCreated: operation.ReservationCreated,
		}
		transitioned, current, err := e.operations.TransitionCanaryOperation(ctx, request.OperationID, request.RequestDigest, store.CanaryProviderStarted, store.CanaryProviderCompleted, terminal)
		if err != nil {
			return result, fmt.Errorf("persist uncertain canary result: %w", err)
		}
		if !transitioned && current.Phase != store.CanaryProviderCompleted && current.Phase != store.CanaryFinalizing && current.Phase != store.CanaryCompleted {
			return result, ErrCanaryOutcomeUncertain
		}
		return e.resume(ctx, request, current, promptTokens)
	case store.CanaryProviderCompleted, store.CanaryFinalizing:
		var result CanaryResult
		if err := json.Unmarshal(operation.Result, &result); err != nil {
			return CanaryResult{}, fmt.Errorf("decode durable canary result: %w", err)
		}
		return e.finalize(ctx, request, operation, result)
	case store.CanaryCompleted:
		var result CanaryResult
		if err := json.Unmarshal(operation.Result, &result); err != nil {
			return CanaryResult{}, fmt.Errorf("decode completed canary result: %w", err)
		}
		return result, nil
	default:
		return CanaryResult{OperationID: request.OperationID, Outcome: CanaryOutcomeExecutionFailed, ClosedReason: v1alpha1.CanaryFailureAccountingUncertain}, ErrCanaryOutcomeUncertain
	}
}

func (e *CPICanaryExecutor) finalize(ctx context.Context, request CanaryRequest, operation store.CanaryOperation, result CanaryResult) (CanaryResult, error) {
	if operation.Phase == store.CanaryProviderCompleted {
		transitioned, current, err := e.operations.TransitionCanaryOperation(ctx, request.OperationID, request.RequestDigest, store.CanaryProviderCompleted, store.CanaryFinalizing, nil)
		if err != nil {
			return result, fmt.Errorf("start canary finalization: %w", err)
		}
		if !transitioned && current.Phase != store.CanaryFinalizing && current.Phase != store.CanaryCompleted {
			return result, ErrCanaryOutcomeUncertain
		}
		operation = current
	}
	if operation.Phase == store.CanaryFinalizing {
		if !operation.ReservationFinalized {
			if operation.ReservationCreated {
				if len(operation.UsageEvent) == 0 {
					if _, err := e.reservations.ReleaseCanaryReservation(ctx, canaryReservationID(request.OperationID)); err != nil && !errors.Is(err, store.ErrNotFound) {
						return result, fmt.Errorf("release canary reservation: %w", err)
					}
				} else if _, err := e.reservations.SettleCanaryReservation(ctx, canaryReservationID(request.OperationID), result.PromptTokens, result.CompletionTokens); err != nil {
					return result, fmt.Errorf("settle canary reservation: %w", err)
				}
			}
			current, err := e.operations.AcknowledgeCanaryFinalization(ctx, request.OperationID, request.RequestDigest, store.CanaryReservationFinalized)
			if err != nil {
				return result, fmt.Errorf("acknowledge canary reservation finalization: %w", err)
			}
			operation = current
		}
		if !operation.UsageApplied && len(operation.UsageEvent) > 0 {
			var event observability.CostRecord
			if err := json.Unmarshal(operation.UsageEvent, &event); err != nil {
				return result, fmt.Errorf("decode canary usage event: %w", err)
			}
			if _, err := e.usage.ApplyUsageEvent(ctx, event); err != nil {
				return result, fmt.Errorf("apply canary usage event: %w", err)
			}
			current, err := e.operations.AcknowledgeCanaryFinalization(ctx, request.OperationID, request.RequestDigest, store.CanaryUsageApplied)
			if err != nil {
				return result, fmt.Errorf("acknowledge canary usage event: %w", err)
			}
			operation = current
		}
		if !operation.AuditDelivered {
			entry, err := canaryAuditEntry(operation.AuditEvent)
			if err != nil {
				return result, fmt.Errorf("decode canary audit event: %w", err)
			}
			if err := e.audit.Append(ctx, entry); err != nil {
				return result, fmt.Errorf("deliver canary audit event: %w", err)
			}
			if _, err := e.operations.AcknowledgeCanaryFinalization(ctx, request.OperationID, request.RequestDigest, store.CanaryAuditDelivered); err != nil {
				return result, fmt.Errorf("acknowledge canary audit event: %w", err)
			}
		}
		if _, _, err := e.operations.TransitionCanaryOperation(ctx, request.OperationID, request.RequestDigest, store.CanaryFinalizing, store.CanaryCompleted, nil); err != nil {
			return result, fmt.Errorf("complete canary finalization: %w", err)
		}
	}
	return result, nil
}

func (e *CPICanaryExecutor) finalizePreCallFailure(ctx context.Context, request CanaryRequest, promptTokens int, reason v1alpha1.CanaryFailureReason, reservationCreated bool) (CanaryResult, error) {
	result := correlateCanaryResult(ctx, preCallCanaryFailure(request, promptTokens, reason))
	resultData, err := json.Marshal(result)
	if err != nil {
		return result, fmt.Errorf("marshal %s canary result: %w", reason, err)
	}
	terminal, err := buildCanaryPreCallTerminalRecord(request, result, resultData, time.Now().UTC(), reservationCreated)
	if err != nil {
		return result, fmt.Errorf("build %s canary terminal record: %w", reason, err)
	}
	transitioned, current, err := e.operations.TransitionCanaryOperation(ctx, request.OperationID, request.RequestDigest, store.CanaryClaimed, store.CanaryFinalizing, terminal)
	if err != nil {
		return result, fmt.Errorf("persist %s canary result: %w", reason, err)
	}
	if !transitioned {
		return e.resume(ctx, request, current, int64(promptTokens))
	}
	return e.finalize(ctx, request, current, result)
}

func (e *CPICanaryExecutor) startCanarySpan(ctx context.Context, request CanaryRequest) (*observability.Span, context.Context) {
	if e.tracer == nil || e.collector == nil {
		return nil, ctx
	}
	bindings := make(map[string]string, len(request.Bindings))
	for _, binding := range request.Bindings {
		bindings[binding.Kind] = binding.Name
	}
	span := e.tracer.StartSpan(ctx, observability.SpanNameReasoningProbe, observability.SpanKindInternal, map[string]interface{}{
		observability.AttrReasoningProbeOperationID: request.OperationID,
		observability.AttrAgentName:                 bindings["Agent"],
		observability.AttrAgentNamespace:            request.Instance.ObjectMeta.Namespace,
		observability.AttrAgentInstanceName:         request.Instance.ObjectMeta.Name,
		observability.AttrAgentRevision:             request.AgentRevision,
		observability.AttrModelName:                 bindings["Model"],
	})
	return span, observability.ContextWithSpanContext(ctx, span.Context)
}

func (e *CPICanaryExecutor) endCanarySpan(ctx context.Context, span *observability.Span, resultErr error) {
	if e.tracer == nil || e.collector == nil || span == nil {
		return
	}
	status, message := observability.StatusOK, ""
	if resultErr != nil {
		status, message = observability.StatusError, "reasoning probe canary failed"
	}
	_ = e.tracer.EndSpan(span, status, message)
	if span.Context.TraceFlags&0x01 != 0 {
		_ = e.collector.FlushSpans(ctx, []*observability.Span{span})
	}
}

func correlateCanaryResult(ctx context.Context, result CanaryResult) CanaryResult {
	if spanContext, ok := observability.SpanContextFromContext(ctx); ok {
		result.TraceID = observability.HexTraceID(spanContext.TraceID)
		result.SpanID = observability.HexSpanID(spanContext.SpanID)
	}
	return result
}

func buildCanaryResult(request CanaryRequest, response *cpi.CompletionResponse, startedAt, completedAt time.Time) CanaryResult {
	expectedDigest := sha256.Sum256([]byte(request.ExpectedOutcome))
	actualDigest := sha256.Sum256([]byte(response.Content))
	outcome := CanaryOutcomeMismatch
	if response.Content == request.ExpectedOutcome {
		outcome = CanaryOutcomePassed
	}
	total := response.Usage.TotalTokens
	if total == 0 {
		total = response.Usage.PromptTokens + response.Usage.CompletionTokens
	}
	return CanaryResult{
		OperationID: request.OperationID, ExpectedBytes: int32(len([]byte(request.ExpectedOutcome))), ActualBytes: int32(len([]byte(response.Content))),
		ExpectedSHA256: hex.EncodeToString(expectedDigest[:]), ActualSHA256: hex.EncodeToString(actualDigest[:]),
		PromptTokens: int64(response.Usage.PromptTokens), CompletionTokens: int64(response.Usage.CompletionTokens), TotalTokens: int64(total),
		ProviderDurationMillis: completedAt.Sub(startedAt).Milliseconds(), Outcome: outcome,
	}
}

func validateCanaryBounds(request CanaryRequest) error {
	if request.Timeout <= 0 || request.MaxInputTokens <= 0 || request.MaxOutputTokens <= 0 {
		return errors.New("canary timeout and token bounds must be positive")
	}
	return validateCanaryConfiguration(request)
}

func validateCanaryConfiguration(request CanaryRequest) error {
	if request.Timeout < 0 || request.MaxInputTokens < 0 || request.MaxOutputTokens < 0 {
		return errors.New("canary timeout and token bounds cannot be negative")
	}
	if !utf8.ValidString(request.ExpectedOutcome) || len([]byte(request.ExpectedOutcome)) > maxCanaryOutcomeBytes {
		return fmt.Errorf("expected outcome must be valid UTF-8 and at most %d bytes", maxCanaryOutcomeBytes)
	}
	return nil
}

func evaluateCanaryResponse(request CanaryRequest, response *cpi.CompletionResponse, promptTokens int, startedAt, completedAt time.Time) (CanaryResult, bool) {
	if response == nil {
		return CanaryResult{
			OperationID: request.OperationID, PromptTokens: int64(promptTokens), CompletionTokens: int64(request.MaxOutputTokens),
			TotalTokens: int64(promptTokens) + int64(request.MaxOutputTokens), Outcome: CanaryOutcomeExecutionFailed, ClosedReason: v1alpha1.CanaryFailureAccountingUncertain,
		}, true
	}
	result := buildCanaryResult(request, response, startedAt, completedAt)
	usageValid := response.Usage.Reported && response.Usage.PromptTokens == promptTokens && response.Usage.PromptTokens >= 0 &&
		response.Usage.CompletionTokens >= 0 && response.Usage.CompletionTokens <= int(request.MaxOutputTokens) &&
		(response.Usage.TotalTokens == 0 || response.Usage.TotalTokens == response.Usage.PromptTokens+response.Usage.CompletionTokens)
	usageEstimated := !usageValid
	if usageEstimated {
		result.PromptTokens = int64(promptTokens)
		result.CompletionTokens = int64(request.MaxOutputTokens)
		result.TotalTokens = result.PromptTokens + result.CompletionTokens
	}
	switch {
	case len(response.ToolCalls) > 0:
		result.Outcome, result.ClosedReason = CanaryOutcomeExecutionFailed, v1alpha1.CanaryFailureToolCallRequested
	case len([]byte(response.Content)) > maxCanaryOutcomeBytes:
		result.Outcome, result.ClosedReason = CanaryOutcomeExecutionFailed, v1alpha1.CanaryFailureOutputLimitExceeded
	case !utf8.ValidString(response.Content):
		result.Outcome, result.ClosedReason = CanaryOutcomeExecutionFailed, v1alpha1.CanaryFailureInvalidEncoding
	case !usageValid:
		result.Outcome, result.ClosedReason = CanaryOutcomeExecutionFailed, v1alpha1.CanaryFailureAccountingUncertain
	}
	return result, usageEstimated
}

func canaryReservationID(operationID string) string { return "reasoning-canary:" + operationID }

type reasoningProbeAuditResourceV1 struct {
	Kind            string `json:"kind"`
	Namespace       string `json:"namespace,omitempty"`
	Name            string `json:"name"`
	UID             string `json:"uid"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
}

type reasoningProbeAuditReasonV1 string

const (
	reasoningProbeAuditExpectedOutcomeMatched  reasoningProbeAuditReasonV1 = "ExpectedOutcomeMatched"
	reasoningProbeAuditExpectedOutcomeMismatch reasoningProbeAuditReasonV1 = "ExpectedOutcomeMismatch"
)

type reasoningProbeAuditEventV1 struct {
	Version          string                        `json:"version"`
	ID               string                        `json:"id"`
	OperationID      string                        `json:"operationId"`
	Phase            string                        `json:"phase"`
	Timestamp        string                        `json:"timestamp"`
	Agent            reasoningProbeAuditResourceV1 `json:"agent"`
	AgentInstance    reasoningProbeAuditResourceV1 `json:"agentInstance"`
	AgentRevision    int64                         `json:"agentRevision"`
	Model            reasoningProbeAuditResourceV1 `json:"model"`
	Outcome          CanaryOutcome                 `json:"outcome"`
	Reason           reasoningProbeAuditReasonV1   `json:"reason"`
	ExpectedBytes    int32                         `json:"expectedBytes"`
	ExpectedSHA256   string                        `json:"expectedSha256"`
	ActualBytes      *int32                        `json:"actualBytes,omitempty"`
	ActualSHA256     string                        `json:"actualSha256,omitempty"`
	PromptTokens     *int64                        `json:"promptTokens,omitempty"`
	CompletionTokens *int64                        `json:"completionTokens,omitempty"`
	UsageEstimated   *bool                         `json:"usageEstimated,omitempty"`
	TraceID          string                        `json:"traceId,omitempty"`
	SpanID           string                        `json:"spanId,omitempty"`
}

func (e *CPICanaryExecutor) buildCanaryTerminalRecord(ctx context.Context, request CanaryRequest, resolved *resolvedCanaryRequest, result CanaryResult, resultData []byte, completedAt time.Time, usageEstimated bool) (*store.CanaryTerminalRecord, error) {
	auditData, err := buildCanaryAuditEvent(request, result, completedAt, usageEstimated)
	if err != nil {
		return nil, err
	}
	usage, err := e.buildCanaryUsageEvent(ctx, request, resolved, result, completedAt, usageEstimated)
	if err != nil {
		return nil, err
	}
	usageData, err := json.Marshal(usage)
	if err != nil {
		return nil, err
	}
	return &store.CanaryTerminalRecord{Result: resultData, UsageEvent: usageData, AuditEvent: auditData, ReservationCreated: true}, nil
}

func buildCanaryPreCallTerminalRecord(request CanaryRequest, result CanaryResult, resultData []byte, completedAt time.Time, reservationCreated bool) (*store.CanaryTerminalRecord, error) {
	auditData, err := buildCanaryAuditEvent(request, result, completedAt, false)
	if err != nil {
		return nil, err
	}
	return &store.CanaryTerminalRecord{Result: resultData, AuditEvent: auditData, ReservationCreated: reservationCreated}, nil
}

func buildCanaryAuditEvent(request CanaryRequest, result CanaryResult, completedAt time.Time, usageEstimated bool) ([]byte, error) {
	auditID := sha256.Sum256([]byte(request.OperationID + "\x00completed"))
	bindings := make(map[string]v1alpha1.CanaryResourceBinding, len(request.Bindings))
	for _, binding := range request.Bindings {
		bindings[binding.Kind] = binding
	}
	resource := func(binding v1alpha1.CanaryResourceBinding) reasoningProbeAuditResourceV1 {
		return reasoningProbeAuditResourceV1{Kind: binding.Kind, Namespace: binding.Namespace, Name: binding.Name, UID: binding.UID, ResourceVersion: binding.ResourceVersion}
	}
	agent := resource(bindings["Agent"])
	model := resource(bindings["Model"])
	instance := reasoningProbeAuditResourceV1{Kind: "AgentInstance", Namespace: request.Instance.ObjectMeta.Namespace, Name: request.Instance.ObjectMeta.Name, UID: request.Instance.ObjectMeta.UID, ResourceVersion: strconv.FormatInt(request.Instance.ObjectMeta.ResourceVersion, 10)}
	event := reasoningProbeAuditEventV1{
		Version: "reasoning-probe/v1", ID: hex.EncodeToString(auditID[:]), OperationID: request.OperationID, Phase: "completed",
		Timestamp: completedAt.UTC().Format(time.RFC3339Nano), Agent: agent, AgentInstance: instance, AgentRevision: request.AgentRevision,
		Model: model, Outcome: result.Outcome, Reason: canaryAuditReason(result), ExpectedBytes: result.ExpectedBytes,
		ExpectedSHA256: result.ExpectedSHA256, TraceID: result.TraceID, SpanID: result.SpanID,
	}
	if result.ActualSHA256 != "" {
		event.ActualBytes, event.ActualSHA256 = &result.ActualBytes, result.ActualSHA256
	}
	if result.PromptTokens > 0 || result.CompletionTokens > 0 {
		event.PromptTokens, event.CompletionTokens = &result.PromptTokens, &result.CompletionTokens
		event.UsageEstimated = &usageEstimated
	}
	auditData, err := json.Marshal(event)
	if err != nil {
		return nil, err
	}
	return auditData, nil
}

func preCallCanaryFailure(request CanaryRequest, promptTokens int, reason v1alpha1.CanaryFailureReason) CanaryResult {
	expectedDigest := sha256.Sum256([]byte(request.ExpectedOutcome))
	return CanaryResult{
		OperationID: request.OperationID, ExpectedBytes: int32(len([]byte(request.ExpectedOutcome))),
		ExpectedSHA256: hex.EncodeToString(expectedDigest[:]), PromptTokens: int64(promptTokens),
		Outcome: CanaryOutcomeExecutionFailed, ClosedReason: reason,
	}
}

func (e *CPICanaryExecutor) buildCanaryUsageEvent(ctx context.Context, request CanaryRequest, resolved *resolvedCanaryRequest, result CanaryResult, completedAt time.Time, usageEstimated bool) (observability.CostRecord, error) {
	pricing, err := observability.ResolveModelPricing(resolved.modelClass.Spec.Parameters)
	if err != nil {
		return observability.CostRecord{}, err
	}
	cost, _, err := observability.ModelCostUnits(resolved.modelClass.Spec.Parameters, result.PromptTokens, result.CompletionTokens)
	if err != nil {
		return observability.CostRecord{}, err
	}
	event := observability.CostRecord{
		ID: request.OperationID, Workspace: request.Instance.ObjectMeta.Namespace, Agent: resolved.agent.ObjectMeta.Name, AgentNamespace: request.Instance.ObjectMeta.Namespace,
		Workflow: request.Instance.ObjectMeta.Annotations[observability.AnnotationWorkflowName], Step: request.Instance.ObjectMeta.Annotations[observability.AnnotationStepName],
		SessionID: request.SessionID, ReservationID: canaryReservationID(request.OperationID), OperationType: observability.CostOpModelInference, Purpose: observability.UsagePurposeReasoningProbe,
		ModelName: resolved.model.Spec.ModelID, ModelProvider: resolved.modelClass.Spec.Provisioner, ModelClass: resolved.modelClass.ObjectMeta.Name,
		ModelClassVersion: resolved.modelClass.Spec.VersionPin, ModelCostTier: resolved.modelClass.Spec.CostTier,
		PromptTokens: result.PromptTokens, CompletionTokens: result.CompletionTokens, TotalTokens: result.TotalTokens, UsageReported: !usageEstimated, UsageEstimated: usageEstimated,
		PricingConfigured: pricing.Configured, InputCostMicrousdPerMillionTokens: pricing.InputMicrousdPerMillionTokens,
		OutputCostMicrousdPerMillionTokens: pricing.OutputMicrousdPerMillionTokens, CostUnits: cost,
		ProviderDurationMillis: result.ProviderDurationMillis, Timestamp: completedAt.UTC(), TraceID: result.TraceID, SpanID: result.SpanID,
	}
	identity, identityErr := getStoredResource[v1alpha1.AgentIdentity](ctx, e.store, "AgentIdentity", request.Instance.ObjectMeta.Namespace, "agent-identity-"+resolved.agent.ObjectMeta.Name)
	if identityErr == nil {
		event.AgentIdentity = identity.Spec.SPIFFEID
	}
	if resolved.agent.ObjectMeta.Labels != nil {
		event.Solution = resolved.agent.ObjectMeta.Labels["kognition.dev/solution"]
		if event.Solution != "" {
			if solution, solutionErr := getStoredResource[v1alpha1.Solution](ctx, e.store, "Solution", request.Instance.ObjectMeta.Namespace, event.Solution); solutionErr == nil {
				event.SolutionRev = solution.Status.ActiveRevision
			}
		}
	}
	return event, nil
}

func canaryAuditEntry(data json.RawMessage) (observability.AuditEntry, error) {
	var event reasoningProbeAuditEventV1
	if err := json.Unmarshal(data, &event); err != nil {
		return observability.AuditEntry{}, err
	}
	timestamp, err := time.Parse(time.RFC3339Nano, event.Timestamp)
	if err != nil {
		return observability.AuditEntry{}, err
	}
	return observability.AuditEntry{
		ID: event.ID, Timestamp: timestamp, Workspace: event.Agent.Namespace, EventType: observability.AuditReasoningStep,
		SubjectKind: "ReasoningProbe", SubjectRef: event.OperationID, Verb: "completed", Details: data,
		TraceID: event.TraceID, SpanID: event.SpanID,
	}, nil
}

func canaryAuditReason(result CanaryResult) reasoningProbeAuditReasonV1 {
	if result.ClosedReason != 0 {
		return reasoningProbeAuditReasonV1(result.ClosedReason.String())
	}
	if result.Outcome == CanaryOutcomePassed {
		return reasoningProbeAuditExpectedOutcomeMatched
	}
	return reasoningProbeAuditExpectedOutcomeMismatch
}
