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
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

const trustProfileKind = "TrustProfile"
const maxTrustAuditEntriesPerIdentity = 10000

// TrustProfileReconciler reconciles TrustProfile resources, updating scores
// and managing tier promotion/demotion.
type TrustProfileReconciler struct {
	store  store.Store
	logger *zap.Logger
	audit  *observability.CognitiveAuditLog
}

// SetAuditLog enables periodic scoring from durable CognitiveAuditLog entries.
func (r *TrustProfileReconciler) SetAuditLog(audit *observability.CognitiveAuditLog) { r.audit = audit }

// NewTrustProfileReconciler creates a new TrustProfileReconciler.
func NewTrustProfileReconciler(s store.Store, logger *zap.Logger) *TrustProfileReconciler {
	return &TrustProfileReconciler{
		store:  s,
		logger: logger.With(zap.String("controller", "trust-profile")),
	}
}

// Trust tier thresholds.
const (
	TierUntrusted  = "untrusted"
	TierRestricted = "restricted"
	TierStandard   = "standard"
	TierTrusted    = "trusted"
	TierPrivileged = "privileged"
)

// Reconcile processes TrustProfile updates.
func (r *TrustProfileReconciler) Reconcile(ctx context.Context, req Request) (Result, error) {
	raw, err := r.store.Get(ctx, trustProfileKind, req.Namespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("get TrustProfile: %w", err)
	}

	var tp v1alpha1.TrustProfile
	if err := json.Unmarshal(raw, &tp); err != nil {
		return Result{}, fmt.Errorf("unmarshal TrustProfile: %w", err)
	}
	if tp.Status.PendingTrustChange != nil {
		if err := r.flushTrustChangeAudit(ctx, req.Namespace, req.Name); err != nil {
			return Result{}, err
		}
		return Result{Requeue: true}, nil
	}

	statusChanged := false
	// Initialize status from spec if needed.
	if initializeTrustScore(&tp) {
		statusChanged = true
	}
	if tp.Status.CurrentClass == "" {
		tp.Status.CurrentClass = effectiveCurrentClass(&tp)
		statusChanged = true
	}
	probationBefore := tp.Status.ProbationUntil
	clearExpiredProbation(&tp, nowFunc())
	if tp.Status.ProbationUntil != probationBefore {
		statusChanged = true
	}
	levels, err := r.autonomyClassLevels(ctx, tp.ObjectMeta.Namespace)
	if err != nil {
		return Result{}, err
	}
	if err := validateTrustProfileClassLevels(&tp, levels); err != nil {
		return Result{}, err
	}
	if target := pendingPromotionClass(&tp, levels); target != "" {
		evidence := latestTrustScoreEvidence(tp.Status.ScoreHistory)
		if err := r.ensurePromotionReview(ctx, &tp, evidence, target); err != nil {
			return Result{}, err
		}
	}

	// Determine tier from score.
	newTier := scoreTier(tp.Status.CurrentScore)
	if newTier != tp.Status.CurrentTier {
		r.logger.Info("trust tier changed",
			zap.String("agent", tp.Spec.AgentRef.Name),
			zap.String("from", tp.Status.CurrentTier),
			zap.String("to", newTier),
			requestid.ZapField(ctx))
		tp.Status.CurrentTier = newTier
		statusChanged = true
	}

	interval, intervalErr := trustEvaluationInterval(tp.Spec.EvaluationInterval)
	if intervalErr != nil {
		return Result{}, intervalErr
	}
	requestedClass := ""
	evaluationID := ""
	if interval > 0 {
		now := nowFunc().UTC()
		last, err := parseStatusTime(tp.Status.LastEvaluationTime)
		if err != nil {
			return Result{}, fmt.Errorf("parse TrustProfile lastEvaluationTime: %w", err)
		}
		if last.IsZero() || !now.Before(last.Add(interval)) {
			if r.audit == nil {
				return Result{}, errors.New("periodic TrustProfile scoring requires a CognitiveAuditLog")
			}
			entries, err := r.queryAgentAudit(ctx, &tp, auditWindowStart(last, now, interval), now)
			if err != nil {
				return Result{}, err
			}
			evaluationID = "audit:" + now.Format(time.RFC3339Nano)
			requestedClass = r.applyAuditEvaluation(&tp, entries, now, evaluationID, levels)
			statusChanged = true
		}
	}

	if statusChanged {
		data, err := json.Marshal(tp)
		if err != nil {
			return Result{}, fmt.Errorf("marshal TrustProfile: %w", err)
		}
		rv := tp.ObjectMeta.ResourceVersion
		if err := r.store.Update(ctx, trustProfileKind, req.Namespace, req.Name, data, rv); err != nil {
			if errors.Is(err, store.ErrConflict) {
				return Result{Requeue: true}, nil
			}
			return Result{}, fmt.Errorf("update TrustProfile: %w", err)
		}
		if err := r.ensurePromotionReview(ctx, &tp, evaluationID, requestedClass); err != nil {
			return Result{}, err
		}
		if tp.Status.PendingTrustChange != nil {
			if err := r.flushTrustChangeAudit(ctx, req.Namespace, req.Name); err != nil {
				return Result{}, err
			}
		}
	}

	delay := time.Duration(0)
	if interval > 0 {
		last, _ := parseStatusTime(tp.Status.LastEvaluationTime)
		delay = last.Add(interval).Sub(nowFunc())
		if delay <= 0 || last.IsZero() {
			delay = interval
		}
	}
	if probationUntil, _ := parseStatusTime(tp.Status.ProbationUntil); !probationUntil.IsZero() {
		probationDelay := probationUntil.Sub(nowFunc())
		if probationDelay > 0 && (delay <= 0 || probationDelay < delay) {
			delay = probationDelay
		}
	}
	if delay > 0 {
		return Result{RequeueAfter: delay}, nil
	}
	return Result{}, nil
}

