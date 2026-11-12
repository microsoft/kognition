// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtime

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TC-0108: CreateSession produces a valid session with correct initial state.
func TestSessionManager_CreateAndGet(t *testing.T) {
	mgr := NewSessionManager()

	session, err := mgr.Create(context.Background(), CreateSessionRequest{
		AgentName:      "test-agent",
		Namespace:      "default",
		Context:        map[string]string{"key": "value"},
		InitialCredits: 50,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if session.ID == "" {
		t.Fatal("expected non-empty session ID")
	}
	if session.AgentName != "test-agent" {
		t.Errorf("AgentName = %q, want %q", session.AgentName, "test-agent")
	}
	if session.Credits() != 50 {
		t.Errorf("Credits = %d, want 50", session.Credits())
	}
	if session.State != StateWaitingForInput {
		t.Errorf("State = %d, want StateWaitingForInput", session.State)
	}

	got, err := mgr.Get(session.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != session.ID {
		t.Errorf("Get returned different session")
	}
}

func TestSessionManager_GetNotFound(t *testing.T) {
	mgr := NewSessionManager()
	_, err := mgr.Get("nonexistent")
	if !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("Get = %v, want ErrSessionNotFound", err)
	}
}

func TestSessionManager_DefaultCredits(t *testing.T) {
	mgr := NewSessionManager()
	session, _ := mgr.Create(context.Background(), CreateSessionRequest{
		AgentName:      "agent",
		Namespace:      "ns",
		InitialCredits: 0, // should default to 100
	})
	if session.Credits() != 100 {
		t.Errorf("Credits = %d, want 100 (default)", session.Credits())
	}
}

// TC-0109: Cancel an active session; verify the runtime acknowledges and no further events.
func TestSessionManager_CancelCooperative(t *testing.T) {
	mgr := NewSessionManager()
	session, _ := mgr.Create(context.Background(), CreateSessionRequest{
		AgentName:      "agent",
		Namespace:      "ns",
		InitialCredits: 10,
	})

	// Simulate the runtime acknowledging cancellation cooperatively.
	go func() {
		<-session.CancelCtx().Done()
		session.Acknowledge()
	}()

	resp, err := mgr.Cancel(context.Background(), CancelRequest{
		SessionID: session.ID,
		TimeoutMS: 1000,
	})
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if resp.State != CancelAcknowledged {
		t.Errorf("State = %d, want CancelAcknowledged", resp.State)
	}
	if resp.CancelledAt.IsZero() {
		t.Error("CancelledAt should not be zero")
	}
	if session.State != StateCancelled {
		t.Errorf("session.State = %d, want StateCancelled", session.State)
	}
}

func TestSessionManager_CancelNotFound(t *testing.T) {
	mgr := NewSessionManager()
	_, err := mgr.Cancel(context.Background(), CancelRequest{SessionID: "nope", TimeoutMS: 100})
	if !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("Cancel = %v, want ErrSessionNotFound", err)
	}
}

func TestSessionManager_CancelAlreadyCompleted(t *testing.T) {
	mgr := NewSessionManager()
	session, _ := mgr.Create(context.Background(), CreateSessionRequest{
		AgentName: "agent", Namespace: "ns", InitialCredits: 10,
	})
	session.mu.Lock()
	session.State = StateCompleted
	session.mu.Unlock()

	resp, err := mgr.Cancel(context.Background(), CancelRequest{SessionID: session.ID, TimeoutMS: 100})
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if resp.State != CancelAcknowledged {
		t.Errorf("State = %d, want CancelAcknowledged for already-completed session", resp.State)
	}
}

// TC-0110: Checkpoint then Resume; verify session resumes from checkpointed state.
func TestSessionManager_Checkpoint_Resume(t *testing.T) {
	mgr := NewSessionManager()
	session, _ := mgr.Create(context.Background(), CreateSessionRequest{
		AgentName:      "ckpt-agent",
		Namespace:      "ckpt-ns",
		Context:        map[string]string{"foo": "bar"},
		InitialCredits: 25,
	})

	cpResp, err := mgr.Checkpoint(session.ID)
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if len(cpResp.Token) == 0 {
		t.Fatal("expected non-empty token")
	}
	if cpResp.CreatedAt.IsZero() {
		t.Error("CreatedAt should not be zero")
	}

	// Resume from token.
	resumed, err := mgr.Resume(context.Background(), cpResp.Token)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.ID == session.ID {
		t.Error("resumed session should have a new ID")
	}
	if resumed.AgentName != "ckpt-agent" {
		t.Errorf("AgentName = %q, want %q", resumed.AgentName, "ckpt-agent")
	}
	if resumed.Namespace != "ckpt-ns" {
		t.Errorf("Namespace = %q, want %q", resumed.Namespace, "ckpt-ns")
	}
	if resumed.Context["foo"] != "bar" {
		t.Errorf("Context[foo] = %q, want %q", resumed.Context["foo"], "bar")
	}
	if resumed.State != StateWaitingForInput {
		t.Errorf("State = %d, want StateWaitingForInput", resumed.State)
	}
}

func TestSessionManager_CheckpointNotFound(t *testing.T) {
	mgr := NewSessionManager()
	_, err := mgr.Checkpoint("no-such-session")
	if !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("Checkpoint = %v, want ErrSessionNotFound", err)
	}
}

