// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

const (
	knowledgeBaseIndexRunKind = "KnowledgeBaseIndexRun"
	lastIndexRunAnnotation    = "kognition.dev/last-index-run"

	// conditionKBRunReady is the condition type reported on IndexRun status.
	conditionKBRunReady lifecycle.ConditionType = "IndexRunReady"
)

type reflectedIndexRun struct {
	UID         string `json:"uid,omitempty"`
	Name        string `json:"name"`
	CompletedAt string `json:"completedAt"`
}

// IndexRunLauncher is the seam between the (store-only) reconciler and the
// outside world that actually runs a one-shot ingest Job. Implementations are
// expected to be idempotent — repeated LaunchRun calls for the same run should
// converge on the same Job (identified by name+ownerRef).
type IndexRunLauncher interface {
	LaunchRun(
		ctx context.Context,
		kb *v1alpha1.KnowledgeBase,
		kbc *v1alpha1.KnowledgeBaseClass,
		run *v1alpha1.KnowledgeBaseIndexRun,
	) (IngestState, IngestStats, string /*jobName*/, error)
}

// KnowledgeBaseIndexRunReconciler reconciles KnowledgeBaseIndexRun resources.
//
// A run is a one-shot request. Its state machine:
//
//	"" | Pending → Running (Job created) → Succeeded | Failed
//
// Once terminal, the reconciler honours TTLSecondsAfterFinished by deleting
// the run when the deadline passes.
type KnowledgeBaseIndexRunReconciler struct {
	store    store.Store
	logger   *zap.Logger
	launcher IndexRunLauncher
}

// NewKnowledgeBaseIndexRunReconciler creates a new reconciler.
func NewKnowledgeBaseIndexRunReconciler(s store.Store, logger *zap.Logger) *KnowledgeBaseIndexRunReconciler {
	return &KnowledgeBaseIndexRunReconciler{
		store:  s,
		logger: logger.With(zap.String("reconciler", "knowledge-base-index-run")),
	}
}

// WithIndexRunLauncher installs a launcher that drives real ingestion. When
// nil, the reconciler still exercises the state machine but reports Skipped
// runs (useful for unit tests that don't care about Jobs).
func (r *KnowledgeBaseIndexRunReconciler) WithIndexRunLauncher(l IndexRunLauncher) *KnowledgeBaseIndexRunReconciler {
	r.launcher = l
	return r
}

// Reconcile implements Reconciler.
func (r *KnowledgeBaseIndexRunReconciler) Reconcile(ctx context.Context, req Request) (Result, error) {
	log := r.logger.With(zap.String("namespace", req.Namespace), zap.String("name", req.Name), requestid.ZapField(ctx))

	raw, err := r.store.Get(ctx, knowledgeBaseIndexRunKind, req.Namespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			log.Debug("knowledgeBaseIndexRun not found")
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("get knowledgeBaseIndexRun: %w", err)
	}

	var run v1alpha1.KnowledgeBaseIndexRun
	if err := json.Unmarshal(raw, &run); err != nil {
		return Result{}, fmt.Errorf("unmarshal knowledgeBaseIndexRun: %w", err)
	}
	originalStatus := snapshotKnowledgeBaseIndexRunStatus(run.Status)

	if run.ObjectMeta.DeletionTimestamp != nil {
		return Result{}, nil
	}

	switch run.Status.Phase {
	case "", v1alpha1.PhasePending:
		return r.startRun(ctx, &run, originalStatus, log)
	case v1alpha1.PhaseRunning:
		return r.pollRun(ctx, &run, originalStatus, log)
	case v1alpha1.PhaseSucceeded, v1alpha1.PhaseFailed:
		if run.Status.Phase == v1alpha1.PhaseSucceeded && completedRealIngest(&run) {
			if !r.reflectRunOntoKB(ctx, &run, IngestStats{
				Documents:      run.Status.DocumentCount,
				Chunks:         run.Status.IndexedChunks,
				SourceDigest:   run.Status.SourceDigest,
				BackendBinding: cloneKnowledgeBackendBinding(run.Status.BackendBinding),
			}, log) {
				return Result{RequeueAfter: 10 * time.Second}, nil
			}
		}
		return r.honorTTL(ctx, &run, log)
	default:
		log.Warn("indexRun in unknown phase, resetting to Pending", zap.String("phase", string(run.Status.Phase)))
		run.Status.Phase = v1alpha1.PhasePending
		if err := r.updateRun(ctx, &run, originalStatus); err != nil {
			return Result{}, fmt.Errorf("reset indexRun with unknown phase: %w", err)
		}
		return Result{Requeue: true}, nil
	}
}

