// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package admission

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
)

func TestTrustProfileValidator(t *testing.T) {
	ctx := context.Background()
	ms := newMemStore()
	for name, level := range map[string]int{"supervised": 10, "assisted": 50, "autonomous": 90} {
		class := v1alpha1.AutonomyClass{ObjectMeta: v1alpha1.ObjectMeta{Name: name, Namespace: "system"}, Spec: v1alpha1.AutonomyClassSpec{Level: level}}
		data, _ := json.Marshal(class)
		if err := ms.Create(ctx, "AutonomyClass", "system", name, data); err != nil {
			t.Fatal(err)
		}
	}
	agent := v1alpha1.Agent{ObjectMeta: v1alpha1.ObjectMeta{Name: "agent-a", Namespace: "team-a", UID: "agent-incarnation-a"}}
	agentData, _ := json.Marshal(agent)
	if err := ms.Create(ctx, "Agent", "team-a", agent.Name, agentData); err != nil {
		t.Fatal(err)
	}
	validator := NewTrustProfileValidator(ms)
	valid := v1alpha1.TrustProfile{Spec: v1alpha1.TrustProfileSpec{
		AgentRef: v1alpha1.LocalObjectReference{Name: "agent-a"}, AgentIncarnationUID: agent.UID, InitialScore: 50, InitialClass: "supervised", MaxClass: "autonomous", EvaluationInterval: "1h",
		ScoringWeights:  &v1alpha1.ScoringWeights{TaskCompletionRate: .25, PolicyCompliance: .30, ApprovalAlignmentRate: .20, CostEfficiency: .10, ToolUsageAccuracy: .15},
		PromotionPolicy: &v1alpha1.PromotionPolicy{MinActions: 50, StabilizationWindowDays: 7, Thresholds: []v1alpha1.PromotionThreshold{{TargetClass: "assisted", MinScore: 60}, {TargetClass: "autonomous", MinScore: 90, RequireReview: true}}},
	}}
	validate := func(profile v1alpha1.TrustProfile) *Response {
		data, _ := json.Marshal(profile)
		return validator.Validate(ctx, &Request{Operation: OperationCreate, Kind: "TrustProfile", Namespace: "team-a", Object: data})
	}
	if response := validate(valid); !response.Allowed {
		t.Fatalf("valid profile denied: %s", response.Reason)
	}

	tests := []struct {
		name, reason string
		mutate       func(*v1alpha1.TrustProfile)
	}{
		{name: "short interval", reason: "at least 1m", mutate: func(p *v1alpha1.TrustProfile) { p.Spec.EvaluationInterval = "30s" }},
		{name: "weights do not sum", reason: "sum to 1.0", mutate: func(p *v1alpha1.TrustProfile) { p.Spec.ScoringWeights.ToolUsageAccuracy = 0 }},
		{name: "negative success weight", reason: "successWeight must be between 0 and 100", mutate: func(p *v1alpha1.TrustProfile) { p.Spec.ScoringWeights.SuccessWeight = -1 }},
		{name: "excessive success weight", reason: "successWeight must be between 0 and 100", mutate: func(p *v1alpha1.TrustProfile) { p.Spec.ScoringWeights.SuccessWeight = 101 }},
		{name: "positive failure weight", reason: "failureWeight must be between -100 and 0", mutate: func(p *v1alpha1.TrustProfile) { p.Spec.ScoringWeights.FailureWeight = 1 }},
		{name: "excessive failure weight", reason: "failureWeight must be between -100 and 0", mutate: func(p *v1alpha1.TrustProfile) { p.Spec.ScoringWeights.FailureWeight = -101 }},
		{name: "positive violation weight", reason: "policyViolationWeight must be between -100 and 0", mutate: func(p *v1alpha1.TrustProfile) { p.Spec.ScoringWeights.PolicyViolationWeight = 1 }},
		{name: "excessive violation weight", reason: "policyViolationWeight must be between -100 and 0", mutate: func(p *v1alpha1.TrustProfile) { p.Spec.ScoringWeights.PolicyViolationWeight = -101 }},
		{name: "inverted class ceiling", reason: "greater than or equal", mutate: func(p *v1alpha1.TrustProfile) { p.Spec.InitialClass, p.Spec.MaxClass = "autonomous", "assisted" }},
		{name: "missing threshold class", reason: "not found", mutate: func(p *v1alpha1.TrustProfile) { p.Spec.PromotionPolicy.Thresholds[0].TargetClass = "missing" }},
		{name: "threshold exceeds ceiling", reason: "exceeds maxClass", mutate: func(p *v1alpha1.TrustProfile) { p.Spec.MaxClass = "assisted" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			profile := valid
			weights := *valid.Spec.ScoringWeights
			profile.Spec.ScoringWeights = &weights
			policy := *valid.Spec.PromotionPolicy
			policy.Thresholds = append([]v1alpha1.PromotionThreshold(nil), valid.Spec.PromotionPolicy.Thresholds...)
			profile.Spec.PromotionPolicy = &policy
			test.mutate(&profile)
			response := validate(profile)
			if response.Allowed || !strings.Contains(response.Reason, test.reason) {
				t.Fatalf("response = %+v, want denial containing %q", response, test.reason)
			}
		})
	}
}

func TestTrustProfileValidatorAllowsMetadataOnlyDeletionCleanupWithoutLiveAgent(t *testing.T) {
	deletedAt := time.Now()
	oldProfile := v1alpha1.TrustProfile{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "profile-a", Namespace: "team-a", DeletionTimestamp: &deletedAt, Finalizers: []string{"kognition.dev/trustprofile-index"}},
		Spec:       v1alpha1.TrustProfileSpec{AgentRef: v1alpha1.LocalObjectReference{Name: "deleted-agent"}, AgentIncarnationUID: "deleted-agent-uid", InitialScore: 50},
	}
	profile := oldProfile
	profile.Finalizers = nil
	oldData, _ := json.Marshal(oldProfile)
	data, _ := json.Marshal(profile)
	validator := NewTrustProfileValidator(newMemStore())

	response := validator.Validate(context.Background(), &Request{Operation: OperationUpdate, Kind: "TrustProfile", Namespace: "team-a", Object: data, OldObject: oldData})
	if !response.Allowed {
		t.Fatalf("metadata-only deletion cleanup denied: %s", response.Reason)
	}

	profile.Spec.InitialScore = 51
	data, _ = json.Marshal(profile)
	response = validator.Validate(context.Background(), &Request{Operation: OperationUpdate, Kind: "TrustProfile", Namespace: "team-a", Object: data, OldObject: oldData})
	if response.Allowed || !strings.Contains(response.Reason, "spec cannot change during deletion") {
		t.Fatalf("response = %+v, want deletion-time spec mutation denial", response)
	}
}
