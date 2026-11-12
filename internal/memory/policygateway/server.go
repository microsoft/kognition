// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package policygateway enforces runtime memory access policy before proxying
// data-plane requests to the memory service.
package policygateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/controller"
	"github.com/kognition-ai/kognition/internal/memory"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/kognition-ai/kognition/internal/store"
)

const maxRequestBytes = 1 << 20

// MemoryInvocationIDHeader carries the caller-stable ADR-0014 Gate 3
// invocation ID used to deduplicate RuntimeApproval resources across retries.
// Gate 3 write requests without this header are rejected.
const MemoryInvocationIDHeader = "X-Kognition-Memory-Invocation-Id"

// Server authenticates runtime memory requests, evaluates current policy, and
// forwards authorized data-plane calls to the backing memory service.
type Server struct {
	store             store.Store
	issuer            *runtimeauth.MemoryCapabilityIssuer
	upstream          *url.URL
	httpClient        *http.Client
	logger            *zap.Logger
	upstreamToken     string
	auditLog          *observability.CognitiveAuditLog
	gate3             Gate3Resolver
	gate3Builder      Gate3RequestBuilder
	runtimeApprovals  store.RuntimeApprovalExecutionStore
	memoryReceipts    store.MemoryMutationReceiptStore
	claimant          v1alpha1.RuntimeApprovalClaimant
	now               func() time.Time
	newPrepToken      func() (string, error)
	preparationWindow time.Duration
}

// Ready verifies that the gateway can read policy resources and reach the raw
// memory service before the workload is marked ready to receive runtime calls.
func (s *Server) Ready(ctx context.Context) error {
	if err := s.store.Ping(ctx); err != nil {
		return fmt.Errorf("policy store unavailable: %w", err)
	}
	target := *s.upstream
	target.Path = "/v1/memory/health"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return fmt.Errorf("build upstream health request: %w", err)
	}
	if s.upstreamToken != "" {
		request.Header.Set("Authorization", "Bearer "+s.upstreamToken)
	}
	response, err := s.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("upstream memory service unavailable: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("upstream memory service health returned %s", response.Status)
	}
	return nil
}

// New creates a memory policy gateway. The upstream endpoint must be the
// memory service base URL without a path component.
type Option func(*Server)

// WithUpstreamToken configures the credential the gateway uses for the raw
// memory service. The runtime capability is never forwarded upstream.
func WithUpstreamToken(token string) Option {
	return func(s *Server) { s.upstreamToken = token }
}

// WithAuditLog records each valid-capability memory operation before it is
// forwarded or denied by current policy.
func WithAuditLog(auditLog *observability.CognitiveAuditLog) Option {
	return func(s *Server) { s.auditLog = auditLog }
}

func New(s store.Store, issuer *runtimeauth.MemoryCapabilityIssuer, upstream string, client *http.Client, logger *zap.Logger, options ...Option) (*Server, error) {
	if s == nil {
		return nil, errors.New("memory policy gateway requires a store")
	}
	if issuer == nil {
		return nil, errors.New("memory policy gateway requires a capability issuer")
	}
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(upstream), "/"))
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.Path != "" {
		return nil, fmt.Errorf("memory policy gateway has invalid upstream %q", upstream)
	}
	if client == nil {
		client = &http.Client{}
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	server := &Server{
		store: s, issuer: issuer, upstream: parsed, httpClient: client,
		logger: logger.Named("memory-policy-gateway"), now: time.Now,
		newPrepToken:      runtimeapproval.GeneratePreparationToken,
		preparationWindow: runtimeapproval.MaxPreparationDeadline,
	}
	for _, option := range options {
		option(server)
	}
	if server.runtimeApprovals != nil {
		if err := server.ensureClaimantDefaults(); err != nil {
			return nil, err
		}
	}
	return server, nil
}