// startRun looks up the referenced KB + class, kicks the launcher, and
// transitions the run to Running.
func (r *KnowledgeBaseIndexRunReconciler) startRun(ctx context.Context, run *v1alpha1.KnowledgeBaseIndexRun, originalStatus v1alpha1.KnowledgeBaseIndexRunStatus, log *zap.Logger) (Result, error) {
	kb, err := r.getKnowledgeBase(ctx, run.ObjectMeta.Namespace, run.Spec.KnowledgeBaseName)
	if errors.Is(err, store.ErrNotFound) {
		return r.markTerminal(ctx, run, originalStatus, v1alpha1.PhaseFailed, "KnowledgeBaseNotFound",
			fmt.Sprintf("KnowledgeBase %q not found in namespace %q", run.Spec.KnowledgeBaseName, run.ObjectMeta.Namespace),
			IngestStats{}, log)
	}
	if err != nil {
		return Result{}, fmt.Errorf("get knowledgeBase: %w", err)
	}

	kbc, err := r.getKnowledgeBaseClass(ctx, kb.Spec.KnowledgeBaseClassName)
	if errors.Is(err, store.ErrNotFound) {
		return r.markTerminal(ctx, run, originalStatus, v1alpha1.PhaseFailed, "KnowledgeBaseClassNotFound",
			fmt.Sprintf("KnowledgeBaseClass %q not found", kb.Spec.KnowledgeBaseClassName), IngestStats{}, log)
	}
	if err != nil {
		return Result{}, fmt.Errorf("get knowledgeBaseClass: %w", err)
	}

	if kbc.Spec.VectorStore == nil && kbc.Spec.BackendRef == nil {
		return r.markTerminal(ctx, run, originalStatus, v1alpha1.PhaseSucceeded, "Skipped",
			"no ingester configured for this KnowledgeBaseClass",
			IngestStats{}, log)
	}
	if r.launcher == nil {
		return r.markTerminal(ctx, run, originalStatus, v1alpha1.PhaseFailed, "IngesterUnavailable",
			"configured knowledge backend requires an ingester", IngestStats{}, log)
	}

	state, stats, jobName, err := r.launcher.LaunchRun(ctx, kb, kbc, run)
	if err != nil {
		log.Warn("launcher errored, requeueing", zap.Error(err))
		return Result{RequeueAfter: 15 * time.Second}, nil
	}

	run.Status.JobName = jobName
	run.Status.StartedAt = time.Now().UTC().Format(time.RFC3339)

	switch state {
	case IngestSucceeded:
		if err := validateIndexRunProviderConfirmation(kb, kbc, stats); err != nil {
			return r.markTerminal(ctx, run, originalStatus, v1alpha1.PhaseFailed, "ProviderConfirmationInvalid", err.Error(), stats, log)
		}
		return r.markTerminal(ctx, run, originalStatus, v1alpha1.PhaseSucceeded, "IndexRunComplete",
			fmt.Sprintf("indexed %d document(s), %d chunk(s)", stats.Documents, stats.Chunks), stats, log)
	case IngestFailed:
		return r.markTerminal(ctx, run, originalStatus, v1alpha1.PhaseFailed, "IndexRunFailed", stats.Error, stats, log)
	case IngestSkipped:
		return r.markTerminal(ctx, run, originalStatus, v1alpha1.PhaseSucceeded, "Skipped",
			"ingester reported the run as skipped", stats, log)
	case IngestRunning:
		run.Status.Phase = v1alpha1.PhaseRunning
		lc := lifecycle.SetCondition(
			apiToLifecycleConditions(run.Status.Conditions),
			conditionKBRunReady, lifecycle.ConditionFalse,
			"IndexRunRunning", "ingest Job is in progress",
		)
		lc = lifecycle.SetCondition(lc, lifecycle.ConditionProgressing, lifecycle.ConditionTrue,
			"IndexRunRunning", "ingest Job is in progress")
		run.Status.Conditions = lifecycleToAPIConditions(lc)
		if err := r.updateRun(ctx, run, originalStatus); err != nil {
			return Result{}, fmt.Errorf("update run to Running: %w", err)
		}
		return Result{RequeueAfter: 10 * time.Second}, nil
	default:
		log.Warn("launcher returned unexpected ingest state during launch; requeueing", zap.Int("state", int(state)))
		return Result{RequeueAfter: 15 * time.Second}, nil
	}
}

