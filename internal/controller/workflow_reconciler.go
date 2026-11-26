// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

const (
	// WorkflowFinalizer is the finalizer applied to Workflow resources.
	WorkflowFinalizer = "kognition.dev/workflow-cleanup"

	// WorkflowRunFinalizer is the finalizer applied to WorkflowRun resources.
	WorkflowRunFinalizer = "kognition.dev/workflow-run-cleanup"

	workflowStepLabel           = "kognition.dev/workflow-step"
	workflowIterationLabel      = "kognition.dev/workflow-iteration"
	workflowApprovedStepsKey    = "kognition.dev/approved-steps"
	workflowRejectedStepsKey    = "kognition.dev/rejected-steps"
	workflowGateEnteredPrefix   = "kognition.dev/gate-entered."
	workflowCurrentIterationKey = "kognition.dev/current-iteration"
	workflowIgnoredFailuresKey  = "kognition.dev/ignored-failures"
	workflowCompletedToolsKey   = "kognition.dev/completed-tool-steps"

	workflowEmergentProposalKey       = "kognition.dev/emergent-proposal"
	workflowEmergentStepsConsumedKey  = "kognition.dev/emergent-steps-consumed"
	workflowEmergentTokensConsumedKey = "kognition.dev/emergent-tokens-consumed"
	workflowEmergentCostConsumedKey   = "kognition.dev/emergent-cost-consumed"
)

type workflowEmergentProposal struct {
	Steps           []v1alpha1.WorkflowStep `json:"steps,omitempty"`
	Edges           []v1alpha1.WorkflowEdge `json:"edges,omitempty"`
	ProposedBy      string                  `json:"proposedBy,omitempty"`
	EstimatedTokens int64                   `json:"estimatedTokens,omitempty"`
	EstimatedCost   float64                 `json:"estimatedCost,omitempty"`
}

// WorkflowReconciler serves two roles:
//   - Template reconciler for Workflow CRs (see Reconcile): manages template
//     lifecycle only and never dispatches jobs.
//   - Shared execution engine (see executeWorkflow): invoked by
//     WorkflowRunReconciler to plan and dispatch AgentJobs against a
//     synthetic Workflow derived from a WorkflowRun.
type WorkflowReconciler struct {
	store               store.Store
	logger              *zap.Logger
	communicationPolicy CommunicationPolicyAuthorizer
	audit               *observability.CognitiveAuditLog

	// ownerKind is the Kind used for AgentJob owner references. Always
	// "WorkflowRun" in production (set by WorkflowRunReconciler); the
	// default "Workflow" applies only to the test-only direct-dispatch
	// shim (see workflow_reconciler_direct_dispatch_test.go).
	ownerKind string

	// workflowInput is the workflow-level input string exposed to step
	// templates via {{ .Input }}. Set to WorkflowRun.Spec.Input by the
	// production path; empty under the test-only shim.
	workflowInput string

	// persistStatus persists the workflow's current state to the store.
	// The production path maps status back to the WorkflowRun and
	// persists that; the test-only shim marshals the Workflow directly.
	persistStatus func(ctx context.Context, wf *v1alpha1.Workflow) error

	// toolInvoker synchronously executes tool-typed workflow steps.
	// May be nil if the controller does not enable workflow tool steps.
	toolInvoker WorkflowToolInvoker

	tracer    *observability.Tracer
	collector *observability.Collector
}

// CommunicationPolicyAuthorizer evaluates whether one Agent may dispatch work
// to another Agent. Workflow dependency edges are agent-to-agent messages when
// both source and target steps are agent-backed.
type CommunicationPolicyAuthorizer interface {
	EvaluateDispatch(context.Context, string, string, string) (bool, error)
}

// SetToolInvoker installs the WorkflowToolInvoker used to execute tool-typed steps.
func (r *WorkflowReconciler) SetToolInvoker(invoker WorkflowToolInvoker) {
	r.toolInvoker = invoker
}

// SetCommunicationPolicyAuthorizer enforces AgentCommunicationPolicy on
// dependent agent workflow steps.
func (r *WorkflowReconciler) SetCommunicationPolicyAuthorizer(authorizer CommunicationPolicyAuthorizer) {
	r.communicationPolicy = authorizer
}

// SetAuditLog installs the durable recorder for workflow communication decisions.
func (r *WorkflowReconciler) SetAuditLog(audit *observability.CognitiveAuditLog) {
	r.audit = audit
}

// SetTracing installs the semantic tracer used for workflow execution spans.
func (r *WorkflowReconciler) SetTracing(tracer *observability.Tracer, collector *observability.Collector) {
	r.tracer = tracer
	r.collector = collector
}

func (r *WorkflowReconciler) startWorkflowStepSpan(ctx context.Context, wf *v1alpha1.Workflow, step v1alpha1.WorkflowStep, iteration int) (*observability.Span, context.Context) {
	if r.tracer == nil || r.collector == nil {
		return nil, ctx
	}
	if _, ok := observability.SpanContextFromContext(ctx); !ok {
		if spanContext, ok := observability.SpanContextFromAnnotations(wf.ObjectMeta.Annotations); ok {
			ctx = observability.ContextWithSpanContext(ctx, spanContext)
		}
	}
	attrs := map[string]interface{}{
		observability.AttrWorkspace:    wf.ObjectMeta.Namespace,
		observability.AttrWorkflowName: wf.ObjectMeta.Name,
		observability.AttrWorkflowRun:  wf.ObjectMeta.Name,
		observability.AttrStepName:     step.Name,
		observability.AttrStepType:     string(step.Type),
	}
	if iteration > 0 {
		attrs[observability.AttrStepIteration] = iteration
	}
	span := r.tracer.StartSpan(ctx, observability.SpanNameWorkflowStep, observability.SpanKindInternal, attrs)
	return span, observability.ContextWithSpanContext(ctx, span.Context)
}

func (r *WorkflowReconciler) endWorkflowSpan(ctx context.Context, span *observability.Span, status observability.StatusCode, statusMsg string) {
	if r.tracer == nil || r.collector == nil || span == nil {
		return
	}
	if err := r.tracer.EndSpan(span, status, statusMsg); err != nil {
		r.logger.Warn("end workflow span failed", zap.String("span", span.Name), zap.Error(err))
	}
	if span.Context.TraceFlags&0x01 == 0 {
		return
	}
	if err := r.collector.FlushSpans(ctx, []*observability.Span{span}); err != nil {
		r.logger.Warn("export workflow span failed", zap.String("span", span.Name), zap.Error(err))
	}
}

// invokeToolStep runs the tool invoker, applying the step's optional per-step
// timeout as a context deadline so a slow tool cannot block the step forever.
func (r *WorkflowReconciler) invokeToolStep(ctx context.Context, namespace, principal, toolName, timeout string, input map[string]any) (map[string]any, error) {
	if timeout != "" {
		if d, err := time.ParseDuration(timeout); err == nil && d > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, d)
			defer cancel()
		}
	}
	return r.toolInvoker.InvokeTool(ctx, namespace, principal, toolName, input)
}

// NewWorkflowReconciler creates a new WorkflowReconciler.
func NewWorkflowReconciler(s store.Store, logger *zap.Logger) *WorkflowReconciler {
	r := &WorkflowReconciler{
		store:     s,
		logger:    logger.With(zap.String("reconciler", "workflow")),
		ownerKind: "Workflow",
	}
	r.persistStatus = func(ctx context.Context, wf *v1alpha1.Workflow) error {
		return fmt.Errorf("workflow execution persistence is not configured")
	}
	return r
}

// Reconcile implements the Reconciler interface for Workflow resources.
//
// A Workflow is always a template — applying its manifest never triggers
// execution. All execution flows through WorkflowRun → WorkflowRunReconciler,
// which drives the shared engine via executeWorkflow. Reconcile here only
// manages template lifecycle: it clears any stale execution status left over
// from a legacy direct-mode run and drops the direct-mode finalizer so
// kubectl/dashboard don't display misleading Failed/Ready phases.
func (r *WorkflowReconciler) Reconcile(ctx context.Context, req Request) (Result, error) {
	log := r.logger.With(zap.String("namespace", req.Namespace), zap.String("name", req.Name), requestid.ZapField(ctx))

	raw, err := r.store.Get(ctx, "Workflow", req.Namespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			log.Debug("workflow not found, likely deleted")
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("get workflow: %w", err)
	}

	var wf v1alpha1.Workflow
	if err := json.Unmarshal(raw, &wf); err != nil {
		return Result{}, fmt.Errorf("unmarshal workflow: %w", err)
	}

	if wf.ObjectMeta.DeletionTimestamp != nil {
		return r.handleDeletion(ctx, &wf)
	}
	if _, err := r.ensureTemplateRevision(ctx, &wf); err != nil {
		return Result{}, fmt.Errorf("ensure workflow revision: %w", err)
	}

	originalStatus := wf.Status
	originalAnnotations := cloneStringMap(wf.ObjectMeta.Annotations)
	originalFinalizers := append([]string(nil), wf.ObjectMeta.Finalizers...)
	changed := false
	// The template status carries only the computed entry points; legacy
	// execution state is cleared.
	entryPoints := workflowEntryPoints(&wf.Spec)
	if !isEmptyWorkflowStatus(wf.Status) || !slices.Equal(wf.Status.EntryPoints, entryPoints) {
		wf.Status = v1alpha1.WorkflowStatus{EntryPoints: entryPoints}
		changed = true
	}
	if finalizers, removed := lifecycle.RemoveFinalizer(wf.ObjectMeta.Finalizers, WorkflowFinalizer); removed {
		wf.ObjectMeta.Finalizers = finalizers
		changed = true
	}
	if changed {
		if err := r.persistWorkflowState(ctx, &wf, originalStatus, originalAnnotations, originalFinalizers); err != nil {
			return Result{}, fmt.Errorf("clear template workflow state: %w", err)
		}
	}
	log.Debug("workflow reconciled as template; execution occurs via WorkflowRun")
	return Result{}, nil
}

func (r *WorkflowReconciler) ensureTemplateRevision(ctx context.Context, workflow *v1alpha1.Workflow) (int64, error) {
	spec, err := json.Marshal(workflow.Spec)
	if err != nil {
		return 0, fmt.Errorf("marshal workflow spec: %w", err)
	}
	revisions, err := r.store.ListRevisions(ctx, "Workflow", workflow.ObjectMeta.Namespace, workflow.ObjectMeta.Name)
	if err != nil {
		return 0, fmt.Errorf("list workflow revisions: %w", err)
	}
	maxRevision := int64(0)
	var latest *store.Revision
	for _, revision := range revisions {
		if revision.Revision > maxRevision {
			maxRevision = revision.Revision
			latest = &revision
		}
	}
	if latest != nil && jsonBytesEqual(latest.Spec, spec) {
		return latest.Revision, nil
	}
	snapshot, err := json.Marshal(workflow)
	if err != nil {
		return 0, fmt.Errorf("marshal workflow snapshot: %w", err)
	}
	nextRevision := maxRevision + 1
	if err := r.store.CreateRevision(ctx, "Workflow", workflow.ObjectMeta.Namespace, workflow.ObjectMeta.Name, nextRevision, spec, snapshot); err != nil {
		return 0, fmt.Errorf("create workflow revision %d: %w", nextRevision, err)
	}
	return nextRevision, nil
}

// isEmptyWorkflowStatus reports whether a WorkflowStatus carries no
// execution state. Entry points are template metadata and are ignored.
func isEmptyWorkflowStatus(s v1alpha1.WorkflowStatus) bool {
	return s.Phase == "" &&
		len(s.Conditions) == 0 &&
		len(s.CompletedSteps) == 0 &&
		s.ActiveStep == "" &&
		len(s.ActiveSteps) == 0 &&
		len(s.StepStatuses) == 0
}

// workflowEntryPoints returns spec.entryPoint when set, otherwise every step
// that has no incoming edge, in declaration order.
func workflowEntryPoints(spec *v1alpha1.WorkflowSpec) []string {
	if spec.EntryPoint != "" {
		return []string{spec.EntryPoint}
	}
	targets := make(map[string]struct{}, len(spec.Edges))
	for _, edge := range spec.Edges {
		targets[edge.To] = struct{}{}
	}
	var entries []string
	for _, step := range spec.Steps {
		if _, incoming := targets[step.Name]; !incoming {
			entries = append(entries, step.Name)
		}
	}
	return entries
}

func snapshotWorkflowStatus(status v1alpha1.WorkflowStatus) v1alpha1.WorkflowStatus {
	status.Conditions = append([]v1alpha1.Condition(nil), status.Conditions...)
	status.CompletedSteps = append([]string(nil), status.CompletedSteps...)
	status.ActiveSteps = append([]string(nil), status.ActiveSteps...)
	status.StepStatuses = append([]v1alpha1.WorkflowStepStatus(nil), status.StepStatuses...)
	return status
}

