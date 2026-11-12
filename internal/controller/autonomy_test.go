// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/store"
)

type autonomyReviewAuditStore struct {
	entries []observability.AuditEntry
	err     error
}

type persistThenFailAuditStore struct {
	autonomyReviewAuditStore
	failOnce bool
}

func (s *persistThenFailAuditStore) Append(_ context.Context, entry observability.AuditEntry) error {
	s.entries = append(s.entries, entry)
	if s.failOnce {
		s.failOnce = false
		return errors.New("acknowledgement lost after persistence")
	}
	return nil
}

func (s *autonomyReviewAuditStore) Append(_ context.Context, entry observability.AuditEntry) error {
	if s.err != nil {
		return s.err
	}
	s.entries = append(s.entries, entry)
	return nil
}

func (s *autonomyReviewAuditStore) AppendBatch(_ context.Context, entries []observability.AuditEntry) error {
	if s.err != nil {
		return s.err
	}
	s.entries = append(s.entries, entries...)
	return nil
}

func (s *autonomyReviewAuditStore) Query(_ context.Context, filter observability.AuditFilter) ([]observability.AuditEntry, error) {
	if s.err != nil {
		return nil, s.err
	}
	result := make([]observability.AuditEntry, 0, len(s.entries))
	for _, entry := range s.entries {
		if filter.Workspace != "" && entry.Workspace != filter.Workspace {
			continue
		}
		if filter.AgentIdentity != "" && entry.AgentIdentity != filter.AgentIdentity {
			continue
		}
		if !filter.Since.IsZero() && entry.Timestamp.Before(filter.Since) {
			continue
		}
		if !filter.Until.IsZero() && entry.Timestamp.After(filter.Until) {
			continue
		}
		result = append(result, entry)
	}
	return result, nil
}

type wrappedAutonomyClassCreateStore struct {
	*memStore
}

func (s *wrappedAutonomyClassCreateStore) Create(context.Context, string, string, string, []byte) error {
	return fmt.Errorf("wrapped concurrent create: %w", store.ErrAlreadyExists)
}

func TestDefaultAutonomyClasses(t *testing.T) {
	classes := DefaultAutonomyClasses()
	if len(classes) != 3 {
		t.Fatalf("expected 3 default classes, got %d", len(classes))
	}

	expected := map[string]int{
		"supervised": 10,
		"assisted":   50,
		"autonomous": 90,
	}

	for _, c := range classes {
		level, ok := expected[c.Name]
		if !ok {
			t.Errorf("unexpected class: %s", c.Name)
			continue
		}
		if c.Spec.Level != level {
			t.Errorf("class %s: expected level %d, got %d", c.Name, level, c.Spec.Level)
		}
		if c.Spec.ApprovalPolicy == nil {
			t.Errorf("class %s: missing approval policy", c.Name)
		}
		if c.Spec.Constraints == nil {
			t.Errorf("class %s: missing constraints", c.Name)
		}
	}
}

func TestEnsureDefaultAutonomyClasses(t *testing.T) {
	ms := newMemStore()
	logger := zaptest.NewLogger(t)
	ctx := context.Background()

	// First call should create all.
	err := EnsureDefaultAutonomyClasses(ctx, ms, logger)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify all three exist.
	for _, name := range []string{"supervised", "assisted", "autonomous"} {
		raw, err := ms.Get(ctx, "AutonomyClass", "system", name)
		if err != nil {
			t.Fatalf("class %s not found: %v", name, err)
		}
		var ac v1alpha1.AutonomyClass
		json.Unmarshal(raw, &ac)
		if ac.Spec.Level == 0 {
			t.Errorf("class %s has zero level", name)
		}
	}

	// Second call should be idempotent.
	err = EnsureDefaultAutonomyClasses(ctx, ms, logger)
	if err != nil {
		t.Fatalf("idempotent call failed: %v", err)
	}
}

func TestEnsureDefaultAutonomyClasses_WrappedAlreadyExistsIsIgnored(t *testing.T) {
	err := EnsureDefaultAutonomyClasses(context.Background(), &wrappedAutonomyClassCreateStore{memStore: newMemStore()}, zaptest.NewLogger(t))
	if err != nil {
		t.Fatalf("EnsureDefaultAutonomyClasses() error = %v, want nil", err)
	}
}

// This configuration test verifies shipped policy values. Runtime Gate 3
// acceptance cases TC-0171 and TC-0172 remain pending under T-0214.
func TestEnsureDefaultAutonomyClasses_ApprovalPolicies(t *testing.T) {
	ms := newMemStore()
	logger := zaptest.NewLogger(t)
	ctx := context.Background()

	EnsureDefaultAutonomyClasses(ctx, ms, logger)

	// Supervised: always requires approval.
	raw, _ := ms.Get(ctx, "AutonomyClass", "system", "supervised")
	var supervised v1alpha1.AutonomyClass
	json.Unmarshal(raw, &supervised)
	if supervised.Spec.ApprovalPolicy.ToolInvocation != "always" {
		t.Errorf("supervised: expected tool approval=always, got %q", supervised.Spec.ApprovalPolicy.ToolInvocation)
	}

	// Assisted: high-risk-only for tools.
	raw, _ = ms.Get(ctx, "AutonomyClass", "system", "assisted")
	var assisted v1alpha1.AutonomyClass
	json.Unmarshal(raw, &assisted)
	if assisted.Spec.ApprovalPolicy.ToolInvocation != "high-risk-only" {
		t.Errorf("assisted: expected tool approval=high-risk-only, got %q", assisted.Spec.ApprovalPolicy.ToolInvocation)
	}

	// Autonomous: never requires tool approval.
	raw, _ = ms.Get(ctx, "AutonomyClass", "system", "autonomous")
	var autonomous v1alpha1.AutonomyClass
	json.Unmarshal(raw, &autonomous)
	if autonomous.Spec.ApprovalPolicy.ToolInvocation != "never" {
		t.Errorf("autonomous: expected tool approval=never, got %q", autonomous.Spec.ApprovalPolicy.ToolInvocation)
	}
}

func TestComputeWeightedScore(t *testing.T) {
	tests := []struct {
		name     string
		current  float64
		events   []TrustScoringEvent
		weights  *v1alpha1.ScoringWeights
		expected float64
	}{
		{
			name:     "successes increase score",
			current:  50.0,
			events:   []TrustScoringEvent{{Type: EventSuccess}, {Type: EventSuccess}, {Type: EventSuccess}},
			expected: 53.0,
		},
		{
			name:     "failures decrease score",
			current:  50.0,
			events:   []TrustScoringEvent{{Type: EventFailure}},
			expected: 48.0,
		},
		{
			name:     "violations severely decrease score",
			current:  50.0,
			events:   []TrustScoringEvent{{Type: EventPolicyViolation}},
			expected: 40.0,
		},
		{
			name:     "custom weights",
			current:  50.0,
			events:   []TrustScoringEvent{{Type: EventSuccess}},
			weights:  &v1alpha1.ScoringWeights{SuccessWeight: 5.0},
			expected: 55.0,
		},
		{
			name:     "clamped at 0",
			current:  5.0,
			events:   []TrustScoringEvent{{Type: EventPolicyViolation}},
			expected: 0.0,
		},
		{
			name:     "clamped at 100",
			current:  99.0,
			events:   []TrustScoringEvent{{Type: EventSuccess}, {Type: EventSuccess}, {Type: EventSuccess}},
			expected: 100.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ComputeWeightedScore(tt.current, tt.events, tt.weights)
			if result != tt.expected {
				t.Errorf("expected %f, got %f", tt.expected, result)
			}
		})
	}
}