// pollRun re-asks the launcher for the current state of the run's Job.
func (r *KnowledgeBaseIndexRunReconciler) pollRun(ctx context.Context, run *v1alpha1.KnowledgeBaseIndexRun, originalStatus v1alpha1.KnowledgeBaseIndexRunStatus, log *zap.Logger) (Result, error) {
	kb, err := r.getKnowledgeBase(ctx, run.ObjectMeta.Namespace, run.Spec.KnowledgeBaseName)
	if errors.Is(err, store.ErrNotFound) {
		return r.markTerminal(ctx, run, originalStatus, v1alpha1.PhaseFailed, "KnowledgeBaseDeleted",
			"KnowledgeBase disappeared while the run was in progress", IngestStats{}, log)
	}
	if err != nil {
		return Result{}, fmt.Errorf("get knowledgeBase: %w", err)
	}

	kbc, err := r.getKnowledgeBaseClass(ctx, kb.Spec.KnowledgeBaseClassName)
	if errors.Is(err, store.ErrNotFound) {
		kbc = nil
	}
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return Result{}, fmt.Errorf("get knowledgeBaseClass: %w", err)
	}

	if kbc != nil && kbc.Spec.VectorStore == nil && kbc.Spec.BackendRef == nil {
		return r.markTerminal(ctx, run, originalStatus, v1alpha1.PhaseSucceeded, "Skipped",
			"no ingester configured for this KnowledgeBaseClass", IngestStats{}, log)
	}
	if r.launcher == nil {
		return r.markTerminal(ctx, run, originalStatus, v1alpha1.PhaseFailed, "IngesterUnavailable",
			"configured knowledge backend requires an ingester", IngestStats{}, log)
	}

	state, stats, jobName, err := r.launcher.LaunchRun(ctx, kb, kbc, run)
	if err != nil {
		log.Warn("launcher errored during poll, requeueing", zap.Error(err))
		return Result{RequeueAfter: 15 * time.Second}, nil
	}
	jobNameDiscovered := jobName != "" && run.Status.JobName == ""
	if jobNameDiscovered {
		run.Status.JobName = jobName
	}

	switch state {
	case IngestRunning:
		if jobNameDiscovered {
			if err := r.updateRun(ctx, run, originalStatus); err != nil {
				return Result{}, fmt.Errorf("persist discovered Job name: %w", err)
			}
		}
		return Result{RequeueAfter: 10 * time.Second}, nil
	case IngestSucceeded:
		if err := validateIndexRunProviderConfirmation(kb, kbc, stats); err != nil {
			return r.markTerminal(ctx, run, originalStatus, v1alpha1.PhaseFailed, "ProviderConfirmationInvalid", err.Error(), stats, log)
		}
		return r.markTerminal(ctx, run, originalStatus, v1alpha1.PhaseSucceeded, "IndexRunComplete",
			fmt.Sprintf("indexed %d document(s), %d chunk(s)", stats.Documents, stats.Chunks), stats, log)
	case IngestFailed:
		return r.markTerminal(ctx, run, originalStatus, v1alpha1.PhaseFailed, "IndexRunFailed", stats.Error, stats, log)
	case IngestSkipped:
		return r.markTerminal(ctx, run, originalStatus, v1alpha1.PhaseSucceeded, "Skipped",
			"ingester reported the run as skipped", stats, log)
	default:
		log.Warn("launcher returned unexpected ingest state; requeueing", zap.Int("state", int(state)))
		return Result{RequeueAfter: 15 * time.Second}, nil
	}
}

