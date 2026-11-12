// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/lifecycle"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

// nowFunc is a package-level variable for time injection in tests.
var nowFunc = time.Now

const cognitiveQuotaKind = "CognitiveQuota"

// CognitiveQuotaReconciler tracks resource usage against defined quotas.
type CognitiveQuotaReconciler struct {
	store  store.Store
	logger *zap.Logger
}

// NewCognitiveQuotaReconciler creates a new CognitiveQuotaReconciler.
func NewCognitiveQuotaReconciler(s store.Store, logger *zap.Logger) *CognitiveQuotaReconciler {
	return &CognitiveQuotaReconciler{
		store:  s,
		logger: logger.With(zap.String("controller", "cognitive-quota")),
	}
}

// Reconcile ensures CognitiveQuota status reflects actual usage.
func (r *CognitiveQuotaReconciler) Reconcile(ctx context.Context, req Request) (Result, error) {
	raw, err := r.store.Get(ctx, cognitiveQuotaKind, req.Namespace, req.Name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			r.logger.Debug("CognitiveQuota deleted, skipping", zap.String("name", req.Name), requestid.ZapField(ctx))
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("get CognitiveQuota: %w", err)
	}

	var quota v1alpha1.CognitiveQuota
	if err := json.Unmarshal(raw, &quota); err != nil {
		return Result{}, fmt.Errorf("unmarshal CognitiveQuota: %w", err)
	}

	// Initialize status.Used if nil.
	if quota.Status.Used == nil {
		quota.Status.Used = make(map[string]int64)
	}

	oldPhase := quota.Status.Phase
	oldMessage := quota.Status.Message

	// Check if usage counters need to be reset based on ResetInterval.
	updated := r.shouldReset(ctx, &quota)
	if updated {
		r.resetUsage(&quota)
	}

	// Calculate current usage for each hard limit resource.
	for resource := range quota.Spec.Hard {
		currentUsage, err := r.calculateUsage(ctx, req.Namespace, resource)
		if err != nil {
			r.logger.Warn("failed to calculate usage",
				zap.String("resource", resource),
				zap.Error(err),
				requestid.ZapField(ctx))
			continue
		}
		if quota.Status.Used[resource] != currentUsage {
			quota.Status.Used[resource] = currentUsage
			updated = true
		}
	}

	// Determine phase from current usage vs hard limits.
	var exceeded []string
	for _, resource := range sortedQuotaResources(quota.Spec.Hard) {
		if quota.Status.Used[resource] > quota.Spec.Hard[resource] {
			exceeded = append(exceeded, resource)
		}
	}

	var (
		newPhase   v1alpha1.Phase
		newMessage string
		readyState lifecycle.ConditionStatus
		reason     string
	)
	if len(exceeded) > 0 {
		newPhase = v1alpha1.PhaseDegraded
		newMessage = fmt.Sprintf("quota exceeded for %s", strings.Join(exceeded, ", "))
		readyState = lifecycle.ConditionFalse
		reason = "QuotaExceeded"
	} else {
		newPhase = v1alpha1.PhaseReady
		newMessage = "usage within configured limits"
		readyState = lifecycle.ConditionTrue
		reason = "WithinLimits"
	}

	if oldPhase != newPhase || oldMessage != newMessage {
		quota.Status.Phase = newPhase
		quota.Status.Message = newMessage
		lc := lifecycle.SetCondition(
			apiToLifecycleConditions(quota.Status.Conditions),
			lifecycle.ConditionReady,
			readyState,
			reason,
			newMessage,
		)
		quota.Status.Conditions = lifecycleToAPIConditions(lc)
		updated = true
	}

	if updated {
		data, err := json.Marshal(quota)
		if err != nil {
			return Result{}, fmt.Errorf("marshal CognitiveQuota: %w", err)
		}
		rv := quota.ObjectMeta.ResourceVersion
		if err := r.store.Update(ctx, cognitiveQuotaKind, req.Namespace, req.Name, data, rv); err != nil {
			if errors.Is(err, store.ErrConflict) {
				return Result{Requeue: true}, nil
			}
			return Result{}, fmt.Errorf("update CognitiveQuota status: %w", err)
		}
		r.logger.Info("CognitiveQuota status updated",
			zap.String("namespace", req.Namespace),
			zap.String("name", req.Name),
			zap.String("phase", string(newPhase)),
			requestid.ZapField(ctx))
	}

	return Result{RequeueAfter: r.resetRequeueAfter(&quota)}, nil
}

