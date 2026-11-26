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
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	v1alpha1 "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

const (
	SolutionFinalizer = "kognition.dev/solution-cleanup"
	solutionKind      = "Solution"
)

// SolutionReconciler implements the planner-controller for Solutions.
// It compiles a goal into child resources, creates revisions, and deploys them.
type SolutionReconciler struct {
	store  store.Store
	logger *zap.Logger
}

// NewSolutionReconciler creates a new SolutionReconciler.
func NewSolutionReconciler(s store.Store, logger *zap.Logger) *SolutionReconciler {
	return &SolutionReconciler{store: s, logger: logger}
}

// Reconcile handles a single reconcile loop for a Solution resource.
func (r *SolutionReconciler) Reconcile(ctx context.Context, req Request) (Result, error) {
	log := r.logger.With(zap.String("solution", req.Namespace+"/"+req.Name), requestid.ZapField(ctx))

	data, err := r.store.Get(ctx, solutionKind, req.Namespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			log.Debug("solution not found, skipping")
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("get solution: %w", err)
	}

	var solution v1alpha1.Solution
	if err := json.Unmarshal(data, &solution); err != nil {
		return Result{}, fmt.Errorf("unmarshal solution: %w", err)
	}
	originalStatus := snapshotSolutionStatus(solution.Status)
	originalFinalizers := append([]string(nil), solution.ObjectMeta.Finalizers...)

	// Handle deletion.
	if solution.ObjectMeta.DeletionTimestamp != nil {
		return r.handleDeletion(ctx, &solution, originalStatus, originalFinalizers, log)
	}

	// Ensure finalizer.
	if updated, added := lifecycle.AddFinalizer(solution.ObjectMeta.Finalizers, SolutionFinalizer); added {
		solution.ObjectMeta.Finalizers = updated
		if err := r.updateSolution(ctx, &solution, originalStatus, originalFinalizers); err != nil {
			return Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		return Result{Requeue: true}, nil
	}

	// Resolve workspace quota for inheritance.
	workspaceQuota, err := r.resolveWorkspaceQuota(ctx, solution.ObjectMeta.Namespace)
	if err != nil {
		log.Warn("failed to resolve workspace quota, proceeding without", zap.Error(err))
	}

	// Compile the goal and detect changes.
	compiled, specHash, err := r.compileGoal(ctx, &solution, workspaceQuota)
	if err != nil {
		return r.setFailedStatus(ctx, &solution, originalStatus, originalFinalizers, "CompilationFailed", err.Error(), log)
	}

	// Check if a new revision is needed by comparing spec hashes.
	needsRevision, currentRevision, err := r.detectGoalChange(ctx, &solution, specHash)
	if err != nil {
		return Result{}, fmt.Errorf("detect goal change: %w", err)
	}

	if needsRevision {
		currentRevision, err = r.createRevision(ctx, &solution, compiled, specHash, currentRevision)
		if err != nil {
			return r.setFailedStatus(ctx, &solution, originalStatus, originalFinalizers, "RevisionFailed", err.Error(), log)
		}
		log.Info("created new revision", zap.Int64("revision", currentRevision))

	}

	if currentRevision > solution.Status.ActiveRevision {
		passed, reason, message, err := r.reconcilePromotionGates(ctx, &solution, currentRevision)
		if err != nil {
			return r.setFailedStatus(ctx, &solution, originalStatus, originalFinalizers, "PromotionGateError", err.Error(), log)
		}
		if !passed {
			return r.updatePromotionBlockedStatus(ctx, &solution, originalStatus, originalFinalizers, currentRevision, reason, message)
		}
		if err := r.deployChildResources(ctx, &solution, compiled); err != nil {
			return r.setFailedStatus(ctx, &solution, originalStatus, originalFinalizers, "DeployFailed", err.Error(), log)
		}
	}

	// Update status.
	return r.updateReadyStatus(ctx, &solution, originalStatus, originalFinalizers, compiled, currentRevision, log)
}

func (r *SolutionReconciler) reconcilePromotionGates(ctx context.Context, solution *v1alpha1.Solution, revision int64) (bool, string, string, error) {
	if len(solution.Spec.PromotionGates) == 0 {
		return true, "", "", nil
	}
	pending := make([]string, 0)
	failed := make([]string, 0)
	seen := make(map[string]struct{}, len(solution.Spec.PromotionGates))
	for _, gate := range solution.Spec.PromotionGates {
		name := gate.EvaluationRef.Name
		if name == "" {
			return false, "PromotionGateInvalid", "promotion gate evaluationRef.name is required", nil
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		raw, err := r.store.Get(ctx, evaluationKind, solution.ObjectMeta.Namespace, name)
		if errors.Is(err, store.ErrNotFound) {
			failed = append(failed, fmt.Sprintf("Evaluation %q was not found", name))
			continue
		}
		if err != nil {
			return false, "", "", fmt.Errorf("get promotion Evaluation %q: %w", name, err)
		}
		var evaluation v1alpha1.Evaluation
		if err := json.Unmarshal(raw, &evaluation); err != nil {
			return false, "", "", fmt.Errorf("unmarshal promotion Evaluation %q: %w", name, err)
		}
		runName, err := solutionPromotionRunName(solution.ObjectMeta.Name, revision, &evaluation)
		if err != nil {
			return false, "", "", fmt.Errorf("identify promotion EvaluationRun for %q: %w", name, err)
		}
		run, err := r.getSolutionPromotionRun(ctx, solution, &evaluation, revision, runName)
		if errors.Is(err, store.ErrNotFound) {
			if err := r.createSolutionPromotionRun(ctx, solution, &evaluation, revision, runName); err != nil {
				return false, "", "", fmt.Errorf("create promotion EvaluationRun %q: %w", runName, err)
			}
			pending = append(pending, fmt.Sprintf("%s (%s created)", name, runName))
			continue
		}
		if err != nil {
			return false, "", "", fmt.Errorf("get promotion EvaluationRun %q: %w", runName, err)
		}
		runName = run.Name
		if run.Status.Phase == v1alpha1.PhaseReady && run.Status.Passed && !run.Status.Degraded {
			continue
		}
		if run.Status.Phase == v1alpha1.PhaseFailed || (run.Status.Phase == v1alpha1.PhaseReady && !run.Status.Passed) {
			detail := run.Status.Message
			if detail == "" {
				detail = "required metric thresholds were not met"
			}
			failed = append(failed, fmt.Sprintf("%s (%s): %s", name, runName, detail))
			continue
		}
		pending = append(pending, fmt.Sprintf("%s (%s is %s)", name, runName, run.Status.Phase))
	}
	if len(failed) > 0 {
		return false, "PromotionBlocked", fmt.Sprintf("revision %d blocked: %s", revision, strings.Join(failed, "; ")), nil
	}
	if len(pending) > 0 {
		return false, "PromotionGatesPending", fmt.Sprintf("revision %d is awaiting: %s", revision, strings.Join(pending, "; ")), nil
	}
	return true, "", "", nil
}

func (r *SolutionReconciler) createSolutionPromotionRun(ctx context.Context, solution *v1alpha1.Solution, evaluation *v1alpha1.Evaluation, revision int64, runName string) error {
	now := time.Now().UTC()
	run := v1alpha1.EvaluationRun{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: evaluationRunKind},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name: runName, Namespace: solution.ObjectMeta.Namespace, UID: uuid.NewString(), CreationTimestamp: now,
			Labels:          map[string]string{"kognition.dev/solution": solution.ObjectMeta.Name, "kognition.dev/evaluation": evaluation.ObjectMeta.Name},
			OwnerReferences: []v1alpha1.OwnerReference{{APIVersion: v1alpha1.APIVersion, Kind: solutionKind, Name: solution.ObjectMeta.Name, UID: solution.ObjectMeta.UID}},
		},
		Spec:   evaluationRunSpecSnapshot(evaluation, v1alpha1.EvaluationTargetRef{Kind: solutionKind, Name: solution.Name, Revision: &revision}),
		Status: v1alpha1.EvaluationRunStatus{Phase: v1alpha1.PhasePending},
	}
	data, err := json.Marshal(run)
	if err != nil {
		return err
	}
	if err := r.store.Create(ctx, evaluationRunKind, solution.ObjectMeta.Namespace, runName, data); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		return err
	}
	return nil
}

