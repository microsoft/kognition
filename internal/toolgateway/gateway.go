// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package toolgateway provides the shared Agent Gateway Tool invocation core.
package toolgateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/invocation"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/runtime"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/store"
)

var (
	// ErrToolNotFound indicates that the requested Tool no longer exists.
	ErrToolNotFound = errors.New("tool not found")
	// ErrToolVersionMismatch prevents a capability for an earlier Tool revision
	// from invoking a newly changed endpoint or schema.
	ErrToolVersionMismatch = errors.New("tool resource version does not match invocation binding")
	// ErrToolInvocationInProgress prevents a retry from repeating a
	// side-effecting invocation while its original request is still running.
	ErrToolInvocationInProgress = errors.New("tool invocation is already in progress")
	// ErrToolArgumentConstraintsDenied indicates that invocation input did not
	// satisfy the matching ToolGrant's redacted JSON Schema constraints.
	ErrToolArgumentConstraintsDenied = errors.New("tool argument constraints denied")
	// ErrRuntimeApprovalPending indicates that Gate 3 resolved a durable
	// pending RuntimeApproval. Callers must wait for a human decision before
	// retrying with the same InvocationID.
	ErrRuntimeApprovalPending = errors.New("runtime approval pending")
	// ErrRuntimeApprovalRejected indicates that a human explicitly denied
	// this exact bound action. Retries must not upgrade the decision.
	ErrRuntimeApprovalRejected = errors.New("runtime approval rejected")
	// ErrRuntimeApprovalExpired indicates that the RuntimeApproval expired
	// before a human decision was recorded.
	ErrRuntimeApprovalExpired = errors.New("runtime approval expired")
)

// Authorizer evaluates ToolGrant, AgentRole, and enforced AgentPolicy rules.
type Authorizer interface {
	AuthorizeTool(context.Context, string, string, string, v1alpha1.ToolGrantVerb) (AuthorizationDecision, error)
}

// InvocationAuthorizer additionally evaluates the concrete invocation input.
type InvocationAuthorizer interface {
	AuthorizeToolInvocation(context.Context, string, string, string, map[string]any) (AuthorizationDecision, error)
}

// AuthorizationDecision identifies the grants, roles, and policies that
// permitted a Tool invocation. References include resource versions whenever
// they originate from a persisted Kognition resource.
type AuthorizationDecision struct {
	GrantRef   string
	RoleRefs   []string
	PolicyRefs []string
}

// QuotaEnforcer reserves one tool call before it can create a side effect.
type QuotaEnforcer interface {
	ReserveToolCall(context.Context, string) error
}

// Executor executes an already authorized tool request.
type Executor interface {
	Execute(context.Context, runtime.ToolExecutionRequest) (runtime.ToolExecutionResult, error)
}

// Request identifies a single tool invocation at the Agent Gateway boundary.
type Request struct {
	Namespace           string
	Workspace           string
	Principal           string
	AgentNamespace      string
	AgentIdentity       string
	Solution            string
	SolutionRev         int64
	SessionID           string
	RunID               string
	SubjectKind         string
	SubjectName         string
	ToolName            string
	ToolResourceVersion int64
	CapabilityIssued    bool
	CapabilityID        string
	Input               map[string]any
	InvocationID        string
	approvalEvidence    *observability.TrustDecisionApproval
}

// Response contains the invoked tool output and gateway execution metadata.
type Response struct {
	InvocationID  string         `json:"invocation_id"`
	Output        map[string]any `json:"output"`
	ExecutionMode string         `json:"execution_mode"`
	Duration      time.Duration  `json:"duration"`
	ErrorCode     string         `json:"error_code,omitempty"`
	ErrorMessage  string         `json:"error_message,omitempty"`

	// RuntimeApprovalState carries the ADR-0014 tagged Gate 3 outcome that
	// the client must key retry behavior on: "ready" (approved or not
	// required, capability may be used), "pending" (durable decision not
	// yet made, retry with the same invocation ID), "terminal" (rejected,
	// expired, or the execution reached a final outcome the client cannot
	// resume from), or "unavailable" (Gate 3 evaluation could not run and
	// the request was fail-closed). Empty for legacy invocations where
	// Gate 3 was not configured.
	RuntimeApprovalState string `json:"runtime_approval_state,omitempty"`
	// RuntimeApproval is the exact persisted RuntimeApproval reference the
	// tagged state refers to. Omitted when Gate 3 did not persist an
	// approval (unavailable or legacy).
	RuntimeApproval *v1alpha1.RuntimeApprovalReference `json:"runtime_approval,omitempty"`
	// RuntimeApprovalExpiresAt is the decision expiry the client must not
	// retry past. Zero when unknown.
	RuntimeApprovalExpiresAt time.Time `json:"runtime_approval_expires_at,omitempty"`
}

// Tagged transport states per ADR-0014 §"Tool and Other Transport Migration".
const (
	RuntimeApprovalStateReady       = "ready"
	RuntimeApprovalStatePending     = "pending"
	RuntimeApprovalStateTerminal    = "terminal"
	RuntimeApprovalStateUnavailable = "unavailable"
)

// Ledger durably records an authorization decision before execution and the
// eventual outcome after it completes. A ledger failure before execution must
// prevent the side effect.
type Ledger interface {
	RecordStart(context.Context, LedgerEntry) error
	RecordCompletion(context.Context, LedgerEntry) error
}

type decisionLedger interface {
	RecordDecision(context.Context, LedgerEntry) error
}

// LedgerEntry is a durable Tool Gateway invocation lifecycle record.
type LedgerEntry struct {
	InvocationID  string
	Namespace     string
	SubjectKind   string
	SubjectName   string
	Principal     string
	AgentIdentity string
	Solution      string
	SolutionRev   int64
	SessionID     string
	CapabilityID  string
	RunID         string
	ToolRef       string
	ToolVersion   int64
	Verb          string
	GrantRef      string
	RoleRefs      []string
	PolicyRefs    []string
	Status        string
	ReasonCode    string
	Duration      time.Duration
	Approval      *observability.TrustDecisionApproval
}

// Gateway is the parity-preserving Tool Gateway core used in process by
// standalone and controller workflow paths, and behind HTTP in Kubernetes.
type Gateway struct {
	store             store.Store
	authorizer        InvocationAuthorizer
	quota             QuotaEnforcer
	executor          Executor
	ledger            Ledger
	receipts          store.ToolInvocationReceiptStore
	reservations      store.ToolCallReservationStore
	resilience        ResilienceCoordinator
	tracer            invocation.Tracer
	logger            *zap.Logger
	newID             func() string
	gate3             Gate3Resolver
	gate3Builder      Gate3RequestBuilder
	runtimeApprovals  store.RuntimeApprovalExecutionStore
	claimant          v1alpha1.RuntimeApprovalClaimant
	now               func() time.Time
	newPrepToken      func() (string, error)
	preparationWindow time.Duration
}

