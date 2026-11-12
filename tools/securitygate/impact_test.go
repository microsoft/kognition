// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

func planRepository(test *testing.T) (string, string) {
	test.Helper()
	root := test.TempDir()
	planGit(test, root, "init", "--quiet")
	planWrite(test, root, "docs/guide.md", "# Guide\n")
	planWrite(test, root, "internal/k8s/activation/control.go", "package activation\n")
	planWrite(test, root, "go.mod", "module example.test/project\n")
	planGit(test, root, "add", ".")
	planGit(test, root, "commit", "--quiet", "-m", "baseline")
	return root, planGit(test, root, "rev-parse", "HEAD")
}

func planGit(test *testing.T, root string, arguments ...string) string {
	test.Helper()
	command := exec.Command("git", append([]string{"-C", root}, arguments...)...)
	command.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Gate Test", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=Gate Test", "GIT_COMMITTER_EMAIL=test@example.invalid")
	output, err := command.CombinedOutput()
	if err != nil {
		test.Fatalf("git %v: %v: %s", arguments, err, output)
	}
	return strings.TrimSpace(string(output))
}

func planWrite(test *testing.T, root, name, content string) {
	test.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		test.Fatal(err)
	}
}

func readPlan(test *testing.T, root, base string, extra ...string) impactPlan {
	test.Helper()
	var output, errors bytes.Buffer
	arguments := append([]string{"--repo", root, "--base", base}, extra...)
	if code := runPlan(arguments, &output, &errors); code != 0 {
		test.Fatalf("plan exit %d: %s", code, &errors)
	}
	var plan impactPlan
	if err := json.Unmarshal(output.Bytes(), &plan); err != nil {
		test.Fatal(err)
	}
	return plan
}

func TestChangePlanSelectsAffectedChecks(test *testing.T) {
	for _, scenario := range []struct {
		name, path, tier, check, control string
	}{
		{"documentation", "docs/new guide.md", "documentation", "documentation", ""},
		{"development-plan", "docs/development-plans/refactoring/repository-refactor-plan.md", "documentation", "documentation", ""},
		{"feature", "internal/scheduler/queue.go", "standard", "affected-tests", ""},
		{"admission", "internal/admission/check.go", "security", "security-regressions", "admission"},
		{"rbac", "deploy/helm/kognition/templates/new-role.yaml", "security", "configuration-scan", "admission"},
		{"dependencies", "go.mod", "security", "dependency-audit", "supply-chain"},
		{"authentication", "internal/auth/session.go", "security", "risk-delta-review", "runtime-authority"},
		{"new-security-module", "internal/futurecrypto/signer.go", "security", "risk-delta-review", "runtime-authority"},
		{"trust-contract", "docs/security.md", "review-required", "classification-review", "assurance"},
		{"gate-policy", "docs/development-gates.md", "review-required", "classification-review", "assurance"},
		{"relocated-assurance-plan", "docs/development-plans/features/knowledge-provider-execution/security-gate-backlog.md", "review-required", "classification-review", "assurance"},
		{"unknown", "unmapped/new-engine.xyz", "review-required", "classification-review", ""},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			root, base := planRepository(test)
			planWrite(test, root, scenario.path, "changed\n")
			planGit(test, root, "add", ".")
			planGit(test, root, "commit", "--quiet", "-m", "change")
			plan := readPlan(test, root, base)
			if plan.Tier != scenario.tier || plan.ClearanceGranted || !slices.Contains(plan.RequiredChecks, scenario.check) {
				test.Fatalf("unexpected plan: %+v", plan)
			}
			if scenario.control != "" && !slices.Contains(plan.AffectedControls, scenario.control) {
				test.Fatalf("missing affected control %s", scenario.control)
			}
			for _, check := range plan.RequiredChecks {
				if plan.CheckGuidance[check] == "" {
					test.Fatalf("missing guidance for %s", check)
				}
			}
			if scenario.tier == "documentation" && (slices.Contains(plan.RequiredChecks, "release-certification") || len(plan.AffectedControls) != 0) {
				test.Fatal("documentation triggered broad security revalidation")
			}
		})
	}
}

func TestChangePlanKeepsUnchangedControlInputs(test *testing.T) {
	root, base := planRepository(test)
	planWrite(test, root, "docs/guide.md", "# Updated guide\n")
	planGit(test, root, "add", ".")
	planGit(test, root, "commit", "--quiet", "-m", "docs")
	plan := readPlan(test, root, base)
	for _, control := range plan.Controls {
		if control.BaseInputSHA256 != control.HeadInputSHA256 || !control.InputsUnchanged {
			test.Fatalf("documentation invalidated %s evidence inputs", control.ID)
		}
	}
	planWrite(test, root, "go.mod", "module example.test/changed\n")
	planGit(test, root, "add", ".")
	planGit(test, root, "commit", "--quiet", "-m", "dependencies")
	plan = readPlan(test, root, base)
	for _, control := range plan.Controls {
		if control.InputsUnchanged {
			test.Fatalf("dependency drift omitted from %s", control.ID)
		}
	}
}

