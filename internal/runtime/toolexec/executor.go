// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package toolexec provides concrete runtime.ToolExecutor implementations
// for invoking tools over network protocols (HTTPS/OpenAPI and MCP).
//
// These executors are network clients: they honor the dispatcher's context
// (which carries the sandbox timeout) but do not enforce CPU or memory
// limits — those apply only to in-process or container-isolated executors,
// which are out of scope here.
package toolexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/mcp"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/runtime"
)

// maxResponseBytes caps the size of tool response bodies the controller will
// buffer in memory, defending against misbehaving or hostile endpoints.
const maxResponseBytes = 1 << 20 // 1 MiB

var (
	lookupEndpointIPAddrs = net.DefaultResolver.LookupIPAddr
	dialEndpointContext   = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
)

// New returns a runtime.ToolExecutor that dispatches by tool protocol.
// HTTPS OpenAPI, remote Browser, and MCP-over-HTTP are supported. Shell and
// stdio tools return a structured unsupported error until a sandbox runner is
// available.
//
// httpClient is reused for all outbound calls. Pass nil to use a default
// client with a 30s transport timeout.
func New(httpClient *http.Client) runtime.ToolExecutor {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &protocolMultiplexer{
		http: &httpExecutor{client: httpClient},
		mcp:  &mcpExecutor{client: httpClient},
	}
}

type protocolMultiplexer struct {
	http *httpExecutor
	mcp  *mcpExecutor
}

func (m *protocolMultiplexer) Execute(ctx context.Context, req runtime.ToolExecutionRequest, limits runtime.ToolSandboxLimits) (runtime.ToolExecutionResult, error) {
	switch req.Tool.Spec.EffectiveProtocol() {
	case v1alpha1.ToolProtocolOpenAPI, v1alpha1.ToolProtocolBrowser:
		return m.http.Execute(ctx, req, limits)
	case v1alpha1.ToolProtocolMCP:
		return m.mcp.Execute(ctx, req, limits)
	default:
		return runtime.ToolExecutionResult{}, &runtime.StructuredToolError{
			Code:      "ToolProtocolUnsupported",
			Message:   fmt.Sprintf("protocol %q has no controller-side executor (supported: OpenAPI, Browser, MCP)", req.Tool.Spec.EffectiveProtocol()),
			Retryable: false,
		}
	}
}

// httpExecutor invokes OpenAPI tools by POSTing the input JSON to the tool's
// configured endpoint and decoding the JSON response body.
type httpExecutor struct {
	client *http.Client
}