func solutionPromotionRunName(solution string, revision int64, evaluation *v1alpha1.Evaluation) (string, error) {
	spec, err := json.Marshal(evaluation.Spec)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(spec)
	return boundedResourceName(solutionPromotionRunPrefix(solution, revision, evaluation.Name) + hex.EncodeToString(digest[:])[:8]), nil
}

func solutionPromotionRunPrefix(solution string, revision int64, evaluation string) string {
	return fmt.Sprintf("%s-r%d-%s-", solution, revision, evaluation)
}

func (r *SolutionReconciler) getSolutionPromotionRun(ctx context.Context, solution *v1alpha1.Solution, evaluation *v1alpha1.Evaluation, revision int64, name string) (*v1alpha1.EvaluationRun, error) {
	run, err := readEvaluationRun(ctx, r.store, solution.Namespace, name)
	if !errors.Is(err, store.ErrNotFound) || len(solutionPromotionRunPrefix(solution.Name, revision, evaluation.Name))+8 <= 63 {
		return run, err
	}
	items, err := r.store.List(ctx, evaluationRunKind, store.ListOptions{
		Namespace: solution.Namespace,
		LabelSelector: map[string]string{
			"kognition.dev/solution": solution.Name, "kognition.dev/evaluation": evaluation.Name,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("list existing promotion EvaluationRuns: %w", err)
	}
	spec := evaluationRunSpecSnapshot(evaluation, v1alpha1.EvaluationTargetRef{Kind: solutionKind, Name: solution.Name, Revision: &revision})
	var existing *v1alpha1.EvaluationRun
	for _, item := range items {
		var candidate v1alpha1.EvaluationRun
		if err := json.Unmarshal(item, &candidate); err != nil {
			return nil, fmt.Errorf("unmarshal existing promotion EvaluationRun: %w", err)
		}
		if !alternateBoundedResourceName(candidate.Name, name) ||
			!evaluationRunMatchesSnapshot(&candidate, solutionKind, solution.ObjectMeta, spec) {
			continue
		}
		if existing != nil {
			return nil, fmt.Errorf("multiple existing promotion EvaluationRuns match Solution %q revision %d and Evaluation %q", solution.Name, revision, evaluation.Name)
		}
		existing = &candidate
	}
	if existing != nil {
		return existing, nil
	}
	return nil, store.ErrNotFound
}

func (r *SolutionReconciler) updatePromotionBlockedStatus(ctx context.Context, solution *v1alpha1.Solution, originalStatus v1alpha1.SolutionStatus, originalFinalizers []string, revision int64, reason, message string) (Result, error) {
	solution.Status.CandidateRevision = revision
	if solution.Status.ActiveRevision > 0 {
		solution.Status.Phase = v1alpha1.PhaseDegraded
	} else {
		solution.Status.Phase = v1alpha1.PhasePending
	}
	solution.Status.Conditions = lifecycleToAPIConditions(lifecycle.SetCondition(
		apiToLifecycleConditions(solution.Status.Conditions),
		lifecycle.ConditionReady,
		lifecycle.ConditionFalse,
		reason,
		message,
	))
	if err := r.updateSolution(ctx, solution, originalStatus, originalFinalizers); err != nil {
		return Result{RequeueAfter: 5 * time.Second}, fmt.Errorf("update promotion gate status: %w", err)
	}
	return Result{RequeueAfter: 5 * time.Second}, nil
}

// resolveWorkspaceQuota fetches the CognitiveQuota for the solution's namespace.
// If no quota exists, returns nil (no limits enforced).
func (r *SolutionReconciler) resolveWorkspaceQuota(ctx context.Context, namespace string) (*v1alpha1.CognitiveQuotaSpec, error) {
	items, err := r.store.List(ctx, "CognitiveQuota", store.ListOptions{Namespace: namespace})
	if err != nil {
		return nil, fmt.Errorf("list cognitive quotas: %w", err)
	}
	if len(items) == 0 {
		return nil, nil
	}

	// Use the first CognitiveQuota in the namespace.
	var quota v1alpha1.CognitiveQuota
	if err := json.Unmarshal(items[0], &quota); err != nil {
		return nil, fmt.Errorf("unmarshal cognitive quota: %w", err)
	}
	return &quota.Spec, nil
}

// effectiveTokenBudget resolves the effective token budget by merging workspace quota
// with solution-level overrides. Overrides cannot exceed workspace maxima.
func (r *SolutionReconciler) effectiveTokenBudget(workspaceQuota *v1alpha1.CognitiveQuotaSpec, overrides *v1alpha1.SolutionOverrides) *v1alpha1.TokenBudget {
	if workspaceQuota == nil && (overrides == nil || overrides.TokenBudget == nil) {
		return nil
	}

	result := &v1alpha1.TokenBudget{}

	// Start with workspace quota defaults.
	if workspaceQuota != nil && workspaceQuota.TokenBudget != nil {
		result.MaxInputTokens = workspaceQuota.TokenBudget.MaxInputTokens
		result.MaxOutputTokens = workspaceQuota.TokenBudget.MaxOutputTokens
		result.MaxTotalTokens = workspaceQuota.TokenBudget.MaxTotalTokens
	}

	// Apply overrides (capped at workspace maxima).
	if overrides != nil && overrides.TokenBudget != nil {
		ov := overrides.TokenBudget
		if ov.MaxInputTokens > 0 {
			if result.MaxInputTokens == 0 || ov.MaxInputTokens <= result.MaxInputTokens {
				result.MaxInputTokens = ov.MaxInputTokens
			}
		}
		if ov.MaxOutputTokens > 0 {
			if result.MaxOutputTokens == 0 || ov.MaxOutputTokens <= result.MaxOutputTokens {
				result.MaxOutputTokens = ov.MaxOutputTokens
			}
		}
		if ov.MaxTotalTokens > 0 {
			if result.MaxTotalTokens == 0 || ov.MaxTotalTokens <= result.MaxTotalTokens {
				result.MaxTotalTokens = ov.MaxTotalTokens
			}
		}
	}

	if result.MaxInputTokens == 0 && result.MaxOutputTokens == 0 && result.MaxTotalTokens == 0 {
		return nil
	}
	return result
}

func effectiveHardQuota(workspaceQuota *v1alpha1.CognitiveQuotaSpec, overrides *v1alpha1.SolutionOverrides) map[string]int64 {
	if workspaceQuota == nil || len(workspaceQuota.Hard) == 0 {
		return nil
	}
	result := make(map[string]int64, len(workspaceQuota.Hard))
	for resource, maximum := range workspaceQuota.Hard {
		result[resource] = maximum
	}
	if overrides != nil {
		for resource, override := range overrides.Hard {
			if maximum, ok := result[resource]; ok && override <= maximum {
				result[resource] = override
			}
		}
	}
	return result
}

// compileGoal produces the set of compiled resources from the Solution's goal.
// With the same Seed + Goal + ModelRef + Constraints, the output is deterministic.
func (r *SolutionReconciler) compileGoal(ctx context.Context, solution *v1alpha1.Solution, quota *v1alpha1.CognitiveQuotaSpec) ([]v1alpha1.CompiledResource, string, error) {
	// Build the canonical spec representation for hashing.
	specData, err := json.Marshal(solution.Spec)
	if err != nil {
		return nil, "", fmt.Errorf("marshal spec: %w", err)
	}
	hash := sha256.Sum256(specData)
	specHash := hex.EncodeToString(hash[:])
	if solution.Spec.Target != nil {
		if _, err := r.store.Get(ctx, solution.Spec.Target.Kind, solution.ObjectMeta.Namespace, solution.Spec.Target.Ref.Name); err != nil {
			return nil, "", fmt.Errorf("get target %s %q: %w", solution.Spec.Target.Kind, solution.Spec.Target.Ref.Name, err)
		}
		return r.compileTargetExecution(ctx, solution, specHash)
	}

	// Compile goal into a deterministic tool-assisted multi-agent pipeline. Keep the original
	// <solution>-agent name as the coordinator so existing references remain stable.
	researchToolName := fmt.Sprintf("%s-research-tool", solution.ObjectMeta.Name)
	memoryClaimName := fmt.Sprintf("%s-memory", solution.ObjectMeta.Name)
	policyName := fmt.Sprintf("%s-policy", solution.ObjectMeta.Name)
	roles := []solutionAgentRole{
		{
			Name:            fmt.Sprintf("%s-agent", solution.ObjectMeta.Name),
			Instructions:    solution.Spec.Goal,
			Timeout:         "60s",
			MemoryClaimRefs: []string{memoryClaimName},
		},
		{
			Name:            fmt.Sprintf("%s-researcher", solution.ObjectMeta.Name),
			Instructions:    fmt.Sprintf("Research the goal and produce a concise evidence brief with assumptions, constraints, and sources. Goal:\n%s", solution.Spec.Goal),
			Timeout:         "90s",
			ToolRefs:        []string{researchToolName},
			MemoryClaimRefs: []string{memoryClaimName},
		},
		{
			Name:            fmt.Sprintf("%s-writer", solution.ObjectMeta.Name),
			Instructions:    fmt.Sprintf("Transform the research brief into the requested deliverable. Preserve constraints and call out unresolved risks. Goal:\n%s", solution.Spec.Goal),
			Timeout:         "90s",
			ToolRefs:        []string{researchToolName},
			MemoryClaimRefs: []string{memoryClaimName},
		},
		{
			Name:            fmt.Sprintf("%s-publisher", solution.ObjectMeta.Name),
			Instructions:    fmt.Sprintf("Prepare the final answer for delivery after approval. Ensure it satisfies the original goal and constraints. Goal:\n%s", solution.Spec.Goal),
			Timeout:         "60s",
			MemoryClaimRefs: []string{memoryClaimName},
		},
	}

	compiled := make([]v1alpha1.CompiledResource, 0, len(roles)+4)
	memoryClaimManifest, err := r.buildMemoryClaimManifest(solution, memoryClaimName)
	if err != nil {
		return nil, "", fmt.Errorf("build memory claim manifest %s: %w", memoryClaimName, err)
	}
	compiled = append(compiled, v1alpha1.CompiledResource{
		Kind:      "MemoryClaim",
		Name:      memoryClaimName,
		Namespace: solution.ObjectMeta.Namespace,
		Manifest:  memoryClaimManifest,
	})

	toolManifest, err := r.buildToolManifest(solution, researchToolName)
	if err != nil {
		return nil, "", fmt.Errorf("build tool manifest %s: %w", researchToolName, err)
	}
	compiled = append(compiled, v1alpha1.CompiledResource{
		Kind:      "Tool",
		Name:      researchToolName,
		Namespace: solution.ObjectMeta.Namespace,
		Manifest:  toolManifest,
	})

	policyManifest, err := r.buildAgentPolicyManifest(solution, policyName, roles)
	if err != nil {
		return nil, "", fmt.Errorf("build agent policy manifest %s: %w", policyName, err)
	}
	compiled = append(compiled, v1alpha1.CompiledResource{
		Kind:      "AgentPolicy",
		Name:      policyName,
		Namespace: solution.ObjectMeta.Namespace,
		Manifest:  policyManifest,
	})

	for _, role := range roles {
		agentManifest, err := r.buildAgentManifest(solution, role, quota)
		if err != nil {
			return nil, "", fmt.Errorf("build agent manifest %s: %w", role.Name, err)
		}
		compiled = append(compiled, v1alpha1.CompiledResource{
			Kind:      "Agent",
			Name:      role.Name,
			Namespace: solution.ObjectMeta.Namespace,
			Manifest:  agentManifest,
		})
	}

	workflowName := fmt.Sprintf("%s-workflow", solution.ObjectMeta.Name)
	workflowManifest, err := r.buildWorkflowManifest(solution, workflowName, roles, quota)
	if err != nil {
		return nil, "", fmt.Errorf("build workflow manifest: %w", err)
	}
	compiled = append(compiled, v1alpha1.CompiledResource{
		Kind:      "Workflow",
		Name:      workflowName,
		Namespace: solution.ObjectMeta.Namespace,
		Manifest:  workflowManifest,
	})

	return compiled, specHash, nil
}

func (r *SolutionReconciler) compileTargetExecution(ctx context.Context, solution *v1alpha1.Solution, specHash string) ([]v1alpha1.CompiledResource, string, error) {
	target := solution.Spec.Target
	labels := map[string]string{
		"kognition.dev/solution":    solution.ObjectMeta.Name,
		"kognition.dev/managed":     "true",
		"kognition.dev/target-kind": target.Kind,
		"kognition.dev/target-name": target.Ref.Name,
	}
	owner := []v1alpha1.OwnerReference{{
		APIVersion: v1alpha1.APIVersion,
		Kind:       solutionKind,
		Name:       solution.ObjectMeta.Name,
		UID:        solution.ObjectMeta.UID,
	}}
	metadata := func(kind, name string) v1alpha1.ObjectMeta {
		return v1alpha1.ObjectMeta{
			Name:              name,
			Namespace:         solution.ObjectMeta.Namespace,
			UID:               stableChildUID(solution, kind, name),
			CreationTimestamp: solution.ObjectMeta.CreationTimestamp,
			Labels:            labels,
			OwnerReferences:   owner,
		}
	}
	if len(solution.Spec.Constraints) > 0 {
		constraints, err := json.Marshal(solution.Spec.Constraints)
		if err != nil {
			return nil, "", fmt.Errorf("marshal constraints: %w", err)
		}
		metadata = func(kind, name string) v1alpha1.ObjectMeta {
			objectMeta := v1alpha1.ObjectMeta{
				Name:              name,
				Namespace:         solution.ObjectMeta.Namespace,
				UID:               stableChildUID(solution, kind, name),
				CreationTimestamp: solution.ObjectMeta.CreationTimestamp,
				Labels:            labels,
				Annotations:       map[string]string{"kognition.dev/constraints": string(constraints)},
				OwnerReferences:   owner,
			}
			return objectMeta
		}
	}
	if target.Kind == "Agent" {
		name := fmt.Sprintf("%s-agent-job", solution.ObjectMeta.Name)
		job := v1alpha1.AgentJob{
			TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "AgentJob"},
			ObjectMeta: metadata("AgentJob", name),
			Spec: v1alpha1.AgentJobSpec{
				AgentRef:         &v1alpha1.LocalObjectReference{Name: target.Ref.Name},
				CompletionPolicy: v1alpha1.CompletionPolicySuccess,
				Input:            solution.Spec.Goal,
			},
		}
		manifest, err := json.Marshal(job)
		if err != nil {
			return nil, "", fmt.Errorf("marshal AgentJob: %w", err)
		}
		return []v1alpha1.CompiledResource{{Kind: "AgentJob", Name: name, Namespace: solution.ObjectMeta.Namespace, Manifest: manifest}}, specHash, nil
	}
	if target.Kind == "AgentService" {
		service, agentRevision, err := r.resolveAgentServiceTarget(ctx, solution.ObjectMeta.Namespace, target.Ref.Name)
		if err != nil {
			return nil, "", err
		}
		name := fmt.Sprintf("%s-agent-service-job", solution.ObjectMeta.Name)
		job := v1alpha1.AgentJob{
			TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "AgentJob"},
			ObjectMeta: metadata("AgentJob", name),
			Spec: v1alpha1.AgentJobSpec{
				AgentRef:         &v1alpha1.LocalObjectReference{Name: service.Spec.AgentRef.Name},
				AgentRevision:    agentRevision,
				CompletionPolicy: v1alpha1.CompletionPolicySuccess,
				Input:            solution.Spec.Goal,
			},
		}
		job.ObjectMeta.Labels["kognition.dev/agent-service"] = service.ObjectMeta.Name
		job.ObjectMeta.Annotations = map[string]string{AgentServiceExecutionAnnotation: service.ObjectMeta.Name}
		manifest, err := json.Marshal(job)
		if err != nil {
			return nil, "", fmt.Errorf("marshal AgentService AgentJob: %w", err)
		}
		return []v1alpha1.CompiledResource{{Kind: "AgentJob", Name: name, Namespace: solution.ObjectMeta.Namespace, Manifest: manifest}}, specHash, nil
	}
	if target.Kind == "Workflow" {
		name := fmt.Sprintf("%s-workflow-run", solution.ObjectMeta.Name)
		run := v1alpha1.WorkflowRun{
			TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "WorkflowRun"},
			ObjectMeta: metadata("WorkflowRun", name),
			Spec: v1alpha1.WorkflowRunSpec{
				WorkflowRef: v1alpha1.LocalObjectReference{Name: target.Ref.Name},
				Input:       solution.Spec.Goal,
			},
		}
		manifest, err := json.Marshal(run)
		if err != nil {
			return nil, "", fmt.Errorf("marshal WorkflowRun: %w", err)
		}
		return []v1alpha1.CompiledResource{{Kind: "WorkflowRun", Name: name, Namespace: solution.ObjectMeta.Namespace, Manifest: manifest}}, specHash, nil
	}
	return nil, "", fmt.Errorf("unsupported target kind %q", target.Kind)
}

