// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package modelgateway

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strings"

	api "github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/cpi/drivers/foundry"
	"github.com/kognition-ai/kognition/internal/k8s/knowledge"
	"github.com/kognition-ai/kognition/internal/modelinvocation"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/kognition-ai/kognition/internal/store"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// EmbeddingExecutor shares the model boundary without exposing an HTTP facade.
type EmbeddingExecutor struct {
	server *Server
}

func NewEmbeddingExecutor(storage store.Store, authorizer ModelAccessAuthorizer, client *http.Client, logger *zap.Logger, options ...Option) (*EmbeddingExecutor, error) {
	core, err := NewCore(storage, authorizer)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{}
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	tokens := foundry.NewTokenProvider()
	server := &Server{core: core, client: client, tokens: tokens, logger: logger.Named("query-embedding"), drivers: invocationDrivers(client, tokens)}
	for _, option := range options {
		if option == nil {
			return nil, errors.New("query embedding option is invalid")
		}
		option(server)
	}
	if server.drivers == nil || server.quota == nil || server.audit == nil || server.tracer == nil {
		return nil, errors.New("query embedding requires registered adapters, quota enforcement, audit and usage tracing")
	}
	return &EmbeddingExecutor{server: server}, nil
}

func (executor *EmbeddingExecutor) EmbedQuery(ctx context.Context, claims *runtimeauth.KnowledgeSessionCapabilityClaims, knowledgeBase string, binding *knowledge.ResolvedBinding, text, requestID string) ([]float64, error) {
	if claims == nil || binding == nil || binding.Scope == nil || binding.EmbeddingDimension < 1 || binding.EmbeddingDimension > 4096 ||
		strings.TrimSpace(text) == "" || len(text) > 65536 {
		return nil, status.Error(codes.InvalidArgument, "invalid bound knowledge embedding request")
	}
	runID, err := observability.NormalizeTrustDecisionRunID(requestID)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "invalid knowledge query request identity")
	}
	server := executor.server
	authorized, err := server.core.AuthorizeKnowledgeEmbedding(ctx, claims.Namespace, claims.AgentName, knowledgeBase, binding.Scope)
	if err != nil {
		var resolution *ResourceResolutionError
		if errors.As(err, &resolution) {
			return nil, status.Error(codes.Unavailable, "knowledge embedding authorization state is unavailable")
		}
		return nil, status.Error(codes.PermissionDenied, "knowledge embedding authorization or index binding changed")
	}
	if authorized.Agent.UID != claims.AgentUID {
		return nil, status.Error(codes.PermissionDenied, "knowledge embedding Agent identity changed")
	}
	identity, err := loadResource[api.AgentIdentity](ctx, server.core.store, "AgentIdentity", claims.Namespace, claims.AgentIdentityName)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, status.Error(codes.Unavailable, "knowledge embedding identity state is unavailable")
	}
	if err != nil || identity.DeletionTimestamp != nil || identity.UID != claims.AgentIdentityUID ||
		identity.Status.Phase != api.PhaseReady || identity.Spec.AgentRef.Name != claims.AgentName || !identityOwnedByAgent(identity, authorized.Agent) {
		return nil, status.Error(codes.PermissionDenied, "knowledge embedding identity is inactive")
	}
	instance, err := loadResource[api.AgentInstance](ctx, server.core.store, "AgentInstance", claims.Namespace, claims.InstanceName)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, status.Error(codes.Unavailable, "knowledge embedding session state is unavailable")
	}
	if err != nil || instance.DeletionTimestamp != nil || instance.UID != claims.InstanceUID ||
		instance.Spec.AgentRef.Name != claims.AgentName || instance.Status.Phase != api.PhaseRunning || instance.Status.SessionID != claims.SessionID {
		return nil, status.Error(codes.PermissionDenied, "knowledge embedding session is inactive")
	}
	model := authorized.Model
	modelClaims := &runtimeauth.ModelSessionCapabilityClaims{
		Namespace: claims.Namespace, AgentName: claims.AgentName, AgentIdentityName: claims.AgentIdentityName,
		InstanceName: claims.InstanceName, InstanceUID: claims.InstanceUID, SessionID: claims.SessionID,
		ModelName: model.Name, ModelResourceVersion: model.ResourceVersion,
	}
	modelClaims.ID = claims.ID
	var tier string
	if authorized.ModelClass != nil {
		tier = authorized.ModelClass.Spec.CostTier
	}
	if err := server.quota.CheckModelQuota(ctx, claims.Namespace, tier); err != nil {
		if auditErr := server.recordOperationDecision(ctx, modelClaims, authorized, identity, "embed", runID, "denied", "quota_exhausted", "blocked", false, true); auditErr != nil {
			return nil, status.Error(codes.Unavailable, "query embedding audit is unavailable")
		}
		return nil, status.Error(codes.ResourceExhausted, "query embedding quota exhausted")
	}
	provider := modelGatewayProvider(model, authorized.ModelClass)
	driver, found := server.drivers.Get(provider)
	if !found {
		return nil, status.Error(codes.FailedPrecondition, "query embedding adapter is unavailable")
	}
	invocation, err := cpi.ResolveInvocation(ctx, server.core.store, driver, model)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "query embedding invocation contract is unavailable")
	}
	var expected *api.ModelInvocationProfileBinding
	if authorized.Invocation != nil {
		expected = &authorized.Invocation.Binding
	}
	if invocation != nil && !modelinvocation.EqualBinding(&invocation.Binding, expected) {
		return nil, status.Error(codes.PermissionDenied, "query embedding invocation profile changed")
	}
	credentials, err := resolveModelCredentials(ctx, server.core.store, claims.Namespace, model, provider)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "query embedding credential is unavailable")
	}
	if err := server.recordOperationDecision(ctx, modelClaims, authorized, identity, "embed", runID, "allowed", "authorized", "not_started", false, true); err != nil {
		return nil, status.Error(codes.Unavailable, "query embedding audit is unavailable")
	}
	span := server.startModelSpan(ctx, modelClaims, identity.Spec.SPIFFEID, authorized, provider)
	response, providerErr := driver.Embed(ctx, &cpi.EmbedRequest{
		Model: model.Spec.ModelID, Endpoint: model.Spec.Endpoint, Creds: credentials, Invocation: invocation,
		Parameters: model.Spec.Parameters, Inputs: []string{text},
	})
	usage := cpi.Usage{}
	if response != nil {
		usage = response.Usage
		if usage.PromptTokens == 0 && usage.TotalTokens > 0 {
			usage.PromptTokens = usage.TotalTokens
		}
	}
	failureCode := codes.Unavailable
	if providerErr == nil && (response == nil || len(response.Embeddings) != 1 || len(response.Embeddings[0]) != binding.EmbeddingDimension || !usage.Reported) {
		providerErr = errors.New("embedding response or usage does not match the committed index")
		failureCode = codes.DataLoss
	}
	if providerErr == nil {
		for _, value := range response.Embeddings[0] {
			if math.IsNaN(value) || math.IsInf(value, 0) {
				providerErr = errors.New("embedding response contains a nonfinite vector")
				failureCode = codes.DataLoss
				break
			}
		}
	}
	if providerErr != nil {
		accountErr := server.endModelSpan(ctx, span, authorized.ModelClass, usage, observability.StatusError, "query embedding failed")
		auditErr := server.recordOperationDecision(ctx, modelClaims, authorized, identity, "embed", runID, "failed", "target_failed", "failed", true, false)
		if accountErr != nil || auditErr != nil {
			return nil, status.Error(codes.Unavailable, "query embedding failure evidence is unavailable")
		}
		return nil, status.Error(failureCode, "query embedding provider failed or returned incompatible data")
	}
	if err := server.endModelSpan(ctx, span, authorized.ModelClass, usage, observability.StatusOK, ""); err != nil {
		return nil, status.Error(codes.Unavailable, "query embedding usage accounting is unavailable")
	}
	if err := server.recordOperationDecision(ctx, modelClaims, authorized, identity, "embed", runID, "completed", "target_completed", "completed", true, false); err != nil {
		return nil, status.Error(codes.Unavailable, "query embedding completion audit is unavailable")
	}
	return append([]float64(nil), response.Embeddings[0]...), nil
}
