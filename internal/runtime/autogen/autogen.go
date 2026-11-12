// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package autogen provides a Runtime adapter that translates the platform's
// Runtime contract into AutoGen API calls, enabling AutoGen-based agent
// frameworks (e.g., kagent) to be orchestrated uniformly.
package autogen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kognition-ai/kognition/internal/runtime"
)

// AgentClient abstracts the AutoGen API surface that the adapter depends on.
// Implementations call the actual AutoGen runtime (e.g., via HTTP or gRPC).
type AgentClient interface {
	// CreateAgent initializes an agent session in AutoGen and returns a handle.
	CreateAgent(ctx context.Context, req CreateAgentRequest) (CreateAgentResponse, error)

	// SendMessage sends a user message to an active AutoGen agent and returns
	// the stream of events produced during execution.
	SendMessage(ctx context.Context, agentID string, message string) (<-chan AgentEvent, error)

	// CancelAgent requests cooperative cancellation of an agent.
	CancelAgent(ctx context.Context, agentID string) error

	// GetAgentState retrieves the serialized state of an agent for checkpointing.
	GetAgentState(ctx context.Context, agentID string) ([]byte, error)

	// RestoreAgent restores an agent from previously serialized state.
	RestoreAgent(ctx context.Context, state []byte) (string, error)

	// Health checks the AutoGen runtime health.
	Health(ctx context.Context) error
}

// CreateAgentRequest maps to AutoGen agent creation parameters.
type CreateAgentRequest struct {
	AgentName string
	Namespace string
	Config    map[string]string
}

// CreateAgentResponse carries the AutoGen agent handle.
type CreateAgentResponse struct {
	AgentID string
}

// AgentEventType classifies AutoGen events.
type AgentEventType int

const (
	AgentEventToken AgentEventType = iota + 1
	AgentEventToolCall
	AgentEventToolResult
	AgentEventOutput
	AgentEventError
	AgentEventDone
)

// AgentEvent is a single event from the AutoGen runtime.
type AgentEvent struct {
	Type       AgentEventType
	Content    string
	ToolName   string
	ToolCallID string
	IsError    bool
	IsFinal    bool
}

// --- Adapter ---

// AutoGenRuntime implements runtime.Runtime by delegating to an AutoGen AgentClient.
type AutoGenRuntime struct {
	client AgentClient

	mu       sync.RWMutex
	sessions map[string]*agentSession
}

type agentSession struct {
	agentID   string
	createdAt time.Time
	cancelled bool
	busy      bool
	credits   uint32
}

// New creates an AutoGenRuntime backed by the given AutoGen client.
func New(client AgentClient) *AutoGenRuntime {
	return &AutoGenRuntime{
		client:   client,
		sessions: make(map[string]*agentSession),
	}
}

func (r *AutoGenRuntime) CreateSession(ctx context.Context, req runtime.CreateSessionRequest) (runtime.CreateSessionResponse, error) {
	resp, err := r.client.CreateAgent(ctx, CreateAgentRequest{
		AgentName: req.AgentName,
		Namespace: req.Namespace,
		Config:    req.Context,
	})
	if err != nil {
		return runtime.CreateSessionResponse{}, fmt.Errorf("autogen create agent: %w", err)
	}

	r.mu.Lock()
	if resp.AgentID == "" || r.sessions[resp.AgentID] != nil {
		r.mu.Unlock()
		return runtime.CreateSessionResponse{}, errors.New("autogen returned an invalid or reused session handle")
	}
	r.sessions[resp.AgentID] = &agentSession{
		agentID:   resp.AgentID,
		createdAt: time.Now(),
		credits:   req.InitialCredits,
	}
	r.mu.Unlock()

	return runtime.CreateSessionResponse{SessionID: resp.AgentID}, nil
}

