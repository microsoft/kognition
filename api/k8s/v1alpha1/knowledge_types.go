// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Provisioner",type=string,JSONPath=`.spec.provisioner`
// +kubebuilder:printcolumn:name="IndexMode",type=string,JSONPath=`.spec.indexMode`
// +kubebuilder:printcolumn:name="Reclaim",type=string,JSONPath=`.spec.reclaimPolicy`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// KnowledgeBaseClass defines a class of knowledge-base provisioner (cluster-scoped).
type KnowledgeBaseClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              KnowledgeBaseClassSpec `json:"spec,omitempty"`
}

// KnowledgeBaseClassSpec defines the provisioner and default configuration for
// knowledge bases of this class.
// +kubebuilder:validation:XValidation:rule="!(has(self.backendRef) && has(self.vectorStore))",message="backendRef and vectorStore are mutually exclusive"
type KnowledgeBaseClassSpec struct {
	// Provisioner is the driver identifier (e.g. "inline", "pgvector", "qdrant").
	Provisioner string `json:"provisioner"`

	// IndexMode selects the vector index strategy.
	// +kubebuilder:validation:Enum=dense;sparse;hybrid;inline
	// +optional
	IndexMode string `json:"indexMode,omitempty"`

	// EmbeddingModelRef references the model used to produce embeddings.
	// Ignored by the "inline" provisioner.
	// +optional
	EmbeddingModelRef *LocalObjectReference `json:"embeddingModelRef,omitempty"`

	// ChunkStrategy defines default chunking behaviour for ingested documents.
	// +optional
	ChunkStrategy *KBChunkStrategy `json:"chunkStrategy,omitempty"`

	// ReclaimPolicy controls what happens to the index storage when a
	// KnowledgeBase is deleted.
	// +optional
	// +kubebuilder:default=Retain
	ReclaimPolicy ReclaimPolicy `json:"reclaimPolicy,omitempty"`

	// VectorStore configures the legacy pgvector backend.
	// +optional
	VectorStore *KBVectorStore `json:"vectorStore,omitempty"`
	// +optional
	BackendRef *KnowledgeBackendReference `json:"backendRef,omitempty"`

	// Parameters are provisioner-specific key-value configuration pairs.
	// +optional
	Parameters map[string]string `json:"parameters,omitempty"`
}

type KnowledgeBackendReference struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	APIGroup string `json:"apiGroup"`
	// +kubebuilder:validation:Pattern=`^[A-Z][A-Za-z0-9]{0,62}$`
	Kind string `json:"kind"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`
	Name string `json:"name"`
}

// KBVectorStore configures the backing vector store used by a
// KnowledgeBaseClass. Exactly one provider-specific block must match Type.
type KBVectorStore struct {
	// Type discriminates the provider.
	// +kubebuilder:validation:Enum=pgvector
	Type string `json:"type"`

	// Pgvector is required when Type is "pgvector".
	// +optional
	Pgvector *KBPgvectorStore `json:"pgvector,omitempty"`
}

// KBPgvectorStore configures a Postgres+pgvector backend. The Secret
// referenced by ConnectionSecretRef must carry the Postgres connection string
// under key "connectionString" (a libpq-style DSN).
type KBPgvectorStore struct {
	// ConnectionSecretRef references a Secret carrying the Postgres DSN.
	ConnectionSecretRef LocalObjectReference `json:"connectionSecretRef"`

	// Table is the Postgres table storing chunk rows. Defaults to
	// "kognition_kb_chunks".
	// +optional
	Table string `json:"table,omitempty"`

	// Dimension is the embedding dimension used when provisioning the vector
	// column. Defaults to 1536.
	// +optional
	Dimension int32 `json:"dimension,omitempty"`
}

// KBChunkStrategy controls how source documents are split before indexing.
type KBChunkStrategy struct {
	// Size is the target chunk size in tokens.
	// +optional
	Size int32 `json:"size,omitempty"`

	// Overlap is the number of tokens shared between adjacent chunks.
	// +optional
	Overlap int32 `json:"overlap,omitempty"`

	// Mode selects the splitting algorithm.
	// +kubebuilder:validation:Enum=fixed;sentence;recursive
	// +optional
	Mode string `json:"mode,omitempty"`
}

// +kubebuilder:object:root=true

