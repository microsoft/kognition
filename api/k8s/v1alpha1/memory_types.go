// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MemoryType classifies the semantic type of memory.
// +kubebuilder:validation:Enum=episodic;semantic;working;procedural;shared
type MemoryType string

const (
	MemoryTypeEpisodic   MemoryType = "episodic"
	MemoryTypeSemantic   MemoryType = "semantic"
	MemoryTypeWorking    MemoryType = "working"
	MemoryTypeProcedural MemoryType = "procedural"
	MemoryTypeShared     MemoryType = "shared"
)

// ReclaimPolicy defines what happens to memory data when the claim is deleted.
// +kubebuilder:validation:Enum=Delete;Retain;Archive
type ReclaimPolicy string

const (
	ReclaimPolicyDelete  ReclaimPolicy = "Delete"
	ReclaimPolicyRetain  ReclaimPolicy = "Retain"
	ReclaimPolicyArchive ReclaimPolicy = "Archive"
)

// ConcurrencyMode defines how concurrent writes to memory are resolved.
// +kubebuilder:validation:Enum=lww;crdt;lock
type ConcurrencyMode string

const (
	ConcurrencyModeLWW  ConcurrencyMode = "lww"
	ConcurrencyModeCRDT ConcurrencyMode = "crdt"
	ConcurrencyModeLock ConcurrencyMode = "lock"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Backend",type=string,JSONPath=`.spec.backendType`
// +kubebuilder:printcolumn:name="Reclaim",type=string,JSONPath=`.spec.reclaimPolicy`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// MemoryClass defines a class of memory backend (cluster-scoped).
type MemoryClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              MemoryClassSpec   `json:"spec,omitempty"`
	Status            MemoryClassStatus `json:"status,omitempty"`
}

// MemoryClassSpec defines the memory backend configuration.
type MemoryClassSpec struct {
	// BackendType is the storage backend type (e.g., "pgvector", "redis", "qdrant").
	BackendType string `json:"backendType"`

	// Parameters are backend-specific configuration.
	// +optional
	Parameters map[string]string `json:"parameters,omitempty"`

	// ReclaimPolicy specifies what happens to data when a claim is deleted.
	// +optional
	// +kubebuilder:default=Retain
	ReclaimPolicy ReclaimPolicy `json:"reclaimPolicy,omitempty"`

	// SummarizationConfig configures automatic summarization.
	// +optional
	SummarizationConfig *MemorySummarizationConfig `json:"summarizationConfig,omitempty"`

	// ConsistencyMode defines how concurrent writes are resolved.
	// +optional
	// +kubebuilder:default=lww
	ConsistencyMode ConcurrencyMode `json:"consistencyMode,omitempty"`
}

// MemorySummarizationConfig defines automatic summarization behaviour.
type MemorySummarizationConfig struct {
	// Enabled enables automatic summarization.
	Enabled bool `json:"enabled"`

	// ThresholdRecords is the record count that triggers a summarization pass.
	// +optional
	ThresholdRecords int32 `json:"thresholdRecords,omitempty"`

	// ModelRef references the model used for summarization.
	// +optional
	ModelRef *LocalObjectReference `json:"modelRef,omitempty"`
}

// MemoryClassStatus describes the observed state of a MemoryClass.
type MemoryClassStatus struct {
	// Phase is the lifecycle phase (Pending|Ready|Failed).
	// +optional
	Phase string `json:"phase,omitempty"`

	// Message is a human-readable message describing the current phase.
	// +optional
	Message string `json:"message,omitempty"`

	// Ready indicates the backend is reachable.
	// +optional
	Ready bool `json:"ready,omitempty"`

	// Conditions represent the latest available observations.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true

// MemoryClassList contains a list of MemoryClasses.
type MemoryClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MemoryClass `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Class",type=string,JSONPath=`.spec.memoryClassName`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// MemoryClaim requests memory resources from a MemoryClass (namespace-scoped).
type MemoryClaim struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              MemoryClaimSpec   `json:"spec,omitempty"`
	Status            MemoryClaimStatus `json:"status,omitempty"`
}

// MemoryClaimSpec defines the desired memory allocation.
type MemoryClaimSpec struct {
	// MemoryClassName references the MemoryClass to provision from.
	MemoryClassName string `json:"memoryClassName"`

	// Type discriminates the memory model requested.
	// +optional
	Type MemoryType `json:"type,omitempty"`

	// Capacity is the requested storage size (e.g., "100Mi", "1Gi").
	// +optional
	Capacity string `json:"capacity,omitempty"`

	// ConsistencyMode overrides the class-level consistency mode for this claim.
	// +optional
	ConsistencyMode ConcurrencyMode `json:"consistencyMode,omitempty"`

	// ConcurrencyMode specifies write-conflict resolution for shared memory.
	// +optional
	ConcurrencyMode ConcurrencyMode `json:"concurrencyMode,omitempty"`

	// AccessModes defines how the memory can be accessed (ReadWrite, ReadOnly, Shared).
	// +optional
	AccessModes []string `json:"accessModes,omitempty"`
}

// MemoryClaimStatus describes the observed state of a MemoryClaim.
type MemoryClaimStatus struct {
	// Phase indicates the binding phase (Pending, Bound, Lost).
	// +optional
	Phase string `json:"phase,omitempty"`

	// BoundVolumeName is the name of the MemoryVolume bound to this claim.
	// +optional
	BoundVolumeName string `json:"boundVolumeName,omitempty"`

	// Conditions represent the latest available observations.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true

// MemoryClaimList contains a list of MemoryClaims.
type MemoryClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MemoryClaim `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Class",type=string,JSONPath=`.spec.memoryClassName`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Capacity",type=string,JSONPath=`.status.capacity`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// MemoryVolume is the provisioned backing store for a MemoryClaim (namespace-scoped).
type MemoryVolume struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              MemoryVolumeSpec   `json:"spec,omitempty"`
	Status            MemoryVolumeStatus `json:"status,omitempty"`
}

