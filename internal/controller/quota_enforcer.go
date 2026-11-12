// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

// ErrQuotaExhausted is returned when workspace-level quota is fully consumed.
var ErrQuotaExhausted = errors.New("quota exhausted")

// ErrBudgetExceeded is returned when a per-agent session budget is exceeded.
var ErrBudgetExceeded = errors.New("budget exceeded")

const (
	tokenReservationMaxAge    = 24 * time.Hour
	tokenReservationReapLimit = 100
)

// Reservation tracks pre-reserved tokens for a session (hybrid accounting).
type Reservation struct {
	SessionID      string
	Namespace      string
	ReservedInput  int64
	ReservedOutput int64
}

// QuotaEnforcer provides multi-level quota enforcement:
// scheduler-time (T-0113), runtime-time (T-0114), policy-time (T-0115),
// and hybrid token accounting (T-0116).
type QuotaEnforcer struct {
	store  store.Store
	quota  *CognitiveQuotaReconciler
	logger *zap.Logger

	mu           sync.Mutex
	reservations map[string]*Reservation // sessionID -> reservation
}

// NewQuotaEnforcer creates a new QuotaEnforcer.
func NewQuotaEnforcer(s store.Store, quota *CognitiveQuotaReconciler, logger *zap.Logger) *QuotaEnforcer {
	return &QuotaEnforcer{
		store:        s,
		quota:        quota,
		logger:       logger.With(zap.String("component", "quota-enforcer")),
		reservations: make(map[string]*Reservation),
	}
}

// CheckSchedulerQuota validates workspace-level quota before session creation (T-0113).
// Returns ErrQuotaExhausted if the workspace has no remaining capacity.
func (e *QuotaEnforcer) CheckSchedulerQuota(ctx context.Context, namespace string) error {
	// Check session count quota.
	if err := e.quota.CheckQuota(ctx, namespace, "sessions", 1); err != nil {
		return fmt.Errorf("%w: %w", ErrQuotaExhausted, err)
	}

	// Check token budget has remaining capacity (zero additional = just check if already exceeded).
	if err := e.quota.CheckTokenQuota(ctx, namespace, 0, 0); err != nil {
		return fmt.Errorf("%w: %w", ErrQuotaExhausted, err)
	}

	// Check cost budget has remaining capacity.
	if err := e.quota.CheckCostQuota(ctx, namespace, 0, 0); err != nil {
		return fmt.Errorf("%w: %w", ErrQuotaExhausted, err)
	}

	return nil
}

// CheckModelQuota validates monetary and model-tier capacity immediately
// before provider dispatch.
func (e *QuotaEnforcer) CheckModelQuota(ctx context.Context, namespace, tier string) error {
	if err := e.quota.CheckCostQuota(ctx, namespace, 0, 0); err != nil {
		return fmt.Errorf("%w: %w", ErrQuotaExhausted, err)
	}
	if err := e.quota.CheckModelTierQuota(ctx, namespace, tier); err != nil {
		return fmt.Errorf("%w: %w", ErrQuotaExhausted, err)
	}
	return nil
}

