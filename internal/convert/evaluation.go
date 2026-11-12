// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package convert

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

func EvaluationToK8s(in *api.Evaluation) *k8s.Evaluation {
	if in == nil {
		return nil
	}
	out := &k8s.Evaluation{
		TypeMeta:   metav1.TypeMeta{Kind: "Evaluation", APIVersion: api.APIVersion},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
		Spec: k8s.EvaluationSpec{
			TargetRef:        evaluationTargetRefToK8s(in.Spec.TargetRef),
			DatasetRef:       evaluationDatasetRefToK8s(in.Spec.DatasetRef),
			Graders:          evaluationGradersToK8s(in.Spec.Graders),
			MetricThresholds: evaluationMetricThresholdsToK8s(in.Spec.MetricThresholds),
			Schedule:         evaluationScheduleToK8s(in.Spec.Schedule),
		},
		Status: k8s.EvaluationStatus{
			Phase:              string(in.Status.Phase),
			Conditions:         conditionsToK8s(in.Status.Conditions),
			ObservedGeneration: in.Status.ObservedGeneration,
			LastRunRef:         localObjectReferencePtrToK8s(in.Status.LastRunRef),
			LastRunTime:        timeToMetav1(in.Status.LastRunTime),
			RunHistory:         evaluationRunSummariesToK8s(in.Status.RunHistory),
		},
	}
	return out
}

func EvaluationFromK8s(in *k8s.Evaluation) *api.Evaluation {
	if in == nil {
		return nil
	}
	out := &api.Evaluation{
		TypeMeta:   api.TypeMeta{Kind: in.Kind, APIVersion: in.APIVersion},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec: api.EvaluationSpec{
			TargetRef:        evaluationTargetRefFromK8s(in.Spec.TargetRef),
			DatasetRef:       evaluationDatasetRefFromK8s(in.Spec.DatasetRef),
			Graders:          evaluationGradersFromK8s(in.Spec.Graders),
			MetricThresholds: evaluationMetricThresholdsFromK8s(in.Spec.MetricThresholds),
			Schedule:         evaluationScheduleFromK8s(in.Spec.Schedule),
		},
		Status: api.EvaluationStatus{
			Phase:              api.Phase(in.Status.Phase),
			Conditions:         conditionsFromK8s(in.Status.Conditions),
			ObservedGeneration: in.Status.ObservedGeneration,
			LastRunRef:         localObjectReferencePtrFromK8s(in.Status.LastRunRef),
			LastRunTime:        timeFromMetav1(in.Status.LastRunTime),
			RunHistory:         evaluationRunSummariesFromK8s(in.Status.RunHistory),
		},
	}
	return out
}

func EvaluationRunToK8s(in *api.EvaluationRun) *k8s.EvaluationRun {
	if in == nil {
		return nil
	}
	out := &k8s.EvaluationRun{
		TypeMeta:   metav1.TypeMeta{Kind: "EvaluationRun", APIVersion: api.APIVersion},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
		Spec: k8s.EvaluationRunSpec{
			EvaluationRef:    localObjectReferenceToK8s(in.Spec.EvaluationRef),
			TargetRef:        evaluationTargetRefToK8s(in.Spec.TargetRef),
			DatasetRef:       evaluationDatasetRefToK8s(in.Spec.DatasetRef),
			Graders:          evaluationGradersToK8s(in.Spec.Graders),
			MetricThresholds: evaluationMetricThresholdsToK8s(in.Spec.MetricThresholds),
		},
		Status: k8s.EvaluationRunStatus{
			Phase:          string(in.Status.Phase),
			Conditions:     conditionsToK8s(in.Status.Conditions),
			StartedAt:      timeToMetav1(in.Status.StartedAt),
			CompletedAt:    timeToMetav1(in.Status.CompletedAt),
			DatasetDigest:  in.Status.DatasetDigest,
			CaseCount:      in.Status.CaseCount,
			CompletedCases: in.Status.CompletedCases,
			FailedCases:    in.Status.FailedCases,
			CaseResults:    evaluationCaseResultsToK8s(in.Status.CaseResults),
			TargetJobRef:   localObjectReferencePtrToK8s(in.Status.TargetJobRef),
			TargetOutput:   in.Status.TargetOutput,
			Passed:         in.Status.Passed,
			Degraded:       in.Status.Degraded,
			Metrics:        evaluationMetricResultsToK8s(in.Status.Metrics),
			Message:        in.Status.Message,
		},
	}
	if in.Status.Score != nil {
		out.Status.Score = new(float64)
		*out.Status.Score = *in.Status.Score
	}
	return out
}

