// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

const (
	knowledgeBaseIndexScheduleKind = "KnowledgeBaseIndexSchedule"

	// scheduleRunOwnerLabel tags IndexRuns produced by a Schedule with the
	// parent's name, so the reconciler can list its children by label without
	// scanning every owner reference.
	scheduleRunOwnerLabel = "kognition.dev/index-schedule"
)

// scheduleParser accepts standard 5-field cron plus @every/@hourly descriptors.
var scheduleParser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
)

// KnowledgeBaseIndexScheduleReconciler creates KnowledgeBaseIndexRun objects on
// a cron schedule. It is a store-only reconciler; the K8s adapter wires it to
// the controller-runtime manager.
type KnowledgeBaseIndexScheduleReconciler struct {
	store  store.Store
	logger *zap.Logger
	now    func() time.Time
}

// NewKnowledgeBaseIndexScheduleReconciler creates a new reconciler.
func NewKnowledgeBaseIndexScheduleReconciler(s store.Store, logger *zap.Logger) *KnowledgeBaseIndexScheduleReconciler {
	return &KnowledgeBaseIndexScheduleReconciler{
		store:  s,
		logger: logger.With(zap.String("reconciler", "knowledge-base-index-schedule")),
		now:    func() time.Time { return time.Now().UTC() },
	}
}

// Reconcile implements Reconciler.
func (r *KnowledgeBaseIndexScheduleReconciler) Reconcile(ctx context.Context, req Request) (Result, error) {
	log := r.logger.With(zap.String("namespace", req.Namespace), zap.String("name", req.Name), requestid.ZapField(ctx))

	raw, err := r.store.Get(ctx, knowledgeBaseIndexScheduleKind, req.Namespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			log.Debug("indexSchedule not found")
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("get indexSchedule: %w", err)
	}

	var sched v1alpha1.KnowledgeBaseIndexSchedule
	if err := json.Unmarshal(raw, &sched); err != nil {
		return Result{}, fmt.Errorf("unmarshal indexSchedule: %w", err)
	}
	originalStatus := snapshotKnowledgeBaseIndexScheduleStatus(sched.Status)

	if sched.ObjectMeta.DeletionTimestamp != nil {
		return Result{}, nil
	}

	// Parse the schedule up-front — admission has already validated it, but
	// the reconciler must not panic on a malformed value from an
	// out-of-webhook path.
	crontab, err := scheduleParser.Parse(strings.TrimSpace(sched.Spec.Schedule))
	if err != nil {
		return r.markScheduleFailed(ctx, &sched, originalStatus, "InvalidSchedule",
			fmt.Sprintf("cron parse failed: %v", err), log)
	}

	// Suspend short-circuits everything except status refresh.
	if sched.Spec.Suspend != nil && *sched.Spec.Suspend {
		return r.markSuspended(ctx, &sched, originalStatus, log)
	}

	now := r.now()

	// Compute the next scheduled tick from the last one, or from creation
	// time if we've never run.
	anchor := now
	if sched.Status.LastScheduleTime != "" {
		if t, perr := time.Parse(time.RFC3339, sched.Status.LastScheduleTime); perr == nil {
			anchor = t
		}
	} else if !sched.ObjectMeta.CreationTimestamp.IsZero() {
		anchor = sched.ObjectMeta.CreationTimestamp
	}

	nextTick := crontab.Next(anchor)

	// Refresh active children (this also prunes stale entries from status).
	active, err := r.listChildRuns(ctx, &sched)
	if err != nil {
		return Result{}, fmt.Errorf("list child runs: %w", err)
	}

	if !nextTick.After(now) {
		// A tick is due — maybe fire it.
		missedDeadline := false
		if sched.Spec.StartingDeadlineSeconds != nil {
			miss := now.Sub(nextTick)
			if miss > time.Duration(*sched.Spec.StartingDeadlineSeconds)*time.Second {
				log.Info("skipping tick past starting deadline",
					zap.Time("tick", nextTick), zap.Duration("missed", miss))
				missedDeadline = true
			}
		}

		if !missedDeadline {
			if err := r.enforceConcurrency(ctx, &sched, active, log); err != nil {
				return Result{}, err
			}
			// Re-list after enforcement so we don't double-count Replaces.
			active, err = r.listChildRuns(ctx, &sched)
			if err != nil {
				return Result{}, fmt.Errorf("re-list child runs: %w", err)
			}
			activeUnfinished := filterUnfinishedRuns(active)

			policy := sched.Spec.ConcurrencyPolicy
			if policy == "" {
				policy = v1alpha1.ConcurrencyPolicyForbid
			}
			if policy == v1alpha1.ConcurrencyPolicyForbid && len(activeUnfinished) > 0 {
				log.Info("Forbid concurrency: prior run still active, skipping tick",
					zap.Int("activeCount", len(activeUnfinished)))
			} else {
				if _, err := r.createChildRun(ctx, &sched, nextTick, log); err != nil {
					return Result{}, fmt.Errorf("create child run: %w", err)
				}
				active, err = r.listChildRuns(ctx, &sched)
				if err != nil {
					return Result{}, fmt.Errorf("post-create list: %w", err)
				}
			}
		}
		sched.Status.LastScheduleTime = now.UTC().Format(time.RFC3339)
	}

	// Prune history and refresh status.
	if err := r.pruneHistory(ctx, &sched, active, log); err != nil {
		log.Warn("history pruning failed", zap.Error(err))
	}
	active, err = r.listChildRuns(ctx, &sched)
	if err != nil {
		return Result{}, fmt.Errorf("final list: %w", err)
	}

	updateStatusFromRuns(&sched, active)
	nextTick = crontab.Next(now)
	sched.Status.NextScheduleTime = nextTick.UTC().Format(time.RFC3339)
	sched.Status.Phase = v1alpha1.PhaseReady

	lc := lifecycle.SetCondition(
		apiToLifecycleConditions(sched.Status.Conditions),
		lifecycle.ConditionReady, lifecycle.ConditionTrue,
		"ScheduleActive",
		fmt.Sprintf("next tick at %s", sched.Status.NextScheduleTime),
	)
	sched.Status.Conditions = lifecycleToAPIConditions(lc)

	if err := r.updateSchedule(ctx, &sched, originalStatus); err != nil {
		return Result{}, fmt.Errorf("update schedule status: %w", err)
	}

	requeueAfter := nextTick.Sub(now)
	if requeueAfter < time.Second {
		requeueAfter = time.Second
	}
	return Result{RequeueAfter: requeueAfter}, nil
}

