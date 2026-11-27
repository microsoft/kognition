// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

// sanitizeK8sName converts an arbitrary step name into a DNS1123-safe segment
// usable inside a Kubernetes object metadata.name. Lowercases the input,
// replaces any character outside [a-z0-9-] with '-', collapses runs of '-',
// and trims leading/trailing '-'. Empty results are replaced with "step" so
// downstream Sprintf never produces a name ending in '-'.
//
// The original step.Name is preserved unchanged for labels, status entries,
// and Go text/template dot-notation keys — this sanitizer only affects the
// composed metadata.name of AgentJob and MemoryClaim objects derived from a
// step. Collisions between two step names that sanitize to the same segment
// are rejected at admission time (see workflow_validator.go).
func sanitizeK8sName(name string) string {
	if name == "" {
		return "step"
	}
	var b strings.Builder
	b.Grow(len(name))
	lastDash := true
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteRune('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "step"
	}
	return out
}

// workflowRunMemoryClaimName returns the deterministic MemoryClaim name
// materialized for a WorkflowRun when spec.memoryClaim is set.
func workflowRunMemoryClaimName(runName string) string {
	return runName + "-memory"
}

// workflowStepMemoryClaimName returns the deterministic MemoryClaim name
// materialized for a single step that declares its own memoryClaim.
func workflowStepMemoryClaimName(runName, stepName string) string {
	return runName + "-" + sanitizeK8sName(stepName) + "-memory"
}

// WorkflowRunReconciler reconciles WorkflowRun resources by loading the referenced
// Workflow template and delegating execution to a WorkflowReconciler configured
// in WorkflowRun mode.
type WorkflowRunReconciler struct {
	store               store.Store
	logger              *zap.Logger
	toolInvoker         WorkflowToolInvoker
	communicationPolicy CommunicationPolicyAuthorizer
	audit               *observability.CognitiveAuditLog
	tracer              *observability.Tracer
	collector           *observability.Collector
	collaborationRunner WorkflowCollaborationRunner
}

// NewWorkflowRunReconciler creates a new WorkflowRunReconciler.
func NewWorkflowRunReconciler(s store.Store, logger *zap.Logger) *WorkflowRunReconciler {
	return &WorkflowRunReconciler{
		store:  s,
		logger: logger.With(zap.String("reconciler", "workflow-run")),
	}
}

// SetToolInvoker installs the WorkflowToolInvoker used to execute tool-typed steps.
func (r *WorkflowRunReconciler) SetToolInvoker(invoker WorkflowToolInvoker) {
	r.toolInvoker = invoker
}

// SetCommunicationPolicyAuthorizer enforces communication policy for Agent
// dependencies dispatched by this WorkflowRun reconciler.
func (r *WorkflowRunReconciler) SetCommunicationPolicyAuthorizer(authorizer CommunicationPolicyAuthorizer) {
	r.communicationPolicy = authorizer
}

// SetAuditLog installs the durable recorder for workflow communication decisions.
func (r *WorkflowRunReconciler) SetAuditLog(audit *observability.CognitiveAuditLog) {
	r.audit = audit
}

// SetTracing installs the semantic tracer used for WorkflowRun root spans.
func (r *WorkflowRunReconciler) SetTracing(tracer *observability.Tracer, collector *observability.Collector) {
	r.tracer = tracer
	r.collector = collector
}

func (r *WorkflowRunReconciler) startWorkflowRunSpan(ctx context.Context, run *v1alpha1.WorkflowRun) (*observability.Span, context.Context) {
	if r.tracer == nil || r.collector == nil {
		return nil, ctx
	}
	if run.ObjectMeta.Annotations == nil {
		run.ObjectMeta.Annotations = map[string]string{}
	}
	attrs := map[string]interface{}{
		observability.AttrWorkspace:    run.ObjectMeta.Namespace,
		observability.AttrWorkflowName: run.Spec.WorkflowRef.Name,
		observability.AttrWorkflowRun:  run.ObjectMeta.Name,
		observability.AttrStatus:       string(run.Status.Phase),
	}
	if spanContext, ok := observability.SpanContextFromAnnotations(run.ObjectMeta.Annotations); ok {
		span := &observability.Span{
			Context:    spanContext,
			Name:       observability.SpanNameWorkflowRun,
			Kind:       observability.SpanKindInternal,
			StartTime:  run.ObjectMeta.CreationTimestamp,
			Attributes: attrs,
			StatusCode: observability.StatusUnset,
		}
		if span.StartTime.IsZero() {
			span.StartTime = time.Now().UTC()
		}
		return span, observability.ContextWithSpanContext(ctx, spanContext)
	}
	span := r.tracer.StartRootSpan(observability.SpanNameWorkflowRun, observability.SpanKindInternal, attrs)
	run.ObjectMeta.Annotations[observability.AnnotationTraceID] = observability.HexTraceID(span.Context.TraceID)
	run.ObjectMeta.Annotations[observability.AnnotationRootSpanID] = observability.HexSpanID(span.Context.SpanID)
	run.ObjectMeta.Annotations[observability.AnnotationSpanID] = observability.HexSpanID(span.Context.SpanID)
	run.ObjectMeta.Annotations[observability.AnnotationTraceparent] = observability.Traceparent(
		run.ObjectMeta.Annotations[observability.AnnotationTraceID],
		run.ObjectMeta.Annotations[observability.AnnotationRootSpanID],
	)
	run.ObjectMeta.Annotations[observability.AnnotationWorkflowRun] = run.ObjectMeta.Name
	if run.Spec.WorkflowRef.Name != "" {
		run.ObjectMeta.Annotations[observability.AnnotationWorkflowName] = run.Spec.WorkflowRef.Name
	}
	return span, observability.ContextWithSpanContext(ctx, span.Context)
}

