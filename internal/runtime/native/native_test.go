// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package native

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/conformance"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/runtime"
)

// --- Fakes / mocks ---

// fakeDriver is a controllable CPI driver for tests.
type fakeDriver struct {
	mu        sync.Mutex
	name      string
	responses []*cpi.CompletionResponse
	errors    []error
	callIdx   int
	requests  []*cpi.CompletionRequest
}

func (d *fakeDriver) Name() string { return d.name }

func (d *fakeDriver) Validate(_ context.Context, _ *v1alpha1.ModelSpec, _ *v1alpha1.ModelClassSpec) error {
	return nil
}

func (d *fakeDriver) Probe(_ context.Context, _ *v1alpha1.ModelSpec, _ *cpi.Credentials) (*cpi.HealthResult, error) {
	return &cpi.HealthResult{Available: true}, nil
}

func (d *fakeDriver) Complete(_ context.Context, req *cpi.CompletionRequest) (*cpi.CompletionResponse, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.requests = append(d.requests, req)
	idx := d.callIdx
	d.callIdx++
	if idx < len(d.errors) && d.errors[idx] != nil {
		return nil, d.errors[idx]
	}
	if idx < len(d.responses) {
		return d.responses[idx], nil
	}
	return &cpi.CompletionResponse{Content: "default"}, nil
}

func (d *fakeDriver) Embed(_ context.Context, _ *cpi.EmbedRequest) (*cpi.EmbedResponse, error) {
	return &cpi.EmbedResponse{}, nil
}

// fakeToolExecutor implements runtime.ToolExecutor.
type fakeToolExecutor struct {
	mu      sync.Mutex
	results map[string]runtime.ToolExecutionResult
	errors  map[string]error
	called  []string
}

func newFakeToolExecutor() *fakeToolExecutor {
	return &fakeToolExecutor{
		results: make(map[string]runtime.ToolExecutionResult),
		errors:  make(map[string]error),
	}
}

func (e *fakeToolExecutor) Execute(_ context.Context, req runtime.ToolExecutionRequest, _ runtime.ToolSandboxLimits) (runtime.ToolExecutionResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	name := req.Tool.ObjectMeta.Name
	e.called = append(e.called, name)
	if err, ok := e.errors[name]; ok && err != nil {
		return runtime.ToolExecutionResult{}, err
	}
	if res, ok := e.results[name]; ok {
		return res, nil
	}
	return runtime.ToolExecutionResult{Output: map[string]any{"result": "ok"}}, nil
}

// --- Helpers ---

func drainEvents(t *testing.T, stream *runtime.StepStream, timeout time.Duration) []runtime.StepEvent {
	t.Helper()
	var events []runtime.StepEvent
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case ev, ok := <-stream.Events:
			if !ok {
				return events
			}
			events = append(events, ev)
		case <-timer.C:
			return events
		}
	}
}

func findEvent(events []runtime.StepEvent, typ runtime.EventType) *runtime.StepEvent {
	for i := range events {
		if events[i].Type == typ {
			return &events[i]
		}
	}
	return nil
}