// Gate3Resolver is the minimum Gate 3 surface the Tool Gateway consumes. The
// production *runtimeapproval.Gate3Coordinator satisfies this directly; tests
// substitute a stub without pulling in the full RuntimeApproval store.
type Gate3Resolver interface {
	Resolve(ctx context.Context, request runtimeapproval.Gate3Request) (runtimeapproval.Gate3Result, error)
}

// Gate3RequestBuilder maps a Tool Gateway Request plus the loaded Tool to the
// ADR-0014 Gate 3 request. Returning ok=false skips Gate 3 for this
// invocation (for example, when the caller supplies neither an approval-bound
// identity nor a runtime autonomy class reference). The context carries the
// active invocation deadline; production builders that resolve AgentIdentity
// or AgentInstance from a shared store must honor it.
type Gate3RequestBuilder func(context.Context, Request, *v1alpha1.Tool) (runtimeapproval.Gate3Request, bool)

// Option configures an optional Tool Gateway dependency.
type Option func(*Gateway)

// WithInvocationTracer records the completed invocation using Kognition's
// existing traceability contract.
func WithInvocationTracer(tracer invocation.Tracer) Option {
	return func(gateway *Gateway) { gateway.tracer = tracer }
}

// WithLedger configures the durable invocation ledger used for write-ahead
// audit records.
func WithLedger(ledger Ledger) Option {
	return func(gateway *Gateway) { gateway.ledger = ledger }
}

// WithResilienceCoordinator overrides the default process-local resilience
// manager. Kubernetes uses a Lease-backed coordinator shared by all replicas.
func WithResilienceCoordinator(coordinator ResilienceCoordinator) Option {
	return func(gateway *Gateway) {
		if coordinator != nil {
			gateway.resilience = coordinator
		}
	}
}

// WithGate3Coordinator enables ADR-0014 Runtime Autonomy Gate 3 enforcement
// for every Tool invocation. A matching Gate3RequestBuilder must also be
// supplied via WithGate3RequestBuilder or Gate 3 is skipped.
func WithGate3Coordinator(coordinator Gate3Resolver) Option {
	return func(gateway *Gateway) { gateway.gate3 = coordinator }
}

// WithGate3RequestBuilder configures how a Tool Gateway Request is projected
// onto a Gate3Request. Returning ok=false from the builder skips Gate 3 for
// that invocation.
func WithGate3RequestBuilder(builder Gate3RequestBuilder) Option {
	return func(gateway *Gateway) { gateway.gate3Builder = builder }
}

// WithRuntimeApprovalExecutionStore wires the ADR-0014 execution-state store
// used to durably Prepare, Claim, and Finalize a RuntimeApproval around every
// approval-bound Tool invocation. When set, every Gate 3 Ready outcome drives
// the plane-receipt lifecycle synchronously with executor dispatch.
func WithRuntimeApprovalExecutionStore(execStore store.RuntimeApprovalExecutionStore) Option {
	return func(gateway *Gateway) { gateway.runtimeApprovals = execStore }
}

// WithClaimant overrides the durable gateway identity attached to every
// successful ClaimRuntimeApprovalExecution call. ClaimedAt and LastHeartbeat
// on the supplied value are ignored; both are stamped at claim time using the
// gateway clock.
func WithClaimant(claimant v1alpha1.RuntimeApprovalClaimant) Option {
	return func(gateway *Gateway) { gateway.claimant = claimant }
}

// WithClock overrides the gateway monotonic clock. Tests use this to inject a
// deterministic time source when asserting preparation deadlines.
func WithClock(now func() time.Time) Option {
	return func(gateway *Gateway) {
		if now != nil {
			gateway.now = now
		}
	}
}

// WithPreparationTokenFactory overrides how preparation tokens are generated.
// Tests use this to inject a deterministic token stream.
func WithPreparationTokenFactory(newToken func() (string, error)) Option {
	return func(gateway *Gateway) {
		if newToken != nil {
			gateway.newPrepToken = newToken
		}
	}
}

// WithPreparationWindow overrides the preparation window applied to the
// approval Prepare deadline. Values that exceed the ADR-0014 cap are clamped
// down to the cap at Prepare time.
func WithPreparationWindow(window time.Duration) Option {
	return func(gateway *Gateway) { gateway.preparationWindow = window }
}