// markScheduleFailed writes a Failed phase; the reconciler keeps requeueing so
// a corrected schedule can recover.
func (r *KnowledgeBaseIndexScheduleReconciler) markScheduleFailed(
	ctx context.Context, sched *v1alpha1.KnowledgeBaseIndexSchedule, originalStatus v1alpha1.KnowledgeBaseIndexScheduleStatus,
	reason, message string, log *zap.Logger,
) (Result, error) {
	sched.Status.Phase = v1alpha1.PhaseFailed
	lc := lifecycle.SetCondition(
		apiToLifecycleConditions(sched.Status.Conditions),
		lifecycle.ConditionReady, lifecycle.ConditionFalse, reason, message,
	)
	sched.Status.Conditions = lifecycleToAPIConditions(lc)
	if err := r.updateSchedule(ctx, sched, originalStatus); err != nil {
		return Result{}, fmt.Errorf("mark schedule failed: %w", err)
	}
	log.Warn("schedule marked failed", zap.String("reason", reason), zap.String("message", message))
	return Result{RequeueAfter: 60 * time.Second}, nil
}

// markSuspended reports the schedule as Suspended without creating runs.
func (r *KnowledgeBaseIndexScheduleReconciler) markSuspended(
	ctx context.Context, sched *v1alpha1.KnowledgeBaseIndexSchedule,
	originalStatus v1alpha1.KnowledgeBaseIndexScheduleStatus, log *zap.Logger,
) (Result, error) {
	sched.Status.Phase = v1alpha1.PhaseSuspended
	sched.Status.NextScheduleTime = ""
	lc := lifecycle.SetCondition(
		apiToLifecycleConditions(sched.Status.Conditions),
		lifecycle.ConditionReady, lifecycle.ConditionFalse,
		"Suspended", "schedule is suspended; no runs will be created",
	)
	sched.Status.Conditions = lifecycleToAPIConditions(lc)
	if err := r.updateSchedule(ctx, sched, originalStatus); err != nil {
		return Result{}, fmt.Errorf("mark schedule suspended: %w", err)
	}
	log.Debug("schedule suspended")
	// Poll periodically in case Suspend flips back off.
	return Result{RequeueAfter: 60 * time.Second}, nil
}

