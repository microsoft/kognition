// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package traceability

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type tcMapping struct {
	tcID     string
	filePath string
	testName string
}

var completedEpicTCMappings = []tcMapping{
	{tcID: "TC-0003", filePath: "internal/store/postgres/store_test.go", testName: "TestDurability_AcrossStoreReinitialization"},
	{tcID: "TC-0005", filePath: "internal/controller/controller_test.go", testName: "TestController_ReconcileOnCreate"},
	{tcID: "TC-0006", filePath: "internal/controller/controller_test.go", testName: "TestController_ErrorTriggersRequeue"},
	{tcID: "TC-0007", filePath: "internal/controller/controller_test.go", testName: "TestLeaderElection_ExpiredLeaseTakeover"},
	{tcID: "TC-0011", filePath: "internal/controller/agent_reconciler_test.go", testName: "TestAgentReconciler_CreatesInstances"},
	{tcID: "TC-0012", filePath: "internal/controller/agent_reconciler_test.go", testName: "TestAgentReconciler_Deletion_CleansUp"},
	{tcID: "TC-0013", filePath: "internal/controller/agent_reconciler_test.go", testName: "TestAgentReconciler_SpecChange_CreatesNewRevision"},
	{tcID: "TC-0014", filePath: "internal/controller/agent_reconciler_test.go", testName: "TestAgentReconciler_SpecChange_CreatesNewRevision"},
	{tcID: "TC-0020", filePath: "internal/controller/agent_instance_reconciler_test.go", testName: "TestAgentInstanceReconciler_PendingToRunning"},
	{tcID: "TC-0021", filePath: "internal/controller/agent_reconciler_test.go", testName: "TestAgentReconciler_CreatesInstances"},
	{tcID: "TC-0022", filePath: "internal/controller/agent_instance_reconciler_test.go", testName: "TestAgentInstanceReconciler_TokenUsageReported"},
	{tcID: "TC-0023", filePath: "internal/controller/agent_job_reconciler_test.go", testName: "TestAgentJobReconciler_SucceedsWhenInstanceReady"},
	{tcID: "TC-0024", filePath: "internal/controller/agent_job_reconciler_test.go", testName: "TestAgentJobReconciler_RetriesUntilBackoffLimitThenFails"},
	{tcID: "TC-0025", filePath: "internal/controller/workflow_reconciler_test.go", testName: "TestWorkflowReconciler_MarksReadyWhenAllStepsComplete"},
	{tcID: "TC-0026", filePath: "internal/controller/workflow_reconciler_test.go", testName: "TestWorkflowReconciler_ResumesAfterControllerRestart"},
	{tcID: "TC-0027", filePath: "internal/controller/workflow_reconciler_test.go", testName: "TestWorkflowReconciler_ApprovalGatePausesUntilApproved"},
	{tcID: "TC-0028", filePath: "internal/controller/workflow_reconciler_test.go", testName: "TestWorkflowReconciler_CyclicWorkflowRespectsMaxIterations"},
	{tcID: "TC-0029", filePath: "internal/controller/workflow_reconciler_test.go", testName: "TestWorkflowReconciler_EmergentMutationAppliedWithinBudgets"},
	{tcID: "TC-0030", filePath: "internal/controller/workflow_reconciler_test.go", testName: "TestWorkflowReconciler_EmergentMutationRejectedWhenBudgetExceeded"},
	{tcID: "TC-0031", filePath: "internal/controller/prompt_reconciler_test.go", testName: "TestPromptReconciler_MarksReferencingAgentsAsDriftDetected"},
	{tcID: "TC-0032", filePath: "internal/controller/prompt_reconciler_test.go", testName: "TestPromptReconciler_ComputesHashAndVersionHistory"},
	{tcID: "TC-0033", filePath: "internal/controller/agent_instance_reconciler_test.go", testName: "TestAgentInstanceReconciler_InjectsResolvedSecretsAtDispatch"},
	{tcID: "TC-0034", filePath: "internal/admission/secretref_admission_test.go", testName: "TestSecretRefValidator_RejectsUnsupportedBackendType"},
	{tcID: "TC-0045", filePath: "internal/controller/memory_record_reconciler_test.go", testName: "TestMemoryRecord_TC0045_RedactsPII"},
	{tcID: "TC-0047", filePath: "internal/controller/tool_reconciler_test.go", testName: "TestToolReconciler_TC0047_MCPToolBecomesAvailable"},
	{tcID: "TC-0048", filePath: "internal/controller/tool_reconciler_test.go", testName: "TestToolReconciler_TC0048_DigestPinMismatchAfterEndpointChange"},
	{tcID: "TC-0049", filePath: "internal/controller/tool_reconciler_test.go", testName: "TestToolReconciler_TC0049_OpenAPIAdaptsToMCP"},
	{tcID: "TC-0050", filePath: "internal/runtime/tool_dispatcher_test.go", testName: "TestToolDispatcher_TC0050_SandboxIsolationBlocksRuntimeMemory"},
	{tcID: "TC-0051", filePath: "internal/runtime/tool_dispatcher_test.go", testName: "TestToolDispatcher_TC0051_SandboxTimeoutReturnsStructuredError"},
	{tcID: "TC-0052", filePath: "internal/runtime/tool_dispatcher_test.go", testName: "TestToolDispatcher_TC0052_TrustedTierUsesInProcessPathWithLowerLatency"},
	{tcID: "TC-0053", filePath: "internal/runtime/tool_dispatcher_test.go", testName: "TestToolDispatcher_TC0053_HumanApprovalBlocksUntilApproved"},
	{tcID: "TC-0054", filePath: "internal/runtime/tool_dispatcher_test.go", testName: "TestToolDispatcher_TC0054_HumanApprovalReturnsRejectedDecision"},
	{tcID: "TC-0055", filePath: "internal/runtime/tool_dispatcher_test.go", testName: "TestToolDispatcher_TC0055_HumanApprovalTimeoutReturnsStructuredError"},
	{tcID: "TC-0056", filePath: "internal/controller/model_reconciler_test.go", testName: "TestModelReconciler_TC0056_ValidCredentialMarksAvailable"},
	{tcID: "TC-0057", filePath: "internal/controller/model_reconciler_test.go", testName: "TestModelReconciler_TC0057_InvalidEndpointMarksUnavailable"},
	{tcID: "TC-0058", filePath: "internal/controller/model_reconciler_test.go", testName: "TestModelReconciler_TC0058_MultiProviderModelsWorkIndependently"},
	{tcID: "TC-0168", filePath: "tests/integration/tools_mcp_test.go", testName: "TestToolsMCP_HTTPHealthProbeDegradesAndRecovers"},
	{tcID: "TC-0156", filePath: "internal/namespace/resolver_test.go", testName: "TestCrossNamespace_KnowledgeBaseDeniedWithoutGrant"},
	{tcID: "TC-0157", filePath: "internal/namespace/resolver_test.go", testName: "TestCrossNamespace_KnowledgeBaseAllowedWithGrant"},
	{tcID: "TC-0159", filePath: "internal/namespace/resolver_test.go", testName: "TestCrossNamespace_KnowledgeBaseDeniedWithoutGrant"},
	{tcID: "TC-0161", filePath: "internal/namespace/resolver_test.go", testName: "TestCrossNamespace_KindMismatch"},
	{tcID: "TC-0290", filePath: "internal/controller/evaluation_reconciler_test.go", testName: "TestEvaluationReconcilerCreatesRunAndEvaluationRunCompletesAgentTarget"},
	{tcID: "TC-0291", filePath: "internal/controller/evaluation_reconciler_test.go", testName: "TestEvaluateAgentJobOutputUsesBuiltInGraders"},
	{tcID: "TC-0292", filePath: "internal/controller/evaluation_reconciler_test.go", testName: "TestEvaluationReconcilerKeepsSolutionRevisionRegressionHistory"},
	{tcID: "TC-0293", filePath: "internal/controller/solution_reconciler_test.go", testName: "TestSolutionReconciler_PromotionGateBlocksAndThenActivatesCandidateRevision"},
	{tcID: "TC-0294", filePath: "internal/k8s/dispatcher/dispatcher_test.go", testName: "TestRuntimeNetworkEgress_TC0294_ModelGatewayDisablesPublicHTTPS"},
	{tcID: "TC-0295", filePath: "internal/toolgateway/gateway_test.go", testName: "TestGatewayRetriesOnlyIdempotentTransientFailures"},
	{tcID: "TC-0296", filePath: "internal/toolgateway/kubernetes_resilience_test.go", testName: "TestKubernetesLeaseResilienceCoordinatorSharesConcurrencyAndCircuit"},
	{tcID: "TC-0299", filePath: "internal/modelgateway/server_test.go", testName: "TestServerDeniesRevokedPolicyAndChangedModelRevision"},
	{tcID: "TC-0300", filePath: "internal/k8s/dispatcher/dispatcher_test.go", testName: "TestBuildEnvVars_ModelGatewayRedactsDirectProviderConfiguration"},
	{tcID: "TC-0301", filePath: "internal/modelgateway/conformance_test.go", testName: "TestModelGatewayCrossModeConformance"},
}

