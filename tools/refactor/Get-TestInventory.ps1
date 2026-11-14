# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

<#
.SYNOPSIS
Generates the file-level test ownership manifest used by the repository refactor (RP01).

.DESCRIPTION
Enumerates tracked test files, counts static test functions and skip sites, and assigns a
proposed owner by path. Core files whose content mentions a provider are flagged for review
because their assertions may belong with that provider. The result is a static inventory:
parameterized and sub-tests are not expanded, and nothing is executed.
#>
[CmdletBinding()]
param(
    [string]$OutputPath = (Join-Path $PSScriptRoot '..\..\docs\development-plans\refactoring\refactor-test-ownership.csv')
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$root = (git -C $PSScriptRoot rev-parse --show-toplevel).Trim()
Push-Location $root
try {
    $files = git ls-files | Where-Object {
        $_ -match '(_test\.go|(^|/)test_[^/]+\.py|_test\.py|\.(test|spec)\.(js|ts)|(^|/)[^/]*tests?[^/]*\.ps1)$' -and $_ -notmatch '(^|/)(node_modules|vendor)/'
    }

    function Get-Owner([string]$path) {
        switch -Regex ($path) {
            '^samples/' { return 'samples', 'path' }
            '^tools/recording/' { return 'samples', 'path' }
            '^e2e/(trustworthiness|trust-profile)' { return 'samples', 'path' }
            'azuresearch|azure-|/azure|foundry|^deploy/aks/|^infra/' { return 'integrations/azure', 'path' }
            'bedrock|(^|[/_-])s3([/_.-]|$)|(^|[/_-])aws' { return 'integrations/aws', 'path' }
            'anthropic' { return 'integrations/anthropic', 'path' }
            'mem0' { return 'integrations/mem0', 'path' }
            default { return 'core', 'default' }
        }
    }

    $providerPattern = [regex]'(?i)azure|foundry|bedrock|anthropic|mem0|\bs3\b|aws'
    $rows = foreach ($path in $files) {
        $text = [IO.File]::ReadAllText((Join-Path $root $path))
        switch -Regex ($path) {
            '\.go$' {
                $runner = 'go'
                $tests = [regex]::Matches($text, '(?m)^func (Test|Fuzz)\w+\(').Count
                $skips = [regex]::Matches($text, '\.(Skip|Skipf|SkipNow)\(|testing\.Short\(\)').Count
                $tag = [regex]::Match($text, '(?m)^//go:build (.+?)\r?$').Groups[1].Value
                break
            }
            '\.py$' {
                $runner = 'pytest'
                $tests = [regex]::Matches($text, '(?m)^\s*(async )?def test_').Count
                $skips = [regex]::Matches($text, 'pytest\.(skip|mark\.skip|importorskip)|@unittest\.skip').Count
                $tag = ''
                break
            }
            '\.(js|ts)$' {
                $runner = 'playwright'
                $tests = [regex]::Matches($text, '(?m)^\s*(test|it)(\.\w+)?\(').Count
                $skips = [regex]::Matches($text, '\.skip\(|test\.fixme\(').Count
                $tag = ''
                break
            }
            default {
                $runner = 'powershell'
                $tests = 0
                $skips = [regex]::Matches($text, '(?i)\bskip(ped)?\b').Count
                $tag = ''
            }
        }
        $owner, $basis = Get-Owner $path
        $mentions = ''
        if ($owner -eq 'core') {
            $found = $providerPattern.Matches($text) | ForEach-Object { $_.Value.ToLowerInvariant() } | Sort-Object -Unique
            if ($found) { $mentions = ($found -join ';'); $basis = 'review' }
        }
        [pscustomobject]@{
            Path = $path; Runner = $runner; Tag = $tag; Tests = $tests; SkipSites = $skips
            ProposedOwner = $owner; Basis = $basis; ProviderMentions = $mentions
        }
    }

    $rows | Sort-Object Path | Export-Csv -LiteralPath $OutputPath -NoTypeInformation -Encoding utf8
    $rows | Group-Object ProposedOwner, Basis | Sort-Object Name | ForEach-Object {
        '{0,-34} files={1,4} tests={2,5} skips={3,3}' -f $_.Name, $_.Count, ($_.Group | Measure-Object Tests -Sum).Sum, ($_.Group | Measure-Object SkipSites -Sum).Sum
    }
}
finally {
    Pop-Location
}
