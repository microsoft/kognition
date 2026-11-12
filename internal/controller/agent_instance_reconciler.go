// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/modelgateway"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

const (
	// AgentInstanceFinalizer is the finalizer applied to AgentInstance resources.
	AgentInstanceFinalizer = "kognition.dev/instance-cleanup"
	// AgentIdentityContextKey carries a non-secret SPIFFE URI on the ephemeral dispatch copy.
	AgentIdentityContextKey = "identity.spiffeID"
	// MemoryCapabilityContextKey holds an ephemeral, session-scoped capability.
	// It must never be persisted to an AgentInstance or a pod template.
	MemoryCapabilityContextKey = "memory.capability"
	// MemoryCapabilityIdentityNameContextKey binds a capability to the exact
	// automatic identity selected for this dispatch. It is ephemeral for the
	// same reason as MemoryCapabilityContextKey.
	MemoryCapabilityIdentityNameContextKey = "memory.capability-identity-name"
	// ToolCapabilityIdentityNameContextKey binds a Tool Gateway session
	// capability to the exact automatic identity selected for this dispatch.
	ToolCapabilityIdentityNameContextKey = "tool.capability-identity-name"
	// ModelCapabilityIdentityNameContextKey binds a Model Gateway session
	// capability to the exact automatic identity selected for this dispatch.
	ModelCapabilityIdentityNameContextKey = "model.capability-identity-name"
	// KnowledgeCapabilityIdentityNameContextKey binds a Knowledge Gateway
	// session capability to the selected automatic identity.
	KnowledgeCapabilityIdentityNameContextKey = "knowledge.capability-identity-name"
	// AgentDelegationCapabilityIdentityNameContextKey binds an Agent
	// Communication Gateway session capability to the selected identity.
	AgentDelegationCapabilityIdentityNameContextKey = "agent-delegation.capability-identity-name"
	DefaultAgentInvocationTimeout                   = 30 * time.Second
)

// RuntimeDispatcher is the interface for dispatching agent instances to a runtime.
type RuntimeDispatcher interface {
	// Dispatch starts the agent instance and returns a session ID.
	Dispatch(ctx context.Context, instance *v1alpha1.AgentInstance) (sessionID string, err error)

	// Status returns the runtime status for a running session.
	Status(ctx context.Context, instance *v1alpha1.AgentInstance) (RuntimeStatus, error)
}

// RuntimeSessionTerminator is implemented by dispatchers that can
// cooperatively stop a live runtime session.
type RuntimeSessionTerminator interface {
	Terminate(ctx context.Context, instance *v1alpha1.AgentInstance, reason string) error
}

// RuntimePlacementDispatcher optionally exposes concrete replicas and accepts
// a best-effort preference. Implementations must revalidate the preference and
// fall back safely without changing the AgentInstance identity.
type RuntimePlacementDispatcher interface {
	RuntimeDispatcher
	RuntimeReplicas(ctx context.Context, instance *v1alpha1.AgentInstance) ([]RuntimeReplica, error)
	DispatchTo(ctx context.Context, instance *v1alpha1.AgentInstance, preferredReplica string) (sessionID string, err error)
}

// RuntimeStatus reports liveness/progress for a running AgentInstance session.
type RuntimeStatus struct {
	Alive bool
	// Pending distinguishes a provisioning runtime from an available warm replica.
	Pending              bool
	Completed            bool
	RuntimeUptimeSeconds float64
	TokenUsage           *v1alpha1.TokenUsage
	ToolCalls            int32
	ToolCallsDetail      []v1alpha1.ToolCallDetail
	Output               string
	SPIFFEID             string
	// Error is a human-readable message describing why the runtime is not
	// alive. Populated when Alive is false (e.g. graph build failed due to
	// missing credentials). Surfaced in the AgentInstance Ready condition
	// so Studio users see the true cause rather than a generic message.
	Error string
}

// AgentInstanceQuotaEnforcer accounts for tokens used by runtime sessions.
type AgentInstanceQuotaEnforcer interface {
	CheckSchedulerQuota(context.Context, string) error
	CheckCallBudget(context.Context, string, string, int64, int64) error
	CheckRuntimeBudget(context.Context, string, string, int64) error
	ReserveTokens(context.Context, string, string, int64, int64) error
	Settle(context.Context, string, int64, int64) error
}

// AgentInstanceTrustRecorder records terminal runtime outcomes for trust scoring.
type AgentInstanceTrustRecorder interface {
	RecordAgentInstanceOutcome(context.Context, *v1alpha1.AgentInstance) error
}

// AgentInstanceModelAuthorizer protects runtime model dispatch.
type AgentInstanceModelAuthorizer interface {
	EvaluateModelAccess(context.Context, string, string, string) error
}

// defaultDispatcher is a stub runtime dispatcher that simulates successful dispatch.
type defaultDispatcher struct{}

func (d *defaultDispatcher) Dispatch(_ context.Context, _ *v1alpha1.AgentInstance) (string, error) {
	return uuid.New().String(), nil
}

func (d *defaultDispatcher) Status(_ context.Context, _ *v1alpha1.AgentInstance) (RuntimeStatus, error) {
	return RuntimeStatus{Alive: true}, nil
}

// AgentInstanceReconcilerConfig controls retry and polling behavior.
type AgentInstanceReconcilerConfig struct {
	DispatchRetryLimit       int
	DispatchBackoffBase      time.Duration
	DispatchBackoffMax       time.Duration
	RunningPollInterval      time.Duration
	RequireAutomaticIdentity bool
}

func (c *AgentInstanceReconcilerConfig) setDefaults() {
	if c.DispatchRetryLimit <= 0 {
		c.DispatchRetryLimit = 3
	}
	if c.DispatchBackoffBase <= 0 {
		c.DispatchBackoffBase = 1 * time.Second
	}
	if c.DispatchBackoffMax <= 0 {
		c.DispatchBackoffMax = 30 * time.Second
	}
	if c.RunningPollInterval <= 0 {
		c.RunningPollInterval = 10 * time.Second
	}
}

