// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import "time"

// Phase extensions for memory and knowledge-base resources.
const (
	PhaseAvailable  Phase = "Available"
	PhaseBound      Phase = "Bound"
	PhaseReleased   Phase = "Released"
	PhaseDeleted    Phase = "Deleted"
	PhaseRetained   Phase = "Retained"
	PhaseArchived   Phase = "Archived"
	PhaseIndexing   Phase = "Indexing"
	PhaseRefreshing Phase = "Refreshing"
	// PhaseSummarized marks a MemoryRecord that has been absorbed into a
	// summary record by the background summarization controller.
	PhaseSummarized Phase = "Summarized"
	// PhaseLost marks a KnowledgeBaseClaim whose bound KnowledgeBase has been
	// deleted or is no longer Ready after having been Bound at least once.
	PhaseLost Phase = "Lost"
)

// ReclaimPolicy controls what happens to a MemoryVolume when its owning claim
// is deleted.
type ReclaimPolicy string

const (
	// ReclaimDelete deletes the MemoryVolume and all backing data.
	ReclaimDelete ReclaimPolicy = "Delete"

	// ReclaimRetain releases the MemoryVolume but preserves backing data for
	// manual reclamation.
	ReclaimRetain ReclaimPolicy = "Retain"

	// ReclaimArchive transitions the MemoryVolume to an archived tier rather
	// than deleting it.
	ReclaimArchive ReclaimPolicy = "Archive"
)

// MemoryType discriminates the class of memory a claim or store represents.
type MemoryType string

const (
	MemoryTypeEpisodic   MemoryType = "episodic"
	MemoryTypeSemantic   MemoryType = "semantic"
	MemoryTypeWorking    MemoryType = "working"
	MemoryTypeProcedural MemoryType = "procedural"
	MemoryTypeShared     MemoryType = "shared"
)

// ConcurrencyMode specifies how concurrent writes are resolved for shared memory.
type ConcurrencyMode string

const (
	// ConcurrencyLWW uses last-write-wins semantics.
	ConcurrencyLWW ConcurrencyMode = "lww"

	// ConcurrencyCRDT uses conflict-free replicated data type merging.
	ConcurrencyCRDT ConcurrencyMode = "crdt"

	// ConcurrencyLock uses pessimistic locking before each write.
	ConcurrencyLock ConcurrencyMode = "lock"
)

// MemoryClass defines a class of memory provisioner.
type MemoryClass struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       MemoryClassSpec   `json:"spec" yaml:"spec"`
	Status     MemoryClassStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

func (m *MemoryClass) GetTypeMeta() TypeMeta      { return m.TypeMeta }
func (m *MemoryClass) GetObjectMeta() *ObjectMeta { return &m.ObjectMeta }

// MemoryClassStatus reports the observed state of a MemoryClass provisioner.
type MemoryClassStatus struct {
	Phase      Phase       `json:"phase,omitempty" yaml:"phase,omitempty"`
	Message    string      `json:"message,omitempty" yaml:"message,omitempty"`
	Conditions []Condition `json:"conditions,omitempty" yaml:"conditions,omitempty"`
}

// MemoryClassSpec defines the provisioner and policy for a class of memory.
type MemoryClassSpec struct {
	// BackendType identifies the storage backend (e.g. "pgvector", "redis", "in-memory").
	BackendType string `json:"backendType" yaml:"backendType"`

	// Parameters are provisioner-specific configuration passed verbatim to the
	// backend driver.
	Parameters map[string]string `json:"parameters,omitempty" yaml:"parameters,omitempty"`

	// ReclaimPolicy controls MemoryVolume lifecycle after a claim is deleted.
	// Defaults to Delete.
	ReclaimPolicy ReclaimPolicy `json:"reclaimPolicy,omitempty" yaml:"reclaimPolicy,omitempty"`

	// SummarizationConfig configures automatic summarization for episodic volumes.
	SummarizationConfig *MemorySummarizationConfig `json:"summarizationConfig,omitempty" yaml:"summarizationConfig,omitempty"`

	// ConsistencyMode specifies the consistency guarantee offered by this class
	// (e.g. "eventual", "strong").
	ConsistencyMode string `json:"consistencyMode,omitempty" yaml:"consistencyMode,omitempty"`
}