// executeWorkflow runs the core execution logic: plan validation, job listing, and dispatch.
// It is shared by both WorkflowReconciler and WorkflowRunReconciler.
func (r *WorkflowReconciler) executeWorkflow(ctx context.Context, wf *v1alpha1.Workflow) (Result, error) {
	r.ensureTraceAnnotations(wf)

	mutated, err := r.applyEmergentMutation(ctx, wf)
	if err != nil {
		return Result{}, fmt.Errorf("apply emergent mutation: %w", err)
	}
	if mutated {
		return Result{Requeue: true}, nil
	}

	plan, err := buildWorkflowPlan(wf)
	if err != nil {
		wf.Status.Phase = v1alpha1.PhaseFailed
		lc := lifecycle.SetCondition(
			apiToLifecycleConditions(wf.Status.Conditions),
			lifecycle.ConditionReady,
			lifecycle.ConditionFalse,
			"InvalidDAG",
			err.Error(),
		)
		wf.Status.Conditions = lifecycleToAPIConditions(lc)
		if updateErr := r.updateWorkflow(ctx, wf); updateErr != nil {
			return Result{}, fmt.Errorf("update invalid workflow: %w", updateErr)
		}
		return Result{}, nil
	}

	jobs, err := r.listOwnedJobs(ctx, wf)
	if err != nil {
		return Result{}, fmt.Errorf("list workflow jobs: %w", err)
	}
	jobsByStep := map[string]v1alpha1.AgentJob{}
	jobsByStepIteration := map[string]map[int]v1alpha1.AgentJob{}
	for _, j := range jobs {
		if stepName, ok := j.ObjectMeta.Labels[workflowStepLabel]; ok {
			jobsByStep[stepName] = j
			iter := parseIterationLabel(j.ObjectMeta.Labels)
			if _, exists := jobsByStepIteration[stepName]; !exists {
				jobsByStepIteration[stepName] = map[int]v1alpha1.AgentJob{}
			}
			jobsByStepIteration[stepName][iter] = j
		}
	}

	if plan.Cyclic {
		return r.reconcileCyclic(ctx, wf, plan, jobsByStepIteration)
	}

	return r.reconcileAcyclic(ctx, wf, plan, jobsByStep)
}

func (r *WorkflowReconciler) handleDeletion(ctx context.Context, wf *v1alpha1.Workflow) (Result, error) {
	jobs, err := r.listOwnedJobs(ctx, wf)
	if err != nil {
		return Result{}, fmt.Errorf("list jobs for deletion: %w", err)
	}
	for _, job := range jobs {
		if err := r.store.Delete(ctx, "AgentJob", job.ObjectMeta.Namespace, job.ObjectMeta.Name); err != nil && !errors.Is(err, store.ErrNotFound) {
			return Result{}, fmt.Errorf("delete owned job %s: %w", job.ObjectMeta.Name, err)
		}
	}

	originalStatus := wf.Status
	originalFinalizers := append([]string(nil), wf.ObjectMeta.Finalizers...)
	finalizers, removed := lifecycle.RemoveFinalizer(wf.ObjectMeta.Finalizers, WorkflowFinalizer)
	if removed {
		wf.ObjectMeta.Finalizers = finalizers
		if err := r.persistWorkflowState(ctx, wf, originalStatus, cloneStringMap(wf.ObjectMeta.Annotations), originalFinalizers); err != nil {
			return Result{}, fmt.Errorf("remove finalizer: %w", err)
		}
	}
	return Result{}, nil
}

func (r *WorkflowReconciler) updateWorkflow(ctx context.Context, wf *v1alpha1.Workflow) error {
	return r.persistStatus(ctx, wf)
}

func (r *WorkflowReconciler) persistWorkflowState(ctx context.Context, wf *v1alpha1.Workflow, originalStatus v1alpha1.WorkflowStatus, originalAnnotations map[string]string, originalFinalizers []string) error {
	const maxRetries = 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		data, err := json.Marshal(wf)
		if err != nil {
			return fmt.Errorf("marshal workflow: %w", err)
		}
		err = r.store.Update(ctx, "Workflow", wf.ObjectMeta.Namespace, wf.ObjectMeta.Name, data, wf.ObjectMeta.ResourceVersion)
		if err == nil {
			return nil
		}
		if !errors.Is(err, store.ErrConflict) {
			return err
		}
		raw, getErr := r.store.Get(ctx, "Workflow", wf.ObjectMeta.Namespace, wf.ObjectMeta.Name)
		if getErr != nil {
			if errors.Is(getErr, store.ErrNotFound) {
				return nil
			}
			return fmt.Errorf("re-read workflow after conflict: %w", getErr)
		}
		var latest v1alpha1.Workflow
		if getErr := json.Unmarshal(raw, &latest); getErr != nil {
			return fmt.Errorf("unmarshal workflow after conflict: %w", getErr)
		}
		mergeWorkflowStatus(&latest.Status, originalStatus, wf.Status)
		latest.ObjectMeta.Annotations = mergeWorkflowRunAnnotations(latest.ObjectMeta.Annotations, originalAnnotations, wf.ObjectMeta.Annotations)
		originalHasFinalizer := hasString(originalFinalizers, WorkflowFinalizer)
		desiredHasFinalizer := hasString(wf.ObjectMeta.Finalizers, WorkflowFinalizer)
		if originalHasFinalizer != desiredHasFinalizer {
			latest.ObjectMeta.Finalizers = mergeOwnedFinalizer(latest.ObjectMeta.Finalizers, wf.ObjectMeta.Finalizers, WorkflowFinalizer, !desiredHasFinalizer)
		}
		*wf = latest
	}
	return store.ErrConflict
}

func mergeWorkflowStatus(latest *v1alpha1.WorkflowStatus, original, desired v1alpha1.WorkflowStatus) {
	if !reflect.DeepEqual(desired.EntryPoints, original.EntryPoints) {
		latest.EntryPoints = desired.EntryPoints
	}
	if desired.Phase != original.Phase {
		latest.Phase = desired.Phase
	}
	if !reflect.DeepEqual(desired.CompletedSteps, original.CompletedSteps) {
		latest.CompletedSteps = desired.CompletedSteps
	}
	if desired.ActiveStep != original.ActiveStep {
		latest.ActiveStep = desired.ActiveStep
	}
	if !reflect.DeepEqual(desired.ActiveSteps, original.ActiveSteps) {
		latest.ActiveSteps = desired.ActiveSteps
	}
	if !reflect.DeepEqual(desired.StepStatuses, original.StepStatuses) {
		latest.StepStatuses = desired.StepStatuses
	}
	conditionTypes := make(map[string]struct{}, len(original.Conditions))
	for _, condition := range original.Conditions {
		conditionTypes[condition.Type] = struct{}{}
	}
	for _, condition := range desired.Conditions {
		conditionTypes[condition.Type] = struct{}{}
	}
	for conditionType := range conditionTypes {
		originalCondition := restartLadderCondition(original.Conditions, conditionType)
		desiredCondition := restartLadderCondition(desired.Conditions, conditionType)
		if !conditionsMatch(originalCondition, desiredCondition) {
			latest.Conditions = applyConditionChange(latest.Conditions, conditionType, desiredCondition)
		}
	}
}

func (r *WorkflowReconciler) listOwnedJobs(ctx context.Context, wf *v1alpha1.Workflow) ([]v1alpha1.AgentJob, error) {
	rawList, err := r.store.List(ctx, "AgentJob", store.ListOptions{Namespace: wf.ObjectMeta.Namespace})
	if err != nil {
		return nil, err
	}

	var owned []v1alpha1.AgentJob
	for _, raw := range rawList {
		var job v1alpha1.AgentJob
		if err := json.Unmarshal(raw, &job); err != nil {
			continue
		}
		for _, ref := range job.ObjectMeta.OwnerReferences {
			if ref.Kind == r.ownerKind && ref.Name == wf.ObjectMeta.Name && ref.UID == wf.ObjectMeta.UID {
				owned = append(owned, job)
				break
			}
		}
	}
	return owned, nil
}

func (r *WorkflowReconciler) createStepJob(ctx context.Context, wf *v1alpha1.Workflow, step v1alpha1.WorkflowStep, sourceAgents []string) (bool, error) {
	return r.createStepJobForIteration(ctx, wf, step, 0, sourceAgents)
}

func (r *WorkflowReconciler) authorizeInboundAgentDispatch(ctx context.Context, wf *v1alpha1.Workflow, plan workflowPlan, target v1alpha1.WorkflowStep, iteration int) ([]string, bool, error) {
	if r.communicationPolicy == nil || target.Type == v1alpha1.WorkflowStepTypeTool || target.AgentRef.Name == "" {
		return nil, true, nil
	}

	steps := make(map[string]v1alpha1.WorkflowStep, len(plan.OrderedSteps))
	for _, step := range plan.OrderedSteps {
		steps[step.Name] = step
	}
	authorizedSources := make([]string, 0, len(plan.Deps[target.Name]))
	for _, sourceName := range plan.Deps[target.Name] {
		source, found := steps[sourceName]
		if !found || source.Type == v1alpha1.WorkflowStepTypeTool || source.AgentRef.Name == "" {
			continue
		}
		allowed, err := r.communicationPolicy.EvaluateDispatch(ctx, wf.ObjectMeta.Namespace, source.AgentRef.Name, target.AgentRef.Name)
		if err != nil {
			return append(authorizedSources, source.AgentRef.Name), false, fmt.Errorf("evaluate communication policy: %w", err)
		}
		if !allowed {
			if err := r.recordWorkflowCommunicationDecision(ctx, wf, source.AgentRef.Name, target.AgentRef.Name, target.Name, iteration, "denied", "communication_policy_denied", "not_started", false, true); err != nil {
				return append(authorizedSources, source.AgentRef.Name), false, fmt.Errorf("record denied communication decision: %w", err)
			}
			return append(authorizedSources, source.AgentRef.Name), false, nil
		}
		authorizedSources = append(authorizedSources, source.AgentRef.Name)
	}
	return authorizedSources, true, nil
}

func (r *WorkflowReconciler) createStepJobForIteration(ctx context.Context, wf *v1alpha1.Workflow, step v1alpha1.WorkflowStep, iteration int, sourceAgents []string) (bool, error) {
	// The AgentJob metadata.name must be DNS1123-safe. step.Name is preserved
	// as the label / status key (labels tolerate uppercase and '_'), but the
	// composed object name goes through sanitizeK8sName to strip anything
	// Kubernetes would reject. Collisions across sanitized names are guarded
	// at admission time.
	stepSegment := sanitizeK8sName(step.Name)
	name := fmt.Sprintf("%s-%s", wf.ObjectMeta.Name, stepSegment)
	if iteration > 0 {
		name = fmt.Sprintf("%s-%s-i%d", wf.ObjectMeta.Name, stepSegment, iteration)
	}

	labels := map[string]string{
		workflowStepLabel: step.Name,
	}
	if iteration > 0 {
		labels[workflowIterationLabel] = strconv.Itoa(iteration)
	}

	annotations := r.buildStepJobAnnotations(ctx, wf, step, iteration)

	job := v1alpha1.AgentJob{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "AgentJob"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:              name,
			Namespace:         wf.ObjectMeta.Namespace,
			CreationTimestamp: time.Now().UTC(),
			Labels:            labels,
			Annotations:       annotations,
			OwnerReferences: []v1alpha1.OwnerReference{{
				APIVersion: v1alpha1.APIVersion,
				Kind:       r.ownerKind,
				Name:       wf.ObjectMeta.Name,
				UID:        wf.ObjectMeta.UID,
			}},
		},
		Spec: v1alpha1.AgentJobSpec{
			AgentRef: &v1alpha1.LocalObjectReference{Name: step.AgentRef.Name},
			Input:    step.Input,
		},
		Status: v1alpha1.AgentJobStatus{Phase: v1alpha1.PhasePending},
	}

	if claimName := r.resolveStepMemoryClaimName(wf, step); claimName != "" {
		job.Spec.MemoryClaimRef = &v1alpha1.LocalObjectReference{Name: claimName}
	}

	data, err := json.Marshal(job)
	if err != nil {
		return false, fmt.Errorf("marshal job: %w", err)
	}
	for _, sourceAgent := range sourceAgents {
		if err := r.recordWorkflowCommunicationDecision(ctx, wf, sourceAgent, step.AgentRef.Name, step.Name, iteration, "allowed", "communication_policy_allowed", "not_started", false, true); err != nil {
			return false, fmt.Errorf("record allowed communication decision: %w", err)
		}
	}
	if err := r.store.Create(ctx, "AgentJob", wf.ObjectMeta.Namespace, name, data); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			return false, nil
		}
		for _, sourceAgent := range sourceAgents {
			if auditErr := r.recordWorkflowCommunicationDecision(ctx, wf, sourceAgent, step.AgentRef.Name, step.Name, iteration, "failed", "agent_job_create_failed", "failed", true, false); auditErr != nil {
				return false, fmt.Errorf("create AgentJob: %w; record failed communication decision: %w", err, auditErr)
			}
		}
		return false, err
	}
	for _, sourceAgent := range sourceAgents {
		if err := r.recordWorkflowCommunicationDecision(ctx, wf, sourceAgent, step.AgentRef.Name, step.Name, iteration, "completed", "agent_job_created", "completed", true, false); err != nil {
			return false, fmt.Errorf("record completed communication decision: %w", err)
		}
	}
	return true, nil
}