func (s *Server) ensureClaimantDefaults() error {
	if s.claimant.WorkloadUID == "" {
		s.claimant.WorkloadUID = os.Getenv("KOGNITION_RUNTIME_APPROVAL_WORKLOAD_UID")
		if s.claimant.WorkloadUID == "" {
			hostname, err := os.Hostname()
			if err != nil || hostname == "" {
				hostname = "memory-policy-gateway"
			}
			s.claimant.WorkloadUID = hostname
		}
	}
	if s.claimant.ProcessStartNonce == "" {
		nonceBytes := make([]byte, 16)
		if _, err := rand.Read(nonceBytes); err != nil {
			return fmt.Errorf("memory policy gateway process start nonce: %w", err)
		}
		s.claimant.ProcessStartNonce = base64.RawURLEncoding.EncodeToString(nonceBytes)
	}
	return nil
}

// ServeHTTP handles only runtime data-plane memory operations. Provisioning
// and deprovisioning remain control-plane-only memory service operations.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r = r.WithContext(observability.ContextWithTraceHeaders(r.Context(), r.Header))
	if r.Method == http.MethodGet && r.URL.Path == "/v1/memory/health" {
		if err := s.Ready(r.Context()); err != nil {
			writeError(w, http.StatusServiceUnavailable, "memory policy gateway is not ready")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
		return
	}
	mode, volumeHandle, body, ok := s.requestAccess(r, w)
	if !ok {
		return
	}
	capability, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		writeError(w, http.StatusUnauthorized, "runtime memory capability is required")
		return
	}
	claims, err := s.issuer.VerifyClaims(capability)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid runtime memory capability")
		return
	}
	runID, err := observability.NormalizeTrustDecisionRunID(r.Header.Get(observability.TrustDecisionRunIDHeader))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid trust decision run ID")
		return
	}
	if claims.VolumeHandle != volumeHandle {
		if err := s.recordPolicyAudit(r.Context(), claims, "", memoryOperation(r), volumeHandle, memoryInvocationID(r), "denied", "", runID, errors.New("capability volume mismatch"), &observability.TrustDecisionApproval{Status: "not_evaluated"}); err != nil {
			writeError(w, http.StatusServiceUnavailable, "memory audit unavailable")
			return
		}
		writeError(w, http.StatusForbidden, "runtime memory capability does not permit this volume")
		return
	}
	authz, err := s.authorize(r.Context(), claims, mode)
	if err != nil {
		outcome := "denied"
		if strings.Contains(strings.ToLower(err.Error()), "revoked") {
			outcome = "revoked"
		}
		if auditErr := s.recordPolicyAudit(r.Context(), claims, authz.VerifiedIdentity, memoryOperation(r), volumeHandle, memoryInvocationID(r), outcome, authz.GrantName, runID, err, &observability.TrustDecisionApproval{Status: "not_evaluated"}); auditErr != nil {
			writeError(w, http.StatusServiceUnavailable, "memory audit unavailable")
			return
		}
		s.logger.Info("memory access denied", zap.String("namespace", claims.Namespace), zap.String("agent", claims.AgentName), zap.String("volume", volumeHandle), zap.String("mode", mode), zap.Error(err))
		writeError(w, http.StatusForbidden, "memory access denied")
		return
	}
	approval, err := s.applyGate3(r, w, claims, authz, mode, volumeHandle, body, runID)
	if err != nil {
		return
	}
	if err := s.recordPolicyAudit(r.Context(), claims, authz.VerifiedIdentity, memoryOperation(r), volumeHandle, memoryInvocationID(r), "authorized", authz.GrantName, runID, nil, observability.TrustApproval(approval)); err != nil {
		writeError(w, http.StatusServiceUnavailable, "memory audit unavailable")
		return
	}
	if r.Method == http.MethodPost && r.URL.Path == "/v1/memory/store" {
		body, err = stampGrantProvenance(body, authz.GrantName)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	replayed, err := s.replayMemoryMutation(r.Context(), approval, memoryOperation(r))
	if err != nil {
		s.logger.Error("read Memory mutation receipt failed", zap.Error(err), zap.String("invocation_id", approvalInvocationID(approval)))
		writeError(w, http.StatusServiceUnavailable, "memory mutation replay unavailable")
		return
	}
	if replayed != nil {
		if err := s.recordPolicyAudit(r.Context(), claims, authz.VerifiedIdentity, memoryOperation(r), volumeHandle, memoryInvocationID(r), "replayed", authz.GrantName, runID, nil, observability.TrustApproval(approval)); err != nil {
			writeError(w, http.StatusServiceUnavailable, "memory audit unavailable")
			return
		}
		writeMemoryOutcome(w, *replayed)
		return
	}
	claimed, receipt, err := s.prepareAndClaimApproval(r.Context(), approval, memoryOperation(r))
	if err != nil {
		s.logger.Error("claim runtime approval execution failed", zap.Error(err), zap.String("invocation_id", approvalInvocationID(approval)))
		writeError(w, http.StatusServiceUnavailable, "memory runtime approval execution unavailable")
		return
	}
	outcome := s.proxy(w, r, body, authz.VerifiedIdentity, claims.Namespace, authz.RevokedGrantIDs)
	if !outcome.terminal {
		return
	}
	auditOutcome := "failed"
	if outcome.succeeded() {
		auditOutcome = "completed"
	}
	if err := s.recordPolicyAudit(r.Context(), claims, authz.VerifiedIdentity, memoryOperation(r), volumeHandle, memoryInvocationID(r), auditOutcome, authz.GrantName, runID, nil, observability.TrustApproval(approval)); err != nil {
		s.logger.Error("record terminal memory trust decision", zap.Error(err), zap.String("outcome", auditOutcome))
		writeError(w, http.StatusServiceUnavailable, "memory audit unavailable")
		return
	}
	disposition := runtimeapproval.ExecutionDispositionFailed
	if outcome.succeeded() {
		disposition = runtimeapproval.ExecutionDispositionSucceeded
	}
	if err := s.completeMemoryMutation(r.Context(), receipt, outcome); err != nil {
		s.logger.Error("complete memory mutation receipt failed", zap.Error(err), zap.String("invocation_id", approvalInvocationID(claimed)))
		writeError(w, http.StatusServiceUnavailable, "memory mutation completion unavailable")
		return
	}
	if err := s.finalizeApproval(r.Context(), claimed, disposition); err != nil {
		s.logger.Error("finalize runtime approval execution failed", zap.Error(err), zap.String("invocation_id", approvalInvocationID(claimed)))
		writeError(w, http.StatusServiceUnavailable, "memory runtime approval finalization unavailable")
		return
	}
	writeMemoryOutcome(w, outcome)
}