// MemorySummarizationConfig configures automatic summarization of memory records.
type MemorySummarizationConfig struct {
	// Enabled activates summarization when the record count exceeds ThresholdRecords.
	Enabled bool `json:"enabled" yaml:"enabled"`

	// ThresholdRecords is the record count that triggers a summarization pass.
	ThresholdRecords int32 `json:"thresholdRecords,omitempty" yaml:"thresholdRecords,omitempty"`

	// ModelRef references the model used to generate summaries.
	ModelRef *LocalObjectReference `json:"modelRef,omitempty" yaml:"modelRef,omitempty"`
}

// MemoryClaim requests memory resources from a MemoryClass.
type MemoryClaim struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       MemoryClaimSpec   `json:"spec" yaml:"spec"`
	Status     MemoryClaimStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

func (m *MemoryClaim) GetTypeMeta() TypeMeta      { return m.TypeMeta }
func (m *MemoryClaim) GetObjectMeta() *ObjectMeta { return &m.ObjectMeta }

// MemoryClaimSpec defines the desired memory allocation.
type MemoryClaimSpec struct {
	// MemoryClassName references the MemoryClass to provision from.
	MemoryClassName string `json:"memoryClassName" yaml:"memoryClassName"`

	// Type discriminates the memory model requested.
	Type MemoryType `json:"type" yaml:"type"`

	// Capacity is the requested storage size (e.g., "100Mi", "1Gi").
	Capacity string `json:"capacity,omitempty" yaml:"capacity,omitempty"`

	// ConsistencyMode overrides the class-level consistency mode for this claim.
	ConsistencyMode string `json:"consistencyMode,omitempty" yaml:"consistencyMode,omitempty"`

	// ConcurrencyMode specifies write-conflict resolution for shared memory.
	// Only relevant when Type is "shared".
	ConcurrencyMode ConcurrencyMode `json:"concurrencyMode,omitempty" yaml:"concurrencyMode,omitempty"`

	// AccessModes defines how the memory can be accessed (ReadWrite, ReadOnly, Shared).
	AccessModes []string `json:"accessModes,omitempty" yaml:"accessModes,omitempty"`
}

// MemoryClaimStatus describes the observed state of a MemoryClaim.
type MemoryClaimStatus struct {
	Phase           Phase       `json:"phase,omitempty" yaml:"phase,omitempty"`
	BoundVolumeName string      `json:"boundVolumeName,omitempty" yaml:"boundVolumeName,omitempty"`
	Conditions      []Condition `json:"conditions,omitempty" yaml:"conditions,omitempty"`
}

// MemoryVolume is the provisioned backing store for a MemoryClaim.
type MemoryVolume struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       MemoryVolumeSpec   `json:"spec" yaml:"spec"`
	Status     MemoryVolumeStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

func (m *MemoryVolume) GetTypeMeta() TypeMeta      { return m.TypeMeta }
func (m *MemoryVolume) GetObjectMeta() *ObjectMeta { return &m.ObjectMeta }

// MemoryVolumeSpec defines the provisioned memory volume.
type MemoryVolumeSpec struct {
	// MemoryClassName is the class that provisioned this volume.
	MemoryClassName string `json:"memoryClassName" yaml:"memoryClassName"`

	// ClaimRef is the claim that this volume is bound to.
	ClaimRef *LocalObjectReference `json:"claimRef,omitempty" yaml:"claimRef,omitempty"`

	// BackendHandle is the provisioner-specific identifier for the backing store
	// (e.g. a database schema name, Redis key prefix, or collection ID).
	BackendHandle string `json:"backendHandle,omitempty" yaml:"backendHandle,omitempty"`

	// Capacity is the provisioned storage size.
	Capacity string `json:"capacity,omitempty" yaml:"capacity,omitempty"`
}