// sortedQuotaResources returns the keys of a string-keyed int64 map in stable order.
func sortedQuotaResources(m map[string]int64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// calculateUsage calculates the current usage of a resource in a namespace.
func (r *CognitiveQuotaReconciler) calculateUsage(ctx context.Context, namespace, resource string) (int64, error) {
	// Map resource names to resource kinds for counting.
	kindMap := map[string]string{
		"agents":       "Agent",
		"tools":        "Tool",
		"sessions":     "Session",
		"tokens":       "", // tokens are tracked via events, not object count
		"cost":         "", // cost is tracked via events, not object count
		"model-grants": "ModelGrant",
		"tool-grants":  "ToolGrant",
	}

	kind, ok := kindMap[resource]
	if !ok || kind == "" {
		// Non-countable resources (tokens, cost) are tracked externally.
		return 0, nil
	}

	items, err := r.store.List(ctx, kind, store.ListOptions{Namespace: namespace})
	if err != nil {
		return 0, fmt.Errorf("list %s: %w", kind, err)
	}

	return int64(len(items)), nil
}

// CheckQuota validates whether a resource creation would exceed quota limits.
func (r *CognitiveQuotaReconciler) CheckQuota(ctx context.Context, namespace, resource string, additional int64) error {
	if additional < 0 {
		return fmt.Errorf("quota request for %q cannot be negative", resource)
	}

	// List all quotas in the namespace.
	items, err := r.store.List(ctx, cognitiveQuotaKind, store.ListOptions{Namespace: namespace})
	if err != nil {
		return fmt.Errorf("list quotas: %w", err)
	}

	for _, raw := range items {
		var quota v1alpha1.CognitiveQuota
		if err := json.Unmarshal(raw, &quota); err != nil {
			continue
		}

		hardLimit, hasLimit := quota.Spec.Hard[resource]
		if !hasLimit {
			continue
		}

		used := quota.Status.Used[resource]
		requested, err := addQuotaUsage(used, additional)
		if err != nil {
			return fmt.Errorf("invalid quota usage for %q: %w", resource, err)
		}
		if requested > hardLimit {
			return fmt.Errorf("quota exceeded for %q: used %d + requested %d > limit %d",
				resource, used, additional, hardLimit)
		}
	}

	return nil
}

// IncrementUsage atomically increments the usage counter for a resource.
func (r *CognitiveQuotaReconciler) IncrementUsage(ctx context.Context, namespace, resource string, delta int64) error {
	if delta < 0 {
		return fmt.Errorf("quota usage increment for %q cannot be negative", resource)
	}

	items, err := r.store.List(ctx, cognitiveQuotaKind, store.ListOptions{Namespace: namespace})
	if err != nil {
		return fmt.Errorf("list quotas: %w", err)
	}

	for _, raw := range items {
		var quota v1alpha1.CognitiveQuota
		if err := json.Unmarshal(raw, &quota); err != nil {
			continue
		}

		if _, hasLimit := quota.Spec.Hard[resource]; !hasLimit {
			continue
		}
		var accountingErr error
		if err := r.updateUsageWithRetry(ctx, quota.ObjectMeta.Namespace, quota.ObjectMeta.Name, func(latest *v1alpha1.CognitiveQuota) bool {
			if _, hasLimit := latest.Spec.Hard[resource]; !hasLimit {
				return false
			}
			if latest.Status.Used == nil {
				latest.Status.Used = make(map[string]int64)
			}
			latest.Status.Used[resource], accountingErr = addQuotaUsage(latest.Status.Used[resource], delta)
			return accountingErr == nil
		}); err != nil {
			return fmt.Errorf("update quota usage: %w", err)
		}
		if accountingErr != nil {
			return fmt.Errorf("update quota usage: %w", accountingErr)
		}
	}

	return nil
}

// shouldReset returns true if the quota's reset interval has elapsed since LastResetTime.
func (r *CognitiveQuotaReconciler) shouldReset(ctx context.Context, quota *v1alpha1.CognitiveQuota) bool {
	if quota.Spec.ResetInterval == "" {
		return false
	}
	interval, err := time.ParseDuration(quota.Spec.ResetInterval)
	if err == nil && interval <= 0 {
		err = fmt.Errorf("duration must be positive")
	}
	if err != nil {
		r.logger.Warn("invalid ResetInterval", zap.String("interval", quota.Spec.ResetInterval), zap.Error(err), requestid.ZapField(ctx))
		return false
	}

	if quota.Status.LastResetTime == "" {
		return true // Never reset before.
	}

	lastReset, err := time.Parse(time.RFC3339, quota.Status.LastResetTime)
	if err != nil {
		r.logger.Warn("invalid LastResetTime", zap.String("time", quota.Status.LastResetTime), zap.Error(err), requestid.ZapField(ctx))
		return true
	}

	return nowFunc().Sub(lastReset) >= interval
}

func (r *CognitiveQuotaReconciler) resetRequeueAfter(quota *v1alpha1.CognitiveQuota) time.Duration {
	if quota.Spec.ResetInterval == "" || quota.Status.LastResetTime == "" {
		return 0
	}
	interval, err := time.ParseDuration(quota.Spec.ResetInterval)
	if err != nil || interval <= 0 {
		return 0
	}
	lastReset, err := time.Parse(time.RFC3339Nano, quota.Status.LastResetTime)
	if err != nil {
		return 0
	}
	remaining := lastReset.Add(interval).Sub(nowFunc())
	if remaining <= 0 {
		return time.Millisecond
	}
	return remaining
}

// resetUsage zeroes out token and cost usage counters and updates LastResetTime.
func (r *CognitiveQuotaReconciler) resetUsage(quota *v1alpha1.CognitiveQuota) {
	quota.Status.TokenUsage = nil
	quota.Status.CostUsage = nil
	quota.Status.LastResetTime = nowFunc().UTC().Format(time.RFC3339Nano)
	r.logger.Info("quota usage reset",
		zap.String("namespace", quota.ObjectMeta.Namespace),
		zap.String("name", quota.ObjectMeta.Name))
}

// IncrementTokenUsage adds token consumption to the quota status.
func (r *CognitiveQuotaReconciler) IncrementTokenUsage(ctx context.Context, namespace string, input, output int64) error {
	total, err := addQuotaDelta(input, output)
	if err != nil {
		return fmt.Errorf("invalid token usage increment: %w", err)
	}

	items, err := r.store.List(ctx, cognitiveQuotaKind, store.ListOptions{Namespace: namespace})
	if err != nil {
		return fmt.Errorf("list quotas: %w", err)
	}

	for _, raw := range items {
		var quota v1alpha1.CognitiveQuota
		if err := json.Unmarshal(raw, &quota); err != nil {
			continue
		}
		if quota.Spec.TokenBudget == nil {
			continue
		}
		var accountingErr error
		if err := r.updateUsageWithRetry(ctx, quota.ObjectMeta.Namespace, quota.ObjectMeta.Name, func(latest *v1alpha1.CognitiveQuota) bool {
			if latest.Spec.TokenBudget == nil {
				return false
			}
			if latest.Status.TokenUsage == nil {
				latest.Status.TokenUsage = &v1alpha1.QuotaTokenUsage{}
			}
			usage := latest.Status.TokenUsage
			usage.InputTokens, accountingErr = applyQuotaDelta(usage.InputTokens, input)
			if accountingErr != nil {
				return false
			}
			usage.OutputTokens, accountingErr = applyQuotaDelta(usage.OutputTokens, output)
			if accountingErr != nil {
				return false
			}
			usage.TotalTokens, accountingErr = applyQuotaDelta(usage.TotalTokens, total)
			return accountingErr == nil
		}); err != nil {
			return fmt.Errorf("update token usage: %w", err)
		}
		if accountingErr != nil {
			return fmt.Errorf("update token usage: %w", accountingErr)
		}
	}
	return nil
}

// IncrementCostUsage adds cost consumption to the quota status.
func (r *CognitiveQuotaReconciler) IncrementCostUsage(ctx context.Context, namespace string, cost int64, toolCalls int64) error {
	if cost < 0 || toolCalls < 0 {
		return fmt.Errorf("cost usage increments cannot be negative")
	}

	items, err := r.store.List(ctx, cognitiveQuotaKind, store.ListOptions{Namespace: namespace})
	if err != nil {
		return fmt.Errorf("list quotas: %w", err)
	}

	for _, raw := range items {
		var quota v1alpha1.CognitiveQuota
		if err := json.Unmarshal(raw, &quota); err != nil {
			continue
		}
		if quota.Spec.CostBudget == nil {
			continue
		}
		var accountingErr error
		if err := r.updateUsageWithRetry(ctx, quota.ObjectMeta.Namespace, quota.ObjectMeta.Name, func(latest *v1alpha1.CognitiveQuota) bool {
			if latest.Spec.CostBudget == nil {
				return false
			}
			if latest.Status.CostUsage == nil {
				latest.Status.CostUsage = &v1alpha1.QuotaCostUsage{}
			}
			usage := latest.Status.CostUsage
			usage.TotalCost, accountingErr = addQuotaUsage(usage.TotalCost, cost)
			if accountingErr != nil {
				return false
			}
			usage.ToolCalls, accountingErr = addQuotaUsage(usage.ToolCalls, toolCalls)
			return accountingErr == nil
		}); err != nil {
			return fmt.Errorf("update cost usage: %w", err)
		}
		if accountingErr != nil {
			return fmt.Errorf("update cost usage: %w", accountingErr)
		}
	}
	return nil
}

func (r *CognitiveQuotaReconciler) updateUsageWithRetry(
	ctx context.Context,
	namespace, name string,
	mutate func(*v1alpha1.CognitiveQuota) bool,
) error {
	const maxRetries = 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		raw, err := r.store.Get(ctx, cognitiveQuotaKind, namespace, name)
		if err != nil {
			return err
		}
		var quota v1alpha1.CognitiveQuota
		if err := json.Unmarshal(raw, &quota); err != nil {
			return fmt.Errorf("unmarshal CognitiveQuota: %w", err)
		}
		if !mutate(&quota) {
			return nil
		}
		data, err := json.Marshal(quota)
		if err != nil {
			return fmt.Errorf("marshal CognitiveQuota: %w", err)
		}
		if err := r.store.Update(ctx, cognitiveQuotaKind, namespace, name, data, quota.ObjectMeta.ResourceVersion); err != nil {
			if errors.Is(err, store.ErrConflict) {
				continue
			}
			return err
		}
		return nil
	}
	return store.ErrConflict
}