func (r *TrustProfileReconciler) autonomyClassLevels(ctx context.Context, namespace string) (map[string]int, error) {
	levels := map[string]int{ClassSupervised: 10, ClassAssisted: 50, ClassAutonomous: 90}
	for _, ns := range []string{"system", namespace} {
		rawClasses, err := r.store.List(ctx, "AutonomyClass", store.ListOptions{Namespace: ns})
		if err != nil {
			return nil, fmt.Errorf("list AutonomyClasses for TrustProfile: %w", err)
		}
		for _, raw := range rawClasses {
			var class v1alpha1.AutonomyClass
			if json.Unmarshal(raw, &class) == nil && class.ObjectMeta.Name != "" {
				levels[class.ObjectMeta.Name] = class.Spec.Level
			}
		}
	}
	return levels, nil
}

func (r *TrustProfileReconciler) queryAgentAudit(ctx context.Context, tp *v1alpha1.TrustProfile, since, until time.Time) ([]observability.AuditEntry, error) {
	identities := []string{tp.Spec.AgentRef.Name}
	rawIdentities, err := r.store.List(ctx, "AgentIdentity", store.ListOptions{Namespace: tp.ObjectMeta.Namespace})
	if err != nil {
		return nil, fmt.Errorf("list AgentIdentities for TrustProfile: %w", err)
	}
	for _, raw := range rawIdentities {
		var identity v1alpha1.AgentIdentity
		if json.Unmarshal(raw, &identity) == nil && identity.Spec.AgentRef.Name == tp.Spec.AgentRef.Name && identity.Spec.SPIFFEID != "" {
			identities = append(identities, identity.Spec.SPIFFEID)
		}
	}
	byID := make(map[string]observability.AuditEntry)
	for _, identity := range identities {
		entries, err := r.audit.Query(ctx, observability.AuditFilter{Workspace: tp.ObjectMeta.Namespace, AgentIdentity: identity, Since: since, Until: until, Limit: maxTrustAuditEntriesPerIdentity + 1})
		if err != nil {
			return nil, fmt.Errorf("query CognitiveAuditLog for TrustProfile: %w", err)
		}
		if len(entries) > maxTrustAuditEntriesPerIdentity {
			return nil, fmt.Errorf("TrustProfile audit window exceeds %d entries for identity %q; reduce evaluationInterval", maxTrustAuditEntriesPerIdentity, identity)
		}
		for _, entry := range entries {
			byID[entry.ID] = entry
		}
	}
	entries := make([]observability.AuditEntry, 0, len(byID))
	for _, entry := range byID {
		entries = append(entries, entry)
	}
	return entries, nil
}

func trustEvaluationInterval(value string) (time.Duration, error) {
	if value == "" {
		return 0, nil
	}
	interval, err := time.ParseDuration(value)
	if err != nil || interval < time.Minute {
		return 0, fmt.Errorf("evaluationInterval must be a valid duration of at least 1m")
	}
	return interval, nil
}

func parseStatusTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, value)
}

func auditWindowStart(last, now time.Time, interval time.Duration) time.Time {
	if last.IsZero() {
		return now.Add(-interval)
	}
	return last.Add(time.Nanosecond)
}

func initializeTrustScore(tp *v1alpha1.TrustProfile) bool {
	if tp.Status.CurrentScore != 0 || tp.Status.TotalEvaluations != 0 || tp.Spec.InitialScore <= 0 {
		return false
	}
	tp.Status.CurrentScore = tp.Spec.InitialScore
	return true
}

// AdjustScore adjusts the trust score for an agent's profile.
func (r *TrustProfileReconciler) AdjustScore(ctx context.Context, namespace, name string, delta float64) error {
	const maxRetries = 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		raw, err := r.store.Get(ctx, trustProfileKind, namespace, name)
		if err != nil {
			return fmt.Errorf("get TrustProfile: %w", err)
		}

		var tp v1alpha1.TrustProfile
		if err := json.Unmarshal(raw, &tp); err != nil {
			return fmt.Errorf("unmarshal TrustProfile: %w", err)
		}

		initializeTrustScore(&tp)
		tp.Status.CurrentScore += delta
		tp.Status.TotalEvaluations++
		if tp.Status.CurrentScore < 0 {
			tp.Status.CurrentScore = 0
		}
		if tp.Status.CurrentScore > 100 {
			tp.Status.CurrentScore = 100
		}

		data, err := json.Marshal(tp)
		if err != nil {
			return fmt.Errorf("marshal TrustProfile: %w", err)
		}
		if err := r.store.Update(ctx, trustProfileKind, namespace, name, data, tp.ObjectMeta.ResourceVersion); err != nil {
			if errors.Is(err, store.ErrConflict) {
				continue
			}
			return fmt.Errorf("update TrustProfile: %w", err)
		}
		return nil
	}
	return fmt.Errorf("update TrustProfile: %w", store.ErrConflict)
}

// scoreTier determines the trust tier from a numeric score.
func scoreTier(score float64) string {
	switch {
	case score >= 90:
		return TierPrivileged
	case score >= 70:
		return TierTrusted
	case score >= 50:
		return TierStandard
	case score >= 25:
		return TierRestricted
	default:
		return TierUntrusted
	}
}
