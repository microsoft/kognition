// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package convert

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

// --- KnowledgeBaseIndexRun ---

func KnowledgeBaseIndexRunToK8s(in *api.KnowledgeBaseIndexRun) *k8s.KnowledgeBaseIndexRun {
	out := &k8s.KnowledgeBaseIndexRun{
		TypeMeta: metav1.TypeMeta{
			Kind:       "KnowledgeBaseIndexRun",
			APIVersion: "kognition.dev/v1alpha1",
		},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
		Spec: k8s.KnowledgeBaseIndexRunSpec{
			KnowledgeBaseName:       in.Spec.KnowledgeBaseName,
			TTLSecondsAfterFinished: in.Spec.TTLSecondsAfterFinished,
		},
		Status: k8s.KnowledgeBaseIndexRunStatus{
			Phase:         string(in.Status.Phase),
			JobName:       in.Status.JobName,
			StartedAt:     in.Status.StartedAt,
			CompletedAt:   in.Status.CompletedAt,
			DocumentCount: in.Status.DocumentCount,
			IndexedChunks: in.Status.IndexedChunks,
			SourceDigest:  in.Status.SourceDigest,
			Message:       in.Status.Message,
			Conditions:    conditionsToK8s(in.Status.Conditions),
		},
	}
	if in.Status.BackendBinding != nil {
		out.Status.BackendBinding = &k8s.KnowledgeBackendBinding{
			Generation:            in.Status.BackendBinding.Generation,
			ProviderName:          in.Status.BackendBinding.ProviderName,
			ReclaimPolicy:         k8s.ReclaimPolicy(in.Status.BackendBinding.ReclaimPolicy),
			ClassUID:              in.Status.BackendBinding.ClassUID,
			ClassGeneration:       in.Status.BackendBinding.ClassGeneration,
			ConfigurationAPIGroup: in.Status.BackendBinding.ConfigurationAPIGroup,
			ConfigurationKind:     in.Status.BackendBinding.ConfigurationKind,
			ConfigurationName:     in.Status.BackendBinding.ConfigurationName,
			ConfigurationUID:      in.Status.BackendBinding.ConfigurationUID,
			ConfigurationDigest:   in.Status.BackendBinding.ConfigurationDigest,
			OperationID:           in.Status.BackendBinding.OperationID,
			BackendOperationID:    in.Status.BackendBinding.BackendOperationID,
		}
	}
	return out
}

func KnowledgeBaseIndexRunFromK8s(in *k8s.KnowledgeBaseIndexRun) *api.KnowledgeBaseIndexRun {
	out := &api.KnowledgeBaseIndexRun{
		TypeMeta: api.TypeMeta{
			Kind:       in.Kind,
			APIVersion: in.APIVersion,
		},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec: api.KnowledgeBaseIndexRunSpec{
			KnowledgeBaseName:       in.Spec.KnowledgeBaseName,
			TTLSecondsAfterFinished: in.Spec.TTLSecondsAfterFinished,
		},
		Status: api.KnowledgeBaseIndexRunStatus{
			Phase:         api.Phase(in.Status.Phase),
			JobName:       in.Status.JobName,
			StartedAt:     in.Status.StartedAt,
			CompletedAt:   in.Status.CompletedAt,
			DocumentCount: in.Status.DocumentCount,
			IndexedChunks: in.Status.IndexedChunks,
			SourceDigest:  in.Status.SourceDigest,
			Message:       in.Status.Message,
			Conditions:    conditionsFromK8s(in.Status.Conditions),
		},
	}
	if in.Status.BackendBinding != nil {
		out.Status.BackendBinding = &api.KnowledgeBackendBinding{
			Generation:            in.Status.BackendBinding.Generation,
			ProviderName:          in.Status.BackendBinding.ProviderName,
			ReclaimPolicy:         api.ReclaimPolicy(in.Status.BackendBinding.ReclaimPolicy),
			ClassUID:              in.Status.BackendBinding.ClassUID,
			ClassGeneration:       in.Status.BackendBinding.ClassGeneration,
			ConfigurationAPIGroup: in.Status.BackendBinding.ConfigurationAPIGroup,
			ConfigurationKind:     in.Status.BackendBinding.ConfigurationKind,
			ConfigurationName:     in.Status.BackendBinding.ConfigurationName,
			ConfigurationUID:      in.Status.BackendBinding.ConfigurationUID,
			ConfigurationDigest:   in.Status.BackendBinding.ConfigurationDigest,
			OperationID:           in.Status.BackendBinding.OperationID,
			BackendOperationID:    in.Status.BackendBinding.BackendOperationID,
		}
	}
	return out
}