func (r *SolutionReconciler) resolveAgentServiceTarget(ctx context.Context, namespace, serviceName string) (*v1alpha1.AgentService, *int64, error) {
	service, err := getStoredResource[v1alpha1.AgentService](ctx, r.store, "AgentService", namespace, serviceName)
	if err != nil {
		return nil, nil, fmt.Errorf("get AgentService %q: %w", serviceName, err)
	}
	if service.Status.Phase != v1alpha1.PhaseReady || service.Status.ReadyInstances < 1 {
		return nil, nil, fmt.Errorf("AgentService %q is not ready", serviceName)
	}
	if service.Spec.AgentRef.Name == "" {
		return nil, nil, fmt.Errorf("AgentService %q has no agentRef", serviceName)
	}
	agent, err := getStoredResource[v1alpha1.Agent](ctx, r.store, "Agent", namespace, service.Spec.AgentRef.Name)
	if err != nil {
		return nil, nil, fmt.Errorf("get AgentService agent %q: %w", service.Spec.AgentRef.Name, err)
	}
	if service.Spec.RevisionPin == nil {
		return service, nil, nil
	}
	revision, err := r.store.GetRevision(ctx, "Agent", namespace, agent.ObjectMeta.Name, *service.Spec.RevisionPin)
	if err != nil {
		return nil, nil, fmt.Errorf("get AgentService pinned revision %d for agent %q: %w", *service.Spec.RevisionPin, agent.ObjectMeta.Name, err)
	}
	if revision.Revision != *service.Spec.RevisionPin {
		return nil, nil, fmt.Errorf("AgentService pinned revision %d for agent %q does not match stored revision", *service.Spec.RevisionPin, agent.ObjectMeta.Name)
	}
	return service, service.Spec.RevisionPin, nil
}

