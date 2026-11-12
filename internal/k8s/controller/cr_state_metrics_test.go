// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
)

func TestCRStateMetricsRefreshesSelectedTrustProfiles(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	planner := &k8s.Agent{ObjectMeta: metav1.ObjectMeta{Name: "planner", Namespace: "team-a", UID: types.UID("agent-planner")}}
	reviewer := &k8s.Agent{ObjectMeta: metav1.ObjectMeta{Name: "reviewer", Namespace: "team-a", UID: types.UID("agent-reviewer")}}
	unconfigured := &k8s.Agent{ObjectMeta: metav1.ObjectMeta{Name: "unconfigured", Namespace: "team-b", UID: types.UID("agent-unconfigured")}}
	plannerProfile := &k8s.TrustProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "planner-selected", Namespace: "team-a", UID: types.UID("profile-planner")},
		Spec:       k8s.TrustProfileSpec{AgentRef: k8s.LocalObjectReference{Name: "planner"}, AgentIncarnationUID: "agent-planner"},
		Status: k8s.TrustProfileStatus{
			CurrentScore: 82, CurrentClass: "supervised", CurrentTier: "trusted", TotalEvaluations: 3,
			ProbationUntil:     now.Add(time.Hour).Format(time.RFC3339Nano),
			PendingTrustChange: &k8s.TrustChangeAudit{Kind: "promotion", FromClass: "restricted", ToClass: "supervised", Evidence: "audit"},
		},
	}
	reviewerProfile := &k8s.TrustProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "reviewer-selected", Namespace: "team-a", UID: types.UID("profile-reviewer")},
		Spec:       k8s.TrustProfileSpec{AgentRef: k8s.LocalObjectReference{Name: "reviewer"}, AgentIncarnationUID: "agent-reviewer", InitialScore: 40},
		Status:     k8s.TrustProfileStatus{CurrentScore: 40, CurrentClass: "restricted", CurrentTier: "restricted"},
	}
	staleProfile := &k8s.TrustProfile{
		ObjectMeta: metav1.ObjectMeta{Name: "planner-stale", Namespace: "team-a", UID: types.UID("profile-stale")},
		Spec:       k8s.TrustProfileSpec{AgentRef: k8s.LocalObjectReference{Name: "planner"}, AgentIncarnationUID: "agent-planner"},
		Status:     k8s.TrustProfileStatus{CurrentScore: 5, CurrentClass: "blocked", CurrentTier: "untrusted", TotalEvaluations: 9},
	}
	plannerIndex := &k8s.TrustProfileIndex{
		ObjectMeta: metav1.ObjectMeta{Name: "planner-index", Namespace: "team-a"},
		Spec:       k8s.TrustProfileIndexSpec{AgentUID: "agent-planner", SelectedProfileName: "planner-selected", SelectedProfileUID: "profile-planner"},
	}
	reviewerIndex := &k8s.TrustProfileIndex{
		ObjectMeta: metav1.ObjectMeta{Name: "reviewer-index", Namespace: "team-a"},
		Spec:       k8s.TrustProfileIndexSpec{AgentUID: "agent-reviewer", SelectedProfileName: "reviewer-selected", SelectedProfileUID: "profile-reviewer"},
	}

	registry := prometheus.NewPedanticRegistry()
	metrics := NewCRStateMetrics(newFakeClient(t, planner, reviewer, unconfigured, plannerProfile, reviewerProfile, staleProfile, plannerIndex, reviewerIndex), registry, nil, time.Minute)
	if err := metrics.refreshTrustProfiles(context.Background(), now); err != nil {
		t.Fatalf("refresh trust metrics: %v", err)
	}

	assertMetricValue(t, registry, "kognition_agent_trust_profile_info", map[string]string{"namespace": "team-a", "agent": "planner", "evaluation_state": "evaluated", "trust_class": "supervised", "trust_tier": "trusted"}, 1)
	assertMetricValue(t, registry, "kognition_agent_trust_score", map[string]string{"namespace": "team-a", "agent": "planner"}, 82)
	assertMetricValue(t, registry, "kognition_agent_trust_probation", map[string]string{"namespace": "team-a", "agent": "planner"}, 1)
	assertMetricValue(t, registry, "kognition_agent_trust_change_pending", map[string]string{"namespace": "team-a", "agent": "planner"}, 1)
	assertMetricValue(t, registry, "kognition_agent_trust_profile_info", map[string]string{"namespace": "team-a", "agent": "reviewer", "evaluation_state": "not_evaluated", "trust_class": "restricted", "trust_tier": "restricted"}, 1)
	assertMetricAbsent(t, registry, "kognition_agent_trust_score", map[string]string{"namespace": "team-a", "agent": "reviewer"})
	assertMetricValue(t, registry, "kognition_agent_trust_profile_info", map[string]string{"namespace": "team-b", "agent": "unconfigured", "evaluation_state": "not_configured", "trust_class": "unknown", "trust_tier": "unknown"}, 1)
}