// CheckTokenQuota validates whether a token operation would exceed budget limits.
func (r *CognitiveQuotaReconciler) CheckTokenQuota(ctx context.Context, namespace string, input, output int64) error {
	total, err := addQuotaUsage(input, output)
	if err != nil {
		return fmt.Errorf("invalid token quota request: %w", err)
	}

	items, err := r.store.List(ctx, cognitiveQuotaKind, store.ListOptions{Namespace: namespace})
	if err != nil {
		return fmt.Errorf("list quotas: %w", err)
	}

	for _, raw := range items {
		var quota v1alpha1.CognitiveQuota
		if err := json.Unmarshal(raw, &quota); err != nil {
			continue
		}
		if quota.Spec.TokenBudget == nil {
			continue
		}

		usage := quota.Status.TokenUsage
		if usage == nil {
			usage = &v1alpha1.QuotaTokenUsage{}
		}

		budget := quota.Spec.TokenBudget
		requestedInput, err := addQuotaUsage(usage.InputTokens, input)
		if err != nil {
			return fmt.Errorf("invalid input token usage: %w", err)
		}
		if budget.MaxInputTokens > 0 && requestedInput > budget.MaxInputTokens {
			return fmt.Errorf("token quota exceeded: input tokens %d + %d > limit %d",
				usage.InputTokens, input, budget.MaxInputTokens)
		}
		requestedOutput, err := addQuotaUsage(usage.OutputTokens, output)
		if err != nil {
			return fmt.Errorf("invalid output token usage: %w", err)
		}
		if budget.MaxOutputTokens > 0 && requestedOutput > budget.MaxOutputTokens {
			return fmt.Errorf("token quota exceeded: output tokens %d + %d > limit %d",
				usage.OutputTokens, output, budget.MaxOutputTokens)
		}
		requestedTotal, err := addQuotaUsage(usage.TotalTokens, total)
		if err != nil {
			return fmt.Errorf("invalid total token usage: %w", err)
		}
		if budget.MaxTotalTokens > 0 && requestedTotal > budget.MaxTotalTokens {
			return fmt.Errorf("token quota exceeded: total tokens %d + %d > limit %d",
				usage.TotalTokens, total, budget.MaxTotalTokens)
		}
	}
	return nil
}

