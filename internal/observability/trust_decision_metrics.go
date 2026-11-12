// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package observability

import (
	"context"
	"encoding/json"

	"github.com/prometheus/client_golang/prometheus"
)

var trustDecisionLabelNames = []string{
	"namespace",
	"plane",
	"outcome",
	"reason_code",
	"identity_state",
	"role_state",
	"grant_state",
	"policy_state",
	"approval_state",
	"execution_state",
	"target_reached_state",
}

var (
	trustDecisionPlanes   = labelSet("tool", "memory", "model", "communication")
	trustDecisionOutcomes = labelSet(
		"allowed", "denied", "revoked", "replayed", "pending", "approved", "rejected", "completed", "failed",
	)
	trustDecisionReasonCodes = labelSet(
		"authorized", "capability_invalid", "capability_expired", "identity_not_ready", "role_denied", "grant_missing",
		"grant_revoked", "argument_constraints_denied", "policy_denied", "quota_exhausted", "replay_detected",
		"approval_required", "approval_approved", "approval_rejected", "target_completed", "target_failed",
	)
	trustDecisionEvidenceStates  = labelSet("allowed", "denied", "revoked", "not_required", "not_evaluated", "unavailable")
	trustDecisionApprovalStates  = labelSet("not_evaluated", "not_required", "pending", "approved", "rejected", "unavailable")
	trustDecisionExecutionStates = labelSet("not_started", "blocked", "completed", "failed", "unavailable")
)

// TrustDecisionMetrics records normalized governance decisions only after an
// immutable audit entry has been newly and durably applied.
type TrustDecisionMetrics struct {
	decisions *prometheus.CounterVec
}

// NewTrustDecisionMetrics registers the durable TrustDecision counter.
func NewTrustDecisionMetrics(registerer prometheus.Registerer) *TrustDecisionMetrics {
	metrics := &TrustDecisionMetrics{
		decisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "kognition_trust_decisions_total",
			Help: "Newly and durably applied normalized TrustDecision records.",
		}, trustDecisionLabelNames),
	}
	registerer.MustRegister(metrics.decisions)
	return metrics
}

// Wrap records metrics for newly applied audit entries written by store.
func (m *TrustDecisionMetrics) Wrap(store AuditApplyStore) AuditStore {
	return &meteredTrustDecisionStore{store: store, metrics: m}
}

type meteredTrustDecisionStore struct {
	store   AuditApplyStore
	metrics *TrustDecisionMetrics
}

func (s *meteredTrustDecisionStore) Append(ctx context.Context, entry AuditEntry) error {
	applied, err := s.store.ApplyAuditEntry(ctx, entry)
	if err == nil && applied {
		s.metrics.record(entry)
	}
	return err
}

func (s *meteredTrustDecisionStore) AppendBatch(ctx context.Context, entries []AuditEntry) error {
	applied, err := s.store.ApplyAuditBatch(ctx, entries)
	if err != nil {
		return err
	}
	for index, entry := range entries {
		if index < len(applied) && applied[index] {
			s.metrics.record(entry)
		}
	}
	return nil
}

func (s *meteredTrustDecisionStore) Query(ctx context.Context, filter AuditFilter) ([]AuditEntry, error) {
	return s.store.Query(ctx, filter)
}

func (m *TrustDecisionMetrics) record(entry AuditEntry) {
	if !isTrustDecisionEventType(entry.EventType) {
		return
	}
	decision, ok := normalizedTrustDecision(entry.Details)
	if !ok {
		return
	}
	m.decisions.With(trustDecisionLabels(decision)).Inc()
}

func normalizedTrustDecision(details json.RawMessage) (TrustDecisionV1, bool) {
	var decision TrustDecisionV1
	if len(details) == 0 || json.Unmarshal(details, &decision) != nil || decision.SchemaVersion != TrustDecisionSchemaVersion {
		return TrustDecisionV1{}, false
	}
	return decision, true
}

func trustDecisionLabels(decision TrustDecisionV1) prometheus.Labels {
	approvalState := notReportedLabel
	if decision.Approval != nil {
		approvalState = boundedLabel(decision.Approval.Status, trustDecisionApprovalStates)
	}
	targetReachedState := notReportedLabel
	if decision.Execution.TargetReached != nil {
		targetReachedState = "false"
		if *decision.Execution.TargetReached {
			targetReachedState = "true"
		}
	}
	return prometheus.Labels{
		"namespace":            labelOr(decision.Workspace, notReportedLabel),
		"plane":                boundedLabel(decision.Plane, trustDecisionPlanes),
		"outcome":              boundedLabel(decision.Outcome, trustDecisionOutcomes),
		"reason_code":          boundedLabel(decision.ReasonCode, trustDecisionReasonCodes),
		"identity_state":       boundedLabel(decision.Authorization.Identity, trustDecisionEvidenceStates),
		"role_state":           boundedLabel(decision.Authorization.Role, trustDecisionEvidenceStates),
		"grant_state":          boundedLabel(decision.Authorization.Grant, trustDecisionEvidenceStates),
		"policy_state":         boundedLabel(decision.Authorization.Policy, trustDecisionEvidenceStates),
		"approval_state":       approvalState,
		"execution_state":      boundedLabel(decision.Execution.Status, trustDecisionExecutionStates),
		"target_reached_state": targetReachedState,
	}
}

func boundedLabel(value string, allowed map[string]struct{}) string {
	if value == "" {
		return notReportedLabel
	}
	if _, ok := allowed[value]; ok {
		return value
	}
	return "unknown"
}

func labelSet(values ...string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}