// New creates a Tool Gateway. The authorization and execution dependencies
// are mandatory because this gateway must fail closed rather than bypass a
// policy or call a tool directly.
func New(st store.Store, authorizer Authorizer, quota QuotaEnforcer, executor Executor, logger *zap.Logger, options ...Option) (*Gateway, error) {
	if st == nil {
		return nil, errors.New("tool gateway requires a store")
	}
	if authorizer == nil {
		return nil, errors.New("tool gateway requires an authorizer")
	}
	invocationAuthorizer, ok := authorizer.(InvocationAuthorizer)
	if !ok {
		return nil, errors.New("tool gateway requires an invocation-aware authorizer")
	}
	if executor == nil {
		return nil, errors.New("tool gateway requires an executor")
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	gateway := &Gateway{store: st, authorizer: invocationAuthorizer, quota: quota, executor: executor, resilience: newResilienceManager(), logger: logger.Named("tool-gateway"), newID: uuid.NewString, now: time.Now, newPrepToken: runtimeapproval.GeneratePreparationToken, preparationWindow: runtimeapproval.MaxPreparationDeadline}
	for _, option := range options {
		option(gateway)
	}
	if gateway.ledger == nil {
		return nil, errors.New("tool gateway requires a durable invocation ledger")
	}
	receipts, ok := st.(store.ToolInvocationReceiptStore)
	if !ok {
		return nil, errors.New("tool gateway requires a durable invocation receipt store")
	}
	gateway.receipts = receipts
	gateway.reservations, _ = st.(store.ToolCallReservationStore)
	if gateway.preparationWindow <= 0 || gateway.preparationWindow > runtimeapproval.MaxPreparationDeadline {
		gateway.preparationWindow = runtimeapproval.MaxPreparationDeadline
	}
	if gateway.runtimeApprovals != nil {
		if err := gateway.ensureClaimantDefaults(); err != nil {
			return nil, err
		}
	}
	return gateway, nil
}

// ensureClaimantDefaults fills required RuntimeApprovalClaimant identity
// fields when the caller did not supply an explicit claimant. WorkloadUID
// falls back to the process hostname and ProcessStartNonce is generated once
// per gateway instance so that every claim from this process shares the same
// incarnation identity.
func (g *Gateway) ensureClaimantDefaults() error {
	if g.claimant.WorkloadUID == "" {
		g.claimant.WorkloadUID = os.Getenv("KOGNITION_RUNTIME_APPROVAL_WORKLOAD_UID")
		if g.claimant.WorkloadUID == "" {
			hostname, err := os.Hostname()
			if err != nil || hostname == "" {
				hostname = "tool-gateway"
			}
			g.claimant.WorkloadUID = hostname
		}
	}
	if g.claimant.ProcessStartNonce == "" {
		nonceBytes := make([]byte, 16)
		if _, err := rand.Read(nonceBytes); err != nil {
			return fmt.Errorf("tool gateway process start nonce: %w", err)
		}
		g.claimant.ProcessStartNonce = base64.RawURLEncoding.EncodeToString(nonceBytes)
	}
	return nil
}

// Invoke resolves the live Tool resource, verifies its expected revision,
// enforces policy and quota, then executes exactly once.
func (g *Gateway) Invoke(ctx context.Context, request Request) (Response, error) {
	if request.Namespace == "" || request.Principal == "" || request.ToolName == "" {
		return Response{}, errors.New("tool invocation requires namespace, principal, and tool name")
	}
	if request.InvocationID == "" {
		request.InvocationID = g.newID()
	}
	tool, err := loadTool(ctx, g.store, request.Namespace, request.ToolName)
	if err != nil {
		return Response{InvocationID: request.InvocationID}, err
	}
	if request.ToolResourceVersion > 0 && tool.ObjectMeta.ResourceVersion != request.ToolResourceVersion {
		return Response{InvocationID: request.InvocationID}, fmt.Errorf("%w: expected %d, got %d", ErrToolVersionMismatch, request.ToolResourceVersion, tool.ObjectMeta.ResourceVersion)
	}
	receipt := g.newReceipt(request, tool)
	claimed, existing, err := g.receipts.ClaimToolInvocation(ctx, receipt)
	if err != nil {
		return Response{InvocationID: request.InvocationID}, fmt.Errorf("claim tool invocation receipt: %w", err)
	}
	replay := !claimed
	if replay {
		receipt = existing
		terminal := receipt.Status == store.ToolInvocationSucceeded || receipt.Status == store.ToolInvocationFailed
		if terminal && (g.gate3 == nil || g.gate3Builder == nil) {
			recordResilience("replay")
			request.approvalEvidence = &observability.TrustDecisionApproval{Status: "not_required"}
			if err := g.recordDecision(ctx, g.ledgerEntry(request, tool, AuthorizationDecision{}, "replayed", 0)); err != nil {
				return Response{InvocationID: request.InvocationID}, err
			}
			return responseFromReceipt(receipt)
		}
	}
	decision, err := g.authorizer.AuthorizeToolInvocation(ctx, request.Namespace, request.Principal, request.ToolName, request.Input)
	if err != nil {
		if receipt.Status == store.ToolInvocationPrepared {
			return Response{InvocationID: request.InvocationID, RuntimeApprovalState: RuntimeApprovalStateUnavailable}, errors.Join(ErrToolInvocationInProgress, err)
		}
		recordInvocation("authorization_denied", 0)
		g.record(ctx, request, tool, invocation.ResultError, nil, 0)
		if decisionErr := g.recordAuthorizationDenial(ctx, request, tool, decision, err); decisionErr != nil {
			return Response{InvocationID: request.InvocationID}, decisionErr
		}
		_ = g.completeReceipt(ctx, receipt, store.ToolInvocationFailed, nil, "", 0, err)
		return Response{InvocationID: request.InvocationID}, fmt.Errorf("authorize tool invocation: %w", err)
	}
	approval, tag, gate3Err := g.enforceGate3(ctx, request, tool, receipt, claimed)
	request.approvalEvidence = observability.TrustApproval(approval)
	if gate3Err != nil {
		return tag.apply(Response{InvocationID: request.InvocationID}), gate3Err
	}
	approvalPrepared := approval != nil && approval.Status.ExecutionState == v1alpha1.RuntimeApprovalExecutionNotStarted && approval.Status.PreparationToken != "" && approval.Status.PreparationDeadline != nil
	if replay && receipt.Status != store.ToolInvocationPrepared && !approvalPrepared {
		recordResilience("replay")
		if err := g.recordDecision(ctx, g.ledgerEntry(request, tool, AuthorizationDecision{}, "replayed", 0)); err != nil {
			return tag.apply(Response{InvocationID: request.InvocationID}), err
		}
		response, err := responseFromReceipt(receipt)
		return tag.apply(response), err
	}
	durablePreparation := approval != nil && g.runtimeApprovals != nil
	if receipt.Status == store.ToolInvocationPrepared && !durablePreparation {
		return tag.apply(Response{InvocationID: request.InvocationID, RuntimeApprovalState: RuntimeApprovalStateUnavailable}), ErrToolInvocationInProgress
	}
	if !durablePreparation && g.quota != nil {
		if err := g.quota.ReserveToolCall(ctx, request.Namespace); err != nil {
			recordInvocation("quota_denied", 0)
			g.record(ctx, request, tool, invocation.ResultError, nil, 0)
			entry := g.ledgerEntry(request, tool, decision, "quota_denied", 0)
			entry.ReasonCode = "quota_exhausted"
			if decisionErr := g.recordDecision(ctx, entry); decisionErr != nil {
				return tag.apply(Response{InvocationID: request.InvocationID}), decisionErr
			}
			_ = g.completeReceipt(ctx, receipt, store.ToolInvocationFailed, nil, "", 0, err)
			return tag.apply(Response{InvocationID: request.InvocationID}), fmt.Errorf("reserve tool-call quota: %w", err)
		}
	}
	entry := g.ledgerEntry(request, tool, decision, "authorized", 0)
	var claimedApproval *v1alpha1.RuntimeApproval
	var preparationToken string
	var prepErr error
	if durablePreparation {
		claimedApproval, preparationToken, prepErr = g.prepareAndClaimApproval(ctx, approval, receipt, entry)
	} else {
		prepErr = g.ledger.RecordStart(ctx, entry)
	}
	if prepErr != nil {
		recordInvocation("runtime_approval_prepare_failed", 0)
		g.record(ctx, request, tool, invocation.ResultError, nil, 0)
		if durablePreparation {
			if errors.Is(prepErr, store.ErrQuotaExceeded) {
				recordInvocation("quota_denied", 0)
				entry := g.ledgerEntry(request, tool, decision, "quota_denied", 0)
				entry.ReasonCode = "quota_exhausted"
				if decisionErr := g.recordDecision(ctx, entry); decisionErr != nil {
					return tag.apply(Response{InvocationID: request.InvocationID, RuntimeApprovalState: RuntimeApprovalStateUnavailable}), errors.Join(prepErr, decisionErr)
				}
				if receiptErr := g.completeReceipt(ctx, receipt, store.ToolInvocationFailed, nil, "", 0, prepErr); receiptErr != nil {
					return tag.apply(Response{InvocationID: request.InvocationID, RuntimeApprovalState: RuntimeApprovalStateUnavailable}), errors.Join(prepErr, receiptErr)
				}
				return tag.apply(Response{InvocationID: request.InvocationID, RuntimeApprovalState: RuntimeApprovalStateTerminal}), prepErr
			}
			return tag.apply(Response{InvocationID: request.InvocationID, RuntimeApprovalState: RuntimeApprovalStateUnavailable}), prepErr
		}
		_ = g.completeReceipt(ctx, receipt, store.ToolInvocationFailed, nil, "", 0, prepErr)
		if ledgerErr := g.ledger.RecordCompletion(ctx, g.ledgerEntry(request, tool, decision, string(invocation.ResultError), 0)); ledgerErr != nil {
			g.logger.Error("record tool invocation completion failed", zap.Error(ledgerErr), zap.String("invocation", request.InvocationID))
		}
		return tag.apply(Response{InvocationID: request.InvocationID}), prepErr
	}
	if ownershipErr := g.verifyClaimOwnership(ctx, claimedApproval); ownershipErr != nil {
		return tag.apply(Response{InvocationID: request.InvocationID, RuntimeApprovalState: RuntimeApprovalStateUnavailable}), ownershipErr
	}

	started := time.Now()
	result, err := g.executeWithResilience(ctx, tool, request.Input, request.InvocationID)
	duration := time.Since(started)
	if result.Duration > 0 {
		duration = result.Duration
	}
	if state, _ := result.Output["runtime_approval_state"].(string); result.CompletionUnconfirmed || strings.EqualFold(state, "Unavailable") || strings.EqualFold(state, "Pending") {
		return tag.apply(Response{InvocationID: request.InvocationID, Duration: duration, ErrorCode: "ToolOutcomeUnconfirmed", ErrorMessage: "Nested operation completion is unconfirmed; retain the claimed invocation.", RuntimeApprovalState: RuntimeApprovalStateUnavailable}), errors.New("tool operation completion is unconfirmed")
	}
	if settlementErr := g.settleApprovalReservation(ctx, receipt, preparationToken); settlementErr != nil {
		g.logger.Error("settle claimed Tool-call reservation failed", zap.Error(settlementErr), zap.String("invocation", receipt.InvocationID), zap.String("namespace", receipt.Namespace))
		return tag.apply(Response{InvocationID: request.InvocationID, Output: result.Output, ExecutionMode: result.ExecutionMode, Duration: duration, RuntimeApprovalState: RuntimeApprovalStateUnavailable}), settlementErr
	}
	if err != nil {
		status := invocation.ResultError
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			status = invocation.ResultTimeout
		}
		g.record(ctx, request, tool, status, nil, duration)
		recordInvocation(string(status), duration)
		if finalizeErr := g.finalizeApproval(ctx, claimedApproval, runtimeapproval.ExecutionDispositionFailed); finalizeErr != nil {
			g.logger.Error("finalize runtime approval failed", zap.Error(finalizeErr), zap.String("invocation", request.InvocationID))
			return tag.apply(Response{InvocationID: request.InvocationID, Duration: duration, RuntimeApprovalState: RuntimeApprovalStateUnavailable}), errors.Join(err, finalizeErr)
		}
		if receiptErr := g.completeReceipt(ctx, receipt, store.ToolInvocationFailed, nil, "", duration, err); receiptErr != nil {
			return tag.apply(Response{InvocationID: request.InvocationID, Duration: duration}), errors.Join(err, receiptErr)
		}
		if ledgerErr := g.ledger.RecordCompletion(ctx, g.ledgerEntry(request, tool, decision, string(status), duration)); ledgerErr != nil {
			g.logger.Error("record tool invocation completion failed", zap.Error(ledgerErr), zap.String("invocation", request.InvocationID))
			return tag.apply(Response{InvocationID: request.InvocationID, Duration: duration}), errors.Join(err, fmt.Errorf("record tool invocation completion: %w", ledgerErr))
		}
		return tag.apply(Response{InvocationID: request.InvocationID, Duration: duration}), err
	}
	g.record(ctx, request, tool, invocation.ResultSuccess, result.Output, duration)
	recordInvocation("success", duration)
	if finalizeErr := g.finalizeApproval(ctx, claimedApproval, runtimeapproval.ExecutionDispositionSucceeded); finalizeErr != nil {
		g.logger.Error("finalize runtime approval failed", zap.Error(finalizeErr), zap.String("invocation", request.InvocationID))
		return tag.apply(Response{InvocationID: request.InvocationID, Output: result.Output, ExecutionMode: result.ExecutionMode, Duration: duration, RuntimeApprovalState: RuntimeApprovalStateUnavailable}), finalizeErr
	}
	if receiptErr := g.completeReceipt(ctx, receipt, store.ToolInvocationSucceeded, result.Output, result.ExecutionMode, duration, nil); receiptErr != nil {
		return tag.apply(Response{InvocationID: request.InvocationID, Output: result.Output, ExecutionMode: result.ExecutionMode, Duration: duration}), receiptErr
	}
	if ledgerErr := g.ledger.RecordCompletion(ctx, g.ledgerEntry(request, tool, decision, string(invocation.ResultSuccess), duration)); ledgerErr != nil {
		recordResilience("audit_completion_failed")
		g.logger.Error("record tool invocation completion failed", zap.Error(ledgerErr), zap.String("invocation", request.InvocationID))
		auditErr := &runtime.StructuredToolError{Code: "ToolAuditCompletionFailed", Message: "tool executed but completion audit could not be persisted", Retryable: false}
		return tag.apply(Response{InvocationID: request.InvocationID, Output: result.Output, ExecutionMode: result.ExecutionMode, Duration: duration, ErrorCode: auditErr.Code, ErrorMessage: auditErr.Message, RuntimeApprovalState: RuntimeApprovalStateUnavailable}), auditErr
	}
	return tag.apply(Response{InvocationID: request.InvocationID, Output: result.Output, ExecutionMode: result.ExecutionMode, Duration: duration}), nil
}

