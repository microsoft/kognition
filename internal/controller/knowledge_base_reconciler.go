// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

const (
	// KnowledgeBaseFinalizer is applied to KnowledgeBase resources so the
	// reconciler can honour the class reclaimPolicy before the object is removed.
	KnowledgeBaseFinalizer = "kognition.dev/knowledgebase-cleanup"

	knowledgeBaseKind      = "KnowledgeBase"
	knowledgeBaseClassKind = "KnowledgeBaseClass"

	// conditionKBReady is the agent condition type set when required KB refs
	// are not in the Ready phase.
	conditionKBReady lifecycle.ConditionType = "KnowledgeBaseReady"
)

var errKnowledgeBaseClassNotFound = errors.New("knowledge base class not found")

var errRefreshScheduleCollision = errors.New("knowledge base refresh schedule name collision")

// IngestState reports the current state of an out-of-process ingestion driven
// by an IngesterLauncher.
type IngestState int

const (
	// IngestSkipped means the KnowledgeBaseClass has no vectorStore
	// configured. The reconciler falls back to its built-in "simulated"
	// bookkeeping in this mode.
	IngestSkipped IngestState = iota
	// IngestRunning means a Job (or equivalent) is in-flight; the reconciler
	// should keep the KB in the Indexing phase and requeue.
	IngestRunning
	// IngestSucceeded means the Job completed cleanly; the reconciler should
	// transition the KB to Ready using IngestStats.
	IngestSucceeded
	// IngestFailed means the Job terminated with a non-zero status; the
	// reconciler should mark the KB Failed.
	IngestFailed
)

// IngestStats carries the counts returned by a successful ingester run so the
// reconciler can populate KB status faithfully instead of using synthetic
// numbers.
type IngestStats struct {
	Documents    int64
	Chunks       int64
	SourceDigest string
	Error        string
	// BackendBinding is the provider's immutable confirmation of the staged
	// generation. It is required for provider-backed ingestion and is published
	// atomically with counts and generation after successful validation.
	BackendBinding *v1alpha1.KnowledgeBackendBinding
}

// IngesterLauncher is the seam between the (store-only) KnowledgeBaseReconciler
// and the outside world that actually runs ingestion. Implementations are
// expected to be idempotent — repeated Ensure calls for the same (kb,
// generation) tuple should converge to the same Job.
type IngesterLauncher interface {
	Ensure(ctx context.Context, kb *v1alpha1.KnowledgeBase, kbc *v1alpha1.KnowledgeBaseClass) (IngestState, IngestStats, error)

	// Reset clears any terminal ingest artifact (e.g. a failed Job) for the
	// KB's pending generation so a subsequent Ensure starts fresh. Used by the
	// reconciler to retry a Failed KB. Idempotent; a no-op when nothing exists.
	Reset(ctx context.Context, kb *v1alpha1.KnowledgeBase) error
}

type KnowledgeBackendCleaner interface {
	Cleanup(context.Context, *v1alpha1.KnowledgeBase, v1alpha1.ReclaimPolicy) error
}

type RetainedKnowledgeBindingRecorder interface {
	Record(context.Context, *v1alpha1.KnowledgeBase) error
}

// kbFailedRetryInterval is how long the reconciler waits after a transient
// ingest failure before automatically retrying. Without this a KB whose first
// ingest lost a startup race (its Job ran before the source Service or vector
// store was Ready) would stay Failed forever.
const kbFailedRetryInterval = 2 * time.Minute

// KnowledgeBaseReconciler reconciles KnowledgeBase resources.
//
// On each reconciliation it:
//  1. Adds a finalizer so it can honour reclaimPolicy during deletion.
//  2. Drives the KB through its lifecycle phases:
//     empty/Pending → Indexing → Ready (generation incremented on completion).
//  3. Handles "Refreshing" triggered externally by incrementing generation.
//  4. On deletion, applies the class reclaimPolicy (Delete/Retain/Archive).
//  5. Updates the "KnowledgeBaseReady" condition on any Agent in the same
//     namespace that holds a Required=true reference to this KB.
type KnowledgeBaseReconciler struct {
	store           store.Store
	logger          *zap.Logger
	launcher        IngesterLauncher
	cleaner         KnowledgeBackendCleaner
	bindingRecorder RetainedKnowledgeBindingRecorder
}

// NewKnowledgeBaseReconciler creates a new KnowledgeBaseReconciler.
func NewKnowledgeBaseReconciler(s store.Store, logger *zap.Logger) *KnowledgeBaseReconciler {
	return &KnowledgeBaseReconciler{
		store:  s,
		logger: logger.With(zap.String("reconciler", "knowledge-base")),
	}
}

// WithIngesterLauncher installs a launcher that drives real ingestion (e.g.
// via a batchv1.Job in a Kubernetes cluster). When the KB's class has no
// vectorStore, the reconciler falls back to its
// built-in simulated bookkeeping so unit tests that don't care about ingestion
// keep working unchanged.
func (r *KnowledgeBaseReconciler) WithIngesterLauncher(l IngesterLauncher) *KnowledgeBaseReconciler {
	r.launcher = l
	return r
}

func (r *KnowledgeBaseReconciler) WithBackendCleaner(cleaner KnowledgeBackendCleaner) *KnowledgeBaseReconciler {
	r.cleaner = cleaner
	return r
}

