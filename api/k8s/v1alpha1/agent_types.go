// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.runtime.ready`
// +kubebuilder:printcolumn:name="Up-to-date",type=integer,JSONPath=`.status.runtime.updatedReplicas`
// +kubebuilder:printcolumn:name="Available",type=integer,JSONPath=`.status.runtime.availableReplicas`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`,priority=1

// Agent declares a cognitive agent workload.
type Agent struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AgentSpec   `json:"spec,omitempty"`
	Status            AgentStatus `json:"status,omitempty"`
}

// AgentSpec defines the desired state of an Agent.
type AgentSpec struct {
	// ModelRef references the Model resource to use for inference.
	ModelRef LocalObjectReference `json:"modelRef"`

	// Instructions contains the agent's system prompt or a reference to a Prompt resource.
	Instructions AgentInstructions `json:"instructions"`

	// Tools lists the tools available to this agent.
	// +optional
	Tools []AgentToolRef `json:"tools,omitempty"`

	// Memory configures memory access for this agent.
	// +optional
	Memory *AgentMemorySpec `json:"memory,omitempty"`

	// Runtime configures execution runtime for this agent.
	// +optional
	Runtime *AgentRuntimeSpec `json:"runtime,omitempty"`

	// Companions lists companion agents that collaborate with this agent.
	// +optional
	Companions []LocalObjectReference `json:"companions,omitempty"`

	// KnowledgeBaseClaims lists KnowledgeBaseClaim references binding this
	// agent to KnowledgeBases in the same namespace. Agents never reference
	// a KnowledgeBase directly; the claim carries access mode, retrieval
	// overrides, and usage quotas.
	// +optional
	KnowledgeBaseClaims []KnowledgeBaseClaimRef `json:"knowledgeBaseClaims,omitempty"`

	// AutonomyClassRef references the AutonomyClass governing this agent.
	// +optional
	AutonomyClassRef *LocalObjectReference `json:"autonomyClassRef,omitempty"`

	// Replicas is the desired number of agent instances (default: 1).
	// +optional
	// +kubebuilder:default=1
	Replicas *int32 `json:"replicas,omitempty"`

	// Probes configures health, readiness, and reasoning probes for this agent.
	// +optional
	Probes *AgentProbeSpec `json:"probes,omitempty"`

	// RestartLadder defines escalation responses when probes fail.
	// +optional
	RestartLadder *RestartLadder `json:"restartLadder,omitempty"`
}

// AgentInstructions defines the agent's instructions inline or by reference.
type AgentInstructions struct {
	// Inline contains the instructions text directly.
	// +optional
	Inline string `json:"inline,omitempty"`

	// Ref references a Prompt resource.
	// +optional
	Ref *LocalObjectReference `json:"ref,omitempty"`
}

// AgentToolRef references a tool available to the agent.
type AgentToolRef struct {
	// Ref names the Tool in the same namespace as the Agent.
	Ref LocalObjectReference `json:"ref"`

	// Required, when true, causes the Agent to transition to not-ready
	// if the referenced Tool is not in the Ready phase.
	// +optional
	Required bool `json:"required,omitempty"`
}

// AgentMemorySpec configures memory for an agent.
type AgentMemorySpec struct {
	// Claims lists memory claims to mount.
	// +optional
	Claims []LocalObjectReference `json:"claims,omitempty"`
}

// AgentRuntimeSpec configures the execution runtime for an agent.
type AgentRuntimeSpec struct {
	// Class selects the runtime adapter (e.g., "default", "native", "autogen").
	// +optional
	Class string `json:"class,omitempty"`

	// Timeout is the maximum duration for a single agent invocation.
	// +optional
	Timeout string `json:"timeout,omitempty"`

	// MaxTokens is the maximum number of tokens per invocation.
	// +optional
	MaxTokens *int64 `json:"maxTokens,omitempty"`
}

