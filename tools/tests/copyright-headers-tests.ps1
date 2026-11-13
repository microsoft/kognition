# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

$ErrorActionPreference = 'Stop'
$tool = Join-Path $PSScriptRoot '../copyright-headers.ps1'
$errors = $null
$ast = [Management.Automation.Language.Parser]::ParseFile($tool, [ref]$null, [ref]$errors)
if ($errors) { throw 'Header tool syntax is invalid' }
$function = @($ast.FindAll({ param($node)
    $node -is [Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq 'Add-SourceCopyrightHeader'
}, $true))
if ($function.Count -ne 1) { throw 'Header function missing' }
. ([scriptblock]::Create($function[0].Extent.Text))
$cases = @(
    @{extension='.go';content="//go:build linux`n`npackage example`n"},
    @{extension='.go';content=([string][char]0xfeff + "package example`n")},
    @{extension='.py';content="#!/usr/bin/env python3`n# coding: utf-8`nprint('example')`n"},
    @{extension='.ps1';content="param()`r`nWrite-Output 'example'`r`n"},
    @{extension='.sh';content="#!/bin/sh`necho example`n"},
    @{extension='.js';content="#!/usr/bin/env node`nconsole.log('example');`n"},
    @{extension='.mjs';content="export const name = 'example';`n"},
    @{extension='.sql';content="SELECT 1;`n"},
    @{extension='.css';content="body { color: black; }`n"},
    @{extension='.yaml';content="apiVersion: v1`nkind: ConfigMap`n"},
    @{extension='.cmd';content="@echo off`r`necho example`r`n"},
    @{extension='.html';content="<!doctype html>`n<html></html>`n"},
    @{extension='.svg';content="<?xml version=`"1.0`"?>`n<svg/>`n"},
    @{extension='.tpl';content="{{ define `"example`" }}value{{ end }}`n"},
    @{extension='.bicep';content="targetScope = 'resourceGroup'`n"},
    @{extension='.requirements';content="-r requirements.txt`nhttpx>=0.28.0`n"},
    @{extension='.helmtxt';content="Namespace: {{ .Release.Namespace }}`n"},
    @{extension='.ini';content="[pytest]`ntimeout = 120`n"},
    @{extension='.env';content="KOGNITION_OTEL_TRACES_ENABLED=false`n"},
    @{extension='.dockerignore';content=".env`n"},
    @{extension='.dockerfile';content="# syntax=docker/dockerfile:1`nFROM scratch`n"},
    @{extension='.makefile';content="build:`n`tgo build ./...`n"}
)
foreach ($case in $cases) {
    $updated = Add-SourceCopyrightHeader $case.content $case.extension
    if ((Add-SourceCopyrightHeader $updated $case.extension) -cne $updated) { throw 'Header insertion is not idempotent' }
    $copyright = $updated.IndexOf('Copyright (c) Microsoft Corporation.', [StringComparison]::Ordinal)
    $license = $updated.IndexOf('Licensed under the MIT License.', [StringComparison]::Ordinal)
    if ($copyright -lt 0 -or $license -lt $copyright) { throw 'Canonical header missing' }
    $body = $case.content
    if ($body.StartsWith('#!') -or $body.StartsWith('# syntax=') -or $body.StartsWith('<?xml')) {
        if (($updated -split "`n")[0] -cne ($body -split "`n")[0]) { throw 'Required first line moved' }
    }
    if ($case.extension -eq '.py' -and ($updated -split "`n")[1] -cne '# coding: utf-8') { throw 'Python encoding declaration moved' }
    if ($updated.Length -le $body.Length) { throw 'Header not inserted' }
    if ($case.extension -eq '.bicep' -and -not $updated.StartsWith('// Copyright')) { throw 'Invalid Bicep comment' }
    if ($case.extension -eq '.requirements' -and -not $updated.StartsWith('# Copyright')) { throw 'Invalid requirements comment' }
    if ($case.extension -eq '.helmtxt' -and -not $updated.StartsWith('{{/* Copyright')) { throw 'Header would leak into Helm output' }
    $headerStart = $updated.LastIndexOf("`n", $copyright)
    $headerStart = if ($headerStart -lt 0) { 0 } else { $headerStart + 1 }
    if ($updated.StartsWith([string][char]0xfeff, [StringComparison]::Ordinal) -and $headerStart -eq 0) { $headerStart = 1 }
    $original = $updated.Remove($headerStart, $updated.Length - $body.Length)
    if ($original -cne $body) { throw "Source body changed for $($case.extension)" }
}
$rejected = $false
try { Add-SourceCopyrightHeader '// Copyright third party' '.go' | Out-Null } catch { $rejected = $true }
if (-not $rejected) { throw 'Existing third-party notice changed' }
@{syntheticCases=$cases.Count;idempotence='PASS';specialFirstLines='PASS';existingNotices='PASS'} | ConvertTo-Json