// TC-0177: maxClass caps promotion (subtest "capped by maxClass").
func TestEvaluatePromotion(t *testing.T) {
	tests := []struct {
		name         string
		currentClass string
		score        float64
		maxClass     string
		expected     string
	}{
		{
			name:         "supervised to assisted",
			currentClass: ClassSupervised,
			score:        55.0,
			expected:     ClassAssisted,
		},
		{
			name:         "assisted to autonomous",
			currentClass: ClassAssisted,
			score:        85.0,
			expected:     ClassAutonomous,
		},
		{
			name:         "no promotion when score matches current class",
			currentClass: ClassAssisted,
			score:        55.0,
			expected:     "",
		},
		{
			name:         "capped by maxClass",
			currentClass: ClassSupervised,
			score:        90.0,
			maxClass:     ClassAssisted,
			expected:     ClassAssisted,
		},
		{
			name:         "maxClass blocks promotion entirely",
			currentClass: ClassAssisted,
			score:        90.0,
			maxClass:     ClassAssisted,
			expected:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tp := &v1alpha1.TrustProfile{}
			tp.Status.CurrentClass = tt.currentClass
			tp.Status.CurrentScore = tt.score
			tp.Spec.MaxClass = tt.maxClass

			result := EvaluatePromotion(tp)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestCustomAutonomyClassOrdering(t *testing.T) {
	levels := map[string]int{"supervised": 10, "guided": 30, "assisted": 50}
	tp := &v1alpha1.TrustProfile{Spec: v1alpha1.TrustProfileSpec{
		MaxClass: "guided", PromotionPolicy: &v1alpha1.PromotionPolicy{Thresholds: []v1alpha1.PromotionThreshold{{TargetClass: "guided", MinScore: 40}, {TargetClass: "assisted", MinScore: 60}}},
	}, Status: v1alpha1.TrustProfileStatus{CurrentClass: "supervised", CurrentScore: 80}}
	if got := evaluatePromotionWithLevels(tp, levels); got != "guided" {
		t.Fatalf("custom promotion = %q, want guided", got)
	}
	tp.Spec.MaxClass = "assisted"
	tp.Status.CurrentClass = "assisted"
	tp.Status.CurrentScore = 30
	result := evaluateDemotionWithLevels(tp, nil, levels)
	if !result.Demoted || result.NewClass != "guided" {
		t.Fatalf("custom step-down = %+v, want guided", result)
	}
}

// TC-0176: Critical violation on autonomous agent triggers demotion to supervised.
func TestEvaluateDemotion_CriticalViolation(t *testing.T) {
	tp := &v1alpha1.TrustProfile{}
	tp.Status.CurrentClass = ClassAutonomous
	tp.Status.CurrentScore = 30.0 // below threshold

	events := []TrustScoringEvent{{Type: EventPolicyViolation, Severity: SeverityCritical}}

	result := EvaluateDemotion(tp, events)
	if !result.Demoted {
		t.Fatal("expected demotion on critical violation")
	}
	if result.NewClass != ClassSupervised {
		t.Errorf("expected fallback to supervised, got %q", result.NewClass)
	}
}

func TestEvaluateDemotion_CriticalWithCustomFallback(t *testing.T) {
	tp := &v1alpha1.TrustProfile{}
	tp.Status.CurrentClass = ClassAutonomous
	tp.Status.CurrentScore = 30.0
	tp.Spec.DemotionPolicy = &v1alpha1.DemotionPolicy{
		FallbackClass: ClassAssisted,
	}

	events := []TrustScoringEvent{{Type: EventPolicyViolation, Severity: SeverityCritical}}

	result := EvaluateDemotion(tp, events)
	if !result.Demoted {
		t.Fatal("expected demotion")
	}
	if result.NewClass != ClassAssisted {
		t.Errorf("expected fallback to assisted, got %q", result.NewClass)
	}
}

func TestEvaluateDemotion_ImmediateStepDown(t *testing.T) {
	tp := &v1alpha1.TrustProfile{}
	tp.Status.CurrentClass = ClassAutonomous
	tp.Status.CurrentScore = 55.0 // above assisted threshold but below autonomous

	events := []TrustScoringEvent{{Type: EventFailure}}

	result := EvaluateDemotion(tp, events)
	if !result.Demoted {
		t.Fatal("expected demotion")
	}
	if result.NewClass != ClassAssisted {
		t.Errorf("expected step-down to assisted, got %q", result.NewClass)
	}
}

func TestEvaluateDemotion_ProbationMode(t *testing.T) {
	tp := &v1alpha1.TrustProfile{}
	tp.Status.CurrentClass = ClassAutonomous
	tp.Status.CurrentScore = 55.0
	tp.Spec.DemotionPolicy = &v1alpha1.DemotionPolicy{
		Mode:           "probation",
		FreezeDuration: "48h",
	}

	events := []TrustScoringEvent{{Type: EventPolicyViolation, Severity: SeverityWarning}}

	result := EvaluateDemotion(tp, events)
	if result.Demoted || !result.Probation {
		t.Fatal("expected warning to freeze the current class without demotion")
	}
	if result.NewClass != ClassAutonomous {
		t.Fatalf("probation class = %q, want current class %q", result.NewClass, ClassAutonomous)
	}
	if result.FreezeUntil.IsZero() {
		t.Fatal("expected freeze timestamp set")
	}
}

func TestApplyScoring_ProbationConditionIsCurrentAndExpires(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	previousNow := nowFunc
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = previousNow })

	tp := &v1alpha1.TrustProfile{
		Spec:   v1alpha1.TrustProfileSpec{DemotionPolicy: &v1alpha1.DemotionPolicy{Mode: "probation", FreezeDuration: "1h"}},
		Status: v1alpha1.TrustProfileStatus{CurrentClass: ClassAssisted, CurrentScore: 70},
	}
	reconciler := NewTrustProfileReconciler(newMemStore(), zaptest.NewLogger(t))
	events := []TrustScoringEvent{{Type: EventPolicyViolation, Severity: SeverityWarning}}
	reconciler.applyScoring(tp, "warning-1", events, map[string]int{ClassSupervised: 10, ClassAssisted: 50, ClassAutonomous: 90})
	reconciler.applyScoring(tp, "warning-2", events, map[string]int{ClassSupervised: 10, ClassAssisted: 50, ClassAutonomous: 90})

	probationConditions := 0
	for _, condition := range tp.Status.Conditions {
		if condition.Type == "Probation" {
			probationConditions++
			if condition.Status != "True" {
				t.Fatalf("active probation condition = %+v", condition)
			}
		}
	}
	if probationConditions != 1 {
		t.Fatalf("probation conditions = %d, want 1; conditions=%+v", probationConditions, tp.Status.Conditions)
	}

	now = now.Add(2 * time.Hour)
	reconciler.applyScoring(tp, "success-1", []TrustScoringEvent{{Type: EventSuccess}}, map[string]int{ClassSupervised: 10, ClassAssisted: 50, ClassAutonomous: 90})
	if tp.Status.ProbationUntil != "" {
		t.Fatalf("expired probation timestamp was not cleared: %q", tp.Status.ProbationUntil)
	}
	for _, condition := range tp.Status.Conditions {
		if condition.Type == "Probation" && (condition.Status != "False" || condition.Reason != "Expired") {
			t.Fatalf("expired probation condition = %+v", condition)
		}
	}
}

func TestApplyAuditEvaluation_ProbationSetsVisibleCondition(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	decision := observability.TrustDecisionV1{
		SchemaVersion: observability.TrustDecisionSchemaVersion,
		ID:            "failed-tool",
		Timestamp:     now.Add(-time.Minute),
		Plane:         "tool",
		Outcome:       "failed",
		Execution:     observability.TrustDecisionExecution{Status: "failed"},
	}
	details, _ := json.Marshal(decision)
	tp := &v1alpha1.TrustProfile{
		Spec:   v1alpha1.TrustProfileSpec{DemotionPolicy: &v1alpha1.DemotionPolicy{Mode: "probation", FreezeDuration: "1h"}},
		Status: v1alpha1.TrustProfileStatus{CurrentClass: ClassAssisted, CurrentScore: 70},
	}
	reconciler := NewTrustProfileReconciler(newMemStore(), zaptest.NewLogger(t))
	reconciler.applyAuditEvaluation(tp, []observability.AuditEntry{{ID: decision.ID, Details: details}}, now, "audit:evaluation", map[string]int{ClassSupervised: 10, ClassAssisted: 50, ClassAutonomous: 90})
	for _, condition := range tp.Status.Conditions {
		if condition.Type == "Probation" && condition.Status == "True" {
			return
		}
	}
	t.Fatalf("periodic probation condition missing: %+v", tp.Status.Conditions)
}

func TestEvaluateDemotion_NoChange(t *testing.T) {
	tp := &v1alpha1.TrustProfile{}
	tp.Status.CurrentClass = ClassAssisted
	tp.Status.CurrentScore = 60.0 // above assisted threshold

	events := []TrustScoringEvent{{Type: EventSuccess}}

	result := EvaluateDemotion(tp, events)
	if result.Demoted {
		t.Fatal("expected no demotion when score is above threshold")
	}
}

// TC-0174, TC-0175: TrustProfile starts supervised, auto-promotes to assisted after successful outcomes.
func TestTrustProfileReconciler_ApplyScoring(t *testing.T) {
	ms := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewTrustProfileReconciler(ms, logger)

	ctx := context.Background()
	ns := "default"

	// Create profile at supervised with score 45.
	tp := v1alpha1.TrustProfile{}
	tp.TypeMeta.Kind = "TrustProfile"
	tp.ObjectMeta.Name = "agent-trust"
	tp.ObjectMeta.Namespace = ns
	tp.Spec.AgentRef.Name = "test-agent"
	tp.Spec.InitialClass = ClassSupervised
	tp.Status.CurrentScore = 45
	tp.Status.CurrentClass = ClassSupervised
	data, _ := json.Marshal(tp)
	ms.Create(ctx, "TrustProfile", ns, "agent-trust", data)

	// Apply 10 successes (score += 10 = 55 → promote to assisted).
	events := make([]TrustScoringEvent, 10)
	for i := range events {
		events[i] = TrustScoringEvent{Type: EventSuccess}
	}

	err := r.ApplyScoring(ctx, ns, "agent-trust", events)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	result, err := r.GetTrustProfile(ctx, ns, "agent-trust")
	if err != nil {
		t.Fatalf("get profile: %v", err)
	}
	if result.Status.CurrentScore != 55.0 {
		t.Errorf("expected score 55, got %f", result.Status.CurrentScore)
	}
	if result.Status.CurrentClass != ClassAssisted {
		t.Errorf("expected class assisted, got %q", result.Status.CurrentClass)
	}
	if result.Status.TotalEvaluations != 1 {
		t.Errorf("expected 1 evaluation, got %d", result.Status.TotalEvaluations)
	}
	if len(result.Status.ScoreHistory) != 1 {
		t.Errorf("expected 1 history entry, got %d", len(result.Status.ScoreHistory))
	}
}

// TC-0176: Critical violation causes demotion and sets condition message.
func TestTrustProfileReconciler_ApplyScoring_Demotion(t *testing.T) {
	ms := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewTrustProfileReconciler(ms, logger)

	ctx := context.Background()
	ns := "default"

	// Create profile at autonomous with score 85.
	tp := v1alpha1.TrustProfile{}
	tp.TypeMeta.Kind = "TrustProfile"
	tp.ObjectMeta.Name = "agent-trust"
	tp.ObjectMeta.Namespace = ns
	tp.Spec.AgentRef.Name = "test-agent"
	tp.Status.CurrentScore = 85
	tp.Status.CurrentClass = ClassAutonomous
	data, _ := json.Marshal(tp)
	ms.Create(ctx, "TrustProfile", ns, "agent-trust", data)

	// Apply critical violation (score drops by 10 to 75, critical → hard reset).
	events := []TrustScoringEvent{{Type: EventPolicyViolation, Severity: SeverityCritical}}

	err := r.ApplyScoring(ctx, ns, "agent-trust", events)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	result, err := r.GetTrustProfile(ctx, ns, "agent-trust")
	if err != nil {
		t.Fatalf("get profile: %v", err)
	}
	if result.Status.CurrentClass != ClassSupervised {
		t.Errorf("expected demotion to supervised, got %q", result.Status.CurrentClass)
	}
	// Verify condition message correctness (regression for the from/to bug).
	found := false
	for _, c := range result.Status.Conditions {
		if c.Type == "Demoted" {
			found = true
			expected := "demoted from autonomous to supervised"
			if c.Message != expected {
				t.Errorf("condition message: expected %q, got %q", expected, c.Message)
			}
		}
	}
	if !found {
		t.Error("expected Demoted condition")
	}
}

func TestTrustProfileReconciler_PeriodicAuditEvaluation(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	previousNow := nowFunc
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = previousNow })
	auditStore := &autonomyReviewAuditStore{}
	decision := observability.TrustDecisionV1{
		SchemaVersion: 1, ID: "decision-1", Timestamp: now.Add(-time.Minute), Plane: "tool", Outcome: "allowed", Workspace: "default",
		Execution: observability.TrustDecisionExecution{Status: "completed"}, Approval: &observability.TrustDecisionApproval{Status: "approved"},
	}
	details, _ := json.Marshal(decision)
	auditStore.entries = []observability.AuditEntry{{ID: decision.ID, Timestamp: decision.Timestamp, Workspace: "default", AgentIdentity: "agent-a", EventType: observability.AuditToolCall, Details: details}}
	profile := v1alpha1.TrustProfile{ObjectMeta: v1alpha1.ObjectMeta{Name: "agent-trust", Namespace: "default"}, Spec: v1alpha1.TrustProfileSpec{
		AgentRef: v1alpha1.LocalObjectReference{Name: "agent-a"}, InitialScore: 20, InitialClass: ClassSupervised, EvaluationInterval: "1h",
		ScoringWeights:  &v1alpha1.ScoringWeights{TaskCompletionRate: .25, PolicyCompliance: .30, ApprovalAlignmentRate: .20, CostEfficiency: .10, ToolUsageAccuracy: .15},
		PromotionPolicy: &v1alpha1.PromotionPolicy{MinActions: 2},
	}}
	data, _ := json.Marshal(profile)
	if err := ms.Create(ctx, trustProfileKind, "default", profile.Name, data); err != nil {
		t.Fatal(err)
	}
	r := NewTrustProfileReconciler(ms, zaptest.NewLogger(t))
	r.SetAuditLog(observability.NewCognitiveAuditLog(auditStore))
	result, err := r.Reconcile(ctx, Request{Namespace: "default", Name: profile.Name})
	if err != nil {
		t.Fatalf("periodic reconcile: %v", err)
	}
	updated, _ := r.GetTrustProfile(ctx, "default", profile.Name)
	if updated.Status.CurrentScore != 100 || updated.Status.AuditedActions != 1 || updated.Status.CurrentClass != ClassSupervised {
		t.Fatalf("unexpected evaluated status: %+v", updated.Status)
	}
	if updated.Status.LastEvaluationTime == "" || result.RequeueAfter <= 0 {
		t.Fatalf("evaluation did not persist checkpoint/requeue: %+v", result)
	}

	// A reconcile before the interval is due must not consume the same audit entry.
	now = now.Add(30 * time.Minute)
	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: profile.Name}); err != nil {
		t.Fatal(err)
	}
	updated, _ = r.GetTrustProfile(ctx, "default", profile.Name)
	if updated.Status.AuditedActions != 1 || updated.Status.TotalEvaluations != 1 {
		t.Fatalf("audit entry was scored twice: %+v", updated.Status)
	}

	// A second distinct action satisfies minActions, but probation blocks promotion.
	now = now.Add(31 * time.Minute)
	decision.ID, decision.Timestamp, decision.Outcome, decision.Execution.Status = "decision-2", now.Add(-time.Minute), "failed", "failed"
	details, _ = json.Marshal(decision)
	auditStore.entries = append(auditStore.entries, observability.AuditEntry{ID: decision.ID, Timestamp: decision.Timestamp, Workspace: "default", AgentIdentity: "agent-a", EventType: observability.AuditToolCall, Details: details})
	if _, err := r.Reconcile(ctx, Request{Namespace: "default", Name: profile.Name}); err != nil {
		t.Fatal(err)
	}
	updated, _ = r.GetTrustProfile(ctx, "default", profile.Name)
	if updated.Status.AuditedActions != 2 {
		t.Fatalf("auditedActions = %d, want 2", updated.Status.AuditedActions)
	}
}

