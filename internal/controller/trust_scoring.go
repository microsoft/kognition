// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/store"
)

// Event types for scoring.
const (
	EventSuccess         = "success"
	EventFailure         = "failure"
	EventPolicyViolation = "policy_violation"
)

const maxTrustScoreHistoryEntries = 1000
const maxProcessedTrustEventIDs = 1000
const maxTrustConditions = 100

// Violation severities.
const (
	SeverityCritical = "critical"
	SeverityWarning  = "warning"
)

// Default scoring weights.
const (
	DefaultSuccessWeight   = 1.0
	DefaultFailureWeight   = -2.0
	DefaultViolationWeight = -10.0
)

// AutonomyClass level thresholds for promotion/demotion.
const (
	ClassSupervised = "supervised"
	ClassAssisted   = "assisted"
	ClassAutonomous = "autonomous"
)

// classLevel maps class names to numeric levels.
func classLevel(class string) int {
	switch class {
	case ClassAutonomous:
		return 90
	case ClassAssisted:
		return 50
	case ClassSupervised:
		return 10
	default:
		return 0
	}
}

// classForScore determines the autonomy class from a trust score.
func classForScore(score float64) string {
	switch {
	case score >= 80:
		return ClassAutonomous
	case score >= 50:
		return ClassAssisted
	default:
		return ClassSupervised
	}
}

// TrustScoringEvent represents a scorable event from the audit log.
type TrustScoringEvent struct {
	Type     string // "success", "failure", "policy_violation"
	Severity string // "critical", "warning", "" for non-violations
}

// ComputeWeightedScore computes the new trust score given a list of events and weights.
func ComputeWeightedScore(currentScore float64, events []TrustScoringEvent, weights *v1alpha1.ScoringWeights) float64 {
	sw := DefaultSuccessWeight
	fw := DefaultFailureWeight
	vw := DefaultViolationWeight

	if weights != nil {
		if weights.SuccessWeight != 0 {
			sw = weights.SuccessWeight
		}
		if weights.FailureWeight != 0 {
			fw = weights.FailureWeight
		}
		if weights.PolicyViolationWeight != 0 {
			vw = weights.PolicyViolationWeight
		}
	}

	delta := 0.0
	for _, e := range events {
		switch e.Type {
		case EventSuccess:
			delta += sw
		case EventFailure:
			delta += fw
		case EventPolicyViolation:
			delta += vw
		}
	}

	result := currentScore + delta
	if result < 0 {
		return 0
	}
	if result > 100 {
		return 100
	}
	return result
}

// EvaluatePromotion checks if a trust profile should be promoted to a higher class.
// Returns the new class name or empty string if no promotion.
func EvaluatePromotion(tp *v1alpha1.TrustProfile) string {
	return evaluatePromotionWithLevels(tp, nil)
}

func evaluatePromotionWithLevels(tp *v1alpha1.TrustProfile, levels map[string]int) string {
	if policy := tp.Spec.PromotionPolicy; policy != nil && len(policy.Thresholds) > 0 {
		current := effectiveCurrentClass(tp)
		candidate := ""
		candidateLevel := 0
		for _, threshold := range policy.Thresholds {
			level := resolvedClassLevel(threshold.TargetClass, levels)
			if tp.Status.CurrentScore >= threshold.MinScore && level > resolvedClassLevel(current, levels) && level > candidateLevel {
				candidate, candidateLevel = threshold.TargetClass, level
			}
		}
		if tp.Spec.MaxClass != "" && candidateLevel > resolvedClassLevel(tp.Spec.MaxClass, levels) {
			candidate = tp.Spec.MaxClass
		}
		if resolvedClassLevel(candidate, levels) > resolvedClassLevel(current, levels) {
			return candidate
		}
		return ""
	}
	targetClass := classForScore(tp.Status.CurrentScore)
	currentClass := effectiveCurrentClass(tp)

	// Check if target is above current.
	if classLevel(targetClass) <= classLevel(currentClass) {
		return ""
	}

	// Check maxClass cap.
	if tp.Spec.MaxClass != "" && classLevel(targetClass) > classLevel(tp.Spec.MaxClass) {
		targetClass = tp.Spec.MaxClass
		if classLevel(targetClass) <= classLevel(currentClass) {
			return ""
		}
	}

	return targetClass
}

func resolvedClassLevel(class string, levels map[string]int) int {
	if level, ok := levels[class]; ok {
		return level
	}
	return classLevel(class)
}

func effectiveCurrentClass(tp *v1alpha1.TrustProfile) string {
	if tp.Status.CurrentClass != "" {
		return tp.Status.CurrentClass
	}
	if tp.Spec.InitialClass != "" {
		return tp.Spec.InitialClass
	}
	return ClassSupervised
}