// applyGate3 runs ADR-0014 Gate 3 enforcement after AuthN + AuthZ succeed.
// It returns nil when the request may proceed (Gate 3 disabled, opted-out, or
// approved). On a Gate 3 denial it records a denial audit, writes the mapped
// HTTP status, and returns a non-nil error signalling the caller to stop.
func (s *Server) applyGate3(r *http.Request, w http.ResponseWriter, claims *runtimeauth.MemoryCapabilityClaims, authz authorizeResult, mode, volumeHandle string, body []byte, runID string) (*v1alpha1.RuntimeApproval, error) {
	invocationID := strings.TrimSpace(r.Header.Get(MemoryInvocationIDHeader))
	if invocationID == "" && mode == "ReadWrite" && s.gate3 != nil && s.gate3Builder != nil {
		err := errors.New("caller-stable Memory invocation ID is required")
		writeError(w, http.StatusBadRequest, err.Error())
		return nil, err
	}
	invocation := Gate3Invocation{
		Namespace:        claims.Namespace,
		Operation:        memoryOperation(r),
		InvocationID:     invocationID,
		RunID:            runID,
		Claims:           claims,
		Identity:         authz.Identity,
		Instance:         authz.Instance,
		Claim:            authz.Claim,
		VolumeHandle:     volumeHandle,
		CanonicalPayload: body,
	}
	approval, err := s.resolveGate3(r.Context(), mode, invocation)
	if err == nil {
		return approval, nil
	}
	approvalEvidence := &observability.TrustDecisionApproval{Status: "unavailable"}
	if approval != nil {
		approvalEvidence = observability.TrustApproval(approval)
	}
	if auditErr := s.recordPolicyAudit(r.Context(), claims, authz.VerifiedIdentity, memoryOperation(r), volumeHandle, memoryInvocationID(r), "denied", authz.GrantName, runID, err, approvalEvidence); auditErr != nil {
		writeError(w, http.StatusServiceUnavailable, "memory audit unavailable")
		return nil, err
	}
	s.logger.Info("memory access denied by runtime approval",
		zap.String("namespace", claims.Namespace),
		zap.String("agent", claims.AgentName),
		zap.String("volume", volumeHandle),
		zap.String("mode", mode),
		zap.String("invocation_id", invocationID),
		zap.String("reason_code", memoryDecisionReasonCode(err)),
		zap.Error(err))
	writeError(w, gate3HTTPStatus(err), "memory access denied by runtime approval")
	return nil, err
}