type solutionAgentRole struct {
	Name            string
	Instructions    string
	Timeout         string
	ToolRefs        []string
	MemoryClaimRefs []string
}

// buildMemoryClaimManifest constructs the JSON manifest for the compiled shared memory claim.
func (r *SolutionReconciler) buildMemoryClaimManifest(solution *v1alpha1.Solution, claimName string) (json.RawMessage, error) {
	claim := v1alpha1.MemoryClaim{
		TypeMeta: v1alpha1.TypeMeta{
			APIVersion: v1alpha1.APIVersion,
			Kind:       "MemoryClaim",
		},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:              claimName,
			Namespace:         solution.ObjectMeta.Namespace,
			UID:               stableChildUID(solution, "MemoryClaim", claimName),
			CreationTimestamp: solution.ObjectMeta.CreationTimestamp,
			Labels: map[string]string{
				"kognition.dev/solution": solution.ObjectMeta.Name,
				"kognition.dev/managed":  "true",
			},
			OwnerReferences: []v1alpha1.OwnerReference{
				{
					APIVersion: v1alpha1.APIVersion,
					Kind:       solutionKind,
					Name:       solution.ObjectMeta.Name,
					UID:        solution.ObjectMeta.UID,
				},
			},
		},
		Spec: v1alpha1.MemoryClaimSpec{
			MemoryClassName: "default",
			Type:            v1alpha1.MemoryTypeShared,
			Capacity:        "64Mi",
			ConcurrencyMode: v1alpha1.ConcurrencyLWW,
			AccessModes:     []string{"ReadWrite"},
		},
	}

	if solution.Spec.Seed != nil {
		if claim.ObjectMeta.Annotations == nil {
			claim.ObjectMeta.Annotations = make(map[string]string)
		}
		claim.ObjectMeta.Annotations["kognition.dev/seed"] = fmt.Sprintf("%d", *solution.Spec.Seed)
	}

	data, err := json.Marshal(claim)
	if err != nil {
		return nil, fmt.Errorf("marshal memory claim: %w", err)
	}
	return json.RawMessage(data), nil
}