func (g *Gateway) executeWithResilience(ctx context.Context, tool *v1alpha1.Tool, input map[string]any, invocationID string) (runtime.ToolExecutionResult, error) {
	policy := tool.Spec.ExecutionPolicy
	key := toolResilienceKey(tool)
	release, err := g.resilience.Acquire(ctx, key, policy, time.Now())
	if err != nil {
		if errors.Is(err, ErrToolCircuitOpen) {
			recordResilience("circuit_open")
		} else if errors.Is(err, ErrToolConcurrencyLimit) {
			recordResilience("concurrency_limit")
		}
		return runtime.ToolExecutionResult{}, &runtime.StructuredToolError{Code: "ToolConcurrencyOrCircuitOpen", Message: err.Error(), Retryable: true}
	}

	var result runtime.ToolExecutionResult
	for attempt := 1; attempt <= executionAttempts(policy); attempt++ {
		invokeCtx := ctx
		cancel := func() {}
		if timeout := executionTimeout(policy); timeout > 0 {
			invokeCtx, cancel = context.WithTimeout(ctx, timeout)
		}
		result, err = g.executor.Execute(invokeCtx, runtime.ToolExecutionRequest{InvocationID: invocationID, Tool: *tool, Input: input})
		timedOut := errors.Is(invokeCtx.Err(), context.DeadlineExceeded)
		cancel()
		if err == nil {
			if coordinationErr := g.finishResilience(ctx, release, key, policy, true, false); coordinationErr != nil {
				return result, coordinationErr
			}
			return result, nil
		}
		if timedOut {
			err = &runtime.StructuredToolError{Code: "ToolGatewayTimeout", Message: fmt.Sprintf("gateway invocation exceeded timeout %s", executionTimeout(policy)), Retryable: true}
		}
		transient := isTransientToolError(err)
		if !transient || attempt == executionAttempts(policy) {
			if transient && policy != nil && policy.CircuitBreaker != nil {
				recordResilience("transient_failure")
			}
			if coordinationErr := g.finishResilience(ctx, release, key, policy, false, transient); coordinationErr != nil {
				return runtime.ToolExecutionResult{}, errors.Join(err, coordinationErr)
			}
			return runtime.ToolExecutionResult{}, err
		}
	}
	releaseCtx, cancelRelease := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancelRelease()
	if releaseErr := release(releaseCtx); releaseErr != nil {
		return runtime.ToolExecutionResult{}, releaseErr
	}
	return runtime.ToolExecutionResult{}, err
}

