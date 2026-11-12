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

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

const (
	autonomyReviewKind       = "AutonomyReview"
	defaultAutonomyReviewTTL = 5 * time.Minute
)

var (
	ErrAutonomyReviewNotPending = errors.New("autonomy review is not pending")
	ErrAutonomyReviewExpired    = errors.New("autonomy review deadline has passed")
)

// AutonomyReviewReconciler manages the lifecycle of AutonomyReview resources,
// handling TTL expiration, auto-denial, and decision recording.
type AutonomyReviewReconciler struct {
	store      store.Store
	logger     *zap.Logger
	defaultTTL time.Duration
	audit      *observability.CognitiveAuditLog
}

// NewAutonomyReviewReconciler creates a new AutonomyReviewReconciler.
func NewAutonomyReviewReconciler(s store.Store, logger *zap.Logger) *AutonomyReviewReconciler {
	return &AutonomyReviewReconciler{
		store:      s,
		logger:     logger.With(zap.String("controller", "autonomy-review")),
		defaultTTL: defaultAutonomyReviewTTL,
	}
}

// WithTTL sets the default TTL for reviews awaiting decision.
func (r *AutonomyReviewReconciler) WithTTL(ttl time.Duration) *AutonomyReviewReconciler {
	r.defaultTTL = ttl
	return r
}

// SetAuditLog installs the durable recorder for review decisions.
func (r *AutonomyReviewReconciler) SetAuditLog(audit *observability.CognitiveAuditLog) *AutonomyReviewReconciler {
	r.audit = audit
	return r
}

// Review decisions.
const (
	DecisionPending  = "Pending"
	DecisionApproved = "Approved"
	DecisionDenied   = "Denied"
)

// Reconcile processes AutonomyReview resources.
func (r *AutonomyReviewReconciler) Reconcile(ctx context.Context, req Request) (Result, error) {
	raw, err := r.store.Get(ctx, autonomyReviewKind, req.Namespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("get AutonomyReview: %w", err)
	}

	var review v1alpha1.AutonomyReview
	if err := json.Unmarshal(raw, &review); err != nil {
		return Result{}, fmt.Errorf("unmarshal AutonomyReview: %w", err)
	}

	now := nowFunc().UTC()

	// Normalize reviews persisted by versions that used the obsolete Expired state.
	if review.Status.Decision == "Expired" {
		r.setTTLDenial(&review, now)
		if err := r.recordDecision(ctx, &review); err != nil {
			return Result{}, err
		}
		if err := r.updateReview(ctx, req, &review); err != nil {
			return Result{}, err
		}
		return Result{}, r.applyTrustProfileDecision(ctx, &review)
	}

	// If already decided, apply its effect idempotently.
	if review.Status.Decision != "" && review.Status.Decision != DecisionPending {
		if err := r.recordDecision(ctx, &review); err != nil {
			return Result{}, err
		}
		return Result{}, r.applyTrustProfileDecision(ctx, &review)
	}

	statusChanged := false
	if review.Status.Decision == "" {
		review.Status.Decision = DecisionPending
		statusChanged = true
	}
	expiresAt, initialized, err := r.expiration(&review, now)
	if err != nil {
		return Result{}, err
	}
	statusChanged = statusChanged || initialized
	if !now.Before(expiresAt) {
		r.setTTLDenial(&review, now)
		if err := r.recordDecision(ctx, &review); err != nil {
			return Result{}, err
		}
		if err := r.updateReview(ctx, req, &review); err != nil {
			return Result{}, err
		}
		r.logger.Info("review auto-denied after TTL expired", zap.String("review", req.Name), requestid.ZapField(ctx))
		return Result{}, r.applyTrustProfileDecision(ctx, &review)
	}
	if statusChanged {
		if err := r.updateReview(ctx, req, &review); err != nil {
			return Result{}, err
		}
	}
	return Result{RequeueAfter: expiresAt.Sub(now)}, nil
}

func (r *AutonomyReviewReconciler) expiration(review *v1alpha1.AutonomyReview, now time.Time) (time.Time, bool, error) {
	if review.Status.ExpiresAt != "" {
		expiresAt, err := time.Parse(time.RFC3339, review.Status.ExpiresAt)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("parse AutonomyReview status.expiresAt: %w", err)
		}
		return expiresAt, false, nil
	}
	createdAt := review.ObjectMeta.CreationTimestamp
	if createdAt.IsZero() {
		createdAt = now
	}
	expiresAt := createdAt.UTC().Add(r.defaultTTL)
	review.Status.ExpiresAt = expiresAt.Format(time.RFC3339)
	return expiresAt, true, nil
}

