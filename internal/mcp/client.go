// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

const (
	maxResponseBytes = 1 << 20
	protocolVersion  = "2025-03-26"
	sessionHeader    = "Mcp-Session-Id"
)

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// Client sends MCP JSON-RPC requests over streamable HTTP.
type Client struct {
	httpClient httpDoer
	nextID     atomic.Int64
	sessions   sync.Map
}

type endpointSession struct {
	initMu      sync.Mutex
	mu          sync.RWMutex
	initialized bool
	id          string
}

// NewClient creates an MCP client using the provided HTTP client.
func NewClient(httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{httpClient: httpClient}
}

// HTTPError reports a non-successful HTTP response from an MCP server.
type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("MCP HTTP status %d: %s", e.StatusCode, e.Body)
}

// RPCError reports a JSON-RPC error returned by an MCP server.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("MCP RPC error %d: %s", e.Code, e.Message)
}

// TransportError reports a failure sending an MCP request.
type TransportError struct {
	Err error
}

func (e *TransportError) Error() string { return fmt.Sprintf("send MCP request: %v", e.Err) }
func (e *TransportError) Unwrap() error { return e.Err }

// ResponseTooLargeError reports an MCP response above the in-memory cap.
type ResponseTooLargeError struct {
	Limit int64
}

func (e *ResponseTooLargeError) Error() string {
	return fmt.Sprintf("MCP response exceeds %d bytes", e.Limit)
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// Call invokes an MCP method and decodes its JSON-RPC result into result.
func (c *Client) Call(ctx context.Context, endpoint, method string, params, result any) error {
	state := c.sessionFor(endpoint)
	sessionID, err := c.ensureInitialized(ctx, endpoint, state)
	if err != nil {
		return err
	}
	err = c.call(ctx, endpoint, method, params, result, sessionID)
	var httpErr *HTTPError
	if sessionID != "" && errors.As(err, &httpErr) && httpErr.StatusCode == http.StatusNotFound {
		state.mu.Lock()
		if state.id == sessionID {
			state.initialized = false
			state.id = ""
		}
		state.mu.Unlock()
		sessionID, initErr := c.ensureInitialized(ctx, endpoint, state)
		if initErr != nil {
			return initErr
		}
		return c.call(ctx, endpoint, method, params, result, sessionID)
	}
	return err
}

func (c *Client) sessionFor(endpoint string) *endpointSession {
	state, _ := c.sessions.LoadOrStore(endpoint, &endpointSession{})
	session, ok := state.(*endpointSession)
	if !ok {
		panic("MCP session cache contains an unexpected value type")
	}
	return session
}

func (c *Client) ensureInitialized(ctx context.Context, endpoint string, state *endpointSession) (string, error) {
	state.mu.RLock()
	if state.initialized {
		id := state.id
		state.mu.RUnlock()
		return id, nil
	}
	state.mu.RUnlock()

	state.initMu.Lock()
	defer state.initMu.Unlock()
	state.mu.RLock()
	if state.initialized {
		id := state.id
		state.mu.RUnlock()
		return id, nil
	}
	state.mu.RUnlock()

	var result struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	params := map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo": map[string]any{
			"name":    "kognition",
			"version": "v1alpha1",
		},
	}
	sessionID, err := c.callWithSessionResult(ctx, endpoint, "initialize", params, &result, "")
	if err != nil {
		return "", fmt.Errorf("initialize MCP session: %w", err)
	}
	if result.ProtocolVersion != protocolVersion {
		return "", fmt.Errorf("initialize MCP session: unsupported protocolVersion %q, want %q", result.ProtocolVersion, protocolVersion)
	}
	if err := c.notify(ctx, endpoint, "notifications/initialized", sessionID); err != nil {
		return "", fmt.Errorf("notify MCP session initialized: %w", err)
	}
	state.mu.Lock()
	state.id = sessionID
	state.initialized = true
	state.mu.Unlock()
	return sessionID, nil
}

func (c *Client) call(ctx context.Context, endpoint, method string, params, result any, sessionID string) error {
	_, err := c.callWithSessionResult(ctx, endpoint, method, params, result, sessionID)
	return err
}

func (c *Client) callWithSessionResult(ctx context.Context, endpoint, method string, params, result any, sessionID string) (string, error) {
	id := c.nextID.Add(1)
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params})
	if err != nil {
		return "", fmt.Errorf("marshal MCP request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("build MCP request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		req.Header.Set(sessionHeader, sessionID)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", &TransportError{Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return "", &TransportError{Err: fmt.Errorf("read MCP response: %w", err)}
	}
	if len(responseBody) > maxResponseBytes {
		return "", &ResponseTooLargeError{Limit: maxResponseBytes}
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", &HTTPError{StatusCode: resp.StatusCode, Body: string(responseBody)}
	}

	var rpcResp rpcResponse
	if err := decodeRPCResponse(resp.Header.Get("Content-Type"), responseBody, id, &rpcResp); err != nil {
		return "", err
	}
	if rpcResp.JSONRPC != "2.0" || rpcResp.ID != id {
		return "", fmt.Errorf("invalid MCP JSON-RPC response envelope")
	}
	if rpcResp.Error != nil {
		return "", rpcResp.Error
	}
	if result != nil {
		if len(rpcResp.Result) == 0 {
			return "", fmt.Errorf("MCP JSON-RPC response is missing result")
		}
		if err := json.Unmarshal(rpcResp.Result, result); err != nil {
			return "", fmt.Errorf("decode MCP result: %w", err)
		}
	}
	return resp.Header.Get(sessionHeader), nil
}

func (c *Client) notify(ctx context.Context, endpoint, method, sessionID string) error {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	if err != nil {
		return fmt.Errorf("marshal MCP notification: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build MCP notification: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		req.Header.Set(sessionHeader, sessionID)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return &TransportError{Err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		return &HTTPError{StatusCode: resp.StatusCode, Body: string(body)}
	}
	return nil
}

func decodeRPCResponse(contentType string, body []byte, id int64, response *rpcResponse) error {
	mediaType, _, _ := mime.ParseMediaType(contentType)
	if mediaType != "text/event-stream" {
		if err := json.Unmarshal(body, response); err != nil {
			return fmt.Errorf("decode MCP JSON-RPC response: %w", err)
		}
		return nil
	}
	var dataLines []string
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
		if line == "" && len(dataLines) > 0 {
			if matchesRPCResponse(dataLines, id, response) {
				return nil
			}
			dataLines = nil
		}
	}
	if len(dataLines) > 0 && matchesRPCResponse(dataLines, id, response) {
		return nil
	}
	return fmt.Errorf("MCP SSE response contained no JSON-RPC response for request %d", id)
}

func matchesRPCResponse(dataLines []string, id int64, response *rpcResponse) bool {
	var candidate rpcResponse
	if json.Unmarshal([]byte(strings.Join(dataLines, "\n")), &candidate) != nil ||
		candidate.JSONRPC != "2.0" || candidate.ID != id ||
		(len(candidate.Result) == 0 && candidate.Error == nil) {
		return false
	}
	*response = candidate
	return true
}
