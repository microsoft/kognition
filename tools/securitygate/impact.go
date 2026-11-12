// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"path"
	"slices"
	"sort"
	"strings"
	"time"
)

const impactPolicyVersion = "kognition.dev/change-gates/v1"

type trackedInput struct {
	Mode string
	Blob string
}

type impactChange struct {
	Path     string   `json:"path"`
	Status   string   `json:"status"`
	Tier     string   `json:"tier"`
	Controls []string `json:"controls"`
	Reason   string   `json:"reason"`
}

type controlInputs struct {
	ID              string   `json:"id"`
	Gates           []string `json:"baselineGates"`
	BaseInputSHA256 string   `json:"baseInputSHA256"`
	HeadInputSHA256 string   `json:"headInputSHA256"`
	InputsUnchanged bool     `json:"inputsUnchanged"`
	Inputs          []string `json:"inputs"`
}

type impactPlan struct {
	SchemaVersion        string            `json:"schemaVersion"`
	Purpose              string            `json:"purpose"`
	BaseCommit           string            `json:"baseCommit"`
	HeadCommit           string            `json:"headCommit"`
	Tier                 string            `json:"tier"`
	CleanWorktree        bool              `json:"cleanWorktree"`
	ClearanceGranted     bool              `json:"clearanceGranted"`
	RequiredChecks       []string          `json:"requiredChecks"`
	AffectedControls     []string          `json:"affectedControls"`
	ChangedControlInputs []string          `json:"changedControlInputs"`
	Controls             []controlInputs   `json:"controls"`
	Changes              []impactChange    `json:"changes"`
	EvidenceRequirements []string          `json:"evidenceRequirements"`
	CheckGuidance        map[string]string `json:"checkGuidance"`
}

type controlSurface struct {
	ID    string
	Gates []string
	Roots []string
}

var controlSurfaces = []controlSurface{
	{"admission", []string{"SG-01", "SG-03", "SG-06"}, []string{"internal/admission/", "internal/k8s/admissionguard/", "internal/k8s/webhook/", "internal/k8s/activation/", "internal/namespace/", "cmd/kognition-k8s-controller/", "deploy/helm/", "internal/traceability/"}},
	{"activation", []string{"SG-02", "SG-04"}, []string{"internal/k8s/activation/", "tools/check-admission-activation/", "internal/k8s/admissionguard/", "deploy/helm/", "cmd/kognition-k8s-controller/", "internal/traceability/"}},
	{"installation", []string{"SG-05", "SG-10"}, []string{"internal/k8s/installation/", "tools/kognition-install/", "make.ps1", "Makefile", "bootstrap.ps1", "deploy/", "samples/helm-umbrella/charts/", "internal/traceability/"}},
	{"runtime-authority", []string{"SG-06", "SG-08"}, []string{"internal/runtimeapproval/", "internal/authorization/", "internal/identity/", "internal/namespace/", "internal/invocation/", "internal/toolgateway/", "internal/modelgateway/", "internal/agentgateway/", "internal/memory/", "internal/runtime/", "internal/k8s/controller/", "internal/k8s/dispatcher/", "internal/controller/", "runtime/", "cmd/", "deploy/helm/", "internal/traceability/"}},
	{"provider", []string{"SG-08"}, []string{"providers/", "pkg/knowledge/", "pkg/pluginsdk/", "internal/knowledge/", "internal/k8s/knowledge/", "internal/cpi/", "internal/plugin/", "internal/k8s/controller/", "internal/controller/", "cmd/kognition-knowledge", "runtime/ingester/", "deploy/helm/", "internal/traceability/"}},
	{"transport", []string{"SG-03", "SG-07", "SG-08"}, []string{"internal/k8s/admissionguard/", "internal/runtime/", "internal/k8s/controller/", "internal/k8s/dispatcher/", "internal/toolgateway/", "internal/agentgateway/", "pkg/pluginsdk/", "runtime/", "deploy/", "internal/traceability/"}},
	{"persistence", []string{"SG-08"}, []string{"internal/store/", "internal/memory/", "internal/runtimeapproval/", "providers/pgvector/", "cmd/kognition-migrate/", "deploy/helm/", "internal/traceability/"}},
	{"supply-chain", []string{"SG-10", "SG-12"}, []string{"Dockerfile", "Dockerfile.", "runtime/", "deploy/", "samples/", "providers/", "buf.yaml", "buf.gen.yaml", "proto/"}},
	{"assurance", []string{"SG-09", "SG-11", "SG-12", "SG-13"}, []string{"tools/securitygate/", ".github/", ".githooks/", "docs/development-gates.md", "docs/security.md", "docs/features/knowledge-provider-execution/", "docs/development-plans/features/knowledge-provider-execution/", "docs/adr/", ".gitignore", ".gitattributes"}},
}

