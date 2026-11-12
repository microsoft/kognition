// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="LastRun",type=string,JSONPath=`.status.lastRunRef.name`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Evaluation declares a repeatable evaluation pipeline for an Agent, Solution,
// or other executable Kognition target.
type Evaluation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              EvaluationSpec   `json:"spec"`
	Status            EvaluationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Evaluation",type=string,JSONPath=`.spec.evaluationRef.name`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Passed",type=boolean,JSONPath=`.status.passed`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// EvaluationRun records one execution of an Evaluation against a target.
type EvaluationRun struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              EvaluationRunSpec   `json:"spec"`
	Status            EvaluationRunStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Version",type=string,JSONPath=`.spec.version`
// +kubebuilder:printcolumn:name="Cases",type=integer,JSONPath=`.status.caseCount`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// EvaluationDataset declares concrete inputs and expectations used by an Evaluation.
type EvaluationDataset struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              EvaluationDatasetSpec   `json:"spec"`
	Status            EvaluationDatasetStatus `json:"status,omitempty"`
}

// EvaluationSpec defines the target, dataset, graders, thresholds, and schedule
// for a repeatable evaluation.
type EvaluationSpec struct {
	TargetRef  EvaluationTargetRef  `json:"targetRef"`
	DatasetRef EvaluationDatasetRef `json:"datasetRef"`
	// +optional
	Graders []EvaluationGrader `json:"graders,omitempty"`
	// +optional
	MetricThresholds []EvaluationMetricThreshold `json:"metricThresholds,omitempty"`
	// +optional
	Schedule *EvaluationSchedule `json:"schedule,omitempty"`
}

// EvaluationTargetRef references the executable resource under evaluation.
type EvaluationTargetRef struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// +optional
	Revision *int64 `json:"revision,omitempty"`
}

// EvaluationDatasetRef references evaluation input data.
type EvaluationDatasetRef struct {
	Name string `json:"name"`
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// +optional
	Version string `json:"version,omitempty"`
}

// EvaluationDatasetSpec defines the cases evaluated by a run.
type EvaluationDatasetSpec struct {
	// +optional
	Version string `json:"version,omitempty"`
	// +kubebuilder:validation:MinItems=1
	Cases []EvaluationDatasetCase `json:"cases"`
}

// EvaluationDatasetCase defines one input and optional expectations.
type EvaluationDatasetCase struct {
	// +kubebuilder:validation:MinLength=1
	ID string `json:"id"`
	// +kubebuilder:validation:MinLength=1
	Input string `json:"input"`
	// +optional
	Expected string `json:"expected,omitempty"`
	// +optional
	Reference string `json:"reference,omitempty"`
	// +optional
	Rubric string `json:"rubric,omitempty"`
	// +optional
	Tags []string `json:"tags,omitempty"`
	// +optional
	Metadata map[string]string `json:"metadata,omitempty"`
}