// MemoryVolumeSpec defines the provisioned memory volume.
type MemoryVolumeSpec struct {
	// MemoryClassName is the class that provisioned this volume.
	MemoryClassName string `json:"memoryClassName"`

	// ClaimRef is the claim that this volume is bound to.
	// +optional
	ClaimRef *LocalObjectReference `json:"claimRef,omitempty"`

	// BackendHandle is the provisioner-specific identifier for the backing store
	// (e.g. a database schema name, Redis key prefix, or collection ID).
	// +optional
	BackendHandle string `json:"backendHandle,omitempty"`

	// Capacity is the provisioned storage size.
	// +optional
	Capacity string `json:"capacity,omitempty"`
}

// MemoryVolumeStatus describes the state of a provisioned volume.
type MemoryVolumeStatus struct {
	// Phase indicates the volume lifecycle phase (Available, Bound, Released, Deleted).
	// +optional
	Phase string `json:"phase,omitempty"`

	// Capacity is the currently provisioned capacity.
	// +optional
	Capacity string `json:"capacity,omitempty"`

	// Conditions represent the latest available observations.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true

// MemoryVolumeList contains a list of MemoryVolumes.
type MemoryVolumeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MemoryVolume `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Claim",type=string,JSONPath=`.spec.claimRef.name`
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Records",type=integer,JSONPath=`.status.recordCount`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Memory represents an agent-owned memory record store.
type Memory struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              MemorySpec   `json:"spec"`
	Status            MemoryStatus `json:"status,omitempty"`
}

// MemorySpec defines the desired state of a Memory store.
type MemorySpec struct {
	ClaimRef        LocalObjectReference `json:"claimRef"`
	Type            MemoryType           `json:"type"`
	TTL             string               `json:"ttl,omitempty"`
	RetentionPolicy string               `json:"retentionPolicy,omitempty"`
}

// MemoryStatus describes the observed state of a Memory store.
type MemoryStatus struct {
	Phase       string             `json:"phase,omitempty"`
	Conditions  []metav1.Condition `json:"conditions,omitempty"`
	RecordCount int64              `json:"recordCount,omitempty"`
	BytesUsed   int64              `json:"bytesUsed,omitempty"`
}

// +kubebuilder:object:root=true

// MemoryList contains a list of Memories.
type MemoryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Memory `json:"items"`
}

// MemoryProvenance captures the authorship and execution context of a record.
type MemoryProvenance struct {
	SourceAgentIdentity    string      `json:"sourceAgentIdentity"`
	SessionID              string      `json:"sessionId"`
	ReasoningStepID        string      `json:"reasoningStepId"`
	Timestamp              metav1.Time `json:"timestamp"`
	GrantID                string      `json:"grantId,omitempty"`
	DerivedFromRecordNames []string    `json:"derivedFromRecordNames,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced
// +kubebuilder:printcolumn:name="Memory",type=string,JSONPath=`.spec.memoryRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// MemoryRecord represents a single entry persisted in a Memory store.
type MemoryRecord struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              MemoryRecordSpec   `json:"spec"`
	Status            MemoryRecordStatus `json:"status,omitempty"`
}

// MemoryRecordSpec holds a record payload and provenance.
type MemoryRecordSpec struct {
	MemoryRef  LocalObjectReference `json:"memoryRef"`
	Content    string               `json:"content"`
	Metadata   map[string]string    `json:"metadata,omitempty"`
	Provenance *MemoryProvenance    `json:"provenance,omitempty"`
}

// MemoryRecordStatus is the observed state of a MemoryRecord.
type MemoryRecordStatus struct {
	Phase      string             `json:"phase,omitempty"`
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true

// MemoryRecordList contains a list of MemoryRecords.
type MemoryRecordList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MemoryRecord `json:"items"`
}

func init() {
	SchemeBuilder.Register(
		&MemoryClass{}, &MemoryClassList{},
		&MemoryClaim{}, &MemoryClaimList{},
		&MemoryVolume{}, &MemoryVolumeList{},
		&Memory{}, &MemoryList{},
		&MemoryRecord{}, &MemoryRecordList{},
	)
}