func (r *KnowledgeBaseReconciler) WithRetainedBindingRecorder(recorder RetainedKnowledgeBindingRecorder) *KnowledgeBaseReconciler {
	r.bindingRecorder = recorder
	return r
}

// Reconcile implements the Reconciler interface for KnowledgeBase resources.
func (r *KnowledgeBaseReconciler) Reconcile(ctx context.Context, req Request) (Result, error) {
	log := r.logger.With(zap.String("namespace", req.Namespace), zap.String("name", req.Name), requestid.ZapField(ctx))

	raw, err := r.store.Get(ctx, knowledgeBaseKind, req.Namespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			log.Debug("knowledgebase not found, likely deleted")
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("get knowledgebase: %w", err)
	}

	var kb v1alpha1.KnowledgeBase
	if err := json.Unmarshal(raw, &kb); err != nil {
		return Result{}, fmt.Errorf("unmarshal knowledgebase: %w", err)
	}

	if kb.ObjectMeta.DeletionTimestamp != nil {
		return r.handleDeletion(ctx, &kb, log)
	}

	if !lifecycle.HasFinalizer(kb.ObjectMeta.Finalizers, KnowledgeBaseFinalizer) {
		finalizers, _ := lifecycle.AddFinalizer(kb.ObjectMeta.Finalizers, KnowledgeBaseFinalizer)
		kb.ObjectMeta.Finalizers = finalizers
		if err := r.updateKB(ctx, &kb); err != nil {
			return Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		return Result{Requeue: true}, nil
	}
	if err := r.reconcileRefreshSchedule(ctx, &kb); err != nil {
		reason := "RefreshScheduleError"
		if errors.Is(err, errRefreshScheduleCollision) {
			reason = "RefreshScheduleCollision"
		}
		return r.markFailed(ctx, &kb, reason, err.Error(), log)
	}
	if kb.Status.Phase == v1alpha1.PhaseFailed && isRefreshScheduleFailure(kb.Status.Conditions) {
		kb.Status.Phase = v1alpha1.PhasePending
		lc := lifecycle.SetCondition(
			apiToLifecycleConditions(kb.Status.Conditions),
			lifecycle.ConditionReady,
			lifecycle.ConditionFalse,
			"RefreshScheduleRepaired",
			"refresh schedule is available; retrying knowledge base reconciliation",
		)
		lc = lifecycle.SetCondition(lc, lifecycle.ConditionProgressing, lifecycle.ConditionTrue, "RefreshScheduleRepaired", "refresh schedule repaired")
		kb.Status.Conditions = lifecycleToAPIConditions(lc)
		if err := r.updateKB(ctx, &kb); err != nil {
			return Result{}, fmt.Errorf("reset repaired refresh schedule failure: %w", err)
		}
		return Result{Requeue: true}, nil
	}

	switch kb.Status.Phase {
	case "", v1alpha1.PhasePending:
		return r.startIndexing(ctx, &kb, log)
	case v1alpha1.PhaseIndexing:
		return r.completeIndexing(ctx, &kb, log)
	case v1alpha1.PhaseRefreshing:
		return r.completeRefreshing(ctx, &kb, log)
	case v1alpha1.PhaseReady:
		// Verify class still exists; no-op if everything is healthy.
		return r.verifyReady(ctx, &kb, log)
	case v1alpha1.PhaseFailed:
		return r.retryFailed(ctx, &kb, log)
	default:
		log.Warn("knowledgebase in unknown phase, resetting to Pending",
			zap.String("phase", string(kb.Status.Phase)))
		kb.Status.Phase = v1alpha1.PhasePending
		if err := r.updateKB(ctx, &kb); err != nil {
			return Result{}, fmt.Errorf("reset knowledgebase with unknown phase: %w", err)
		}
		return Result{Requeue: true}, nil
	}
}

// startIndexing transitions the KB from empty/Pending to Indexing.
func (r *KnowledgeBaseReconciler) startIndexing(ctx context.Context, kb *v1alpha1.KnowledgeBase, log *zap.Logger) (Result, error) {
	log.Info("starting indexing", zap.String("class", kb.Spec.KnowledgeBaseClassName))

	kb.Status.Phase = v1alpha1.PhaseIndexing
	lc := lifecycle.SetCondition(
		apiToLifecycleConditions(kb.Status.Conditions),
		lifecycle.ConditionReady,
		lifecycle.ConditionFalse,
		"Indexing",
		"ingesting sources into the vector index",
	)
	lc = lifecycle.SetCondition(lc, lifecycle.ConditionProgressing, lifecycle.ConditionTrue, "Indexing", "indexing in progress")
	kb.Status.Conditions = lifecycleToAPIConditions(lc)

	if err := r.updateKB(ctx, kb); err != nil {
		return Result{}, fmt.Errorf("set indexing phase: %w", err)
	}
	return Result{Requeue: true}, nil
}

// runIngester consults the installed IngesterLauncher and, if applicable,
// returns its state + stats. Returns IngestSkipped only when the
// KnowledgeBaseClass has no vectorStore configured —
// in that case callers fall back to the built-in simulated bookkeeping.
func (r *KnowledgeBaseReconciler) runIngester(ctx context.Context, kb *v1alpha1.KnowledgeBase) (IngestState, IngestStats, error) {
	kbc, err := r.getKnowledgeBaseClass(ctx, kb.Spec.KnowledgeBaseClassName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return IngestSkipped, IngestStats{}, fmt.Errorf("%w: %s", errKnowledgeBaseClassNotFound, kb.Spec.KnowledgeBaseClassName)
		}
		return IngestSkipped, IngestStats{}, err
	}
	if kbc == nil || (kbc.Spec.VectorStore == nil && kbc.Spec.BackendRef == nil) {
		return IngestSkipped, IngestStats{}, nil
	}
	if r.launcher == nil {
		return IngestFailed, IngestStats{Error: "configured knowledge backend requires an ingester; install and enable its provider"}, nil
	}
	state, stats, err := r.launcher.Ensure(ctx, kb, kbc)
	if err != nil {
		return state, stats, err
	}
	if state == IngestSucceeded && kbc.Spec.BackendRef != nil {
		if err := validateProviderIngestConfirmation(kb, kbc, stats.BackendBinding); err != nil {
			return IngestFailed, IngestStats{Error: err.Error()}, nil
		}
		stats.BackendBinding.ReclaimPolicy = effectiveKnowledgeReclaimPolicy(kbc.Spec.ReclaimPolicy)
	}
	switch state {
	case IngestRunning, IngestSucceeded, IngestFailed:
		return state, stats, nil
	case IngestSkipped:
		return IngestFailed, IngestStats{Error: "installed ingester does not support the configured knowledge backend; check provider configuration"}, nil
	default:
		return IngestFailed, IngestStats{Error: fmt.Sprintf("knowledge ingester returned invalid state %d; check provider compatibility", state)}, nil
	}
}

