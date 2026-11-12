# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

#Requires -Version 7.0
[CmdletBinding()]
param(
    [Parameter(Mandatory)][ValidateNotNullOrEmpty()][string]$Context,
    [Parameter(Mandatory)][ValidatePattern('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')][string]$Namespace,
    [Parameter(Mandatory)][ValidatePattern('^[a-z0-9]+\.azurecr\.io$')][string]$ImageRegistry,
    [Parameter(Mandatory)][ValidatePattern('^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$')][string]$ImageTag,
    [ValidatePattern('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')][string]$Release = 'kognition',
    [ValidateRange(1,60)][int]$TimeoutMinutes = 15,
    [ValidatePattern('^(\d{1,3}\.){3}\d{1,3}/\d{1,2}$')][string]$KubeletProbeIngressCidr = '10.240.0.0/22'
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $false
if ($Release.Length -gt 53) { throw 'Helm release names must not exceed 53 characters.' }
if ($Namespace.Length -gt 63) { throw 'Kubernetes namespace names must not exceed 63 characters.' }

$chart = Join-Path $PSScriptRoot '../helm/kognition'
$databaseSecret = "${Release}-database"
$runtimeSecret = "${Release}-runtime-auth"
$values = & (Join-Path $PSScriptRoot 'New-RuntimeValues.ps1') -ImageRegistry $ImageRegistry -ImageTag $ImageTag -ReleaseFullname $Release -DatabaseSecret $databaseSecret -RuntimeSecret $runtimeSecret -KubeletProbeIngressCidr $KubeletProbeIngressCidr
$valuesPath = [IO.Path]::GetTempFileName()
$duration = "${TimeoutMinutes}m"
$kubeArguments = @('--context', $Context, '--namespace', $Namespace)
try {
    $values | Set-Content -LiteralPath $valuesPath -Encoding utf8
    & helm lint $chart -f $valuesPath
    if ($LASTEXITCODE -ne 0) { throw 'Runtime chart validation failed; no cluster changes were made.' }

    $registryName = $ImageRegistry.Substring(0, $ImageRegistry.Length - '.azurecr.io'.Length)
    $null = & (Join-Path $PSScriptRoot 'Sync-ThirdPartyImages.ps1') -RegistryName $registryName -ImageName pgvector, busybox

    & kubectl @kubeArguments wait --for=condition=Established crd/certificates.cert-manager.io crd/issuers.cert-manager.io --timeout=30s
    if ($LASTEXITCODE -ne 0) { throw 'cert-manager Certificate and Issuer CRDs must be installed before the runtime base; no credentials were created.' }

    $namespaceResource = & kubectl @kubeArguments get namespace $Namespace --ignore-not-found -o name 2>$null
    if ($LASTEXITCODE -ne 0) { throw 'Cannot inspect the target namespace in the explicitly selected context.' }
    if (-not $namespaceResource) {
        $null = & kubectl @kubeArguments create namespace $Namespace
        if ($LASTEXITCODE -ne 0) { throw 'Cannot create the target namespace in the explicitly selected context.' }
    }

    $null = & (Join-Path $PSScriptRoot 'Initialize-Secrets.ps1') -Context $Context -Namespace $Namespace -ReleaseFullname $Release -DatabaseSecret $databaseSecret -RuntimeSecret $runtimeSecret

    & helm upgrade --install $Release $chart --kube-context $Context --namespace $Namespace -f $valuesPath --timeout $duration --wait=false
    if ($LASTEXITCODE -ne 0) { throw 'Helm installation or migration hook failed. Resources are retained for diagnosis; no automatic rollback was attempted.' }

    & kubectl @kubeArguments rollout status "statefulset/${Release}-postgres" "--timeout=$duration"
    if ($LASTEXITCODE -ne 0) { throw 'PostgreSQL did not become ready.' }
    foreach ($component in @('k8s-controller', 'intent-gateway', 'memory-service', 'memory-policy-gateway', 'tool-gateway', 'model-gateway', 'agent-communication-gateway')) {
        & kubectl @kubeArguments rollout status "deployment/${Release}-${component}" "--timeout=$duration"
        if ($LASTEXITCODE -ne 0) { throw "Runtime component did not become ready: $component" }
    }
} finally {
    Remove-Item -LiteralPath $valuesPath -ErrorAction SilentlyContinue
}
Write-Output "Runtime base ready in context '$Context', namespace '$Namespace'. Inference endpoints and sample resources still require configuration and validation."