// listChildRuns returns all KnowledgeBaseIndexRuns owned by this schedule.
func (r *KnowledgeBaseIndexScheduleReconciler) listChildRuns(
	ctx context.Context, sched *v1alpha1.KnowledgeBaseIndexSchedule,
) ([]v1alpha1.KnowledgeBaseIndexRun, error) {
	items, err := r.store.List(ctx, knowledgeBaseIndexRunKind, store.ListOptions{Namespace: sched.ObjectMeta.Namespace})
	if err != nil {
		return nil, err
	}
	var out []v1alpha1.KnowledgeBaseIndexRun
	for _, raw := range items {
		var run v1alpha1.KnowledgeBaseIndexRun
		if err := json.Unmarshal(raw, &run); err != nil {
			continue
		}
		if run.ObjectMeta.Labels[scheduleRunOwnerLabel] != sched.ObjectMeta.Name {
			continue
		}
		if !scheduleOwnsRun(sched, &run) {
			continue
		}
		out = append(out, run)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ObjectMeta.CreationTimestamp.Equal(out[j].ObjectMeta.CreationTimestamp) {
			return out[i].ObjectMeta.Name < out[j].ObjectMeta.Name
		}
		return out[i].ObjectMeta.CreationTimestamp.Before(out[j].ObjectMeta.CreationTimestamp)
	})
	return out, nil
}

func scheduleOwnsRun(sched *v1alpha1.KnowledgeBaseIndexSchedule, run *v1alpha1.KnowledgeBaseIndexRun) bool {
	for _, owner := range run.ObjectMeta.OwnerReferences {
		if owner.Kind == knowledgeBaseIndexScheduleKind && owner.Name == sched.ObjectMeta.Name && owner.UID == sched.ObjectMeta.UID {
			return true
		}
	}
	return false
}

// enforceConcurrency cancels active runs when policy is Replace.
func (r *KnowledgeBaseIndexScheduleReconciler) enforceConcurrency(
	ctx context.Context, sched *v1alpha1.KnowledgeBaseIndexSchedule,
	active []v1alpha1.KnowledgeBaseIndexRun, log *zap.Logger,
) error {
	if sched.Spec.ConcurrencyPolicy != v1alpha1.ConcurrencyPolicyReplace {
		return nil
	}
	for _, run := range active {
		if isTerminal(run.Status.Phase) {
			continue
		}
		if err := r.store.Delete(ctx, knowledgeBaseIndexRunKind, run.ObjectMeta.Namespace, run.ObjectMeta.Name); err != nil && !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("delete active run %q: %w", run.ObjectMeta.Name, err)
		}
		log.Info("Replace policy: cancelled active run", zap.String("run", run.ObjectMeta.Name))
	}
	return nil
}

// createChildRun materialises a new IndexRun in the store, owned by sched.
func (r *KnowledgeBaseIndexScheduleReconciler) createChildRun(
	ctx context.Context, sched *v1alpha1.KnowledgeBaseIndexSchedule, tick time.Time, log *zap.Logger,
) (*v1alpha1.KnowledgeBaseIndexRun, error) {
	name := childRunName(sched.ObjectMeta.Name, tick)
	run := &v1alpha1.KnowledgeBaseIndexRun{
		TypeMeta: v1alpha1.TypeMeta{
			APIVersion: v1alpha1.APIVersion,
			Kind:       "KnowledgeBaseIndexRun",
		},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:      name,
			Namespace: sched.ObjectMeta.Namespace,
			Labels: map[string]string{
				scheduleRunOwnerLabel: sched.ObjectMeta.Name,
			},
			OwnerReferences: []v1alpha1.OwnerReference{{
				APIVersion: v1alpha1.APIVersion,
				Kind:       "KnowledgeBaseIndexSchedule",
				Name:       sched.ObjectMeta.Name,
				UID:        sched.ObjectMeta.UID,
			}},
		},
		Spec: v1alpha1.KnowledgeBaseIndexRunSpec{
			KnowledgeBaseName:       sched.Spec.KnowledgeBaseName,
			TTLSecondsAfterFinished: sched.Spec.RunTemplate.TTLSecondsAfterFinished,
		},
	}
	data, err := json.Marshal(run)
	if err != nil {
		return nil, fmt.Errorf("marshal run: %w", err)
	}
	if err := r.store.Create(ctx, knowledgeBaseIndexRunKind, run.ObjectMeta.Namespace, run.ObjectMeta.Name, data); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			raw, getErr := r.store.Get(ctx, knowledgeBaseIndexRunKind, run.ObjectMeta.Namespace, run.ObjectMeta.Name)
			if getErr != nil {
				return nil, fmt.Errorf("get existing child run %q: %w", name, getErr)
			}
			var existing v1alpha1.KnowledgeBaseIndexRun
			if getErr := json.Unmarshal(raw, &existing); getErr != nil {
				return nil, fmt.Errorf("unmarshal existing child run %q: %w", name, getErr)
			}
			if !scheduleOwnsRun(sched, &existing) {
				return nil, fmt.Errorf("child run %q is not owned by schedule UID %q: %w", name, sched.ObjectMeta.UID, store.ErrAlreadyExists)
			}
			log.Debug("owned child run already exists (concurrent tick), skipping", zap.String("run", name))
			return &existing, nil
		}
		return nil, err
	}
	log.Info("created child indexRun", zap.String("run", name), zap.Time("tick", tick))
	return run, nil
}

