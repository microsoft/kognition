// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package agentgateway authorizes capability-bound runtime AgentService delegation.
package agentgateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/kognition-ai/kognition/internal/store"
)

const (
	maxRequestBytes     = 1 << 20
	targetInvokeTimeout = 25 * time.Second
)

// ErrRuntimeApprovalPending signals that the ADR-0014 Runtime Autonomy Gate
// blocked the delegation invocation while a durable RuntimeApproval remains
// undecided. Callers that consume the Server programmatically can use
// errors.Is to distinguish this from other invocation failures. The HTTP
// surface conveys the same semantics via HTTP 202 Accepted with a tagged
// pending response body.
var ErrRuntimeApprovalPending = errors.New("runtime approval pending")

// Gate3Resolver is the ADR-0014 Runtime Autonomy Gate coordinator entry
// point the Agent Communication Gateway calls between Gate 2 authorization
// and the durable invocation receipt claim.
type Gate3Resolver interface {
	Resolve(ctx context.Context, request runtimeapproval.Gate3Request) (runtimeapproval.Gate3Result, error)
}

// Gate3RequestBuilder projects the invocation context of a single exact-capability
// delegation into a runtimeapproval.Gate3Request. It returns (request, true)
// when Gate 3 must be evaluated for this invocation, or (_, false) to opt
// out (for example when the deployment intentionally disables Gate 3 for a
// specific plane). The context is scoped to the inbound HTTP request so
// production builders can load additional resources (for example the source
// AgentInstance) from the shared store under the same cancellation domain.
type Gate3RequestBuilder func(
	ctx context.Context,
	claims *runtimeauth.AgentDelegationSessionCapabilityClaims,
	sourceIdentity *v1alpha1.AgentIdentity,
	invocationID string,
	targetNamespace, targetService, targetAgent, targetUID string,
	targetResourceVersion int64,
	message string,
) (runtimeapproval.Gate3Request, bool)

// CommunicationAuthorizer re-evaluates the current communication policy at
// both capability exchange and invocation time.
type CommunicationAuthorizer interface {
	EvaluateDispatch(context.Context, string, string, string) (bool, error)
	EvaluateCrossWorkspaceDispatch(context.Context, string, string, string, string) (bool, error)
}

// Server is the Kubernetes data-plane boundary for one Agent runtime
// delegating to a bound AgentService.
type Server struct {
	store             store.Store
	sessionIssuer     *runtimeauth.AgentDelegationSessionCapabilityIssuer
	exactIssuer       *runtimeauth.AgentDelegationCapabilityIssuer
	authorizer        CommunicationAuthorizer
	receipts          store.ToolInvocationReceiptStore
	runtimeToken      string
	runtimeNamespace  string
	standaloneAPI     string
	standaloneToken   string
	client            *http.Client
	logger            *zap.Logger
	audit             *observability.CognitiveAuditLog
	gate3             Gate3Resolver
	gate3Builder      Gate3RequestBuilder
	runtimeApprovals  store.RuntimeApprovalExecutionStore
	claimant          v1alpha1.RuntimeApprovalClaimant
	now               func() time.Time
	newPrepToken      func() (string, error)
	issueCapability   func(runtimeauth.AgentDelegationCapabilityBinding) (string, error)
	preparationWindow time.Duration
}

// Option configures optional Agent Communication Gateway dependencies.
type Option func(*Server)

// WithAuditLog records normalized Communication Trust Decisions.
func WithAuditLog(audit *observability.CognitiveAuditLog) Option {
	return func(server *Server) { server.audit = audit }
}

// WithGate3Coordinator wires the ADR-0014 Runtime Autonomy Gate coordinator
// that evaluates every exact-capability delegation before the durable
// invocation receipt is claimed and the target AgentService is reached.
// The gate is only invoked when a Gate3RequestBuilder is also provided.
func WithGate3Coordinator(coordinator Gate3Resolver) Option {
	return func(server *Server) { server.gate3 = coordinator }
}

// WithGate3RequestBuilder installs the projection from a validated exact
// delegation into the ADR-0014 Gate3Request. Deployments retain control of
// which invocations are subject to Gate 3 and how identity, instance, and
// target metadata are stamped into the durable RuntimeApproval spec.
func WithGate3RequestBuilder(builder Gate3RequestBuilder) Option {
	return func(server *Server) { server.gate3Builder = builder }
}

// WithRuntimeApprovalExecutionStore enables durable execution ownership for
// Gate 3 Ready delegations before the target AgentService is contacted.
func WithRuntimeApprovalExecutionStore(executionStore store.RuntimeApprovalExecutionStore) Option {
	return func(server *Server) { server.runtimeApprovals = executionStore }
}