func validateTrustProfileClassLevels(tp *v1alpha1.TrustProfile, levels map[string]int) error {
	references := []struct {
		field string
		name  string
	}{
		{field: "spec.initialClass", name: tp.Spec.InitialClass},
		{field: "spec.maxClass", name: tp.Spec.MaxClass},
		{field: "status.currentClass", name: tp.Status.CurrentClass},
	}
	if tp.Spec.DemotionPolicy != nil {
		references = append(references, struct {
			field string
			name  string
		}{field: "spec.demotionPolicy.fallbackClass", name: tp.Spec.DemotionPolicy.FallbackClass})
	}
	if tp.Spec.PromotionPolicy != nil {
		for i, threshold := range tp.Spec.PromotionPolicy.Thresholds {
			references = append(references, struct {
				field string
				name  string
			}{field: fmt.Sprintf("spec.promotionPolicy.thresholds[%d].targetClass", i), name: threshold.TargetClass})
		}
	}
	for _, reference := range references {
		if reference.name == "" {
			continue
		}
		if _, ok := levels[reference.name]; !ok {
			return fmt.Errorf("TrustProfile %s references missing AutonomyClass %q", reference.field, reference.name)
		}
	}
	return nil
}

// DemotionResult describes the outcome of a demotion evaluation.
type DemotionResult struct {
	Demoted     bool
	NewClass    string
	Reason      string
	FreezeUntil time.Time
	Probation   bool
}

// EvaluateDemotion checks if a trust profile should be demoted based on events.
// Implements: immediate step-down, critical hard reset, warning probation.
func EvaluateDemotion(tp *v1alpha1.TrustProfile, events []TrustScoringEvent) DemotionResult {
	return evaluateDemotionWithLevels(tp, events, nil)
}

func evaluateDemotionWithLevels(tp *v1alpha1.TrustProfile, events []TrustScoringEvent, levels map[string]int) DemotionResult {
	currentClass := tp.Status.CurrentClass
	if currentClass == "" {
		currentClass = tp.Spec.InitialClass
	}
	if currentClass == "" {
		currentClass = ClassSupervised
	}

	policy := tp.Spec.DemotionPolicy
	fallbackClass := ClassSupervised
	if policy != nil && policy.FallbackClass != "" {
		fallbackClass = policy.FallbackClass
	}

	// Check for critical violations → hard reset to fallbackClass.
	for _, e := range events {
		if e.Type == EventPolicyViolation && e.Severity == SeverityCritical {
			return DemotionResult{
				Demoted:  true,
				NewClass: fallbackClass,
				Reason:   "critical policy violation: hard reset",
			}
		}
	}

	// Warning probation freezes the current class; it does not demote it.
	if policy != nil && policy.Mode == "probation" {
		for _, e := range events {
			if e.Type == EventPolicyViolation && e.Severity == SeverityWarning {
				freezeDur := 24 * time.Hour
				if policy.FreezeDuration != "" {
					if d, err := time.ParseDuration(policy.FreezeDuration); err == nil {
						freezeDur = d
					}
				}
				return DemotionResult{Probation: true, NewClass: currentClass, Reason: "warning violation: probation", FreezeUntil: nowFunc().Add(freezeDur)}
			}
		}
	}

	if currentClass == ClassSupervised {
		return DemotionResult{}
	}

	// Check if score has dropped below current class threshold.
	targetClass := classForScore(tp.Status.CurrentScore)
	currentLevel := resolvedClassLevel(currentClass, levels)
	if tp.Spec.PromotionPolicy != nil && len(tp.Spec.PromotionPolicy.Thresholds) > 0 {
		minimum := -1.0
		for _, threshold := range tp.Spec.PromotionPolicy.Thresholds {
			if threshold.TargetClass == currentClass {
				minimum = threshold.MinScore
				break
			}
		}
		if minimum >= 0 {
			if tp.Status.CurrentScore >= minimum {
				return DemotionResult{}
			}
			targetClass, currentLevel = "", resolvedClassLevel(currentClass, levels)
			bestLevel := -1
			for class, level := range levels {
				if level < currentLevel && level > bestLevel {
					targetClass, bestLevel = class, level
				}
			}
			if targetClass == "" {
				return DemotionResult{}
			}
		}
	}
	if resolvedClassLevel(targetClass, levels) >= currentLevel {
		return DemotionResult{}
	}

	// Default: immediate step-down.
	return DemotionResult{
		Demoted:  true,
		NewClass: targetClass,
		Reason:   "score below threshold: immediate step-down",
	}
}

// ApplyScoring applies a batch of trust scoring events to a TrustProfile and persists it.
func (r *TrustProfileReconciler) ApplyScoring(ctx context.Context, namespace, name string, events []TrustScoringEvent) error {
	return r.applyScoringOnce(ctx, namespace, name, "", events)
}

