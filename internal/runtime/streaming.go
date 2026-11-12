// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtime

import (
	"context"
	"sync"
	"sync/atomic"
)

// EventEmitter manages the streaming side of a step execution,
// enforcing credit-based backpressure and sequential event ordering.
type EventEmitter struct {
	session  *Session
	eventsCh chan StepEvent
	sequence atomic.Uint64
	ctx      context.Context
	cancel   context.CancelFunc

	mu        sync.Mutex
	closed    bool
	closeOnce sync.Once
}

// NewEventEmitter creates an emitter bound to a session.
// bufferSize controls the channel capacity for events.
func NewEventEmitter(ctx context.Context, session *Session, bufferSize int) *EventEmitter {
	if bufferSize <= 0 {
		bufferSize = 16
	}
	emitCtx, cancel := context.WithCancel(ctx)
	return &EventEmitter{
		session:  session,
		eventsCh: make(chan StepEvent, bufferSize),
		ctx:      emitCtx,
		cancel:   cancel,
	}
}

// Stream returns a StepStream that the caller uses to receive events
// and grant credits back to the runtime.
func (e *EventEmitter) Stream() *StepStream {
	return &StepStream{
		Events: e.eventsCh,
		GrantCredits: func(credits uint32) {
			e.session.AddCredits(credits)
		},
		cancel: e.cancel,
	}
}

// Emit sends an event through the stream. It blocks if credits are
// exhausted until credits arrive or the context is cancelled.
// Returns an error if the stream was cancelled.
func (e *EventEmitter) Emit(event StepEvent) error {
	// Wait for available credits (backpressure).
	// Loop is necessary because another concurrent Emit may consume the
	// credit between the signal and ConsumeCredit succeeding.
	for !e.session.ConsumeCredit() {
		// Capture the credit signal BEFORE sending the request. This
		// guarantees we observe the broadcast triggered by our own request
		// even if it is processed before we reach the select.
		ch := e.session.creditSignal()
		e.sendCreditRequest()
		select {
		case <-e.ctx.Done():
			return e.ctx.Err()
		case <-ch:
		}
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return e.ctx.Err()
	}
	// Assign monotonic sequence number inside the lock to guarantee ordering.
	event.Sequence = e.sequence.Add(1)
	select {
	case e.eventsCh <- event:
		return nil
	case <-e.ctx.Done():
		return e.ctx.Err()
	}
}

// sendCreditRequest emits a credit request event without consuming a credit.
func (e *EventEmitter) sendCreditRequest() {
	req := StepEvent{
		Type: EventCreditRequest,
		CreditRequest: &CreditRequestEvent{
			SessionID: e.session.ID,
			Needed:    10, // suggest batch size
		},
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	// Assign sequence inside the lock to guarantee ordering.
	req.Sequence = e.sequence.Add(1)
	select {
	case e.eventsCh <- req:
	case <-e.ctx.Done():
	}
}

// Close shuts down the emitter and closes the events channel.
// Safe to call multiple times.
func (e *EventEmitter) Close() {
	e.closeOnce.Do(func() {
		e.cancel()
		e.mu.Lock()
		e.closed = true
		close(e.eventsCh)
		e.mu.Unlock()
	})
}

// Done returns a channel that is closed when the emitter context is cancelled.
func (e *EventEmitter) Done() <-chan struct{} {
	return e.ctx.Done()
}
