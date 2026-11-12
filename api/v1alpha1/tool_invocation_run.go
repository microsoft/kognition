// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import "time"

// ToolInvocationRun records one authorized, durable invocation of a Tool.
// The controller resolves the referenced Tool and ToolClass before creating a
// sandbox, so callers cannot select an image or process argv.
type ToolInvocationRun struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       ToolInvocationRunSpec   `json:"spec" yaml:"spec"`
	Status     ToolInvocationRunStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

func (r *ToolInvocationRun) GetTypeMeta() TypeMeta      { return r.TypeMeta }
func (r *ToolInvocationRun) GetObjectMeta() *ObjectMeta { return &r.ObjectMeta }

// ToolInvocationRunSpec is the immutable, gateway-authorized invocation
// snapshot. ToolResourceVersion binds the execution to one Tool revision.
type ToolInvocationRunSpec struct {
	ToolName            string         `json:"toolName" yaml:"toolName"`
	ToolResourceVersion int64          `json:"toolResourceVersion" yaml:"toolResourceVersion"`
	InvocationID        string         `json:"invocationID" yaml:"invocationID"`
	Input               map[string]any `json:"input,omitempty" yaml:"input,omitempty"`
}

// ToolInvocationRunStatus reports a normalized terminal result to the Tool
// Gateway. Output and ErrorMessage are bounded by the runner.
type ToolInvocationRunStatus struct {
	Phase          Phase          `json:"phase,omitempty" yaml:"phase,omitempty"`
	Conditions     []Condition    `json:"conditions,omitempty" yaml:"conditions,omitempty"`
	Output         map[string]any `json:"output,omitempty" yaml:"output,omitempty"`
	ErrorCode      string         `json:"errorCode,omitempty" yaml:"errorCode,omitempty"`
	ErrorMessage   string         `json:"errorMessage,omitempty" yaml:"errorMessage,omitempty"`
	StartedAt      *time.Time     `json:"startedAt,omitempty" yaml:"startedAt,omitempty"`
	CompletedAt    *time.Time     `json:"completedAt,omitempty" yaml:"completedAt,omitempty"`
	ExecutionMode  string         `json:"executionMode,omitempty" yaml:"executionMode,omitempty"`
	DurationMillis int64          `json:"durationMillis,omitempty" yaml:"durationMillis,omitempty"`
}
