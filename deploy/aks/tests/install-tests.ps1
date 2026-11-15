# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

#Requires -Version 7.0
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$installer = Join-Path $PSScriptRoot '../Install-RuntimeBase.ps1'
$global:aksFreshTest = @{ Calls = 0; Fail = $false; ValuesPath = '' }

function Assert-True([bool]$Condition, [string]$Message) {
    if (-not $Condition) { throw $Message }
}

function go {
    $arguments = @($args)
    $global:aksFreshTest.Calls++
    Assert-True (($arguments[0..2] -join ' ') -eq 'run ./tools/kognition-install fresh') 'AKS must use the shared fresh installer'
    Assert-True ($arguments -contains '--exclusive') 'Installation must require exclusive writes'
    foreach ($pair in @(
        @('--namespace', 'demo-system'),
        @('--release', 'demo'),
        @('--workload-namespaces', 'team-a'),
        @('--context', 'disposable-demo')
    )) {
        Assert-True ($arguments[[Array]::IndexOf($arguments, $pair[0]) + 1] -eq $pair[1]) "Wrong target option $($pair[0])"
    }
    $path = $arguments[[Array]::IndexOf($arguments, '--values') + 1]
    $global:aksFreshTest.ValuesPath = $path
    $values = Get-Content -LiteralPath $path -Raw | ConvertFrom-Json -AsHashtable
    Assert-True ($values.k8sController.image.digest -eq ('sha256:' + ('a' * 64))) 'Controller image must be digest pinned'
    Assert-True ($values.k8sController.managedNamespaces[0] -eq 'team-a') 'Only the selected workload namespace is enrolled'
    Assert-True ($values.runtimeAuth.create -and -not $values.runtimeAuth.existingSecret) 'The fresh renderer must create runtime credentials once'
    Assert-True (-not $values.postgres.auth.existingSecret) 'The fresh installer must prepare its new database credentials'
    Assert-True ($values.admissionGuard.stdioIsolationProfile -eq 'cilium') 'Cilium isolation must remain enabled'
    $global:LASTEXITCODE = $(if ($global:aksFreshTest.Fail) { 1 } else { 0 })
}

function kubectl { throw 'The AKS wrapper must not mutate the cluster before the shared installer checks freshness.' }
function helm { throw 'The AKS wrapper must not bypass the shared installer.' }
function az { throw 'Application installation must not change Azure infrastructure.' }
function docker { throw 'Image publication must be a separate operation.' }

try {
    $parameters = @{
        Kubeconfig = 'reviewed.kubeconfig'; Context = 'disposable-demo'; ChartPackage = 'chart.tgz'
        Namespace = 'demo-system'; WorkloadNamespace = 'team-a'; Release = 'demo'
        ImageRegistry = 'demotest.azurecr.io'; ImageTag = 'build-123'
        ControllerImageDigest = 'sha256:' + ('a' * 64); Exclusive = $true
    }
    & $installer @parameters
    Assert-True ($global:aksFreshTest.Calls -eq 1) 'The wrapper must run one shared installation'
    Assert-True (-not (Test-Path -LiteralPath $global:aksFreshTest.ValuesPath)) 'Temporary values must be removed after capture'
    $global:aksFreshTest.Fail = $true
    $failed = $false
    try { & $installer @parameters } catch { $failed = $true }
    Assert-True $failed 'A failed fresh installer must not be reported as ready'
    Assert-True (-not (Test-Path -LiteralPath $global:aksFreshTest.ValuesPath)) 'Temporary values must be removed after failure'
    $parameters.Exclusive = $false
    $before = $global:aksFreshTest.Calls
    $failed = $false
    try { & $installer @parameters } catch { $failed = $true }
    Assert-True ($failed -and $global:aksFreshTest.Calls -eq $before) 'Missing exclusive-write confirmation must fail before invoking tools'
    Write-Output 'PASS: AKS fresh installation delegates target selection, credentials and controller startup to the checked installer.'
} finally {
    Remove-Variable aksFreshTest -Scope Global
    foreach ($name in @('go', 'kubectl', 'helm', 'az', 'docker')) {
        Remove-Item -LiteralPath "Function:$name"
    }
}
