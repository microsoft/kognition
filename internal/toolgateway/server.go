// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package toolgateway

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/runtime"
	"github.com/kognition-ai/kognition/internal/runtimeapproval"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/kognition-ai/kognition/internal/store"
)

const maxRequestBytes = 1 << 20

// Server authenticates runtime-originated tool requests before delegating to
// the shared Tool Gateway core. It is the Kubernetes data-plane boundary; the
// core itself remains transport-independent for standalone parity.
type Server struct {
	store         store.Store
	sessionIssuer *runtimeauth.ToolSessionCapabilityIssuer
	toolIssuer    *runtimeauth.ToolCapabilityIssuer
	gateway       *Gateway
	logger        *zap.Logger
}

// NewServer creates a Tool Gateway HTTP server.
func NewServer(store store.Store, sessionIssuer *runtimeauth.ToolSessionCapabilityIssuer, toolIssuer *runtimeauth.ToolCapabilityIssuer, gateway *Gateway, logger *zap.Logger) (*Server, error) {
	if store == nil {
		return nil, errors.New("tool gateway server requires a store")
	}
	if sessionIssuer == nil || toolIssuer == nil {
		return nil, errors.New("tool gateway server requires session and tool capability issuers")
	}
	if gateway == nil {
		return nil, errors.New("tool gateway server requires a gateway")
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Server{store: store, sessionIssuer: sessionIssuer, toolIssuer: toolIssuer, gateway: gateway, logger: logger.Named("tool-gateway-server")}, nil
}

// ServeHTTP handles runtime tool invocation requests.
func (s *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	request = request.WithContext(observability.ContextWithTraceHeaders(request.Context(), request.Header))
	if request.Method == http.MethodGet && request.URL.Path == "/healthz" {
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	if request.Method != http.MethodPost {
		writeError(writer, http.StatusNotFound, "tool gateway operation is not available")
		return
	}
	switch request.URL.Path {
	case "/v1/tools/capabilities":
		s.exchangeCapability(writer, request)
		return
	case "/v1/tools/invoke":
	default:
		writeError(writer, http.StatusNotFound, "tool gateway operation is not available")
		return
	}
	capability, ok := bearerToken(request.Header.Get("Authorization"))
	if !ok {
		writeError(writer, http.StatusUnauthorized, "runtime tool capability is required")
		return
	}
	claims, err := s.toolIssuer.VerifyClaims(capability)
	if err != nil {
		writeError(writer, http.StatusUnauthorized, "invalid runtime tool capability")
		return
	}
	runID, err := observability.NormalizeTrustDecisionRunID(request.Header.Get(observability.TrustDecisionRunIDHeader))
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid trust decision run ID")
		return
	}
	var body struct {
		ToolName            string         `json:"tool_name"`
		ToolResourceVersion int64          `json:"tool_resource_version"`
		InvocationID        string         `json:"invocation_id"`
		Input               map[string]any `json:"input"`
	}
	raw, err := io.ReadAll(io.LimitReader(request.Body, maxRequestBytes+1))
	_, canonicalErr := runtimeapproval.CanonicalizeJSON(raw)
	if err != nil || len(raw) > maxRequestBytes || canonicalErr != nil || json.Unmarshal(raw, &body) != nil {
		writeError(writer, http.StatusBadRequest, "invalid tool invocation request")
		return
	}
	inputDigest, err := toolInputDigest(body.Input)
	if err != nil || body.ToolName != claims.ToolName || body.ToolResourceVersion != claims.ToolResourceVersion || body.InvocationID != claims.InvocationID || inputDigest != claims.InputDigest || claims.Verb != string(v1alpha1.ToolGrantVerbInvoke) {
		s.logger.Info("tool invocation denied", zap.String("stage", "capability-binding"),
			zap.String("request_tool", body.ToolName), zap.String("capability_tool", claims.ToolName),
			zap.Int64("request_tool_version", body.ToolResourceVersion), zap.Int64("capability_tool_version", claims.ToolResourceVersion),
			zap.String("request_invocation", body.InvocationID), zap.String("capability_invocation", claims.InvocationID),
			zap.String("capability_verb", claims.Verb))
		writeError(writer, http.StatusForbidden, "runtime tool capability does not permit this invocation")
		return
	}
	agentIdentity, err := s.validateWorkload(request.Context(), claims)
	if err != nil {
		s.logger.Info("tool invocation denied", zap.String("namespace", claims.Namespace), zap.String("agent", claims.AgentName), zap.String("tool", claims.ToolName), zap.Error(err))
		writeError(writer, http.StatusForbidden, "tool invocation is not authorized")
		return
	}
	response, err := s.gateway.Invoke(request.Context(), Request{
		Namespace: claims.Namespace, Workspace: claims.Namespace, Principal: claims.AgentName, AgentNamespace: claims.Namespace, AgentIdentity: agentIdentity, Solution: claims.Solution, SolutionRev: claims.SolutionRev, SessionID: claims.SessionID, RunID: runID, SubjectKind: "AgentInstance", SubjectName: claims.InstanceName,
		ToolName: claims.ToolName, ToolResourceVersion: claims.ToolResourceVersion, CapabilityIssued: true, CapabilityID: claims.ID, InvocationID: claims.InvocationID, Input: body.Input,
	})
	if err != nil {
		s.logger.Info("tool invocation failed", zap.String("namespace", claims.Namespace), zap.String("agent", claims.AgentName), zap.String("tool", claims.ToolName), zap.String("invocation", claims.InvocationID), zap.Error(err))
		s.writeInvocationError(writer, response, err)
		return
	}
	writeJSON(writer, http.StatusOK, response)
}

// exchangeCapability exchanges a non-executable runtime session capability
// for one exact Tool invocation capability after current workload, tool, and
// policy validation. Authenticated authorization denials are audited, but no
// invocation receipt or quota is reserved before execution.
func (s *Server) exchangeCapability(writer http.ResponseWriter, request *http.Request) {
	sessionCapability, ok := bearerToken(request.Header.Get("Authorization"))
	if !ok {
		s.logger.Info("tool capability exchange denied", zap.String("stage", "missing-session-capability"))
		writeError(writer, http.StatusUnauthorized, "runtime tool session capability is required")
		return
	}
	claims, err := s.sessionIssuer.VerifyClaims(sessionCapability)
	if err != nil {
		s.logger.Info("tool capability exchange denied", zap.String("stage", "invalid-session-capability"), zap.Error(err))
		writeError(writer, http.StatusUnauthorized, "invalid runtime tool session capability")
		return
	}
	runID, err := observability.NormalizeTrustDecisionRunID(request.Header.Get(observability.TrustDecisionRunIDHeader))
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid trust decision run ID")
		return
	}
	var body struct {
		ToolName     string         `json:"tool_name"`
		InvocationID string         `json:"invocation_id"`
		Input        map[string]any `json:"input"`
	}
	raw, err := io.ReadAll(io.LimitReader(request.Body, maxRequestBytes+1))
	canonical, canonicalErr := runtimeapproval.CanonicalizeJSON(raw)
	if err != nil || len(raw) > maxRequestBytes || canonicalErr != nil || json.Unmarshal(canonical, &body) != nil || body.ToolName == "" || body.InvocationID == "" || body.Input == nil {
		s.logger.Info("tool capability exchange denied", zap.String("stage", "invalid-request"), zap.String("namespace", claims.Namespace), zap.String("agent", claims.AgentName), zap.String("instance", claims.InstanceName))
		writeError(writer, http.StatusBadRequest, "tool name, invocation ID, and input are required")
		return
	}
	agentIdentity, err := s.validateSessionWorkload(request.Context(), claims)
	if err != nil {
		s.logger.Info("tool capability exchange denied", zap.String("stage", "workload-validation"), zap.String("namespace", claims.Namespace), zap.String("agent", claims.AgentName), zap.String("instance", claims.InstanceName), zap.String("tool", body.ToolName), zap.Error(err))
		writeError(writer, http.StatusForbidden, "runtime tool session is not authorized")
		return
	}
	tool, err := loadTool(request.Context(), s.store, claims.Namespace, body.ToolName)
	if err != nil {
		s.logger.Info("tool capability exchange denied", zap.String("stage", "tool-lookup"), zap.String("namespace", claims.Namespace), zap.String("agent", claims.AgentName), zap.String("instance", claims.InstanceName), zap.String("tool", body.ToolName), zap.Error(err))
		s.writeGatewayError(writer, err)
		return
	}
	if decision, err := s.gateway.authorizer.AuthorizeToolInvocation(request.Context(), claims.Namespace, claims.AgentName, tool.ObjectMeta.Name, body.Input); err != nil {
		s.logger.Info("tool capability exchange denied", zap.String("stage", "policy-authorization"), zap.String("namespace", claims.Namespace), zap.String("agent", claims.AgentName), zap.String("instance", claims.InstanceName), zap.String("tool", tool.ObjectMeta.Name), zap.Error(err))
		auditErr := s.gateway.recordAuthorizationDenial(request.Context(), Request{
			Namespace: claims.Namespace, Workspace: claims.Namespace, Principal: claims.AgentName,
			AgentNamespace: claims.Namespace, AgentIdentity: agentIdentity, Solution: claims.Solution, SolutionRev: claims.SolutionRev,
			SessionID: claims.SessionID, RunID: runID, SubjectKind: "AgentInstance", SubjectName: claims.InstanceName,
			ToolName: tool.ObjectMeta.Name, ToolResourceVersion: tool.ObjectMeta.ResourceVersion, InvocationID: body.InvocationID,
		}, tool, decision, err)
		if auditErr != nil {
			s.logger.Error("persist tool capability denial", zap.String("namespace", claims.Namespace),
				zap.String("agent", claims.AgentName), zap.String("invocation", body.InvocationID), zap.Error(auditErr))
			writeError(writer, http.StatusServiceUnavailable, "tool authorization audit is unavailable")
			return
		}
		writeError(writer, http.StatusForbidden, "tool invocation is not authorized")
		return
	}
	inputDigest, err := toolInputDigest(body.Input)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid tool invocation input")
		return
	}
	capability, err := s.toolIssuer.Issue(runtimeauth.ToolCapabilityBinding{
		Namespace: claims.Namespace, AgentName: claims.AgentName, AgentIdentityName: claims.AgentIdentityName,
		InstanceName: claims.InstanceName, InstanceUID: claims.InstanceUID, SessionID: claims.SessionID, Solution: claims.Solution, SolutionRev: claims.SolutionRev,
		ToolName: tool.ObjectMeta.Name, ToolResourceVersion: tool.ObjectMeta.ResourceVersion,
		Verb: string(v1alpha1.ToolGrantVerbInvoke), InvocationID: body.InvocationID, InputDigest: inputDigest,
	})
	if err != nil {
		s.logger.Error("issue tool capability", zap.Error(err))
		writeError(writer, http.StatusInternalServerError, "could not issue tool capability")
		return
	}
	writeJSON(writer, http.StatusOK, struct {
		Capability          string `json:"capability"`
		ToolResourceVersion int64  `json:"tool_resource_version"`
	}{
		Capability:          capability,
		ToolResourceVersion: tool.ObjectMeta.ResourceVersion,
	})
}