func TestTrustProfileReconciler_RejectsMissingRuntimeAutonomyClass(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	profile := v1alpha1.TrustProfile{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "agent-trust", Namespace: "default"},
		Spec:       v1alpha1.TrustProfileSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "agent-a"}, InitialClass: "guided"},
		Status:     v1alpha1.TrustProfileStatus{CurrentClass: "guided", CurrentScore: 50},
	}
	data, _ := json.Marshal(profile)
	if err := ms.Create(ctx, trustProfileKind, "default", profile.Name, data); err != nil {
		t.Fatal(err)
	}
	_, err := NewTrustProfileReconciler(ms, zaptest.NewLogger(t)).Reconcile(ctx, Request{Namespace: "default", Name: profile.Name})
	if err == nil || !strings.Contains(err.Error(), `missing AutonomyClass "guided"`) {
		t.Fatalf("Reconcile error = %v, want missing custom class failure", err)
	}
}

func TestTrustProfileReconciler_RequeuesAtProbationExpiry(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	previousNow := nowFunc
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = previousNow })
	profile := v1alpha1.TrustProfile{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "agent-trust", Namespace: "default"},
		Spec:       v1alpha1.TrustProfileSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "agent-a"}, InitialClass: ClassAssisted},
		Status: v1alpha1.TrustProfileStatus{
			CurrentClass:   ClassAssisted,
			CurrentScore:   70,
			ProbationUntil: now.Add(90 * time.Minute).Format(time.RFC3339Nano),
			Conditions:     []v1alpha1.StatusCondition{{Type: "Probation", Status: "True"}},
		},
	}
	data, _ := json.Marshal(profile)
	if err := ms.Create(ctx, trustProfileKind, "default", profile.Name, data); err != nil {
		t.Fatal(err)
	}
	result, err := NewTrustProfileReconciler(ms, zaptest.NewLogger(t)).Reconcile(ctx, Request{Namespace: "default", Name: profile.Name})
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != 90*time.Minute {
		t.Fatalf("RequeueAfter = %s, want 1h30m", result.RequeueAfter)
	}
}