func (r *WorkflowRunReconciler) endWorkflowRunSpan(ctx context.Context, span *observability.Span, run *v1alpha1.WorkflowRun, status observability.StatusCode, statusMsg string) {
	if r.tracer == nil || r.collector == nil || span == nil {
		return
	}
	if span.Attributes == nil {
		span.Attributes = map[string]interface{}{}
	}
	span.Attributes[observability.AttrStatus] = string(run.Status.Phase)
	span.Attributes["kognition.workflow.completed_steps"] = len(run.Status.CompletedSteps)
	span.Attributes["kognition.workflow.active_steps"] = len(run.Status.ActiveSteps)
	if err := r.tracer.EndSpan(span, status, statusMsg); err != nil {
		r.logger.Warn("end workflow run span failed", zap.String("span", span.Name), zap.Error(err))
	}
	if span.Context.TraceFlags&0x01 == 0 {
		return
	}
	if err := r.collector.FlushSpans(ctx, []*observability.Span{span}); err != nil {
		r.logger.Warn("export workflow run span failed", zap.String("span", span.Name), zap.Error(err))
	}
}

// Reconcile implements the Reconciler interface for WorkflowRun resources.
func (r *WorkflowRunReconciler) Reconcile(ctx context.Context, req Request) (Result, error) {
	log := r.logger.With(zap.String("namespace", req.Namespace), zap.String("name", req.Name), requestid.ZapField(ctx))

	raw, err := r.store.Get(ctx, "WorkflowRun", req.Namespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			log.Debug("workflow run not found, likely deleted")
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("get workflow run: %w", err)
	}

	var run v1alpha1.WorkflowRun
	if err := json.Unmarshal(raw, &run); err != nil {
		return Result{}, fmt.Errorf("unmarshal workflow run: %w", err)
	}

	if run.ObjectMeta.DeletionTimestamp != nil {
		return r.handleDeletion(ctx, &run, log)
	}
	if run.ObjectMeta.Annotations[runtimeapproval.WorkflowRunAcceptanceRequiredAnnotation] == "true" {
		return Result{}, runtimeapproval.ErrWorkflowDispatchUnsupported
	}

	// Terminal phases – nothing left to reconcile.
	if run.Status.Phase == v1alpha1.PhaseReady || run.Status.Phase == v1alpha1.PhaseFailed {
		if err := r.reconcileTerminalMetrics(ctx, &run, log); err != nil {
			return Result{}, err
		}
		r.applyMemoryRetention(ctx, &run, log)
		return Result{}, nil
	}

	if !lifecycle.HasFinalizer(run.ObjectMeta.Finalizers, WorkflowRunFinalizer) {
		originalStatus, originalAnnotations, originalFinalizers := snapshotWorkflowRun(&run)
		finalizers, _ := lifecycle.AddFinalizer(run.ObjectMeta.Finalizers, WorkflowRunFinalizer)
		run.ObjectMeta.Finalizers = finalizers
		if err := r.updateRun(ctx, &run, originalStatus, originalAnnotations, originalFinalizers); err != nil {
			return Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		return Result{Requeue: true}, nil
	}

	if run.ObjectMeta.Annotations == nil {
		run.ObjectMeta.Annotations = map[string]string{}
	}
	if run.ObjectMeta.Annotations[observability.AnnotationMetricsStarted] != "true" {
		originalStatus, originalAnnotations, originalFinalizers := snapshotWorkflowRun(&run)
		run.ObjectMeta.Annotations[observability.AnnotationMetricsStarted] = "true"
		if run.Status.StartTime == nil {
			now := time.Now().UTC()
			run.Status.StartTime = &now
		}
		if err := r.updateRun(ctx, &run, originalStatus, originalAnnotations, originalFinalizers); err != nil {
			delete(run.ObjectMeta.Annotations, observability.AnnotationMetricsStarted)
			return Result{}, fmt.Errorf("persist metrics-started annotation: %w", err)
		}
	}

	// Resolve the workflow spec: prefer snapshot, then load from template.
	spec, err := r.resolveSpec(ctx, &run, log)
	if err != nil {
		return Result{}, err
	}

	// Merge parameters: template defaults overridden by run-level params.
	mergedParams := make(map[string]string)
	for k, v := range spec.Parameters {
		mergedParams[k] = v
	}
	for k, v := range run.Spec.Parameters {
		mergedParams[k] = v
	}
	spec.Parameters = mergedParams

	// Check maxConcurrentRuns.
	if spec.MaxConcurrentRuns > 0 {
		queued, err := r.checkConcurrency(ctx, &run, spec.MaxConcurrentRuns)
		if err != nil {
			return Result{}, err
		}
		if queued {
			// Nothing re-enqueues a queued run when the blocking run finishes.
			return Result{RequeueAfter: queuedRunRecheckInterval}, nil
		}
	}

	// Build a synthetic Workflow for the execution engine.
	wf := r.buildSyntheticWorkflow(&run, spec)

	// Materialize the workflow-scoped MemoryClaim (T-0301) before any step runs.
	if spec.MemoryClaim != nil {
		if err := r.ensureWorkflowMemoryClaim(ctx, &run, spec.MemoryClaim, log); err != nil {
			return Result{}, fmt.Errorf("ensure workflow memory claim: %w", err)
		}
	}
	// Materialize per-step MemoryClaims (T-0304).
	if err := r.ensureStepMemoryClaims(ctx, &run, spec, log); err != nil {
		return Result{}, fmt.Errorf("ensure step memory claims: %w", err)
	}

	traceAlreadyPersisted := false
	if _, ok := observability.SpanContextFromAnnotations(run.ObjectMeta.Annotations); ok {
		traceAlreadyPersisted = true
	}
	persistedStatus, persistedAnnotations, persistedFinalizers := snapshotWorkflowRun(&run)
	runSpan, runCtx := r.startWorkflowRunSpan(ctx, &run)
	// startWorkflowRunSpan may allocate the annotations map after the synthetic
	// workflow was built, so keep the execution object on the same trace context.
	wf.ObjectMeta.Annotations = run.ObjectMeta.Annotations
	if !traceAlreadyPersisted && runSpan != nil {
		if err := r.updateRun(ctx, &run, persistedStatus, persistedAnnotations, persistedFinalizers); err != nil {
			return Result{}, fmt.Errorf("persist workflow run trace annotations: %w", err)
		}
		persistedStatus, persistedAnnotations, persistedFinalizers = snapshotWorkflowRun(&run)
		wf.ObjectMeta.Annotations = run.ObjectMeta.Annotations
	}

	// Create a WorkflowReconciler in run mode.
	inner := &WorkflowReconciler{
		store:               r.store,
		logger:              r.logger,
		ownerKind:           "WorkflowRun",
		workflowInput:       run.Spec.Input,
		toolInvoker:         r.toolInvoker,
		collaborationRunner: r.collaborationRunner,
		communicationPolicy: r.communicationPolicy,
		audit:               r.audit,
		tracer:              r.tracer,
		collector:           r.collector,
	}
	inner.persistStatus = func(ctx context.Context, wf *v1alpha1.Workflow) error {
		// Map synthetic workflow status back to the run.
		run.Status.Phase = wf.Status.Phase
		run.Status.Conditions = wf.Status.Conditions
		run.Status.CompletedSteps = wf.Status.CompletedSteps
		run.Status.ActiveStep = wf.Status.ActiveStep
		run.Status.ActiveSteps = wf.Status.ActiveSteps
		run.Status.StepStatuses = wf.Status.StepStatuses
		if wf.Status.Phase == v1alpha1.PhaseReady {
			run.Status.Output = workflowRunOutput(wf.Spec, wf.Status.StepStatuses)
		} else {
			run.Status.Output = ""
		}
		// Preserve ObjectMeta changes (e.g. annotations, finalizers).
		run.ObjectMeta.Annotations = wf.ObjectMeta.Annotations
		run.ObjectMeta.Finalizers = wf.ObjectMeta.Finalizers
		if err := r.updateRun(ctx, &run, persistedStatus, persistedAnnotations, persistedFinalizers); err != nil {
			return err
		}
		persistedStatus, persistedAnnotations, persistedFinalizers = snapshotWorkflowRun(&run)
		// Sync back the refreshed ResourceVersion to the synthetic workflow.
		wf.ObjectMeta.ResourceVersion = run.ObjectMeta.ResourceVersion
		return nil
	}

	res, err := inner.executeWorkflow(runCtx, wf)
	if err != nil {
		if run.Status.Phase == v1alpha1.PhaseFailed {
			r.endWorkflowRunSpan(ctx, runSpan, &run, observability.StatusError, err.Error())
		}
		return res, err
	}

	// After terminal completion, enforce history limit.

	spanStatus := observability.StatusOK
	spanStatusMsg := ""
	if run.Status.Phase == v1alpha1.PhaseFailed {
		spanStatus = observability.StatusError
		spanStatusMsg = "workflow run failed"
	}
	if run.Status.Phase == v1alpha1.PhaseReady || run.Status.Phase == v1alpha1.PhaseFailed {
		r.endWorkflowRunSpan(ctx, runSpan, &run, spanStatus, spanStatusMsg)
		if err := r.reconcileTerminalMetrics(ctx, &run, log); err != nil {
			return Result{}, err
		}
		if spec.HistoryLimit > 0 {
			r.enforceHistoryLimit(ctx, &run, spec.HistoryLimit, log)
		}
		r.applyMemoryRetention(ctx, &run, log)
	}

	return res, nil
}

func (r *WorkflowRunReconciler) reconcileTerminalMetrics(ctx context.Context, run *v1alpha1.WorkflowRun, log *zap.Logger) error {
	if run.ObjectMeta.Annotations[observability.AnnotationMetricsCompleted] == "true" {
		return nil
	}
	originalStatus, originalAnnotations, originalFinalizers := snapshotWorkflowRun(run)
	if run.ObjectMeta.Annotations == nil {
		run.ObjectMeta.Annotations = map[string]string{}
	}
	run.ObjectMeta.Annotations[observability.AnnotationMetricsCompleted] = "true"
	if err := r.updateRun(ctx, run, originalStatus, originalAnnotations, originalFinalizers); err != nil {
		delete(run.ObjectMeta.Annotations, observability.AnnotationMetricsCompleted)
		return fmt.Errorf("persist metrics-completed annotation: %w", err)
	}
	return nil
}

// resolveSpec returns the WorkflowSpec to use for execution.
// It snapshots the template spec on first execution and uses the snapshot thereafter.
func (r *WorkflowRunReconciler) resolveSpec(ctx context.Context, run *v1alpha1.WorkflowRun, log *zap.Logger) (*v1alpha1.WorkflowSpec, error) {
	if run.Status.SnapshotSpec != nil {
		return run.Status.SnapshotSpec, nil
	}

	// Load template.
	raw, err := r.store.Get(ctx, "Workflow", run.ObjectMeta.Namespace, run.Spec.WorkflowRef.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			originalStatus, originalAnnotations, originalFinalizers := snapshotWorkflowRun(run)
			run.Status.Phase = v1alpha1.PhaseFailed
			run.Status.Conditions = lifecycleToAPIConditions(lifecycle.SetCondition(
				apiToLifecycleConditions(run.Status.Conditions),
				lifecycle.ConditionReady,
				lifecycle.ConditionFalse,
				"TemplateNotFound",
				fmt.Sprintf("Workflow %q not found", run.Spec.WorkflowRef.Name),
			))
			if updateErr := r.updateRun(ctx, run, originalStatus, originalAnnotations, originalFinalizers); updateErr != nil {
				return nil, fmt.Errorf("update run after template not found: %w", updateErr)
			}
			return nil, fmt.Errorf("workflow template %q not found", run.Spec.WorkflowRef.Name)
		}
		return nil, fmt.Errorf("get workflow template: %w", err)
	}

	var wf v1alpha1.Workflow
	if err := json.Unmarshal(raw, &wf); err != nil {
		return nil, fmt.Errorf("unmarshal workflow template: %w", err)
	}

	// Snapshot the spec.
	originalStatus, originalAnnotations, originalFinalizers := snapshotWorkflowRun(run)
	spec := wf.Spec
	run.Status.SnapshotSpec = &spec
	if err := r.updateRun(ctx, run, originalStatus, originalAnnotations, originalFinalizers); err != nil {
		return nil, fmt.Errorf("snapshot spec: %w", err)
	}
	log.Info("snapshotted workflow spec", zap.String("workflow", run.Spec.WorkflowRef.Name))

	return &spec, nil
}

