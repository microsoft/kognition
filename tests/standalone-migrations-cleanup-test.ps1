# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$migrationScript = Join-Path $PSScriptRoot 'standalone-migrations.ps1'
$originalDatabaseURL = $env:DATABASE_URL
$originalBaseline = $env:KOGNITION_MIGRATION_BASELINE
$originalMemoryDatabaseURL = $env:KOGNITION_TEST_MEMORY_DATABASE_URL
$originalLocation = Get-Location

function docker {
    $global:LASTEXITCODE = 0
    switch ($args[0]) {
        'ps' { 'migration-test-postgres' }
        'inspect' { '{"migration-test-network":{}}' }
        'build' { }
        'run' { throw 'injected migrator failure' }
        'exec' {
            $sql = @($input) -join "`n"
            if ($sql -match '^DROP DATABASE ') { throw 'injected database cleanup failure' }
        }
        default { throw 'Unexpected Docker command in cleanup test' }
    }
}

try {
    foreach ($initialState in @('configured', 'absent')) {
        $env:DATABASE_URL = if ($initialState -eq 'configured') { 'original-test-url' } else { $null }
        $env:KOGNITION_MIGRATION_BASELINE = if ($initialState -eq 'configured') { 'original-test-baseline' } else { $null }
        $env:KOGNITION_TEST_MEMORY_DATABASE_URL = if ($initialState -eq 'configured') { 'original-memory-test-url' } else { $null }
        $expectedDatabaseURL = $env:DATABASE_URL
        $expectedBaseline = $env:KOGNITION_MIGRATION_BASELINE
        $expectedMemoryDatabaseURL = $env:KOGNITION_TEST_MEMORY_DATABASE_URL
        Push-Location ([IO.Path]::GetTempPath())
        try {
            $expectedLocation = (Get-Location).Path
            $failure = $null
            try { & $migrationScript } catch { $failure = $_.Exception.Message }
            if ($failure -ne 'injected database cleanup failure') { throw "Expected cleanup failure, received: $failure" }
            if ($env:DATABASE_URL -ne $expectedDatabaseURL) { throw 'DATABASE_URL was not restored after cleanup failure' }
            if ($env:KOGNITION_MIGRATION_BASELINE -ne $expectedBaseline) { throw 'Migration baseline was not restored after cleanup failure' }
            if ($env:KOGNITION_TEST_MEMORY_DATABASE_URL -ne $expectedMemoryDatabaseURL) { throw 'Memory database URL was not restored after cleanup failure' }
            if ((Get-Location).Path -ne $expectedLocation) { throw 'Working directory was not restored after cleanup failure' }
        } finally {
            if ((Get-Location).Path -ne $expectedLocation) { Pop-Location }
            Pop-Location
        }
    }
    Write-Host 'PASS: failed database cleanup restores configured and absent environment values and caller location.'
} finally {
    $env:DATABASE_URL = $originalDatabaseURL
    $env:KOGNITION_MIGRATION_BASELINE = $originalBaseline
    $env:KOGNITION_TEST_MEMORY_DATABASE_URL = $originalMemoryDatabaseURL
    Set-Location $originalLocation
}