func findEvents(events []runtime.StepEvent, typ runtime.EventType) []runtime.StepEvent {
	var out []runtime.StepEvent
	for _, ev := range events {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

func newTestAgent(instructions string) *v1alpha1.Agent {
	return &v1alpha1.Agent{
		ObjectMeta: v1alpha1.ObjectMeta{Name: "test-agent", Namespace: "default"},
		Spec: v1alpha1.AgentSpec{
			Instructions: v1alpha1.AgentInstructions{Inline: instructions},
		},
	}
}

func newTestTool(name, description string) v1alpha1.Tool {
	return v1alpha1.Tool{
		ObjectMeta: v1alpha1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: v1alpha1.ToolSpec{
			Description: description,
			Schema:      map[string]any{"type": "object"},
		},
	}
}

// --- Tests ---

func TestNativeRuntime_Conformance(t *testing.T) {
	conformance.RunRuntimeSuite(t, func(t *testing.T) runtime.Runtime {
		t.Helper()
		return New(Config{
			Driver:      &fakeDriver{name: "conformance-driver"},
			DriverModel: "conformance-model",
			Agent:       newTestAgent("You are a conformance test agent."),
		})
	})
}

// TC-0112: Run S1 scenario end-to-end on native runtime — CreateSession + Step produces
// structured events with monotonic sequence and correct lifecycle.
func TestNativeRuntime_EndToEnd_SimpleCompletion(t *testing.T) {
	driver := &fakeDriver{
		name: "test-driver",
		responses: []*cpi.CompletionResponse{
			{Content: "Hello, world!", FinishReason: "stop"},
		},
	}

	rt := New(Config{
		Driver:      driver,
		DriverModel: "test-model",
		Agent:       newTestAgent("You are a helpful assistant."),
	})

	ctx := context.Background()

	// CreateSession.
	resp, err := rt.CreateSession(ctx, runtime.CreateSessionRequest{
		AgentName:      "test-agent",
		Namespace:      "default",
		InitialCredits: 50,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if resp.SessionID == "" {
		t.Fatal("expected non-empty session ID")
	}

	// Step with user input.
	stream, err := rt.Step(ctx, runtime.StepRequest{
		SessionID: resp.SessionID,
		Input:     "Hi",
	})
	if err != nil {
		t.Fatalf("Step: %v", err)
	}

	events := drainEvents(t, stream, 2*time.Second)
	if len(events) == 0 {
		t.Fatal("expected events, got none")
	}

	// Verify monotonic sequences.
	for i := 1; i < len(events); i++ {
		if events[i].Sequence <= events[i-1].Sequence {
			t.Errorf("event[%d].Sequence=%d <= event[%d].Sequence=%d",
				i, events[i].Sequence, i-1, events[i-1].Sequence)
		}
	}

	// Should have: status(thinking), token, output(final), status(completed).
	statusEvents := findEvents(events, runtime.EventStatus)
	if len(statusEvents) < 2 {
		t.Fatalf("expected at least 2 status events, got %d", len(statusEvents))
	}
	if statusEvents[0].Status.State != runtime.StateThinking {
		t.Errorf("first status = %d, want StateThinking", statusEvents[0].Status.State)
	}
	lastStatus := statusEvents[len(statusEvents)-1]
	if lastStatus.Status.State != runtime.StateCompleted {
		t.Errorf("last status = %d, want StateCompleted", lastStatus.Status.State)
	}

	output := findEvent(events, runtime.EventOutput)
	if output == nil {
		t.Fatal("expected output event")
	}
	if output.Output.Content != "Hello, world!" {
		t.Errorf("output = %q, want %q", output.Output.Content, "Hello, world!")
	}
	if !output.Output.IsFinal {
		t.Error("expected IsFinal=true")
	}

	// Verify driver was called with system prompt + user message.
	driver.mu.Lock()
	if len(driver.requests) != 1 {
		t.Fatalf("driver called %d times, want 1", len(driver.requests))
	}
	msgs := driver.requests[0].Messages
	driver.mu.Unlock()
	if len(msgs) < 2 {
		t.Fatalf("expected >= 2 messages, got %d", len(msgs))
	}
	if msgs[0].Role != "system" {
		t.Errorf("msgs[0].Role = %q, want system", msgs[0].Role)
	}
	if msgs[1].Role != "user" {
		t.Errorf("msgs[1].Role = %q, want user", msgs[1].Role)
	}
	if msgs[1].Content != "Hi" {
		t.Errorf("msgs[1].Content = %q, want Hi", msgs[1].Content)
	}
}

// TC-0112: Run S1 scenario end-to-end — tool calling cycle.
func TestNativeRuntime_EndToEnd_ToolCalling(t *testing.T) {
	driver := &fakeDriver{
		name: "test-driver",
		responses: []*cpi.CompletionResponse{
			// First call: model wants to call a tool.
			{
				Content: "",
				ToolCalls: []cpi.ToolCall{
					{ID: "call-1", Name: "get_weather", Arguments: `{"city":"NYC"}`},
				},
			},
			// Second call: model produces final answer using tool result.
			{Content: "The weather in NYC is sunny.", FinishReason: "stop"},
		},
	}

	executor := newFakeToolExecutor()
	executor.results["get_weather"] = runtime.ToolExecutionResult{
		Output: map[string]any{"temp": 72, "condition": "sunny"},
	}
	dispatcher := runtime.NewToolDispatcher(executor, executor)

	tools := []v1alpha1.Tool{newTestTool("get_weather", "Get weather for a city")}

	rt := New(Config{
		Driver:         driver,
		DriverModel:    "test-model",
		Agent:          newTestAgent("You are a weather assistant."),
		Tools:          tools,
		ToolDispatcher: dispatcher,
	})

	ctx := context.Background()
	resp, _ := rt.CreateSession(ctx, runtime.CreateSessionRequest{
		AgentName: "test-agent", Namespace: "default", InitialCredits: 50,
	})

	stream, err := rt.Step(ctx, runtime.StepRequest{
		SessionID: resp.SessionID,
		Input:     "What is the weather in NYC?",
	})
	if err != nil {
		t.Fatalf("Step: %v", err)
	}

	events := drainEvents(t, stream, 2*time.Second)

	// Should contain: tool call, tool result, and final output events.
	tcEvent := findEvent(events, runtime.EventToolCall)
	if tcEvent == nil {
		t.Fatal("expected tool call event")
	}
	if tcEvent.ToolCall.ToolName != "get_weather" {
		t.Errorf("ToolCall.ToolName = %q, want get_weather", tcEvent.ToolCall.ToolName)
	}

	trEvent := findEvent(events, runtime.EventToolResult)
	if trEvent == nil {
		t.Fatal("expected tool result event")
	}
	if trEvent.ToolResult.IsError {
		t.Error("expected tool result not to be error")
	}
	if trEvent.ToolResult.CallID != "call-1" {
		t.Errorf("ToolResult.CallID = %q, want call-1", trEvent.ToolResult.CallID)
	}

	output := findEvent(events, runtime.EventOutput)
	if output == nil {
		t.Fatal("expected final output event")
	}
	if output.Output.Content != "The weather in NYC is sunny." {
		t.Errorf("output = %q, want 'The weather in NYC is sunny.'", output.Output.Content)
	}

	// Verify tool executor was called.
	executor.mu.Lock()
	if len(executor.called) != 1 || executor.called[0] != "get_weather" {
		t.Errorf("tool executor called = %v, want [get_weather]", executor.called)
	}
	executor.mu.Unlock()

	// Verify driver called twice (initial + after tool result).
	driver.mu.Lock()
	if len(driver.requests) != 2 {
		t.Errorf("driver called %d times, want 2", len(driver.requests))
	}
	// Second call should include tool result in messages.
	if len(driver.requests) == 2 {
		msgs := driver.requests[1].Messages
		var hasToolMsg bool
		for _, m := range msgs {
			if m.Role == "tool" {
				hasToolMsg = true
				break
			}
		}
		if !hasToolMsg {
			t.Error("second driver call should include tool result message")
		}
	}
	driver.mu.Unlock()
}

// TC-0112: Cancellation during step.
func TestNativeRuntime_StepCancellation(t *testing.T) {
	// Driver that blocks until cancelled.
	driver := &fakeDriver{
		name: "test-driver",
		responses: []*cpi.CompletionResponse{
			{Content: "should not appear"},
		},
	}

	rt := New(Config{
		Driver:      driver,
		DriverModel: "test-model",
		Agent:       newTestAgent("agent"),
	})

	ctx, cancel := context.WithCancel(context.Background())
	resp, _ := rt.CreateSession(ctx, runtime.CreateSessionRequest{
		AgentName: "a", Namespace: "n", InitialCredits: 10,
	})

	// Cancel before step can complete model call.
	cancel()

	stream, err := rt.Step(ctx, runtime.StepRequest{
		SessionID: resp.SessionID,
		Input:     "test",
	})
	if err != nil {
		// Context cancelled may prevent step entirely — acceptable.
		return
	}

	events := drainEvents(t, stream, 1*time.Second)
	// Should get an error event about cancellation or no events.
	errEv := findEvent(events, runtime.EventError)
	if errEv != nil && errEv.Error.Code != "Cancelled" && errEv.Error.Code != "ModelError" {
		t.Errorf("unexpected error code: %s", errEv.Error.Code)
	}
}

// TC-0112: Model error propagates as ErrorStreamEvent.
func TestNativeRuntime_ModelError(t *testing.T) {
	driver := &fakeDriver{
		name:   "test-driver",
		errors: []error{fmt.Errorf("rate limited")},
	}

	rt := New(Config{
		Driver:      driver,
		DriverModel: "test-model",
		Agent:       newTestAgent("agent"),
	})

	ctx := context.Background()
	resp, _ := rt.CreateSession(ctx, runtime.CreateSessionRequest{
		AgentName: "a", Namespace: "n", InitialCredits: 10,
	})

	stream, err := rt.Step(ctx, runtime.StepRequest{
		SessionID: resp.SessionID,
		Input:     "test",
	})
	if err != nil {
		t.Fatalf("Step: %v", err)
	}

	events := drainEvents(t, stream, 2*time.Second)
	errEv := findEvent(events, runtime.EventError)
	if errEv == nil {
		t.Fatal("expected error event")
	}
	if errEv.Error.Code != "ModelError" {
		t.Errorf("error code = %q, want ModelError", errEv.Error.Code)
	}
	if errEv.Error.Retryable != true {
		t.Error("expected retryable=true for model errors")
	}
}

// TC-0112: Max iterations limit.
func TestNativeRuntime_MaxIterations(t *testing.T) {
	// Driver always returns tool calls — should hit max iterations.
	driver := &fakeDriver{
		name: "test-driver",
	}
	// Override Complete to always return a tool call.
	for i := 0; i < 5; i++ {
		driver.responses = append(driver.responses, &cpi.CompletionResponse{
			ToolCalls: []cpi.ToolCall{{ID: fmt.Sprintf("call-%d", i), Name: "loop_tool", Arguments: "{}"}},
		})
	}

	executor := newFakeToolExecutor()
	dispatcher := runtime.NewToolDispatcher(executor, executor)
	tools := []v1alpha1.Tool{newTestTool("loop_tool", "loops")}

	rt := New(Config{
		Driver:         driver,
		DriverModel:    "test-model",
		Agent:          newTestAgent("agent"),
		Tools:          tools,
		ToolDispatcher: dispatcher,
		MaxIterations:  3,
	})

	ctx := context.Background()
	resp, _ := rt.CreateSession(ctx, runtime.CreateSessionRequest{
		AgentName: "a", Namespace: "n", InitialCredits: 100,
	})

	stream, _ := rt.Step(ctx, runtime.StepRequest{
		SessionID: resp.SessionID,
		Input:     "go",
	})

	events := drainEvents(t, stream, 3*time.Second)
	errEv := findEvent(events, runtime.EventError)
	if errEv == nil {
		t.Fatal("expected MaxIterations error event")
	}
	if errEv.Error.Code != "MaxIterations" {
		t.Errorf("error code = %q, want MaxIterations", errEv.Error.Code)
	}
}

// TC-0112: Tool execution error is reported in ToolResult with IsError=true.
func TestNativeRuntime_ToolError(t *testing.T) {
	driver := &fakeDriver{
		name: "test-driver",
		responses: []*cpi.CompletionResponse{
			{ToolCalls: []cpi.ToolCall{{ID: "c1", Name: "broken_tool", Arguments: "{}"}}},
			{Content: "tool failed, sorry"},
		},
	}

	executor := newFakeToolExecutor()
	executor.errors["broken_tool"] = fmt.Errorf("connection refused")
	dispatcher := runtime.NewToolDispatcher(executor, executor)
	tools := []v1alpha1.Tool{newTestTool("broken_tool", "breaks")}

	rt := New(Config{
		Driver:         driver,
		DriverModel:    "m",
		Agent:          newTestAgent("a"),
		Tools:          tools,
		ToolDispatcher: dispatcher,
	})

	ctx := context.Background()
	resp, _ := rt.CreateSession(ctx, runtime.CreateSessionRequest{
		AgentName: "a", Namespace: "n", InitialCredits: 50,
	})

	stream, _ := rt.Step(ctx, runtime.StepRequest{
		SessionID: resp.SessionID,
		Input:     "do it",
	})

	events := drainEvents(t, stream, 2*time.Second)
	trEvent := findEvent(events, runtime.EventToolResult)
	if trEvent == nil {
		t.Fatal("expected tool result event")
	}
	if !trEvent.ToolResult.IsError {
		t.Error("expected IsError=true for failed tool")
	}
	if trEvent.ToolResult.Output == "" {
		t.Error("expected non-empty error output")
	}
}

func TestNativeRuntime_GetCapabilities(t *testing.T) {
	rt := New(Config{
		Driver:                &fakeDriver{name: "d"},
		DriverModel:           "m",
		MaxConcurrentSessions: 10,
	})

	caps, err := rt.GetCapabilities(context.Background())
	if err != nil {
		t.Fatalf("GetCapabilities: %v", err)
	}
	if caps.RuntimeVersion != runtimeVersion {
		t.Errorf("RuntimeVersion = %q, want %q", caps.RuntimeVersion, runtimeVersion)
	}
	if caps.MaxConcurrentSessions != 10 {
		t.Errorf("MaxConcurrentSessions = %d, want 10", caps.MaxConcurrentSessions)
	}
	// Without memory backend, should not include "memory" feature.
	for _, f := range caps.SupportedFeatures {
		if f == "memory" {
			t.Error("memory feature should not be present without memory backend")
		}
	}
}

func TestNativeRuntime_Health(t *testing.T) {
	rt := New(Config{
		Driver:      &fakeDriver{name: "d"},
		DriverModel: "m",
	})

	health, err := rt.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health.State != runtime.HealthHealthy {
		t.Errorf("State = %d, want HealthHealthy", health.State)
	}
}

// TC-0112: Checkpoint and Resume work through native runtime.
func TestNativeRuntime_CheckpointResume(t *testing.T) {
	driver := &fakeDriver{
		name: "test-driver",
		responses: []*cpi.CompletionResponse{
			{Content: "first response"},
			{Content: "resumed response"},
		},
	}

	rt := New(Config{
		Driver:      driver,
		DriverModel: "m",
		Agent:       newTestAgent("agent"),
	})

	ctx := context.Background()
	resp, _ := rt.CreateSession(ctx, runtime.CreateSessionRequest{
		AgentName: "a", Namespace: "n", InitialCredits: 50,
	})

	// Step to build some history.
	stream, _ := rt.Step(ctx, runtime.StepRequest{SessionID: resp.SessionID, Input: "hello"})
	drainEvents(t, stream, 2*time.Second)

	// Checkpoint.
	cpResp, err := rt.Checkpoint(ctx, resp.SessionID)
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if len(cpResp.Token) == 0 {
		t.Fatal("expected non-empty checkpoint token")
	}

	// Resume.
	resumeResp, err := rt.Resume(ctx, cpResp.Token)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumeResp.SessionID == "" {
		t.Fatal("expected non-empty resumed session ID")
	}
	if resumeResp.SessionID == resp.SessionID {
		t.Error("resumed session should have a new ID")
	}
}

// TC-0112: Verify tool arguments are correctly parsed and passed.
func TestNativeRuntime_CheckpointRestoresConversation(t *testing.T) {
	for _, freshAdapter := range []bool{false, true} {
		t.Run(fmt.Sprintf("fresh-adapter-%t", freshAdapter), func(test *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			driver := &fakeDriver{responses: []*cpi.CompletionResponse{{Content: "first answer"}, {Content: "second answer"}}}
			config := Config{Driver: driver, Agent: newTestAgent("original system instructions")}
			adapter := New(config)
			session, err := adapter.CreateSession(ctx, runtime.CreateSessionRequest{AgentName: "worker", Namespace: "tenant", InitialCredits: 32})
			if err != nil {
				test.Fatal(err)
			}
			first, err := adapter.Step(ctx, runtime.StepRequest{SessionID: session.SessionID, Input: "first question"})
			if err != nil {
				test.Fatal(err)
			}
			for range first.Events {
			}
			checkpoint, err := adapter.Checkpoint(ctx, session.SessionID)
			if err != nil {
				test.Fatal(err)
			}
			if freshAdapter {
				adapter = New(Config{Driver: driver, Agent: newTestAgent("replacement instructions must not overwrite restored history")})
			}
			resumed, err := adapter.Resume(ctx, checkpoint.Token)
			if err != nil {
				test.Fatal(err)
			}
			second, err := adapter.Step(ctx, runtime.StepRequest{SessionID: resumed.SessionID, Input: "second question"})
			if err != nil {
				test.Fatal(err)
			}
			for range second.Events {
			}
			driver.mu.Lock()
			defer driver.mu.Unlock()
			if len(driver.requests) != 2 {
				test.Fatalf("expected two model calls, got %d", len(driver.requests))
			}
			messages := driver.requests[1].Messages
			if len(messages) != 4 || messages[0].Content != "original system instructions" || messages[1].Content != "first question" || messages[2].Content != "first answer" || messages[3].Content != "second question" {
				test.Fatalf("checkpoint did not restore complete conversation: %+v", messages)
			}
		})
	}
}

func TestNativeRuntime_ResumeRejectsInvalidCheckpoints(test *testing.T) {
	for _, token := range [][]byte{
		[]byte(`{}`),
		[]byte(`{"session_id":"legacy","agent_name":"worker","namespace":"tenant"}`),
		[]byte(`{"version":2,"session":{},"messages":[]}`),
		[]byte(`{"version":1,"session":{"session_id":"old"},"messages":[]} {}`),
		[]byte(`{"version":1,"session":{"session_id":"old"},"messages":[{"Role":"tool"}]}`),
		[]byte(`{"version":1,"session":{"session_id":"old"},"messages":[{"Role":"system"},{"Role":"invalid"}]}`),
		[]byte(`{"version":1,"session":null,"messages":[{"Role":"system"}]}`),
		[]byte(`{"version":1,"session":{"session_id":"old"},"messages":[{"Role":"system"}],"unknown":true}`),
		bytes.Repeat([]byte("x"), (4<<20)+1),
	} {
		adapter := New(Config{})
		if _, err := adapter.Resume(context.Background(), token); !errors.Is(err, runtime.ErrInvalidCheckpoint) {
			test.Errorf("invalid checkpoint returned %v", err)
		}
		health, err := adapter.Health(context.Background())
		if err != nil || health.ActiveSessions != 0 {
			test.Fatalf("invalid token created a session: %+v %v", health, err)
		}
	}
}

type checkpointBlockingDriver struct {
	*fakeDriver
	started chan struct{}
	release chan struct{}
}

func (driver *checkpointBlockingDriver) Complete(ctx context.Context, request *cpi.CompletionRequest) (*cpi.CompletionResponse, error) {
	close(driver.started)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-driver.release:
		return driver.fakeDriver.Complete(ctx, request)
	}
}

func TestNativeRuntime_CheckpointRequiresQuiescentTurn(test *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	driver := &checkpointBlockingDriver{fakeDriver: &fakeDriver{}, started: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	defer release.Do(func() { close(driver.release) })
	adapter := New(Config{Driver: driver, Agent: newTestAgent("system")})
	session, err := adapter.CreateSession(ctx, runtime.CreateSessionRequest{InitialCredits: 32})
	if err != nil {
		test.Fatal(err)
	}
	stream, err := adapter.Step(ctx, runtime.StepRequest{SessionID: session.SessionID, Input: "question"})
	if err != nil {
		test.Fatal(err)
	}
	select {
	case <-driver.started:
	case <-ctx.Done():
		test.Fatal(ctx.Err())
	}
	if _, err := adapter.Checkpoint(ctx, session.SessionID); err == nil {
		test.Fatal("checkpoint captured a running turn")
	}
	if _, err := adapter.Step(ctx, runtime.StepRequest{SessionID: session.SessionID, Input: "concurrent question"}); err == nil {
		test.Fatal("native runtime interleaved a second active turn")
	}
	release.Do(func() { close(driver.release) })
	for range stream.Events {
	}
	if _, err := adapter.Checkpoint(ctx, session.SessionID); err != nil {
		test.Fatalf("completed turn cannot checkpoint: %v", err)
	}
}

func TestNativeRuntime_CheckpointRetainsToolHistoryWithoutReplay(test *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	driver := &fakeDriver{responses: []*cpi.CompletionResponse{
		{ToolCalls: []cpi.ToolCall{{ID: "tool-call", Name: "search", Arguments: `{}`}}},
		{Content: "first answer"},
		{Content: "second answer"},
	}}
	executor := newFakeToolExecutor()
	config := Config{Driver: driver, Agent: newTestAgent("system"), Tools: []v1alpha1.Tool{newTestTool("search", "search")}, ToolDispatcher: runtime.NewToolDispatcher(executor, executor)}
	adapter := New(config)
	session, err := adapter.CreateSession(ctx, runtime.CreateSessionRequest{InitialCredits: 32})
	if err != nil {
		test.Fatal(err)
	}
	first, err := adapter.Step(ctx, runtime.StepRequest{SessionID: session.SessionID, Input: "first question"})
	if err != nil {
		test.Fatal(err)
	}
	for range first.Events {
	}
	checkpoint, err := adapter.Checkpoint(ctx, session.SessionID)
	if err != nil {
		test.Fatal(err)
	}
	adapter = New(config)
	resumed, err := adapter.Resume(ctx, checkpoint.Token)
	if err != nil {
		test.Fatal(err)
	}
	second, err := adapter.Step(ctx, runtime.StepRequest{SessionID: resumed.SessionID, Input: "second question"})
	if err != nil {
		test.Fatal(err)
	}
	for range second.Events {
	}
	driver.mu.Lock()
	defer driver.mu.Unlock()
	if len(driver.requests) != 3 || len(driver.requests[2].Messages) != 6 {
		test.Fatalf("tool conversation not restored: %+v", driver.requests)
	}
	messages := driver.requests[2].Messages
	if len(messages[2].ToolCalls) != 1 || messages[2].ToolCalls[0].ID != "tool-call" || messages[3].ToolCallID != "tool-call" || messages[3].Role != "tool" {
		test.Fatal("restored tool result lost its call binding")
	}
	executor.mu.Lock()
	defer executor.mu.Unlock()
	if len(executor.called) != 1 {
		test.Fatal("resume re-executed a completed tool")
	}
}

func TestNativeRuntime_CheckpointRejectsOversizedState(test *testing.T) {
	adapter := New(Config{Agent: newTestAgent(strings.Repeat("x", 4<<20))})
	session, err := adapter.CreateSession(context.Background(), runtime.CreateSessionRequest{})
	if err != nil {
		test.Fatal(err)
	}
	if _, err := adapter.Checkpoint(context.Background(), session.SessionID); err == nil {
		test.Fatal("oversized checkpoint was accepted")
	}
}

func TestNativeRuntime_ToolArgumentParsing(t *testing.T) {
	args := map[string]any{"query": "test", "limit": float64(5)}
	argsJSON, _ := json.Marshal(args)

	driver := &fakeDriver{
		name: "test-driver",
		responses: []*cpi.CompletionResponse{
			{ToolCalls: []cpi.ToolCall{{ID: "c1", Name: "search", Arguments: string(argsJSON)}}},
			{Content: "done"},
		},
	}

	executor := newFakeToolExecutor()
	dispatcher := runtime.NewToolDispatcher(executor, executor)
	tools := []v1alpha1.Tool{newTestTool("search", "search")}

	rt := New(Config{
		Driver:         driver,
		DriverModel:    "m",
		Agent:          newTestAgent("a"),
		Tools:          tools,
		ToolDispatcher: dispatcher,
	})

	ctx := context.Background()
	resp, _ := rt.CreateSession(ctx, runtime.CreateSessionRequest{
		AgentName: "a", Namespace: "n", InitialCredits: 50,
	})

	stream, _ := rt.Step(ctx, runtime.StepRequest{SessionID: resp.SessionID, Input: "search for test"})
	drainEvents(t, stream, 2*time.Second)

	executor.mu.Lock()
	defer executor.mu.Unlock()
	if len(executor.called) != 1 || executor.called[0] != "search" {
		t.Fatalf("expected [search], got %v", executor.called)
	}
}

// TC-0112: Session not found returns error.
func TestNativeRuntime_StepSessionNotFound(t *testing.T) {
	rt := New(Config{
		Driver:      &fakeDriver{name: "d"},
		DriverModel: "m",
	})

	_, err := rt.Step(context.Background(), runtime.StepRequest{
		SessionID: "nonexistent",
		Input:     "hi",
	})
	if err == nil {
		t.Fatal("expected error for nonexistent session")
	}
}

// --- CPI Driver Resolution Tests ---

func TestResolveDriver_ExplicitDriver(t *testing.T) {
	driver := &fakeDriver{name: "explicit", responses: []*cpi.CompletionResponse{{Content: "hi"}}}
	rt := New(Config{
		Driver:         driver,
		DriverModel:    "my-model",
		DriverEndpoint: "http://example.com",
		Credentials:    &cpi.Credentials{Type: "api-key", Data: map[string]string{"apiKey": "key"}},
		Agent:          newTestAgent("test"),
	})

	drv, model, endpoint, creds, params, err := rt.resolveDriver()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if drv != driver {
		t.Fatal("expected explicit driver to be returned")
	}
	if model != "my-model" {
		t.Errorf("model = %q, want %q", model, "my-model")
	}
	if endpoint != "http://example.com" {
		t.Errorf("endpoint = %q, want %q", endpoint, "http://example.com")
	}
	if creds == nil || creds.Data["apiKey"] != "key" {
		t.Errorf("credentials not returned correctly")
	}
	if params != nil {
		t.Errorf("expected nil params for explicit driver, got %v", params)
	}
}

func TestResolveDriver_RegistryResolution(t *testing.T) {
	driver := &fakeDriver{name: "openai", responses: []*cpi.CompletionResponse{{Content: "hi"}}}
	registry := cpi.NewRegistry()
	if err := registry.Register(driver); err != nil {
		t.Fatalf("failed to register driver: %v", err)
	}

	model := &v1alpha1.Model{
		Spec: v1alpha1.ModelSpec{
			ClassName: "openai-class",
			ModelID:   "gpt-4o",
			Endpoint:  "https://api.openai.com/v1",
		},
	}
	modelClass := &v1alpha1.ModelClass{
		Spec: v1alpha1.ModelClassSpec{
			Provisioner: "openai",
		},
	}

	rt := New(Config{
		Registry:    registry,
		Model:       model,
		ModelClass:  modelClass,
		Credentials: &cpi.Credentials{Type: "api-key", Data: map[string]string{"apiKey": "sk-test"}},
		Agent:       newTestAgent("test"),
	})

	drv, modelID, endpoint, creds, params, err := rt.resolveDriver()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if drv != driver {
		t.Fatal("expected registry driver to be returned")
	}
	if modelID != "gpt-4o" {
		t.Errorf("modelID = %q, want %q", modelID, "gpt-4o")
	}
	if endpoint != "https://api.openai.com/v1" {
		t.Errorf("endpoint = %q, want %q", endpoint, "https://api.openai.com/v1")
	}
	if creds == nil || creds.Data["apiKey"] != "sk-test" {
		t.Errorf("credentials not returned correctly")
	}
	if params != nil {
		t.Errorf("expected nil params (model has none), got %v", params)
	}
}

func TestResolveDriver_ProvisionerNotFound(t *testing.T) {
	registry := cpi.NewRegistry()
	model := &v1alpha1.Model{
		Spec: v1alpha1.ModelSpec{ModelID: "gpt-4o"},
	}
	modelClass := &v1alpha1.ModelClass{
		Spec: v1alpha1.ModelClassSpec{Provisioner: "nonexistent"},
	}

	rt := New(Config{
		Registry:   registry,
		Model:      model,
		ModelClass: modelClass,
		Agent:      newTestAgent("test"),
	})

	_, _, _, _, _, err := rt.resolveDriver()
	if err == nil {
		t.Fatal("expected error for missing provisioner")
	}
	if got := err.Error(); got != `no CPI driver registered for provisioner "nonexistent"` {
		t.Errorf("error = %q, want provisioner-not-found message", got)
	}
}

func TestResolveDriver_NoDriverOrRegistry(t *testing.T) {
	rt := New(Config{Agent: newTestAgent("test")})

	_, _, _, _, _, err := rt.resolveDriver()
	if err == nil {
		t.Fatal("expected error when no driver and no registry configured")
	}
}

func TestStep_RegistryBasedDriver(t *testing.T) {
	driver := &fakeDriver{
		name:      "openai",
		responses: []*cpi.CompletionResponse{{Content: "Hello from registry!"}},
	}
	registry := cpi.NewRegistry()
	if err := registry.Register(driver); err != nil {
		t.Fatalf("register: %v", err)
	}

	model := &v1alpha1.Model{
		Spec: v1alpha1.ModelSpec{
			ClassName: "openai-class",
			ModelID:   "gpt-4o",
			Endpoint:  "https://api.openai.com/v1",
		},
	}
	modelClass := &v1alpha1.ModelClass{
		Spec: v1alpha1.ModelClassSpec{Provisioner: "openai"},
	}

	rt := New(Config{
		Registry:   registry,
		Model:      model,
		ModelClass: modelClass,
		Agent:      newTestAgent("You are a helpful assistant."),
	})

	ctx := context.Background()
	sess, err := rt.CreateSession(ctx, runtime.CreateSessionRequest{})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	stream, err := rt.Step(ctx, runtime.StepRequest{SessionID: sess.SessionID, Input: "hi"})
	if err != nil {
		t.Fatalf("Step: %v", err)
	}

	events := drainEvents(t, stream, 5*time.Second)

	// Should get token + done events with the driver's response.
	tokenEvt := findEvent(events, runtime.EventToken)
	if tokenEvt == nil || tokenEvt.Token == nil || tokenEvt.Token.Content != "Hello from registry!" {
		t.Errorf("expected token event with 'Hello from registry!', got events: %v", events)
	}

	// Verify the driver received the correct model parameters.
	driver.mu.Lock()
	defer driver.mu.Unlock()
	if len(driver.requests) != 1 {
		t.Fatalf("expected 1 driver call, got %d", len(driver.requests))
	}
	req := driver.requests[0]
	if req.Model != "gpt-4o" {
		t.Errorf("request.Model = %q, want %q", req.Model, "gpt-4o")
	}
	if req.Endpoint != "https://api.openai.com/v1" {
		t.Errorf("request.Endpoint = %q, want %q", req.Endpoint, "https://api.openai.com/v1")
	}
}

func TestStep_RegistryBasedDriver_ParametersFlowThrough(t *testing.T) {
	driver := &fakeDriver{
		name:      "openai",
		responses: []*cpi.CompletionResponse{{Content: "warm response"}},
	}
	registry := cpi.NewRegistry()
	if err := registry.Register(driver); err != nil {
		t.Fatalf("register: %v", err)
	}

	model := &v1alpha1.Model{
		Spec: v1alpha1.ModelSpec{
			ClassName: "openai-class",
			ModelID:   "gpt-4o",
			Endpoint:  "https://api.openai.com/v1",
			Parameters: map[string]string{
				"temperature": "0.9",
				"max_tokens":  "2048",
			},
		},
	}
	modelClass := &v1alpha1.ModelClass{
		Spec: v1alpha1.ModelClassSpec{Provisioner: "openai"},
	}

	rt := New(Config{
		Registry:   registry,
		Model:      model,
		ModelClass: modelClass,
		Agent:      newTestAgent("You are a helpful assistant."),
	})

	ctx := context.Background()
	sess, err := rt.CreateSession(ctx, runtime.CreateSessionRequest{})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	stream, err := rt.Step(ctx, runtime.StepRequest{SessionID: sess.SessionID, Input: "hi"})
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	drainEvents(t, stream, 5*time.Second)

	// Verify that Model.Spec.Parameters were passed to the driver.
	driver.mu.Lock()
	defer driver.mu.Unlock()
	if len(driver.requests) != 1 {
		t.Fatalf("expected 1 driver call, got %d", len(driver.requests))
	}
	req := driver.requests[0]
	if req.Parameters == nil {
		t.Fatal("expected Parameters to be set on CompletionRequest")
	}
	if req.Parameters["temperature"] != "0.9" {
		t.Errorf("Parameters[temperature] = %q, want %q", req.Parameters["temperature"], "0.9")
	}
	if req.Parameters["max_tokens"] != "2048" {
		t.Errorf("Parameters[max_tokens] = %q, want %q", req.Parameters["max_tokens"], "2048")
	}
}

func TestResolveDriver_RegistryResolution_WithParameters(t *testing.T) {
	driver := &fakeDriver{name: "openai", responses: []*cpi.CompletionResponse{{Content: "hi"}}}
	registry := cpi.NewRegistry()
	if err := registry.Register(driver); err != nil {
		t.Fatalf("failed to register driver: %v", err)
	}

	model := &v1alpha1.Model{
		Spec: v1alpha1.ModelSpec{
			ClassName: "openai-class",
			ModelID:   "gpt-4o",
			Endpoint:  "https://api.openai.com/v1",
			Parameters: map[string]string{
				"temperature": "0.7",
				"top_p":       "0.95",
			},
		},
	}
	modelClass := &v1alpha1.ModelClass{
		Spec: v1alpha1.ModelClassSpec{Provisioner: "openai"},
	}

	rt := New(Config{
		Registry:    registry,
		Model:       model,
		ModelClass:  modelClass,
		Credentials: &cpi.Credentials{Type: "api-key", Data: map[string]string{"apiKey": "sk-test"}},
		Agent:       newTestAgent("test"),
	})

	_, _, _, _, params, err := rt.resolveDriver()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if params == nil {
		t.Fatal("expected non-nil params from model spec")
	}
	if params["temperature"] != "0.7" {
		t.Errorf("params[temperature] = %q, want %q", params["temperature"], "0.7")
	}
	if params["top_p"] != "0.95" {
		t.Errorf("params[top_p] = %q, want %q", params["top_p"], "0.95")
	}
}

func TestResolveDriver_RegistryResolution_MergesModelClassAndModelParams(t *testing.T) {
	driver := &fakeDriver{name: "openai", responses: []*cpi.CompletionResponse{{Content: "hi"}}}
	registry := cpi.NewRegistry()
	if err := registry.Register(driver); err != nil {
		t.Fatalf("failed to register driver: %v", err)
	}

	model := &v1alpha1.Model{
		Spec: v1alpha1.ModelSpec{
			ClassName: "openai-class",
			ModelID:   "gpt-4o",
			Endpoint:  "https://api.openai.com/v1",
			Parameters: map[string]string{
				"temperature": "0.9",  // overrides ModelClass value
				"max_tokens":  "4096", // model-only param
			},
		},
	}
	modelClass := &v1alpha1.ModelClass{
		Spec: v1alpha1.ModelClassSpec{
			Provisioner: "openai",
			Parameters: map[string]string{
				"api_version": "2024-02-15", // provisioner-only param
				"temperature": "0.5",        // will be overridden by model
			},
		},
	}

	rt := New(Config{
		Registry:    registry,
		Model:       model,
		ModelClass:  modelClass,
		Credentials: &cpi.Credentials{Type: "api-key", Data: map[string]string{"apiKey": "sk-test"}},
		Agent:       newTestAgent("test"),
	})

	_, _, _, _, params, err := rt.resolveDriver()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if params == nil {
		t.Fatal("expected non-nil merged params")
	}
	// ModelClass param preserved.
	if params["api_version"] != "2024-02-15" {
		t.Errorf("params[api_version] = %q, want %q", params["api_version"], "2024-02-15")
	}
	// Model param overrides ModelClass.
	if params["temperature"] != "0.9" {
		t.Errorf("params[temperature] = %q, want %q (model should override class)", params["temperature"], "0.9")
	}
	// Model-only param present.
	if params["max_tokens"] != "4096" {
		t.Errorf("params[max_tokens] = %q, want %q", params["max_tokens"], "4096")
	}
	// Total count: api_version + temperature + max_tokens = 3.
	if len(params) != 3 {
		t.Errorf("len(params) = %d, want 3", len(params))
	}
}

func TestResolveDriver_RegistryResolution_NilParamsBothEmpty(t *testing.T) {
	driver := &fakeDriver{name: "openai", responses: []*cpi.CompletionResponse{{Content: "hi"}}}
	registry := cpi.NewRegistry()
	if err := registry.Register(driver); err != nil {
		t.Fatalf("failed to register driver: %v", err)
	}

	model := &v1alpha1.Model{
		Spec: v1alpha1.ModelSpec{
			ClassName: "openai-class",
			ModelID:   "gpt-4o",
			Endpoint:  "https://api.openai.com/v1",
		},
	}
	modelClass := &v1alpha1.ModelClass{
		Spec: v1alpha1.ModelClassSpec{Provisioner: "openai"},
	}

	rt := New(Config{
		Registry:   registry,
		Model:      model,
		ModelClass: modelClass,
		Agent:      newTestAgent("test"),
	})

	_, _, _, _, params, err := rt.resolveDriver()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if params != nil {
		t.Errorf("expected nil params when both ModelClass and Model have no parameters, got %v", params)
	}
}

// TC-0197: Execute agent step with a granted Model; verify runtime resolves driver
// and calls Complete() with correct parameters (model, endpoint, credentials, messages).
func TestStep_GrantedModel_DriverCompleteParameters(t *testing.T) {
	driver := &fakeDriver{
		name: "foundry",
		responses: []*cpi.CompletionResponse{{
			Content:      "I can help with that.",
			FinishReason: "stop",
			Usage:        cpi.Usage{PromptTokens: 20, CompletionTokens: 8, TotalTokens: 28, Reported: true},
		}},
	}
	registry := cpi.NewRegistry()
	if err := registry.Register(driver); err != nil {
		t.Fatalf("register: %v", err)
	}

	model := &v1alpha1.Model{
		Spec: v1alpha1.ModelSpec{
			ClassName: "foundry-class",
			ModelID:   "gpt-4o-2024-08-06",
			Endpoint:  "https://myproject.services.ai.azure.com/models",
			Parameters: map[string]string{
				"temperature": "0.7",
				"max_tokens":  "4096",
			},
		},
	}
	modelClass := &v1alpha1.ModelClass{
		Spec: v1alpha1.ModelClassSpec{Provisioner: "foundry"},
	}
	creds := &cpi.Credentials{
		Type: "api-key",
		Data: map[string]string{"apiKey": "key-abc123"},
	}

	agent := newTestAgent("You are a coding assistant. Be concise.")

	rt := New(Config{
		Registry:    registry,
		Model:       model,
		ModelClass:  modelClass,
		Credentials: creds,
		Agent:       agent,
	})

	ctx := context.Background()
	sess, err := rt.CreateSession(ctx, runtime.CreateSessionRequest{})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	stream, err := rt.Step(ctx, runtime.StepRequest{
		SessionID: sess.SessionID,
		Input:     "Explain Go interfaces in one sentence.",
	})
	if err != nil {
		t.Fatalf("Step: %v", err)
	}

	events := drainEvents(t, stream, 5*time.Second)

	// Verify output event contains the driver's response.
	tokenEvt := findEvent(events, runtime.EventToken)
	if tokenEvt == nil || tokenEvt.Token == nil || tokenEvt.Token.Content != "I can help with that." {
		t.Errorf("expected token event with driver response, got events: %v", events)
	}

	// Verify the CompletionRequest received by the driver.
	driver.mu.Lock()
	defer driver.mu.Unlock()
	if len(driver.requests) != 1 {
		t.Fatalf("expected 1 driver call, got %d", len(driver.requests))
	}
	req := driver.requests[0]

	// Model and endpoint must match the granted Model spec.
	if req.Model != "gpt-4o-2024-08-06" {
		t.Errorf("request.Model = %q, want %q", req.Model, "gpt-4o-2024-08-06")
	}
	if req.Endpoint != "https://myproject.services.ai.azure.com/models" {
		t.Errorf("request.Endpoint = %q, want %q", req.Endpoint, "https://myproject.services.ai.azure.com/models")
	}

	// Credentials must be passed through.
	if req.Creds == nil {
		t.Fatal("expected credentials to be set on CompletionRequest")
	}
	if req.Creds.Type != "api-key" {
		t.Errorf("request.Creds.Type = %q, want %q", req.Creds.Type, "api-key")
	}
	if req.Creds.Data["apiKey"] != "key-abc123" {
		t.Errorf("request.Creds.Data[apiKey] = %q, want %q", req.Creds.Data["apiKey"], "key-abc123")
	}

	// Parameters must be passed through.
	if req.Parameters == nil {
		t.Fatal("expected Parameters to be set on CompletionRequest")
	}
	if req.Parameters["temperature"] != "0.7" {
		t.Errorf("request.Parameters[temperature] = %q, want %q", req.Parameters["temperature"], "0.7")
	}
	if req.Parameters["max_tokens"] != "4096" {
		t.Errorf("request.Parameters[max_tokens] = %q, want %q", req.Parameters["max_tokens"], "4096")
	}

	// Messages must include system prompt + user input.
	if len(req.Messages) < 2 {
		t.Fatalf("expected at least 2 messages (system + user), got %d", len(req.Messages))
	}
	if req.Messages[0].Role != "system" || req.Messages[0].Content != "You are a coding assistant. Be concise." {
		t.Errorf("messages[0] = %+v, want system prompt", req.Messages[0])
	}
	if req.Messages[1].Role != "user" || req.Messages[1].Content != "Explain Go interfaces in one sentence." {
		t.Errorf("messages[1] = %+v, want user input", req.Messages[1])
	}
}

// Verify the NativeRuntime implements the Runtime interface at compile time.
var _ runtime.Runtime = (*NativeRuntime)(nil)
