// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	"encoding/json"
	"fmt"
)

// AgentProbeSpec aggregates all probe configurations for an Agent.
type AgentProbeSpec struct {
	// HealthProbe configures runtime liveness checks.
	HealthProbe *AgentHealthProbe `json:"healthProbe,omitempty" yaml:"healthProbe,omitempty"`

	// ReadinessProbe configures model/tool/memory reachability checks.
	ReadinessProbe *AgentReadinessProbe `json:"readinessProbe,omitempty" yaml:"readinessProbe,omitempty"`

	// ReasoningProbe configures canary-task validation checks.
	ReasoningProbe *AgentReasoningProbe `json:"reasoningProbe,omitempty" yaml:"reasoningProbe,omitempty"`
}

// AgentHealthProbe defines runtime liveness probe configuration.
type AgentHealthProbe struct {
	// IntervalSeconds is how often (in seconds) to perform the probe.
	// Defaults to 30.
	IntervalSeconds int32 `json:"intervalSeconds,omitempty" yaml:"intervalSeconds,omitempty"`

	// TimeoutSeconds is the probe timeout in seconds. Defaults to 5.
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty" yaml:"timeoutSeconds,omitempty"`

	// FailureThreshold is the consecutive failures before marking unhealthy.
	// Defaults to 3.
	FailureThreshold int32 `json:"failureThreshold,omitempty" yaml:"failureThreshold,omitempty"`

	// SuccessThreshold is the consecutive successes to recover. Defaults to 1.
	SuccessThreshold int32 `json:"successThreshold,omitempty" yaml:"successThreshold,omitempty"`
}

// AgentReadinessProbe checks model, tool, and memory reachability.
type AgentReadinessProbe struct {
	// IntervalSeconds is how often (in seconds) to perform the probe.
	// Defaults to 30.
	IntervalSeconds int32 `json:"intervalSeconds,omitempty" yaml:"intervalSeconds,omitempty"`

	// TimeoutSeconds is the probe timeout in seconds. Defaults to 10.
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty" yaml:"timeoutSeconds,omitempty"`

	// FailureThreshold is the consecutive failures before marking unready.
	// Defaults to 3.
	FailureThreshold int32 `json:"failureThreshold,omitempty" yaml:"failureThreshold,omitempty"`

	// SuccessThreshold is the consecutive successes to recover. Defaults to 1.
	SuccessThreshold int32 `json:"successThreshold,omitempty" yaml:"successThreshold,omitempty"`
}

// AgentReasoningProbe configures periodic canary-task validation.
type AgentReasoningProbe struct {
	// CanaryTask is the input prompt for the canary evaluation.
	CanaryTask string `json:"canaryTask" yaml:"canaryTask"`

	// ExpectedOutcome is the exact UTF-8 output the canary must return.
	ExpectedOutcome string `json:"expectedOutcome" yaml:"expectedOutcome"`

	// IntervalSeconds is how often (in seconds) to perform the probe.
	// Defaults to 300 (5 minutes).
	IntervalSeconds int32 `json:"intervalSeconds,omitempty" yaml:"intervalSeconds,omitempty"`

	// TimeoutSeconds is the probe timeout in seconds. Defaults to 60.
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty" yaml:"timeoutSeconds,omitempty"`

	// MaxTokens is the token budget for the canary run. Defaults to 256.
	MaxTokens int32 `json:"maxTokens,omitempty" yaml:"maxTokens,omitempty"`

	// MaxInputTokens is the maximum exact input-token count accepted for the
	// canary request. Defaults to 4096.
	MaxInputTokens int32 `json:"maxInputTokens,omitempty" yaml:"maxInputTokens,omitempty"`

	// FailureThreshold is the consecutive failures before marking degraded.
	// Defaults to 2.
	FailureThreshold int32 `json:"failureThreshold,omitempty" yaml:"failureThreshold,omitempty"`

	// SuccessThreshold is the consecutive successes to recover. Defaults to 1.
	SuccessThreshold int32 `json:"successThreshold,omitempty" yaml:"successThreshold,omitempty"`
}