// markTerminal writes the terminal phase, stats, and conditions.
func (r *KnowledgeBaseIndexRunReconciler) markTerminal(
	ctx context.Context,
	run *v1alpha1.KnowledgeBaseIndexRun,
	originalStatus v1alpha1.KnowledgeBaseIndexRunStatus,
	phase v1alpha1.Phase,
	reason, message string,
	stats IngestStats,
	log *zap.Logger,
) (Result, error) {
	run.Status.Phase = phase
	run.Status.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if run.Status.StartedAt == "" {
		run.Status.StartedAt = run.Status.CompletedAt
	}
	run.Status.DocumentCount = stats.Documents
	run.Status.IndexedChunks = stats.Chunks
	run.Status.SourceDigest = stats.SourceDigest
	run.Status.BackendBinding = cloneKnowledgeBackendBinding(stats.BackendBinding)
	run.Status.Message = message

	condStatus := lifecycle.ConditionTrue
	if phase == v1alpha1.PhaseFailed {
		condStatus = lifecycle.ConditionFalse
	}
	lc := lifecycle.SetCondition(
		apiToLifecycleConditions(run.Status.Conditions),
		conditionKBRunReady, condStatus, reason, message,
	)
	lc = lifecycle.SetCondition(lc, lifecycle.ConditionProgressing, lifecycle.ConditionFalse, reason, message)
	run.Status.Conditions = lifecycleToAPIConditions(lc)

	if err := r.updateRun(ctx, run, originalStatus); err != nil {
		return Result{}, fmt.Errorf("mark run terminal: %w", err)
	}
	// Reflect a successful *real* ingest onto the target KnowledgeBase so an
	// explicit reindex updates the KB (e.g. Failed → Ready), not just the run
	// record. Failed reflection is retried after the terminal status is durable.
	if phase == v1alpha1.PhaseSucceeded && reason == "IndexRunComplete" {
		if !r.reflectRunOntoKB(ctx, run, stats, log) {
			return Result{RequeueAfter: 10 * time.Second}, nil
		}
	}
	log.Info("index run terminal",
		zap.String("phase", string(phase)),
		zap.Int64("documents", run.Status.DocumentCount),
		zap.Int64("chunks", run.Status.IndexedChunks),
	)
	return r.honorTTL(ctx, run, log)
}

