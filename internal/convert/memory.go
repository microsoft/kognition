// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package convert

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

// --- MemoryClass ---

func MemoryClassToK8s(in *api.MemoryClass) *k8s.MemoryClass {
	out := &k8s.MemoryClass{
		TypeMeta: metav1.TypeMeta{
			Kind:       "MemoryClass",
			APIVersion: "kognition.dev/v1alpha1",
		},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
		Spec: k8s.MemoryClassSpec{
			BackendType:     in.Spec.BackendType,
			Parameters:      in.Spec.Parameters,
			ReclaimPolicy:   k8s.ReclaimPolicy(in.Spec.ReclaimPolicy),
			ConsistencyMode: k8s.ConcurrencyMode(in.Spec.ConsistencyMode),
		},
	}
	if in.Spec.SummarizationConfig != nil {
		out.Spec.SummarizationConfig = &k8s.MemorySummarizationConfig{
			Enabled:          in.Spec.SummarizationConfig.Enabled,
			ThresholdRecords: in.Spec.SummarizationConfig.ThresholdRecords,
		}
		if in.Spec.SummarizationConfig.ModelRef != nil {
			out.Spec.SummarizationConfig.ModelRef = &k8s.LocalObjectReference{
				Name: in.Spec.SummarizationConfig.ModelRef.Name,
			}
		}
	}
	out.Status = k8s.MemoryClassStatus{
		Phase:      string(in.Status.Phase),
		Message:    in.Status.Message,
		Conditions: conditionsToK8s(in.Status.Conditions),
	}
	return out
}

func MemoryClassFromK8s(in *k8s.MemoryClass) *api.MemoryClass {
	out := &api.MemoryClass{
		TypeMeta: api.TypeMeta{
			Kind:       in.Kind,
			APIVersion: in.APIVersion,
		},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec: api.MemoryClassSpec{
			BackendType:     in.Spec.BackendType,
			Parameters:      in.Spec.Parameters,
			ReclaimPolicy:   api.ReclaimPolicy(in.Spec.ReclaimPolicy),
			ConsistencyMode: string(in.Spec.ConsistencyMode),
		},
	}
	if in.Spec.SummarizationConfig != nil {
		out.Spec.SummarizationConfig = &api.MemorySummarizationConfig{
			Enabled:          in.Spec.SummarizationConfig.Enabled,
			ThresholdRecords: in.Spec.SummarizationConfig.ThresholdRecords,
		}
		if in.Spec.SummarizationConfig.ModelRef != nil {
			out.Spec.SummarizationConfig.ModelRef = &api.LocalObjectReference{
				Name: in.Spec.SummarizationConfig.ModelRef.Name,
			}
		}
	}
	out.Status = api.MemoryClassStatus{
		Phase:      api.Phase(in.Status.Phase),
		Message:    in.Status.Message,
		Conditions: conditionsFromK8s(in.Status.Conditions),
	}
	return out
}

// --- MemoryClaim ---

func MemoryClaimToK8s(in *api.MemoryClaim) *k8s.MemoryClaim {
	out := &k8s.MemoryClaim{
		TypeMeta: metav1.TypeMeta{
			Kind:       "MemoryClaim",
			APIVersion: "kognition.dev/v1alpha1",
		},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
		Spec: k8s.MemoryClaimSpec{
			MemoryClassName: in.Spec.MemoryClassName,
			Type:            k8s.MemoryType(in.Spec.Type),
			Capacity:        in.Spec.Capacity,
			ConsistencyMode: k8s.ConcurrencyMode(in.Spec.ConsistencyMode),
			ConcurrencyMode: k8s.ConcurrencyMode(in.Spec.ConcurrencyMode),
			AccessModes:     in.Spec.AccessModes,
		},
		Status: k8s.MemoryClaimStatus{
			Phase:           string(in.Status.Phase),
			BoundVolumeName: in.Status.BoundVolumeName,
			Conditions:      conditionsToK8s(in.Status.Conditions),
		},
	}
	return out
}

func MemoryClaimFromK8s(in *k8s.MemoryClaim) *api.MemoryClaim {
	out := &api.MemoryClaim{
		TypeMeta: api.TypeMeta{
			Kind:       in.Kind,
			APIVersion: in.APIVersion,
		},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec: api.MemoryClaimSpec{
			MemoryClassName: in.Spec.MemoryClassName,
			Type:            api.MemoryType(in.Spec.Type),
			Capacity:        in.Spec.Capacity,
			ConsistencyMode: string(in.Spec.ConsistencyMode),
			ConcurrencyMode: api.ConcurrencyMode(in.Spec.ConcurrencyMode),
			AccessModes:     in.Spec.AccessModes,
		},
		Status: api.MemoryClaimStatus{
			Phase:           api.Phase(in.Status.Phase),
			BoundVolumeName: in.Status.BoundVolumeName,
			Conditions:      conditionsFromK8s(in.Status.Conditions),
		},
	}
	return out
}

// --- MemoryVolume ---

func MemoryVolumeToK8s(in *api.MemoryVolume) *k8s.MemoryVolume {
	out := &k8s.MemoryVolume{
		TypeMeta: metav1.TypeMeta{
			Kind:       "MemoryVolume",
			APIVersion: "kognition.dev/v1alpha1",
		},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
		Spec: k8s.MemoryVolumeSpec{
			MemoryClassName: in.Spec.MemoryClassName,
			BackendHandle:   in.Spec.BackendHandle,
			Capacity:        in.Spec.Capacity,
		},
		Status: k8s.MemoryVolumeStatus{
			Phase:      string(in.Status.Phase),
			Capacity:   in.Status.Capacity,
			Conditions: conditionsToK8s(in.Status.Conditions),
		},
	}
	if in.Spec.ClaimRef != nil {
		out.Spec.ClaimRef = &k8s.LocalObjectReference{
			Name: in.Spec.ClaimRef.Name,
		}
	}
	return out
}

