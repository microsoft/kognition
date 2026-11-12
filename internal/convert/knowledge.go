// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package convert

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	k8s "github.com/kognition-ai/kognition/api/k8s/v1alpha1"
	api "github.com/kognition-ai/kognition/api/v1alpha1"
)

// --- KnowledgeBaseClass ---

func KnowledgeBaseClassToK8s(in *api.KnowledgeBaseClass) *k8s.KnowledgeBaseClass {
	out := &k8s.KnowledgeBaseClass{
		TypeMeta: metav1.TypeMeta{
			Kind:       "KnowledgeBaseClass",
			APIVersion: "kognition.dev/v1alpha1",
		},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
		Spec: k8s.KnowledgeBaseClassSpec{
			Provisioner:   in.Spec.Provisioner,
			IndexMode:     in.Spec.IndexMode,
			ReclaimPolicy: k8s.ReclaimPolicy(in.Spec.ReclaimPolicy),
			Parameters:    in.Spec.Parameters,
		},
	}
	if in.Spec.EmbeddingModelRef != nil {
		out.Spec.EmbeddingModelRef = &k8s.LocalObjectReference{Name: in.Spec.EmbeddingModelRef.Name}
	}
	if in.Spec.ChunkStrategy != nil {
		out.Spec.ChunkStrategy = &k8s.KBChunkStrategy{
			Size:    in.Spec.ChunkStrategy.Size,
			Overlap: in.Spec.ChunkStrategy.Overlap,
			Mode:    in.Spec.ChunkStrategy.Mode,
		}
	}
	if in.Spec.VectorStore != nil {
		out.Spec.VectorStore = kbVectorStoreToK8s(in.Spec.VectorStore)
	}
	if in.Spec.BackendRef != nil {
		out.Spec.BackendRef = &k8s.KnowledgeBackendReference{APIGroup: in.Spec.BackendRef.APIGroup, Kind: in.Spec.BackendRef.Kind, Name: in.Spec.BackendRef.Name}
	}
	return out
}

func KnowledgeBaseClassFromK8s(in *k8s.KnowledgeBaseClass) *api.KnowledgeBaseClass {
	out := &api.KnowledgeBaseClass{
		TypeMeta: api.TypeMeta{
			Kind:       in.Kind,
			APIVersion: in.APIVersion,
		},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec: api.KnowledgeBaseClassSpec{
			Provisioner:   in.Spec.Provisioner,
			IndexMode:     in.Spec.IndexMode,
			ReclaimPolicy: api.ReclaimPolicy(in.Spec.ReclaimPolicy),
			Parameters:    in.Spec.Parameters,
		},
	}
	if in.Spec.EmbeddingModelRef != nil {
		out.Spec.EmbeddingModelRef = &api.LocalObjectReference{Name: in.Spec.EmbeddingModelRef.Name}
	}
	if in.Spec.ChunkStrategy != nil {
		out.Spec.ChunkStrategy = &api.KBChunkStrategy{
			Size:    in.Spec.ChunkStrategy.Size,
			Overlap: in.Spec.ChunkStrategy.Overlap,
			Mode:    in.Spec.ChunkStrategy.Mode,
		}
	}
	if in.Spec.VectorStore != nil {
		out.Spec.VectorStore = kbVectorStoreFromK8s(in.Spec.VectorStore)
	}
	if in.Spec.BackendRef != nil {
		out.Spec.BackendRef = &api.KnowledgeBackendReference{APIGroup: in.Spec.BackendRef.APIGroup, Kind: in.Spec.BackendRef.Kind, Name: in.Spec.BackendRef.Name}
	}
	return out
}

// --- KnowledgeBase ---