// pruneHistory removes completed runs beyond the configured limits.
func (r *KnowledgeBaseIndexScheduleReconciler) pruneHistory(
	ctx context.Context, sched *v1alpha1.KnowledgeBaseIndexSchedule,
	runs []v1alpha1.KnowledgeBaseIndexRun, log *zap.Logger,
) error {
	successLimit := int32(3)
	if sched.Spec.SuccessfulRunsHistoryLimit != nil {
		successLimit = *sched.Spec.SuccessfulRunsHistoryLimit
	}
	failLimit := int32(1)
	if sched.Spec.FailedRunsHistoryLimit != nil {
		failLimit = *sched.Spec.FailedRunsHistoryLimit
	}

	var successes, failures []v1alpha1.KnowledgeBaseIndexRun
	for _, run := range runs {
		switch run.Status.Phase {
		case v1alpha1.PhaseSucceeded:
			successes = append(successes, run)
		case v1alpha1.PhaseFailed:
			failures = append(failures, run)
		}
	}
	// Oldest first thanks to listChildRuns sort.
	if excess := int32(len(successes)) - successLimit; excess > 0 {
		for _, run := range successes[:excess] {
			if err := r.store.Delete(ctx, knowledgeBaseIndexRunKind, run.ObjectMeta.Namespace, run.ObjectMeta.Name); err != nil && !errors.Is(err, store.ErrNotFound) {
				log.Warn("prune successful run failed", zap.String("run", run.ObjectMeta.Name), zap.Error(err))
			}
		}
	}
	if excess := int32(len(failures)) - failLimit; excess > 0 {
		for _, run := range failures[:excess] {
			if err := r.store.Delete(ctx, knowledgeBaseIndexRunKind, run.ObjectMeta.Namespace, run.ObjectMeta.Name); err != nil && !errors.Is(err, store.ErrNotFound) {
				log.Warn("prune failed run failed", zap.String("run", run.ObjectMeta.Name), zap.Error(err))
			}
		}
	}
	return nil
}

func (r *KnowledgeBaseIndexScheduleReconciler) updateSchedule(
	ctx context.Context, sched *v1alpha1.KnowledgeBaseIndexSchedule, originalStatus v1alpha1.KnowledgeBaseIndexScheduleStatus,
) error {
	if reflect.DeepEqual(sched.Status, originalStatus) {
		return nil
	}

	const maxRetries = 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		err := r.updateScheduleObject(ctx, sched)
		if err == nil {
			return nil
		}
		if !errors.Is(err, store.ErrConflict) {
			return err
		}
		raw, getErr := r.store.Get(ctx, knowledgeBaseIndexScheduleKind, sched.ObjectMeta.Namespace, sched.ObjectMeta.Name)
		if getErr != nil {
			return fmt.Errorf("re-read schedule after conflict: %w", getErr)
		}
		var latest v1alpha1.KnowledgeBaseIndexSchedule
		if getErr := json.Unmarshal(raw, &latest); getErr != nil {
			return fmt.Errorf("unmarshal schedule after conflict: %w", getErr)
		}
		mergeKnowledgeBaseIndexScheduleStatus(&latest.Status, originalStatus, sched.Status)
		*sched = latest
	}
	return store.ErrConflict
}

