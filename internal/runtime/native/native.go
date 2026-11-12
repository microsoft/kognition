// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package native implements the reference Runtime adapter using in-process
// CPI drivers, memory backends, and tool dispatchers. This is the "batteries
// included" runtime that drives the full prompt → model → tool → memory loop.
package native

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/memory"
	"github.com/kognition-ai/kognition/internal/modelinvocation"
	"github.com/kognition-ai/kognition/internal/runtime"
	"github.com/kognition-ai/kognition/internal/store"
)

const (
	defaultMaxIterations = 20
	runtimeVersion       = "native-v0.1.0"
	maxCheckpointBytes   = 4 << 20
)

type nativeCheckpoint struct {
	Version                  int                                     `json:"version"`
	Session                  json.RawMessage                         `json:"session"`
	Messages                 []cpi.Message                           `json:"messages"`
	InvocationProfileBinding *v1alpha1.ModelInvocationProfileBinding `json:"invocationProfileBinding,omitempty"`
}

// Config holds the parameters needed to instantiate a NativeRuntime.
type Config struct {
	// Store resolves immutable invocation profiles for profile-backed Models.
	Store store.Store
	// Driver is the CPI model driver for inference.
	// If set, used directly (backward compatibility). Otherwise resolved via Registry.
	Driver cpi.Driver

	// DriverModel is the model identifier passed to the driver.
	DriverModel string

	// DriverEndpoint is the endpoint URL for the model driver.
	DriverEndpoint string

	// Credentials supplies model credentials.
	Credentials *cpi.Credentials

	// Registry is the CPI driver registry for dynamic Model → driver resolution.
	// When set alongside Model and ModelClass, the runtime resolves the driver
	// at step time using ModelClass.Spec.Provisioner as the registry key.
	Registry *cpi.Registry

	// Model is the resolved Model resource providing model ID, endpoint, and credentials.
	Model *v1alpha1.Model

	// ModelClass is the resolved ModelClass providing the provisioner name.
	ModelClass *v1alpha1.ModelClass

	// MemoryBackend is the memory data-plane (nil to disable memory).
	MemoryBackend memory.Backend

	// MemoryHandle identifies the provisioned volume for memory ops.
	MemoryHandle string

	// ToolDispatcher routes tool executions.
	ToolDispatcher *runtime.ToolDispatcher

	// Agent holds the agent spec (instructions, tools, memory config).
	Agent *v1alpha1.Agent

	// Tools are resolved tool objects available to the agent.
	Tools []v1alpha1.Tool

	// MaxIterations caps the model→tool loop (default 20).
	MaxIterations int

	// MaxConcurrentSessions caps active sessions (0 = unlimited).
	MaxConcurrentSessions uint32
}

// NativeRuntime is the reference implementation of runtime.Runtime.
type NativeRuntime struct {
	cfg      Config
	mgr      *runtime.SessionManager
	mu       sync.Mutex
	history  map[string][]cpi.Message // sessionID → conversation messages
	active   map[string]int
	profiles map[string]*v1alpha1.ModelInvocationProfileBinding
}

// New creates a NativeRuntime from the given config.
func New(cfg Config) *NativeRuntime {
	if cfg.MaxIterations <= 0 {
		cfg.MaxIterations = defaultMaxIterations
	}
	return &NativeRuntime{
		cfg:      cfg,
		mgr:      runtime.NewSessionManager(),
		history:  make(map[string][]cpi.Message),
		active:   make(map[string]int),
		profiles: make(map[string]*v1alpha1.ModelInvocationProfileBinding),
	}
}