func KnowledgeBaseToK8s(in *api.KnowledgeBase) *k8s.KnowledgeBase {
	out := &k8s.KnowledgeBase{
		TypeMeta: metav1.TypeMeta{
			Kind:       "KnowledgeBase",
			APIVersion: "kognition.dev/v1alpha1",
		},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
		Spec: k8s.KnowledgeBaseSpec{
			KnowledgeBaseClassName: in.Spec.KnowledgeBaseClassName,
			Sources:                kbSourcesToK8s(in.Spec.Sources),
		},
	}
	if in.Spec.IndexConfig != nil {
		out.Spec.IndexConfig = &k8s.KBIndexConfig{
			ChunkSize:    in.Spec.IndexConfig.ChunkSize,
			ChunkOverlap: in.Spec.IndexConfig.ChunkOverlap,
		}
		if in.Spec.IndexConfig.EmbeddingModelRef != nil {
			out.Spec.IndexConfig.EmbeddingModelRef = &k8s.LocalObjectReference{Name: in.Spec.IndexConfig.EmbeddingModelRef.Name}
		}
	}
	if in.Spec.RefreshPolicy != nil {
		out.Spec.RefreshPolicy = &k8s.KBRefreshPolicy{
			Mode:        in.Spec.RefreshPolicy.Mode,
			Interval:    in.Spec.RefreshPolicy.Interval,
			WebhookPath: in.Spec.RefreshPolicy.WebhookPath,
		}
	}
	out.Status = k8s.KnowledgeBaseStatus{
		Phase:         string(in.Status.Phase),
		Conditions:    conditionsToK8s(in.Status.Conditions),
		DocumentCount: in.Status.DocumentCount,
		IndexedChunks: in.Status.IndexedChunks,
		Generation:    in.Status.Generation,
		LastRefresh:   in.Status.LastRefresh,
	}
	if binding := in.Status.BackendBinding; binding != nil {
		out.Status.BackendBinding = KnowledgeBackendBindingToK8s(binding)
	}
	return out
}

func KnowledgeBackendBindingToK8s(binding *api.KnowledgeBackendBinding) *k8s.KnowledgeBackendBinding {
	if binding == nil {
		return nil
	}
	out := &k8s.KnowledgeBackendBinding{
		Generation: binding.Generation, ProviderName: binding.ProviderName, ReclaimPolicy: k8s.ReclaimPolicy(binding.ReclaimPolicy),
		ClassUID: binding.ClassUID, ClassGeneration: binding.ClassGeneration,
		ConfigurationAPIGroup: binding.ConfigurationAPIGroup, ConfigurationKind: binding.ConfigurationKind, ConfigurationName: binding.ConfigurationName,
		ConfigurationUID: binding.ConfigurationUID, ConfigurationDigest: binding.ConfigurationDigest,
		OperationID:        binding.OperationID,
		BackendOperationID: binding.BackendOperationID,
	}
	if binding.EmbeddingModelBinding != nil {
		converted := k8s.ModelVersionBinding(*binding.EmbeddingModelBinding)
		out.EmbeddingModelBinding = &converted
	}
	return out
}

func KnowledgeBaseFromK8s(in *k8s.KnowledgeBase) *api.KnowledgeBase {
	out := &api.KnowledgeBase{
		TypeMeta: api.TypeMeta{
			Kind:       in.Kind,
			APIVersion: in.APIVersion,
		},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec: api.KnowledgeBaseSpec{
			KnowledgeBaseClassName: in.Spec.KnowledgeBaseClassName,
			Sources:                kbSourcesFromK8s(in.Spec.Sources),
		},
	}
	if in.Spec.IndexConfig != nil {
		out.Spec.IndexConfig = &api.KBIndexConfig{
			ChunkSize:    in.Spec.IndexConfig.ChunkSize,
			ChunkOverlap: in.Spec.IndexConfig.ChunkOverlap,
		}
		if in.Spec.IndexConfig.EmbeddingModelRef != nil {
			out.Spec.IndexConfig.EmbeddingModelRef = &api.LocalObjectReference{Name: in.Spec.IndexConfig.EmbeddingModelRef.Name}
		}
	}
	if in.Spec.RefreshPolicy != nil {
		out.Spec.RefreshPolicy = &api.KBRefreshPolicy{
			Mode:        in.Spec.RefreshPolicy.Mode,
			Interval:    in.Spec.RefreshPolicy.Interval,
			WebhookPath: in.Spec.RefreshPolicy.WebhookPath,
		}
	}
	out.Status = api.KnowledgeBaseStatus{
		Phase:         api.Phase(in.Status.Phase),
		Conditions:    conditionsFromK8s(in.Status.Conditions),
		DocumentCount: in.Status.DocumentCount,
		IndexedChunks: in.Status.IndexedChunks,
		Generation:    in.Status.Generation,
		LastRefresh:   in.Status.LastRefresh,
	}
	if binding := in.Status.BackendBinding; binding != nil {
		out.Status.BackendBinding = &api.KnowledgeBackendBinding{
			Generation: binding.Generation, ProviderName: binding.ProviderName, ReclaimPolicy: api.ReclaimPolicy(binding.ReclaimPolicy),
			ClassUID: binding.ClassUID, ClassGeneration: binding.ClassGeneration,
			ConfigurationAPIGroup: binding.ConfigurationAPIGroup, ConfigurationKind: binding.ConfigurationKind, ConfigurationName: binding.ConfigurationName,
			ConfigurationUID: binding.ConfigurationUID, ConfigurationDigest: binding.ConfigurationDigest,
			OperationID:        binding.OperationID,
			BackendOperationID: binding.BackendOperationID,
		}
		if binding.EmbeddingModelBinding != nil {
			converted := api.ModelVersionBinding(*binding.EmbeddingModelBinding)
			out.Status.BackendBinding.EmbeddingModelBinding = &converted
		}
	}
	return out
}

