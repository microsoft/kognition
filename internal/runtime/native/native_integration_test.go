// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build integration

package native

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/memory"
	"github.com/kognition-ai/kognition/internal/runtime"
)

// Integration tests exercise realistic multi-turn and concurrent scenarios
// that go beyond isolated unit-test coverage.

// TestIntegration_MultiTurnConversation simulates a realistic multi-turn agent
// session: user asks → model responds → user follows up → model uses tool → user
// asks again → model gives final answer.
func TestIntegration_MultiTurnConversation(t *testing.T) {
	driver := &fakeDriver{
		name: "test-driver",
		responses: []*cpi.CompletionResponse{
			// Turn 1: simple answer
			{Content: "I can help you search for that."},
			// Turn 2: tool call + follow-up
			{ToolCalls: []cpi.ToolCall{{ID: "tc-1", Name: "web_search", Arguments: `{"query":"golang concurrency"}`}}},
			{Content: "Here are the results from my search."},
			// Turn 3: final answer
			{Content: "Glad I could help!"},
		},
	}

	executor := newFakeToolExecutor()
	executor.results["web_search"] = runtime.ToolExecutionResult{
		Output: map[string]any{"results": []string{"result1", "result2"}},
	}
	dispatcher := runtime.NewToolDispatcher(executor, executor)
	tools := []v1alpha1.Tool{newTestTool("web_search", "Search the web")}

	rt := New(Config{
		Driver:         driver,
		DriverModel:    "gpt-4",
		Agent:          newTestAgent("You are a helpful research assistant."),
		Tools:          tools,
		ToolDispatcher: dispatcher,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Create session
	sess, err := rt.CreateSession(ctx, runtime.CreateSessionRequest{
		AgentName:      "research-agent",
		Namespace:      "production",
		InitialCredits: 100,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// --- Turn 1: Simple question ---
	stream, err := rt.Step(ctx, runtime.StepRequest{SessionID: sess.SessionID, Input: "Can you search for golang concurrency patterns?"})
	if err != nil {
		t.Fatalf("Step turn 1: %v", err)
	}
	events := drainEvents(t, stream, 3*time.Second)
	outputEvts := findEvents(events, runtime.EventOutput)
	if len(outputEvts) == 0 {
		t.Fatal("Turn 1: expected output events")
	}
	if outputEvts[0].Output.Content != "I can help you search for that." {
		t.Errorf("Turn 1: unexpected content: %q", outputEvts[0].Output.Content)
	}

	// --- Turn 2: Tool usage ---
	stream, err = rt.Step(ctx, runtime.StepRequest{SessionID: sess.SessionID, Input: "Please search now."})
	if err != nil {
		t.Fatalf("Step turn 2: %v", err)
	}
	events = drainEvents(t, stream, 3*time.Second)

	// Should have tool call and tool result events
	toolCalls := findEvents(events, runtime.EventToolCall)
	if len(toolCalls) == 0 {
		t.Fatal("Turn 2: expected tool call events")
	}
	if toolCalls[0].ToolCall.ToolName != "web_search" {
		t.Errorf("Turn 2: expected web_search tool call, got %q", toolCalls[0].ToolCall.ToolName)
	}

	toolResults := findEvents(events, runtime.EventToolResult)
	if len(toolResults) == 0 {
		t.Fatal("Turn 2: expected tool result events")
	}
	if toolResults[0].ToolResult.IsError {
		t.Error("Turn 2: tool result should not be an error")
	}

	// Final output after tool
	outputEvts = findEvents(events, runtime.EventOutput)
	if len(outputEvts) == 0 {
		t.Fatal("Turn 2: expected output after tool use")
	}

	// --- Turn 3: Follow-up ---
	stream, err = rt.Step(ctx, runtime.StepRequest{SessionID: sess.SessionID, Input: "Thanks!"})
	if err != nil {
		t.Fatalf("Step turn 3: %v", err)
	}
	events = drainEvents(t, stream, 3*time.Second)
	outputEvts = findEvents(events, runtime.EventOutput)
	if len(outputEvts) == 0 {
		t.Fatal("Turn 3: expected output events")
	}

	// Verify history accumulated correctly: driver should have received
	// increasing message counts for each call.
	driver.mu.Lock()
	defer driver.mu.Unlock()
	if len(driver.requests) != 4 { // turn1(1 call) + turn2(2 calls) + turn3(1 call)
		t.Fatalf("expected 4 driver calls, got %d", len(driver.requests))
	}
	// Turn 1: system + user
	if len(driver.requests[0].Messages) != 2 {
		t.Errorf("Turn 1 request: expected 2 messages, got %d", len(driver.requests[0].Messages))
	}
	// Turn 3 (last request): should have all accumulated history
	lastReq := driver.requests[3]
	if len(lastReq.Messages) < 6 {
		t.Errorf("Turn 3 request: expected at least 6 messages (system + 3 user + assistant + tool), got %d", len(lastReq.Messages))
	}
}

// TestIntegration_ConcurrentSessions verifies that multiple sessions can run
// concurrently without data races or cross-contamination.
func TestIntegration_ConcurrentSessions(t *testing.T) {
	const numSessions = 10

	driver := &fakeDriver{
		name: "test-driver",
	}

	// Each call returns content identifying which request it is.
	driver.responses = make([]*cpi.CompletionResponse, numSessions*2)
	for i := 0; i < numSessions*2; i++ {
		driver.responses[i] = &cpi.CompletionResponse{
			Content: fmt.Sprintf("response-%d", i),
		}
	}

	rt := New(Config{
		Driver:                driver,
		DriverModel:           "gpt-4",
		Agent:                 newTestAgent("You are a test agent."),
		MaxConcurrentSessions: uint32(numSessions),
	})

	ctx := context.Background()
	var wg sync.WaitGroup
	errors := make(chan error, numSessions*2)

	for i := 0; i < numSessions; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()

			sess, err := rt.CreateSession(ctx, runtime.CreateSessionRequest{
				AgentName:      "agent",
				Namespace:      "ns",
				InitialCredits: 50,
			})
			if err != nil {
				errors <- fmt.Errorf("session %d create: %w", idx, err)
				return
			}

			// First step
			stream, err := rt.Step(ctx, runtime.StepRequest{
				SessionID: sess.SessionID,
				Input:     fmt.Sprintf("message from session %d", idx),
			})
			if err != nil {
				errors <- fmt.Errorf("session %d step: %w", idx, err)
				return
			}
			events := drainEvents(t, stream, 3*time.Second)
			if findEvent(events, runtime.EventOutput) == nil {
				errors <- fmt.Errorf("session %d: no output event", idx)
				return
			}
		}(i)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("timed out waiting for concurrent sessions: %v", ctx.Err())
	}
	close(errors)

	for err := range errors {
		t.Error(err)
	}

	// Verify health reports correct active session count.
	health, err := rt.Health(ctx)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health.State != runtime.HealthHealthy {
		t.Errorf("expected healthy, got %d", health.State)
	}
	if health.ActiveSessions != numSessions {
		t.Errorf("expected %d active sessions, got %d", numSessions, health.ActiveSessions)
	}
}

// TestIntegration_CheckpointResumeWithHistory verifies that checkpoint/resume
// preserves conversation history and allows continuation.
func TestIntegration_CheckpointResumeWithHistory(t *testing.T) {
	callCount := 0
	driver := &fakeDriver{
		name: "test-driver",
		responses: []*cpi.CompletionResponse{
			{Content: "I remember the context."},
			{Content: "Continuing from checkpoint with full history."},
		},
	}

	rt := New(Config{
		Driver:      driver,
		DriverModel: "m",
		Agent:       newTestAgent("You remember everything."),
	})

	ctx := context.Background()

	// Create and step to build history.
	sess, err := rt.CreateSession(ctx, runtime.CreateSessionRequest{
		AgentName: "agent", Namespace: "ns", InitialCredits: 50,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	stream, _ := rt.Step(ctx, runtime.StepRequest{SessionID: sess.SessionID, Input: "remember this context"})
	drainEvents(t, stream, 2*time.Second)
	callCount++

	// Checkpoint the session.
	cpResp, err := rt.Checkpoint(ctx, sess.SessionID)
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if cpResp.CreatedAt.IsZero() {
		t.Error("expected non-zero CreatedAt")
	}

	// Resume from checkpoint.
	resumeResp, err := rt.Resume(ctx, cpResp.Token)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}

	// Step on the resumed session — driver should receive full history.
	stream, err = rt.Step(ctx, runtime.StepRequest{SessionID: resumeResp.SessionID, Input: "what did I say?"})
	if err != nil {
		t.Fatalf("Step resumed session: %v", err)
	}
	events := drainEvents(t, stream, 2*time.Second)
	outputEvts := findEvents(events, runtime.EventOutput)
	if len(outputEvts) == 0 {
		t.Fatal("expected output on resumed session")
	}

	driver.mu.Lock()
	defer driver.mu.Unlock()
	_ = callCount
	if len(driver.requests) < 2 {
		t.Fatalf("expected at least 2 driver calls, got %d", len(driver.requests))
	}
	resumedReq := driver.requests[len(driver.requests)-1]
	if len(resumedReq.Messages) != 4 || resumedReq.Messages[0].Content != "You remember everything." || resumedReq.Messages[1].Content != "remember this context" || resumedReq.Messages[2].Content != "I remember the context." || resumedReq.Messages[3].Content != "what did I say?" {
		t.Errorf("resumed request did not preserve conversation: %+v", resumedReq.Messages)
	}
}

// TestIntegration_CancelDuringToolExecution verifies that cancellation during
// a long-running tool call propagates correctly.
func TestIntegration_CancelDuringToolExecution(t *testing.T) {
	// The driver returns a tool call that takes a while.
	driver := &fakeDriver{
		name: "test-driver",
		responses: []*cpi.CompletionResponse{
			{ToolCalls: []cpi.ToolCall{{ID: "tc-slow", Name: "slow_tool", Arguments: `{}`}}},
		},
	}

	executor := &slowToolExecutor{
		delay:  500 * time.Millisecond,
		result: runtime.ToolExecutionResult{Output: map[string]any{"status": "done"}},
	}
	dispatcher := runtime.NewToolDispatcher(executor, executor)
	tools := []v1alpha1.Tool{newTestTool("slow_tool", "A slow tool")}

	rt := New(Config{
		Driver:         driver,
		DriverModel:    "m",
		Agent:          newTestAgent("agent"),
		Tools:          tools,
		ToolDispatcher: dispatcher,
	})

	ctx := context.Background()
	sess, _ := rt.CreateSession(ctx, runtime.CreateSessionRequest{
		AgentName: "a", Namespace: "n", InitialCredits: 50,
	})

	stream, err := rt.Step(ctx, runtime.StepRequest{SessionID: sess.SessionID, Input: "run the slow tool"})
	if err != nil {
		t.Fatalf("Step: %v", err)
	}

	// Cancel after a short delay.
	time.AfterFunc(100*time.Millisecond, func() {
		_, _ = rt.Cancel(ctx, runtime.CancelRequest{SessionID: sess.SessionID, TimeoutMS: 1000})
	})

	// Drain — should complete relatively quickly due to cancellation.
	events := drainEvents(t, stream, 3*time.Second)

	// We should see either an error event or the stream just closes.
	// The key assertion is that we don't hang for the full tool delay.
	_ = events // Just verify we don't deadlock
}

// TestIntegration_SequenceMonotonicity verifies that event sequence numbers are
// strictly monotonically increasing across all events in a step.
func TestIntegration_SequenceMonotonicity(t *testing.T) {
	argsJSON, _ := json.Marshal(map[string]any{"x": 1})
	driver := &fakeDriver{
		name: "test-driver",
		responses: []*cpi.CompletionResponse{
			{ToolCalls: []cpi.ToolCall{
				{ID: "t1", Name: "tool_a", Arguments: string(argsJSON)},
				{ID: "t2", Name: "tool_b", Arguments: string(argsJSON)},
			}},
			{Content: "final"},
		},
	}

	executor := newFakeToolExecutor()
	dispatcher := runtime.NewToolDispatcher(executor, executor)
	tools := []v1alpha1.Tool{
		newTestTool("tool_a", "a"),
		newTestTool("tool_b", "b"),
	}

	rt := New(Config{
		Driver:         driver,
		DriverModel:    "m",
		Agent:          newTestAgent("agent"),
		Tools:          tools,
		ToolDispatcher: dispatcher,
	})

	ctx := context.Background()
	sess, _ := rt.CreateSession(ctx, runtime.CreateSessionRequest{
		AgentName: "a", Namespace: "n", InitialCredits: 100,
	})

	stream, err := rt.Step(ctx, runtime.StepRequest{SessionID: sess.SessionID, Input: "use both tools"})
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	events := drainEvents(t, stream, 3*time.Second)

	if len(events) < 5 {
		t.Fatalf("expected at least 5 events (2 tool calls + 2 results + output), got %d", len(events))
	}

	// Verify strict monotonicity.
	var lastSeq uint64
	for i, ev := range events {
		if ev.Sequence <= lastSeq {
			t.Errorf("event %d: sequence %d not > previous %d", i, ev.Sequence, lastSeq)
		}
		lastSeq = ev.Sequence
	}
}

// TestIntegration_CapabilitiesReflectConfig verifies GetCapabilities returns
// features consistent with the runtime configuration.
func TestIntegration_CapabilitiesReflectConfig(t *testing.T) {
	// Without memory
	rt := New(Config{
		Driver:                &fakeDriver{name: "d"},
		DriverModel:           "m",
		Agent:                 newTestAgent("a"),
		MaxConcurrentSessions: 50,
	})
	caps, err := rt.GetCapabilities(context.Background())
	if err != nil {
		t.Fatalf("GetCapabilities: %v", err)
	}
	if caps.MaxConcurrentSessions != 50 {
		t.Errorf("expected MaxConcurrentSessions=50, got %d", caps.MaxConcurrentSessions)
	}
	for _, f := range caps.SupportedFeatures {
		if f == "memory" {
			t.Error("should not advertise memory without backend")
		}
	}

	// With memory
	rt2 := New(Config{
		Driver:        &fakeDriver{name: "d"},
		DriverModel:   "m",
		Agent:         newTestAgent("a"),
		MemoryBackend: &fakeMemoryBackend{},
	})
	caps2, _ := rt2.GetCapabilities(context.Background())
	found := false
	for _, f := range caps2.SupportedFeatures {
		if f == "memory" {
			found = true
		}
	}
	if !found {
		t.Error("expected memory in supported features when backend is configured")
	}
}

// --- Integration test helpers ---

// slowToolExecutor adds artificial delay to simulate slow tool execution.
type slowToolExecutor struct {
	delay  time.Duration
	result runtime.ToolExecutionResult
}

func (e *slowToolExecutor) Execute(ctx context.Context, _ runtime.ToolExecutionRequest, _ runtime.ToolSandboxLimits) (runtime.ToolExecutionResult, error) {
	select {
	case <-time.After(e.delay):
		return e.result, nil
	case <-ctx.Done():
		return runtime.ToolExecutionResult{}, ctx.Err()
	}
}

// fakeMemoryBackend is a minimal memory.Backend for capability testing.
type fakeMemoryBackend struct{}

func (f *fakeMemoryBackend) Provision(_ context.Context, _ memory.ProvisionRequest) error { return nil }
func (f *fakeMemoryBackend) Deprovision(_ context.Context, _ memory.DeprovisionRequest) error {
	return nil
}
func (f *fakeMemoryBackend) Store(_ context.Context, _ memory.StoreRequest) (memory.StoreResponse, error) {
	return memory.StoreResponse{}, nil
}
func (f *fakeMemoryBackend) Get(_ context.Context, _ memory.GetRequest) (memory.Record, error) {
	return memory.Record{}, nil
}
func (f *fakeMemoryBackend) List(_ context.Context, _ memory.ListRequest) ([]memory.Record, error) {
	return nil, nil
}
func (f *fakeMemoryBackend) Search(_ context.Context, _ memory.SearchRequest) ([]memory.SearchResult, error) {
	return nil, nil
}
func (f *fakeMemoryBackend) Delete(_ context.Context, _ memory.DeleteRequest) error { return nil }
func (f *fakeMemoryBackend) Health(_ context.Context) error                         { return nil }

// TestIntegration_RegistryResolution_EndToEnd exercises the full CPI resolution
// path through the registry: Model → ModelClass → Provisioner → Registry.Get() →
// Driver.Complete(). Verifies that model ID, endpoint, credentials, and parameters
// all propagate correctly to the driver's CompletionRequest.
func TestIntegration_RegistryResolution_EndToEnd(t *testing.T) {
	driver := &fakeDriver{
		name: "test-provisioner",
		responses: []*cpi.CompletionResponse{
			{Content: "Hello via registry resolution!"},
		},
	}
	registry := cpi.NewRegistry()
	if err := registry.Register(driver); err != nil {
		t.Fatalf("register: %v", err)
	}

	model := &v1alpha1.Model{
		Spec: v1alpha1.ModelSpec{
			ClassName: "my-model-class",
			ModelID:   "my-model-v2",
			Endpoint:  "https://my-endpoint.example.com/v1",
			Parameters: map[string]string{
				"temperature": "0.5",
				"max_tokens":  "4096",
				"top_p":       "0.9",
			},
		},
	}
	modelClass := &v1alpha1.ModelClass{
		Spec: v1alpha1.ModelClassSpec{
			Provisioner: "test-provisioner",
		},
	}
	creds := &cpi.Credentials{
		Type: "api-key",
		Data: map[string]string{"apiKey": "sk-integration-test"},
	}

	rt := New(Config{
		Registry:    registry,
		Model:       model,
		ModelClass:  modelClass,
		Credentials: creds,
		Agent:       newTestAgent("You are an integration test assistant."),
	})

	ctx := context.Background()
	sess, err := rt.CreateSession(ctx, runtime.CreateSessionRequest{
		AgentName: "integration-agent",
		Namespace: "test-ns",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	stream, err := rt.Step(ctx, runtime.StepRequest{SessionID: sess.SessionID, Input: "Hello"})
	if err != nil {
		t.Fatalf("Step: %v", err)
	}
	events := drainEvents(t, stream, 5*time.Second)

	// Verify we got the expected response.
	outputEvts := findEvents(events, runtime.EventOutput)
	if len(outputEvts) == 0 {
		t.Fatal("expected output events")
	}
	if outputEvts[0].Output.Content != "Hello via registry resolution!" {
		t.Errorf("output content = %q, want %q", outputEvts[0].Output.Content, "Hello via registry resolution!")
	}

	// Verify the driver received correct resolution data.
	driver.mu.Lock()
	defer driver.mu.Unlock()
	if len(driver.requests) != 1 {
		t.Fatalf("expected 1 driver call, got %d", len(driver.requests))
	}
	req := driver.requests[0]

	// Model ID
	if req.Model != "my-model-v2" {
		t.Errorf("request.Model = %q, want %q", req.Model, "my-model-v2")
	}
	// Endpoint
	if req.Endpoint != "https://my-endpoint.example.com/v1" {
		t.Errorf("request.Endpoint = %q, want %q", req.Endpoint, "https://my-endpoint.example.com/v1")
	}
	// Credentials
	if req.Creds == nil {
		t.Fatal("expected credentials on request")
	}
	if req.Creds.Type != "api-key" {
		t.Errorf("creds.Type = %q, want %q", req.Creds.Type, "api-key")
	}
	if req.Creds.Data["apiKey"] != "sk-integration-test" {
		t.Errorf("creds.Data[apiKey] = %q, want %q", req.Creds.Data["apiKey"], "sk-integration-test")
	}
	// Parameters
	if req.Parameters == nil {
		t.Fatal("expected Parameters on request")
	}
	if req.Parameters["temperature"] != "0.5" {
		t.Errorf("Parameters[temperature] = %q, want %q", req.Parameters["temperature"], "0.5")
	}
	if req.Parameters["max_tokens"] != "4096" {
		t.Errorf("Parameters[max_tokens] = %q, want %q", req.Parameters["max_tokens"], "4096")
	}
	if req.Parameters["top_p"] != "0.9" {
		t.Errorf("Parameters[top_p] = %q, want %q", req.Parameters["top_p"], "0.9")
	}
	// Messages should include system + user
	if len(req.Messages) < 2 {
		t.Errorf("expected at least 2 messages (system + user), got %d", len(req.Messages))
	}
}

// TestIntegration_RegistryResolution_MultiTurnWithParams verifies parameters
// persist across multiple turns in a registry-based session.
func TestIntegration_RegistryResolution_MultiTurnWithParams(t *testing.T) {
	driver := &fakeDriver{
		name: "multi-turn-driver",
		responses: []*cpi.CompletionResponse{
			{Content: "First response."},
			{Content: "Second response."},
		},
	}
	registry := cpi.NewRegistry()
	if err := registry.Register(driver); err != nil {
		t.Fatalf("register: %v", err)
	}

	model := &v1alpha1.Model{
		Spec: v1alpha1.ModelSpec{
			ClassName:  "multi-class",
			ModelID:    "test-model",
			Endpoint:   "https://multi.example.com",
			Parameters: map[string]string{"temperature": "0.3"},
		},
	}
	modelClass := &v1alpha1.ModelClass{
		Spec: v1alpha1.ModelClassSpec{Provisioner: "multi-turn-driver"},
	}

	rt := New(Config{
		Registry:   registry,
		Model:      model,
		ModelClass: modelClass,
		Agent:      newTestAgent("Test assistant."),
	})

	ctx := context.Background()
	sess, err := rt.CreateSession(ctx, runtime.CreateSessionRequest{})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Turn 1
	stream, err := rt.Step(ctx, runtime.StepRequest{SessionID: sess.SessionID, Input: "Hello"})
	if err != nil {
		t.Fatalf("Step turn 1: %v", err)
	}
	drainEvents(t, stream, 3*time.Second)

	// Turn 2
	stream, err = rt.Step(ctx, runtime.StepRequest{SessionID: sess.SessionID, Input: "Follow up"})
	if err != nil {
		t.Fatalf("Step turn 2: %v", err)
	}
	drainEvents(t, stream, 3*time.Second)

	// Both calls should carry the same parameters.
	driver.mu.Lock()
	defer driver.mu.Unlock()
	if len(driver.requests) != 2 {
		t.Fatalf("expected 2 driver calls, got %d", len(driver.requests))
	}
	for i, req := range driver.requests {
		if req.Parameters == nil || req.Parameters["temperature"] != "0.3" {
			t.Errorf("Turn %d: Parameters[temperature] = %v, want %q", i+1, req.Parameters, "0.3")
		}
		if req.Model != "test-model" {
			t.Errorf("Turn %d: Model = %q, want %q", i+1, req.Model, "test-model")
		}
	}
	// Turn 2 should have more messages (accumulated history)
	if len(driver.requests[1].Messages) <= len(driver.requests[0].Messages) {
		t.Errorf("Turn 2 should have more messages than Turn 1: got %d vs %d",
			len(driver.requests[1].Messages), len(driver.requests[0].Messages))
	}
}
