// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"sigs.k8s.io/controller-runtime/pkg/client"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
)

const defaultCRStateMetricsInterval = 30 * time.Second

// CRStateMetrics periodically projects bounded Kubernetes resource state into
// Prometheus gauges. It uses the manager cache and does not require leadership.
type CRStateMetrics struct {
	client   client.Client
	logger   *zap.Logger
	interval time.Duration

	trustProfileInfo     *prometheus.GaugeVec
	trustScore           *prometheus.GaugeVec
	trustProbation       *prometheus.GaugeVec
	trustChangePending   *prometheus.GaugeVec
	quotaInfo            *prometheus.GaugeVec
	quotaConfigured      *prometheus.GaugeVec
	quotaStatusPresent   *prometheus.GaugeVec
	quotaTokenLimit      *prometheus.GaugeVec
	quotaTokenUsage      *prometheus.GaugeVec
	quotaCostLimit       *prometheus.GaugeVec
	quotaCostUsage       *prometheus.GaugeVec
	quotaToolCallLimit   *prometheus.GaugeVec
	quotaToolCallUsage   *prometheus.GaugeVec
	runtimeApprovals     *prometheus.GaugeVec
	toolGrants           *prometheus.GaugeVec
	toolGrantedAgents    *prometheus.GaugeVec
	modelGrants          *prometheus.GaugeVec
	memoryGrants         *prometheus.GaugeVec
	modelClassInfo       *prometheus.GaugeVec
	models               *prometheus.GaugeVec
	agentModelInfo       *prometheus.GaugeVec
	agentInstances       *prometheus.GaugeVec
	agentProbeFailures   *prometheus.GaugeVec
	workflowParticipants *prometheus.GaugeVec
}

