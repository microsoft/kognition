// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRepositoryTestEntrypointsHaveTimeouts(t *testing.T) {
	root, err := findRepoRoot()
	if err != nil {
		t.Fatalf("find repository root: %v", err)
	}

	checks := []struct {
		path     string
		required [][]string
	}{
		{path: "Makefile", required: [][]string{{"$(GOTEST)", "-timeout=3m", "./..."}}},
		{path: "make.ps1", required: [][]string{{"function Target-Test", "'-timeout=3m'", "'./...'"}}},
		{path: "tests/integration/run_tests.ps1", required: [][]string{{"$testArgs", "\"-tags\", \"integration\"", "\"-timeout=10m\""}}},
		{path: ".github/workflows/ci.yml", required: [][]string{
			{"go test", "-timeout=10m", "-coverprofile=coverage.out", "./..."},
			{"go test", "-tags integration", "-timeout=15m", "./..."},
		}},
		{path: ".vscode/settings.json", required: [][]string{{"\"go.testTimeout\": \"3m\""}, {"\"go.testFlags\": [\"-count=1\", \"-v\"]"}}},
	}

	for _, check := range checks {
		t.Run(check.path, func(t *testing.T) {
			data, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(check.path)))
			if readErr != nil {
				t.Fatalf("read entry point: %v", readErr)
			}
			content := string(data)
			for _, commandParts := range check.required {
				for _, required := range commandParts {
					if !strings.Contains(content, required) {
						t.Fatalf("missing bounded test configuration component %q", required)
					}
				}
			}
		})
	}
}

func TestObservabilityInstallRequiresKognitionReleaseBeforeProvisioning(t *testing.T) {
	root, err := findRepoRoot()
	if err != nil {
		t.Fatalf("find repository root: %v", err)
	}
	checks := []struct {
		path      string
		preflight string
		provision string
	}{
		{path: "make.ps1", preflight: "& helm status $Release --namespace $Namespace", provision: "kubectl create namespace $ObservabilityNamespace"},
		{path: "Makefile", preflight: "@if ! helm status $(RELEASE) --namespace $(NAMESPACE)", provision: "kubectl create namespace $(OBSERVABILITY_NAMESPACE)"},
	}
	for _, check := range checks {
		t.Run(check.path, func(t *testing.T) {
			data, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(check.path)))
			if readErr != nil {
				t.Fatalf("read entry point: %v", readErr)
			}
			content := string(data)
			preflightIndex := strings.Index(content, check.preflight)
			provisionIndex := strings.Index(content, check.provision)
			if preflightIndex < 0 || provisionIndex < 0 || preflightIndex > provisionIndex {
				t.Fatalf("observability release preflight must precede provisioning in %s", check.path)
			}
		})
	}
}
