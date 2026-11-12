// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package conformance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/runtime"
)

// ToolExecutorFactory creates a fresh ToolExecutor for testing.
type ToolExecutorFactory func(t *testing.T) runtime.ToolExecutor

// RunToolSuite executes the conformance suite for tool executor implementations.
func RunToolSuite(t *testing.T, factory ToolExecutorFactory) {
	t.Helper()

	t.Run("Execute_ReturnsOutput", func(t *testing.T) {
		runToolExecuteReturnsOutput(t, factory)
	})
	t.Run("Execute_WithTimeout", func(t *testing.T) {
		runToolExecuteWithTimeout(t, factory)
	})
	t.Run("Execute_InvalidInput", func(t *testing.T) {
		runToolExecuteInvalidInput(t, factory)
	})
}

func runToolExecuteReturnsOutput(t *testing.T, factory ToolExecutorFactory) {
	t.Helper()
	executor := factory(t)
	ctx := context.Background()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var input map[string]any
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"message": input["message"]})
	}))
	t.Cleanup(server.Close)

	req := runtime.ToolExecutionRequest{
		Tool: v1alpha1.Tool{
			ObjectMeta: v1alpha1.ObjectMeta{
				Name:      "echo-tool",
				Namespace: "default",
			},
			Spec: v1alpha1.ToolSpec{
				Description: "Echoes input back",
				Protocol:    v1alpha1.ToolProtocolOpenAPI,
				Endpoint:    server.URL,
				Metadata:    map[string]string{"allowPrivateNetwork": "true"},
			},
		},
		Input: map[string]any{"message": "hello"},
	}

	result, err := executor.Execute(ctx, req, runtime.ToolSandboxLimits{
		Timeout:  5 * time.Second,
		CPUMilli: 500,
		MemoryMB: 256,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Output == nil {
		t.Error("expected non-nil output")
	}
	if result.Output["message"] != "hello" {
		t.Errorf("output message = %v, want hello", result.Output["message"])
	}
}

func runToolExecuteWithTimeout(t *testing.T, factory ToolExecutorFactory) {
	t.Helper()
	executor := factory(t)
	requestReceived := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requestReceived <- struct{}{}
	}))
	t.Cleanup(server.Close)

	req := runtime.ToolExecutionRequest{
		Tool: v1alpha1.Tool{
			ObjectMeta: v1alpha1.ObjectMeta{
				Name:      "slow-tool",
				Namespace: "default",
			},
			Spec: v1alpha1.ToolSpec{
				Description: "A tool that may time out",
				Protocol:    v1alpha1.ToolProtocolOpenAPI,
				Endpoint:    server.URL,
			},
		},
		Input: map[string]any{},
	}

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := executor.Execute(cancelledCtx, req, runtime.ToolSandboxLimits{
		Timeout:  100 * time.Millisecond,
		CPUMilli: 500,
		MemoryMB: 256,
	})
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	select {
	case <-requestReceived:
		t.Fatal("executor dispatched a request after cancellation")
	case <-time.After(50 * time.Millisecond):
	}
}

func runToolExecuteInvalidInput(t *testing.T, factory ToolExecutorFactory) {
	t.Helper()
	executor := factory(t)
	ctx := context.Background()

	req := runtime.ToolExecutionRequest{
		Tool: v1alpha1.Tool{
			ObjectMeta: v1alpha1.ObjectMeta{
				Name:      "nonexistent-tool",
				Namespace: "default",
			},
			Spec: v1alpha1.ToolSpec{
				Description: "Rejects malformed input",
				Protocol:    v1alpha1.ToolProtocolOpenAPI,
				Endpoint:    "http://example.invalid",
			},
		},
		Input: map[string]any{"unsupported": make(chan struct{})},
	}

	_, err := executor.Execute(ctx, req, runtime.ToolSandboxLimits{
		Timeout:  5 * time.Second,
		CPUMilli: 500,
		MemoryMB: 256,
	})
	if err == nil {
		t.Error("expected error for non-JSON input")
	}
}