func EvaluationRunFromK8s(in *k8s.EvaluationRun) *api.EvaluationRun {
	if in == nil {
		return nil
	}
	out := &api.EvaluationRun{
		TypeMeta:   api.TypeMeta{Kind: in.Kind, APIVersion: in.APIVersion},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec: api.EvaluationRunSpec{
			EvaluationRef:    localObjectReferenceFromK8s(in.Spec.EvaluationRef),
			TargetRef:        evaluationTargetRefFromK8s(in.Spec.TargetRef),
			DatasetRef:       evaluationDatasetRefFromK8s(in.Spec.DatasetRef),
			Graders:          evaluationGradersFromK8s(in.Spec.Graders),
			MetricThresholds: evaluationMetricThresholdsFromK8s(in.Spec.MetricThresholds),
		},
		Status: api.EvaluationRunStatus{
			Phase:          api.Phase(in.Status.Phase),
			Conditions:     conditionsFromK8s(in.Status.Conditions),
			StartedAt:      timeFromMetav1(in.Status.StartedAt),
			CompletedAt:    timeFromMetav1(in.Status.CompletedAt),
			DatasetDigest:  in.Status.DatasetDigest,
			CaseCount:      in.Status.CaseCount,
			CompletedCases: in.Status.CompletedCases,
			FailedCases:    in.Status.FailedCases,
			CaseResults:    evaluationCaseResultsFromK8s(in.Status.CaseResults),
			TargetJobRef:   localObjectReferencePtrFromK8s(in.Status.TargetJobRef),
			TargetOutput:   in.Status.TargetOutput,
			Passed:         in.Status.Passed,
			Degraded:       in.Status.Degraded,
			Metrics:        evaluationMetricResultsFromK8s(in.Status.Metrics),
			Message:        in.Status.Message,
		},
	}
	if in.Status.Score != nil {
		out.Status.Score = new(float64)
		*out.Status.Score = *in.Status.Score
	}
	return out
}

func EvaluationDatasetToK8s(in *api.EvaluationDataset) *k8s.EvaluationDataset {
	if in == nil {
		return nil
	}
	out := &k8s.EvaluationDataset{
		TypeMeta:   metav1.TypeMeta{Kind: "EvaluationDataset", APIVersion: api.APIVersion},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
		Spec: k8s.EvaluationDatasetSpec{
			Version: in.Spec.Version,
			Cases:   evaluationDatasetCasesToK8s(in.Spec.Cases),
		},
		Status: k8s.EvaluationDatasetStatus{
			Phase:              string(in.Status.Phase),
			Conditions:         conditionsToK8s(in.Status.Conditions),
			ObservedGeneration: in.Status.ObservedGeneration,
			CaseCount:          in.Status.CaseCount,
			Digest:             in.Status.Digest,
			LastValidatedAt:    timeToMetav1(in.Status.LastValidatedAt),
			Message:            in.Status.Message,
		},
	}
	return out
}

