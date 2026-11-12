# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$scriptPath = Join-Path $PSScriptRoot "hackathon-trustworthiness-e2e.ps1"
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