// CheckCostQuota validates whether a cost operation would exceed budget limits.
func (r *CognitiveQuotaReconciler) CheckCostQuota(ctx context.Context, namespace string, cost, toolCalls int64) error {
	if cost < 0 || toolCalls < 0 {
		return fmt.Errorf("cost quota requests cannot be negative")
	}

	items, err := r.store.List(ctx, cognitiveQuotaKind, store.ListOptions{Namespace: namespace})
	if err != nil {
		return fmt.Errorf("list quotas: %w", err)
	}

	for _, raw := range items {
		var quota v1alpha1.CognitiveQuota
		if err := json.Unmarshal(raw, &quota); err != nil {
			continue
		}
		if quota.Spec.CostBudget == nil {
			continue
		}

		usage := quota.Status.CostUsage
		if usage == nil {
			usage = &v1alpha1.QuotaCostUsage{}
		}

		budget := quota.Spec.CostBudget
		requestedCost, err := addQuotaUsage(usage.TotalCost, cost)
		if err != nil {
			return fmt.Errorf("invalid cost usage: %w", err)
		}
		if budget.MaxCostPerPeriod > 0 && (requestedCost > budget.MaxCostPerPeriod || (cost == 0 && usage.TotalCost >= budget.MaxCostPerPeriod)) {
			return fmt.Errorf("cost quota exceeded: cost %d + %d > limit %d",
				usage.TotalCost, cost, budget.MaxCostPerPeriod)
		}
		requestedToolCalls, err := addQuotaUsage(usage.ToolCalls, toolCalls)
		if err != nil {
			return fmt.Errorf("invalid tool call usage: %w", err)
		}
		if budget.MaxToolCallsPerPeriod > 0 && (requestedToolCalls > budget.MaxToolCallsPerPeriod || (toolCalls == 0 && usage.ToolCalls >= budget.MaxToolCallsPerPeriod)) {
			return fmt.Errorf("cost quota exceeded: tool calls %d + %d > limit %d",
				usage.ToolCalls, toolCalls, budget.MaxToolCallsPerPeriod)
		}
	}
	return nil
}