func matchesSurface(name string, roots []string) bool {
	for _, root := range roots {
		if name == root || strings.HasPrefix(name, root) && (strings.HasSuffix(root, "/") || strings.HasSuffix(root, ".") || strings.HasSuffix(root, "kognition-knowledge") || strings.HasSuffix(root, "/security") || strings.HasSuffix(root, "/sg")) {
			return true
		}
	}
	return false
}

func governancePath(name string) bool {
	return matchesSurface(name, controlSurfaces[len(controlSurfaces)-1].Roots) || strings.HasPrefix(name, "tools/kognition-install/") || strings.HasPrefix(name, "tools/check-admission-activation/")
}

func dependencyPath(name string) bool {
	base := path.Base(name)
	return base == "go.mod" || base == "go.sum" || base == "package.json" || base == "package-lock.json" || base == "pyproject.toml" || strings.Contains(base, "requirements") || strings.HasSuffix(name, ".lock") || strings.Contains(base, "Dockerfile") || name == "buf.gen.yaml"
}

func securityPath(name string) bool {
	for _, word := range []string{"auth", "rbac", "credential", "quota", "audit", "policy", "opa", "gateway", "migration", "network", "secret", "signing", "crypto", "capability", "token", "approval"} {
		if strings.Contains(strings.ToLower(name), word) {
			return true
		}
	}
	return false
}

func classifyChange(name string, before, after trackedInput) impactChange {
	change := impactChange{Path: name, Status: "modified", Tier: "review-required", Controls: []string{}, Reason: "unmapped path requires human impact classification"}
	if before.Blob == "" {
		change.Status = "added"
	}
	if after.Blob == "" {
		change.Status = "deleted"
	}
	if before.Mode != "" && before.Mode != "100644" || after.Mode != "" && after.Mode != "100644" {
		change.Reason = "executable, symlink or submodule change requires review"
		return change
	}
	if governancePath(name) {
		change.Controls = []string{"assurance"}
		change.Reason = "gate policy, verifier or automation changes require review; a plan cannot approve itself"
		return change
	}
	if dependencyPath(name) {
		change.Tier, change.Reason = "security", "dependency, toolchain or image inputs changed"
		change.Controls = []string{"supply-chain"}
		return change
	}
	if strings.HasSuffix(strings.ToLower(name), ".md") && (strings.HasPrefix(name, "docs/") || strings.HasPrefix(name, "samples/") || strings.HasPrefix(name, "deploy/") || !strings.Contains(name, "/")) {
		change.Tier, change.Reason = "documentation", "documentation and examples; review operational or trust-contract changes explicitly"
		return change
	}
	for _, control := range controlSurfaces[:len(controlSurfaces)-1] {
		if matchesSurface(name, control.Roots) {
			change.Controls = append(change.Controls, control.ID)
		}
	}
	if len(change.Controls) > 0 {
		change.Tier, change.Reason = "security", "tracked security-control or shared dependency surface changed"
		return change
	}
	if securityPath(name) || sharedControlInput(name) {
		change.Tier, change.Reason = "security", "security-sensitive or shared contract path; review relevant control coupling"
		change.Controls = []string{"runtime-authority"}
		return change
	}
	if strings.HasPrefix(name, "api/") || strings.HasPrefix(name, "pkg/") || strings.HasPrefix(name, "internal/") || strings.HasPrefix(name, "cmd/") || strings.HasPrefix(name, "runtime/") || strings.HasPrefix(name, "e2e/") || strings.HasPrefix(name, "tests/") || strings.HasPrefix(name, "tools/") {
		change.Tier, change.Reason = "standard", "application or test change; validate affected behavior and consumers"
	}
	return change
}

