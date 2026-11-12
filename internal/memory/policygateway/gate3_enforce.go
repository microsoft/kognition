// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package policygateway

import (
	"context"
	"errors"
	"fmt"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
)

var (
	// ErrRuntimeApprovalPending indicates that Gate 3 resolved a durable
	// Pending decision. The caller must return a tagged 403 response so the
	// runtime can either wait for a human decision or retry with the same
	// InvocationID.
	ErrRuntimeApprovalPending = errors.New("runtime approval pending")
	// ErrRuntimeApprovalRejected indicates that a human explicitly denied
	// this exact bound memory action.
	ErrRuntimeApprovalRejected = errors.New("runtime approval rejected")
	// ErrRuntimeApprovalExpired indicates that the RuntimeApproval expired
	// before a terminal decision was recorded.
	ErrRuntimeApprovalExpired = errors.New("runtime approval expired")
	// ErrRuntimeApprovalUnavailable indicates that Gate 3 could not evaluate
	// the request (resolver error, invalid state, or transient failure).
	ErrRuntimeApprovalUnavailable = errors.New("runtime approval unavailable")
)

// enforceGate3 runs the ADR-0014 Runtime Autonomy Gate 3 evaluation for a
// memory-plane request. It returns nil when Gate 3 is disabled, when the
// mode does not require Gate 3 enforcement (ReadOnly operations), when the
// request builder opts out, or when the resolver returns Ready. It returns
// a sentinel error for Pending / Rejected / Expired outcomes and wraps
// ErrRuntimeApprovalUnavailable for resolver errors and unexpected states.
//
// enforceGate3 is intentionally free of audit and response side effects.
// The caller is responsible for translating a sentinel error into the
// tagged HTTP response and denial audit entry.
func (s *Server) enforceGate3(ctx context.Context, mode string, invocation Gate3Invocation) error {
	_, err := s.resolveGate3(ctx, mode, invocation)
	return err
}

func (s *Server) resolveGate3(ctx context.Context, mode string, invocation Gate3Invocation) (*v1alpha1.RuntimeApproval, error) {
	if s.gate3 == nil || s.gate3Builder == nil {
		return nil, nil
	}
	if mode != "ReadWrite" {
		return nil, nil
	}
	request, ok := s.gate3Builder(ctx, invocation)
	if !ok {
		return nil, fmt.Errorf("%w: request projection failed", ErrRuntimeApprovalUnavailable)
	}
	result, err := s.gate3.Resolve(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRuntimeApprovalUnavailable, err)
	}
	switch result.State {
	case runtimeapproval.Gate3StateReady:
		return &result.Approval, nil
	case runtimeapproval.Gate3StatePending:
		return &result.Approval, ErrRuntimeApprovalPending
	case runtimeapproval.Gate3StateRejected:
		return &result.Approval, ErrRuntimeApprovalRejected
	case runtimeapproval.Gate3StateExpired:
		return &result.Approval, ErrRuntimeApprovalExpired
	default:
		return nil, fmt.Errorf("%w: unexpected gate3 state %q", ErrRuntimeApprovalUnavailable, result.State)
	}
}
