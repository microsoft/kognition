# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

param([string]$DemoRunId = "tc0316-$([Guid]::NewGuid().ToString('N').Substring(0, 8))")

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$repoRoot = Split-Path $PSScriptRoot -Parent
$reportPath = Join-Path $repoRoot "test-results/trustworthiness-$DemoRunId.json"
$rehearsalPath = Join-Path $repoRoot "test-results/trustworthiness-rehearsal-$DemoRunId.json"
$screenshotPath = Join-Path $repoRoot "test-results/trustworthiness-rehearsal-$DemoRunId.png"
$missingContext = "kognition-unavailable-$DemoRunId"
$report = $null

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
    & (Join-Path $PSScriptRoot "trustworthiness-demo.ps1") -DemoRunId $DemoRunId -PreflightContext $missingContext -ReportPath $reportPath -KeepFallbackServers
    if ($LASTEXITCODE -ne 0) {
        throw "Demo runner exited with code $LASTEXITCODE"
    }

    $report = Get-Content $reportPath -Raw | ConvertFrom-Json
    Assert-True ($report.demoRunId -eq $DemoRunId) "Demo report run ID does not match the rehearsal"
    Assert-True ($report.mode -eq "standalone-fallback") "Expected a standalone fallback rehearsal"
    Assert-True ($report.verificationScope -eq "local-tool-memory-fixture") "Fallback must identify fixture-only evidence"
    Assert-True (-not $report.deployedDemoParityVerified -and -not $report.recordingReady) "Fixture cannot certify production parity or recording readiness"
    Assert-True ($report.cleanup.deferred) "Fallback servers were not retained for browser observation"
    Assert-True ($null -ne $report.retainedRuntime) "Retained runtime metadata is missing"

    & node (Join-Path $PSScriptRoot "trustworthiness-browser-rehearsal.js") $report.dashboardURL $DemoRunId $rehearsalPath $screenshotPath
    if ($LASTEXITCODE -ne 0) {
        throw "Browser rehearsal exited with code $LASTEXITCODE"
    }
    $rehearsal = Get-Content $rehearsalPath -Raw | ConvertFrom-Json
    Assert-True ($rehearsal.verificationScope -eq "technical-fallback-traversal") "Browser report must identify technical-only traversal"
    Assert-True (-not $rehearsal.narratedRehearsalVerified -and -not $rehearsal.recordingReady) "Browser traversal cannot attest a narrated recording"
    Assert-True ($rehearsal.elapsedSeconds -le 115) "Measured rehearsal duration $($rehearsal.elapsedSeconds) seconds exceeds 1 minute 55 seconds"
    Assert-True ($rehearsal.initialScreen -eq "Trust Focus") "Browser rehearsal did not start on Trust Focus"
    Assert-True ($rehearsal.finalScreen -eq "Trust Focus") "Browser rehearsal did not finish on Trust Focus"
    Assert-True ($rehearsal.connection.Trim() -eq "Live") "Trust Focus was not live at the final observation"
    Assert-True ($rehearsal.runProof.scenario -eq $report.runProof.scenario) "Run Proof scenario does not match the demo report"
    Assert-True ($rehearsal.runProof.protectedAction -eq $report.runProof.protectedAction) "Run Proof protected action does not match the demo report"
    Assert-True ($rehearsal.runProof.mode -eq $report.runProof.mode) "Run Proof mode does not match the demo report"
    Assert-True ($rehearsal.runProof.status -eq "Passed") "Run Proof did not show a passed result"
    Assert-True ($rehearsal.runProof.assertions -eq "$(@($report.assertions | Where-Object passed).Count)/$(@($report.assertions).Count) passed") "Run Proof assertion totals do not match the demo report"
    Assert-True (@($rehearsal.observedDecisionIds).Count -eq 4) "Browser did not inspect all executable decisions"
    Assert-True (@($rehearsal.emptyStates).Count -eq 0) "Browser observed an unexplained empty Trust Focus panel"
    Assert-True (@($rehearsal.pageErrors).Count -eq 0) "Browser reported page errors"
    Assert-True (Test-Path $screenshotPath) "Final Trust Focus screenshot is missing"

    Write-Host "Technical fallback traversal passed for $DemoRunId in $($rehearsal.elapsedSeconds) seconds. TC-0316 remains open until the presenter attests the narrated take and recording checklist."
}
finally {
    if ($null -ne $report -and $null -ne $report.retainedRuntime) {
        foreach ($processId in @($report.retainedRuntime.dashboardProcessId, $report.retainedRuntime.fixtureProcessId)) {
            Stop-Process -Id $processId -Force -ErrorAction SilentlyContinue
            Wait-Process -Id $processId -Timeout 10 -ErrorAction SilentlyContinue
        }
        Remove-Item -Path $report.retainedRuntime.tempDirectory -Recurse -Force -ErrorAction SilentlyContinue
        $runningProcessIds = @(@($report.retainedRuntime.dashboardProcessId, $report.retainedRuntime.fixtureProcessId) | Where-Object { Get-Process -Id $_ -ErrorAction SilentlyContinue })
        if ($runningProcessIds.Count -gt 0 -or (Test-Path $report.retainedRuntime.tempDirectory)) {
            throw "Browser rehearsal cleanup failed; running process IDs: $($runningProcessIds -join ', ')"
        }
    }
    Remove-Item $reportPath -Force -ErrorAction SilentlyContinue
}