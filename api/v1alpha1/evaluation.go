// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import "time"

// Evaluation declares a repeatable evaluation pipeline for an Agent, Solution,
// or other executable Kognition target.
type Evaluation struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       EvaluationSpec   `json:"spec" yaml:"spec"`
	Status     EvaluationStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

func (e *Evaluation) GetTypeMeta() TypeMeta      { return e.TypeMeta }
func (e *Evaluation) GetObjectMeta() *ObjectMeta { return &e.ObjectMeta }

// EvaluationRun records one execution of an Evaluation against a target.
type EvaluationRun struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       EvaluationRunSpec   `json:"spec" yaml:"spec"`
	Status     EvaluationRunStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

func (e *EvaluationRun) GetTypeMeta() TypeMeta      { return e.TypeMeta }
func (e *EvaluationRun) GetObjectMeta() *ObjectMeta { return &e.ObjectMeta }

// EvaluationDataset declares concrete inputs and expectations used by an Evaluation.
type EvaluationDataset struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       EvaluationDatasetSpec   `json:"spec" yaml:"spec"`
	Status     EvaluationDatasetStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

func (e *EvaluationDataset) GetTypeMeta() TypeMeta      { return e.TypeMeta }
func (e *EvaluationDataset) GetObjectMeta() *ObjectMeta { return &e.ObjectMeta }

// EvaluationSpec defines the target, dataset, graders, thresholds, and schedule
// for a repeatable evaluation.
type EvaluationSpec struct {
	// TargetRef references the resource being evaluated, such as an Agent or Solution.
	TargetRef EvaluationTargetRef `json:"targetRef" yaml:"targetRef"`

	// DatasetRef references the dataset used as evaluation input.
	DatasetRef EvaluationDatasetRef `json:"datasetRef" yaml:"datasetRef"`

	// Graders lists the graders that score each evaluation run.
	Graders []EvaluationGrader `json:"graders,omitempty" yaml:"graders,omitempty"`

	// MetricThresholds declares pass/fail boundaries for grader metrics.
	MetricThresholds []EvaluationMetricThreshold `json:"metricThresholds,omitempty" yaml:"metricThresholds,omitempty"`

	// Schedule configures periodic evaluation execution.
	Schedule *EvaluationSchedule `json:"schedule,omitempty" yaml:"schedule,omitempty"`
}

// EvaluationTargetRef references the executable resource under evaluation.
type EvaluationTargetRef struct {
	// Kind is the resource kind being evaluated, such as Agent or Solution.
	Kind string `json:"kind" yaml:"kind"`

	// Name is the resource name in the Evaluation namespace unless Namespace is set.
	Name string `json:"name" yaml:"name"`

	// Namespace optionally references a target in another namespace.
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`

	// Revision optionally pins the evaluation to a specific target revision.
	Revision *int64 `json:"revision,omitempty" yaml:"revision,omitempty"`
}

// EvaluationDatasetRef references evaluation input data.
type EvaluationDatasetRef struct {
	// Name is the dataset resource name.
	Name string `json:"name" yaml:"name"`

	// Namespace optionally references a dataset in another namespace.
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`

	// Version optionally pins the dataset version.
	Version string `json:"version,omitempty" yaml:"version,omitempty"`
}

// EvaluationDatasetSpec defines the cases evaluated by a run.
type EvaluationDatasetSpec struct {
	// Version is an optional user-facing dataset version label.
	Version string `json:"version,omitempty" yaml:"version,omitempty"`

	// Cases contains the concrete inputs used to exercise the target.
	Cases []EvaluationDatasetCase `json:"cases" yaml:"cases"`
}