func (r *AutonomyReviewReconciler) setTTLDenial(review *v1alpha1.AutonomyReview, now time.Time) {
	review.Status.Decision = DecisionDenied
	review.Status.DecidedBy = "system/ttl"
	review.Status.Reason = "TTL expired"
	review.Status.DecidedAt = now.Format(time.RFC3339)
}

func (r *AutonomyReviewReconciler) applyTrustProfileDecision(ctx context.Context, review *v1alpha1.AutonomyReview) error {
	if review.Spec.TrustProfileRef == "" {
		return nil
	}
	const maxRetries = 5
	marker := autonomyReviewDecisionMarker(review)
	for attempt := 0; attempt < maxRetries; attempt++ {
		raw, err := r.store.Get(ctx, trustProfileKind, review.ObjectMeta.Namespace, review.Spec.TrustProfileRef)
		if err != nil {
			return fmt.Errorf("get promotion TrustProfile: %w", err)
		}
		var profile v1alpha1.TrustProfile
		if err := json.Unmarshal(raw, &profile); err != nil {
			return fmt.Errorf("unmarshal promotion TrustProfile: %w", err)
		}
		if trustScoreHistoryContains(profile.Status.ScoreHistory, marker) {
			return r.flushTrustProfileChange(ctx, &profile)
		}
		if profile.Status.ApprovalStats == nil {
			profile.Status.ApprovalStats = &v1alpha1.ApprovalStats{}
		}
		profile.Status.ApprovalStats.TotalRequested++
		levels, err := NewTrustProfileReconciler(r.store, r.logger).autonomyClassLevels(ctx, review.ObjectMeta.Namespace)
		if err != nil {
			return err
		}
		if err := validateTrustProfileClassLevels(&profile, levels); err != nil {
			return err
		}
		switch review.Status.Decision {
		case DecisionApproved:
			profile.Status.ApprovalStats.TotalApproved++
			if promotionReviewStillEligible(&profile, review, levels) {
				previousClass := profile.Status.CurrentClass
				profile.Status.CurrentClass = review.Spec.RequestedClass
				evidence := review.Spec.Evidence
				if evidence == "" {
					evidence = marker
				}
				profile.Status.PendingTrustChange = newTrustChangeAudit("promotion", previousClass, review.Spec.RequestedClass, evidence, nowFunc())
				removeTrustConditions(&profile, "Demoted", "Probation", "PromotionPending", "PromotionRejected")
				setTrustCondition(&profile, v1alpha1.StatusCondition{
					Type: "Promoted", Status: "True", Reason: "review approved",
					Message: fmt.Sprintf("promotion to %s approved by %s", review.Spec.RequestedClass, review.Status.DecidedBy),
				})
			} else {
				removeTrustConditions(&profile, "PromotionPending")
				setTrustCondition(&profile, v1alpha1.StatusCondition{
					Type: "PromotionRejected", Status: "True", Reason: "eligibility changed",
					Message: fmt.Sprintf("approved promotion to %s was not applied because current eligibility changed", review.Spec.RequestedClass),
				})
			}
		case DecisionDenied:
			profile.Status.ApprovalStats.TotalDenied++
		}
		profile.Status.ScoreHistory = append(profile.Status.ScoreHistory, v1alpha1.ScoreEntry{
			Timestamp: nowFunc().Format(time.RFC3339),
			Score:     profile.Status.CurrentScore,
			Reason:    marker,
		})
		trimTrustScoreHistory(&profile)
		data, err := json.Marshal(profile)
		if err != nil {
			return fmt.Errorf("marshal promotion TrustProfile: %w", err)
		}
		if err := r.store.Update(ctx, trustProfileKind, profile.ObjectMeta.Namespace, profile.ObjectMeta.Name, data, profile.ObjectMeta.ResourceVersion); err != nil {
			if errors.Is(err, store.ErrConflict) {
				continue
			}
			return fmt.Errorf("update promotion TrustProfile: %w", err)
		}
		return r.flushTrustProfileChange(ctx, &profile)
	}
	return fmt.Errorf("update promotion TrustProfile: %w", store.ErrConflict)
}

