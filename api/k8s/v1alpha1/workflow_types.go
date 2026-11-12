// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Entry",type=string,JSONPath=`.spec.entryPoint`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Workflow declares a multi-agent workflow DAG.
type Workflow struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              WorkflowSpec   `json:"spec,omitempty"`
	Status            WorkflowStatus `json:"status,omitempty"`
}

// WorkflowSpec defines the desired state of a Workflow.
type WorkflowSpec struct {
	// Steps defines the workflow steps (agent invocations).
	Steps []WorkflowStep `json:"steps"`

	// EntryPoint is the name of the first step.
	// +optional
	EntryPoint string `json:"entryPoint,omitempty"`

	// Edges defines directed transitions between steps.
	// +optional
	Edges []WorkflowEdge `json:"edges,omitempty"`

	// ApprovalGates define manual approval checkpoints before selected steps.
	// +optional
	ApprovalGates []WorkflowApprovalGate `json:"approvalGates,omitempty"`

	// Budgets enforce workflow-level resource limits.
	// +optional
	Budgets *WorkflowBudget `json:"budgets,omitempty"`

	// Emergent config enables planner-driven DAG mutation with bounded budgets.
	// +optional
	Emergent *WorkflowEmergentSpec `json:"emergent,omitempty"`

	// Parameters are user-supplied key-value pairs available to step input templates.
	// +optional
	Parameters map[string]string `json:"parameters,omitempty"`

	// MaxParallelSteps limits the number of steps that can run concurrently.
	// 0 (default) means unlimited parallelism.
	// +optional
	MaxParallelSteps int32 `json:"maxParallelSteps,omitempty"`

	// FailFast causes the workflow to cancel all pending steps immediately when any step fails.
	// +optional
	FailFast bool `json:"failFast,omitempty"`

	// ContinueOnError allows the workflow to continue executing remaining parallel steps
	// when a step fails, rather than failing immediately.
	// +optional
	ContinueOnError bool `json:"continueOnError,omitempty"`

	// MaxConcurrentRuns limits the number of WorkflowRuns that may execute
	// concurrently for this Workflow. 0 (default) means unlimited.
	// +optional
	MaxConcurrentRuns int32 `json:"maxConcurrentRuns,omitempty"`

	// HistoryLimit is the maximum number of completed WorkflowRuns to retain.
	// 0 (default) means no limit.
	// +optional
	HistoryLimit int32 `json:"historyLimit,omitempty"`

	// MemoryClaim, when set, causes the WorkflowRunReconciler to materialize a
	// per-run MemoryClaim shared across every step in the run.
	// +optional
	MemoryClaim *WorkflowMemoryClaim `json:"memoryClaim,omitempty"`

	// MemoryRetentionPolicy controls the fate of the per-run MemoryClaim when
	// the WorkflowRun reaches a terminal phase. Defaults to Delete.
	// +kubebuilder:validation:Enum=Delete;Retain
	// +optional
	MemoryRetentionPolicy WorkflowMemoryRetentionPolicy `json:"memoryRetentionPolicy,omitempty"`
}

// WorkflowMemoryClaim configures the per-run MemoryClaim used to share state
// across workflow steps.
type WorkflowMemoryClaim struct {
	// MemoryClassName references the MemoryClass that backs the claim.
	MemoryClassName string `json:"memoryClassName"`

	// AccessMode is the requested access mode for the claim.
	// +optional
	AccessMode string `json:"accessMode,omitempty"`
}

// WorkflowMemoryRetentionPolicy controls per-run MemoryClaim teardown.
type WorkflowMemoryRetentionPolicy string

const (
	// WorkflowMemoryRetentionDelete deletes the per-run MemoryClaim on completion.
	WorkflowMemoryRetentionDelete WorkflowMemoryRetentionPolicy = "Delete"
	// WorkflowMemoryRetentionRetain preserves the per-run MemoryClaim on completion.
	WorkflowMemoryRetentionRetain WorkflowMemoryRetentionPolicy = "Retain"
)

// WorkflowEmergentSpec configures emergent DAG mutation behavior.
type WorkflowEmergentSpec struct {
	// Enabled activates emergent behavior.
	Enabled bool `json:"enabled,omitempty"`

	// Budgets bounds planner-driven DAG mutations.
	// +optional
	Budgets WorkflowEmergentBudgets `json:"budgets,omitempty"`
}

// WorkflowEmergentBudgets bounds planner-driven DAG mutations.
type WorkflowEmergentBudgets struct {
	// MaxSteps is the maximum number of emergent steps the planner may add.
	// +optional
	MaxSteps int32 `json:"maxSteps,omitempty"`

	// MaxTokens is the maximum tokens the planner may spend on emergent steps.
	// +optional
	MaxTokens int64 `json:"maxTokens,omitempty"`

	// MaxCost is the maximum cost budget (e.g. "$0.50").
	// +optional
	MaxCost string `json:"maxCost,omitempty"`
}

