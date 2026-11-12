// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package knowledgegateway exposes capability-authorized Knowledge queries to
// agent runtimes and routes them to installation-owned provider registrations.
package knowledgegateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/kognition-ai/kognition/internal/k8s/knowledge"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/runtimeauth"
	"github.com/kognition-ai/kognition/pkg/pluginsdk"
	protocol "github.com/kognition-ai/kognition/proto/gen/kognition/v1alpha1"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/types"
)

const maxRequestBytes = 4 << 20

var ErrInactiveCapabilityBinding = errors.New("runtime knowledge capability binding is inactive")

type BindingResolver interface {
	Resolve(context.Context, types.NamespacedName) (*knowledge.ResolvedBinding, error)
}

type QueryClient interface {
	PreflightQuery(context.Context, *protocol.KnowledgeQueryRequest) error
	Query(context.Context, *protocol.KnowledgeQueryRequest) (*protocol.KnowledgeQueryResponse, error)
}

type QueryEmbedder interface {
	EmbedQuery(context.Context, *runtimeauth.KnowledgeSessionCapabilityClaims, string, *knowledge.ResolvedBinding, string, string) ([]float64, error)
}

type Option func(*Server)

func WithQueryEmbedder(embedder QueryEmbedder) Option {
	return func(server *Server) { server.embedder = embedder }
}

type CapabilityBindingValidator interface {
	Validate(context.Context, *runtimeauth.KnowledgeSessionCapabilityClaims, string) error
}

type QueryConstraints struct {
	Mode     string
	TopK     int32
	MinScore *float64
	Filters  map[string]string
}

type QueryConstraintResolver interface {
	QueryConstraints(context.Context, *runtimeauth.KnowledgeSessionCapabilityClaims, string) (*QueryConstraints, error)
}

type Server struct {
	issuer    *runtimeauth.KnowledgeSessionCapabilityIssuer
	validator CapabilityBindingValidator
	resolver  BindingResolver
	clients   map[string]QueryClient
	logger    *zap.Logger
	embedder  QueryEmbedder
	audit     *observability.CognitiveAuditLog
}

type queryRequest struct {
	KnowledgeBase string            `json:"knowledge_base"`
	RequestID     string            `json:"request_id"`
	Mode          string            `json:"mode"`
	Text          string            `json:"text,omitempty"`
	QueryText     string            `json:"query_text,omitempty"`
	Vector        []float64         `json:"vector,omitempty"`
	TopK          int32             `json:"top_k"`
	Filters       map[string]string `json:"filters,omitempty"`
}

type queryMatch struct {
	KnowledgeBase string  `json:"kb"`
	DocumentID    string  `json:"document_id"`
	ChunkID       string  `json:"chunk_id"`
	Content       string  `json:"chunk"`
	Score         float64 `json:"score"`
	SourceURI     string  `json:"source_uri"`
	Location      string  `json:"location,omitempty"`
}

type queryResponse struct {
	RequestID string       `json:"request_id"`
	Matches   []queryMatch `json:"matches"`
}