// WithClaimant identifies this gateway process when claiming an execution.
func WithClaimant(claimant v1alpha1.RuntimeApprovalClaimant) Option {
	return func(server *Server) { server.claimant = claimant }
}

func WithStandaloneAPIServer(endpoint, token string) Option {
	return func(server *Server) {
		server.standaloneAPI = endpoint
		server.standaloneToken = token
	}
}

// NewServer creates an Agent Communication Gateway HTTP server.
func NewServer(st store.Store, sessionIssuer *runtimeauth.AgentDelegationSessionCapabilityIssuer, exactIssuer *runtimeauth.AgentDelegationCapabilityIssuer, authorizer CommunicationAuthorizer, runtimeToken, runtimeNamespace string, client *http.Client, logger *zap.Logger, options ...Option) (*Server, error) {
	if st == nil {
		return nil, errors.New("agent communication gateway requires a store")
	}
	if sessionIssuer == nil || exactIssuer == nil {
		return nil, errors.New("agent communication gateway requires session and exact capability issuers")
	}
	if authorizer == nil {
		return nil, errors.New("agent communication gateway requires a communication authorizer")
	}
	if runtimeToken == "" {
		return nil, errors.New("agent communication gateway requires a runtime control token")
	}
	receipts, ok := st.(store.ToolInvocationReceiptStore)
	if !ok {
		return nil, errors.New("agent communication gateway requires durable invocation receipts")
	}
	if client == nil {
		client = &http.Client{}
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	server := &Server{store: st, sessionIssuer: sessionIssuer, exactIssuer: exactIssuer, authorizer: authorizer, receipts: receipts, runtimeToken: runtimeToken, runtimeNamespace: runtimeNamespace, client: client, logger: logger.Named("agent-communication-gateway"), now: time.Now, newPrepToken: runtimeapproval.GeneratePreparationToken, issueCapability: exactIssuer.Issue, preparationWindow: runtimeapproval.MaxPreparationDeadline}
	for _, option := range options {
		option(server)
	}
	if server.standaloneAPI != "" || server.standaloneToken != "" {
		if server.standaloneToken == runtimeToken {
			return nil, errors.New("standalone API invocation credential must differ from the runtime control token")
		}
		endpoint, err := url.Parse(server.standaloneAPI)
		if err != nil || endpoint == nil || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Path != "" && endpoint.Path != "/") || server.standaloneToken == "" {
			return nil, errors.New("standalone delegation requires an HTTP(S) API server origin and an API credential")
		}
		server.standaloneAPI = strings.TrimRight(endpoint.String(), "/")
		standaloneClient := *server.client
		standaloneClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		server.client = &standaloneClient
	}
	gate3Configured := server.gate3 != nil || server.gate3Builder != nil || server.runtimeApprovals != nil
	if gate3Configured && (server.gate3 == nil || server.gate3Builder == nil || server.runtimeApprovals == nil) {
		return nil, errors.New("agent communication gateway requires Gate 3 coordinator, request builder, and execution store together")
	}
	if server.runtimeApprovals != nil {
		server.ensureClaimantDefaults()
	}
	return server, nil
}

func (s *Server) ensureClaimantDefaults() {
	if s.claimant.WorkloadUID == "" {
		s.claimant.WorkloadUID = os.Getenv("KOGNITION_RUNTIME_APPROVAL_WORKLOAD_UID")
		if s.claimant.WorkloadUID == "" {
			hostname, err := os.Hostname()
			if err != nil || hostname == "" {
				hostname = "agent-communication-gateway"
			}
			s.claimant.WorkloadUID = hostname
		}
	}
	if s.claimant.ProcessStartNonce == "" {
		nonce, err := runtimeapproval.GeneratePreparationToken()
		if err != nil {
			nonce = fmt.Sprintf("startup-%d", time.Now().UnixNano())
		}
		s.claimant.ProcessStartNonce = nonce
	}
}

// ServeHTTP handles capability exchange and the resulting exact delegation.
func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	request = request.WithContext(observability.ContextWithTraceHeaders(request.Context(), request.Header))
	if request.Method == http.MethodGet && request.URL.Path == "/healthz" {
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	operation := ""
	switch request.URL.Path {
	case "/v1/agent-services/capabilities":
		operation = "exchange"
	case "/v1/agent-services/invoke":
		operation = "invoke"
	}
	if operation == "" {
		writeError(writer, http.StatusNotFound, "agent communication gateway operation is not available")
		return
	}
	started := time.Now()
	recorder := &statusRecordingWriter{ResponseWriter: writer}
	defer func() { recordRequest(operation, requestOutcome(recorder.statusCode()), time.Since(started)) }()
	if request.Method != http.MethodPost {
		writeError(recorder, http.StatusNotFound, "agent communication gateway operation is not available")
		return
	}
	runID, err := observability.NormalizeTrustDecisionRunID(request.Header.Get(observability.TrustDecisionRunIDHeader))
	if err != nil {
		writeError(recorder, http.StatusBadRequest, "invalid trust decision run ID")
		return
	}
	switch operation {
	case "exchange":
		s.exchangeCapability(recorder, request, runID)
	case "invoke":
		s.invoke(recorder, request, runID)
	}
}

type statusRecordingWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusRecordingWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusRecordingWriter) Write(payload []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(payload)
}

func (w *statusRecordingWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func (s *Server) exchangeCapability(writer http.ResponseWriter, request *http.Request, runID string) {
	token, ok := bearerToken(request.Header.Get("Authorization"))
	if !ok {
		writeError(writer, http.StatusUnauthorized, "runtime delegation session capability is required")
		return
	}
	claims, err := s.sessionIssuer.VerifyClaims(token)
	if err != nil {
		writeError(writer, http.StatusUnauthorized, "invalid runtime delegation session capability")
		return
	}
	var body struct {
		TargetNamespace    string `json:"target_namespace"`
		TargetAgentService string `json:"target_agent_service"`
		InvocationID       string `json:"invocation_id"`
		Message            string `json:"message"`
	}
	if err := decodeRequest(request, &body); err != nil || body.TargetNamespace == "" || body.TargetAgentService == "" || body.InvocationID == "" || strings.TrimSpace(body.Message) == "" {
		writeError(writer, http.StatusBadRequest, "target namespace, AgentService, invocation ID, and message are required")
		return
	}
	earlyDecision := communicationDecisionInputFromSession(claims, body.TargetNamespace, body.TargetAgentService, body.InvocationID, runID)
	earlyDecision.approval = &observability.TrustDecisionApproval{Status: "not_evaluated"}
	identity, err := s.validateSource(request.Context(), sourceFromSessionClaims(claims))
	if err != nil {
		s.logDenied("source-session", claims.Namespace, claims.AgentName, body.TargetNamespace, body.TargetAgentService, err)
		if auditErr := s.recordCommunicationDecision(request.Context(), earlyDecision, nil, "denied", "identity_not_ready", "blocked", false, true); auditErr != nil {
			writeError(writer, http.StatusServiceUnavailable, "communication audit evidence is unavailable")
			return
		}
		writeError(writer, http.StatusForbidden, "runtime delegation session is not authorized")
		return
	}
	target, err := s.resolveTarget(request.Context(), body.TargetNamespace, body.TargetAgentService)
	if err != nil {
		s.logDenied("target-service", claims.Namespace, claims.AgentName, body.TargetNamespace, body.TargetAgentService, err)
		writeError(writer, http.StatusForbidden, "target AgentService is not authorized")
		return
	}
	if err := s.authorize(request.Context(), claims.Namespace, claims.AgentName, target.namespace, target.agent); err != nil {
		s.logDenied("policy", claims.Namespace, claims.AgentName, target.namespace, target.service, err)
		if auditErr := s.recordCommunicationDecision(request.Context(), earlyDecision, identity, "denied", "policy_denied", "blocked", false, true); auditErr != nil {
			writeError(writer, http.StatusServiceUnavailable, "communication audit evidence is unavailable")
			return
		}
		writeError(writer, http.StatusForbidden, "agent delegation is not authorized")
		return
	}
	var readyApproval *v1alpha1.RuntimeApproval
	if s.gate3 != nil && s.gate3Builder != nil {
		gateReq, gated := s.gate3Builder(request.Context(), claims, identity, body.InvocationID, target.namespace, target.service, target.agent, target.uid, target.resourceVersion, body.Message)
		if gated {
			result, err := s.gate3.Resolve(request.Context(), gateReq)
			if err != nil {
				s.logger.Error("resolve runtime autonomy gate", zap.Error(err))
				input := communicationDecisionInputFromSession(claims, target.namespace, target.service, body.InvocationID, runID)
				input.approval = &observability.TrustDecisionApproval{Status: "unavailable"}
				if auditErr := s.recordCommunicationDecision(request.Context(), input, identity, "denied", "runtime_approval_unavailable", "blocked", false, true); auditErr != nil {
					writeError(writer, http.StatusServiceUnavailable, "communication audit evidence is unavailable")
					return
				}
				writeError(writer, http.StatusServiceUnavailable, "runtime autonomy gate is unavailable")
				return
			}
			switch result.State {
			case runtimeapproval.Gate3StatePending:
				if auditErr := s.recordCommunicationApproval(request.Context(), communicationDecisionInputFromSession(claims, target.namespace, target.service, body.InvocationID, runID), identity, "pending", "approval_required", "blocked", result.Approval); auditErr != nil {
					writeError(writer, http.StatusServiceUnavailable, "communication audit evidence is unavailable")
					return
				}
				writePendingRuntimeApproval(writer, body.InvocationID, string(runtimeapproval.Gate3StatePending), result.Approval, result.ExpiresAt)
				return
			case runtimeapproval.Gate3StateRejected, runtimeapproval.Gate3StateExpired:
				reasonCode := "approval_rejected"
				if result.State == runtimeapproval.Gate3StateExpired {
					reasonCode = "approval_expired"
				}
				if auditErr := s.recordCommunicationApproval(request.Context(), communicationDecisionInputFromSession(claims, target.namespace, target.service, body.InvocationID, runID), identity, "rejected", reasonCode, "blocked", result.Approval); auditErr != nil {
					writeError(writer, http.StatusServiceUnavailable, "communication audit evidence is unavailable")
					return
				}
				writeError(writer, http.StatusForbidden, "runtime autonomy gate rejected the delegation")
				return
			case runtimeapproval.Gate3StateReady:
				readyApproval = &result.Approval
			default:
				s.logger.Error("runtime autonomy gate returned unknown state", zap.String("state", string(result.State)))
				input := communicationDecisionInputFromSession(claims, target.namespace, target.service, body.InvocationID, runID)
				input.approval = &observability.TrustDecisionApproval{Status: "unavailable"}
				if auditErr := s.recordCommunicationDecision(request.Context(), input, identity, "denied", "runtime_approval_unavailable", "blocked", false, true); auditErr != nil {
					writeError(writer, http.StatusServiceUnavailable, "communication audit evidence is unavailable")
					return
				}
				writeError(writer, http.StatusServiceUnavailable, "runtime autonomy gate returned an invalid state")
				return
			}
		}
	}
	decisionInput := communicationDecisionInputFromSession(claims, target.namespace, target.service, body.InvocationID, runID)
	decisionInput.approval = observability.TrustApproval(readyApproval)
	if err := s.recordCommunicationDecision(request.Context(), decisionInput, identity, "allowed", "authorized", "not_started", false, true); err != nil {
		s.logger.Error("record communication exchange authorization", zap.Error(err))
		writeError(writer, http.StatusServiceUnavailable, "communication audit evidence is unavailable")
		return
	}
	claimedApproval, err := s.prepareAndClaimApproval(request.Context(), readyApproval)
	if err != nil {
		s.logger.Error("claim runtime approval execution", zap.Error(err), zap.String("invocation", body.InvocationID))
		writeError(writer, http.StatusServiceUnavailable, "runtime approval execution is unavailable")
		return
	}
	capability, err := s.issueCapability(runtimeauth.AgentDelegationCapabilityBinding{
		Namespace: claims.Namespace, AgentName: claims.AgentName, AgentIdentityName: claims.AgentIdentityName,
		InstanceName: claims.InstanceName, InstanceUID: claims.InstanceUID, SessionID: claims.SessionID,
		Solution: claims.Solution, SolutionRev: claims.SolutionRev,
		TargetNamespace: target.namespace, TargetAgentService: target.service, TargetAgent: target.agent, InvocationID: body.InvocationID,
		MessageDigest: delegationMessageDigest(body.Message),
	})
	if err != nil {
		if finalizeErr := s.finalizeApproval(request.Context(), claimedApproval, runtimeapproval.ExecutionDispositionFailed); finalizeErr != nil {
			s.logger.Error("finalize failed capability issuance", zap.Error(finalizeErr), zap.String("invocation", body.InvocationID))
		}
		s.logger.Error("issue delegation capability", zap.Error(err))
		writeError(writer, http.StatusInternalServerError, "could not issue agent delegation capability")
		return
	}
	if err := s.finalizeApproval(request.Context(), claimedApproval, runtimeapproval.ExecutionDispositionSucceeded); err != nil {
		s.logger.Error("finalize successful capability issuance", zap.Error(err), zap.String("invocation", body.InvocationID))
		writeError(writer, http.StatusInternalServerError, "capability was issued but execution ownership could not be finalized")
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"capability": capability})
}