func TestSessionManager_ResumeInvalidToken(t *testing.T) {
	mgr := NewSessionManager()
	_, err := mgr.Resume(context.Background(), []byte("garbage"))
	if !errors.Is(err, ErrInvalidCheckpoint) {
		t.Errorf("Resume = %v, want ErrInvalidCheckpoint", err)
	}

	// Empty session ID in valid JSON.
	_, err = mgr.Resume(context.Background(), []byte(`{"session_id":""}`))
	if !errors.Is(err, ErrInvalidCheckpoint) {
		t.Errorf("Resume empty session_id = %v, want ErrInvalidCheckpoint", err)
	}
}

func TestSessionManager_ActiveCount(t *testing.T) {
	mgr := NewSessionManager()
	if mgr.ActiveCount() != 0 {
		t.Fatalf("ActiveCount = %d, want 0", mgr.ActiveCount())
	}
	s1, _ := mgr.Create(context.Background(), CreateSessionRequest{AgentName: "a", Namespace: "n"})
	mgr.Create(context.Background(), CreateSessionRequest{AgentName: "b", Namespace: "n"})
	if mgr.ActiveCount() != 2 {
		t.Errorf("ActiveCount = %d, want 2", mgr.ActiveCount())
	}
	mgr.Remove(s1.ID)
	if mgr.ActiveCount() != 1 {
		t.Errorf("ActiveCount = %d, want 1 after remove", mgr.ActiveCount())
	}
}

func TestSession_CreditOperations(t *testing.T) {
	mgr := NewSessionManager()
	session, _ := mgr.Create(context.Background(), CreateSessionRequest{
		AgentName: "a", Namespace: "n", InitialCredits: 3,
	})

	// Consume all credits.
	for i := 0; i < 3; i++ {
		if !session.ConsumeCredit() {
			t.Fatalf("ConsumeCredit failed at iteration %d", i)
		}
	}
	if session.ConsumeCredit() {
		t.Error("ConsumeCredit should return false when exhausted")
	}
	if session.Credits() != 0 {
		t.Errorf("Credits = %d, want 0", session.Credits())
	}

	// Add credits.
	session.AddCredits(5)
	if session.Credits() != 5 {
		t.Errorf("Credits = %d, want 5 after AddCredits", session.Credits())
	}
}