func sharedControlInput(name string) bool {
	return dependencyPath(name) || governancePath(name) || strings.HasPrefix(name, "api/") || strings.HasPrefix(name, "proto/") || strings.HasPrefix(name, "pkg/") || strings.HasPrefix(name, "internal/store/") || strings.HasPrefix(name, "internal/convert/") || strings.HasPrefix(name, "internal/runtimeapproval/") || strings.HasPrefix(name, "internal/auth/") || strings.HasPrefix(name, "internal/controlplaneauth/") || strings.HasPrefix(name, "internal/runtimeauth/") || strings.HasPrefix(name, "internal/opa/") || strings.HasPrefix(name, "internal/namespace/")
}

func inputFingerprint(tree map[string]trackedInput, control controlSurface) (string, []string) {
	names := []string{}
	for name := range tree {
		if (sharedControlInput(name) || matchesSurface(name, control.Roots)) && (!strings.HasSuffix(name, ".md") || governancePath(name)) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	digest := sha256.New()
	_, _ = fmt.Fprintf(digest, "%s\x00%s\x00", impactPolicyVersion, control.ID)
	for _, name := range names {
		input := tree[name]
		_, _ = fmt.Fprintf(digest, "%s\x00%s\x00%s\x00", name, input.Mode, input.Blob)
	}
	return fmt.Sprintf("%x", digest.Sum(nil)), names
}

func planGitOutput(ctx context.Context, repository string, arguments ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", repository}, arguments...)...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s failed: %w: %s", arguments[0], err, strings.TrimSpace(stderr.String()))
	}
	return output, nil
}

func readTrackedTree(ctx context.Context, repository, commit string) (map[string]trackedInput, error) {
	output, err := planGitOutput(ctx, repository, "ls-tree", "-r", "-z", "--full-tree", commit)
	if err != nil {
		return nil, err
	}
	tree := map[string]trackedInput{}
	for _, entry := range bytes.Split(output, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		metadata, name, found := strings.Cut(string(entry), "\t")
		fields := strings.Fields(metadata)
		if !found || len(fields) != 3 || name == "" {
			return nil, errors.New("malformed tracked-tree entry")
		}
		tree[name] = trackedInput{Mode: fields[0], Blob: fields[2]}
	}
	return tree, nil
}

func uniqueSorted(values []string) []string {
	sort.Strings(values)
	return slices.Compact(values)
}

