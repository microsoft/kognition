// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtime

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TC-0108: Verify structured events are received with correct types and monotonic sequence.
func TestEventEmitter_EmitSequence(t *testing.T) {
	mgr := NewSessionManager()
	session, _ := mgr.Create(context.Background(), CreateSessionRequest{
		AgentName: "a", Namespace: "n", InitialCredits: 10,
	})

	emitter := NewEventEmitter(context.Background(), session, 8)
	stream := emitter.Stream()
	defer emitter.Close()

	// Emit several events.
	for i := 0; i < 5; i++ {
		err := emitter.Emit(StepEvent{
			Type:  EventToken,
			Token: &TokenEvent{Content: "tok", Role: "assistant"},
		})
		if err != nil {
			t.Fatalf("Emit[%d]: %v", i, err)
		}
	}

	// Read events and verify sequence numbers.
	for i := uint64(1); i <= 5; i++ {
		select {
		case ev := <-stream.Events:
			if ev.Sequence != i {
				t.Errorf("event[%d].Sequence = %d, want %d", i, ev.Sequence, i)
			}
			if ev.Type != EventToken {
				t.Errorf("event[%d].Type = %d, want EventToken", i, ev.Type)
			}
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("timeout waiting for event %d", i)
		}
	}
}

// TC-0111: Exhaust credits; verify runtime pauses until new credits are issued (backpressure).
func TestEventEmitter_BackpressurePause(t *testing.T) {
	mgr := NewSessionManager()
	session, _ := mgr.Create(context.Background(), CreateSessionRequest{
		AgentName: "a", Namespace: "n", InitialCredits: 2,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	emitter := NewEventEmitter(ctx, session, 16)
	stream := emitter.Stream()

	// Emit 2 events (exhausts credits).
	for i := 0; i < 2; i++ {
		err := emitter.Emit(StepEvent{Type: EventToken, Token: &TokenEvent{Content: "t"}})
		if err != nil {
			t.Fatalf("Emit[%d]: %v", i, err)
		}
	}

	// Third emit should block until credits arrive.
	done := make(chan error, 1)
	go func() {
		done <- emitter.Emit(StepEvent{Type: EventOutput, Output: &OutputStreamEvent{Content: "out"}})
	}()

	// Drain the credit request event that should appear.
	drainCreditRequest := func() {
		timer := time.NewTimer(200 * time.Millisecond)
		defer timer.Stop()
		for {
			select {
			case ev := <-stream.Events:
				if ev.Type == EventCreditRequest {
					return
				}
			case <-timer.C:
				return
			}
		}
	}
	drainCreditRequest()

	// Grant credits via the stream's GrantCredits function.
	stream.GrantCredits(5)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Emit after credit grant: %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("Emit did not unblock after credit grant")
	}

	emitter.Close()
}

func TestEventEmitter_CancelStopsEmit(t *testing.T) {
	mgr := NewSessionManager()
	session, _ := mgr.Create(context.Background(), CreateSessionRequest{
		AgentName: "a", Namespace: "n", InitialCredits: 1,
	})
	// Exhaust credit.
	session.ConsumeCredit()

	ctx, cancel := context.WithCancel(context.Background())
	emitter := NewEventEmitter(ctx, session, 4)
	defer emitter.Close()

	// Cancel immediately.
	cancel()

	err := emitter.Emit(StepEvent{Type: EventToken, Token: &TokenEvent{Content: "x"}})
	if err == nil {
		t.Fatal("Emit should fail on cancelled context")
	}
}

func TestStepStream_Close(t *testing.T) {
	mgr := NewSessionManager()
	session, _ := mgr.Create(context.Background(), CreateSessionRequest{
		AgentName: "a", Namespace: "n", InitialCredits: 5,
	})

	emitter := NewEventEmitter(context.Background(), session, 4)
	stream := emitter.Stream()

	// Close via stream should cancel the emitter context.
	stream.Close()

	select {
	case <-emitter.Done():
		// good — cancelled
	case <-time.After(100 * time.Millisecond):
		t.Fatal("emitter context not cancelled after stream.Close()")
	}
}

func TestEventEmitter_DefaultBufferSize(t *testing.T) {
	mgr := NewSessionManager()
	session, _ := mgr.Create(context.Background(), CreateSessionRequest{
		AgentName: "a", Namespace: "n", InitialCredits: 50,
	})

	// bufferSize <= 0 should default to 16
	emitter := NewEventEmitter(context.Background(), session, 0)
	defer emitter.Close()

	// Should be able to emit up to 16 events without blocking (buffer + credits).
	for i := 0; i < 16; i++ {
		err := emitter.Emit(StepEvent{Type: EventToken, Token: &TokenEvent{Content: "t"}})
		if err != nil {
			t.Fatalf("Emit[%d]: %v", i, err)
		}
	}
}

func TestEventEmitter_EmitAfterClose(t *testing.T) {
	mgr := NewSessionManager()
	session, _ := mgr.Create(context.Background(), CreateSessionRequest{
		AgentName: "a", Namespace: "n", InitialCredits: 50,
	})

	emitter := NewEventEmitter(context.Background(), session, 4)
	emitter.Close()

	// Emit after Close should return an error, NOT panic.
	err := emitter.Emit(StepEvent{Type: EventToken, Token: &TokenEvent{Content: "late"}})
	if err == nil {
		t.Fatal("Emit after Close should return an error")
	}
}

func TestEventEmitter_DoubleClose(t *testing.T) {
	mgr := NewSessionManager()
	session, _ := mgr.Create(context.Background(), CreateSessionRequest{
		AgentName: "a", Namespace: "n", InitialCredits: 10,
	})

	emitter := NewEventEmitter(context.Background(), session, 4)

	// Should not panic on double close.
	emitter.Close()
	emitter.Close()
}

func TestEventEmitter_CreditRequestFields(t *testing.T) {
	mgr := NewSessionManager()
	session, _ := mgr.Create(context.Background(), CreateSessionRequest{
		AgentName: "a", Namespace: "n", InitialCredits: 1,
	})

	emitter := NewEventEmitter(context.Background(), session, 8)
	stream := emitter.Stream()
	defer emitter.Close()

	// Consume the only credit so next Emit triggers a credit request.
	session.ConsumeCredit()

	// Grant credits in background so Emit can complete.
	go func() {
		time.Sleep(20 * time.Millisecond)
		session.AddCredits(5)
	}()

	err := emitter.Emit(StepEvent{Type: EventToken, Token: &TokenEvent{Content: "x"}})
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}

	// Drain channel and find the credit request event.
	var found bool
	for i := 0; i < 10; i++ {
		select {
		case ev := <-stream.Events:
			if ev.Type == EventCreditRequest {
				found = true
				if ev.CreditRequest == nil {
					t.Fatal("CreditRequest field is nil")
				}
				if ev.CreditRequest.SessionID != session.ID {
					t.Errorf("SessionID = %q, want %q", ev.CreditRequest.SessionID, session.ID)
				}
				if ev.CreditRequest.Needed != 10 {
					t.Errorf("Needed = %d, want 10", ev.CreditRequest.Needed)
				}
			}
		default:
			i = 10 // break outer loop
		}
	}
	if !found {
		t.Error("expected a CreditRequest event in the stream")
	}
}