// resolveDriver resolves the CPI driver and model parameters for inference.
// If Config.Driver is set explicitly, it is used directly (backward-compatible).
// Otherwise, Config.Registry + Config.ModelClass are used to resolve dynamically.
func (r *NativeRuntime) resolveDriver() (driver cpi.Driver, model string, endpoint string, creds *cpi.Credentials, params map[string]string, err error) {
	// Fast path: explicit driver (testing / legacy callers).
	if r.cfg.Driver != nil {
		return r.cfg.Driver, r.cfg.DriverModel, r.cfg.DriverEndpoint, r.cfg.Credentials, nil, nil
	}

	// Dynamic resolution via Registry.
	if r.cfg.Registry == nil {
		return nil, "", "", nil, nil, fmt.Errorf("no CPI driver configured: set Driver or Registry+Model+ModelClass")
	}
	if r.cfg.ModelClass == nil {
		return nil, "", "", nil, nil, fmt.Errorf("ModelClass is required for registry-based driver resolution")
	}
	if r.cfg.Model == nil {
		return nil, "", "", nil, nil, fmt.Errorf("Model is required for registry-based driver resolution")
	}

	provisioner := r.cfg.ModelClass.Spec.Provisioner
	drv, ok := r.cfg.Registry.Get(provisioner)
	if !ok {
		return nil, "", "", nil, nil, fmt.Errorf("no CPI driver registered for provisioner %q", provisioner)
	}

	// Merge parameters: ModelClass (provisioner-level) as base, Model (instance-level) overrides.
	merged := make(map[string]string, len(r.cfg.ModelClass.Spec.Parameters)+len(r.cfg.Model.Spec.Parameters))
	for k, v := range r.cfg.ModelClass.Spec.Parameters {
		if r.cfg.Model.Spec.InvocationProfileRef == nil {
			merged[k] = v
		}
	}
	for k, v := range r.cfg.Model.Spec.Parameters {
		merged[k] = v
	}
	if len(merged) == 0 {
		merged = nil
	}

	return drv, r.cfg.Model.Spec.ModelID, r.cfg.Model.Spec.Endpoint, r.cfg.Credentials, merged, nil
}

// CreateSession implements runtime.Runtime.
func (r *NativeRuntime) CreateSession(ctx context.Context, req runtime.CreateSessionRequest) (runtime.CreateSessionResponse, error) {
	var profileBinding *v1alpha1.ModelInvocationProfileBinding
	if r.cfg.Model != nil && r.cfg.Model.Spec.InvocationProfileRef != nil {
		profile, err := modelinvocation.Load(ctx, r.cfg.Store, r.cfg.Model)
		if err != nil {
			return runtime.CreateSessionResponse{}, err
		}
		profileBinding = &profile.Binding
	}
	session, err := r.mgr.Create(ctx, req)
	if err != nil {
		return runtime.CreateSessionResponse{}, err
	}

	// Initialize conversation history with the system prompt.
	r.mu.Lock()
	r.history[session.ID] = []cpi.Message{{
		Role:    "system",
		Content: r.systemPrompt(),
	}}
	r.profiles[session.ID] = profileBinding
	r.mu.Unlock()

	return runtime.CreateSessionResponse{SessionID: session.ID}, nil
}

// Step implements runtime.Runtime. It runs the agent loop until the model
// produces a final answer (no tool calls) or an error/cancel occurs.
func (r *NativeRuntime) Step(ctx context.Context, req runtime.StepRequest) (*runtime.StepStream, error) {
	session, err := r.mgr.Get(req.SessionID)
	if err != nil {
		return nil, err
	}

	// Append user input to history.
	r.mu.Lock()
	if r.active[session.ID] != 0 {
		r.mu.Unlock()
		return nil, fmt.Errorf("native session already has an active turn")
	}
	r.history[session.ID] = append(r.history[session.ID], cpi.Message{
		Role:    "user",
		Content: req.Input,
	})
	r.active[session.ID]++
	r.mu.Unlock()

	emitter := runtime.NewEventEmitter(ctx, session, 16)
	stream := emitter.Stream()

	// Run the agent loop in a goroutine.
	go r.agentLoop(ctx, session, emitter)

	return stream, nil
}

// Cancel implements runtime.Runtime.
func (r *NativeRuntime) Cancel(ctx context.Context, req runtime.CancelRequest) (runtime.CancelResponse, error) {
	return r.mgr.Cancel(ctx, req)
}

