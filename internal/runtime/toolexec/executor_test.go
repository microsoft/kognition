// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package toolexec

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/conformance"
	"github.com/kognition-ai/kognition/internal/observability"
	"github.com/kognition-ai/kognition/internal/runtime"
)

const testTraceparent = "00-0123456789abcdef0123456789abcdef-fedcba9876543210-01"

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type failingReadCloser struct{}

func (failingReadCloser) Read([]byte) (int, error) { return 0, errors.New("read failed") }
func (failingReadCloser) Close() error             { return nil }

type testMCPRequest struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      int64          `json:"id"`
	Method  string         `json:"method"`
	Params  map[string]any `json:"params"`
}

type testMCPResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *testMCPError   `json:"error,omitempty"`
}

type testMCPError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func privateToolSpec(protocol v1alpha1.ToolProtocol, endpoint string) v1alpha1.ToolSpec {
	return v1alpha1.ToolSpec{
		Protocol: protocol,
		Endpoint: endpoint,
		Metadata: map[string]string{"allowPrivateNetwork": "true"},
	}
}

func contextWithTestSpan(t *testing.T) context.Context {
	t.Helper()
	var traceID observability.TraceID
	var spanID observability.SpanID
	traceBytes, err := hex.DecodeString("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("decode trace id: %v", err)
	}
	spanBytes, err := hex.DecodeString("fedcba9876543210")
	if err != nil {
		t.Fatalf("decode span id: %v", err)
	}
	copy(traceID[:], traceBytes)
	copy(spanID[:], spanBytes)
	return observability.ContextWithSpanContext(context.Background(), observability.SpanContext{TraceID: traceID, SpanID: spanID, TraceFlags: 0x01})
}

func TestExecutorConformance(t *testing.T) {
	conformance.RunToolSuite(t, func(t *testing.T) runtime.ToolExecutor {
		t.Helper()
		return New(nil)
	})
}

func handleTestMCPHandshake(w http.ResponseWriter, req testMCPRequest) bool {
	switch req.Method {
	case "initialize":
		_ = json.NewEncoder(w).Encode(testMCPResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  json.RawMessage(`{"protocolVersion":"2025-03-26","capabilities":{},"serverInfo":{"name":"test","version":"1"}}`),
		})
		return true
	case "notifications/initialized":
		w.WriteHeader(http.StatusAccepted)
		return true
	default:
		return false
	}
}

func TestHTTPExecutor_OpenAPI_PostsInputAndReturnsJSON(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("expected Content-Type application/json, got %q", ct)
		}
		if r.Header.Get("X-Kognition-Tool-Invocation-Id") != "reviewed-invocation" {
			t.Error("authorized invocation identity was not forwarded to the backend")
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"answer": 42, "ok": true}`))
	}))
	defer srv.Close()

	exec := New(srv.Client())
	req := runtime.ToolExecutionRequest{
		Tool: v1alpha1.Tool{
			ObjectMeta: v1alpha1.ObjectMeta{Name: "calc"},
			Spec:       privateToolSpec(v1alpha1.ToolProtocolOpenAPI, srv.URL),
		},
		InvocationID: "reviewed-invocation",
		Input:        map[string]any{"x": float64(1), "y": float64(2)},
	}
	res, err := exec.Execute(context.Background(), req, runtime.ToolSandboxLimits{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotBody["x"] != float64(1) || gotBody["y"] != float64(2) {
		t.Errorf("server did not receive input correctly: %v", gotBody)
	}
	if res.Output["answer"] != float64(42) || res.Output["ok"] != true {
		t.Errorf("unexpected output: %v", res.Output)
	}
}

func TestHTTPExecutor_OpenAPI_InjectsTraceparent(t *testing.T) {
	var gotTraceparent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTraceparent = r.Header.Get("traceparent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok": true}`))
	}))
	defer srv.Close()

	exec := New(srv.Client())
	_, err := exec.Execute(contextWithTestSpan(t), runtime.ToolExecutionRequest{
		Tool: v1alpha1.Tool{
			ObjectMeta: v1alpha1.ObjectMeta{Name: "calc"},
			Spec:       privateToolSpec(v1alpha1.ToolProtocolOpenAPI, srv.URL),
		},
		Input: map[string]any{"x": float64(1)},
	}, runtime.ToolSandboxLimits{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotTraceparent != testTraceparent {
		t.Fatalf("traceparent = %q, want %q", gotTraceparent, testTraceparent)
	}
}

func TestHTTPExecutor_OpenAPI_HTTPErrorIsStructured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()

	exec := New(srv.Client())
	req := runtime.ToolExecutionRequest{
		Tool: v1alpha1.Tool{
			ObjectMeta: v1alpha1.ObjectMeta{Name: "calc"},
			Spec:       privateToolSpec(v1alpha1.ToolProtocolOpenAPI, srv.URL),
		},
	}
	_, err := exec.Execute(context.Background(), req, runtime.ToolSandboxLimits{})
	if err == nil {
		t.Fatal("expected error")
	}
	var se *runtime.StructuredToolError
	if !errors.As(err, &se) {
		t.Fatalf("expected StructuredToolError, got %T", err)
	}
	if se.Code != "ToolHTTPError" || !se.Retryable {
		t.Errorf("unexpected structured error: %+v", se)
	}
}

