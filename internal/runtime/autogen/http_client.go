// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package autogen

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/propagation"
)

const maxBridgeBytes = 6 << 20

type HTTPClientConfig struct {
	Endpoint             string
	AgentName            string
	Namespace            string
	RuntimeConfiguration string
	AuthToken            string
	SessionID            string
	InstanceUID          string
	Client               *http.Client
	Capabilities         func(context.Context, string) (map[string]string, error)
}

type HTTPAgentClient struct {
	config   HTTPClientConfig
	client   *http.Client
	mu       sync.Mutex
	sessions map[string]bool
}

func NewHTTPAgentClient(config HTTPClientConfig) (*HTTPAgentClient, error) {
	endpoint, err := url.Parse(config.Endpoint)
	if err != nil || endpoint == nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Path != "" && endpoint.Path != "/") {
		return nil, errors.New("invalid AutoGen worker endpoint")
	}
	ip := net.ParseIP(endpoint.Hostname())
	if endpoint.Scheme != "https" && !(endpoint.Scheme == "http" && ip != nil && ip.IsLoopback()) {
		return nil, errors.New("AutoGen worker requires HTTPS or literal loopback")
	}
	if config.AuthToken == "" || config.AgentName == "" || config.Namespace == "" || config.Capabilities == nil || len(config.RuntimeConfiguration) > 65536 {
		return nil, errors.New("AutoGen worker requires bound configuration and authentication")
	}
	if config.SessionID != "" {
		parsed, err := uuid.Parse(config.SessionID)
		if err != nil || parsed == uuid.Nil || parsed.String() != config.SessionID || config.InstanceUID == "" || len(config.InstanceUID) > 128 {
			return nil, errors.New("invalid AutoGen controller session binding")
		}
	} else if config.InstanceUID != "" {
		return nil, errors.New("AutoGen instance binding requires a session ID")
	}
	var configuration map[string]string
	if json.Unmarshal([]byte(config.RuntimeConfiguration), &configuration) != nil || configuration["KOGNITION_RUNTIME_CLASS"] != "autogen" || configuration["KOGNITION_AGENT_NAME"] != config.AgentName || configuration["KOGNITION_NAMESPACE"] != config.Namespace {
		return nil, errors.New("AutoGen worker configuration does not match its principal")
	}
	config.Endpoint = strings.TrimRight(config.Endpoint, "/")
	client := &http.Client{Timeout: 30 * time.Second}
	if config.Client != nil {
		copy := *config.Client
		client = &copy
		if client.Timeout <= 0 {
			client.Timeout = 30 * time.Second
		}
	}
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	return &HTTPAgentClient{config: config, client: client, sessions: map[string]bool{}}, nil
}

func (client *HTTPAgentClient) request(ctx context.Context, method, path string, input, output interface{}) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil || len(encoded) > maxBridgeBytes {
			return errors.New("AutoGen worker request exceeds bounds")
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, client.config.Endpoint+path, body)
	if err != nil {
		return errors.New("invalid AutoGen worker request")
	}
	request.Header.Set("Authorization", "Bearer "+client.config.AuthToken)
	request.Header.Set("Content-Type", "application/json")
	(propagation.TraceContext{}).Inject(ctx, propagation.HeaderCarrier(request.Header))
	response, err := client.client.Do(request)
	if err != nil {
		return errors.New("AutoGen worker transport unavailable")
	}
	defer func() { _ = response.Body.Close() }()
	encoded, err := io.ReadAll(io.LimitReader(response.Body, maxBridgeBytes+1))
	if err != nil || len(encoded) > maxBridgeBytes {
		return errors.New("AutoGen worker response exceeds bounds")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return errors.New("AutoGen worker rejected request")
	}
	if output != nil && json.Unmarshal(encoded, output) != nil {
		return errors.New("invalid AutoGen worker response")
	}
	return nil
}

func (client *HTTPAgentClient) sessionPath(sessionID, operation string) (string, error) {
	client.mu.Lock()
	known := client.sessions[sessionID]
	client.mu.Unlock()
	if !known {
		return "", errors.New("AutoGen worker session is not owned by this client")
	}
	return "/sessions/" + url.PathEscape(sessionID) + "/" + operation, nil
}