func effectiveKnowledgeReclaimPolicy(policy v1alpha1.ReclaimPolicy) v1alpha1.ReclaimPolicy {
	if policy == "" {
		return v1alpha1.ReclaimDelete
	}
	return policy
}

func validateProviderIngestConfirmation(kb *v1alpha1.KnowledgeBase, kbc *v1alpha1.KnowledgeBaseClass, binding *v1alpha1.KnowledgeBackendBinding) error {
	if binding == nil {
		return fmt.Errorf("knowledge provider did not confirm an immutable backend binding")
	}
	if binding.Generation != kb.Status.Generation+1 || binding.ProviderName != kbc.Spec.Provisioner ||
		binding.ClassUID != kbc.ObjectMeta.UID || binding.ClassGeneration != kbc.ObjectMeta.Generation ||
		binding.ConfigurationAPIGroup == "" || binding.ConfigurationKind == "" || binding.ConfigurationName == "" ||
		binding.ConfigurationUID == "" || binding.ConfigurationDigest == "" || binding.OperationID == "" || binding.BackendOperationID == "" {
		return fmt.Errorf("knowledge provider confirmation does not match the requested immutable binding")
	}
	return nil
}

func cloneKnowledgeBackendBinding(binding *v1alpha1.KnowledgeBackendBinding) *v1alpha1.KnowledgeBackendBinding {
	if binding == nil {
		return nil
	}
	copy := *binding
	return &copy
}

// markFailed transitions the KB into Failed with a reason/message and notifies
// dependent agents.
func (r *KnowledgeBaseReconciler) markFailed(ctx context.Context, kb *v1alpha1.KnowledgeBase, reason, message string, log *zap.Logger) (Result, error) {
	kb.Status.Phase = v1alpha1.PhaseFailed
	lc := lifecycle.SetCondition(
		apiToLifecycleConditions(kb.Status.Conditions),
		lifecycle.ConditionReady,
		lifecycle.ConditionFalse,
		reason,
		message,
	)
	lc = lifecycle.SetCondition(lc, lifecycle.ConditionProgressing, lifecycle.ConditionFalse, reason, message)
	kb.Status.Conditions = lifecycleToAPIConditions(lc)
	if err := r.updateKB(ctx, kb); err != nil {
		return Result{}, fmt.Errorf("mark kb failed: %w", err)
	}
	if err := r.updateAgentKBConditions(ctx, kb, false, log); err != nil {
		log.Warn("failed to update agent KB conditions on failure", zap.Error(err))
	}
	return Result{RequeueAfter: 30 * time.Second}, nil
}

// retryFailed automatically re-attempts ingestion for a KB that previously
// failed, but only for transient ingest failures (reasons IngestFailed /
// RefreshFailed) — not e.g. a removed class. It waits kbFailedRetryInterval
// since the failure, clears the terminal ingest Job via the launcher, and
// transitions back to Pending so the next reconcile re-indexes. This lets a KB
// that lost a startup race self-heal instead of staying Failed forever.
func (r *KnowledgeBaseReconciler) retryFailed(ctx context.Context, kb *v1alpha1.KnowledgeBase, log *zap.Logger) (Result, error) {
	ready := findConditionByType(kb.Status.Conditions, string(lifecycle.ConditionReady))
	// Only auto-retry transient ingest failures. Other failures (e.g. the
	// KnowledgeBaseClass was removed) must not be papered over by re-indexing.
	if ready == nil || (ready.Reason != "IngestFailed" && ready.Reason != "RefreshFailed") {
		return Result{RequeueAfter: 30 * time.Second}, nil
	}
	// No external ingester → nothing to retry (simulated bookkeeping can't fail).
	if r.launcher == nil {
		return Result{RequeueAfter: 30 * time.Second}, nil
	}
	// Back off since the last failure so a genuinely broken source isn't hammered.
	if !ready.LastTransitionTime.IsZero() {
		if since := time.Since(ready.LastTransitionTime); since < kbFailedRetryInterval {
			return Result{RequeueAfter: kbFailedRetryInterval - since}, nil
		}
	}
	// Clear the terminal Job so Ensure starts a fresh one, then re-index.
	if err := r.launcher.Reset(ctx, kb); err != nil {
		log.Warn("failed to reset ingest job for retry, will requeue", zap.Error(err))
		return Result{RequeueAfter: kbFailedRetryInterval}, nil
	}
	log.Info("retrying failed knowledgebase ingest")
	return r.startIndexing(ctx, kb, log)
}

