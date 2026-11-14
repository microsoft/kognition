// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import "time"

// Agent declares a cognitive agent workload.
type Agent struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       AgentSpec   `json:"spec" yaml:"spec"`
	Status     AgentStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

func (a *Agent) GetTypeMeta() TypeMeta      { return a.TypeMeta }
func (a *Agent) GetObjectMeta() *ObjectMeta { return &a.ObjectMeta }

// AgentSpec defines the desired state of an Agent.
type AgentSpec struct {
	// ModelRef references the Model resource to use for inference.
	ModelRef LocalObjectReference `json:"modelRef" yaml:"modelRef"`

	// Instructions contains the agent's system prompt or a reference to a Prompt resource.
	Instructions AgentInstructions `json:"instructions" yaml:"instructions"`

	// Tools lists the tools available to this agent.
	Tools []AgentToolRef `json:"tools,omitempty" yaml:"tools,omitempty"`

	// Memory configures memory access for this agent.
	Memory *AgentMemorySpec `json:"memory,omitempty" yaml:"memory,omitempty"`

	// Runtime configures execution runtime for this agent.
	Runtime *AgentRuntimeSpec `json:"runtime,omitempty" yaml:"runtime,omitempty"`

	// Companions lists companion agents that collaborate with this agent.
	Companions []LocalObjectReference `json:"companions,omitempty" yaml:"companions,omitempty"`

	// KnowledgeBaseClaims lists KnowledgeBaseClaim references binding this
	// agent to KnowledgeBases in the same namespace.
	KnowledgeBaseClaims []KnowledgeBaseClaimRef `json:"knowledgeBaseClaims,omitempty" yaml:"knowledgeBaseClaims,omitempty"`

	// AutonomyClassRef references the AutonomyClass governing this agent.
	AutonomyClassRef *LocalObjectReference `json:"autonomyClassRef,omitempty" yaml:"autonomyClassRef,omitempty"`

	// Replicas is the desired number of agent instances (default: 1).
	Replicas *int32 `json:"replicas,omitempty" yaml:"replicas,omitempty"`

	// Probes configures health, readiness, and reasoning probes for this agent.
	Probes *AgentProbeSpec `json:"probes,omitempty" yaml:"probes,omitempty"`

	// RestartLadder defines escalation responses when probes fail.
	RestartLadder *RestartLadder `json:"restartLadder,omitempty" yaml:"restartLadder,omitempty"`
}

// KnowledgeBaseClaimRef references a KnowledgeBaseClaim in the same
// namespace as the Agent, with an optional readiness dependency flag.
type KnowledgeBaseClaimRef struct {
	// Name names the KnowledgeBaseClaim in the same namespace as the Agent.
	Name string `json:"name" yaml:"name"`

	// Required, when true, causes the Agent to depend on the referenced
	// KnowledgeBaseClaim (and its bound KnowledgeBase) being Ready.
	// When false (default) the Agent remains ready even if the claim is
	// unbound, but queries through it will fail gracefully.
	Required bool `json:"required,omitempty" yaml:"required,omitempty"`
}

// AgentRuntimeSpec configures the execution runtime for an agent.
type AgentRuntimeSpec struct {
	// Class selects the runtime adapter (e.g., "default", "native", "autogen").
	Class string `json:"class,omitempty" yaml:"class,omitempty"`

	// Timeout is the maximum duration for a single agent invocation.
	Timeout string `json:"timeout,omitempty" yaml:"timeout,omitempty"`

	// MaxTokens is the maximum number of tokens per invocation.
	MaxTokens *int64 `json:"maxTokens,omitempty" yaml:"maxTokens,omitempty"`
}

// AgentInstructions defines the agent's instructions inline or by reference.
type AgentInstructions struct {
	// Inline contains the instructions text directly.
	Inline string `json:"inline,omitempty" yaml:"inline,omitempty"`

	// Ref references a Prompt resource.
	Ref *LocalObjectReference `json:"ref,omitempty" yaml:"ref,omitempty"`
}

// AgentToolRef references a tool available to the agent with an optional
// readiness dependency flag.
type AgentToolRef struct {
	// Ref names the Tool in the same namespace as the Agent.
	Ref LocalObjectReference `json:"ref" yaml:"ref"`

	// Required, when true, causes the Agent to transition to not-ready
	// if the referenced Tool is not in the Ready phase.
	// When false (default) the Agent remains ready even if the Tool is
	// unavailable, but invocations to it will fail gracefully.
	Required bool `json:"required,omitempty" yaml:"required,omitempty"`
}