func (e *httpExecutor) Execute(ctx context.Context, req runtime.ToolExecutionRequest, _ runtime.ToolSandboxLimits) (runtime.ToolExecutionResult, error) {
	endpoint := strings.TrimSpace(req.Tool.Spec.Endpoint)
	if endpoint == "" {
		return runtime.ToolExecutionResult{}, &runtime.StructuredToolError{
			Code:    "ToolEndpointMissing",
			Message: "OpenAPI tool requires spec.endpoint",
		}
	}
	if req.Tool.Spec.EffectiveProtocol() == v1alpha1.ToolProtocolBrowser {
		if err := validateBrowserNavigationTarget(ctx, req.Input); err != nil {
			return runtime.ToolExecutionResult{}, err
		}
	}
	body, err := json.Marshal(req.Input)
	if err != nil {
		return runtime.ToolExecutionResult{}, fmt.Errorf("marshal input: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return runtime.ToolExecutionResult{}, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if req.InvocationID != "" {
		httpReq.Header.Set("X-Kognition-Tool-Invocation-Id", req.InvocationID)
	}

	client, err := SecureToolHTTPClient(ctx, e.client, &req.Tool)
	if err != nil {
		return runtime.ToolExecutionResult{}, endpointPolicyError(err)
	}
	client = withTraceContextTransport(client)
	resp, err := client.Do(httpReq)
	if err != nil {
		return runtime.ToolExecutionResult{}, &runtime.StructuredToolError{
			Code:      "ToolTransportError",
			Message:   err.Error(),
			Retryable: true,
		}
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := readCappedBody(resp.Body)
	if err != nil {
		return runtime.ToolExecutionResult{}, err
	}
	if resp.StatusCode >= 400 {
		return runtime.ToolExecutionResult{}, &runtime.StructuredToolError{
			Code:      "ToolHTTPError",
			Message:   fmt.Sprintf("status %d: %s", resp.StatusCode, truncate(string(respBody), 256)),
			Retryable: resp.StatusCode >= 500,
		}
	}
	out := decodeJSONResponse(respBody)
	return runtime.ToolExecutionResult{Output: out, CompletionUnconfirmed: resp.StatusCode == http.StatusAccepted}, nil
}

func validateBrowserNavigationTarget(ctx context.Context, input map[string]any) error {
	target, ok := input["url"].(string)
	if !ok || strings.TrimSpace(target) == "" {
		return &runtime.StructuredToolError{Code: "ToolBrowserTargetInvalid", Message: "Browser tool input.url must be a non-empty URL"}
	}
	if err := ValidateToolEndpoint(ctx, target, false); err != nil {
		return &runtime.StructuredToolError{Code: "ToolBrowserTargetDenied", Message: err.Error()}
	}
	return nil
}

// mcpExecutor invokes MCP tools using the JSON-RPC 2.0 "tools/call" method
// over HTTP (MCP streamable HTTP transport). The tool's Endpoint must be
// the MCP server URL.
type mcpExecutor struct {
	client *http.Client
}

func (e *mcpExecutor) Execute(ctx context.Context, req runtime.ToolExecutionRequest, _ runtime.ToolSandboxLimits) (runtime.ToolExecutionResult, error) {
	endpoint := strings.TrimSpace(req.Tool.Spec.Endpoint)
	if endpoint == "" {
		return runtime.ToolExecutionResult{}, &runtime.StructuredToolError{
			Code:    "ToolEndpointMissing",
			Message: "MCP tool requires spec.endpoint",
		}
	}
	client, err := SecureToolHTTPClient(ctx, e.client, &req.Tool)
	if err != nil {
		return runtime.ToolExecutionResult{}, endpointPolicyError(err)
	}
	var result map[string]any
	err = mcp.NewClient(withTraceContextTransport(client)).Call(ctx, endpoint, "tools/call", map[string]any{
		"name":      req.Tool.ObjectMeta.Name,
		"arguments": req.Input,
	}, &result)
	if err != nil {
		return runtime.ToolExecutionResult{}, mcpExecutionError(err)
	}
	// MCP signals tool-level failures with isError=true on a successful
	// JSON-RPC response. Surface these as structured errors rather than
	// letting the workflow treat a failed call as success.
	if isErr, _ := result["isError"].(bool); isErr {
		return runtime.ToolExecutionResult{}, &runtime.StructuredToolError{
			Code:    "ToolCallError",
			Message: truncate(mcpContentText(result), 256),
		}
	}
	return runtime.ToolExecutionResult{Output: result}, nil
}

type traceContextTransport struct {
	base http.RoundTripper
}

func (t traceContextTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	injectTraceContext(req.Context(), req.Header)
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

func withTraceContextTransport(client *http.Client) *http.Client {
	if client == nil {
		client = http.DefaultClient
	}
	clone := *client
	clone.Transport = traceContextTransport{base: client.Transport}
	return &clone
}

func injectTraceContext(ctx context.Context, header http.Header) {
	spanContext, ok := observability.SpanContextFromContext(ctx)
	if !ok {
		return
	}
	traceparent := observability.Traceparent(observability.HexTraceID(spanContext.TraceID), observability.HexSpanID(spanContext.SpanID))
	if traceparent == "" {
		return
	}
	header.Set("traceparent", traceparent)
}

func endpointPolicyError(err error) error {
	return &runtime.StructuredToolError{Code: "ToolEndpointDenied", Message: err.Error()}
}

// SecureToolHTTPClient validates a Tool endpoint and returns a client that
// revalidates redirects and pins public-only DNS resolution at dial time.
func SecureToolHTTPClient(ctx context.Context, base *http.Client, tool *v1alpha1.Tool) (*http.Client, error) {
	allowPrivate := strings.EqualFold(tool.Spec.Metadata["allowPrivateNetwork"], "true")
	if err := ValidateToolEndpoint(ctx, tool.Spec.Endpoint, allowPrivate); err != nil {
		return nil, err
	}
	client := *base
	previousRedirect := base.CheckRedirect
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if err := ValidateToolEndpoint(request.Context(), request.URL.String(), allowPrivate); err != nil {
			return err
		}
		if previousRedirect != nil {
			return previousRedirect(request, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	if allowPrivate {
		return &client, nil
	}
	var transport *http.Transport
	switch configured := base.Transport.(type) {
	case nil:
		defaultTransport, ok := http.DefaultTransport.(*http.Transport)
		if !ok || defaultTransport == nil {
			return &client, nil
		}
		transport = defaultTransport.Clone()
	case *http.Transport:
		transport = configured.Clone()
	default:
		// Custom transports are used by tests and specialized callers. Endpoint
		// preflight and redirect validation still apply, but dialing is delegated.
		return &client, nil
	}
	transport.Proxy = nil
	transport.DialContext = DialPublicAddress
	client.Transport = transport
	return &client, nil
}

// ValidateToolEndpoint enforces public HTTPS unless private networking is explicit.
func ValidateToolEndpoint(ctx context.Context, endpoint string, allowPrivate bool) error {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || parsed.Hostname() == "" {
		return fmt.Errorf("invalid tool endpoint")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("tool endpoint scheme %q is not allowed", parsed.Scheme)
	}
	if !allowPrivate && parsed.Scheme != "https" {
		return fmt.Errorf("public tool endpoints must use HTTPS")
	}
	if parsed.User != nil {
		return fmt.Errorf("tool endpoint must not contain URL credentials")
	}
	if allowPrivate {
		return nil
	}
	addresses, err := resolveEndpointIPs(ctx, parsed.Hostname())
	if err != nil {
		return fmt.Errorf("resolve tool endpoint: %w", err)
	}
	for _, address := range addresses {
		if !isPublicIP(address) {
			return fmt.Errorf("tool endpoint resolves to a non-public address")
		}
	}
	return nil
}

// DialPublicAddress resolves and dials only public IP addresses.
func DialPublicAddress(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addresses, err := resolveEndpointIPs(ctx, host)
	if err != nil {
		return nil, err
	}
	for _, resolved := range addresses {
		if !isPublicIP(resolved) {
			continue
		}
		connection, dialErr := dialEndpointContext(ctx, network, net.JoinHostPort(resolved.String(), port))
		if dialErr == nil {
			return connection, nil
		}
		err = dialErr
	}
	if err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("tool endpoint has no public address")
}

// DialToolEndpoint applies endpoint policy and dials a TCP tool probe.
func DialToolEndpoint(ctx context.Context, endpoint string, allowPrivate bool, dialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}) (net.Conn, error) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || parsed.Hostname() == "" || parsed.Port() == "" {
		return nil, fmt.Errorf("TCP tool endpoint must include a host and port")
	}
	if parsed.Scheme != "tcp" && parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("TCP tool endpoint scheme %q is not allowed", parsed.Scheme)
	}
	address := net.JoinHostPort(parsed.Hostname(), parsed.Port())
	if allowPrivate {
		return dialer.DialContext(ctx, "tcp", address)
	}
	addresses, err := resolveEndpointIPs(ctx, parsed.Hostname())
	if err != nil {
		return nil, fmt.Errorf("resolve tool endpoint: %w", err)
	}
	for _, address := range addresses {
		if !isPublicIP(address) {
			return nil, fmt.Errorf("tool endpoint resolves to a non-public address")
		}
	}
	return DialPublicAddress(ctx, "tcp", address)
}

func resolveEndpointIPs(ctx context.Context, host string) ([]net.IP, error) {
	if parsed := net.ParseIP(host); parsed != nil {
		return []net.IP{parsed}, nil
	}
	addresses, err := lookupEndpointIPAddrs(ctx, host)
	if err != nil {
		return nil, err
	}
	result := make([]net.IP, 0, len(addresses))
	for _, address := range addresses {
		result = append(result, address.IP)
	}
	return result, nil
}

func isPublicIP(address net.IP) bool {
	return address.IsGlobalUnicast() &&
		!address.IsPrivate() &&
		!address.IsLoopback() &&
		!address.IsLinkLocalUnicast() &&
		!address.IsLinkLocalMulticast() &&
		!address.IsUnspecified()
}

func mcpExecutionError(err error) error {
	var transportErr *mcp.TransportError
	if errors.As(err, &transportErr) {
		return &runtime.StructuredToolError{Code: "ToolTransportError", Message: err.Error(), Retryable: true}
	}
	var httpErr *mcp.HTTPError
	if errors.As(err, &httpErr) {
		return &runtime.StructuredToolError{
			Code:      "ToolHTTPError",
			Message:   fmt.Sprintf("status %d: %s", httpErr.StatusCode, truncate(httpErr.Body, 256)),
			Retryable: httpErr.StatusCode >= 500,
		}
	}
	var rpcErr *mcp.RPCError
	if errors.As(err, &rpcErr) {
		return &runtime.StructuredToolError{Code: "ToolRPCError", Message: fmt.Sprintf("rpc error %d: %s", rpcErr.Code, rpcErr.Message)}
	}
	var tooLargeErr *mcp.ResponseTooLargeError
	if errors.As(err, &tooLargeErr) {
		return &runtime.StructuredToolError{Code: "ToolResponseTooLarge", Message: err.Error()}
	}
	return &runtime.StructuredToolError{Code: "ToolProtocolError", Message: err.Error()}
}

// mcpContentText extracts the first text fragment from an MCP tools/call
// result's content array, falling back to a generic message.
func mcpContentText(result map[string]any) string {
	content, _ := result["content"].([]any)
	for _, item := range content {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if text, ok := obj["text"].(string); ok && text != "" {
			return text
		}
	}
	return "tool reported isError=true"
}

// readCappedBody reads at most maxResponseBytes+1 from r and reports a
// structured error if the body would exceed the cap.
func readCappedBody(r io.Reader) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, maxResponseBytes+1))
	if err != nil {
		return nil, &runtime.StructuredToolError{
			Code:      "ToolTransportError",
			Message:   fmt.Sprintf("read response: %v", err),
			Retryable: true,
		}
	}
	if len(body) > maxResponseBytes {
		return nil, &runtime.StructuredToolError{
			Code:    "ToolResponseTooLarge",
			Message: fmt.Sprintf("response exceeds %d bytes", maxResponseBytes),
		}
	}
	return body, nil
}

// decodeJSONResponse returns the response body parsed as a JSON object.
// Non-object payloads are wrapped under a "result" key so callers always
// receive a map of native Go values (never a json.RawMessage).
func decodeJSONResponse(body []byte) map[string]any {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return map[string]any{}
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err == nil {
		return obj
	}
	var v any
	if err := json.Unmarshal(body, &v); err == nil {
		return map[string]any{"result": v}
	}
	return map[string]any{"result": string(body)}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