// MemoryVolumeStatus describes the state of a provisioned volume.
type MemoryVolumeStatus struct {
	Phase      Phase       `json:"phase,omitempty" yaml:"phase,omitempty"`
	Capacity   string      `json:"capacity,omitempty" yaml:"capacity,omitempty"`
	Conditions []Condition `json:"conditions,omitempty" yaml:"conditions,omitempty"`
}

// Memory represents an agent-owned memory record store with type discrimination.
// It binds a MemoryClaim to an agent context and exposes operations (persist,
// retrieve, summarize, expire) through the cognitive API.
type Memory struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       MemorySpec   `json:"spec" yaml:"spec"`
	Status     MemoryStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

func (m *Memory) GetTypeMeta() TypeMeta      { return m.TypeMeta }
func (m *Memory) GetObjectMeta() *ObjectMeta { return &m.ObjectMeta }

// MemorySpec defines the desired state of an agent Memory store.
type MemorySpec struct {
	// ClaimRef references the MemoryClaim that backs this memory store.
	ClaimRef LocalObjectReference `json:"claimRef" yaml:"claimRef"`

	// Type discriminates the memory model (episodic, semantic, working,
	// procedural, shared).
	Type MemoryType `json:"type" yaml:"type"`

	// TTL is the time-to-live for records in this store (e.g., "24h", "7d").
	// If empty, records do not expire automatically.
	TTL string `json:"ttl,omitempty" yaml:"ttl,omitempty"`

	// RetentionPolicy controls record management at capacity ("truncate",
	// "summarize").
	RetentionPolicy string `json:"retentionPolicy,omitempty" yaml:"retentionPolicy,omitempty"`
}

// MemoryStatus describes the observed state of a Memory store.
type MemoryStatus struct {
	Phase      Phase       `json:"phase,omitempty" yaml:"phase,omitempty"`
	Conditions []Condition `json:"conditions,omitempty" yaml:"conditions,omitempty"`
	// RecordCount is the number of non-summarized, non-deleted MemoryRecords.
	RecordCount int64 `json:"recordCount,omitempty" yaml:"recordCount,omitempty"`
	BytesUsed   int64 `json:"bytesUsed,omitempty" yaml:"bytesUsed,omitempty"`
}

// MemoryProvenance captures the authorship and execution context of a memory
// record. All fields except GrantID are required when writing a record.
type MemoryProvenance struct {
	// SourceAgentIdentity is the name of the AgentIdentity that produced this
	// record.
	SourceAgentIdentity string `json:"sourceAgentIdentity" yaml:"sourceAgentIdentity"`

	// SessionID identifies the runtime session during which the record was
	// written. Records sharing a SessionID form a read-your-writes consistency
	// domain.
	SessionID string `json:"sessionId" yaml:"sessionId"`

	// ReasoningStepID identifies the specific reasoning step (chain-of-thought
	// node, tool call, or LLM turn) that produced the record.
	ReasoningStepID string `json:"reasoningStepId" yaml:"reasoningStepId"`

	// Timestamp is the UTC time at which the record was persisted. Auto-stamped
	// by the reconciler when zero.
	Timestamp time.Time `json:"timestamp" yaml:"timestamp"`

	// GrantID is the MemoryGrant that authorised the write. Optional — may be
	// empty for same-namespace writes that do not require a grant.
	GrantID string `json:"grantId,omitempty" yaml:"grantId,omitempty"`

	// DerivedFromRecordNames references upstream MemoryRecord names this record
	// was derived from. This is used for provenance-chain traversals such as
	// right-to-be-forgotten cascades.
	DerivedFromRecordNames []string `json:"derivedFromRecordNames,omitempty" yaml:"derivedFromRecordNames,omitempty"`
}