// queuedRunRecheckInterval is how often a run held back by maxConcurrentRuns retries.
const queuedRunRecheckInterval = 5 * time.Second

// checkConcurrency returns true if the run should be queued (not started yet).
func (r *WorkflowRunReconciler) checkConcurrency(ctx context.Context, run *v1alpha1.WorkflowRun, maxConcurrent int32) (bool, error) {
	rawList, err := r.store.List(ctx, "WorkflowRun", store.ListOptions{Namespace: run.ObjectMeta.Namespace})
	if err != nil {
		return false, fmt.Errorf("list workflow runs for concurrency check: %w", err)
	}

	var activeCount int32
	for _, raw := range rawList {
		var other v1alpha1.WorkflowRun
		if err := json.Unmarshal(raw, &other); err != nil {
			continue
		}
		if other.Spec.WorkflowRef.Name != run.Spec.WorkflowRef.Name {
			continue
		}
		if other.ObjectMeta.Name == run.ObjectMeta.Name {
			continue
		}
		if other.Status.Phase == v1alpha1.PhaseRunning {
			activeCount++
		}
	}

	if activeCount >= maxConcurrent {
		if run.Status.Phase != v1alpha1.PhasePending {
			originalStatus, originalAnnotations, originalFinalizers := snapshotWorkflowRun(run)
			run.Status.Phase = v1alpha1.PhasePending
			run.Status.Conditions = lifecycleToAPIConditions(lifecycle.SetCondition(
				apiToLifecycleConditions(run.Status.Conditions),
				lifecycle.ConditionReady,
				lifecycle.ConditionFalse,
				"ConcurrencyLimitReached",
				fmt.Sprintf("max concurrent runs (%d) reached", maxConcurrent),
			))
			if err := r.updateRun(ctx, run, originalStatus, originalAnnotations, originalFinalizers); err != nil {
				return false, fmt.Errorf("update run to pending: %w", err)
			}
		}
		return true, nil
	}
	return false, nil
}

