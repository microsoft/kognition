// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import "time"

// Workflow declares a multi-agent orchestration graph.
// Topology is edge-primary: steps are pure execution nodes, edges define transitions.
type Workflow struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       WorkflowSpec   `json:"spec" yaml:"spec"`
	Status     WorkflowStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

func (w *Workflow) GetTypeMeta() TypeMeta      { return w.TypeMeta }
func (w *Workflow) GetObjectMeta() *ObjectMeta { return &w.ObjectMeta }

// WorkflowSpec defines the desired state of a Workflow.
type WorkflowSpec struct {
	// Steps defines the nodes in the workflow graph.
	Steps []WorkflowStep `json:"steps" yaml:"steps"`

	// Edges define the directed transitions between steps.
	// All topology is expressed through edges; steps with no inbound edges are entry points.
	Edges []WorkflowEdge `json:"edges,omitempty" yaml:"edges,omitempty"`

	// EntryPoint optionally designates the starting step for cyclic graphs.
	// For DAGs this is inferred (steps with in-degree 0). Required for cyclic workflows.
	EntryPoint string `json:"entryPoint,omitempty" yaml:"entryPoint,omitempty"`

	// ApprovalGates define manual approval checkpoints before selected steps.
	ApprovalGates []WorkflowApprovalGate `json:"approvalGates,omitempty" yaml:"approvalGates,omitempty"`

	// Budgets enforce workflow-level resource limits.
	Budgets *WorkflowBudget `json:"budgets,omitempty" yaml:"budgets,omitempty"`

	// Emergent config enables planner-driven DAG mutation with bounded budgets.
	Emergent *WorkflowEmergentSpec `json:"emergent,omitempty" yaml:"emergent,omitempty"`

	// Parameters are user-supplied key-value pairs available to step input templates via {{ .Params.key }}.
	Parameters map[string]string `json:"parameters,omitempty" yaml:"parameters,omitempty"`

	// MaxParallelSteps limits the number of steps that can run concurrently.
	// 0 (default) means unlimited parallelism.
	MaxParallelSteps int32 `json:"maxParallelSteps,omitempty" yaml:"maxParallelSteps,omitempty"`

	// FailFast causes the workflow to cancel all pending steps immediately when any step fails.
	FailFast bool `json:"failFast,omitempty" yaml:"failFast,omitempty"`

	// ContinueOnError allows the workflow to continue executing remaining parallel steps
	// when a step fails, rather than failing immediately.
	ContinueOnError bool `json:"continueOnError,omitempty" yaml:"continueOnError,omitempty"`

	// MaxConcurrentRuns limits the number of WorkflowRuns that may execute
	// concurrently for this Workflow. 0 (default) means unlimited.
	MaxConcurrentRuns int32 `json:"maxConcurrentRuns,omitempty" yaml:"maxConcurrentRuns,omitempty"`

	// HistoryLimit is the maximum number of completed WorkflowRuns to retain.
	// Oldest completed runs are garbage-collected when the limit is exceeded.
	// 0 (default) means no limit.
	HistoryLimit int32 `json:"historyLimit,omitempty" yaml:"historyLimit,omitempty"`

	// MemoryClaim, when set, causes the WorkflowRunReconciler to materialize a
	// per-run MemoryClaim shared across every step in the run so steps may
	// exchange data through the bound MemoryVolume.
	MemoryClaim *WorkflowMemoryClaim `json:"memoryClaim,omitempty" yaml:"memoryClaim,omitempty"`

	// MemoryRetentionPolicy controls the fate of the per-run MemoryClaim once
	// the WorkflowRun reaches a terminal phase. Defaults to Delete.
	MemoryRetentionPolicy WorkflowMemoryRetentionPolicy `json:"memoryRetentionPolicy,omitempty" yaml:"memoryRetentionPolicy,omitempty"`
}

// WorkflowMemoryClaim configures the per-run MemoryClaim used to share state
// across workflow steps.
type WorkflowMemoryClaim struct {
	// MemoryClassName references the MemoryClass that backs the claim. Required.
	MemoryClassName string `json:"memoryClassName" yaml:"memoryClassName"`

	// AccessMode is the requested access mode for the claim (e.g., "ReadWriteMany").
	AccessMode string `json:"accessMode,omitempty" yaml:"accessMode,omitempty"`
}

// WorkflowMemoryRetentionPolicy controls per-run MemoryClaim teardown.
type WorkflowMemoryRetentionPolicy string

const (
	// WorkflowMemoryRetentionDelete deletes the per-run MemoryClaim when the
	// WorkflowRun reaches a terminal phase. This is the default.
	WorkflowMemoryRetentionDelete WorkflowMemoryRetentionPolicy = "Delete"
	// WorkflowMemoryRetentionRetain preserves the per-run MemoryClaim after
	// the WorkflowRun completes, leaving cleanup to the operator.
	WorkflowMemoryRetentionRetain WorkflowMemoryRetentionPolicy = "Retain"
)