// EvaluationDatasetCase defines one input and optional expectations.
type EvaluationDatasetCase struct {
	// ID is unique within the dataset and is used for stable child job names.
	ID string `json:"id" yaml:"id"`

	// Input is sent to the target under evaluation.
	Input string `json:"input" yaml:"input"`

	// Expected is an optional deterministic expectation for task success checks.
	Expected string `json:"expected,omitempty" yaml:"expected,omitempty"`

	// Reference is optional source material for future groundedness graders.
	Reference string `json:"reference,omitempty" yaml:"reference,omitempty"`

	// Rubric is optional human-readable grading guidance for future judge graders.
	Rubric string `json:"rubric,omitempty" yaml:"rubric,omitempty"`

	// Tags classify the case for filtering and reporting.
	Tags []string `json:"tags,omitempty" yaml:"tags,omitempty"`

	// Metadata carries small case-specific labels or parameters.
	Metadata map[string]string `json:"metadata,omitempty" yaml:"metadata,omitempty"`
}

// EvaluationDatasetStatus summarizes observed dataset validation state.
type EvaluationDatasetStatus struct {
	Phase              Phase       `json:"phase,omitempty" yaml:"phase,omitempty"`
	Conditions         []Condition `json:"conditions,omitempty" yaml:"conditions,omitempty"`
	ObservedGeneration int64       `json:"observedGeneration,omitempty" yaml:"observedGeneration,omitempty"`
	CaseCount          int32       `json:"caseCount,omitempty" yaml:"caseCount,omitempty"`
	Digest             string      `json:"digest,omitempty" yaml:"digest,omitempty"`
	LastValidatedAt    *time.Time  `json:"lastValidatedAt,omitempty" yaml:"lastValidatedAt,omitempty"`
	Message            string      `json:"message,omitempty" yaml:"message,omitempty"`
}

// EvaluationGrader defines one grader used to score an Evaluation.
type EvaluationGrader struct {
	// Name is unique within the Evaluation.
	Name string `json:"name" yaml:"name"`

	// Type selects a built-in or provider-backed grader.
	Type string `json:"type" yaml:"type"`

	// Parameters carries grader-specific configuration.
	Parameters map[string]string `json:"parameters,omitempty" yaml:"parameters,omitempty"`
}

const (
	// EvaluationGraderTypeBuiltin preserves the generic built-in selector used by early Evaluation resources.
	EvaluationGraderTypeBuiltin = "builtin"
	// EvaluationGraderTypeTaskSuccess scores whether the target produced a successful task response.
	EvaluationGraderTypeTaskSuccess = "task_success"
	// EvaluationGraderTypeHallucination scores whether the response avoids configured unsupported claims.
	EvaluationGraderTypeHallucination = "hallucination"
	// EvaluationGraderTypeSafety scores whether the response avoids configured unsafe content.
	EvaluationGraderTypeSafety = "safety"
	// EvaluationGraderTypeLatency records AgentJob execution latency in milliseconds.
	EvaluationGraderTypeLatency = "latency"
	// EvaluationGraderTypeCost records AgentJob cost units or token consumption when available.
	EvaluationGraderTypeCost = "cost"
	// EvaluationGraderTypeToolReliability scores the fraction of successful tool calls.
	EvaluationGraderTypeToolReliability = "tool_reliability"
	// EvaluationGraderTypeMemoryQuality scores whether memory-backed behavior meets configured quality signals.
	EvaluationGraderTypeMemoryQuality = "memory_quality"
	// EvaluationGraderTypeLLMJudge scores target output with a configured judge Agent.
	EvaluationGraderTypeLLMJudge = "llm_judge"
)

// EvaluationMetricThreshold declares pass/fail bounds for one metric.
type EvaluationMetricThreshold struct {
	// Metric is the metric name emitted by a grader.
	Metric string `json:"metric" yaml:"metric"`

	// Min is the inclusive minimum passing value.
	Min *float64 `json:"min,omitempty" yaml:"min,omitempty"`

	// Max is the inclusive maximum passing value.
	Max *float64 `json:"max,omitempty" yaml:"max,omitempty"`

	// Required marks the Evaluation as failed when the metric is missing.
	Required bool `json:"required,omitempty" yaml:"required,omitempty"`
}

