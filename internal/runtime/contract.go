// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtime

import (
	"context"
	"time"
)

// Runtime defines the cognitive execution contract.
// Implementations wrap agent frameworks (native MCP, AutoGen, etc.) and
// expose a unified interface to the control plane.
type Runtime interface {
	// CreateSession initializes a new agent execution session.
	CreateSession(ctx context.Context, req CreateSessionRequest) (CreateSessionResponse, error)

	// Step sends input to a session and returns a channel of streaming events.
	// The caller must grant credits via the returned CreditChan for the runtime
	// to continue emitting events (backpressure).
	Step(ctx context.Context, req StepRequest) (*StepStream, error)

	// Cancel requests cooperative termination of a running session.
	// The runtime acknowledges within timeoutMS; if not, the caller
	// performs a hard-kill.
	Cancel(ctx context.Context, req CancelRequest) (CancelResponse, error)

	// Checkpoint saves session state as an opaque token.
	Checkpoint(ctx context.Context, sessionID string) (CheckpointResponse, error)

	// Resume restores a previously checkpointed session.
	Resume(ctx context.Context, token []byte) (ResumeResponse, error)

	// GetCapabilities returns the runtime's supported features.
	GetCapabilities(ctx context.Context) (Capabilities, error)

	// Health returns the runtime's current health status.
	Health(ctx context.Context) (HealthStatus, error)
}

// --- Request / Response types ---

// CreateSessionRequest contains parameters for session creation.
type CreateSessionRequest struct {
	AgentName      string
	Namespace      string
	Context        map[string]string
	InitialCredits uint32
}

// CreateSessionResponse is returned after session creation.
type CreateSessionResponse struct {
	SessionID string
}

// StepRequest sends user or system input into an active session.
type StepRequest struct {
	SessionID string
	Input     string
}

// CancelRequest asks for cooperative cancellation of a session.
type CancelRequest struct {
	SessionID string
	TimeoutMS uint32
}

// CancelAckState indicates how cancellation was resolved.
type CancelAckState int

const (
	CancelAckUnspecified CancelAckState = iota
	CancelAcknowledged
	CancelForced
)

// CancelResponse carries the result of a cancellation request.
type CancelResponse struct {
	State       CancelAckState
	CancelledAt time.Time
}

// CheckpointResponse carries the opaque checkpoint token.
type CheckpointResponse struct {
	Token     []byte
	CreatedAt time.Time
}

// ResumeResponse is returned after resuming from a checkpoint.
type ResumeResponse struct {
	SessionID string
}

// Capabilities describes what the runtime supports.
type Capabilities struct {
	SupportedFeatures     []string
	RuntimeVersion        string
	MaxConcurrentSessions uint32
}

// HealthState represents the runtime health level.
type HealthState int

const (
	HealthUnknown HealthState = iota
	HealthHealthy
	HealthDegraded
	HealthUnhealthy
)

// HealthStatus is the runtime's current health.
type HealthStatus struct {
	State          HealthState
	Message        string
	ActiveSessions uint32
}

// --- Streaming event types ---

// EventType classifies a streaming event.
type EventType int

const (
	EventToken EventType = iota + 1
	EventToolCall
	EventToolResult
	EventMemoryOp
	EventStatus
	EventOutput
	EventError
	EventCreditRequest
)

// StepEvent is a single event emitted during a Step stream.
type StepEvent struct {
	Type     EventType
	Sequence uint64

	// Only one of these is set, determined by Type.
	Token         *TokenEvent
	ToolCall      *ToolCallStreamEvent
	ToolResult    *ToolResultEvent
	MemoryOp      *MemoryOpEvent
	Status        *StatusEvent
	Output        *OutputStreamEvent
	Error         *ErrorStreamEvent
	CreditRequest *CreditRequestEvent
}

// TokenEvent delivers incremental token output.
type TokenEvent struct {
	Content string
	Role    string // "assistant", "thinking", etc.
}

// ToolCallStreamEvent indicates the runtime is invoking a tool.
type ToolCallStreamEvent struct {
	ToolName  string
	Arguments string // JSON-encoded
	CallID    string
}

// ToolResultEvent carries the result of a tool invocation.
type ToolResultEvent struct {
	CallID  string
	Output  string // JSON-encoded
	IsError bool
}

// MemoryOperation describes the kind of memory operation.
type MemoryOperation int

const (
	MemoryOpUnspecified MemoryOperation = iota
	MemoryOpRead
	MemoryOpWrite
	MemoryOpSummarize
	MemoryOpForget
)

// MemoryOpEvent indicates a memory operation performed by the runtime.
type MemoryOpEvent struct {
	Operation MemoryOperation
	Key       string
	Value     string // JSON-encoded, empty for reads/forgets
	Timestamp time.Time
}

// SessionState represents the runtime's state during execution.
type SessionState int

const (
	StateUnspecified SessionState = iota
	StateThinking
	StateExecutingTool
	StateWaitingForInput
	StatePaused
	StateCompleted
	StateCancelled
	StateFailed
)

// StatusEvent communicates runtime state transitions.
type StatusEvent struct {
	State   SessionState
	Message string
}

// OutputStreamEvent delivers final or partial output.
type OutputStreamEvent struct {
	Content string
	IsFinal bool
}

// ErrorStreamEvent reports an error during execution.
type ErrorStreamEvent struct {
	Code      string
	Message   string
	Retryable bool
}

// CreditRequestEvent is sent when the runtime has exhausted credits.
type CreditRequestEvent struct {
	SessionID string
	Needed    uint32
}

// --- Step stream with backpressure ---

// StepStream provides the event channel and credit grant mechanism.
type StepStream struct {
	// Events delivers streaming events from the runtime.
	// Closed when the step completes or errors.
	Events <-chan StepEvent

	// GrantCredits sends additional credits to the runtime, allowing it
	// to continue emitting events. The runtime pauses when credits reach zero.
	GrantCredits func(credits uint32)

	// cancel is the internal cancellation for the stream.
	cancel context.CancelFunc
}

func NewStepStream(events <-chan StepEvent, grantCredits func(uint32), cancel context.CancelFunc) *StepStream {
	return &StepStream{Events: events, GrantCredits: grantCredits, cancel: cancel}
}

// Close terminates the step stream.
func (s *StepStream) Close() {
	if s.cancel != nil {
		s.cancel()
	}
}