func (r *AutoGenRuntime) Step(ctx context.Context, req runtime.StepRequest) (*runtime.StepStream, error) {
	r.mu.Lock()
	sess, ok := r.sessions[req.SessionID]
	cancelled := ok && sess.cancelled
	if !ok {
		r.mu.Unlock()
		return nil, fmt.Errorf("session %s not found", req.SessionID)
	}
	if cancelled {
		r.mu.Unlock()
		return nil, errors.New("session is cancelled")
	}
	if sess.busy {
		r.mu.Unlock()
		return nil, errors.New("session has an active turn")
	}
	sess.busy = true
	initialCredits := sess.credits
	r.mu.Unlock()
	finish := func() {
		r.mu.Lock()
		sess.busy = false
		r.mu.Unlock()
	}

	streamCtx, cancel := context.WithCancel(ctx)
	agentEvents, err := r.client.SendMessage(streamCtx, sess.agentID, req.Input)
	if err != nil {
		cancel()
		finish()
		return nil, fmt.Errorf("autogen send message: %w", err)
	}

	eventCh := make(chan runtime.StepEvent, 64)

	var credits atomic.Int64
	if initialCredits == 0 {
		initialCredits = 64
	}
	credits.Store(int64(initialCredits))

	grantCredits := func(c uint32) {
		credits.Add(int64(c))
	}

	go func() {
		defer close(eventCh)
		defer cancel()
		defer finish()
		var seq uint64
		for {
			select {
			case <-streamCtx.Done():
				return
			case ev, ok := <-agentEvents:
				if !ok {
					return
				}
				// Wait for credits
				for credits.Load() <= 0 {
					select {
					case <-streamCtx.Done():
						return
					case <-time.After(10 * time.Millisecond):
					}
				}
				credits.Add(-1)
				seq++
				translated := translateEvent(ev, seq)
				select {
				case eventCh <- translated:
				case <-streamCtx.Done():
					return
				}
			}
		}
	}()

	return runtime.NewStepStream(eventCh, grantCredits, cancel), nil
}

func (r *AutoGenRuntime) Cancel(ctx context.Context, req runtime.CancelRequest) (runtime.CancelResponse, error) {
	r.mu.Lock()
	sess, ok := r.sessions[req.SessionID]
	if ok {
		sess.cancelled = true
	}
	r.mu.Unlock()
	if !ok {
		return runtime.CancelResponse{}, fmt.Errorf("session %s not found", req.SessionID)
	}

	if req.TimeoutMS > 0 {
		boundedContext, cancel := context.WithTimeout(ctx, time.Duration(req.TimeoutMS)*time.Millisecond)
		defer cancel()
		ctx = boundedContext
	}
	err := r.client.CancelAgent(ctx, sess.agentID)
	if err != nil {
		return runtime.CancelResponse{
			State:       runtime.CancelForced,
			CancelledAt: time.Now(),
		}, nil
	}
	return runtime.CancelResponse{
		State:       runtime.CancelAcknowledged,
		CancelledAt: time.Now(),
	}, nil
}

func (r *AutoGenRuntime) Checkpoint(ctx context.Context, sessionID string) (runtime.CheckpointResponse, error) {
	r.mu.Lock()
	sess, ok := r.sessions[sessionID]
	if !ok {
		r.mu.Unlock()
		return runtime.CheckpointResponse{}, fmt.Errorf("session %s not found", sessionID)
	}
	if sess.busy {
		r.mu.Unlock()
		return runtime.CheckpointResponse{}, errors.New("session has an active turn")
	}
	if sess.cancelled {
		r.mu.Unlock()
		return runtime.CheckpointResponse{}, errors.New("session is cancelled")
	}
	sess.busy = true
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		sess.busy = false
		r.mu.Unlock()
	}()

	state, err := r.client.GetAgentState(ctx, sess.agentID)
	if err != nil {
		return runtime.CheckpointResponse{}, fmt.Errorf("autogen get state: %w", err)
	}

	token, err := json.Marshal(checkpointToken{
		AgentID: sess.agentID,
		State:   state,
	})
	if err != nil {
		return runtime.CheckpointResponse{}, err
	}
	r.mu.RLock()
	cancelled := sess.cancelled
	r.mu.RUnlock()
	if cancelled {
		return runtime.CheckpointResponse{}, errors.New("session is cancelled")
	}

	return runtime.CheckpointResponse{
		Token:     token,
		CreatedAt: time.Now(),
	}, nil
}

