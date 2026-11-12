# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$repoRoot = Split-Path $PSScriptRoot -Parent
$tokens = $null
$parseErrors = $null
$syntax = [System.Management.Automation.Language.Parser]::ParseFile(
    (Join-Path $repoRoot 'make.ps1'), [ref]$tokens, [ref]$parseErrors)
if ($parseErrors.Count -ne 0) { throw 'make.ps1 has syntax errors' }
$definition = $syntax.Find({
    param($node)
    $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq 'Target-Migrate'
}, $true)
if ($null -eq $definition) { throw 'Target-Migrate is missing' }
. ([scriptblock]::Create($definition.Extent.Text))
$script:migrationCalls = 0
$Migrations = @('legacy-migration.sql')
function Resolve-Tool { param($Name) $Name }
function Invoke-Native {
    param($Command, $Arguments)
    if ($Command -ne 'go' -or ($Arguments -join ' ') -ne 'run ./cmd/kognition-migrate') {
        throw 'PowerShell migration entry point bypasses the migration ledger'
    }
    $script:migrationCalls++
}
$originalPassword = $env:PGPASSWORD
try {
    $env:PGPASSWORD = 'preserve-existing-test-value'
    Target-Migrate
    if ($script:migrationCalls -ne 1) { throw 'Expected one migration runner invocation' }
    if ($env:PGPASSWORD -ne 'preserve-existing-test-value') { throw 'Migration entry point changed PGPASSWORD' }
} finally {
    $env:PGPASSWORD = $originalPassword
}
$makefile = Get-Content (Join-Path $repoRoot 'Makefile') -Raw
$recipe = [regex]::Match($makefile, '(?m)^migrate:\r?\n((?:\t[^\r\n]*\r?\n)+)').Groups[1].Value.Trim()
if ($recipe -ne '$(GOCMD) run ./cmd/kognition-migrate') {
    throw 'Makefile migration entry point bypasses the migration ledger'
}
Write-Host 'PASS: both migration entry points use the ledger-aware runner and preserve caller credentials.'