func (s *Server) requestAccess(r *http.Request, w http.ResponseWriter) (mode, volumeHandle string, body []byte, ok bool) {
	switch {
	case r.Method == http.MethodPost && (r.URL.Path == "/v1/memory/store" || r.URL.Path == "/v1/memory/list" || r.URL.Path == "/v1/memory/search"):
		body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
		if err != nil || len(body) > maxRequestBytes {
			writeError(w, http.StatusBadRequest, "invalid request body")
			return "", "", nil, false
		}
		var payload struct {
			VolumeHandle string `json:"volume_handle"`
		}
		if json.Unmarshal(body, &payload) != nil || payload.VolumeHandle == "" {
			writeError(w, http.StatusBadRequest, "volume_handle is required")
			return "", "", nil, false
		}
		if r.URL.Path == "/v1/memory/store" {
			return "ReadWrite", payload.VolumeHandle, body, true
		}
		return "ReadOnly", payload.VolumeHandle, body, true
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/memory/get/"):
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/memory/get/"), "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			writeError(w, http.StatusBadRequest, "volume_handle and id are required")
			return "", "", nil, false
		}
		return "ReadOnly", parts[0], nil, true
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/memory/delete/"):
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/memory/delete/"), "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			writeError(w, http.StatusBadRequest, "volume_handle and id are required")
			return "", "", nil, false
		}
		return "ReadWrite", parts[0], nil, true
	default:
		writeError(w, http.StatusNotFound, "memory operation is not available through the policy gateway")
		return "", "", nil, false
	}
}

// authorizeResult carries the outputs of authorize(), including the loaded
// AgentIdentity, AgentInstance, and MemoryClaim so downstream stages (such as
// ADR-0014 Gate 3 enforcement) can project them into a Gate3Invocation
// without re-reading the policy store.
type authorizeResult struct {
	VerifiedIdentity string
	GrantName        string
	RevokedGrantIDs  map[string]struct{}
	Identity         v1alpha1.AgentIdentity
	Instance         v1alpha1.AgentInstance
	Claim            v1alpha1.MemoryClaim
}

