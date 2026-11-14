# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

param(
    [string]$ReportPath = ""
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

function Assert-ConformanceEvents {
    param([object[]]$Events, [hashtable]$RequiredTests, [int]$ExitCode)

    if ($ExitCode -ne 0) { throw "Conformance test command failed with exit code $ExitCode" }
    foreach ($event in $Events) {
        if ($event.Action -in @("fail", "skip")) {
            throw "Conformance cannot pass with failed or skipped tests: $($event.Package)"
        }
    }
    foreach ($package in $RequiredTests.Keys) {
        foreach ($testName in $RequiredTests[$package]) {
            $passed = @($Events | Where-Object {
                $_.Action -eq "pass" -and $_.Package -eq $package -and
                $_.PSObject.Properties.Name -contains "Test" -and $_.Test -eq $testName
            })
            if ($passed.Count -ne 1) { throw "Missing passing conformance test: $package/$testName" }
        }
    }
}

$module = "github.com/kognition-ai/kognition"
$requiredTests = @{
    "$module/internal/toolgateway" = @("TestToolGatewayCrossModeConformance", "TestKubernetesLeaseResilienceCoordinatorKeepsActiveHolders")
    "$module/internal/modelgateway" = @("TestModelGatewayCrossModeConformance")
    "$module/internal/store/postgres" = @("TestRuntimeApprovalExecutionConformance", "TestStandaloneGatewayBackendPersistsAcrossRestart", "TestStandaloneGatewayCommandsStartWithoutKubernetes", "TestStandaloneControllerSessionPersistsAcrossRestart", "TestStandaloneToolResilienceSharesDurableState", "TestStandaloneToolResilienceKeepsActiveHolders", "TestMigrationRunnerPreservesDataAndRejectsDrift", "TestMigrationRunnerRequiresExplicitLegacyBaseline")
    "$module/internal/gatewaybackend" = @("TestOpenRejectsInvalidConfiguration", "TestStandaloneDoesNotLoadKubernetesConfiguration", "TestStandaloneDatabaseErrorsDoNotExposeCredentials")
    "$module/internal/k8s/controller" = @("TestKubernetesRuntimeApprovalExecutionConformance")
    "$module/internal/controller" = @("TestWorkflowRunReconciler_ApprovalGatePausesUntilApproved", "TestWorkflowRunReconciler_ApprovalGatedToolExecutesOnce")
    "$module/internal/memory/policygateway" = @(
        "TestServerGate3ReadyClaimsBeforeMemoryMutationAndFinalizesSuccess",
        "TestServerGate3PendingReturns202AndDeniedAuditOnly",
        "TestServerGate3AmbiguousCompletionRetryDoesNotDuplicateMutation"
    )
    "$module/internal/agentgateway" = @(
        "TestServerAgentCommunicationGate3ReadyClaimsBeforeIssuanceAndFinalizesSuccess",
        "TestServerAgentCommunicationGate3PendingBlocksExactCapabilityIssuance",
        "TestServerAgentCommunicationGate3AmbiguousCompletionDoesNotIssueAgain",
        "TestStandaloneDelegationTransportConformance",
        "TestStandaloneDelegationInvokesAuthenticatedAPIServer",
        "TestStandaloneDelegationRejectsInvalidOrigins",
        "TestStandaloneDelegationRejectsReusedRuntimeCredential"
    )
    "$module/internal/observability" = @("TestQueryTrustDecisionsReturnsAllFourPlanes")
    "$module/internal/apiserver" = @("TestStandaloneAgentServiceAuthenticatedInvocation", "TestAgentServiceCredentialCannotAccessControlPlane")
    "$module/cmd/kognition-apiserver" = @("TestBuildServerOptionsAgentServiceCredential")
}
$repoRoot = Split-Path -Parent $PSScriptRoot
if (-not $ReportPath) {
    $ReportPath = Join-Path $repoRoot "test-results/trustworthiness-parity.json"
}
$ReportPath = [IO.Path]::GetFullPath($ReportPath)
$testPattern = "^(" + (($requiredTests.Values | ForEach-Object { $_ } | ForEach-Object { [regex]::Escape($_) }) -join "|") + ")$"
$packages = @($requiredTests.Keys | Sort-Object)
$report = [ordered]@{
    schemaVersion = 1
    verificationScope = "production-contract-conformance"
    passed = $false
    deployedDemoParityVerified = $false
    recordingReady = $false
    requiredTests = $requiredTests
    completedAt = $null
}

Push-Location $repoRoot
try {
    $events = @(& go test @packages -run $testPattern -count=1 -timeout 300s -json | ForEach-Object { $_ | ConvertFrom-Json })
    $testExitCode = $LASTEXITCODE
    Assert-ConformanceEvents -Events $events -RequiredTests $requiredTests -ExitCode $testExitCode
    $report.passed = $true
}
finally {
    Pop-Location
    $report.completedAt = [DateTimeOffset]::UtcNow.ToString("o")
    New-Item -ItemType Directory -Force -Path (Split-Path -Parent $ReportPath) | Out-Null
    $report | ConvertTo-Json -Depth 8 | Set-Content -Path $ReportPath -Encoding utf8
}
Write-Host "Production contract conformance passed. Deployed demo parity and narrated recording acceptance remain separate gates. Report: $ReportPath"