// buildToolManifest constructs the JSON manifest for the compiled research tool.
func (r *SolutionReconciler) buildToolManifest(solution *v1alpha1.Solution, toolName string) (json.RawMessage, error) {
	tool := v1alpha1.Tool{
		TypeMeta: v1alpha1.TypeMeta{
			APIVersion: v1alpha1.APIVersion,
			Kind:       "Tool",
		},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:              toolName,
			Namespace:         solution.ObjectMeta.Namespace,
			UID:               stableChildUID(solution, "Tool", toolName),
			CreationTimestamp: solution.ObjectMeta.CreationTimestamp,
			Labels: map[string]string{
				"kognition.dev/solution": solution.ObjectMeta.Name,
				"kognition.dev/managed":  "true",
			},
			OwnerReferences: []v1alpha1.OwnerReference{
				{
					APIVersion: v1alpha1.APIVersion,
					Kind:       solutionKind,
					Name:       solution.ObjectMeta.Name,
					UID:        solution.ObjectMeta.UID,
				},
			},
		},
		Spec: v1alpha1.ToolSpec{
			Protocol:    v1alpha1.ToolProtocolOpenAPI,
			ClassName:   "sandbox-openapi",
			Description: "Searches supporting knowledge and source material for a compiled Solution plan.",
			Endpoint:    "http://tool-sandbox.samples-tool-sandbox.svc.cluster.local:8080/knowledge-search",
			Metadata: map[string]string{
				"allowPrivateNetwork":   "true",
				"kognition.dev/purpose": "solution-research",
			},
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "Research query derived from the Solution goal or workflow context.",
					},
					"top_k": map[string]any{
						"type":        "integer",
						"description": "Maximum number of passages to return.",
						"default":     float64(3),
					},
				},
				"required": []any{"query"},
			},
		},
	}

	if solution.Spec.Seed != nil {
		if tool.ObjectMeta.Annotations == nil {
			tool.ObjectMeta.Annotations = make(map[string]string)
		}
		tool.ObjectMeta.Annotations["kognition.dev/seed"] = fmt.Sprintf("%d", *solution.Spec.Seed)
	}

	data, err := json.Marshal(tool)
	if err != nil {
		return nil, fmt.Errorf("marshal tool: %w", err)
	}
	return json.RawMessage(data), nil
}

// buildAgentPolicyManifest constructs the JSON manifest authorizing compiled tool use.
func (r *SolutionReconciler) buildAgentPolicyManifest(solution *v1alpha1.Solution, policyName string, roles []solutionAgentRole) (json.RawMessage, error) {
	policy := v1alpha1.AgentPolicy{
		TypeMeta: v1alpha1.TypeMeta{
			APIVersion: v1alpha1.APIVersion,
			Kind:       "AgentPolicy",
		},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:              policyName,
			Namespace:         solution.ObjectMeta.Namespace,
			UID:               stableChildUID(solution, "AgentPolicy", policyName),
			CreationTimestamp: solution.ObjectMeta.CreationTimestamp,
			Labels: map[string]string{
				"kognition.dev/solution": solution.ObjectMeta.Name,
				"kognition.dev/managed":  "true",
			},
			OwnerReferences: []v1alpha1.OwnerReference{
				{
					APIVersion: v1alpha1.APIVersion,
					Kind:       solutionKind,
					Name:       solution.ObjectMeta.Name,
					UID:        solution.ObjectMeta.UID,
				},
			},
		},
		Spec: v1alpha1.AgentPolicySpec{
			Enforce:      true,
			Scope:        "agent",
			TargetAgents: policyTargetAgents(roles),
		},
	}

	if solution.Spec.Seed != nil {
		if policy.ObjectMeta.Annotations == nil {
			policy.ObjectMeta.Annotations = make(map[string]string)
		}
		policy.ObjectMeta.Annotations["kognition.dev/seed"] = fmt.Sprintf("%d", *solution.Spec.Seed)
	}

	data, err := json.Marshal(policy)
	if err != nil {
		return nil, fmt.Errorf("marshal agent policy: %w", err)
	}
	return json.RawMessage(data), nil
}

func policyTargetAgents(roles []solutionAgentRole) []string {
	targets := make([]string, 0, len(roles))
	for _, role := range roles {
		if len(role.ToolRefs) == 0 {
			continue
		}
		targets = append(targets, role.Name)
	}
	return targets
}

// buildAgentManifest constructs the JSON manifest for the compiled agent.
func (r *SolutionReconciler) buildAgentManifest(solution *v1alpha1.Solution, role solutionAgentRole, quota *v1alpha1.CognitiveQuotaSpec) (json.RawMessage, error) {
	var modelRef v1alpha1.LocalObjectReference
	if solution.Spec.ModelRef != nil {
		modelRef = *solution.Spec.ModelRef
	}

	agent := v1alpha1.Agent{
		TypeMeta: v1alpha1.TypeMeta{
			APIVersion: v1alpha1.APIVersion,
			Kind:       "Agent",
		},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:              role.Name,
			Namespace:         solution.ObjectMeta.Namespace,
			UID:               stableChildUID(solution, "Agent", role.Name),
			CreationTimestamp: solution.ObjectMeta.CreationTimestamp,
			Labels: map[string]string{
				"kognition.dev/solution": solution.ObjectMeta.Name,
				"kognition.dev/managed":  "true",
			},
			OwnerReferences: []v1alpha1.OwnerReference{
				{
					APIVersion: v1alpha1.APIVersion,
					Kind:       solutionKind,
					Name:       solution.ObjectMeta.Name,
					UID:        solution.ObjectMeta.UID,
				},
			},
		},
		Spec: v1alpha1.AgentSpec{
			Instructions: v1alpha1.AgentInstructions{
				Inline: role.Instructions,
			},
			ModelRef: modelRef,
			Memory:   agentMemorySpec(role.MemoryClaimRefs),
			Tools:    agentToolRefs(role.ToolRefs),
			Runtime: &v1alpha1.AgentRuntimeSpec{
				Class:   "default",
				Timeout: role.Timeout,
			},
		},
	}

	// Apply seed as an annotation for determinism tracking.
	if solution.Spec.Seed != nil {
		if agent.ObjectMeta.Annotations == nil {
			agent.ObjectMeta.Annotations = make(map[string]string)
		}
		agent.ObjectMeta.Annotations["kognition.dev/seed"] = fmt.Sprintf("%d", *solution.Spec.Seed)
	}

	// Apply constraints as agent annotations.
	if len(solution.Spec.Constraints) > 0 {
		if agent.ObjectMeta.Annotations == nil {
			agent.ObjectMeta.Annotations = make(map[string]string)
		}
		constraintsJSON, err := json.Marshal(solution.Spec.Constraints)
		if err != nil {
			return nil, fmt.Errorf("marshal constraints: %w", err)
		}
		agent.ObjectMeta.Annotations["kognition.dev/constraints"] = string(constraintsJSON)
	}

	// Apply workspace quota inheritance with solution overrides.
	effectiveBudget := r.effectiveTokenBudget(quota, solution.Spec.Overrides)
	if effectiveBudget != nil && effectiveBudget.MaxTotalTokens > 0 {
		maxTokens := effectiveBudget.MaxTotalTokens
		agent.Spec.Runtime.MaxTokens = &maxTokens
	}

	// Apply effective hard quota limits as annotations.
	if effectiveHard := effectiveHardQuota(quota, solution.Spec.Overrides); len(effectiveHard) > 0 {
		if agent.ObjectMeta.Annotations == nil {
			agent.ObjectMeta.Annotations = make(map[string]string)
		}
		hardJSON, err := json.Marshal(effectiveHard)
		if err != nil {
			return nil, fmt.Errorf("marshal hard quota: %w", err)
		}
		agent.ObjectMeta.Annotations["kognition.dev/workspace-quota"] = string(hardJSON)
	}

	data, err := json.Marshal(agent)
	if err != nil {
		return nil, fmt.Errorf("marshal agent: %w", err)
	}
	return json.RawMessage(data), nil
}