// AgentMemorySpec configures memory for an agent.
type AgentMemorySpec struct {
	// Claims lists memory claims to mount.
	Claims []LocalObjectReference `json:"claims,omitempty" yaml:"claims,omitempty"`
}

// AgentStatus describes the observed state of an Agent.
type AgentStatus struct {
	Phase         Phase               `json:"phase,omitempty" yaml:"phase,omitempty"`
	Conditions    []Condition         `json:"conditions,omitempty" yaml:"conditions,omitempty"`
	Replicas      int32               `json:"replicas,omitempty" yaml:"replicas,omitempty"`
	ReadyReplicas int32               `json:"readyReplicas,omitempty" yaml:"readyReplicas,omitempty"`
	RevisionCount int64               `json:"revisionCount,omitempty" yaml:"revisionCount,omitempty"`
	Runtime       *AgentRuntimeStatus `json:"runtime,omitempty" yaml:"runtime,omitempty"`
}

// AgentRuntimeStatus observes the managed runtime workload, independently of
// AgentInstance session phases or successful provider invocations.
type AgentRuntimeStatus struct {
	Ready             string `json:"ready" yaml:"ready"`
	DesiredReplicas   int32  `json:"desiredReplicas" yaml:"desiredReplicas"`
	Replicas          int32  `json:"replicas" yaml:"replicas"`
	ReadyReplicas     int32  `json:"readyReplicas" yaml:"readyReplicas"`
	UpdatedReplicas   int32  `json:"updatedReplicas" yaml:"updatedReplicas"`
	AvailableReplicas int32  `json:"availableReplicas" yaml:"availableReplicas"`
}

// AgentInstance represents a running instance of an Agent.
type AgentInstance struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       AgentInstanceSpec   `json:"spec" yaml:"spec"`
	Status     AgentInstanceStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

func (a *AgentInstance) GetTypeMeta() TypeMeta      { return a.TypeMeta }
func (a *AgentInstance) GetObjectMeta() *ObjectMeta { return &a.ObjectMeta }

// AgentInstanceSpec defines the desired state of an AgentInstance.
type AgentInstanceSpec struct {
	// AgentRef references the parent Agent.
	AgentRef LocalObjectReference `json:"agentRef" yaml:"agentRef"`

	// Revision is the Agent revision this instance was created from.
	Revision int64 `json:"revision" yaml:"revision"`

	// AgentRevision optionally selects an immutable Agent configuration for
	// dispatch. It is distinct from Revision, which identifies the instance.
	AgentRevision *int64 `json:"agentRevision,omitempty" yaml:"agentRevision,omitempty"`

	// SessionContext carries caller-supplied context for this instance's session.
	SessionContext map[string]string `json:"sessionContext,omitempty" yaml:"sessionContext,omitempty"`
}

// AgentInstanceStatus describes the observed state of an AgentInstance.
type AgentInstanceStatus struct {
	Phase            Phase            `json:"phase,omitempty" yaml:"phase,omitempty"`
	Conditions       []Condition      `json:"conditions,omitempty" yaml:"conditions,omitempty"`
	DispatchAttempts int32            `json:"dispatchAttempts,omitempty" yaml:"dispatchAttempts,omitempty"`
	SessionID        string           `json:"sessionID,omitempty" yaml:"sessionID,omitempty"`
	StartTime        *time.Time       `json:"startTime,omitempty" yaml:"startTime,omitempty"`
	TokenUsage       *TokenUsage      `json:"tokenUsage,omitempty" yaml:"tokenUsage,omitempty"`
	ToolCalls        int32            `json:"toolCalls,omitempty" yaml:"toolCalls,omitempty"`
	ToolCallsDetail  []ToolCallDetail `json:"toolCallsDetail,omitempty" yaml:"toolCallsDetail,omitempty"`
	Output           string           `json:"output,omitempty" yaml:"output,omitempty"`
	ProbeStatus      *ProbeStatus     `json:"probeStatus,omitempty" yaml:"probeStatus,omitempty"`
	RestartCount     int32            `json:"restartCount,omitempty" yaml:"restartCount,omitempty"`
}