// KnowledgeBaseClaimRef references a KnowledgeBaseClaim in the same
// namespace as the Agent, with an optional readiness dependency.
type KnowledgeBaseClaimRef struct {
	// Name names the KnowledgeBaseClaim in the same namespace as the Agent.
	Name string `json:"name"`

	// Required, when true, causes the Agent to depend on the referenced
	// KnowledgeBaseClaim (and its bound KnowledgeBase) being Ready.
	// +optional
	Required bool `json:"required,omitempty"`
}

// AgentProbeSpec configures probes for an agent.
type AgentProbeSpec struct {
	// HealthProbe configures runtime liveness checks.
	// +optional
	HealthProbe *AgentHealthProbe `json:"healthProbe,omitempty"`

	// ReadinessProbe configures model/tool/memory reachability checks.
	// +optional
	ReadinessProbe *AgentReadinessProbe `json:"readinessProbe,omitempty"`

	// ReasoningProbe configures canary-task validation checks.
	// +optional
	ReasoningProbe *AgentReasoningProbe `json:"reasoningProbe,omitempty"`
}

// AgentHealthProbe defines runtime liveness probe configuration.
type AgentHealthProbe struct {
	// IntervalSeconds is how often (in seconds) to perform the probe.
	// +optional
	// +kubebuilder:default=30
	IntervalSeconds int32 `json:"intervalSeconds,omitempty"`

	// TimeoutSeconds is the probe timeout in seconds.
	// +optional
	// +kubebuilder:default=5
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`

	// FailureThreshold is the consecutive failures before marking unhealthy.
	// +optional
	// +kubebuilder:default=3
	FailureThreshold int32 `json:"failureThreshold,omitempty"`

	// SuccessThreshold is the consecutive successes to recover.
	// +optional
	// +kubebuilder:default=1
	SuccessThreshold int32 `json:"successThreshold,omitempty"`
}

// AgentReadinessProbe checks model, tool, and memory reachability.
type AgentReadinessProbe struct {
	// IntervalSeconds is how often (in seconds) to perform the probe.
	// +optional
	// +kubebuilder:default=30
	IntervalSeconds int32 `json:"intervalSeconds,omitempty"`

	// TimeoutSeconds is the probe timeout in seconds.
	// +optional
	// +kubebuilder:default=10
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`

	// FailureThreshold is the consecutive failures before marking unready.
	// +optional
	// +kubebuilder:default=3
	FailureThreshold int32 `json:"failureThreshold,omitempty"`

	// SuccessThreshold is the consecutive successes to recover.
	// +optional
	// +kubebuilder:default=1
	SuccessThreshold int32 `json:"successThreshold,omitempty"`
}