func TestCRStateMetricsRefreshesGovernanceState(t *testing.T) {
	quota := &k8s.CognitiveQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "team-budget", Namespace: "team-a"},
		Spec: k8s.CognitiveQuotaSpec{
			TokenBudget: &k8s.TokenBudget{MaxInputTokens: 700, MaxOutputTokens: 300, MaxTotalTokens: 1000},
			CostBudget:  &k8s.CostBudget{MaxCostPerPeriod: 25_000, MaxToolCallsPerPeriod: 40},
		},
		Status: k8s.CognitiveQuotaStatus{
			Phase:      "Ready",
			TokenUsage: &k8s.QuotaTokenUsage{InputTokens: 70, OutputTokens: 30, TotalTokens: 100},
		},
	}
	approval := &k8s.RuntimeApproval{
		ObjectMeta: metav1.ObjectMeta{Name: "approval-unique-id", Namespace: "team-a"},
		Spec:       k8s.RuntimeApprovalSpec{AgentRef: k8s.RuntimeApprovalReference{Name: "planner"}, Plane: "tool"},
		Status:     k8s.RuntimeApprovalStatus{DecisionState: "Pending", ExecutionState: "NotStarted"},
	}
	activeToolGrant := &k8s.ToolGrant{
		ObjectMeta: metav1.ObjectMeta{Name: "tool-direct", Namespace: "team-a"},
		Spec:       k8s.ToolGrantSpec{AgentRef: k8s.LocalObjectReference{Name: "planner"}, ToolRef: k8s.LocalObjectReference{Name: "search"}},
		Status:     k8s.ToolGrantStatus{Active: true, LastEvaluatedTime: "2026-09-10T12:00:00Z", GrantedAgents: 1},
	}
	unevaluatedSelectorGrant := &k8s.ToolGrant{
		ObjectMeta: metav1.ObjectMeta{Name: "tool-selector", Namespace: "team-a"},
		Spec:       k8s.ToolGrantSpec{AgentSelector: map[string]string{"team": "a"}, ToolRef: k8s.LocalObjectReference{Name: "search"}},
	}
	modelGrant := &k8s.ModelGrant{ObjectMeta: metav1.ObjectMeta{Name: "model", Namespace: "team-a"}, Spec: k8s.ModelGrantSpec{AgentRef: k8s.LocalObjectReference{Name: "planner"}}}
	memoryGrant := &k8s.MemoryGrant{ObjectMeta: metav1.ObjectMeta{Name: "memory", Namespace: "team-a"}, Spec: k8s.MemoryGrantSpec{AgentRef: k8s.LocalObjectReference{Name: "planner"}, AccessMode: "ReadOnly"}}

	registry := prometheus.NewPedanticRegistry()
	metrics := NewCRStateMetrics(newFakeClient(t, quota, approval, activeToolGrant, unevaluatedSelectorGrant, modelGrant, memoryGrant), registry, nil, time.Minute)
	if err := metrics.refreshGovernance(context.Background()); err != nil {
		t.Fatalf("refresh governance metrics: %v", err)
	}

	assertMetricValue(t, registry, "kognition_namespace_shared_quota_info", map[string]string{"namespace": "team-a", "quota": "team-budget", "phase": "Ready"}, 1)
	assertMetricValue(t, registry, "kognition_namespace_shared_quota_token_limit", map[string]string{"namespace": "team-a", "quota": "team-budget", "token_type": "total"}, 1000)
	assertMetricValue(t, registry, "kognition_namespace_shared_quota_token_usage", map[string]string{"namespace": "team-a", "quota": "team-budget", "token_type": "total"}, 100)
	assertMetricValue(t, registry, "kognition_namespace_shared_quota_usage_status_present", map[string]string{"namespace": "team-a", "quota": "team-budget", "usage": "cost"}, 0)
	assertMetricAbsent(t, registry, "kognition_namespace_shared_quota_cost_usage_micro_usd", map[string]string{"namespace": "team-a", "quota": "team-budget"})
	assertMetricValue(t, registry, "kognition_agent_runtime_approvals", map[string]string{"namespace": "team-a", "agent": "planner", "plane": "tool", "decision_state": "Pending", "execution_state": "NotStarted"}, 1)
	assertMetricValue(t, registry, "kognition_agent_tool_grants", map[string]string{"namespace": "team-a", "agent": "planner", "subject_type": "agent", "state": "active"}, 1)
	assertMetricValue(t, registry, "kognition_agent_tool_grants", map[string]string{"namespace": "team-a", "agent": "selector", "subject_type": "selector", "state": "not_evaluated"}, 1)
	assertMetricValue(t, registry, "kognition_tool_grant_granted_agents", map[string]string{"namespace": "team-a", "subject_type": "agent", "state": "active"}, 1)
	assertMetricValue(t, registry, "kognition_agent_model_grants_declared", map[string]string{"namespace": "team-a", "agent": "planner"}, 1)
	assertMetricValue(t, registry, "kognition_agent_memory_grants_declared", map[string]string{"namespace": "team-a", "agent": "planner", "access_mode": "ReadOnly"}, 1)
}

