// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Hand-written deepcopy methods for KnowledgeBase types.
// controller-gen v0.21.0 panics on this package; these are equivalent to
// what controller-gen would produce. Keep in sync with the KnowledgeBase*
// structs in knowledge_types.go.

package v1alpha1

import (
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
)

// ---- KBChunkStrategy ----

func (in *KBChunkStrategy) DeepCopyInto(out *KBChunkStrategy) {
	*out = *in
}

func (in *KBChunkStrategy) DeepCopy() *KBChunkStrategy {
	if in == nil {
		return nil
	}
	out := new(KBChunkStrategy)
	in.DeepCopyInto(out)
	return out
}

// ---- KBPgvectorStore ----

func (in *KBPgvectorStore) DeepCopyInto(out *KBPgvectorStore) {
	*out = *in
}

func (in *KBPgvectorStore) DeepCopy() *KBPgvectorStore {
	if in == nil {
		return nil
	}
	out := new(KBPgvectorStore)
	in.DeepCopyInto(out)
	return out
}

// ---- KBVectorStore ----

func (in *KnowledgeBackendReference) DeepCopyInto(out *KnowledgeBackendReference) {
	*out = *in
}

func (in *KnowledgeBackendReference) DeepCopy() *KnowledgeBackendReference {
	if in == nil {
		return nil
	}
	out := new(KnowledgeBackendReference)
	in.DeepCopyInto(out)
	return out
}

func (in *KBVectorStore) DeepCopyInto(out *KBVectorStore) {
	*out = *in
	if in.Pgvector != nil {
		out.Pgvector = new(KBPgvectorStore)
		*out.Pgvector = *in.Pgvector
	}
}

func (in *KBVectorStore) DeepCopy() *KBVectorStore {
	if in == nil {
		return nil
	}
	out := new(KBVectorStore)
	in.DeepCopyInto(out)
	return out
}

// ---- KBGitSource ----

func (in *KBGitSource) DeepCopyInto(out *KBGitSource) {
	*out = *in
}

func (in *KBGitSource) DeepCopy() *KBGitSource {
	if in == nil {
		return nil
	}
	out := new(KBGitSource)
	in.DeepCopyInto(out)
	return out
}

// ---- KBBlobSource ----

func (in *KBBlobSource) DeepCopyInto(out *KBBlobSource) {
	*out = *in
	if in.CredentialSecretRef != nil {
		out.CredentialSecretRef = new(LocalObjectReference)
		*out.CredentialSecretRef = *in.CredentialSecretRef
	}
	if in.Include != nil {
		out.Include = make([]string, len(in.Include))
		copy(out.Include, in.Include)
	}
	if in.Exclude != nil {
		out.Exclude = make([]string, len(in.Exclude))
		copy(out.Exclude, in.Exclude)
	}
}

func (in *KBBlobSource) DeepCopy() *KBBlobSource {
	if in == nil {
		return nil
	}
	out := new(KBBlobSource)
	in.DeepCopyInto(out)
	return out
}

// ---- KBIndexConfig ----

func (in *KBIndexConfig) DeepCopyInto(out *KBIndexConfig) {
	*out = *in
	if in.EmbeddingModelRef != nil {
		out.EmbeddingModelRef = new(LocalObjectReference)
		*out.EmbeddingModelRef = *in.EmbeddingModelRef
	}
}

func (in *KBIndexConfig) DeepCopy() *KBIndexConfig {
	if in == nil {
		return nil
	}
	out := new(KBIndexConfig)
	in.DeepCopyInto(out)
	return out
}

// ---- KBRefreshPolicy ----

func (in *KBRefreshPolicy) DeepCopyInto(out *KBRefreshPolicy) {
	*out = *in
}

func (in *KBRefreshPolicy) DeepCopy() *KBRefreshPolicy {
	if in == nil {
		return nil
	}
	out := new(KBRefreshPolicy)
	in.DeepCopyInto(out)
	return out
}

// ---- KBSource ----

func (in *KBSource) DeepCopyInto(out *KBSource) {
	*out = *in
	if in.Git != nil {
		out.Git = new(KBGitSource)
		*out.Git = *in.Git
	}
	if in.ConfigMap != nil {
		out.ConfigMap = new(LocalObjectReference)
		*out.ConfigMap = *in.ConfigMap
	}
	if in.Blob != nil {
		out.Blob = new(KBBlobSource)
		in.Blob.DeepCopyInto(out.Blob)
	}
}

func (in *KBSource) DeepCopy() *KBSource {
	if in == nil {
		return nil
	}
	out := new(KBSource)
	in.DeepCopyInto(out)
	return out
}

// ---- KnowledgeBaseClassSpec ----