func TestTrustProfilePromotionGatesAndProbation(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	tp := &v1alpha1.TrustProfile{Spec: v1alpha1.TrustProfileSpec{
		PromotionPolicy: &v1alpha1.PromotionPolicy{MinActions: 2, MinScoreDuration: "24h"},
		DemotionPolicy:  &v1alpha1.DemotionPolicy{Mode: "probation", FreezeDuration: "48h"},
	}, Status: v1alpha1.TrustProfileStatus{CurrentClass: ClassAssisted, CurrentScore: 90, AuditedActions: 2, ThresholdMetSince: now.Add(-25 * time.Hour).Format(time.RFC3339Nano), ThresholdClass: ClassAutonomous}}
	if !promotionEligible(tp, ClassAutonomous, now) {
		t.Fatal("expected stabilization and minActions gates to be satisfied")
	}
	tp.Status.AuditedActions = 1
	if promotionEligible(tp, ClassAutonomous, now) {
		t.Fatal("promotion ignored minActions")
	}
	tp.Status.AuditedActions = 2
	tp.Status.ProbationUntil = now.Add(time.Hour).Format(time.RFC3339Nano)
	if promotionEligible(tp, ClassAutonomous, now) {
		t.Fatal("promotion ignored active probation")
	}
	if !promotionEligible(tp, ClassAutonomous, now.Add(2*time.Hour)) {
		t.Fatal("expired probation still blocked promotion")
	}
}

