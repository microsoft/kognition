// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package modelgateway enforces runtime model access before forwarding an
// OpenAI-compatible chat completion to the configured upstream provider.
package modelgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/cpi/drivers/foundry"
	"github.com/kognition-ai/kognition/internal/modelinvocation"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/kognition-ai/kognition/internal/store"
)

const maxRequestBytes = 4 << 20

var errStreamingUnsupported = errors.New("streaming model completions are not supported")

// ModelAccessAuthorizer evaluates the live ModelGrant and AgentPolicy state.
type ModelAccessAuthorizer interface {
	EvaluateModelAccess(context.Context, string, string, string) error
}

// ModelQuotaChecker validates monetary and model-tier capacity before dispatch.
type ModelQuotaChecker interface {
	CheckModelQuota(context.Context, string, string) error
}

// Server is the Kubernetes model data-plane authorization boundary.
type Server struct {
	core      *Core
	issuer    *runtimeauth.ModelSessionCapabilityIssuer
	client    *http.Client
	logger    *zap.Logger
	tokens    foundry.TokenProvider
	tracer    *observability.Tracer
	collector *observability.Collector
	quota     ModelQuotaChecker
	audit     *observability.CognitiveAuditLog
	drivers   *cpi.Registry
	recorder  cpi.CallRecorder
}

// Option configures optional Model Gateway dependencies.
type Option func(*Server)

// WithCPIRegistry uses administrator-registered adapters rather than a new discovery mechanism.
func WithCPIRegistry(registry *cpi.Registry) Option {
	return func(server *Server) { server.drivers = registry }
}

// WithCallRecorder reports the outcome of every provider call made through the
// gateway's CPI registry. Observation applies to the registry in use after all
// options are applied, so it also covers a registry supplied by WithCPIRegistry.
func WithCallRecorder(recorder cpi.CallRecorder) Option {
	return func(server *Server) { server.recorder = recorder }
}

// WithObservability emits model-inference spans after the gateway verifies a
// runtime capability and receives the provider response.
func WithObservability(tracer *observability.Tracer, collector *observability.Collector) Option {
	return func(server *Server) {
		server.tracer = tracer
		server.collector = collector
	}
}

// WithQuotaChecker enforces current workspace model quotas before dispatch.
func WithQuotaChecker(checker ModelQuotaChecker) Option {
	return func(server *Server) { server.quota = checker }
}

// WithAuditLog records normalized Model Trust Decisions. Authorization
// evidence is persisted before a request can reach the provider.
func WithAuditLog(audit *observability.CognitiveAuditLog) Option {
	return func(server *Server) { server.audit = audit }
}

type upstreamAuth struct {
	Header      string
	Value       string
	Provider    string
	Credentials *cpi.Credentials
	Invocation  *modelinvocation.Resolved
	// Credential is the cpi.CredentialFingerprint of the Model's secret. It lets a
	// direct provider call be attributed to the account that made it.
	Credential string
}

func New(store store.Store, issuer *runtimeauth.ModelSessionCapabilityIssuer, authorizer ModelAccessAuthorizer, client *http.Client, logger *zap.Logger, options ...Option) (*Server, error) {
	return newServer(store, issuer, authorizer, client, logger, foundry.NewTokenProvider(), options...)
}

