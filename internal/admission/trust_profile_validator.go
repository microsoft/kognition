// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/store"
)

// TrustProfileValidator enforces the cross-resource scoring contract.
type TrustProfileValidator struct{ store store.Store }

func NewTrustProfileValidator(s store.Store) *TrustProfileValidator {
	return &TrustProfileValidator{store: s}
}
func (*TrustProfileValidator) Name() string { return "trust-profile-validator" }
func (*TrustProfileValidator) Handles(kind string, op Operation) bool {
	return kind == "TrustProfile" && (op == OperationCreate || op == OperationUpdate)
}

func (v *TrustProfileValidator) Validate(ctx context.Context, req *Request) *Response {
	var profile v1alpha1.TrustProfile
	if err := json.Unmarshal(req.Object, &profile); err != nil {
		return &Response{Allowed: false, Reason: "invalid TrustProfile: " + err.Error()}
	}
	if profile.Spec.AgentRef.Name == "" || profile.Spec.AgentIncarnationUID == "" {
		return denyTrustProfile("agentRef.name and agentIncarnationUID are required")
	}
	if req.Operation == OperationUpdate && profile.DeletionTimestamp != nil {
		var oldProfile v1alpha1.TrustProfile
		if len(req.OldObject) == 0 || json.Unmarshal(req.OldObject, &oldProfile) != nil {
			return denyTrustProfile("existing TrustProfile is required to verify deletion cleanup")
		}
		if !reflect.DeepEqual(oldProfile.Spec, profile.Spec) {
			return denyTrustProfile("TrustProfile spec cannot change during deletion")
		}
		return &Response{Allowed: true}
	}
	if response := v.validateAgentIncarnation(ctx, req, &profile); response != nil {
		return response
	}
	if profile.Spec.InitialScore < 0 || profile.Spec.InitialScore > 100 {
		return denyTrustProfile("initialScore must be between 0 and 100")
	}
	if profile.Spec.EvaluationInterval != "" {
		d, err := time.ParseDuration(profile.Spec.EvaluationInterval)
		if err != nil || d < time.Minute {
			return denyTrustProfile("evaluationInterval must be a valid duration of at least 1m")
		}
	}
	if weights := profile.Spec.ScoringWeights; weights != nil {
		sum := weights.TaskCompletionRate + weights.PolicyCompliance + weights.ApprovalAlignmentRate + weights.CostEfficiency + weights.ToolUsageAccuracy
		if sum > 0 && math.Abs(sum-1) > .01 {
			return denyTrustProfile("scoringWeights dimensions must sum to 1.0 (±0.01)")
		}
		for _, value := range []float64{weights.TaskCompletionRate, weights.PolicyCompliance, weights.ApprovalAlignmentRate, weights.CostEfficiency, weights.ToolUsageAccuracy} {
			if value < 0 || value > 1 {
				return denyTrustProfile("scoringWeights dimensions must be between 0 and 1")
			}
		}
		if weights.SuccessWeight < 0 || weights.SuccessWeight > 100 {
			return denyTrustProfile("scoringWeights.successWeight must be between 0 and 100")
		}
		if weights.FailureWeight < -100 || weights.FailureWeight > 0 {
			return denyTrustProfile("scoringWeights.failureWeight must be between -100 and 0")
		}
		if weights.PolicyViolationWeight < -100 || weights.PolicyViolationWeight > 0 {
			return denyTrustProfile("scoringWeights.policyViolationWeight must be between -100 and 0")
		}
	}
	initialLevel, response := v.classLevel(ctx, req.Namespace, profile.Spec.InitialClass)
	if response != nil {
		return response
	}
	maxLevel, response := v.classLevel(ctx, req.Namespace, profile.Spec.MaxClass)
	if response != nil {
		return response
	}
	if profile.Spec.InitialClass != "" && profile.Spec.MaxClass != "" && maxLevel < initialLevel {
		return denyTrustProfile("maxClass level must be greater than or equal to initialClass level")
	}
	if policy := profile.Spec.PromotionPolicy; policy != nil {
		if policy.MinActions < 0 || policy.StabilizationWindowDays < 0 {
			return denyTrustProfile("promotion policy durations and action counts must not be negative")
		}
		if policy.MinScoreDuration != "" {
			if d, err := time.ParseDuration(policy.MinScoreDuration); err != nil || d < 0 {
				return denyTrustProfile("minScoreDuration must be a valid non-negative duration")
			}
		}
		for _, threshold := range policy.Thresholds {
			if threshold.MinScore < 0 || threshold.MinScore > 100 {
				return denyTrustProfile("promotion threshold minScore must be between 0 and 100")
			}
			if _, response := v.classLevel(ctx, req.Namespace, threshold.TargetClass); response != nil {
				return response
			}
			if profile.Spec.MaxClass != "" {
				level, _ := v.classLevel(ctx, req.Namespace, threshold.TargetClass)
				if level > maxLevel {
					return denyTrustProfile(fmt.Sprintf("promotion targetClass %q exceeds maxClass", threshold.TargetClass))
				}
			}
		}
	}
	if policy := profile.Spec.DemotionPolicy; policy != nil {
		if policy.Mode != "" && policy.Mode != "immediate" && policy.Mode != "probation" {
			return denyTrustProfile("demotionPolicy.mode must be immediate or probation")
		}
		if policy.FreezeDuration != "" {
			if d, err := time.ParseDuration(policy.FreezeDuration); err != nil || d <= 0 {
				return denyTrustProfile("freezeDuration must be a valid positive duration")
			}
		}
		if policy.Mode == "probation" && policy.FreezeDuration == "" {
			return denyTrustProfile("probation demotion requires freezeDuration")
		}
		if policy.FallbackClass != "" {
			fallbackLevel, response := v.classLevel(ctx, req.Namespace, policy.FallbackClass)
			if response != nil {
				return response
			}
			if profile.Spec.InitialClass != "" && fallbackLevel > initialLevel {
				return denyTrustProfile("fallbackClass level must not exceed initialClass level")
			}
		}
	}
	return &Response{Allowed: true}
}