// TestCompletedEpicTCMappings validates that explicit high-risk TC->test mappings remain real and discoverable.
func TestCompletedEpicTCMappings(t *testing.T) {
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatalf("find repo root: %v", err)
	}

	for _, m := range completedEpicTCMappings {
		abs := filepath.Join(repoRoot, m.filePath)
		b, readErr := os.ReadFile(abs)
		if readErr != nil {
			t.Fatalf("read mapped file for %s: %v", m.tcID, readErr)
		}
		text := string(b)
		if !strings.Contains(text, m.tcID) {
			t.Fatalf("mapped file %s does not contain TC tag %s", m.filePath, m.tcID)
		}
		if !strings.Contains(text, "func "+m.testName+"(") {
			t.Fatalf("mapped file %s does not contain test function %s", m.filePath, m.testName)
		}
	}
}

// TC-0314: Trust Focus browser acceptance runs at the required desktop and mobile viewports.
func TestTrustFocusBrowserAcceptanceContract(t *testing.T) {
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatalf("find repo root: %v", err)
	}

	assertArtifactContains(t, repoRoot, "playwright.config.js",
		`outputDir: "./test-results/playwright"`,
		`{ name: "desktop", use: { viewport: { width: 1440, height: 900 } } }`,
		`{ name: "mobile", use: { viewport: { width: 390, height: 844 } } }`,
	)
	assertArtifactContains(t, repoRoot, "cmd/kognitionctl/cmd/dashboard_assets/browser.test.js",
		`Trust Focus opens directly and preserves selected evidence across auto-refresh`,
		`expect(after).toEqual(before)`,
		`expect(geometry.clippedControls).toEqual([])`,
	)
}