func (r *TrustProfileReconciler) applyScoringOnce(ctx context.Context, namespace, name, eventID string, events []TrustScoringEvent) error {
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
		levels, err := r.autonomyClassLevels(ctx, namespace)
		if err != nil {
			return err
		}
		if err := validateTrustProfileClassLevels(&tp, levels); err != nil {
			return err
		}
		if trustEventProcessed(&tp, eventID) {
			return r.ensurePromotionReview(ctx, &tp, eventID, pendingPromotionClass(&tp, levels))
		}
		requestedClass := r.applyScoring(&tp, eventID, events, levels)

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
		if tp.Status.PendingTrustChange != nil {
			if err := r.flushTrustChangeAudit(ctx, namespace, name); err != nil {
				return err
			}
		}
		return r.ensurePromotionReview(ctx, &tp, eventID, requestedClass)
	}
	return fmt.Errorf("update TrustProfile: %w", store.ErrConflict)
}

func trustScoreHistoryContains(history []v1alpha1.ScoreEntry, eventID string) bool {
	if eventID == "" {
		return false
	}
	for _, entry := range history {
		if entry.Reason == eventID {
			return true
		}
	}
	return false
}

func latestTrustScoreEvidence(history []v1alpha1.ScoreEntry) string {
	if len(history) == 0 {
		return ""
	}
	return history[len(history)-1].Reason
}

func (r *TrustProfileReconciler) applyScoring(tp *v1alpha1.TrustProfile, eventID string, events []TrustScoringEvent, levels map[string]int) string {
	defer trimTrustScoreHistory(tp)
	initializeTrustScore(tp)
	clearExpiredProbation(tp, nowFunc())

	// Initialize current class if empty.
	if tp.Status.CurrentClass == "" {
		tp.Status.CurrentClass = tp.Spec.InitialClass
		if tp.Status.CurrentClass == "" {
			tp.Status.CurrentClass = ClassSupervised
		}
	}

	// Compute new score.
	oldScore := tp.Status.CurrentScore
	tp.Status.CurrentScore = ComputeWeightedScore(oldScore, events, tp.Spec.ScoringWeights)
	tp.Status.TotalEvaluations++
	tp.Status.AuditedActions += int64(len(events))

	// Record score history entry.
	reason := eventID
	if reason == "" {
		reason = fmt.Sprintf("scoring: %d events", len(events))
	}
	tp.Status.ScoreHistory = append(tp.Status.ScoreHistory, v1alpha1.ScoreEntry{
		Timestamp: nowFunc().Format(time.RFC3339),
		Score:     tp.Status.CurrentScore,
		Reason:    reason,
	})
	if eventID != "" {
		tp.Status.ProcessedEventIDs = append(tp.Status.ProcessedEventIDs, eventID)
		if len(tp.Status.ProcessedEventIDs) > maxProcessedTrustEventIDs {
			tp.Status.ProcessedEventIDs = append([]string(nil), tp.Status.ProcessedEventIDs[len(tp.Status.ProcessedEventIDs)-maxProcessedTrustEventIDs:]...)
		}
	}
	trimTrustScoreHistory(tp)

	// Evaluate demotion first (takes priority).
	demotion := evaluateDemotionWithLevels(tp, events, levels)
	if demotion.Probation {
		tp.Status.ProbationUntil = demotion.FreezeUntil.UTC().Format(time.RFC3339Nano)
		tp.Status.PendingTrustChange = newTrustChangeAudit("probation", tp.Status.CurrentClass, tp.Status.CurrentClass, reason, nowFunc())
		removeTrustConditions(tp, "Demoted", "Promoted", "PromotionPending")
		setTrustCondition(tp, v1alpha1.StatusCondition{Type: "Probation", Status: "True", Reason: demotion.Reason, Message: fmt.Sprintf("promotion frozen until %s", tp.Status.ProbationUntil)})
	} else if demotion.Demoted {
		previousClass := tp.Status.CurrentClass
		r.logger.Warn("trust profile demoted",
			zap.String("agent", tp.Spec.AgentRef.Name),
			zap.String("from", previousClass),
			zap.String("to", demotion.NewClass),
			zap.String("reason", demotion.Reason))
		tp.Status.CurrentClass = demotion.NewClass
		tp.Status.PendingTrustChange = newTrustChangeAudit("demotion", previousClass, demotion.NewClass, reason, nowFunc())
		removeTrustConditions(tp, "Probation", "Promoted", "PromotionPending")
		setTrustCondition(tp, v1alpha1.StatusCondition{
			Type:    "Demoted",
			Status:  "True",
			Reason:  demotion.Reason,
			Message: fmt.Sprintf("demoted from %s to %s", previousClass, demotion.NewClass),
		})
	} else {
		// Evaluate promotion.
		newClass := evaluatePromotionWithLevels(tp, levels)
		if newClass == "" {
			tp.Status.ThresholdMetSince = ""
			tp.Status.ThresholdClass = ""
			removeTrustConditions(tp, "PromotionPending")
		} else if tp.Status.ThresholdMetSince == "" || tp.Status.ThresholdClass != newClass {
			tp.Status.ThresholdMetSince = nowFunc().UTC().Format(time.RFC3339Nano)
			tp.Status.ThresholdClass = newClass
		}
		if newClass != "" && promotionEligible(tp, newClass, nowFunc()) {
			if promotionRequiresReview(tp, newClass) {
				removeTrustConditions(tp, "Demoted", "Probation", "Promoted")
				setTrustCondition(tp, v1alpha1.StatusCondition{
					Type:    "PromotionPending",
					Status:  "True",
					Reason:  "review required",
					Message: fmt.Sprintf("promotion to %s requires review", newClass),
				})
				tp.Status.CurrentTier = scoreTier(tp.Status.CurrentScore)
				return newClass
			}
			previousClass := tp.Status.CurrentClass
			r.logger.Info("trust profile promoted",
				zap.String("agent", tp.Spec.AgentRef.Name),
				zap.String("from", tp.Status.CurrentClass),
				zap.String("to", newClass))
			tp.Status.CurrentClass = newClass
			tp.Status.PendingTrustChange = newTrustChangeAudit("promotion", previousClass, newClass, reason, nowFunc())
			tp.Status.ThresholdMetSince = ""
			tp.Status.ThresholdClass = ""
			removeTrustConditions(tp, "Demoted", "Probation", "PromotionPending")
			setTrustCondition(tp, v1alpha1.StatusCondition{
				Type:    "Promoted",
				Status:  "True",
				Reason:  "score threshold met",
				Message: fmt.Sprintf("promoted to %s", newClass),
			})
		}
	}

	// Update tier from score.
	tp.Status.CurrentTier = scoreTier(tp.Status.CurrentScore)
	return ""
}

