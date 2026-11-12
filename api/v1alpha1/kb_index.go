// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

// Job-analog / CronJob-analog phase and policy constants for
// KnowledgeBaseIndexRun and KnowledgeBaseIndexSchedule.
const (
	// PhaseSucceeded is the terminal successful phase for one-shot runs.
	PhaseSucceeded Phase = "Succeeded"
	// PhaseSuspended reports that an IndexSchedule is not producing runs.
	PhaseSuspended Phase = "Suspended"

	// ConcurrencyPolicyAllow permits concurrent runs of the same schedule.
	ConcurrencyPolicyAllow = "Allow"
	// ConcurrencyPolicyForbid (default) skips a tick while a prior run is active.
	ConcurrencyPolicyForbid = "Forbid"
	// ConcurrencyPolicyReplace cancels the active run and starts a new one.
	ConcurrencyPolicyReplace = "Replace"
)

// KnowledgeBaseIndexRun is a one-shot request to reindex a KnowledgeBase.
// It is the Job-analog of KnowledgeBase indexing: creating one triggers a
// single ingest attempt against the referenced KB, and its Status reports the
// outcome. Successful real ingests also refresh the referenced KnowledgeBase's
// status and completed generation.
type KnowledgeBaseIndexRun struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       KnowledgeBaseIndexRunSpec   `json:"spec" yaml:"spec"`
	Status     KnowledgeBaseIndexRunStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

// GetTypeMeta returns the TypeMeta.
func (k *KnowledgeBaseIndexRun) GetTypeMeta() TypeMeta { return k.TypeMeta }

// GetObjectMeta returns a pointer to the ObjectMeta.
func (k *KnowledgeBaseIndexRun) GetObjectMeta() *ObjectMeta { return &k.ObjectMeta }

// KnowledgeBaseIndexRunSpec describes what a single indexing run should do.
type KnowledgeBaseIndexRunSpec struct {
	// KnowledgeBaseName references the KnowledgeBase in the same namespace to
	// reindex. Required.
	KnowledgeBaseName string `json:"knowledgeBaseName" yaml:"knowledgeBaseName"`

	// TTLSecondsAfterFinished, when set, causes the reconciler to delete the
	// IndexRun this many seconds after it enters a terminal phase.
	TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished,omitempty" yaml:"ttlSecondsAfterFinished,omitempty"`
}

// KnowledgeBaseIndexRunStatus reports the observed state of an indexing run.
type KnowledgeBaseIndexRunStatus struct {
	// Phase is Pending | Running | Succeeded | Failed.
	Phase Phase `json:"phase,omitempty" yaml:"phase,omitempty"`

	// JobName is the name of the underlying batch/v1 Job in the KB's
	// namespace. Only meaningful when the KB's class has an out-of-process
	// ingester configured; empty otherwise.
	JobName string `json:"jobName,omitempty" yaml:"jobName,omitempty"`

	// StartedAt is the RFC 3339 timestamp when the run entered Running.
	StartedAt string `json:"startedAt,omitempty" yaml:"startedAt,omitempty"`

	// CompletedAt is the RFC 3339 timestamp when the run reached a terminal
	// phase.
	CompletedAt string `json:"completedAt,omitempty" yaml:"completedAt,omitempty"`

	// DocumentCount is the number of documents processed by the run.
	DocumentCount int64 `json:"documentCount,omitempty" yaml:"documentCount,omitempty"`

	// IndexedChunks is the number of chunks written to the vector index by
	// the run.
	IndexedChunks int64 `json:"indexedChunks,omitempty" yaml:"indexedChunks,omitempty"`

	// SourceDigest identifies the version of the source data that was
	// indexed, when the ingester emits one.
	SourceDigest string `json:"sourceDigest,omitempty" yaml:"sourceDigest,omitempty"`

	// BackendBinding is the provider's immutable confirmation of the indexed
	// generation. It is present only for provider-backed runs.
	BackendBinding *KnowledgeBackendBinding `json:"backendBinding,omitempty" yaml:"backendBinding,omitempty"`

	// Message is a human-readable summary of the terminal state (empty while
	// Pending or Running, error text when Failed).
	Message string `json:"message,omitempty" yaml:"message,omitempty"`

	// Conditions is the standard condition list.
	Conditions []Condition `json:"conditions,omitempty" yaml:"conditions,omitempty"`
}

