# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

#Requires -Version 7.0
[CmdletBinding()]
param(
    [Parameter(Mandatory)][ValidatePattern('^[a-z0-9]+\.azurecr\.io$')][string]$ImageRegistry,
    [Parameter(Mandatory)][ValidatePattern('^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$')][string]$ImageTag,
    [ValidatePattern('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')][string]$ReleaseFullname = 'kognition',
    [ValidatePattern('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')][string]$DatabaseSecret = 'kognition-database',
    [ValidatePattern('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')][string]$RuntimeSecret = 'kognition-runtime-auth',
    [ValidatePattern('^(\d{1,3}\.){3}\d{1,3}/\d{1,2}$')][string]$KubeletProbeIngressCidr = '10.240.0.0/22'
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
if ($ImageTag -in @('latest', 'dev', 'main')) { throw 'Use a versioned build tag, not latest, dev or main.' }
if ($DatabaseSecret -eq $RuntimeSecret) { throw 'Database and runtime Secrets must have distinct names.' }

$values = [ordered]@{
    mode = 'kubernetes'
    fullnameOverride = $ReleaseFullname
    global = @{
        imageRegistry = $ImageRegistry
        thirdPartyImageRegistry = $ImageRegistry
        aksAdmissionsEnforcerOptOut = $true
    }
    runtimeAuth = @{ create = $false; existingSecret = $RuntimeSecret }
    postgres = @{
        enabled = $true
        auth = @{ existingSecret = $DatabaseSecret; password = '' }
        storage = @{ size = '10Gi'; storageClass = 'managed-csi' }
    }
    memoryService = @{ enabled = $true; backendType = 'pgvector' }
    memoryPolicyGateway = @{ enabled = $true }
    toolGateway = @{ enabled = $true }
    modelGateway = @{ enabled = $true }
    agentCommunicationGateway = @{ enabled = $true }
    kbPgvector = @{ enabled = $false }
    kbIngester = @{ enabled = $true }
    k8sController = @{
        agentImage = "${ImageRegistry}/kognition-agent-runtime:${ImageTag}"
    }
    intentGateway = @{ enabled = $true }
    ingress = @{ enabled = $false }
    networkPolicy = @{
        enabled = $true
        kubeletProbeIngressCIDRs = @($KubeletProbeIngressCidr)
        ciliumHostProbeIngress = $true
    }
    podDisruptionBudget = @{ enabled = $false }
}
foreach ($component in @('k8sController', 'intentGateway', 'memoryService', 'memoryPolicyGateway', 'toolGateway', 'modelGateway', 'agentCommunicationGateway', 'kbIngester')) {
    $values[$component].image = @{ tag = $ImageTag; pullPolicy = 'IfNotPresent' }
    if ($component -ne 'kbIngester') { $values[$component].replicaCount = 1 }
}
$values | ConvertTo-Json -Depth 10