// ensureWorkflowMemoryClaim materializes the workflow-scoped MemoryClaim
// on first reconcile. It is idempotent — subsequent calls see ErrAlreadyExists
// and return nil.
func (r *WorkflowRunReconciler) ensureWorkflowMemoryClaim(ctx context.Context, run *v1alpha1.WorkflowRun, claim *v1alpha1.WorkflowMemoryClaim, log *zap.Logger) error {
	name := workflowRunMemoryClaimName(run.ObjectMeta.Name)
	return r.materializeMemoryClaim(ctx, run, name, claim, log)
}

// ensureStepMemoryClaims materializes per-step MemoryClaims for any step that
// declares its own claim. Step claims override the workflow-level claim for
// that step's AgentJob.
func (r *WorkflowRunReconciler) ensureStepMemoryClaims(ctx context.Context, run *v1alpha1.WorkflowRun, spec *v1alpha1.WorkflowSpec, log *zap.Logger) error {
	for _, step := range spec.Steps {
		if step.MemoryClaim == nil {
			continue
		}
		name := workflowStepMemoryClaimName(run.ObjectMeta.Name, step.Name)
		if err := r.materializeMemoryClaim(ctx, run, name, step.MemoryClaim, log); err != nil {
			return fmt.Errorf("step %q: %w", step.Name, err)
		}
	}
	return nil
}