// KnowledgeBaseIndexSchedule declares a cron schedule that produces
// KnowledgeBaseIndexRun objects on each tick. It is the CronJob-analog of KB
// indexing.
type KnowledgeBaseIndexSchedule struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       KnowledgeBaseIndexScheduleSpec   `json:"spec" yaml:"spec"`
	Status     KnowledgeBaseIndexScheduleStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

// GetTypeMeta returns the TypeMeta.
func (k *KnowledgeBaseIndexSchedule) GetTypeMeta() TypeMeta { return k.TypeMeta }

// GetObjectMeta returns a pointer to the ObjectMeta.
func (k *KnowledgeBaseIndexSchedule) GetObjectMeta() *ObjectMeta { return &k.ObjectMeta }

// KnowledgeBaseIndexScheduleSpec defines the desired scheduling behaviour.
type KnowledgeBaseIndexScheduleSpec struct {
	// KnowledgeBaseName references the target KnowledgeBase in the same
	// namespace. Required.
	KnowledgeBaseName string `json:"knowledgeBaseName" yaml:"knowledgeBaseName"`

	// Schedule is a standard 5-field cron expression or a supported descriptor
	// such as @every 6h, evaluated in UTC. Required.
	Schedule string `json:"schedule" yaml:"schedule"`

	// Suspend, when true, prevents new runs from being created. Existing
	// runs are left alone. Defaults to false.
	Suspend *bool `json:"suspend,omitempty" yaml:"suspend,omitempty"`

	// ConcurrencyPolicy controls what happens when the previous run has not
	// finished. One of Allow, Forbid (default), or Replace.
	ConcurrencyPolicy string `json:"concurrencyPolicy,omitempty" yaml:"concurrencyPolicy,omitempty"`

	// SuccessfulRunsHistoryLimit caps how many completed successful runs are
	// retained. Older runs are garbage collected. Defaults to 3.
	SuccessfulRunsHistoryLimit *int32 `json:"successfulRunsHistoryLimit,omitempty" yaml:"successfulRunsHistoryLimit,omitempty"`

	// FailedRunsHistoryLimit caps how many failed runs are retained.
	// Defaults to 1.
	FailedRunsHistoryLimit *int32 `json:"failedRunsHistoryLimit,omitempty" yaml:"failedRunsHistoryLimit,omitempty"`

	// StartingDeadlineSeconds, when set, causes the reconciler to skip a
	// tick if more than this many seconds have passed since its scheduled
	// time.
	StartingDeadlineSeconds *int64 `json:"startingDeadlineSeconds,omitempty" yaml:"startingDeadlineSeconds,omitempty"`

	// RunTemplate is the KnowledgeBaseIndexRunSpec used to instantiate each
	// child IndexRun. knowledgeBaseName in the template is ignored; the
	// schedule's own knowledgeBaseName is authoritative.
	RunTemplate KnowledgeBaseIndexRunSpec `json:"runTemplate,omitempty" yaml:"runTemplate,omitempty"`
}

// KnowledgeBaseIndexScheduleStatus reports the observed state of a schedule.
type KnowledgeBaseIndexScheduleStatus struct {
	// Phase is Pending | Ready | Suspended | Failed.
	Phase Phase `json:"phase,omitempty" yaml:"phase,omitempty"`

	// LastScheduleTime is the RFC 3339 timestamp of the most recent tick
	// this reconciler acted on (whether or not it produced a run).
	LastScheduleTime string `json:"lastScheduleTime,omitempty" yaml:"lastScheduleTime,omitempty"`

	// LastSuccessfulTime is the completion time of the most recent
	// successful run.
	LastSuccessfulTime string `json:"lastSuccessfulTime,omitempty" yaml:"lastSuccessfulTime,omitempty"`

	// NextScheduleTime is the RFC 3339 timestamp of the next planned tick.
	NextScheduleTime string `json:"nextScheduleTime,omitempty" yaml:"nextScheduleTime,omitempty"`

	// Active lists IndexRuns owned by this Schedule that have not reached a
	// terminal phase.
	Active []LocalObjectReference `json:"active,omitempty" yaml:"active,omitempty"`

	// Conditions is the standard condition list.
	Conditions []Condition `json:"conditions,omitempty" yaml:"conditions,omitempty"`
}