// RestartLadder defines an ordered set of escalation responses to probe failures.
type RestartLadder struct {
	// Steps lists the escalation steps in order.
	Steps []RestartLadderStep `json:"steps" yaml:"steps"`
}

// RestartLadderStep defines a single escalation response.
type RestartLadderStep struct {
	// Action is the response action to execute.
	Action RestartAction `json:"action" yaml:"action"`

	// StabilisationSeconds is how long to wait after this action before
	// deciding whether to escalate. Defaults to 60.
	StabilisationSeconds int32 `json:"stabilisationSeconds,omitempty" yaml:"stabilisationSeconds,omitempty"`

	// MaxAttempts is the maximum number of times this step can be tried
	// before escalating to the next step. Defaults to 1.
	MaxAttempts int32 `json:"maxAttempts,omitempty" yaml:"maxAttempts,omitempty"`
}

// RestartAction enumerates supported restart-ladder response actions.
type RestartAction string

const (
	// RestartActionRestart restarts the agent instance.
	RestartActionRestart RestartAction = "Restart"

	// RestartActionRollbackRevision rolls back to the previous agent revision.
	RestartActionRollbackRevision RestartAction = "RollbackRevision"

	// RestartActionDegradeModel switches to a fallback model.
	RestartActionDegradeModel RestartAction = "DegradeModel"

	// RestartActionQuarantine isolates the instance from serving traffic.
	RestartActionQuarantine RestartAction = "Quarantine"

	// RestartActionAlert emits an alert event without taking automated action.
	RestartActionAlert RestartAction = "Alert"
)

// ProbeStatus tracks the observed probe state for an agent instance.
type ProbeStatus struct {
	// Healthy indicates whether the health probe is passing.
	Healthy *bool `json:"healthy,omitempty" yaml:"healthy,omitempty"`

	// Ready indicates whether the readiness probe is passing.
	Ready *bool `json:"ready,omitempty" yaml:"ready,omitempty"`

	// ReasoningHealthy indicates whether the reasoning probe is passing.
	ReasoningHealthy *bool `json:"reasoningHealthy,omitempty" yaml:"reasoningHealthy,omitempty"`

	// ConsecutiveHealthFailures counts consecutive health probe failures.
	ConsecutiveHealthFailures int32 `json:"consecutiveHealthFailures,omitempty" yaml:"consecutiveHealthFailures,omitempty"`

	// ConsecutiveReadinessFailures counts consecutive readiness probe failures.
	ConsecutiveReadinessFailures int32 `json:"consecutiveReadinessFailures,omitempty" yaml:"consecutiveReadinessFailures,omitempty"`

	// ConsecutiveReasoningFailures counts consecutive reasoning probe failures.
	ConsecutiveReasoningFailures int32 `json:"consecutiveReasoningFailures,omitempty" yaml:"consecutiveReasoningFailures,omitempty"`

	// ConsecutiveHealthSuccesses counts consecutive health probe successes (for recovery).
	ConsecutiveHealthSuccesses int32 `json:"consecutiveHealthSuccesses,omitempty" yaml:"consecutiveHealthSuccesses,omitempty"`

	// ConsecutiveReadinessSuccesses counts consecutive readiness probe successes (for recovery).
	ConsecutiveReadinessSuccesses int32 `json:"consecutiveReadinessSuccesses,omitempty" yaml:"consecutiveReadinessSuccesses,omitempty"`

	// ConsecutiveReasoningSuccesses counts consecutive reasoning probe successes (for recovery).
	ConsecutiveReasoningSuccesses int32 `json:"consecutiveReasoningSuccesses,omitempty" yaml:"consecutiveReasoningSuccesses,omitempty"`

	// LadderStep is the current position in the restart ladder (0-indexed).
	// -1 or absent means no escalation is in progress.
	LadderStep int32 `json:"ladderStep,omitempty" yaml:"ladderStep,omitempty"`

	// LadderAttempts is the number of attempts at the current ladder step.
	LadderAttempts int32 `json:"ladderAttempts,omitempty" yaml:"ladderAttempts,omitempty"`

	// LastProbeTime records when probes were last executed.
	LastProbeTime *string `json:"lastProbeTime,omitempty" yaml:"lastProbeTime,omitempty"`

	// LastReasoningScheduleSlot is the canonical boundary owned by the current
	// or most recently completed reasoning canary.
	LastReasoningScheduleSlot *string `json:"lastReasoningScheduleSlot,omitempty" yaml:"lastReasoningScheduleSlot,omitempty"`

	// ReasoningOperationID is the deterministic operation identity for
	// LastReasoningScheduleSlot.
	ReasoningOperationID string `json:"reasoningOperationID,omitempty" yaml:"reasoningOperationID,omitempty"`

	// ReasoningRequestDigest binds every execution-affecting request field.
	ReasoningRequestDigest string `json:"reasoningRequestDigest,omitempty" yaml:"reasoningRequestDigest,omitempty"`

	// ReasoningBindings records the versioned, non-secret resources used by the
	// current or most recently completed reasoning canary.
	ReasoningBindings []CanaryResourceBinding `json:"reasoningBindings,omitempty" yaml:"reasoningBindings,omitempty"`

	// LastReasoningResult is the metadata-only terminal canary result.
	LastReasoningResult *ReasoningCanaryResult `json:"lastReasoningResult,omitempty" yaml:"lastReasoningResult,omitempty"`

	// LastLadderAction records the last restart-ladder action executed.
	LastLadderAction *RestartAction `json:"lastLadderAction,omitempty" yaml:"lastLadderAction,omitempty"`

	// RolledBackToRevision records which agent revision was rolled back to.
	RolledBackToRevision *int64 `json:"rolledBackToRevision,omitempty" yaml:"rolledBackToRevision,omitempty"`
}