func TestCRStateMetricsRefreshesDependenciesAndReasoning(t *testing.T) {
	passing, failing := true, false
	modelClass := &k8s.ModelClass{
		ObjectMeta: metav1.ObjectMeta{Name: "premium-chat"},
		Spec:       k8s.ModelClassSpec{CostTier: "premium"},
		Status:     k8s.ModelClassStatus{Phase: "Ready", Provisioned: true, Health: "Healthy"},
	}
	model := &k8s.Model{
		ObjectMeta: metav1.ObjectMeta{Name: "chat", Namespace: "team-a"},
		Spec:       k8s.ModelSpec{ClassName: "premium-chat"},
		Status:     k8s.ModelStatus{Phase: "Ready", Available: true},
	}
	agent := &k8s.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "planner", Namespace: "team-a"},
		Spec:       k8s.AgentSpec{ModelRef: k8s.LocalObjectReference{Name: "chat"}},
	}
	failedInstance := &k8s.AgentInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "planner-instance-unique", Namespace: "team-a"},
		Spec:       k8s.AgentInstanceSpec{AgentRef: k8s.LocalObjectReference{Name: "planner"}},
		Status: k8s.AgentInstanceStatus{
			Phase: "Running",
			ProbeStatus: &k8s.ProbeStatus{
				Healthy: &passing, Ready: &passing, ReasoningHealthy: &failing, ConsecutiveReasoningFailures: 2,
				LastReasoningResult: &k8s.ReasoningCanaryResult{Outcome: k8s.CanaryOutcomeExecutionFailed, ClosedReason: k8s.CanaryFailureProviderFailed},
			},
		},
	}
	unreportedInstance := &k8s.AgentInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "planner-instance-other", Namespace: "team-a"},
		Spec:       k8s.AgentInstanceSpec{AgentRef: k8s.LocalObjectReference{Name: "planner"}},
		Status:     k8s.AgentInstanceStatus{Phase: "Pending"},
	}

	registry := prometheus.NewPedanticRegistry()
	metrics := NewCRStateMetrics(newFakeClient(t, modelClass, model, agent, failedInstance, unreportedInstance), registry, nil, time.Minute)
	if err := metrics.refreshDependencies(context.Background()); err != nil {
		t.Fatalf("refresh dependency metrics: %v", err)
	}

	assertMetricValue(t, registry, "kognition_model_class_info", map[string]string{"model_class": "premium-chat", "phase": "Ready", "health": "Healthy", "cost_tier": "premium", "provisioning_state": "provisioned"}, 1)
	assertMetricValue(t, registry, "kognition_models", map[string]string{"namespace": "team-a", "model_class": "premium-chat", "phase": "Ready", "availability_state": "available"}, 1)
	assertMetricValue(t, registry, "kognition_agent_model_dependency_info", map[string]string{"namespace": "team-a", "agent": "planner", "model": "chat", "model_class": "premium-chat", "model_phase": "Ready", "model_availability": "available", "model_class_health": "Healthy", "cost_tier": "premium"}, 1)
	assertMetricValue(t, registry, "kognition_agent_instances", map[string]string{"namespace": "team-a", "agent": "planner", "phase": "Running", "health_state": "passing", "readiness_state": "passing", "reasoning_state": "failing", "canary_outcome": "ExecutionFailed", "canary_failure_reason": "ProviderFailed"}, 1)
	assertMetricValue(t, registry, "kognition_agent_instances", map[string]string{"namespace": "team-a", "agent": "planner", "phase": "Pending", "health_state": "not_reported", "readiness_state": "not_reported", "reasoning_state": "not_reported", "canary_outcome": "not_evaluated", "canary_failure_reason": "none"}, 1)
	assertMetricValue(t, registry, "kognition_agent_probe_consecutive_failures", map[string]string{"namespace": "team-a", "agent": "planner", "probe": "reasoning"}, 2)
}

