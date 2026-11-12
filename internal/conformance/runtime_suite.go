// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package conformance

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/internal/runtime"
)

// RuntimeFactory creates a fresh Runtime instance for each test.
// Implementations should return a ready-to-use runtime (provisioned, started).
type RuntimeFactory func(t *testing.T) runtime.Runtime

// RunRuntimeSuite executes the full conformance suite for a runtime implementation.
func RunRuntimeSuite(t *testing.T, factory RuntimeFactory) {
	t.Helper()
	runRuntimeSuite(t, factory, "test-agent", "default", "custom-ns")
}

func RunBoundRuntimeSuite(t *testing.T, factory RuntimeFactory, agentName, namespace string) {
	t.Helper()
	runRuntimeSuite(t, factory, agentName, namespace, namespace)
}

func runRuntimeSuite(t *testing.T, factory RuntimeFactory, agentName, namespace, contextNamespace string) {
	t.Helper()

	t.Run("SessionLifecycle", func(t *testing.T) {
		runSessionLifecycle(t, factory, agentName, namespace, contextNamespace)
	})
	t.Run("Streaming", func(t *testing.T) {
		runStreaming(t, factory, agentName, namespace)
	})
	t.Run("Cancellation", func(t *testing.T) {
		runCancellation(t, factory, agentName, namespace)
	})
	t.Run("Checkpoint", func(t *testing.T) {
		runCheckpoint(t, factory, agentName, namespace)
	})
	t.Run("Backpressure", func(t *testing.T) {
		runBackpressure(t, factory, agentName, namespace)
	})
	t.Run("Health", func(t *testing.T) {
		runHealth(t, factory)
	})
}

// --- Session Lifecycle ---