// --- KnowledgeBaseIndexSchedule ---

func KnowledgeBaseIndexScheduleToK8s(in *api.KnowledgeBaseIndexSchedule) *k8s.KnowledgeBaseIndexSchedule {
	out := &k8s.KnowledgeBaseIndexSchedule{
		TypeMeta: metav1.TypeMeta{
			Kind:       "KnowledgeBaseIndexSchedule",
			APIVersion: "kognition.dev/v1alpha1",
		},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
		Spec: k8s.KnowledgeBaseIndexScheduleSpec{
			KnowledgeBaseName:          in.Spec.KnowledgeBaseName,
			Schedule:                   in.Spec.Schedule,
			Suspend:                    in.Spec.Suspend,
			ConcurrencyPolicy:          in.Spec.ConcurrencyPolicy,
			SuccessfulRunsHistoryLimit: in.Spec.SuccessfulRunsHistoryLimit,
			FailedRunsHistoryLimit:     in.Spec.FailedRunsHistoryLimit,
			StartingDeadlineSeconds:    in.Spec.StartingDeadlineSeconds,
			RunTemplate: k8s.KnowledgeBaseIndexRunSpec{
				KnowledgeBaseName:       in.Spec.RunTemplate.KnowledgeBaseName,
				TTLSecondsAfterFinished: in.Spec.RunTemplate.TTLSecondsAfterFinished,
			},
		},
		Status: k8s.KnowledgeBaseIndexScheduleStatus{
			Phase:              string(in.Status.Phase),
			LastScheduleTime:   in.Status.LastScheduleTime,
			LastSuccessfulTime: in.Status.LastSuccessfulTime,
			NextScheduleTime:   in.Status.NextScheduleTime,
			Active:             kbActiveRefsToK8s(in.Status.Active),
			Conditions:         conditionsToK8s(in.Status.Conditions),
		},
	}
	return out
}

func KnowledgeBaseIndexScheduleFromK8s(in *k8s.KnowledgeBaseIndexSchedule) *api.KnowledgeBaseIndexSchedule {
	out := &api.KnowledgeBaseIndexSchedule{
		TypeMeta: api.TypeMeta{
			Kind:       in.Kind,
			APIVersion: in.APIVersion,
		},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec: api.KnowledgeBaseIndexScheduleSpec{
			KnowledgeBaseName:          in.Spec.KnowledgeBaseName,
			Schedule:                   in.Spec.Schedule,
			Suspend:                    in.Spec.Suspend,
			ConcurrencyPolicy:          in.Spec.ConcurrencyPolicy,
			SuccessfulRunsHistoryLimit: in.Spec.SuccessfulRunsHistoryLimit,
			FailedRunsHistoryLimit:     in.Spec.FailedRunsHistoryLimit,
			StartingDeadlineSeconds:    in.Spec.StartingDeadlineSeconds,
			RunTemplate: api.KnowledgeBaseIndexRunSpec{
				KnowledgeBaseName:       in.Spec.RunTemplate.KnowledgeBaseName,
				TTLSecondsAfterFinished: in.Spec.RunTemplate.TTLSecondsAfterFinished,
			},
		},
		Status: api.KnowledgeBaseIndexScheduleStatus{
			Phase:              api.Phase(in.Status.Phase),
			LastScheduleTime:   in.Status.LastScheduleTime,
			LastSuccessfulTime: in.Status.LastSuccessfulTime,
			NextScheduleTime:   in.Status.NextScheduleTime,
			Active:             kbActiveRefsFromK8s(in.Status.Active),
			Conditions:         conditionsFromK8s(in.Status.Conditions),
		},
	}
	return out
}

func kbActiveRefsToK8s(in []api.LocalObjectReference) []k8s.LocalObjectReference {
	if len(in) == 0 {
		return nil
	}
	out := make([]k8s.LocalObjectReference, len(in))
	for i, r := range in {
		out[i] = k8s.LocalObjectReference{Name: r.Name}
	}
	return out
}

func kbActiveRefsFromK8s(in []k8s.LocalObjectReference) []api.LocalObjectReference {
	if len(in) == 0 {
		return nil
	}
	out := make([]api.LocalObjectReference, len(in))
	for i, r := range in {
		out[i] = api.LocalObjectReference{Name: r.Name}
	}
	return out
}
