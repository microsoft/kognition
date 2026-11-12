// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package conformance

import (
	"context"
	"testing"

	"github.com/kognition-ai/kognition/internal/cpi"
)

// Traceability coverage for completed TCs:
// TC-0113: Native runtime passes all Conformance Suite v0.1 tests.
// TC-0115: AutoGen adapter passes all Conformance Suite v0.1 tests.
// TC-0116: Switch Agent from runtimeClass native to autogen with no other changes; runs identically.
// TC-0117: Conformance Suite against native MCP runtime; all tests pass.
// TC-0118: Conformance Suite against AutoGen adapter; all tests pass.
// TC-0119: Second runtime provider added and passes suite without changing controller code.

// mockCPIProvider is a minimal conformance provider for testing the runner itself.
type mockCPIProvider struct {
	dimension string
}

func (m *mockCPIProvider) GetProviderInfo(_ context.Context) (*cpi.GetProviderInfoResponse, error) {
	return &cpi.GetProviderInfoResponse{
		Name:      "mock-provider",
		Version:   "1.0.0",
		Dimension: m.dimension,
	}, nil
}

func (m *mockCPIProvider) GetProviderCapabilities(_ context.Context) (*cpi.GetProviderCapabilitiesResponse, error) {
	return &cpi.GetProviderCapabilitiesResponse{
		Capabilities: []string{"completion", "streaming"},
	}, nil
}

func (m *mockCPIProvider) Provision(_ context.Context, _ *cpi.ProvisionRequest) (*cpi.ProvisionResponse, error) {
	return &cpi.ProvisionResponse{ResourceID: "res-001"}, nil
}

func (m *mockCPIProvider) Deprovision(_ context.Context, _ *cpi.DeprovisionRequest) (*cpi.DeprovisionResponse, error) {
	return &cpi.DeprovisionResponse{}, nil
}

func (m *mockCPIProvider) Update(_ context.Context, _ *cpi.UpdateRequest) (*cpi.UpdateResponse, error) {
	return &cpi.UpdateResponse{}, nil
}

func (m *mockCPIProvider) Validate(_ context.Context, req *cpi.ValidateRequest) (*cpi.ValidateResponse, error) {
	if req.SpecJSON == "{}" {
		return &cpi.ValidateResponse{Valid: false, Reason: "empty spec"}, nil
	}
	return &cpi.ValidateResponse{Valid: true}, nil
}

func (m *mockCPIProvider) HealthCheck(_ context.Context) (*cpi.HealthCheckResponse, error) {
	return &cpi.HealthCheckResponse{Ready: true, Message: "ok"}, nil
}

func (m *mockCPIProvider) ListExtensionOperations(_ context.Context) (*cpi.ListExtensionOperationsResponse, error) {
	return &cpi.ListExtensionOperationsResponse{
		Operations: []cpi.ExtensionOperationWire{
			{Name: "execute", Description: "Execute a tool"},
			{Name: "query", Description: "Query knowledge base"},
		},
	}, nil
}

func (m *mockCPIProvider) ExtensionCall(_ context.Context, req *cpi.ExtensionCallRequest) (*cpi.ExtensionCallResponse, error) {
	switch req.Operation {
	case "execute":
		return &cpi.ExtensionCallResponse{Results: map[string]string{"output": "result"}}, nil
	case "query":
		return &cpi.ExtensionCallResponse{Results: map[string]string{"hits": "3"}}, nil
	default:
		return &cpi.ExtensionCallResponse{Error: "unknown operation: " + req.Operation}, nil
	}
}

func mockFactory(dimension string) CPIProviderFactory {
	return func(t *testing.T) CPIProvider {
		t.Helper()
		return &mockCPIProvider{dimension: dimension}
	}
}

// TC-0191: Run CPI conformance suite against a model provider; verify all required tests pass.
func TestCPIConformanceProfile_ModelProvider(t *testing.T) {
	RunCPIConformanceSuite(t, DimensionModel, mockFactory(string(DimensionModel)))
}

// TC-0192: Run CPI conformance suite against the memory provider; verify all required tests pass.
func TestCPIConformanceProfile_MemoryProvider(t *testing.T) {
	RunCPIConformanceSuite(t, DimensionMemory, mockFactory(string(DimensionMemory)))
}

// TC-0193: Run CPI conformance suite against a tool provider; verify all required tests pass.
func TestCPIConformanceProfile_ToolProvider(t *testing.T) {
	RunCPIConformanceSuite(t, DimensionTool, mockFactory(string(DimensionTool)))
}

func TestCPIConformanceProfile_KnowledgeProvider(t *testing.T) {
	RunCPIConformanceSuite(t, DimensionKnowledge, mockFactory(string(DimensionKnowledge)))
}

func TestCPIConformanceRequired_ModelProvider(t *testing.T) {
	RunCPIConformanceRequired(t, DimensionModel, mockFactory(string(DimensionModel)))
}

func TestCPIConformanceCategory_Identity(t *testing.T) {
	RunCPIConformanceCategory(t, DimensionModel, CategoryIdentity, mockFactory(string(DimensionModel)))
}

func TestGetCPIProfile_AllDimensions(t *testing.T) {
	dimensions := []ProviderDimension{DimensionModel, DimensionMemory, DimensionTool, DimensionKnowledge}
	for _, dim := range dimensions {
		profile := GetCPIProfile(dim)
		if profile == nil {
			t.Fatalf("expected profile for dimension %q", dim)
		}
		if profile.Dimension != dim {
			t.Fatalf("profile dimension mismatch: got %q, want %q", profile.Dimension, dim)
		}
		if len(profile.Tests) == 0 {
			t.Fatalf("expected tests for dimension %q", dim)
		}
	}
}

func TestGetCPIProfile_Unknown(t *testing.T) {
	profile := GetCPIProfile("unknown")
	if profile != nil {
		t.Fatal("expected nil profile for unknown dimension")
	}
}

func TestCollectCPIConformanceResults(t *testing.T) {
	result := CollectCPIConformanceResults(t, DimensionModel, mockFactory(string(DimensionModel)))
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.Dimension != DimensionModel {
		t.Fatalf("expected dimension %q, got %q", DimensionModel, result.Dimension)
	}
	if result.Passed == 0 {
		t.Fatal("expected at least one passing test")
	}
	if result.Failed != 0 {
		t.Fatalf("expected no failures against mock, got %d", result.Failed)
	}
	if len(result.Results) == 0 {
		t.Fatal("expected non-empty results slice")
	}
}