// NewCRStateMetrics creates and registers the current-state collectors.
func NewCRStateMetrics(c client.Client, registerer prometheus.Registerer, logger *zap.Logger, interval time.Duration) *CRStateMetrics {
	if interval <= 0 {
		interval = defaultCRStateMetricsInterval
	}
	metrics := &CRStateMetrics{
		client:   c,
		logger:   logger,
		interval: interval,
		trustProfileInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kognition_agent_trust_profile_info",
			Help: "Selected TrustProfile state for each Agent; evaluation_state is not_configured, not_evaluated, or evaluated.",
		}, []string{"namespace", "agent", "evaluation_state", "trust_class", "trust_tier"}),
		trustScore: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kognition_agent_trust_score",
			Help: "Current evaluated trust score for each Agent; absent until an evaluation has been retained.",
		}, []string{"namespace", "agent"}),
		trustProbation: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kognition_agent_trust_probation",
			Help: "Whether the selected TrustProfile is currently in probation.",
		}, []string{"namespace", "agent"}),
		trustChangePending: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kognition_agent_trust_change_pending",
			Help: "Whether the selected TrustProfile has a pending trust-change audit record.",
		}, []string{"namespace", "agent"}),
		quotaInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kognition_namespace_shared_quota_info",
			Help: "Configured CognitiveQuota objects representing shared namespace budgets.",
		}, []string{"namespace", "quota", "phase"}),
		quotaConfigured: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kognition_namespace_shared_quota_configured",
			Help: "Whether a token or cost budget is configured on a shared namespace quota.",
		}, []string{"namespace", "quota", "budget"}),
		quotaStatusPresent: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kognition_namespace_shared_quota_usage_status_present",
			Help: "Whether usage status has been reported for a shared namespace quota.",
		}, []string{"namespace", "quota", "usage"}),
		quotaTokenLimit: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kognition_namespace_shared_quota_token_limit",
			Help: "Configured token limits for a shared namespace quota.",
		}, []string{"namespace", "quota", "token_type"}),
		quotaTokenUsage: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kognition_namespace_shared_quota_token_usage",
			Help: "Reported token usage for a shared namespace quota.",
		}, []string{"namespace", "quota", "token_type"}),
		quotaCostLimit: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kognition_namespace_shared_quota_cost_limit_micro_usd",
			Help: "Configured normalized cost limit in micro-USD for a shared namespace quota.",
		}, []string{"namespace", "quota"}),
		quotaCostUsage: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kognition_namespace_shared_quota_cost_usage_micro_usd",
			Help: "Reported normalized cost usage in micro-USD for a shared namespace quota.",
		}, []string{"namespace", "quota"}),
		quotaToolCallLimit: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kognition_namespace_shared_quota_tool_call_limit",
			Help: "Configured tool-call limit for a shared namespace quota.",
		}, []string{"namespace", "quota"}),
		quotaToolCallUsage: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kognition_namespace_shared_quota_tool_call_usage",
			Help: "Reported tool-call usage for a shared namespace quota.",
		}, []string{"namespace", "quota"}),
		runtimeApprovals: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kognition_agent_runtime_approvals",
			Help: "Current RuntimeApproval objects grouped by bounded governance state.",
		}, []string{"namespace", "agent", "plane", "decision_state", "execution_state"}),
		toolGrants: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kognition_agent_tool_grants",
			Help: "ToolGrant declarations grouped by direct Agent or selector and evaluated state.",
		}, []string{"namespace", "agent", "subject_type", "state"}),
		toolGrantedAgents: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kognition_tool_grant_granted_agents",
			Help: "Agents reported as granted by evaluated ToolGrants.",
		}, []string{"namespace", "subject_type", "state"}),
		modelGrants: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kognition_agent_model_grants_declared",
			Help: "Declared ModelGrants; ModelGrant has no evaluated status.",
		}, []string{"namespace", "agent"}),
		memoryGrants: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kognition_agent_memory_grants_declared",
			Help: "Declared MemoryGrants by access mode; MemoryGrant has no evaluated status.",
		}, []string{"namespace", "agent", "access_mode"}),
		modelClassInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kognition_model_class_info",
			Help: "Current ModelClass lifecycle, health, cost tier, and provisioning state.",
		}, []string{"model_class", "phase", "health", "cost_tier", "provisioning_state"}),
		models: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kognition_models",
			Help: "Current Models grouped by class, lifecycle phase, and availability state.",
		}, []string{"namespace", "model_class", "phase", "availability_state"}),
		agentModelInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kognition_agent_model_dependency_info",
			Help: "Resolved Model and ModelClass dependency state for each Agent.",
		}, []string{"namespace", "agent", "model", "model_class", "model_phase", "model_availability", "model_class_health", "cost_tier"}),
		agentInstances: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kognition_agent_instances",
			Help: "AgentInstances grouped by bounded lifecycle, probe, and terminal reasoning-canary state.",
		}, []string{"namespace", "agent", "phase", "health_state", "readiness_state", "reasoning_state", "canary_outcome", "canary_failure_reason"}),
		agentProbeFailures: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kognition_agent_probe_consecutive_failures",
			Help: "Sum of consecutive probe failures across current AgentInstances.",
		}, []string{"namespace", "agent", "probe"}),
		workflowParticipants: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "kognition_workflow_step_participant_info",
			Help: "Declared workflow step participants from the current Workflow template.",
		}, []string{"namespace", "workflow", "step", "step_type", "agent"}),
	}
	registerer.MustRegister(
		metrics.trustProfileInfo, metrics.trustScore, metrics.trustProbation, metrics.trustChangePending,
		metrics.quotaInfo, metrics.quotaConfigured, metrics.quotaStatusPresent,
		metrics.quotaTokenLimit, metrics.quotaTokenUsage, metrics.quotaCostLimit, metrics.quotaCostUsage,
		metrics.quotaToolCallLimit, metrics.quotaToolCallUsage, metrics.runtimeApprovals,
		metrics.toolGrants, metrics.toolGrantedAgents, metrics.modelGrants, metrics.memoryGrants,
		metrics.modelClassInfo, metrics.models, metrics.agentModelInfo, metrics.agentInstances, metrics.agentProbeFailures,
		metrics.workflowParticipants,
	)
	return metrics
}

// Start refreshes current-state gauges until the manager stops.
func (m *CRStateMetrics) Start(ctx context.Context) error {
	m.refresh(ctx)
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			m.refresh(ctx)
		}
	}
}

// NeedLeaderElection lets every controller manager replica expose its cached view.
func (*CRStateMetrics) NeedLeaderElection() bool { return false }