func kbSourcesToK8s(in []api.KBSource) []k8s.KBSource {
	if len(in) == 0 {
		return nil
	}
	out := make([]k8s.KBSource, len(in))
	for i, s := range in {
		out[i] = k8s.KBSource{
			Type:      s.Type,
			Data:      s.Data,
			URI:       s.URI,
			Directory: s.Directory,
		}
		if s.Git != nil {
			out[i].Git = &k8s.KBGitSource{URL: s.Git.URL, Branch: s.Git.Branch, Path: s.Git.Path}
		}
		if s.ConfigMap != nil {
			out[i].ConfigMap = &k8s.LocalObjectReference{Name: s.ConfigMap.Name}
		}
		if s.Blob != nil {
			out[i].Blob = kbBlobSourceToK8s(s.Blob)
		}
	}
	return out
}

func kbSourcesFromK8s(in []k8s.KBSource) []api.KBSource {
	if len(in) == 0 {
		return nil
	}
	out := make([]api.KBSource, len(in))
	for i, s := range in {
		out[i] = api.KBSource{
			Type:      s.Type,
			Data:      s.Data,
			URI:       s.URI,
			Directory: s.Directory,
		}
		if s.Git != nil {
			out[i].Git = &api.KBGitSource{URL: s.Git.URL, Branch: s.Git.Branch, Path: s.Git.Path}
		}
		if s.ConfigMap != nil {
			out[i].ConfigMap = &api.LocalObjectReference{Name: s.ConfigMap.Name}
		}
		if s.Blob != nil {
			out[i].Blob = kbBlobSourceFromK8s(s.Blob)
		}
	}
	return out
}

func kbBlobSourceToK8s(in *api.KBBlobSource) *k8s.KBBlobSource {
	if in == nil {
		return nil
	}
	out := &k8s.KBBlobSource{
		Provider: in.Provider,
		Bucket:   in.Bucket,
		Prefix:   in.Prefix,
		Region:   in.Region,
		Endpoint: in.Endpoint,
	}
	if in.CredentialSecretRef != nil {
		out.CredentialSecretRef = &k8s.LocalObjectReference{Name: in.CredentialSecretRef.Name}
	}
	if len(in.Include) > 0 {
		out.Include = append([]string(nil), in.Include...)
	}
	if len(in.Exclude) > 0 {
		out.Exclude = append([]string(nil), in.Exclude...)
	}
	return out
}

func kbBlobSourceFromK8s(in *k8s.KBBlobSource) *api.KBBlobSource {
	if in == nil {
		return nil
	}
	out := &api.KBBlobSource{
		Provider: in.Provider,
		Bucket:   in.Bucket,
		Prefix:   in.Prefix,
		Region:   in.Region,
		Endpoint: in.Endpoint,
	}
	if in.CredentialSecretRef != nil {
		out.CredentialSecretRef = &api.LocalObjectReference{Name: in.CredentialSecretRef.Name}
	}
	if len(in.Include) > 0 {
		out.Include = append([]string(nil), in.Include...)
	}
	if len(in.Exclude) > 0 {
		out.Exclude = append([]string(nil), in.Exclude...)
	}
	return out
}

// --- KnowledgeBaseClaim ---

func KnowledgeBaseClaimToK8s(in *api.KnowledgeBaseClaim) *k8s.KnowledgeBaseClaim {
	out := &k8s.KnowledgeBaseClaim{
		TypeMeta: metav1.TypeMeta{
			Kind:       "KnowledgeBaseClaim",
			APIVersion: "kognition.dev/v1alpha1",
		},
		ObjectMeta: objectMetaToK8s(in.ObjectMeta),
		Spec: k8s.KnowledgeBaseClaimSpec{
			KnowledgeBaseName: in.Spec.KnowledgeBaseName,
			AccessMode:        in.Spec.AccessMode,
			Retrieval:         kbRetrievalOverrideToK8s(in.Spec.Retrieval),
			Usage:             kbUsageQuotaToK8s(in.Spec.Usage),
		},
		Status: k8s.KnowledgeBaseClaimStatus{
			Phase:              string(in.Status.Phase),
			BoundKnowledgeBase: in.Status.BoundKnowledgeBase,
			ObservedGeneration: in.Status.ObservedGeneration,
			Conditions:         conditionsToK8s(in.Status.Conditions),
		},
	}
	return out
}