func TestHTTPExecutor_MissingEndpoint(t *testing.T) {
	exec := New(nil)
	req := runtime.ToolExecutionRequest{
		Tool: v1alpha1.Tool{
			ObjectMeta: v1alpha1.ObjectMeta{Name: "calc"},
			Spec:       v1alpha1.ToolSpec{Protocol: v1alpha1.ToolProtocolOpenAPI},
		},
	}
	_, err := exec.Execute(context.Background(), req, runtime.ToolSandboxLimits{})
	var se *runtime.StructuredToolError
	if !errors.As(err, &se) || se.Code != "ToolEndpointMissing" {
		t.Fatalf("expected ToolEndpointMissing, got %v", err)
	}
}

func TestMCPExecutor_CallsToolsCallAndReturnsResult(t *testing.T) {
	var gotReq testMCPRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotReq)
		if handleTestMCPHandshake(w, gotReq) {
			return
		}
		resp := testMCPResponse{
			JSONRPC: "2.0",
			ID:      gotReq.ID,
			Result:  json.RawMessage(`{"content":[{"type":"text","text":"hi"}],"isError":false}`),
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	exec := New(srv.Client())
	req := runtime.ToolExecutionRequest{
		Tool: v1alpha1.Tool{
			ObjectMeta: v1alpha1.ObjectMeta{Name: "echo"},
			Spec:       privateToolSpec(v1alpha1.ToolProtocolMCP, srv.URL),
		},
		Input: map[string]any{"msg": "hi"},
	}
	res, err := exec.Execute(context.Background(), req, runtime.ToolSandboxLimits{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotReq.Method != "tools/call" {
		t.Errorf("expected method tools/call, got %q", gotReq.Method)
	}
	if name, _ := gotReq.Params["name"].(string); name != "echo" {
		t.Errorf("expected name=echo, got %v", gotReq.Params["name"])
	}
	args, _ := gotReq.Params["arguments"].(map[string]any)
	if args["msg"] != "hi" {
		t.Errorf("expected arguments.msg=hi, got %v", args)
	}
	if res.Output["isError"] != false {
		t.Errorf("unexpected output: %v", res.Output)
	}
}

func TestMCPExecutor_InjectsTraceparent(t *testing.T) {
	var gotTraceparent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req testMCPRequest
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		if handleTestMCPHandshake(w, req) {
			return
		}
		gotTraceparent = r.Header.Get("traceparent")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(testMCPResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  json.RawMessage(`{"content":[{"type":"text","text":"hi"}],"isError":false}`),
		})
	}))
	defer srv.Close()

	exec := New(srv.Client())
	_, err := exec.Execute(contextWithTestSpan(t), runtime.ToolExecutionRequest{
		Tool: v1alpha1.Tool{
			ObjectMeta: v1alpha1.ObjectMeta{Name: "echo"},
			Spec:       privateToolSpec(v1alpha1.ToolProtocolMCP, srv.URL),
		},
		Input: map[string]any{"msg": "hi"},
	}, runtime.ToolSandboxLimits{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotTraceparent != testTraceparent {
		t.Fatalf("traceparent = %q, want %q", gotTraceparent, testTraceparent)
	}
}

func TestMCPExecutor_RPCErrorIsStructured(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := testMCPResponse{
			JSONRPC: "2.0",
			ID:      1,
			Error:   &testMCPError{Code: -32601, Message: "method not found"},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	exec := New(srv.Client())
	req := runtime.ToolExecutionRequest{
		Tool: v1alpha1.Tool{
			ObjectMeta: v1alpha1.ObjectMeta{Name: "echo"},
			Spec:       privateToolSpec(v1alpha1.ToolProtocolMCP, srv.URL),
		},
	}
	_, err := exec.Execute(context.Background(), req, runtime.ToolSandboxLimits{})
	var se *runtime.StructuredToolError
	if !errors.As(err, &se) || se.Code != "ToolRPCError" {
		t.Fatalf("expected ToolRPCError, got %v", err)
	}
}

func TestExecutor_UnsupportedProtocol(t *testing.T) {
	exec := New(nil)
	req := runtime.ToolExecutionRequest{
		Tool: v1alpha1.Tool{
			ObjectMeta: v1alpha1.ObjectMeta{Name: "sh"},
			Spec:       v1alpha1.ToolSpec{Protocol: v1alpha1.ToolProtocolShell, Endpoint: "x"},
		},
	}
	_, err := exec.Execute(context.Background(), req, runtime.ToolSandboxLimits{})
	var se *runtime.StructuredToolError
	if !errors.As(err, &se) || se.Code != "ToolProtocolUnsupported" {
		t.Fatalf("expected ToolProtocolUnsupported, got %v", err)
	}
}

func TestBrowserExecutor_UsesHardenedHTTPTransport(t *testing.T) {
	var received map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", req.Method)
		}
		if err := json.NewDecoder(req.Body).Decode(&received); err != nil {
			t.Fatalf("decode browser input: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"title":"Kognition"}`))
	}))
	defer srv.Close()

	exec := New(srv.Client())
	result, err := exec.Execute(context.Background(), runtime.ToolExecutionRequest{
		Tool: v1alpha1.Tool{
			ObjectMeta: v1alpha1.ObjectMeta{Name: "remote-browser"},
			Spec:       privateToolSpec(v1alpha1.ToolProtocolBrowser, srv.URL),
		},
		Input: map[string]any{"url": "https://8.8.8.8", "action": "navigate"},
	}, runtime.ToolSandboxLimits{})
	if err != nil {
		t.Fatalf("Execute browser Tool: %v", err)
	}
	if received["action"] != "navigate" || result.Output["title"] != "Kognition" {
		t.Fatalf("browser request=%#v result=%#v", received, result)
	}
}