func (client *HTTPAgentClient) remember(sessionID string) error {
	parsed, err := uuid.Parse(sessionID)
	if err != nil || parsed == uuid.Nil || parsed.String() != sessionID {
		return errors.New("invalid AutoGen worker session handle")
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.sessions[sessionID] {
		return errors.New("AutoGen worker reused a session handle")
	}
	client.sessions[sessionID] = true
	return nil
}

func (client *HTTPAgentClient) CreateAgent(ctx context.Context, req CreateAgentRequest) (CreateAgentResponse, error) {
	if req.AgentName != client.config.AgentName || req.Namespace != client.config.Namespace {
		return CreateAgentResponse{}, errors.New("AutoGen session principal mismatch")
	}
	var response struct {
		SessionID string `json:"session_id"`
	}
	input := map[string]string{"agent_name": req.AgentName, "namespace": req.Namespace, "runtime_configuration": client.config.RuntimeConfiguration}
	if client.config.SessionID != "" {
		input["session_id"], input["instance_uid"] = client.config.SessionID, client.config.InstanceUID
	}
	if err := client.request(ctx, http.MethodPost, "/sessions", input, &response); err != nil {
		return CreateAgentResponse{}, err
	}
	if client.config.SessionID != "" && response.SessionID != client.config.SessionID {
		return CreateAgentResponse{}, errors.New("AutoGen worker changed the controller session binding")
	}
	if err := client.remember(response.SessionID); err != nil {
		return CreateAgentResponse{}, err
	}
	return CreateAgentResponse{AgentID: response.SessionID}, nil
}

func (client *HTTPAgentClient) SendMessage(ctx context.Context, sessionID, message string) (<-chan AgentEvent, error) {
	path, err := client.sessionPath(sessionID, "step")
	if err != nil || len(message) > 65536 {
		return nil, errors.New("invalid AutoGen turn")
	}
	turnID := uuid.NewString()
	if client.config.SessionID != "" {
		turnID = client.config.SessionID
	}
	capabilities, err := client.config.Capabilities(ctx, turnID)
	if err != nil || capabilities["model_session_capability"] == "" {
		return nil, errors.New("AutoGen turn capabilities unavailable")
	}
	input := map[string]string{"message": message, "turn_id": turnID}
	for _, name := range []string{"model_session_capability", "memory_capability", "tool_session_capability", "agent_delegation_session_capability", "knowledge_session_capability"} {
		if capabilities[name] != "" {
			input[name] = capabilities[name]
		}
	}
	if err := client.request(ctx, http.MethodPost, path, input, nil); err != nil {
		return nil, err
	}
	events := make(chan AgentEvent, 16)
	go client.readTurn(ctx, sessionID, events)
	return events, nil
}

func (client *HTTPAgentClient) readTurn(ctx context.Context, sessionID string, events chan<- AgentEvent) {
	defer close(events)
	emit := func(event AgentEvent) bool {
		select {
		case events <- event:
			return true
		case <-ctx.Done():
			return false
		}
	}
	_, err := client.sessionPath(sessionID, "status")
	if err != nil {
		emit(AgentEvent{Type: AgentEventError, Content: "AutoGen session unavailable"})
		return
	}
	for {
		if ctx.Err() != nil {
			cancelContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			_ = client.CancelAgent(cancelContext, sessionID)
			cancel()
			return
		}
		status, err := client.Status(ctx, sessionID)
		if err != nil {
			if ctx.Err() != nil {
				continue
			}
			emit(AgentEvent{Type: AgentEventError, Content: "AutoGen worker status unavailable"})
			return
		}
		if status.Phase == "Completed" || status.Phase == "Failed" {
			if status.Phase == "Failed" || status.RuntimeApprovalState != "" {
				emit(AgentEvent{Type: AgentEventError, Content: "AutoGen execution requires controller recovery"})
				return
			}
			for _, tool := range status.ToolCallsDetail {
				arguments, _ := json.Marshal(tool.Args)
				callID := tool.CallID
				if callID == "" {
					emit(AgentEvent{Type: AgentEventError, Content: "AutoGen tool binding unavailable"})
					return
				}
				if !emit(AgentEvent{Type: AgentEventToolCall, ToolName: tool.Name, ToolCallID: callID, Content: string(arguments)}) || !emit(AgentEvent{Type: AgentEventToolResult, ToolName: tool.Name, ToolCallID: callID, Content: tool.Result, IsError: tool.Error != nil}) {
					return
				}
			}
			if len(status.Output) > 65536 {
				emit(AgentEvent{Type: AgentEventError, Content: "AutoGen output exceeds bounds"})
				return
			}
			if emit(AgentEvent{Type: AgentEventOutput, Content: status.Output, IsFinal: true}) {
				emit(AgentEvent{Type: AgentEventDone})
			}
			return
		}
		if status.Phase != "Running" {
			emit(AgentEvent{Type: AgentEventError, Content: "invalid AutoGen execution phase"})
			return
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
		}
	}
}

func (client *HTTPAgentClient) CancelAgent(ctx context.Context, sessionID string) error {
	path, err := client.sessionPath(sessionID, "cancel")
	if err != nil {
		return err
	}
	var response struct {
		Accepted bool `json:"accepted"`
	}
	if err := client.request(ctx, http.MethodPost, path, nil, &response); err != nil || !response.Accepted {
		return errors.New("AutoGen cancellation was not acknowledged")
	}
	return nil
}

func (client *HTTPAgentClient) GetAgentState(ctx context.Context, sessionID string) ([]byte, error) {
	path, err := client.sessionPath(sessionID, "checkpoint")
	if err != nil {
		return nil, err
	}
	var checkpoint json.RawMessage
	if err := client.request(ctx, http.MethodGet, path, nil, &checkpoint); err != nil {
		return nil, err
	}
	return checkpoint, nil
}

func (client *HTTPAgentClient) RestoreAgent(ctx context.Context, state []byte) (string, error) {
	if len(state) > maxBridgeBytes || !json.Valid(state) {
		return "", errors.New("invalid AutoGen checkpoint")
	}
	var response struct {
		SessionID string `json:"session_id"`
	}
	if client.config.SessionID != "" {
		var bound map[string]json.RawMessage
		if json.Unmarshal(state, &bound) != nil {
			return "", errors.New("invalid AutoGen checkpoint")
		}
		bound["restore_bound"] = json.RawMessage("true")
		bound["session_id"], _ = json.Marshal(client.config.SessionID)
		bound["instance_uid"], _ = json.Marshal(client.config.InstanceUID)
		state, _ = json.Marshal(bound)
	}
	if err := client.request(ctx, http.MethodPost, "/sessions/restore", json.RawMessage(state), &response); err != nil {
		return "", err
	}
	if client.config.SessionID != "" {
		if response.SessionID != client.config.SessionID {
			return "", errors.New("AutoGen restored a different controller session")
		}
		return response.SessionID, nil
	}
	if err := client.remember(response.SessionID); err != nil {
		return "", err
	}
	return response.SessionID, nil
}

func (client *HTTPAgentClient) Health(ctx context.Context) error {
	var status struct {
		Status string `json:"status"`
	}
	if err := client.request(ctx, http.MethodGet, "/readyz", nil, &status); err != nil || status.Status != "ok" {
		return errors.New("AutoGen worker is not ready")
	}
	return nil
}

func (client *HTTPAgentClient) ReleaseAgent(ctx context.Context, sessionID string) error {
	path, err := client.sessionPath(sessionID, "release")
	if err != nil {
		return err
	}
	var response struct {
		Accepted  bool   `json:"accepted"`
		SessionID string `json:"session_id"`
	}
	if err := client.request(ctx, http.MethodPost, path, nil, &response); err != nil || !response.Accepted || response.SessionID != sessionID {
		return errors.New("AutoGen session reclamation not acknowledged")
	}
	return nil
}

type HTTPAgentStatus struct {
	Phase                string `json:"phase"`
	SessionID            string `json:"session_id"`
	AgentName            string `json:"agent_name"`
	InstanceUID          string `json:"instance_uid"`
	Output               string `json:"output"`
	Error                string `json:"error"`
	RuntimeApprovalState string `json:"runtime_approval_state"`
	TokenUsage           *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
	} `json:"token_usage"`
	ToolCalls       int32 `json:"tool_calls"`
	ToolCallsDetail []struct {
		Name       string      `json:"name"`
		CallID     string      `json:"call_id"`
		Args       interface{} `json:"args"`
		Result     string      `json:"result"`
		DurationMS float64     `json:"duration_ms"`
		Error      interface{} `json:"error"`
	} `json:"tool_calls_detail"`
}