func buildImpactPlan(ctx context.Context, repository, base, head string, release bool) (impactPlan, error) {
	plan := impactPlan{SchemaVersion: impactPolicyVersion, Purpose: "validation-plan-not-clearance", Tier: "none", RequiredChecks: []string{"secret-check"}, AffectedControls: []string{}, ChangedControlInputs: []string{}, Controls: []controlInputs{}, Changes: []impactChange{}, EvidenceRequirements: []string{
		"Compare trusted passing receipts against control input hashes, command, toolchain, configuration, image and environment identities before reusing evidence.",
		"Fingerprints cover declared surfaces, not inferred transitive dependencies; newly coupled code or unknown impact requires review.",
		"Scan freshness and exact finding/source/report approval bindings remain mandatory; this plan never imports or expands a disposition.",
		"Deployment activation still requires fresh exact-target evidence and approval; no plan output grants privileges or starts workloads.",
	}}
	resolve := func(reference string) (string, error) {
		output, err := planGitOutput(ctx, repository, "rev-parse", "--verify", "--end-of-options", reference+"^{commit}")
		return strings.TrimSpace(string(output)), err
	}
	var err error
	if plan.HeadCommit, err = resolve(head); err != nil {
		return plan, err
	}
	before := map[string]trackedInput{}
	if base == "EMPTY" {
		plan.BaseCommit, plan.Tier = "EMPTY", "review-required"
		plan.RequiredChecks = append(plan.RequiredChecks, "classification-review")
		plan.EvidenceRequirements = append(plan.EvidenceRequirements, "No baseline is available: review all candidate paths and establish baseline evidence; no evidence inheritance is established.")
	} else {
		if plan.BaseCommit, err = resolve(base); err != nil {
			return plan, err
		}
		if before, err = readTrackedTree(ctx, repository, plan.BaseCommit); err != nil {
			return plan, err
		}
	}
	after, err := readTrackedTree(ctx, repository, plan.HeadCommit)
	if err != nil {
		return plan, err
	}
	names := []string{}
	for name := range before {
		names = append(names, name)
	}
	for name := range after {
		names = append(names, name)
	}
	rank := map[string]int{"none": 0, "documentation": 1, "standard": 2, "security": 3, "review-required": 4}
	for _, name := range uniqueSorted(names) {
		if before[name] == after[name] {
			continue
		}
		change := classifyChange(name, before[name], after[name])
		plan.Changes = append(plan.Changes, change)
		plan.AffectedControls = append(plan.AffectedControls, change.Controls...)
		if rank[change.Tier] > rank[plan.Tier] {
			plan.Tier = change.Tier
		}
		if dependencyPath(name) {
			plan.RequiredChecks = append(plan.RequiredChecks, "dependency-audit", "image-and-build-validation")
		}
		if strings.HasPrefix(name, "deploy/") || strings.Contains(name, "/charts/") {
			plan.RequiredChecks = append(plan.RequiredChecks, "configuration-scan", "chart-parity")
		}
	}
	for _, control := range controlSurfaces {
		baseHash, _ := inputFingerprint(before, control)
		headHash, inputs := inputFingerprint(after, control)
		plan.Controls = append(plan.Controls, controlInputs{ID: control.ID, Gates: control.Gates, BaseInputSHA256: baseHash, HeadInputSHA256: headHash, InputsUnchanged: baseHash == headHash, Inputs: inputs})
		if baseHash != headHash {
			plan.ChangedControlInputs = append(plan.ChangedControlInputs, control.ID)
		}
	}
	if len(plan.Changes) > 0 {
		plan.RequiredChecks = append(plan.RequiredChecks, "documentation")
	}
	if rank[plan.Tier] >= rank["standard"] {
		plan.RequiredChecks = append(plan.RequiredChecks, "affected-tests", "lint", "development-ci")
	}
	if rank[plan.Tier] >= rank["security"] {
		plan.RequiredChecks = append(plan.RequiredChecks, "security-regressions", "risk-delta-review")
	}
	if plan.Tier == "review-required" {
		plan.RequiredChecks = append(plan.RequiredChecks, "classification-review")
	}
	if release {
		plan.RequiredChecks = append(plan.RequiredChecks, "release-certification", "fresh-security-scans", "release-evidence-review")
	}
	status, err := planGitOutput(ctx, repository, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return plan, err
	}
	plan.CleanWorktree = len(status) == 0
	if !plan.CleanWorktree {
		plan.RequiredChecks = append(plan.RequiredChecks, "worktree-review")
	}
	plan.RequiredChecks = uniqueSorted(plan.RequiredChecks)
	plan.AffectedControls = uniqueSorted(plan.AffectedControls)
	guidance := map[string]string{
		"secret-check":               "Scan the exact committed candidate with Trivy secret scanning; preserve report and candidate hash. Never print secrets.",
		"documentation":              "Validate changed Markdown links and examples; review any operational/trust contract changes.",
		"affected-tests":             "Run go test -race -count=1 for changed Go packages and public consumers; run the relevant Python/PowerShell/Playwright tests for those runtime surfaces.",
		"lint":                       "Run golangci-lint on touched Go packages and applicable script/type checks.",
		"development-ci":             "Require existing CI traceability, proto, lint, test/coverage, integration, Python, watchdog, security and build results; planner success does not replace them.",
		"security-regressions":       "Select changed admission, identity, lifecycle or isolation cases from docs/testing.md and SG-01 through SG-10; deployed evidence only when a changed control depends on that boundary.",
		"risk-delta-review":          "Review changed trust/privilege boundaries and exact new/changed scanner findings. Do not transfer old per-finding approvals to changed targets.",
		"classification-review":      "Review unknown paths or changes to assurance code/policy; record control mapping and selected checks before publication. No automatic full-campaign escalation.",
		"dependency-audit":           "Run govulncheck ./... and pip-audit -r for changed Python requirements; refresh the relevant image and JavaScript audits when those inputs change.",
		"image-and-build-validation": "Build changed targets and record toolchain/base-image digests; rerun affected runtime tests, not unrelated deployed suites.",
		"configuration-scan":         "Render affected Helm profiles and run Trivy misconfiguration/secret scans; review exact finding deltas and retained residual risks.",
		"chart-parity":               "Run deploy/helm/kognition/tests/helm-regression-tests.ps1 and affected chart/archive traceability tests. For timestamp-only archives verify every payload and metadata difference.",
		"worktree-review":            "This plan excludes uncommitted/untracked files. Review and commit intended changes, then rerun against the final committed candidate.",
		"release-certification":      "Run the broad release-candidate quality/security/packaging matrix for supported profiles, reusing only verified unaffected control evidence.",
		"fresh-security-scans":       "Refresh release artifact dependency, secret and misconfiguration scans with recorded scanner/database versions and existing freshness constraints.",
		"release-evidence-review":    "Bind the release to exact artifacts and reviewed findings; production activation remains a separate exact-target approval.",
	}
	plan.CheckGuidance = map[string]string{}
	for _, check := range plan.RequiredChecks {
		plan.CheckGuidance[check] = guidance[check]
	}
	return plan, nil
}

