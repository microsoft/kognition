# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

param([string]$DemoRunId = "tc0313-$([Guid]::NewGuid().ToString('N').Substring(0, 8))")

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$repoRoot = Split-Path $PSScriptRoot -Parent
$reportPath = Join-Path $repoRoot "test-results/trustworthiness-$DemoRunId.json"
$missingContext = "kognition-unavailable-$DemoRunId"

function Assert-True {
    param(
        [Parameter(Mandatory = $true)][bool]$Condition,
        [Parameter(Mandatory = $true)][string]$Message
    )

    if (-not $Condition) {
        throw $Message
    }
}

try {
    & (Join-Path $PSScriptRoot "trustworthiness-demo.ps1") -DemoRunId $DemoRunId -PreflightContext $missingContext -ReportPath $reportPath
    if ($LASTEXITCODE -ne 0) {
        throw "Fallback runner exited with code $LASTEXITCODE"
    }

    $report = Get-Content $reportPath -Raw | ConvertFrom-Json
    Assert-True ($report.mode -eq "standalone-fallback") "Expected standalone fallback mode"
    Assert-True ($report.verificationScope -eq "local-tool-memory-fixture") "Fallback must identify its fixture-only scope"
    Assert-True ($report.deployedDemoParityVerified -eq $false -and $report.recordingReady -eq $false) "Fixture cannot certify production parity or recording readiness"
    Assert-True ($report.preflight.available -eq $false) "Unavailable context unexpectedly passed preflight"
    Assert-True (@($report.preflight.commands).Count -ge 1) "No read-only preflight command was recorded"
    Assert-True (@($report.preflight.mutationCommands).Count -eq 0) "Preflight recorded a mutation command"
    Assert-True ($report.preflight.failure -match [Regex]::Escape($missingContext)) "Preflight failure does not identify the unavailable context"

    $requiredAssertions = @("toolAllow", "memoryAllow", "denyBeforeExecution", "standaloneApprovalEvidence", "liveRevocationEvidence", "decisionExplorer")
    foreach ($name in $requiredAssertions) {
        $assertion = @($report.assertions | Where-Object { $_.name -eq $name })
        Assert-True ($assertion.Count -eq 1 -and $assertion[0].passed) "Assertion '$name' is absent or failed"
    }

    $requiredUnavailableClaims = @("kubernetesNamespaceIsolation", "kubernetesAdmission", "kubernetesNetworkPolicy", "productionStandaloneParity", "publication")
    foreach ($name in $requiredUnavailableClaims) {
        $claim = @($report.claims | Where-Object { $_.name -eq $name })
        Assert-True ($claim.Count -eq 1 -and $claim[0].availability -eq "unavailable") "Production claim '$name' is not explicitly unavailable"
    }
    $approvalClaim = @($report.claims | Where-Object { $_.name -eq "approval" })
    Assert-True ($approvalClaim.Count -eq 1 -and $approvalClaim[0].availability -eq "standalone-evidence-only") "Fallback approval overstates Kubernetes evidence"
    Assert-True ($report.cleanup.passed) "Fallback cleanup did not pass"
    Assert-True (-not (Test-Path (Join-Path $repoRoot ".tmp/trustworthiness-$DemoRunId"))) "Fallback temp directory remains"

    Write-Host "TC-0313 passed for $DemoRunId"
}
finally {
    Remove-Item $reportPath -Force -ErrorAction SilentlyContinue
}