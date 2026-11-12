// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package conformance

import (
	"fmt"
	"testing"
)

// RunCPIConformanceSuite executes the full conformance profile for a CPI provider
// of the given dimension. All tests (required and optional) are run.
func RunCPIConformanceSuite(t *testing.T, dimension ProviderDimension, factory CPIProviderFactory) {
	t.Helper()

	profile := GetCPIProfile(dimension)
	if profile == nil {
		t.Fatalf("no conformance profile defined for dimension %q", dimension)
	}

	for _, test := range profile.Tests {
		test := test // capture
		t.Run(fmt.Sprintf("%s/%s/%s", test.Category, test.Tier, test.Name), func(t *testing.T) {
			provider := factory(t)
			test.Run(t, provider)
		})
	}
}

// RunCPIConformanceRequired executes only the required-tier tests for a CPI
// provider. This is useful for quick smoke-testing of provider compliance.
func RunCPIConformanceRequired(t *testing.T, dimension ProviderDimension, factory CPIProviderFactory) {
	t.Helper()

	profile := GetCPIProfile(dimension)
	if profile == nil {
		t.Fatalf("no conformance profile defined for dimension %q", dimension)
	}

	for _, test := range profile.Tests {
		if test.Tier != Required {
			continue
		}
		test := test // capture
		t.Run(fmt.Sprintf("%s/%s", test.Category, test.Name), func(t *testing.T) {
			provider := factory(t)
			test.Run(t, provider)
		})
	}
}

// RunCPIConformanceCategory executes tests for a specific category within a
// dimension's profile. Useful for targeted validation of a single CPI service.
func RunCPIConformanceCategory(t *testing.T, dimension ProviderDimension, category TestCategory, factory CPIProviderFactory) {
	t.Helper()

	profile := GetCPIProfile(dimension)
	if profile == nil {
		t.Fatalf("no conformance profile defined for dimension %q", dimension)
	}

	for _, test := range profile.Tests {
		if test.Category != category {
			continue
		}
		test := test // capture
		t.Run(fmt.Sprintf("%s/%s/%s", test.Tier, test.Name, test.Category), func(t *testing.T) {
			provider := factory(t)
			test.Run(t, provider)
		})
	}
}

// CollectCPIConformanceResults runs the full profile and collects structured results
// without using t.Fatal — useful for programmatic conformance reporting.
func CollectCPIConformanceResults(t *testing.T, dimension ProviderDimension, factory CPIProviderFactory) *CPIConformanceResult {
	t.Helper()

	profile := GetCPIProfile(dimension)
	if profile == nil {
		t.Fatalf("no conformance profile defined for dimension %q", dimension)
		return nil
	}

	result := &CPIConformanceResult{
		Dimension: dimension,
		Results:   make([]TestResult, 0, len(profile.Tests)),
	}

	for _, test := range profile.Tests {
		test := test // capture
		testName := fmt.Sprintf("%s/%s/%s", test.Category, test.Tier, test.Name)
		passed := t.Run(testName, func(t *testing.T) {
			provider := factory(t)
			test.Run(t, provider)
		})

		tr := TestResult{
			Name:     test.Name,
			Category: test.Category,
			Tier:     test.Tier,
			Passed:   passed,
		}
		if passed {
			result.Passed++
		} else {
			result.Failed++
		}
		result.Results = append(result.Results, tr)
	}

	return result
}

// GetCPIProfile returns the conformance profile for a given provider dimension.
// Returns nil if the dimension is not recognized.
func GetCPIProfile(dimension ProviderDimension) *CPIConformanceProfile {
	switch dimension {
	case DimensionModel:
		return ModelProviderProfile()
	case DimensionMemory:
		return MemoryProviderProfile()
	case DimensionTool:
		return ToolProviderProfile()
	case DimensionKnowledge:
		return KnowledgeProviderProfile()
	default:
		return nil
	}
}