func runPlan(arguments []string, output, errorsOutput io.Writer) int {
	flags := flag.NewFlagSet("securitygate plan", flag.ContinueOnError)
	flags.SetOutput(errorsOutput)
	repository := flags.String("repo", ".", "Git repository to inspect without mutation")
	base := flags.String("base", "", "Explicit reviewed baseline commit or ref")
	head := flags.String("head", "HEAD", "Committed candidate; excludes working edits")
	release := flags.Bool("release", false, "Add broad release certification without implying deployment approval")
	format := flags.String("format", "json", "Output format: json or markdown")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *base == "" || *head == "" || (*format != "json" && *format != "markdown") {
		_, _ = fmt.Fprintln(errorsOutput, "plan requires --base REF, optional --head REF/--repo PATH/--release/--format json|markdown")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	plan, err := buildImpactPlan(ctx, *repository, *base, *head, *release)
	if err == nil {
		if *format == "json" {
			err = json.NewEncoder(output).Encode(plan)
		} else {
			_, err = fmt.Fprintf(output, "## Change Gate Plan\n\nTier: **%s**. Planning only; no clearance granted.\n\nBaseline: `%s`\n\nCandidate: `%s`\n\nClean worktree: `%t`\n\n| Changed Path | Status | Tier | Reason |\n| --- | --- | --- | --- |\n", plan.Tier, plan.BaseCommit, plan.HeadCommit, plan.CleanWorktree)
			for _, change := range plan.Changes {
				if err != nil {
					break
				}
				name := strings.NewReplacer("|", "&#124;", "`", "&#96;", "\n", " ", "\r", " ").Replace(change.Path)
				_, err = fmt.Fprintf(output, "| `%s` | %s | %s | %s |\n", name, change.Status, change.Tier, change.Reason)
			}
			if err == nil {
				_, err = fmt.Fprintln(output, "\nRequired checks: "+strings.Join(plan.RequiredChecks, ", ")+".\n\nUnchanged control inputs are reusable only after verifying trusted receipts, toolchain, configuration and environment. Fresh scans, changed risks and deployment approvals remain separate.")
			}
		}
	}
	if err != nil {
		_, _ = fmt.Fprintln(errorsOutput, err)
		return 1
	}
	return 0
}