func (m *CRStateMetrics) refresh(ctx context.Context) {
	if err := m.refreshTrustProfiles(ctx, time.Now().UTC()); err != nil && m.logger != nil {
		m.logger.Warn("refresh TrustProfile metrics failed", zap.Error(err))
	}
	if err := m.refreshGovernance(ctx); err != nil && m.logger != nil {
		m.logger.Warn("refresh governance metrics failed", zap.Error(err))
	}
	if err := m.refreshDependencies(ctx); err != nil && m.logger != nil {
		m.logger.Warn("refresh dependency metrics failed", zap.Error(err))
	}
	if err := m.refreshWorkflowParticipants(ctx); err != nil && m.logger != nil {
		m.logger.Warn("refresh workflow participant metrics failed", zap.Error(err))
	}
}

func (m *CRStateMetrics) refreshTrustProfiles(ctx context.Context, now time.Time) error {
	agents := &k8s.AgentList{}
	if err := m.client.List(ctx, agents); err != nil {
		return fmt.Errorf("list Agents: %w", err)
	}
	indexes := &k8s.TrustProfileIndexList{}
	if err := m.client.List(ctx, indexes); err != nil {
		return fmt.Errorf("list TrustProfileIndexes: %w", err)
	}
	profiles := &k8s.TrustProfileList{}
	if err := m.client.List(ctx, profiles); err != nil {
		return fmt.Errorf("list TrustProfiles: %w", err)
	}

	indexByAgent := make(map[string]*k8s.TrustProfileIndex, len(indexes.Items))
	for index := range indexes.Items {
		item := &indexes.Items[index]
		indexByAgent[item.Namespace+"/"+item.Spec.AgentUID] = item
	}
	profileByName := make(map[string]*k8s.TrustProfile, len(profiles.Items))
	for index := range profiles.Items {
		item := &profiles.Items[index]
		profileByName[item.Namespace+"/"+item.Name] = item
	}

	type trustSample struct {
		namespace, agent, state, class, tier string
		evaluated, probation, pending        bool
		score                                float64
	}
	samples := make([]trustSample, 0, len(agents.Items))
	for index := range agents.Items {
		agent := &agents.Items[index]
		sample := trustSample{namespace: agent.Namespace, agent: agent.Name, state: "not_configured", class: "unknown", tier: "unknown"}
		selectedIndex := indexByAgent[agent.Namespace+"/"+string(agent.UID)]
		if selectedIndex != nil {
			profile := profileByName[agent.Namespace+"/"+selectedIndex.Spec.SelectedProfileName]
			if validSelectedTrustProfile(agent, selectedIndex, profile) {
				sample.state = "not_evaluated"
				sample.class = metricValueOrUnknown(profile.Status.CurrentClass)
				sample.tier = metricValueOrUnknown(profile.Status.CurrentTier)
				sample.evaluated = profile.Status.TotalEvaluations > 0 || profile.Status.LastEvaluationTime != ""
				if sample.evaluated {
					sample.state = "evaluated"
					sample.score = profile.Status.CurrentScore
				}
				sample.probation = activeStatusDeadline(profile.Status.ProbationUntil, now)
				sample.pending = profile.Status.PendingTrustChange != nil
			}
		}
		samples = append(samples, sample)
	}

	m.trustProfileInfo.Reset()
	m.trustScore.Reset()
	m.trustProbation.Reset()
	m.trustChangePending.Reset()
	for _, sample := range samples {
		m.trustProfileInfo.WithLabelValues(sample.namespace, sample.agent, sample.state, sample.class, sample.tier).Set(1)
		m.trustProbation.WithLabelValues(sample.namespace, sample.agent).Set(boolMetric(sample.probation))
		m.trustChangePending.WithLabelValues(sample.namespace, sample.agent).Set(boolMetric(sample.pending))
		if sample.evaluated {
			m.trustScore.WithLabelValues(sample.namespace, sample.agent).Set(sample.score)
		}
	}
	return nil
}

