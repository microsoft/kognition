# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest
$parseErrors = $null
$tokens = $null
$source = Join-Path $PSScriptRoot "trustworthiness-parity-test.ps1"
$ast = [Management.Automation.Language.Parser]::ParseFile($source, [ref]$tokens, [ref]$parseErrors)
if ($parseErrors.Count -gt 0) { throw "Parity gate syntax errors" }
$function = $ast.Find({ param($node) $node -is [Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq "Assert-ConformanceEvents" }, $true)
. ([scriptblock]::Create($function.Extent.Text))

$required = @{ "example/standalone" = @("TestApproval"); "example/kubernetes" = @("TestApproval") }
$passing = @(
    [pscustomobject]@{ Action = "pass"; Package = "example/standalone"; Test = "TestApproval" },
    [pscustomobject]@{ Action = "pass"; Package = "example/kubernetes"; Test = "TestApproval" },
    [pscustomobject]@{ Action = "pass"; Package = "example/kubernetes" }
)
Assert-ConformanceEvents -Events $passing -RequiredTests $required -ExitCode 0
$rejectedCases = @(
    @{ Name = "missing mode"; Events = @($passing[0]); ExitCode = 0 },
    @{ Name = "no tests"; Events = @(); ExitCode = 0 },
    @{ Name = "nonzero exit"; Events = $passing; ExitCode = 1 },
    @{ Name = "skipped subtest"; Events = $passing + [pscustomobject]@{ Action = "skip"; Package = "example/standalone"; Test = "TestApproval/database" }; ExitCode = 0 },
    @{ Name = "failed subtest"; Events = $passing + [pscustomobject]@{ Action = "fail"; Package = "example/kubernetes"; Test = "TestApproval/replay" }; ExitCode = 0 }
)
foreach ($case in $rejectedCases) {
    $rejected = $false
    try { Assert-ConformanceEvents -Events $case.Events -RequiredTests $required -ExitCode $case.ExitCode }
    catch { $rejected = $true }
    if (-not $rejected) { throw "Gate accepted $($case.Name)" }
}
Write-Host "Parity gate regression checks passed: one valid report and five rejected reports."