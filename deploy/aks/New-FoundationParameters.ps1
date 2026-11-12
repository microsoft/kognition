# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

#Requires -Version 7.0
[CmdletBinding()]
param(
    [Parameter(Mandatory)][guid]$OperatorPrincipalId,
    [Parameter(Mandatory)][ValidateNotNullOrEmpty()][string[]]$ApiServerAuthorizedIpRanges,
    [ValidateSet('User', 'Group', 'ServicePrincipal')][string]$OperatorPrincipalType = 'User',
    [ValidatePattern('^[a-z0-9]+$')][string]$Location = 'eastus2',
    [ValidatePattern('^\d+\.\d+(\.\d+)?$')][string]$KubernetesVersion = '1.35',
    [ValidateSet('Standard_D4s_v5', 'Standard_D8s_v5')][string]$NodeVmSize = 'Standard_D8s_v5',
    [ValidateRange(1, 2)][int]$NodeCount = 1
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
if ($OperatorPrincipalId -eq [guid]::Empty) { throw 'OperatorPrincipalId must not be the empty GUID.' }

foreach ($cidr in $ApiServerAuthorizedIpRanges) {
    if ($cidr -notmatch '^(\d{1,3}\.){3}\d{1,3}/(\d|[12]\d|3[0-2])$') {
        throw "Invalid IPv4 CIDR: $cidr"
    }
    $addressText, $prefixText = $cidr.Split('/')
    $address = $null
    if (-not [System.Net.IPAddress]::TryParse($addressText, [ref]$address)) {
        throw "Invalid IPv4 address: $cidr"
    }
    if ([int]$prefixText -lt 24) {
        throw "Demo API access must use an explicit /24 or narrower operator range: $cidr"
    }
    $bytes = $address.GetAddressBytes()
    $hostMask = (1 -shl (32 - [int]$prefixText)) - 1
    if (($bytes[3] -band $hostMask) -ne 0) {
        throw "CIDR must use its network address: $cidr"
    }
}

[ordered]@{
    '$schema' = 'https://schema.management.azure.com/schemas/2019-04-01/deploymentParameters.json#'
    contentVersion = '1.0.0.0'
    parameters = [ordered]@{
        location = @{ value = $Location }
        operatorPrincipalId = @{ value = $OperatorPrincipalId.ToString() }
        operatorPrincipalType = @{ value = $OperatorPrincipalType }
        apiServerAuthorizedIpRanges = @{ value = @($ApiServerAuthorizedIpRanges | Sort-Object -Unique) }
        kubernetesVersion = @{ value = $KubernetesVersion }
        nodeVmSize = @{ value = $NodeVmSize }
        nodeCount = @{ value = $NodeCount }
    }
} | ConvertTo-Json -Depth 10