func (r *WorkflowReconciler) recordWorkflowCommunicationDecision(ctx context.Context, wf *v1alpha1.Workflow, sourceAgent, targetAgent, stepName string, iteration int, outcome, reasonCode, executionStatus string, targetReached, beforeEffect bool) error {
	if r.audit == nil {
		return nil
	}
	now := time.Now().UTC()
	jobName := fmt.Sprintf("%s-%s", wf.ObjectMeta.Name, sanitizeK8sName(stepName))
	if iteration > 0 {
		jobName = fmt.Sprintf("%s-i%d", jobName, iteration)
	}
	authorization := observability.TrustDecisionAuthorization{Identity: "not_evaluated", Role: "not_required", Grant: "not_required", Policy: "allowed"}
	if outcome == "denied" {
		authorization.Policy = "denied"
	}
	decision := observability.TrustDecisionV1{
		SchemaVersion: 1, ID: requestid.Generate(), Timestamp: now, Plane: "communication", Outcome: outcome, ReasonCode: reasonCode, Workspace: wf.ObjectMeta.Namespace,
		ClaimedIdentityRef: &observability.TrustDecisionResourceRef{Kind: "Agent", Namespace: wf.ObjectMeta.Namespace, Name: sourceAgent},
		Session:            &observability.TrustDecisionSession{SessionID: wf.ObjectMeta.Name},
		Request:            observability.TrustDecisionRequest{Operation: "dispatch", InvocationID: jobName, TargetRef: observability.TrustDecisionResourceRef{Kind: "Agent", Namespace: wf.ObjectMeta.Namespace, Name: targetAgent}},
		Authorization:      authorization,
		Execution:          observability.TrustDecisionExecution{Status: executionStatus, TargetReached: workflowBoolPointer(targetReached)},
	}
	details, err := json.Marshal(decision)
	if err != nil {
		return fmt.Errorf("encode workflow communication trust decision: %w", err)
	}
	entry := observability.AuditEntry{ID: decision.ID, Timestamp: now, Workspace: wf.ObjectMeta.Namespace, SessionID: wf.ObjectMeta.Name, EventType: observability.AuditCommunication, SubjectKind: "Agent", SubjectRef: wf.ObjectMeta.Namespace + "/" + targetAgent, Verb: "dispatch", Details: details}
	if beforeEffect {
		return r.audit.RecordBeforeExec(ctx, entry)
	}
	return r.audit.Record(ctx, entry)
}

func workflowBoolPointer(value bool) *bool { return &value }

// ensureTraceAnnotations seeds a trace-id / root-span-id pair onto the
// workflow's annotations if one is not already present and the workflow is
// owned by a WorkflowRun. The pair is persisted back to the WorkflowRun by
// the inner reconciler's persistStatus closure.
func (r *WorkflowReconciler) ensureTraceAnnotations(wf *v1alpha1.Workflow) {
	if r.ownerKind != "WorkflowRun" {
		return
	}
	if wf.ObjectMeta.Annotations == nil {
		wf.ObjectMeta.Annotations = map[string]string{}
	}
	if wf.ObjectMeta.Annotations[observability.AnnotationTraceID] == "" {
		wf.ObjectMeta.Annotations[observability.AnnotationTraceID] = observability.NewHexTraceID()
	}
	if wf.ObjectMeta.Annotations[observability.AnnotationRootSpanID] == "" {
		wf.ObjectMeta.Annotations[observability.AnnotationRootSpanID] = observability.NewHexSpanID()
	}
}

// buildStepJobAnnotations returns trace-context annotations for an AgentJob
// dispatched on behalf of a workflow step. Returns nil when the parent
// workflow lacks a trace-id annotation (e.g. standalone Workflow without
// observability bootstrap).
func (r *WorkflowReconciler) buildStepJobAnnotations(ctx context.Context, wf *v1alpha1.Workflow, step v1alpha1.WorkflowStep, iteration int) map[string]string {
	if wf.ObjectMeta.Annotations == nil {
		return nil
	}
	traceID := wf.ObjectMeta.Annotations[observability.AnnotationTraceID]
	rootSpanID := wf.ObjectMeta.Annotations[observability.AnnotationRootSpanID]
	if !observability.IsValidTraceID(traceID) || !observability.IsValidSpanID(rootSpanID) {
		return nil
	}
	spanID := observability.NewHexSpanID()
	parentSpanID := rootSpanID
	if spanContext, ok := observability.SpanContextFromContext(ctx); ok {
		traceID = observability.HexTraceID(spanContext.TraceID)
		spanID = observability.HexSpanID(spanContext.SpanID)
		parentSpanID = observability.HexSpanID(spanContext.ParentID)
		if !observability.IsValidTraceID(traceID) || !observability.IsValidSpanID(spanID) {
			return nil
		}
	}
	ann := map[string]string{
		observability.AnnotationTraceID:       traceID,
		observability.AnnotationSpanID:        spanID,
		observability.AnnotationTraceparent:   observability.Traceparent(traceID, spanID),
		observability.AnnotationWorkflowName:  wf.ObjectMeta.Name,
		observability.AnnotationStepName:      step.Name,
		observability.AnnotationStepType:      string(step.Type),
		observability.AnnotationStepIteration: strconv.Itoa(iteration),
	}
	if observability.IsValidSpanID(parentSpanID) {
		ann[observability.AnnotationParentSpanID] = parentSpanID
	}
	if r.ownerKind == "WorkflowRun" {
		ann[observability.AnnotationWorkflowRun] = wf.ObjectMeta.Name
		if workflowName := wf.ObjectMeta.Annotations[observability.AnnotationWorkflowName]; workflowName != "" {
			ann[observability.AnnotationWorkflowName] = workflowName
		}
	}
	return ann
}

// resolveStepMemoryClaimName returns the name of the MemoryClaim that should
// back the AgentJob for the given step, or "" if none applies. Per-step claims
// take precedence over the workflow-level claim. Injection is gated on
// WorkflowRun ownership because materialization happens in WorkflowRunReconciler.
func (r *WorkflowReconciler) resolveStepMemoryClaimName(wf *v1alpha1.Workflow, step v1alpha1.WorkflowStep) string {
	if r.ownerKind != "WorkflowRun" {
		return ""
	}
	if step.MemoryClaim != nil {
		return workflowStepMemoryClaimName(wf.ObjectMeta.Name, step.Name)
	}
	if wf.Spec.MemoryClaim != nil {
		return workflowRunMemoryClaimName(wf.ObjectMeta.Name)
	}
	return ""
}

type workflowPlan struct {
	OrderedSteps []v1alpha1.WorkflowStep
	Deps         map[string][]string
	Cyclic       bool
}

func buildWorkflowPlan(wf *v1alpha1.Workflow) (workflowPlan, error) {
	if len(wf.Spec.Steps) == 0 {
		return workflowPlan{}, fmt.Errorf("workflow must define at least one step")
	}

	stepByName := make(map[string]v1alpha1.WorkflowStep, len(wf.Spec.Steps))
	inDegree := make(map[string]int, len(wf.Spec.Steps))
	adj := make(map[string][]string, len(wf.Spec.Steps))
	deps := make(map[string][]string, len(wf.Spec.Steps))

	for _, step := range wf.Spec.Steps {
		if step.Name == "" {
			return workflowPlan{}, fmt.Errorf("workflow step name cannot be empty")
		}
		if _, exists := stepByName[step.Name]; exists {
			return workflowPlan{}, fmt.Errorf("duplicate workflow step name %q", step.Name)
		}
		stepByName[step.Name] = step
		inDegree[step.Name] = 0
		deps[step.Name] = []string{}
	}

	// Build graph exclusively from edges.
	for _, e := range wf.Spec.Edges {
		if _, ok := stepByName[e.From]; !ok {
			return workflowPlan{}, fmt.Errorf("edge from unknown step %q", e.From)
		}
		if _, ok := stepByName[e.To]; !ok {
			return workflowPlan{}, fmt.Errorf("edge to unknown step %q", e.To)
		}
		adj[e.From] = append(adj[e.From], e.To)
		inDegree[e.To]++
		deps[e.To] = append(deps[e.To], e.From)
	}

	if wf.Spec.EntryPoint != "" {
		if _, ok := stepByName[wf.Spec.EntryPoint]; !ok {
			return workflowPlan{}, fmt.Errorf("entryPoint %q does not match any step", wf.Spec.EntryPoint)
		}
	}

	queue := make([]string, 0)
	for name, deg := range inDegree {
		if deg == 0 {
			queue = append(queue, name)
		}
	}
	sort.Strings(queue)

	orderNames := make([]string, 0, len(stepByName))
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		orderNames = append(orderNames, n)
		for _, next := range adj[n] {
			inDegree[next]--
			if inDegree[next] == 0 {
				queue = append(queue, next)
				sort.Strings(queue)
			}
		}
	}

	if len(orderNames) != len(stepByName) {
		if wf.Spec.Budgets != nil && wf.Spec.Budgets.MaxIterations > 0 {
			ordered := make([]v1alpha1.WorkflowStep, 0, len(wf.Spec.Steps))
			ordered = append(ordered, wf.Spec.Steps...)
			if wf.Spec.EntryPoint != "" {
				for i := range ordered {
					if ordered[i].Name == wf.Spec.EntryPoint {
						ordered[0], ordered[i] = ordered[i], ordered[0]
						break
					}
				}
			}
			return workflowPlan{OrderedSteps: ordered, Deps: deps, Cyclic: true}, nil
		}
		return workflowPlan{}, fmt.Errorf("workflow DAG contains a cycle")
	}

	if wf.Spec.EntryPoint != "" {
		for i, n := range orderNames {
			if n == wf.Spec.EntryPoint {
				orderNames[0], orderNames[i] = orderNames[i], orderNames[0]
				break
			}
		}
	}

	ordered := make([]v1alpha1.WorkflowStep, 0, len(orderNames))
	for _, n := range orderNames {
		ordered = append(ordered, stepByName[n])
	}
	return workflowPlan{OrderedSteps: ordered, Deps: deps, Cyclic: false}, nil
}

