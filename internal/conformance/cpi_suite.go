// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package conformance

import (
	"context"
	"testing"

	"github.com/kognition-ai/kognition/internal/cpi"
)

// ModelProviderProfile returns the conformance profile for model-dimension providers.
// Model providers must support Identity, Validate, and HealthCheck at minimum.
func ModelProviderProfile() *CPIConformanceProfile {
	return &CPIConformanceProfile{
		Dimension: DimensionModel,
		Tests: []CPIConformanceTest{
			// --- Identity (required) ---
			{
				Name:     "GetProviderInfo_ReturnsName",
				Category: CategoryIdentity,
				Tier:     Required,
				Run:      testIdentityReturnsName,
			},
			{
				Name:     "GetProviderInfo_ReportsModelDimension",
				Category: CategoryIdentity,
				Tier:     Required,
				Run:      testIdentityReportsDimension(string(DimensionModel)),
			},
			{
				Name:     "GetProviderInfo_ReturnsVersion",
				Category: CategoryIdentity,
				Tier:     Required,
				Run:      testIdentityReturnsVersion,
			},
			{
				Name:     "GetProviderCapabilities_ReturnsNonEmpty",
				Category: CategoryIdentity,
				Tier:     Required,
				Run:      testCapabilitiesNonEmpty,
			},
			// --- Lifecycle (required) ---
			{
				Name:     "Validate_ValidSpec_ReturnsValid",
				Category: CategoryLifecycle,
				Tier:     Required,
				Run:      testValidateValidSpec,
			},
			{
				Name:     "Validate_InvalidSpec_ReturnsInvalid",
				Category: CategoryLifecycle,
				Tier:     Required,
				Run:      testValidateInvalidSpec,
			},
			// --- Health (required) ---
			{
				Name:     "HealthCheck_ReturnsReadyStatus",
				Category: CategoryHealth,
				Tier:     Required,
				Run:      testHealthCheckReady,
			},
			// --- Lifecycle (optional) ---
			{
				Name:     "Provision_ReturnsResourceID",
				Category: CategoryLifecycle,
				Tier:     Optional,
				Run:      testProvisionReturnsResourceID,
			},
			{
				Name:     "Deprovision_Succeeds",
				Category: CategoryLifecycle,
				Tier:     Optional,
				Run:      testDeprovisionSucceeds,
			},
			// --- Extension (optional) ---
			{
				Name:     "ListExtensionOperations",
				Category: CategoryExtension,
				Tier:     Optional,
				Run:      testListExtensionOperations,
			},
			{
				Name:     "ExtensionCall_UnknownOp_ReturnsError",
				Category: CategoryExtension,
				Tier:     Optional,
				Run:      testExtensionCallUnknownOp,
			},
		},
	}
}

// MemoryProviderProfile returns the conformance profile for memory-dimension providers.
// Memory providers must support Identity, Provision, Deprovision, Validate, and HealthCheck.
func MemoryProviderProfile() *CPIConformanceProfile {
	return &CPIConformanceProfile{
		Dimension: DimensionMemory,
		Tests: []CPIConformanceTest{
			// --- Identity (required) ---
			{
				Name:     "GetProviderInfo_ReturnsName",
				Category: CategoryIdentity,
				Tier:     Required,
				Run:      testIdentityReturnsName,
			},
			{
				Name:     "GetProviderInfo_ReportsMemoryDimension",
				Category: CategoryIdentity,
				Tier:     Required,
				Run:      testIdentityReportsDimension(string(DimensionMemory)),
			},
			{
				Name:     "GetProviderInfo_ReturnsVersion",
				Category: CategoryIdentity,
				Tier:     Required,
				Run:      testIdentityReturnsVersion,
			},
			{
				Name:     "GetProviderCapabilities_ReturnsNonEmpty",
				Category: CategoryIdentity,
				Tier:     Required,
				Run:      testCapabilitiesNonEmpty,
			},
			// --- Lifecycle (required) ---
			{
				Name:     "Validate_ValidSpec_ReturnsValid",
				Category: CategoryLifecycle,
				Tier:     Required,
				Run:      testValidateValidSpec,
			},
			{
				Name:     "Validate_InvalidSpec_ReturnsInvalid",
				Category: CategoryLifecycle,
				Tier:     Required,
				Run:      testValidateInvalidSpec,
			},
			{
				Name:     "Provision_ReturnsResourceID",
				Category: CategoryLifecycle,
				Tier:     Required,
				Run:      testProvisionReturnsResourceID,
			},
			{
				Name:     "Deprovision_Succeeds",
				Category: CategoryLifecycle,
				Tier:     Required,
				Run:      testDeprovisionSucceeds,
			},
			// --- Health (required) ---
			{
				Name:     "HealthCheck_ReturnsReadyStatus",
				Category: CategoryHealth,
				Tier:     Required,
				Run:      testHealthCheckReady,
			},
			// --- Lifecycle (optional) ---
			{
				Name:     "Update_Succeeds",
				Category: CategoryLifecycle,
				Tier:     Optional,
				Run:      testUpdateSucceeds,
			},
			// --- Extension (optional) ---
			{
				Name:     "ListExtensionOperations",
				Category: CategoryExtension,
				Tier:     Optional,
				Run:      testListExtensionOperations,
			},
			{
				Name:     "ExtensionCall_UnknownOp_ReturnsError",
				Category: CategoryExtension,
				Tier:     Optional,
				Run:      testExtensionCallUnknownOp,
			},
		},
	}
}