func (s *Server) invoke(writer http.ResponseWriter, request *http.Request, runID string) {
	token, ok := bearerToken(request.Header.Get("Authorization"))
	if !ok {
		writeError(writer, http.StatusUnauthorized, "runtime agent delegation capability is required")
		return
	}
	claims, err := s.exactIssuer.VerifyClaims(token)
	if err != nil {
		writeError(writer, http.StatusUnauthorized, "invalid runtime agent delegation capability")
		return
	}
	var body struct {
		TargetNamespace    string `json:"target_namespace"`
		TargetAgentService string `json:"target_agent_service"`
		InvocationID       string `json:"invocation_id"`
		Message            string `json:"message"`
	}
	if err := decodeRequest(request, &body); err != nil || body.TargetNamespace == "" || body.TargetAgentService == "" || body.InvocationID == "" || strings.TrimSpace(body.Message) == "" {
		writeError(writer, http.StatusBadRequest, "target namespace, AgentService, invocation ID, and message are required")
		return
	}
	earlyDecision := communicationDecisionInputFromExact(claims, runID)
	earlyDecision.approval = &observability.TrustDecisionApproval{Status: "not_evaluated"}
	identity, err := s.validateSource(request.Context(), sourceFromExactClaims(claims))
	if err != nil {
		s.logDenied("source-session", claims.Namespace, claims.AgentName, claims.TargetNamespace, claims.TargetAgentService, err)
		if auditErr := s.recordCommunicationDecision(request.Context(), earlyDecision, nil, "revoked", "identity_not_ready", "blocked", false, true); auditErr != nil {
			writeError(writer, http.StatusServiceUnavailable, "communication audit evidence is unavailable")
			return
		}
		writeError(writer, http.StatusForbidden, "runtime agent delegation is not authorized")
		return
	}
	if body.TargetNamespace != claims.TargetNamespace || body.TargetAgentService != claims.TargetAgentService || body.InvocationID != claims.InvocationID || delegationMessageDigest(body.Message) != claims.MessageDigest {
		if auditErr := s.recordCommunicationDecision(request.Context(), earlyDecision, identity, "denied", "capability_invalid", "blocked", false, true); auditErr != nil {
			writeError(writer, http.StatusServiceUnavailable, "communication audit evidence is unavailable")
			return
		}
		writeError(writer, http.StatusForbidden, "agent delegation capability does not permit this invocation")
		return
	}
	target, err := s.resolveTarget(request.Context(), claims.TargetNamespace, claims.TargetAgentService)
	if err != nil || target.agent != claims.TargetAgent {
		if err == nil {
			err = errors.New("target AgentService no longer resolves to capability-bound Agent")
		}
		s.logDenied("target-service", claims.Namespace, claims.AgentName, claims.TargetNamespace, claims.TargetAgentService, err)
		if auditErr := s.recordCommunicationDecision(request.Context(), earlyDecision, identity, "revoked", "policy_denied", "blocked", false, true); auditErr != nil {
			writeError(writer, http.StatusServiceUnavailable, "communication audit evidence is unavailable")
			return
		}
		writeError(writer, http.StatusForbidden, "target AgentService is not authorized")
		return
	}
	if err := s.authorize(request.Context(), claims.Namespace, claims.AgentName, target.namespace, target.agent); err != nil {
		s.logDenied("policy", claims.Namespace, claims.AgentName, target.namespace, target.service, err)
		if auditErr := s.recordCommunicationDecision(request.Context(), earlyDecision, identity, "revoked", "policy_denied", "blocked", false, true); auditErr != nil {
			writeError(writer, http.StatusServiceUnavailable, "communication audit evidence is unavailable")
			return
		}
		writeError(writer, http.StatusForbidden, "agent delegation is not authorized")
		return
	}
	decisionInput := communicationDecisionInputFromExact(claims, runID)
	if s.runtimeApprovals != nil {
		approval, lookupErr := s.runtimeApprovals.GetRuntimeApprovalByInvocation(request.Context(), claims.Namespace, claims.InvocationID)
		if lookupErr == nil && approval.Spec.Plane == v1alpha1.RuntimeApprovalPlaneCommunication && approval.Spec.SessionRef == claims.SessionID && approval.Spec.AgentInstanceRef.UID == claims.InstanceUID && approval.Spec.TargetRef.Namespace == claims.TargetNamespace && approval.Spec.TargetRef.Name == claims.TargetAgentService {
			decisionInput.approval = observability.TrustApproval(&approval)
		}
	}
	if err := s.recordCommunicationDecision(request.Context(), decisionInput, identity, "allowed", "authorized", "not_started", false, true); err != nil {
		s.logger.Error("record communication invocation authorization", zap.Error(err))
		writeError(writer, http.StatusServiceUnavailable, "communication audit evidence is unavailable")
		return
	}
	receipt := s.newReceipt(claims, body.Message)
	claimed, existing, err := s.receipts.ClaimToolInvocation(request.Context(), receipt)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeError(writer, http.StatusConflict, "delegation invocation ID is already bound to another request")
			return
		}
		writeError(writer, http.StatusInternalServerError, "could not record agent delegation")
		return
	}
	if !claimed {
		if err := s.recordCommunicationDecision(request.Context(), decisionInput, identity, "replayed", "replay_detected", "not_started", false, false); err != nil {
			writeError(writer, http.StatusServiceUnavailable, "communication audit evidence is unavailable")
			return
		}
		s.writeReceiptResponse(writer, existing)
		return
	}
	if err := s.proxy(request.Context(), request, target.endpoint, claims.InvocationID, body.Message); err != nil {
		_ = s.completeReceipt(request.Context(), receipt, store.ToolInvocationFailed, nil, err)
		if auditErr := s.recordCommunicationDecision(request.Context(), decisionInput, identity, "failed", "target_failed", "failed", true, false); auditErr != nil {
			s.logger.Error("record failed communication decision", zap.Error(auditErr))
		}
		s.logger.Warn("target AgentService invocation failed", zap.String("targetNamespace", target.namespace), zap.String("targetService", target.service), zap.String("invocation", claims.InvocationID), zap.Error(err))
		writeError(writer, http.StatusBadGateway, "target AgentService is unavailable")
		return
	}
	response := delegationResponse{SessionID: claims.InvocationID, TargetAgent: target.agent}
	if err := s.completeReceipt(request.Context(), receipt, store.ToolInvocationSucceeded, response, nil); err != nil {
		writeError(writer, http.StatusInternalServerError, "delegation was accepted but could not be recorded")
		return
	}
	if err := s.recordCommunicationDecision(request.Context(), decisionInput, identity, "completed", "target_completed", "completed", true, false); err != nil {
		writeError(writer, http.StatusServiceUnavailable, "communication audit evidence is unavailable")
		return
	}
	writeJSON(writer, http.StatusAccepted, response)
}