func TestBrowserExecutor_DeniesUnsafeNavigationTarget(t *testing.T) {
	for _, test := range []struct {
		name  string
		input map[string]any
		code  string
	}{
		{name: "missing URL", input: map[string]any{"action": "navigate"}, code: "ToolBrowserTargetInvalid"},
		{name: "plaintext URL", input: map[string]any{"url": "http://8.8.8.8", "action": "navigate"}, code: "ToolBrowserTargetDenied"},
		{name: "metadata URL", input: map[string]any{"url": "https://169.254.169.254/latest/meta-data", "action": "navigate"}, code: "ToolBrowserTargetDenied"},
	} {
		t.Run(test.name, func(t *testing.T) {
			called := false
			srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
			defer srv.Close()

			exec := New(srv.Client())
			_, err := exec.Execute(context.Background(), runtime.ToolExecutionRequest{
				Tool: v1alpha1.Tool{
					ObjectMeta: v1alpha1.ObjectMeta{Name: "remote-browser"},
					Spec:       privateToolSpec(v1alpha1.ToolProtocolBrowser, srv.URL),
				},
				Input: test.input,
			}, runtime.ToolSandboxLimits{})
			var structured *runtime.StructuredToolError
			if !errors.As(err, &structured) || structured.Code != test.code {
				t.Fatalf("Execute browser Tool error = %v, want %s", err, test.code)
			}
			if called {
				t.Fatal("Browser service received a denied navigation target")
			}
		})
	}
}

func TestHTTPExecutor_OpenAPI_4xxIsNotRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad"))
	}))
	defer srv.Close()

	exec := New(srv.Client())
	req := runtime.ToolExecutionRequest{
		Tool: v1alpha1.Tool{
			ObjectMeta: v1alpha1.ObjectMeta{Name: "calc"},
			Spec:       privateToolSpec(v1alpha1.ToolProtocolOpenAPI, srv.URL),
		},
	}
	_, err := exec.Execute(context.Background(), req, runtime.ToolSandboxLimits{})
	var se *runtime.StructuredToolError
	if !errors.As(err, &se) {
		t.Fatalf("expected StructuredToolError, got %T", err)
	}
	if se.Code != "ToolHTTPError" || se.Retryable {
		t.Errorf("expected non-retryable ToolHTTPError, got %+v", se)
	}
}

