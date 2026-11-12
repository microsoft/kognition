# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

#Requires -Version 7.0
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$PythonExecutable,
    [string]$EnvironmentFile = (Join-Path $PSScriptRoot '..\..\.env'),
    [string]$ResultsPath
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$repository = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
. (Join-Path $repository 'samples\azure-settings.ps1')
$settings = Get-AzureSampleSettings -Path $EnvironmentFile
if (-not (Test-Path -LiteralPath $PythonExecutable -PathType Leaf)) {
    throw 'PythonExecutable must identify an existing interpreter with the runtime LangChain dependencies.'
}
$required = @(
    'MS_FOUNDRY_INFERENCE_ENDPOINT', 'MS_FOUNDRY_MODEL', 'MS_FOUNDRY_INVOCATION_CONTRACT',
    'MS_FOUNDRY_API_VERSION', 'MS_FOUNDRY_EMBEDDING_MODEL', 'MS_FOUNDRY_EMBEDDING_API_VERSION',
    'AZURE_TENANT_ID', 'AZURE_CLIENT_ID', 'AZURE_CLIENT_SECRET'
)
foreach ($name in $required) {
    if (-not $settings[$name]) { throw "Live profile verification requires $name in the environment file." }
}
$previous = @{}
$keys = $required + @('KOGNITION_FOUNDRY_LIVE_TESTS', 'KOGNITION_FOUNDRY_LIVE_PYTHON')
foreach ($name in $keys) { $previous[$name] = [Environment]::GetEnvironmentVariable($name, 'Process') }
$directory = (Get-Location).Path
try {
    foreach ($name in $required) { [Environment]::SetEnvironmentVariable($name, $settings[$name], 'Process') }
    $env:KOGNITION_FOUNDRY_LIVE_TESTS = 'true'
    $env:KOGNITION_FOUNDRY_LIVE_PYTHON = (Resolve-Path -LiteralPath $PythonExecutable).Path
    Set-Location -LiteralPath $repository
    $output = @(& go test -json -tags=integration .\internal\modelgateway -run '^TestFoundryInvocationProfilesLive$' -count=1 -timeout=8m)
    $exitCode = $LASTEXITCODE
    if ($ResultsPath) {
        [IO.File]::WriteAllLines([IO.Path]::GetFullPath($ResultsPath), [string[]]$output, [Text.UTF8Encoding]::new($false))
    }
    $events = @($output | ForEach-Object { $_ | ConvertFrom-Json })
    foreach ($event in $events) {
        if ($event.PSObject.Properties['Output']) { Write-Host $event.Output.TrimEnd() }
    }
    if ($exitCode -ne 0) { throw "Live invocation-profile tests failed with exit code $exitCode; see retained test output." }
    $requiredTests = @(
        'CPIChat', 'GatewayChat', 'GatewayToolRoundTrip', 'GatewayStructuredOutput',
        'NativeRuntime', 'PythonSDKThroughGateway', 'ProfiledEmbeddings',
        'RejectInvalidSettingsBeforeProvider'
    )
    foreach ($name in $requiredTests) {
        $test = "TestFoundryInvocationProfilesLive/$name"
        if (@($events | Where-Object { $_.PSObject.Properties['Test'] -and $_.Test -eq $test -and $_.Action -eq 'pass' }).Count -ne 1) {
            throw "Live acceptance is incomplete: $test did not pass exactly once."
        }
    }
    if (@($events | Where-Object { $_.Action -in @('skip', 'fail') }).Count -ne 0) {
        throw 'Live acceptance contains a skipped or failed test.'
    }
    Write-Output 'PASS: all eight live profile scenarios completed without skips. Azure resources and Kubernetes were not modified.'
} finally {
    foreach ($name in $keys) { [Environment]::SetEnvironmentVariable($name, $previous[$name], 'Process') }
    Set-Location -LiteralPath $directory
}