func (g *Gateway) finishResilience(ctx context.Context, release func(context.Context) error, key string, policy *v1alpha1.ToolExecutionPolicy, succeeded, transient bool) error {
	recordCtx, cancelRecord := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	recordErr := g.resilience.Record(recordCtx, key, policy, succeeded, transient, time.Now())
	cancelRecord()
	releaseCtx, cancelRelease := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancelRelease()
	if recordErr != nil {
		recordResilience("coordination_failure")
		if releaseErr := release(releaseCtx); releaseErr != nil {
			return errors.Join(recordErr, releaseErr)
		}
		return recordErr
	}
	if err := release(releaseCtx); err != nil {
		recordResilience("coordination_failure")
		return err
	}
	return nil
}

func (g *Gateway) newReceipt(request Request, tool *v1alpha1.Tool) store.ToolInvocationReceipt {
	binding, _ := json.Marshal(struct {
		Principal string         `json:"principal"`
		Subject   string         `json:"subject"`
		Tool      string         `json:"tool"`
		Version   int64          `json:"version"`
		Input     map[string]any `json:"input"`
	}{request.Principal, request.SubjectKind + "/" + request.SubjectName, tool.ObjectMeta.Name, tool.ObjectMeta.ResourceVersion, request.Input})
	digest := sha256.Sum256(binding)
	return store.ToolInvocationReceipt{
		Namespace: request.Namespace, InvocationID: request.InvocationID, Binding: fmt.Sprintf("%x", digest[:]),
		Status: store.ToolInvocationPending, CreatedAt: time.Now().UTC(),
	}
}

func (g *Gateway) completeReceipt(ctx context.Context, receipt store.ToolInvocationReceipt, status store.ToolInvocationReceiptStatus, output map[string]any, executionMode string, duration time.Duration, invocationErr error) error {
	if output != nil {
		encoded, err := json.Marshal(output)
		if err != nil {
			return fmt.Errorf("marshal tool invocation receipt output: %w", err)
		}
		receipt.Output = encoded
	}
	now := time.Now().UTC()
	receipt.Status, receipt.ExecutionMode, receipt.DurationMS, receipt.CompletedAt = status, executionMode, duration.Milliseconds(), &now
	if invocationErr != nil {
		receipt.ErrorMessage = invocationErr.Error()
	}
	if err := g.receipts.CompleteToolInvocation(ctx, receipt); err != nil {
		return fmt.Errorf("complete tool invocation receipt: %w", err)
	}
	return nil
}

func responseFromReceipt(receipt store.ToolInvocationReceipt) (Response, error) {
	response := Response{InvocationID: receipt.InvocationID, ExecutionMode: receipt.ExecutionMode, Duration: time.Duration(receipt.DurationMS) * time.Millisecond}
	switch receipt.Status {
	case store.ToolInvocationPending:
		response.RuntimeApprovalState = RuntimeApprovalStateUnavailable
		return response, ErrToolInvocationInProgress
	case store.ToolInvocationSucceeded:
		if len(receipt.Output) > 0 && json.Unmarshal(receipt.Output, &response.Output) != nil {
			return Response{InvocationID: receipt.InvocationID}, errors.New("stored tool invocation receipt has invalid output")
		}
		return response, nil
	case store.ToolInvocationFailed:
		return response, fmt.Errorf("previous tool invocation failed: %s", receipt.ErrorMessage)
	default:
		return response, fmt.Errorf("stored tool invocation receipt has invalid status %q", receipt.Status)
	}
}

// InvokeTool implements the existing controller and API tool-invoker contract.
// It preserves standalone and workflow call semantics while ensuring gateway
// ledger events attach to the concrete Agent or Workflow resource.
func (g *Gateway) InvokeTool(ctx context.Context, namespace, principal, toolName string, input map[string]any) (map[string]any, error) {
	subjectKind, subjectName := "Agent", principal
	if workflowName, found := strings.CutPrefix(principal, "workflow:"); found && workflowName != "" {
		subjectKind, subjectName = "Workflow", workflowName
	}
	response, err := g.Invoke(ctx, Request{
		Namespace: namespace, Workspace: namespace, Principal: principal, AgentNamespace: namespace, AgentIdentity: principal, SubjectKind: subjectKind, SubjectName: subjectName, ToolName: toolName, Input: input,
	})
	return response.Output, err
}

func (g *Gateway) ledgerEntry(request Request, tool *v1alpha1.Tool, decision AuthorizationDecision, status string, duration time.Duration) LedgerEntry {
	subjectKind := request.SubjectKind
	if subjectKind == "" {
		subjectKind = "Agent"
	}
	subjectName := request.SubjectName
	if subjectName == "" {
		subjectName = request.Principal
	}
	return LedgerEntry{
		InvocationID: request.InvocationID, Namespace: request.Namespace, SubjectKind: subjectKind, SubjectName: subjectName,
		CapabilityID: request.CapabilityID,
		Approval:     request.approvalEvidence,
		Principal:    request.Principal, AgentIdentity: request.AgentIdentity, Solution: request.Solution, SolutionRev: request.SolutionRev, SessionID: request.SessionID, RunID: request.RunID,
		ToolRef: request.Namespace + "/" + tool.ObjectMeta.Name, ToolVersion: tool.ObjectMeta.ResourceVersion,
		Verb: string(v1alpha1.ToolGrantVerbInvoke), GrantRef: decision.GrantRef, RoleRefs: append([]string(nil), decision.RoleRefs...),
		PolicyRefs: append([]string(nil), decision.PolicyRefs...), Status: status, Duration: duration,
	}
}