func EvaluationDatasetFromK8s(in *k8s.EvaluationDataset) *api.EvaluationDataset {
	if in == nil {
		return nil
	}
	out := &api.EvaluationDataset{
		TypeMeta:   api.TypeMeta{Kind: in.Kind, APIVersion: in.APIVersion},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec: api.EvaluationDatasetSpec{
			Version: in.Spec.Version,
			Cases:   evaluationDatasetCasesFromK8s(in.Spec.Cases),
		},
		Status: api.EvaluationDatasetStatus{
			Phase:              api.Phase(in.Status.Phase),
			Conditions:         conditionsFromK8s(in.Status.Conditions),
			ObservedGeneration: in.Status.ObservedGeneration,
			CaseCount:          in.Status.CaseCount,
			Digest:             in.Status.Digest,
			LastValidatedAt:    timeFromMetav1(in.Status.LastValidatedAt),
			Message:            in.Status.Message,
		},
	}
	return out
}

func evaluationTargetRefToK8s(in api.EvaluationTargetRef) k8s.EvaluationTargetRef {
	out := k8s.EvaluationTargetRef{Kind: in.Kind, Name: in.Name, Namespace: in.Namespace}
	if in.Revision != nil {
		out.Revision = new(int64)
		*out.Revision = *in.Revision
	}
	return out
}

func evaluationTargetRefFromK8s(in k8s.EvaluationTargetRef) api.EvaluationTargetRef {
	out := api.EvaluationTargetRef{Kind: in.Kind, Name: in.Name, Namespace: in.Namespace}
	if in.Revision != nil {
		out.Revision = new(int64)
		*out.Revision = *in.Revision
	}
	return out
}

func evaluationDatasetRefToK8s(in api.EvaluationDatasetRef) k8s.EvaluationDatasetRef {
	return k8s.EvaluationDatasetRef{Name: in.Name, Namespace: in.Namespace, Version: in.Version}
}

func evaluationDatasetRefFromK8s(in k8s.EvaluationDatasetRef) api.EvaluationDatasetRef {
	return api.EvaluationDatasetRef{Name: in.Name, Namespace: in.Namespace, Version: in.Version}
}

func localObjectReferenceToK8s(in api.LocalObjectReference) k8s.LocalObjectReference {
	return k8s.LocalObjectReference{Name: in.Name}
}

func localObjectReferenceFromK8s(in k8s.LocalObjectReference) api.LocalObjectReference {
	return api.LocalObjectReference{Name: in.Name}
}

func evaluationGradersToK8s(in []api.EvaluationGrader) []k8s.EvaluationGrader {
	if in == nil {
		return nil
	}
	out := make([]k8s.EvaluationGrader, len(in))
	for i, grader := range in {
		out[i] = k8s.EvaluationGrader{Name: grader.Name, Type: grader.Type, Parameters: cloneStringsMap(grader.Parameters)}
	}
	return out
}

func evaluationGradersFromK8s(in []k8s.EvaluationGrader) []api.EvaluationGrader {
	if in == nil {
		return nil
	}
	out := make([]api.EvaluationGrader, len(in))
	for i, grader := range in {
		out[i] = api.EvaluationGrader{Name: grader.Name, Type: grader.Type, Parameters: cloneStringsMap(grader.Parameters)}
	}
	return out
}

func evaluationMetricThresholdsToK8s(in []api.EvaluationMetricThreshold) []k8s.EvaluationMetricThreshold {
	if in == nil {
		return nil
	}
	out := make([]k8s.EvaluationMetricThreshold, len(in))
	for i, threshold := range in {
		out[i] = k8s.EvaluationMetricThreshold{Metric: threshold.Metric, Required: threshold.Required}
		if threshold.Min != nil {
			out[i].Min = new(float64)
			*out[i].Min = *threshold.Min
		}
		if threshold.Max != nil {
			out[i].Max = new(float64)
			*out[i].Max = *threshold.Max
		}
	}
	return out
}

