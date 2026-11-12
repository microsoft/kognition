// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestClient_CallSendsJSONRPCAndDecodesResult(t *testing.T) {
	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", req.Method)
		}
		if got := req.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		var message struct {
			JSONRPC string         `json:"jsonrpc"`
			ID      int64          `json:"id"`
			Method  string         `json:"method"`
			Params  map[string]any `json:"params"`
		}
		if err := json.NewDecoder(req.Body).Decode(&message); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		methods = append(methods, message.Method)
		switch message.Method {
		case "initialize":
			if message.Params["protocolVersion"] != protocolVersion {
				t.Errorf("protocolVersion = %v, want %s", message.Params["protocolVersion"], protocolVersion)
			}
			w.Header().Set(sessionHeader, "session-1")
			_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: message.ID, Result: json.RawMessage(fmt.Sprintf(`{"protocolVersion":%q,"capabilities":{},"serverInfo":{"name":"test","version":"1"}}`, protocolVersion))})
		case "notifications/initialized":
			if got := req.Header.Get(sessionHeader); got != "session-1" {
				t.Errorf("notification session = %q, want session-1", got)
			}
			w.WriteHeader(http.StatusAccepted)
		case "ping":
			if got := req.Header.Get(sessionHeader); got != "session-1" {
				t.Errorf("ping session = %q, want session-1", got)
			}
			_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: message.ID, Result: json.RawMessage(`{}`)})
		default:
			t.Errorf("unexpected MCP method %q", message.Method)
		}
	}))
	defer server.Close()

	var result map[string]any
	if err := NewClient(server.Client()).Call(context.Background(), server.URL, "ping", nil, &result); err != nil {
		t.Fatalf("call ping: %v", err)
	}
	if want := []string{"initialize", "notifications/initialized", "ping"}; !reflect.DeepEqual(methods, want) {
		t.Fatalf("methods = %v, want %v", methods, want)
	}
}

func TestClient_CallDecodesSSEResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var message rpcRequest
		_ = json.NewDecoder(req.Body).Decode(&message)
		if message.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		result := `{}`
		if message.Method == "initialize" {
			result = fmt.Sprintf(`{"protocolVersion":%q}`, protocolVersion)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%d,\"method\":\"sampling/createMessage\",\"params\":{}}\n\nevent: message\ndata: {\"jsonrpc\":\"2.0\",\"id\":%d,\"result\":%s}\n\n", message.ID, message.ID, result)
	}))
	defer server.Close()

	if err := NewClient(server.Client()).Call(context.Background(), server.URL, "ping", nil, &map[string]any{}); err != nil {
		t.Fatalf("call ping over SSE: %v", err)
	}
}

func TestDecodeRPCResponse_AcceptsFinalSSEEventWithoutDelimiter(t *testing.T) {
	const requestID int64 = 42
	body := []byte("event: message\r\ndata: {\"jsonrpc\":\"2.0\",\"id\":42,\"result\":{}}")
	var response rpcResponse
	if err := decodeRPCResponse("text/event-stream; charset=utf-8", body, requestID, &response); err != nil {
		t.Fatalf("decodeRPCResponse: %v", err)
	}
	if response.ID != requestID || string(response.Result) != "{}" {
		t.Fatalf("response = %+v, want result for request %d", response, requestID)
	}
}