// ToolProviderProfile returns the conformance profile for tool-dimension providers.
// Tool providers must support Identity, Validate, HealthCheck, and the "execute" extension.
func ToolProviderProfile() *CPIConformanceProfile {
	return &CPIConformanceProfile{
		Dimension: DimensionTool,
		Tests: []CPIConformanceTest{
			// --- Identity (required) ---
			{
				Name:     "GetProviderInfo_ReturnsName",
				Category: CategoryIdentity,
				Tier:     Required,
				Run:      testIdentityReturnsName,
			},
			{
				Name:     "GetProviderInfo_ReportsToolDimension",
				Category: CategoryIdentity,
				Tier:     Required,
				Run:      testIdentityReportsDimension(string(DimensionTool)),
			},
			{
				Name:     "GetProviderInfo_ReturnsVersion",
				Category: CategoryIdentity,
				Tier:     Required,
				Run:      testIdentityReturnsVersion,
			},
			{
				Name:     "GetProviderCapabilities_ReturnsNonEmpty",
				Category: CategoryIdentity,
				Tier:     Required,
				Run:      testCapabilitiesNonEmpty,
			},
			// --- Lifecycle (required) ---
			{
				Name:     "Validate_ValidSpec_ReturnsValid",
				Category: CategoryLifecycle,
				Tier:     Required,
				Run:      testValidateValidSpec,
			},
			{
				Name:     "Validate_InvalidSpec_ReturnsInvalid",
				Category: CategoryLifecycle,
				Tier:     Required,
				Run:      testValidateInvalidSpec,
			},
			// --- Health (required) ---
			{
				Name:     "HealthCheck_ReturnsReadyStatus",
				Category: CategoryHealth,
				Tier:     Required,
				Run:      testHealthCheckReady,
			},
			// --- Extension (required for tool providers) ---
			{
				Name:     "ListExtensionOperations_IncludesExecute",
				Category: CategoryExtension,
				Tier:     Required,
				Run:      testExtensionIncludesExecute,
			},
			{
				Name:     "ExtensionCall_Execute_ReturnsResult",
				Category: CategoryExtension,
				Tier:     Required,
				Run:      testExtensionCallExecute,
			},
			// --- Extension (optional) ---
			{
				Name:     "ExtensionCall_UnknownOp_ReturnsError",
				Category: CategoryExtension,
				Tier:     Optional,
				Run:      testExtensionCallUnknownOp,
			},
			// --- Lifecycle (optional) ---
			{
				Name:     "Provision_ReturnsResourceID",
				Category: CategoryLifecycle,
				Tier:     Optional,
				Run:      testProvisionReturnsResourceID,
			},
			{
				Name:     "Deprovision_Succeeds",
				Category: CategoryLifecycle,
				Tier:     Optional,
				Run:      testDeprovisionSucceeds,
			},
		},
	}
}