func validSelectedTrustProfile(agent *k8s.Agent, index *k8s.TrustProfileIndex, profile *k8s.TrustProfile) bool {
	return profile != nil && profile.DeletionTimestamp.IsZero() &&
		index.Spec.AgentUID == string(agent.UID) &&
		index.Spec.SelectedProfileUID == string(profile.UID) &&
		profile.Spec.AgentIncarnationUID == string(agent.UID) &&
		profile.Spec.AgentRef.Name == agent.Name
}

func metricValueOrUnknown(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}

func activeStatusDeadline(value string, now time.Time) bool {
	deadline, err := time.Parse(time.RFC3339Nano, value)
	return err == nil && now.Before(deadline)
}

func boolMetric(value bool) float64 {
	if value {
		return 1
	}
	return 0
}

func (m *CRStateMetrics) refreshGovernance(ctx context.Context) error {
	quotas := &k8s.CognitiveQuotaList{}
	if err := m.client.List(ctx, quotas); err != nil {
		return fmt.Errorf("list CognitiveQuotas: %w", err)
	}
	approvals := &k8s.RuntimeApprovalList{}
	if err := m.client.List(ctx, approvals); err != nil {
		return fmt.Errorf("list RuntimeApprovals: %w", err)
	}
	toolGrants := &k8s.ToolGrantList{}
	if err := m.client.List(ctx, toolGrants); err != nil {
		return fmt.Errorf("list ToolGrants: %w", err)
	}
	modelGrants := &k8s.ModelGrantList{}
	if err := m.client.List(ctx, modelGrants); err != nil {
		return fmt.Errorf("list ModelGrants: %w", err)
	}
	memoryGrants := &k8s.MemoryGrantList{}
	if err := m.client.List(ctx, memoryGrants); err != nil {
		return fmt.Errorf("list MemoryGrants: %w", err)
	}

	m.quotaInfo.Reset()
	m.quotaConfigured.Reset()
	m.quotaStatusPresent.Reset()
	m.quotaTokenLimit.Reset()
	m.quotaTokenUsage.Reset()
	m.quotaCostLimit.Reset()
	m.quotaCostUsage.Reset()
	m.quotaToolCallLimit.Reset()
	m.quotaToolCallUsage.Reset()
	for index := range quotas.Items {
		quota := &quotas.Items[index]
		m.quotaInfo.WithLabelValues(quota.Namespace, quota.Name, metricValueOrUnknown(quota.Status.Phase)).Set(1)
		m.quotaConfigured.WithLabelValues(quota.Namespace, quota.Name, "token").Set(boolMetric(quota.Spec.TokenBudget != nil))
		m.quotaConfigured.WithLabelValues(quota.Namespace, quota.Name, "cost").Set(boolMetric(quota.Spec.CostBudget != nil))
		m.quotaStatusPresent.WithLabelValues(quota.Namespace, quota.Name, "token").Set(boolMetric(quota.Status.TokenUsage != nil))
		m.quotaStatusPresent.WithLabelValues(quota.Namespace, quota.Name, "cost").Set(boolMetric(quota.Status.CostUsage != nil))
		if budget := quota.Spec.TokenBudget; budget != nil {
			m.quotaTokenLimit.WithLabelValues(quota.Namespace, quota.Name, "input").Set(float64(budget.MaxInputTokens))
			m.quotaTokenLimit.WithLabelValues(quota.Namespace, quota.Name, "output").Set(float64(budget.MaxOutputTokens))
			m.quotaTokenLimit.WithLabelValues(quota.Namespace, quota.Name, "total").Set(float64(budget.MaxTotalTokens))
		}
		if usage := quota.Status.TokenUsage; usage != nil {
			m.quotaTokenUsage.WithLabelValues(quota.Namespace, quota.Name, "input").Set(float64(usage.InputTokens))
			m.quotaTokenUsage.WithLabelValues(quota.Namespace, quota.Name, "output").Set(float64(usage.OutputTokens))
			m.quotaTokenUsage.WithLabelValues(quota.Namespace, quota.Name, "total").Set(float64(usage.TotalTokens))
		}
		if budget := quota.Spec.CostBudget; budget != nil {
			m.quotaCostLimit.WithLabelValues(quota.Namespace, quota.Name).Set(float64(budget.MaxCostPerPeriod))
			m.quotaToolCallLimit.WithLabelValues(quota.Namespace, quota.Name).Set(float64(budget.MaxToolCallsPerPeriod))
		}
		if usage := quota.Status.CostUsage; usage != nil {
			m.quotaCostUsage.WithLabelValues(quota.Namespace, quota.Name).Set(float64(usage.TotalCost))
			m.quotaToolCallUsage.WithLabelValues(quota.Namespace, quota.Name).Set(float64(usage.ToolCalls))
		}
	}

	m.runtimeApprovals.Reset()
	for index := range approvals.Items {
		approval := &approvals.Items[index]
		m.runtimeApprovals.WithLabelValues(
			approval.Namespace,
			metricValueOrUnknown(approval.Spec.AgentRef.Name),
			metricValueOrUnknown(approval.Spec.Plane),
			metricValueOrUnknown(approval.Status.DecisionState),
			metricValueOrUnknown(approval.Status.ExecutionState),
		).Inc()
	}

	m.toolGrants.Reset()
	m.toolGrantedAgents.Reset()
	for index := range toolGrants.Items {
		grant := &toolGrants.Items[index]
		agent, subjectType := grant.Spec.AgentRef.Name, "agent"
		if agent == "" {
			agent, subjectType = "selector", "selector"
		}
		state := "not_evaluated"
		if grant.Status.LastEvaluatedTime != "" {
			state = "inactive"
			if grant.Status.Active {
				state = "active"
			}
		}
		m.toolGrants.WithLabelValues(grant.Namespace, agent, subjectType, state).Inc()
		if grant.Status.LastEvaluatedTime != "" {
			m.toolGrantedAgents.WithLabelValues(grant.Namespace, subjectType, state).Add(float64(grant.Status.GrantedAgents))
		}
	}

	m.modelGrants.Reset()
	for index := range modelGrants.Items {
		grant := &modelGrants.Items[index]
		m.modelGrants.WithLabelValues(grant.Namespace, metricValueOrUnknown(grant.Spec.AgentRef.Name)).Inc()
	}
	m.memoryGrants.Reset()
	for index := range memoryGrants.Items {
		grant := &memoryGrants.Items[index]
		m.memoryGrants.WithLabelValues(grant.Namespace, metricValueOrUnknown(grant.Spec.AgentRef.Name), metricValueOrUnknown(grant.Spec.AccessMode)).Inc()
	}
	return nil
}