// MemoryRecord is a first-class resource representing a single entry persisted
// in a Memory store. It carries full provenance metadata and is reconciled by
// the MemoryRecordReconciler, which validates authorship fields and
// auto-stamps the timestamp.
type MemoryRecord struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       MemoryRecordSpec   `json:"spec" yaml:"spec"`
	Status     MemoryRecordStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

// MemoryRecordSpec holds the payload and provenance of a memory record.
type MemoryRecordSpec struct {
	// MemoryRef references the Memory store this record belongs to.
	MemoryRef LocalObjectReference `json:"memoryRef" yaml:"memoryRef"`

	// Content is the textual or serialised content of the record.
	Content string `json:"content" yaml:"content"`

	// Metadata holds arbitrary annotation key-value pairs.
	Metadata map[string]string `json:"metadata,omitempty" yaml:"metadata,omitempty"`

	// Provenance captures the authorship and execution context of this record.
	// All fields except GrantID are required; the reconciler rejects records
	// that omit them and auto-stamps Provenance.Timestamp when zero.
	Provenance *MemoryProvenance `json:"provenance,omitempty" yaml:"provenance,omitempty"`
}

// MemoryRecordStatus is the observed state of a MemoryRecord.
type MemoryRecordStatus struct {
	// Phase is the lifecycle phase of this record.
	Phase Phase `json:"phase,omitempty" yaml:"phase,omitempty"`
	// Conditions is the list of observed conditions.
	Conditions []Condition `json:"conditions,omitempty" yaml:"conditions,omitempty"`
}

func (m *MemoryRecord) GetTypeMeta() TypeMeta      { return m.TypeMeta }
func (m *MemoryRecord) GetObjectMeta() *ObjectMeta { return &m.ObjectMeta }

// KnowledgeBaseClass defines a class of knowledge base provisioner.
type KnowledgeBaseClass struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       KnowledgeBaseClassSpec `json:"spec" yaml:"spec"`
}

func (k *KnowledgeBaseClass) GetTypeMeta() TypeMeta      { return k.TypeMeta }
func (k *KnowledgeBaseClass) GetObjectMeta() *ObjectMeta { return &k.ObjectMeta }

// KnowledgeBaseClassSpec defines the provisioner and default configuration for
// knowledge bases of this class.
type KnowledgeBaseClassSpec struct {
	// Provisioner is the driver identifier (e.g. "pgvector", "qdrant", "memory").
	Provisioner string `json:"provisioner" yaml:"provisioner"`

	// IndexMode selects the vector index strategy ("dense", "sparse", "hybrid").
	IndexMode string `json:"indexMode,omitempty" yaml:"indexMode,omitempty"`

	// EmbeddingModelRef references the model used to produce embeddings.
	EmbeddingModelRef *LocalObjectReference `json:"embeddingModelRef,omitempty" yaml:"embeddingModelRef,omitempty"`

	// ChunkStrategy defines default chunking behaviour for ingested documents.
	ChunkStrategy *KBChunkStrategy `json:"chunkStrategy,omitempty" yaml:"chunkStrategy,omitempty"`

	// ReclaimPolicy controls what happens to the index storage when a
	// KnowledgeBase is deleted ("Delete", "Retain", "Archive").
	ReclaimPolicy ReclaimPolicy `json:"reclaimPolicy,omitempty" yaml:"reclaimPolicy,omitempty"`

	// VectorStore configures the legacy pgvector backend. Inline content is
	// used only when neither VectorStore nor BackendRef is configured.
	VectorStore *KBVectorStore             `json:"vectorStore,omitempty" yaml:"vectorStore,omitempty"`
	BackendRef  *KnowledgeBackendReference `json:"backendRef,omitempty" yaml:"backendRef,omitempty"`

	// Parameters are provisioner-specific key-value configuration pairs.
	Parameters map[string]string `json:"parameters,omitempty" yaml:"parameters,omitempty"`
}