func (r *AutoGenRuntime) Resume(ctx context.Context, token []byte) (runtime.ResumeResponse, error) {
	var ckpt checkpointToken
	if err := json.Unmarshal(token, &ckpt); err != nil {
		return runtime.ResumeResponse{}, fmt.Errorf("invalid checkpoint token: %w", err)
	}

	agentID, err := r.client.RestoreAgent(ctx, ckpt.State)
	if err != nil {
		return runtime.ResumeResponse{}, fmt.Errorf("autogen restore: %w", err)
	}

	r.mu.Lock()
	if agentID == "" || r.sessions[agentID] != nil {
		r.mu.Unlock()
		return runtime.ResumeResponse{}, errors.New("autogen returned an invalid or reused session handle")
	}
	r.sessions[agentID] = &agentSession{
		agentID:   agentID,
		createdAt: time.Now(),
	}
	r.mu.Unlock()

	return runtime.ResumeResponse{SessionID: agentID}, nil
}

func (r *AutoGenRuntime) GetCapabilities(ctx context.Context) (runtime.Capabilities, error) {
	return runtime.Capabilities{
		SupportedFeatures:     []string{"streaming", "checkpoint", "tool-calling"},
		RuntimeVersion:        "autogen-adapter/0.1.0",
		MaxConcurrentSessions: 64,
	}, nil
}

func (r *AutoGenRuntime) Health(ctx context.Context) (runtime.HealthStatus, error) {
	err := r.client.Health(ctx)
	if err != nil {
		return runtime.HealthStatus{
			State:   runtime.HealthDegraded,
			Message: err.Error(),
		}, nil
	}

	r.mu.RLock()
	active := uint32(len(r.sessions))
	r.mu.RUnlock()

	return runtime.HealthStatus{
		State:          runtime.HealthHealthy,
		ActiveSessions: active,
	}, nil
}

// --- Internal ---

type checkpointToken struct {
	AgentID string `json:"agent_id"`
	State   []byte `json:"state"`
}

func translateEvent(ev AgentEvent, seq uint64) runtime.StepEvent {
	switch ev.Type {
	case AgentEventToken:
		return runtime.StepEvent{
			Type:     runtime.EventToken,
			Sequence: seq,
			Token:    &runtime.TokenEvent{Content: ev.Content, Role: "assistant"},
		}
	case AgentEventToolCall:
		return runtime.StepEvent{
			Type:     runtime.EventToolCall,
			Sequence: seq,
			ToolCall: &runtime.ToolCallStreamEvent{
				ToolName:  ev.ToolName,
				Arguments: ev.Content,
				CallID:    ev.ToolCallID,
			},
		}
	case AgentEventToolResult:
		return runtime.StepEvent{
			Type:     runtime.EventToolResult,
			Sequence: seq,
			ToolResult: &runtime.ToolResultEvent{
				CallID:  ev.ToolCallID,
				Output:  ev.Content,
				IsError: ev.IsError,
			},
		}
	case AgentEventOutput:
		return runtime.StepEvent{
			Type:     runtime.EventOutput,
			Sequence: seq,
			Output:   &runtime.OutputStreamEvent{Content: ev.Content, IsFinal: ev.IsFinal},
		}
	case AgentEventError:
		return runtime.StepEvent{
			Type:     runtime.EventError,
			Sequence: seq,
			Error:    &runtime.ErrorStreamEvent{Code: "AutoGenError", Message: ev.Content, Retryable: false},
		}
	case AgentEventDone:
		return runtime.StepEvent{
			Type:     runtime.EventStatus,
			Sequence: seq,
			Status:   &runtime.StatusEvent{State: runtime.StateCompleted, Message: "done"},
		}
	default:
		return runtime.StepEvent{
			Type:     runtime.EventStatus,
			Sequence: seq,
			Status:   &runtime.StatusEvent{State: runtime.StateUnspecified, Message: "unknown event"},
		}
	}
}

// Compile-time interface check.
var _ runtime.Runtime = (*AutoGenRuntime)(nil)