// CheckModelTierQuota validates that a model cost tier has remaining provider
// time in the current quota period.
func (r *CognitiveQuotaReconciler) CheckModelTierQuota(ctx context.Context, namespace, tier string) error {
	if tier == "" {
		return nil
	}
	items, err := r.store.List(ctx, cognitiveQuotaKind, store.ListOptions{Namespace: namespace})
	if err != nil {
		return fmt.Errorf("list quotas: %w", err)
	}
	for _, raw := range items {
		var quota v1alpha1.CognitiveQuota
		if err := json.Unmarshal(raw, &quota); err != nil || quota.Spec.CostBudget == nil {
			continue
		}
		limit := quota.Spec.CostBudget.MaxModelTierSeconds[tier]
		if limit <= 0 {
			continue
		}
		var used int64
		if quota.Status.CostUsage != nil {
			used = quota.Status.CostUsage.ModelTierSeconds[tier]
		}
		if used >= limit {
			return fmt.Errorf("model-tier quota exceeded: tier %q used %d >= limit %d", tier, used, limit)
		}
	}
	return nil
}

func addQuotaUsage(current, additional int64) (int64, error) {
	if current < 0 || additional < 0 {
		return 0, fmt.Errorf("usage values cannot be negative")
	}
	if additional > math.MaxInt64-current {
		return 0, fmt.Errorf("usage accounting overflow")
	}
	return current + additional, nil
}

func addQuotaDelta(left, right int64) (int64, error) {
	if right > 0 && left > math.MaxInt64-right {
		return 0, fmt.Errorf("usage accounting overflow")
	}
	if right < 0 && left < math.MinInt64-right {
		return 0, fmt.Errorf("usage accounting underflow")
	}
	return left + right, nil
}

func applyQuotaDelta(current, delta int64) (int64, error) {
	if current < 0 {
		return 0, fmt.Errorf("usage values cannot be negative")
	}
	updated, err := addQuotaDelta(current, delta)
	if err != nil {
		return 0, err
	}
	if updated < 0 {
		return 0, fmt.Errorf("usage accounting cannot become negative")
	}
	return updated, nil
}