func TestTrustChangeAuditOutboxRetriesAfterAuditFailure(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	auditErr := errors.New("audit unavailable")
	auditStore := &autonomyReviewAuditStore{err: auditErr}
	profile := v1alpha1.TrustProfile{ObjectMeta: v1alpha1.ObjectMeta{Name: "agent-trust", Namespace: "default"}, Spec: v1alpha1.TrustProfileSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "agent-a"}}, Status: v1alpha1.TrustProfileStatus{CurrentClass: ClassAutonomous, CurrentScore: 85}}
	data, _ := json.Marshal(profile)
	if err := ms.Create(ctx, trustProfileKind, "default", profile.Name, data); err != nil {
		t.Fatal(err)
	}
	r := NewTrustProfileReconciler(ms, zaptest.NewLogger(t))
	r.SetAuditLog(observability.NewCognitiveAuditLog(auditStore))
	err := r.ApplyScoring(ctx, "default", profile.Name, []TrustScoringEvent{{Type: EventPolicyViolation, Severity: SeverityCritical}})
	if !errors.Is(err, auditErr) {
		t.Fatalf("ApplyScoring error = %v, want audit failure", err)
	}
	updated, _ := r.GetTrustProfile(ctx, "default", profile.Name)
	if updated.Status.CurrentClass != ClassSupervised || updated.Status.PendingTrustChange == nil {
		t.Fatalf("demotion/outbox was not committed: %+v", updated.Status)
	}

	auditStore.err = nil
	result, err := r.Reconcile(ctx, Request{Namespace: "default", Name: profile.Name})
	if err != nil || !result.Requeue {
		t.Fatalf("outbox retry result=%+v err=%v", result, err)
	}
	updated, _ = r.GetTrustProfile(ctx, "default", profile.Name)
	if updated.Status.PendingTrustChange != nil || len(auditStore.entries) != 1 || auditStore.entries[0].Verb != "demotion" {
		t.Fatalf("outbox not flushed exactly once: status=%+v audits=%+v", updated.Status, auditStore.entries)
	}
}

func TestTrustChangeAuditRetryUsesStablePayloadAfterLostAcknowledgement(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	auditStore := &persistThenFailAuditStore{failOnce: true}
	profile := v1alpha1.TrustProfile{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "agent-trust", Namespace: "default"},
		Spec:       v1alpha1.TrustProfileSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "agent-a"}},
		Status:     v1alpha1.TrustProfileStatus{CurrentClass: ClassAutonomous, CurrentScore: 85},
	}
	data, _ := json.Marshal(profile)
	if err := ms.Create(ctx, trustProfileKind, "default", profile.Name, data); err != nil {
		t.Fatal(err)
	}
	reconciler := NewTrustProfileReconciler(ms, zaptest.NewLogger(t))
	reconciler.SetAuditLog(observability.NewCognitiveAuditLog(auditStore))
	if err := reconciler.ApplyScoring(ctx, "default", profile.Name, []TrustScoringEvent{{Type: EventPolicyViolation, Severity: SeverityCritical}}); err == nil {
		t.Fatal("expected lost audit acknowledgement")
	}
	if len(auditStore.entries) != 1 || auditStore.entries[0].Timestamp.IsZero() {
		t.Fatalf("first persisted audit = %+v", auditStore.entries)
	}
	if _, err := reconciler.Reconcile(ctx, Request{Namespace: "default", Name: profile.Name}); err != nil {
		t.Fatalf("retry reconcile: %v", err)
	}
	if len(auditStore.entries) != 2 {
		t.Fatalf("audit attempts = %d, want 2", len(auditStore.entries))
	}
	first, retry := auditStore.entries[0], auditStore.entries[1]
	if first.ID != retry.ID || !first.Timestamp.Equal(retry.Timestamp) || string(first.Details) != string(retry.Details) {
		t.Fatalf("audit retry changed immutable payload:\nfirst=%+v\nretry=%+v", first, retry)
	}
	updated, _ := reconciler.GetTrustProfile(ctx, "default", profile.Name)
	if updated.Status.PendingTrustChange != nil {
		t.Fatalf("outbox remained after successful retry: %+v", updated.Status.PendingTrustChange)
	}
}

func TestTrustProfilePromotionRequiresAndAppliesAutonomyReview(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	profile := v1alpha1.TrustProfile{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "researcher-trust", Namespace: "default"},
		Spec: v1alpha1.TrustProfileSpec{
			AgentRef:        v1alpha1.LocalObjectReference{Name: "researcher"},
			InitialScore:    79,
			InitialClass:    ClassAssisted,
			MaxClass:        ClassAutonomous,
			PromotionPolicy: &v1alpha1.PromotionPolicy{RequireReview: true},
		},
	}
	data, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := ms.Create(ctx, trustProfileKind, "default", profile.ObjectMeta.Name, data); err != nil {
		t.Fatal(err)
	}
	trust := NewTrustProfileReconciler(ms, zaptest.NewLogger(t))
	instance := makeInstance("default", "promotion-instance", "researcher", v1alpha1.PhaseReady)
	instance.ObjectMeta.UID = "promotion-instance-uid"

	if err := trust.RecordAgentInstanceOutcome(ctx, &instance); err != nil {
		t.Fatalf("record successful outcome: %v", err)
	}
	if err := trust.RecordAgentInstanceOutcome(ctx, &instance); err != nil {
		t.Fatalf("replay successful outcome: %v", err)
	}
	updated, err := trust.GetTrustProfile(ctx, "default", profile.ObjectMeta.Name)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status.CurrentScore != 80 || updated.Status.CurrentClass != ClassAssisted || updated.Status.TotalEvaluations != 1 {
		t.Fatalf("profile before review = %+v, want score 80 and class assisted", updated.Status)
	}
	reviews, err := ms.List(ctx, autonomyReviewKind, store.ListOptions{Namespace: "default"})
	if err != nil || len(reviews) != 1 {
		t.Fatalf("promotion reviews = %d, err = %v, want 1", len(reviews), err)
	}
	var review v1alpha1.AutonomyReview
	if err := json.Unmarshal(reviews[0], &review); err != nil {
		t.Fatal(err)
	}
	if review.Status.Decision != DecisionPending || review.Spec.CurrentClass != ClassAssisted || review.Spec.RequestedClass != ClassAutonomous || review.Spec.TrustProfileRef != profile.ObjectMeta.Name {
		t.Fatalf("promotion review = %+v", review)
	}
	auditStore := &autonomyReviewAuditStore{}
	reviewsController := NewAutonomyReviewReconciler(ms, zaptest.NewLogger(t)).SetAuditLog(observability.NewCognitiveAuditLog(auditStore))
	if err := reviewsController.Approve(ctx, "default", review.ObjectMeta.Name, "platform-admin", "evidence accepted"); err != nil {
		t.Fatalf("approve promotion: %v", err)
	}
	request := Request{Namespace: "default", Name: review.ObjectMeta.Name, Kind: autonomyReviewKind}
	if _, err := reviewsController.Reconcile(ctx, request); err != nil {
		t.Fatalf("apply approved promotion: %v", err)
	}
	if _, err := reviewsController.Reconcile(ctx, request); err != nil {
		t.Fatalf("replay approved promotion: %v", err)
	}
	updated, err = trust.GetTrustProfile(ctx, "default", profile.ObjectMeta.Name)
	if err != nil {
		t.Fatal(err)
	}
	stats := updated.Status.ApprovalStats
	if updated.Status.CurrentClass != ClassAutonomous || stats == nil || stats.TotalRequested != 1 || stats.TotalApproved != 1 || stats.TotalDenied != 0 {
		t.Fatalf("profile after review = %+v, want autonomous with one approval", updated.Status)
	}
	if updated.Status.PendingTrustChange != nil {
		t.Fatalf("review promotion audit outbox was not flushed: %+v", updated.Status.PendingTrustChange)
	}
	foundPromotionAudit := false
	for _, entry := range auditStore.entries {
		if entry.EventType == observability.AuditTrustChange && entry.Verb == "promotion" {
			foundPromotionAudit = true
			break
		}
	}
	if !foundPromotionAudit {
		t.Fatalf("review promotion trust-change audit missing: %+v", auditStore.entries)
	}
}

