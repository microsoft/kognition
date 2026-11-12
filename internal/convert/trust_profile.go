// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package convert

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

func TrustProfileToK8s(in *api.TrustProfile) *k8s.TrustProfile {
	if in == nil {
		return nil
	}
	out := &k8s.TrustProfile{
		TypeMeta:   metav1.TypeMeta{Kind: "TrustProfile", APIVersion: "kognition.dev/v1alpha1"},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
		Spec: k8s.TrustProfileSpec{
			AgentRef: localRefToK8s(in.Spec.AgentRef.Name), AgentIncarnationUID: in.Spec.AgentIncarnationUID,
			InitialScore: in.Spec.InitialScore,
			InitialClass: in.Spec.InitialClass, MaxClass: in.Spec.MaxClass,
			EvaluationInterval: in.Spec.EvaluationInterval,
		},
		Status: k8s.TrustProfileStatus{
			CurrentScore: in.Status.CurrentScore, CurrentTier: in.Status.CurrentTier,
			CurrentClass: in.Status.CurrentClass, TotalEvaluations: in.Status.TotalEvaluations,
			LastEvaluationTime: in.Status.LastEvaluationTime, AuditedActions: in.Status.AuditedActions,
			ThresholdMetSince: in.Status.ThresholdMetSince, ProbationUntil: in.Status.ProbationUntil,
			ThresholdClass: in.Status.ThresholdClass,
		},
	}
	if in.Spec.ScoringWeights != nil {
		v := k8s.ScoringWeights(*in.Spec.ScoringWeights)
		out.Spec.ScoringWeights = &v
	}
	if in.Spec.PromotionPolicy != nil {
		v := k8s.PromotionPolicy{RequireReview: in.Spec.PromotionPolicy.RequireReview, MinActions: in.Spec.PromotionPolicy.MinActions, MinScoreDuration: in.Spec.PromotionPolicy.MinScoreDuration, StabilizationWindowDays: in.Spec.PromotionPolicy.StabilizationWindowDays}
		for _, threshold := range in.Spec.PromotionPolicy.Thresholds {
			v.Thresholds = append(v.Thresholds, k8s.PromotionThreshold(threshold))
		}
		out.Spec.PromotionPolicy = &v
	}
	if in.Spec.DemotionPolicy != nil {
		v := k8s.DemotionPolicy(*in.Spec.DemotionPolicy)
		out.Spec.DemotionPolicy = &v
	}
	if in.Status.ApprovalStats != nil {
		v := k8s.ApprovalStats(*in.Status.ApprovalStats)
		out.Status.ApprovalStats = &v
	}
	if in.Status.PendingTrustChange != nil {
		value := k8s.TrustChangeAudit(*in.Status.PendingTrustChange)
		out.Status.PendingTrustChange = &value
	}
	out.Status.ProcessedEventIDs = append([]string(nil), in.Status.ProcessedEventIDs...)
	out.Status.ScoreHistory = make([]k8s.ScoreEntry, len(in.Status.ScoreHistory))
	for i := range in.Status.ScoreHistory {
		entry := in.Status.ScoreHistory[i]
		out.Status.ScoreHistory[i] = k8s.ScoreEntry{Timestamp: entry.Timestamp, Score: entry.Score, Reason: entry.Reason, Class: entry.Class}
		if entry.Dimensions != nil {
			value := k8s.TrustScoreDimensions(*entry.Dimensions)
			out.Status.ScoreHistory[i].Dimensions = &value
		}
	}
	out.Status.Conditions = make([]k8s.StatusCondition, len(in.Status.Conditions))
	for i := range in.Status.Conditions {
		out.Status.Conditions[i] = k8s.StatusCondition(in.Status.Conditions[i])
	}
	return out
}

func TrustProfileFromK8s(in *k8s.TrustProfile) *api.TrustProfile {
	if in == nil {
		return nil
	}
	out := &api.TrustProfile{
		TypeMeta:   api.TypeMeta{Kind: in.Kind, APIVersion: in.APIVersion},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec: api.TrustProfileSpec{
			AgentRef: api.LocalObjectReference{Name: in.Spec.AgentRef.Name}, AgentIncarnationUID: in.Spec.AgentIncarnationUID,
			InitialScore: in.Spec.InitialScore,
			InitialClass: in.Spec.InitialClass, MaxClass: in.Spec.MaxClass,
			EvaluationInterval: in.Spec.EvaluationInterval,
		},
		Status: api.TrustProfileStatus{
			CurrentScore: in.Status.CurrentScore, CurrentTier: in.Status.CurrentTier,
			CurrentClass: in.Status.CurrentClass, TotalEvaluations: in.Status.TotalEvaluations,
			LastEvaluationTime: in.Status.LastEvaluationTime, AuditedActions: in.Status.AuditedActions,
			ThresholdMetSince: in.Status.ThresholdMetSince, ProbationUntil: in.Status.ProbationUntil,
			ThresholdClass: in.Status.ThresholdClass,
		},
	}
	if in.Spec.ScoringWeights != nil {
		v := api.ScoringWeights(*in.Spec.ScoringWeights)
		out.Spec.ScoringWeights = &v
	}
	if in.Spec.PromotionPolicy != nil {
		v := api.PromotionPolicy{RequireReview: in.Spec.PromotionPolicy.RequireReview, MinActions: in.Spec.PromotionPolicy.MinActions, MinScoreDuration: in.Spec.PromotionPolicy.MinScoreDuration, StabilizationWindowDays: in.Spec.PromotionPolicy.StabilizationWindowDays}
		for _, threshold := range in.Spec.PromotionPolicy.Thresholds {
			v.Thresholds = append(v.Thresholds, api.PromotionThreshold(threshold))
		}
		out.Spec.PromotionPolicy = &v
	}
	if in.Spec.DemotionPolicy != nil {
		v := api.DemotionPolicy(*in.Spec.DemotionPolicy)
		out.Spec.DemotionPolicy = &v
	}
	if in.Status.ApprovalStats != nil {
		v := api.ApprovalStats(*in.Status.ApprovalStats)
		out.Status.ApprovalStats = &v
	}
	if in.Status.PendingTrustChange != nil {
		value := api.TrustChangeAudit(*in.Status.PendingTrustChange)
		out.Status.PendingTrustChange = &value
	}
	out.Status.ProcessedEventIDs = append([]string(nil), in.Status.ProcessedEventIDs...)
	out.Status.ScoreHistory = make([]api.ScoreEntry, len(in.Status.ScoreHistory))
	for i := range in.Status.ScoreHistory {
		entry := in.Status.ScoreHistory[i]
		out.Status.ScoreHistory[i] = api.ScoreEntry{Timestamp: entry.Timestamp, Score: entry.Score, Reason: entry.Reason, Class: entry.Class}
		if entry.Dimensions != nil {
			value := api.TrustScoreDimensions(*entry.Dimensions)
			out.Status.ScoreHistory[i].Dimensions = &value
		}
	}
	out.Status.Conditions = make([]api.StatusCondition, len(in.Status.Conditions))
	for i := range in.Status.Conditions {
		out.Status.Conditions[i] = api.StatusCondition(in.Status.Conditions[i])
	}
	return out
}