func (s *Server) authorize(ctx context.Context, claims *runtimeauth.MemoryCapabilityClaims, mode string) (authorizeResult, error) {
	result := authorizeResult{}
	identity, err := loadResource[v1alpha1.AgentIdentity](ctx, s.store, "AgentIdentity", claims.Namespace, claims.AgentIdentityName)
	if err != nil {
		return result, fmt.Errorf("load identity: %w", err)
	}
	agent, err := loadResource[v1alpha1.Agent](ctx, s.store, "Agent", claims.Namespace, claims.AgentName)
	if err != nil {
		return result, fmt.Errorf("load agent: %w", err)
	}
	if identity.ObjectMeta.DeletionTimestamp != nil || identity.Status.Phase != v1alpha1.PhaseReady ||
		strings.TrimSpace(identity.Spec.SPIFFEID) == "" ||
		identity.Spec.AgentRef.Name != agent.ObjectMeta.Name ||
		identity.ObjectMeta.Labels["kognition.dev/auto-created-for-agent"] != agent.ObjectMeta.Name ||
		!identityHasCurrentAgentOwner(identity, agent) {
		return result, errors.New("capability identity is not current and ready")
	}
	result.VerifiedIdentity = identity.Spec.SPIFFEID
	result.Identity = *identity
	instance, err := loadResource[v1alpha1.AgentInstance](ctx, s.store, "AgentInstance", claims.Namespace, claims.InstanceName)
	if err != nil || instance.ObjectMeta.DeletionTimestamp != nil || instance.ObjectMeta.UID != claims.InstanceUID ||
		instance.Spec.AgentRef.Name != agent.ObjectMeta.Name || instance.Status.Phase != v1alpha1.PhaseRunning ||
		instance.Status.SessionID != claims.SessionID {
		return result, errors.New("capability instance does not belong to the current agent")
	}
	result.Instance = *instance
	claim, err := s.claimForVolume(ctx, claims.Namespace, claims.VolumeHandle)
	if err != nil {
		return result, err
	}
	result.Claim = *claim
	revokedGrantIDs, err := controller.RevokedMemoryGrantIDs(claim.ObjectMeta.Annotations)
	if err != nil {
		return result, fmt.Errorf("read memory grant revocations: %w", err)
	}
	result.RevokedGrantIDs = revokedGrantIDs
	decision, err := controller.EvaluateMemoryAccessDecision(ctx, s.store, controller.NewRBACEngine(s.store, s.logger), identity, claim.ObjectMeta.Name, mode)
	if err != nil {
		if strings.Contains(err.Error(), "no MemoryGrant found") && len(revokedGrantIDs) > 0 {
			grantNames := make([]string, 0, len(revokedGrantIDs))
			for grantName := range revokedGrantIDs {
				grantNames = append(grantNames, grantName)
			}
			sort.Strings(grantNames)
			result.GrantName = grantNames[0]
			return result, errors.New("memory grant revoked")
		}
		return result, err
	}
	result.GrantName = decision.GrantName
	return result, nil
}

func (s *Server) claimForVolume(ctx context.Context, namespace, volumeHandle string) (*v1alpha1.MemoryClaim, error) {
	items, err := s.store.List(ctx, "MemoryVolume", store.ListOptions{Namespace: namespace})
	if err != nil {
		return nil, fmt.Errorf("list memory volumes: %w", err)
	}
	var volume *v1alpha1.MemoryVolume
	for _, raw := range items {
		var candidate v1alpha1.MemoryVolume
		if json.Unmarshal(raw, &candidate) == nil && candidate.Spec.BackendHandle == volumeHandle {
			volume = &candidate
			break
		}
	}
	if volume == nil || volume.Spec.ClaimRef == nil || volume.Spec.ClaimRef.Name == "" || volume.Status.Phase != v1alpha1.PhaseBound {
		return nil, errors.New("memory backend handle is not bound to a claim")
	}
	claim, err := loadResource[v1alpha1.MemoryClaim](ctx, s.store, "MemoryClaim", namespace, volume.Spec.ClaimRef.Name)
	if err != nil {
		return nil, fmt.Errorf("load memory claim: %w", err)
	}
	if claim.ObjectMeta.DeletionTimestamp != nil || claim.Status.BoundVolumeName != volume.ObjectMeta.Name {
		return nil, errors.New("memory claim is not bound to this volume")
	}
	return claim, nil
}

type memoryMutationOutcome struct {
	statusCode   int
	payload      []byte
	contentType  string
	errorMessage string
	terminal     bool
}

func (o memoryMutationOutcome) succeeded() bool {
	return o.statusCode >= http.StatusOK && o.statusCode < http.StatusMultipleChoices && o.errorMessage == ""
}

