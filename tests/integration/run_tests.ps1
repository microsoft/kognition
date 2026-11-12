# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# Run Kognition integration tests
# Usage: .\tests\integration\run_tests.ps1

param(
    [switch]$Verbose,
    [string]$Run = ""
)

$ErrorActionPreference = "Stop"
$root = git rev-parse --show-toplevel 2>$null
if (-not $root) { $root = Split-Path -Parent (Split-Path -Parent $PSScriptRoot) }
Push-Location $root

try {
    $testArgs = @("-tags", "integration", "-count=1", "-timeout=10m")
    if ($Verbose) { $testArgs += "-v" }
    if ($Run) { $testArgs += "-run"; $testArgs += $Run }
    $testArgs += "./tests/integration/..."

    Write-Host "Running: go test $($testArgs -join ' ')" -ForegroundColor Cyan
    & go test @testArgs
    if ($LASTEXITCODE -ne 0) {
        Write-Host "`nFAILED (exit code $LASTEXITCODE)" -ForegroundColor Red
        exit $LASTEXITCODE
    }
    Write-Host "`nALL TESTS PASSED" -ForegroundColor Green
} finally {
    Pop-Location
}
