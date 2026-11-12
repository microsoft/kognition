// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/modelgateway"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/requestid"
	"github.com/kognition-ai/kognition/internal/store"
)

const (
	modelInferenceFailedMessage = "model inference failed"
	toolInvocationFailedMessage = "tool invocation failed"
	maxChatToolRounds           = 8
)

var errChatToolInvocation = errors.New("chat tool invocation failed")
var errChatUsageAccounting = errors.New("chat model usage accounting failed")

type agentRevisionContextKey struct{}

type chatAttribution struct {
	workspace         string
	agentName         string
	agentIdentity     string
	solution          string
	solutionRev       int64
	sessionID         string
	modelName         string
	modelProvider     string
	modelClass        string
	modelClassVersion string
	modelCostTier     string
	modelParameters   map[string]string
	reservationID     string
}

func withAgentRevision(request *http.Request, revision int64) *http.Request {
	return request.WithContext(context.WithValue(request.Context(), agentRevisionContextKey{}, revision))
}

func agentRevisionFromRequest(request *http.Request) (int64, bool) {
	revision, ok := request.Context().Value(agentRevisionContextKey{}).(int64)
	return revision, ok
}

// ChatRequest is the request body for chatting with an agent.
type ChatRequest struct {
	// Message is the user message to send to the agent.
	Message string `json:"message"`

	// SessionID is an optional session identifier for multi-turn conversations.
	SessionID string `json:"sessionID,omitempty"`
}

// ChatResponse is the response returned from the chat endpoint.
type ChatResponse struct {
	// SessionID is the session identifier for continuing the conversation.
	SessionID string `json:"sessionID"`

	// Response is the agent's reply.
	Response string `json:"response"`

	// Usage reports token consumption for this turn.
	Usage *ChatUsage `json:"usage,omitempty"`
}

// ChatUsage reports token consumption.
type ChatUsage struct {
	PromptTokens     int `json:"promptTokens"`
	CompletionTokens int `json:"completionTokens"`
	TotalTokens      int `json:"totalTokens"`
}