func (s *Server) proxy(w http.ResponseWriter, r *http.Request, body []byte, agentIdentity, workspace string, revokedGrantIDs map[string]struct{}) memoryMutationOutcome {
	target := *s.upstream
	target.Path = r.URL.Path
	target.RawQuery = r.URL.RawQuery
	request, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusBadGateway, "memory service unavailable")
		return memoryMutationOutcome{errorMessage: "create memory service request: " + err.Error()}
	}
	request.Header.Set("Content-Type", r.Header.Get("Content-Type"))
	if s.upstreamToken != "" {
		request.Header.Set("Authorization", "Bearer "+s.upstreamToken)
	}
	if agentIdentity != "" {
		request.Header.Set(memory.TrustedAgentIdentityHeader, agentIdentity)
	}
	if workspace != "" {
		request.Header.Set(memory.TrustedWorkspaceHeader, workspace)
	}
	response, err := s.httpClient.Do(request)
	if err != nil {
		writeError(w, http.StatusBadGateway, "memory service unavailable")
		return memoryMutationOutcome{errorMessage: "memory service request failed: " + err.Error()}
	}
	defer func() { _ = response.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxRequestBytes))
	if err != nil {
		writeError(w, http.StatusBadGateway, "memory service unavailable")
		return memoryMutationOutcome{statusCode: response.StatusCode, errorMessage: "read memory service response: " + err.Error()}
	}
	if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices && isMemoryRead(r.Method, r.URL.Path) {
		var revoked bool
		payload, revoked, err = filterRevokedGrantRecords(r.URL.Path, payload, revokedGrantIDs)
		if err != nil {
			writeError(w, http.StatusBadGateway, "memory service returned an invalid response")
			return memoryMutationOutcome{statusCode: response.StatusCode, payload: payload, errorMessage: "memory service returned an invalid response"}
		}
		if revoked {
			writeError(w, http.StatusNotFound, "record not found")
			return memoryMutationOutcome{statusCode: http.StatusNotFound, errorMessage: "memory record was revoked"}
		}
	}
	outcome := memoryMutationOutcome{statusCode: response.StatusCode, payload: payload, contentType: response.Header.Get("Content-Type"), terminal: true}
	if !outcome.succeeded() {
		outcome.errorMessage = fmt.Sprintf("memory service returned HTTP %d", response.StatusCode)
	}
	return outcome
}

func writeMemoryOutcome(w http.ResponseWriter, outcome memoryMutationOutcome) {
	if outcome.contentType != "" {
		w.Header().Set("Content-Type", outcome.contentType)
	}
	w.WriteHeader(outcome.statusCode)
	_, _ = w.Write(outcome.payload)
}

func stampGrantProvenance(body []byte, grantName string) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	metadata, found := payload["metadata"]
	if !found {
		metadata = map[string]any{}
	}
	metadataMap, ok := metadata.(map[string]any)
	if !ok {
		return nil, errors.New("metadata must be an object")
	}
	metadataMap[controller.MemoryGrantProvenanceMetadataKey] = grantName
	payload["metadata"] = metadataMap
	return json.Marshal(payload)
}

func isMemoryRead(method, path string) bool {
	return method == http.MethodGet && strings.HasPrefix(path, "/v1/memory/get/") ||
		method == http.MethodPost && (path == "/v1/memory/list" || path == "/v1/memory/search")
}

func filterRevokedGrantRecords(path string, payload []byte, revokedGrantIDs map[string]struct{}) ([]byte, bool, error) {
	switch path {
	case "/v1/memory/list":
		var records []map[string]any
		if err := json.Unmarshal(payload, &records); err != nil {
			return nil, false, err
		}
		filtered := records[:0]
		for _, record := range records {
			if recordUsesRevokedGrant(record, revokedGrantIDs) {
				continue
			}
			hideGrantProvenance(record)
			filtered = append(filtered, record)
		}
		encoded, err := json.Marshal(filtered)
		return encoded, false, err
	case "/v1/memory/search":
		var results []map[string]any
		if err := json.Unmarshal(payload, &results); err != nil {
			return nil, false, err
		}
		filtered := results[:0]
		for _, result := range results {
			record, _ := result["record"].(map[string]any)
			if recordUsesRevokedGrant(record, revokedGrantIDs) {
				continue
			}
			hideGrantProvenance(record)
			filtered = append(filtered, result)
		}
		encoded, err := json.Marshal(filtered)
		return encoded, false, err
	default:
		var record map[string]any
		if err := json.Unmarshal(payload, &record); err != nil {
			return nil, false, err
		}
		if recordUsesRevokedGrant(record, revokedGrantIDs) {
			return nil, true, nil
		}
		hideGrantProvenance(record)
		encoded, err := json.Marshal(record)
		return encoded, false, err
	}
}