func TestApprovedPromotionIsNotAppliedAfterEligibilityChanges(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	profile := v1alpha1.TrustProfile{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "researcher-trust", Namespace: "default"},
		Spec: v1alpha1.TrustProfileSpec{
			AgentRef: v1alpha1.LocalObjectReference{Name: "researcher"}, InitialScore: 80,
			InitialClass: ClassAssisted, MaxClass: ClassAutonomous,
		},
		Status: v1alpha1.TrustProfileStatus{CurrentScore: 40, CurrentClass: ClassAssisted},
	}
	data, _ := json.Marshal(profile)
	if err := ms.Create(ctx, trustProfileKind, "default", profile.ObjectMeta.Name, data); err != nil {
		t.Fatal(err)
	}
	review := v1alpha1.AutonomyReview{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "stale-promotion", Namespace: "default"},
		Spec: v1alpha1.AutonomyReviewSpec{
			AgentRef: profile.Spec.AgentRef, CurrentClass: ClassAssisted, RequestedClass: ClassAutonomous, TrustProfileRef: profile.ObjectMeta.Name,
		},
		Status: v1alpha1.AutonomyReviewStatus{Decision: DecisionApproved, DecidedBy: "reviewer"},
	}
	data, _ = json.Marshal(review)
	if err := ms.Create(ctx, autonomyReviewKind, "default", review.ObjectMeta.Name, data); err != nil {
		t.Fatal(err)
	}
	reconciler := NewAutonomyReviewReconciler(ms, zaptest.NewLogger(t))
	request := Request{Namespace: "default", Name: review.ObjectMeta.Name, Kind: autonomyReviewKind}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("reconcile stale approval: %v", err)
	}
	updated, err := NewTrustProfileReconciler(ms, zaptest.NewLogger(t)).GetTrustProfile(ctx, "default", profile.ObjectMeta.Name)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status.CurrentClass != ClassAssisted || updated.Status.ApprovalStats == nil || updated.Status.ApprovalStats.TotalApproved != 1 {
		t.Fatalf("profile = %+v, want no promotion with recorded approval", updated.Status)
	}
	condition := updated.Status.Conditions[len(updated.Status.Conditions)-1]
	if condition.Type != "PromotionRejected" || condition.Reason != "eligibility changed" {
		t.Fatalf("condition = %+v, want PromotionRejected", condition)
	}
}

// TC-0179: Approve a pending review updates TrustProfile.
func TestAutonomyReviewReconciler_Approve(t *testing.T) {
	ms := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewAutonomyReviewReconciler(ms, logger)

	ctx := context.Background()
	ns := "default"

	// Create a review.
	err := r.CreateReview(ctx, ns, "review-1", "agent-1", "promote to assisted", "sess-001")
	if err != nil {
		t.Fatalf("create review: %v", err)
	}

	// Approve it.
	err = r.Approve(ctx, ns, "review-1", "admin@example.com", "agent has demonstrated reliability")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}

	// Verify.
	raw, _ := ms.Get(ctx, "AutonomyReview", ns, "review-1")
	var review v1alpha1.AutonomyReview
	json.Unmarshal(raw, &review)
	if review.Status.Decision != DecisionApproved {
		t.Errorf("expected Approved, got %q", review.Status.Decision)
	}
	if review.Status.DecidedBy != "admin@example.com" {
		t.Errorf("expected decidedBy admin@example.com, got %q", review.Status.DecidedBy)
	}
	if review.Status.Reason != "agent has demonstrated reliability" {
		t.Errorf("expected reason, got %q", review.Status.Reason)
	}
	if review.Status.DecidedAt == "" {
		t.Error("expected decidedAt timestamp")
	}
}

// TC-0179: authenticated decisions are recorded durably before status mutation.
func TestAutonomyReviewReconciler_Approve_AuditsAndFailsClosed(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	auditStore := &autonomyReviewAuditStore{}
	r := NewAutonomyReviewReconciler(ms, zaptest.NewLogger(t)).SetAuditLog(observability.NewCognitiveAuditLog(auditStore))
	if err := r.CreateReview(ctx, "default", "audited-review", "agent-1", "promote", "session-1"); err != nil {
		t.Fatal(err)
	}
	if err := r.Approve(ctx, "default", "audited-review", "alice", "verified"); err != nil {
		t.Fatal(err)
	}
	if len(auditStore.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(auditStore.entries))
	}
	entry := auditStore.entries[0]
	if entry.EventType != observability.AuditAutonomyReview || entry.Verb != "decision-approved" || entry.AgentIdentity != "agent-1" {
		t.Fatalf("unexpected audit entry: %#v", entry)
	}

	if err := r.CreateReview(ctx, "default", "blocked-review", "agent-2", "promote", "session-2"); err != nil {
		t.Fatal(err)
	}
	auditStore.err = errors.New("audit unavailable")
	if err := r.Deny(ctx, "default", "blocked-review", "alice", "unsafe"); err == nil {
		t.Fatal("expected audit failure")
	}
	raw, err := ms.Get(ctx, autonomyReviewKind, "default", "blocked-review")
	if err != nil {
		t.Fatal(err)
	}
	var blocked v1alpha1.AutonomyReview
	if err := json.Unmarshal(raw, &blocked); err != nil {
		t.Fatal(err)
	}
	if blocked.Status.Decision != DecisionPending {
		t.Fatalf("decision = %q, want Pending after audit failure", blocked.Status.Decision)
	}
}