func toolInputDigest(input map[string]any) (string, error) {
	canonical, err := canonicalizeInvocationInput(input)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(canonical)), nil
}

func (s *Server) validateWorkload(ctx context.Context, claims *runtimeauth.ToolCapabilityClaims) (string, error) {
	agent, err := loadResource[v1alpha1.Agent](ctx, s.store, "Agent", claims.Namespace, claims.AgentName)
	if err != nil {
		return "", fmt.Errorf("load agent: %w", err)
	}
	if err := runtimeauth.RequireAgentWorkloadPeer(ctx, agent); err != nil {
		return "", err
	}
	identity, err := loadResource[v1alpha1.AgentIdentity](ctx, s.store, "AgentIdentity", claims.Namespace, claims.AgentIdentityName)
	if err != nil {
		return "", fmt.Errorf("load identity: %w", err)
	}
	if identity.ObjectMeta.DeletionTimestamp != nil || identity.Status.Phase != v1alpha1.PhaseReady || identity.Spec.AgentRef.Name != agent.ObjectMeta.Name ||
		identity.ObjectMeta.Labels["kognition.dev/auto-created-for-agent"] != agent.ObjectMeta.Name || !identityOwnedByAgent(identity, agent) {
		return "", errors.New("capability identity is not current and ready")
	}
	instance, err := loadResource[v1alpha1.AgentInstance](ctx, s.store, "AgentInstance", claims.Namespace, claims.InstanceName)
	if err != nil || instance.ObjectMeta.DeletionTimestamp != nil || instance.ObjectMeta.UID != claims.InstanceUID || instance.Spec.AgentRef.Name != agent.ObjectMeta.Name ||
		instance.Status.Phase != v1alpha1.PhaseRunning || instance.Status.SessionID != claims.SessionID {
		return "", errors.New("capability instance does not belong to the current agent session")
	}
	return identity.Spec.SPIFFEID, nil
}