type KnowledgeBackendReference struct {
	APIGroup string `json:"apiGroup" yaml:"apiGroup"`
	Kind     string `json:"kind" yaml:"kind"`
	Name     string `json:"name" yaml:"name"`
}

// KBVectorStore configures the backing vector store used by a
// KnowledgeBaseClass. Exactly one provider-specific block must be populated,
// and it must match Type.
type KBVectorStore struct {
	// Type discriminates the legacy provider ("pgvector").
	Type string `json:"type" yaml:"type"`

	// Pgvector is required when Type is "pgvector".
	Pgvector *KBPgvectorStore `json:"pgvector,omitempty" yaml:"pgvector,omitempty"`
}

// KBPgvectorStore configures a Postgres+pgvector backend. The connection
// string is sourced from the referenced Secret so credentials never live in
// the CR. Expected Secret key: "connectionString" (a libpq-style DSN, e.g.
// "postgres://user:pass@host:5432/db?sslmode=disable").
type KBPgvectorStore struct {
	// ConnectionSecretRef references a Secret in the consuming KnowledgeBase namespace
	// carrying the Postgres connection string under key "connectionString".
	ConnectionSecretRef LocalObjectReference `json:"connectionSecretRef" yaml:"connectionSecretRef"`

	// Table is the Postgres table that stores document chunks. Defaults to
	// "kognition_kb_chunks" when unset. One table serves all KBs of this
	// class; rows are keyed by (kb_namespace, kb_name, source_uri, chunk_idx).
	Table string `json:"table,omitempty" yaml:"table,omitempty"`

	// Dimension is the embedding dimension used when creating the vector
	// column. Must match the embedding model's output size. Common values:
	// 1536 (text-embedding-3-small, text-embedding-ada-002), 3072
	// (text-embedding-3-large). Defaults to 1536 when unset.
	Dimension int32 `json:"dimension,omitempty" yaml:"dimension,omitempty"`
}

// KBChunkStrategy controls how source documents are split before indexing.
type KBChunkStrategy struct {
	// Size is the target chunk size in tokens.
	Size int32 `json:"size,omitempty" yaml:"size,omitempty"`

	// Overlap is the number of tokens shared between adjacent chunks.
	Overlap int32 `json:"overlap,omitempty" yaml:"overlap,omitempty"`

	// Mode selects the splitting algorithm ("fixed", "sentence", "recursive").
	Mode string `json:"mode,omitempty" yaml:"mode,omitempty"`
}

// KnowledgeBase declares a queryable knowledge store.
type KnowledgeBase struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       KnowledgeBaseSpec   `json:"spec" yaml:"spec"`
	Status     KnowledgeBaseStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

func (k *KnowledgeBase) GetTypeMeta() TypeMeta      { return k.TypeMeta }
func (k *KnowledgeBase) GetObjectMeta() *ObjectMeta { return &k.ObjectMeta }

// KnowledgeBaseSpec defines the desired state of a KnowledgeBase.
type KnowledgeBaseSpec struct {
	// KnowledgeBaseClassName references the KnowledgeBaseClass that provisions
	// the backing index for this knowledge base.
	KnowledgeBaseClassName string `json:"knowledgeBaseClassName" yaml:"knowledgeBaseClassName"`

	// Sources lists the data sources to ingest into this knowledge base.
	Sources []KBSource `json:"sources,omitempty" yaml:"sources,omitempty"`

	// IndexConfig overrides class-level chunking and embedding settings.
	IndexConfig *KBIndexConfig `json:"indexConfig,omitempty" yaml:"indexConfig,omitempty"`

	// RefreshPolicy controls when and how the index is rebuilt.
	RefreshPolicy *KBRefreshPolicy `json:"refreshPolicy,omitempty" yaml:"refreshPolicy,omitempty"`
}

