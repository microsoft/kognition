// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"go/version"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestReleaseGoToolchainIncludesSecurityFixes(test *testing.T) {
	root, err := findRepoRoot()
	if err != nil {
		test.Fatal(err)
	}
	for _, path := range []string{"go.mod", "Dockerfile", "runtime/ingester/Dockerfile", "e2e/sg08-provider.Dockerfile"} {
		test.Run(path, func(test *testing.T) {
			body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
			if err != nil {
				test.Fatal(err)
			}
			pattern := `golang:([0-9]+\.[0-9]+\.[0-9]+)-alpine@`
			if path == "go.mod" {
				pattern = `(?m)^go ([0-9]+\.[0-9]+\.[0-9]+)$`
			}
			match := regexp.MustCompile(pattern).FindStringSubmatch(string(body))
			if len(match) != 2 || !version.IsValid("go"+match[1]) || version.Compare("go"+match[1], "go1.26.10") < 0 {
				test.Fatal("release toolchain must include the Go 1.26.10 security fixes")
			}
		})
	}
}

func TestReleaseImagesPinBasesAndCrossCompileGo(test *testing.T) {
	root, err := findRepoRoot()
	if err != nil {
		test.Fatal(err)
	}
	pinned := regexp.MustCompile(`@sha256:[0-9a-f]{64}$`)
	for _, path := range []string{"Dockerfile", "runtime/agent/Dockerfile", "runtime/ingester/Dockerfile"} {
		test.Run(path, func(test *testing.T) {
			body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
			if err != nil {
				test.Fatal(err)
			}
			stages := map[string]bool{}
			for _, line := range strings.Split(string(body), "\n") {
				fields := strings.Fields(line)
				if len(fields) < 2 || !strings.EqualFold(fields[0], "FROM") {
					continue
				}
				image := fields[1]
				if strings.HasPrefix(image, "--platform=") && len(fields) > 2 {
					image = fields[2]
				}
				if !stages[strings.ToLower(image)] && !pinned.MatchString(image) {
					test.Errorf("release base is not digest-pinned: %s", line)
				}
				if len(fields) >= 4 && strings.EqualFold(fields[len(fields)-2], "AS") {
					stages[strings.ToLower(fields[len(fields)-1])] = true
				}
			}
			if path != "runtime/agent/Dockerfile" {
				for _, contract := range []string{"FROM --platform=$BUILDPLATFORM golang:", "ARG TARGETOS", "ARG TARGETARCH", "GOOS=$TARGETOS", "GOARCH=$TARGETARCH"} {
					if !strings.Contains(string(body), contract) {
						test.Errorf("Go release builder is missing target-platform contract %q", contract)
					}
				}
			}
		})
	}
}

func TestPythonReleaseFeedsRequireTLSWithoutPersistingConfiguration(test *testing.T) {
	root, err := findRepoRoot()
	if err != nil {
		test.Fatal(err)
	}
	for _, path := range []string{"runtime/agent/Dockerfile", "runtime/ingester/Dockerfile"} {
		test.Run(path, func(test *testing.T) {
			body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
			if err != nil {
				test.Fatal(err)
			}
			text := string(body)
			if !strings.Contains(text, "ARG PIP_INDEX_URL=https://pypi.org/simple") || !strings.Contains(text, `case "$PIP_INDEX_URL" in https://*)`) {
				test.Fatal("release package feed must retain the verified public default and require HTTPS")
			}
			if !strings.Contains(text, "https://deb.debian.org") || !strings.Contains(text, "apt-get update --error-on=any") {
				test.Error("OS update indexes must use verified HTTPS and fail on incomplete downloads")
			}
			if !strings.Contains(text, "python -m pip uninstall --yes pip") {
				test.Error("release runtime must remove the build-only installer and its vendored dependencies")
			}
			for _, forbidden := range []string{"ENV PIP_INDEX_URL", "--trusted-host", "PIP_TRUSTED_HOST", "packagefeedproxy.microsoft.io"} {
				if strings.Contains(text, forbidden) {
					test.Errorf("release recipe persists, hardcodes or weakens package feed configuration: %s", forbidden)
				}
			}
		})
	}
}

func TestCISecurityScannerPinsReviewedVersion(test *testing.T) {
	root, err := findRepoRoot()
	if err != nil {
		test.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "ci.yml"))
	if err != nil {
		test.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Uses string            `json:"uses"`
				With map[string]string `json:"with"`
			} `json:"steps"`
		} `json:"jobs"`
	}
	if err := yaml.Unmarshal(contents, &workflow); err != nil {
		test.Fatal(err)
	}
	for _, step := range workflow.Jobs["security"].Steps {
		if !strings.HasPrefix(step.Uses, "aquasecurity/trivy-action@") {
			continue
		}
		if step.With["version"] != "v0.74.0" || step.With["scanners"] != "vuln,secret,misconfig" || step.With["exit-code"] != "1" {
			test.Fatalf("scanner version or strict failure contract drifted: %+v", step.With)
		}
		return
	}
	test.Fatal("required repository security scanner missing")
}

