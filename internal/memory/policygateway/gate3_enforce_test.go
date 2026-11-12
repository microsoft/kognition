// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package policygateway

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
)

type stubGate3Resolver struct {
	result  runtimeapproval.Gate3Result
	err     error
	calls   int
	lastReq runtimeapproval.Gate3Request
}

func (s *stubGate3Resolver) Resolve(_ context.Context, request runtimeapproval.Gate3Request) (runtimeapproval.Gate3Result, error) {
	s.calls++
	s.lastReq = request
	return s.result, s.err
}

func gate3EnforceTestInvocation() Gate3Invocation {
	return Gate3Invocation{
		Namespace:    "team-a",
		Operation:    "store",
		InvocationID: "invocation-1",
		RunID:        "run-1",
		Claims: &runtimeauth.MemoryCapabilityClaims{
			Namespace:         "team-a",
			AgentName:         "planner",
			AgentIdentityName: "agent-identity-planner",
			InstanceName:      "planner-instance",
			InstanceUID:       "instance-uid",
			SessionID:         "session-1",
			VolumeHandle:      "vol-planner",
		},
		Identity: v1alpha1.AgentIdentity{
			ObjectMeta: v1alpha1.ObjectMeta{
				Namespace:       "team-a",
				Name:            "agent-identity-planner",
				UID:             "identity-uid",
				ResourceVersion: 3,
			},
		},
		Instance: v1alpha1.AgentInstance{
			ObjectMeta: v1alpha1.ObjectMeta{
				Namespace:       "team-a",
				Name:            "planner-instance",
				UID:             "instance-uid",
				ResourceVersion: 5,
			},
		},
		Claim: v1alpha1.MemoryClaim{
			ObjectMeta: v1alpha1.ObjectMeta{
				Namespace:       "team-a",
				Name:            "planner-claim",
				UID:             "claim-uid",
				ResourceVersion: 11,
			},
		},
		VolumeHandle:     "vol-planner",
		CanonicalPayload: []byte(`{"volume_handle":"vol-planner","content":"hi"}`),
	}
}