// KBSource defines a single data source for ingestion.
type KBSource struct {
	// Type discriminates the source variant:
	// "inline", "url", "file", "directory", "git", "configMap", "blob".
	Type string `json:"type" yaml:"type"`

	// Data holds inline document content (when Type is "inline").
	Data string `json:"data,omitempty" yaml:"data,omitempty"`

	// URI is the URL or file path (when Type is "url" or "file").
	URI string `json:"uri,omitempty" yaml:"uri,omitempty"`

	// Directory is a filesystem path to ingest recursively (when Type is "directory").
	Directory string `json:"directory,omitempty" yaml:"directory,omitempty"`

	// Git configures a Git repository source (when Type is "git").
	Git *KBGitSource `json:"git,omitempty" yaml:"git,omitempty"`

	// ConfigMap references a ConfigMap whose data values are ingested
	// (when Type is "configMap").
	ConfigMap *LocalObjectReference `json:"configMap,omitempty" yaml:"configMap,omitempty"`

	// Blob configures an object-storage source (when Type is "blob").
	// Supports Azure Blob Storage and S3-compatible providers.
	Blob *KBBlobSource `json:"blob,omitempty" yaml:"blob,omitempty"`
}

// KBGitSource identifies a Git repository and optional sub-path to ingest.
type KBGitSource struct {
	// URL is the HTTPS or SSH clone URL.
	URL string `json:"url" yaml:"url"`

	// Branch is the branch, tag, or SHA to check out (default: "main").
	Branch string `json:"branch,omitempty" yaml:"branch,omitempty"`

	// Path restricts ingestion to the given sub-directory within the repo.
	Path string `json:"path,omitempty" yaml:"path,omitempty"`
}

// KBBlobSource identifies an object-storage location (Azure Blob or S3) whose
// objects are ingested into the knowledge base. The Provider field selects the
// backend; a single provisioner reads the appropriate credentials out of the
// referenced secret.
type KBBlobSource struct {
	// Provider selects the object-storage backend: "azure" or "s3".
	// "s3" also matches S3-compatible services (MinIO, Ceph, R2, etc.)
	// when Endpoint is set.
	Provider string `json:"provider" yaml:"provider"`

	// Bucket is the container name (for "azure") or bucket name (for "s3").
	Bucket string `json:"bucket" yaml:"bucket"`

	// Prefix restricts ingestion to objects whose key starts with this
	// prefix. Empty prefix ingests the entire bucket.
	Prefix string `json:"prefix,omitempty" yaml:"prefix,omitempty"`

	// Region is the storage region. Required for "s3" unless Endpoint is
	// set (e.g. MinIO). Ignored for "azure".
	Region string `json:"region,omitempty" yaml:"region,omitempty"`

	// Endpoint overrides the default service URL. Used for
	// S3-compatible services (MinIO, Ceph, R2) or Azure sovereign clouds.
	// Empty falls back to the provider's public endpoint.
	Endpoint string `json:"endpoint,omitempty" yaml:"endpoint,omitempty"`

	// CredentialSecretRef points at a Secret in the same namespace holding
	// provider credentials. Expected keys per provider:
	//   azure: either "connectionString", or the pair
	//          ("accountName", "accountKey"), or ("accountName", "sasToken").
	//   s3:    "accessKeyId" and "secretAccessKey", plus optional
	//          "sessionToken" for temporary credentials.
	// May be omitted only when the provisioner is configured to use
	// workload identity / IRSA / IMDS.
	CredentialSecretRef *LocalObjectReference `json:"credentialSecretRef,omitempty" yaml:"credentialSecretRef,omitempty"`

	// Include is a list of glob patterns; only objects whose key matches
	// at least one pattern are ingested. Empty means "include everything".
	Include []string `json:"include,omitempty" yaml:"include,omitempty"`

	// Exclude is a list of glob patterns; objects matching any pattern are
	// skipped even if they matched Include.
	Exclude []string `json:"exclude,omitempty" yaml:"exclude,omitempty"`
}

