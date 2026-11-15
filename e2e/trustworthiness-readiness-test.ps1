# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$scriptPath = Join-Path $PSScriptRoot "trustworthiness-e2e.ps1"
$tokens = $null
$parseErrors = $null
$scriptAst = [Management.Automation.Language.Parser]::ParseFile($scriptPath, [ref]$tokens, [ref]$parseErrors)
if ($parseErrors.Count -gt 0) {
    throw "Failed to parse $scriptPath`: $($parseErrors[0].Message)"
}

foreach ($functionName in @("Get-OptionalProperty", "Test-EndpointReady", "Assert-ServiceHasReadyEndpoint")) {
    $functionAst = $scriptAst.Find({
        param($node)
        $node -is [Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq $functionName
    }, $true)
    if ($null -eq $functionAst) {
        throw "Function $functionName was not found in $scriptPath"
    }
    . ([scriptblock]::Create($functionAst.Extent.Text))
}

$cases = @(
    @{ Name = "missing conditions"; Endpoint = [PSCustomObject]@{}; ExpectedCount = 0 }
    @{ Name = "null conditions"; Endpoint = [PSCustomObject]@{ conditions = $null }; ExpectedCount = 0 }
    @{ Name = "false readiness"; Endpoint = [PSCustomObject]@{ conditions = [PSCustomObject]@{ ready = $false } }; ExpectedCount = 0 }
    @{ Name = "true readiness"; Endpoint = [PSCustomObject]@{ conditions = [PSCustomObject]@{ ready = $true } }; ExpectedCount = 1 }
)

foreach ($case in $cases) {
    $endpointSlices = @([PSCustomObject]@{ endpoints = @($case.Endpoint) })
    if ($case.ExpectedCount -eq 0) {
        try {
            $null = Assert-ServiceHasReadyEndpoint -EndpointSlices $endpointSlices -Service "test-webhook"
            throw "Case '$($case.Name)' did not produce the readiness assertion"
        }
        catch {
            if ($_.Exception.Message -ne "Service/test-webhook has no ready EndpointSlice addresses") {
                throw "Case '$($case.Name)' produced an unexpected error: $($_.Exception.Message)"
            }
        }
        continue
    }

    $actualCount = Assert-ServiceHasReadyEndpoint -EndpointSlices $endpointSlices -Service "test-webhook"
    if ($actualCount -ne $case.ExpectedCount) {
        throw "Case '$($case.Name)' returned $actualCount ready endpoints; expected $($case.ExpectedCount)"
    }
}

Write-Host "Passed $($cases.Count) strict-mode EndpointSlice readiness cases"

$demoAst = [Management.Automation.Language.Parser]::ParseFile((Join-Path $PSScriptRoot "trustworthiness-demo.ps1"), [ref]$tokens, [ref]$parseErrors)
if ($parseErrors.Count) { throw ($parseErrors | Out-String) }
. (Join-Path $PSScriptRoot "capability-mint.ps1")
$launch = $demoAst.Find({
    param($node)
    $node -is [Management.Automation.Language.AssignmentStatementAst] -and $node.Left.Extent.Text -eq '$script:dashboardProcess'
}, $true)
if (-not $launch) { throw "The fallback dashboard launch was not found" }
$scratch = Join-Path (Split-Path $PSScriptRoot -Parent) ".demo-host-tests-$([guid]::NewGuid().ToString('N'))"
$script:dashboardProcess = $null
try {
    New-Item -ItemType Directory -Path $scratch | Out-Null
    $argumentProbe = Join-Path $scratch "argument probe.ps1"
    Set-Content -LiteralPath $argumentProbe -Value 'ConvertTo-Json -InputObject @($args) -Compress'
    $ctlBinary = (Get-Process -Id $PID).Path
    $expectedArguments = @("--run-proof", (Join-Path $scratch "report path\proof.json"), 'literal"quote', 'trailing space\', '')
    $dashboardArguments = @("-NoLogo", "-NoProfile", "-NonInteractive", "-File", $argumentProbe) + $expectedArguments
    $dashboardLog = Join-Path $scratch "dashboard output.log"
    $dashboardErrorLog = Join-Path $scratch "dashboard error.log"
    . ([scriptblock]::Create($launch.Extent.Text))
    if (-not $script:dashboardProcess.WaitForExit(10000)) { throw "Argument probe exceeded its deadline" }
    if ($script:dashboardProcess.ExitCode -ne 0) {
        throw "Fallback launch did not preserve argument boundaries: $(Get-Content -LiteralPath $dashboardErrorLog -Raw)"
    }
    $actualArguments = @(Get-Content -LiteralPath $dashboardLog -Raw | ConvertFrom-Json)
    if ($actualArguments.Count -ne $expectedArguments.Count) { throw "Fallback launch changed the argument count" }
    for ($index = 0; $index -lt $expectedArguments.Count; $index++) {
        if ($actualArguments[$index] -cne $expectedArguments[$index]) { throw "Fallback launch changed argument $index" }
    }
} finally {
    if ($script:dashboardProcess) {
        if (-not $script:dashboardProcess.HasExited) { Stop-Process -Id $script:dashboardProcess.Id -Force; $script:dashboardProcess.WaitForExit() }
        $script:dashboardProcess.Dispose()
    }
    Remove-Item -LiteralPath $scratch -Recurse -Force -ErrorAction SilentlyContinue
}
Write-Host "Fallback dashboard launch preserves report paths, quotes, trailing separators, and empty arguments."

$phaseAst = [Management.Automation.Language.Parser]::ParseFile((Join-Path $PSScriptRoot "trust-profile-phases.ps1"), [ref]$tokens, [ref]$parseErrors)
if ($parseErrors.Count) { throw ($parseErrors | Out-String) }
$helperBuild = $phaseAst.Find({
    param($node)
    $node -is [Management.Automation.Language.TryStatementAst] -and
    @($node.Body.Statements | Where-Object { $_ -is [Management.Automation.Language.PipelineAst] -and $_.Extent.Text -match '^& go build -o \$modelCapabilityHelper ' }).Count -eq 1
}, $true)
if (-not $helperBuild) { throw "The local model capability helper build was not found" }
& {
    $originalGOOS = $env:GOOS
    $originalGOARCH = $env:GOARCH
    $originalCGOEnabled = $env:CGO_ENABLED
    $builds = [Collections.Generic.List[object]]::new()
    $modelCapabilityHelper = "capability-helper-fixture.exe"
    function go {
        $global:LASTEXITCODE = 0
        if ($args[0] -eq "env") { return ($hostTarget | ConvertTo-Json -Compress) }
        if (($args -join " ") -ne "build -o capability-helper-fixture.exe ./e2e/mint-model-session-capability.go") { throw "Unexpected Go invocation" }
        $builds.Add(@{ GOOS = $env:GOOS; GOARCH = $env:GOARCH; CGO_ENABLED = $env:CGO_ENABLED })
        $global:LASTEXITCODE = $buildExitCode
    }
    try {
        foreach ($hostTarget in @(
            @{ GOHOSTOS = "windows"; GOHOSTARCH = "amd64" },
            @{ GOHOSTOS = "linux"; GOHOSTARCH = "arm64" },
            @{ GOHOSTOS = "darwin"; GOHOSTARCH = "arm64" }
        )) {
            foreach ($buildExitCode in @(0, 7)) {
                $env:GOOS = "inherited-cross-target"
                $env:GOARCH = "inherited-cross-architecture"
                $env:CGO_ENABLED = "1"
                $previousGOOS = $env:GOOS
                $previousGOARCH = $env:GOARCH
                $previousCGOEnabled = $env:CGO_ENABLED
                $failure = $null
                try { . ([scriptblock]::Create($helperBuild.Extent.Text)) } catch { $failure = $_.Exception.Message }
                if ([bool]$failure -ne ($buildExitCode -ne 0)) { throw "Native helper build did not propagate failure" }
                if ($failure -and $failure -notlike "*build Model Gateway capability helper failed*") { throw $failure }
                if ($builds[-1].GOOS -cne $hostTarget.GOHOSTOS -or $builds[-1].GOARCH -cne $hostTarget.GOHOSTARCH -or $builds[-1].CGO_ENABLED -cne "0") {
                    throw "Model capability helper must target the executing Go host, not Windows/amd64 or inherited cross-compilation settings"
                }
                if ($env:GOOS -cne $previousGOOS -or $env:GOARCH -cne $previousGOARCH -or $env:CGO_ENABLED -cne $previousCGOEnabled) { throw "Native helper build did not restore the caller's Go environment" }
            }
        }
    } finally {
        $env:GOOS = $originalGOOS
        $env:GOARCH = $originalGOARCH
        $env:CGO_ENABLED = $originalCGOEnabled
        $global:LASTEXITCODE = 0
    }
}
Write-Host "Model capability helper targets Windows/Linux/macOS host architectures and restores cross-compilation settings on success or failure."