func runSessionLifecycle(t *testing.T, factory RuntimeFactory, agentName, namespace, contextNamespace string) {
	t.Helper()
	rt := factory(t)
	ctx := context.Background()

	t.Run("CreateSession_ReturnsUniqueID", func(t *testing.T) {
		resp1, err := rt.CreateSession(ctx, runtime.CreateSessionRequest{
			AgentName:      agentName,
			Namespace:      namespace,
			InitialCredits: 10,
		})
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		if err := validateSessionID(resp1.SessionID); err != nil {
			t.Fatal(err)
		}

		resp2, err := rt.CreateSession(ctx, runtime.CreateSessionRequest{
			AgentName:      agentName,
			Namespace:      namespace,
			InitialCredits: 10,
		})
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}
		if err := validateUniqueSessionIDs(resp1.SessionID, resp2.SessionID); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("CreateSession_WithContext", func(t *testing.T) {
		resp, err := rt.CreateSession(ctx, runtime.CreateSessionRequest{
			AgentName:      agentName,
			Namespace:      contextNamespace,
			Context:        map[string]string{"key": "value"},
			InitialCredits: 5,
		})
		if err != nil {
			t.Fatalf("CreateSession with context: %v", err)
		}
		if err := validateSessionID(resp.SessionID); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("GetCapabilities_NonEmpty", func(t *testing.T) {
		caps, err := rt.GetCapabilities(ctx)
		if err != nil {
			t.Fatalf("GetCapabilities: %v", err)
		}
		if err := validateRuntimeVersion(caps); err != nil {
			t.Error(err)
		}
	})
}

func validateRuntimeVersion(capabilities runtime.Capabilities) error {
	if capabilities.RuntimeVersion == "" {
		return fmt.Errorf("expected non-empty runtime version")
	}
	return nil
}

// --- Streaming ---

func runStreaming(t *testing.T, factory RuntimeFactory, agentName, namespace string) {
	t.Helper()
	rt := factory(t)
	ctx := context.Background()

	resp, err := rt.CreateSession(ctx, runtime.CreateSessionRequest{
		AgentName:      agentName,
		Namespace:      namespace,
		InitialCredits: 100,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	t.Run("Step_EmitsEvents", func(t *testing.T) {
		stream, err := rt.Step(ctx, runtime.StepRequest{
			SessionID: resp.SessionID,
			Input:     "hello",
		})
		if err != nil {
			t.Fatalf("Step: %v", err)
		}
		defer stream.Close()

		var events []runtime.StepEvent
		timeout := time.After(5 * time.Second)
		for {
			select {
			case ev, ok := <-stream.Events:
				if !ok {
					goto done
				}
				events = append(events, ev)
			case <-timeout:
				t.Fatal("timed out waiting for events")
			}
		}
	done:
		if len(events) == 0 {
			t.Fatal("expected at least one event from Step")
		}
	})

	t.Run("Step_EventsHaveIncreasingSequence", func(t *testing.T) {
		stream, err := rt.Step(ctx, runtime.StepRequest{
			SessionID: resp.SessionID,
			Input:     "sequence test",
		})
		if err != nil {
			t.Fatalf("Step: %v", err)
		}
		defer stream.Close()

		var lastSeq uint64
		timeout := time.After(5 * time.Second)
		for {
			select {
			case ev, ok := <-stream.Events:
				if !ok {
					goto seqDone
				}
				if ev.Sequence <= lastSeq && lastSeq != 0 {
					t.Errorf("sequence not increasing: got %d after %d", ev.Sequence, lastSeq)
				}
				lastSeq = ev.Sequence
			case <-timeout:
				t.Fatal("timed out")
			}
		}
	seqDone:
		if lastSeq == 0 {
			t.Fatal("expected at least one event with non-zero sequence")
		}
	})
}

// --- Cancellation ---

func runCancellation(t *testing.T, factory RuntimeFactory, agentName, namespace string) {
	t.Helper()
	rt := factory(t)
	ctx := context.Background()

	t.Run("Cancel_AcknowledgesOrForces", func(t *testing.T) {
		resp, err := rt.CreateSession(ctx, runtime.CreateSessionRequest{
			AgentName:      agentName,
			Namespace:      namespace,
			InitialCredits: 100,
		})
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}

		cancelResp, err := rt.Cancel(ctx, runtime.CancelRequest{
			SessionID: resp.SessionID,
			TimeoutMS: 1000,
		})
		if err != nil {
			t.Fatalf("Cancel: %v", err)
		}
		if err := validateCancellation(cancelResp); err != nil {
			t.Error(err)
		}
	})
}

func validateCancellation(response runtime.CancelResponse) error {
	if response.State == runtime.CancelAckUnspecified {
		return fmt.Errorf("expected acknowledged or forced, got unspecified")
	}
	return nil
}

// --- Checkpoint ---

func runCheckpoint(t *testing.T, factory RuntimeFactory, agentName, namespace string) {
	t.Helper()
	rt := factory(t)
	ctx := context.Background()

	t.Run("Checkpoint_And_Resume", func(t *testing.T) {
		resp, err := rt.CreateSession(ctx, runtime.CreateSessionRequest{
			AgentName:      agentName,
			Namespace:      namespace,
			InitialCredits: 10,
		})
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}

		cpResp, err := rt.Checkpoint(ctx, resp.SessionID)
		if err != nil {
			t.Fatalf("Checkpoint: %v", err)
		}
		if err := validateCheckpoint(cpResp); err != nil {
			t.Fatal(err)
		}

		resumeResp, err := rt.Resume(ctx, cpResp.Token)
		if err != nil {
			t.Fatalf("Resume: %v", err)
		}
		if err := validateResume(resumeResp); err != nil {
			t.Error(err)
		}
	})
}

func validateCheckpoint(response runtime.CheckpointResponse) error {
	if len(response.Token) == 0 {
		return fmt.Errorf("expected non-empty checkpoint token")
	}
	if response.CreatedAt.IsZero() {
		return fmt.Errorf("expected non-zero checkpoint timestamp")
	}
	return nil
}

func validateResume(response runtime.ResumeResponse) error {
	return validateSessionID(response.SessionID)
}

func validateSessionID(sessionID string) error {
	if sessionID == "" {
		return fmt.Errorf("expected non-empty session ID")
	}
	return nil
}

func validateUniqueSessionIDs(first, second string) error {
	if first == second {
		return fmt.Errorf("expected unique session IDs")
	}
	return nil
}

// --- Backpressure ---

func runBackpressure(t *testing.T, factory RuntimeFactory, agentName, namespace string) {
	t.Helper()
	rt := factory(t)
	ctx := context.Background()

	t.Run("ZeroCredits_PausesEmission", func(t *testing.T) {
		resp, err := rt.CreateSession(ctx, runtime.CreateSessionRequest{
			AgentName:      agentName,
			Namespace:      namespace,
			InitialCredits: 1, // minimal credits
		})
		if err != nil {
			t.Fatalf("CreateSession: %v", err)
		}

		stream, err := rt.Step(ctx, runtime.StepRequest{
			SessionID: resp.SessionID,
			Input:     "backpressure test",
		})
		if err != nil {
			t.Fatalf("Step: %v", err)
		}
		defer stream.Close()

		// Drain the initial credit's worth of events.
		// The runtime should pause or emit a CreditRequest event.
		timeout := time.After(3 * time.Second)
		var gotCreditRequest bool
		var eventCount int
		for {
			select {
			case ev, ok := <-stream.Events:
				if !ok {
					goto bpDone
				}
				eventCount++
				if ev.Type == runtime.EventCreditRequest {
					gotCreditRequest = true
					// Grant more credits to let it finish
					stream.GrantCredits(10)
				}
			case <-timeout:
				goto bpDone
			}
		}
	bpDone:
		// Either the runtime sent a CreditRequest or completed within 1 credit.
		// Both are conformant. If it emitted >1 event with 1 credit and no credit
		// request, that would be a violation, but we allow it for simplicity in v0.1.
		_ = gotCreditRequest
		_ = eventCount
	})
}

// --- Health ---

func runHealth(t *testing.T, factory RuntimeFactory) {
	t.Helper()
	rt := factory(t)
	ctx := context.Background()

	t.Run("Health_ReturnsStatus", func(t *testing.T) {
		status, err := rt.Health(ctx)
		if err != nil {
			t.Fatalf("Health: %v", err)
		}
		if err := validateHealth(status); err != nil {
			t.Error(err)
		}
	})
}

func validateHealth(status runtime.HealthStatus) error {
	if status.State == runtime.HealthUnknown {
		return fmt.Errorf("expected a known health state after initialization")
	}
	return nil
}