func (s *Server) validateSessionWorkload(ctx context.Context, claims *runtimeauth.ToolSessionCapabilityClaims) (string, error) {
	return s.validateWorkload(ctx, &runtimeauth.ToolCapabilityClaims{
		Namespace: claims.Namespace, AgentName: claims.AgentName, AgentIdentityName: claims.AgentIdentityName,
		InstanceName: claims.InstanceName, InstanceUID: claims.InstanceUID, SessionID: claims.SessionID, Solution: claims.Solution, SolutionRev: claims.SolutionRev,
	})
}

func (s *Server) writeGatewayError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrToolInvocationInProgress):
		writeError(writer, http.StatusConflict, "tool invocation is already in progress")
	case errors.Is(err, ErrToolNotFound):
		writeError(writer, http.StatusNotFound, "tool not found")
	case errors.Is(err, ErrToolVersionMismatch):
		writeError(writer, http.StatusConflict, "tool version changed; request a new capability")
	default:
		var structured *runtime.StructuredToolError
		if errors.As(err, &structured) && structured.Code == "ToolAuditCompletionFailed" {
			writeError(writer, http.StatusInternalServerError, structured.Message)
			return
		}
		writeError(writer, http.StatusForbidden, "tool invocation is not authorized")
	}
}

func (s *Server) writeInvocationError(writer http.ResponseWriter, response Response, err error) {
	if errors.Is(err, ErrRuntimeApprovalPending) {
		// ADR-0014 tagged transport: pending runtime approval is a
		// non-terminal outcome. Return 202 Accepted so the caller retries
		// with the same invocation ID once the approval flips ready.
		writeJSON(writer, http.StatusAccepted, response)
		return
	}
	if errors.Is(err, store.ErrConflict) {
		writeJSON(writer, http.StatusConflict, response)
		return
	}
	var structured *runtime.StructuredToolError
	if errors.As(err, &structured) && structured.Code == "ToolAuditCompletionFailed" {
		if response.ErrorCode == "" {
			response.ErrorCode = structured.Code
			response.ErrorMessage = structured.Message
		}
		writeJSON(writer, http.StatusInternalServerError, response)
		return
	}
	if response.RuntimeApprovalState == RuntimeApprovalStateUnavailable {
		if response.ErrorCode == "" {
			response.ErrorCode = "ToolInvocationUnavailable"
			response.ErrorMessage = "tool invocation outcome is temporarily unavailable"
		}
		writeJSON(writer, http.StatusServiceUnavailable, response)
		return
	}
	response.ErrorCode = "ToolInvocationDenied"
	response.ErrorMessage = "tool invocation is not authorized"
	if errors.As(err, &structured) {
		response.ErrorCode = structured.Code
	}
	writeJSON(writer, http.StatusForbidden, response)
}

func loadResource[T any](ctx context.Context, store store.Store, kind, namespace, name string) (*T, error) {
	raw, err := store.Get(ctx, kind, namespace, name)
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