// WorkflowApprovalGate pauses execution on a specific edge until approved.
type WorkflowApprovalGate struct {
	// From is the source step of the edge requiring approval.
	From string `json:"from"`

	// To is the destination step of the edge requiring approval.
	To string `json:"to"`

	// RequiredApprovers is the minimum number of distinct approvers needed
	// to open this gate. Zero or 1 means a single approval suffices.
	RequiredApprovers int32 `json:"requiredApprovers,omitempty"`

	// Timeout is the maximum duration (e.g. "30m", "2h") the gate may remain
	// pending before being auto-rejected. Empty means no timeout.
	// +optional
	Timeout string `json:"timeout,omitempty"`
}

// WorkflowBudget defines high-level execution limits for a workflow.
type WorkflowBudget struct {
	// MaxDuration is the maximum wall-clock time (e.g. "30m").
	// +optional
	MaxDuration string `json:"maxDuration,omitempty"`

	// MaxTokens is the maximum total tokens for the workflow.
	// +optional
	MaxTokens int64 `json:"maxTokens,omitempty"`

	// MaxIterations is the maximum number of step iterations.
	// +optional
	MaxIterations int32 `json:"maxIterations,omitempty"`

	// MaxTokensPerCycle is the maximum tokens per iteration cycle.
	// +optional
	MaxTokensPerCycle int64 `json:"maxTokensPerCycle,omitempty"`
}

// WorkflowStepType discriminates between agent-backed and tool-backed steps.
// +kubebuilder:validation:Enum=agent;tool
type WorkflowStepType string

const (
	// WorkflowStepTypeAgent dispatches the step via an AgentJob (default).
	WorkflowStepTypeAgent WorkflowStepType = "agent"
	// WorkflowStepTypeTool invokes a Tool directly, with no AgentJob or LLM round-trip.
	WorkflowStepTypeTool WorkflowStepType = "tool"
)

// WorkflowStep defines a single step in a workflow DAG.
// Steps are pure execution nodes; all topology is expressed through Edges.
type WorkflowStep struct {
	// Name is the unique step identifier within this workflow.
	Name string `json:"name"`

	// Type selects the dispatch mechanism for this step. Defaults to "agent".
	// +optional
	Type WorkflowStepType `json:"type,omitempty"`

	// AgentRef references the Agent to invoke for this step.
	// Required when type="agent"; optional when type="tool" (acts as the
	// identity principal for ToolGrant evaluation when present).
	// +optional
	AgentRef LocalObjectReference `json:"agentRef,omitempty"`

	// ToolRef references the Tool to invoke for this step.
	// Required when type="tool"; must be empty when type="agent".
	// +optional
	ToolRef LocalObjectReference `json:"toolRef,omitempty"`

	// Input is the input to the agent for this step.
	// +optional
	Input string `json:"input,omitempty"`

	// Timeout is the maximum duration for this step (e.g. "5m").
	// +optional
	Timeout string `json:"timeout,omitempty"`

	// Condition is a CEL expression that gates step dispatch.
	// If present and evaluates to false, the step is set to Skipped.
	// +optional
	Condition string `json:"condition,omitempty"`

	// Retry configures retry behavior for this step.
	// +optional
	Retry *WorkflowStepRetry `json:"retry,omitempty"`

	// ContinueOnError allows this step to fail without failing the workflow.
	// +optional
	ContinueOnError bool `json:"continueOnError,omitempty"`

	// MemoryClaim overrides the workflow-level MemoryClaim for this step.
	// +optional
	MemoryClaim *WorkflowMemoryClaim `json:"memoryClaim,omitempty"`
}

// WorkflowStepRetry configures retry behavior for a workflow step.
type WorkflowStepRetry struct {
	// MaxAttempts is the maximum total number of attempts (initial + retries).
	// Values <= 1 disable retry; the step is attempted at most once.
	MaxAttempts int32 `json:"maxAttempts,omitempty"`

	// Backoff is the backoff duration between retries (e.g. "10s").
	Backoff string `json:"backoff,omitempty"`

	// BackoffMultiplier scales backoff exponentially per attempt. Default 2.0.
	// +optional
	BackoffMultiplier float64 `json:"backoffMultiplier,omitempty"`

	// MaxBackoff caps the computed backoff duration (e.g. "1m").
	// +optional
	MaxBackoff string `json:"maxBackoff,omitempty"`
}