func TestAutonomyReviewReconciler_Deny(t *testing.T) {
	ms := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewAutonomyReviewReconciler(ms, logger)

	ctx := context.Background()
	ns := "default"

	err := r.CreateReview(ctx, ns, "review-2", "agent-1", "promote to autonomous", "sess-002")
	if err != nil {
		t.Fatalf("create review: %v", err)
	}

	err = r.Deny(ctx, ns, "review-2", "security@example.com", "insufficient track record")
	if err != nil {
		t.Fatalf("deny: %v", err)
	}

	raw, _ := ms.Get(ctx, "AutonomyReview", ns, "review-2")
	var review v1alpha1.AutonomyReview
	json.Unmarshal(raw, &review)
	if review.Status.Decision != DecisionDenied {
		t.Errorf("expected Denied, got %q", review.Status.Decision)
	}
	if review.Status.DecidedBy != "security@example.com" {
		t.Errorf("expected decidedBy security@example.com, got %q", review.Status.DecidedBy)
	}
}

func TestAutonomyReviewReconciler_CannotDecideNonPending(t *testing.T) {
	ms := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewAutonomyReviewReconciler(ms, logger)

	ctx := context.Background()
	ns := "default"

	err := r.CreateReview(ctx, ns, "review-3", "agent-1", "action", "sess-003")
	if err != nil {
		t.Fatalf("create review: %v", err)
	}

	// Approve it first.
	r.Approve(ctx, ns, "review-3", "admin", "ok")

	// Try to deny it - should fail.
	err = r.Deny(ctx, ns, "review-3", "other", "too late")
	if err == nil {
		t.Fatal("expected error when deciding a non-pending review")
	}
}

// ---------------------------------------------------------------------------
// AutonomyReviewReconciler.Reconcile tests
// ---------------------------------------------------------------------------

