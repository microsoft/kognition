#!/usr/bin/env pwsh
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

#Requires -Version 7.0
<#
.SYNOPSIS
    Install a new Kubernetes release, verify admission and migrations, then start the controller.
.DESCRIPTION
    Requires a prepared cluster, Kognition CRDs, cert-manager, Cilium and available
    images. Existing releases are refused. Does not reset a cluster, build images,
    provision providers, inject .env credentials or install samples.
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$Kubeconfig,
    [Parameter(Mandatory)][string]$Chart,
    [Parameter(Mandatory)][string]$Values,
    [Parameter(Mandatory)][string[]]$WorkloadNamespace,
    [string]$Context = '',
    [string]$Namespace = 'kognition-system',
    [string]$Release = 'kognition',
    [string]$AuthorityNamespace = 'kognition-installation-authority',
    [string]$StateDirectory = '.local/kognition-install',
    [ValidateRange(1,100)][int]$Replicas = 1,
    [switch]$Exclusive
)

$ErrorActionPreference = 'Stop'
if (-not $Exclusive) {
    throw 'Use -Exclusive after confirming no other installer, GitOps or workload writer targets these namespaces during installation.'
}
$resolve = { param($Path) $ExecutionContext.SessionState.Path.GetUnresolvedProviderPathFromPSPath($Path) }
$arguments = @(
    'run', './tools/kognition-install', 'fresh',
    '--kubeconfig', (& $resolve $Kubeconfig),
    '--chart', (& $resolve $Chart),
    '--values', (& $resolve $Values),
    '--workload-namespaces', ($WorkloadNamespace -join ','),
    '--namespace', $Namespace,
    '--release', $Release,
    '--authority-namespace', $AuthorityNamespace,
    '--state-dir', (& $resolve $StateDirectory),
    '--replicas', [string]$Replicas,
    '--exclusive'
)
if ($Context) { $arguments += @('--context', $Context) }
Push-Location $PSScriptRoot
try {
    & go @arguments
    if ($LASTEXITCODE -ne 0) { throw "Fresh installation failed (exit $LASTEXITCODE). Preserve the installation state for recovery." }
} finally {
    Pop-Location
}