func trustEventProcessed(tp *v1alpha1.TrustProfile, eventID string) bool {
	if eventID == "" {
		return false
	}
	for _, processed := range tp.Status.ProcessedEventIDs {
		if processed == eventID {
			return true
		}
	}
	return trustScoreHistoryContains(tp.Status.ScoreHistory, eventID)
}

func promotionRequiresReview(tp *v1alpha1.TrustProfile, target string) bool {
	if tp.Spec.PromotionPolicy == nil {
		return false
	}
	for _, threshold := range tp.Spec.PromotionPolicy.Thresholds {
		if threshold.TargetClass == target {
			return threshold.RequireReview
		}
	}
	return tp.Spec.PromotionPolicy.RequireReview
}

func promotionEligible(tp *v1alpha1.TrustProfile, target string, now time.Time) bool {
	policy := tp.Spec.PromotionPolicy
	if policy == nil {
		return !probationActive(tp, now)
	}
	if probationActive(tp, now) || (policy.MinActions > 0 && tp.Status.AuditedActions < policy.MinActions) {
		return false
	}
	duration := time.Duration(0)
	if policy.MinScoreDuration != "" {
		duration, _ = time.ParseDuration(policy.MinScoreDuration)
	}
	if policy.StabilizationWindowDays > 0 {
		duration = time.Duration(policy.StabilizationWindowDays) * 24 * time.Hour
	}
	if duration <= 0 {
		return true
	}
	since, _ := parseStatusTime(tp.Status.ThresholdMetSince)
	return !since.IsZero() && !now.Before(since.Add(duration))
}

func probationActive(tp *v1alpha1.TrustProfile, now time.Time) bool {
	until, _ := parseStatusTime(tp.Status.ProbationUntil)
	return !until.IsZero() && now.Before(until)
}

func clearExpiredProbation(tp *v1alpha1.TrustProfile, now time.Time) {
	if tp.Status.ProbationUntil == "" || probationActive(tp, now) {
		return
	}
	tp.Status.ProbationUntil = ""
	setTrustCondition(tp, v1alpha1.StatusCondition{Type: "Probation", Status: "False", Reason: "Expired", Message: "promotion probation has expired"})
}

func setTrustCondition(tp *v1alpha1.TrustProfile, condition v1alpha1.StatusCondition) {
	filtered := tp.Status.Conditions[:0]
	for _, existing := range tp.Status.Conditions {
		if existing.Type != condition.Type {
			filtered = append(filtered, existing)
		}
	}
	tp.Status.Conditions = append(filtered, condition)
}

func removeTrustConditions(tp *v1alpha1.TrustProfile, conditionTypes ...string) {
	removed := make(map[string]struct{}, len(conditionTypes))
	for _, conditionType := range conditionTypes {
		removed[conditionType] = struct{}{}
	}
	filtered := tp.Status.Conditions[:0]
	for _, condition := range tp.Status.Conditions {
		if _, ok := removed[condition.Type]; !ok {
			filtered = append(filtered, condition)
		}
	}
	tp.Status.Conditions = filtered
}