// AgentInstanceReconciler reconciles AgentInstance resources by dispatching
// to a runtime and tracking lifecycle phases.
type AgentInstanceReconciler struct {
	store      store.Store
	dispatcher RuntimeDispatcher
	scheduler  CognitiveScheduler
	secrets    runtimeSecretResolver
	quota      AgentInstanceQuotaEnforcer
	trust      AgentInstanceTrustRecorder
	models     AgentInstanceModelAuthorizer
	audit      *observability.CognitiveAuditLog
	logger     *zap.Logger
	config     AgentInstanceReconcilerConfig
}

// SetScheduler enables best-effort same-Agent runtime placement.
func (r *AgentInstanceReconciler) SetScheduler(scheduler CognitiveScheduler) {
	r.scheduler = scheduler
}

// SetQuotaEnforcer enables quota accounting for dispatched runtime sessions.
func (r *AgentInstanceReconciler) SetQuotaEnforcer(enforcer AgentInstanceQuotaEnforcer) {
	r.quota = enforcer
}

// SetAuditLog installs the durable recorder for pre-dispatch Model decisions.
func (r *AgentInstanceReconciler) SetAuditLog(audit *observability.CognitiveAuditLog) {
	r.audit = audit
}

// SetTrustRecorder enables terminal AgentInstance trust scoring.
func (r *AgentInstanceReconciler) SetTrustRecorder(recorder AgentInstanceTrustRecorder) {
	r.trust = recorder
}

// SetModelAuthorizer enables model policy checks immediately before dispatch.
func (r *AgentInstanceReconciler) SetModelAuthorizer(authorizer AgentInstanceModelAuthorizer) {
	r.models = authorizer
}

// SetRequireAutomaticIdentity requires a controller-managed identity for every
// dispatch. Model Gateway enables it because every session capability is bound
// to the automatic AgentIdentity.
func (r *AgentInstanceReconciler) SetRequireAutomaticIdentity(required bool) {
	r.config.RequireAutomaticIdentity = required
}

// NewAgentInstanceReconciler creates a new AgentInstanceReconciler.
func NewAgentInstanceReconciler(s store.Store, logger *zap.Logger) *AgentInstanceReconciler {
	config := AgentInstanceReconcilerConfig{}
	config.setDefaults()
	return &AgentInstanceReconciler{
		store:      s,
		dispatcher: &defaultDispatcher{},
		secrets:    newStoreSecretResolver(s),
		logger:     logger.With(zap.String("reconciler", "agent-instance")),
		config:     config,
	}
}

// NewAgentInstanceReconcilerWithDispatcher creates a reconciler with a custom dispatcher.
func NewAgentInstanceReconcilerWithDispatcher(s store.Store, dispatcher RuntimeDispatcher, logger *zap.Logger) *AgentInstanceReconciler {
	config := AgentInstanceReconcilerConfig{}
	config.setDefaults()
	return &AgentInstanceReconciler{
		store:      s,
		dispatcher: dispatcher,
		secrets:    newStoreSecretResolver(s),
		logger:     logger.With(zap.String("reconciler", "agent-instance")),
		config:     config,
	}
}

// NewAgentInstanceReconcilerWithConfig creates a reconciler with custom retry/poll settings.
func NewAgentInstanceReconcilerWithConfig(s store.Store, dispatcher RuntimeDispatcher, logger *zap.Logger, config AgentInstanceReconcilerConfig) *AgentInstanceReconciler {
	config.setDefaults()
	return &AgentInstanceReconciler{
		store:      s,
		dispatcher: dispatcher,
		secrets:    newStoreSecretResolver(s),
		logger:     logger.With(zap.String("reconciler", "agent-instance")),
		config:     config,
	}
}