func TestHTTPExecutor_TransportErrorIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // server is down; client.Do will fail.

	exec := New(nil)
	req := runtime.ToolExecutionRequest{
		Tool: v1alpha1.Tool{
			ObjectMeta: v1alpha1.ObjectMeta{Name: "calc"},
			Spec:       privateToolSpec(v1alpha1.ToolProtocolOpenAPI, url),
		},
	}
	_, err := exec.Execute(context.Background(), req, runtime.ToolSandboxLimits{})
	var se *runtime.StructuredToolError
	if !errors.As(err, &se) || se.Code != "ToolTransportError" || !se.Retryable {
		t.Fatalf("expected retryable ToolTransportError, got %v", err)
	}
}

func TestMCPExecutor_IsErrorIsReportedAsToolCallError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got testMCPRequest
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		if handleTestMCPHandshake(w, got) {
			return
		}
		resp := testMCPResponse{
			JSONRPC: "2.0",
			ID:      got.ID,
			Result:  json.RawMessage(`{"isError":true,"content":[{"type":"text","text":"upstream 404"}]}`),
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	exec := New(srv.Client())
	req := runtime.ToolExecutionRequest{
		Tool: v1alpha1.Tool{
			ObjectMeta: v1alpha1.ObjectMeta{Name: "echo"},
			Spec:       privateToolSpec(v1alpha1.ToolProtocolMCP, srv.URL),
		},
	}
	_, err := exec.Execute(context.Background(), req, runtime.ToolSandboxLimits{})
	var se *runtime.StructuredToolError
	if !errors.As(err, &se) || se.Code != "ToolCallError" {
		t.Fatalf("expected ToolCallError, got %v", err)
	}
	if !strings.Contains(se.Message, "upstream 404") {
		t.Errorf("expected message to include content text, got %q", se.Message)
	}
	if se.Retryable {
		t.Errorf("ToolCallError should not be retryable")
	}
}