// reflectRunOntoKB writes a successful run's stats onto the target
// KnowledgeBase so an explicit reindex converges the KB status too (Failed /
// Indexing → Ready), which in turn lets the claim reconciler bind dependent
// claims. Reflection identity and completion time make retries idempotent and
// prevent an older concurrent run from overwriting a newer completion.
func (r *KnowledgeBaseIndexRunReconciler) reflectRunOntoKB(ctx context.Context, run *v1alpha1.KnowledgeBaseIndexRun, stats IngestStats, log *zap.Logger) bool {
	kb, err := r.getKnowledgeBase(ctx, run.ObjectMeta.Namespace, run.Spec.KnowledgeBaseName)
	if err != nil {
		return errors.Is(err, store.ErrNotFound)
	}
	marker := reflectedIndexRun{
		UID:         run.ObjectMeta.UID,
		Name:        run.ObjectMeta.Name,
		CompletedAt: run.Status.CompletedAt,
	}
	if reflectedRunIsCurrent(kb.ObjectMeta.Annotations[lastIndexRunAnnotation], marker) {
		return true
	}
	if stats.BackendBinding != nil {
		kbc, err := r.getKnowledgeBaseClass(ctx, kb.Spec.KnowledgeBaseClassName)
		if errors.Is(err, store.ErrNotFound) {
			if err := validateRecoveredProviderConfirmation(kb, stats.BackendBinding); err != nil {
				log.Warn("reflect run onto KB: recovered provider confirmation is invalid", zap.Error(err))
				return false
			}
		} else if err != nil {
			return false
		} else if err := validateIndexRunProviderConfirmation(kb, kbc, stats); err != nil {
			log.Warn("reflect run onto KB: provider confirmation is no longer valid", zap.Error(err))
			return false
		}
	}
	if kb.ObjectMeta.Annotations == nil {
		kb.ObjectMeta.Annotations = make(map[string]string)
	}
	markerJSON, err := json.Marshal(marker)
	if err != nil {
		log.Warn("reflect run onto KB: marshal marker failed", zap.Error(err))
		return false
	}
	kb.ObjectMeta.Annotations[lastIndexRunAnnotation] = string(markerJSON)
	kb.Status.DocumentCount = stats.Documents
	kb.Status.IndexedChunks = stats.Chunks
	kb.Status.Generation++
	if stats.BackendBinding != nil {
		kb.Status.BackendBinding = cloneKnowledgeBackendBinding(stats.BackendBinding)
	}
	kb.Status.LastRefresh = time.Now().UTC().Format(time.RFC3339)
	kb.Status.Phase = v1alpha1.PhaseReady
	lc := lifecycle.SetCondition(
		apiToLifecycleConditions(kb.Status.Conditions),
		lifecycle.ConditionReady, lifecycle.ConditionTrue,
		"IndexRunComplete",
		fmt.Sprintf("index run %q indexed %d document(s), %d chunk(s)",
			run.ObjectMeta.Name, kb.Status.DocumentCount, kb.Status.IndexedChunks),
	)
	lc = lifecycle.SetCondition(lc, lifecycle.ConditionProgressing, lifecycle.ConditionFalse, "Idle", "index run complete")
	kb.Status.Conditions = lifecycleToAPIConditions(lc)

	b, err := json.Marshal(kb)
	if err != nil {
		log.Warn("reflect run onto KB: marshal failed", zap.Error(err))
		return false
	}
	if err := r.store.Update(ctx, knowledgeBaseKind, kb.ObjectMeta.Namespace, kb.ObjectMeta.Name, b, kb.ObjectMeta.ResourceVersion); err != nil {
		log.Warn("reflect run onto KB: update failed; retrying", zap.Error(err))
		return false
	}
	log.Info("reflected successful index run onto KnowledgeBase",
		zap.String("kb", kb.ObjectMeta.Name),
		zap.Int64("chunks", kb.Status.IndexedChunks))
	return true
}

func reflectedRunIsCurrent(raw string, candidate reflectedIndexRun) bool {
	var current reflectedIndexRun
	if json.Unmarshal([]byte(raw), &current) != nil {
		return false
	}
	if current.UID != "" && candidate.UID != "" && current.UID == candidate.UID {
		return true
	}
	if current.UID == "" && candidate.UID == "" && current.Name == candidate.Name && current.CompletedAt == candidate.CompletedAt {
		return true
	}
	currentCompleted, currentErr := time.Parse(time.RFC3339Nano, current.CompletedAt)
	candidateCompleted, candidateErr := time.Parse(time.RFC3339Nano, candidate.CompletedAt)
	if currentErr != nil || candidateErr != nil {
		return false
	}
	if currentCompleted.After(candidateCompleted) {
		return true
	}
	if currentCompleted.Equal(candidateCompleted) {
		// UIDs provide a stable total order when concurrent completions have the
		// same timestamp; the empty UID sorts before every assigned UID.
		return current.UID >= candidate.UID
	}
	return false
}