// EvaluationDatasetStatus summarizes observed dataset validation state.
type EvaluationDatasetStatus struct {
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	CaseCount int32 `json:"caseCount,omitempty"`
	// +optional
	Digest string `json:"digest,omitempty"`
	// +optional
	LastValidatedAt *metav1.Time `json:"lastValidatedAt,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

// EvaluationGrader defines one grader used to score an Evaluation.
type EvaluationGrader struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// +optional
	Parameters map[string]string `json:"parameters,omitempty"`
}

// EvaluationMetricThreshold declares pass/fail bounds for one metric.
type EvaluationMetricThreshold struct {
	Metric string `json:"metric"`
	// +optional
	Min *float64 `json:"min,omitempty"`
	// +optional
	Max *float64 `json:"max,omitempty"`
	// +optional
	Required bool `json:"required,omitempty"`
}

// EvaluationSchedule configures periodic evaluation execution.
type EvaluationSchedule struct {
	// +optional
	Cron string `json:"cron,omitempty"`
	// +optional
	Timezone string `json:"timezone,omitempty"`
	// +optional
	Suspend bool `json:"suspend,omitempty"`
	// +kubebuilder:validation:Enum=Allow;Forbid;Replace
	// +optional
	ConcurrencyPolicy EvaluationConcurrencyPolicy `json:"concurrencyPolicy,omitempty"`
	// +optional
	SuccessfulRunHistoryLimit int32 `json:"successfulRunHistoryLimit,omitempty"`
	// +optional
	FailedRunHistoryLimit int32 `json:"failedRunHistoryLimit,omitempty"`
}

// EvaluationConcurrencyPolicy controls overlapping scheduled evaluation runs.
type EvaluationConcurrencyPolicy string

const (
	EvaluationConcurrencyAllow   EvaluationConcurrencyPolicy = "Allow"
	EvaluationConcurrencyForbid  EvaluationConcurrencyPolicy = "Forbid"
	EvaluationConcurrencyReplace EvaluationConcurrencyPolicy = "Replace"
)

// EvaluationStatus describes observed evaluation state and recent run history.
type EvaluationStatus struct {
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// +optional
	LastRunRef *LocalObjectReference `json:"lastRunRef,omitempty"`
	// +optional
	LastRunTime *metav1.Time `json:"lastRunTime,omitempty"`
	// +optional
	RunHistory []EvaluationRunSummary `json:"runHistory,omitempty"`
}

// EvaluationRunSpec defines one immutable evaluation execution request.
type EvaluationRunSpec struct {
	EvaluationRef LocalObjectReference `json:"evaluationRef"`
	TargetRef     EvaluationTargetRef  `json:"targetRef"`
	DatasetRef    EvaluationDatasetRef `json:"datasetRef"`
	// +optional
	Graders []EvaluationGrader `json:"graders,omitempty"`
	// +optional
	MetricThresholds []EvaluationMetricThreshold `json:"metricThresholds,omitempty"`
}

// EvaluationRunStatus describes observed state for one EvaluationRun.
type EvaluationRunStatus struct {
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	// +optional
	DatasetDigest string `json:"datasetDigest,omitempty"`
	// +optional
	CaseCount int32 `json:"caseCount,omitempty"`
	// +optional
	CompletedCases int32 `json:"completedCases,omitempty"`
	// +optional
	FailedCases int32 `json:"failedCases,omitempty"`
	// +optional
	CaseResults []EvaluationCaseResult `json:"caseResults,omitempty"`
	// +optional
	TargetJobRef *LocalObjectReference `json:"targetJobRef,omitempty"`
	// +optional
	TargetOutput string `json:"targetOutput,omitempty"`
	// +optional
	Score *float64 `json:"score,omitempty"`
	// +optional
	Passed bool `json:"passed,omitempty"`
	// +optional
	Degraded bool `json:"degraded,omitempty"`
	// +optional
	Metrics []EvaluationMetricResult `json:"metrics,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

// EvaluationCaseResult records the outcome for one dataset case.
type EvaluationCaseResult struct {
	ID string `json:"id"`
	// +optional
	TargetJobRef *LocalObjectReference `json:"targetJobRef,omitempty"`
	// +optional
	TargetOutput string `json:"targetOutput,omitempty"`
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	// +optional
	Passed bool `json:"passed,omitempty"`
	// +optional
	Metrics []EvaluationMetricResult `json:"metrics,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

// EvaluationRunSummary records the result of one observed evaluation run.
type EvaluationRunSummary struct {
	Name string `json:"name"`
	// +optional
	TargetRef EvaluationTargetRef `json:"targetRef,omitempty"`
	// +optional
	DatasetRef EvaluationDatasetRef `json:"datasetRef,omitempty"`
	// +optional
	Phase string `json:"phase,omitempty"`
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`
	// +optional
	Score *float64 `json:"score,omitempty"`
	// +optional
	Passed bool `json:"passed,omitempty"`
	// +optional
	Metrics []EvaluationMetricResult `json:"metrics,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

// EvaluationMetricResult records one metric emitted by a grader.
type EvaluationMetricResult struct {
	Metric string  `json:"metric"`
	Value  float64 `json:"value"`
	// +optional
	Passed bool `json:"passed,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true

// EvaluationList contains a list of Evaluations.
type EvaluationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Evaluation `json:"items"`
}

// +kubebuilder:object:root=true

// EvaluationRunList contains a list of EvaluationRuns.
type EvaluationRunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EvaluationRun `json:"items"`
}

// +kubebuilder:object:root=true

// EvaluationDatasetList contains a list of EvaluationDatasets.
type EvaluationDatasetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []EvaluationDataset `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Evaluation{}, &EvaluationList{}, &EvaluationRun{}, &EvaluationRunList{}, &EvaluationDataset{}, &EvaluationDatasetList{})
}