// Reconcile implements the Reconciler interface for AgentInstance resources.
func (r *AgentInstanceReconciler) Reconcile(ctx context.Context, req Request) (Result, error) {
	log := r.logger.With(zap.String("namespace", req.Namespace), zap.String("name", req.Name), requestid.ZapField(ctx))

	// Fetch the AgentInstance from the store.
	raw, err := r.store.Get(ctx, agentInstanceKind, req.Namespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			log.Debug("agent instance not found, likely deleted")
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("get agent instance: %w", err)
	}

	var instance v1alpha1.AgentInstance
	if err := json.Unmarshal(raw, &instance); err != nil {
		return Result{}, fmt.Errorf("unmarshal agent instance: %w", err)
	}
	originalStatus := snapshotAgentInstanceStatus(instance.Status)
	originalFinalizers := append([]string(nil), instance.ObjectMeta.Finalizers...)

	// Handle deletion.
	if instance.ObjectMeta.DeletionTimestamp != nil {
		return r.handleDeletion(ctx, &instance, originalStatus, originalFinalizers, log)
	}

	// Ensure finalizer is present.
	if !lifecycle.HasFinalizer(instance.ObjectMeta.Finalizers, AgentInstanceFinalizer) {
		finalizers, _ := lifecycle.AddFinalizer(instance.ObjectMeta.Finalizers, AgentInstanceFinalizer)
		instance.ObjectMeta.Finalizers = finalizers
		if err := r.updateInstance(ctx, &instance, originalStatus, originalFinalizers); err != nil {
			return Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		return Result{Requeue: true}, nil
	}

	// Reconcile based on current phase.
	switch instance.Status.Phase {
	case v1alpha1.PhasePending:
		return r.reconcilePending(ctx, &instance, originalStatus, originalFinalizers, log)
	case v1alpha1.PhaseRunning:
		return r.reconcileRunning(ctx, &instance, originalStatus, originalFinalizers, log)
	case v1alpha1.PhaseReady, v1alpha1.PhaseFailed:
		return Result{}, r.finishTerminalAccounting(ctx, &instance)
	default:
		// Unknown phase, treat as pending.
		instance.Status.Phase = v1alpha1.PhasePending
		if err := r.updateInstance(ctx, &instance, originalStatus, originalFinalizers); err != nil {
			return Result{}, fmt.Errorf("reset phase: %w", err)
		}
		return Result{Requeue: true}, nil
	}
}

// reconcilePending dispatches the instance to the runtime and transitions to Running.
func (r *AgentInstanceReconciler) reconcilePending(ctx context.Context, instance *v1alpha1.AgentInstance, originalStatus v1alpha1.AgentInstanceStatus, originalFinalizers []string, log *zap.Logger) (Result, error) {
	log.Info("dispatching agent instance to runtime")

	if err := r.authorizeModel(ctx, instance); err != nil {
		lcConditions := lifecycle.SetCondition(
			apiToLifecycleConditions(instance.Status.Conditions),
			lifecycle.ConditionReady,
			lifecycle.ConditionFalse,
			"ModelAccessDenied",
			err.Error(),
		)
		instance.Status.Conditions = lifecycleToAPIConditions(lcConditions)
		if updateErr := r.updateInstance(ctx, instance, originalStatus, originalFinalizers); updateErr != nil {
			return Result{}, fmt.Errorf("update after model access denial: %w", updateErr)
		}
		return Result{RequeueAfter: 30 * time.Second}, nil
	}
	dispatchInstance, err := r.buildDispatchInstance(ctx, instance)
	if err != nil {
		if errors.Is(err, ErrAgentIdentityNotReady) {
			lcConditions := lifecycle.SetCondition(
				apiToLifecycleConditions(instance.Status.Conditions),
				lifecycle.ConditionReady,
				lifecycle.ConditionFalse,
				"IdentityPending",
				"waiting for AgentIdentity to become ready",
			)
			instance.Status.Conditions = lifecycleToAPIConditions(lcConditions)
			if updateErr := r.updateInstance(ctx, instance, originalStatus, originalFinalizers); updateErr != nil {
				return Result{}, fmt.Errorf("update while waiting for identity: %w", updateErr)
			}
			return Result{RequeueAfter: time.Second}, nil
		}
		instance.Status.DispatchAttempts++
		attempt := instance.Status.DispatchAttempts

		if int(attempt) <= r.config.DispatchRetryLimit {
			backoff := r.dispatchBackoff(attempt)
			lcConditions := lifecycle.SetCondition(
				apiToLifecycleConditions(instance.Status.Conditions),
				lifecycle.ConditionReady,
				lifecycle.ConditionFalse,
				"DispatchRetrying",
				fmt.Sprintf("dispatch preparation attempt %d/%d failed: %v", attempt, r.config.DispatchRetryLimit, err),
			)
			instance.Status.Conditions = lifecycleToAPIConditions(lcConditions)
			if updateErr := r.updateInstance(ctx, instance, originalStatus, originalFinalizers); updateErr != nil {
				return Result{}, fmt.Errorf("update after dispatch preparation failure: %w", updateErr)
			}
			return Result{RequeueAfter: backoff}, nil
		}

		instance.Status.Phase = v1alpha1.PhaseFailed
		lcConditions := lifecycle.SetCondition(
			apiToLifecycleConditions(instance.Status.Conditions),
			lifecycle.ConditionReady,
			lifecycle.ConditionFalse,
			"DispatchFailed",
			fmt.Sprintf("failed to prepare runtime dispatch after %d attempts: %v", attempt, err),
		)
		instance.Status.Conditions = lifecycleToAPIConditions(lcConditions)
		if updateErr := r.updateInstance(ctx, instance, originalStatus, originalFinalizers); updateErr != nil {
			return Result{}, fmt.Errorf("update after dispatch preparation terminal failure: %w", updateErr)
		}
		return Result{}, nil
	}
	quotaModelName, quotaErr := r.reserveQuota(ctx, dispatchInstance)
	if quotaErr != nil {
		decision := modelgateway.StandaloneDecisionInput{Workspace: dispatchInstance.ObjectMeta.Namespace, SessionID: agentInstanceReservationID(dispatchInstance), InstanceName: dispatchInstance.ObjectMeta.Name, InstanceUID: dispatchInstance.ObjectMeta.UID, ModelName: quotaModelName}
		if auditErr := modelgateway.RecordStandaloneDecision(ctx, r.audit, decision, "denied", "quota_exhausted", "blocked", false, true); auditErr != nil {
			return Result{}, fmt.Errorf("persist quota-denied model evidence: %w", auditErr)
		}
		lcConditions := lifecycle.SetCondition(
			apiToLifecycleConditions(instance.Status.Conditions),
			lifecycle.ConditionReady,
			lifecycle.ConditionFalse,
			"QuotaExceeded",
			quotaErr.Error(),
		)
		instance.Status.Conditions = lifecycleToAPIConditions(lcConditions)
		if updateErr := r.updateInstance(ctx, instance, originalStatus, originalFinalizers); updateErr != nil {
			return Result{}, fmt.Errorf("update after quota denial: %w", updateErr)
		}
		return Result{RequeueAfter: 30 * time.Second}, nil
	}

	sessionID, err := r.dispatch(ctx, dispatchInstance, log)
	if err != nil {
		instance.Status.DispatchAttempts++
		attempt := instance.Status.DispatchAttempts
		if errors.Is(err, ErrDispatchAccountingUncertain) {
			instance.Status.Phase = v1alpha1.PhaseFailed
			lcConditions := lifecycle.SetCondition(
				apiToLifecycleConditions(instance.Status.Conditions),
				lifecycle.ConditionReady,
				lifecycle.ConditionFalse,
				"UsageAccountingUncertain",
				"provider execution completed but usage accounting is uncertain; automatic retry is disabled",
			)
			instance.Status.Conditions = lifecycleToAPIConditions(lcConditions)
			if updateErr := r.updateInstance(ctx, instance, originalStatus, originalFinalizers); updateErr != nil {
				return Result{}, fmt.Errorf("update after uncertain usage accounting: %w", updateErr)
			}
			return Result{}, nil
		}

		if int(attempt) <= r.config.DispatchRetryLimit {
			backoff := r.dispatchBackoff(attempt)
			lcConditions := lifecycle.SetCondition(
				apiToLifecycleConditions(instance.Status.Conditions),
				lifecycle.ConditionReady,
				lifecycle.ConditionFalse,
				"DispatchRetrying",
				fmt.Sprintf("dispatch attempt %d/%d failed: %v", attempt, r.config.DispatchRetryLimit, err),
			)
			instance.Status.Conditions = lifecycleToAPIConditions(lcConditions)
			if updateErr := r.updateInstance(ctx, instance, originalStatus, originalFinalizers); updateErr != nil {
				return Result{}, fmt.Errorf("update after dispatch retry failure: %w", updateErr)
			}
			return Result{RequeueAfter: backoff}, nil
		}

		// Retry budget exhausted — mark as Failed.
		instance.Status.Phase = v1alpha1.PhaseFailed
		lcConditions := lifecycle.SetCondition(
			apiToLifecycleConditions(instance.Status.Conditions),
			lifecycle.ConditionReady,
			lifecycle.ConditionFalse,
			"DispatchFailed",
			fmt.Sprintf("failed to dispatch to runtime after %d attempts: %v", attempt, err),
		)
		instance.Status.Conditions = lifecycleToAPIConditions(lcConditions)
		if updateErr := r.updateInstance(ctx, instance, originalStatus, originalFinalizers); updateErr != nil {
			return Result{}, fmt.Errorf("update after dispatch failure: %w", updateErr)
		}
		return Result{}, r.finishTerminalAccounting(ctx, instance)
	}

	// Transition to Running.
	now := time.Now().UTC()
	instance.Status.Phase = v1alpha1.PhaseRunning
	instance.Status.SessionID = sessionID
	instance.Status.StartTime = &now
	instance.Status.DispatchAttempts = 0

	lcConditions := lifecycle.SetCondition(
		apiToLifecycleConditions(instance.Status.Conditions),
		lifecycle.ConditionReady,
		lifecycle.ConditionTrue,
		"Dispatched",
		"instance dispatched to runtime successfully",
	)
	instance.Status.Conditions = lifecycleToAPIConditions(lcConditions)

	if err := r.updateInstance(ctx, instance, originalStatus, originalFinalizers); err != nil {
		return Result{}, fmt.Errorf("update after dispatch: %w", err)
	}

	log.Info("agent instance dispatched",
		zap.String("sessionID", sessionID),
	)
	return Result{}, nil
}

func (r *AgentInstanceReconciler) dispatch(ctx context.Context, instance *v1alpha1.AgentInstance, log *zap.Logger) (string, error) {
	placementDispatcher, supportsPlacement := r.dispatcher.(RuntimePlacementDispatcher)
	if r.scheduler == nil || !supportsPlacement {
		return r.dispatcher.Dispatch(ctx, instance)
	}

	preferredReplica := ""
	replicas, err := placementDispatcher.RuntimeReplicas(ctx, instance)
	if err != nil {
		log.Warn("runtime replica discovery failed; using Agent service", zap.Error(err))
	} else {
		result, scheduleErr := r.scheduler.Schedule(ctx, ScheduleRequest{
			Namespace: instance.ObjectMeta.Namespace,
			AgentName: instance.Spec.AgentRef.Name,
			SessionID: agentInstanceReservationID(instance),
			Replicas:  replicas,
		})
		if scheduleErr != nil {
			log.Warn("runtime placement failed; using Agent service", zap.Error(scheduleErr))
		} else if result.AgentName != "" && result.AgentName != instance.Spec.AgentRef.Name {
			log.Warn("runtime placement returned a different Agent; using Agent service",
				zap.String("requestedAgent", instance.Spec.AgentRef.Name),
				zap.String("resultAgent", result.AgentName))
		} else {
			preferredReplica = result.PreferredReplica
		}
	}

	return placementDispatcher.DispatchTo(ctx, instance, preferredReplica)
}

func (r *AgentInstanceReconciler) authorizeModel(ctx context.Context, instance *v1alpha1.AgentInstance) error {
	if r.models == nil {
		return nil
	}
	agent, err := ResolveExecutionAgent(ctx, r.store, instance.ObjectMeta.Namespace, instance)
	if err != nil {
		return fmt.Errorf("load agent for model authorization: %w", err)
	}
	if err := r.models.EvaluateModelAccess(ctx, instance.ObjectMeta.Namespace, instance.Spec.AgentRef.Name, agent.Spec.ModelRef.Name); err != nil {
		return fmt.Errorf("authorize model %q for agent %q: %w", agent.Spec.ModelRef.Name, instance.Spec.AgentRef.Name, err)
	}
	return nil
}

func (r *AgentInstanceReconciler) buildDispatchInstance(ctx context.Context, instance *v1alpha1.AgentInstance) (*v1alpha1.AgentInstance, error) {
	dispatchInstance := *instance
	dispatchInstance.Spec = instance.Spec
	dispatchInstance.Spec.SessionContext = cloneStringMap(instance.Spec.SessionContext)

	identityRequired := instance.ObjectMeta.Annotations[requireAgentIdentityAnnotation] == "true" || r.config.RequireAutomaticIdentity
	identityName := instance.ObjectMeta.Annotations[autoAgentIdentityNameAnnotation]
	if identityName == "" {
		identityName = defaultAgentIdentityName(instance.Spec.AgentRef.Name)
	}
	var identity *v1alpha1.AgentIdentity
	var identityErr error
	if !identityRequired {
		agent, agentErr := getStoredResource[v1alpha1.Agent](ctx, r.store, "Agent", instance.ObjectMeta.Namespace, instance.Spec.AgentRef.Name)
		if agentErr == nil && (len(agent.Spec.Tools) > 0 || len(agent.Spec.KnowledgeBaseClaims) > 0 || (agent.Spec.Memory != nil && len(agent.Spec.Memory.Claims) > 0)) {
			identityRequired = true
		}
	}
	if identityRequired {
		automaticIdentity, automaticErr := ResolveAutomaticAgentIdentity(ctx, r.store, instance.ObjectMeta.Namespace, instance.Spec.AgentRef.Name, identityName)
		if automaticErr == nil {
			identity = automaticIdentity
		} else {
			return nil, fmt.Errorf("%w: %w", ErrAgentIdentityNotReady, automaticErr)
		}
	}
	if identity == nil && identityRequired {
		identity, identityErr = ResolveAutomaticAgentIdentity(ctx, r.store, instance.ObjectMeta.Namespace, instance.Spec.AgentRef.Name, identityName)
	} else if identity == nil {
		identity, identityErr = ResolveAgentIdentity(ctx, r.store, instance.ObjectMeta.Namespace, instance.Spec.AgentRef.Name)
	}
	if identityErr != nil {
		if errors.Is(identityErr, ErrAgentIdentityNotFound) {
			if identityRequired {
				return nil, fmt.Errorf("%w: %w", ErrAgentIdentityNotReady, identityErr)
			}
			// Legacy AgentInstances may predate automatic identity provisioning.
			identity = nil
		} else {
			return nil, identityErr
		}
	}
	if identityRequired && identity != nil && (identity.Status.Phase != v1alpha1.PhaseReady || identity.Spec.SPIFFEID == "") {
		return nil, fmt.Errorf("%w: %q", ErrAgentIdentityNotReady, identity.ObjectMeta.Name)
	}
	if identity != nil && dispatchInstance.Spec.SessionContext == nil {
		dispatchInstance.Spec.SessionContext = make(map[string]string)
	}
	if identity != nil {
		dispatchInstance.Spec.SessionContext[AgentIdentityContextKey] = identity.Spec.SPIFFEID
		if identityRequired {
			dispatchInstance.Spec.SessionContext[MemoryCapabilityIdentityNameContextKey] = identity.ObjectMeta.Name
			dispatchInstance.Spec.SessionContext[ToolCapabilityIdentityNameContextKey] = identity.ObjectMeta.Name
			dispatchInstance.Spec.SessionContext[ModelCapabilityIdentityNameContextKey] = identity.ObjectMeta.Name
			dispatchInstance.Spec.SessionContext[KnowledgeCapabilityIdentityNameContextKey] = identity.ObjectMeta.Name
			dispatchInstance.Spec.SessionContext[AgentDelegationCapabilityIdentityNameContextKey] = identity.ObjectMeta.Name
		}
	}

	if r.secrets == nil {
		return &dispatchInstance, nil
	}

	secrets, err := r.secrets.ResolveAgentRuntimeSecrets(ctx, instance.ObjectMeta.Namespace, instance)
	if err != nil {
		return nil, err
	}
	if len(secrets) == 0 {
		return &dispatchInstance, nil
	}
	if dispatchInstance.Spec.SessionContext == nil {
		dispatchInstance.Spec.SessionContext = make(map[string]string, len(secrets))
	}
	for key, value := range secrets {
		dispatchInstance.Spec.SessionContext[key] = value
	}
	return &dispatchInstance, nil
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

// reconcileRunning polls runtime status and transitions terminal states.
func (r *AgentInstanceReconciler) reconcileRunning(ctx context.Context, instance *v1alpha1.AgentInstance, originalStatus v1alpha1.AgentInstanceStatus, originalFinalizers []string, _ *zap.Logger) (Result, error) {
	status, err := r.dispatcher.Status(ctx, instance)
	if err != nil {
		instance.Status.Phase = v1alpha1.PhaseFailed
		reason := "RuntimeStatusError"
		message := fmt.Sprintf("runtime status polling failed: %v", err)
		if errors.Is(err, ErrDispatchAccountingUncertain) {
			reason = "UsageAccountingUncertain"
			message = "runtime outcome cannot be verified; automatic retry is disabled and usage reservation is retained"
		}
		lcConditions := lifecycle.SetCondition(
			apiToLifecycleConditions(instance.Status.Conditions),
			lifecycle.ConditionReady,
			lifecycle.ConditionFalse,
			reason,
			message,
		)
		instance.Status.Conditions = lifecycleToAPIConditions(lcConditions)
		if updateErr := r.updateInstance(ctx, instance, originalStatus, originalFinalizers); updateErr != nil {
			return Result{}, fmt.Errorf("update after runtime status error: %w", updateErr)
		}
		return Result{}, r.finishTerminalAccounting(ctx, instance)
	}

	if status.TokenUsage != nil {
		instance.Status.TokenUsage = status.TokenUsage
		if r.quota != nil && instance.Spec.SessionContext["input"] != "" {
			componentTokens := status.TokenUsage.PromptTokens + status.TokenUsage.CompletionTokens
			usedTokens := status.TokenUsage.TotalTokens
			if usedTokens == 0 {
				usedTokens = componentTokens
			} else if componentTokens == 0 || usedTokens != componentTokens {
				return Result{}, fmt.Errorf("runtime token usage is inconsistent: total=%d prompt=%d completion=%d", usedTokens, status.TokenUsage.PromptTokens, status.TokenUsage.CompletionTokens)
			}
			if quotaErr := r.quota.CheckRuntimeBudget(ctx, instance.ObjectMeta.Namespace, instance.Spec.AgentRef.Name, usedTokens); quotaErr != nil {
				if !errors.Is(quotaErr, ErrBudgetExceeded) {
					return Result{}, fmt.Errorf("check runtime token budget: %w", quotaErr)
				}
				if status.Alive && !status.Completed {
					terminator, ok := r.dispatcher.(RuntimeSessionTerminator)
					if !ok {
						return Result{}, fmt.Errorf("runtime token budget exceeded but dispatcher does not support session termination")
					}
					if terminateErr := terminator.Terminate(ctx, instance, "token budget exceeded"); terminateErr != nil {
						return Result{}, fmt.Errorf("terminate runtime session after token budget exhaustion: %w", terminateErr)
					}
				}
				if eventErr := r.appendRuntimeQuotaWarningOnce(ctx, instance, usedTokens); eventErr != nil {
					return Result{}, fmt.Errorf("record runtime token budget exhaustion: %w", eventErr)
				}
				instance.Status.Phase = v1alpha1.PhaseFailed
				lcConditions := lifecycle.SetCondition(
					apiToLifecycleConditions(instance.Status.Conditions),
					lifecycle.ConditionBudgetExceeded,
					lifecycle.ConditionTrue,
					"RuntimeTokenBudgetExceeded",
					quotaErr.Error(),
				)
				lcConditions = lifecycle.SetCondition(
					lcConditions,
					lifecycle.ConditionReady,
					lifecycle.ConditionFalse,
					"RuntimeTokenBudgetExceeded",
					"runtime session terminated after reaching its token budget",
				)
				instance.Status.Conditions = lifecycleToAPIConditions(lcConditions)
				if updateErr := r.updateInstance(ctx, instance, originalStatus, originalFinalizers); updateErr != nil {
					return Result{}, fmt.Errorf("update after runtime token budget exhaustion: %w", updateErr)
				}
				return Result{}, r.finishTerminalAccounting(ctx, instance)
			}
		}
	}
	instance.Status.ToolCalls = status.ToolCalls
	instance.Status.ToolCallsDetail = status.ToolCallsDetail

	if status.Completed {
		instance.Status.Phase = v1alpha1.PhaseReady
		instance.Status.Output = status.Output
		lcConditions := lifecycle.SetCondition(
			apiToLifecycleConditions(instance.Status.Conditions),
			lifecycle.ConditionReady,
			lifecycle.ConditionTrue,
			"Completed",
			"instance session completed successfully",
		)
		instance.Status.Conditions = lifecycleToAPIConditions(lcConditions)
		if updateErr := r.updateInstance(ctx, instance, originalStatus, originalFinalizers); updateErr != nil {
			return Result{}, fmt.Errorf("update after completion: %w", updateErr)
		}
		return Result{}, r.finishTerminalAccounting(ctx, instance)
	}

	if status.Alive && !status.Pending && isWarmPoolInstance(instance) {
		instance.Status.Phase = v1alpha1.PhaseReady
		lcConditions := lifecycle.SetCondition(
			apiToLifecycleConditions(instance.Status.Conditions),
			lifecycle.ConditionReady,
			lifecycle.ConditionTrue,
			"RuntimeReady",
			"warm runtime replica is ready to accept sessions",
		)
		instance.Status.Conditions = lifecycleToAPIConditions(lcConditions)
		if updateErr := r.updateInstance(ctx, instance, originalStatus, originalFinalizers); updateErr != nil {
			return Result{}, fmt.Errorf("update after warm runtime readiness: %w", updateErr)
		}
		return Result{}, nil
	}

	if !status.Alive {
		instance.Status.Phase = v1alpha1.PhaseFailed
		msg := "runtime reported inactive session"
		if status.Error != "" {
			msg = fmt.Sprintf("runtime reported inactive session: %s", status.Error)
		}
		lcConditions := lifecycle.SetCondition(
			apiToLifecycleConditions(instance.Status.Conditions),
			lifecycle.ConditionReady,
			lifecycle.ConditionFalse,
			"SessionNotAlive",
			msg,
		)
		instance.Status.Conditions = lifecycleToAPIConditions(lcConditions)
		if updateErr := r.updateInstance(ctx, instance, originalStatus, originalFinalizers); updateErr != nil {
			return Result{}, fmt.Errorf("update after inactive session: %w", updateErr)
		}
		return Result{}, r.finishTerminalAccounting(ctx, instance)
	}

	expired, timeoutMessage, err := r.runningSessionExpired(ctx, instance, time.Now().UTC())
	if err != nil {
		instance.Status.Phase = v1alpha1.PhaseFailed
		lcConditions := lifecycle.SetCondition(
			apiToLifecycleConditions(instance.Status.Conditions),
			lifecycle.ConditionReady,
			lifecycle.ConditionFalse,
			"InvalidRuntimeTimeout",
			err.Error(),
		)
		instance.Status.Conditions = lifecycleToAPIConditions(lcConditions)
		if updateErr := r.updateInstance(ctx, instance, originalStatus, originalFinalizers); updateErr != nil {
			return Result{}, fmt.Errorf("update after invalid runtime timeout: %w", updateErr)
		}
		return Result{}, r.finishTerminalAccounting(ctx, instance)
	}
	if expired {
		terminator, ok := r.dispatcher.(RuntimeSessionTerminator)
		if !ok {
			return Result{}, fmt.Errorf("runtime session timed out but dispatcher does not support session termination")
		}
		if terminateErr := terminator.Terminate(ctx, instance, "session timeout exceeded"); terminateErr != nil {
			return Result{}, fmt.Errorf("terminate timed-out runtime session: %w", terminateErr)
		}
		instance.Status.Phase = v1alpha1.PhaseFailed
		lcConditions := lifecycle.SetCondition(
			apiToLifecycleConditions(instance.Status.Conditions),
			lifecycle.ConditionReady,
			lifecycle.ConditionFalse,
			"SessionTimeoutExceeded",
			timeoutMessage,
		)
		instance.Status.Conditions = lifecycleToAPIConditions(lcConditions)
		if updateErr := r.updateInstance(ctx, instance, originalStatus, originalFinalizers); updateErr != nil {
			return Result{}, fmt.Errorf("update after runtime session timeout: %w", updateErr)
		}
		return Result{}, r.finishTerminalAccounting(ctx, instance)
	}

	if err := r.updateInstance(ctx, instance, originalStatus, originalFinalizers); err != nil {
		return Result{}, fmt.Errorf("update running status: %w", err)
	}

	return Result{RequeueAfter: r.config.RunningPollInterval}, nil
}

func (r *AgentInstanceReconciler) runningSessionExpired(ctx context.Context, instance *v1alpha1.AgentInstance, now time.Time) (bool, string, error) {
	if isWarmPoolInstance(instance) || instance.Spec.SessionContext["input"] == "" {
		return false, "", nil
	}
	timeout, err := r.agentInvocationTimeout(ctx, instance)
	if err != nil {
		return false, "", err
	}
	if timeout <= 0 {
		return false, "", nil
	}
	startedAt := instance.Status.StartTime
	if startedAt == nil && !instance.ObjectMeta.CreationTimestamp.IsZero() {
		createdAt := instance.ObjectMeta.CreationTimestamp
		startedAt = &createdAt
	}
	if startedAt == nil || now.Sub(*startedAt) <= timeout {
		return false, "", nil
	}
	return true, fmt.Sprintf("runtime session exceeded timeout %s", timeout), nil
}

func (r *AgentInstanceReconciler) agentInvocationTimeout(ctx context.Context, instance *v1alpha1.AgentInstance) (time.Duration, error) {
	agent, err := getStoredResource[v1alpha1.Agent](ctx, r.store, "Agent", instance.ObjectMeta.Namespace, instance.Spec.AgentRef.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return 0, nil
		}
		return 0, fmt.Errorf("load agent runtime timeout: %w", err)
	}
	if agent.Spec.Runtime == nil || strings.TrimSpace(agent.Spec.Runtime.Timeout) == "" {
		return DefaultAgentInvocationTimeout, nil
	}
	timeout, err := ParseAgentInvocationTimeout(agent.Spec.Runtime.Timeout)
	if err != nil {
		return 0, fmt.Errorf("agent %q runtime timeout %q is invalid: %w", agent.ObjectMeta.Name, agent.Spec.Runtime.Timeout, err)
	}
	return timeout, nil
}

// ParseAgentInvocationTimeout accepts a positive Go duration or bare seconds.
func ParseAgentInvocationTimeout(raw string) (time.Duration, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return 0, nil
	}
	if timeout, err := time.ParseDuration(value); err == nil {
		if timeout <= 0 {
			return 0, fmt.Errorf("must be positive")
		}
		return timeout, nil
	}
	seconds, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, err
	}
	if seconds <= 0 {
		return 0, fmt.Errorf("must be positive")
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

func parseAgentInvocationTimeout(raw string) (time.Duration, error) {
	return ParseAgentInvocationTimeout(raw)
}

func (r *AgentInstanceReconciler) reserveQuota(ctx context.Context, instance *v1alpha1.AgentInstance) (string, error) {
	if r.quota == nil || instance.Spec.SessionContext["input"] == "" {
		return "", nil
	}
	namespace := instance.ObjectMeta.Namespace
	agent, err := getStoredResource[v1alpha1.Agent](ctx, r.store, "Agent", namespace, instance.Spec.AgentRef.Name)
	if err != nil {
		return "", fmt.Errorf("load agent for quota reservation: %w", err)
	}
	modelName := agent.Spec.ModelRef.Name
	if err := r.quota.CheckSchedulerQuota(ctx, namespace); err != nil {
		return modelName, fmt.Errorf("session quota unavailable: %w", err)
	}
	var outputCeiling int64
	if agent.Spec.Runtime != nil && agent.Spec.Runtime.MaxTokens != nil && *agent.Spec.Runtime.MaxTokens > 0 {
		outputCeiling = *agent.Spec.Runtime.MaxTokens
	}
	input := agent.Spec.Instructions.Inline + instance.Spec.SessionContext["input"]
	inputCeiling := int64(len([]rune(input))+3) / 4
	if err := r.quota.CheckCallBudget(ctx, namespace, instance.Spec.AgentRef.Name, 0, inputCeiling+outputCeiling); err != nil {
		return modelName, fmt.Errorf("session token budget unavailable: %w", err)
	}
	if err := r.quota.ReserveTokens(ctx, namespace, agentInstanceReservationID(instance), inputCeiling, outputCeiling); err != nil {
		return modelName, fmt.Errorf("reserve session tokens: %w", err)
	}
	return modelName, nil
}

func (r *AgentInstanceReconciler) settleQuota(ctx context.Context, instance *v1alpha1.AgentInstance) error {
	if r.quota == nil || instance.Spec.SessionContext["input"] == "" {
		return nil
	}
	var promptTokens, completionTokens int64
	if instance.Status.TokenUsage != nil {
		promptTokens = instance.Status.TokenUsage.PromptTokens
		completionTokens = instance.Status.TokenUsage.CompletionTokens
	}
	if err := r.quota.Settle(ctx, agentInstanceReservationID(instance), promptTokens, completionTokens); err != nil {
		return fmt.Errorf("settle agent instance quota: %w", err)
	}
	return nil
}

func (r *AgentInstanceReconciler) finishTerminalAccounting(ctx context.Context, instance *v1alpha1.AgentInstance) error {
	if hasUsageAccountingUncertainCondition(instance.Status.Conditions) {
		return nil
	}
	if err := r.settleQuota(ctx, instance); err != nil {
		return err
	}
	if r.trust != nil {
		if err := r.trust.RecordAgentInstanceOutcome(ctx, instance); err != nil {
			return fmt.Errorf("record agent instance trust outcome: %w", err)
		}
	}
	return nil
}

func hasUsageAccountingUncertainCondition(conditions []v1alpha1.Condition) bool {
	for index := range conditions {
		if conditions[index].Reason == "UsageAccountingUncertain" {
			return true
		}
	}
	return false
}

func agentInstanceReservationID(instance *v1alpha1.AgentInstance) string {
	if instance.ObjectMeta.UID != "" {
		return "agentinstance:" + instance.ObjectMeta.UID
	}
	return "agentinstance:" + instance.ObjectMeta.Namespace + "/" + instance.ObjectMeta.Name
}

func isWarmPoolInstance(instance *v1alpha1.AgentInstance) bool {
	if instance.Spec.SessionContext["input"] != "" {
		return false
	}
	for _, ref := range instance.ObjectMeta.OwnerReferences {
		if ref.Kind == "Agent" && ref.Name == instance.Spec.AgentRef.Name {
			return true
		}
	}
	return false
}

// handleDeletion removes the finalizer to allow deletion to proceed.
func (r *AgentInstanceReconciler) handleDeletion(ctx context.Context, instance *v1alpha1.AgentInstance, originalStatus v1alpha1.AgentInstanceStatus, originalFinalizers []string, log *zap.Logger) (Result, error) {
	log.Info("handling agent instance deletion")

	finalizers, removed := lifecycle.RemoveFinalizer(instance.ObjectMeta.Finalizers, AgentInstanceFinalizer)
	if removed {
		instance.ObjectMeta.Finalizers = finalizers
		if err := r.updateInstance(ctx, instance, originalStatus, originalFinalizers); err != nil {
			return Result{}, fmt.Errorf("remove finalizer: %w", err)
		}
	}

	return Result{}, nil
}

// updateInstance persists the current AgentInstance state back to the store.
// On resource-version conflict it re-reads the latest object and reapplies only
// lifecycle fields changed by this reconciliation.
func (r *AgentInstanceReconciler) updateInstance(ctx context.Context, instance *v1alpha1.AgentInstance, originalStatus v1alpha1.AgentInstanceStatus, originalFinalizers []string) error {
	if reflect.DeepEqual(originalStatus, instance.Status) && reflect.DeepEqual(originalFinalizers, instance.ObjectMeta.Finalizers) {
		return nil
	}

	const maxRetries = 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		data, err := json.Marshal(instance)
		if err != nil {
			return fmt.Errorf("marshal instance: %w", err)
		}
		err = r.store.Update(ctx, agentInstanceKind, instance.ObjectMeta.Namespace, instance.ObjectMeta.Name, data, instance.ObjectMeta.ResourceVersion)
		if err == nil {
			return nil
		}
		if !errors.Is(err, store.ErrConflict) {
			return err
		}
		raw, getErr := r.store.Get(ctx, agentInstanceKind, instance.ObjectMeta.Namespace, instance.ObjectMeta.Name)
		if getErr != nil {
			if errors.Is(getErr, store.ErrNotFound) {
				return nil
			}
			return fmt.Errorf("re-read after conflict: %w", getErr)
		}
		var latest v1alpha1.AgentInstance
		if getErr := json.Unmarshal(raw, &latest); getErr != nil {
			return fmt.Errorf("unmarshal after conflict: %w", getErr)
		}
		mergeAgentInstanceStatus(&latest.Status, originalStatus, instance.Status)
		originalHasFinalizer := hasString(originalFinalizers, AgentInstanceFinalizer)
		desiredHasFinalizer := hasString(instance.ObjectMeta.Finalizers, AgentInstanceFinalizer)
		if originalHasFinalizer != desiredHasFinalizer {
			latest.ObjectMeta.Finalizers = mergeOwnedFinalizer(
				latest.ObjectMeta.Finalizers,
				instance.ObjectMeta.Finalizers,
				AgentInstanceFinalizer,
				!desiredHasFinalizer,
			)
		}
		*instance = latest
	}
	return store.ErrConflict
}

