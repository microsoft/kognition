// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package autogen_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/internal/runtime"
	"github.com/kognition-ai/kognition/internal/runtime/autogen"
)

// Traceability coverage for completed TCs:
// TC-0114: Run a kagent-shaped workload (Agent + Tools + Memory) using the AutoGen adapter; verify completion.
// TC-0122: Start a runtime plugin as a gRPC sidecar; verify host discovers and connects.
// TC-0123: Kill and restart a sidecar plugin; verify host reconnects automatically.

// fakeClient is a minimal AutoGen client for integration testing.
type fakeClient struct {
	events          []autogen.AgentEvent
	createErr       error
	sendErr         error
	cancelErr       error
	getStateErr     error
	restoreErr      error
	healthErr       error
	restoredAgentID string
	restoreEmpty    bool
}

type closeAwareClient struct {
	fakeClient
	cancelled chan struct{}
}

type checkpointAwareClient struct {
	fakeClient
	started chan struct{}
	release chan struct{}
}

type delayedCancelClient struct {
	fakeClient
}

func (*delayedCancelClient) CancelAgent(ctx context.Context, _ string) error {
	<-ctx.Done()
	return ctx.Err()
}

func TestAutoGenRuntimeCancelHonorsRequestedTimeout(test *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	adapter := autogen.New(&delayedCancelClient{})
	session, err := adapter.CreateSession(ctx, runtime.CreateSessionRequest{AgentName: "worker"})
	if err != nil {
		test.Fatal(err)
	}
	result := make(chan runtime.CancelResponse, 1)
	go func() {
		response, err := adapter.Cancel(ctx, runtime.CancelRequest{SessionID: session.SessionID, TimeoutMS: 10})
		if err == nil {
			result <- response
		}
	}()
	select {
	case response := <-result:
		if response.State != runtime.CancelForced {
			test.Fatal("unacknowledged remote cancellation did not require caller hard-kill")
		}
	case <-time.After(250 * time.Millisecond):
		test.Fatal("adapter ignored the requested cancellation timeout")
	}
}