// Checkpoint implements runtime.Runtime.
func (r *NativeRuntime) Checkpoint(ctx context.Context, sessionID string) (runtime.CheckpointResponse, error) {
	if err := ctx.Err(); err != nil {
		return runtime.CheckpointResponse{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active[sessionID] != 0 {
		return runtime.CheckpointResponse{}, fmt.Errorf("native checkpoint requires a quiescent session")
	}
	checkpoint, err := r.mgr.Checkpoint(sessionID)
	if err != nil {
		return runtime.CheckpointResponse{}, err
	}
	messages := r.history[sessionID]
	if len(messages) == 0 {
		return runtime.CheckpointResponse{}, runtime.ErrInvalidCheckpoint
	}
	checkpoint.Token, err = json.Marshal(nativeCheckpoint{Version: 1, Session: checkpoint.Token, Messages: messages, InvocationProfileBinding: r.profiles[sessionID]})
	if err != nil {
		return runtime.CheckpointResponse{}, fmt.Errorf("marshal native checkpoint: %w", err)
	}
	if len(checkpoint.Token) > maxCheckpointBytes {
		return runtime.CheckpointResponse{}, fmt.Errorf("native checkpoint exceeds %d bytes", maxCheckpointBytes)
	}
	return checkpoint, nil
}

// Resume implements runtime.Runtime.
func (r *NativeRuntime) Resume(ctx context.Context, token []byte) (runtime.ResumeResponse, error) {
	if err := ctx.Err(); err != nil {
		return runtime.ResumeResponse{}, err
	}
	if len(token) == 0 || len(token) > maxCheckpointBytes {
		return runtime.ResumeResponse{}, runtime.ErrInvalidCheckpoint
	}
	var checkpoint nativeCheckpoint
	decoder := json.NewDecoder(bytes.NewReader(token))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&checkpoint); err != nil {
		return runtime.ResumeResponse{}, runtime.ErrInvalidCheckpoint
	}
	if decoder.Decode(&struct{}{}) != io.EOF || checkpoint.Version != 1 || len(checkpoint.Session) == 0 || len(checkpoint.Messages) == 0 || checkpoint.Messages[0].Role != "system" {
		return runtime.ResumeResponse{}, runtime.ErrInvalidCheckpoint
	}
	for _, message := range checkpoint.Messages {
		switch message.Role {
		case "system", "user", "assistant", "tool":
		default:
			return runtime.ResumeResponse{}, runtime.ErrInvalidCheckpoint
		}
	}
	var current *v1alpha1.ModelInvocationProfileBinding
	if r.cfg.Model != nil {
		profile, err := modelinvocation.Load(ctx, r.cfg.Store, r.cfg.Model)
		if err != nil {
			return runtime.ResumeResponse{}, fmt.Errorf("%w: invocation profile unavailable: %v", runtime.ErrInvalidCheckpoint, err)
		}
		if profile != nil {
			current = &profile.Binding
		}
	}
	if !modelinvocation.ValidBinding(checkpoint.InvocationProfileBinding) || !modelinvocation.EqualBinding(current, checkpoint.InvocationProfileBinding) {
		return runtime.ResumeResponse{}, fmt.Errorf("%w: invocation profile identity or digest changed", runtime.ErrInvalidCheckpoint)
	}
	session, err := r.mgr.Resume(ctx, checkpoint.Session)
	if err != nil {
		return runtime.ResumeResponse{}, err
	}
	r.mu.Lock()
	r.history[session.ID] = checkpoint.Messages
	r.profiles[session.ID] = checkpoint.InvocationProfileBinding
	r.mu.Unlock()
	return runtime.ResumeResponse{SessionID: session.ID}, nil
}

// GetCapabilities implements runtime.Runtime.
func (r *NativeRuntime) GetCapabilities(_ context.Context) (runtime.Capabilities, error) {
	features := []string{
		"streaming",
		"tool-calling",
		"checkpoint-resume",
		"credit-backpressure",
	}
	if r.cfg.MemoryBackend != nil {
		features = append(features, "memory")
	}
	return runtime.Capabilities{
		SupportedFeatures:     features,
		RuntimeVersion:        runtimeVersion,
		MaxConcurrentSessions: r.cfg.MaxConcurrentSessions,
	}, nil
}

// Health implements runtime.Runtime.
func (r *NativeRuntime) Health(_ context.Context) (runtime.HealthStatus, error) {
	return runtime.HealthStatus{
		State:          runtime.HealthHealthy,
		Message:        "ok",
		ActiveSessions: uint32(r.mgr.ActiveCount()),
	}, nil
}

// agentLoop runs the iterative model→tool cycle.
func (r *NativeRuntime) agentLoop(ctx context.Context, session *runtime.Session, emitter *runtime.EventEmitter) {
	defer emitter.Close()
	defer func() {
		r.mu.Lock()
		r.active[session.ID]--
		if r.active[session.ID] == 0 {
			delete(r.active, session.ID)
		}
		r.mu.Unlock()
	}()

	// Emit status: thinking
	_ = emitter.Emit(runtime.StepEvent{
		Type:   runtime.EventStatus,
		Status: &runtime.StatusEvent{State: runtime.StateThinking, Message: "thinking"},
	})

	for i := 0; i < r.cfg.MaxIterations; i++ {
		select {
		case <-ctx.Done():
			_ = emitter.Emit(runtime.StepEvent{
				Type:  runtime.EventError,
				Error: &runtime.ErrorStreamEvent{Code: "Cancelled", Message: "context cancelled"},
			})
			return
		case <-session.CancelCtx().Done():
			_ = emitter.Emit(runtime.StepEvent{
				Type:  runtime.EventError,
				Error: &runtime.ErrorStreamEvent{Code: "Cancelled", Message: "session cancelled"},
			})
			return
		default:
		}

		// Assemble context.
		messages := r.assembleMessages(session.ID)
		tools := r.toolDefinitions()

		// Resolve CPI driver and model parameters.
		driver, modelID, endpoint, creds, params, err := r.resolveDriver()
		if err != nil {
			_ = emitter.Emit(runtime.StepEvent{
				Type:  runtime.EventError,
				Error: &runtime.ErrorStreamEvent{Code: "DriverResolutionError", Message: err.Error()},
			})
			return
		}

		// Call model.
		completion := &cpi.CompletionRequest{
			Model:      modelID,
			Endpoint:   endpoint,
			Creds:      creds,
			Messages:   messages,
			Parameters: params,
			Tools:      tools,
		}
		if r.cfg.Model != nil {
			if r.cfg.Agent != nil && r.cfg.Agent.Spec.Runtime != nil && r.cfg.Agent.Spec.Runtime.MaxTokens != nil {
				completion.MaxCompletionTokens = *r.cfg.Agent.Spec.Runtime.MaxTokens
			}
			completion, err = cpi.BindCompletion(ctx, r.cfg.Store, driver, r.cfg.Model, completion)
			if err == nil {
				var current *v1alpha1.ModelInvocationProfileBinding
				if completion.Invocation != nil {
					current = &completion.Invocation.Binding
				}
				r.mu.Lock()
				snapshot := r.profiles[session.ID]
				r.mu.Unlock()
				if !modelinvocation.EqualBinding(snapshot, current) {
					err = fmt.Errorf("invocation profile changed during the session")
				}
			}
			if err != nil {
				_ = emitter.Emit(runtime.StepEvent{Type: runtime.EventError, Error: &runtime.ErrorStreamEvent{Code: "InvocationProfileInvalid", Message: err.Error()}})
				return
			}
		}
		resp, err := driver.Complete(ctx, completion)
		if err != nil {
			_ = emitter.Emit(runtime.StepEvent{
				Type:  runtime.EventError,
				Error: &runtime.ErrorStreamEvent{Code: "ModelError", Message: err.Error(), Retryable: true},
			})
			return
		}

		// If no tool calls, emit the final output and return.
		if len(resp.ToolCalls) == 0 {
			// Append assistant message to history.
			r.mu.Lock()
			r.history[session.ID] = append(r.history[session.ID], cpi.Message{
				Role:    "assistant",
				Content: resp.Content,
			})
			r.mu.Unlock()

			// Stream tokens for the content.
			if resp.Content != "" {
				_ = emitter.Emit(runtime.StepEvent{
					Type:  runtime.EventToken,
					Token: &runtime.TokenEvent{Content: resp.Content, Role: "assistant"},
				})
			}

			// Emit final output.
			_ = emitter.Emit(runtime.StepEvent{
				Type:   runtime.EventOutput,
				Output: &runtime.OutputStreamEvent{Content: resp.Content, IsFinal: true},
			})
			_ = emitter.Emit(runtime.StepEvent{
				Type:   runtime.EventStatus,
				Status: &runtime.StatusEvent{State: runtime.StateCompleted, Message: "done"},
			})
			return
		}

		// Process tool calls.
		r.mu.Lock()
		r.history[session.ID] = append(r.history[session.ID], cpi.Message{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})
		r.mu.Unlock()

		_ = emitter.Emit(runtime.StepEvent{
			Type:   runtime.EventStatus,
			Status: &runtime.StatusEvent{State: runtime.StateExecutingTool, Message: "executing tools"},
		})

		for _, tc := range resp.ToolCalls {
			// Emit tool call event.
			_ = emitter.Emit(runtime.StepEvent{
				Type:     runtime.EventToolCall,
				ToolCall: &runtime.ToolCallStreamEvent{ToolName: tc.Name, Arguments: tc.Arguments, CallID: tc.ID},
			})

			// Execute the tool.
			result, toolErr := r.executeTool(ctx, tc)

			var output string
			var isError bool
			if toolErr != nil {
				output = toolErr.Error()
				isError = true
			} else {
				outputBytes, _ := json.Marshal(result.Output)
				output = string(outputBytes)
			}

			// Emit tool result event.
			_ = emitter.Emit(runtime.StepEvent{
				Type:       runtime.EventToolResult,
				ToolResult: &runtime.ToolResultEvent{CallID: tc.ID, Output: output, IsError: isError},
			})

			// Append tool result to history.
			r.mu.Lock()
			r.history[session.ID] = append(r.history[session.ID], cpi.Message{
				Role:       "tool",
				Content:    output,
				ToolCallID: tc.ID,
			})
			r.mu.Unlock()
		}

		// Continue loop — model will see tool results.
		_ = emitter.Emit(runtime.StepEvent{
			Type:   runtime.EventStatus,
			Status: &runtime.StatusEvent{State: runtime.StateThinking, Message: "thinking"},
		})
	}

	// Max iterations exceeded.
	_ = emitter.Emit(runtime.StepEvent{
		Type:  runtime.EventError,
		Error: &runtime.ErrorStreamEvent{Code: "MaxIterations", Message: fmt.Sprintf("exceeded %d iterations", r.cfg.MaxIterations)},
	})
}

// assembleMessages returns the current conversation messages for a session.
func (r *NativeRuntime) assembleMessages(sessionID string) []cpi.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	msgs := r.history[sessionID]
	out := make([]cpi.Message, len(msgs))
	copy(out, msgs)
	return out
}