// KBIndexConfig overrides class-level chunking and embedding configuration.
type KBIndexConfig struct {
	// EmbeddingModelRef overrides the class-level embedding model.
	EmbeddingModelRef *LocalObjectReference `json:"embeddingModelRef,omitempty" yaml:"embeddingModelRef,omitempty"`

	// ChunkSize is the target chunk size in tokens.
	ChunkSize int32 `json:"chunkSize,omitempty" yaml:"chunkSize,omitempty"`

	// ChunkOverlap is the number of overlapping tokens between adjacent chunks.
	ChunkOverlap int32 `json:"chunkOverlap,omitempty" yaml:"chunkOverlap,omitempty"`
}

// KBRefreshPolicy controls when and how the knowledge base index is rebuilt.
type KBRefreshPolicy struct {
	// Mode selects the trigger mechanism: "periodic", "onDemand", or "webhook".
	Mode string `json:"mode" yaml:"mode"`

	// Interval is the refresh cadence for "periodic" mode (e.g. "24h", "6h").
	Interval string `json:"interval,omitempty" yaml:"interval,omitempty"`

	// WebhookPath is the API path that triggers a refresh in "webhook" mode.
	WebhookPath string `json:"webhookPath,omitempty" yaml:"webhookPath,omitempty"`
}

// KnowledgeBaseStatus describes the observed state of a KnowledgeBase.
type KnowledgeBaseStatus struct {
	Phase      Phase       `json:"phase,omitempty" yaml:"phase,omitempty"`
	Conditions []Condition `json:"conditions,omitempty" yaml:"conditions,omitempty"`

	// DocumentCount is the number of source documents successfully ingested.
	DocumentCount int64 `json:"documentCount,omitempty" yaml:"documentCount,omitempty"`

	// IndexedChunks is the total number of chunks written to the vector index.
	IndexedChunks int64 `json:"indexedChunks,omitempty" yaml:"indexedChunks,omitempty"`

	// Generation is a monotonically increasing counter that increments each time
	// the index is successfully rebuilt.  Consumers may use it for cache
	// invalidation; during a Refreshing cycle the value reflects the previous
	// completed generation.
	Generation int64 `json:"generation,omitempty" yaml:"generation,omitempty"`

	// LastRefresh is the RFC 3339 timestamp of the most recent successful index
	// build (or rebuild).
	LastRefresh    string                   `json:"lastRefresh,omitempty" yaml:"lastRefresh,omitempty"`
	BackendBinding *KnowledgeBackendBinding `json:"backendBinding,omitempty" yaml:"backendBinding,omitempty"`
}

type KnowledgeBackendBinding struct {
	Generation            int64                `json:"generation" yaml:"generation"`
	ProviderName          string               `json:"providerName" yaml:"providerName"`
	ReclaimPolicy         ReclaimPolicy        `json:"reclaimPolicy,omitempty" yaml:"reclaimPolicy,omitempty"`
	ClassUID              string               `json:"classUID" yaml:"classUID"`
	ClassGeneration       int64                `json:"classGeneration" yaml:"classGeneration"`
	ConfigurationAPIGroup string               `json:"configurationAPIGroup" yaml:"configurationAPIGroup"`
	ConfigurationKind     string               `json:"configurationKind" yaml:"configurationKind"`
	ConfigurationName     string               `json:"configurationName" yaml:"configurationName"`
	ConfigurationUID      string               `json:"configurationUID" yaml:"configurationUID"`
	ConfigurationDigest   string               `json:"configurationDigest" yaml:"configurationDigest"`
	OperationID           string               `json:"operationID" yaml:"operationID"`
	BackendOperationID    string               `json:"backendOperationID" yaml:"backendOperationID"`
	EmbeddingModelBinding *ModelVersionBinding `json:"embeddingModelBinding,omitempty" yaml:"embeddingModelBinding,omitempty"`
}