// applyAuditEvaluation computes an absolute 0..100 score from normalized audit decisions.
func (r *TrustProfileReconciler) applyAuditEvaluation(tp *v1alpha1.TrustProfile, entries []observability.AuditEntry, now time.Time, evaluationID string, levels map[string]int) string {
	defer trimTrustScoreHistory(tp)
	dimensions, observations, actions, events := aggregateTrustAudit(entries)
	initializeTrustScore(tp)
	clearExpiredProbation(tp, now)
	if tp.Status.CurrentClass == "" {
		tp.Status.CurrentClass = effectiveCurrentClass(tp)
	}
	if actions > 0 {
		tp.Status.CurrentScore = weightedDimensionScore(dimensions, observations, tp.Spec.ScoringWeights)
		tp.Status.AuditedActions += int64(actions)
		tp.Status.TotalEvaluations++
	}
	tp.Status.CurrentTier = scoreTier(tp.Status.CurrentScore)
	tp.Status.LastEvaluationTime = now.UTC().Format(time.RFC3339Nano)
	tp.Status.ScoreHistory = append(tp.Status.ScoreHistory, v1alpha1.ScoreEntry{Timestamp: tp.Status.LastEvaluationTime, Score: tp.Status.CurrentScore, Class: tp.Status.CurrentClass, Reason: evaluationID, Dimensions: &dimensions})
	trimTrustScoreHistory(tp)
	demotion := evaluateDemotionWithLevels(tp, events, levels)
	if demotion.Probation {
		tp.Status.ProbationUntil = demotion.FreezeUntil.UTC().Format(time.RFC3339Nano)
		tp.Status.PendingTrustChange = newTrustChangeAudit("probation", tp.Status.CurrentClass, tp.Status.CurrentClass, evaluationID, now)
		removeTrustConditions(tp, "Demoted", "Promoted", "PromotionPending")
		setTrustCondition(tp, v1alpha1.StatusCondition{Type: "Probation", Status: "True", Reason: demotion.Reason, Message: fmt.Sprintf("promotion frozen until %s", tp.Status.ProbationUntil)})
	} else if demotion.Demoted {
		previousClass := tp.Status.CurrentClass
		tp.Status.CurrentClass = demotion.NewClass
		tp.Status.PendingTrustChange = newTrustChangeAudit("demotion", previousClass, demotion.NewClass, evaluationID, now)
		tp.Status.ThresholdMetSince = ""
		tp.Status.ThresholdClass = ""
		removeTrustConditions(tp, "Probation", "Promoted", "PromotionPending")
		setTrustCondition(tp, v1alpha1.StatusCondition{Type: "Demoted", Status: "True", Reason: demotion.Reason, Message: "periodic audit evaluation triggered demotion"})
		return ""
	}
	target := evaluatePromotionWithLevels(tp, levels)
	if target == "" {
		tp.Status.ThresholdMetSince = ""
		tp.Status.ThresholdClass = ""
		removeTrustConditions(tp, "PromotionPending")
		return ""
	}
	if tp.Status.ThresholdMetSince == "" || tp.Status.ThresholdClass != target {
		tp.Status.ThresholdMetSince = now.UTC().Format(time.RFC3339Nano)
		tp.Status.ThresholdClass = target
	}
	if !promotionEligible(tp, target, now) {
		return ""
	}
	if promotionRequiresReview(tp, target) {
		removeTrustConditions(tp, "Demoted", "Probation", "Promoted")
		setTrustCondition(tp, v1alpha1.StatusCondition{Type: "PromotionPending", Status: "True", Reason: "review required", Message: fmt.Sprintf("promotion to %s requires review", target)})
		return target
	}
	previousClass := tp.Status.CurrentClass
	tp.Status.CurrentClass = target
	tp.Status.PendingTrustChange = newTrustChangeAudit("promotion", previousClass, target, evaluationID, now)
	tp.Status.ThresholdMetSince = ""
	tp.Status.ThresholdClass = ""
	removeTrustConditions(tp, "Demoted", "Probation", "PromotionPending")
	setTrustCondition(tp, v1alpha1.StatusCondition{Type: "Promoted", Status: "True", Reason: "audit score threshold met", Message: fmt.Sprintf("promoted to %s", target)})
	return ""
}