// materializeMemoryClaim writes a MemoryClaim object owned by the run. It is
// idempotent: ErrAlreadyExists is treated as success.
func (r *WorkflowRunReconciler) materializeMemoryClaim(ctx context.Context, run *v1alpha1.WorkflowRun, name string, claim *v1alpha1.WorkflowMemoryClaim, log *zap.Logger) error {
	ns := run.ObjectMeta.Namespace

	accessModes := []string{}
	if claim.AccessMode != "" {
		accessModes = append(accessModes, claim.AccessMode)
	}

	mc := v1alpha1.MemoryClaim{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "MemoryClaim"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			UID:               uuid.New().String(),
			CreationTimestamp: time.Now().UTC(),
			OwnerReferences: []v1alpha1.OwnerReference{{
				APIVersion: v1alpha1.APIVersion,
				Kind:       "WorkflowRun",
				Name:       run.ObjectMeta.Name,
				UID:        run.ObjectMeta.UID,
			}},
		},
		Spec: v1alpha1.MemoryClaimSpec{
			MemoryClassName: claim.MemoryClassName,
			Type:            v1alpha1.MemoryTypeShared,
			AccessModes:     accessModes,
		},
	}

	data, err := json.Marshal(mc)
	if err != nil {
		return fmt.Errorf("marshal memory claim: %w", err)
	}
	if err := r.store.Create(ctx, "MemoryClaim", ns, name, data); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			return nil
		}
		return err
	}
	log.Info("materialized memory claim", zap.String("claim", name), zap.String("memoryClass", claim.MemoryClassName))
	return nil
}

// applyMemoryRetention removes claims owned by a terminal run. Cleanup remains
// best effort so terminal status is stable, and subsequent reconciles retry it.
func (r *WorkflowRunReconciler) applyMemoryRetention(ctx context.Context, run *v1alpha1.WorkflowRun, log *zap.Logger) {
	if err := r.cleanupMemoryClaims(ctx, run); err != nil {
		log.Warn("clean up retained memory claims failed", zap.Error(err))
	}
}