// AgentReasoningProbe configures periodic canary-task validation.
type AgentReasoningProbe struct {
	// CanaryTask is the input prompt for the canary evaluation.
	CanaryTask string `json:"canaryTask"`

	// ExpectedOutcome is the exact UTF-8 output the canary must return.
	ExpectedOutcome string `json:"expectedOutcome"`

	// IntervalSeconds is how often (in seconds) to perform the probe.
	// +optional
	// +kubebuilder:default=300
	IntervalSeconds int32 `json:"intervalSeconds,omitempty"`

	// TimeoutSeconds is the probe timeout in seconds.
	// +optional
	// +kubebuilder:default=60
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`

	// MaxTokens is the token budget for the canary run.
	// +optional
	// +kubebuilder:default=256
	MaxTokens int32 `json:"maxTokens,omitempty"`

	// MaxInputTokens is the maximum exact input-token count accepted for the
	// canary request.
	// +optional
	// +kubebuilder:default=4096
	MaxInputTokens int32 `json:"maxInputTokens,omitempty"`

	// FailureThreshold is the consecutive failures before marking degraded.
	// +optional
	// +kubebuilder:default=2
	FailureThreshold int32 `json:"failureThreshold,omitempty"`

	// SuccessThreshold is the consecutive successes to recover.
	// +optional
	// +kubebuilder:default=1
	SuccessThreshold int32 `json:"successThreshold,omitempty"`
}

// RestartLadder defines escalation responses when probes fail.
type RestartLadder struct {
	// Steps defines the escalation steps.
	// +optional
	Steps []RestartLadderStep `json:"steps"`
}

// RestartLadderStep defines a single escalation response.
type RestartLadderStep struct {
	// Action is the response action to execute.
	Action RestartAction `json:"action"`

	// StabilisationSeconds is how long to wait after this action before
	// deciding whether to escalate.
	// +optional
	// +kubebuilder:default=60
	StabilisationSeconds int32 `json:"stabilisationSeconds,omitempty"`

	// MaxAttempts is the maximum number of times this step can be tried
	// before escalating to the next step.
	// +optional
	// +kubebuilder:default=1
	MaxAttempts int32 `json:"maxAttempts,omitempty"`
}

// RestartAction enumerates supported restart-ladder response actions.
// +kubebuilder:validation:Enum=Restart;RollbackRevision;DegradeModel;Quarantine;Alert
type RestartAction string

const (
	RestartActionRestart          RestartAction = "Restart"
	RestartActionRollbackRevision RestartAction = "RollbackRevision"
	RestartActionDegradeModel     RestartAction = "DegradeModel"
	RestartActionQuarantine       RestartAction = "Quarantine"
	RestartActionAlert            RestartAction = "Alert"
)

// AgentStatus describes the observed state of an Agent.
type AgentStatus struct {
	// Phase is the current lifecycle phase.
	// +optional
	Phase string `json:"phase,omitempty"`

	// Conditions represent the latest available observations.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Replicas is the desired number of controller-owned instances.
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// ReadyReplicas is the number of ready instances.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// RevisionCount is the number of spec revisions processed.
	// +optional
	RevisionCount int64 `json:"revisionCount,omitempty"`

	// Runtime observes the backing Deployment rather than completed sessions.
	// +optional
	Runtime *AgentRuntimeStatus `json:"runtime,omitempty"`
}

// AgentRuntimeStatus is the observed rollout of the managed runtime Deployment.
type AgentRuntimeStatus struct {
	Ready             string `json:"ready"`
	DesiredReplicas   int32  `json:"desiredReplicas"`
	Replicas          int32  `json:"replicas"`
	ReadyReplicas     int32  `json:"readyReplicas"`
	UpdatedReplicas   int32  `json:"updatedReplicas"`
	AvailableReplicas int32  `json:"availableReplicas"`
}

// +kubebuilder:object:root=true

// AgentList contains a list of Agents.
type AgentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Agent `json:"items"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Agent",type=string,JSONPath=`.spec.agentRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// AgentInstance represents a running instance of an Agent.
type AgentInstance struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              AgentInstanceSpec   `json:"spec,omitempty"`
	Status            AgentInstanceStatus `json:"status,omitempty"`
}

// AgentInstanceSpec defines the desired state of an AgentInstance.
type AgentInstanceSpec struct {
	// AgentRef references the parent Agent.
	AgentRef LocalObjectReference `json:"agentRef"`

	// Revision is the Agent revision this instance was created from.
	Revision int64 `json:"revision"`

	// AgentRevision optionally selects an immutable Agent configuration for
	// dispatch. It is distinct from Revision, which identifies the instance.
	// +optional
	AgentRevision *int64 `json:"agentRevision,omitempty"`

	// SessionContext carries caller-supplied context for this instance's session.
	// +optional
	SessionContext map[string]string `json:"sessionContext,omitempty"`
}

// AgentInstanceStatus describes the observed state of an AgentInstance.
type AgentInstanceStatus struct {
	// Phase is the current lifecycle phase.
	// +optional
	Phase string `json:"phase,omitempty"`

	// Conditions represent the latest available observations.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// DispatchAttempts counts scheduling attempts for this instance.
	// +optional
	DispatchAttempts int32 `json:"dispatchAttempts,omitempty"`

	// SessionID is the active session identifier.
	// +optional
	SessionID string `json:"sessionID,omitempty"`

	// StartTime is when the instance started.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`

	// TokenUsage reports token consumption.
	// +optional
	TokenUsage *TokenUsage `json:"tokenUsage,omitempty"`

	// ToolCalls is the number of tool calls made.
	// +optional
	ToolCalls int32 `json:"toolCalls,omitempty"`

	// ToolCallsDetail records the bounded inputs and result of each tool call.
	// +optional
	ToolCallsDetail []ToolCallDetail `json:"toolCallsDetail,omitempty"`

	// Output is the final output of the agent instance.
	// +optional
	Output string `json:"output,omitempty"`

	// ProbeStatus tracks the observed probe state.
	// +optional
	ProbeStatus *ProbeStatus `json:"probeStatus,omitempty"`

	// RestartCount is how many times this instance has been restarted.
	// +optional
	RestartCount int32 `json:"restartCount,omitempty"`
}