func newServer(store store.Store, issuer *runtimeauth.ModelSessionCapabilityIssuer, authorizer ModelAccessAuthorizer, client *http.Client, logger *zap.Logger, tokens foundry.TokenProvider, options ...Option) (*Server, error) {
	if issuer == nil {
		return nil, errors.New("model gateway requires a model session capability issuer")
	}
	core, err := NewCore(store, authorizer)
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{}
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	if tokens == nil {
		tokens = foundry.NewTokenProvider()
	}
	server := &Server{core: core, issuer: issuer, client: client, logger: logger.Named("model-gateway"), tokens: tokens}
	server.drivers = invocationDrivers(client, tokens)
	for _, option := range options {
		option(server)
	}
	if server.drivers == nil {
		return nil, errors.New("model gateway CPI registry is required")
	}
	if server.recorder != nil {
		server.drivers.Observe(server.recorder)
	}
	return server, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r = r.WithContext(observability.ContextWithTraceHeaders(r.Context(), r.Header))
	if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
		if err := s.core.Ping(r.Context()); err != nil {
			writeError(w, http.StatusServiceUnavailable, "model gateway is not ready")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" {
		writeError(w, http.StatusNotFound, "model gateway operation is not available")
		return
	}
	capability, ok := bearerToken(r.Header.Get("Authorization"))
	if !ok {
		writeError(w, http.StatusUnauthorized, "runtime model session capability is required")
		return
	}
	claims, err := s.issuer.VerifyClaims(capability)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid runtime model session capability")
		return
	}
	runID, err := observability.NormalizeTrustDecisionRunID(r.Header.Get(observability.TrustDecisionRunIDHeader))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid trust decision run ID")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBytes+1))
	if err != nil || len(body) > maxRequestBytes || !json.Valid(body) {
		writeError(w, http.StatusBadRequest, "invalid model completion request")
		return
	}
	authorized, identity, auth, err := s.authorize(r.Context(), claims)
	if err != nil {
		s.logger.Info("model invocation denied", zap.String("namespace", claims.Namespace), zap.String("agent", claims.AgentName), zap.String("model", claims.ModelName), zap.Error(err))
		if auditErr := s.recordDecision(r.Context(), claims, authorized, identity, runID, modelDeniedOutcome(err), modelDecisionReasonCode(err), "blocked", false, true); auditErr != nil {
			s.logger.Error("record denied model trust decision", zap.Error(auditErr))
			writeError(w, http.StatusServiceUnavailable, "model audit evidence is unavailable")
			return
		}
		writeError(w, http.StatusForbidden, "model invocation is not authorized")
		return
	}
	if s.quota != nil {
		var tier string
		if authorized.ModelClass != nil {
			tier = authorized.ModelClass.Spec.CostTier
		}
		if err := s.quota.CheckModelQuota(r.Context(), claims.Namespace, tier); err != nil {
			s.logger.Info("model invocation denied by quota", zap.String("namespace", claims.Namespace), zap.String("agent", claims.AgentName), zap.String("model", claims.ModelName), zap.String("model_tier", tier), zap.Error(err))
			if auditErr := s.recordDecision(r.Context(), claims, authorized, identity, runID, "denied", "quota_exhausted", "blocked", false, true); auditErr != nil {
				writeError(w, http.StatusServiceUnavailable, "model audit evidence is unavailable")
				return
			}
			writeError(w, http.StatusTooManyRequests, "model quota exhausted")
			return
		}
	}
	body, err = pinCompletionModel(body, authorized.Model.Spec.ModelID)
	if err != nil {
		if errors.Is(err, errStreamingUnsupported) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusBadRequest, "invalid model completion request")
		return
	}
	invocation := authorized.Invocation
	if invocation != nil {
		if !modelinvocation.EqualBinding(&invocation.Binding, claims.InvocationProfileBinding) {
			writeError(w, http.StatusForbidden, "invocation profile changed after authorization")
			return
		}
		driver, found := s.drivers.Get(auth.Provider)
		if !found {
			writeError(w, http.StatusBadRequest, "invocation adapter is unavailable")
			return
		}
		contractProvider, supported := driver.(cpi.InvocationContractProvider)
		if !supported {
			writeError(w, http.StatusBadRequest, "invocation adapter does not support contracts")
			return
		}
		contract, contractErr := contractProvider.InvocationContract(invocation.Binding.Contract)
		err = contractErr
		if err == nil {
			err = invocation.ValidateContract(contract)
		}
		if err == nil {
			_, err = invocation.Prepare(contract, authorized.Model.Spec.Parameters, false, false, 0)
		}
		if err == nil {
			var limit int64
			if authorized.Agent.Spec.Runtime != nil && authorized.Agent.Spec.Runtime.MaxTokens != nil {
				limit = *authorized.Agent.Spec.Runtime.MaxTokens
			}
			body, err = invocation.PrepareChatJSON(contract, body, limit)
		}
		if err != nil {
			s.logger.Info("invalid model invocation parameters", zap.Error(err))
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		auth.Invocation = invocation
		if auth.Provider != "foundry" && auth.Provider != "openai-compatible" {
			if _, err := decodeCPICompletion(body); err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
	} else if auth.Provider == "foundry" {
		body, err = foundry.PrepareChatRequest(body, authorized.Model.Spec.ModelID)
		if err != nil {
			s.logger.Info("invalid Foundry completion parameters", zap.Error(err))
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if err := s.recordDecision(r.Context(), claims, authorized, identity, runID, "allowed", "authorized", "not_started", false, true); err != nil {
		s.logger.Error("record model authorization before provider dispatch", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "model audit evidence is unavailable")
		return
	}
	span := s.startModelSpan(r.Context(), claims, identity.Spec.SPIFFEID, authorized, auth.Provider)
	usage, responseBody, contentType, err := s.proxy(w, r, body, authorized.Model, auth)
	if err != nil {
		_ = s.endModelSpan(r.Context(), span, authorized.ModelClass, cpi.Usage{}, observability.StatusError, err.Error())
		if auditErr := s.recordDecision(r.Context(), claims, authorized, identity, runID, "failed", "target_failed", "failed", true, false); auditErr != nil {
			s.logger.Error("record failed model trust decision", zap.Error(auditErr))
		}
		return
	}
	if err := s.endModelSpan(r.Context(), span, authorized.ModelClass, usage, observability.StatusOK, ""); err != nil {
		s.logger.Error("persist model usage", zap.Error(err), zap.String("namespace", claims.Namespace), zap.String("agent", claims.AgentName), zap.String("model", claims.ModelName))
		writeError(w, http.StatusServiceUnavailable, "model usage accounting is unavailable")
		return
	}
	if err := s.recordDecision(r.Context(), claims, authorized, identity, runID, "completed", "target_completed", "completed", true, false); err != nil {
		s.logger.Error("record completed model trust decision", zap.Error(err))
		writeError(w, http.StatusServiceUnavailable, "model audit evidence is unavailable")
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(responseBody)
}

func (s *Server) authorize(ctx context.Context, claims *runtimeauth.ModelSessionCapabilityClaims) (*AuthorizedModel, *v1alpha1.AgentIdentity, upstreamAuth, error) {
	agent, err := loadResource[v1alpha1.Agent](ctx, s.core.store, "Agent", claims.Namespace, claims.AgentName)
	if err != nil {
		return nil, nil, upstreamAuth{}, err
	}
	identity, err := loadResource[v1alpha1.AgentIdentity](ctx, s.core.store, "AgentIdentity", claims.Namespace, claims.AgentIdentityName)
	if err != nil {
		return nil, nil, upstreamAuth{}, fmt.Errorf("load identity: %w", err)
	}
	if identity.ObjectMeta.DeletionTimestamp != nil || identity.Status.Phase != v1alpha1.PhaseReady || identity.Spec.AgentRef.Name != agent.ObjectMeta.Name || identity.ObjectMeta.Labels["kognition.dev/auto-created-for-agent"] != agent.ObjectMeta.Name || !identityOwnedByAgent(identity, agent) {
		return nil, nil, upstreamAuth{}, errors.New("capability identity is not current and ready")
	}
	instance, err := loadResource[v1alpha1.AgentInstance](ctx, s.core.store, "AgentInstance", claims.Namespace, claims.InstanceName)
	if err != nil || instance.ObjectMeta.DeletionTimestamp != nil || instance.ObjectMeta.UID != claims.InstanceUID || instance.Spec.AgentRef.Name != agent.ObjectMeta.Name || instance.Status.Phase != v1alpha1.PhaseRunning || instance.Status.SessionID != claims.SessionID {
		return nil, identity, upstreamAuth{}, errors.New("capability instance does not belong to the active agent session")
	}
	authorized, err := s.core.AuthorizeAtRevision(ctx, claims.Namespace, claims.AgentName, claims.ModelName, claims.ModelResourceVersion)
	if err != nil {
		return nil, identity, upstreamAuth{}, err
	}
	model := authorized.Model
	if class := authorized.ModelClass; class != nil && len(class.Spec.VersionCandidates) != 0 {
		if class.Status.ActiveBinding == nil || claims.CandidateBinding == nil || *claims.CandidateBinding != *class.Status.ActiveBinding || claims.CandidateBinding.Namespace != claims.Namespace {
			return authorized, identity, upstreamAuth{}, errors.New("capability candidate is not the active serving binding")
		}
		candidate, err := runtimeauth.ResolveModelBinding(ctx, s.core.store, *claims.CandidateBinding)
		if err != nil || candidate.Spec.ClassName != class.ObjectMeta.Name || candidate.ObjectMeta.Namespace != claims.Namespace {
			return authorized, identity, upstreamAuth{}, errors.New("serving candidate is not available in this workspace")
		}
		if err := s.core.authorizer.EvaluateModelAccess(ctx, claims.Namespace, claims.AgentName, candidate.ObjectMeta.Name); err != nil {
			return authorized, identity, upstreamAuth{}, errors.New("serving candidate ModelGrant is not authorized")
		}
		model = candidate
		authorized.Model = candidate
	} else if claims.CandidateBinding != nil {
		return authorized, identity, upstreamAuth{}, errors.New("capability candidate profile is no longer active")
	}
	provider := modelGatewayProvider(model, authorized.ModelClass)
	if _, registered := s.drivers.Get(provider); provider == "" || !registered {
		return authorized, identity, upstreamAuth{}, errors.New("capability model is not supported by the Model Gateway")
	}
	invocation := authorized.Invocation
	if invocation == nil && model.Spec.InvocationProfileRef != nil {
		var err error
		invocation, err = modelinvocation.Load(ctx, s.core.store, model)
		if err != nil {
			return authorized, identity, upstreamAuth{}, err
		}
		authorized.Invocation = invocation
	}
	var binding *v1alpha1.ModelInvocationProfileBinding
	if invocation != nil {
		binding = &invocation.Binding
	}
	if invocation == nil && provider != "foundry" && provider != "openai-compatible" {
		return authorized, identity, upstreamAuth{}, errors.New("this gateway provider requires an explicit invocation profile")
	}
	if !modelinvocation.EqualBinding(binding, claims.InvocationProfileBinding) {
		return authorized, identity, upstreamAuth{}, errors.New("capability invocation profile identity or digest is no longer current")
	}
	auth, err := s.resolveUpstreamAuth(ctx, claims.Namespace, model, provider)
	if err != nil {
		return authorized, identity, upstreamAuth{}, err
	}
	auth.Provider = provider
	return authorized, identity, auth, nil
}

func (s *Server) resolveUpstreamAuth(ctx context.Context, namespace string, model *v1alpha1.Model, provider string) (upstreamAuth, error) {
	credentials, err := resolveModelCredentials(ctx, s.core.store, namespace, model, provider)
	if err != nil {
		return upstreamAuth{}, err
	}
	if credentials == nil {
		return upstreamAuth{}, nil
	}
	values := credentials.Data
	fingerprint := cpi.CredentialFingerprint(credentials)
	if provider != "foundry" && provider != "openai-compatible" {
		if provider == "anthropic" && values["apiKey"] == "" {
			return upstreamAuth{}, errors.New("Anthropic invocation requires an API-key SecretRef")
		}
		if provider == "aws-bedrock" && (values["accessKeyId"] == "" || values["secretAccessKey"] == "" || values["region"] == "") {
			return upstreamAuth{}, errors.New("Bedrock invocation requires accessKeyId, secretAccessKey and region")
		}
		return upstreamAuth{Credentials: credentials}, nil
	}
	if values["type"] == "api-key" {
		apiKey := values["apiKey"]
		if apiKey == "" {
			return upstreamAuth{}, errors.New("model API-key SecretRef is empty")
		}
		if provider == "foundry" {
			return upstreamAuth{Header: "api-key", Value: apiKey, Credential: fingerprint}, nil
		}
		return upstreamAuth{Header: "Authorization", Value: "Bearer " + apiKey, Credential: fingerprint}, nil
	}
	if provider != "foundry" || (values["type"] != "azure-identity" && values["credentialType"] == "") {
		return upstreamAuth{}, errors.New("model credential is not supported by the Model Gateway")
	}
	token, err := s.tokens.GetToken(ctx, &cpi.Credentials{Type: "azure-identity", Data: values})
	if err != nil {
		return upstreamAuth{}, fmt.Errorf("acquire Foundry Azure identity token: %w", err)
	}
	return upstreamAuth{Header: "Authorization", Value: "Bearer " + token, Credential: fingerprint}, nil
}

func (s *Server) proxy(w http.ResponseWriter, r *http.Request, body []byte, model *v1alpha1.Model, auth upstreamAuth) (cpi.Usage, []byte, string, error) {
	if auth.Provider != "foundry" && auth.Provider != "openai-compatible" {
		return s.proxyCPI(w, r, body, model, auth)
	}
	target, err := modelChatEndpoint(model, auth.Provider)
	if err != nil {
		writeError(w, http.StatusBadGateway, "model provider is unavailable")
		return cpi.Usage{}, nil, "", err
	}
	request, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusBadGateway, "model provider is unavailable")
		return cpi.Usage{}, nil, "", err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(auth.Header, auth.Value)
	for _, header := range []string{"traceparent", "tracestate"} {
		if value := r.Header.Get(header); value != "" {
			request.Header.Set(header, value)
		}
	}
	start := time.Now()
	response, err := s.client.Do(request)
	if err != nil {
		s.recordCall(auth, model, start, err)
		writeError(w, http.StatusBadGateway, "model provider is unavailable")
		return cpi.Usage{}, nil, "", err
	}
	defer func() { _ = response.Body.Close() }()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxRequestBytes+1))
	if err != nil || len(responseBody) > maxRequestBytes {
		if err == nil {
			err = errors.New("model provider response is too large")
		}
		s.recordCall(auth, model, start, err)
		writeError(w, http.StatusBadGateway, "model provider response is unavailable")
		return cpi.Usage{}, nil, "", errors.New("read model provider response")
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		s.recordCall(auth, model, start, &cpi.ProviderError{Provider: auth.Provider, Status: response.StatusCode, Reason: cpi.ClassifyStatus(response.StatusCode)})
		if response.StatusCode >= http.StatusInternalServerError {
			writeError(w, http.StatusBadGateway, "model provider is unavailable")
			return cpi.Usage{}, nil, "", fmt.Errorf("model provider returned %s", response.Status)
		}
		writeError(w, http.StatusBadGateway, "model provider rejected the request")
		return cpi.Usage{}, nil, "", fmt.Errorf("model provider returned %s", response.Status)
	}
	contentType := response.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	usage, reported := completionUsage(responseBody)
	if !reported {
		err := errors.New("model provider response omitted token usage")
		s.recordCall(auth, model, start, err)
		writeError(w, http.StatusBadGateway, err.Error())
		return cpi.Usage{}, nil, "", err
	}
	s.recordCall(auth, model, start, nil)
	return usage, responseBody, contentType, nil
}

// recordCall reports a direct provider call; calls through the CPI registry are recorded by the observed drivers.
// It carries the Model's endpoint and credential like they do, so that the
// outcome counts for the same destination and account as the Model's other calls.
func (s *Server) recordCall(auth upstreamAuth, model *v1alpha1.Model, start time.Time, err error) {
	if s.recorder == nil {
		return
	}
	outcome := cpi.CallOutcome{
		Provider: auth.Provider, Operation: cpi.OperationComplete, Model: model.Spec.ModelID,
		Endpoint: model.Spec.Endpoint, Credential: auth.Credential, Duration: time.Since(start),
	}
	if err != nil {
		outcome.Reason, outcome.ProviderFault = cpi.ClassifyCallError(err)
	}
	s.recorder.RecordCall(outcome)
}

func (s *Server) startModelSpan(ctx context.Context, claims *runtimeauth.ModelSessionCapabilityClaims, agentIdentity string, authorized *AuthorizedModel, provider string) *observability.Span {
	if s.tracer == nil || authorized == nil || authorized.Model == nil {
		return nil
	}
	attributes := map[string]interface{}{
		observability.AttrWorkspace:      claims.Namespace,
		observability.AttrAgentName:      claims.AgentName,
		observability.AttrAgentNamespace: claims.Namespace,
		observability.AttrAgentIdentity:  agentIdentity,
		observability.AttrSolution:       claims.Solution,
		observability.AttrSolutionRev:    claims.SolutionRev,
		observability.AttrSessionID:      claims.SessionID,
		observability.AttrReservationID:  "agentinstance:" + claims.InstanceUID,
		observability.AttrModelName:      authorized.Model.Spec.ModelID,
		observability.AttrModelProvider:  provider,
	}
	if claims.WorkflowName != "" {
		attributes[observability.AttrWorkflowName] = claims.WorkflowName
	}
	if claims.StepName != "" {
		attributes[observability.AttrStepName] = claims.StepName
	}
	if authorized.ModelClass != nil {
		attributes[observability.AttrModelClass] = authorized.ModelClass.ObjectMeta.Name
		attributes[observability.AttrModelClassVersion] = authorized.ModelClass.Spec.VersionPin
		attributes[observability.AttrModelCostTier] = authorized.ModelClass.Spec.CostTier
	}
	return s.tracer.StartSpan(ctx, observability.SpanNameModelInfer, observability.SpanKindClient, attributes)
}

func (s *Server) endModelSpan(ctx context.Context, span *observability.Span, modelClass *v1alpha1.ModelClass, usage cpi.Usage, status observability.StatusCode, statusMessage string) error {
	if s.tracer == nil || span == nil {
		return nil
	}
	span.Attributes[observability.AttrPromptTokens] = int64(usage.PromptTokens)
	span.Attributes[observability.AttrCompletionTokens] = int64(usage.CompletionTokens)
	span.Attributes[observability.AttrModelUsageReported] = usage.Reported
	totalTokens := usage.TotalTokens
	if totalTokens == 0 {
		totalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	span.Attributes[observability.AttrTotalTokens] = int64(totalTokens)
	if modelClass != nil {
		pricing, pricingErr := observability.ResolveModelPricing(modelClass.Spec.Parameters)
		span.Attributes[observability.AttrModelPricingConfigured] = pricing.Configured
		if pricingErr == nil && pricing.Configured {
			span.Attributes[observability.AttrModelInputRate] = pricing.InputMicrousdPerMillionTokens
			span.Attributes[observability.AttrModelOutputRate] = pricing.OutputMicrousdPerMillionTokens
		}
		cost, configured, err := observability.ModelCostUnits(modelClass.Spec.Parameters, int64(usage.PromptTokens), int64(usage.CompletionTokens))
		if err != nil {
			s.logger.Warn("invalid ModelClass pricing; omitting model cost tag", zap.String("model-class", modelClass.ObjectMeta.Name), zap.Error(err))
		} else if configured {
			span.Attributes[observability.AttrModelCostUnits] = int64(cost)
		}
	}
	if status == observability.StatusError {
		span.Attributes[observability.AttrErrorMessage] = statusMessage
	}
	sinkErr := s.tracer.EndSpan(span, status, statusMessage)
	if s.collector != nil && span.Context.TraceFlags&0x01 != 0 {
		if err := s.collector.FlushSpans(ctx, []*observability.Span{span}); err != nil {
			s.logger.Warn("failed to export model inference span", zap.Error(err))
		}
	}
	return sinkErr
}

func completionUsage(body []byte) (cpi.Usage, bool) {
	var response struct {
		Usage *struct {
			PromptTokens     *int `json:"prompt_tokens"`
			CompletionTokens *int `json:"completion_tokens"`
			TotalTokens      *int `json:"total_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &response) != nil || response.Usage == nil || response.Usage.PromptTokens == nil || response.Usage.CompletionTokens == nil {
		return cpi.Usage{}, false
	}
	return cpi.NormalizeCompletionUsage(*response.Usage.PromptTokens, *response.Usage.CompletionTokens, response.Usage.TotalTokens)
}

func openAIChatEndpoint(base string) (string, error) {
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(base), "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("invalid model endpoint")
	}
	if strings.HasSuffix(parsed.Path, "/v1") {
		parsed.Path += "/chat/completions"
	} else {
		parsed.Path += "/v1/chat/completions"
	}
	return parsed.String(), nil
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

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func modelGatewayProvider(model *v1alpha1.Model, modelClass *v1alpha1.ModelClass) string {
	provider := normalizeGatewayProvider(model.Spec.Provider)
	if provider == "" && modelClass != nil {
		provider = normalizeGatewayProvider(modelClass.Spec.Provisioner)
	}
	return provider
}

func normalizeGatewayProvider(provider string) string {
	return cpi.CanonicalProvisioner(strings.ToLower(strings.TrimSpace(provider)))
}

func modelChatEndpoint(model *v1alpha1.Model, provider string) (string, error) {
	if provider != "foundry" {
		return openAIChatEndpoint(model.Spec.Endpoint)
	}
	target, err := foundry.ChatEndpoint(model.Spec.Endpoint, model.Spec.ModelID)
	if err != nil {
		return "", err
	}
	parsed, err := url.Parse(target)
	if err != nil {
		return "", err
	}
	if version := model.Spec.Parameters["api-version"]; version != "" {
		query := parsed.Query()
		query.Set("api-version", version)
		parsed.RawQuery = query.Encode()
	} else if version := model.Spec.Parameters["apiVersion"]; version != "" {
		query := parsed.Query()
		query.Set("api-version", version)
		parsed.RawQuery = query.Encode()
	}
	return parsed.String(), nil
}

func pinCompletionModel(body []byte, modelID string) ([]byte, error) {
	if modelID == "" {
		return nil, errors.New("bound Model has no modelID")
	}
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	if stream, requested := request["stream"]; requested {
		streaming, ok := stream.(bool)
		if !ok || streaming {
			return nil, errStreamingUnsupported
		}
	}
	request["model"] = modelID
	return json.Marshal(request)
}