// CanaryResourceBinding identifies one immutable execution dependency without
// persisting its content.
type CanaryResourceBinding struct {
	Kind            string `json:"kind" yaml:"kind"`
	Namespace       string `json:"namespace" yaml:"namespace"`
	Name            string `json:"name" yaml:"name"`
	UID             string `json:"uid" yaml:"uid"`
	ResourceVersion string `json:"resourceVersion,omitempty" yaml:"resourceVersion,omitempty"`
	Revision        *int64 `json:"revision,omitempty" yaml:"revision,omitempty"`
}

// CanaryOutcome is the closed set of terminal reasoning canary outcomes.
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

// CanaryFailureReason is the closed set of ADR-0012 execution-failure reasons.
// The empty value is valid only when the outcome is not ExecutionFailed.
type CanaryFailureReason uint8

const (
	CanaryFailureInvalidConfiguration CanaryFailureReason = iota + 1
	CanaryFailureBindingChanged
	CanaryFailureAuthorizationDenied
	CanaryFailureBudgetDenied
	CanaryFailureTimedOut
	CanaryFailureProviderFailed
	CanaryFailureToolCallRequested
	CanaryFailureInvalidEncoding
	CanaryFailureOutputLimitExceeded
	CanaryFailureAccountingUncertain
	CanaryFailurePersistenceFailed
)

func (r CanaryFailureReason) String() string {
	switch r {
	case 0:
		return ""
	case CanaryFailureInvalidConfiguration:
		return "InvalidConfiguration"
	case CanaryFailureBindingChanged:
		return "BindingChanged"
	case CanaryFailureAuthorizationDenied:
		return "AuthorizationDenied"
	case CanaryFailureBudgetDenied:
		return "BudgetDenied"
	case CanaryFailureTimedOut:
		return "TimedOut"
	case CanaryFailureProviderFailed:
		return "ProviderFailed"
	case CanaryFailureToolCallRequested:
		return "ToolCallRequested"
	case CanaryFailureInvalidEncoding:
		return "InvalidEncoding"
	case CanaryFailureOutputLimitExceeded:
		return "OutputLimitExceeded"
	case CanaryFailureAccountingUncertain:
		return "AccountingUncertain"
	case CanaryFailurePersistenceFailed:
		return "PersistenceFailed"
	default:
		return ""
	}
}