func agentToolRefs(names []string) []v1alpha1.AgentToolRef {
	if len(names) == 0 {
		return nil
	}
	refs := make([]v1alpha1.AgentToolRef, 0, len(names))
	for _, name := range names {
		refs = append(refs, v1alpha1.AgentToolRef{
			Ref:      v1alpha1.LocalObjectReference{Name: name},
			Required: true,
		})
	}
	return refs
}

func agentMemorySpec(names []string) *v1alpha1.AgentMemorySpec {
	if len(names) == 0 {
		return nil
	}
	refs := make([]v1alpha1.LocalObjectReference, 0, len(names))
	for _, name := range names {
		refs = append(refs, v1alpha1.LocalObjectReference{Name: name})
	}
	return &v1alpha1.AgentMemorySpec{Claims: refs}
}

// buildWorkflowManifest constructs the JSON manifest for the compiled workflow.
func (r *SolutionReconciler) buildWorkflowManifest(solution *v1alpha1.Solution, workflowName string, roles []solutionAgentRole, quota *v1alpha1.CognitiveQuotaSpec) (json.RawMessage, error) {
	workflow := v1alpha1.Workflow{
		TypeMeta: v1alpha1.TypeMeta{
			APIVersion: v1alpha1.APIVersion,
			Kind:       "Workflow",
		},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:              workflowName,
			Namespace:         solution.ObjectMeta.Namespace,
			UID:               stableChildUID(solution, "Workflow", workflowName),
			CreationTimestamp: solution.ObjectMeta.CreationTimestamp,
			Labels: map[string]string{
				"kognition.dev/solution": solution.ObjectMeta.Name,
				"kognition.dev/managed":  "true",
			},
			OwnerReferences: []v1alpha1.OwnerReference{
				{
					APIVersion: v1alpha1.APIVersion,
					Kind:       solutionKind,
					Name:       solution.ObjectMeta.Name,
					UID:        solution.ObjectMeta.UID,
				},
			},
		},
		Spec: v1alpha1.WorkflowSpec{
			EntryPoint: "plan",
			Steps: []v1alpha1.WorkflowStep{
				{
					Name:     "plan",
					AgentRef: v1alpha1.LocalObjectReference{Name: roles[0].Name},
					Input:    "Plan the work needed for this goal: {{.Input}}",
					Timeout:  roles[0].Timeout,
				},
				{
					Name:     "research",
					AgentRef: v1alpha1.LocalObjectReference{Name: roles[1].Name},
					Input:    "Research the plan and source material:\n{{.Steps.plan.Output}}",
					Timeout:  roles[1].Timeout,
				},
				{
					Name:     "write",
					AgentRef: v1alpha1.LocalObjectReference{Name: roles[2].Name},
					Input:    "Create the deliverable from this research:\n{{.Steps.research.Output}}",
					Timeout:  roles[2].Timeout,
				},
				{
					Name:     "publish",
					AgentRef: v1alpha1.LocalObjectReference{Name: roles[3].Name},
					Input:    "Finalize the approved deliverable:\n{{.Steps.write.Output}}",
					Timeout:  roles[3].Timeout,
				},
			},
			Edges: []v1alpha1.WorkflowEdge{
				{From: "plan", To: "research"},
				{From: "research", To: "write"},
				{From: "write", To: "publish"},
			},
			ApprovalGates: []v1alpha1.WorkflowApprovalGate{
				{From: "write", To: "publish"},
			},
			Budgets: &v1alpha1.WorkflowBudget{
				MaxDuration: "10m",
			},
		},
	}

	if solution.Spec.Seed != nil {
		if workflow.ObjectMeta.Annotations == nil {
			workflow.ObjectMeta.Annotations = make(map[string]string)
		}
		workflow.ObjectMeta.Annotations["kognition.dev/seed"] = fmt.Sprintf("%d", *solution.Spec.Seed)
	}
	if len(solution.Spec.Constraints) > 0 {
		if workflow.ObjectMeta.Annotations == nil {
			workflow.ObjectMeta.Annotations = make(map[string]string)
		}
		constraintsJSON, err := json.Marshal(solution.Spec.Constraints)
		if err != nil {
			return nil, fmt.Errorf("marshal constraints: %w", err)
		}
		workflow.ObjectMeta.Annotations["kognition.dev/constraints"] = string(constraintsJSON)
	}

	effectiveBudget := r.effectiveTokenBudget(quota, solution.Spec.Overrides)
	if effectiveBudget != nil && effectiveBudget.MaxTotalTokens > 0 {
		workflow.Spec.Budgets.MaxTokens = effectiveBudget.MaxTotalTokens
	}

	data, err := json.Marshal(workflow)
	if err != nil {
		return nil, fmt.Errorf("marshal workflow: %w", err)
	}
	return json.RawMessage(data), nil
}

func stableChildUID(solution *v1alpha1.Solution, kind, name string) string {
	identity := fmt.Sprintf("%s/%s/%s/%s/%s", solution.ObjectMeta.UID, solution.ObjectMeta.Namespace, solution.ObjectMeta.Name, kind, name)
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(identity)).String()
}

// detectGoalChange checks whether the spec has changed since the last revision.
func (r *SolutionReconciler) detectGoalChange(ctx context.Context, solution *v1alpha1.Solution, specHash string) (bool, int64, error) {
	revisions, err := r.store.ListRevisions(ctx, solutionKind, solution.ObjectMeta.Namespace, solution.ObjectMeta.Name)
	if err != nil {
		return false, 0, fmt.Errorf("list revisions: %w", err)
	}

	if len(revisions) == 0 {
		return true, 0, nil
	}

	// Sort by revision number descending.
	sort.Slice(revisions, func(i, j int) bool {
		return revisions[i].Revision > revisions[j].Revision
	})

	latest := revisions[0]

	// Compare spec hash stored in Data field.
	if string(latest.Data) == specHash {
		return false, latest.Revision, nil
	}

	return true, latest.Revision, nil
}