func trimTrustScoreHistory(tp *v1alpha1.TrustProfile) {
	if len(tp.Status.ScoreHistory) > maxTrustScoreHistoryEntries {
		tp.Status.ScoreHistory = append([]v1alpha1.ScoreEntry(nil), tp.Status.ScoreHistory[len(tp.Status.ScoreHistory)-maxTrustScoreHistoryEntries:]...)
	}
	if len(tp.Status.Conditions) > maxTrustConditions {
		tp.Status.Conditions = append([]v1alpha1.StatusCondition(nil), tp.Status.Conditions[len(tp.Status.Conditions)-maxTrustConditions:]...)
	}
}

type trustScoreObservations struct {
	TaskCompletion, PolicyCompliance, ApprovalAlignment, CostEfficiency, ToolUsage bool
}

func weightedDimensionScore(d v1alpha1.TrustScoreDimensions, observed trustScoreObservations, weights *v1alpha1.ScoringWeights) float64 {
	w := v1alpha1.ScoringWeights{TaskCompletionRate: .25, PolicyCompliance: .30, ApprovalAlignmentRate: .20, CostEfficiency: .10, ToolUsageAccuracy: .15}
	if weights != nil && weights.TaskCompletionRate+weights.PolicyCompliance+weights.ApprovalAlignmentRate+weights.CostEfficiency+weights.ToolUsageAccuracy > 0 {
		w = *weights
	}
	score, denominator := 0.0, 0.0
	if observed.TaskCompletion {
		score += d.TaskCompletionRate * w.TaskCompletionRate
		denominator += w.TaskCompletionRate
	}
	if observed.PolicyCompliance {
		score += d.PolicyCompliance * w.PolicyCompliance
		denominator += w.PolicyCompliance
	}
	if observed.ApprovalAlignment {
		score += d.ApprovalAlignmentRate * w.ApprovalAlignmentRate
		denominator += w.ApprovalAlignmentRate
	}
	if observed.CostEfficiency {
		score += d.CostEfficiency * w.CostEfficiency
		denominator += w.CostEfficiency
	}
	if observed.ToolUsage {
		score += d.ToolUsageAccuracy * w.ToolUsageAccuracy
		denominator += w.ToolUsageAccuracy
	}
	if denominator == 0 {
		return 0
	}
	return 100 * score / denominator
}

func aggregateTrustAudit(entries []observability.AuditEntry) (v1alpha1.TrustScoreDimensions, trustScoreObservations, int, []TrustScoringEvent) {
	type counts struct{ good, total int }
	task, policy, approval, cost, tool := counts{}, counts{}, counts{}, counts{}, counts{}
	decisions := make(map[string]observability.TrustDecisionV1)
	for _, entry := range entries {
		var decision observability.TrustDecisionV1
		if json.Unmarshal(entry.Details, &decision) != nil || decision.SchemaVersion != observability.TrustDecisionSchemaVersion {
			continue
		}
		key := decision.Request.InvocationID
		if key == "" {
			key = decision.ID
		}
		previous, exists := decisions[key]
		terminal := decision.Execution.Status == "completed" || decision.Execution.Status == "failed" || decision.Outcome == "denied" || decision.Outcome == "revoked"
		previousTerminal := previous.Execution.Status == "completed" || previous.Execution.Status == "failed" || previous.Outcome == "denied" || previous.Outcome == "revoked"
		if !exists || (terminal && !previousTerminal) || (terminal == previousTerminal && decision.Timestamp.After(previous.Timestamp)) {
			decisions[key] = decision
		}
	}
	events := make([]TrustScoringEvent, 0)
	actions := 0
	for _, decision := range decisions {
		terminal := decision.Execution.Status == "completed" || decision.Execution.Status == "failed" || decision.Outcome == "denied" || decision.Outcome == "revoked"
		if !terminal {
			continue
		}
		actions++
		if decision.Execution.Status == "completed" || decision.Execution.Status == "failed" {
			task.total++
			if decision.Execution.Status == "completed" {
				task.good++
			}
		}
		policy.total++
		if decision.ReasonCode == "policy_denied" || decision.ReasonCode == "argument_constraints_denied" {
			events = append(events, TrustScoringEvent{Type: EventPolicyViolation, Severity: SeverityCritical})
		} else {
			policy.good++
		}
		if decision.Approval != nil {
			approval.total++
			if decision.Approval.Status == "approved" {
				approval.good++
			} else if decision.Approval.Status == "rejected" {
				events = append(events, TrustScoringEvent{Type: EventPolicyViolation, Severity: SeverityWarning})
			}
		}
		cost.total++
		if decision.ReasonCode != "quota_exhausted" {
			cost.good++
		}
		if decision.Plane == "tool" {
			if decision.Execution.Status == "completed" || decision.Execution.Status == "failed" {
				tool.total++
				if decision.Execution.Status == "completed" {
					tool.good++
				} else {
					events = append(events, TrustScoringEvent{Type: EventPolicyViolation, Severity: SeverityWarning})
				}
			}
		}
	}
	rate := func(c counts) float64 {
		if c.total == 0 {
			return 0
		}
		return float64(c.good) / float64(c.total)
	}
	dimensions := v1alpha1.TrustScoreDimensions{TaskCompletionRate: rate(task), PolicyCompliance: rate(policy), ApprovalAlignmentRate: rate(approval), CostEfficiency: rate(cost), ToolUsageAccuracy: rate(tool)}
	observed := trustScoreObservations{TaskCompletion: task.total > 0, PolicyCompliance: policy.total > 0, ApprovalAlignment: approval.total > 0, CostEfficiency: cost.total > 0, ToolUsage: tool.total > 0}
	return dimensions, observed, actions, events
}