// TokenUsage reports token consumption for an agent instance.
type TokenUsage struct {
	PromptTokens     int64 `json:"promptTokens,omitempty" yaml:"promptTokens,omitempty"`
	CompletionTokens int64 `json:"completionTokens,omitempty" yaml:"completionTokens,omitempty"`
	TotalTokens      int64 `json:"totalTokens,omitempty" yaml:"totalTokens,omitempty"`
}

// ToolCallDetail records the bounded inputs and result of one runtime tool call.
// Args and Result are JSON strings when their underlying values are structured.
type ToolCallDetail struct {
	Name       string `json:"name" yaml:"name"`
	Args       string `json:"args,omitempty" yaml:"args,omitempty"`
	Result     string `json:"result,omitempty" yaml:"result,omitempty"`
	DurationMS int64  `json:"durationMs,omitempty" yaml:"durationMs,omitempty"`
	Error      string `json:"error,omitempty" yaml:"error,omitempty"`
}

// AgentJob represents a one-shot agent execution.
type AgentJob struct {
	TypeMeta   `json:",inline" yaml:",inline"`
	ObjectMeta `json:"metadata" yaml:"metadata"`
	Spec       AgentJobSpec   `json:"spec" yaml:"spec"`
	Status     AgentJobStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

func (a *AgentJob) GetTypeMeta() TypeMeta      { return a.TypeMeta }
func (a *AgentJob) GetObjectMeta() *ObjectMeta { return &a.ObjectMeta }

// AgentJobSpec defines a one-shot agent task.
type AgentJobSpec struct {
	// AgentRef references an existing Agent resource.
	AgentRef *LocalObjectReference `json:"agentRef,omitempty" yaml:"agentRef,omitempty"`

	// AgentRevision optionally selects an immutable revision of AgentRef for
	// execution while preserving AgentRef as the authorization identity.
	AgentRevision *int64 `json:"agentRevision,omitempty" yaml:"agentRevision,omitempty"`

	// AgentSpec allows inline definition of a one-shot agent workload.
	AgentSpec *AgentSpec `json:"agentSpec,omitempty" yaml:"agentSpec,omitempty"`

	// CompletionPolicy defines when the job is considered complete.
	CompletionPolicy AgentJobCompletionPolicy `json:"completionPolicy,omitempty" yaml:"completionPolicy,omitempty"`

	// BackoffLimit is the number of retry attempts before marking the job failed.
	BackoffLimit *int32 `json:"backoffLimit,omitempty" yaml:"backoffLimit,omitempty"`

	// Input is the one-shot task input payload.
	// Kept for backward compatibility with earlier AgentJob manifests.
	Input string `json:"input,omitempty" yaml:"input,omitempty"`

	// MemoryClaimRef binds this AgentJob to an existing MemoryClaim. The
	// WorkflowReconciler injects this when the parent Workflow (or step)
	// declares a MemoryClaim, giving agents shared access to the bound volume.
	MemoryClaimRef *LocalObjectReference `json:"memoryClaimRef,omitempty" yaml:"memoryClaimRef,omitempty"`
}

// AgentJobCompletionPolicy controls completion semantics for AgentJob.
type AgentJobCompletionPolicy string

const (
	// CompletionPolicySuccess marks the job complete when execution succeeds.
	CompletionPolicySuccess AgentJobCompletionPolicy = "Success"

	// CompletionPolicyAny marks the job complete when the first terminal state is reached.
	CompletionPolicyAny AgentJobCompletionPolicy = "Any"
)

// AgentJobStatus describes the observed state of an AgentJob.
type AgentJobStatus struct {
	Phase          Phase       `json:"phase,omitempty" yaml:"phase,omitempty"`
	Conditions     []Condition `json:"conditions,omitempty" yaml:"conditions,omitempty"`
	Succeeded      int32       `json:"succeeded,omitempty" yaml:"succeeded,omitempty"`
	Failed         int32       `json:"failed,omitempty" yaml:"failed,omitempty"`
	StartTime      *time.Time  `json:"startTime,omitempty" yaml:"startTime,omitempty"`
	CompletionTime *time.Time  `json:"completionTime,omitempty" yaml:"completionTime,omitempty"`
	TokenUsage     *TokenUsage `json:"tokenUsage,omitempty" yaml:"tokenUsage,omitempty"`
	ToolCalls      int32       `json:"toolCalls,omitempty" yaml:"toolCalls,omitempty"`
	Output         string      `json:"output,omitempty" yaml:"output,omitempty"`
}