func depsSatisfied(step string, deps map[string][]string, completed map[string]struct{}) bool {
	for _, dep := range deps[step] {
		if _, ok := completed[dep]; !ok {
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]struct{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// firstSorted returns the lexicographically first key from a set.
func firstSorted(m map[string]struct{}) string {
	keys := sortedKeys(m)
	if len(keys) == 0 {
		return ""
	}
	return keys[0]
}

func parseIterationLabel(labels map[string]string) int {
	if labels == nil {
		return 0
	}
	raw := labels[workflowIterationLabel]
	if raw == "" {
		return 0
	}
	iter, err := strconv.Atoi(raw)
	if err != nil || iter < 1 {
		return 0
	}
	return iter
}

func (r *WorkflowReconciler) reconcileAcyclic(ctx context.Context, wf *v1alpha1.Workflow, plan workflowPlan, jobsByStep map[string]v1alpha1.AgentJob) (Result, error) {
	now := time.Now().UTC()

	// Build lookup of step spec by name for retry/timeout decisions.
	stepByName := make(map[string]v1alpha1.WorkflowStep, len(plan.OrderedSteps))
	for _, s := range plan.OrderedSteps {
		stepByName[s.Name] = s
	}

	// T-0281: Build sets of completed, failed, and active steps.
	completed := make(map[string]struct{})
	failed := make(map[string]struct{})
	var active []string
	for step, j := range jobsByStep {
		switch j.Status.Phase {
		case v1alpha1.PhaseReady:
			completed[step] = struct{}{}
		case v1alpha1.PhaseFailed, v1alpha1.PhaseTimedOut:
			failed[step] = struct{}{}
		case v1alpha1.PhasePending, v1alpha1.PhaseRunning:
			active = append(active, step)
		}
	}
	// T-0279: Skipped steps count as completed for downstream dependency checks.
	// T-0296: Tool-typed steps execute synchronously and have no AgentJob, so
	// seed completed/failed from their StepStatus phases.
	for _, ss := range wf.Status.StepStatuses {
		if ss.Phase == v1alpha1.PhaseSkipped {
			completed[ss.Name] = struct{}{}
			continue
		}
		if _, hasJob := jobsByStep[ss.Name]; hasJob {
			continue
		}
		switch ss.Phase {
		case v1alpha1.PhaseReady:
			completed[ss.Name] = struct{}{}
		case v1alpha1.PhaseFailed, v1alpha1.PhaseTimedOut:
			failed[ss.Name] = struct{}{}
		}
	}

	// T-0271: Capture outputs from completed jobs into step statuses.
	r.syncStepStatuses(wf, jobsByStep)

	// T-0293: Per-step timeout enforcement. For jobs currently active, if the
	// step.Timeout has elapsed since ss.StartedAt, cancel the job and mark
	// PhaseTimedOut so retry logic below may pick it up.
	stillActive := active[:0]
	for _, name := range active {
		step, ok := stepByName[name]
		if !ok || step.Timeout == "" {
			stillActive = append(stillActive, name)
			continue
		}
		timeout, err := time.ParseDuration(step.Timeout)
		if err != nil || timeout <= 0 {
			stillActive = append(stillActive, name)
			continue
		}
		ss := findStepStatus(wf, name)
		if ss == nil || ss.StartedAt == nil {
			stillActive = append(stillActive, name)
			continue
		}
		if now.Sub(*ss.StartedAt) <= timeout {
			stillActive = append(stillActive, name)
			continue
		}
		// Timed out: delete the job, mark failed, emit event.
		if job, ok := jobsByStep[name]; ok {
			if err := r.store.Delete(ctx, "AgentJob", job.ObjectMeta.Namespace, job.ObjectMeta.Name); err != nil {
				return Result{}, fmt.Errorf("delete timed-out agent job %q: %w", job.ObjectMeta.Name, err)
			}
			delete(jobsByStep, name)
		}
		ss.Phase = v1alpha1.PhaseTimedOut
		failed[name] = struct{}{}
		if err := r.emitEvent(ctx, wf, "StepTimedOut", fmt.Sprintf("step %q timed out after %s", name, step.Timeout), map[string]string{"step": name}); err != nil {
			return Result{}, fmt.Errorf("append workflow event: %w", err)
		}
	}
	active = stillActive

	// T-0292: Per-step retry handling. For each failed step eligible for retry
	// (Retry != nil and attempts <= MaxAttempts), cancel the failed job, reset
	// the step status to PhasePending, schedule the next attempt via
	// NextRetryAt, and remove it from the failed set so the dispatch loop can
	// re-create the job once the backoff elapses.
	var nextRequeue time.Duration
	updateRequeue := func(d time.Duration) {
		if d <= 0 {
			return
		}
		if nextRequeue == 0 || d < nextRequeue {
			nextRequeue = d
		}
	}
	for name := range failed {
		step, ok := stepByName[name]
		if !ok {
			continue
		}
		ss := findStepStatus(wf, name)
		if ss == nil {
			continue
		}
		if !retryEligible(step.Retry, ss.Attempts) {
			// Retries exhausted (or none configured): clear any leftover
			// schedule so observers don't see a stale NextRetryAt on the
			// terminal-failed step.
			ss.NextRetryAt = nil
			continue
		}
		// Cancel the failed job to free the slot for the retry.
		if job, ok := jobsByStep[name]; ok {
			if err := r.store.Delete(ctx, "AgentJob", job.ObjectMeta.Namespace, job.ObjectMeta.Name); err != nil {
				return Result{}, fmt.Errorf("delete failed agent job %q for retry: %w", job.ObjectMeta.Name, err)
			}
			delete(jobsByStep, name)
		}
		delay := computeBackoff(step.Retry, ss.Attempts)
		retryAt := now.Add(delay)
		ss.Phase = v1alpha1.PhasePending
		ss.Output = ""
		ss.NextRetryAt = &retryAt
		ss.StartedAt = nil
		delete(failed, name)
		updateRequeue(delay)
		if err := r.emitEvent(ctx, wf, "StepRetrying", fmt.Sprintf("step %q scheduled for retry in %s", name, delay), map[string]string{"step": name}); err != nil {
			return Result{}, fmt.Errorf("append workflow event: %w", err)
		}
	}

	// T-0294: Compute blocking failures — failures that should halt the
	// workflow. A per-step ContinueOnError suppresses workflow failure for
	// that step (once retries are exhausted).
	blockingFailed := make(map[string]struct{}, len(failed))
	for name := range failed {
		if step, ok := stepByName[name]; ok && step.ContinueOnError {
			continue
		}
		blockingFailed[name] = struct{}{}
	}

	// Handle step failures before dispatching new work.
	if len(blockingFailed) > 0 && !wf.Spec.ContinueOnError {
		// T-0283: failFast — cancel pending/running jobs on failure.
		if wf.Spec.FailFast {
			for _, j := range jobsByStep {
				if j.Status.Phase == v1alpha1.PhasePending || j.Status.Phase == v1alpha1.PhaseRunning {
					if err := r.store.Delete(ctx, "AgentJob", j.ObjectMeta.Namespace, j.ObjectMeta.Name); err != nil {
						return Result{}, fmt.Errorf("cancel agent job %q on fail-fast: %w", j.ObjectMeta.Name, err)
					}
				}
			}
		}
		failedStep := firstSorted(failed)
		wf.Status.Phase = v1alpha1.PhaseFailed
		wf.Status.ActiveStep = failedStep
		wf.Status.ActiveSteps = nil
		wf.Status.CompletedSteps = sortedKeys(completed)
		if err := r.emitEvent(ctx, wf, "StepFailed", fmt.Sprintf("step %q failed", failedStep), map[string]string{"step": failedStep}); err != nil {
			return Result{}, fmt.Errorf("append workflow event: %w", err)
		}
		lc := lifecycle.SetCondition(
			apiToLifecycleConditions(wf.Status.Conditions),
			lifecycle.ConditionReady,
			lifecycle.ConditionFalse,
			"StepFailed",
			fmt.Sprintf("step %q failed", failedStep),
		)
		wf.Status.Conditions = lifecycleToAPIConditions(lc)
		if err := r.updateWorkflow(ctx, wf); err != nil {
			return Result{}, fmt.Errorf("update failed workflow: %w", err)
		}
		return Result{}, nil
	}

	// T-0284/T-0285: Build terminal set for dependency resolution.
	// In continueOnError mode, failed steps count as terminal so downstream
	// fan-in gates can evaluate once all sources finish.
	terminal := make(map[string]struct{}, len(completed)+len(failed))
	for k := range completed {
		terminal[k] = struct{}{}
	}
	if wf.Spec.ContinueOnError {
		for k := range failed {
			terminal[k] = struct{}{}
		}
	}

	// T-0281: Dispatch ALL ready steps per reconciliation pass.
	var dispatched []string
	var approvalBlockedSteps []string
	for _, step := range plan.OrderedSteps {
		// Skip steps already in terminal state.
		if isStepSkipped(wf, step.Name) {
			continue
		}
		if _, ok := completed[step.Name]; ok {
			continue
		}
		if _, ok := failed[step.Name]; ok {
			continue
		}
		// Skip steps that already have a job (pending/running).
		if _, ok := jobsByStep[step.Name]; ok {
			continue
		}

		// T-0292: Honor scheduled retry backoff — defer dispatch until NextRetryAt.
		if ss := findStepStatus(wf, step.Name); ss != nil && ss.NextRetryAt != nil {
			if now.Before(*ss.NextRetryAt) {
				updateRequeue(ss.NextRetryAt.Sub(now))
				continue
			}
		}

		// T-0285: Fan-in gate — wait for ALL source steps to reach terminal state.
		if !depsSatisfied(step.Name, plan.Deps, terminal) {
			continue
		}

		// T-0277: Evaluate CEL conditions on inbound edges.
		edgeAllowed, edgeErr := evaluateEdgeConditions(ctx, wf.Spec.Edges, step.Name, wf.Status.StepStatuses, r.workflowInput, wf.Spec.Parameters)
		if edgeErr != nil {
			return r.failWorkflowWithCELError(ctx, wf, step.Name, edgeErr)
		}
		if !edgeAllowed {
			r.skipStep(wf, step.Name)
			completed[step.Name] = struct{}{}
			terminal[step.Name] = struct{}{}
			continue
		}

		// T-0278: Evaluate CEL condition on the step itself.
		stepAllowed, stepCondErr := evaluateStepCondition(ctx, step, wf.Status.StepStatuses, r.workflowInput, wf.Spec.Parameters)
		if stepCondErr != nil {
			return r.failWorkflowWithCELError(ctx, wf, step.Name, stepCondErr)
		}
		if !stepAllowed {
			r.skipStep(wf, step.Name)
			completed[step.Name] = struct{}{}
			terminal[step.Name] = struct{}{}
			continue
		}
		sourceAgents, allowed, communicationErr := r.authorizeInboundAgentDispatch(ctx, wf, plan, step, 0)
		if communicationErr != nil {
			return Result{}, fmt.Errorf("authorize communication for step %q: %w", step.Name, communicationErr)
		}
		if !allowed {
			return r.failWorkflowWithCommunicationDenied(ctx, wf, sourceAgents[len(sourceAgents)-1], step.AgentRef.Name, step.Name, 0)
		}

		blocked, err := r.handleApprovalGate(ctx, wf, step.Name, 0)
		if err != nil {
			return Result{}, fmt.Errorf("handle approval gate: %w", err)
		}
		// Rejection/timeout inside the gate sets PhaseFailed; persist and exit
		// before downstream logic resets the phase back to Pending.
		if wf.Status.Phase == v1alpha1.PhaseFailed {
			if err := r.updateWorkflow(ctx, wf); err != nil {
				return Result{}, fmt.Errorf("update failed workflow: %w", err)
			}
			return Result{}, nil
		}
		if blocked {
			approvalBlockedSteps = append(approvalBlockedSteps, step.Name)
			continue
		}

		// T-0282: maxParallelSteps throttle — limit concurrent active steps.
		if wf.Spec.MaxParallelSteps > 0 && int32(len(active)+len(dispatched)) >= wf.Spec.MaxParallelSteps {
			continue
		}

		// T-0272/T-0273: Resolve template in step input before dispatch.
		resolvedInput, tmplErr := r.resolveStepInputFromWorkflow(wf, step)
		if tmplErr != nil {
			return r.failWorkflowWithTemplateError(ctx, wf, step.Name, tmplErr)
		}

		resolvedStep := step
		resolvedStep.Input = resolvedInput

		// T-0296: Tool-typed steps execute synchronously via WorkflowToolInvoker
		// instead of creating an AgentJob.
		if step.Type == v1alpha1.WorkflowStepTypeTool {
			if r.toolInvoker == nil {
				// Misconfiguration: fail the workflow terminally instead of retrying forever.
				msg := fmt.Sprintf("step %q has type=tool but controller has no WorkflowToolInvoker configured", step.Name)
				wf.Status.Phase = v1alpha1.PhaseFailed
				wf.Status.ActiveStep = step.Name
				wf.Status.ActiveSteps = nil
				lc := lifecycle.SetCondition(
					apiToLifecycleConditions(wf.Status.Conditions),
					lifecycle.ConditionReady,
					lifecycle.ConditionFalse,
					"ToolInvokerUnavailable",
					msg,
				)
				wf.Status.Conditions = lifecycleToAPIConditions(lc)
				if err := r.emitEvent(ctx, wf, "ToolStepFailed", msg, map[string]string{"step": step.Name}); err != nil {
					return Result{}, fmt.Errorf("append workflow event: %w", err)
				}
				if err := r.updateWorkflow(ctx, wf); err != nil {
					return Result{}, fmt.Errorf("update failed workflow: %w", err)
				}
				return Result{}, nil
			}
			principal := step.AgentRef.Name
			if principal == "" {
				principal = strings.ToLower(r.ownerKind) + ":" + wf.ObjectMeta.Name
			}
			var toolInput map[string]any
			if resolvedInput != "" {
				if err := json.Unmarshal([]byte(resolvedInput), &toolInput); err != nil {
					return r.failWorkflowWithTemplateError(ctx, wf, step.Name, fmt.Errorf("tool step input must be a JSON object: %w", err))
				}
			}
			startedAt := now
			ss := findStepStatus(wf, step.Name)
			if ss == nil {
				wf.Status.StepStatuses = append(wf.Status.StepStatuses, v1alpha1.WorkflowStepStatus{
					Name:      step.Name,
					Phase:     v1alpha1.PhaseRunning,
					Attempts:  1,
					StartedAt: &startedAt,
				})
				ss = findStepStatus(wf, step.Name)
			} else {
				ss.Attempts++
				ss.Phase = v1alpha1.PhaseRunning
				ss.Output = ""
				ss.StartedAt = &startedAt
				ss.NextRetryAt = nil
			}
			stepSpan, stepCtx := r.startWorkflowStepSpan(ctx, wf, step, 0)
			output, invokeErr := r.invokeToolStep(stepCtx, wf.ObjectMeta.Namespace, principal, step.ToolRef.Name, step.Timeout, toolInput)
			if invokeErr != nil {
				r.endWorkflowSpan(ctx, stepSpan, observability.StatusError, invokeErr.Error())
				// Per-step retry: reschedule with backoff until MaxAttempts is
				// reached, mirroring agent-step retry semantics. A per-step
				// timeout surfaces here as a context error and is retried too.
				if retryEligible(step.Retry, ss.Attempts) {
					delay := computeBackoff(step.Retry, ss.Attempts)
					retryAt := now.Add(delay)
					ss.Phase = v1alpha1.PhasePending
					ss.Output = ""
					ss.NextRetryAt = &retryAt
					ss.StartedAt = nil
					updateRequeue(delay)
					if err := r.emitEvent(ctx, wf, "StepRetrying", fmt.Sprintf("tool step %q scheduled for retry in %s", step.Name, delay), map[string]string{"step": step.Name}); err != nil {
						return Result{}, fmt.Errorf("append workflow event: %w", err)
					}
					continue
				}
				ss.Phase = v1alpha1.PhaseFailed
				// Leave Output empty: it is consumed by downstream template
				// resolution and must not carry an error string.
				ss.Output = ""
				failed[step.Name] = struct{}{}
				if err := r.emitEvent(ctx, wf, "ToolStepFailed", fmt.Sprintf("tool step %q failed: %s", step.Name, invokeErr.Error()), map[string]string{"step": step.Name, "error": invokeErr.Error()}); err != nil {
					return Result{}, fmt.Errorf("append workflow event: %w", err)
				}
				continue
			}
			outputJSON, mErr := json.Marshal(output)
			if mErr != nil {
				r.endWorkflowSpan(ctx, stepSpan, observability.StatusError, mErr.Error())
				return Result{}, fmt.Errorf("marshal tool output for step %q: %w", step.Name, mErr)
			}
			r.endWorkflowSpan(ctx, stepSpan, observability.StatusOK, "")
			ss.Phase = v1alpha1.PhaseReady
			ss.Output, _ = truncateOutput(string(outputJSON))
			completed[step.Name] = struct{}{}
			terminal[step.Name] = struct{}{}
			if err := r.emitEvent(ctx, wf, "ToolStepCompleted", fmt.Sprintf("tool step %q completed", step.Name), map[string]string{"step": step.Name}); err != nil {
				return Result{}, fmt.Errorf("append workflow event: %w", err)
			}
			continue
		}

		stepSpan, stepCtx := r.startWorkflowStepSpan(ctx, wf, step, 0)
		created, err := r.createStepJob(stepCtx, wf, resolvedStep, sourceAgents)
		if err != nil {
			r.endWorkflowSpan(ctx, stepSpan, observability.StatusError, err.Error())
			return Result{}, fmt.Errorf("create step job %q: %w", step.Name, err)
		}
		if created {
			r.endWorkflowSpan(ctx, stepSpan, observability.StatusOK, "")
		} else if r.tracer != nil {
			_ = r.tracer.EndSpan(stepSpan, observability.StatusOK, "")
		}
		// T-0292: Track attempt counter and start time for retry/timeout logic.
		startedAt := now
		if ss := findStepStatus(wf, step.Name); ss != nil {
			ss.Attempts++
			ss.Phase = v1alpha1.PhasePending
			ss.Output = ""
			ss.StartedAt = &startedAt
			ss.NextRetryAt = nil
		} else {
			wf.Status.StepStatuses = append(wf.Status.StepStatuses, v1alpha1.WorkflowStepStatus{
				Name:      step.Name,
				Phase:     v1alpha1.PhasePending,
				Attempts:  1,
				StartedAt: &startedAt,
			})
		}
		if err := r.emitEvent(ctx, wf, "StepDispatched", fmt.Sprintf("step %q dispatched", step.Name), map[string]string{"step": step.Name}); err != nil {
			return Result{}, fmt.Errorf("append workflow event: %w", err)
		}
		dispatched = append(dispatched, step.Name)
	}

	// Compute final status.
	wf.Status.CompletedSteps = sortedKeys(completed)
	allActive := make([]string, 0, len(active))
	allActive = append(allActive, active...)
	allActive = append(allActive, dispatched...)
	sort.Strings(allActive)
	wf.Status.ActiveSteps = allActive
	if len(allActive) > 0 {
		wf.Status.ActiveStep = allActive[0]
	} else {
		wf.Status.ActiveStep = ""
	}

	// Recompute blockingFailed: tool-typed step failures occur during the
	// dispatch loop above, so the earlier computation may be stale.
	blockingFailed = make(map[string]struct{}, len(failed))
	for name := range failed {
		if step, ok := stepByName[name]; ok && step.ContinueOnError {
			continue
		}
		blockingFailed[name] = struct{}{}
	}

	totalTerminal := len(completed) + len(failed)
	if len(plan.OrderedSteps) > 0 && totalTerminal == len(plan.OrderedSteps) {
		// All steps reached terminal state.
		if len(blockingFailed) > 0 {
			// T-0284/T-0294: at least one failure is not suppressed by continueOnError.
			wf.Status.Phase = v1alpha1.PhaseFailed
			wf.Status.ActiveStep = firstSorted(blockingFailed)
			wf.Status.ActiveSteps = nil
			if err := r.emitEvent(ctx, wf, "StepFailed", fmt.Sprintf("step %q failed", firstSorted(blockingFailed)), map[string]string{"step": firstSorted(blockingFailed)}); err != nil {
				return Result{}, fmt.Errorf("append workflow event: %w", err)
			}
			lc := lifecycle.SetCondition(
				apiToLifecycleConditions(wf.Status.Conditions),
				lifecycle.ConditionReady,
				lifecycle.ConditionFalse,
				"StepFailed",
				fmt.Sprintf("step %q failed", firstSorted(blockingFailed)),
			)
			wf.Status.Conditions = lifecycleToAPIConditions(lc)
			if err := r.updateWorkflow(ctx, wf); err != nil {
				return Result{}, fmt.Errorf("update failed workflow: %w", err)
			}
			return Result{}, nil
		}
		wf.Status.Phase = v1alpha1.PhaseReady
		wf.Status.ActiveStep = ""
		wf.Status.ActiveSteps = nil
		if err := r.emitEvent(ctx, wf, "WorkflowCompleted", "workflow completed successfully", nil); err != nil {
			return Result{}, fmt.Errorf("append workflow event: %w", err)
		}
		lc := lifecycle.SetCondition(
			apiToLifecycleConditions(wf.Status.Conditions),
			lifecycle.ConditionReady,
			lifecycle.ConditionTrue,
			"Completed",
			"workflow completed successfully",
		)
		wf.Status.Conditions = lifecycleToAPIConditions(lc)
		if err := r.updateWorkflow(ctx, wf); err != nil {
			return Result{}, fmt.Errorf("update completed workflow: %w", err)
		}
		return Result{}, nil
	}

	if len(allActive) > 0 {
		wf.Status.Phase = v1alpha1.PhaseRunning
		stepNames := strings.Join(allActive, ", ")
		if err := r.emitEvent(ctx, wf, "StepRunning", fmt.Sprintf("steps executing: %s", stepNames), nil); err != nil {
			return Result{}, fmt.Errorf("append workflow event: %w", err)
		}
		lc := lifecycle.SetCondition(
			apiToLifecycleConditions(wf.Status.Conditions),
			lifecycle.ConditionReady,
			lifecycle.ConditionFalse,
			"Executing",
			fmt.Sprintf("steps executing: %s", stepNames),
		)
		wf.Status.Conditions = lifecycleToAPIConditions(lc)
		if err := r.updateWorkflow(ctx, wf); err != nil {
			return Result{}, fmt.Errorf("update running workflow: %w", err)
		}
		if len(dispatched) > 0 {
			return Result{Requeue: true}, nil
		}
		if nextRequeue > 0 && nextRequeue < 2*time.Second {
			return Result{RequeueAfter: nextRequeue}, nil
		}
		return Result{RequeueAfter: 2 * time.Second}, nil
	}

	wf.Status.Phase = v1alpha1.PhasePending
	waitingReason := "Waiting"
	waitingMessage := "workflow waiting for dependencies"
	if len(approvalBlockedSteps) > 0 {
		sort.Strings(approvalBlockedSteps)
		wf.Status.ActiveStep = approvalBlockedSteps[0]
		wf.Status.ActiveSteps = approvalBlockedSteps
		waitingReason = "WaitingApproval"
		waitingMessage = fmt.Sprintf("approval required for step %q", approvalBlockedSteps[0])
	} else {
		wf.Status.ActiveStep = ""
		wf.Status.ActiveSteps = nil
	}
	lc := lifecycle.SetCondition(
		apiToLifecycleConditions(wf.Status.Conditions),
		lifecycle.ConditionReady,
		lifecycle.ConditionFalse,
		waitingReason,
		waitingMessage,
	)
	wf.Status.Conditions = lifecycleToAPIConditions(lc)
	if len(approvalBlockedSteps) > 0 {
		if err := r.updateWorkflow(ctx, wf); err != nil {
			return Result{}, fmt.Errorf("update waiting approval workflow: %w", err)
		}
		return Result{RequeueAfter: 2 * time.Second}, nil
	}
	if err := r.updateWorkflow(ctx, wf); err != nil {
		return Result{}, fmt.Errorf("update pending workflow: %w", err)
	}
	if nextRequeue > 0 && nextRequeue < 2*time.Second {
		return Result{RequeueAfter: nextRequeue}, nil
	}
	return Result{RequeueAfter: 2 * time.Second}, nil
}

// reconcileCyclic drives cyclic workflows one step at a time within each
// iteration, applying retry, timeout, and continue-on-error controls to the
// current iteration's AgentJob.
func (r *WorkflowReconciler) reconcileCyclic(ctx context.Context, wf *v1alpha1.Workflow, plan workflowPlan, jobsByStepIteration map[string]map[int]v1alpha1.AgentJob) (Result, error) {
	now := time.Now().UTC()
	maxIterations := int(wf.Spec.Budgets.MaxIterations)
	if maxIterations < 1 {
		wf.Status.Phase = v1alpha1.PhaseFailed
		lc := lifecycle.SetCondition(
			apiToLifecycleConditions(wf.Status.Conditions),
			lifecycle.ConditionReady,
			lifecycle.ConditionFalse,
			"InvalidBudget",
			"cyclic workflows require budgets.maxIterations >= 1",
		)
		wf.Status.Conditions = lifecycleToAPIConditions(lc)
		if err := r.updateWorkflow(ctx, wf); err != nil {
			return Result{}, fmt.Errorf("update invalid cyclic workflow: %w", err)
		}
		return Result{}, nil
	}

	iteration := currentIteration(wf)
	if iteration < 1 {
		iteration = 1
	}
	ensureAnnotationMap(&wf.ObjectMeta)
	wf.ObjectMeta.Annotations[workflowCurrentIterationKey] = strconv.Itoa(iteration)

	completedSet := map[string]struct{}{}
	for _, token := range parseCSVTokens(wf.ObjectMeta.Annotations[workflowCompletedToolsKey]) {
		completedSet[token] = struct{}{}
	}
	for stepName, byIter := range jobsByStepIteration {
		for iter, job := range byIter {
			if job.Status.Phase == v1alpha1.PhaseReady {
				completedSet[fmt.Sprintf("%s@%d", stepName, iter)] = struct{}{}
			}
		}
	}

	// T-0271: Capture outputs from completed cyclic jobs into step statuses.
	r.syncCyclicStepStatuses(wf, jobsByStepIteration)

	wf.Status.CompletedSteps = sortedKeys(completedSet)
	wf.Status.ActiveStep = ""

	if iteration > maxIterations {
		wf.Status.Phase = v1alpha1.PhaseReady
		if err := r.emitEvent(ctx, wf, "WorkflowCompleted", fmt.Sprintf("workflow completed after %d iterations", maxIterations), map[string]string{"iterations": strconv.Itoa(maxIterations)}); err != nil {
			return Result{}, fmt.Errorf("append workflow event: %w", err)
		}
		lc := lifecycle.SetCondition(
			apiToLifecycleConditions(wf.Status.Conditions),
			lifecycle.ConditionReady,
			lifecycle.ConditionTrue,
			"Completed",
			"workflow completed successfully",
		)
		wf.Status.Conditions = lifecycleToAPIConditions(lc)
		if err := r.updateWorkflow(ctx, wf); err != nil {
			return Result{}, fmt.Errorf("update completed cyclic workflow: %w", err)
		}
		return Result{}, nil
	}

	completedThisIter := map[string]struct{}{}
	iterationTokenUsage := int64(0)
	for _, step := range plan.OrderedSteps {
		if cyclicFailureIgnored(wf, step.Name, iteration) || cyclicToolCompleted(wf, step.Name, iteration) {
			completedThisIter[step.Name] = struct{}{}
		}
	}
	for stepName, byIter := range jobsByStepIteration {
		if job, ok := byIter[iteration]; ok {
			if job.Status.Phase == v1alpha1.PhaseReady {
				completedThisIter[stepName] = struct{}{}
			}
			iterationTokenUsage += estimateTokens(job.Spec.Input)
		}
	}

	handleFailure := func(step v1alpha1.WorkflowStep, job v1alpha1.AgentJob, phase v1alpha1.Phase) (Result, bool, error) {
		ss := findStepStatus(wf, step.Name)
		if ss == nil {
			startedAt := job.ObjectMeta.CreationTimestamp
			wf.Status.StepStatuses = append(wf.Status.StepStatuses, v1alpha1.WorkflowStepStatus{
				Name:      step.Name,
				Phase:     phase,
				Attempts:  1,
				StartedAt: &startedAt,
			})
			ss = findStepStatus(wf, step.Name)
		}
		if ss.Attempts < 1 {
			ss.Attempts = 1
		}
		ss.Phase = phase
		ss.NextRetryAt = nil

		if retryEligible(step.Retry, ss.Attempts) {
			if err := r.store.Delete(ctx, "AgentJob", job.ObjectMeta.Namespace, job.ObjectMeta.Name); err != nil && !errors.Is(err, store.ErrNotFound) {
				return Result{}, true, fmt.Errorf("delete failed cyclic agent job %q for retry: %w", job.ObjectMeta.Name, err)
			}
			delay := computeBackoff(step.Retry, ss.Attempts)
			retryAt := now.Add(delay)
			ss.Phase = v1alpha1.PhasePending
			ss.Output = ""
			ss.StartedAt = nil
			ss.NextRetryAt = &retryAt
			if err := r.emitEvent(ctx, wf, "StepRetrying", fmt.Sprintf("step %q scheduled for retry in iteration %d after %s", step.Name, iteration, delay), map[string]string{"step": step.Name, "iteration": strconv.Itoa(iteration)}); err != nil {
				return Result{}, true, fmt.Errorf("append workflow event: %w", err)
			}
			wf.Status.Phase = v1alpha1.PhasePending
			wf.Status.ActiveStep = fmt.Sprintf("%s@%d", step.Name, iteration)
			if err := r.updateWorkflow(ctx, wf); err != nil {
				return Result{}, true, fmt.Errorf("update cyclic workflow retry: %w", err)
			}
			if delay > 0 {
				return Result{RequeueAfter: delay}, true, nil
			}
			return Result{Requeue: true}, true, nil
		}

		if step.ContinueOnError || wf.Spec.ContinueOnError {
			ensureAnnotationMap(&wf.ObjectMeta)
			token := gateAnnotationToken(step.Name, iteration)
			wf.ObjectMeta.Annotations[workflowIgnoredFailuresKey] = appendRejectedToken(wf.ObjectMeta.Annotations[workflowIgnoredFailuresKey], token)
			completedThisIter[step.Name] = struct{}{}
			return Result{}, false, nil
		}

		wf.Status.Phase = v1alpha1.PhaseFailed
		wf.Status.ActiveStep = fmt.Sprintf("%s@%d", step.Name, iteration)
		if err := r.emitEvent(ctx, wf, "StepFailed", fmt.Sprintf("step %q failed in iteration %d", step.Name, iteration), map[string]string{"step": step.Name, "iteration": strconv.Itoa(iteration)}); err != nil {
			return Result{}, true, fmt.Errorf("append workflow event: %w", err)
		}
		lc := lifecycle.SetCondition(
			apiToLifecycleConditions(wf.Status.Conditions),
			lifecycle.ConditionReady,
			lifecycle.ConditionFalse,
			"StepFailed",
			fmt.Sprintf("step %q failed in iteration %d", step.Name, iteration),
		)
		wf.Status.Conditions = lifecycleToAPIConditions(lc)
		if err := r.updateWorkflow(ctx, wf); err != nil {
			return Result{}, true, fmt.Errorf("update cyclic failed workflow: %w", err)
		}
		return Result{}, true, nil
	}

	for idx, step := range plan.OrderedSteps {
		if cyclicFailureIgnored(wf, step.Name, iteration) || cyclicToolCompleted(wf, step.Name, iteration) {
			continue
		}
		iterJobs := jobsByStepIteration[step.Name]
		job, exists := iterJobs[iteration]
		if exists {
			switch job.Status.Phase {
			case v1alpha1.PhaseReady:
				continue
			case v1alpha1.PhaseFailed, v1alpha1.PhaseTimedOut:
				result, handled, err := handleFailure(step, job, job.Status.Phase)
				if err != nil || handled {
					return result, err
				}
				continue
			case v1alpha1.PhasePending, v1alpha1.PhaseRunning:
				ss := findStepStatus(wf, step.Name)
				if ss == nil {
					startedAt := job.ObjectMeta.CreationTimestamp
					wf.Status.StepStatuses = append(wf.Status.StepStatuses, v1alpha1.WorkflowStepStatus{
						Name:      step.Name,
						Phase:     job.Status.Phase,
						Attempts:  1,
						StartedAt: &startedAt,
					})
					ss = findStepStatus(wf, step.Name)
				}
				if step.Timeout != "" && ss.StartedAt != nil {
					if timeout, err := time.ParseDuration(step.Timeout); err == nil && timeout > 0 && now.Sub(*ss.StartedAt) > timeout {
						if err := r.store.Delete(ctx, "AgentJob", job.ObjectMeta.Namespace, job.ObjectMeta.Name); err != nil && !errors.Is(err, store.ErrNotFound) {
							return Result{}, fmt.Errorf("delete timed-out cyclic agent job %q: %w", job.ObjectMeta.Name, err)
						}
						ss.Phase = v1alpha1.PhaseTimedOut
						if err := r.emitEvent(ctx, wf, "StepTimedOut", fmt.Sprintf("step %q timed out after %s in iteration %d", step.Name, step.Timeout, iteration), map[string]string{"step": step.Name, "iteration": strconv.Itoa(iteration)}); err != nil {
							return Result{}, fmt.Errorf("append workflow event: %w", err)
						}
						result, handled, failureErr := handleFailure(step, job, v1alpha1.PhaseTimedOut)
						if failureErr != nil || handled {
							return result, failureErr
						}
						continue
					}
				}
				wf.Status.Phase = v1alpha1.PhaseRunning
				wf.Status.ActiveStep = fmt.Sprintf("%s@%d", step.Name, iteration)
				if err := r.emitEvent(ctx, wf, "StepRunning", fmt.Sprintf("step %q is executing in iteration %d", step.Name, iteration), map[string]string{"step": step.Name, "iteration": strconv.Itoa(iteration)}); err != nil {
					return Result{}, fmt.Errorf("append workflow event: %w", err)
				}
				lc := lifecycle.SetCondition(
					apiToLifecycleConditions(wf.Status.Conditions),
					lifecycle.ConditionReady,
					lifecycle.ConditionFalse,
					"Executing",
					fmt.Sprintf("step %q is executing in iteration %d", step.Name, iteration),
				)
				wf.Status.Conditions = lifecycleToAPIConditions(lc)
				if err := r.updateWorkflow(ctx, wf); err != nil {
					return Result{}, fmt.Errorf("update cyclic running workflow: %w", err)
				}
				return Result{RequeueAfter: 2 * time.Second}, nil
			}
		}

		if ss := findStepStatus(wf, step.Name); ss != nil && ss.NextRetryAt != nil && now.Before(*ss.NextRetryAt) {
			wf.Status.Phase = v1alpha1.PhasePending
			wf.Status.ActiveStep = fmt.Sprintf("%s@%d", step.Name, iteration)
			if err := r.updateWorkflow(ctx, wf); err != nil {
				return Result{}, fmt.Errorf("update cyclic workflow waiting for retry: %w", err)
			}
			return Result{RequeueAfter: ss.NextRetryAt.Sub(now)}, nil
		}

		if idx > 0 && !depsSatisfied(step.Name, plan.Deps, completedThisIter) {
			continue
		}
		if idx == 0 && len(completedThisIter) > 0 && !depsSatisfied(step.Name, plan.Deps, completedThisIter) {
			continue
		}

		// CEL edge condition evaluation for cyclic steps.
		edgeAllowed, edgeErr := evaluateEdgeConditions(ctx, wf.Spec.Edges, step.Name, wf.Status.StepStatuses, r.workflowInput, wf.Spec.Parameters)
		if edgeErr != nil {
			return r.failWorkflowWithCELError(ctx, wf, step.Name, edgeErr)
		}
		if !edgeAllowed {
			r.skipStep(wf, step.Name)
			completedThisIter[step.Name] = struct{}{}
			continue
		}

		// CEL step condition evaluation for cyclic steps.
		stepAllowed, stepCondErr := evaluateStepCondition(ctx, step, wf.Status.StepStatuses, r.workflowInput, wf.Spec.Parameters)
		if stepCondErr != nil {
			return r.failWorkflowWithCELError(ctx, wf, step.Name, stepCondErr)
		}
		if !stepAllowed {
			r.skipStep(wf, step.Name)
			completedThisIter[step.Name] = struct{}{}
			continue
		}
		sourceAgents, allowed, communicationErr := r.authorizeInboundAgentDispatch(ctx, wf, plan, step, iteration)
		if communicationErr != nil {
			return Result{}, fmt.Errorf("authorize communication for cyclic step %q: %w", step.Name, communicationErr)
		}
		if !allowed {
			return r.failWorkflowWithCommunicationDenied(ctx, wf, sourceAgents[len(sourceAgents)-1], step.AgentRef.Name, step.Name, iteration)
		}

		if wf.Spec.Budgets != nil && wf.Spec.Budgets.MaxTokensPerCycle > 0 {
			nextTokens := iterationTokenUsage + estimateTokens(step.Input)
			if nextTokens > wf.Spec.Budgets.MaxTokensPerCycle {
				wf.Status.Phase = v1alpha1.PhaseFailed
				wf.Status.ActiveStep = fmt.Sprintf("%s@%d", step.Name, iteration)
				if err := r.emitEvent(ctx, wf, "BudgetExceeded", fmt.Sprintf("iteration %d exceeded maxTokensPerCycle budget", iteration), map[string]string{"iteration": strconv.Itoa(iteration), "step": step.Name, "maxTokensPerCycle": strconv.FormatInt(wf.Spec.Budgets.MaxTokensPerCycle, 10), "estimatedTokens": strconv.FormatInt(nextTokens, 10)}); err != nil {
					return Result{}, fmt.Errorf("append workflow event: %w", err)
				}
				lc := lifecycle.SetCondition(
					apiToLifecycleConditions(wf.Status.Conditions),
					lifecycle.ConditionReady,
					lifecycle.ConditionFalse,
					"BudgetExceeded",
					fmt.Sprintf("iteration %d exceeded maxTokensPerCycle budget", iteration),
				)
				wf.Status.Conditions = lifecycleToAPIConditions(lc)
				if err := r.updateWorkflow(ctx, wf); err != nil {
					return Result{}, fmt.Errorf("update budget exceeded workflow: %w", err)
				}
				return Result{}, nil
			}
		}

		blocked, err := r.handleApprovalGate(ctx, wf, step.Name, iteration)
		if err != nil {
			return Result{}, fmt.Errorf("handle approval gate: %w", err)
		}
		if blocked {
			if err := r.updateWorkflow(ctx, wf); err != nil {
				return Result{}, fmt.Errorf("update cyclic waiting approval workflow: %w", err)
			}
			return Result{RequeueAfter: 2 * time.Second}, nil
		}

		// T-0272/T-0273: Resolve template in step input before cyclic dispatch.
		resolvedInput, tmplErr := r.resolveStepInputFromWorkflow(wf, step)
		if tmplErr != nil {
			return r.failWorkflowWithTemplateError(ctx, wf, step.Name, tmplErr)
		}

		resolvedStep := step
		resolvedStep.Input = resolvedInput
		if step.Type == v1alpha1.WorkflowStepTypeTool {
			if r.toolInvoker == nil {
				msg := fmt.Sprintf("step %q has type=tool but controller has no WorkflowToolInvoker configured", step.Name)
				wf.Status.Phase = v1alpha1.PhaseFailed
				wf.Status.ActiveStep = fmt.Sprintf("%s@%d", step.Name, iteration)
				lc := lifecycle.SetCondition(
					apiToLifecycleConditions(wf.Status.Conditions),
					lifecycle.ConditionReady,
					lifecycle.ConditionFalse,
					"ToolInvokerUnavailable",
					msg,
				)
				wf.Status.Conditions = lifecycleToAPIConditions(lc)
				if err := r.emitEvent(ctx, wf, "ToolStepFailed", msg, map[string]string{"step": step.Name, "iteration": strconv.Itoa(iteration)}); err != nil {
					return Result{}, fmt.Errorf("append workflow event: %w", err)
				}
				if err := r.updateWorkflow(ctx, wf); err != nil {
					return Result{}, fmt.Errorf("update failed cyclic workflow: %w", err)
				}
				return Result{}, nil
			}

			principal := step.AgentRef.Name
			if principal == "" {
				principal = strings.ToLower(r.ownerKind) + ":" + wf.ObjectMeta.Name
			}
			var toolInput map[string]any
			if resolvedInput != "" {
				if err := json.Unmarshal([]byte(resolvedInput), &toolInput); err != nil {
					return r.failWorkflowWithTemplateError(ctx, wf, step.Name, fmt.Errorf("tool step input must be a JSON object: %w", err))
				}
			}

			startedAt := now
			ss := findStepStatus(wf, step.Name)
			if ss == nil {
				wf.Status.StepStatuses = append(wf.Status.StepStatuses, v1alpha1.WorkflowStepStatus{
					Name:      step.Name,
					Phase:     v1alpha1.PhaseRunning,
					Attempts:  1,
					StartedAt: &startedAt,
				})
				ss = findStepStatus(wf, step.Name)
			} else {
				if ss.NextRetryAt != nil {
					ss.Attempts++
				} else {
					ss.Attempts = 1
				}
				ss.Phase = v1alpha1.PhaseRunning
				ss.Output = ""
				ss.StartedAt = &startedAt
				ss.NextRetryAt = nil
			}

			stepSpan, stepCtx := r.startWorkflowStepSpan(ctx, wf, step, iteration)
			output, invokeErr := r.invokeToolStep(stepCtx, wf.ObjectMeta.Namespace, principal, step.ToolRef.Name, step.Timeout, toolInput)
			if invokeErr != nil {
				r.endWorkflowSpan(ctx, stepSpan, observability.StatusError, invokeErr.Error())
				if retryEligible(step.Retry, ss.Attempts) {
					delay := computeBackoff(step.Retry, ss.Attempts)
					retryAt := now.Add(delay)
					ss.Phase = v1alpha1.PhasePending
					ss.StartedAt = nil
					ss.NextRetryAt = &retryAt
					wf.Status.Phase = v1alpha1.PhasePending
					wf.Status.ActiveStep = fmt.Sprintf("%s@%d", step.Name, iteration)
					if err := r.emitEvent(ctx, wf, "StepRetrying", fmt.Sprintf("tool step %q scheduled for retry in iteration %d after %s", step.Name, iteration, delay), map[string]string{"step": step.Name, "iteration": strconv.Itoa(iteration)}); err != nil {
						return Result{}, fmt.Errorf("append workflow event: %w", err)
					}
					if err := r.updateWorkflow(ctx, wf); err != nil {
						return Result{}, fmt.Errorf("update cyclic workflow retry: %w", err)
					}
					if delay > 0 {
						return Result{RequeueAfter: delay}, nil
					}
					return Result{Requeue: true}, nil
				}

				ss.Phase = v1alpha1.PhaseFailed
				ss.Output = ""
				if err := r.emitEvent(ctx, wf, "ToolStepFailed", fmt.Sprintf("tool step %q failed in iteration %d: %s", step.Name, iteration, invokeErr.Error()), map[string]string{"step": step.Name, "iteration": strconv.Itoa(iteration), "error": invokeErr.Error()}); err != nil {
					return Result{}, fmt.Errorf("append workflow event: %w", err)
				}
				if step.ContinueOnError || wf.Spec.ContinueOnError {
					wf.ObjectMeta.Annotations[workflowIgnoredFailuresKey] = appendRejectedToken(wf.ObjectMeta.Annotations[workflowIgnoredFailuresKey], gateAnnotationToken(step.Name, iteration))
					completedThisIter[step.Name] = struct{}{}
					continue
				}

				wf.Status.Phase = v1alpha1.PhaseFailed
				wf.Status.ActiveStep = fmt.Sprintf("%s@%d", step.Name, iteration)
				lc := lifecycle.SetCondition(
					apiToLifecycleConditions(wf.Status.Conditions),
					lifecycle.ConditionReady,
					lifecycle.ConditionFalse,
					"StepFailed",
					fmt.Sprintf("tool step %q failed in iteration %d", step.Name, iteration),
				)
				wf.Status.Conditions = lifecycleToAPIConditions(lc)
				if err := r.updateWorkflow(ctx, wf); err != nil {
					return Result{}, fmt.Errorf("update cyclic failed workflow: %w", err)
				}
				return Result{}, nil
			}

			outputJSON, err := json.Marshal(output)
			if err != nil {
				r.endWorkflowSpan(ctx, stepSpan, observability.StatusError, err.Error())
				return Result{}, fmt.Errorf("marshal tool output for step %q: %w", step.Name, err)
			}
			r.endWorkflowSpan(ctx, stepSpan, observability.StatusOK, "")
			ss.Phase = v1alpha1.PhaseReady
			ss.Output, _ = truncateOutput(string(outputJSON))
			token := gateAnnotationToken(step.Name, iteration)
			wf.ObjectMeta.Annotations[workflowCompletedToolsKey] = appendRejectedToken(wf.ObjectMeta.Annotations[workflowCompletedToolsKey], token)
			completedThisIter[step.Name] = struct{}{}
			completedSet[token] = struct{}{}
			if err := r.emitEvent(ctx, wf, "ToolStepCompleted", fmt.Sprintf("tool step %q completed in iteration %d", step.Name, iteration), map[string]string{"step": step.Name, "iteration": strconv.Itoa(iteration)}); err != nil {
				return Result{}, fmt.Errorf("append workflow event: %w", err)
			}
			continue
		}

		stepSpan, stepCtx := r.startWorkflowStepSpan(ctx, wf, step, iteration)
		created, err := r.createStepJobForIteration(stepCtx, wf, resolvedStep, iteration, sourceAgents)
		if err != nil {
			r.endWorkflowSpan(ctx, stepSpan, observability.StatusError, err.Error())
			return Result{}, fmt.Errorf("create cyclic step job %q: %w", step.Name, err)
		}
		if created {
			r.endWorkflowSpan(ctx, stepSpan, observability.StatusOK, "")
		} else if r.tracer != nil {
			_ = r.tracer.EndSpan(stepSpan, observability.StatusOK, "")
		}
		startedAt := now
		if ss := findStepStatus(wf, step.Name); ss != nil {
			if ss.NextRetryAt != nil {
				ss.Attempts++
			} else {
				ss.Attempts = 1
			}
			ss.Phase = v1alpha1.PhasePending
			ss.Output = ""
			ss.StartedAt = &startedAt
			ss.NextRetryAt = nil
		} else {
			wf.Status.StepStatuses = append(wf.Status.StepStatuses, v1alpha1.WorkflowStepStatus{
				Name:      step.Name,
				Phase:     v1alpha1.PhasePending,
				Attempts:  1,
				StartedAt: &startedAt,
			})
		}
		if err := r.emitEvent(ctx, wf, "StepDispatched", fmt.Sprintf("step %q dispatched in iteration %d", step.Name, iteration), map[string]string{"step": step.Name, "iteration": strconv.Itoa(iteration)}); err != nil {
			return Result{}, fmt.Errorf("append workflow event: %w", err)
		}
		wf.Status.Phase = v1alpha1.PhaseRunning
		wf.Status.ActiveStep = fmt.Sprintf("%s@%d", step.Name, iteration)
		lc := lifecycle.SetCondition(
			apiToLifecycleConditions(wf.Status.Conditions),
			lifecycle.ConditionReady,
			lifecycle.ConditionFalse,
			"Executing",
			fmt.Sprintf("step %q is executing in iteration %d", step.Name, iteration),
		)
		wf.Status.Conditions = lifecycleToAPIConditions(lc)
		if err := r.updateWorkflow(ctx, wf); err != nil {
			return Result{}, fmt.Errorf("update cyclic workflow after job create: %w", err)
		}
		return Result{Requeue: true}, nil
	}

	wf.Status.CompletedSteps = sortedKeys(completedSet)
	nextIter := iteration + 1
	if nextIter > maxIterations {
		wf.Status.Phase = v1alpha1.PhaseReady
		wf.Status.ActiveStep = ""
		if err := r.emitEvent(ctx, wf, "WorkflowCompleted", fmt.Sprintf("workflow completed after %d iterations", iteration), map[string]string{"iterations": strconv.Itoa(iteration)}); err != nil {
			return Result{}, fmt.Errorf("append workflow event: %w", err)
		}
		lc := lifecycle.SetCondition(
			apiToLifecycleConditions(wf.Status.Conditions),
			lifecycle.ConditionReady,
			lifecycle.ConditionTrue,
			"Completed",
			"workflow completed successfully",
		)
		wf.Status.Conditions = lifecycleToAPIConditions(lc)
		if err := r.updateWorkflow(ctx, wf); err != nil {
			return Result{}, fmt.Errorf("update cyclic completed workflow: %w", err)
		}
		return Result{}, nil
	}

	wf.ObjectMeta.Annotations[workflowCurrentIterationKey] = strconv.Itoa(nextIter)
	wf.Status.Phase = v1alpha1.PhasePending
	wf.Status.ActiveStep = ""
	if err := r.emitEvent(ctx, wf, "IterationCompleted", fmt.Sprintf("iteration %d completed", iteration), map[string]string{"iteration": strconv.Itoa(iteration)}); err != nil {
		return Result{}, fmt.Errorf("append workflow event: %w", err)
	}
	lc := lifecycle.SetCondition(
		apiToLifecycleConditions(wf.Status.Conditions),
		lifecycle.ConditionReady,
		lifecycle.ConditionFalse,
		"Waiting",
		fmt.Sprintf("advancing to iteration %d", nextIter),
	)
	wf.Status.Conditions = lifecycleToAPIConditions(lc)
	if err := r.updateWorkflow(ctx, wf); err != nil {
		return Result{}, fmt.Errorf("update cyclic next iteration workflow: %w", err)
	}
	return Result{Requeue: true}, nil
}

func (r *WorkflowReconciler) handleApprovalGate(ctx context.Context, wf *v1alpha1.Workflow, step string, iteration int) (bool, error) {
	gate, ok := lookupApprovalGate(wf, step)
	if !ok {
		return false, nil
	}
	if err := r.evaluateApprovalDecisions(ctx, wf, step, iteration, gate); err != nil {
		return false, err
	}
	if stepRejected(wf, step, iteration) {
		return r.failWorkflowOnApprovalRejection(ctx, wf, step, iteration)
	}
	if stepApproved(wf, step, iteration) {
		if err := r.emitEvent(ctx, wf, "ApprovalGranted", fmt.Sprintf("approval granted for step %q", step), map[string]string{"step": step, "iteration": strconv.Itoa(iteration)}); err != nil {
			return false, err
		}
		return false, nil
	}

	// T-0307: track gate entry timestamp and enforce timeout.
	enteredKey := workflowGateEnteredPrefix + gateAnnotationToken(step, iteration)
	now := time.Now().UTC()
	ensureAnnotationMap(&wf.ObjectMeta)
	enteredRaw, hadEntered := wf.ObjectMeta.Annotations[enteredKey]
	if !hadEntered {
		wf.ObjectMeta.Annotations[enteredKey] = now.Format(time.RFC3339Nano)
	} else if gate.Timeout != "" {
		if timeout, err := time.ParseDuration(gate.Timeout); err == nil && timeout > 0 {
			if entered, err := time.Parse(time.RFC3339Nano, enteredRaw); err == nil {
				if now.Sub(entered) >= timeout {
					token := gateAnnotationToken(step, iteration)
					existing := wf.ObjectMeta.Annotations[workflowRejectedStepsKey]
					wf.ObjectMeta.Annotations[workflowRejectedStepsKey] = appendRejectedToken(existing, token)
					if err := r.emitEvent(ctx, wf, "ApprovalTimedOut", fmt.Sprintf("approval timed out for step %q after %s", step, gate.Timeout), map[string]string{"step": step, "iteration": strconv.Itoa(iteration), "timeout": gate.Timeout}); err != nil {
						return false, err
					}
					return r.failWorkflowOnApprovalRejection(ctx, wf, step, iteration)
				}
			}
		}
	}

	wf.Status.Phase = v1alpha1.PhasePending
	if iteration > 0 {
		wf.Status.ActiveStep = fmt.Sprintf("%s@%d", step, iteration)
	} else {
		wf.Status.ActiveStep = step
	}
	if !hadEntered {
		if err := r.emitEvent(ctx, wf, "ApprovalRequested", fmt.Sprintf("approval required for step %q", step), map[string]string{"step": step, "iteration": strconv.Itoa(iteration)}); err != nil {
			return false, err
		}
	}
	lc := lifecycle.SetCondition(
		apiToLifecycleConditions(wf.Status.Conditions),
		lifecycle.ConditionReady,
		lifecycle.ConditionFalse,
		"WaitingApproval",
		fmt.Sprintf("approval required for step %q", step),
	)
	wf.Status.Conditions = lifecycleToAPIConditions(lc)
	return true, nil
}

func (r *WorkflowReconciler) failWorkflowOnApprovalRejection(ctx context.Context, wf *v1alpha1.Workflow, step string, iteration int) (bool, error) {
	wf.Status.Phase = v1alpha1.PhaseFailed
	if iteration > 0 {
		wf.Status.ActiveStep = fmt.Sprintf("%s@%d", step, iteration)
	} else {
		wf.Status.ActiveStep = step
	}
	lc := lifecycle.SetCondition(
		apiToLifecycleConditions(wf.Status.Conditions),
		lifecycle.ConditionReady,
		lifecycle.ConditionFalse,
		"ApprovalRejected",
		fmt.Sprintf("approval rejected for step %q", step),
	)
	wf.Status.Conditions = lifecycleToAPIConditions(lc)
	if err := r.emitEvent(ctx, wf, "ApprovalRejected", fmt.Sprintf("approval rejected for step %q", step), map[string]string{"step": step, "iteration": strconv.Itoa(iteration)}); err != nil {
		return true, err
	}
	return true, nil
}

func gateAnnotationToken(step string, iteration int) string {
	if iteration > 0 {
		return fmt.Sprintf("%s@%d", step, iteration)
	}
	return step
}

func lookupApprovalGate(wf *v1alpha1.Workflow, step string) (v1alpha1.WorkflowApprovalGate, bool) {
	for _, gate := range wf.Spec.ApprovalGates {
		if gate.To == step {
			return gate, true
		}
	}
	return v1alpha1.WorkflowApprovalGate{}, false
}

func stepRejected(wf *v1alpha1.Workflow, step string, iteration int) bool {
	if wf.ObjectMeta.Annotations == nil {
		return false
	}
	raw := strings.TrimSpace(wf.ObjectMeta.Annotations[workflowRejectedStepsKey])
	if raw == "" {
		return false
	}
	token := gateAnnotationToken(step, iteration)
	for _, v := range parseCSVTokens(raw) {
		if iteration > 0 {
			if v == token {
				return true
			}
			continue
		}
		if v == step {
			return true
		}
	}
	return false
}

func appendRejectedToken(existing, token string) string {
	tokens := parseCSVTokens(existing)
	for _, v := range tokens {
		if v == token {
			return existing
		}
	}
	tokens = append(tokens, token)
	var encoded strings.Builder
	w := csv.NewWriter(&encoded)
	if err := w.Write(tokens); err != nil {
		return existing
	}
	w.Flush()
	if w.Error() != nil {
		return existing
	}
	return strings.TrimSuffix(encoded.String(), "\n")
}

func appendApprovedToken(existing, token string) string {
	return appendRejectedToken(existing, token)
}

func cyclicFailureIgnored(wf *v1alpha1.Workflow, step string, iteration int) bool {
	if wf.ObjectMeta.Annotations == nil {
		return false
	}
	token := gateAnnotationToken(step, iteration)
	for _, value := range parseCSVTokens(wf.ObjectMeta.Annotations[workflowIgnoredFailuresKey]) {
		if value == token {
			return true
		}
	}
	return false
}

func cyclicToolCompleted(wf *v1alpha1.Workflow, step string, iteration int) bool {
	if wf.ObjectMeta.Annotations == nil {
		return false
	}
	token := gateAnnotationToken(step, iteration)
	for _, value := range parseCSVTokens(wf.ObjectMeta.Annotations[workflowCompletedToolsKey]) {
		if value == token {
			return true
		}
	}
	return false
}

// evaluateApprovalDecisions consults ApprovalDecision CRs in the workflow's
// namespace and, when a quorum of approvers or any rejection is recorded for
// the given step/iteration, writes the result into the workflow's approval
// annotations so the existing gate state machine can consume it. The CRs
// themselves serve as the durable audit log.
func (r *WorkflowReconciler) evaluateApprovalDecisions(ctx context.Context, wf *v1alpha1.Workflow, step string, iteration int, gate v1alpha1.WorkflowApprovalGate) error {
	token := gateAnnotationToken(step, iteration)
	// Skip if the gate has already been decided via annotations.
	if stepApproved(wf, step, iteration) || stepRejected(wf, step, iteration) {
		return nil
	}
	raws, err := r.store.List(ctx, "ApprovalDecision", store.ListOptions{Namespace: wf.ObjectMeta.Namespace})
	if err != nil {
		return fmt.Errorf("list approval decisions: %w", err)
	}
	if len(raws) == 0 {
		return nil
	}

	approvers := map[string]struct{}{}
	var anyDeny bool
	for _, raw := range raws {
		var d v1alpha1.ApprovalDecision
		if err := json.Unmarshal(raw, &d); err != nil {
			return fmt.Errorf("unmarshal ApprovalDecision: %w", err)
		}
		if d.Spec.WorkflowRunRef.Name != wf.ObjectMeta.Name {
			continue
		}
		if d.Spec.Step != step {
			continue
		}
		if int(d.Spec.Iteration) != iteration {
			continue
		}
		switch d.Spec.Decision {
		case v1alpha1.ApprovalDecisionDeny:
			anyDeny = true
		case v1alpha1.ApprovalDecisionApprove:
			key := d.Spec.Approver
			if key == "" {
				key = d.ObjectMeta.Name
			}
			approvers[key] = struct{}{}
		}
	}

	ensureAnnotationMap(&wf.ObjectMeta)
	if anyDeny {
		wf.ObjectMeta.Annotations[workflowRejectedStepsKey] = appendRejectedToken(wf.ObjectMeta.Annotations[workflowRejectedStepsKey], token)
		return nil
	}
	required := int(gate.RequiredApprovers)
	if required < 1 {
		required = 1
	}
	if len(approvers) >= required {
		wf.ObjectMeta.Annotations[workflowApprovedStepsKey] = appendApprovedToken(wf.ObjectMeta.Annotations[workflowApprovedStepsKey], token)
	}
	return nil
}

func stepApproved(wf *v1alpha1.Workflow, step string, iteration int) bool {
	if wf.ObjectMeta.Annotations == nil {
		return false
	}
	raw := strings.TrimSpace(wf.ObjectMeta.Annotations[workflowApprovedStepsKey])
	if raw == "" {
		return false
	}
	vals := parseCSVTokens(raw)
	iterToken := fmt.Sprintf("%s@%d", step, iteration)
	for _, v := range vals {
		if iteration > 0 {
			if v == iterToken {
				return true
			}
			continue
		}
		if v == step {
			return true
		}
	}
	return false
}

func parseCSVTokens(raw string) []string {
	r := csv.NewReader(strings.NewReader(raw))
	r.TrimLeadingSpace = true
	records, err := r.ReadAll()
	if err != nil {
		parts := strings.Split(raw, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	out := make([]string, 0)
	for _, rec := range records {
		for _, entry := range rec {
			entry = strings.TrimSpace(entry)
			if entry != "" {
				out = append(out, entry)
			}
		}
	}
	return out
}

func mergeOwnedFinalizer(latest, desired []string, owned string, removing bool) []string {
	merged := make([]string, 0, len(latest))
	for _, finalizer := range latest {
		if finalizer != owned {
			merged = append(merged, finalizer)
		}
	}
	if !removing && hasString(desired, owned) {
		merged = append(merged, owned)
	}
	return merged
}

func hasString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func ensureAnnotationMap(meta *v1alpha1.ObjectMeta) {
	if meta.Annotations == nil {
		meta.Annotations = map[string]string{}
	}
}

func currentIteration(wf *v1alpha1.Workflow) int {
	if wf.ObjectMeta.Annotations == nil {
		return 1
	}
	raw := wf.ObjectMeta.Annotations[workflowCurrentIterationKey]
	if raw == "" {
		return 1
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v < 1 {
		return 1
	}
	return v
}

func (r *WorkflowReconciler) emitEvent(ctx context.Context, wf *v1alpha1.Workflow, reason, message string, details map[string]string) error {
	detailsJSON := []byte(nil)
	if details != nil {
		detailsJSON, _ = json.Marshal(details)
	}
	return r.store.AppendEvent(ctx, r.ownerKind, wf.ObjectMeta.Namespace, wf.ObjectMeta.Name, store.InstanceEvent{
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
		Type:      "Normal",
		Reason:    reason,
		Message:   message,
		Details:   detailsJSON,
	})
}

func estimateTokens(s string) int64 {
	if strings.TrimSpace(s) == "" {
		return 0
	}
	return int64(len(strings.Fields(s)))
}

func (r *WorkflowReconciler) applyEmergentMutation(ctx context.Context, wf *v1alpha1.Workflow) (bool, error) {
	if wf.Spec.Emergent == nil || !wf.Spec.Emergent.Enabled {
		return false, nil
	}
	if wf.ObjectMeta.Annotations == nil {
		return false, nil
	}

	rawProposal := strings.TrimSpace(wf.ObjectMeta.Annotations[workflowEmergentProposalKey])
	if rawProposal == "" {
		return false, nil
	}

	var proposal workflowEmergentProposal
	if err := json.Unmarshal([]byte(rawProposal), &proposal); err != nil {
		reason := "invalid emergent proposal payload"
		if evErr := r.emitEvent(ctx, wf, "EmergentMutationRejected", reason, map[string]string{"error": err.Error()}); evErr != nil {
			return false, fmt.Errorf("append workflow event: %w", evErr)
		}
		delete(wf.ObjectMeta.Annotations, workflowEmergentProposalKey)
		if err := r.updateWorkflow(ctx, wf); err != nil {
			return false, fmt.Errorf("update workflow after malformed proposal: %w", err)
		}
		return true, nil
	}

	if len(proposal.Steps) == 0 && len(proposal.Edges) == 0 {
		if err := r.emitEvent(ctx, wf, "EmergentMutationRejected", "empty emergent proposal", map[string]string{
			"proposedBy": proposal.ProposedBy,
		}); err != nil {
			return false, fmt.Errorf("append workflow event: %w", err)
		}
		delete(wf.ObjectMeta.Annotations, workflowEmergentProposalKey)
		if err := r.updateWorkflow(ctx, wf); err != nil {
			return false, fmt.Errorf("update workflow after empty proposal: %w", err)
		}
		return true, nil
	}

	if err := validateEmergentProposal(wf, proposal); err != nil {
		if evErr := r.emitEvent(ctx, wf, "EmergentMutationRejected", "invalid emergent proposal", map[string]string{
			"proposedBy": proposal.ProposedBy,
			"error":      err.Error(),
		}); evErr != nil {
			return false, fmt.Errorf("append workflow event: %w", evErr)
		}
		delete(wf.ObjectMeta.Annotations, workflowEmergentProposalKey)
		if updateErr := r.updateWorkflow(ctx, wf); updateErr != nil {
			return false, fmt.Errorf("update workflow after invalid proposal: %w", updateErr)
		}
		return true, nil
	}

	budgets := wf.Spec.Emergent.Budgets
	stepsConsumed := annotationInt(wf.ObjectMeta.Annotations, workflowEmergentStepsConsumedKey)
	tokensConsumed := annotationInt64(wf.ObjectMeta.Annotations, workflowEmergentTokensConsumedKey)
	costConsumed := annotationFloat(wf.ObjectMeta.Annotations, workflowEmergentCostConsumedKey)

	proposalStepCount := int32(len(proposal.Steps))
	if proposalStepCount > math.MaxInt32-stepsConsumed ||
		proposal.EstimatedTokens > math.MaxInt64-tokensConsumed ||
		proposal.EstimatedCost > math.MaxFloat64-costConsumed {
		reason := "emergent budget accounting overflow"
		if err := r.emitEvent(ctx, wf, "EmergentMutationRejected", reason, map[string]string{
			"proposedBy": proposal.ProposedBy,
		}); err != nil {
			return false, fmt.Errorf("append workflow event: %w", err)
		}
		delete(wf.ObjectMeta.Annotations, workflowEmergentProposalKey)
		if err := r.updateWorkflow(ctx, wf); err != nil {
			return false, fmt.Errorf("update workflow after accounting overflow: %w", err)
		}
		return true, nil
	}
	if budgets.MaxSteps > 0 && stepsConsumed+proposalStepCount > budgets.MaxSteps {
		reason := "emergent maxSteps budget exceeded"
		if err := r.emitEvent(ctx, wf, "EmergentMutationRejected", reason, map[string]string{
			"proposedBy":    proposal.ProposedBy,
			"maxSteps":      strconv.Itoa(int(budgets.MaxSteps)),
			"stepsConsumed": strconv.Itoa(int(stepsConsumed)),
			"proposedSteps": strconv.Itoa(int(proposalStepCount)),
		}); err != nil {
			return false, fmt.Errorf("append workflow event: %w", err)
		}
		delete(wf.ObjectMeta.Annotations, workflowEmergentProposalKey)
		if err := r.updateWorkflow(ctx, wf); err != nil {
			return false, fmt.Errorf("update workflow after maxSteps rejection: %w", err)
		}
		return true, nil
	}

	if budgets.MaxTokens > 0 && tokensConsumed+proposal.EstimatedTokens > budgets.MaxTokens {
		reason := "emergent maxTokens budget exceeded"
		if err := r.emitEvent(ctx, wf, "EmergentMutationRejected", reason, map[string]string{
			"proposedBy":     proposal.ProposedBy,
			"maxTokens":      strconv.FormatInt(budgets.MaxTokens, 10),
			"tokensConsumed": strconv.FormatInt(tokensConsumed, 10),
			"proposedTokens": strconv.FormatInt(proposal.EstimatedTokens, 10),
		}); err != nil {
			return false, fmt.Errorf("append workflow event: %w", err)
		}
		delete(wf.ObjectMeta.Annotations, workflowEmergentProposalKey)
		if err := r.updateWorkflow(ctx, wf); err != nil {
			return false, fmt.Errorf("update workflow after maxTokens rejection: %w", err)
		}
		return true, nil
	}

	if maxCost := parseBudgetCost(budgets.MaxCost); maxCost > 0 && costConsumed+proposal.EstimatedCost > maxCost {
		reason := "emergent maxCost budget exceeded"
		if err := r.emitEvent(ctx, wf, "EmergentMutationRejected", reason, map[string]string{
			"proposedBy":   proposal.ProposedBy,
			"maxCost":      strconv.FormatFloat(maxCost, 'f', -1, 64),
			"costConsumed": strconv.FormatFloat(costConsumed, 'f', -1, 64),
			"proposedCost": strconv.FormatFloat(proposal.EstimatedCost, 'f', -1, 64),
		}); err != nil {
			return false, fmt.Errorf("append workflow event: %w", err)
		}
		delete(wf.ObjectMeta.Annotations, workflowEmergentProposalKey)
		if err := r.updateWorkflow(ctx, wf); err != nil {
			return false, fmt.Errorf("update workflow after maxCost rejection: %w", err)
		}
		return true, nil
	}

	wf.Spec.Steps = append(wf.Spec.Steps, proposal.Steps...)
	wf.Spec.Edges = append(wf.Spec.Edges, proposal.Edges...)

	ensureAnnotationMap(&wf.ObjectMeta)
	wf.ObjectMeta.Annotations[workflowEmergentStepsConsumedKey] = strconv.Itoa(int(stepsConsumed + proposalStepCount))
	wf.ObjectMeta.Annotations[workflowEmergentTokensConsumedKey] = strconv.FormatInt(tokensConsumed+proposal.EstimatedTokens, 10)
	wf.ObjectMeta.Annotations[workflowEmergentCostConsumedKey] = strconv.FormatFloat(costConsumed+proposal.EstimatedCost, 'f', -1, 64)
	delete(wf.ObjectMeta.Annotations, workflowEmergentProposalKey)

	if err := r.updateWorkflow(ctx, wf); err != nil {
		return false, fmt.Errorf("update workflow after emergent mutation: %w", err)
	}

	if err := r.emitEvent(ctx, wf, "EmergentMutationApplied", "emergent DAG mutation applied", map[string]string{
		"proposedBy":     proposal.ProposedBy,
		"addedSteps":     strconv.Itoa(len(proposal.Steps)),
		"addedEdges":     strconv.Itoa(len(proposal.Edges)),
		"stepsConsumed":  wf.ObjectMeta.Annotations[workflowEmergentStepsConsumedKey],
		"tokensConsumed": wf.ObjectMeta.Annotations[workflowEmergentTokensConsumedKey],
		"costConsumed":   wf.ObjectMeta.Annotations[workflowEmergentCostConsumedKey],
	}); err != nil {
		return false, fmt.Errorf("append workflow event: %w", err)
	}
	return true, nil
}

func validateEmergentProposal(wf *v1alpha1.Workflow, proposal workflowEmergentProposal) error {
	if proposal.EstimatedTokens < 0 {
		return fmt.Errorf("proposed token estimate cannot be negative")
	}
	if math.IsNaN(proposal.EstimatedCost) || math.IsInf(proposal.EstimatedCost, 0) || proposal.EstimatedCost < 0 {
		return fmt.Errorf("proposed cost estimate must be finite and non-negative")
	}

	existing := make(map[string]struct{}, len(wf.Spec.Steps))
	for _, step := range wf.Spec.Steps {
		existing[step.Name] = struct{}{}
	}

	added := make(map[string]struct{}, len(proposal.Steps))
	for _, step := range proposal.Steps {
		if strings.TrimSpace(step.Name) == "" {
			return fmt.Errorf("proposed step name cannot be empty")
		}
		if strings.TrimSpace(step.AgentRef.Name) == "" {
			return fmt.Errorf("proposed step %q has empty agentRef", step.Name)
		}
		if _, ok := existing[step.Name]; ok {
			return fmt.Errorf("proposed step %q already exists", step.Name)
		}
		if _, ok := added[step.Name]; ok {
			return fmt.Errorf("duplicate proposed step %q", step.Name)
		}
		added[step.Name] = struct{}{}
	}

	known := make(map[string]struct{}, len(existing)+len(added))
	for k := range existing {
		known[k] = struct{}{}
	}
	for k := range added {
		known[k] = struct{}{}
	}

	for _, edge := range proposal.Edges {
		if _, ok := known[edge.From]; !ok {
			return fmt.Errorf("proposed edge from unknown step %q", edge.From)
		}
		if _, ok := known[edge.To]; !ok {
			return fmt.Errorf("proposed edge to unknown step %q", edge.To)
		}
	}

	return nil
}

func annotationInt(annotations map[string]string, key string) int32 {
	if annotations == nil {
		return 0
	}
	raw := strings.TrimSpace(annotations[key])
	if raw == "" {
		return 0
	}
	v, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || v < 0 {
		return 0
	}
	return int32(v)
}

func annotationInt64(annotations map[string]string, key string) int64 {
	if annotations == nil {
		return 0
	}
	raw := strings.TrimSpace(annotations[key])
	if raw == "" {
		return 0
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v < 0 {
		return 0
	}
	return v
}

func annotationFloat(annotations map[string]string, key string) float64 {
	if annotations == nil {
		return 0
	}
	raw := strings.TrimSpace(annotations[key])
	if raw == "" {
		return 0
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		return 0
	}
	return v
}

func parseBudgetCost(v string) float64 {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return 0
	}
	return f
}
