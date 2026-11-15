# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

param(
    [Parameter(Mandatory = $true)][string]$DemoRunId,
    [string]$PreflightContext = "",
    [string]$ControllerNamespace = "kognition-system",
    [string]$ReleaseName = "kognition",
    [string]$ReportPath = "",
    [ValidateRange(0, 65535)][int]$FixturePort = 0,
    [ValidateRange(0, 65535)][int]$DashboardPort = 0,
    [switch]$KeepFallbackServers
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

if ($DemoRunId -notmatch '^[a-z0-9]([-a-z0-9]{0,38}[a-z0-9])?$') {
    throw "DemoRunId must be 1-40 lowercase DNS-label characters"
}

$repoRoot = Split-Path $PSScriptRoot -Parent
. (Join-Path $PSScriptRoot "capability-mint.ps1")
$workspace = "kognition-demo-$DemoRunId"
if (-not $ReportPath) {
    $ReportPath = Join-Path $repoRoot "test-results/trustworthiness-$DemoRunId.json"
}
$reportDirectory = Split-Path $ReportPath -Parent
if ($reportDirectory) {
    New-Item -ItemType Directory -Force -Path $reportDirectory | Out-Null
}

$preflightCommands = [Collections.Generic.List[string]]::new()
$fixtureProcess = $null
$dashboardProcess = $null
$fallbackCleanupPassed = $false
$fallbackFailure = $null
$startedAt = [DateTimeOffset]::UtcNow

function Invoke-ReadOnlyPreflight {
    $contextArguments = @()
    if ($PreflightContext) {
        $contextArguments = @("--context", $PreflightContext)
    }
    $checks = @(
        @("cluster-info"),
        @("get", "deployment/$ReleaseName-k8s-controller", "-n", $ControllerNamespace),
        @("get", "crd/workflows.kognition.dev"),
        @("get", "crd/approvaldecisions.kognition.dev")
    )
    foreach ($check in $checks) {
        $arguments = @($contextArguments + $check)
        $preflightCommands.Add("kubectl $($arguments -join ' ')")
        $output = & kubectl @arguments 2>&1
        if ($LASTEXITCODE -ne 0) {
            return [PSCustomObject]@{
                available = $false
                failure = "kubectl $($arguments -join ' ') failed: $($output -join ' ')"
            }
        }
    }
    return [PSCustomObject]@{ available = $true; failure = $null }
}

function Wait-ForHttp {
    param(
        [Parameter(Mandatory = $true)][string]$Uri,
        [int]$TimeoutSeconds = 45
    )

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        try {
            return Invoke-RestMethod -Uri $Uri -TimeoutSec 2
        }
        catch {
            [Threading.Tasks.Task]::Delay(250).GetAwaiter().GetResult()
        }
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Timed out waiting for $Uri"
}

function Wait-ForListeningURL {
    param(
        [Parameter(Mandatory = $true)][Diagnostics.Process]$Process,
        [Parameter(Mandatory = $true)][string]$OutputLog,
        [Parameter(Mandatory = $true)][string]$ErrorLog,
        [int]$TimeoutSeconds = 45
    )

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        if (Test-Path $OutputLog) {
            $output = Get-Content $OutputLog -Raw
            if ($output -match 'listening on (http://\S+)') {
                return $Matches[1]
            }
        }
        if ($Process.HasExited) {
            $errorOutput = if (Test-Path $ErrorLog) { (Get-Content $ErrorLog -Raw).Trim() } else { "no stderr output" }
            throw "Local server exited with code $($Process.ExitCode): $errorOutput"
        }
        [Threading.Tasks.Task]::Delay(100).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Timed out waiting for local server PID $($Process.Id) to report its listening URL"
}

function Test-HttpUnavailable {
    param([Parameter(Mandatory = $true)][string]$Uri)

    try {
        Invoke-WebRequest -Uri $Uri -TimeoutSec 2 | Out-Null
        return $false
    }
    catch {
        return $true
    }
}

function Remove-DirectoryWithRetry {
    param(
        [Parameter(Mandatory = $true)][string]$Path,
        [int]$TimeoutSeconds = 10
    )

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        Remove-Item -Recurse -Force $Path -ErrorAction SilentlyContinue
        if (-not (Test-Path $Path)) {
            return $true
        }
        [Threading.Tasks.Task]::Delay(100).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    return $false
}

function Find-Decision {
    param(
        [Parameter(Mandatory = $true)][object[]]$Items,
        [Parameter(Mandatory = $true)][string]$ID
    )

    return @($Items | Where-Object { $_.id -eq $ID }) | Select-Object -First 1
}

function Get-FixtureCounter {
    param(
        [Parameter(Mandatory = $true)][object]$Counters,
        [Parameter(Mandatory = $true)][string]$Name
    )

    $property = $Counters.PSObject.Properties[$Name]
    if ($null -eq $property) {
        return 0
    }
    return [int]$property.Value
}

function Invoke-StandaloneFallback {
    $tempDirectory = Join-Path $repoRoot ".tmp/trustworthiness-$DemoRunId"
    if (Test-Path $tempDirectory) {
        throw "Fallback scope already exists: $tempDirectory"
    }
    New-Item -ItemType Directory -Force -Path $tempDirectory | Out-Null
    $fixtureBinary = Join-Path $tempDirectory "trustworthiness-fixture.exe"
    $ctlBinary = Join-Path $tempDirectory "kognitionctl.exe"
    $fixtureLog = Join-Path $tempDirectory "fixture.log"
    $fixtureErrorLog = Join-Path $tempDirectory "fixture.err.log"
    $dashboardLog = Join-Path $tempDirectory "dashboard.log"
    $dashboardErrorLog = Join-Path $tempDirectory "dashboard.err.log"
    $fixtureURL = $null
    $dashboardURL = $null
    $fallbackSucceeded = $false

    try {
        & go build -o $fixtureBinary ./cmd/kognition-trustworthiness-fixture
        if ($LASTEXITCODE -ne 0) { throw "building the trustworthiness fixture failed" }
        & go build -o $ctlBinary ./cmd/kognitionctl
        if ($LASTEXITCODE -ne 0) { throw "building kognitionctl failed" }

        $fixtureArguments = @("--listen", "127.0.0.1:$FixturePort", "--workspace", $workspace, "--run-id", $DemoRunId)
        $script:fixtureProcess = Start-Process -FilePath $fixtureBinary -ArgumentList @($fixtureArguments | ForEach-Object { ConvertTo-NativeProcessArgument $_ }) -RedirectStandardOutput $fixtureLog -RedirectStandardError $fixtureErrorLog -PassThru
        $fixtureURL = Wait-ForListeningURL -Process $script:fixtureProcess -OutputLog $fixtureLog -ErrorLog $fixtureErrorLog
        Wait-ForHttp -Uri "$fixtureURL/healthz" | Out-Null

        $dashboardArguments = @("--mode", "standalone", "--server", $fixtureURL, "--namespace", $workspace, "dashboard", "--bind", "127.0.0.1", "--port", "$DashboardPort", "--run-proof", $ReportPath)
        $script:dashboardProcess = Start-Process -FilePath $ctlBinary -ArgumentList @($dashboardArguments | ForEach-Object { ConvertTo-NativeProcessArgument $_ }) -RedirectStandardOutput $dashboardLog -RedirectStandardError $dashboardErrorLog -PassThru
        $dashboardURL = Wait-ForListeningURL -Process $script:dashboardProcess -OutputLog $dashboardLog -ErrorLog $dashboardErrorLog
        Wait-ForHttp -Uri "$dashboardURL/api/meta" | Out-Null

        Invoke-RestMethod -Method Post -Uri "$fixtureURL/fixture/grant" | Out-Null
        Invoke-RestMethod -Method Post -Uri "$fixtureURL/fixture/operations/tool/weather-read" | Out-Null
        Invoke-RestMethod -Method Post -Uri "$fixtureURL/fixture/operations/memory/research-notes" | Out-Null
        $beforeBlockedAttempts = Invoke-RestMethod -Uri "$fixtureURL/fixture/counters"
        $denyResponse = Invoke-WebRequest -Method Post -Uri "$fixtureURL/fixture/operations/tool/admin-write" -SkipHttpErrorCheck
        $afterDenial = Invoke-RestMethod -Uri "$fixtureURL/fixture/counters"
        $approvalEvidence = Invoke-RestMethod -Method Post -Uri "$fixtureURL/fixture/approval"
        Invoke-RestMethod -Method Delete -Uri "$fixtureURL/fixture/grant" | Out-Null
        $revokedResponse = Invoke-WebRequest -Method Post -Uri "$fixtureURL/fixture/operations/memory/research-notes" -SkipHttpErrorCheck
        $afterRevocation = Invoke-RestMethod -Uri "$fixtureURL/fixture/counters"

        $queryURL = "$dashboardURL/api/trust-decisions?namespace=$workspace&runId=$DemoRunId&limit=20"
        $result = Wait-ForHttp -Uri $queryURL
        $items = @($result.items)
        $toolAllow = Find-Decision -Items $items -ID "$DemoRunId`:tool-allow"
        $memoryAllow = Find-Decision -Items $items -ID "$DemoRunId`:memory-allow"
        $toolDeny = Find-Decision -Items $items -ID "$DemoRunId`:tool-deny"
        $revoked = Find-Decision -Items $items -ID "$DemoRunId`:memory-revoked"
        $toolAllowCount = Get-FixtureCounter -Counters $beforeBlockedAttempts -Name "tool:weather-read"
        $memoryAllowCount = Get-FixtureCounter -Counters $beforeBlockedAttempts -Name "memory:research-notes"
        $deniedCount = Get-FixtureCounter -Counters $afterDenial -Name "tool:admin-write"
        $memoryAfterRevocationCount = Get-FixtureCounter -Counters $afterRevocation -Name "memory:research-notes"

        $assertions = @(
            [PSCustomObject]@{ name = "toolAllow"; passed = ($null -ne $toolAllow -and $toolAllow.outcome -eq "allowed" -and $toolAllow.execution.targetReached -and $toolAllowCount -eq 1); evidence = "$DemoRunId`:tool-allow; target count=1" },
            [PSCustomObject]@{ name = "memoryAllow"; passed = ($null -ne $memoryAllow -and $memoryAllow.outcome -eq "allowed" -and $memoryAllow.execution.targetReached -and $memoryAllowCount -eq 1); evidence = "$DemoRunId`:memory-allow; target count=1" },
            [PSCustomObject]@{ name = "denyBeforeExecution"; passed = ($denyResponse.StatusCode -eq 403 -and $null -ne $toolDeny -and $toolDeny.outcome -eq "denied" -and $toolDeny.execution.targetReached -eq $false -and $deniedCount -eq 0); evidence = "$DemoRunId`:tool-deny; denied target count=0" },
            [PSCustomObject]@{ name = "standaloneApprovalEvidence"; passed = ($approvalEvidence.status -eq "approved" -and $approvalEvidence.kind -eq "StandaloneApprovalEvidence"); evidence = "$($approvalEvidence.name) (not a Trust Decision or Kubernetes ApprovalDecision)" },
            [PSCustomObject]@{ name = "liveRevocationEvidence"; passed = ($revokedResponse.StatusCode -eq 403 -and $null -ne $revoked -and $revoked.outcome -eq "revoked" -and $revoked.reasonCode -eq "grant_revoked" -and $revoked.execution.targetReached -eq $false -and $memoryAfterRevocationCount -eq $memoryAllowCount); evidence = "$DemoRunId`:memory-revoked; target count unchanged after grant deletion" },
            [PSCustomObject]@{ name = "decisionExplorer"; passed = ($items.Count -eq 4 -and @($items | Where-Object { $_.plane -notin @('tool', 'memory') }).Count -eq 0); evidence = "$($items.Count) executable Tool/Memory decisions returned through $queryURL" }
        )
        $failedAssertions = @($assertions | Where-Object { -not $_.passed })
        if ($failedAssertions.Count -gt 0) {
            throw "Standalone fallback assertions failed: $($failedAssertions.name -join ', ')"
        }

        $report = [PSCustomObject]@{
            schemaVersion = 1
            demoRunId = $DemoRunId
            mode = "standalone-fallback"
            passed = $true
            verificationScope = "local-tool-memory-fixture"
            deployedDemoParityVerified = $false
            recordingReady = $false
            startedAt = $startedAt.ToString("o")
            completedAt = [DateTimeOffset]::UtcNow.ToString("o")
            preflight = [PSCustomObject]@{ available = $false; commands = @($preflightCommands); mutationCommands = @(); failure = $preflight.failure }
            assertions = $assertions
            runProof = [PSCustomObject]@{
                schemaVersion = 1
                scenario = "Local Tool and Memory fixture walkthrough"
                protectedAction = "Fixture access checks; no workflow publication"
                mode = "standalone-fallback"
                reportRef = Split-Path $ReportPath -Leaf
            }
            claims = @(
                [PSCustomObject]@{ name = "scopedToolAuthority"; availability = "fixture-evidence"; measurement = "allowed decision, targetReached=true, and target count incremented" },
                [PSCustomObject]@{ name = "scopedMemoryAuthority"; availability = "fixture-evidence"; measurement = "allowed decision, targetReached=true, and target count incremented" },
                [PSCustomObject]@{ name = "denialBeforeExecution"; availability = "fixture-evidence"; measurement = "HTTP 403, denied decision, and target count unchanged" },
                [PSCustomObject]@{ name = "approval"; availability = "standalone-evidence-only"; measurement = "static fixture response only; no human interaction, durable gate, or production approval" },
                [PSCustomObject]@{ name = "productionStandaloneParity"; availability = "unavailable"; measurement = "fixture does not run the production API server, PostgreSQL, or workflow controller" },
                [PSCustomObject]@{ name = "publication"; availability = "unavailable"; measurement = "no publication target is invoked" },
                [PSCustomObject]@{ name = "revocation"; availability = "fixture-evidence"; measurement = "grant deleted, retry returned HTTP 403/grant_revoked, and target count unchanged" },
                [PSCustomObject]@{ name = "decisionExplorer"; availability = "available"; measurement = "four executable Tool/Memory decisions returned through dashboard proxy" },
                [PSCustomObject]@{ name = "kubernetesNamespaceIsolation"; availability = "unavailable"; measurement = "standalone fallback" },
                [PSCustomObject]@{ name = "kubernetesAdmission"; availability = "unavailable"; measurement = "standalone fallback" },
                [PSCustomObject]@{ name = "kubernetesNetworkPolicy"; availability = "unavailable"; measurement = "standalone fallback" }
            )
            dashboardURL = "$dashboardURL/?view=trust&runId=$DemoRunId&focus=1"
            retainedRuntime = if ($KeepFallbackServers) {
                [PSCustomObject]@{
                    fixtureProcessId = $script:fixtureProcess.Id
                    dashboardProcessId = $script:dashboardProcess.Id
                    tempDirectory = $tempDirectory
                }
            } else { $null }
            cleanup = [PSCustomObject]@{ passed = $false; deferred = [bool]$KeepFallbackServers }
        }
        $fallbackSucceeded = $true
        return $report
    }
    catch {
        $script:fallbackFailure = $_.Exception.Message
        throw
    }
    finally {
        if (-not $KeepFallbackServers -or -not $fallbackSucceeded) {
            if ($null -ne $script:dashboardProcess -and -not $script:dashboardProcess.HasExited) {
                Stop-Process -Id $script:dashboardProcess.Id -Force
                $script:dashboardProcess.WaitForExit()
            }
            if ($null -ne $script:fixtureProcess -and -not $script:fixtureProcess.HasExited) {
                Stop-Process -Id $script:fixtureProcess.Id -Force
                $script:fixtureProcess.WaitForExit()
            }
            $tempRemoved = Remove-DirectoryWithRetry -Path $tempDirectory
            $fixtureStopped = $null -eq $fixtureURL -or (Test-HttpUnavailable -Uri "$fixtureURL/healthz")
            $dashboardStopped = $null -eq $dashboardURL -or (Test-HttpUnavailable -Uri "$dashboardURL/api/meta")
            $script:fallbackCleanupPassed = $tempRemoved -and $fixtureStopped -and $dashboardStopped
        }
    }
}

$preflightCommands.Add("Get-Command kubectl")
if (-not (Get-Command kubectl -ErrorAction SilentlyContinue)) {
    $preflight = [PSCustomObject]@{ available = $false; failure = "kubectl is unavailable" }
}
else {
    $preflight = Invoke-ReadOnlyPreflight
}

if ($preflight.available) {
    & (Join-Path $PSScriptRoot "trustworthiness-e2e.ps1") -DemoRunId $DemoRunId -KubernetesContext $PreflightContext -ControllerNamespace $ControllerNamespace -ReleaseName $ReleaseName -ReportPath $ReportPath
    exit $LASTEXITCODE
}

try {
    $report = Invoke-StandaloneFallback
    if (-not $KeepFallbackServers) {
        $report.cleanup.passed = $fallbackCleanupPassed
        if (-not $fallbackCleanupPassed) {
            throw "Standalone fallback cleanup verification failed"
        }
    }
    $report | ConvertTo-Json -Depth 20 | Set-Content -Path $ReportPath -Encoding utf8
}
catch {
    $failureReport = [PSCustomObject]@{
        schemaVersion = 1
        demoRunId = $DemoRunId
        mode = "standalone-fallback"
        passed = $false
        verificationScope = "local-tool-memory-fixture"
        deployedDemoParityVerified = $false
        recordingReady = $false
        startedAt = $startedAt.ToString("o")
        completedAt = [DateTimeOffset]::UtcNow.ToString("o")
        preflight = [PSCustomObject]@{ available = $false; commands = @($preflightCommands); mutationCommands = @(); failure = $preflight.failure }
        assertions = @()
        runProof = [PSCustomObject]@{
            schemaVersion = 1
            scenario = "Local Tool and Memory fixture walkthrough"
            protectedAction = "Fixture access checks; no workflow publication"
            mode = "standalone-fallback"
            reportRef = Split-Path $ReportPath -Leaf
        }
        claims = @()
        cleanup = [PSCustomObject]@{ passed = $fallbackCleanupPassed; deferred = $false }
        error = if ($fallbackFailure) { $fallbackFailure } else { $_.Exception.Message }
    }
    $failureReport | ConvertTo-Json -Depth 20 | Set-Content -Path $ReportPath -Encoding utf8
    throw
}
Write-Host "Local Tool/Memory fixture passed. Report: $ReportPath"
Write-Host "Production standalone parity, human approval, and publication are not verified."
if ($KeepFallbackServers) {
    Write-Host "Trust Focus: $($report.dashboardURL)"
    Write-Host "Stop fixture PID $($fixtureProcess.Id) and dashboard PID $($dashboardProcess.Id) after inspection."
}