func TestCRStateMetricsRefreshesWorkflowParticipants(t *testing.T) {
	workflow := &k8s.Workflow{
		ObjectMeta: metav1.ObjectMeta{Name: "research", Namespace: "team-a"},
		Spec: k8s.WorkflowSpec{Steps: []k8s.WorkflowStep{
			{Name: "plan", AgentRef: k8s.LocalObjectReference{Name: "planner"}},
			{Name: "search", Type: k8s.WorkflowStepTypeTool},
		}},
	}

	registry := prometheus.NewPedanticRegistry()
	metrics := NewCRStateMetrics(newFakeClient(t, workflow), registry, nil, time.Minute)
	if err := metrics.refreshWorkflowParticipants(context.Background()); err != nil {
		t.Fatalf("refresh workflow participant metrics: %v", err)
	}

	assertMetricValue(t, registry, "kognition_workflow_step_participant_info", map[string]string{"namespace": "team-a", "workflow": "research", "step": "plan", "step_type": "agent", "agent": "planner"}, 1)
	assertMetricValue(t, registry, "kognition_workflow_step_participant_info", map[string]string{"namespace": "team-a", "workflow": "research", "step": "search", "step_type": "tool", "agent": "none"}, 1)
}

func assertMetricValue(t *testing.T, gatherer prometheus.Gatherer, name string, labels map[string]string, want float64) {
	t.Helper()
	value, ok := gatheredMetricValue(t, gatherer, name, labels)
	if !ok {
		t.Fatalf("metric %s labels %v not found", name, labels)
	}
	if value != want {
		t.Fatalf("metric %s labels %v = %v, want %v", name, labels, value, want)
	}
}

func assertMetricAbsent(t *testing.T, gatherer prometheus.Gatherer, name string, labels map[string]string) {
	t.Helper()
	if value, ok := gatheredMetricValue(t, gatherer, name, labels); ok {
		t.Fatalf("metric %s labels %v unexpectedly present with value %v", name, labels, value)
	}
}

func gatheredMetricValue(t *testing.T, gatherer prometheus.Gatherer, name string, labels map[string]string) (float64, bool) {
	t.Helper()
	families, err := gatherer.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			matched := len(metric.Label) == len(labels)
			for _, pair := range metric.Label {
				if labels[pair.GetName()] != pair.GetValue() {
					matched = false
					break
				}
			}
			if matched && metric.Gauge != nil {
				return metric.Gauge.GetValue(), true
			}
		}
	}
	return 0, false
}