func evaluationMetricThresholdsFromK8s(in []k8s.EvaluationMetricThreshold) []api.EvaluationMetricThreshold {
	if in == nil {
		return nil
	}
	out := make([]api.EvaluationMetricThreshold, len(in))
	for i, threshold := range in {
		out[i] = api.EvaluationMetricThreshold{Metric: threshold.Metric, Required: threshold.Required}
		if threshold.Min != nil {
			out[i].Min = new(float64)
			*out[i].Min = *threshold.Min
		}
		if threshold.Max != nil {
			out[i].Max = new(float64)
			*out[i].Max = *threshold.Max
		}
	}
	return out
}

func evaluationScheduleToK8s(in *api.EvaluationSchedule) *k8s.EvaluationSchedule {
	if in == nil {
		return nil
	}
	return &k8s.EvaluationSchedule{
		Cron:                      in.Cron,
		Timezone:                  in.Timezone,
		Suspend:                   in.Suspend,
		ConcurrencyPolicy:         k8s.EvaluationConcurrencyPolicy(in.ConcurrencyPolicy),
		SuccessfulRunHistoryLimit: in.SuccessfulRunHistoryLimit,
		FailedRunHistoryLimit:     in.FailedRunHistoryLimit,
	}
}

func evaluationScheduleFromK8s(in *k8s.EvaluationSchedule) *api.EvaluationSchedule {
	if in == nil {
		return nil
	}
	return &api.EvaluationSchedule{
		Cron:                      in.Cron,
		Timezone:                  in.Timezone,
		Suspend:                   in.Suspend,
		ConcurrencyPolicy:         api.EvaluationConcurrencyPolicy(in.ConcurrencyPolicy),
		SuccessfulRunHistoryLimit: in.SuccessfulRunHistoryLimit,
		FailedRunHistoryLimit:     in.FailedRunHistoryLimit,
	}
}

func evaluationDatasetCasesToK8s(in []api.EvaluationDatasetCase) []k8s.EvaluationDatasetCase {
	if in == nil {
		return nil
	}
	out := make([]k8s.EvaluationDatasetCase, len(in))
	for i, c := range in {
		out[i] = k8s.EvaluationDatasetCase{
			ID:        c.ID,
			Input:     c.Input,
			Expected:  c.Expected,
			Reference: c.Reference,
			Rubric:    c.Rubric,
			Tags:      append([]string(nil), c.Tags...),
			Metadata:  cloneStringsMap(c.Metadata),
		}
	}
	return out
}

func evaluationDatasetCasesFromK8s(in []k8s.EvaluationDatasetCase) []api.EvaluationDatasetCase {
	if in == nil {
		return nil
	}
	out := make([]api.EvaluationDatasetCase, len(in))
	for i, c := range in {
		out[i] = api.EvaluationDatasetCase{
			ID:        c.ID,
			Input:     c.Input,
			Expected:  c.Expected,
			Reference: c.Reference,
			Rubric:    c.Rubric,
			Tags:      append([]string(nil), c.Tags...),
			Metadata:  cloneStringsMap(c.Metadata),
		}
	}
	return out
}

func localObjectReferencePtrToK8s(in *api.LocalObjectReference) *k8s.LocalObjectReference {
	if in == nil {
		return nil
	}
	return &k8s.LocalObjectReference{Name: in.Name}
}

func localObjectReferencePtrFromK8s(in *k8s.LocalObjectReference) *api.LocalObjectReference {
	if in == nil {
		return nil
	}
	return &api.LocalObjectReference{Name: in.Name}
}