func (client *HTTPAgentClient) Status(ctx context.Context, sessionID string) (HTTPAgentStatus, error) {
	path, err := client.sessionPath(sessionID, "status")
	if err != nil {
		return HTTPAgentStatus{}, err
	}
	var status HTTPAgentStatus
	if err := client.request(ctx, http.MethodGet, path, nil, &status); err != nil {
		return HTTPAgentStatus{}, err
	}
	if client.config.SessionID != "" && (status.SessionID != client.config.SessionID || status.AgentName != client.config.AgentName || status.InstanceUID != client.config.InstanceUID) {
		return HTTPAgentStatus{}, errors.New("AutoGen status does not match the controller session")
	}
	if status.Phase != "Ready" && status.Phase != "Running" && status.Phase != "Completed" && status.Phase != "Failed" {
		return HTTPAgentStatus{}, errors.New("invalid AutoGen status phase")
	}
	if len(status.Output) > 65536 || len(status.ToolCallsDetail) > 50 {
		return HTTPAgentStatus{}, errors.New("AutoGen status exceeds bounds")
	}
	if status.TokenUsage != nil {
		usage := status.TokenUsage
		if usage.PromptTokens < 0 || usage.CompletionTokens < 0 || usage.TotalTokens < usage.PromptTokens || usage.TotalTokens-usage.PromptTokens != usage.CompletionTokens {
			return HTTPAgentStatus{}, errors.New("invalid AutoGen usage accounting")
		}
	}
	return status, nil
}