func pendingPromotionClass(profile *v1alpha1.TrustProfile, levels map[string]int) string {
	if profile.Spec.PromotionPolicy == nil {
		return ""
	}
	target := evaluatePromotionWithLevels(profile, levels)
	if target == "" || !promotionRequiresReview(profile, target) || !promotionEligible(profile, target, nowFunc()) {
		return ""
	}
	return target
}

func (r *TrustProfileReconciler) ensurePromotionReview(ctx context.Context, profile *v1alpha1.TrustProfile, eventID, requestedClass string) error {
	if requestedClass == "" {
		return nil
	}
	reviews, err := r.store.List(ctx, autonomyReviewKind, store.ListOptions{Namespace: profile.ObjectMeta.Namespace})
	if err != nil {
		return fmt.Errorf("list promotion reviews: %w", err)
	}
	for _, raw := range reviews {
		var review v1alpha1.AutonomyReview
		if json.Unmarshal(raw, &review) == nil &&
			review.Spec.AgentRef.Name == profile.Spec.AgentRef.Name &&
			review.Spec.TrustProfileRef == profile.ObjectMeta.Name &&
			review.Spec.RequestedClass == requestedClass &&
			review.Spec.Evidence == eventID &&
			(review.Status.Decision == "" || review.Status.Decision == DecisionPending) {
			return nil
		}
	}
	agentName := profile.Spec.AgentRef.Name
	hash := sha256.Sum256([]byte(agentName + "\x00" + requestedClass + "\x00" + eventID))
	baseName := agentName
	if len(baseName) > 40 {
		baseName = baseName[:40]
	}
	reviewName := fmt.Sprintf("%s-promotion-%x", baseName, hash[:6])
	review := v1alpha1.AutonomyReview{
		TypeMeta: v1alpha1.TypeMeta{Kind: autonomyReviewKind, APIVersion: v1alpha1.APIVersion},
		ObjectMeta: v1alpha1.ObjectMeta{
			Name:              reviewName,
			Namespace:         profile.ObjectMeta.Namespace,
			CreationTimestamp: nowFunc(),
		},
		Spec: v1alpha1.AutonomyReviewSpec{
			AgentRef:          profile.Spec.AgentRef,
			ActionDescription: fmt.Sprintf("promote agent from %s to %s", profile.Status.CurrentClass, requestedClass),
			CurrentClass:      profile.Status.CurrentClass,
			RequestedClass:    requestedClass,
			TrustProfileRef:   profile.ObjectMeta.Name,
			Evidence:          eventID,
		},
		Status: v1alpha1.AutonomyReviewStatus{
			Decision:  DecisionPending,
			ExpiresAt: nowFunc().UTC().Add(defaultAutonomyReviewTTL).Format(time.RFC3339),
		},
	}
	data, err := json.Marshal(review)
	if err != nil {
		return fmt.Errorf("marshal promotion review: %w", err)
	}
	if err := r.store.Create(ctx, autonomyReviewKind, review.ObjectMeta.Namespace, review.ObjectMeta.Name, data); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		return fmt.Errorf("create promotion review: %w", err)
	}
	return nil
}

func (r *TrustProfileReconciler) recordTrustChange(ctx context.Context, profile *v1alpha1.TrustProfile, change *v1alpha1.TrustChangeAudit) error {
	if r.audit == nil {
		return nil
	}
	details, err := json.Marshal(map[string]string{"profile": profile.ObjectMeta.Name, "kind": change.Kind, "fromClass": change.FromClass, "toClass": change.ToClass, "evidence": change.Evidence})
	if err != nil {
		return err
	}
	idHash := sha256.Sum256([]byte(profile.ObjectMeta.Namespace + "\x00" + profile.ObjectMeta.Name + "\x00" + change.Kind + "\x00" + change.FromClass + "\x00" + change.ToClass + "\x00" + change.Evidence))
	timestamp, err := parseStatusTime(change.Timestamp)
	if err != nil {
		return fmt.Errorf("parse TrustProfile trust-change timestamp: %w", err)
	}
	if timestamp.IsZero() {
		return errors.New("TrustProfile trust-change timestamp is required")
	}
	return r.audit.Record(ctx, observability.AuditEntry{ID: fmt.Sprintf("trust-change-%x", idHash[:12]), Timestamp: timestamp, Workspace: profile.ObjectMeta.Namespace, AgentIdentity: profile.Spec.AgentRef.Name, EventType: observability.AuditTrustChange, SubjectKind: trustProfileKind, SubjectRef: profile.ObjectMeta.Name, Verb: change.Kind, Details: details})
}