// WorkflowEdge defines a directed transition between workflow steps.
type WorkflowEdge struct {
	// From is the source step name.
	From string `json:"from"`

	// To is the destination step name.
	To string `json:"to"`

	// Condition is a CEL expression for conditional transitions.
	// +optional
	Condition string `json:"condition,omitempty"`
}

// WorkflowStatus describes the observed state of a Workflow.
type WorkflowStatus struct {
	// Phase is the current lifecycle phase.
	// +optional
	Phase string `json:"phase,omitempty"`

	// Conditions represent the latest available observations.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// CompletedSteps lists the names of steps that have finished.
	// +optional
	CompletedSteps []string `json:"completedSteps,omitempty"`

	// ActiveStep is the name of the currently executing step.
	// +optional
	ActiveStep string `json:"activeStep,omitempty"`

	// ActiveSteps lists the names of concurrently executing steps.
	// +optional
	ActiveSteps []string `json:"activeSteps,omitempty"`

	// StepStatuses reports the status of individual workflow steps.
	// +optional
	StepStatuses []WorkflowStepStatus `json:"stepStatuses,omitempty"`
}

// +kubebuilder:object:root=true

// WorkflowList contains a list of Workflows.
type WorkflowList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Workflow `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Workflow",type=string,JSONPath=`.spec.workflowRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Active",type=string,JSONPath=`.status.activeStep`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// WorkflowRun represents a single execution of a Workflow.
type WorkflowRun struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              WorkflowRunSpec   `json:"spec,omitempty"`
	Status            WorkflowRunStatus `json:"status,omitempty"`
}

// WorkflowRunSpec defines the desired state of a WorkflowRun.
type WorkflowRunSpec struct {
	// WorkflowRef references the Workflow to execute.
	WorkflowRef LocalObjectReference `json:"workflowRef"`

	// Input is the initial input for the workflow.
	// +optional
	Input string `json:"input,omitempty"`

	// Parameters are runtime parameters passed to the workflow.
	// +optional
	Parameters map[string]string `json:"parameters,omitempty"`
}

// WorkflowRunStatus describes the observed state of a WorkflowRun.
type WorkflowRunStatus struct {
	// Phase is the current lifecycle phase.
	// +optional
	Phase string `json:"phase,omitempty"`

	// Conditions represent the latest available observations.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// CompletedSteps lists the names of steps that have finished.
	// +optional
	CompletedSteps []string `json:"completedSteps,omitempty"`

	// ActiveStep is the name of the currently executing step.
	// +optional
	ActiveStep string `json:"activeStep,omitempty"`

	// ActiveSteps lists the names of concurrently executing steps.
	// +optional
	ActiveSteps []string `json:"activeSteps,omitempty"`

	// StepStatuses reports the status of individual workflow steps.
	// +optional
	StepStatuses []WorkflowStepStatus `json:"stepStatuses,omitempty"`

	// SnapshotSpec is the resolved WorkflowSpec snapshotted on first execution.
	// Used to continue execution if the parent Workflow template is deleted.
	// +optional
	SnapshotSpec *WorkflowSpec `json:"snapshotSpec,omitempty"`

	// StartTime is when the run started.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// CompletionTime is when the run completed.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`

	// Output is the final output of the workflow.
	// +optional
	Output string `json:"output,omitempty"`

	// ApprovalRecords is the audit log of approve/reject decisions recorded
	// against this run's approval gates.
	// +optional
	ApprovalRecords []WorkflowApprovalRecord `json:"approvalRecords,omitempty"`
}

// WorkflowApprovalRecord captures a single approve/reject decision against an
// approval gate, including who made it and when.
type WorkflowApprovalRecord struct {
	Step     string `json:"step"`
	Decision string `json:"decision"`
	// +optional
	Approver string `json:"approver,omitempty"`
	// +optional
	Comment   string `json:"comment,omitempty"`
	Timestamp string `json:"timestamp"`
}

// WorkflowStepStatus reports the status of an individual workflow step.
type WorkflowStepStatus struct {
	// Name is the step identifier.
	Name string `json:"name"`

	// Phase is the step's current phase.
	// +optional
	Phase string `json:"phase,omitempty"`

	// Output is the step's output.
	// +optional
	Output string `json:"output,omitempty"`

	// Attempts is the number of execution attempts for this step.
	// +optional
	Attempts int32 `json:"attempts,omitempty"`

	// StartedAt is the time the current attempt started.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// NextRetryAt is the time at which the next retry is eligible to dispatch.
	// +optional
	NextRetryAt *metav1.Time `json:"nextRetryAt,omitempty"`
}

// +kubebuilder:object:root=true

// WorkflowRunList contains a list of WorkflowRuns.
type WorkflowRunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WorkflowRun `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Workflow{}, &WorkflowList{})
	SchemeBuilder.Register(&WorkflowRun{}, &WorkflowRunList{})
}