func (r *WorkflowRunReconciler) cleanupMemoryClaims(ctx context.Context, run *v1alpha1.WorkflowRun) error {
	spec := run.Status.SnapshotSpec
	if spec == nil || spec.MemoryRetentionPolicy == v1alpha1.WorkflowMemoryRetentionRetain {
		return nil
	}
	rawClaims, err := r.store.List(ctx, "MemoryClaim", store.ListOptions{Namespace: run.ObjectMeta.Namespace})
	if err != nil {
		return fmt.Errorf("list memory claims: %w", err)
	}
	for _, raw := range rawClaims {
		var claim v1alpha1.MemoryClaim
		if err := json.Unmarshal(raw, &claim); err != nil {
			continue
		}
		if !workflowRunOwns(claim.ObjectMeta.OwnerReferences, run) {
			continue
		}
		if err := r.store.Delete(ctx, "MemoryClaim", claim.ObjectMeta.Namespace, claim.ObjectMeta.Name); err != nil && !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("delete memory claim %q: %w", claim.ObjectMeta.Name, err)
		}
	}
	return nil
}

func workflowRunOwns(refs []v1alpha1.OwnerReference, run *v1alpha1.WorkflowRun) bool {
	for _, ref := range refs {
		if ref.Kind == "WorkflowRun" && ref.Name == run.ObjectMeta.Name && ref.UID == run.ObjectMeta.UID {
			return true
		}
	}
	return false
}

// buildSyntheticWorkflow creates a Workflow object from the run for the execution engine.
func (r *WorkflowRunReconciler) buildSyntheticWorkflow(run *v1alpha1.WorkflowRun, spec *v1alpha1.WorkflowSpec) *v1alpha1.Workflow {
	return &v1alpha1.Workflow{
		TypeMeta: v1alpha1.TypeMeta{Kind: "WorkflowRun", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      run.ObjectMeta.Name,
			Namespace: run.ObjectMeta.Namespace,
			UID:       run.ObjectMeta.UID,
			// Annotations alias the run's map by reference so the workflow reconciler can write back trace-context updates in-place.
			Annotations:     run.ObjectMeta.Annotations,
			Finalizers:      run.ObjectMeta.Finalizers,
			ResourceVersion: run.ObjectMeta.ResourceVersion,
		},
		Spec: *spec,
		Status: v1alpha1.WorkflowStatus{
			Phase:          run.Status.Phase,
			Conditions:     run.Status.Conditions,
			CompletedSteps: run.Status.CompletedSteps,
			ActiveStep:     run.Status.ActiveStep,
			ActiveSteps:    run.Status.ActiveSteps,
			StepStatuses:   run.Status.StepStatuses,
		},
	}
}

func workflowRunOutput(spec v1alpha1.WorkflowSpec, statuses []v1alpha1.WorkflowStepStatus) string {
	outputs := make(map[string]string, len(statuses))
	for _, status := range statuses {
		if status.Output != "" {
			outputs[status.Name] = status.Output
		}
	}
	if len(outputs) == 0 {
		return ""
	}

	hasOutgoing := make(map[string]bool, len(spec.Edges))
	for _, edge := range spec.Edges {
		hasOutgoing[edge.From] = true
	}
	for i := len(spec.Steps) - 1; i >= 0; i-- {
		step := spec.Steps[i]
		if hasOutgoing[step.Name] {
			continue
		}
		if output := outputs[step.Name]; output != "" {
			return output
		}
	}
	for i := len(statuses) - 1; i >= 0; i-- {
		if statuses[i].Output != "" {
			return statuses[i].Output
		}
	}
	return ""
}