func New(issuer *runtimeauth.KnowledgeSessionCapabilityIssuer, validator CapabilityBindingValidator, resolver BindingResolver, clients map[string]QueryClient, logger *zap.Logger, options ...Option) (*Server, error) {
	if issuer == nil || validator == nil || resolver == nil || len(clients) == 0 {
		return nil, errors.New("knowledge gateway requires a capability issuer, live binding validator, binding resolver and provider registrations")
	}
	registered := make(map[string]QueryClient, len(clients))
	for name, client := range clients {
		if strings.TrimSpace(name) == "" || client == nil {
			return nil, errors.New("knowledge gateway contains an invalid provider registration")
		}
		registered[name] = client
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	server := &Server{issuer: issuer, validator: validator, resolver: resolver, clients: registered, logger: logger.Named("knowledge-gateway")}
	for _, option := range options {
		if option == nil {
			return nil, errors.New("knowledge gateway option is invalid")
		}
		option(server)
	}
	return server, nil
}

func (server *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	switch request.URL.Path {
	case "/healthz":
		if request.Method != http.MethodGet {
			writer.Header().Set("Allow", http.MethodGet)
			writeError(writer, http.StatusMethodNotAllowed, "method is not allowed")
			return
		}
		writeJSON(writer, http.StatusOK, map[string]string{"status": "ok"})
		return
	case "/v1/query":
		if request.Method != http.MethodPost {
			writer.Header().Set("Allow", http.MethodPost)
			writeError(writer, http.StatusMethodNotAllowed, "method is not allowed")
			return
		}
	default:
		writeError(writer, http.StatusNotFound, "knowledge gateway operation is not available")
		return
	}
	capability, ok := bearerToken(request.Header.Get("Authorization"))
	if !ok {
		writeError(writer, http.StatusUnauthorized, "runtime knowledge session capability is required")
		return
	}
	claims, err := server.issuer.VerifyClaims(capability)
	if err != nil {
		writeError(writer, http.StatusUnauthorized, "invalid runtime knowledge session capability")
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxRequestBytes+1))
	if err != nil || len(body) > maxRequestBytes {
		writeError(writer, http.StatusRequestEntityTooLarge, "knowledge query limit exceeded")
		return
	}
	var input queryRequest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		writeError(writer, http.StatusBadRequest, "invalid knowledge query request")
		return
	}
	if !slices.Contains(claims.KnowledgeBases, input.KnowledgeBase) {
		writeError(writer, http.StatusForbidden, "knowledge query is not authorized")
		return
	}
	if err := server.validator.Validate(request.Context(), claims, input.KnowledgeBase); err != nil {
		if errors.Is(err, ErrInactiveCapabilityBinding) {
			writeError(writer, http.StatusForbidden, "knowledge query is not authorized")
		} else {
			server.logger.Error("validate runtime knowledge capability binding", zap.Error(err))
			writeError(writer, http.StatusServiceUnavailable, "knowledge authorization state is unavailable")
		}
		return
	}
	binding, err := server.resolver.Resolve(request.Context(), types.NamespacedName{Namespace: claims.Namespace, Name: input.KnowledgeBase})
	if err != nil {
		server.writeProviderError(writer, err)
		return
	}
	var constraints *QueryConstraints
	if resolver, ok := server.validator.(QueryConstraintResolver); ok {
		constraints, err = resolver.QueryConstraints(request.Context(), claims, input.KnowledgeBase)
		if err != nil {
			server.logger.Warn("resolve live knowledge query constraints", zap.Error(err))
			writeError(writer, http.StatusServiceUnavailable, "knowledge query constraints are unavailable")
			return
		}
		if constraints != nil {
			if constraints.Mode != "" && constraints.Mode != input.Mode || constraints.TopK > 0 && input.TopK > constraints.TopK {
				writeError(writer, http.StatusForbidden, "knowledge query exceeds its claim retrieval constraints")
				return
			}
			for key, value := range constraints.Filters {
				actual, present := input.Filters[key]
				if !present || actual != value {
					writeError(writer, http.StatusForbidden, "knowledge query does not satisfy its claim filters")
					return
				}
			}
		}
	}
	client, found := server.clients[binding.ProviderName]
	if !found {
		writeError(writer, http.StatusServiceUnavailable, "knowledge provider is not registered")
		return
	}
	filters := make([]*protocol.KnowledgeFilter, 0, len(input.Filters))
	keys := make([]string, 0, len(input.Filters))
	for key := range input.Filters {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		filters = append(filters, &protocol.KnowledgeFilter{Key: key, Value: input.Filters[key]})
	}
	providerRequest := &protocol.KnowledgeQueryRequest{
		Scope: binding.Scope, RequestId: input.RequestID, Mode: input.Mode, Text: input.Text,
		Vector: input.Vector, TopK: input.TopK, Filters: filters,
	}
	embedText := false
	if input.QueryText != "" {
		if len(input.QueryText) > 65536 || strings.TrimSpace(input.QueryText) == "" || input.Text != "" || len(input.Vector) != 0 {
			writeError(writer, http.StatusBadRequest, "query_text conflicts with explicit text/vector or exceeds its bound")
			return
		}
		switch input.Mode {
		case "keyword":
			providerRequest.Text = input.QueryText
		case "dense":
			providerRequest.Vector = []float64{1}
			embedText = true
		case "hybrid":
			providerRequest.Text, providerRequest.Vector = input.QueryText, []float64{1}
			embedText = true
		default:
			writeError(writer, http.StatusBadRequest, "unsupported knowledge query mode")
			return
		}
	}
	if err := pluginsdk.ValidateKnowledgeQuery(providerRequest); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid knowledge query request")
		return
	}
	if err := client.PreflightQuery(request.Context(), providerRequest); err != nil {
		server.writeProviderError(writer, err)
		return
	}
	if embedText && (server.embedder == nil || server.audit == nil) {
		writeError(writer, http.StatusServiceUnavailable, "governed server-side query embedding is not configured")
		return
	}
	if err := server.recordQuery(request.Context(), claims, binding, input, "allowed", true); err != nil {
		writeError(writer, http.StatusServiceUnavailable, "knowledge query authorization audit is unavailable")
		return
	}
	if embedText {
		vector, err := server.embedder.EmbedQuery(request.Context(), claims, input.KnowledgeBase, binding, input.QueryText, input.RequestID)
		if err != nil {
			if auditErr := server.recordQuery(request.Context(), claims, binding, input, "failed", false); auditErr != nil {
				writeError(writer, http.StatusServiceUnavailable, "knowledge query failure audit is unavailable")
				return
			}
			if status.Code(err) == codes.PermissionDenied {
				writeError(writer, http.StatusForbidden, "knowledge query embedding is not authorized")
				return
			}
			server.writeProviderError(writer, err)
			return
		}
		providerRequest.Vector = vector
		if err := pluginsdk.ValidateKnowledgeQuery(providerRequest); err != nil {
			if auditErr := server.recordQuery(request.Context(), claims, binding, input, "failed", false); auditErr != nil {
				writeError(writer, http.StatusServiceUnavailable, "knowledge query failure audit is unavailable")
				return
			}
			writeError(writer, http.StatusBadGateway, "query embedder returned an invalid vector")
			return
		}
	}
	result, err := client.Query(request.Context(), providerRequest)
	if err == nil && result == nil {
		err = status.Error(codes.DataLoss, "knowledge provider returned no response")
	}
	if err != nil {
		server.logger.Info("knowledge provider query failed", zap.String("namespace", claims.Namespace), zap.String("knowledgeBase", input.KnowledgeBase), zap.String("provider", binding.ProviderName), zap.Error(err))
		if auditErr := server.recordQuery(request.Context(), claims, binding, input, "failed", false); auditErr != nil {
			writeError(writer, http.StatusServiceUnavailable, "knowledge query failure audit is unavailable")
			return
		}
		server.writeProviderError(writer, err)
		return
	}
	output := queryResponse{RequestID: result.RequestId, Matches: make([]queryMatch, 0, len(result.Matches))}
	for _, match := range result.Matches {
		if constraints != nil && constraints.MinScore != nil && match.Score < *constraints.MinScore {
			continue
		}
		output.Matches = append(output.Matches, queryMatch{
			KnowledgeBase: input.KnowledgeBase, DocumentID: match.DocumentId, ChunkID: match.ChunkId,
			Content: match.Content, Score: match.Score, SourceURI: match.SourceUri, Location: match.Location,
		})
	}
	if err := server.recordQuery(request.Context(), claims, binding, input, "completed", false); err != nil {
		writeError(writer, http.StatusServiceUnavailable, "knowledge query completion audit is unavailable")
		return
	}
	writeJSON(writer, http.StatusOK, output)
}