// KnowledgeProviderProfile returns the conformance profile for knowledge-dimension providers.
// Knowledge providers must support Identity, Provision, Deprovision, HealthCheck, and the "query" extension.
func KnowledgeProviderProfile() *CPIConformanceProfile {
	return &CPIConformanceProfile{
		Dimension: DimensionKnowledge,
		Tests: []CPIConformanceTest{
			// --- Identity (required) ---
			{
				Name:     "GetProviderInfo_ReturnsName",
				Category: CategoryIdentity,
				Tier:     Required,
				Run:      testIdentityReturnsName,
			},
			{
				Name:     "GetProviderInfo_ReportsKnowledgeDimension",
				Category: CategoryIdentity,
				Tier:     Required,
				Run:      testIdentityReportsDimension(string(DimensionKnowledge)),
			},
			{
				Name:     "GetProviderInfo_ReturnsVersion",
				Category: CategoryIdentity,
				Tier:     Required,
				Run:      testIdentityReturnsVersion,
			},
			{
				Name:     "GetProviderCapabilities_ReturnsNonEmpty",
				Category: CategoryIdentity,
				Tier:     Required,
				Run:      testCapabilitiesNonEmpty,
			},
			// --- Lifecycle (required) ---
			{
				Name:     "Validate_ValidSpec_ReturnsValid",
				Category: CategoryLifecycle,
				Tier:     Required,
				Run:      testValidateValidSpec,
			},
			{
				Name:     "Validate_InvalidSpec_ReturnsInvalid",
				Category: CategoryLifecycle,
				Tier:     Required,
				Run:      testValidateInvalidSpec,
			},
			{
				Name:     "Provision_ReturnsResourceID",
				Category: CategoryLifecycle,
				Tier:     Required,
				Run:      testProvisionReturnsResourceID,
			},
			{
				Name:     "Deprovision_Succeeds",
				Category: CategoryLifecycle,
				Tier:     Required,
				Run:      testDeprovisionSucceeds,
			},
			// --- Health (required) ---
			{
				Name:     "HealthCheck_ReturnsReadyStatus",
				Category: CategoryHealth,
				Tier:     Required,
				Run:      testHealthCheckReady,
			},
			// --- Extension (required for knowledge providers) ---
			{
				Name:     "ListExtensionOperations_IncludesQuery",
				Category: CategoryExtension,
				Tier:     Required,
				Run:      testExtensionIncludesQuery,
			},
			{
				Name:     "ExtensionCall_Query_ReturnsResult",
				Category: CategoryExtension,
				Tier:     Required,
				Run:      testExtensionCallQuery,
			},
			// --- Extension (optional) ---
			{
				Name:     "ExtensionCall_UnknownOp_ReturnsError",
				Category: CategoryExtension,
				Tier:     Optional,
				Run:      testExtensionCallUnknownOp,
			},
			// --- Lifecycle (optional) ---
			{
				Name:     "Update_Succeeds",
				Category: CategoryLifecycle,
				Tier:     Optional,
				Run:      testUpdateSucceeds,
			},
		},
	}
}

// --- Shared test implementations ---

func testIdentityReturnsName(t *testing.T, provider CPIProvider) {
	t.Helper()
	ctx := context.Background()
	info, err := provider.GetProviderInfo(ctx)
	if err != nil {
		t.Fatalf("GetProviderInfo: %v", err)
	}
	if info.Name == "" {
		t.Fatal("expected non-empty provider name")
	}
}

func testIdentityReportsDimension(expected string) func(t *testing.T, provider CPIProvider) {
	return func(t *testing.T, provider CPIProvider) {
		t.Helper()
		ctx := context.Background()
		info, err := provider.GetProviderInfo(ctx)
		if err != nil {
			t.Fatalf("GetProviderInfo: %v", err)
		}
		if info.Dimension != expected {
			t.Fatalf("expected dimension %q, got %q", expected, info.Dimension)
		}
	}
}

func testIdentityReturnsVersion(t *testing.T, provider CPIProvider) {
	t.Helper()
	ctx := context.Background()
	info, err := provider.GetProviderInfo(ctx)
	if err != nil {
		t.Fatalf("GetProviderInfo: %v", err)
	}
	if info.Version == "" {
		t.Fatal("expected non-empty provider version")
	}
}

func testCapabilitiesNonEmpty(t *testing.T, provider CPIProvider) {
	t.Helper()
	ctx := context.Background()
	caps, err := provider.GetProviderCapabilities(ctx)
	if err != nil {
		t.Fatalf("GetProviderCapabilities: %v", err)
	}
	if len(caps.Capabilities) == 0 {
		t.Fatal("expected at least one capability")
	}
}