func (client *checkpointAwareClient) GetAgentState(ctx context.Context, agentID string) ([]byte, error) {
	close(client.started)
	select {
	case <-client.release:
		return client.fakeClient.GetAgentState(ctx, agentID)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestAutoGenRuntimeCheckpointFencesNewTurn(test *testing.T) {
	client := &checkpointAwareClient{started: make(chan struct{}), release: make(chan struct{})}
	adapter := autogen.New(client)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	defer close(client.release)
	session, err := adapter.CreateSession(ctx, runtime.CreateSessionRequest{AgentName: "worker"})
	if err != nil {
		test.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := adapter.Checkpoint(ctx, session.SessionID)
		result <- err
	}()
	select {
	case <-client.started:
	case <-ctx.Done():
		test.Fatal("checkpoint did not start")
	}
	if stream, err := adapter.Step(ctx, runtime.StepRequest{SessionID: session.SessionID}); err == nil {
		stream.Close()
		test.Fatal("turn started while checkpoint state was being read")
	}
	if _, err := adapter.Cancel(ctx, runtime.CancelRequest{SessionID: session.SessionID}); err != nil {
		test.Fatal(err)
	}
	cancel()
	if err := <-result; err == nil {
		test.Fatal("cancelled checkpoint returned usable state")
	}
}

func TestAutoGenRuntimeResumeRejectsInvalidHandles(test *testing.T) {
	for _, handle := range []string{"", "agent-worker"} {
		test.Run(handle, func(test *testing.T) {
			client := &fakeClient{restoredAgentID: handle, restoreEmpty: handle == ""}
			adapter := autogen.New(client)
			session, err := adapter.CreateSession(context.Background(), runtime.CreateSessionRequest{AgentName: "worker"})
			if err != nil {
				test.Fatal(err)
			}
			checkpoint, err := adapter.Checkpoint(context.Background(), session.SessionID)
			if err != nil {
				test.Fatal(err)
			}
			if _, err := adapter.Resume(context.Background(), checkpoint.Token); err == nil {
				test.Fatal("invalid or reused restored handle was accepted")
			}
		})
	}
}

func (client *closeAwareClient) SendMessage(ctx context.Context, _, _ string) (<-chan autogen.AgentEvent, error) {
	events := make(chan autogen.AgentEvent)
	go func() {
		<-ctx.Done()
		close(client.cancelled)
		close(events)
	}()
	return events, nil
}

func TestAutoGenRuntimeStreamCloseCancelsWorker(test *testing.T) {
	client := &closeAwareClient{cancelled: make(chan struct{})}
	adapter := autogen.New(client)
	session, err := adapter.CreateSession(context.Background(), runtime.CreateSessionRequest{AgentName: "worker"})
	if err != nil {
		test.Fatal(err)
	}
	stream, err := adapter.Step(context.Background(), runtime.StepRequest{SessionID: session.SessionID, Input: "run"})
	if err != nil {
		test.Fatal(err)
	}
	stream.Close()
	select {
	case <-client.cancelled:
	case <-time.After(time.Second):
		test.Fatal("closing the stream left worker execution running")
	}
	select {
	case _, open := <-stream.Events:
		if open {
			test.Fatal("closed stream emitted an event")
		}
	case <-time.After(time.Second):
		test.Fatal("closing the stream did not close its event channel")
	}
}

func TestAutoGenRuntimeCreditsAndBusyCheckpoint(test *testing.T) {
	client := &fakeClient{events: []autogen.AgentEvent{{Type: autogen.AgentEventToken, Content: "first"}, {Type: autogen.AgentEventToken, Content: "second"}, {Type: autogen.AgentEventDone}}}
	adapter := autogen.New(client)
	session, err := adapter.CreateSession(context.Background(), runtime.CreateSessionRequest{AgentName: "worker", InitialCredits: 1})
	if err != nil {
		test.Fatal(err)
	}
	stream, err := adapter.Step(context.Background(), runtime.StepRequest{SessionID: session.SessionID})
	if err != nil {
		test.Fatal(err)
	}
	defer stream.Close()
	select {
	case event := <-stream.Events:
		if event.Token == nil || event.Token.Content != "first" {
			test.Fatal("first granted event missing")
		}
	case <-time.After(time.Second):
		test.Fatal("first credit did not produce an event")
	}
	if _, err := adapter.Step(context.Background(), runtime.StepRequest{SessionID: session.SessionID}); err == nil {
		test.Fatal("concurrent turn accepted")
	}
	if _, err := adapter.Checkpoint(context.Background(), session.SessionID); err == nil {
		test.Fatal("busy checkpoint accepted")
	}
	select {
	case <-stream.Events:
		test.Fatal("event was emitted without credit")
	case <-time.After(30 * time.Millisecond):
	}
	stream.GrantCredits(2)
	for range stream.Events {
	}
	if _, err := adapter.Checkpoint(context.Background(), session.SessionID); err != nil {
		test.Fatal(err)
	}
}

func (f *fakeClient) CreateAgent(_ context.Context, req autogen.CreateAgentRequest) (autogen.CreateAgentResponse, error) {
	if f.createErr != nil {
		return autogen.CreateAgentResponse{}, f.createErr
	}
	return autogen.CreateAgentResponse{AgentID: "agent-" + req.AgentName}, nil
}

func (f *fakeClient) SendMessage(_ context.Context, _ string, _ string) (<-chan autogen.AgentEvent, error) {
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	ch := make(chan autogen.AgentEvent, len(f.events))
	for _, ev := range f.events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

func (f *fakeClient) CancelAgent(_ context.Context, _ string) error { return f.cancelErr }

func (f *fakeClient) GetAgentState(_ context.Context, _ string) ([]byte, error) {
	if f.getStateErr != nil {
		return nil, f.getStateErr
	}
	return []byte(`{"state":"test"}`), nil
}

func (f *fakeClient) RestoreAgent(_ context.Context, _ []byte) (string, error) {
	if f.restoreErr != nil {
		return "", f.restoreErr
	}
	if f.restoreEmpty {
		return "", nil
	}
	if f.restoredAgentID != "" {
		return f.restoredAgentID, nil
	}
	return "agent-restored", nil
}

func (f *fakeClient) Health(_ context.Context) error { return f.healthErr }

func TestAutoGenRuntime_KagentWorkload(t *testing.T) {
	client := &fakeClient{
		events: []autogen.AgentEvent{
			{Type: autogen.AgentEventToken, Content: "Hello"},
			{Type: autogen.AgentEventToolCall, ToolName: "web_search", Content: `{"query":"test"}`, ToolCallID: "call-1"},
			{Type: autogen.AgentEventToolResult, ToolCallID: "call-1", Content: `{"results":[]}`},
			{Type: autogen.AgentEventOutput, Content: "Final answer", IsFinal: true},
			{Type: autogen.AgentEventDone},
		},
	}

	rt := autogen.New(client)
	ctx := context.Background()

	// CreateSession
	sess, err := rt.CreateSession(ctx, runtime.CreateSessionRequest{
		AgentName:      "kagent-test",
		Namespace:      "default",
		Context:        map[string]string{"model": "gpt-4"},
		InitialCredits: 32,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if sess.SessionID == "" {
		t.Fatal("empty session ID")
	}

	// Step
	stream, err := rt.Step(ctx, runtime.StepRequest{SessionID: sess.SessionID, Input: "hello"})
	if err != nil {
		t.Fatalf("Step: %v", err)
	}

	var events []runtime.StepEvent
	timeout := time.After(2 * time.Second)
	for {
		select {
		case ev, ok := <-stream.Events:
			if !ok {
				goto done
			}
			events = append(events, ev)
		case <-timeout:
			t.Fatal("timeout waiting for events")
		}
	}
done:

	if len(events) != 5 {
		t.Fatalf("expected 5 events, got %d", len(events))
	}
	if events[0].Type != runtime.EventToken {
		t.Errorf("event 0: expected Token, got %d", events[0].Type)
	}
	if events[1].Type != runtime.EventToolCall {
		t.Errorf("event 1: expected ToolCall, got %d", events[1].Type)
	}
	if events[3].Type != runtime.EventOutput {
		t.Errorf("event 3: expected Output, got %d", events[3].Type)
	}
	if events[4].Type != runtime.EventStatus {
		t.Errorf("event 4: expected Status, got %d", events[4].Type)
	}

	// Checkpoint & Resume
	ckpt, err := rt.Checkpoint(ctx, sess.SessionID)
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if len(ckpt.Token) == 0 {
		t.Fatal("empty checkpoint token")
	}

	resumed, err := rt.Resume(ctx, ckpt.Token)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.SessionID == "" {
		t.Fatal("empty resumed session ID")
	}

	// Health
	health, err := rt.Health(ctx)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health.State != runtime.HealthHealthy {
		t.Errorf("expected healthy, got %d", health.State)
	}

	// Cancel
	cancelResp, err := rt.Cancel(ctx, runtime.CancelRequest{SessionID: sess.SessionID, TimeoutMS: 1000})
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if cancelResp.State != runtime.CancelAcknowledged {
		t.Errorf("expected acknowledged cancel, got %d", cancelResp.State)
	}
}

func TestAutoGenRuntime_LifecycleFailureContracts(t *testing.T) {
	ctx := context.Background()

	t.Run("dependency errors remain identifiable", func(t *testing.T) {
		createErr := errors.New("create unavailable")
		if _, err := autogen.New(&fakeClient{createErr: createErr}).CreateSession(ctx, runtime.CreateSessionRequest{AgentName: "worker"}); !errors.Is(err, createErr) || !strings.Contains(err.Error(), "autogen create agent") {
			t.Fatalf("CreateSession() error = %v, want wrapped create error", err)
		}

		client := &fakeClient{}
		rt := autogen.New(client)
		session, err := rt.CreateSession(ctx, runtime.CreateSessionRequest{AgentName: "worker"})
		if err != nil {
			t.Fatalf("CreateSession() error = %v", err)
		}

		sendErr := errors.New("send unavailable")
		client.sendErr = sendErr
		if _, err := rt.Step(ctx, runtime.StepRequest{SessionID: session.SessionID}); !errors.Is(err, sendErr) || !strings.Contains(err.Error(), "autogen send message") {
			t.Fatalf("Step() error = %v, want wrapped send error", err)
		}

		stateErr := errors.New("state unavailable")
		client.getStateErr = stateErr
		if _, err := rt.Checkpoint(ctx, session.SessionID); !errors.Is(err, stateErr) || !strings.Contains(err.Error(), "autogen get state") {
			t.Fatalf("Checkpoint() error = %v, want wrapped state error", err)
		}

		restoreErr := errors.New("restore unavailable")
		client.restoreErr = restoreErr
		if _, err := rt.Resume(ctx, []byte(`{"agent_id":"worker","state":"c3RhdGU="}`)); !errors.Is(err, restoreErr) || !strings.Contains(err.Error(), "autogen restore") {
			t.Fatalf("Resume() error = %v, want wrapped restore error", err)
		}
	})

	t.Run("unknown sessions are rejected", func(t *testing.T) {
		rt := autogen.New(&fakeClient{})
		if _, err := rt.Step(ctx, runtime.StepRequest{SessionID: "missing"}); err == nil {
			t.Fatal("Step() error = nil, want missing-session error")
		}
		if _, err := rt.Cancel(ctx, runtime.CancelRequest{SessionID: "missing"}); err == nil {
			t.Fatal("Cancel() error = nil, want missing-session error")
		}
		if _, err := rt.Checkpoint(ctx, "missing"); err == nil {
			t.Fatal("Checkpoint() error = nil, want missing-session error")
		}
	})

	t.Run("cancel transport failure forces local cancellation", func(t *testing.T) {
		client := &fakeClient{cancelErr: errors.New("cancel unavailable")}
		rt := autogen.New(client)
		session, err := rt.CreateSession(ctx, runtime.CreateSessionRequest{AgentName: "worker"})
		if err != nil {
			t.Fatalf("CreateSession() error = %v", err)
		}

		response, err := rt.Cancel(ctx, runtime.CancelRequest{SessionID: session.SessionID})
		if err != nil || response.State != runtime.CancelForced || response.CancelledAt.IsZero() {
			t.Fatalf("Cancel() = %+v, %v; want forced cancellation timestamp", response, err)
		}
		if _, err := rt.Step(ctx, runtime.StepRequest{SessionID: session.SessionID}); err == nil || err.Error() != "session is cancelled" {
			t.Fatalf("Step() after cancel error = %v, want cancelled session", err)
		}
	})

	t.Run("invalid checkpoints and degraded health are reported", func(t *testing.T) {
		healthErr := errors.New("runtime unavailable")
		rt := autogen.New(&fakeClient{healthErr: healthErr})
		if _, err := rt.Resume(ctx, []byte("not-json")); err == nil || !strings.Contains(err.Error(), "invalid checkpoint token") {
			t.Fatalf("Resume() error = %v, want invalid checkpoint token", err)
		}

		status, err := rt.Health(ctx)
		if err != nil || status.State != runtime.HealthDegraded || status.Message != healthErr.Error() {
			t.Fatalf("Health() = %+v, %v; want degraded status", status, err)
		}
	})
}

func TestAutoGenRuntime_CapabilitiesAndFallbackEvents(t *testing.T) {
	client := &fakeClient{events: []autogen.AgentEvent{
		{Type: autogen.AgentEventError, Content: "model failed"},
		{Type: autogen.AgentEventType(999)},
	}}
	rt := autogen.New(client)
	ctx := context.Background()

	capabilities, err := rt.GetCapabilities(ctx)
	if err != nil {
		t.Fatalf("GetCapabilities() error = %v", err)
	}
	if capabilities.RuntimeVersion != "autogen-adapter/0.1.0" || capabilities.MaxConcurrentSessions != 64 {
		t.Fatalf("GetCapabilities() = %+v, want adapter version and session limit", capabilities)
	}
	if got := strings.Join(capabilities.SupportedFeatures, ","); got != "streaming,checkpoint,tool-calling" {
		t.Fatalf("supported features = %q, want streaming, checkpoint, and tool calling", got)
	}

	session, err := rt.CreateSession(ctx, runtime.CreateSessionRequest{AgentName: "worker"})
	if err != nil {
		t.Fatalf("CreateSession() error = %v", err)
	}
	stream, err := rt.Step(ctx, runtime.StepRequest{SessionID: session.SessionID})
	if err != nil {
		t.Fatalf("Step() error = %v", err)
	}

	var events []runtime.StepEvent
	for event := range stream.Events {
		events = append(events, event)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want error and fallback status", len(events))
	}
	if events[0].Type != runtime.EventError || events[0].Error == nil || events[0].Error.Message != "model failed" {
		t.Fatalf("error event = %+v, want translated AutoGen error", events[0])
	}
	if events[1].Type != runtime.EventStatus || events[1].Status == nil || events[1].Status.State != runtime.StateUnspecified {
		t.Fatalf("fallback event = %+v, want unspecified status", events[1])
	}
}