func TestReleasePublishesChartImagesAndRuntimeProfiles(test *testing.T) {
	root, err := findRepoRoot()
	if err != nil {
		test.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "release.yml"))
	if err != nil {
		test.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Strategy struct {
				Matrix struct {
					Include []struct {
						Name       string `json:"name"`
						Target     string `json:"target"`
						Repository string `json:"repository"`
						Profile    string `json:"profile"`
					} `json:"include"`
				} `json:"matrix"`
			} `json:"strategy"`
			Steps []struct {
				Uses string            `json:"uses"`
				With map[string]string `json:"with"`
				Run  string            `json:"run"`
			} `json:"steps"`
		} `json:"jobs"`
	}
	if err := yaml.Unmarshal(contents, &workflow); err != nil {
		test.Fatal(err)
	}
	required := map[string]string{
		"migrate": "kognition-migrate", "apiserver": "kognition-apiserver",
		"controller": "kognition-controller-manager", "ctl": "kognition-ctl",
		"k8s-controller": "kognition-k8s-controller", "memory-service": "kognition-memory-service",
		"memory-policy-gateway": "kognition-memory-policy-gateway", "tool-gateway": "kognition-tool-gateway",
		"model-gateway": "kognition-model-gateway", "agent-communication-gateway": "kognition-agent-communication-gateway",
		"intent-gateway": "kognition-intent-gateway", "knowledge-gateway": "kognition-knowledge-gateway",
		"knowledge-azuresearch":   "kognition-knowledge-azuresearch",
		"knowledge-pgvector":      "kognition-knowledge-pgvector",
		"workload-identity-proxy": "kognition-workload-identity-proxy",
	}
	seen := map[string]bool{}
	for _, image := range workflow.Jobs["images"].Strategy.Matrix.Include {
		if seen[image.Target] || required[image.Target] != image.Repository || image.Name == "" {
			test.Errorf("invalid or duplicate release image: %+v", image)
		}
		seen[image.Target] = true
	}
	for target := range required {
		if !seen[target] {
			test.Errorf("production Docker stage %q has no release image", target)
		}
	}
	dockerfile, err := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if err != nil {
		test.Fatal(err)
	}
	for _, line := range strings.Split(string(dockerfile), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 4 && strings.EqualFold(fields[0], "FROM") && strings.EqualFold(fields[2], "AS") &&
			fields[3] != "builder" && fields[3] != "runtime" && !seen[fields[3]] {
			test.Errorf("Dockerfile stage %q is not published", fields[3])
		}
	}
	for _, chart := range []string{"kognition", "kognition-knowledge-azuresearch", "kognition-knowledge-pgvector"} {
		valuesBytes, err := os.ReadFile(filepath.Join(root, "deploy", "helm", chart, "values.yaml"))
		if err != nil {
			test.Fatal(err)
		}
		var values map[string]interface{}
		if err := yaml.Unmarshal(valuesBytes, &values); err != nil {
			test.Fatal(err)
		}
		var checkRepositories func(interface{})
		checkRepositories = func(value interface{}) {
			switch node := value.(type) {
			case map[string]interface{}:
				if repository, ok := node["repository"].(string); ok && strings.HasPrefix(repository, "kognition-") {
					published := repository == "kognition-kb-ingester"
					for _, image := range workflow.Jobs["images"].Strategy.Matrix.Include {
						published = published || image.Repository == repository
					}
					if !published {
						test.Errorf("chart %s uses unpublished repository %s", chart, repository)
					}
				}
				for _, child := range node {
					checkRepositories(child)
				}
			case []interface{}:
				for _, child := range node {
					checkRepositories(child)
				}
			}
		}
		checkRepositories(values)
	}
	runtimeProfiles := map[string]string{"native": "kognition-agent-runtime", "autogen": "kognition-agent-runtime-autogen"}
	for _, image := range workflow.Jobs["runtime-agent-image"].Strategy.Matrix.Include {
		if runtimeProfiles[image.Profile] != image.Repository || image.Name == "" {
			test.Errorf("invalid runtime image: %+v", image)
		}
		delete(runtimeProfiles, image.Profile)
	}
	if len(runtimeProfiles) != 0 {
		test.Errorf("missing runtime profiles: %+v", runtimeProfiles)
	}
	for _, jobName := range []string{"images", "runtime-agent-image"} {
		found := false
		for _, step := range workflow.Jobs[jobName].Steps {
			if step.Uses != "docker/build-push-action@v6" {
				continue
			}
			found = true
			for _, repository := range []string{"repository", "name"} {
				for _, tag := range []string{"${{ needs.resolve-tag.outputs.tag }}", "latest"} {
					if !strings.Contains(step.With["tags"], "/${{ matrix."+repository+" }}:"+tag) {
						test.Errorf("%s omits %s tag for %s", jobName, tag, repository)
					}
				}
			}
			if step.With["provenance"] != "true" || step.With["sbom"] != "true" || step.With["push"] != "true" {
				test.Errorf("%s weakened publication attestation", jobName)
			}
			if jobName == "runtime-agent-image" &&
				(!strings.Contains(step.With["build-args"], "KOGNITION_RUNTIME_PROFILE=${{ matrix.profile }}") ||
					!strings.Contains(step.With["build-args"], "KOGNITION_NOTICE_STRICT=true")) {
				test.Error("runtime release must select the tested profile and require complete installed-package notices")
			}
		}
		if !found {
			test.Errorf("%s has no build/push action", jobName)
		}
	}
	for _, jobName := range []string{"runtime-agent-image", "runtime-ingester-image"} {
		found := false
		for _, step := range workflow.Jobs[jobName].Steps {
			if step.Uses == "docker/build-push-action@v6" &&
				(step.With["context"] != "." || !strings.Contains(step.With["build-args"], "KOGNITION_NOTICE_STRICT=true")) {
				test.Errorf("%s must include root legal inputs and fail incomplete attribution before publishing", jobName)
			}
			if strings.Contains(step.Run, "/tmp/check-runtime-image.py") {
				found = true
				for _, required := range []string{"for architecture in amd64 arm64", "--network none", "--read-only", "--architecture \"$architecture\""} {
					if !strings.Contains(step.Run, required) {
						test.Errorf("%s omits runtime image verification contract %q", jobName, required)
					}
				}
			}
		}
		if !found {
			test.Errorf("%s does not verify the exact built runtime platforms", jobName)
		}
	}
	var chartCommands strings.Builder
	for _, step := range workflow.Jobs["helm-chart"].Steps {
		chartCommands.WriteString(step.Run)
		chartCommands.WriteByte('\n')
	}
	for _, chart := range []string{"kognition", "kognition-knowledge-azuresearch", "kognition-knowledge-pgvector"} {
		for _, command := range []string{"helm lint deploy/helm/" + chart, "deploy/helm/" + chart + " > /dev/null", "helm package deploy/helm/" + chart + " \\", "helm push .out/" + chart + "-${{ needs.resolve-tag.outputs.tag }}.tgz"} {
			if !strings.Contains(chartCommands.String(), command) {
				test.Errorf("release chart %s omits %s", chart, command)
			}
		}
	}
}