func TestDecodeRPCResponse_RejectsNonResponseSSEEvents(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
	}{
		{name: "malformed JSON", data: `{"jsonrpc":`},
		{name: "wrong request ID", data: `{"jsonrpc":"2.0","id":41,"result":{}}`},
		{name: "notification without result", data: `{"jsonrpc":"2.0","id":42,"method":"progress"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var response rpcResponse
			err := decodeRPCResponse("text/event-stream", []byte("data: "+tc.data+"\n\n"), 42, &response)
			if err == nil || !strings.Contains(err.Error(), "contained no JSON-RPC response") {
				t.Fatalf("error = %v, want missing JSON-RPC response", err)
			}
			if response.JSONRPC != "" || response.ID != 0 || response.Error != nil || len(response.Result) != 0 {
				t.Fatalf("rejected event populated response: %+v", response)
			}
		})
	}
}

func TestClient_CallClassifiesResponseReadFailureAsTransportError(t *testing.T) {
	readErr := errors.New("connection reset while reading")
	client := &Client{httpClient: httpDoerFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       failingReadCloser{err: readErr},
		}, nil
	})}

	err := client.Call(context.Background(), "http://mcp.example.test", "ping", nil, &map[string]any{})
	var transportErr *TransportError
	if !errors.As(err, &transportErr) || !errors.Is(err, readErr) {
		t.Fatalf("error = %v, want TransportError wrapping read failure", err)
	}
}

type httpDoerFunc func(*http.Request) (*http.Response, error)

func (fn httpDoerFunc) Do(req *http.Request) (*http.Response, error) {
	return fn(req)
}

type failingReadCloser struct {
	err error
}

func (r failingReadCloser) Read([]byte) (int, error) { return 0, r.err }
func (failingReadCloser) Close() error               { return nil }

func TestClient_CallReinitializesInvalidSession(t *testing.T) {
	var initializeCount atomic.Int32
	var rejectSession atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var message rpcRequest
		_ = json.NewDecoder(req.Body).Decode(&message)
		switch message.Method {
		case "initialize":
			count := initializeCount.Add(1)
			w.Header().Set(sessionHeader, fmt.Sprintf("session-%d", count))
			_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: message.ID, Result: json.RawMessage(fmt.Sprintf(`{"protocolVersion":%q}`, protocolVersion))})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "ping":
			if rejectSession.CompareAndSwap(true, false) {
				http.Error(w, "session expired", http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: message.ID, Result: json.RawMessage(`{}`)})
		}
	}))
	defer server.Close()

	client := NewClient(server.Client())
	if err := client.Call(context.Background(), server.URL, "ping", nil, &map[string]any{}); err != nil {
		t.Fatalf("first ping: %v", err)
	}
	rejectSession.Store(true)
	if err := client.Call(context.Background(), server.URL, "ping", nil, &map[string]any{}); err != nil {
		t.Fatalf("ping after session expiry: %v", err)
	}
	if got := initializeCount.Load(); got != 2 {
		t.Fatalf("initialize count = %d, want 2", got)
	}
}

func TestClient_ConcurrentCallsInitializeOnce(t *testing.T) {
	var initializeCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var message rpcRequest
		_ = json.NewDecoder(req.Body).Decode(&message)
		switch message.Method {
		case "initialize":
			initializeCount.Add(1)
			w.Header().Set(sessionHeader, "shared-session")
			_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: message.ID, Result: json.RawMessage(fmt.Sprintf(`{"protocolVersion":%q}`, protocolVersion))})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "ping":
			if got := req.Header.Get(sessionHeader); got != "shared-session" {
				t.Errorf("ping session = %q, want shared-session", got)
			}
			_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: message.ID, Result: json.RawMessage(`{}`)})
		}
	}))
	defer server.Close()

	client := NewClient(server.Client())
	var group sync.WaitGroup
	errors := make(chan error, 8)
	for range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			errors <- client.Call(ctx, server.URL, "ping", nil, &map[string]any{})
		}()
	}
	done := make(chan struct{})
	go func() {
		group.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for concurrent MCP calls")
	}
	close(errors)
	for err := range errors {
		if err != nil {
			t.Errorf("concurrent ping: %v", err)
		}
	}
	if got := initializeCount.Load(); got != 1 {
		t.Fatalf("initialize count = %d, want 1", got)
	}
}

func TestClient_CallRejectsUnsupportedProtocolVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var message rpcRequest
		_ = json.NewDecoder(req.Body).Decode(&message)
		_ = json.NewEncoder(w).Encode(rpcResponse{
			JSONRPC: "2.0",
			ID:      message.ID,
			Result:  json.RawMessage(`{"protocolVersion":"2099-01-01"}`),
		})
	}))
	defer server.Close()

	err := NewClient(server.Client()).Call(context.Background(), server.URL, "ping", nil, &map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "unsupported protocolVersion") {
		t.Fatalf("error = %v, want unsupported protocolVersion", err)
	}
}

func TestClient_CallReturnsTypedErrors(t *testing.T) {
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		check   func(error) bool
	}{
		{
			name: "HTTP error",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
			},
			check: func(err error) bool {
				var target *HTTPError
				return errors.As(err, &target) && target.StatusCode == http.StatusServiceUnavailable
			},
		},
		{
			name: "RPC error",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: 1, Error: &RPCError{Code: -32601, Message: "method not found"}})
			},
			check: func(err error) bool {
				var target *RPCError
				return errors.As(err, &target) && target.Code == -32601
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(tc.handler)
			defer server.Close()
			err := NewClient(server.Client()).Call(context.Background(), server.URL, "ping", nil, &map[string]any{})
			if !tc.check(err) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestClient_CallRejectsInvalidJSONRPCResponses(t *testing.T) {
	for _, tc := range []struct {
		name       string
		writeReply func(http.ResponseWriter, int64)
		wantError  string
	}{
		{
			name: "malformed JSON",
			writeReply: func(w http.ResponseWriter, _ int64) {
				_, _ = w.Write([]byte(`{"jsonrpc":`))
			},
			wantError: "decode MCP JSON-RPC response",
		},
		{
			name: "mismatched envelope",
			writeReply: func(w http.ResponseWriter, id int64) {
				_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "1.0", ID: id, Result: json.RawMessage(`{}`)})
			},
			wantError: "invalid MCP JSON-RPC response envelope",
		},
		{
			name: "missing result",
			writeReply: func(w http.ResponseWriter, id int64) {
				_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: id})
			},
			wantError: "response is missing result",
		},
		{
			name: "invalid result shape",
			writeReply: func(w http.ResponseWriter, id int64) {
				_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: id, Result: json.RawMessage(`"not-an-object"`)})
			},
			wantError: "decode MCP result",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				var message rpcRequest
				if err := json.NewDecoder(req.Body).Decode(&message); err != nil {
					t.Fatalf("decode request: %v", err)
				}
				switch message.Method {
				case "initialize":
					_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: message.ID, Result: json.RawMessage(fmt.Sprintf(`{"protocolVersion":%q}`, protocolVersion))})
				case "notifications/initialized":
					w.WriteHeader(http.StatusAccepted)
				case "ping":
					tc.writeReply(w, message.ID)
				default:
					t.Errorf("unexpected MCP method %q", message.Method)
				}
			}))
			defer server.Close()

			err := NewClient(server.Client()).Call(context.Background(), server.URL, "ping", nil, &map[string]any{})
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error = %v, want containing %q", err, tc.wantError)
			}
		})
	}
}