func (m *CRStateMetrics) refreshDependencies(ctx context.Context) error {
	agents := &k8s.AgentList{}
	if err := m.client.List(ctx, agents); err != nil {
		return fmt.Errorf("list Agents for dependencies: %w", err)
	}
	instances := &k8s.AgentInstanceList{}
	if err := m.client.List(ctx, instances); err != nil {
		return fmt.Errorf("list AgentInstances: %w", err)
	}
	models := &k8s.ModelList{}
	if err := m.client.List(ctx, models); err != nil {
		return fmt.Errorf("list Models: %w", err)
	}
	classes := &k8s.ModelClassList{}
	if err := m.client.List(ctx, classes); err != nil {
		return fmt.Errorf("list ModelClasses: %w", err)
	}

	classByName := make(map[string]*k8s.ModelClass, len(classes.Items))
	m.modelClassInfo.Reset()
	for index := range classes.Items {
		class := &classes.Items[index]
		classByName[class.Name] = class
		m.modelClassInfo.WithLabelValues(
			class.Name,
			metricValueOrUnknown(class.Status.Phase),
			metricValueOrUnknown(class.Status.Health),
			metricValueOrUnknown(class.Spec.CostTier),
			modelClassProvisioningState(class),
		).Set(1)
	}

	modelByName := make(map[string]*k8s.Model, len(models.Items))
	m.models.Reset()
	for index := range models.Items {
		model := &models.Items[index]
		modelByName[model.Namespace+"/"+model.Name] = model
		m.models.WithLabelValues(
			model.Namespace,
			metricValueOrUnknown(model.Spec.ClassName),
			metricValueOrUnknown(model.Status.Phase),
			modelAvailabilityState(model),
		).Inc()
	}

	m.agentModelInfo.Reset()
	for index := range agents.Items {
		agent := &agents.Items[index]
		modelName := metricValueOrUnknown(agent.Spec.ModelRef.Name)
		modelClass, modelPhase, availability, classHealth, costTier := "unknown", "missing", "missing", "missing", "unknown"
		if model := modelByName[agent.Namespace+"/"+agent.Spec.ModelRef.Name]; model != nil {
			modelClass = metricValueOrUnknown(model.Spec.ClassName)
			modelPhase = metricValueOrUnknown(model.Status.Phase)
			availability = modelAvailabilityState(model)
			if class := classByName[model.Spec.ClassName]; class != nil {
				classHealth = metricValueOrUnknown(class.Status.Health)
				costTier = metricValueOrUnknown(class.Spec.CostTier)
			}
		}
		m.agentModelInfo.WithLabelValues(agent.Namespace, agent.Name, modelName, modelClass, modelPhase, availability, classHealth, costTier).Set(1)
	}

	m.agentInstances.Reset()
	m.agentProbeFailures.Reset()
	for index := range instances.Items {
		instance := &instances.Items[index]
		health, readiness, reasoning := "not_reported", "not_reported", "not_reported"
		outcome, failureReason := "not_evaluated", "none"
		if probes := instance.Status.ProbeStatus; probes != nil {
			health = optionalBoolState(probes.Healthy)
			readiness = optionalBoolState(probes.Ready)
			reasoning = optionalBoolState(probes.ReasoningHealthy)
			m.agentProbeFailures.WithLabelValues(instance.Namespace, metricValueOrUnknown(instance.Spec.AgentRef.Name), "health").Add(float64(probes.ConsecutiveHealthFailures))
			m.agentProbeFailures.WithLabelValues(instance.Namespace, metricValueOrUnknown(instance.Spec.AgentRef.Name), "readiness").Add(float64(probes.ConsecutiveReadinessFailures))
			m.agentProbeFailures.WithLabelValues(instance.Namespace, metricValueOrUnknown(instance.Spec.AgentRef.Name), "reasoning").Add(float64(probes.ConsecutiveReasoningFailures))
			if result := probes.LastReasoningResult; result != nil {
				outcome = metricValueOrUnknown(string(result.Outcome))
				if result.ClosedReason != "" {
					failureReason = string(result.ClosedReason)
				}
			}
		}
		m.agentInstances.WithLabelValues(
			instance.Namespace,
			metricValueOrUnknown(instance.Spec.AgentRef.Name),
			metricValueOrUnknown(instance.Status.Phase),
			health,
			readiness,
			reasoning,
			outcome,
			failureReason,
		).Inc()
	}
	return nil
}