func completedRealIngest(run *v1alpha1.KnowledgeBaseIndexRun) bool {
	for _, condition := range run.Status.Conditions {
		if condition.Type == string(conditionKBRunReady) && condition.Reason == "IndexRunComplete" {
			return true
		}
	}
	return false
}

func validateIndexRunProviderConfirmation(kb *v1alpha1.KnowledgeBase, kbc *v1alpha1.KnowledgeBaseClass, stats IngestStats) error {
	if kbc == nil {
		return validateRecoveredProviderConfirmation(kb, stats.BackendBinding)
	}
	if kbc.Spec.BackendRef == nil {
		return nil
	}
	return validateProviderIngestConfirmation(kb, kbc, stats.BackendBinding)
}

func validateRecoveredProviderConfirmation(kb *v1alpha1.KnowledgeBase, binding *v1alpha1.KnowledgeBackendBinding) error {
	if binding == nil || binding.Generation != kb.Status.Generation+1 || binding.ProviderName == "" ||
		!validKnowledgeReclaimPolicy(binding.ReclaimPolicy) || binding.ClassUID == "" || binding.ClassGeneration < 1 ||
		binding.ConfigurationAPIGroup == "" || binding.ConfigurationKind == "" || binding.ConfigurationName == "" ||
		binding.ConfigurationUID == "" || binding.ConfigurationDigest == "" || binding.OperationID == "" || binding.BackendOperationID == "" {
		return fmt.Errorf("recovered knowledge provider confirmation is not a deletion-safe immutable binding")
	}
	return nil
}

func knowledgeBackendBindingsEqual(left, right *v1alpha1.KnowledgeBackendBinding) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

// honorTTL removes the run if TTLSecondsAfterFinished has elapsed; otherwise
// requeues at the appropriate time.
func (r *KnowledgeBaseIndexRunReconciler) honorTTL(ctx context.Context, run *v1alpha1.KnowledgeBaseIndexRun, log *zap.Logger) (Result, error) {
	if run.Spec.TTLSecondsAfterFinished == nil || run.Status.CompletedAt == "" {
		return Result{}, nil
	}
	completedAt, err := time.Parse(time.RFC3339Nano, run.Status.CompletedAt)
	if err != nil {
		log.Warn("cannot parse completedAt, skipping TTL", zap.Error(err))
		return Result{}, nil
	}
	deadline := completedAt.Add(time.Duration(*run.Spec.TTLSecondsAfterFinished) * time.Second)
	remaining := time.Until(deadline)
	if remaining <= 0 {
		if err := r.store.Delete(ctx, knowledgeBaseIndexRunKind, run.ObjectMeta.Namespace, run.ObjectMeta.Name); err != nil && !errors.Is(err, store.ErrNotFound) {
			return Result{}, fmt.Errorf("delete expired run: %w", err)
		}
		log.Info("index run TTL expired, deleted")
		return Result{}, nil
	}
	return Result{RequeueAfter: remaining}, nil
}

func (r *KnowledgeBaseIndexRunReconciler) getKnowledgeBase(ctx context.Context, namespace, name string) (*v1alpha1.KnowledgeBase, error) {
	raw, err := r.store.Get(ctx, knowledgeBaseKind, namespace, name)
	if err != nil {
		return nil, err
	}
	var kb v1alpha1.KnowledgeBase
	if err := json.Unmarshal(raw, &kb); err != nil {
		return nil, fmt.Errorf("unmarshal knowledgeBase: %w", err)
	}
	return &kb, nil
}

func (r *KnowledgeBaseIndexRunReconciler) getKnowledgeBaseClass(ctx context.Context, name string) (*v1alpha1.KnowledgeBaseClass, error) {
	raw, err := r.store.Get(ctx, knowledgeBaseClassKind, "", name)
	if err != nil {
		return nil, err
	}
	var kbc v1alpha1.KnowledgeBaseClass
	if err := json.Unmarshal(raw, &kbc); err != nil {
		return nil, fmt.Errorf("unmarshal knowledgeBaseClass: %w", err)
	}
	return &kbc, nil
}