// handleDeletion cleans up owned jobs and removes the finalizer.
func (r *WorkflowRunReconciler) handleDeletion(ctx context.Context, run *v1alpha1.WorkflowRun, log *zap.Logger) (Result, error) {
	if !lifecycle.HasFinalizer(run.ObjectMeta.Finalizers, WorkflowRunFinalizer) {
		return Result{}, nil
	}
	if r.collaborationRunner != nil && run.Status.Phase != v1alpha1.PhaseReady && run.Status.Phase != v1alpha1.PhaseFailed {
		spec, err := r.resolveSpec(ctx, run, log)
		if err != nil {
			log.Warn("collaboration steps not stopped: workflow spec unavailable")
		} else if err := stopWorkflowCollaborations(ctx, r.collaborationRunner, r.buildSyntheticWorkflow(run, spec), "WorkflowRun", "WorkflowRunDeleted"); err != nil {
			return Result{}, err
		}
	}
	if run.Status.Phase != v1alpha1.PhaseReady && run.Status.Phase != v1alpha1.PhaseFailed {
		originalStatus, originalAnnotations, originalFinalizers := snapshotWorkflowRun(run)
		run.Status.Phase = v1alpha1.PhaseFailed
		if run.ObjectMeta.Annotations == nil {
			run.ObjectMeta.Annotations = map[string]string{}
		}
		run.ObjectMeta.Annotations[observability.AnnotationMetricsCompleted] = "true"
		if err := r.updateRun(ctx, run, originalStatus, originalAnnotations, originalFinalizers); err != nil {
			return Result{}, fmt.Errorf("persist deletion metrics: %w", err)
		}
	}
	// Delete owned jobs.
	rawJobs, err := r.store.List(ctx, "AgentJob", store.ListOptions{Namespace: run.ObjectMeta.Namespace})
	if err != nil {
		return Result{}, fmt.Errorf("list jobs for cleanup: %w", err)
	}
	for _, raw := range rawJobs {
		var job v1alpha1.AgentJob
		if err := json.Unmarshal(raw, &job); err != nil {
			continue
		}
		if workflowRunOwns(job.ObjectMeta.OwnerReferences, run) {
			if delErr := r.store.Delete(ctx, "AgentJob", job.ObjectMeta.Namespace, job.ObjectMeta.Name); delErr != nil && !errors.Is(delErr, store.ErrNotFound) {
				return Result{}, fmt.Errorf("delete owned job %q: %w", job.ObjectMeta.Name, delErr)
			}
		}
	}
	if err := r.cleanupMemoryClaims(ctx, run); err != nil {
		return Result{}, fmt.Errorf("clean up memory claims: %w", err)
	}

	originalStatus, originalAnnotations, originalFinalizers := snapshotWorkflowRun(run)
	finalizers, _ := lifecycle.RemoveFinalizer(run.ObjectMeta.Finalizers, WorkflowRunFinalizer)
	run.ObjectMeta.Finalizers = finalizers
	if err := r.updateRun(ctx, run, originalStatus, originalAnnotations, originalFinalizers); err != nil {
		return Result{}, fmt.Errorf("remove finalizer: %w", err)
	}
	return Result{}, nil
}

// enforceHistoryLimit garbage-collects the oldest completed runs beyond the limit.
func (r *WorkflowRunReconciler) enforceHistoryLimit(ctx context.Context, current *v1alpha1.WorkflowRun, limit int32, log *zap.Logger) {
	rawList, err := r.store.List(ctx, "WorkflowRun", store.ListOptions{Namespace: current.ObjectMeta.Namespace})
	if err != nil {
		log.Error("list runs for history GC", zap.Error(err))
		return
	}

	type completedRun struct {
		name        string
		completedAt time.Time
	}
	var completed []completedRun
	for _, raw := range rawList {
		var other v1alpha1.WorkflowRun
		if err := json.Unmarshal(raw, &other); err != nil {
			continue
		}
		if other.Spec.WorkflowRef.Name != current.Spec.WorkflowRef.Name {
			continue
		}
		if other.Status.Phase == v1alpha1.PhaseReady || other.Status.Phase == v1alpha1.PhaseFailed {
			// Prefer the latest terminal condition's transition time so that
			// GC order reflects completion order, not creation order.
			ts := other.ObjectMeta.CreationTimestamp
			for _, c := range other.Status.Conditions {
				if c.LastTransitionTime.After(ts) {
					ts = c.LastTransitionTime
				}
			}
			completed = append(completed, completedRun{name: other.ObjectMeta.Name, completedAt: ts})
		}
	}

	if int32(len(completed)) <= limit {
		return
	}

	// Sort oldest first.
	sort.Slice(completed, func(i, j int) bool {
		return completed[i].completedAt.Before(completed[j].completedAt)
	})

	excess := int32(len(completed)) - limit
	var deleted int32
	for i := 0; i < len(completed) && deleted < excess; i++ {
		// Never delete the run that just completed; it is counted toward the
		// limit but must not be removed mid-reconcile.
		if completed[i].name == current.ObjectMeta.Name {
			continue
		}
		if err := r.store.Delete(ctx, "WorkflowRun", current.ObjectMeta.Namespace, completed[i].name); err != nil && !errors.Is(err, store.ErrNotFound) {
			log.Error("failed to GC workflow run", zap.String("run", completed[i].name), zap.Error(err))
		} else {
			log.Info("garbage-collected workflow run", zap.String("run", completed[i].name))
		}
		deleted++
	}
}