func recordUsesRevokedGrant(record map[string]any, revokedGrantIDs map[string]struct{}) bool {
	if record == nil {
		return false
	}
	metadata, _ := record["metadata"].(map[string]any)
	grantName, _ := metadata[controller.MemoryGrantProvenanceMetadataKey].(string)
	_, revoked := revokedGrantIDs[grantName]
	return revoked
}

func hideGrantProvenance(record map[string]any) {
	if record == nil {
		return
	}
	metadata, _ := record["metadata"].(map[string]any)
	delete(metadata, controller.MemoryGrantProvenanceMetadataKey)
}

func memoryOperation(request *http.Request) string {
	if request.Method == http.MethodGet {
		return "get"
	}
	if request.Method == http.MethodDelete {
		return "delete"
	}
	return strings.TrimPrefix(request.URL.Path, "/v1/memory/")
}

func memoryInvocationID(request *http.Request) string {
	return strings.TrimSpace(request.Header.Get(MemoryInvocationIDHeader))
}

func (s *Server) recordPolicyAudit(ctx context.Context, claims *runtimeauth.MemoryCapabilityClaims, agentIdentity, operation, volumeHandle, invocationID, outcome, grantName, runID string, decisionErr error, approval *observability.TrustDecisionApproval) error {
	if s.auditLog == nil {
		return nil
	}
	auditID := uuid.NewString()
	decision := memoryTrustDecision(auditID, time.Now().UTC(), claims, agentIdentity, operation, volumeHandle, invocationID, outcome, grantName, runID, decisionErr)
	if approval != nil {
		decision.Approval = approval
	}
	details, err := json.Marshal(decision)
	if err != nil {
		return fmt.Errorf("marshal memory policy audit details: %w", err)
	}
	entry := observability.AuditEntry{
		ID:            auditID,
		Workspace:     claims.Namespace,
		AgentIdentity: agentIdentity,
		SessionID:     claims.SessionID,
		EventType:     observability.AuditMemoryOp,
		SubjectKind:   "MemoryVolume",
		SubjectRef:    volumeHandle,
		Verb:          operation,
		Details:       details,
	}
	if outcome == "authorized" {
		return s.auditLog.RecordBeforeExec(ctx, entry)
	}
	return s.auditLog.Record(ctx, entry)
}

func memoryTrustDecision(id string, timestamp time.Time, claims *runtimeauth.MemoryCapabilityClaims, agentIdentity, operation, volumeHandle, invocationID, outcome, grantName, runID string, decisionErr error) observability.TrustDecisionV1 {
	reasonCode, executionStatus := "authorized", "not_started"
	authorization := observability.TrustDecisionAuthorization{Identity: "allowed", Role: "allowed", Grant: "allowed", Policy: "allowed"}
	targetReached := false
	switch outcome {
	case "replayed":
		reasonCode, executionStatus = "replay_detected", "not_started"
	case "completed":
		reasonCode, executionStatus, targetReached = "target_completed", "completed", true
	case "failed":
		reasonCode, executionStatus, targetReached = "target_failed", "failed", true
	case "denied":
		reasonCode, executionStatus = memoryDecisionReasonCode(decisionErr), "blocked"
		authorization = observability.TrustDecisionAuthorization{Identity: "not_evaluated", Role: "not_evaluated", Grant: "denied", Policy: "not_evaluated"}
	case "revoked":
		reasonCode, executionStatus = "grant_revoked", "blocked"
		authorization = observability.TrustDecisionAuthorization{Identity: "allowed", Role: "allowed", Grant: "revoked", Policy: "not_evaluated"}
	}
	if grantName != "" {
		authorization.GrantRef = &observability.TrustDecisionResourceRef{Kind: "MemoryGrant", Namespace: claims.Namespace, Name: grantName}
	}
	approvalStatus := "unavailable"
	if strings.HasPrefix(reasonCode, "runtime_approval_") {
		authorization.Identity, authorization.Role, authorization.Grant, authorization.Policy = "allowed", "allowed", "allowed", "allowed"
		switch reasonCode {
		case "runtime_approval_pending":
			approvalStatus = "pending"
		case "runtime_approval_rejected", "runtime_approval_expired":
			approvalStatus = "rejected"
		}
	}
	decision := observability.TrustDecisionV1{
		SchemaVersion: 1, ID: id, Timestamp: timestamp, Plane: "memory", Outcome: mapMemoryOutcome(outcome), ReasonCode: reasonCode, Workspace: claims.Namespace, RunID: runID,
		Request:       observability.TrustDecisionRequest{Operation: operation, InvocationID: invocationID, TargetRef: observability.TrustDecisionResourceRef{Kind: "MemoryVolume", Namespace: claims.Namespace, Name: volumeHandle}},
		Authorization: authorization, Approval: &observability.TrustDecisionApproval{Status: approvalStatus},
		Execution: observability.TrustDecisionExecution{Status: executionStatus, TargetReached: &targetReached},
	}
	if agentIdentity != "" {
		decision.Identity = &observability.TrustDecisionIdentity{AgentIdentity: agentIdentity}
	} else if claims.AgentIdentityName != "" {
		decision.ClaimedIdentityRef = &observability.TrustDecisionResourceRef{Kind: "AgentIdentity", Namespace: claims.Namespace, Name: claims.AgentIdentityName}
	}
	if claims.SessionID != "" {
		decision.Session = &observability.TrustDecisionSession{SessionID: claims.SessionID, CapabilityID: claims.ID}
	}
	return decision
}