type sourceBinding struct {
	namespace   string
	agent       string
	identity    string
	instance    string
	instanceUID string
	session     string
}

type resolvedTarget struct {
	namespace       string
	service         string
	agent           string
	endpoint        string
	uid             string
	resourceVersion int64
}

func delegationMessageDigest(message string) string {
	digest := sha256.Sum256([]byte(message))
	return fmt.Sprintf("sha256:%x", digest)
}

func sourceFromSessionClaims(claims *runtimeauth.AgentDelegationSessionCapabilityClaims) sourceBinding {
	return sourceBinding{namespace: claims.Namespace, agent: claims.AgentName, identity: claims.AgentIdentityName, instance: claims.InstanceName, instanceUID: claims.InstanceUID, session: claims.SessionID}
}

func sourceFromExactClaims(claims *runtimeauth.AgentDelegationCapabilityClaims) sourceBinding {
	return sourceBinding{namespace: claims.Namespace, agent: claims.AgentName, identity: claims.AgentIdentityName, instance: claims.InstanceName, instanceUID: claims.InstanceUID, session: claims.SessionID}
}

func (s *Server) validateSource(ctx context.Context, source sourceBinding) (*v1alpha1.AgentIdentity, error) {
	agent, err := loadResource[v1alpha1.Agent](ctx, s.store, "Agent", source.namespace, source.agent)
	if err != nil {
		return nil, fmt.Errorf("load source Agent: %w", err)
	}
	identity, err := loadResource[v1alpha1.AgentIdentity](ctx, s.store, "AgentIdentity", source.namespace, source.identity)
	if err != nil {
		return nil, fmt.Errorf("load source AgentIdentity: %w", err)
	}
	if identity.ObjectMeta.DeletionTimestamp != nil || identity.Status.Phase != v1alpha1.PhaseReady || identity.Spec.AgentRef.Name != agent.ObjectMeta.Name || identity.ObjectMeta.Labels["kognition.dev/auto-created-for-agent"] != agent.ObjectMeta.Name || !identityOwnedByAgent(identity, agent) {
		return nil, errors.New("source capability identity is not current and ready")
	}
	instance, err := loadResource[v1alpha1.AgentInstance](ctx, s.store, "AgentInstance", source.namespace, source.instance)
	if err != nil || instance.ObjectMeta.DeletionTimestamp != nil || instance.ObjectMeta.UID != source.instanceUID || instance.Spec.AgentRef.Name != agent.ObjectMeta.Name || instance.Status.Phase != v1alpha1.PhaseRunning || instance.Status.SessionID != source.session {
		return nil, errors.New("source capability instance does not belong to the active Agent session")
	}
	return identity, nil
}