// KnowledgeBaseClaim binds a set of agents to an existing KnowledgeBase in the
// same namespace, carrying per-claim access mode, retrieval overrides, and
// usage quotas. Unlike MemoryClaim (which provisions a private backing store),
// a KnowledgeBaseClaim is a grant/binding against a shared corpus — many
// claims may point at the same KnowledgeBase.
type KnowledgeBaseClaim struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       KnowledgeBaseClaimSpec   `json:"spec" yaml:"spec"`
	Status     KnowledgeBaseClaimStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

func (k *KnowledgeBaseClaim) GetTypeMeta() TypeMeta      { return k.TypeMeta }
func (k *KnowledgeBaseClaim) GetObjectMeta() *ObjectMeta { return &k.ObjectMeta }

// KnowledgeBaseClaimSpec defines the desired binding between a claim and a
// KnowledgeBase.
type KnowledgeBaseClaimSpec struct {
	// KnowledgeBaseName is the KnowledgeBase in the same namespace this claim
	// binds to.
	KnowledgeBaseName string `json:"knowledgeBaseName" yaml:"knowledgeBaseName"`

	// AccessMode is "ReadOnly" (default) or "ReadWrite". ReadWrite grants any
	// tool wired through this claim permission to add or delete documents on
	// the underlying KnowledgeBase.
	AccessMode string `json:"accessMode,omitempty" yaml:"accessMode,omitempty"`

	// Retrieval overrides class-level and KnowledgeBase-level defaults for
	// queries made through this claim. Overrides never mutate the corpus.
	Retrieval *KBRetrievalOverride `json:"retrieval,omitempty" yaml:"retrieval,omitempty"`

	// Usage caps enforced by governance/quota admission at query time.
	Usage *KBUsageQuota `json:"usage,omitempty" yaml:"usage,omitempty"`
}

// KBRetrievalOverride carries per-claim overrides for the KnowledgeBase's
// default query behaviour.
type KBRetrievalOverride struct {
	Mode string `json:"mode,omitempty" yaml:"mode,omitempty"`

	// TopK is the maximum number of results returned per query.
	TopK *int32 `json:"topK,omitempty" yaml:"topK,omitempty"`

	// MinScore filters out results below this relevance score. Decimal string
	// (e.g. "0.35") to keep the field type-stable across providers.
	MinScore string `json:"minScore,omitempty" yaml:"minScore,omitempty"`

	// Filter is a set of key=value constraints applied to document metadata.
	Filter map[string]string `json:"filter,omitempty" yaml:"filter,omitempty"`

	// HybridWeights tunes the mix of keyword vs vector scoring when the
	// underlying class supports hybrid retrieval.
	HybridWeights *KBHybridWeights `json:"hybridWeights,omitempty" yaml:"hybridWeights,omitempty"`
}

// KBHybridWeights carries the keyword/vector mix for hybrid retrieval. Weights
// are decimal strings (e.g. "0.4") so the schema does not depend on
// floating-point representation across serializers.
type KBHybridWeights struct {
	Keyword string `json:"keyword,omitempty" yaml:"keyword,omitempty"`
	Vector  string `json:"vector,omitempty" yaml:"vector,omitempty"`
}

// KBUsageQuota caps how aggressively a claim may query its KnowledgeBase.
type KBUsageQuota struct {
	MaxQueriesPerMinute *int32 `json:"maxQueriesPerMinute,omitempty" yaml:"maxQueriesPerMinute,omitempty"`
	MaxTokensReturned   *int32 `json:"maxTokensReturned,omitempty" yaml:"maxTokensReturned,omitempty"`
}

// KnowledgeBaseClaimStatus describes the observed state of a
// KnowledgeBaseClaim.
type KnowledgeBaseClaimStatus struct {
	Phase              Phase       `json:"phase,omitempty" yaml:"phase,omitempty"`
	BoundKnowledgeBase string      `json:"boundKnowledgeBase,omitempty" yaml:"boundKnowledgeBase,omitempty"`
	ObservedGeneration int64       `json:"observedGeneration,omitempty" yaml:"observedGeneration,omitempty"`
	Conditions         []Condition `json:"conditions,omitempty" yaml:"conditions,omitempty"`
}