func TestHTTPExecutor_ResponseTooLargeIsRejected(t *testing.T) {
	big := strings.Repeat("a", (1<<20)+10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"x":"` + big + `"}`))
	}))
	defer srv.Close()

	exec := New(srv.Client())
	req := runtime.ToolExecutionRequest{
		Tool: v1alpha1.Tool{
			ObjectMeta: v1alpha1.ObjectMeta{Name: "calc"},
			Spec:       privateToolSpec(v1alpha1.ToolProtocolOpenAPI, srv.URL),
		},
	}
	_, err := exec.Execute(context.Background(), req, runtime.ToolSandboxLimits{})
	var se *runtime.StructuredToolError
	if !errors.As(err, &se) || se.Code != "ToolResponseTooLarge" {
		t.Fatalf("expected ToolResponseTooLarge, got %v", err)
	}
}

func TestHTTPExecutor_NormalizesResponseBodies(t *testing.T) {
	tests := []struct {
		name string
		body string
		want map[string]any
	}{
		{name: "empty", body: "  ", want: map[string]any{}},
		{name: "array", body: `[1,"two"]`, want: map[string]any{"result": []any{float64(1), "two"}}},
		{name: "scalar", body: `true`, want: map[string]any{"result": true}},
		{name: "malformed JSON", body: `{broken`, want: map[string]any{"result": "{broken"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			res, err := New(srv.Client()).Execute(context.Background(), runtime.ToolExecutionRequest{
				Tool: v1alpha1.Tool{Spec: privateToolSpec(v1alpha1.ToolProtocolOpenAPI, srv.URL)},
			}, runtime.ToolSandboxLimits{})
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			if got, want := mustJSON(t, res.Output), mustJSON(t, tt.want); got != want {
				t.Fatalf("Execute() output = %s, want %s", got, want)
			}
		})
	}
}

func TestHTTPExecutor_ResponseReadFailureIsRetryable(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       failingReadCloser{},
		}, nil
	})}

	_, err := New(client).Execute(context.Background(), runtime.ToolExecutionRequest{
		Tool: v1alpha1.Tool{Spec: privateToolSpec(v1alpha1.ToolProtocolOpenAPI, "https://tool.test")},
	}, runtime.ToolSandboxLimits{})
	var se *runtime.StructuredToolError
	if !errors.As(err, &se) || se.Code != "ToolTransportError" || !se.Retryable {
		t.Fatalf("expected retryable ToolTransportError, got %v", err)
	}
}

func TestExecutorRejectsUnsafeEndpointsByDefault(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
	}{
		{name: "loopback", endpoint: "http://127.0.0.1:8080/tool"},
		{name: "metadata", endpoint: "http://169.254.169.254/latest/meta-data"},
		{name: "private address", endpoint: "https://10.0.0.10/tool"},
		{name: "URL credentials", endpoint: "https://user:pass@example.com/tool"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := New(nil).Execute(context.Background(), runtime.ToolExecutionRequest{
				Tool: v1alpha1.Tool{Spec: v1alpha1.ToolSpec{Protocol: v1alpha1.ToolProtocolOpenAPI, Endpoint: test.endpoint}},
			}, runtime.ToolSandboxLimits{})
			var structured *runtime.StructuredToolError
			if !errors.As(err, &structured) || structured.Code != "ToolEndpointDenied" {
				t.Fatalf("error = %v, want ToolEndpointDenied", err)
			}
		})
	}
}

func TestExecutorPrivateNetworkRequiresExplicitOptIn(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	result, err := New(server.Client()).Execute(context.Background(), runtime.ToolExecutionRequest{
		Tool: v1alpha1.Tool{Spec: privateToolSpec(v1alpha1.ToolProtocolOpenAPI, server.URL)},
	}, runtime.ToolSandboxLimits{})
	if err != nil || result.Output["ok"] != true {
		t.Fatalf("private opt-in result = %+v, error = %v", result, err)
	}
}

type recordingDialer struct {
	address string
	err     error
}

func (d *recordingDialer) DialContext(_ context.Context, _, address string) (net.Conn, error) {
	d.address = address
	if d.err != nil {
		return nil, d.err
	}
	left, right := net.Pipe()
	_ = right.Close()
	return left, nil
}

func TestEndpointPolicyHelpers(t *testing.T) {
	for _, test := range []struct {
		name         string
		endpoint     string
		allowPrivate bool
		wantErr      bool
	}{
		{name: "invalid", endpoint: "://", wantErr: true},
		{name: "unsupported scheme", endpoint: "ftp://8.8.8.8/file", wantErr: true},
		{name: "public HTTP", endpoint: "http://8.8.8.8/tool", wantErr: true},
		{name: "credentials", endpoint: "https://user:pass@8.8.8.8/tool", wantErr: true},
		{name: "private denied", endpoint: "https://10.0.0.1/tool", wantErr: true},
		{name: "private opted in", endpoint: "http://10.0.0.1/tool", allowPrivate: true},
		{name: "public HTTPS", endpoint: "https://8.8.8.8/tool"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateToolEndpoint(context.Background(), test.endpoint, test.allowPrivate)
			if (err != nil) != test.wantErr {
				t.Fatalf("ValidateToolEndpoint() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestSecureToolHTTPClientConfiguresTransportAndRedirectPolicy(t *testing.T) {
	tool := &v1alpha1.Tool{Spec: v1alpha1.ToolSpec{Endpoint: "https://8.8.8.8/tool"}}
	client, err := SecureToolHTTPClient(context.Background(), &http.Client{Timeout: time.Second}, tool)
	if err != nil {
		t.Fatalf("SecureToolHTTPClient() error = %v", err)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil || transport.DialContext == nil {
		t.Fatalf("secured transport = %#v", client.Transport)
	}
	redirect, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1/private", nil)
	if err := client.CheckRedirect(redirect, nil); err == nil {
		t.Fatal("redirect to private HTTP endpoint was allowed")
	}

	custom := roundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("unused") })
	customClient, err := SecureToolHTTPClient(context.Background(), &http.Client{Transport: custom}, tool)
	_, customPreserved := customClient.Transport.(roundTripFunc)
	if err != nil || !customPreserved {
		t.Fatalf("custom transport client = %#v, error = %v", customClient, err)
	}
}

func TestDialPolicyUsesApprovedResolvedAddress(t *testing.T) {
	originalLookup := lookupEndpointIPAddrs
	originalDial := dialEndpointContext
	t.Cleanup(func() {
		lookupEndpointIPAddrs = originalLookup
		dialEndpointContext = originalDial
	})
	lookupEndpointIPAddrs = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("10.0.0.1")}, {IP: net.ParseIP("8.8.8.8")}}, nil
	}
	var dialed string
	dialEndpointContext = func(_ context.Context, _, address string) (net.Conn, error) {
		dialed = address
		left, right := net.Pipe()
		_ = right.Close()
		return left, nil
	}
	connection, err := DialPublicAddress(context.Background(), "tcp", "tool.example:443")
	if err != nil {
		t.Fatalf("DialPublicAddress() error = %v", err)
	}
	_ = connection.Close()
	if dialed != "8.8.8.8:443" {
		t.Fatalf("dialed address = %q, want approved public IP", dialed)
	}
	if _, err := DialPublicAddress(context.Background(), "tcp", "missing-port"); err == nil {
		t.Fatal("DialPublicAddress() accepted malformed address")
	}
}

func TestDialToolEndpointPrivateOptInAndDenial(t *testing.T) {
	dialer := &recordingDialer{}
	connection, err := DialToolEndpoint(context.Background(), "tcp://127.0.0.1:8080", true, dialer)
	if err != nil {
		t.Fatalf("private DialToolEndpoint() error = %v", err)
	}
	_ = connection.Close()
	if dialer.address != "127.0.0.1:8080" {
		t.Fatalf("private dial address = %q", dialer.address)
	}
	for _, endpoint := range []string{"tcp://127.0.0.1:8080", "udp://8.8.8.8:53", "tcp://missing-port"} {
		if _, err := DialToolEndpoint(context.Background(), endpoint, false, dialer); err == nil {
			t.Fatalf("DialToolEndpoint(%q) error = nil, want denial", endpoint)
		}
	}
}

func TestMCPExecutor_MissingEndpoint(t *testing.T) {
	_, err := New(nil).Execute(context.Background(), runtime.ToolExecutionRequest{
		Tool: v1alpha1.Tool{Spec: v1alpha1.ToolSpec{Protocol: v1alpha1.ToolProtocolMCP}},
	}, runtime.ToolSandboxLimits{})
	var se *runtime.StructuredToolError
	if !errors.As(err, &se) || se.Code != "ToolEndpointMissing" {
		t.Fatalf("expected ToolEndpointMissing, got %v", err)
	}
}

func TestMCPExecutor_ClassifiesProtocolFailures(t *testing.T) {
	tests := []struct {
		name      string
		client    *http.Client
		wantCode  string
		retryable bool
	}{
		{
			name: "transport",
			client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return nil, errors.New("connection refused")
			})},
			wantCode:  "ToolTransportError",
			retryable: true,
		},
		{
			name: "HTTP server error",
			client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusServiceUnavailable,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader("maintenance")),
				}, nil
			})},
			wantCode:  "ToolHTTPError",
			retryable: true,
		},
		{
			name: "oversized response",
			client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(strings.Repeat("x", (1<<20)+1))),
				}, nil
			})},
			wantCode: "ToolResponseTooLarge",
		},
		{
			name: "invalid JSON-RPC envelope",
			client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"jsonrpc":"1.0","id":1,"result":{}}`)),
				}, nil
			})},
			wantCode: "ToolProtocolError",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(tt.client).Execute(context.Background(), runtime.ToolExecutionRequest{
				Tool: v1alpha1.Tool{Spec: privateToolSpec(v1alpha1.ToolProtocolMCP, "https://mcp.test")},
			}, runtime.ToolSandboxLimits{})
			var se *runtime.StructuredToolError
			if !errors.As(err, &se) || se.Code != tt.wantCode || se.Retryable != tt.retryable {
				t.Fatalf("expected %s retryable=%v, got %v", tt.wantCode, tt.retryable, err)
			}
		})
	}
}

func TestMCPExecutor_ToolErrorWithoutTextUsesFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req testMCPRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if handleTestMCPHandshake(w, req) {
			return
		}
		_ = json.NewEncoder(w).Encode(testMCPResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result:  json.RawMessage(`{"isError":true,"content":[42,{"type":"text","text":""}]}`),
		})
	}))
	defer srv.Close()

	_, err := New(srv.Client()).Execute(context.Background(), runtime.ToolExecutionRequest{
		Tool: v1alpha1.Tool{
			ObjectMeta: v1alpha1.ObjectMeta{Name: "echo"},
			Spec:       privateToolSpec(v1alpha1.ToolProtocolMCP, srv.URL),
		},
	}, runtime.ToolSandboxLimits{})
	var se *runtime.StructuredToolError
	if !errors.As(err, &se) || se.Code != "ToolCallError" || se.Message != "tool reported isError=true" {
		t.Fatalf("expected fallback ToolCallError, got %v", err)
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	return string(b)
}