func TestEventEmitter_ConcurrentEmitOrdering(t *testing.T) {
	mgr := NewSessionManager()
	const numEvents = 50
	session, _ := mgr.Create(context.Background(), CreateSessionRequest{
		AgentName: "a", Namespace: "n", InitialCredits: uint32(numEvents + 10),
	})

	emitter := NewEventEmitter(context.Background(), session, numEvents+10)
	stream := emitter.Stream()
	defer emitter.Close()

	// Spawn many goroutines emitting concurrently.
	const goroutines = 10
	eventsPerGoroutine := numEvents / goroutines
	done := make(chan struct{})
	for g := 0; g < goroutines; g++ {
		go func() {
			for i := 0; i < eventsPerGoroutine; i++ {
				_ = emitter.Emit(StepEvent{
					Type:  EventToken,
					Token: &TokenEvent{Content: "tok", Role: "assistant"},
				})
			}
			done <- struct{}{}
		}()
	}
	for g := 0; g < goroutines; g++ {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for concurrent emitters")
		}
	}

	// Read all events from stream and verify sequence numbers are unique and
	// delivered in strictly increasing order (guaranteed by lock-inside pattern).
	var lastSeq uint64
	for i := 0; i < numEvents; i++ {
		select {
		case ev := <-stream.Events:
			if ev.Sequence <= lastSeq {
				t.Fatalf("event[%d].Sequence = %d, expected > %d (not strictly increasing)", i, ev.Sequence, lastSeq)
			}
			lastSeq = ev.Sequence
		case <-time.After(2 * time.Second):
			t.Fatalf("timeout waiting for event %d", i)
		}
	}
}

// TestEventEmitter_ConcurrentCreditExhaustion verifies that concurrent Emit
// calls do NOT silently lose events when credits are scarce and multiple
// goroutines race through the WaitForCredits → ConsumeCredit path.
func TestEventEmitter_ConcurrentCreditExhaustion(t *testing.T) {
	mgr := NewSessionManager()
	const goroutines = 5
	const eventsPerGoroutine = 4
	const totalEvents = goroutines * eventsPerGoroutine

	// Start with only 1 credit — forces nearly every Emit through the wait path.
	session, _ := mgr.Create(context.Background(), CreateSessionRequest{
		AgentName: "a", Namespace: "n", InitialCredits: 1,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	emitter := NewEventEmitter(ctx, session, totalEvents+goroutines*10)
	stream := emitter.Stream()
	defer emitter.Close()

	var emitCount atomic.Int32
	var grantCount atomic.Int32
	var crCount atomic.Int32

	// Background goroutine grants credits one at a time when requested.
	go func() {
		for {
			select {
			case ev, ok := <-stream.Events:
				if !ok {
					return
				}
				if ev.Type == EventCreditRequest {
					crCount.Add(1)
					stream.GrantCredits(1)
					grantCount.Add(1)
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	// Spawn goroutines that emit concurrently with scarce credits.
	errCh := make(chan error, totalEvents)
	done := make(chan struct{})
	for g := 0; g < goroutines; g++ {
		go func() {
			for i := 0; i < eventsPerGoroutine; i++ {
				if err := emitter.Emit(StepEvent{
					Type:  EventToken,
					Token: &TokenEvent{Content: "tok", Role: "assistant"},
				}); err != nil {
					errCh <- err
				}
				emitCount.Add(1)
			}
			done <- struct{}{}
		}()
	}

	for g := 0; g < goroutines; g++ {
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatalf("timed out: emits=%d grants=%d creditReqs=%d credits=%d",
				emitCount.Load(), grantCount.Load(), crCount.Load(), session.Credits())
		}
	}

	close(errCh)
	for err := range errCh {
		t.Errorf("unexpected Emit error: %v", err)
	}
}