// EvaluationSchedule configures periodic evaluation execution.
type EvaluationSchedule struct {
	// Cron is a standard cron expression for scheduled runs.
	Cron string `json:"cron,omitempty" yaml:"cron,omitempty"`

	// Timezone is the IANA timezone used to interpret Cron.
	Timezone string `json:"timezone,omitempty" yaml:"timezone,omitempty"`

	// Suspend disables scheduled runs without deleting the Evaluation.
	Suspend bool `json:"suspend,omitempty" yaml:"suspend,omitempty"`

	// ConcurrencyPolicy controls overlapping scheduled runs.
	ConcurrencyPolicy EvaluationConcurrencyPolicy `json:"concurrencyPolicy,omitempty" yaml:"concurrencyPolicy,omitempty"`

	// SuccessfulRunHistoryLimit bounds retained successful run summaries.
	SuccessfulRunHistoryLimit int32 `json:"successfulRunHistoryLimit,omitempty" yaml:"successfulRunHistoryLimit,omitempty"`

	// FailedRunHistoryLimit bounds retained failed run summaries.
	FailedRunHistoryLimit int32 `json:"failedRunHistoryLimit,omitempty" yaml:"failedRunHistoryLimit,omitempty"`
}

// EvaluationConcurrencyPolicy controls overlapping scheduled evaluation runs.
type EvaluationConcurrencyPolicy string

const (
	// EvaluationConcurrencyAllow permits overlapping runs.
	EvaluationConcurrencyAllow EvaluationConcurrencyPolicy = "Allow"
	// EvaluationConcurrencyForbid skips a scheduled run when a previous run is active.
	EvaluationConcurrencyForbid EvaluationConcurrencyPolicy = "Forbid"
	// EvaluationConcurrencyReplace cancels an active run before starting a new one.
	EvaluationConcurrencyReplace EvaluationConcurrencyPolicy = "Replace"
)

// EvaluationStatus describes observed evaluation state and recent run history.
type EvaluationStatus struct {
	Phase              Phase                  `json:"phase,omitempty" yaml:"phase,omitempty"`
	Conditions         []Condition            `json:"conditions,omitempty" yaml:"conditions,omitempty"`
	ObservedGeneration int64                  `json:"observedGeneration,omitempty" yaml:"observedGeneration,omitempty"`
	LastRunRef         *LocalObjectReference  `json:"lastRunRef,omitempty" yaml:"lastRunRef,omitempty"`
	LastRunTime        *time.Time             `json:"lastRunTime,omitempty" yaml:"lastRunTime,omitempty"`
	RunHistory         []EvaluationRunSummary `json:"runHistory,omitempty" yaml:"runHistory,omitempty"`
}

// EvaluationRunSpec defines one immutable evaluation execution request.
type EvaluationRunSpec struct {
	// EvaluationRef references the parent Evaluation that created this run.
	EvaluationRef LocalObjectReference `json:"evaluationRef" yaml:"evaluationRef"`

	// TargetRef is the snapshotted target from the parent Evaluation.
	TargetRef EvaluationTargetRef `json:"targetRef" yaml:"targetRef"`

	// DatasetRef is the snapshotted dataset from the parent Evaluation.
	DatasetRef EvaluationDatasetRef `json:"datasetRef" yaml:"datasetRef"`

	// Graders is the snapshotted grader list from the parent Evaluation.
	Graders []EvaluationGrader `json:"graders,omitempty" yaml:"graders,omitempty"`

	// MetricThresholds is the snapshotted threshold list from the parent Evaluation.
	MetricThresholds []EvaluationMetricThreshold `json:"metricThresholds,omitempty" yaml:"metricThresholds,omitempty"`
}

