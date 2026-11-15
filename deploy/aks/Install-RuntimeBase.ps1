# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

#Requires -Version 7.0
[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$Kubeconfig,
    [Parameter(Mandatory)][ValidateNotNullOrEmpty()][string]$Context,
    [Parameter(Mandatory)][string]$ChartPackage,
    [Parameter(Mandatory)][ValidatePattern('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')][string]$Namespace,
    [Parameter(Mandatory)][ValidatePattern('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')][string]$WorkloadNamespace,
    [Parameter(Mandatory)][ValidatePattern('^[a-z0-9]+\.azurecr\.io$')][string]$ImageRegistry,
    [Parameter(Mandatory)][ValidatePattern('^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$')][string]$ImageTag,
    [Parameter(Mandatory)][ValidatePattern('^sha256:[a-f0-9]{64}$')][string]$ControllerImageDigest,
    [ValidatePattern('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')][string]$Release = 'kognition',
    [string]$StateDirectory = '.local/kognition-install',
    [switch]$Exclusive
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
if (-not $Exclusive) { throw 'Use -Exclusive after confirming no other writer targets the selected installation namespaces.' }
$values = & (Join-Path $PSScriptRoot 'New-RuntimeValues.ps1') `
    -ImageRegistry $ImageRegistry -ImageTag $ImageTag -ReleaseFullname $Release `
    -WorkloadNamespace $WorkloadNamespace -ControllerImageDigest $ControllerImageDigest -GenerateCredentials
$valuesPath = [IO.Path]::GetTempFileName()
try {
    $values | Set-Content -LiteralPath $valuesPath -Encoding utf8
    & (Join-Path $PSScriptRoot '../../bootstrap.ps1') `
        -Kubeconfig $Kubeconfig -Context $Context -Chart $ChartPackage -Values $valuesPath `
        -Namespace $Namespace -WorkloadNamespace $WorkloadNamespace -Release $Release `
        -StateDirectory $StateDirectory -Exclusive
} finally {
    Remove-Item -LiteralPath $valuesPath
}