// findConditionByType returns a pointer to the condition of the given type, or nil.
func findConditionByType(conds []v1alpha1.Condition, condType string) *v1alpha1.Condition {
	for i := range conds {
		if conds[i].Type == condType {
			return &conds[i]
		}
	}
	return nil
}

// completeIndexing simulates source ingestion, then transitions to Ready.
// Generation is incremented only after a successful transition to Ready.
func (r *KnowledgeBaseReconciler) completeIndexing(ctx context.Context, kb *v1alpha1.KnowledgeBase, log *zap.Logger) (Result, error) {
	state, stats, err := r.runIngester(ctx, kb)
	if err != nil {
		if errors.Is(err, errKnowledgeBaseClassNotFound) {
			return r.markFailed(ctx, kb, "ClassNotFound", fmt.Sprintf("KnowledgeBaseClass %q no longer exists", kb.Spec.KnowledgeBaseClassName), log)
		}
		log.Warn("ingester launcher errored, requeueing", zap.Error(err))
		return Result{RequeueAfter: 15 * time.Second}, nil
	}
	switch state {
	case IngestRunning:
		return Result{RequeueAfter: 10 * time.Second}, nil
	case IngestFailed:
		return r.markFailed(ctx, kb, "IngestFailed", stats.Error, log)
	case IngestSucceeded:
		kb.Status.DocumentCount = stats.Documents
		kb.Status.IndexedChunks = stats.Chunks
		if stats.BackendBinding != nil {
			kb.Status.BackendBinding = cloneKnowledgeBackendBinding(stats.BackendBinding)
		}
	case IngestSkipped:
		docCount := int64(len(kb.Spec.Sources))
		if docCount == 0 {
			docCount = 1 // at least one synthetic document for empty source lists
		}
		kb.Status.DocumentCount = docCount
		kb.Status.IndexedChunks = docCount * 10
	}

	kb.Status.Generation++
	kb.Status.LastRefresh = time.Now().UTC().Format(time.RFC3339)
	kb.Status.Phase = v1alpha1.PhaseReady

	lc := lifecycle.SetCondition(
		apiToLifecycleConditions(kb.Status.Conditions),
		lifecycle.ConditionReady,
		lifecycle.ConditionTrue,
		"IndexingComplete",
		fmt.Sprintf("indexed %d document(s), %d chunk(s)", kb.Status.DocumentCount, kb.Status.IndexedChunks),
	)
	lc = lifecycle.SetCondition(lc, lifecycle.ConditionProgressing, lifecycle.ConditionFalse, "Idle", "indexing complete")
	kb.Status.Conditions = lifecycleToAPIConditions(lc)

	if err := r.updateKB(ctx, kb); err != nil {
		return Result{}, fmt.Errorf("complete indexing: %w", err)
	}
	log.Info("indexing complete",
		zap.Int64("documentCount", kb.Status.DocumentCount),
		zap.Int64("indexedChunks", kb.Status.IndexedChunks),
		zap.Int64("generation", kb.Status.Generation),
	)

	// Notify agents that required this KB.
	if err := r.updateAgentKBConditions(ctx, kb, true, log); err != nil {
		log.Warn("failed to update agent KB conditions", zap.Error(err))
	}

	return Result{}, nil
}

