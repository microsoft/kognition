// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package conformance

import (
	"context"
	"testing"

	"github.com/kognition-ai/kognition/internal/cpi"
)

// Tier indicates whether a test is required or optional for conformance.
type Tier string

const (
	// Required tests must pass for a provider to be conformant.
	Required Tier = "required"

	// Optional tests validate extended capabilities but are not mandatory.
	Optional Tier = "optional"
)

// TestCategory groups related conformance tests by CPI service area.
type TestCategory string

const (
	// CategoryIdentity validates the Identity RPC (GetProviderInfo, GetProviderCapabilities).
	CategoryIdentity TestCategory = "identity"

	// CategoryLifecycle validates the Lifecycle RPC (Provision, Deprovision, Validate, HealthCheck).
	CategoryLifecycle TestCategory = "lifecycle"

	// CategoryHealth validates health-check behavior under various conditions.
	CategoryHealth TestCategory = "health"

	// CategoryExtension validates the Extension RPC (ListOperations, Call).
	CategoryExtension TestCategory = "extension"
)

// ProviderDimension identifies the CPI provider type being tested.
type ProviderDimension string

const (
	DimensionModel     ProviderDimension = "model"
	DimensionMemory    ProviderDimension = "memory"
	DimensionTool      ProviderDimension = "tool"
	DimensionKnowledge ProviderDimension = "knowledge"
)

// CPIProvider is the interface that conformance tests call to interact with
// a CPI provider under test. Implementations wrap real gRPC connections
// or in-process driver adapters.
type CPIProvider interface {
	// GetProviderInfo returns the provider's identity metadata.
	GetProviderInfo(ctx context.Context) (*cpi.GetProviderInfoResponse, error)

	// GetProviderCapabilities returns the provider's declared capabilities.
	GetProviderCapabilities(ctx context.Context) (*cpi.GetProviderCapabilitiesResponse, error)

	// Provision requests the provider to allocate resources.
	Provision(ctx context.Context, req *cpi.ProvisionRequest) (*cpi.ProvisionResponse, error)

	// Deprovision requests the provider to release resources.
	Deprovision(ctx context.Context, req *cpi.DeprovisionRequest) (*cpi.DeprovisionResponse, error)

	// Update requests the provider to update an existing resource.
	Update(ctx context.Context, req *cpi.UpdateRequest) (*cpi.UpdateResponse, error)

	// Validate requests the provider to validate a spec.
	Validate(ctx context.Context, req *cpi.ValidateRequest) (*cpi.ValidateResponse, error)

	// HealthCheck checks the provider's readiness.
	HealthCheck(ctx context.Context) (*cpi.HealthCheckResponse, error)

	// ListExtensionOperations lists available extension operations.
	ListExtensionOperations(ctx context.Context) (*cpi.ListExtensionOperationsResponse, error)

	// ExtensionCall invokes an extension operation.
	ExtensionCall(ctx context.Context, req *cpi.ExtensionCallRequest) (*cpi.ExtensionCallResponse, error)
}

// CPIProviderFactory creates a fresh provider connection for each test.
type CPIProviderFactory func(t *testing.T) CPIProvider

// CPIConformanceTest describes a single test within a conformance profile.
type CPIConformanceTest struct {
	// Name is the test name (used as t.Run sub-test).
	Name string

	// Category is the CPI service area being tested.
	Category TestCategory

	// Tier indicates whether this test is required or optional.
	Tier Tier

	// Run executes the test against the provided CPIProvider.
	Run func(t *testing.T, provider CPIProvider)
}

// CPIConformanceProfile groups tests for a specific provider dimension.
type CPIConformanceProfile struct {
	// Dimension identifies which provider type this profile validates.
	Dimension ProviderDimension

	// Tests is the ordered list of conformance tests.
	Tests []CPIConformanceTest
}

// TestResult records the outcome of a single conformance test.
type TestResult struct {
	Name     string
	Category TestCategory
	Tier     Tier
	Passed   bool
	Skipped  bool
	Error    string
}

// CPIConformanceResult records the aggregate outcome of running a profile.
type CPIConformanceResult struct {
	Dimension ProviderDimension
	Passed    int
	Failed    int
	Skipped   int
	Results   []TestResult
}