func (s *Server) resolveTarget(ctx context.Context, namespace, serviceName string) (resolvedTarget, error) {
	service, err := loadResource[v1alpha1.AgentService](ctx, s.store, "AgentService", namespace, serviceName)
	if err != nil {
		return resolvedTarget{}, fmt.Errorf("load target AgentService: %w", err)
	}
	if service.ObjectMeta.DeletionTimestamp != nil || service.Status.Phase != v1alpha1.PhaseReady || service.Status.ReadyInstances < 1 || service.Spec.AgentRef.Name == "" {
		return resolvedTarget{}, errors.New("target AgentService is not ready")
	}
	runtimeNamespace := s.runtimeNamespace
	if runtimeNamespace == "" {
		runtimeNamespace = namespace
	}
	port := service.Spec.Port
	if port <= 0 {
		port = 8080
	}
	endpoint := fmt.Sprintf("http://%s.%s.svc.cluster.local:%d/invoke", agentServiceEndpointName(namespace, service.ObjectMeta.Name, runtimeNamespace), runtimeNamespace, port)
	if s.standaloneAPI != "" {
		if namespace == "." || namespace == ".." || service.ObjectMeta.Name == "." || service.ObjectMeta.Name == ".." || strings.ContainsAny(namespace+service.ObjectMeta.Name, "/\\%?#") {
			return resolvedTarget{}, errors.New("invalid standalone AgentService route")
		}
		endpoint = fmt.Sprintf("%s/apis/kognition.dev/v1alpha1/namespaces/%s/agentservices/%s/invoke", s.standaloneAPI, url.PathEscape(namespace), url.PathEscape(service.ObjectMeta.Name))
	}
	return resolvedTarget{namespace: namespace, service: service.ObjectMeta.Name, agent: service.Spec.AgentRef.Name, endpoint: endpoint, uid: service.ObjectMeta.UID, resourceVersion: service.ObjectMeta.ResourceVersion}, nil
}