func KnowledgeBaseClaimFromK8s(in *k8s.KnowledgeBaseClaim) *api.KnowledgeBaseClaim {
	out := &api.KnowledgeBaseClaim{
		TypeMeta: api.TypeMeta{
			Kind:       in.Kind,
			APIVersion: in.APIVersion,
		},
		ObjectMeta: objectMetaFromK8s(in.ObjectMeta),
		Spec: api.KnowledgeBaseClaimSpec{
			KnowledgeBaseName: in.Spec.KnowledgeBaseName,
			AccessMode:        in.Spec.AccessMode,
			Retrieval:         kbRetrievalOverrideFromK8s(in.Spec.Retrieval),
			Usage:             kbUsageQuotaFromK8s(in.Spec.Usage),
		},
		Status: api.KnowledgeBaseClaimStatus{
			Phase:              api.Phase(in.Status.Phase),
			BoundKnowledgeBase: in.Status.BoundKnowledgeBase,
			ObservedGeneration: in.Status.ObservedGeneration,
			Conditions:         conditionsFromK8s(in.Status.Conditions),
		},
	}
	return out
}

func kbRetrievalOverrideToK8s(in *api.KBRetrievalOverride) *k8s.KBRetrievalOverride {
	if in == nil {
		return nil
	}
	out := &k8s.KBRetrievalOverride{
		Mode:     in.Mode,
		TopK:     in.TopK,
		MinScore: in.MinScore,
		Filter:   in.Filter,
	}
	if in.HybridWeights != nil {
		out.HybridWeights = &k8s.KBHybridWeights{
			Keyword: in.HybridWeights.Keyword,
			Vector:  in.HybridWeights.Vector,
		}
	}
	return out
}

func kbRetrievalOverrideFromK8s(in *k8s.KBRetrievalOverride) *api.KBRetrievalOverride {
	if in == nil {
		return nil
	}
	out := &api.KBRetrievalOverride{
		Mode:     in.Mode,
		TopK:     in.TopK,
		MinScore: in.MinScore,
		Filter:   in.Filter,
	}
	if in.HybridWeights != nil {
		out.HybridWeights = &api.KBHybridWeights{
			Keyword: in.HybridWeights.Keyword,
			Vector:  in.HybridWeights.Vector,
		}
	}
	return out
}

func kbUsageQuotaToK8s(in *api.KBUsageQuota) *k8s.KBUsageQuota {
	if in == nil {
		return nil
	}
	return &k8s.KBUsageQuota{
		MaxQueriesPerMinute: in.MaxQueriesPerMinute,
		MaxTokensReturned:   in.MaxTokensReturned,
	}
}

func kbUsageQuotaFromK8s(in *k8s.KBUsageQuota) *api.KBUsageQuota {
	if in == nil {
		return nil
	}
	return &api.KBUsageQuota{
		MaxQueriesPerMinute: in.MaxQueriesPerMinute,
		MaxTokensReturned:   in.MaxTokensReturned,
	}
}

// --- KBVectorStore ---

func kbVectorStoreToK8s(in *api.KBVectorStore) *k8s.KBVectorStore {
	if in == nil {
		return nil
	}
	out := &k8s.KBVectorStore{Type: in.Type}
	if in.Pgvector != nil {
		out.Pgvector = &k8s.KBPgvectorStore{
			ConnectionSecretRef: k8s.LocalObjectReference{Name: in.Pgvector.ConnectionSecretRef.Name},
			Table:               in.Pgvector.Table,
			Dimension:           in.Pgvector.Dimension,
		}
	}
	return out
}

func kbVectorStoreFromK8s(in *k8s.KBVectorStore) *api.KBVectorStore {
	if in == nil {
		return nil
	}
	out := &api.KBVectorStore{Type: in.Type}
	if in.Pgvector != nil {
		out.Pgvector = &api.KBPgvectorStore{
			ConnectionSecretRef: api.LocalObjectReference{Name: in.Pgvector.ConnectionSecretRef.Name},
			Table:               in.Pgvector.Table,
			Dimension:           in.Pgvector.Dimension,
		}
	}
	return out
}