func (server *Server) writeProviderError(writer http.ResponseWriter, err error) {
	switch status.Code(err) {
	case codes.InvalidArgument, codes.Unimplemented:
		writeError(writer, http.StatusBadRequest, "invalid knowledge query request")
	case codes.PermissionDenied, codes.Unauthenticated:
		writeError(writer, http.StatusBadGateway, "knowledge provider credentials were rejected")
	case codes.ResourceExhausted:
		writeError(writer, http.StatusTooManyRequests, "knowledge provider throttled the query")
	case codes.FailedPrecondition, codes.NotFound:
		writeError(writer, http.StatusConflict, "knowledge binding is not queryable")
	case codes.Unavailable, codes.DeadlineExceeded:
		writeError(writer, http.StatusServiceUnavailable, "knowledge provider is unavailable")
	default:
		writeError(writer, http.StatusBadGateway, "knowledge provider is unavailable")
	}
}

func bearerToken(header string) (string, bool) {
	scheme, token, found := strings.Cut(strings.TrimSpace(header), " ")
	return token, found && strings.EqualFold(scheme, "Bearer") && strings.TrimSpace(token) != ""
}

func writeError(writer http.ResponseWriter, code int, message string) {
	writeJSON(writer, code, map[string]string{"error": message})
}

func writeJSON(writer http.ResponseWriter, code int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(code)
	_ = json.NewEncoder(writer).Encode(value)
}