func modelClassProvisioningState(class *k8s.ModelClass) string {
	if class.Status.Phase == "" && class.Status.Health == "" && len(class.Status.Conditions) == 0 && class.Status.LastProbeTime == "" {
		return "not_reported"
	}
	if class.Status.Provisioned {
		return "provisioned"
	}
	return "not_provisioned"
}

func modelAvailabilityState(model *k8s.Model) string {
	if model.Status.Phase == "" && len(model.Status.Conditions) == 0 && model.Status.LastCheck == "" {
		return "not_reported"
	}
	if model.Status.Available {
		return "available"
	}
	return "unavailable"
}

func optionalBoolState(value *bool) string {
	if value == nil {
		return "not_reported"
	}
	if *value {
		return "passing"
	}
	return "failing"
}

func (m *CRStateMetrics) refreshWorkflowParticipants(ctx context.Context) error {
	workflows := &k8s.WorkflowList{}
	if err := m.client.List(ctx, workflows); err != nil {
		return fmt.Errorf("list Workflows: %w", err)
	}

	m.workflowParticipants.Reset()
	for workflowIndex := range workflows.Items {
		workflow := &workflows.Items[workflowIndex]
		for stepIndex := range workflow.Spec.Steps {
			step := &workflow.Spec.Steps[stepIndex]
			stepType := string(step.Type)
			if stepType == "" {
				stepType = string(k8s.WorkflowStepTypeAgent)
			}
			agent := step.AgentRef.Name
			if agent == "" {
				agent = "none"
			}
			m.workflowParticipants.WithLabelValues(workflow.Namespace, workflow.Name, step.Name, stepType, agent).Set(1)
		}
	}
	return nil
}