func snapshotAgentInstanceStatus(status v1alpha1.AgentInstanceStatus) v1alpha1.AgentInstanceStatus {
	cloned := status
	cloned.Conditions = append([]v1alpha1.Condition(nil), status.Conditions...)
	if status.StartTime != nil {
		startTime := *status.StartTime
		cloned.StartTime = &startTime
	}
	if status.TokenUsage != nil {
		tokenUsage := *status.TokenUsage
		cloned.TokenUsage = &tokenUsage
	}
	return cloned
}

func mergeAgentInstanceStatus(latest *v1alpha1.AgentInstanceStatus, original, desired v1alpha1.AgentInstanceStatus) {
	sameSession := latest.SessionID == original.SessionID
	if sameSession && desired.Phase != original.Phase {
		latest.Phase = desired.Phase
	}
	if desired.DispatchAttempts != original.DispatchAttempts {
		latest.DispatchAttempts = desired.DispatchAttempts
	}
	if sameSession && desired.SessionID != original.SessionID {
		latest.SessionID = desired.SessionID
	}
	if sameSession {
		if !agentJobTimesMatch(original.StartTime, desired.StartTime) {
			latest.StartTime = desired.StartTime
		}
		if !reflect.DeepEqual(desired.TokenUsage, original.TokenUsage) {
			latest.TokenUsage = desired.TokenUsage
		}
		if desired.ToolCalls != original.ToolCalls {
			latest.ToolCalls = desired.ToolCalls
		}
		if !reflect.DeepEqual(desired.ToolCallsDetail, original.ToolCallsDetail) {
			latest.ToolCallsDetail = desired.ToolCallsDetail
		}
		if desired.Output != original.Output {
			latest.Output = desired.Output
		}
		for _, conditionType := range []string{string(lifecycle.ConditionReady), string(lifecycle.ConditionBudgetExceeded)} {
			originalCondition := restartLadderCondition(original.Conditions, conditionType)
			desiredCondition := restartLadderCondition(desired.Conditions, conditionType)
			if !conditionsMatch(originalCondition, desiredCondition) {
				latest.Conditions = applyConditionChange(latest.Conditions, conditionType, desiredCondition)
			}
		}
	}
}

