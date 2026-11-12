// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="KnowledgeBase",type=string,JSONPath=`.spec.knowledgeBaseName`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Job",type=string,JSONPath=`.status.jobName`
// +kubebuilder:printcolumn:name="Docs",type=integer,JSONPath=`.status.documentCount`
// +kubebuilder:printcolumn:name="Chunks",type=integer,JSONPath=`.status.indexedChunks`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// KnowledgeBaseIndexRun is a one-shot request to reindex a KnowledgeBase.
type KnowledgeBaseIndexRun struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              KnowledgeBaseIndexRunSpec   `json:"spec,omitempty"`
	Status            KnowledgeBaseIndexRunStatus `json:"status,omitempty"`
}

// KnowledgeBaseIndexRunSpec describes what a single indexing run should do.
type KnowledgeBaseIndexRunSpec struct {
	// KnowledgeBaseName references the KnowledgeBase in the same namespace to
	// reindex.
	KnowledgeBaseName string `json:"knowledgeBaseName"`

	// TTLSecondsAfterFinished, when set, causes the reconciler to delete the
	// IndexRun this many seconds after it enters a terminal phase.
	// +optional
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty"`
}

// KnowledgeBaseIndexRunStatus reports the observed state of an indexing run.
type KnowledgeBaseIndexRunStatus struct {
	// Phase is Pending | Running | Succeeded | Failed.
	// +optional
	Phase string `json:"phase,omitempty"`

	// JobName is the name of the underlying batch/v1 Job in the KB's
	// namespace.
	// +optional
	JobName string `json:"jobName,omitempty"`

	// StartedAt is the RFC 3339 timestamp when the run entered Running.
	// +optional
	StartedAt string `json:"startedAt,omitempty"`

	// CompletedAt is the RFC 3339 timestamp when the run reached a terminal
	// phase.
	// +optional
	CompletedAt string `json:"completedAt,omitempty"`

	// DocumentCount is the number of documents processed by the run.
	// +optional
	DocumentCount int64 `json:"documentCount,omitempty"`

	// IndexedChunks is the number of chunks written to the vector index by
	// the run.
	// +optional
	IndexedChunks int64 `json:"indexedChunks,omitempty"`

	// SourceDigest identifies the version of the source data that was
	// indexed.
	// +optional
	SourceDigest string `json:"sourceDigest,omitempty"`

	// BackendBinding is the provider's immutable confirmation of the indexed
	// generation.
	// +optional
	BackendBinding *KnowledgeBackendBinding `json:"backendBinding,omitempty"`

	// Message is a human-readable summary of the terminal state.
	// +optional
	Message string `json:"message,omitempty"`

	// Conditions represent the latest available observations.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true

// KnowledgeBaseIndexRunList contains a list of KnowledgeBaseIndexRuns.
type KnowledgeBaseIndexRunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KnowledgeBaseIndexRun `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="KnowledgeBase",type=string,JSONPath=`.spec.knowledgeBaseName`
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.spec.schedule`
// +kubebuilder:printcolumn:name="Suspend",type=boolean,JSONPath=`.spec.suspend`
// +kubebuilder:printcolumn:name="LastRun",type=string,JSONPath=`.status.lastScheduleTime`
// +kubebuilder:printcolumn:name="LastSuccess",type=string,JSONPath=`.status.lastSuccessfulTime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// KnowledgeBaseIndexSchedule declares a cron schedule that produces
// KnowledgeBaseIndexRun objects on each tick.
type KnowledgeBaseIndexSchedule struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              KnowledgeBaseIndexScheduleSpec   `json:"spec,omitempty"`
	Status            KnowledgeBaseIndexScheduleStatus `json:"status,omitempty"`
}

// KnowledgeBaseIndexScheduleSpec defines the desired scheduling behaviour.
type KnowledgeBaseIndexScheduleSpec struct {
	// KnowledgeBaseName references the target KnowledgeBase in the same
	// namespace.
	KnowledgeBaseName string `json:"knowledgeBaseName"`

	// Schedule is a standard 5-field cron expression or a supported descriptor
	// such as @every 6h, evaluated in UTC.
	Schedule string `json:"schedule"`

	// Suspend, when true, prevents new runs from being created.
	// +optional
	Suspend *bool `json:"suspend,omitempty"`

	// ConcurrencyPolicy controls what happens when the previous run has not
	// finished. One of Allow, Forbid (default), or Replace.
	// +optional
	// +kubebuilder:validation:Enum=Allow;Forbid;Replace
	ConcurrencyPolicy string `json:"concurrencyPolicy,omitempty"`

	// SuccessfulRunsHistoryLimit caps retained successful runs. Defaults to 3.
	// +optional
	SuccessfulRunsHistoryLimit *int32 `json:"successfulRunsHistoryLimit,omitempty"`

	// FailedRunsHistoryLimit caps retained failed runs. Defaults to 1.
	// +optional
	FailedRunsHistoryLimit *int32 `json:"failedRunsHistoryLimit,omitempty"`

	// StartingDeadlineSeconds, when set, skips ticks older than this.
	// +optional
	StartingDeadlineSeconds *int64 `json:"startingDeadlineSeconds,omitempty"`

	// RunTemplate is the KnowledgeBaseIndexRunSpec used to instantiate each
	// child IndexRun. Its knowledgeBaseName is ignored.
	// +optional
	RunTemplate KnowledgeBaseIndexRunSpec `json:"runTemplate,omitempty"`
}

// KnowledgeBaseIndexScheduleStatus reports the observed state of a schedule.
type KnowledgeBaseIndexScheduleStatus struct {
	// Phase is Pending | Ready | Suspended | Failed.
	// +optional
	Phase string `json:"phase,omitempty"`

	// LastScheduleTime is the RFC 3339 timestamp of the most recent tick
	// this reconciler acted on.
	// +optional
	LastScheduleTime string `json:"lastScheduleTime,omitempty"`

	// LastSuccessfulTime is the completion time of the most recent successful
	// run.
	// +optional
	LastSuccessfulTime string `json:"lastSuccessfulTime,omitempty"`

	// NextScheduleTime is the RFC 3339 timestamp of the next planned tick.
	// +optional
	NextScheduleTime string `json:"nextScheduleTime,omitempty"`

	// Active lists IndexRuns owned by this Schedule that have not reached a
	// terminal phase.
	// +optional
	Active []LocalObjectReference `json:"active,omitempty"`

	// Conditions represent the latest available observations.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true

// KnowledgeBaseIndexScheduleList contains a list of KnowledgeBaseIndexSchedules.
type KnowledgeBaseIndexScheduleList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []KnowledgeBaseIndexSchedule `json:"items"`
}

func init() {
	SchemeBuilder.Register(
		&KnowledgeBaseIndexRun{}, &KnowledgeBaseIndexRunList{},
		&KnowledgeBaseIndexSchedule{}, &KnowledgeBaseIndexScheduleList{},
	)
}