func (g *Gateway) recordDecision(ctx context.Context, entry LedgerEntry) error {
	ledger, ok := g.ledger.(decisionLedger)
	if !ok {
		return nil
	}
	if err := ledger.RecordDecision(ctx, entry); err != nil {
		return fmt.Errorf("record tool trust decision: %w", err)
	}
	return nil
}

func (g *Gateway) recordAuthorizationDenial(ctx context.Context, request Request, tool *v1alpha1.Tool, decision AuthorizationDecision, cause error) error {
	status := "denied"
	reasonCode := toolDecisionReasonCode(cause)
	if request.CapabilityIssued && reasonCode == "grant_missing" {
		reasonCode = "grant_revoked"
	}
	if reasonCode == "grant_revoked" {
		status = "revoked"
	}
	entry := g.ledgerEntry(request, tool, decision, status, 0)
	entry.ReasonCode = reasonCode
	entry.Approval = &observability.TrustDecisionApproval{Status: "not_evaluated"}
	return g.recordDecision(ctx, entry)
}

// gate3Tag captures the ADR-0014 tagged-response fields the Tool Gateway
// surfaces to the client alongside the invocation outcome. Empty when Gate 3
// is not configured or opted out for this Tool.
type gate3Tag struct {
	state     string
	reference *v1alpha1.RuntimeApprovalReference
	expiresAt time.Time
	resolved  bool
}

func newGate3Tag(state string, approval v1alpha1.RuntimeApproval, expiresAt time.Time) gate3Tag {
	ref := &v1alpha1.RuntimeApprovalReference{
		Kind:            "RuntimeApproval",
		Namespace:       approval.ObjectMeta.Namespace,
		Name:            approval.ObjectMeta.Name,
		UID:             string(approval.ObjectMeta.UID),
		ResourceVersion: approval.ObjectMeta.ResourceVersion,
	}
	return gate3Tag{state: state, reference: ref, expiresAt: expiresAt, resolved: true}
}

func (t gate3Tag) apply(response Response) Response {
	if t.state != "" && response.RuntimeApprovalState == "" {
		response.RuntimeApprovalState = t.state
	}
	if t.resolved && response.RuntimeApprovalState == "" {
		response.RuntimeApprovalState = RuntimeApprovalStateTerminal
	}
	if t.reference != nil {
		response.RuntimeApproval = t.reference
	}
	if !t.expiresAt.IsZero() {
		response.RuntimeApprovalExpiresAt = t.expiresAt
	}
	return response
}

// enforceGate3 runs the ADR-0014 Runtime Autonomy Gate 3 evaluation between
// Gate 2 authorization and quota reservation. It returns (nil, nil) when
// Gate 3 is disabled, when the builder opts out, or when Gate 3 does not
// produce a persisted RuntimeApproval object to bind to the execution store.
// It returns (&approval, nil) on Ready, providing the caller with the
// RuntimeApproval that must drive the Prepare/Claim/Finalize lifecycle. Any
// Pending/Rejected/Expired outcome terminates the receipt with the matching
// reason code and returns a sentinel error.
func (g *Gateway) enforceGate3(ctx context.Context, request Request, tool *v1alpha1.Tool, receipt store.ToolInvocationReceipt, ownsReceipt bool) (*v1alpha1.RuntimeApproval, gate3Tag, error) {
	if g.gate3 == nil || g.gate3Builder == nil {
		return nil, gate3Tag{}, nil
	}
	gateReq, ok := g.gate3Builder(ctx, request, tool)
	if !ok {
		return nil, gate3Tag{}, nil
	}
	result, err := g.gate3.Resolve(ctx, gateReq)
	if err != nil {
		entry := g.ledgerEntry(request, tool, AuthorizationDecision{}, "denied", 0)
		entry.ReasonCode = "runtime_approval_unavailable"
		entry.Approval = &observability.TrustDecisionApproval{Status: "unavailable"}
		if decisionErr := g.recordDecision(ctx, entry); decisionErr != nil {
			g.logger.Error("record gate3 unavailable decision failed", zap.Error(decisionErr), zap.String("invocation", request.InvocationID))
		}
		if ownsReceipt && receipt.Status == store.ToolInvocationPending {
			receipt.Status = store.ToolInvocationAwaitingApproval
			if receiptErr := g.receipts.CompleteToolInvocation(ctx, receipt); receiptErr != nil {
				err = errors.Join(err, receiptErr)
			}
		}
		return nil, gate3Tag{state: RuntimeApprovalStateUnavailable}, fmt.Errorf("gate3 resolve: %w", err)
	}
	request.approvalEvidence = observability.TrustApproval(&result.Approval)
	switch result.State {
	case runtimeapproval.Gate3StateReady:
		entry := g.ledgerEntry(request, tool, AuthorizationDecision{}, "runtime_approval_ready", 0)
		entry.ReasonCode = gate3ReadyReason(result.Approval)
		if decisionErr := g.recordDecision(ctx, entry); decisionErr != nil {
			g.logger.Error("record gate3 ready decision failed", zap.Error(decisionErr), zap.String("invocation", request.InvocationID))
		}
		approval := result.Approval
		// Ready is a transient Gate 3 outcome; the transport state is set
		// by the /invoke exit path (terminal on completion or unavailable
		// on audit failure). Only propagate the reference and expiry here.
		return &approval, newGate3Tag("", result.Approval, result.ExpiresAt), nil
	case runtimeapproval.Gate3StatePending:
		if !ownsReceipt || receipt.Status == store.ToolInvocationPrepared {
			return nil, newGate3Tag(RuntimeApprovalStateUnavailable, result.Approval, result.ExpiresAt), ErrToolInvocationInProgress
		}
		tag := newGate3Tag(RuntimeApprovalStatePending, result.Approval, result.ExpiresAt)
		return nil, tag, g.parkGate3Pending(ctx, request, tool, receipt)
	case runtimeapproval.Gate3StateRejected:
		tag := newGate3Tag(RuntimeApprovalStateTerminal, result.Approval, result.ExpiresAt)
		return nil, tag, g.finalizeGate3Terminal(ctx, request, tool, receipt, "denied", "runtime_approval_rejected", ErrRuntimeApprovalRejected)
	case runtimeapproval.Gate3StateExpired:
		tag := newGate3Tag(RuntimeApprovalStateTerminal, result.Approval, result.ExpiresAt)
		return nil, tag, g.finalizeGate3Terminal(ctx, request, tool, receipt, "denied", "runtime_approval_expired", ErrRuntimeApprovalExpired)
	default:
		err := fmt.Errorf("unexpected gate3 state %q", result.State)
		if receipt.Status != store.ToolInvocationPrepared {
			_ = g.completeReceipt(ctx, receipt, store.ToolInvocationFailed, nil, "", 0, err)
		}
		return nil, gate3Tag{state: RuntimeApprovalStateUnavailable}, err
	}
}