func findScriptsDir(t *testing.T) string {
	t.Helper()
	root, err := findRepoRoot()
	if err != nil {
		t.Fatalf("failed to find repo root: %v", err)
	}
	return filepath.Join(root, ".github", "scripts")
}

// TC-0144 TC-0145
// TestTraceabilityValidators runs the commit-message and PR-description
// validator test script.
func TestTraceabilityValidators(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash scripts not supported on Windows")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	sourceDir := findScriptsDir(t)
	scriptsDir := filepath.Join(t.TempDir(), ".github", "scripts")
	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Run outside the checkout so fixtures cannot rely on its Git history.
	for _, name := range []string{"test-traceability-validators.sh", "validate-commit-messages.sh", "validate-pr-description.sh"} {
		body, err := os.ReadFile(filepath.Join(sourceDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(scriptsDir, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cmd := exec.Command("bash", filepath.Join(scriptsDir, "test-traceability-validators.sh"))
	cmd.Dir = scriptsDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("traceability validators failed:\n%s", out)
	}
}

// TC-0146 TC-0147
// TestFRNFRCoverage runs the FR/NFR coverage checker test script.
func TestFRNFRCoverage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash scripts not supported on Windows")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	script := filepath.Join(findScriptsDir(t), "test-fr-nfr-coverage.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("script not found: %v", err)
	}

	cmd := exec.Command("bash", script)
	cmd.Dir = filepath.Dir(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("FR/NFR coverage test failed:\n%s", out)
	}
}

// TC-0148 TC-0149
// TestTraceCLI runs the trace CLI test script.
func TestTraceCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash scripts not supported on Windows")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}

	script := filepath.Join(findScriptsDir(t), "test-trace-cli.sh")
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("script not found: %v", err)
	}

	cmd := exec.Command("bash", script)
	cmd.Dir = filepath.Dir(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("trace CLI test failed:\n%s", out)
	}
}