func (r *KnowledgeBaseIndexScheduleReconciler) updateScheduleObject(ctx context.Context, sched *v1alpha1.KnowledgeBaseIndexSchedule) error {
	b, err := json.Marshal(sched)
	if err != nil {
		return fmt.Errorf("marshal schedule: %w", err)
	}
	return r.store.Update(ctx, knowledgeBaseIndexScheduleKind, sched.ObjectMeta.Namespace, sched.ObjectMeta.Name, b, sched.ObjectMeta.ResourceVersion)
}

func snapshotKnowledgeBaseIndexScheduleStatus(status v1alpha1.KnowledgeBaseIndexScheduleStatus) v1alpha1.KnowledgeBaseIndexScheduleStatus {
	cloned := status
	cloned.Active = append([]v1alpha1.LocalObjectReference(nil), status.Active...)
	cloned.Conditions = append([]v1alpha1.Condition(nil), status.Conditions...)
	return cloned
}

func mergeKnowledgeBaseIndexScheduleStatus(
	latest *v1alpha1.KnowledgeBaseIndexScheduleStatus, original, desired v1alpha1.KnowledgeBaseIndexScheduleStatus,
) {
	if desired.Phase != original.Phase {
		latest.Phase = desired.Phase
	}
	if desired.LastScheduleTime != original.LastScheduleTime {
		latest.LastScheduleTime = desired.LastScheduleTime
	}
	if desired.LastSuccessfulTime != original.LastSuccessfulTime {
		latest.LastSuccessfulTime = desired.LastSuccessfulTime
	}
	if desired.NextScheduleTime != original.NextScheduleTime {
		latest.NextScheduleTime = desired.NextScheduleTime
	}
	if !slices.Equal(desired.Active, original.Active) {
		latest.Active = append([]v1alpha1.LocalObjectReference(nil), desired.Active...)
	}
	originalReady := restartLadderCondition(original.Conditions, string(lifecycle.ConditionReady))
	desiredReady := restartLadderCondition(desired.Conditions, string(lifecycle.ConditionReady))
	if !conditionsMatch(originalReady, desiredReady) {
		latest.Conditions = applyConditionChange(latest.Conditions, string(lifecycle.ConditionReady), desiredReady)
	}
}

// updateStatusFromRuns refreshes Active and LastSuccessfulTime.
func updateStatusFromRuns(sched *v1alpha1.KnowledgeBaseIndexSchedule, runs []v1alpha1.KnowledgeBaseIndexRun) {
	var active []v1alpha1.LocalObjectReference
	var lastSuccess string
	for _, run := range runs {
		if !isTerminal(run.Status.Phase) {
			active = append(active, v1alpha1.LocalObjectReference{Name: run.ObjectMeta.Name})
			continue
		}
		if run.Status.Phase == v1alpha1.PhaseSucceeded && run.Status.CompletedAt > lastSuccess {
			lastSuccess = run.Status.CompletedAt
		}
	}
	sched.Status.Active = active
	if lastSuccess != "" {
		sched.Status.LastSuccessfulTime = lastSuccess
	}
}

func filterUnfinishedRuns(runs []v1alpha1.KnowledgeBaseIndexRun) []v1alpha1.KnowledgeBaseIndexRun {
	var out []v1alpha1.KnowledgeBaseIndexRun
	for _, r := range runs {
		if !isTerminal(r.Status.Phase) {
			out = append(out, r)
		}
	}
	return out
}

func isTerminal(p v1alpha1.Phase) bool {
	return p == v1alpha1.PhaseSucceeded || p == v1alpha1.PhaseFailed
}

// childRunName builds a DNS1123-compatible name of the form
// <schedule>-<unixtimestamp>, truncated to 63 chars.
func childRunName(schedule string, tick time.Time) string {
	const maxLen = 63
	suffix := fmt.Sprintf("-%d", tick.Unix())
	keep := maxLen - len(suffix)
	if keep < 1 {
		keep = 1
	}
	if keep > len(schedule) {
		keep = len(schedule)
	}
	return schedule[:keep] + suffix
}