// ToolCallDetail records the bounded inputs and result of one runtime tool call.
type ToolCallDetail struct {
	// Name is the Tool name selected by the model.
	Name string `json:"name"`

	// Args is the JSON-encoded tool input for the call.
	// +optional
	Args string `json:"args,omitempty"`

	// Result is the JSON-encoded tool output for the call.
	// +optional
	Result string `json:"result,omitempty"`

	// DurationMS is the execution duration in milliseconds.
	// +optional
	DurationMS int64 `json:"durationMs,omitempty"`

	// Error describes an execution failure, when present.
	// +optional
	Error string `json:"error,omitempty"`
}

// ProbeStatus tracks the observed probe state for an agent instance.
type ProbeStatus struct {
	// Healthy indicates whether the health probe is passing.
	// +optional
	Healthy *bool `json:"healthy,omitempty"`

	// Ready indicates whether the readiness probe is passing.
	// +optional
	Ready *bool `json:"ready,omitempty"`

	// ReasoningHealthy indicates whether the reasoning probe is passing.
	// +optional
	ReasoningHealthy *bool `json:"reasoningHealthy,omitempty"`

	// ConsecutiveHealthFailures counts consecutive health probe failures.
	// +optional
	ConsecutiveHealthFailures int32 `json:"consecutiveHealthFailures,omitempty"`

	// ConsecutiveReadinessFailures counts consecutive readiness probe failures.
	// +optional
	ConsecutiveReadinessFailures int32 `json:"consecutiveReadinessFailures,omitempty"`

	// ConsecutiveReasoningFailures counts consecutive reasoning probe failures.
	// +optional
	ConsecutiveReasoningFailures int32 `json:"consecutiveReasoningFailures,omitempty"`

	// ConsecutiveHealthSuccesses counts consecutive health probe successes.
	// +optional
	ConsecutiveHealthSuccesses int32 `json:"consecutiveHealthSuccesses,omitempty"`

	// ConsecutiveReadinessSuccesses counts consecutive readiness probe successes.
	// +optional
	ConsecutiveReadinessSuccesses int32 `json:"consecutiveReadinessSuccesses,omitempty"`

	// ConsecutiveReasoningSuccesses counts consecutive reasoning probe successes.
	// +optional
	ConsecutiveReasoningSuccesses int32 `json:"consecutiveReasoningSuccesses,omitempty"`

	// LadderStep is the current position in the restart ladder (0-indexed).
	// +optional
	LadderStep int32 `json:"ladderStep,omitempty"`

	// LadderAttempts is the number of attempts at the current ladder step.
	// +optional
	LadderAttempts int32 `json:"ladderAttempts,omitempty"`

	// LastProbeTime records when probes were last executed.
	// +optional
	LastProbeTime *string `json:"lastProbeTime,omitempty"`

	// LastReasoningScheduleSlot is the canonical boundary owned by the current
	// or most recently completed reasoning canary.
	// +optional
	LastReasoningScheduleSlot *string `json:"lastReasoningScheduleSlot,omitempty"`

	// ReasoningOperationID is the deterministic operation identity for the
	// last reasoning schedule slot.
	// +optional
	ReasoningOperationID string `json:"reasoningOperationID,omitempty"`

	// ReasoningRequestDigest binds every execution-affecting request field.
	// +optional
	ReasoningRequestDigest string `json:"reasoningRequestDigest,omitempty"`

	// ReasoningBindings records the versioned, non-secret resources used by the
	// current or most recently completed reasoning canary.
	// +optional
	ReasoningBindings []CanaryResourceBinding `json:"reasoningBindings,omitempty"`

	// LastReasoningResult is the metadata-only terminal canary result.
	// +optional
	LastReasoningResult *ReasoningCanaryResult `json:"lastReasoningResult,omitempty"`

	// LastLadderAction records the last restart-ladder action executed.
	// +optional
	LastLadderAction *RestartAction `json:"lastLadderAction,omitempty"`

	// RolledBackToRevision records which agent revision was rolled back to.
	// +optional
	RolledBackToRevision *int64 `json:"rolledBackToRevision,omitempty"`
}