// KnowledgeBaseClassList contains a list of KnowledgeBaseClasses.
type KnowledgeBaseClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KnowledgeBaseClass `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Class",type=string,JSONPath=`.spec.knowledgeBaseClassName`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Docs",type=integer,JSONPath=`.status.documentCount`
// +kubebuilder:printcolumn:name="Chunks",type=integer,JSONPath=`.status.indexedChunks`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// KnowledgeBase declares a queryable knowledge store (namespace-scoped).
type KnowledgeBase struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              KnowledgeBaseSpec   `json:"spec,omitempty"`
	Status            KnowledgeBaseStatus `json:"status,omitempty"`
}

// KnowledgeBaseSpec defines the desired state of a KnowledgeBase.
type KnowledgeBaseSpec struct {
	// KnowledgeBaseClassName references the KnowledgeBaseClass that provisions
	// the backing index for this knowledge base.
	KnowledgeBaseClassName string `json:"knowledgeBaseClassName"`

	// Sources lists the data sources to ingest into this knowledge base.
	// +optional
	Sources []KBSource `json:"sources,omitempty"`

	// IndexConfig overrides class-level chunking and embedding settings.
	// +optional
	IndexConfig *KBIndexConfig `json:"indexConfig,omitempty"`

	// RefreshPolicy controls when and how the index is rebuilt.
	// +optional
	RefreshPolicy *KBRefreshPolicy `json:"refreshPolicy,omitempty"`
}

// KBSource defines a single data source for ingestion.
type KBSource struct {
	// Type discriminates the source variant.
	// +kubebuilder:validation:Enum=inline;url;file;directory;git;configMap;blob
	Type string `json:"type"`

	// Data holds inline document content (when Type is "inline").
	// +optional
	Data string `json:"data,omitempty"`

	// URI is the URL or file path (when Type is "url" or "file").
	// +optional
	URI string `json:"uri,omitempty"`

	// Directory is a filesystem path to ingest recursively (when Type is "directory").
	// +optional
	Directory string `json:"directory,omitempty"`

	// Git configures a Git repository source (when Type is "git").
	// +optional
	Git *KBGitSource `json:"git,omitempty"`

	// ConfigMap references a ConfigMap whose data values are ingested
	// (when Type is "configMap").
	// +optional
	ConfigMap *LocalObjectReference `json:"configMap,omitempty"`

	// Blob configures an object-storage source (when Type is "blob").
	// Supports Azure Blob Storage and S3-compatible providers.
	// +optional
	Blob *KBBlobSource `json:"blob,omitempty"`
}

// KBGitSource identifies a Git repository and optional sub-path to ingest.
type KBGitSource struct {
	// URL is the HTTPS or SSH clone URL.
	URL string `json:"url"`

	// Branch is the branch, tag, or SHA to check out (default: "main").
	// +optional
	Branch string `json:"branch,omitempty"`

	// Path restricts ingestion to the given sub-directory within the repo.
	// +optional
	Path string `json:"path,omitempty"`
}

// KBBlobSource identifies an object-storage location (Azure Blob or S3) whose
// objects are ingested into the knowledge base. Provider selects the backend;
// a single provisioner reads the appropriate credentials out of the referenced
// secret.
type KBBlobSource struct {
	// Provider selects the object-storage backend.
	// +kubebuilder:validation:Enum=azure;s3
	Provider string `json:"provider"`

	// Bucket is the container name (for "azure") or bucket name (for "s3").
	Bucket string `json:"bucket"`

	// Prefix restricts ingestion to objects whose key starts with this prefix.
	// +optional
	Prefix string `json:"prefix,omitempty"`

	// Region is the storage region. Required for "s3" unless Endpoint is set.
	// Ignored for "azure".
	// +optional
	Region string `json:"region,omitempty"`

	// Endpoint overrides the default service URL. Used for S3-compatible
	// services (MinIO, Ceph, R2) or Azure sovereign clouds.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// CredentialSecretRef points at a Secret in the same namespace holding
	// provider credentials. Expected keys per provider:
	//   azure: "connectionString", or ("accountName", "accountKey"),
	//          or ("accountName", "sasToken").
	//   s3:    "accessKeyId" and "secretAccessKey", plus optional
	//          "sessionToken" for temporary credentials.
	// May be omitted only when the provisioner uses workload identity.
	// +optional
	CredentialSecretRef *LocalObjectReference `json:"credentialSecretRef,omitempty"`

	// Include is a list of glob patterns; only matching objects are ingested.
	// +optional
	Include []string `json:"include,omitempty"`

	// Exclude is a list of glob patterns; matching objects are skipped.
	// +optional
	Exclude []string `json:"exclude,omitempty"`
}

// KBIndexConfig overrides class-level chunking and embedding configuration.
type KBIndexConfig struct {
	// EmbeddingModelRef overrides the class-level embedding model.
	// +optional
	EmbeddingModelRef *LocalObjectReference `json:"embeddingModelRef,omitempty"`

	// ChunkSize is the target chunk size in tokens.
	// +optional
	ChunkSize int32 `json:"chunkSize,omitempty"`

	// ChunkOverlap is the number of overlapping tokens between adjacent chunks.
	// +optional
	ChunkOverlap int32 `json:"chunkOverlap,omitempty"`
}

// KBRefreshPolicy controls when and how the knowledge base index is rebuilt.
type KBRefreshPolicy struct {
	// Mode selects the trigger mechanism.
	// +kubebuilder:validation:Enum=periodic;onDemand;webhook
	Mode string `json:"mode"`

	// Interval is the refresh cadence for "periodic" mode (e.g. "24h", "6h").
	// +optional
	Interval string `json:"interval,omitempty"`

	// WebhookPath is the API path that triggers a refresh in "webhook" mode.
	// +optional
	WebhookPath string `json:"webhookPath,omitempty"`
}

// KnowledgeBaseStatus describes the observed state of a KnowledgeBase.
type KnowledgeBaseStatus struct {
	// Phase is the lifecycle phase (Pending|Indexing|Ready|Refreshing|Failed).
	// +optional
	Phase string `json:"phase,omitempty"`

	// Conditions represent the latest available observations.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// DocumentCount is the number of source documents successfully ingested.
	// +optional
	DocumentCount int64 `json:"documentCount,omitempty"`

	// IndexedChunks is the total number of chunks written to the index.
	// +optional
	IndexedChunks int64 `json:"indexedChunks,omitempty"`

	// Generation is a monotonically increasing counter that increments each
	// time the index is successfully rebuilt.
	// +optional
	Generation int64 `json:"generation,omitempty"`

	// LastRefresh is the RFC 3339 timestamp of the most recent successful
	// index build.
	// +optional
	LastRefresh    string                   `json:"lastRefresh,omitempty"`
	BackendBinding *KnowledgeBackendBinding `json:"backendBinding,omitempty"`
}

type KnowledgeBackendBinding struct {
	// +kubebuilder:validation:Minimum=1
	Generation int64 `json:"generation"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	ProviderName string `json:"providerName"`
	// +kubebuilder:validation:Enum=Delete;Retain;Archive
	ReclaimPolicy ReclaimPolicy `json:"reclaimPolicy,omitempty"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	ClassUID string `json:"classUID"`
	// +kubebuilder:validation:Minimum=1
	ClassGeneration int64 `json:"classGeneration"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	ConfigurationAPIGroup string `json:"configurationAPIGroup"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	ConfigurationKind string `json:"configurationKind"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	ConfigurationName string `json:"configurationName"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	ConfigurationUID string `json:"configurationUID"`
	// +kubebuilder:validation:Pattern=`^[a-f0-9]{64}$`
	ConfigurationDigest string `json:"configurationDigest"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	OperationID string `json:"operationID"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=1024
	BackendOperationID string `json:"backendOperationID"`
	// EmbeddingModelBinding pins the configuration that produced the committed vectors.
	// +optional
	EmbeddingModelBinding *ModelVersionBinding `json:"embeddingModelBinding,omitempty"`
}

// +kubebuilder:object:root=true

// KnowledgeBaseList contains a list of KnowledgeBases.
type KnowledgeBaseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KnowledgeBase `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="KnowledgeBase",type=string,JSONPath=`.spec.knowledgeBaseName`
// +kubebuilder:printcolumn:name="AccessMode",type=string,JSONPath=`.spec.accessMode`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Bound",type=string,JSONPath=`.status.boundKnowledgeBase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// KnowledgeBaseClaim binds an agent (or agents) to an existing KnowledgeBase
// in the same namespace, carrying per-claim access mode, retrieval overrides,
// and usage quotas. Unlike MemoryClaim, a KnowledgeBaseClaim does not
// provision a private backing store — the KnowledgeBase it references is a
// shared corpus. Multiple claims may reference the same KnowledgeBase.
type KnowledgeBaseClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              KnowledgeBaseClaimSpec   `json:"spec,omitempty"`
	Status            KnowledgeBaseClaimStatus `json:"status,omitempty"`
}