func (in *KnowledgeBaseClassSpec) DeepCopyInto(out *KnowledgeBaseClassSpec) {
	*out = *in
	if in.EmbeddingModelRef != nil {
		out.EmbeddingModelRef = new(LocalObjectReference)
		*out.EmbeddingModelRef = *in.EmbeddingModelRef
	}
	if in.ChunkStrategy != nil {
		out.ChunkStrategy = new(KBChunkStrategy)
		*out.ChunkStrategy = *in.ChunkStrategy
	}
	if in.VectorStore != nil {
		out.VectorStore = new(KBVectorStore)
		in.VectorStore.DeepCopyInto(out.VectorStore)
	}
	if in.BackendRef != nil {
		out.BackendRef = new(KnowledgeBackendReference)
		*out.BackendRef = *in.BackendRef
	}
	if in.Parameters != nil {
		out.Parameters = make(map[string]string, len(in.Parameters))
		for k, v := range in.Parameters {
			out.Parameters[k] = v
		}
	}
}

func (in *KnowledgeBaseClassSpec) DeepCopy() *KnowledgeBaseClassSpec {
	if in == nil {
		return nil
	}
	out := new(KnowledgeBaseClassSpec)
	in.DeepCopyInto(out)
	return out
}

// ---- KnowledgeBaseClass ----

func (in *KnowledgeBaseClass) DeepCopyInto(out *KnowledgeBaseClass) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
}

func (in *KnowledgeBaseClass) DeepCopy() *KnowledgeBaseClass {
	if in == nil {
		return nil
	}
	out := new(KnowledgeBaseClass)
	in.DeepCopyInto(out)
	return out
}

func (in *KnowledgeBaseClass) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// ---- KnowledgeBaseClassList ----

func (in *KnowledgeBaseClassList) DeepCopyInto(out *KnowledgeBaseClassList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]KnowledgeBaseClass, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *KnowledgeBaseClassList) DeepCopy() *KnowledgeBaseClassList {
	if in == nil {
		return nil
	}
	out := new(KnowledgeBaseClassList)
	in.DeepCopyInto(out)
	return out
}

