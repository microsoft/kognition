// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package convert

import (
	"encoding/json"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

func SolutionToK8s(in *api.Solution) *k8s.Solution {
	if in == nil {
		return nil
	}
	out := &k8s.Solution{
		TypeMeta:   metav1.TypeMeta{Kind: "Solution", APIVersion: api.APIVersion},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
		Spec:       k8s.SolutionSpec{Goal: in.Spec.Goal, Constraints: append([]string(nil), in.Spec.Constraints...), Preferences: cloneStringsMap(in.Spec.Preferences)},
		Status:     k8s.SolutionStatus{Phase: string(in.Status.Phase), Conditions: conditionsToK8s(in.Status.Conditions), ActiveRevision: in.Status.ActiveRevision, CandidateRevision: in.Status.CandidateRevision},
	}
	if in.Spec.PromotionGates != nil {
		out.Spec.PromotionGates = make([]k8s.SolutionPromotionGate, len(in.Spec.PromotionGates))
		for i, gate := range in.Spec.PromotionGates {
			out.Spec.PromotionGates[i] = k8s.SolutionPromotionGate{EvaluationRef: k8s.LocalObjectReference{Name: gate.EvaluationRef.Name}}
		}
	}
	if in.Spec.Seed != nil {
		out.Spec.Seed = new(int64)
		*out.Spec.Seed = *in.Spec.Seed
	}
	if in.Spec.ModelRef != nil {
		out.Spec.ModelRef = &k8s.LocalObjectReference{Name: in.Spec.ModelRef.Name}
	}
	if in.Spec.Target != nil {
		out.Spec.Target = &k8s.SolutionTarget{Kind: in.Spec.Target.Kind, Ref: k8s.LocalObjectReference{Name: in.Spec.Target.Ref.Name}}
	}
	if in.Spec.Overrides != nil {
		out.Spec.Overrides = &k8s.SolutionOverrides{Hard: cloneInt64Map(in.Spec.Overrides.Hard)}
		if in.Spec.Overrides.TokenBudget != nil {
			out.Spec.Overrides.TokenBudget = &k8s.TokenBudget{MaxInputTokens: in.Spec.Overrides.TokenBudget.MaxInputTokens, MaxOutputTokens: in.Spec.Overrides.TokenBudget.MaxOutputTokens, MaxTotalTokens: in.Spec.Overrides.TokenBudget.MaxTotalTokens}
		}
	}
	out.Status.CompiledResources = make([]k8s.CompiledResource, len(in.Status.CompiledResources))
	for i, resource := range in.Status.CompiledResources {
		out.Status.CompiledResources[i] = k8s.CompiledResource{Kind: resource.Kind, Name: resource.Name, Namespace: resource.Namespace, Manifest: append(json.RawMessage(nil), resource.Manifest...)}
	}
	out.Status.GeneratedResources = make([]k8s.GeneratedResource, len(in.Status.GeneratedResources))
	for i, resource := range in.Status.GeneratedResources {
		out.Status.GeneratedResources[i] = k8s.GeneratedResource{Kind: resource.Kind, Name: resource.Name}
	}
	return out
}

func SolutionFromK8s(in *k8s.Solution) *api.Solution {
	if in == nil {
		return nil
	}
	out := &api.Solution{
		TypeMeta:   api.TypeMeta{Kind: in.Kind, APIVersion: in.APIVersion},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec:       api.SolutionSpec{Goal: in.Spec.Goal, Constraints: append([]string(nil), in.Spec.Constraints...), Preferences: cloneStringsMap(in.Spec.Preferences)},
		Status:     api.SolutionStatus{Phase: api.Phase(in.Status.Phase), Conditions: conditionsFromK8s(in.Status.Conditions), ActiveRevision: in.Status.ActiveRevision, CandidateRevision: in.Status.CandidateRevision},
	}
	if in.Spec.PromotionGates != nil {
		out.Spec.PromotionGates = make([]api.SolutionPromotionGate, len(in.Spec.PromotionGates))
		for i, gate := range in.Spec.PromotionGates {
			out.Spec.PromotionGates[i] = api.SolutionPromotionGate{EvaluationRef: api.LocalObjectReference{Name: gate.EvaluationRef.Name}}
		}
	}
	if in.Spec.Seed != nil {
		out.Spec.Seed = new(int64)
		*out.Spec.Seed = *in.Spec.Seed
	}
	if in.Spec.ModelRef != nil {
		out.Spec.ModelRef = &api.LocalObjectReference{Name: in.Spec.ModelRef.Name}
	}
	if in.Spec.Target != nil {
		out.Spec.Target = &api.SolutionTarget{Kind: in.Spec.Target.Kind, Ref: api.LocalObjectReference{Name: in.Spec.Target.Ref.Name}}
	}
	if in.Spec.Overrides != nil {
		out.Spec.Overrides = &api.SolutionOverrides{Hard: cloneInt64Map(in.Spec.Overrides.Hard)}
		if in.Spec.Overrides.TokenBudget != nil {
			out.Spec.Overrides.TokenBudget = &api.TokenBudget{MaxInputTokens: in.Spec.Overrides.TokenBudget.MaxInputTokens, MaxOutputTokens: in.Spec.Overrides.TokenBudget.MaxOutputTokens, MaxTotalTokens: in.Spec.Overrides.TokenBudget.MaxTotalTokens}
		}
	}
	out.Status.CompiledResources = make([]api.CompiledResource, len(in.Status.CompiledResources))
	for i, resource := range in.Status.CompiledResources {
		out.Status.CompiledResources[i] = api.CompiledResource{Kind: resource.Kind, Name: resource.Name, Namespace: resource.Namespace, Manifest: append(json.RawMessage(nil), resource.Manifest...)}
	}
	out.Status.GeneratedResources = make([]api.GeneratedResource, len(in.Status.GeneratedResources))
	for i, resource := range in.Status.GeneratedResources {
		out.Status.GeneratedResources[i] = api.GeneratedResource{Kind: resource.Kind, Name: resource.Name}
	}
	return out
}

func cloneInt64Map(in map[string]int64) map[string]int64 {
	if in == nil {
		return nil
	}
	out := make(map[string]int64, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