func (v *TrustProfileValidator) validateAgentIncarnation(ctx context.Context, req *Request, profile *v1alpha1.TrustProfile) *Response {
	rawAgent, err := v.store.Get(ctx, "Agent", req.Namespace, profile.Spec.AgentRef.Name)
	if err != nil {
		return denyTrustProfile(fmt.Sprintf("failed to resolve live Agent incarnation: %v", err))
	}
	var agent v1alpha1.Agent
	if err := json.Unmarshal(rawAgent, &agent); err != nil || agent.UID == "" {
		return denyTrustProfile("referenced Agent has no stable incarnation UID")
	}
	if profile.Spec.AgentIncarnationUID != agent.UID {
		return denyTrustProfile("agentIncarnationUID does not match the live Agent")
	}
	if req.Operation == OperationUpdate {
		var oldProfile v1alpha1.TrustProfile
		if len(req.OldObject) == 0 || json.Unmarshal(req.OldObject, &oldProfile) != nil {
			return denyTrustProfile("existing TrustProfile is required to verify immutable agentIncarnationUID")
		}
		if oldProfile.Spec.AgentRef.Name != profile.Spec.AgentRef.Name || oldProfile.Spec.AgentIncarnationUID != profile.Spec.AgentIncarnationUID {
			return denyTrustProfile("agentRef and agentIncarnationUID are immutable")
		}
		return nil
	}
	profiles, err := v.store.List(ctx, "TrustProfile", store.ListOptions{Namespace: req.Namespace})
	if err != nil {
		return denyTrustProfile(fmt.Sprintf("failed to check TrustProfile uniqueness: %v", err))
	}
	for _, raw := range profiles {
		var existing v1alpha1.TrustProfile
		if json.Unmarshal(raw, &existing) != nil {
			return denyTrustProfile("failed to check TrustProfile uniqueness: malformed existing profile")
		}
		if existing.DeletionTimestamp == nil && existing.Spec.AgentIncarnationUID == profile.Spec.AgentIncarnationUID {
			return denyTrustProfile("a live TrustProfile already exists for this Agent incarnation")
		}
	}
	return nil
}

func (v *TrustProfileValidator) classLevel(ctx context.Context, namespace, name string) (int, *Response) {
	if name == "" {
		return 0, nil
	}
	for _, ns := range []string{namespace, "system"} {
		raw, err := v.store.Get(ctx, "AutonomyClass", ns, name)
		if err == nil {
			var class v1alpha1.AutonomyClass
			if json.Unmarshal(raw, &class) != nil {
				return 0, denyTrustProfile(fmt.Sprintf("autonomy class %q is invalid", name))
			}
			return class.Spec.Level, nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return 0, denyTrustProfile(fmt.Sprintf("failed to get autonomy class %q: %v", name, err))
		}
	}
	return 0, denyTrustProfile(fmt.Sprintf("autonomy class %q not found", name))
}

func denyTrustProfile(reason string) *Response { return &Response{Allowed: false, Reason: reason} }