// completeRefreshing is equivalent to completeIndexing but preserves the
// current Generation until the new index is ready, then increments it.
func (r *KnowledgeBaseReconciler) completeRefreshing(ctx context.Context, kb *v1alpha1.KnowledgeBase, log *zap.Logger) (Result, error) {
	log.Info("completing refresh", zap.Int64("currentGeneration", kb.Status.Generation))

	state, stats, err := r.runIngester(ctx, kb)
	if err != nil {
		if errors.Is(err, errKnowledgeBaseClassNotFound) {
			return r.markFailed(ctx, kb, "ClassNotFound", fmt.Sprintf("KnowledgeBaseClass %q no longer exists", kb.Spec.KnowledgeBaseClassName), log)
		}
		log.Warn("ingester launcher errored during refresh, requeueing", zap.Error(err))
		return Result{RequeueAfter: 15 * time.Second}, nil
	}
	switch state {
	case IngestRunning:
		return Result{RequeueAfter: 10 * time.Second}, nil
	case IngestFailed:
		return r.markFailed(ctx, kb, "RefreshFailed", stats.Error, log)
	case IngestSucceeded:
		kb.Status.DocumentCount = stats.Documents
		kb.Status.IndexedChunks = stats.Chunks
		if stats.BackendBinding != nil {
			kb.Status.BackendBinding = cloneKnowledgeBackendBinding(stats.BackendBinding)
		}
	case IngestSkipped:
		docCount := int64(len(kb.Spec.Sources))
		if docCount == 0 {
			docCount = 1
		}
		kb.Status.DocumentCount = docCount
		kb.Status.IndexedChunks = docCount * 10
	}

	kb.Status.Generation++ // increment only now, on successful completion
	kb.Status.LastRefresh = time.Now().UTC().Format(time.RFC3339)
	kb.Status.Phase = v1alpha1.PhaseReady

	lc := lifecycle.SetCondition(
		apiToLifecycleConditions(kb.Status.Conditions),
		lifecycle.ConditionReady,
		lifecycle.ConditionTrue,
		"RefreshComplete",
		fmt.Sprintf("refreshed to generation %d: %d document(s), %d chunk(s)",
			kb.Status.Generation, kb.Status.DocumentCount, kb.Status.IndexedChunks),
	)
	lc = lifecycle.SetCondition(lc, lifecycle.ConditionProgressing, lifecycle.ConditionFalse, "Idle", "refresh complete")
	kb.Status.Conditions = lifecycleToAPIConditions(lc)

	if err := r.updateKB(ctx, kb); err != nil {
		return Result{}, fmt.Errorf("complete refreshing: %w", err)
	}
	log.Info("refresh complete",
		zap.Int64("documentCount", kb.Status.DocumentCount),
		zap.Int64("indexedChunks", kb.Status.IndexedChunks),
		zap.Int64("generation", kb.Status.Generation),
	)

	// Notify agents.
	if err := r.updateAgentKBConditions(ctx, kb, true, log); err != nil {
		log.Warn("failed to update agent KB conditions after refresh", zap.Error(err))
	}

	return Result{}, nil
}

// verifyReady checks that the backing KnowledgeBaseClass still exists.
func (r *KnowledgeBaseReconciler) verifyReady(ctx context.Context, kb *v1alpha1.KnowledgeBase, log *zap.Logger) (Result, error) {
	_, err := r.getKnowledgeBaseClass(ctx, kb.Spec.KnowledgeBaseClassName)
	if errors.Is(err, store.ErrNotFound) {
		log.Warn("KnowledgeBaseClass removed, marking KB as Failed",
			zap.String("class", kb.Spec.KnowledgeBaseClassName))
		kb.Status.Phase = v1alpha1.PhaseFailed
		lc := lifecycle.SetCondition(
			apiToLifecycleConditions(kb.Status.Conditions),
			lifecycle.ConditionReady,
			lifecycle.ConditionFalse,
			"ClassNotFound",
			fmt.Sprintf("KnowledgeBaseClass %q no longer exists", kb.Spec.KnowledgeBaseClassName),
		)
		kb.Status.Conditions = lifecycleToAPIConditions(lc)
		if updateErr := r.updateKB(ctx, kb); updateErr != nil {
			return Result{}, fmt.Errorf("mark kb failed: %w", updateErr)
		}
		if notifyErr := r.updateAgentKBConditions(ctx, kb, false, log); notifyErr != nil {
			log.Warn("failed to update agent KB conditions on failure", zap.Error(notifyErr))
		}
		return Result{RequeueAfter: 30 * time.Second}, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("verify knowledge base class: %w", err)
	}
	return Result{}, nil
}

// handleDeletion applies the durable reclaim policy and removes the finalizer.
func (r *KnowledgeBaseReconciler) handleDeletion(ctx context.Context, kb *v1alpha1.KnowledgeBase, log *zap.Logger) (Result, error) {
	// Notify agents that a required KB is going away before removing it.
	if err := r.updateAgentKBConditions(ctx, kb, false, log); err != nil {
		log.Warn("failed to update agent KB conditions on deletion", zap.Error(err))
	}
	if err := r.deleteRefreshSchedule(ctx, kb); err != nil {
		return Result{}, fmt.Errorf("delete refresh schedule: %w", err)
	}

	reclaimPolicy := v1alpha1.ReclaimDelete
	kbc, err := r.getKnowledgeBaseClass(ctx, kb.Spec.KnowledgeBaseClassName)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return Result{}, fmt.Errorf("get knowledge base class %q for deletion: %w", kb.Spec.KnowledgeBaseClassName, err)
	}
	if err == nil && kbc.Spec.ReclaimPolicy != "" {
		reclaimPolicy = kbc.Spec.ReclaimPolicy
	}
	providerBacked := kb.Status.BackendBinding != nil || (kbc != nil && kbc.Spec.BackendRef != nil)
	if lifecycle.HasFinalizer(kb.ObjectMeta.Finalizers, KnowledgeBaseFinalizer) && providerBacked {
		if kb.Status.BackendBinding == nil || !validKnowledgeReclaimPolicy(kb.Status.BackendBinding.ReclaimPolicy) {
			return r.markFailed(ctx, kb, "ProviderCleanupRequired", "knowledge backend binding has no deletion-safe reclaim policy", log)
		}
		reclaimPolicy = kb.Status.BackendBinding.ReclaimPolicy
		if r.cleaner == nil {
			return r.markFailed(ctx, kb, "ProviderCleanupRequired", "provider lifecycle confirmation is required before releasing knowledge backend ownership", log)
		}
		if err := r.cleaner.Cleanup(ctx, kb, reclaimPolicy); err != nil {
			return r.markFailed(ctx, kb, "ProviderCleanupFailed", err.Error(), log)
		}
	}

	switch reclaimPolicy {
	case v1alpha1.ReclaimDelete:
		log.Info("applying reclaimPolicy=Delete: index data released",
			zap.String("class", kb.Spec.KnowledgeBaseClassName))
		// In a real provisioner this would call the provisioner to delete the
		// vector index data. In this simulation, nothing external needs cleaning.

	case v1alpha1.ReclaimRetain:
		if r.bindingRecorder == nil {
			return r.markFailed(ctx, kb, "RetainedBindingRecordRequired", "durable retained knowledge backend ownership must be recorded before releasing the finalizer", log)
		}
		if err := r.bindingRecorder.Record(ctx, kb); err != nil {
			return r.markFailed(ctx, kb, "RetainedBindingRecordFailed", err.Error(), log)
		}
		log.Info("applying reclaimPolicy=Retain: index data preserved",
			zap.String("class", kb.Spec.KnowledgeBaseClassName))

	case v1alpha1.ReclaimArchive:
		log.Info("applying reclaimPolicy=Archive: index data archived",
			zap.String("class", kb.Spec.KnowledgeBaseClassName))
	}

	finalizers, removed := lifecycle.RemoveFinalizer(kb.ObjectMeta.Finalizers, KnowledgeBaseFinalizer)
	if !removed {
		return Result{}, nil
	}
	kb.ObjectMeta.Finalizers = finalizers
	if err := r.updateKB(ctx, kb); err != nil {
		return Result{}, fmt.Errorf("remove finalizer: %w", err)
	}
	return Result{}, nil
}