func (r *AutonomyReviewReconciler) flushTrustProfileChange(ctx context.Context, profile *v1alpha1.TrustProfile) error {
	if profile.Status.PendingTrustChange == nil {
		return nil
	}
	reconciler := NewTrustProfileReconciler(r.store, r.logger)
	reconciler.SetAuditLog(r.audit)
	return reconciler.flushTrustChangeAudit(ctx, profile.ObjectMeta.Namespace, profile.ObjectMeta.Name)
}

func autonomyReviewDecisionMarker(review *v1alpha1.AutonomyReview) string {
	spec, _ := json.Marshal(review.Spec)
	digest := sha256.Sum256(spec)
	identity := review.ObjectMeta.UID
	if identity == "" {
		identity = review.ObjectMeta.Namespace + "/" + review.ObjectMeta.Name
	}
	return fmt.Sprintf("autonomyreview:%s:%x:%s", identity, digest[:8], review.Status.Decision)
}

func promotionReviewStillEligible(profile *v1alpha1.TrustProfile, review *v1alpha1.AutonomyReview, levels map[string]int) bool {
	requested := review.Spec.RequestedClass
	if requested == "" || profile.Spec.AgentRef.Name != review.Spec.AgentRef.Name || profile.Status.CurrentClass != review.Spec.CurrentClass {
		return false
	}
	if review.Spec.Evidence != "" && !trustScoreHistoryContains(profile.Status.ScoreHistory, review.Spec.Evidence) {
		return false
	}
	if resolvedClassLevel(requested, levels) <= resolvedClassLevel(profile.Status.CurrentClass, levels) {
		return false
	}
	if profile.Spec.MaxClass != "" && resolvedClassLevel(requested, levels) > resolvedClassLevel(profile.Spec.MaxClass, levels) {
		return false
	}
	return evaluatePromotionWithLevels(profile, levels) == requested && promotionEligible(profile, requested, nowFunc())
}

// CreateReview creates a new AutonomyReview for an agent action that needs approval.
func (r *AutonomyReviewReconciler) CreateReview(ctx context.Context, namespace, name string, agentRef string, action string, sessionID string) error {
	review := v1alpha1.AutonomyReview{}
	review.TypeMeta.Kind = autonomyReviewKind
	review.TypeMeta.APIVersion = "v1alpha1"
	review.ObjectMeta.Name = name
	review.ObjectMeta.Namespace = namespace
	review.ObjectMeta.CreationTimestamp = nowFunc().UTC()
	review.Spec.AgentRef.Name = agentRef
	review.Spec.ActionDescription = action
	review.Spec.SessionID = sessionID
	review.Status.Decision = DecisionPending
	review.Status.ExpiresAt = review.ObjectMeta.CreationTimestamp.UTC().Add(r.defaultTTL).Format(time.RFC3339)

	data, err := json.Marshal(review)
	if err != nil {
		return fmt.Errorf("marshal AutonomyReview: %w", err)
	}

	if err := r.store.Create(ctx, autonomyReviewKind, namespace, name, data); err != nil {
		return fmt.Errorf("create AutonomyReview: %w", err)
	}

	r.logger.Info("created autonomy review",
		zap.String("name", name),
		zap.String("agent", agentRef),
		zap.String("action", action))

	return nil
}

func (r *AutonomyReviewReconciler) updateReview(ctx context.Context, req Request, review *v1alpha1.AutonomyReview) error {
	data, err := json.Marshal(review)
	if err != nil {
		return fmt.Errorf("marshal AutonomyReview: %w", err)
	}
	rv := review.ObjectMeta.ResourceVersion
	if err := r.store.Update(ctx, autonomyReviewKind, req.Namespace, req.Name, data, rv); err != nil {
		return fmt.Errorf("update AutonomyReview: %w", err)
	}
	return nil
}

// Approve approves an AutonomyReview with the given reason and decidedBy fields.
func (r *AutonomyReviewReconciler) Approve(ctx context.Context, namespace, name, decidedBy, reason string) error {
	return r.decide(ctx, namespace, name, DecisionApproved, decidedBy, reason)
}