func evaluationRunSummariesToK8s(in []api.EvaluationRunSummary) []k8s.EvaluationRunSummary {
	if in == nil {
		return nil
	}
	out := make([]k8s.EvaluationRunSummary, len(in))
	for i, summary := range in {
		out[i] = k8s.EvaluationRunSummary{
			Name:        summary.Name,
			TargetRef:   evaluationTargetRefToK8s(summary.TargetRef),
			DatasetRef:  evaluationDatasetRefToK8s(summary.DatasetRef),
			Phase:       string(summary.Phase),
			StartedAt:   timeToMetav1(summary.StartedAt),
			CompletedAt: timeToMetav1(summary.CompletedAt),
			Passed:      summary.Passed,
			Metrics:     evaluationMetricResultsToK8s(summary.Metrics),
			Message:     summary.Message,
		}
		if summary.Score != nil {
			out[i].Score = new(float64)
			*out[i].Score = *summary.Score
		}
	}
	return out
}

func evaluationRunSummariesFromK8s(in []k8s.EvaluationRunSummary) []api.EvaluationRunSummary {
	if in == nil {
		return nil
	}
	out := make([]api.EvaluationRunSummary, len(in))
	for i, summary := range in {
		out[i] = api.EvaluationRunSummary{
			Name:        summary.Name,
			TargetRef:   evaluationTargetRefFromK8s(summary.TargetRef),
			DatasetRef:  evaluationDatasetRefFromK8s(summary.DatasetRef),
			Phase:       api.Phase(summary.Phase),
			StartedAt:   timeFromMetav1(summary.StartedAt),
			CompletedAt: timeFromMetav1(summary.CompletedAt),
			Passed:      summary.Passed,
			Metrics:     evaluationMetricResultsFromK8s(summary.Metrics),
			Message:     summary.Message,
		}
		if summary.Score != nil {
			out[i].Score = new(float64)
			*out[i].Score = *summary.Score
		}
	}
	return out
}

func evaluationMetricResultsToK8s(in []api.EvaluationMetricResult) []k8s.EvaluationMetricResult {
	if in == nil {
		return nil
	}
	out := make([]k8s.EvaluationMetricResult, len(in))
	for i, result := range in {
		out[i] = k8s.EvaluationMetricResult{Metric: result.Metric, Value: result.Value, Passed: result.Passed, Message: result.Message}
	}
	return out
}

func evaluationMetricResultsFromK8s(in []k8s.EvaluationMetricResult) []api.EvaluationMetricResult {
	if in == nil {
		return nil
	}
	out := make([]api.EvaluationMetricResult, len(in))
	for i, result := range in {
		out[i] = api.EvaluationMetricResult{Metric: result.Metric, Value: result.Value, Passed: result.Passed, Message: result.Message}
	}
	return out
}

func evaluationCaseResultsToK8s(in []api.EvaluationCaseResult) []k8s.EvaluationCaseResult {
	if in == nil {
		return nil
	}
	out := make([]k8s.EvaluationCaseResult, len(in))
	for i, result := range in {
		out[i] = k8s.EvaluationCaseResult{
			ID:           result.ID,
			TargetJobRef: localObjectReferencePtrToK8s(result.TargetJobRef),
			TargetOutput: result.TargetOutput,
			StartedAt:    timeToMetav1(result.StartedAt),
			CompletedAt:  timeToMetav1(result.CompletedAt),
			Passed:       result.Passed,
			Metrics:      evaluationMetricResultsToK8s(result.Metrics),
			Message:      result.Message,
		}
	}
	return out
}

func evaluationCaseResultsFromK8s(in []k8s.EvaluationCaseResult) []api.EvaluationCaseResult {
	if in == nil {
		return nil
	}
	out := make([]api.EvaluationCaseResult, len(in))
	for i, result := range in {
		out[i] = api.EvaluationCaseResult{
			ID:           result.ID,
			TargetJobRef: localObjectReferencePtrFromK8s(result.TargetJobRef),
			TargetOutput: result.TargetOutput,
			StartedAt:    timeFromMetav1(result.StartedAt),
			CompletedAt:  timeFromMetav1(result.CompletedAt),
			Passed:       result.Passed,
			Metrics:      evaluationMetricResultsFromK8s(result.Metrics),
			Message:      result.Message,
		}
	}
	return out
}