func validKnowledgeReclaimPolicy(policy v1alpha1.ReclaimPolicy) bool {
	switch policy {
	case v1alpha1.ReclaimDelete, v1alpha1.ReclaimRetain, v1alpha1.ReclaimArchive:
		return true
	default:
		return false
	}
}

func (r *KnowledgeBaseReconciler) reconcileRefreshSchedule(ctx context.Context, kb *v1alpha1.KnowledgeBase) error {
	schedule, periodic, err := knowledgeBaseRefreshSchedule(kb)
	if err != nil {
		return err
	}
	if !periodic {
		return r.deleteRefreshSchedule(ctx, kb)
	}

	name := knowledgeBaseRefreshScheduleName(kb.ObjectMeta.Name)
	raw, err := r.store.Get(ctx, knowledgeBaseIndexScheduleKind, kb.ObjectMeta.Namespace, name)
	if errors.Is(err, store.ErrNotFound) {
		child := v1alpha1.KnowledgeBaseIndexSchedule{
			TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: knowledgeBaseIndexScheduleKind},
			ObjectMeta: v1alpha1.ObjectMeta{
				Name:      name,
				Namespace: kb.ObjectMeta.Namespace,
				OwnerReferences: []v1alpha1.OwnerReference{{
					APIVersion: v1alpha1.APIVersion,
					Kind:       knowledgeBaseKind,
					Name:       kb.ObjectMeta.Name,
					UID:        kb.ObjectMeta.UID,
				}},
			},
			Spec: v1alpha1.KnowledgeBaseIndexScheduleSpec{
				KnowledgeBaseName: kb.ObjectMeta.Name,
				Schedule:          schedule,
			},
		}
		data, marshalErr := json.Marshal(child)
		if marshalErr != nil {
			return fmt.Errorf("marshal refresh schedule: %w", marshalErr)
		}
		createErr := r.store.Create(ctx, knowledgeBaseIndexScheduleKind, child.ObjectMeta.Namespace, child.ObjectMeta.Name, data)
		if createErr != nil && !errors.Is(createErr, store.ErrAlreadyExists) {
			return createErr
		}
		if createErr == nil {
			return nil
		}
		raw, err = r.store.Get(ctx, knowledgeBaseIndexScheduleKind, kb.ObjectMeta.Namespace, name)
	}
	if err != nil {
		return fmt.Errorf("get refresh schedule: %w", err)
	}

	var existing v1alpha1.KnowledgeBaseIndexSchedule
	if err := json.Unmarshal(raw, &existing); err != nil {
		return fmt.Errorf("unmarshal refresh schedule: %w", err)
	}
	const maxUpdateAttempts = 5
	for attempt := 0; attempt < maxUpdateAttempts; attempt++ {
		if !knowledgeBaseOwnsRefreshSchedule(kb, &existing) {
			return fmt.Errorf("%w: %q is not owned by KnowledgeBase UID %q", errRefreshScheduleCollision, name, kb.ObjectMeta.UID)
		}
		if existing.Spec.KnowledgeBaseName == kb.ObjectMeta.Name && existing.Spec.Schedule == schedule {
			return nil
		}
		existing.Spec.KnowledgeBaseName = kb.ObjectMeta.Name
		existing.Spec.Schedule = schedule
		data, err := json.Marshal(existing)
		if err != nil {
			return fmt.Errorf("marshal refresh schedule update: %w", err)
		}
		err = r.store.Update(ctx, knowledgeBaseIndexScheduleKind, existing.ObjectMeta.Namespace, existing.ObjectMeta.Name, data, existing.ObjectMeta.ResourceVersion)
		if err == nil {
			return nil
		}
		if !errors.Is(err, store.ErrConflict) {
			return err
		}
		raw, err = r.store.Get(ctx, knowledgeBaseIndexScheduleKind, kb.ObjectMeta.Namespace, name)
		if err != nil {
			return fmt.Errorf("re-read refresh schedule after conflict: %w", err)
		}
		if err := json.Unmarshal(raw, &existing); err != nil {
			return fmt.Errorf("unmarshal refresh schedule after conflict: %w", err)
		}
	}
	return store.ErrConflict
}