func (r *KnowledgeBaseIndexRunReconciler) updateRun(ctx context.Context, run *v1alpha1.KnowledgeBaseIndexRun, originalStatus v1alpha1.KnowledgeBaseIndexRunStatus) error {
	const maxRetries = 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		err := r.updateRunObject(ctx, run)
		if err == nil {
			return nil
		}
		if !errors.Is(err, store.ErrConflict) {
			return err
		}
		raw, getErr := r.store.Get(ctx, knowledgeBaseIndexRunKind, run.ObjectMeta.Namespace, run.ObjectMeta.Name)
		if getErr != nil {
			return fmt.Errorf("re-read run after conflict: %w", getErr)
		}
		var latest v1alpha1.KnowledgeBaseIndexRun
		if getErr := json.Unmarshal(raw, &latest); getErr != nil {
			return fmt.Errorf("unmarshal run after conflict: %w", getErr)
		}
		if isTerminal(latest.Status.Phase) && !isTerminal(run.Status.Phase) {
			*run = latest
			return nil
		}
		mergeKnowledgeBaseIndexRunStatus(&latest.Status, originalStatus, run.Status)
		*run = latest
	}
	return store.ErrConflict
}

func (r *KnowledgeBaseIndexRunReconciler) updateRunObject(ctx context.Context, run *v1alpha1.KnowledgeBaseIndexRun) error {
	b, err := json.Marshal(run)
	if err != nil {
		return fmt.Errorf("marshal run: %w", err)
	}
	return r.store.Update(ctx, knowledgeBaseIndexRunKind, run.ObjectMeta.Namespace, run.ObjectMeta.Name, b, run.ObjectMeta.ResourceVersion)
}

func snapshotKnowledgeBaseIndexRunStatus(status v1alpha1.KnowledgeBaseIndexRunStatus) v1alpha1.KnowledgeBaseIndexRunStatus {
	cloned := status
	cloned.BackendBinding = cloneKnowledgeBackendBinding(status.BackendBinding)
	cloned.Conditions = append([]v1alpha1.Condition(nil), status.Conditions...)
	return cloned
}

func mergeKnowledgeBaseIndexRunStatus(latest *v1alpha1.KnowledgeBaseIndexRunStatus, original, desired v1alpha1.KnowledgeBaseIndexRunStatus) {
	if desired.Phase != original.Phase {
		latest.Phase = desired.Phase
	}
	executionIdentityChanged := desired.JobName != original.JobName || desired.StartedAt != original.StartedAt
	executionIdentityUnchanged := latest.JobName == original.JobName && latest.StartedAt == original.StartedAt
	if executionIdentityChanged && executionIdentityUnchanged {
		latest.JobName = desired.JobName
		latest.StartedAt = desired.StartedAt
	}
	if desired.CompletedAt != original.CompletedAt {
		latest.CompletedAt = desired.CompletedAt
	}
	if desired.DocumentCount != original.DocumentCount {
		latest.DocumentCount = desired.DocumentCount
	}
	if desired.IndexedChunks != original.IndexedChunks {
		latest.IndexedChunks = desired.IndexedChunks
	}
	if desired.SourceDigest != original.SourceDigest {
		latest.SourceDigest = desired.SourceDigest
	}
	if !knowledgeBackendBindingsEqual(desired.BackendBinding, original.BackendBinding) {
		latest.BackendBinding = cloneKnowledgeBackendBinding(desired.BackendBinding)
	}
	if desired.Message != original.Message {
		latest.Message = desired.Message
	}
	for _, conditionType := range []string{string(conditionKBRunReady), string(lifecycle.ConditionProgressing)} {
		originalCondition := restartLadderCondition(original.Conditions, conditionType)
		desiredCondition := restartLadderCondition(desired.Conditions, conditionType)
		if !conditionsMatch(originalCondition, desiredCondition) {
			latest.Conditions = applyConditionChange(latest.Conditions, conditionType, desiredCondition)
		}
	}
}