// CanaryResourceBinding identifies one immutable execution dependency without
// persisting its content.
type CanaryResourceBinding struct {
	Kind            string `json:"kind"`
	Namespace       string `json:"namespace"`
	Name            string `json:"name"`
	UID             string `json:"uid"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
	Revision        *int64 `json:"revision,omitempty"`
}

// CanaryOutcome is the closed set of terminal reasoning canary outcomes.
// +kubebuilder:validation:Enum=Passed;OutcomeMismatch;ExecutionFailed
type CanaryOutcome string

const (
	CanaryOutcomePassed          CanaryOutcome = "Passed"
	CanaryOutcomeMismatch        CanaryOutcome = "OutcomeMismatch"
	CanaryOutcomeExecutionFailed CanaryOutcome = "ExecutionFailed"
)

// UnmarshalJSON rejects outcomes outside ADR-0012's closed set.
func (o *CanaryOutcome) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	outcome := CanaryOutcome(value)
	switch outcome {
	case CanaryOutcomePassed, CanaryOutcomeMismatch, CanaryOutcomeExecutionFailed:
		*o = outcome
		return nil
	default:
		return fmt.Errorf("unknown reasoning canary outcome %q", value)
	}
}

// CanaryFailureReason is the closed set of reasoning canary failure reasons.
// +kubebuilder:validation:Enum=InvalidConfiguration;BindingChanged;AuthorizationDenied;BudgetDenied;TimedOut;ProviderFailed;ToolCallRequested;InvalidEncoding;OutputLimitExceeded;AccountingUncertain;PersistenceFailed
type CanaryFailureReason string

const (
	CanaryFailureInvalidConfiguration CanaryFailureReason = "InvalidConfiguration"
	CanaryFailureBindingChanged       CanaryFailureReason = "BindingChanged"
	CanaryFailureAuthorizationDenied  CanaryFailureReason = "AuthorizationDenied"
	CanaryFailureBudgetDenied         CanaryFailureReason = "BudgetDenied"
	CanaryFailureTimedOut             CanaryFailureReason = "TimedOut"
	CanaryFailureProviderFailed       CanaryFailureReason = "ProviderFailed"
	CanaryFailureToolCallRequested    CanaryFailureReason = "ToolCallRequested"
	CanaryFailureInvalidEncoding      CanaryFailureReason = "InvalidEncoding"
	CanaryFailureOutputLimitExceeded  CanaryFailureReason = "OutputLimitExceeded"
	CanaryFailureAccountingUncertain  CanaryFailureReason = "AccountingUncertain"
	CanaryFailurePersistenceFailed    CanaryFailureReason = "PersistenceFailed"
)

// UnmarshalJSON rejects failure reasons outside ADR-0012's closed set.
func (r *CanaryFailureReason) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	reason := CanaryFailureReason(value)
	switch reason {
	case "", CanaryFailureInvalidConfiguration, CanaryFailureBindingChanged,
		CanaryFailureAuthorizationDenied, CanaryFailureBudgetDenied,
		CanaryFailureTimedOut, CanaryFailureProviderFailed,
		CanaryFailureToolCallRequested, CanaryFailureInvalidEncoding,
		CanaryFailureOutputLimitExceeded, CanaryFailureAccountingUncertain,
		CanaryFailurePersistenceFailed:
		*r = reason
		return nil
	default:
		return fmt.Errorf("unknown reasoning canary failure reason %q", value)
	}
}

// ReasoningCanaryResult is the bounded, metadata-only terminal result retained
// for replay. It never contains prompts, instructions, credentials, or output.
// +kubebuilder:validation:XValidation:rule="(self.outcome == 'ExecutionFailed') == has(self.closedReason)",message="closedReason is required only when outcome is ExecutionFailed"
type ReasoningCanaryResult struct {
	OperationID            string              `json:"operationID"`
	ExpectedBytes          int32               `json:"expectedBytes,omitempty"`
	ActualBytes            int32               `json:"actualBytes,omitempty"`
	ExpectedSHA256         string              `json:"expectedSHA256,omitempty"`
	ActualSHA256           string              `json:"actualSHA256,omitempty"`
	PromptTokens           int64               `json:"promptTokens,omitempty"`
	CompletionTokens       int64               `json:"completionTokens,omitempty"`
	TotalTokens            int64               `json:"totalTokens,omitempty"`
	ProviderDurationMillis int64               `json:"providerDurationMillis,omitempty"`
	TraceID                string              `json:"traceID,omitempty"`
	SpanID                 string              `json:"spanID,omitempty"`
	Outcome                CanaryOutcome       `json:"outcome"`
	ClosedReason           CanaryFailureReason `json:"closedReason,omitempty"`
}

// Validate enforces the relationship between terminal outcomes and failure reasons.
func (r ReasoningCanaryResult) Validate() error {
	switch r.Outcome {
	case CanaryOutcomePassed, CanaryOutcomeMismatch:
		if r.ClosedReason != "" {
			return fmt.Errorf("reasoning canary outcome %q cannot have failure reason %q", r.Outcome, r.ClosedReason)
		}
	case CanaryOutcomeExecutionFailed:
		if r.ClosedReason == "" {
			return fmt.Errorf("reasoning canary outcome ExecutionFailed requires a failure reason")
		}
		switch r.ClosedReason {
		case CanaryFailureInvalidConfiguration, CanaryFailureBindingChanged,
			CanaryFailureAuthorizationDenied, CanaryFailureBudgetDenied,
			CanaryFailureTimedOut, CanaryFailureProviderFailed,
			CanaryFailureToolCallRequested, CanaryFailureInvalidEncoding,
			CanaryFailureOutputLimitExceeded, CanaryFailureAccountingUncertain,
			CanaryFailurePersistenceFailed:
		default:
			return fmt.Errorf("unknown reasoning canary failure reason %q", r.ClosedReason)
		}
	default:
		return fmt.Errorf("unknown reasoning canary outcome %q", r.Outcome)
	}
	return nil
}

// MarshalJSON rejects terminal results with invalid outcome/reason combinations.
func (r ReasoningCanaryResult) MarshalJSON() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	type resultJSON ReasoningCanaryResult
	return json.Marshal(resultJSON(r))
}

// UnmarshalJSON rejects terminal results with invalid outcome/reason combinations.
func (r *ReasoningCanaryResult) UnmarshalJSON(data []byte) error {
	type resultJSON ReasoningCanaryResult
	var decoded resultJSON
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	result := ReasoningCanaryResult(decoded)
	if err := result.Validate(); err != nil {
		return err
	}
	*r = result
	return nil
}

// TokenUsage reports token consumption for an agent instance.
type TokenUsage struct {
	PromptTokens     int64 `json:"promptTokens,omitempty"`
	CompletionTokens int64 `json:"completionTokens,omitempty"`
	TotalTokens      int64 `json:"totalTokens,omitempty"`
}

// +kubebuilder:object:root=true

// AgentInstanceList contains a list of AgentInstances.
type AgentInstanceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentInstance `json:"items"`
}

// LocalObjectReference references a resource by name within the same namespace.
type LocalObjectReference struct {
	// Name is the name of the referenced resource.
	Name string `json:"name"`
}

func init() {
	SchemeBuilder.Register(&Agent{}, &AgentList{})
	SchemeBuilder.Register(&AgentInstance{}, &AgentInstanceList{})
}