func testValidateValidSpec(t *testing.T, provider CPIProvider) {
	t.Helper()
	ctx := context.Background()
	resp, err := provider.Validate(ctx, &cpi.ValidateRequest{
		ResourceKind: "Model",
		SpecJSON:     `{"model":"test-model","endpoint":"https://example.com/v1"}`,
		Parameters:   map[string]string{"temperature": "0.7"},
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !resp.Valid {
		t.Fatalf("expected valid spec, got reason: %s", resp.Reason)
	}
}

func testValidateInvalidSpec(t *testing.T, provider CPIProvider) {
	t.Helper()
	ctx := context.Background()
	resp, err := provider.Validate(ctx, &cpi.ValidateRequest{
		ResourceKind: "Model",
		SpecJSON:     `{}`,
		Parameters:   nil,
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if resp.Valid {
		t.Fatal("expected invalid spec to be rejected")
	}
	if resp.Reason == "" {
		t.Fatal("expected non-empty rejection reason")
	}
}

func testHealthCheckReady(t *testing.T, provider CPIProvider) {
	t.Helper()
	ctx := context.Background()
	resp, err := provider.HealthCheck(ctx)
	if err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	if !resp.Ready {
		t.Fatalf("expected ready=true, got message: %s", resp.Message)
	}
}

func testProvisionReturnsResourceID(t *testing.T, provider CPIProvider) {
	t.Helper()
	ctx := context.Background()
	resp, err := provider.Provision(ctx, &cpi.ProvisionRequest{
		ResourceKind: "Memory",
		ResourceName: "test-resource",
		Namespace:    "default",
		Parameters:   map[string]string{"capacity": "1Gi"},
	})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if resp.ResourceID == "" {
		t.Fatal("expected non-empty resource ID from Provision")
	}
}

func testDeprovisionSucceeds(t *testing.T, provider CPIProvider) {
	t.Helper()
	ctx := context.Background()

	// First provision to get a resource ID.
	provResp, err := provider.Provision(ctx, &cpi.ProvisionRequest{
		ResourceKind: "Memory",
		ResourceName: "deprovisioning-test",
		Namespace:    "default",
		Parameters:   map[string]string{},
	})
	if err != nil {
		t.Fatalf("Provision (setup): %v", err)
	}

	_, err = provider.Deprovision(ctx, &cpi.DeprovisionRequest{
		ResourceID: provResp.ResourceID,
	})
	if err != nil {
		t.Fatalf("Deprovision: %v", err)
	}
}

func testUpdateSucceeds(t *testing.T, provider CPIProvider) {
	t.Helper()
	ctx := context.Background()

	// Provision first.
	provResp, err := provider.Provision(ctx, &cpi.ProvisionRequest{
		ResourceKind: "Memory",
		ResourceName: "update-test",
		Namespace:    "default",
		Parameters:   map[string]string{"capacity": "1Gi"},
	})
	if err != nil {
		t.Fatalf("Provision (setup): %v", err)
	}

	_, err = provider.Update(ctx, &cpi.UpdateRequest{
		ResourceID: provResp.ResourceID,
		Parameters: map[string]string{"capacity": "2Gi"},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
}

func testListExtensionOperations(t *testing.T, provider CPIProvider) {
	t.Helper()
	ctx := context.Background()
	resp, err := provider.ListExtensionOperations(ctx)
	if err != nil {
		t.Fatalf("ListExtensionOperations: %v", err)
	}
	// Optional test: just verify it doesn't error. Operations list may be empty.
	_ = resp
}

func testExtensionCallUnknownOp(t *testing.T, provider CPIProvider) {
	t.Helper()
	ctx := context.Background()
	resp, err := provider.ExtensionCall(ctx, &cpi.ExtensionCallRequest{
		Operation:  "nonexistent-operation-xyz",
		Parameters: map[string]string{},
	})
	if err != nil {
		// Provider may return an error or an error in the response — both valid.
		return
	}
	if resp.Error == "" {
		t.Fatal("expected error response for unknown extension operation")
	}
}

func testExtensionIncludesExecute(t *testing.T, provider CPIProvider) {
	t.Helper()
	ctx := context.Background()
	resp, err := provider.ListExtensionOperations(ctx)
	if err != nil {
		t.Fatalf("ListExtensionOperations: %v", err)
	}
	for _, op := range resp.Operations {
		if op.Name == "execute" {
			return
		}
	}
	t.Fatal("tool provider must declare 'execute' extension operation")
}

func testExtensionCallExecute(t *testing.T, provider CPIProvider) {
	t.Helper()
	ctx := context.Background()
	resp, err := provider.ExtensionCall(ctx, &cpi.ExtensionCallRequest{
		Operation: "execute",
		Parameters: map[string]string{
			"tool":  "echo",
			"input": `{"message":"hello"}`,
		},
	})
	if err != nil {
		t.Fatalf("ExtensionCall(execute): %v", err)
	}
	if resp.Error != "" {
		t.Fatalf("ExtensionCall(execute) returned error: %s", resp.Error)
	}
	if len(resp.Results) == 0 {
		t.Fatal("expected non-empty results from execute")
	}
}

func testExtensionIncludesQuery(t *testing.T, provider CPIProvider) {
	t.Helper()
	ctx := context.Background()
	resp, err := provider.ListExtensionOperations(ctx)
	if err != nil {
		t.Fatalf("ListExtensionOperations: %v", err)
	}
	for _, op := range resp.Operations {
		if op.Name == "query" {
			return
		}
	}
	t.Fatal("knowledge provider must declare 'query' extension operation")
}

func testExtensionCallQuery(t *testing.T, provider CPIProvider) {
	t.Helper()
	ctx := context.Background()
	resp, err := provider.ExtensionCall(ctx, &cpi.ExtensionCallRequest{
		Operation: "query",
		Parameters: map[string]string{
			"query": "test query",
			"top_k": "5",
		},
	})
	if err != nil {
		t.Fatalf("ExtensionCall(query): %v", err)
	}
	if resp.Error != "" {
		t.Fatalf("ExtensionCall(query) returned error: %s", resp.Error)
	}
}