// WorkflowEmergentSpec configures emergent DAG mutation behavior.
type WorkflowEmergentSpec struct {
	Enabled bool                    `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	Budgets WorkflowEmergentBudgets `json:"budgets,omitempty" yaml:"budgets,omitempty"`
}

// WorkflowEmergentBudgets bounds planner-driven DAG mutations.
type WorkflowEmergentBudgets struct {
	MaxSteps  int32  `json:"maxSteps,omitempty" yaml:"maxSteps,omitempty"`
	MaxTokens int64  `json:"maxTokens,omitempty" yaml:"maxTokens,omitempty"`
	MaxCost   string `json:"maxCost,omitempty" yaml:"maxCost,omitempty"`
}

// WorkflowEdge defines a dependency edge in the workflow DAG.
type WorkflowEdge struct {
	From string `json:"from" yaml:"from"`
	To   string `json:"to" yaml:"to"`

	// Condition is a CEL expression for conditional transitions.
	Condition string `json:"condition,omitempty" yaml:"condition,omitempty"`
}

// WorkflowApprovalGate pauses execution before traversing a specific edge until approved.
type WorkflowApprovalGate struct {
	// Edge identifies the transition that requires approval (from→to).
	From string `json:"from" yaml:"from"`
	To   string `json:"to" yaml:"to"`
	// RequiredApprovers is the minimum number of distinct approvers required
	// to open this gate. Zero or 1 means a single approval suffices.
	RequiredApprovers int32 `json:"requiredApprovers,omitempty" yaml:"requiredApprovers,omitempty"`
	// Timeout is the maximum duration (e.g. "30m", "2h") the gate may remain
	// pending before being auto-rejected. Empty means no timeout.
	Timeout string `json:"timeout,omitempty" yaml:"timeout,omitempty"`
}

// WorkflowBudget defines high-level execution limits for a workflow.
type WorkflowBudget struct {
	MaxDuration       string `json:"maxDuration,omitempty" yaml:"maxDuration,omitempty"`
	MaxTokens         int64  `json:"maxTokens,omitempty" yaml:"maxTokens,omitempty"`
	MaxIterations     int32  `json:"maxIterations,omitempty" yaml:"maxIterations,omitempty"`
	MaxTokensPerCycle int64  `json:"maxTokensPerCycle,omitempty" yaml:"maxTokensPerCycle,omitempty"`
}

// WorkflowStepType discriminates between agent-backed and tool-backed steps.
type WorkflowStepType string

const (
	// WorkflowStepTypeAgent dispatches the step via an AgentJob (default).
	WorkflowStepTypeAgent WorkflowStepType = "agent"
	// WorkflowStepTypeTool invokes a Tool directly, with no AgentJob or LLM round-trip.
	WorkflowStepTypeTool WorkflowStepType = "tool"
)

// WorkflowStep defines a single node in a workflow graph.
// Topology is expressed exclusively through WorkflowEdge declarations.
type WorkflowStep struct {
	// Name is the unique identifier of this step within the workflow.
	Name string `json:"name" yaml:"name"`

	// Type selects the dispatch mechanism for this step. Defaults to "agent".
	Type WorkflowStepType `json:"type,omitempty" yaml:"type,omitempty"`

	// AgentRef references the Agent to execute for this step.
	// Required when type="agent"; optional when type="tool" (acts as the
	// identity principal for ToolGrant evaluation when present).
	AgentRef LocalObjectReference `json:"agentRef,omitempty" yaml:"agentRef,omitempty"`

	// ToolRef references the Tool to invoke for this step.
	// Required when type="tool"; must be empty when type="agent".
	ToolRef LocalObjectReference `json:"toolRef,omitempty" yaml:"toolRef,omitempty"`

	// Input is a Go template string for step input (may reference outputs of prior steps).
	// Example: "Summarize: {{.Steps.fetch.Output}}"
	Input string `json:"input,omitempty" yaml:"input,omitempty"`

	// Timeout overrides the workflow-level timeout for this step.
	Timeout string `json:"timeout,omitempty" yaml:"timeout,omitempty"`

	// Condition is a CEL expression that gates step dispatch.
	// If present and evaluates to false, the step is set to Skipped.
	Condition string `json:"condition,omitempty" yaml:"condition,omitempty"`

	// Retry configures retry behavior for this step.
	Retry *WorkflowStepRetry `json:"retry,omitempty" yaml:"retry,omitempty"`

	// ContinueOnError allows this step to fail without failing the workflow.
	ContinueOnError bool `json:"continueOnError,omitempty" yaml:"continueOnError,omitempty"`

	// MemoryClaim overrides the workflow-level MemoryClaim for this step,
	// materializing an isolated per-step MemoryClaim instead of sharing the
	// run-level one.
	MemoryClaim *WorkflowMemoryClaim `json:"memoryClaim,omitempty" yaml:"memoryClaim,omitempty"`
}

// WorkflowStepRetry configures retry semantics for a workflow step.
type WorkflowStepRetry struct {
	// MaxAttempts is the maximum total number of attempts (initial + retries).
	// Values <= 1 disable retry; the step is attempted at most once.
	MaxAttempts int32 `json:"maxAttempts" yaml:"maxAttempts"`

	// Backoff is the backoff duration between retries (e.g., "5s", "1m").
	Backoff string `json:"backoff,omitempty" yaml:"backoff,omitempty"`

	// BackoffMultiplier scales the backoff exponentially per attempt. Default 2.0.
	BackoffMultiplier float64 `json:"backoffMultiplier,omitempty" yaml:"backoffMultiplier,omitempty"`

	// MaxBackoff caps the computed backoff duration (e.g., "1m").
	MaxBackoff string `json:"maxBackoff,omitempty" yaml:"maxBackoff,omitempty"`
}

// WorkflowStatus describes the observed state of a Workflow.
type WorkflowStatus struct {
	Phase          Phase                `json:"phase,omitempty" yaml:"phase,omitempty"`
	Conditions     []Condition          `json:"conditions,omitempty" yaml:"conditions,omitempty"`
	CompletedSteps []string             `json:"completedSteps,omitempty" yaml:"completedSteps,omitempty"`
	ActiveStep     string               `json:"activeStep,omitempty" yaml:"activeStep,omitempty"`
	ActiveSteps    []string             `json:"activeSteps,omitempty" yaml:"activeSteps,omitempty"`
	StepStatuses   []WorkflowStepStatus `json:"stepStatuses,omitempty" yaml:"stepStatuses,omitempty"`
}

// WorkflowRun represents a single execution of a Workflow.
type WorkflowRun struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       WorkflowRunSpec   `json:"spec" yaml:"spec"`
	Status     WorkflowRunStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

func (w *WorkflowRun) GetTypeMeta() TypeMeta      { return w.TypeMeta }
func (w *WorkflowRun) GetObjectMeta() *ObjectMeta { return &w.ObjectMeta }

// WorkflowRunSpec defines parameters for a workflow execution.
type WorkflowRunSpec struct {
	WorkflowRef LocalObjectReference `json:"workflowRef" yaml:"workflowRef"`
	Input       string               `json:"input,omitempty" yaml:"input,omitempty"`
	Parameters  map[string]string    `json:"parameters,omitempty" yaml:"parameters,omitempty"`
}

// WorkflowRunStatus describes the observed state of a WorkflowRun.
type WorkflowRunStatus struct {
	Phase          Phase                `json:"phase,omitempty" yaml:"phase,omitempty"`
	Conditions     []Condition          `json:"conditions,omitempty" yaml:"conditions,omitempty"`
	CompletedSteps []string             `json:"completedSteps,omitempty" yaml:"completedSteps,omitempty"`
	ActiveStep     string               `json:"activeStep,omitempty" yaml:"activeStep,omitempty"`
	ActiveSteps    []string             `json:"activeSteps,omitempty" yaml:"activeSteps,omitempty"`
	StepStatuses   []WorkflowStepStatus `json:"stepStatuses,omitempty" yaml:"stepStatuses,omitempty"`
	// SnapshotSpec is the resolved WorkflowSpec snapshotted on first execution.
	// Used to continue execution if the parent Workflow template is deleted.
	SnapshotSpec   *WorkflowSpec `json:"snapshotSpec,omitempty" yaml:"snapshotSpec,omitempty"`
	StartTime      *time.Time    `json:"startTime,omitempty" yaml:"startTime,omitempty"`
	CompletionTime *time.Time    `json:"completionTime,omitempty" yaml:"completionTime,omitempty"`
	Output         string        `json:"output,omitempty" yaml:"output,omitempty"`
	// ApprovalRecords is the audit log of approve/reject decisions recorded
	// against this run's approval gates.
	ApprovalRecords []WorkflowApprovalRecord `json:"approvalRecords,omitempty" yaml:"approvalRecords,omitempty"`
}

// WorkflowApprovalRecord captures a single approve/reject decision against an
// approval gate, including who made it and when.
type WorkflowApprovalRecord struct {
	Step      string `json:"step" yaml:"step"`
	Decision  string `json:"decision" yaml:"decision"`
	Approver  string `json:"approver,omitempty" yaml:"approver,omitempty"`
	Comment   string `json:"comment,omitempty" yaml:"comment,omitempty"`
	Timestamp string `json:"timestamp" yaml:"timestamp"`
}

// WorkflowStepStatus reports the status of an individual workflow step.
type WorkflowStepStatus struct {
	Name        string     `json:"name" yaml:"name"`
	Phase       Phase      `json:"phase,omitempty" yaml:"phase,omitempty"`
	Output      string     `json:"output,omitempty" yaml:"output,omitempty"`
	Attempts    int32      `json:"attempts,omitempty" yaml:"attempts,omitempty"`
	StartedAt   *time.Time `json:"startedAt,omitempty" yaml:"startedAt,omitempty"`
	NextRetryAt *time.Time `json:"nextRetryAt,omitempty" yaml:"nextRetryAt,omitempty"`
}