func (s *Server) authorize(ctx context.Context, sourceNamespace, sourceAgent, targetNamespace, targetAgent string) error {
	var allowed bool
	var err error
	if sourceNamespace == targetNamespace {
		allowed, err = s.authorizer.EvaluateDispatch(ctx, targetNamespace, sourceAgent, targetAgent)
	} else {
		allowed, err = s.authorizer.EvaluateCrossWorkspaceDispatch(ctx, sourceNamespace, sourceAgent, targetNamespace, targetAgent)
	}
	if err != nil {
		return err
	}
	if !allowed {
		return errors.New("AgentCommunicationPolicy denied delegation")
	}
	return nil
}

func (s *Server) proxy(ctx context.Context, incoming *http.Request, endpoint, sessionID, message string) error {
	sessionField := "session_id"
	if s.standaloneAPI != "" {
		sessionField = "sessionID"
	}
	body, err := json.Marshal(map[string]string{"message": message, sessionField: sessionID})
	if err != nil {
		return err
	}
	targetCtx, cancel := context.WithTimeout(ctx, targetInvokeTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(targetCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	token := s.runtimeToken
	if s.standaloneAPI != "" {
		token = s.standaloneToken
	}
	request.Header.Set("Authorization", "Bearer "+token)
	for _, header := range []string{"traceparent", "tracestate", "X-Kognition-Run-ID"} {
		if value := incoming.Header.Get(header); value != "" {
			request.Header.Set(header, value)
		}
	}
	response, err := s.client.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("target AgentService returned %d", response.StatusCode)
	}
	return nil
}

func (s *Server) logDenied(stage, sourceNamespace, sourceAgent, targetNamespace, targetService string, err error) {
	s.logger.Info("agent delegation denied", zap.String("stage", stage), zap.String("source", sourceNamespace+"/"+sourceAgent), zap.String("target", targetNamespace+"/"+targetService), zap.Error(err))
}

func decodeRequest(request *http.Request, destination any) error {
	raw, err := io.ReadAll(io.LimitReader(request.Body, maxRequestBytes+1))
	if err != nil || len(raw) > maxRequestBytes || json.Unmarshal(raw, destination) != nil {
		return errors.New("invalid request")
	}
	return nil
}

type delegationResponse struct {
	SessionID   string `json:"session_id"`
	TargetAgent string `json:"target_agent"`
}

// delegationPendingResponse is the ADR-0014 tagged-transport body written by
// the Agent Communication Gateway when the Runtime Autonomy Gate parks an
// exact-capability delegation on a durable RuntimeApproval. The client
// receives HTTP 202 Accepted with these fields so it can watch the
// referenced RuntimeApproval and retry the same invocation later without
// bypassing the gate.
type delegationPendingResponse struct {
	InvocationID             string                             `json:"invocation_id"`
	RuntimeApprovalState     string                             `json:"runtime_approval_state"`
	RuntimeApproval          *v1alpha1.RuntimeApprovalReference `json:"runtime_approval,omitempty"`
	RuntimeApprovalExpiresAt *time.Time                         `json:"runtime_approval_expires_at,omitempty"`
}

func (s *Server) newReceipt(claims *runtimeauth.AgentDelegationCapabilityClaims, message string) store.ToolInvocationReceipt {
	binding, _ := json.Marshal(struct {
		SourceNamespace string `json:"source_namespace"`
		SourceAgent     string `json:"source_agent"`
		SourceInstance  string `json:"source_instance"`
		SourceSession   string `json:"source_session"`
		TargetNamespace string `json:"target_namespace"`
		TargetService   string `json:"target_service"`
		TargetAgent     string `json:"target_agent"`
		Message         string `json:"message"`
	}{claims.Namespace, claims.AgentName, claims.InstanceName, claims.SessionID, claims.TargetNamespace, claims.TargetAgentService, claims.TargetAgent, message})
	digest := sha256.Sum256(binding)
	return store.ToolInvocationReceipt{Namespace: claims.Namespace, InvocationID: claims.InvocationID, Binding: fmt.Sprintf("%x", digest[:]), Status: store.ToolInvocationPending, CreatedAt: time.Now().UTC()}
}

func (s *Server) completeReceipt(ctx context.Context, receipt store.ToolInvocationReceipt, status store.ToolInvocationReceiptStatus, response any, invocationErr error) error {
	if response != nil {
		encoded, err := json.Marshal(response)
		if err != nil {
			return err
		}
		receipt.Output = encoded
	}
	now := time.Now().UTC()
	receipt.Status, receipt.CompletedAt = status, &now
	if invocationErr != nil {
		receipt.ErrorMessage = invocationErr.Error()
	}
	return s.receipts.CompleteToolInvocation(ctx, receipt)
}

func (s *Server) writeReceiptResponse(writer http.ResponseWriter, receipt store.ToolInvocationReceipt) {
	switch receipt.Status {
	case store.ToolInvocationPending:
		writeError(writer, http.StatusConflict, "agent delegation is already in progress")
	case store.ToolInvocationSucceeded:
		var response delegationResponse
		if len(receipt.Output) == 0 || json.Unmarshal(receipt.Output, &response) != nil {
			writeError(writer, http.StatusInternalServerError, "stored agent delegation receipt is invalid")
			return
		}
		writeJSON(writer, http.StatusAccepted, response)
	default:
		writeError(writer, http.StatusBadGateway, "previous agent delegation failed")
	}
}

func agentServiceEndpointName(serviceNamespace, serviceName, runtimeNamespace string) string {
	if runtimeNamespace == serviceNamespace {
		return serviceName
	}
	digest := sha256.Sum256([]byte(serviceNamespace + "\x00" + serviceName))
	prefix := strings.ToLower(serviceNamespace + "-" + serviceName)
	prefix = strings.Trim(strings.ReplaceAll(prefix, "_", "-"), "-.")
	if len(prefix) > 53 {
		prefix = prefix[:53]
	}
	return fmt.Sprintf("%s-%x", prefix, digest[:4])
}

func loadResource[T any](ctx context.Context, st store.Store, kind, namespace, name string) (*T, error) {
	raw, err := st.Get(ctx, kind, namespace, name)
	if err != nil {
		return nil, err
	}
	var value T
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func identityOwnedByAgent(identity *v1alpha1.AgentIdentity, agent *v1alpha1.Agent) bool {
	for _, owner := range identity.ObjectMeta.OwnerReferences {
		if owner.Kind == "Agent" && owner.Name == agent.ObjectMeta.Name && owner.UID != "" && owner.UID == agent.ObjectMeta.UID {
			return true
		}
	}
	return false
}

func bearerToken(header string) (string, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	token := strings.TrimSpace(strings.TrimPrefix(header, prefix))
	return token, token != ""
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]string{"error": message})
}

// writePendingRuntimeApproval emits the ADR-0014 tagged-transport HTTP 202
// pending body, including the durable RuntimeApproval reference the client
// needs to observe the parked delegation.
func writePendingRuntimeApproval(writer http.ResponseWriter, invocationID, state string, approval v1alpha1.RuntimeApproval, expiresAt time.Time) {
	response := delegationPendingResponse{InvocationID: invocationID, RuntimeApprovalState: state}
	if approval.ObjectMeta.Name != "" {
		response.RuntimeApproval = &v1alpha1.RuntimeApprovalReference{
			Kind:            "RuntimeApproval",
			Scope:           "Namespaced",
			Namespace:       approval.ObjectMeta.Namespace,
			Name:            approval.ObjectMeta.Name,
			UID:             approval.ObjectMeta.UID,
			ResourceVersion: approval.ObjectMeta.ResourceVersion,
		}
	}
	if !expiresAt.IsZero() {
		expires := expiresAt.UTC()
		response.RuntimeApprovalExpiresAt = &expires
	}
	writeJSON(writer, http.StatusAccepted, response)
}
