// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package conformance_test

import (
	"context"
	"testing"

	"github.com/kognition-ai/kognition/api/v1alpha1"
	"github.com/kognition-ai/kognition/internal/conformance"
	"github.com/kognition-ai/kognition/internal/cpi"
	"github.com/kognition-ai/kognition/internal/memory"
	"github.com/kognition-ai/kognition/internal/memory/inmemory"
	"github.com/kognition-ai/kognition/internal/runtime"
	"github.com/kognition-ai/kognition/internal/runtime/native"
	"github.com/kognition-ai/kognition/internal/runtime/toolexec"
)

type conformanceDriver struct{}

func (conformanceDriver) Name() string { return "conformance" }

func (conformanceDriver) Validate(context.Context, *v1alpha1.ModelSpec, *v1alpha1.ModelClassSpec) error {
	return nil
}

func (conformanceDriver) Probe(context.Context, *v1alpha1.ModelSpec, *cpi.Credentials) (*cpi.HealthResult, error) {
	return &cpi.HealthResult{Available: true}, nil
}

func (conformanceDriver) Complete(context.Context, *cpi.CompletionRequest) (*cpi.CompletionResponse, error) {
	return &cpi.CompletionResponse{Content: "conformance response", FinishReason: "stop"}, nil
}

func (conformanceDriver) Embed(context.Context, *cpi.EmbedRequest) (*cpi.EmbedResponse, error) {
	return &cpi.EmbedResponse{}, nil
}

func TestPublicSuitesWithReferenceImplementations(t *testing.T) {
	t.Run("MemoryBackend", func(t *testing.T) {
		conformance.RunMemoryBackendSuite(t, func(t *testing.T) (memory.Backend, string) {
			t.Helper()
			backend := inmemory.New()
			handle := "conformance-" + t.Name()
			if err := backend.Provision(context.Background(), memory.ProvisionRequest{
				BackendHandle:      handle,
				EmbeddingDimension: 3,
			}); err != nil {
				t.Fatalf("provision memory backend: %v", err)
			}
			return backend, handle
		})
	})

	t.Run("Runtime", func(t *testing.T) {
		conformance.RunRuntimeSuite(t, func(t *testing.T) runtime.Runtime {
			t.Helper()
			return native.New(native.Config{
				Driver:      conformanceDriver{},
				DriverModel: "conformance-model",
				Agent: &v1alpha1.Agent{
					ObjectMeta: v1alpha1.ObjectMeta{Name: "conformance-agent", Namespace: "default"},
					Spec: v1alpha1.AgentSpec{
						Instructions: v1alpha1.AgentInstructions{Inline: "Run the conformance suite."},
					},
				},
			})
		})
	})

	t.Run("ToolExecutor", func(t *testing.T) {
		conformance.RunToolSuite(t, func(t *testing.T) runtime.ToolExecutor {
			t.Helper()
			return toolexec.New(nil)
		})
	})
}
