// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

var (
	ErrSessionNotFound   = errors.New("session not found")
	ErrSessionCancelled  = errors.New("session cancelled")
	ErrSessionCompleted  = errors.New("session already completed")
	ErrInvalidCheckpoint = errors.New("invalid checkpoint token")
)

// Session represents an active agent execution session.
type Session struct {
	ID        string
	AgentName string
	Namespace string
	Context   map[string]string
	State     SessionState
	CreatedAt time.Time

	// credits is the current credit balance for backpressure.
	credits uint32
	// creditCh signals when new credits arrive.
	creditCh chan struct{}

	// cancelFn triggers cooperative cancellation.
	cancelFn context.CancelFunc
	// cancelCtx is the session-scoped context that respects cancellation.
	cancelCtx context.Context

	// doneCh is closed when the runtime acknowledges cancellation.
	// Cancel() waits on this (not cancelCtx) to distinguish cooperative vs forced.
	doneCh   chan struct{}
	doneOnce sync.Once

	mu sync.Mutex
}

// checkpoint is the serialized form of a session checkpoint.
type checkpoint struct {
	SessionID string            `json:"session_id"`
	AgentName string            `json:"agent_name"`
	Namespace string            `json:"namespace"`
	Context   map[string]string `json:"context"`
	State     SessionState      `json:"state"`
	CreatedAt time.Time         `json:"created_at"`
}

// SessionManager tracks and manages active sessions.
type SessionManager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	now      func() time.Time
	newID    func() string
}

// NewSessionManager creates a new session manager.
func NewSessionManager() *SessionManager {
	return &SessionManager{
		sessions: make(map[string]*Session),
		now:      time.Now,
		newID:    func() string { return uuid.New().String() },
	}
}

// Create initializes a new session and returns its ID.
func (m *SessionManager) Create(ctx context.Context, req CreateSessionRequest) (*Session, error) {
	sessionCtx, cancelFn := context.WithCancel(ctx)

	session := &Session{
		ID:        m.newID(),
		AgentName: req.AgentName,
		Namespace: req.Namespace,
		Context:   req.Context,
		State:     StateWaitingForInput,
		CreatedAt: m.now(),
		credits:   req.InitialCredits,
		creditCh:  make(chan struct{}),
		cancelFn:  cancelFn,
		cancelCtx: sessionCtx,
		doneCh:    make(chan struct{}),
	}

	if session.credits == 0 {
		session.credits = 100 // default credit allocation
	}

	m.mu.Lock()
	m.sessions[session.ID] = session
	m.mu.Unlock()

	return session, nil
}

// Get retrieves a session by ID.
func (m *SessionManager) Get(sessionID string) (*Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	s, ok := m.sessions[sessionID]
	if !ok {
		return nil, ErrSessionNotFound
	}
	return s, nil
}

// Cancel performs cooperative cancellation with timeout fallback.
// Returns whether the runtime acknowledged or was force-killed.
func (m *SessionManager) Cancel(ctx context.Context, req CancelRequest) (CancelResponse, error) {
	session, err := m.Get(req.SessionID)
	if err != nil {
		return CancelResponse{}, err
	}

	session.mu.Lock()
	if session.State == StateCompleted || session.State == StateCancelled {
		session.mu.Unlock()
		return CancelResponse{
			State:       CancelAcknowledged,
			CancelledAt: m.now(),
		}, nil
	}
	session.mu.Unlock()

	timeout := time.Duration(req.TimeoutMS) * time.Millisecond
	if timeout == 0 {
		timeout = 5 * time.Second
	}

	// Signal cooperative cancellation.
	session.cancelFn()

	// Wait for the runtime to acknowledge (doneCh) or timeout.
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-session.doneCh:
		// Runtime acknowledged cooperatively.
		session.mu.Lock()
		session.State = StateCancelled
		session.mu.Unlock()
		return CancelResponse{
			State:       CancelAcknowledged,
			CancelledAt: m.now(),
		}, nil
	case <-timer.C:
		// Timeout — force kill.
		session.mu.Lock()
		session.State = StateCancelled
		session.mu.Unlock()
		return CancelResponse{
			State:       CancelForced,
			CancelledAt: m.now(),
		}, nil
	case <-ctx.Done():
		return CancelResponse{}, ctx.Err()
	}
}