// ReserveTokens pre-allocates worst-case token ceiling for a session (T-0116).
// The reserved tokens are immediately counted against workspace quota.
// Call Settle when the session completes to refund the unused portion.
func (e *QuotaEnforcer) ReserveTokens(ctx context.Context, namespace, sessionID string, inputCeiling, outputCeiling int64) error {
	if inputCeiling < 0 || outputCeiling < 0 {
		return fmt.Errorf("token reservation ceilings must be non-negative")
	}
	if reservationStore, ok := e.store.(store.TokenReservationStore); ok {
		if reaper, ok := e.store.(store.ExpiredTokenReservationStore); ok {
			reaped, err := reaper.ReapExpiredTokenReservations(
				ctx, nowFunc().Add(-tokenReservationMaxAge), tokenReservationReapLimit,
			)
			if err != nil {
				return fmt.Errorf("reap expired token reservations: %w", err)
			}
			if reaped > 0 {
				e.logger.Info("expired token reservations reclaimed", zap.Int("count", reaped))
			}
		}
		_, err := reservationStore.CreateTokenReservation(ctx, store.TokenReservation{
			ID:             sessionID,
			Namespace:      namespace,
			ReservedInput:  inputCeiling,
			ReservedOutput: outputCeiling,
		})
		if errors.Is(err, store.ErrQuotaExceeded) {
			return fmt.Errorf("%w: %w", ErrQuotaExhausted, err)
		}
		if errors.Is(err, store.ErrConflict) {
			return fmt.Errorf("session %q already has a different token reservation", sessionID)
		}
		if err != nil {
			return fmt.Errorf("reserve tokens: %w", err)
		}
		e.logger.Debug("tokens durably reserved",
			zap.String("session", sessionID),
			zap.Int64("inputCeiling", inputCeiling),
			zap.Int64("outputCeiling", outputCeiling))
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if existing, ok := e.reservations[sessionID]; ok {
		if existing.Namespace == namespace &&
			existing.ReservedInput == inputCeiling &&
			existing.ReservedOutput == outputCeiling {
			return nil
		}
		return fmt.Errorf("session %q already has a different token reservation", sessionID)
	}

	// Validate that the reservation won't exceed quota.
	if err := e.quota.CheckTokenQuota(ctx, namespace, inputCeiling, outputCeiling); err != nil {
		return fmt.Errorf("%w: %w", ErrQuotaExhausted, err)
	}

	// Charge the ceiling against workspace quota.
	if err := e.quota.IncrementTokenUsage(ctx, namespace, inputCeiling, outputCeiling); err != nil {
		return fmt.Errorf("reserve tokens: %w", err)
	}

	// Track the reservation for later settlement.
	e.reservations[sessionID] = &Reservation{
		SessionID:      sessionID,
		Namespace:      namespace,
		ReservedInput:  inputCeiling,
		ReservedOutput: outputCeiling,
	}

	e.logger.Debug("tokens reserved",
		zap.String("session", sessionID),
		zap.Int64("inputCeiling", inputCeiling),
		zap.Int64("outputCeiling", outputCeiling))

	return nil
}

// CheckRuntimeBudget checks whether a session has exceeded its per-agent token budget (T-0114).
// Returns ErrBudgetExceeded if the agent's per-session limit (from CognitiveLimitRange) is breached.
func (e *QuotaEnforcer) CheckRuntimeBudget(ctx context.Context, namespace, agentName string, usedTokens int64) error {
	limit, err := e.getAgentTokenLimit(ctx, namespace)
	if err != nil {
		return err
	}

	if limit > 0 && usedTokens >= limit {
		e.logger.Warn("runtime budget exceeded",
			zap.String("namespace", namespace),
			zap.String("agent", agentName),
			zap.Int64("used", usedTokens),
			zap.Int64("limit", limit))
		return fmt.Errorf("%w: agent %s used %d tokens, limit %d",
			ErrBudgetExceeded, agentName, usedTokens, limit)
	}

	return nil
}

// CheckCallBudget evaluates per-agent limits for a single tool/model call (T-0115).
// Returns ErrBudgetExceeded if the call would breach the per-agent session limit.
func (e *QuotaEnforcer) CheckCallBudget(ctx context.Context, namespace, agentName string, currentUsed, callTokens int64) error {
	limit, err := e.getAgentTokenLimit(ctx, namespace)
	if err != nil {
		return err
	}

	if limit > 0 && currentUsed+callTokens > limit {
		e.logger.Warn("call budget would exceed limit",
			zap.String("namespace", namespace),
			zap.String("agent", agentName),
			zap.Int64("current", currentUsed),
			zap.Int64("call", callTokens),
			zap.Int64("limit", limit))
		return fmt.Errorf("%w: agent %s call would use %d + %d tokens, limit %d",
			ErrBudgetExceeded, agentName, currentUsed, callTokens, limit)
	}

	// Also verify workspace-level quota isn't exceeded.
	if err := e.quota.CheckTokenQuota(ctx, namespace, callTokens, 0); err != nil {
		return fmt.Errorf("%w: %w", ErrQuotaExhausted, err)
	}

	return nil
}

// ReserveToolCall atomically charges one attempted tool invocation against cost quota.
func (e *QuotaEnforcer) ReserveToolCall(ctx context.Context, namespace string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.quota.CheckCostQuota(ctx, namespace, 0, 1); err != nil {
		return fmt.Errorf("%w: %w", ErrQuotaExhausted, err)
	}
	if err := e.quota.IncrementCostUsage(ctx, namespace, 0, 1); err != nil {
		return fmt.Errorf("reserve tool call: %w", err)
	}
	return nil
}

// Settle finalizes a session by adjusting the reservation to actual usage (T-0116).
// actualInput/actualOutput are the real tokens consumed during the session.
func (e *QuotaEnforcer) Settle(ctx context.Context, sessionID string, actualInput, actualOutput int64) error {
	if actualInput < 0 || actualOutput < 0 {
		return fmt.Errorf("actual token usage must be non-negative")
	}
	if reservationStore, ok := e.store.(store.TokenReservationStore); ok {
		found, err := reservationStore.SettleTokenReservation(ctx, sessionID, actualInput, actualOutput)
		if err != nil {
			return fmt.Errorf("settle tokens: %w", err)
		}
		if found {
			e.logger.Debug("durable token reservation settled",
				zap.String("session", sessionID),
				zap.Int64("actualInput", actualInput),
				zap.Int64("actualOutput", actualOutput))
		}
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	res, ok := e.reservations[sessionID]
	if !ok {
		return nil // No reservation to settle.
	}

	inputDelta := actualInput - res.ReservedInput
	outputDelta := actualOutput - res.ReservedOutput

	if inputDelta == 0 && outputDelta == 0 {
		delete(e.reservations, sessionID)
		return nil // Session used all reserved tokens.
	}

	if err := e.quota.IncrementTokenUsage(ctx, res.Namespace, inputDelta, outputDelta); err != nil {
		return fmt.Errorf("settle tokens: %w", err)
	}
	delete(e.reservations, sessionID)

	e.logger.Debug("tokens settled",
		zap.String("session", sessionID),
		zap.Int64("inputDelta", inputDelta),
		zap.Int64("outputDelta", outputDelta))

	return nil
}

// getAgentTokenLimit retrieves the per-session token limit from CognitiveLimitRange.
func (e *QuotaEnforcer) getAgentTokenLimit(ctx context.Context, namespace string) (int64, error) {
	items, err := e.store.List(ctx, "CognitiveLimitRange", store.ListOptions{Namespace: namespace})
	if err != nil {
		return 0, fmt.Errorf("list limit ranges: %w", err)
	}

	var maxLimit int64
	for _, raw := range items {
		var lr v1alpha1.CognitiveLimitRange
		if err := json.Unmarshal(raw, &lr); err != nil {
			continue
		}
		for _, limit := range lr.Spec.Limits {
			if limit.TokensPerSession > maxLimit {
				maxLimit = limit.TokensPerSession
			}
		}
	}

	return maxLimit, nil
}