// handleChat accepts a message and returns the agent's response.
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	namespace := r.PathValue("namespace")
	agentName := r.PathValue("name")

	if s.cpiRegistry == nil {
		writeError(w, http.StatusServiceUnavailable, "chat is not available: no CPI drivers configured")
		return
	}

	// Read and validate request body.
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	if len(body) == 0 {
		writeError(w, http.StatusBadRequest, "request body is required")
		return
	}

	var req ChatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	if strings.TrimSpace(req.Message) == "" {
		writeError(w, http.StatusBadRequest, "message is required and cannot be empty")
		return
	}

	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = uuid.New().String()
	}
	chatSpan, tracedRequest := s.startChatSpan(r, namespace, agentName, sessionID)
	if tracedRequest != r {
		r = tracedRequest
	}
	chatStatus := observability.StatusOK
	chatStatusMsg := ""
	var chatUsage cpi.Usage
	defer func() {
		s.endChatSpan(r.Context(), chatSpan, chatUsage, chatStatus, chatStatusMsg)
	}()

	// Fetch the Agent.
	agentRaw, err := s.store.Get(r.Context(), "Agent", namespace, agentName)
	if err != nil {
		chatStatus, chatStatusMsg = chatSpanError(chatSpan, err)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, fmt.Sprintf("Agent %q not found in namespace %q", agentName, namespace))
			return
		}
		s.logger.Error("get agent failed", zap.Error(err), requestid.ZapField(r.Context()))
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	var agent v1alpha1.Agent
	if err := json.Unmarshal(agentRaw, &agent); err != nil {
		chatStatus, chatStatusMsg = chatSpanError(chatSpan, err)
		s.logger.Error("unmarshal agent failed", zap.Error(err), requestid.ZapField(r.Context()))
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	executionPinned := false
	if revision, pinned := agentRevisionFromRequest(r); pinned {
		if revision <= 0 {
			chatStatus, chatStatusMsg = chatSpanError(chatSpan, fmt.Errorf("invalid agent revision %d", revision))
			writeError(w, http.StatusInternalServerError, "invalid pinned agent revision")
			return
		}
		snapshot, snapshotErr := s.store.GetRevision(r.Context(), "Agent", namespace, agentName, revision)
		if snapshotErr != nil {
			chatStatus, chatStatusMsg = chatSpanError(chatSpan, snapshotErr)
			if errors.Is(snapshotErr, store.ErrNotFound) {
				writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("Agent revision %d not found", revision))
				return
			}
			s.logger.Error("get agent revision failed", zap.Error(snapshotErr), requestid.ZapField(r.Context()))
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		var revisionAgent v1alpha1.Agent
		if err := json.Unmarshal(snapshot.Data, &revisionAgent); err != nil {
			chatStatus, chatStatusMsg = chatSpanError(chatSpan, err)
			s.logger.Error("unmarshal agent revision failed", zap.Error(err), requestid.ZapField(r.Context()))
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		agent.Spec = revisionAgent.Spec
		executionPinned = true
	}

	if agent.Status.Phase != v1alpha1.PhaseReady {
		chatStatus, chatStatusMsg = chatSpanError(chatSpan, fmt.Errorf("agent phase %s", agent.Status.Phase))
		writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("Agent %q is not ready (phase: %s)", agentName, agent.Status.Phase))
		return
	}

	var model v1alpha1.Model
	var modelClass v1alpha1.ModelClass
	if s.modelGatewayCore != nil {
		var authorized *modelgateway.AuthorizedModel
		var authorizeErr error
		if executionPinned {
			authorized, authorizeErr = s.modelGatewayCore.AuthorizeExecution(r.Context(), namespace, agentName, agent.Spec.ModelRef.Name)
		} else {
			authorized, authorizeErr = s.modelGatewayCore.Authorize(r.Context(), namespace, agentName, agent.Spec.ModelRef.Name)
		}
		if authorizeErr != nil {
			chatStatus, chatStatusMsg = chatSpanError(chatSpan, authorizeErr)
			if auditErr := modelgateway.RecordStandaloneDecision(r.Context(), s.auditLog, modelgateway.StandaloneDecisionInput{Agent: &agent, RunID: r.Header.Get("X-Kognition-Run-ID"), Workspace: namespace, SessionID: sessionID, ModelName: agent.Spec.ModelRef.Name}, modelgateway.DeniedOutcome(authorizeErr), modelgateway.DecisionReasonCode(authorizeErr), "blocked", false, true); auditErr != nil {
				s.logger.Error("record denied model trust decision", zap.Error(auditErr), requestid.ZapField(r.Context()))
				writeError(w, http.StatusServiceUnavailable, "model audit evidence is unavailable")
				return
			}
			var resolutionErr *modelgateway.ResourceResolutionError
			if errors.As(authorizeErr, &resolutionErr) {
				if errors.Is(resolutionErr.Err, store.ErrNotFound) {
					switch resolutionErr.Resource {
					case "Model":
						writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("Model %q referenced by agent not found", agent.Spec.ModelRef.Name))
					case "ModelClass":
						writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("ModelClass %q referenced by model not found", agent.Spec.ModelRef.Name))
					default:
						writeError(w, http.StatusInternalServerError, "internal error")
					}
					return
				}
				writeError(w, http.StatusInternalServerError, "internal error")
				return
			}
			s.logger.Warn("model access denied", zap.String("agent", agentName), zap.String("namespace", namespace), zap.String("model", agent.Spec.ModelRef.Name), requestid.ZapField(r.Context()))
			writeError(w, http.StatusForbidden, "model access denied")
			return
		}
		if authorized.ModelClass == nil {
			chatStatus, chatStatusMsg = chatSpanError(chatSpan, fmt.Errorf("Model %q has no ModelClass", agent.Spec.ModelRef.Name))
			writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("ModelClass referenced by model %q not found", agent.Spec.ModelRef.Name))
			return
		}
		model = *authorized.Model
		modelClass = *authorized.ModelClass
	} else {
		modelRaw, getErr := s.store.Get(r.Context(), "Model", namespace, agent.Spec.ModelRef.Name)
		if getErr != nil {
			chatStatus, chatStatusMsg = chatSpanError(chatSpan, getErr)
			if errors.Is(getErr, store.ErrNotFound) {
				writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("Model %q referenced by agent not found", agent.Spec.ModelRef.Name))
				return
			}
			s.logger.Error("get model failed", zap.Error(getErr), requestid.ZapField(r.Context()))
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if err := json.Unmarshal(modelRaw, &model); err != nil {
			chatStatus, chatStatusMsg = chatSpanError(chatSpan, err)
			s.logger.Error("unmarshal model failed", zap.Error(err), requestid.ZapField(r.Context()))
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if s.modelAccessAuthorizer != nil {
			if err := s.modelAccessAuthorizer.EvaluateModelAccess(r.Context(), namespace, agentName, agent.Spec.ModelRef.Name); err != nil {
				chatStatus, chatStatusMsg = chatSpanError(chatSpan, err)
				s.logger.Warn("model access denied", zap.String("agent", agentName), zap.String("namespace", namespace), zap.String("model", agent.Spec.ModelRef.Name), requestid.ZapField(r.Context()))
				writeError(w, http.StatusForbidden, "model access denied")
				return
			}
		}
		classRaw, classErr := s.store.Get(r.Context(), "ModelClass", "", model.Spec.ClassName)
		if classErr != nil {
			chatStatus, chatStatusMsg = chatSpanError(chatSpan, classErr)
			if errors.Is(classErr, store.ErrNotFound) {
				writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("ModelClass %q referenced by model not found", model.Spec.ClassName))
				return
			}
			s.logger.Error("get model class failed", zap.Error(classErr), requestid.ZapField(r.Context()))
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if err := json.Unmarshal(classRaw, &modelClass); err != nil {
			chatStatus, chatStatusMsg = chatSpanError(chatSpan, err)
			s.logger.Error("unmarshal model class failed", zap.Error(err), requestid.ZapField(r.Context()))
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	attribution := s.resolveChatAttribution(r.Context(), namespace, &agent, &model, &modelClass, sessionID)
	s.applyChatAttribution(chatSpan, attribution)

	// Get the CPI driver for this provisioner.
	driver, ok := s.cpiRegistry.Get(modelClass.Spec.Provisioner)
	if !ok {
		chatStatus, chatStatusMsg = chatSpanError(chatSpan, fmt.Errorf("no CPI driver registered for provisioner %q", modelClass.Spec.Provisioner))
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("no CPI driver registered for provisioner %q", modelClass.Spec.Provisioner))
		return
	}

	// Resolve credentials if present.
	var creds *cpi.Credentials
	if model.Spec.CredentialRef != nil {
		secretRaw, err := s.store.Get(r.Context(), "SecretRef", namespace, model.Spec.CredentialRef.Name)
		if err == nil {
			var secretRef v1alpha1.SecretRef
			if err := json.Unmarshal(secretRaw, &secretRef); err == nil {
				creds = s.resolveSecretRefCredentials(&secretRef)
			}
		}
	}

	// Build the system prompt from agent instructions.
	systemPrompt := agent.Spec.Instructions.Inline
	if systemPrompt == "" && agent.Spec.Instructions.Ref != nil {
		promptRaw, err := s.store.Get(r.Context(), "Prompt", namespace, agent.Spec.Instructions.Ref.Name)
		if err == nil {
			var prompt v1alpha1.Prompt
			if err := json.Unmarshal(promptRaw, &prompt); err == nil {
				systemPrompt = prompt.Spec.ResolvedContent()
			}
		}
	}

	// Build messages for the completion request.
	messages := make([]cpi.Message, 0, 2)
	if systemPrompt != "" {
		messages = append(messages, cpi.Message{Role: "system", Content: systemPrompt})
	}
	messages = append(messages, cpi.Message{Role: "user", Content: req.Message})

	completionReq := &cpi.CompletionRequest{
		Model: model.Spec.ModelID, Endpoint: model.Spec.Endpoint, Creds: creds,
		Messages: messages, Parameters: model.Spec.Parameters,
		Tools: s.resolveChatTools(r, namespace, &agent),
	}
	if agent.Spec.Runtime != nil && agent.Spec.Runtime.MaxTokens != nil {
		completionReq.MaxCompletionTokens = *agent.Spec.Runtime.MaxTokens
	}
	completionReq, err = cpi.BindCompletion(r.Context(), s.store, driver, &model, completionReq)
	if err != nil {
		s.logger.Info("invalid model invocation", zap.Error(err), requestid.ZapField(r.Context()))
		writeError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	// Reserve quota only after all local resolution succeeds, so pre-dispatch
	// validation errors cannot leak reservations.
	reservationID := "chat:" + uuid.NewString()
	attribution.reservationID = reservationID
	if s.chatQuotaEnforcer != nil {
		if err := s.chatQuotaEnforcer.CheckSchedulerQuota(r.Context(), namespace); err != nil {
			chatStatus, chatStatusMsg = chatSpanError(chatSpan, err)
			s.logger.Warn("chat quota exhausted", zap.String("agent", agentName), zap.String("namespace", namespace), requestid.ZapField(r.Context()))
			writeError(w, http.StatusTooManyRequests, "chat quota exhausted")
			return
		}
		if err := s.chatQuotaEnforcer.CheckModelQuota(r.Context(), namespace, modelClass.Spec.CostTier); err != nil {
			chatStatus, chatStatusMsg = chatSpanError(chatSpan, err)
			s.logger.Warn("chat model quota exhausted", zap.String("agent", agentName), zap.String("namespace", namespace), zap.String("model_tier", modelClass.Spec.CostTier), requestid.ZapField(r.Context()))
			writeError(w, http.StatusTooManyRequests, "chat model quota exhausted")
			return
		}
		inputCeiling := estimateChatInputTokens(systemPrompt, req.Message)
		outputCeiling := configuredOutputTokenCeiling(&agent, &model)
		if completionReq.Invocation != nil {
			for _, key := range []string{"max_tokens", "max_completion_tokens"} {
				if value := completionReq.Parameters[key]; value != "" {
					outputCeiling, err = strconv.ParseInt(value, 10, 64)
					if err != nil {
						writeError(w, http.StatusUnprocessableEntity, "invalid resolved completion-token limit")
						return
					}
				}
			}
		}
		if err := s.chatQuotaEnforcer.CheckCallBudget(r.Context(), namespace, agentName, 0, inputCeiling+outputCeiling); err != nil {
			chatStatus, chatStatusMsg = chatSpanError(chatSpan, err)
			s.logger.Warn("chat session budget exceeded", zap.String("agent", agentName), zap.String("namespace", namespace), requestid.ZapField(r.Context()))
			writeError(w, http.StatusTooManyRequests, "chat session budget exceeded")
			return
		}
		if err := s.chatQuotaEnforcer.ReserveTokens(r.Context(), namespace, reservationID, inputCeiling, outputCeiling); err != nil {
			chatStatus, chatStatusMsg = chatSpanError(chatSpan, err)
			s.logger.Warn("chat token reservation denied", zap.String("agent", agentName), zap.String("namespace", namespace), requestid.ZapField(r.Context()))
			writeError(w, http.StatusTooManyRequests, "chat token budget exceeded")
			return
		}
	}

	// Call the model via CPI driver.
	modelDecision := modelgateway.StandaloneDecisionInput{Agent: &agent, RunID: r.Header.Get("X-Kognition-Run-ID"), Workspace: namespace, SessionID: sessionID, ModelName: agent.Spec.ModelRef.Name, Model: &model}
	if err := modelgateway.RecordStandaloneDecision(r.Context(), s.auditLog, modelDecision, "allowed", "authorized", "not_started", false, true); err != nil {
		s.logger.Error("record model authorization before provider dispatch", zap.Error(err), requestid.ZapField(r.Context()))
		writeError(w, http.StatusServiceUnavailable, "model audit evidence is unavailable")
		return
	}

	s.logger.Info("chat request received",
		zap.String("agent", agentName),
		zap.String("namespace", namespace),
		zap.String("model", model.Spec.ModelID),
	)

	resp, usage, err := s.completeChatWithTools(r, driver, completionReq, attribution)
	chatUsage = usage
	if err != nil {
		chatStatus, chatStatusMsg = chatSpanError(chatSpan, err)
		if auditErr := modelgateway.RecordStandaloneDecision(r.Context(), s.auditLog, modelDecision, "failed", "target_failed", "failed", true, false); auditErr != nil {
			s.logger.Error("record failed model trust decision", zap.Error(auditErr), requestid.ZapField(r.Context()))
		}
		if !errors.Is(err, errChatUsageAccounting) {
			s.settleChatQuota(r, namespace, agentName, reservationID, int64(usage.PromptTokens), int64(usage.CompletionTokens))
		}
		s.logger.Error("completion request failed", zap.String("agent", agentName), requestid.ZapField(r.Context()))
		failureMessage := modelInferenceFailedMessage
		if errors.Is(err, errChatToolInvocation) {
			failureMessage = toolInvocationFailedMessage
		}
		_ = s.store.AppendEvent(r.Context(), "Agent", namespace, agentName, store.InstanceEvent{
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Type:      "Warning",
			Reason:    "ChatFailed",
			Message:   failureMessage,
		})
		writeError(w, http.StatusBadGateway, failureMessage)
		return
	}

	s.settleChatQuota(r, namespace, agentName, reservationID, int64(usage.PromptTokens), int64(usage.CompletionTokens))
	if err := modelgateway.RecordStandaloneDecision(r.Context(), s.auditLog, modelDecision, "completed", "target_completed", "completed", true, false); err != nil {
		s.logger.Error("record completed model trust decision", zap.Error(err), requestid.ZapField(r.Context()))
		writeError(w, http.StatusServiceUnavailable, "model audit evidence is unavailable")
		return
	}

	s.logger.Info("chat request completed",
		zap.String("agent", agentName),
		zap.String("namespace", namespace),
		zap.String("sessionID", sessionID),
		zap.Int("promptTokens", usage.PromptTokens),
		zap.Int("completionTokens", usage.CompletionTokens),
	)

	_ = s.store.AppendEvent(r.Context(), "Agent", namespace, agentName, store.InstanceEvent{
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Type:      "Normal",
		Reason:    "ChatCompleted",
		Message:   fmt.Sprintf("session=%s tokens=%d", sessionID, usage.TotalTokens),
	})

	writeJSON(w, http.StatusOK, ChatResponse{
		SessionID: sessionID,
		Response:  resp.Content,
		Usage: &ChatUsage{
			PromptTokens:     usage.PromptTokens,
			CompletionTokens: usage.CompletionTokens,
			TotalTokens:      usage.TotalTokens,
		},
	})
}

func (s *Server) resolveChatTools(r *http.Request, namespace string, agent *v1alpha1.Agent) []cpi.ToolDefinition {
	if s.chatToolInvoker == nil {
		return nil
	}
	definitions := make([]cpi.ToolDefinition, 0, len(agent.Spec.Tools))
	for _, reference := range agent.Spec.Tools {
		raw, err := s.store.Get(r.Context(), "Tool", namespace, reference.Ref.Name)
		if err != nil {
			s.logger.Warn("chat tool unavailable", zap.String("tool", reference.Ref.Name), requestid.ZapField(r.Context()))
			continue
		}
		var tool v1alpha1.Tool
		if err := json.Unmarshal(raw, &tool); err != nil {
			s.logger.Warn("chat tool is invalid", zap.String("tool", reference.Ref.Name), requestid.ZapField(r.Context()))
			continue
		}
		schema := tool.Spec.Schema
		if schema == nil {
			schema = tool.Spec.InputSchema
		}
		definitions = append(definitions, cpi.ToolDefinition{
			Name:        tool.ObjectMeta.Name,
			Description: tool.Spec.Description,
			Parameters:  schema,
		})
	}
	return definitions
}

func (s *Server) completeChatWithTools(r *http.Request, driver cpi.Driver, request *cpi.CompletionRequest, attribution chatAttribution) (*cpi.CompletionResponse, cpi.Usage, error) {
	var usage cpi.Usage
	for round := range maxChatToolRounds {
		modelSpan, modelCtx := s.startChatModelSpan(r.Context(), attribution, request, round+1)
		response, err := driver.Complete(modelCtx, request)
		if err != nil {
			_ = s.endChatModelSpan(r.Context(), modelSpan, attribution, cpi.Usage{}, observability.StatusError, err.Error())
			return nil, usage, err
		}
		if !response.Usage.Reported {
			usage.Reported = false
			message := "model provider response omitted token usage"
			_ = s.endChatModelSpan(r.Context(), modelSpan, attribution, cpi.Usage{}, observability.StatusError, message)
			return nil, usage, fmt.Errorf("%w: %s", errChatUsageAccounting, message)
		}
		turnTotal := response.Usage.TotalTokens
		if turnTotal == 0 {
			turnTotal = response.Usage.PromptTokens + response.Usage.CompletionTokens
			response.Usage.TotalTokens = turnTotal
		}
		usage.PromptTokens += response.Usage.PromptTokens
		usage.CompletionTokens += response.Usage.CompletionTokens
		usage.TotalTokens += turnTotal
		usage.Reported = true
		if err := s.endChatModelSpan(r.Context(), modelSpan, attribution, response.Usage, observability.StatusOK, ""); err != nil {
			return nil, usage, fmt.Errorf("%w: persist model usage: %w", errChatUsageAccounting, err)
		}
		if len(response.ToolCalls) == 0 {
			return response, usage, nil
		}
		if s.chatToolInvoker == nil {
			return nil, usage, fmt.Errorf("%w: no tool invoker configured", errChatToolInvocation)
		}
		request.Messages = append(request.Messages, cpi.Message{
			Role: "assistant", Content: response.Content, ToolCalls: response.ToolCalls,
		})
		for _, toolCall := range response.ToolCalls {
			arguments := map[string]any{}
			if strings.TrimSpace(toolCall.Arguments) != "" {
				if err := json.Unmarshal([]byte(toolCall.Arguments), &arguments); err != nil {
					return nil, usage, fmt.Errorf("%w: decode arguments for tool %q", errChatToolInvocation, toolCall.Name)
				}
			}
			toolSpan, toolCtx := s.startChatToolSpan(r.Context(), attribution, toolCall.Name, arguments)
			toolStarted := time.Now()
			output, err := s.chatToolInvoker.InvokeTool(toolCtx, attribution.workspace, attribution.agentName, toolCall.Name, arguments)
			if err != nil {
				s.endChatToolSpan(r.Context(), toolSpan, output, toolStarted, observability.StatusError, err.Error())
				return nil, usage, fmt.Errorf("%w: invoke tool %q", errChatToolInvocation, toolCall.Name)
			}
			s.endChatToolSpan(r.Context(), toolSpan, output, toolStarted, observability.StatusOK, "")
			content, err := json.Marshal(output)
			if err != nil {
				return nil, usage, fmt.Errorf("%w: encode output for tool %q", errChatToolInvocation, toolCall.Name)
			}
			request.Messages = append(request.Messages, cpi.Message{
				Role: "tool", Content: string(content), ToolCallID: toolCall.ID,
			})
		}
	}
	return nil, usage, fmt.Errorf("%w: exceeded %d model/tool rounds", errChatToolInvocation, maxChatToolRounds)
}

func (s *Server) startChatSpan(r *http.Request, namespace, agentName, sessionID string) (*observability.Span, *http.Request) {
	if s.chatTracer == nil {
		return nil, r
	}
	attributes := map[string]interface{}{
		observability.AttrWorkspace:      namespace,
		observability.AttrAgentNamespace: namespace,
		observability.AttrAgentName:      agentName,
		observability.AttrSessionID:      sessionID,
	}
	ctx := observability.ContextWithTraceHeaders(r.Context(), r.Header)
	var span *observability.Span
	if _, ok := observability.SpanContextFromContext(ctx); ok {
		span = s.chatTracer.StartSpan(ctx, observability.SpanNameAgentChat, observability.SpanKindServer, attributes)
	} else {
		span = s.chatTracer.StartRootSpan(observability.SpanNameAgentChat, observability.SpanKindServer, attributes)
	}
	ctx = observability.ContextWithSpanContext(ctx, span.Context)
	return span, r.WithContext(ctx)
}

func (s *Server) endChatSpan(ctx context.Context, span *observability.Span, usage cpi.Usage, status observability.StatusCode, statusMsg string) {
	if s.chatTracer == nil || span == nil {
		return
	}
	span.Attributes[observability.AttrPromptTokens] = usage.PromptTokens
	span.Attributes[observability.AttrCompletionTokens] = usage.CompletionTokens
	span.Attributes[observability.AttrTotalTokens] = usage.TotalTokens
	span.Attributes[observability.AttrModelUsageReported] = usage.Reported
	if err := s.chatTracer.EndSpan(span, status, statusMsg); err != nil {
		s.logger.Warn("failed to end chat span", zap.Error(err), requestid.ZapField(ctx))
	}
	if s.chatCollector == nil || span.Context.TraceFlags&0x01 == 0 {
		return
	}
	if err := s.chatCollector.FlushSpans(ctx, []*observability.Span{span}); err != nil {
		s.logger.Warn("failed to export chat span", zap.Error(err), requestid.ZapField(ctx))
	}
}

func (s *Server) startChatModelSpan(ctx context.Context, attribution chatAttribution, request *cpi.CompletionRequest, round int) (*observability.Span, context.Context) {
	if s.chatTracer == nil {
		return nil, ctx
	}
	span := s.chatTracer.StartSpan(ctx, observability.SpanNameModelInfer, observability.SpanKindClient, map[string]interface{}{
		observability.AttrWorkspace:         attribution.workspace,
		observability.AttrAgentNamespace:    attribution.workspace,
		observability.AttrAgentName:         attribution.agentName,
		observability.AttrAgentIdentity:     attribution.agentIdentity,
		observability.AttrSolution:          attribution.solution,
		observability.AttrSolutionRev:       attribution.solutionRev,
		observability.AttrSessionID:         attribution.sessionID,
		observability.AttrReservationID:     attribution.reservationID,
		observability.AttrModelName:         request.Model,
		observability.AttrModelProvider:     attribution.modelProvider,
		observability.AttrModelClass:        attribution.modelClass,
		observability.AttrModelClassVersion: attribution.modelClassVersion,
		observability.AttrModelCostTier:     attribution.modelCostTier,
		"kognition.model.round":             round,
	})
	return span, observability.ContextWithSpanContext(ctx, span.Context)
}

func (s *Server) endChatModelSpan(ctx context.Context, span *observability.Span, attribution chatAttribution, usage cpi.Usage, status observability.StatusCode, statusMsg string) error {
	if s.chatTracer == nil || span == nil {
		return nil
	}
	span.Attributes[observability.AttrPromptTokens] = usage.PromptTokens
	span.Attributes[observability.AttrCompletionTokens] = usage.CompletionTokens
	span.Attributes[observability.AttrModelUsageReported] = usage.Reported
	totalTokens := usage.TotalTokens
	if totalTokens == 0 {
		totalTokens = usage.PromptTokens + usage.CompletionTokens
	}
	span.Attributes[observability.AttrTotalTokens] = totalTokens
	pricing, pricingErr := observability.ResolveModelPricing(attribution.modelParameters)
	span.Attributes[observability.AttrModelPricingConfigured] = pricing.Configured
	if pricingErr == nil && pricing.Configured {
		span.Attributes[observability.AttrModelInputRate] = pricing.InputMicrousdPerMillionTokens
		span.Attributes[observability.AttrModelOutputRate] = pricing.OutputMicrousdPerMillionTokens
	}
	if cost, configured, err := observability.ModelCostUnits(attribution.modelParameters, int64(usage.PromptTokens), int64(usage.CompletionTokens)); err != nil {
		s.logger.Warn("invalid ModelClass pricing; omitting model cost tag", zap.String("model", attribution.modelName), zap.Error(err))
	} else if configured {
		span.Attributes[observability.AttrModelCostUnits] = int64(cost)
	}
	if status == observability.StatusError {
		span.Attributes[observability.AttrErrorMessage] = statusMsg
	}
	sinkErr := s.chatTracer.EndSpan(span, status, statusMsg)
	if s.chatCollector == nil || span.Context.TraceFlags&0x01 == 0 {
		return sinkErr
	}
	if err := s.chatCollector.FlushSpans(ctx, []*observability.Span{span}); err != nil {
		s.logger.Warn("failed to export chat model span", zap.Error(err), requestid.ZapField(ctx))
	}
	return sinkErr
}

func (s *Server) startChatToolSpan(ctx context.Context, attribution chatAttribution, toolName string, input map[string]any) (*observability.Span, context.Context) {
	if s.chatTracer == nil {
		return nil, ctx
	}
	span := s.chatTracer.StartSpan(ctx, observability.SpanNameToolCall, observability.SpanKindClient, map[string]interface{}{
		observability.AttrWorkspace:      attribution.workspace,
		observability.AttrAgentNamespace: attribution.workspace,
		observability.AttrAgentName:      attribution.agentName,
		observability.AttrAgentIdentity:  attribution.agentIdentity,
		observability.AttrSolution:       attribution.solution,
		observability.AttrSolutionRev:    attribution.solutionRev,
		observability.AttrSessionID:      attribution.sessionID,
		observability.AttrToolName:       toolName,
		observability.AttrToolArgs:       input,
	})
	return span, observability.ContextWithSpanContext(ctx, span.Context)
}

func (s *Server) resolveChatAttribution(ctx context.Context, namespace string, agent *v1alpha1.Agent, model *v1alpha1.Model, modelClass *v1alpha1.ModelClass, sessionID string) chatAttribution {
	attribution := chatAttribution{workspace: namespace, sessionID: sessionID}
	if agent != nil {
		attribution.agentName = agent.ObjectMeta.Name
		attribution.agentIdentity = agent.ObjectMeta.Name
		if agent.ObjectMeta.Labels != nil {
			attribution.solution = agent.ObjectMeta.Labels["kognition.dev/solution"]
		}
	}
	if model != nil {
		attribution.modelName = model.Spec.ModelID
	}
	if modelClass != nil {
		attribution.modelProvider = modelClass.Spec.Provisioner
		attribution.modelClass = modelClass.ObjectMeta.Name
		attribution.modelClassVersion = modelClass.Spec.VersionPin
		attribution.modelCostTier = modelClass.Spec.CostTier
		attribution.modelParameters = modelClass.Spec.Parameters
	}
	if attribution.agentName != "" {
		identityName := "agent-identity-" + attribution.agentName
		if raw, err := s.store.Get(ctx, "AgentIdentity", namespace, identityName); err == nil {
			var identity v1alpha1.AgentIdentity
			if json.Unmarshal(raw, &identity) == nil && identity.Spec.SPIFFEID != "" {
				attribution.agentIdentity = identity.Spec.SPIFFEID
			}
		}
	}
	if attribution.solution != "" {
		if raw, err := s.store.Get(ctx, "Solution", namespace, attribution.solution); err == nil {
			var solution v1alpha1.Solution
			if json.Unmarshal(raw, &solution) == nil {
				attribution.solutionRev = solution.Status.ActiveRevision
			}
		}
	}
	return attribution
}

func (s *Server) applyChatAttribution(span *observability.Span, attribution chatAttribution) {
	if span == nil {
		return
	}
	span.Attributes[observability.AttrAgentIdentity] = attribution.agentIdentity
	span.Attributes[observability.AttrSolution] = attribution.solution
	span.Attributes[observability.AttrSolutionRev] = attribution.solutionRev
	span.Attributes[observability.AttrModelName] = attribution.modelName
	span.Attributes[observability.AttrModelProvider] = attribution.modelProvider
}

func (s *Server) endChatToolSpan(ctx context.Context, span *observability.Span, output map[string]any, started time.Time, status observability.StatusCode, statusMsg string) {
	if s.chatTracer == nil || span == nil {
		return
	}
	span.Attributes[observability.AttrToolLatencyMs] = time.Since(started).Milliseconds()
	if output != nil {
		span.Attributes[observability.AttrToolResult] = output
	}
	if status == observability.StatusError {
		span.Attributes[observability.AttrErrorMessage] = statusMsg
	}
	if err := s.chatTracer.EndSpan(span, status, statusMsg); err != nil {
		s.logger.Warn("failed to end chat tool span", zap.Error(err), requestid.ZapField(ctx))
	}
	if s.chatCollector == nil || span.Context.TraceFlags&0x01 == 0 {
		return
	}
	if err := s.chatCollector.FlushSpans(ctx, []*observability.Span{span}); err != nil {
		s.logger.Warn("failed to export chat tool span", zap.Error(err), requestid.ZapField(ctx))
	}
}

func chatSpanError(span *observability.Span, err error) (observability.StatusCode, string) {
	if err == nil {
		return observability.StatusOK, ""
	}
	if span != nil {
		span.Attributes[observability.AttrErrorMessage] = err.Error()
	}
	return observability.StatusError, err.Error()
}

func estimateChatInputTokens(parts ...string) int64 {
	var characters int64
	for _, part := range parts {
		characters += int64(len([]rune(part)))
	}
	if characters == 0 {
		return 0
	}
	return (characters + 3) / 4
}

func configuredOutputTokenCeiling(agent *v1alpha1.Agent, model *v1alpha1.Model) int64 {
	if agent.Spec.Runtime != nil && agent.Spec.Runtime.MaxTokens != nil && *agent.Spec.Runtime.MaxTokens > 0 {
		return *agent.Spec.Runtime.MaxTokens
	}
	for _, key := range []string{"max_tokens", "maxTokens"} {
		if value := model.Spec.Parameters[key]; value != "" {
			ceiling, err := strconv.ParseInt(value, 10, 64)
			if err == nil && ceiling > 0 {
				return ceiling
			}
		}
	}
	return 0
}

func (s *Server) settleChatQuota(r *http.Request, namespace, agentName, sessionID string, promptTokens, completionTokens int64) {
	if s.chatQuotaEnforcer == nil {
		return
	}
	if err := s.chatQuotaEnforcer.Settle(r.Context(), sessionID, promptTokens, completionTokens); err != nil {
		s.logger.Error("chat quota settlement failed", zap.String("agent", agentName), zap.String("namespace", namespace), zap.String("sessionID", sessionID), requestid.ZapField(r.Context()))
		_ = s.store.AppendEvent(r.Context(), "Agent", namespace, agentName, store.InstanceEvent{
			Timestamp: time.Now().UTC().Format(time.RFC3339),
			Type:      "Warning",
			Reason:    "QuotaSettlementFailed",
			Message:   fmt.Sprintf("session=%s token accounting requires reconciliation", sessionID),
		})
	}
}

// resolveSecretRefCredentials converts a SecretRef into CPI credentials.
func (s *Server) resolveSecretRefCredentials(secretRef *v1alpha1.SecretRef) *cpi.Credentials {
	switch secretRef.Spec.ResolvedBackendType() {
	case "env":
		value, ok := os.LookupEnv(secretRef.Spec.ResolvedBackendRef())
		if !ok {
			return nil
		}
		key := secretRef.Spec.ResolvedKey()
		return &cpi.Credentials{Type: "api-key", Data: map[string]string{key: value}}

	case "internal":
		if secretRef.ObjectMeta.Annotations == nil {
			return nil
		}
		raw := secretRef.ObjectMeta.Annotations["kognition.dev/internal-secret-data"]
		if raw == "" {
			return nil
		}
		var values map[string]string
		if err := json.Unmarshal([]byte(raw), &values); err != nil {
			return nil
		}
		credType := values["type"]
		delete(values, "type")
		return &cpi.Credentials{Type: credType, Data: values}

	default:
		return nil
	}
}