// Deny denies an AutonomyReview with the given reason and decidedBy fields.
func (r *AutonomyReviewReconciler) Deny(ctx context.Context, namespace, name, decidedBy, reason string) error {
	return r.decide(ctx, namespace, name, DecisionDenied, decidedBy, reason)
}

func (r *AutonomyReviewReconciler) decide(ctx context.Context, namespace, name, decision, decidedBy, reason string) error {
	raw, err := r.store.Get(ctx, autonomyReviewKind, namespace, name)
	if err != nil {
		return fmt.Errorf("get AutonomyReview: %w", err)
	}

	var review v1alpha1.AutonomyReview
	if err := json.Unmarshal(raw, &review); err != nil {
		return fmt.Errorf("unmarshal AutonomyReview: %w", err)
	}

	// Only pending, unexpired reviews can be decided.
	if review.Status.Decision != DecisionPending {
		return fmt.Errorf("%w: review %s/%s is %s", ErrAutonomyReviewNotPending, namespace, name, review.Status.Decision)
	}
	now := nowFunc().UTC()
	expiresAt, _, err := r.expiration(&review, now)
	if err != nil {
		return err
	}
	if !now.Before(expiresAt) {
		r.setTTLDenial(&review, now)
		if err := r.recordDecision(ctx, &review); err != nil {
			return err
		}
		if err := r.updateReview(ctx, Request{Namespace: namespace, Name: name, Kind: autonomyReviewKind}, &review); err != nil {
			return err
		}
		return fmt.Errorf("%w: review %s/%s", ErrAutonomyReviewExpired, namespace, name)
	}

	review.Status.Decision = decision
	review.Status.DecidedBy = decidedBy
	review.Status.Reason = reason
	review.Status.DecidedAt = now.Format(time.RFC3339)
	if err := r.recordDecision(ctx, &review); err != nil {
		return err
	}

	data, err := json.Marshal(review)
	if err != nil {
		return fmt.Errorf("marshal AutonomyReview: %w", err)
	}

	rv := review.ObjectMeta.ResourceVersion
	if err := r.store.Update(ctx, autonomyReviewKind, namespace, name, data, rv); err != nil {
		return fmt.Errorf("update AutonomyReview: %w", err)
	}

	r.logger.Info("autonomy review decided",
		zap.String("name", name),
		zap.String("decision", decision),
		zap.String("decidedBy", decidedBy),
		zap.String("reason", reason))

	return nil
}

func (r *AutonomyReviewReconciler) recordDecision(ctx context.Context, review *v1alpha1.AutonomyReview) error {
	if r.audit == nil {
		return nil
	}
	details, err := json.Marshal(struct {
		Decision  string `json:"decision"`
		DecidedBy string `json:"decidedBy"`
		Reason    string `json:"reason,omitempty"`
		ExpiresAt string `json:"expiresAt"`
	}{Decision: review.Status.Decision, DecidedBy: review.Status.DecidedBy, Reason: review.Status.Reason, ExpiresAt: review.Status.ExpiresAt})
	if err != nil {
		return fmt.Errorf("marshal AutonomyReview audit details: %w", err)
	}
	auditKey := fmt.Sprintf("%s/%s|%s|%s|%s", review.ObjectMeta.Namespace, review.ObjectMeta.Name, review.ObjectMeta.UID, review.Status.Decision, review.Status.DecidedAt)
	timestamp := review.ObjectMeta.CreationTimestamp.UTC()
	if review.Status.DecidedAt != "" {
		var err error
		timestamp, err = time.Parse(time.RFC3339, review.Status.DecidedAt)
		if err != nil {
			return fmt.Errorf("parse AutonomyReview status.decidedAt for audit: %w", err)
		}
	}
	if timestamp.IsZero() {
		timestamp = time.Unix(0, 0).UTC()
	}
	return r.audit.RecordBeforeExec(ctx, observability.AuditEntry{
		ID:            uuid.NewSHA1(uuid.NameSpaceOID, []byte(auditKey)).String(),
		Timestamp:     timestamp,
		Workspace:     review.ObjectMeta.Namespace,
		AgentIdentity: review.Spec.AgentRef.Name,
		EventType:     observability.AuditAutonomyReview,
		SubjectKind:   autonomyReviewKind,
		SubjectRef:    review.ObjectMeta.Namespace + "/" + review.ObjectMeta.Name,
		Verb:          "decision-" + strings.ToLower(review.Status.Decision),
		Details:       details,
	})
}