func (r *KnowledgeBaseReconciler) deleteRefreshSchedule(ctx context.Context, kb *v1alpha1.KnowledgeBase) error {
	name := knowledgeBaseRefreshScheduleName(kb.ObjectMeta.Name)
	raw, err := r.store.Get(ctx, knowledgeBaseIndexScheduleKind, kb.ObjectMeta.Namespace, name)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var schedule v1alpha1.KnowledgeBaseIndexSchedule
	if err := json.Unmarshal(raw, &schedule); err != nil {
		return fmt.Errorf("unmarshal refresh schedule: %w", err)
	}
	if !knowledgeBaseOwnsRefreshSchedule(kb, &schedule) {
		return nil
	}
	if err := r.deleteRefreshScheduleRuns(ctx, &schedule); err != nil {
		return err
	}
	if err := r.store.Delete(ctx, knowledgeBaseIndexScheduleKind, schedule.ObjectMeta.Namespace, schedule.ObjectMeta.Name); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil
}

func (r *KnowledgeBaseReconciler) deleteRefreshScheduleRuns(ctx context.Context, schedule *v1alpha1.KnowledgeBaseIndexSchedule) error {
	raws, err := r.store.List(ctx, knowledgeBaseIndexRunKind, store.ListOptions{Namespace: schedule.ObjectMeta.Namespace})
	if err != nil {
		return fmt.Errorf("list refresh schedule runs: %w", err)
	}
	for _, raw := range raws {
		var run v1alpha1.KnowledgeBaseIndexRun
		if err := json.Unmarshal(raw, &run); err != nil {
			return fmt.Errorf("unmarshal refresh schedule run: %w", err)
		}
		if !scheduleOwnsRun(schedule, &run) {
			continue
		}
		if err := r.store.Delete(ctx, knowledgeBaseIndexRunKind, run.ObjectMeta.Namespace, run.ObjectMeta.Name); err != nil && !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("delete refresh schedule run %q: %w", run.ObjectMeta.Name, err)
		}
	}
	return nil
}

func knowledgeBaseRefreshSchedule(kb *v1alpha1.KnowledgeBase) (string, bool, error) {
	if kb.Spec.RefreshPolicy == nil {
		return "", false, nil
	}
	switch kb.Spec.RefreshPolicy.Mode {
	case "onDemand", "webhook":
		return "", false, nil
	case "periodic":
	default:
		return "", false, fmt.Errorf("unsupported refresh policy mode %q", kb.Spec.RefreshPolicy.Mode)
	}
	interval := strings.TrimSpace(kb.Spec.RefreshPolicy.Interval)
	duration, err := time.ParseDuration(interval)
	if err != nil || duration <= 0 {
		return "", false, fmt.Errorf("periodic refresh interval %q must be a duration greater than zero", interval)
	}
	return "@every " + interval, true, nil
}

func isRefreshScheduleFailure(conditions []v1alpha1.Condition) bool {
	ready := findConditionByType(conditions, string(lifecycle.ConditionReady))
	return ready != nil && (ready.Reason == "RefreshScheduleCollision" || ready.Reason == "RefreshScheduleError")
}

func knowledgeBaseRefreshScheduleName(knowledgeBaseName string) string {
	const suffix = "-refresh"
	const maxNameLength = 253
	if len(knowledgeBaseName)+len(suffix) <= maxNameLength {
		return knowledgeBaseName + suffix
	}
	sum := sha256.Sum256([]byte(knowledgeBaseName))
	hashSuffix := fmt.Sprintf("-%x", sum[:4])
	prefixLength := maxNameLength - len(suffix) - len(hashSuffix)
	return strings.TrimRight(knowledgeBaseName[:prefixLength], "-") + suffix + hashSuffix
}

func knowledgeBaseOwnsRefreshSchedule(kb *v1alpha1.KnowledgeBase, schedule *v1alpha1.KnowledgeBaseIndexSchedule) bool {
	if kb.ObjectMeta.UID == "" {
		return false
	}
	for _, owner := range schedule.ObjectMeta.OwnerReferences {
		if owner.Kind == knowledgeBaseKind && owner.Name == kb.ObjectMeta.Name && owner.UID != "" && owner.UID == kb.ObjectMeta.UID {
			return true
		}
	}
	return false
}