// KnowledgeBaseClaimSpec defines the desired binding between a claim and a
// KnowledgeBase.
type KnowledgeBaseClaimSpec struct {
	// KnowledgeBaseName is the KnowledgeBase in the same namespace this claim
	// binds to.
	KnowledgeBaseName string `json:"knowledgeBaseName"`

	// AccessMode is "ReadOnly" (default) or "ReadWrite". ReadWrite grants any
	// tool wired through this claim permission to add or delete documents on
	// the underlying KnowledgeBase.
	// +kubebuilder:validation:Enum=ReadOnly;ReadWrite
	// +kubebuilder:default=ReadOnly
	// +optional
	AccessMode string `json:"accessMode,omitempty"`

	// Retrieval overrides class-level and KnowledgeBase-level defaults for
	// queries made through this claim. Overrides never mutate the corpus.
	// +optional
	Retrieval *KBRetrievalOverride `json:"retrieval,omitempty"`

	// Usage caps enforced by governance/quota admission at query time.
	// +optional
	Usage *KBUsageQuota `json:"usage,omitempty"`
}

// KBRetrievalOverride carries per-claim overrides for the KnowledgeBase's
// default query behaviour.
type KBRetrievalOverride struct {
	// +kubebuilder:validation:Enum=keyword;dense;hybrid
	// +optional
	Mode string `json:"mode,omitempty"`

	// TopK is the maximum number of results returned per query.
	// +optional
	TopK *int32 `json:"topK,omitempty"`

	// MinScore filters out results below this relevance score. Decimal string
	// (e.g. "0.35") to keep the CRD field type-stable across providers.
	// +optional
	MinScore string `json:"minScore,omitempty"`

	// Filter is a set of key=value constraints applied to document metadata.
	// +optional
	Filter map[string]string `json:"filter,omitempty"`

	// HybridWeights tunes the mix of keyword vs vector scoring when the
	// underlying class supports hybrid retrieval.
	// +optional
	HybridWeights *KBHybridWeights `json:"hybridWeights,omitempty"`
}