// MarshalText preserves the canonical string representation for text codecs.
func (r CanaryFailureReason) MarshalText() ([]byte, error) {
	value := r.String()
	if r != 0 && value == "" {
		return nil, fmt.Errorf("unknown reasoning canary failure reason %d", r)
	}
	return []byte(value), nil
}

// UnmarshalText rejects failure reasons outside ADR-0012's closed set.
func (r *CanaryFailureReason) UnmarshalText(data []byte) error {
	var reason CanaryFailureReason
	switch string(data) {
	case "":
		reason = 0
	case "InvalidConfiguration":
		reason = CanaryFailureInvalidConfiguration
	case "BindingChanged":
		reason = CanaryFailureBindingChanged
	case "AuthorizationDenied":
		reason = CanaryFailureAuthorizationDenied
	case "BudgetDenied":
		reason = CanaryFailureBudgetDenied
	case "TimedOut":
		reason = CanaryFailureTimedOut
	case "ProviderFailed":
		reason = CanaryFailureProviderFailed
	case "ToolCallRequested":
		reason = CanaryFailureToolCallRequested
	case "InvalidEncoding":
		reason = CanaryFailureInvalidEncoding
	case "OutputLimitExceeded":
		reason = CanaryFailureOutputLimitExceeded
	case "AccountingUncertain":
		reason = CanaryFailureAccountingUncertain
	case "PersistenceFailed":
		reason = CanaryFailurePersistenceFailed
	default:
		return fmt.Errorf("unknown reasoning canary failure reason %q", data)
	}
	*r = reason
	return nil
}

// MarshalJSON preserves the canonical string representation in persisted data.
func (r CanaryFailureReason) MarshalJSON() ([]byte, error) {
	value, err := r.MarshalText()
	if err != nil {
		return nil, err
	}
	return json.Marshal(string(value))
}

// UnmarshalJSON rejects failure reasons outside ADR-0012's closed set.
func (r *CanaryFailureReason) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	return r.UnmarshalText([]byte(value))
}

// ReasoningCanaryResult is the bounded, metadata-only terminal result retained
// for replay. It never contains prompts, instructions, credentials, or output.
type ReasoningCanaryResult struct {
	OperationID            string              `json:"operationID" yaml:"operationID"`
	ExpectedBytes          int32               `json:"expectedBytes,omitempty" yaml:"expectedBytes,omitempty"`
	ActualBytes            int32               `json:"actualBytes,omitempty" yaml:"actualBytes,omitempty"`
	ExpectedSHA256         string              `json:"expectedSHA256,omitempty" yaml:"expectedSHA256,omitempty"`
	ActualSHA256           string              `json:"actualSHA256,omitempty" yaml:"actualSHA256,omitempty"`
	PromptTokens           int64               `json:"promptTokens,omitempty" yaml:"promptTokens,omitempty"`
	CompletionTokens       int64               `json:"completionTokens,omitempty" yaml:"completionTokens,omitempty"`
	TotalTokens            int64               `json:"totalTokens,omitempty" yaml:"totalTokens,omitempty"`
	ProviderDurationMillis int64               `json:"providerDurationMillis,omitempty" yaml:"providerDurationMillis,omitempty"`
	TraceID                string              `json:"traceID,omitempty" yaml:"traceID,omitempty"`
	SpanID                 string              `json:"spanID,omitempty" yaml:"spanID,omitempty"`
	Outcome                CanaryOutcome       `json:"outcome" yaml:"outcome"`
	ClosedReason           CanaryFailureReason `json:"closedReason,omitempty" yaml:"closedReason,omitempty"`
}

// Validate enforces the relationship between terminal outcomes and failure reasons.
func (r ReasoningCanaryResult) Validate() error {
	switch r.Outcome {
	case CanaryOutcomePassed, CanaryOutcomeMismatch:
		if r.ClosedReason != 0 {
			return fmt.Errorf("reasoning canary outcome %q cannot have failure reason %q", r.Outcome, r.ClosedReason)
		}
	case CanaryOutcomeExecutionFailed:
		if r.ClosedReason == 0 {
			return fmt.Errorf("reasoning canary outcome ExecutionFailed requires a failure reason")
		}
		if _, err := r.ClosedReason.MarshalText(); err != nil {
			return err
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