func newTrustChangeAudit(kind, fromClass, toClass, evidence string, timestamp time.Time) *v1alpha1.TrustChangeAudit {
	return &v1alpha1.TrustChangeAudit{Kind: kind, FromClass: fromClass, ToClass: toClass, Evidence: evidence, Timestamp: timestamp.UTC().Format(time.RFC3339Nano)}
}

func (r *TrustProfileReconciler) flushTrustChangeAudit(ctx context.Context, namespace, name string) error {
	const maxRetries = 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		raw, err := r.store.Get(ctx, trustProfileKind, namespace, name)
		if err != nil {
			return err
		}
		var profile v1alpha1.TrustProfile
		if err := json.Unmarshal(raw, &profile); err != nil {
			return err
		}
		change := profile.Status.PendingTrustChange
		if change == nil {
			return nil
		}
		if change.Timestamp == "" {
			change.Timestamp = nowFunc().UTC().Format(time.RFC3339Nano)
			data, marshalErr := json.Marshal(profile)
			if marshalErr != nil {
				return marshalErr
			}
			if updateErr := r.store.Update(ctx, trustProfileKind, namespace, name, data, profile.ObjectMeta.ResourceVersion); updateErr != nil {
				if errors.Is(updateErr, store.ErrConflict) {
					continue
				}
				return updateErr
			}
			continue
		}
		if err := r.recordTrustChange(ctx, &profile, change); err != nil {
			return fmt.Errorf("record TrustProfile trust change: %w", err)
		}
		profile.Status.PendingTrustChange = nil
		data, err := json.Marshal(profile)
		if err != nil {
			return err
		}
		if err := r.store.Update(ctx, trustProfileKind, namespace, name, data, profile.ObjectMeta.ResourceVersion); err != nil {
			if errors.Is(err, store.ErrConflict) {
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("clear TrustProfile trust-change outbox: %w", store.ErrConflict)
}

// RecordAgentInstanceOutcome applies one idempotent score event to every TrustProfile for the agent.
func (r *TrustProfileReconciler) RecordAgentInstanceOutcome(ctx context.Context, instance *v1alpha1.AgentInstance) error {
	if instance == nil || instance.Spec.SessionContext["input"] == "" {
		return nil
	}
	// Periodic normalized audit ingestion is authoritative when available.
	// Legacy stores without a CognitiveAuditLog retain event-driven scoring.
	if r.audit != nil {
		return nil
	}
	var event TrustScoringEvent
	switch instance.Status.Phase {
	case v1alpha1.PhaseReady:
		event = TrustScoringEvent{Type: EventSuccess}
	case v1alpha1.PhaseFailed:
		event = TrustScoringEvent{Type: EventFailure}
	default:
		return nil
	}
	profiles, err := r.store.List(ctx, trustProfileKind, store.ListOptions{Namespace: instance.ObjectMeta.Namespace})
	if err != nil {
		return fmt.Errorf("list TrustProfiles for agent outcome: %w", err)
	}
	eventID := agentInstanceTrustEventID(instance, event.Type)
	for _, raw := range profiles {
		var profile v1alpha1.TrustProfile
		if err := json.Unmarshal(raw, &profile); err != nil || profile.Spec.AgentRef.Name != instance.Spec.AgentRef.Name {
			continue
		}
		if err := r.applyScoringOnce(ctx, profile.ObjectMeta.Namespace, profile.ObjectMeta.Name, eventID, []TrustScoringEvent{event}); err != nil {
			return err
		}
	}
	return nil
}

func agentInstanceTrustEventID(instance *v1alpha1.AgentInstance, eventType string) string {
	identity := instance.ObjectMeta.UID
	if identity == "" {
		identity = instance.ObjectMeta.Namespace + "/" + instance.ObjectMeta.Name
	}
	return "agentinstance:" + identity + ":" + eventType
}

// GetTrustProfile retrieves a TrustProfile by namespace and name.
func (r *TrustProfileReconciler) GetTrustProfile(ctx context.Context, namespace, name string) (*v1alpha1.TrustProfile, error) {
	raw, err := r.store.Get(ctx, trustProfileKind, namespace, name)
	if err != nil {
		return nil, err
	}

	var tp v1alpha1.TrustProfile
	if err := json.Unmarshal(raw, &tp); err != nil {
		return nil, fmt.Errorf("unmarshal TrustProfile: %w", err)
	}
	return &tp, nil
}