// systemPrompt builds the system prompt from the agent spec.
func (r *NativeRuntime) systemPrompt() string {
	if r.cfg.Agent == nil {
		return ""
	}
	return r.cfg.Agent.Spec.Instructions.Inline
}

// toolDefinitions builds the CPI tool definitions from resolved tools.
func (r *NativeRuntime) toolDefinitions() []cpi.ToolDefinition {
	defs := make([]cpi.ToolDefinition, 0, len(r.cfg.Tools))
	for _, t := range r.cfg.Tools {
		defs = append(defs, cpi.ToolDefinition{
			Name:        t.ObjectMeta.Name,
			Description: t.Spec.Description,
			Parameters:  t.Spec.Schema,
		})
	}
	return defs
}

// executeTool invokes a single tool via the ToolDispatcher.
func (r *NativeRuntime) executeTool(ctx context.Context, tc cpi.ToolCall) (runtime.ToolExecutionResult, error) {
	if r.cfg.ToolDispatcher == nil {
		return runtime.ToolExecutionResult{}, fmt.Errorf("no tool dispatcher configured")
	}

	// Find the tool by name.
	var tool v1alpha1.Tool
	var found bool
	for _, t := range r.cfg.Tools {
		if t.ObjectMeta.Name == tc.Name {
			tool = t
			found = true
			break
		}
	}
	if !found {
		return runtime.ToolExecutionResult{}, fmt.Errorf("tool %q not found", tc.Name)
	}

	// Parse arguments.
	var input map[string]any
	if tc.Arguments != "" {
		if err := json.Unmarshal([]byte(tc.Arguments), &input); err != nil {
			return runtime.ToolExecutionResult{}, fmt.Errorf("invalid tool arguments: %w", err)
		}
	}

	return r.cfg.ToolDispatcher.Execute(ctx, runtime.ToolExecutionRequest{
		Tool:  tool,
		Input: input,
	})
}

// ReadMemory reads context from the memory backend for context assembly.
func (r *NativeRuntime) ReadMemory(ctx context.Context, vector []float32, topK int) ([]memory.SearchResult, error) {
	if r.cfg.MemoryBackend == nil {
		return nil, nil
	}
	return r.cfg.MemoryBackend.Search(ctx, memory.SearchRequest{
		BackendHandle: r.cfg.MemoryHandle,
		Vector:        vector,
		TopK:          topK,
	})
}

// WriteMemory stores a record in the memory backend.
func (r *NativeRuntime) WriteMemory(ctx context.Context, id, content string, embedding []float32, metadata map[string]string) error {
	if r.cfg.MemoryBackend == nil {
		return nil
	}
	_, err := r.cfg.MemoryBackend.Store(ctx, memory.StoreRequest{
		BackendHandle: r.cfg.MemoryHandle,
		ID:            id,
		Content:       content,
		Embedding:     embedding,
		Metadata:      metadata,
		Timestamp:     time.Now(),
	})
	return err
}