func MemoryVolumeFromK8s(in *k8s.MemoryVolume) *api.MemoryVolume {
	out := &api.MemoryVolume{
		TypeMeta: api.TypeMeta{
			Kind:       in.Kind,
			APIVersion: in.APIVersion,
		},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec: api.MemoryVolumeSpec{
			MemoryClassName: in.Spec.MemoryClassName,
			BackendHandle:   in.Spec.BackendHandle,
			Capacity:        in.Spec.Capacity,
		},
		Status: api.MemoryVolumeStatus{
			Phase:      api.Phase(in.Status.Phase),
			Capacity:   in.Status.Capacity,
			Conditions: conditionsFromK8s(in.Status.Conditions),
		},
	}
	if in.Spec.ClaimRef != nil {
		out.Spec.ClaimRef = &api.LocalObjectReference{
			Name: in.Spec.ClaimRef.Name,
		}
	}
	return out
}

// --- Memory ---

func MemoryToK8s(in *api.Memory) *k8s.Memory {
	if in == nil {
		return nil
	}
	return &k8s.Memory{
		TypeMeta:   metav1.TypeMeta{Kind: "Memory", APIVersion: api.APIVersion},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
		Spec: k8s.MemorySpec{
			ClaimRef:        k8s.LocalObjectReference{Name: in.Spec.ClaimRef.Name},
			Type:            k8s.MemoryType(in.Spec.Type),
			TTL:             in.Spec.TTL,
			RetentionPolicy: in.Spec.RetentionPolicy,
		},
		Status: k8s.MemoryStatus{
			Phase:       string(in.Status.Phase),
			Conditions:  conditionsToK8s(in.Status.Conditions),
			RecordCount: in.Status.RecordCount,
			BytesUsed:   in.Status.BytesUsed,
		},
	}
}

func MemoryFromK8s(in *k8s.Memory) *api.Memory {
	if in == nil {
		return nil
	}
	return &api.Memory{
		TypeMeta:   api.TypeMeta{Kind: in.Kind, APIVersion: in.APIVersion},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec: api.MemorySpec{
			ClaimRef:        api.LocalObjectReference{Name: in.Spec.ClaimRef.Name},
			Type:            api.MemoryType(in.Spec.Type),
			TTL:             in.Spec.TTL,
			RetentionPolicy: in.Spec.RetentionPolicy,
		},
		Status: api.MemoryStatus{
			Phase:       api.Phase(in.Status.Phase),
			Conditions:  conditionsFromK8s(in.Status.Conditions),
			RecordCount: in.Status.RecordCount,
			BytesUsed:   in.Status.BytesUsed,
		},
	}
}

// --- MemoryRecord ---

func MemoryRecordToK8s(in *api.MemoryRecord) *k8s.MemoryRecord {
	if in == nil {
		return nil
	}
	out := &k8s.MemoryRecord{
		TypeMeta:   metav1.TypeMeta{Kind: "MemoryRecord", APIVersion: api.APIVersion},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
		Spec: k8s.MemoryRecordSpec{
			MemoryRef: k8s.LocalObjectReference{Name: in.Spec.MemoryRef.Name},
			Content:   in.Spec.Content,
			Metadata:  cloneStringMap(in.Spec.Metadata),
		},
		Status: k8s.MemoryRecordStatus{
			Phase:      string(in.Status.Phase),
			Conditions: conditionsToK8s(in.Status.Conditions),
		},
	}
	if in.Spec.Provenance != nil {
		out.Spec.Provenance = &k8s.MemoryProvenance{
			SourceAgentIdentity:    in.Spec.Provenance.SourceAgentIdentity,
			SessionID:              in.Spec.Provenance.SessionID,
			ReasoningStepID:        in.Spec.Provenance.ReasoningStepID,
			Timestamp:              metav1.NewTime(in.Spec.Provenance.Timestamp),
			GrantID:                in.Spec.Provenance.GrantID,
			DerivedFromRecordNames: append([]string(nil), in.Spec.Provenance.DerivedFromRecordNames...),
		}
	}
	return out
}

func MemoryRecordFromK8s(in *k8s.MemoryRecord) *api.MemoryRecord {
	if in == nil {
		return nil
	}
	out := &api.MemoryRecord{
		TypeMeta:   api.TypeMeta{Kind: in.Kind, APIVersion: in.APIVersion},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec: api.MemoryRecordSpec{
			MemoryRef: api.LocalObjectReference{Name: in.Spec.MemoryRef.Name},
			Content:   in.Spec.Content,
			Metadata:  cloneStringMap(in.Spec.Metadata),
		},
		Status: api.MemoryRecordStatus{
			Phase:      api.Phase(in.Status.Phase),
			Conditions: conditionsFromK8s(in.Status.Conditions),
		},
	}
	if in.Spec.Provenance != nil {
		out.Spec.Provenance = &api.MemoryProvenance{
			SourceAgentIdentity:    in.Spec.Provenance.SourceAgentIdentity,
			SessionID:              in.Spec.Provenance.SessionID,
			ReasoningStepID:        in.Spec.Provenance.ReasoningStepID,
			Timestamp:              in.Spec.Provenance.Timestamp.Time,
			GrantID:                in.Spec.Provenance.GrantID,
			DerivedFromRecordNames: append([]string(nil), in.Spec.Provenance.DerivedFromRecordNames...),
		}
	}
	return out
}