func (g *Gateway) parkGate3Pending(ctx context.Context, request Request, tool *v1alpha1.Tool, receipt store.ToolInvocationReceipt) error {
	entry := g.ledgerEntry(request, tool, AuthorizationDecision{}, "runtime_approval_pending", 0)
	entry.ReasonCode = "runtime_approval_pending"
	if decisionErr := g.recordDecision(ctx, entry); decisionErr != nil {
		g.logger.Error("record gate3 pending decision failed", zap.Error(decisionErr), zap.String("invocation", request.InvocationID))
	}
	receipt.Status = store.ToolInvocationAwaitingApproval
	if receiptErr := g.receipts.CompleteToolInvocation(ctx, receipt); receiptErr != nil {
		return errors.Join(ErrRuntimeApprovalPending, receiptErr)
	}
	return ErrRuntimeApprovalPending
}

func (g *Gateway) finalizeGate3Terminal(ctx context.Context, request Request, tool *v1alpha1.Tool, receipt store.ToolInvocationReceipt, status, reasonCode string, terminalErr error) error {
	entry := g.ledgerEntry(request, tool, AuthorizationDecision{}, status, 0)
	entry.ReasonCode = reasonCode
	if decisionErr := g.recordDecision(ctx, entry); decisionErr != nil {
		g.logger.Error("record gate3 terminal decision failed", zap.Error(decisionErr), zap.String("invocation", request.InvocationID))
	}
	if receiptErr := g.completeReceipt(ctx, receipt, store.ToolInvocationFailed, nil, "", 0, terminalErr); receiptErr != nil {
		g.logger.Error("complete gate3 terminal receipt failed", zap.Error(receiptErr), zap.String("invocation", request.InvocationID))
	}
	return terminalErr
}

func gate3ReadyReason(approval v1alpha1.RuntimeApproval) string {
	if approval.Status.DecisionState == v1alpha1.RuntimeApprovalDecisionApproved {
		return "runtime_approval_approved"
	}
	return "runtime_approval_not_required"
}

// prepareAndClaimApproval durably transitions a Ready RuntimeApproval through
// the ADR-0014 pre-dispatch sequence: PrepareRuntimeApproval bumps the
// execution state to Prepared, then ClaimRuntimeApprovalExecution stamps the
// gateway claimant and fencing token before the executor is invoked. Returns
// nil, nil when Gate 3 did not produce an approval or the execution store is
// not wired (backward-compatible with non-approval-bound Tools).
func (g *Gateway) prepareAndClaimApproval(ctx context.Context, approval *v1alpha1.RuntimeApproval, receipt store.ToolInvocationReceipt, entry LedgerEntry) (*v1alpha1.RuntimeApproval, string, error) {
	if approval == nil || g.runtimeApprovals == nil {
		return nil, "", nil
	}
	if g.reservations == nil {
		return nil, "", errors.New("prepare runtime approval: durable Tool-call reservation store is unavailable")
	}
	resumed := receipt.Status == store.ToolInvocationPrepared || (approval.Status.ExecutionState == v1alpha1.RuntimeApprovalExecutionNotStarted && approval.Status.PreparationToken != "")
	var token string
	var deadline time.Time
	prepared := *approval
	if resumed {
		if approval.Status.ExecutionState != v1alpha1.RuntimeApprovalExecutionNotStarted || approval.Status.PreparationToken == "" || approval.Status.PreparationDeadline == nil {
			return nil, "", ErrToolInvocationInProgress
		}
		token = approval.Status.PreparationToken
		deadline = *approval.Status.PreparationDeadline
		if receipt.Status == store.ToolInvocationPrepared && (receipt.PreparationToken != token || receipt.PreparationDeadline == nil || !receipt.PreparationDeadline.Equal(deadline)) {
			return nil, "", ErrToolInvocationInProgress
		}
	} else {
		var err error
		token, err = g.newPrepToken()
		if err != nil {
			return nil, "", fmt.Errorf("generate preparation token: %w", err)
		}
		window := g.preparationWindow
		if window <= 0 || window > runtimeapproval.MaxPreparationDeadline {
			window = runtimeapproval.MaxPreparationDeadline
		}
		deadline = g.now().Add(window)
		prepared, err = g.runtimeApprovals.PrepareRuntimeApproval(ctx, approval.ObjectMeta.Namespace, approval.Spec.InvocationID, approval.ObjectMeta.ResourceVersion, token, deadline)
		if err != nil {
			return nil, "", fmt.Errorf("prepare runtime approval: %w", err)
		}
	}
	abortPreparation := func(reason string) {
		if resumed {
			return
		}
		if abortErr := g.abortPreparedApproval(ctx, prepared, token, reason); abortErr != nil {
			g.logger.Warn("abort prepared runtime approval", zap.Error(abortErr), zap.String("invocation", prepared.Spec.InvocationID), zap.String("namespace", prepared.ObjectMeta.Namespace))
			return
		}
		if _, releaseErr := g.reservations.ReleaseToolCallReservation(ctx, receipt.Namespace, receipt.InvocationID, token); releaseErr != nil {
			g.logger.Warn("release prepared Tool-call reservation", zap.Error(releaseErr), zap.String("invocation", receipt.InvocationID), zap.String("namespace", receipt.Namespace))
			return
		}
		if _, releaseErr := g.receipts.ReleasePreparedToolInvocation(ctx, receipt.Namespace, receipt.InvocationID, token); releaseErr != nil {
			g.logger.Warn("release prepared Tool receipt", zap.Error(releaseErr), zap.String("invocation", receipt.InvocationID), zap.String("namespace", receipt.Namespace))
		}
	}
	reservation := store.ToolCallReservation{Namespace: receipt.Namespace, InvocationID: receipt.InvocationID, Binding: receipt.Binding, PreparationToken: token, PreparationDeadline: deadline}
	if _, err := g.reservations.PrepareToolCallReservation(ctx, reservation); err != nil {
		if errors.Is(err, store.ErrQuotaExceeded) && !resumed {
			if abortErr := g.abortPreparedApproval(ctx, prepared, token, "quota_exhausted"); abortErr != nil {
				return nil, "", errors.Join(fmt.Errorf("prepare Tool-call reservation: %w", err), abortErr)
			}
			if _, releaseErr := g.reservations.ReleaseToolCallReservation(ctx, receipt.Namespace, receipt.InvocationID, token); releaseErr != nil {
				return nil, "", errors.Join(fmt.Errorf("prepare Tool-call reservation: %w", err), fmt.Errorf("release Tool-call reservation: %w", releaseErr))
			}
		}
		return nil, "", fmt.Errorf("prepare Tool-call reservation: %w", err)
	}
	preparedReceipt := receipt
	preparedReceipt.Status = store.ToolInvocationPrepared
	preparedReceipt.PreparationToken = token
	preparedReceipt.PreparationDeadline = &deadline
	if _, _, err := g.receipts.PrepareToolInvocation(ctx, preparedReceipt); err != nil {
		return nil, "", fmt.Errorf("prepare Tool invocation receipt: %w", err)
	}
	if err := g.ledger.RecordStart(ctx, entry); err != nil {
		abortPreparation("audit_failed")
		return nil, "", fmt.Errorf("record tool invocation before execution: %w", err)
	}
	claimant := g.claimant
	claimant.ClaimedAt = g.now()
	claimant.LastHeartbeat = claimant.ClaimedAt
	claimed, err := g.runtimeApprovals.ClaimRuntimeApprovalExecution(ctx, prepared.ObjectMeta.Namespace, prepared.Spec.InvocationID, prepared.ObjectMeta.ResourceVersion, token, claimant)
	if err != nil {
		return nil, "", fmt.Errorf("claim runtime approval execution: %w", err)
	}
	return &claimed, token, nil
}