// KBHybridWeights carries the keyword/vector mix for hybrid retrieval.
// Weights are decimal strings (e.g. "0.4") so the CRD does not depend on
// floating-point representation across serializers.
type KBHybridWeights struct {
	// Keyword weight (0.0 - 1.0 as a decimal string).
	// +optional
	Keyword string `json:"keyword,omitempty"`

	// Vector weight (0.0 - 1.0 as a decimal string).
	// +optional
	Vector string `json:"vector,omitempty"`
}

// KBUsageQuota caps how aggressively a claim may query its KnowledgeBase.
type KBUsageQuota struct {
	// MaxQueriesPerMinute caps queries per minute across all consumers of this
	// claim.
	// +optional
	MaxQueriesPerMinute *int32 `json:"maxQueriesPerMinute,omitempty"`

	// MaxTokensReturned caps the total tokens returned per query.
	// +optional
	MaxTokensReturned *int32 `json:"maxTokensReturned,omitempty"`
}

// KnowledgeBaseClaimStatus describes the observed state of a
// KnowledgeBaseClaim.
type KnowledgeBaseClaimStatus struct {
	// Phase is the lifecycle phase (Pending|Bound|Lost).
	// +optional
	Phase string `json:"phase,omitempty"`

	// BoundKnowledgeBase mirrors spec.knowledgeBaseName once binding succeeds.
	// It is cleared if the underlying KnowledgeBase is deleted.
	// +optional
	BoundKnowledgeBase string `json:"boundKnowledgeBase,omitempty"`

	// ObservedGeneration is the .metadata.generation this status was produced
	// from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions represent the latest available observations.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true

// KnowledgeBaseClaimList contains a list of KnowledgeBaseClaims.
type KnowledgeBaseClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KnowledgeBaseClaim `json:"items"`
}

func init() {
	SchemeBuilder.Register(
		&KnowledgeBaseClass{}, &KnowledgeBaseClassList{},
		&KnowledgeBase{}, &KnowledgeBaseList{},
		&KnowledgeBaseClaim{}, &KnowledgeBaseClaimList{},
	)
}