func TestChangePlanAccountsForRenamesDeletionsAndModes(test *testing.T) {
	root, base := planRepository(test)
	planGit(test, root, "mv", "internal/k8s/activation/control.go", "docs/control.md")
	planGit(test, root, "commit", "--quiet", "-m", "rename")
	plan := readPlan(test, root, base)
	if plan.Tier != "security" || len(plan.Changes) != 2 || !slices.Contains(plan.AffectedControls, "activation") {
		test.Fatalf("security deletion hidden by rename: %+v", plan)
	}
	planGit(test, root, "update-index", "--chmod=+x", "docs/guide.md")
	planGit(test, root, "commit", "--quiet", "-m", "mode")
	plan = readPlan(test, root, base)
	if plan.Tier != "review-required" {
		test.Fatal("executable documentation treated as text-only")
	}
}

func TestChangePlanRejectsInvalidInputsAndFlagsDirtyState(test *testing.T) {
	root, base := planRepository(test)
	for _, arguments := range [][]string{{"--repo", root}, {"--repo", root, "--base", "--help"}, {"--repo", root, "--base", "missing-ref"}, {"--repo", root, "--base", base, "--format", "invalid"}} {
		var output, errors bytes.Buffer
		if runPlan(arguments, &output, &errors) == 0 || output.Len() != 0 {
			test.Fatalf("invalid plan accepted: %v", arguments)
		}
	}
	planWrite(test, root, "untracked.txt", "not part of committed candidate\n")
	plan := readPlan(test, root, base, "--release")
	if plan.CleanWorktree || !slices.Contains(plan.RequiredChecks, "worktree-review") || !slices.Contains(plan.RequiredChecks, "release-certification") {
		test.Fatal("dirty state or explicit release gates omitted")
	}
}

func TestChangePlanMissingBaselineAndDeterministicSummary(test *testing.T) {
	root, base := planRepository(test)
	plan := readPlan(test, root, "EMPTY")
	if plan.Tier != "review-required" || len(plan.Changes) != 3 || plan.BaseCommit != "EMPTY" {
		test.Fatalf("missing baseline not conservatively handled: %+v", plan)
	}
	planWrite(test, root, "docs/tick`guide.md", "# Changed\n")
	planGit(test, root, "add", ".")
	planGit(test, root, "commit", "--quiet", "-m", "docs")
	var first, second, errors bytes.Buffer
	arguments := []string{"--repo", root, "--base", base}
	if runPlan(arguments, &first, &errors) != 0 || runPlan(arguments, &second, &errors) != 0 || first.String() != second.String() {
		test.Fatalf("plan is not deterministic: %s", &errors)
	}
	var summary bytes.Buffer
	if runPlan(append(arguments, "--format", "markdown"), &summary, &errors) != 0 || !strings.Contains(summary.String(), "tick&#96;guide.md") || !strings.Contains(summary.String(), "no clearance granted") {
		test.Fatalf("unsafe or missing Markdown summary: %s: %s", &summary, &errors)
	}
}

func TestChangePlanWorkflowIsAdvisory(test *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "ci.yml"))
	if err != nil {
		test.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Needs       any               `json:"needs"`
			Permissions map[string]string `json:"permissions"`
			Steps       []struct {
				Run  string            `json:"run"`
				Uses string            `json:"uses"`
				With map[string]string `json:"with"`
			} `json:"steps"`
		} `json:"jobs"`
	}
	if err := yaml.Unmarshal(contents, &workflow); err != nil {
		test.Fatal(err)
	}
	for _, required := range []string{"change-plan", "traceability", "proto", "lint", "test", "integration-test", "python-tests", "test-watchdog", "security", "build"} {
		if _, exists := workflow.Jobs[required]; !exists {
			test.Fatalf("missing existing CI gate %s", required)
		}
	}
	planner := workflow.Jobs["change-plan"]
	if planner.Needs != nil || planner.Permissions["contents"] != "read" || len(planner.Permissions) != 1 {
		test.Fatal("advisory planner requires privileges or becomes an execution dependency")
	}
	var commands string
	var artifactPath string
	for _, step := range planner.Steps {
		commands += step.Run
		if strings.Contains(step.Uses, "upload-artifact") {
			artifactPath = step.With["path"]
		}
	}
	for _, required := range []string{"BASE_SHA=EMPTY", "--base", "--head", "--format markdown", "${RUNNER_TEMP}/security-change-plan.json"} {
		if !strings.Contains(commands, required) {
			test.Fatalf("missing conservative report handling: %s", required)
		}
	}
	if artifactPath != "${{ runner.temp }}/security-change-plan.json" {
		test.Fatal("plan artifact must be outside the candidate repository")
	}
	for name, job := range workflow.Jobs {
		if name == "change-plan" {
			continue
		}
		encoded, err := json.Marshal(job.Needs)
		if err != nil {
			test.Fatal(err)
		}
		if bytes.Contains(encoded, []byte("change-plan")) {
			test.Fatalf("%s trusts advisory plan as a gate", name)
		}
	}
}