// updateAgentKBConditions iterates all agents in the KB's namespace and sets
// the "KnowledgeBaseReady" condition for any agent that holds a Required=true
// KnowledgeBaseClaim ref whose claim binds to this KB.
func (r *KnowledgeBaseReconciler) updateAgentKBConditions(ctx context.Context, kb *v1alpha1.KnowledgeBase, kbReady bool, log *zap.Logger) error {
	// First, find every claim in the namespace that binds to this KB. Agents
	// only reference claims — never KBs directly — so the KB→agent link runs
	// through the set of claims whose spec.knowledgeBaseName matches.
	claimItems, err := r.store.List(ctx, "KnowledgeBaseClaim", store.ListOptions{Namespace: kb.ObjectMeta.Namespace})
	if err != nil {
		return fmt.Errorf("list knowledge base claims: %w", err)
	}
	matchingClaims := make(map[string]struct{})
	for _, raw := range claimItems {
		var claim v1alpha1.KnowledgeBaseClaim
		if err := json.Unmarshal(raw, &claim); err != nil {
			log.Warn("failed to unmarshal knowledge base claim during KB condition update", zap.Error(err))
			continue
		}
		if claim.Spec.KnowledgeBaseName == kb.ObjectMeta.Name {
			matchingClaims[claim.ObjectMeta.Name] = struct{}{}
		}
	}
	if len(matchingClaims) == 0 {
		return nil
	}

	items, err := r.store.List(ctx, "Agent", store.ListOptions{Namespace: kb.ObjectMeta.Namespace})
	if err != nil {
		return fmt.Errorf("list agents: %w", err)
	}

	for _, raw := range items {
		var agent v1alpha1.Agent
		if err := json.Unmarshal(raw, &agent); err != nil {
			log.Warn("failed to unmarshal agent during KB condition update", zap.Error(err))
			continue
		}

		needsUpdate := false
		for _, claimRef := range agent.Spec.KnowledgeBaseClaims {
			if _, ok := matchingClaims[claimRef.Name]; !ok || !claimRef.Required {
				continue
			}

			// This agent has a Required claim ref bound to the KB — update its condition.
			condStatus := lifecycle.ConditionTrue
			reason := "KnowledgeBaseReady"
			message := fmt.Sprintf("KnowledgeBase %q is Ready (generation %d)", kb.ObjectMeta.Name, kb.Status.Generation)
			if !kbReady {
				condStatus = lifecycle.ConditionFalse
				reason = "KnowledgeBaseNotReady"
				message = fmt.Sprintf("required KnowledgeBase %q is not in Ready phase (phase: %s)", kb.ObjectMeta.Name, kb.Status.Phase)
			}

			lc := lifecycle.SetCondition(
				apiToLifecycleConditions(agent.Status.Conditions),
				conditionKBReady,
				condStatus,
				reason,
				message,
			)
			agent.Status.Conditions = lifecycleToAPIConditions(lc)
			needsUpdate = true
			break
		}

		if !needsUpdate {
			continue
		}

		if err := r.updateAgentWithRetry(ctx, &agent); err != nil {
			log.Warn("failed to update agent KB condition",
				zap.String("agent", agent.ObjectMeta.Name))
		}
	}
	return nil
}

// getKnowledgeBaseClass fetches and unmarshals the named KnowledgeBaseClass.
// KnowledgeBaseClass is cluster-scoped (empty namespace).
func (r *KnowledgeBaseReconciler) getKnowledgeBaseClass(ctx context.Context, name string) (*v1alpha1.KnowledgeBaseClass, error) {
	raw, err := r.store.Get(ctx, knowledgeBaseClassKind, "", name)
	if err != nil {
		return nil, err
	}
	var kbc v1alpha1.KnowledgeBaseClass
	if err := json.Unmarshal(raw, &kbc); err != nil {
		return nil, fmt.Errorf("unmarshal knowledge base class: %w", err)
	}
	return &kbc, nil
}

// updateKB persists the latest KnowledgeBase state to the store.
func (r *KnowledgeBaseReconciler) updateKB(ctx context.Context, kb *v1alpha1.KnowledgeBase) error {
	b, err := json.Marshal(kb)
	if err != nil {
		return fmt.Errorf("marshal knowledgebase: %w", err)
	}
	return r.store.Update(ctx, knowledgeBaseKind, kb.ObjectMeta.Namespace, kb.ObjectMeta.Name, b, kb.ObjectMeta.ResourceVersion)
}

func (r *KnowledgeBaseReconciler) updateAgentWithRetry(ctx context.Context, agent *v1alpha1.Agent) error {
	const maxRetries = 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		data, err := json.Marshal(agent)
		if err != nil {
			return fmt.Errorf("marshal agent: %w", err)
		}
		err = r.store.Update(ctx, "Agent", agent.ObjectMeta.Namespace, agent.ObjectMeta.Name, data, agent.ObjectMeta.ResourceVersion)
		if err == nil {
			return nil
		}
		if !errors.Is(err, store.ErrConflict) {
			return err
		}
		raw, getErr := r.store.Get(ctx, "Agent", agent.ObjectMeta.Namespace, agent.ObjectMeta.Name)
		if getErr != nil {
			return fmt.Errorf("re-read after conflict: %w", getErr)
		}
		var latest v1alpha1.Agent
		if getErr := json.Unmarshal(raw, &latest); getErr != nil {
			return fmt.Errorf("unmarshal after conflict: %w", getErr)
		}
		latest.Status.Conditions = mergeAgentCondition(latest.Status.Conditions, agent.Status.Conditions, string(conditionKBReady))
		*agent = latest
	}
	return store.ErrConflict
}