func (g *Gateway) settleApprovalReservation(ctx context.Context, receipt store.ToolInvocationReceipt, preparationToken string) error {
	if preparationToken == "" || g.reservations == nil {
		return nil
	}
	settled, err := g.reservations.SettleToolCallReservation(ctx, receipt.Namespace, receipt.InvocationID, preparationToken)
	if err != nil {
		return fmt.Errorf("settle Tool-call reservation after target return: %w", err)
	}
	if !settled {
		return errors.New("settle Tool-call reservation after target return: matching reservation not found")
	}
	return nil
}

func (g *Gateway) verifyClaimOwnership(ctx context.Context, claimed *v1alpha1.RuntimeApproval) error {
	if claimed == nil || g.runtimeApprovals == nil {
		return nil
	}
	current, err := g.runtimeApprovals.GetRuntimeApprovalByInvocation(ctx, claimed.ObjectMeta.Namespace, claimed.Spec.InvocationID)
	if err != nil {
		return fmt.Errorf("verify runtime approval claim ownership: %w", err)
	}
	if current.Status.ExecutionState != v1alpha1.RuntimeApprovalExecutionClaimed || current.Status.FencingToken != claimed.Status.FencingToken || current.Status.Claimant.WorkloadUID != claimed.Status.Claimant.WorkloadUID || current.Status.Claimant.ProcessStartNonce != claimed.Status.Claimant.ProcessStartNonce {
		return errors.New("verify runtime approval claim ownership: claim is no longer owned by this gateway")
	}
	return nil
}

// abortPreparedApproval invokes AbortRuntimeApprovalBeforeDispatch against a
// RuntimeApproval whose Prepare step succeeded but which must not be
// dispatched (Claim failure, or any post-Prepare failure before executor
// runs). Returns nil when there is nothing to abort.
func (g *Gateway) abortPreparedApproval(ctx context.Context, prepared v1alpha1.RuntimeApproval, token, reason string) error {
	if g.runtimeApprovals == nil {
		return nil
	}
	_, err := g.runtimeApprovals.AbortRuntimeApprovalBeforeDispatch(ctx, prepared.ObjectMeta.Namespace, prepared.Spec.InvocationID, prepared.ObjectMeta.ResourceVersion, token, reason)
	if err != nil {
		return fmt.Errorf("abort runtime approval before dispatch: %w", err)
	}
	return nil
}

// finalizeApproval durably transitions a claimed RuntimeApproval into its
// terminal Completed or Failed execution state using the fencing token
// captured during Claim. No-op when no claim was performed.
func (g *Gateway) finalizeApproval(ctx context.Context, claimed *v1alpha1.RuntimeApproval, disposition string) error {
	if claimed == nil || g.runtimeApprovals == nil {
		return nil
	}
	_, err := g.runtimeApprovals.FinalizeRuntimeApprovalExecution(ctx, claimed.ObjectMeta.Namespace, claimed.Spec.InvocationID, claimed.ObjectMeta.ResourceVersion, claimed.Status.FencingToken, disposition)
	if err != nil {
		return fmt.Errorf("finalize runtime approval execution: %w", err)
	}
	return nil
}

func toolDecisionReasonCode(err error) string {
	if errors.Is(err, ErrToolArgumentConstraintsDenied) {
		return "argument_constraints_denied"
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "revoked"):
		return "grant_revoked"
	case strings.Contains(message, "grant"):
		return "grant_missing"
	case strings.Contains(message, "role"):
		return "role_denied"
	case strings.Contains(message, "capability"):
		return "capability_invalid"
	default:
		return "policy_denied"
	}
}

func loadTool(ctx context.Context, st store.Store, namespace, name string) (*v1alpha1.Tool, error) {
	raw, err := st.Get(ctx, "Tool", namespace, name)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, fmt.Errorf("%w: %s/%s", ErrToolNotFound, namespace, name)
		}
		return nil, fmt.Errorf("load tool: %w", err)
	}
	var tool v1alpha1.Tool
	if err := json.Unmarshal(raw, &tool); err != nil {
		return nil, fmt.Errorf("decode tool: %w", err)
	}
	return &tool, nil
}

func (g *Gateway) record(ctx context.Context, request Request, tool *v1alpha1.Tool, status invocation.ResultStatus, output map[string]any, duration time.Duration) {
	if g.tracer == nil {
		return
	}
	arguments, _ := json.Marshal(request.Input)
	payload, _ := json.Marshal(output)
	g.tracer.RecordInvocation(ctx, invocation.SpanAttributes{
		InvocationID:   request.InvocationID,
		Workspace:      request.workspace(),
		AgentNamespace: request.agentNamespace(),
		AgentIdentity:  request.agentIdentity(),
		Solution:       request.Solution,
		SolutionRev:    request.SolutionRev,
		SessionID:      request.SessionID,
		ToolName:       tool.ObjectMeta.Name,
		ToolDigest:     strconv.FormatInt(tool.ObjectMeta.ResourceVersion, 10),
		Arguments:      string(arguments),
		ResultStatus:   status,
		ResultPayload:  string(payload),
		LatencyMs:      duration.Milliseconds(),
	}, invocation.AuditEntry{
		InvocationID:   request.InvocationID,
		Timestamp:      time.Now().UTC(),
		Workspace:      request.workspace(),
		AgentNamespace: request.agentNamespace(),
		AgentIdentity:  request.agentIdentity(),
		Solution:       request.Solution,
		SolutionRev:    request.SolutionRev,
		SessionID:      request.SessionID,
		ToolRef:        request.Namespace + "/" + tool.ObjectMeta.Name,
		ToolDigest:     strconv.FormatInt(tool.ObjectMeta.ResourceVersion, 10),
		Verb:           string(v1alpha1.ToolGrantVerbInvoke),
		Arguments:      string(arguments),
		Result:         status,
		LatencyMs:      duration.Milliseconds(),
	})
}

func (r Request) workspace() string {
	if r.Workspace != "" {
		return r.Workspace
	}
	return r.Namespace
}

func (r Request) agentNamespace() string {
	if r.AgentNamespace != "" {
		return r.AgentNamespace
	}
	return r.Namespace
}

func (r Request) agentIdentity() string {
	if r.AgentIdentity != "" {
		return r.AgentIdentity
	}
	return r.Principal
}