// createRevision stores a new immutable revision in the store.
func (r *SolutionReconciler) createRevision(ctx context.Context, solution *v1alpha1.Solution, _ []v1alpha1.CompiledResource, specHash string, currentRevision int64) (int64, error) {
	newRevision := currentRevision + 1

	spec, err := json.Marshal(solution.Spec)
	if err != nil {
		return 0, fmt.Errorf("marshal spec for revision: %w", err)
	}

	if err := r.store.CreateRevision(ctx, solutionKind, solution.ObjectMeta.Namespace, solution.ObjectMeta.Name, newRevision, spec, []byte(specHash)); err != nil {
		return 0, fmt.Errorf("create revision %d: %w", newRevision, err)
	}

	return newRevision, nil
}

// deployChildResources creates or updates child resources in the store.
func (r *SolutionReconciler) deployChildResources(ctx context.Context, solution *v1alpha1.Solution, compiled []v1alpha1.CompiledResource) error {
	for _, res := range compiled {
		ns := res.Namespace
		if ns == "" {
			ns = solution.ObjectMeta.Namespace
		}

		// Try create; if already exists, update.
		err := r.store.Create(ctx, res.Kind, ns, res.Name, res.Manifest)
		if errors.Is(err, store.ErrAlreadyExists) {
			// Fetch existing to get resourceVersion for update.
			existing, getErr := r.store.Get(ctx, res.Kind, ns, res.Name)
			if getErr != nil {
				return fmt.Errorf("get existing %s/%s: %w", res.Kind, res.Name, getErr)
			}
			var meta struct {
				Metadata v1alpha1.ObjectMeta `json:"metadata"`
				Status   json.RawMessage     `json:"status"`
			}
			if jsonErr := json.Unmarshal(existing, &meta); jsonErr != nil {
				return fmt.Errorf("unmarshal existing %s/%s metadata: %w", res.Kind, res.Name, jsonErr)
			}
			if !solutionOwnsResource(solution, meta.Metadata.OwnerReferences) {
				return fmt.Errorf("existing %s/%s is not owned by Solution %s/%s: %w", res.Kind, res.Name, solution.ObjectMeta.Namespace, solution.ObjectMeta.Name, store.ErrAlreadyExists)
			}
			var desired map[string]json.RawMessage
			if err := json.Unmarshal(res.Manifest, &desired); err != nil {
				return fmt.Errorf("unmarshal compiled %s/%s: %w", res.Kind, res.Name, err)
			}
			var generatedMetadata v1alpha1.ObjectMeta
			if err := json.Unmarshal(desired["metadata"], &generatedMetadata); err != nil {
				return fmt.Errorf("unmarshal compiled %s/%s metadata: %w", res.Kind, res.Name, err)
			}
			if meta.Metadata.Labels == nil {
				meta.Metadata.Labels = make(map[string]string)
			}
			for key, value := range generatedMetadata.Labels {
				meta.Metadata.Labels[key] = value
			}
			if meta.Metadata.Annotations == nil {
				meta.Metadata.Annotations = make(map[string]string)
			}
			for key, value := range generatedMetadata.Annotations {
				meta.Metadata.Annotations[key] = value
			}
			// Compilation owns the spec, not API-assigned identity or child lifecycle state.
			desired["metadata"], err = json.Marshal(meta.Metadata)
			if err != nil {
				return fmt.Errorf("marshal preserved %s/%s metadata: %w", res.Kind, res.Name, err)
			}
			delete(desired, "status")
			if len(meta.Status) != 0 {
				desired["status"] = meta.Status
			}
			payload, err := json.Marshal(desired)
			if err != nil {
				return fmt.Errorf("marshal updated %s/%s: %w", res.Kind, res.Name, err)
			}
			if updateErr := r.store.Update(ctx, res.Kind, ns, res.Name, payload, meta.Metadata.ResourceVersion); updateErr != nil {
				return fmt.Errorf("update %s/%s: %w", res.Kind, res.Name, updateErr)
			}
		} else if err != nil {
			return fmt.Errorf("create %s/%s: %w", res.Kind, res.Name, err)
		}
	}
	return nil
}

func solutionOwnsResource(solution *v1alpha1.Solution, refs []v1alpha1.OwnerReference) bool {
	for _, ref := range refs {
		if ref.Kind == solutionKind && ref.Name == solution.ObjectMeta.Name && ref.UID == solution.ObjectMeta.UID {
			return true
		}
	}
	return false
}