func mapMemoryOutcome(outcome string) string {
	if outcome == "authorized" {
		return "allowed"
	}
	return outcome
}

func memoryDecisionReasonCode(err error) string {
	if err == nil {
		return "policy_denied"
	}
	switch {
	case errors.Is(err, ErrRuntimeApprovalPending):
		return "runtime_approval_pending"
	case errors.Is(err, ErrRuntimeApprovalRejected):
		return "runtime_approval_rejected"
	case errors.Is(err, ErrRuntimeApprovalExpired):
		return "runtime_approval_expired"
	case errors.Is(err, ErrRuntimeApprovalUnavailable):
		return "runtime_approval_unavailable"
	}
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "revoked"):
		return "grant_revoked"
	case strings.Contains(message, "capability"):
		return "capability_invalid"
	case strings.Contains(message, "identity"):
		return "identity_not_ready"
	case strings.Contains(message, "rbac"):
		return "role_denied"
	case strings.Contains(message, "grant"):
		return "grant_missing"
	default:
		return "policy_denied"
	}
}

// gate3HTTPStatus maps ADR-0014 Gate 3 sentinels to the HTTP status the memory
// data-plane returns to callers. Mirrors the tool-plane convention:
// pending → 202 Accepted (retry-friendly), rejected/expired → 403 Forbidden,
// unavailable → 503 Service Unavailable.
func gate3HTTPStatus(err error) int {
	switch {
	case errors.Is(err, ErrRuntimeApprovalPending):
		return http.StatusAccepted
	case errors.Is(err, ErrRuntimeApprovalRejected):
		return http.StatusForbidden
	case errors.Is(err, ErrRuntimeApprovalExpired):
		return http.StatusForbidden
	case errors.Is(err, ErrRuntimeApprovalUnavailable):
		return http.StatusServiceUnavailable
	default:
		return http.StatusForbidden
	}
}

func bearerToken(header string) (string, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) || strings.TrimSpace(strings.TrimPrefix(header, prefix)) == "" {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(header, prefix)), true
}

func identityHasCurrentAgentOwner(identity *v1alpha1.AgentIdentity, agent *v1alpha1.Agent) bool {
	for _, owner := range identity.ObjectMeta.OwnerReferences {
		if owner.Kind == "Agent" && owner.Name == agent.ObjectMeta.Name && owner.UID != "" && owner.UID == agent.ObjectMeta.UID {
			return true
		}
	}
	return false
}

func loadResource[T any](ctx context.Context, s store.Store, kind, namespace, name string) (*T, error) {
	raw, err := s.Get(ctx, kind, namespace, name)
	if err != nil {
		return nil, err
	}
	var resource T
	if err := json.Unmarshal(raw, &resource); err != nil {
		return nil, err
	}
	return &resource, nil
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