func TestAutonomyReviewReconciler_Reconcile_NotFound(t *testing.T) {
	ms := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewAutonomyReviewReconciler(ms, logger)

	ctx := context.Background()

	// Reconcile a non-existent review → no error, no requeue.
	result, err := r.Reconcile(ctx, Request{Namespace: "default", Name: "nonexistent", Kind: "AutonomyReview"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Fatal("expected no requeue for not-found review")
	}
}

func TestAutonomyReviewReconciler_Reconcile_AlreadyDecided(t *testing.T) {
	ms := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewAutonomyReviewReconciler(ms, logger)

	ctx := context.Background()
	ns := "default"

	// Create and approve a review.
	if err := r.CreateReview(ctx, ns, "decided-review", "agent-1", "action", "sess-1"); err != nil {
		t.Fatal(err)
	}
	if err := r.Approve(ctx, ns, "decided-review", "admin", "lgtm"); err != nil {
		t.Fatal(err)
	}

	// Reconcile an already-decided review → no-op.
	result, err := r.Reconcile(ctx, Request{Namespace: ns, Name: "decided-review", Kind: "AutonomyReview"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Fatal("expected no requeue for already-decided review")
	}
}

// TC-0178: Review is initialized to Pending when requireReview is set.
func TestAutonomyReviewReconciler_Reconcile_InitializesPending(t *testing.T) {
	ms := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewAutonomyReviewReconciler(ms, logger).WithTTL(10 * time.Minute)

	ctx := context.Background()
	ns := "default"

	// Store a review with empty Decision (simulating a raw create without CreateReview helper).
	review := v1alpha1.AutonomyReview{
		TypeMeta:   v1alpha1.TypeMeta{Kind: "AutonomyReview", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: ns, Name: "raw-review", CreationTimestamp: time.Now()},
		Spec: v1alpha1.AutonomyReviewSpec{
			AgentRef:          v1alpha1.LocalObjectReference{Name: "agent-1"},
			ActionDescription: "delete-file",
		},
	}
	data, _ := json.Marshal(review)
	ms.Create(ctx, "AutonomyReview", ns, "raw-review", data)

	// Reconcile should initialize to Pending and requeue after TTL.
	result, err := r.Reconcile(ctx, Request{Namespace: ns, Name: "raw-review", Kind: "AutonomyReview"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter <= 9*time.Minute || result.RequeueAfter > 10*time.Minute {
		t.Fatalf("expected RequeueAfter near 10m, got %v", result.RequeueAfter)
	}

	// Verify status is now Pending.
	raw, _ := ms.Get(ctx, "AutonomyReview", ns, "raw-review")
	var updated v1alpha1.AutonomyReview
	json.Unmarshal(raw, &updated)
	if updated.Status.Decision != DecisionPending {
		t.Errorf("expected Pending, got %q", updated.Status.Decision)
	}
	if updated.Status.ExpiresAt == "" {
		t.Error("expected persisted status.expiresAt")
	}
}

// TC-0180: Review expires past TTL with Denied status.
func TestAutonomyReviewReconciler_Reconcile_TTLExpired_Denies(t *testing.T) {
	ms := newMemStore()
	logger := zaptest.NewLogger(t)
	auditStore := &autonomyReviewAuditStore{}
	r := NewAutonomyReviewReconciler(ms, logger).WithTTL(1 * time.Minute).SetAuditLog(observability.NewCognitiveAuditLog(auditStore))

	ctx := context.Background()
	ns := "default"
	profile := v1alpha1.TrustProfile{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "agent-2-trust", Namespace: ns},
		Spec:       v1alpha1.TrustProfileSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "agent-2"}},
		Status:     v1alpha1.TrustProfileStatus{CurrentScore: 80, CurrentClass: ClassAssisted},
	}
	profileData, _ := json.Marshal(profile)
	if err := ms.Create(ctx, trustProfileKind, ns, profile.ObjectMeta.Name, profileData); err != nil {
		t.Fatal(err)
	}

	// Create a review with a CreationTimestamp beyond the TTL.
	review := v1alpha1.AutonomyReview{
		TypeMeta:   v1alpha1.TypeMeta{Kind: "AutonomyReview", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: ns, Name: "ttl-expire", CreationTimestamp: time.Now().Add(-10 * time.Minute)},
		Spec: v1alpha1.AutonomyReviewSpec{
			AgentRef:          v1alpha1.LocalObjectReference{Name: "agent-2"},
			ActionDescription: "destructive-action",
			CurrentClass:      ClassAssisted,
			RequestedClass:    ClassAutonomous,
			TrustProfileRef:   profile.ObjectMeta.Name,
		},
		Status: v1alpha1.AutonomyReviewStatus{Decision: DecisionPending},
	}
	data, _ := json.Marshal(review)
	ms.Create(ctx, "AutonomyReview", ns, "ttl-expire", data)

	// Reconcile should expire the review.
	result, err := r.Reconcile(ctx, Request{Namespace: ns, Name: "ttl-expire", Kind: "AutonomyReview"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Requeue || result.RequeueAfter != 0 {
		t.Fatal("expected no requeue after expiration")
	}

	// Verify it expired.
	raw, _ := ms.Get(ctx, "AutonomyReview", ns, "ttl-expire")
	var updated v1alpha1.AutonomyReview
	json.Unmarshal(raw, &updated)
	if updated.Status.Decision != DecisionDenied {
		t.Errorf("expected Denied, got %q", updated.Status.Decision)
	}
	if updated.Status.DecidedBy != "system/ttl" || updated.Status.Reason != "TTL expired" {
		t.Errorf("unexpected TTL decision metadata: %#v", updated.Status)
	}
	if updated.Status.ExpiresAt == "" || updated.Status.DecidedAt == "" {
		t.Errorf("expected persisted deadline and decision time: %#v", updated.Status)
	}
	expiresAt, expiresErr := time.Parse(time.RFC3339, updated.Status.ExpiresAt)
	decidedAt, decidedErr := time.Parse(time.RFC3339, updated.Status.DecidedAt)
	if expiresErr != nil || decidedErr != nil || decidedAt.Before(expiresAt) {
		t.Errorf("invalid TTL timestamps: expiresAt=%q (%v), decidedAt=%q (%v)", updated.Status.ExpiresAt, expiresErr, updated.Status.DecidedAt, decidedErr)
	}
	if len(auditStore.entries) != 1 || auditStore.entries[0].Verb != "decision-denied" {
		t.Fatalf("TTL audit entries = %#v, want one denied decision", auditStore.entries)
	}
	if _, err := r.Reconcile(ctx, Request{Namespace: ns, Name: "ttl-expire", Kind: autonomyReviewKind}); err != nil {
		t.Fatalf("replay TTL denial: %v", err)
	}
	profileRaw, _ := ms.Get(ctx, trustProfileKind, ns, profile.ObjectMeta.Name)
	if err := json.Unmarshal(profileRaw, &profile); err != nil {
		t.Fatal(err)
	}
	stats := profile.Status.ApprovalStats
	if stats == nil || stats.TotalRequested != 1 || stats.TotalDenied != 1 || stats.TotalApproved != 0 {
		t.Fatalf("approval stats = %#v, want one idempotent denial", stats)
	}
}

func TestAutonomyReviewReconciler_RejectsLateDecisionAndMalformedDeadline(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	r := NewAutonomyReviewReconciler(ms, zaptest.NewLogger(t))

	late := v1alpha1.AutonomyReview{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "late", Namespace: "default", CreationTimestamp: time.Now().Add(-time.Hour)},
		Spec:       v1alpha1.AutonomyReviewSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "agent-1"}},
		Status:     v1alpha1.AutonomyReviewStatus{Decision: DecisionPending, ExpiresAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)},
	}
	data, _ := json.Marshal(late)
	if err := ms.Create(ctx, autonomyReviewKind, "default", "late", data); err != nil {
		t.Fatal(err)
	}
	if err := r.Approve(ctx, "default", "late", "alice", "too late"); !errors.Is(err, ErrAutonomyReviewExpired) {
		t.Fatalf("late approval error = %v, want ErrAutonomyReviewExpired", err)
	}
	raw, _ := ms.Get(ctx, autonomyReviewKind, "default", "late")
	if err := json.Unmarshal(raw, &late); err != nil {
		t.Fatal(err)
	}
	if late.Status.Decision != DecisionDenied || late.Status.Reason != "TTL expired" {
		t.Fatalf("late review status = %#v, want TTL denial", late.Status)
	}

	malformed := v1alpha1.AutonomyReview{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "malformed", Namespace: "default"},
		Spec:       v1alpha1.AutonomyReviewSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "agent-2"}},
		Status:     v1alpha1.AutonomyReviewStatus{Decision: DecisionPending, ExpiresAt: "not-a-time"},
	}
	data, _ = json.Marshal(malformed)
	if err := ms.Create(ctx, autonomyReviewKind, "default", "malformed", data); err != nil {
		t.Fatal(err)
	}
	if err := r.Approve(ctx, "default", "malformed", "alice", "unsafe"); err == nil {
		t.Fatal("expected malformed deadline to fail closed")
	}
	raw, _ = ms.Get(ctx, autonomyReviewKind, "default", "malformed")
	if err := json.Unmarshal(raw, &malformed); err != nil {
		t.Fatal(err)
	}
	if malformed.Status.Decision != DecisionPending {
		t.Fatalf("malformed review decision = %q, want Pending", malformed.Status.Decision)
	}
}

func TestAutonomyReviewReconciler_Reconcile_StillPending(t *testing.T) {
	ms := newMemStore()
	logger := zaptest.NewLogger(t)
	r := NewAutonomyReviewReconciler(ms, logger).WithTTL(10 * time.Minute)

	ctx := context.Background()
	ns := "default"

	// Create a review with recent CreationTimestamp (within TTL).
	review := v1alpha1.AutonomyReview{
		TypeMeta:   v1alpha1.TypeMeta{Kind: "AutonomyReview", APIVersion: "v1alpha1"},
		ObjectMeta: v1alpha1.ObjectMeta{Namespace: ns, Name: "still-pending", CreationTimestamp: time.Now().Add(-2 * time.Minute)},
		Spec: v1alpha1.AutonomyReviewSpec{
			AgentRef:          v1alpha1.LocalObjectReference{Name: "agent-1"},
			ActionDescription: "some-action",
		},
		Status: v1alpha1.AutonomyReviewStatus{Decision: DecisionPending},
	}
	data, _ := json.Marshal(review)
	ms.Create(ctx, "AutonomyReview", ns, "still-pending", data)

	// Reconcile should requeue with half TTL since still pending.
	result, err := r.Reconcile(ctx, Request{Namespace: ns, Name: "still-pending", Kind: "AutonomyReview"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RequeueAfter <= 7*time.Minute || result.RequeueAfter > 8*time.Minute {
		t.Fatalf("expected RequeueAfter near 8m, got %v", result.RequeueAfter)
	}

	// Verify status is still Pending (not changed).
	raw, _ := ms.Get(ctx, "AutonomyReview", ns, "still-pending")
	var updated v1alpha1.AutonomyReview
	json.Unmarshal(raw, &updated)
	if updated.Status.Decision != DecisionPending {
		t.Errorf("expected still Pending, got %q", updated.Status.Decision)
	}
}