// EvaluationRunStatus describes observed state for one EvaluationRun.
type EvaluationRunStatus struct {
	Phase          Phase                    `json:"phase,omitempty" yaml:"phase,omitempty"`
	Conditions     []Condition              `json:"conditions,omitempty" yaml:"conditions,omitempty"`
	StartedAt      *time.Time               `json:"startedAt,omitempty" yaml:"startedAt,omitempty"`
	CompletedAt    *time.Time               `json:"completedAt,omitempty" yaml:"completedAt,omitempty"`
	DatasetDigest  string                   `json:"datasetDigest,omitempty" yaml:"datasetDigest,omitempty"`
	CaseCount      int32                    `json:"caseCount,omitempty" yaml:"caseCount,omitempty"`
	CompletedCases int32                    `json:"completedCases,omitempty" yaml:"completedCases,omitempty"`
	FailedCases    int32                    `json:"failedCases,omitempty" yaml:"failedCases,omitempty"`
	CaseResults    []EvaluationCaseResult   `json:"caseResults,omitempty" yaml:"caseResults,omitempty"`
	TargetJobRef   *LocalObjectReference    `json:"targetJobRef,omitempty" yaml:"targetJobRef,omitempty"`
	TargetOutput   string                   `json:"targetOutput,omitempty" yaml:"targetOutput,omitempty"`
	Score          *float64                 `json:"score,omitempty" yaml:"score,omitempty"`
	Passed         bool                     `json:"passed,omitempty" yaml:"passed,omitempty"`
	Degraded       bool                     `json:"degraded,omitempty" yaml:"degraded,omitempty"`
	Metrics        []EvaluationMetricResult `json:"metrics,omitempty" yaml:"metrics,omitempty"`
	Message        string                   `json:"message,omitempty" yaml:"message,omitempty"`
}

// EvaluationCaseResult records the outcome for one dataset case.
type EvaluationCaseResult struct {
	ID           string                   `json:"id" yaml:"id"`
	TargetJobRef *LocalObjectReference    `json:"targetJobRef,omitempty" yaml:"targetJobRef,omitempty"`
	TargetOutput string                   `json:"targetOutput,omitempty" yaml:"targetOutput,omitempty"`
	StartedAt    *time.Time               `json:"startedAt,omitempty" yaml:"startedAt,omitempty"`
	CompletedAt  *time.Time               `json:"completedAt,omitempty" yaml:"completedAt,omitempty"`
	Passed       bool                     `json:"passed,omitempty" yaml:"passed,omitempty"`
	Metrics      []EvaluationMetricResult `json:"metrics,omitempty" yaml:"metrics,omitempty"`
	Message      string                   `json:"message,omitempty" yaml:"message,omitempty"`
}

// EvaluationRunSummary records the result of one observed evaluation run.
type EvaluationRunSummary struct {
	Name        string                   `json:"name" yaml:"name"`
	TargetRef   EvaluationTargetRef      `json:"targetRef,omitempty" yaml:"targetRef,omitempty"`
	DatasetRef  EvaluationDatasetRef     `json:"datasetRef,omitempty" yaml:"datasetRef,omitempty"`
	Phase       Phase                    `json:"phase,omitempty" yaml:"phase,omitempty"`
	StartedAt   *time.Time               `json:"startedAt,omitempty" yaml:"startedAt,omitempty"`
	CompletedAt *time.Time               `json:"completedAt,omitempty" yaml:"completedAt,omitempty"`
	Score       *float64                 `json:"score,omitempty" yaml:"score,omitempty"`
	Passed      bool                     `json:"passed,omitempty" yaml:"passed,omitempty"`
	Metrics     []EvaluationMetricResult `json:"metrics,omitempty" yaml:"metrics,omitempty"`
	Message     string                   `json:"message,omitempty" yaml:"message,omitempty"`
}

// EvaluationMetricResult records one metric emitted by a grader.
type EvaluationMetricResult struct {
	Metric  string  `json:"metric" yaml:"metric"`
	Value   float64 `json:"value" yaml:"value"`
	Passed  bool    `json:"passed,omitempty" yaml:"passed,omitempty"`
	Message string  `json:"message,omitempty" yaml:"message,omitempty"`
}