// updateRun persists the WorkflowRun to the store and refreshes the in-memory ResourceVersion.
func (r *WorkflowRunReconciler) updateRun(ctx context.Context, run *v1alpha1.WorkflowRun, originalStatus v1alpha1.WorkflowRunStatus, originalAnnotations map[string]string, originalFinalizers []string) error {
	const maxRetries = 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		if (run.Status.Phase == v1alpha1.PhaseReady || run.Status.Phase == v1alpha1.PhaseFailed) &&
			originalStatus.Phase != v1alpha1.PhaseReady && originalStatus.Phase != v1alpha1.PhaseFailed &&
			run.Status.CompletionTime == nil && attempt == 0 {
			now := time.Now().UTC()
			run.Status.CompletionTime = &now
		}
		data, err := json.Marshal(run)
		if err != nil {
			return fmt.Errorf("marshal workflow run: %w", err)
		}
		err = r.store.Update(ctx, "WorkflowRun", run.ObjectMeta.Namespace, run.ObjectMeta.Name, data, run.ObjectMeta.ResourceVersion)
		if err == nil {
			raw, getErr := r.store.Get(ctx, "WorkflowRun", run.ObjectMeta.Namespace, run.ObjectMeta.Name)
			if getErr != nil {
				return fmt.Errorf("re-read workflow run after update: %w", getErr)
			}
			return json.Unmarshal(raw, run)
		}
		if !errors.Is(err, store.ErrConflict) {
			return err
		}

		raw, getErr := r.store.Get(ctx, "WorkflowRun", run.ObjectMeta.Namespace, run.ObjectMeta.Name)
		if getErr != nil {
			return fmt.Errorf("re-read workflow run after conflict: %w", getErr)
		}
		var latest v1alpha1.WorkflowRun
		if getErr := json.Unmarshal(raw, &latest); getErr != nil {
			return fmt.Errorf("unmarshal workflow run after conflict: %w", getErr)
		}
		latestTerminal := latest.Status.Phase == v1alpha1.PhaseReady || latest.Status.Phase == v1alpha1.PhaseFailed
		desiredTerminal := run.Status.Phase == v1alpha1.PhaseReady || run.Status.Phase == v1alpha1.PhaseFailed
		if !latestTerminal || desiredTerminal {
			mergeWorkflowRunStatus(&latest.Status, originalStatus, run.Status)
		}
		latest.ObjectMeta.Annotations = mergeWorkflowRunAnnotations(latest.ObjectMeta.Annotations, originalAnnotations, run.ObjectMeta.Annotations)
		originalHasFinalizer := hasString(originalFinalizers, WorkflowRunFinalizer)
		desiredHasFinalizer := hasString(run.ObjectMeta.Finalizers, WorkflowRunFinalizer)
		if originalHasFinalizer != desiredHasFinalizer {
			latest.ObjectMeta.Finalizers = mergeOwnedFinalizer(latest.ObjectMeta.Finalizers, run.ObjectMeta.Finalizers, WorkflowRunFinalizer, !desiredHasFinalizer)
		}
		*run = latest
	}
	return store.ErrConflict
}

func snapshotWorkflowRun(run *v1alpha1.WorkflowRun) (v1alpha1.WorkflowRunStatus, map[string]string, []string) {
	return run.Status, cloneStringMap(run.ObjectMeta.Annotations), append([]string(nil), run.ObjectMeta.Finalizers...)
}

func mergeWorkflowRunStatus(latest *v1alpha1.WorkflowRunStatus, original, desired v1alpha1.WorkflowRunStatus) {
	if latest.StartTime == nil {
		latest.StartTime = desired.StartTime
	}
	if latest.CompletionTime == nil {
		latest.CompletionTime = desired.CompletionTime
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
	if desired.Output != original.Output {
		latest.Output = desired.Output
	}
	if !reflect.DeepEqual(desired.SnapshotSpec, original.SnapshotSpec) {
		latest.SnapshotSpec = desired.SnapshotSpec
	}
	if !reflect.DeepEqual(desired.ApprovalRecords, original.ApprovalRecords) {
		latest.ApprovalRecords = desired.ApprovalRecords
	}
	conditionType := string(lifecycle.ConditionReady)
	originalCondition := restartLadderCondition(original.Conditions, conditionType)
	desiredCondition := restartLadderCondition(desired.Conditions, conditionType)
	if !conditionsMatch(originalCondition, desiredCondition) {
		latest.Conditions = applyConditionChange(latest.Conditions, conditionType, desiredCondition)
	}
}

func mergeWorkflowRunAnnotations(latest, original, desired map[string]string) map[string]string {
	merged := cloneStringMap(latest)
	if merged == nil && len(desired) > 0 {
		merged = make(map[string]string)
	}
	for key, originalValue := range original {
		desiredValue, exists := desired[key]
		if !exists {
			delete(merged, key)
		} else if desiredValue != originalValue {
			merged[key] = desiredValue
		}
	}
	for key, desiredValue := range desired {
		if _, existed := original[key]; !existed {
			merged[key] = desiredValue
		}
	}
	return merged
}