func (r *AgentInstanceReconciler) appendRuntimeQuotaWarningOnce(ctx context.Context, instance *v1alpha1.AgentInstance, usedTokens int64) error {
	message := fmt.Sprintf("session %s reached its token budget at %d tokens", instance.Status.SessionID, usedTokens)
	events, err := r.store.ListEvents(ctx, agentInstanceKind, instance.ObjectMeta.Namespace, instance.ObjectMeta.Name)
	if err != nil {
		return err
	}
	for _, event := range events {
		if event.Type == "Warning" && event.Reason == "RuntimeTokenBudgetExceeded" && event.Message == message {
			return nil
		}
	}
	return r.store.AppendEvent(ctx, agentInstanceKind, instance.ObjectMeta.Namespace, instance.ObjectMeta.Name, store.InstanceEvent{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Type:      "Warning",
		Reason:    "RuntimeTokenBudgetExceeded",
		Message:   message,
	})
}

func (r *AgentInstanceReconciler) dispatchBackoff(attempt int32) time.Duration {
	backoff := r.config.DispatchBackoffBase
	for i := int32(1); i < attempt; i++ {
		backoff *= 2
		if backoff >= r.config.DispatchBackoffMax {
			return r.config.DispatchBackoffMax
		}
	}
	if backoff > r.config.DispatchBackoffMax {
		return r.config.DispatchBackoffMax
	}
	return backoff
}