// TC-0315: Trust Focus links and every Tool Gateway Grafana panel have acceptance coverage.
func TestTrustFocusObservabilityAcceptanceContract(t *testing.T) {
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatalf("find repo root: %v", err)
	}

	assertArtifactContains(t, repoRoot, "cmd/kognitionctl/cmd/dashboard_assets/browser.test.js",
		`toHaveAttribute("href", "https://observe.example.test/jaeger/trace/0123456789abcdef0123456789abcdef")`,
		`Trust Focus omits observability links when URLs are not configured`,
		`not.toContainText(/token|secret/i)`,
	)
	assertArtifactContains(t, repoRoot, "deploy/helm/kognition/tests/helm-regression-tests.ps1",
		`'"title": "Tool Gateway Operations"'`,
		`'"title": "P95 Invocation Latency"'`,
		`'"title": "Resilience Event Mix"'`,
	)
	assertArtifactContains(t, repoRoot, "e2e/kubernetes-tool-gateway-monitoring-e2e.ps1",
		`foreach ($panel in $panels)`,
		`Assert-GrafanaPanelData`,
		`returned no data through its configured datasource`,
	)
}

// TC-0312: The primary trustworthiness demo enforces clean scopes, records every claim, and verifies cleanup.
func TestTrustworthinessPrimaryPathAcceptanceContract(t *testing.T) {
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatalf("find repo root: %v", err)
	}

	assertArtifactContains(t, repoRoot, "e2e/trustworthiness-e2e.ps1",
		`$namespace = "kognition-demo-$DemoRunId"`,
		`Add-Assertion -Name "control-plane-ready"`,
		`kubernetes.io/service-name=$webhookService`,
		`--reuse-values --wait --timeout 5m`,
		`Add-Assertion -Name "preflight-clean-scope"`,
		`"tool-phase"`,
		`"memory-phase"`,
		`"model-phase"`,
		`"communication-phase"`,
		`"workflow-waiting-approval"`,
		`"approval-decision-reconciled"`,
		`"decision-audit-correlation"`,
		`function Get-AuditRunID`,
		`$_.eventType -eq "model_call" -and (Get-AuditRunID -Entry $_) -eq $DemoRunId`,
		`$_.eventType -eq "communication" -and (Get-AuditRunID -Entry $_) -eq $DemoRunId`,
		`$_.details.reasonCode -eq "target_completed" -and $_.details.execution.targetReached -eq $true`,
		`"decision-timeline"`,
		`"cleanup"`,
		`Get-OptionalProperty -InputObject $runStatus -Name "conditions" -Default @()`,
		"toolAllow = \"$DemoRunId`:tool-allow\"",
		`memoryRevoked = if ($memoryPhaseReport) { $memoryPhaseReport.invocationIds.revoked } else { $null }`,
		`Invoke-Kubectl @("api-resources", "--namespaced=true", "--verbs=list", "-o", "name")`,
		`Invoke-Kubectl @("get", ($resourceTypes -join ','), "-n", $namespace, "-o", "json")`,
		`throw "Invalid retention record: $($record.kind)/$($record.metadata.name)"`,
	)
	assertArtifactContains(t, repoRoot, "samples/trustworthiness/workflow-approval.json.tmpl",
		`"name": "prepared"`,
		`"type": "tool"`,
		`"agentRef": { "name": "tool-e2e-agent" }`,
		`"toolRef": { "name": "tool-e2e-timestamp" }`,
	)
}