func (in *KnowledgeBaseClassList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// ---- KnowledgeBaseSpec ----

func (in *KnowledgeBaseSpec) DeepCopyInto(out *KnowledgeBaseSpec) {
	*out = *in
	if in.Sources != nil {
		out.Sources = make([]KBSource, len(in.Sources))
		for i := range in.Sources {
			in.Sources[i].DeepCopyInto(&out.Sources[i])
		}
	}
	if in.IndexConfig != nil {
		out.IndexConfig = new(KBIndexConfig)
		in.IndexConfig.DeepCopyInto(out.IndexConfig)
	}
	if in.RefreshPolicy != nil {
		out.RefreshPolicy = new(KBRefreshPolicy)
		*out.RefreshPolicy = *in.RefreshPolicy
	}
}

func (in *KnowledgeBaseSpec) DeepCopy() *KnowledgeBaseSpec {
	if in == nil {
		return nil
	}
	out := new(KnowledgeBaseSpec)
	in.DeepCopyInto(out)
	return out
}

// ---- KnowledgeBaseStatus ----

func (in *KnowledgeBaseStatus) DeepCopyInto(out *KnowledgeBaseStatus) {
	*out = *in
	if in.BackendBinding != nil {
		out.BackendBinding = new(KnowledgeBackendBinding)
		*out.BackendBinding = *in.BackendBinding
		if in.BackendBinding.EmbeddingModelBinding != nil {
			out.BackendBinding.EmbeddingModelBinding = new(ModelVersionBinding)
			*out.BackendBinding.EmbeddingModelBinding = *in.BackendBinding.EmbeddingModelBinding
		}
	}
	if in.Conditions != nil {
		out.Conditions = make([]v1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func (in *KnowledgeBaseStatus) DeepCopy() *KnowledgeBaseStatus {
	if in == nil {
		return nil
	}
	out := new(KnowledgeBaseStatus)
	in.DeepCopyInto(out)
	return out
}

// ---- KnowledgeBase ----

func (in *KnowledgeBase) DeepCopyInto(out *KnowledgeBase) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *KnowledgeBase) DeepCopy() *KnowledgeBase {
	if in == nil {
		return nil
	}
	out := new(KnowledgeBase)
	in.DeepCopyInto(out)
	return out
}

func (in *KnowledgeBase) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// ---- KnowledgeBaseList ----

func (in *KnowledgeBaseList) DeepCopyInto(out *KnowledgeBaseList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]KnowledgeBase, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *KnowledgeBaseList) DeepCopy() *KnowledgeBaseList {
	if in == nil {
		return nil
	}
	out := new(KnowledgeBaseList)
	in.DeepCopyInto(out)
	return out
}

func (in *KnowledgeBaseList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// ---- KnowledgeBindingRecord ----

func (in *KnowledgeBindingRecord) DeepCopyInto(out *KnowledgeBindingRecord) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Spec = in.Spec
	if in.Spec.Binding.EmbeddingModelBinding != nil {
		out.Spec.Binding.EmbeddingModelBinding = new(ModelVersionBinding)
		*out.Spec.Binding.EmbeddingModelBinding = *in.Spec.Binding.EmbeddingModelBinding
	}
}

func (in *KnowledgeBindingRecord) DeepCopy() *KnowledgeBindingRecord {
	if in == nil {
		return nil
	}
	out := new(KnowledgeBindingRecord)
	in.DeepCopyInto(out)
	return out
}

func (in *KnowledgeBindingRecord) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// ---- KnowledgeBindingRecordList ----

func (in *KnowledgeBindingRecordList) DeepCopyInto(out *KnowledgeBindingRecordList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]KnowledgeBindingRecord, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *KnowledgeBindingRecordList) DeepCopy() *KnowledgeBindingRecordList {
	if in == nil {
		return nil
	}
	out := new(KnowledgeBindingRecordList)
	in.DeepCopyInto(out)
	return out
}

func (in *KnowledgeBindingRecordList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// ---- KBHybridWeights ----

func (in *KBHybridWeights) DeepCopyInto(out *KBHybridWeights) {
	*out = *in
}

func (in *KBHybridWeights) DeepCopy() *KBHybridWeights {
	if in == nil {
		return nil
	}
	out := new(KBHybridWeights)
	in.DeepCopyInto(out)
	return out
}

// ---- KBRetrievalOverride ----

func (in *KBRetrievalOverride) DeepCopyInto(out *KBRetrievalOverride) {
	*out = *in
	if in.TopK != nil {
		out.TopK = new(int32)
		*out.TopK = *in.TopK
	}
	if in.Filter != nil {
		out.Filter = make(map[string]string, len(in.Filter))
		for k, v := range in.Filter {
			out.Filter[k] = v
		}
	}
	if in.HybridWeights != nil {
		out.HybridWeights = new(KBHybridWeights)
		*out.HybridWeights = *in.HybridWeights
	}
}

func (in *KBRetrievalOverride) DeepCopy() *KBRetrievalOverride {
	if in == nil {
		return nil
	}
	out := new(KBRetrievalOverride)
	in.DeepCopyInto(out)
	return out
}

// ---- KBUsageQuota ----

func (in *KBUsageQuota) DeepCopyInto(out *KBUsageQuota) {
	*out = *in
	if in.MaxQueriesPerMinute != nil {
		out.MaxQueriesPerMinute = new(int32)
		*out.MaxQueriesPerMinute = *in.MaxQueriesPerMinute
	}
	if in.MaxTokensReturned != nil {
		out.MaxTokensReturned = new(int32)
		*out.MaxTokensReturned = *in.MaxTokensReturned
	}
}

func (in *KBUsageQuota) DeepCopy() *KBUsageQuota {
	if in == nil {
		return nil
	}
	out := new(KBUsageQuota)
	in.DeepCopyInto(out)
	return out
}

// ---- KnowledgeBaseClaimSpec ----

func (in *KnowledgeBaseClaimSpec) DeepCopyInto(out *KnowledgeBaseClaimSpec) {
	*out = *in
	if in.Retrieval != nil {
		out.Retrieval = new(KBRetrievalOverride)
		in.Retrieval.DeepCopyInto(out.Retrieval)
	}
	if in.Usage != nil {
		out.Usage = new(KBUsageQuota)
		in.Usage.DeepCopyInto(out.Usage)
	}
}

func (in *KnowledgeBaseClaimSpec) DeepCopy() *KnowledgeBaseClaimSpec {
	if in == nil {
		return nil
	}
	out := new(KnowledgeBaseClaimSpec)
	in.DeepCopyInto(out)
	return out
}

// ---- KnowledgeBaseClaimStatus ----

func (in *KnowledgeBaseClaimStatus) DeepCopyInto(out *KnowledgeBaseClaimStatus) {
	*out = *in
	if in.Conditions != nil {
		out.Conditions = make([]v1.Condition, len(in.Conditions))
		for i := range in.Conditions {
			in.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}
}

func (in *KnowledgeBaseClaimStatus) DeepCopy() *KnowledgeBaseClaimStatus {
	if in == nil {
		return nil
	}
	out := new(KnowledgeBaseClaimStatus)
	in.DeepCopyInto(out)
	return out
}

// ---- KnowledgeBaseClaim ----

func (in *KnowledgeBaseClaim) DeepCopyInto(out *KnowledgeBaseClaim) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	in.Spec.DeepCopyInto(&out.Spec)
	in.Status.DeepCopyInto(&out.Status)
}

func (in *KnowledgeBaseClaim) DeepCopy() *KnowledgeBaseClaim {
	if in == nil {
		return nil
	}
	out := new(KnowledgeBaseClaim)
	in.DeepCopyInto(out)
	return out
}

func (in *KnowledgeBaseClaim) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}

// ---- KnowledgeBaseClaimList ----

func (in *KnowledgeBaseClaimList) DeepCopyInto(out *KnowledgeBaseClaimList) {
	*out = *in
	out.TypeMeta = in.TypeMeta
	in.ListMeta.DeepCopyInto(&out.ListMeta)
	if in.Items != nil {
		out.Items = make([]KnowledgeBaseClaim, len(in.Items))
		for i := range in.Items {
			in.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

func (in *KnowledgeBaseClaimList) DeepCopy() *KnowledgeBaseClaimList {
	if in == nil {
		return nil
	}
	out := new(KnowledgeBaseClaimList)
	in.DeepCopyInto(out)
	return out
}

func (in *KnowledgeBaseClaimList) DeepCopyObject() runtime.Object {
	if c := in.DeepCopy(); c != nil {
		return c
	}
	return nil
}