// Checkpoint produces an opaque token capturing session state.
func (m *SessionManager) Checkpoint(sessionID string) (CheckpointResponse, error) {
	session, err := m.Get(sessionID)
	if err != nil {
		return CheckpointResponse{}, err
	}

	session.mu.Lock()
	cp := checkpoint{
		SessionID: session.ID,
		AgentName: session.AgentName,
		Namespace: session.Namespace,
		Context:   session.Context,
		State:     session.State,
		CreatedAt: session.CreatedAt,
	}
	session.mu.Unlock()

	token, err := json.Marshal(cp)
	if err != nil {
		return CheckpointResponse{}, fmt.Errorf("marshal checkpoint: %w", err)
	}

	return CheckpointResponse{
		Token:     token,
		CreatedAt: m.now(),
	}, nil
}

// Resume restores a session from an opaque checkpoint token.
func (m *SessionManager) Resume(ctx context.Context, token []byte) (*Session, error) {
	var cp checkpoint
	if err := json.Unmarshal(token, &cp); err != nil {
		return nil, ErrInvalidCheckpoint
	}
	if cp.SessionID == "" {
		return nil, ErrInvalidCheckpoint
	}

	sessionCtx, cancelFn := context.WithCancel(ctx)

	session := &Session{
		ID:        m.newID(), // New session ID for resumed session
		AgentName: cp.AgentName,
		Namespace: cp.Namespace,
		Context:   cp.Context,
		State:     StateWaitingForInput,
		CreatedAt: m.now(),
		credits:   100,
		creditCh:  make(chan struct{}),
		cancelFn:  cancelFn,
		cancelCtx: sessionCtx,
		doneCh:    make(chan struct{}),
	}

	m.mu.Lock()
	m.sessions[session.ID] = session
	m.mu.Unlock()

	return session, nil
}

// Remove deletes a session from the manager.
func (m *SessionManager) Remove(sessionID string) {
	m.mu.Lock()
	delete(m.sessions, sessionID)
	m.mu.Unlock()
}

// ActiveCount returns the number of active sessions.
func (m *SessionManager) ActiveCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.sessions)
}

// --- Credit operations on Session ---

// ConsumeCredit decrements the session's credit by 1.
// Returns true if a credit was available, false if exhausted.
func (s *Session) ConsumeCredit() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.credits == 0 {
		return false
	}
	s.credits--
	return true
}

// AddCredits grants additional credits to the session.
func (s *Session) AddCredits(n uint32) {
	s.mu.Lock()
	s.credits += n
	ch := s.creditCh
	s.creditCh = make(chan struct{})
	s.mu.Unlock()

	// Broadcast to all waiters by closing the old channel.
	close(ch)
}

// WaitForCredits blocks until credits become available or context is cancelled.
// Returns nil when a credit signal is received; the caller must still call
// ConsumeCredit and retry if the credit was consumed by another goroutine.
func (s *Session) WaitForCredits(ctx context.Context) error {
	s.mu.Lock()
	if s.credits > 0 {
		s.mu.Unlock()
		return nil
	}
	ch := s.creditCh
	s.mu.Unlock()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-ch:
		return nil
	}
}

// creditSignal returns the current credit broadcast channel.
// Callers should capture this BEFORE sending a credit request to avoid
// missing the resulting broadcast.
func (s *Session) creditSignal() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.creditCh
}

// Credits returns the current credit balance.
func (s *Session) Credits() uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.credits
}

// Acknowledge signals that the runtime has cooperatively handled cancellation.
// This unblocks Cancel() and results in CancelAcknowledged.
// Safe to call concurrently and multiple times.
func (s *Session) Acknowledge() {
	s.doneOnce.Do(func() {
		close(s.doneCh)
	})
}

// CancelCtx returns the session's cancellation context.
// The runtime should watch this to detect cancellation signals.
func (s *Session) CancelCtx() context.Context {
	return s.cancelCtx
}