// TC-0313: An unavailable Kubernetes context selects the deterministic standalone fallback before mutation.
func TestTrustworthinessFallbackAcceptanceContract(t *testing.T) {
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatalf("find repo root: %v", err)
	}

	assertArtifactContains(t, repoRoot, "e2e/trustworthiness-demo.ps1",
		`@("cluster-info")`,
		`@("get", "deployment/$ReleaseName-k8s-controller"`,
		`mutationCommands = @()`,
		`mode = "standalone-fallback"`,
		`name = "standaloneApprovalEvidence"`,
		`name = "liveRevocationEvidence"`,
		`name = "kubernetesNamespaceIsolation"; availability = "unavailable"`,
		`name = "kubernetesAdmission"; availability = "unavailable"`,
		`name = "kubernetesNetworkPolicy"; availability = "unavailable"`,
		`$script:fallbackCleanupPassed = $tempRemoved -and $fixtureStopped -and $dashboardStopped`,
	)
	assertArtifactContains(t, repoRoot, "e2e/trustworthiness-fallback-e2e.ps1",
		`$missingContext = "kognition-unavailable-$DemoRunId"`,
		`$report.preflight.mutationCommands`,
		`$requiredAssertions = @("toolAllow", "memoryAllow", "denyBeforeExecution", "standaloneApprovalEvidence", "liveRevocationEvidence", "decisionExplorer")`,
		`$requiredUnavailableClaims = @("kubernetesNamespaceIsolation", "kubernetesAdmission", "kubernetesNetworkPolicy", "productionStandaloneParity", "publication")`,
		`$report.cleanup.passed`,
	)
	assertArtifactContains(t, repoRoot, "samples/trustworthiness/runbook.md",
		`trustworthiness-demo.ps1`,
		`three sequential runs with distinct`,
		`Restart that validation sequence after a failure or manual repair.`,
		`do not count it as a passing cleanup assertion or a clean three-run sequence.`,
		`trustworthiness-fallback-e2e.ps1`,
		`standalone-evidence-only`,
	)
}

// TC-0316: A fixed rehearsal completes within 1 minute 55 seconds and returns to Trust Focus without prohibited actions.
func TestTrustworthinessStoryboardAcceptanceContract(t *testing.T) {
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatalf("find repo root: %v", err)
	}

	assertArtifactContains(t, repoRoot, "e2e/trustworthiness-storyboard-e2e.ps1",
		`$rehearsal.elapsedSeconds -le 115`,
		`$rehearsal.initialScreen -eq "Trust Focus"`,
		`$rehearsal.finalScreen -eq "Trust Focus"`,
		`@($rehearsal.observedDecisionIds).Count -eq 4`,
		`@($rehearsal.emptyStates).Count -eq 0`,
		`@($rehearsal.pageErrors).Count -eq 0`,
	)
	assertArtifactContains(t, repoRoot, "e2e/trustworthiness-browser-rehearsal.js",
		`const started = performance.now();`,
		`elapsedSeconds: Math.round((performance.now() - started) / 10) / 100`,
		`initialScreen: "Trust Focus"`,
		`finalScreen: await page.locator("#trust-focus").isVisible() ? "Trust Focus" : "unknown"`,
		`expectedIDs`,
		`pageErrors`,
	)
	assertArtifactContains(t, repoRoot, "samples/trustworthiness/runbook.md",
		`Browser Rehearsal Contract`,
		`Browser viewport`,
		`1440 x 900`,
		`At most 115 seconds`,
		`Initial and final screen`,
		`Four run-scoped Tool/Memory decisions`,
		`Empty states and page errors`,
		`Final screenshot`,
		`TC-0316 remains open`,
	)
}

func assertArtifactContains(t *testing.T, repoRoot, relativePath string, required ...string) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(repoRoot, relativePath))
	if err != nil {
		t.Fatalf("read %s: %v", relativePath, err)
	}
	for _, fragment := range required {
		if !strings.Contains(string(content), fragment) {
			t.Errorf("%s does not contain acceptance fragment %q", relativePath, fragment)
		}
	}
}