func TestSession_WaitForCredits(t *testing.T) {
	mgr := NewSessionManager()
	session, _ := mgr.Create(context.Background(), CreateSessionRequest{
		AgentName: "a", Namespace: "n", InitialCredits: 0,
	})
	// InitialCredits=0 defaults to 100; drain them all.
	for session.ConsumeCredit() {
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// Grant credits after a short delay.
	go func() {
		time.Sleep(20 * time.Millisecond)
		session.AddCredits(1)
	}()

	err := session.WaitForCredits(ctx)
	if err != nil {
		t.Fatalf("WaitForCredits: %v", err)
	}
	if session.Credits() != 1 {
		t.Errorf("Credits = %d, want 1", session.Credits())
	}
}

func TestSession_WaitForCredits_Cancelled(t *testing.T) {
	mgr := NewSessionManager()
	session, _ := mgr.Create(context.Background(), CreateSessionRequest{
		AgentName: "a", Namespace: "n", InitialCredits: 1,
	})
	session.ConsumeCredit() // exhaust

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // immediately cancel

	err := session.WaitForCredits(ctx)
	if err == nil {
		t.Fatal("WaitForCredits should return error on cancelled context")
	}
}

func TestSessionManager_CancelForced(t *testing.T) {
	mgr := NewSessionManager()
	session, _ := mgr.Create(context.Background(), CreateSessionRequest{
		AgentName: "agent", Namespace: "ns", InitialCredits: 10,
	})

	// Do NOT call Acknowledge — runtime never responds.
	resp, err := mgr.Cancel(context.Background(), CancelRequest{
		SessionID: session.ID,
		TimeoutMS: 50, // short timeout to trigger forced path
	})
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if resp.State != CancelForced {
		t.Errorf("State = %d, want CancelForced", resp.State)
	}
	if session.State != StateCancelled {
		t.Errorf("session.State = %d, want StateCancelled", session.State)
	}
}

func TestSessionManager_CancelAlreadyCancelled(t *testing.T) {
	mgr := NewSessionManager()
	session, _ := mgr.Create(context.Background(), CreateSessionRequest{
		AgentName: "agent", Namespace: "ns", InitialCredits: 10,
	})
	session.mu.Lock()
	session.State = StateCancelled
	session.mu.Unlock()

	resp, err := mgr.Cancel(context.Background(), CancelRequest{SessionID: session.ID, TimeoutMS: 100})
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if resp.State != CancelAcknowledged {
		t.Errorf("State = %d, want CancelAcknowledged for already-cancelled session", resp.State)
	}
}

func TestSessionManager_CancelContextCancelled(t *testing.T) {
	mgr := NewSessionManager()
	session, _ := mgr.Create(context.Background(), CreateSessionRequest{
		AgentName: "agent", Namespace: "ns", InitialCredits: 10,
	})
	// Do NOT call Acknowledge — simulate stalled runtime.
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // immediately cancel the caller context

	_, err := mgr.Cancel(ctx, CancelRequest{
		SessionID: session.ID,
		TimeoutMS: 5000,
	})
	if err == nil {
		t.Fatal("Cancel should return error when caller context is cancelled")
	}
}

func TestSession_AcknowledgeIdempotent(t *testing.T) {
	mgr := NewSessionManager()
	session, _ := mgr.Create(context.Background(), CreateSessionRequest{
		AgentName: "agent", Namespace: "ns", InitialCredits: 10,
	})

	// Should not panic on multiple calls.
	session.Acknowledge()
	session.Acknowledge()
}

func TestSession_AcknowledgeConcurrent(t *testing.T) {
	mgr := NewSessionManager()
	session, _ := mgr.Create(context.Background(), CreateSessionRequest{
		AgentName: "agent", Namespace: "ns", InitialCredits: 10,
	})

	// Spawn many goroutines calling Acknowledge concurrently.
	// With the race detector enabled, this verifies no double-close panic.
	const goroutines = 100
	done := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		go func() {
			session.Acknowledge()
			done <- struct{}{}
		}()
	}
	for i := 0; i < goroutines; i++ {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("timeout waiting for concurrent Acknowledge goroutines")
		}
	}
}

// TestSession_MultiWaiterCredits verifies that multiple goroutines competing for
// scarce credits (1 at a time) all eventually make progress.
func TestSession_MultiWaiterCredits(t *testing.T) {
	mgr := NewSessionManager()
	session, _ := mgr.Create(context.Background(), CreateSessionRequest{
		AgentName: "a", Namespace: "n", InitialCredits: 0,
	})

	const goroutines = 5
	const consumesPerGoroutine = 4
	const totalConsumes = goroutines * consumesPerGoroutine

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan struct{}, goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			for i := 0; i < consumesPerGoroutine; i++ {
				for !session.ConsumeCredit() {
					if err := session.WaitForCredits(ctx); err != nil {
						return
					}
				}
			}
			done <- struct{}{}
		}()
	}

	// Grant credits one at a time.
	for i := 0; i < totalConsumes; i++ {
		session.AddCredits(1)
		// Small sleep to let goroutines compete.
		time.Sleep(10 * time.Microsecond)
	}

	for g := 0; g < goroutines; g++ {
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatalf("timed out: only %d/%d goroutines finished", g, goroutines)
		}
	}
}
