// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package policygateway

import (
	"context"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/kognition-ai/kognition/internal/store"
)

// Gate3Resolver is the minimum Gate 3 surface the Memory Policy Gateway
// consumes. The production *runtimeapproval.Gate3Coordinator satisfies this
// directly; tests substitute a stub without pulling in the full
// RuntimeApproval store.
type Gate3Resolver interface {
	Resolve(ctx context.Context, request runtimeapproval.Gate3Request) (runtimeapproval.Gate3Result, error)
}

// Gate3Invocation carries the authenticated memory operation context passed
// to a Gate3RequestBuilder. Only write-mode operations (store, delete)
// exercise Gate 3; read paths bypass the projection entirely. The
// AgentIdentity, AgentInstance, and MemoryClaim are the resource
// incarnations that the policy gateway already loaded and authorized: the
// builder projects them directly onto the ADR-0014 approval target rather
// than reloading them from the store.
type Gate3Invocation struct {
	Namespace        string
	Operation        string
	InvocationID     string
	RunID            string
	Claims           *runtimeauth.MemoryCapabilityClaims
	Identity         v1alpha1.AgentIdentity
	Instance         v1alpha1.AgentInstance
	Claim            v1alpha1.MemoryClaim
	VolumeHandle     string
	CanonicalPayload []byte
}

// Gate3RequestBuilder maps a Memory Policy Gateway invocation onto an
// ADR-0014 Gate 3 request. Returning ok=false skips Gate 3 for this
// invocation (for example, when the projection cannot be canonicalized).
// The context carries the active request deadline; production builders that
// perform additional lookups must honor it.
type Gate3RequestBuilder func(context.Context, Gate3Invocation) (runtimeapproval.Gate3Request, bool)

// WithGate3Coordinator enables ADR-0014 Runtime Autonomy Gate 3 enforcement
// for every write-mode memory operation. A matching Gate3RequestBuilder must
// also be supplied via WithGate3RequestBuilder or Gate 3 is skipped.
func WithGate3Coordinator(coordinator Gate3Resolver) Option {
	return func(s *Server) { s.gate3 = coordinator }
}

// WithGate3RequestBuilder configures how a Memory Policy Gateway invocation
// is projected onto a Gate3Request. Returning ok=false from the builder
// skips Gate 3 for that invocation.
func WithGate3RequestBuilder(builder Gate3RequestBuilder) Option {
	return func(s *Server) { s.gate3Builder = builder }
}

// WithRuntimeApprovalExecutionStore enables durable Prepare, Claim, and
// Finalize transitions around approval-bound memory mutations.
func WithRuntimeApprovalExecutionStore(executionStore store.RuntimeApprovalExecutionStore) Option {
	return func(s *Server) { s.runtimeApprovals = executionStore }
}

// WithMemoryMutationReceiptStore enables durable pre-claim and terminal
// receipts for approval-bound Memory store/delete mutations.
func WithMemoryMutationReceiptStore(receiptStore store.MemoryMutationReceiptStore) Option {
	return func(s *Server) { s.memoryReceipts = receiptStore }
}

func WithClaimant(claimant v1alpha1.RuntimeApprovalClaimant) Option {
	return func(s *Server) { s.claimant = claimant }
}

func WithClock(now func() time.Time) Option {
	return func(s *Server) {
		if now != nil {
			s.now = now
		}
	}
}

func WithPreparationTokenFactory(newToken func() (string, error)) Option {
	return func(s *Server) {
		if newToken != nil {
			s.newPrepToken = newToken
		}
	}
}

func WithPreparationWindow(window time.Duration) Option {
	return func(s *Server) { s.preparationWindow = window }
}