func gate3EnforceReadyResult() runtimeapproval.Gate3Result {
	return runtimeapproval.Gate3Result{
		State: runtimeapproval.Gate3StateReady,
		Approval: v1alpha1.RuntimeApproval{
			ObjectMeta: v1alpha1.ObjectMeta{
				Name:            "invocation-1",
				Namespace:       "team-a",
				UID:             "uid-ready-1",
				ResourceVersion: 7,
			},
			Spec:   v1alpha1.RuntimeApprovalSpec{InvocationID: "invocation-1"},
			Status: v1alpha1.RuntimeApprovalStatus{DecisionState: v1alpha1.RuntimeApprovalDecisionNotRequired},
		},
		ExpiresAt: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

func TestEnforceGate3ReturnsNilWhenDisabled(t *testing.T) {
	s := &Server{}
	if err := s.enforceGate3(context.Background(), "ReadWrite", gate3EnforceTestInvocation()); err != nil {
		t.Fatalf("expected nil when Gate 3 is disabled, got %v", err)
	}
}

func TestEnforceGate3ReturnsNilWhenBuilderNil(t *testing.T) {
	resolver := &stubGate3Resolver{result: gate3EnforceReadyResult()}
	s := &Server{gate3: resolver}
	if err := s.enforceGate3(context.Background(), "ReadWrite", gate3EnforceTestInvocation()); err != nil {
		t.Fatalf("expected nil when builder is nil, got %v", err)
	}
	if resolver.calls != 0 {
		t.Fatalf("resolver must not be called when builder is nil (calls=%d)", resolver.calls)
	}
}

func TestEnforceGate3SkipsResolverForReadOnlyMode(t *testing.T) {
	resolver := &stubGate3Resolver{result: gate3EnforceReadyResult()}
	s := &Server{gate3: resolver, gate3Builder: NewGate3RequestBuilder()}
	if err := s.enforceGate3(context.Background(), "ReadOnly", gate3EnforceTestInvocation()); err != nil {
		t.Fatalf("expected nil for ReadOnly mode, got %v", err)
	}
	if resolver.calls != 0 {
		t.Fatalf("resolver must not be called for ReadOnly mode (calls=%d)", resolver.calls)
	}
}

func TestEnforceGate3FailsClosedWhenBuilderCannotProject(t *testing.T) {
	resolver := &stubGate3Resolver{result: gate3EnforceReadyResult()}
	builder := func(context.Context, Gate3Invocation) (runtimeapproval.Gate3Request, bool) {
		return runtimeapproval.Gate3Request{}, false
	}
	s := &Server{gate3: resolver, gate3Builder: builder}
	if err := s.enforceGate3(context.Background(), "ReadWrite", gate3EnforceTestInvocation()); !errors.Is(err, ErrRuntimeApprovalUnavailable) {
		t.Fatalf("error = %v, want ErrRuntimeApprovalUnavailable", err)
	}
	if resolver.calls != 0 {
		t.Fatalf("resolver must not be called when builder opts out (calls=%d)", resolver.calls)
	}
}

func TestEnforceGate3ReturnsNilOnReady(t *testing.T) {
	resolver := &stubGate3Resolver{result: gate3EnforceReadyResult()}
	s := &Server{gate3: resolver, gate3Builder: NewGate3RequestBuilder()}
	if err := s.enforceGate3(context.Background(), "ReadWrite", gate3EnforceTestInvocation()); err != nil {
		t.Fatalf("expected nil for Ready, got %v", err)
	}
	if resolver.calls != 1 {
		t.Fatalf("expected resolver to be called once, got %d", resolver.calls)
	}
	if resolver.lastReq.Plane != v1alpha1.RuntimeApprovalPlaneMemory {
		t.Fatalf("expected plane %q, got %q", v1alpha1.RuntimeApprovalPlaneMemory, resolver.lastReq.Plane)
	}
	if resolver.lastReq.Action != runtimeapproval.ActionMemoryWrite {
		t.Fatalf("expected action %q, got %q", runtimeapproval.ActionMemoryWrite, resolver.lastReq.Action)
	}
}

func TestEnforceGate3ReturnsPendingSentinel(t *testing.T) {
	resolver := &stubGate3Resolver{result: runtimeapproval.Gate3Result{State: runtimeapproval.Gate3StatePending}}
	s := &Server{gate3: resolver, gate3Builder: NewGate3RequestBuilder()}
	err := s.enforceGate3(context.Background(), "ReadWrite", gate3EnforceTestInvocation())
	if !errors.Is(err, ErrRuntimeApprovalPending) {
		t.Fatalf("expected ErrRuntimeApprovalPending, got %v", err)
	}
}

func TestEnforceGate3ReturnsRejectedSentinel(t *testing.T) {
	resolver := &stubGate3Resolver{result: runtimeapproval.Gate3Result{State: runtimeapproval.Gate3StateRejected}}
	s := &Server{gate3: resolver, gate3Builder: NewGate3RequestBuilder()}
	err := s.enforceGate3(context.Background(), "ReadWrite", gate3EnforceTestInvocation())
	if !errors.Is(err, ErrRuntimeApprovalRejected) {
		t.Fatalf("expected ErrRuntimeApprovalRejected, got %v", err)
	}
}

func TestEnforceGate3ReturnsExpiredSentinel(t *testing.T) {
	resolver := &stubGate3Resolver{result: runtimeapproval.Gate3Result{State: runtimeapproval.Gate3StateExpired}}
	s := &Server{gate3: resolver, gate3Builder: NewGate3RequestBuilder()}
	err := s.enforceGate3(context.Background(), "ReadWrite", gate3EnforceTestInvocation())
	if !errors.Is(err, ErrRuntimeApprovalExpired) {
		t.Fatalf("expected ErrRuntimeApprovalExpired, got %v", err)
	}
}

func TestEnforceGate3WrapsResolverError(t *testing.T) {
	resolver := &stubGate3Resolver{err: errors.New("boom")}
	s := &Server{gate3: resolver, gate3Builder: NewGate3RequestBuilder()}
	err := s.enforceGate3(context.Background(), "ReadWrite", gate3EnforceTestInvocation())
	if !errors.Is(err, ErrRuntimeApprovalUnavailable) || !errors.Is(err, resolver.err) {
		t.Fatalf("expected ErrRuntimeApprovalUnavailable and resolver cause, got %v", err)
	}
}

func TestEnforceGate3WrapsUnexpectedState(t *testing.T) {
	resolver := &stubGate3Resolver{result: runtimeapproval.Gate3Result{State: runtimeapproval.Gate3State("weird")}}
	s := &Server{gate3: resolver, gate3Builder: NewGate3RequestBuilder()}
	err := s.enforceGate3(context.Background(), "ReadWrite", gate3EnforceTestInvocation())
	if !errors.Is(err, ErrRuntimeApprovalUnavailable) {
		t.Fatalf("expected ErrRuntimeApprovalUnavailable, got %v", err)
	}
}