// handleDeletion cleans up child resources and removes the finalizer.
func (r *SolutionReconciler) handleDeletion(ctx context.Context, solution *v1alpha1.Solution, originalStatus v1alpha1.SolutionStatus, originalFinalizers []string, log *zap.Logger) (Result, error) {
	if !lifecycle.HasFinalizer(solution.ObjectMeta.Finalizers, SolutionFinalizer) {
		return Result{}, nil
	}

	log.Info("handling solution deletion")

	// Delete all compiled child resources.
	for _, res := range solution.Status.CompiledResources {
		ns := res.Namespace
		if ns == "" {
			ns = solution.ObjectMeta.Namespace
		}
		raw, err := r.store.Get(ctx, res.Kind, ns, res.Name)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return Result{}, fmt.Errorf("get child %s/%s for deletion: %w", res.Kind, res.Name, err)
		}
		var obj struct {
			Metadata v1alpha1.ObjectMeta `json:"metadata"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil {
			return Result{}, fmt.Errorf("unmarshal child %s/%s for deletion: %w", res.Kind, res.Name, err)
		}
		if !solutionOwnsResource(solution, obj.Metadata.OwnerReferences) {
			continue
		}
		if err := r.store.Delete(ctx, res.Kind, ns, res.Name); err != nil && !errors.Is(err, store.ErrNotFound) {
			return Result{}, fmt.Errorf("delete child %s/%s: %w", res.Kind, res.Name, err)
		}
	}

	// Also clean up resources from the label selector to cover any residual.
	kinds := []string{"Agent", "AgentJob", "Workflow", "WorkflowRun", "Tool", "AgentPolicy", "MemoryClaim", "Memory", evaluationRunKind}
	for _, kind := range kinds {
		items, err := r.store.List(ctx, kind, store.ListOptions{
			Namespace: solution.ObjectMeta.Namespace,
			LabelSelector: map[string]string{
				"kognition.dev/solution": solution.ObjectMeta.Name,
			},
		})
		if err != nil {
			return Result{}, fmt.Errorf("list owned %s resources: %w", kind, err)
		}
		for _, item := range items {
			var obj struct {
				Metadata v1alpha1.ObjectMeta `json:"metadata"`
			}
			if err := json.Unmarshal(item, &obj); err != nil {
				return Result{}, fmt.Errorf("unmarshal owned %s resource: %w", kind, err)
			}
			if !solutionOwnsResource(solution, obj.Metadata.OwnerReferences) {
				continue
			}
			if delErr := r.store.Delete(ctx, kind, solution.ObjectMeta.Namespace, obj.Metadata.Name); delErr != nil && !errors.Is(delErr, store.ErrNotFound) {
				return Result{}, fmt.Errorf("delete owned %s/%s: %w", kind, obj.Metadata.Name, delErr)
			}
		}
	}

	// Remove finalizer.
	updated, _ := lifecycle.RemoveFinalizer(solution.ObjectMeta.Finalizers, SolutionFinalizer)
	solution.ObjectMeta.Finalizers = updated
	if err := r.updateSolution(ctx, solution, originalStatus, originalFinalizers); err != nil {
		return Result{}, fmt.Errorf("remove finalizer: %w", err)
	}

	return Result{}, nil
}

// RollbackToRevision rolls back the solution to the specified revision.
func (r *SolutionReconciler) RollbackToRevision(ctx context.Context, namespace, name string, targetRevision int64) error {
	rev, err := r.store.GetRevision(ctx, solutionKind, namespace, name, targetRevision)
	if err != nil {
		return fmt.Errorf("get revision %d: %w", targetRevision, err)
	}

	// Get the current solution.
	data, err := r.store.Get(ctx, solutionKind, namespace, name)
	if err != nil {
		return fmt.Errorf("get solution: %w", err)
	}

	var solution v1alpha1.Solution
	if err := json.Unmarshal(data, &solution); err != nil {
		return fmt.Errorf("unmarshal solution: %w", err)
	}

	// Restore the spec from the revision.
	var spec v1alpha1.SolutionSpec
	if err := json.Unmarshal(rev.Spec, &spec); err != nil {
		return fmt.Errorf("unmarshal revision spec: %w", err)
	}
	solution.Spec = spec

	// The next reconcile loop will detect the spec change and redeploy.
	return r.updateSolutionObject(ctx, &solution)
}

// updateReadyStatus updates the solution status to reflect successful compilation.
func (r *SolutionReconciler) updateReadyStatus(ctx context.Context, solution *v1alpha1.Solution, originalStatus v1alpha1.SolutionStatus, originalFinalizers []string, compiled []v1alpha1.CompiledResource, revision int64, _ *zap.Logger) (Result, error) {
	solution.Status.Phase = v1alpha1.PhaseReady
	solution.Status.ActiveRevision = revision
	solution.Status.CandidateRevision = 0
	solution.Status.CompiledResources = compiled

	// Populate GeneratedResources for backward compat.
	generated := make([]v1alpha1.GeneratedResource, len(compiled))
	for i, c := range compiled {
		generated[i] = v1alpha1.GeneratedResource{Kind: c.Kind, Name: c.Name}
	}
	solution.Status.GeneratedResources = generated

	lcConditions := lifecycle.SetCondition(
		apiToLifecycleConditions(solution.Status.Conditions),
		lifecycle.ConditionReady,
		lifecycle.ConditionTrue,
		"GoalCompiled",
		fmt.Sprintf("revision %d active with %d resources", revision, len(compiled)),
	)
	solution.Status.Conditions = lifecycleToAPIConditions(lcConditions)

	if err := r.updateSolution(ctx, solution, originalStatus, originalFinalizers); err != nil {
		return Result{}, fmt.Errorf("update ready status: %w", err)
	}
	return Result{}, nil
}

// setFailedStatus updates the solution status to reflect a failure.
func (r *SolutionReconciler) setFailedStatus(ctx context.Context, solution *v1alpha1.Solution, originalStatus v1alpha1.SolutionStatus, originalFinalizers []string, reason, message string, log *zap.Logger) (Result, error) {
	log.Error("solution reconciliation failed", zap.String("reason", reason), zap.String("message", message))

	solution.Status.Phase = v1alpha1.PhaseFailed
	lcConditions := lifecycle.SetCondition(
		apiToLifecycleConditions(solution.Status.Conditions),
		lifecycle.ConditionReady,
		lifecycle.ConditionFalse,
		reason,
		message,
	)
	solution.Status.Conditions = lifecycleToAPIConditions(lcConditions)

	if err := r.updateSolution(ctx, solution, originalStatus, originalFinalizers); err != nil {
		return Result{RequeueAfter: 30 * time.Second}, fmt.Errorf("update failed status: %w", err)
	}
	return Result{RequeueAfter: 30 * time.Second}, nil
}

// updateSolution persists the solution back to the store.
func (r *SolutionReconciler) updateSolution(ctx context.Context, solution *v1alpha1.Solution, originalStatus v1alpha1.SolutionStatus, originalFinalizers []string) error {
	const maxRetries = 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		err := r.updateSolutionObject(ctx, solution)
		if err == nil {
			return nil
		}
		if !errors.Is(err, store.ErrConflict) {
			return err
		}
		data, getErr := r.store.Get(ctx, solutionKind, solution.ObjectMeta.Namespace, solution.ObjectMeta.Name)
		if getErr != nil {
			return fmt.Errorf("re-read solution after conflict: %w", getErr)
		}
		var latest v1alpha1.Solution
		if getErr := json.Unmarshal(data, &latest); getErr != nil {
			return fmt.Errorf("unmarshal solution after conflict: %w", getErr)
		}
		mergeSolutionStatus(&latest.Status, originalStatus, solution.Status)
		originalHasFinalizer := hasString(originalFinalizers, SolutionFinalizer)
		desiredHasFinalizer := hasString(solution.ObjectMeta.Finalizers, SolutionFinalizer)
		if originalHasFinalizer != desiredHasFinalizer {
			latest.ObjectMeta.Finalizers = mergeOwnedFinalizer(latest.ObjectMeta.Finalizers, solution.ObjectMeta.Finalizers, SolutionFinalizer, !desiredHasFinalizer)
		}
		*solution = latest
	}
	return store.ErrConflict
}

func (r *SolutionReconciler) updateSolutionObject(ctx context.Context, solution *v1alpha1.Solution) error {
	data, err := json.Marshal(solution)
	if err != nil {
		return fmt.Errorf("marshal solution: %w", err)
	}
	return r.store.Update(ctx, solutionKind, solution.ObjectMeta.Namespace, solution.ObjectMeta.Name, data, solution.ObjectMeta.ResourceVersion)
}

func snapshotSolutionStatus(status v1alpha1.SolutionStatus) v1alpha1.SolutionStatus {
	cloned := status
	cloned.Conditions = append([]v1alpha1.Condition(nil), status.Conditions...)
	cloned.CompiledResources = append([]v1alpha1.CompiledResource(nil), status.CompiledResources...)
	cloned.GeneratedResources = append([]v1alpha1.GeneratedResource(nil), status.GeneratedResources...)
	return cloned
}

func mergeSolutionStatus(latest *v1alpha1.SolutionStatus, original, desired v1alpha1.SolutionStatus) {
	if desired.Phase != original.Phase {
		latest.Phase = desired.Phase
	}
	if desired.ActiveRevision != original.ActiveRevision {
		latest.ActiveRevision = desired.ActiveRevision
	}
	if desired.CandidateRevision != original.CandidateRevision {
		latest.CandidateRevision = desired.CandidateRevision
	}
	if !reflect.DeepEqual(desired.CompiledResources, original.CompiledResources) {
		latest.CompiledResources = append([]v1alpha1.CompiledResource(nil), desired.CompiledResources...)
	}
	if !reflect.DeepEqual(desired.GeneratedResources, original.GeneratedResources) {
		latest.GeneratedResources = append([]v1alpha1.GeneratedResource(nil), desired.GeneratedResources...)
	}
	conditionType := string(lifecycle.ConditionReady)
	originalCondition := restartLadderCondition(original.Conditions, conditionType)
	desiredCondition := restartLadderCondition(desired.Conditions, conditionType)
	if !conditionsMatch(originalCondition, desiredCondition) {
		latest.Conditions = applyConditionChange(latest.Conditions, conditionType, desiredCondition)
	}
}
