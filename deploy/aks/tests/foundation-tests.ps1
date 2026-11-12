# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

#Requires -Version 7.0
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$root = (Resolve-Path (Join-Path $PSScriptRoot '../../..')).Path
$generator = Join-Path $PSScriptRoot '../New-FoundationParameters.ps1'

function Assert-True([bool]$Condition, [string]$Message) {
    if (-not $Condition) { throw $Message }
}

function Get-TemplateResources($Template) {
    foreach ($resource in $Template.resources) {
        $resource
        if ($resource.type -eq 'Microsoft.Resources/deployments') {
            Get-TemplateResources $resource.properties.template
        }
    }
}

$compiled = & az bicep build --file (Join-Path $root 'infra/main.bicep') --stdout
if ($LASTEXITCODE -ne 0) { throw "Bicep build failed: $LASTEXITCODE" }
$template = ($compiled -join "`n") | ConvertFrom-Json -AsHashtable -Depth 100
Assert-True ($template.outputs.inferenceCompute.value -eq 'none') 'Foundation does not deploy an inference service'
$resources = @(Get-TemplateResources $template)
$clusters = @($resources | Where-Object type -EQ 'Microsoft.ContainerService/managedClusters')
Assert-True ($clusters.Count -eq 1) 'Exactly one AKS cluster is deployed'
$cluster = $clusters[0]
Assert-True ($cluster.sku.tier -eq 'Free') 'Demo uses the Free control-plane tier'
Assert-True $cluster.properties.disableLocalAccounts 'Local cluster credentials must be disabled'
Assert-True $cluster.properties.aadProfile.enableAzureRBAC 'Entra Azure RBAC is required'
Assert-True $cluster.properties.oidcIssuerProfile.enabled 'OIDC issuer is required'
Assert-True $cluster.properties.securityProfile.workloadIdentity.enabled 'Workload Identity is required'
Assert-True ($cluster.properties.apiServerAccessProfile.authorizedIPRanges -eq "[parameters('apiServerAuthorizedIpRanges')]") 'API allowlist must be wired'
Assert-True ($cluster.properties.agentPoolProfiles.Count -eq 1) 'Demo uses one shared CPU pool'
Assert-True (-not $cluster.properties.agentPoolProfiles[0].enableNodePublicIP) 'Nodes must not have public IPs'
Assert-True ($cluster.properties.networkProfile.networkPluginMode -eq 'overlay') 'CNI Overlay is required'
Assert-True ($cluster.properties.networkProfile.networkDataplane -eq 'cilium') 'Cilium is required'
Assert-True ($template.parameters.nodeCount.defaultValue -eq 1) 'Default is one demo node'
Assert-True ($template.parameters.nodeCount.maxValue -eq 2) 'Demo count is bounded'
Assert-True ($template.parameters.nodeVmSize.defaultValue -eq 'Standard_D8s_v5') 'Default has 32 GiB CPU memory'
Assert-True (($template.parameters.nodeVmSize.allowedValues -join ',') -eq 'Standard_D4s_v5,Standard_D8s_v5') 'Only explicitly supported CPU sizes are allowed'
Assert-True ($template.parameters.apiServerAuthorizedIpRanges.minLength -eq 1) 'An explicit API allowlist is mandatory'

$registries = @($resources | Where-Object type -EQ 'Microsoft.ContainerRegistry/registries')
Assert-True ($registries.Count -eq 1) 'Exactly one registry is deployed'
$registry = $registries[0]
Assert-True ($registry.sku.name -eq 'Premium') 'Private ACR requires Premium'
Assert-True ($registry.properties.publicNetworkAccess -eq 'Disabled') 'Registry must not expose a public data endpoint'
Assert-True (-not $registry.properties.adminUserEnabled) 'Registry admin credentials must be disabled'
Assert-True (-not $registry.properties.anonymousPullEnabled) 'Registry anonymous pulls must be disabled'
Assert-True ($registry.properties.roleAssignmentMode -eq 'LegacyRegistryPermissions') 'AcrPull needs registry-level RBAC mode'
Assert-True (@($resources | Where-Object type -EQ 'Microsoft.Network/privateEndpoints').Count -eq 1) 'Registry private endpoint must exist'
Assert-True (@($resources | Where-Object type -EQ 'Microsoft.Network/privateEndpoints/privateDnsZoneGroups').Count -eq 1) 'Registry endpoint must register its private DNS records'
Assert-True (@($resources | Where-Object type -EQ 'Microsoft.Network/privateDnsZones/virtualNetworkLinks').Count -eq 1) 'Registry DNS must be linked to the cluster VNet'
Assert-True (@($resources | Where-Object { $_.type -match 'Microsoft\.(DBforPostgreSQL|CognitiveServices|Search|Storage)/' }).Count -eq 0) 'Foundation must not silently provision optional managed services'
$roles = @($resources | Where-Object type -EQ 'Microsoft.Authorization/roleAssignments')
Assert-True ($roles.Count -eq 4) 'Network, kubelet, registry and operator role loop must be present'
$roleTemplate = $roles | ConvertTo-Json -Depth 30 -Compress
Assert-True ($roleTemplate.Contains('4abbcc35-e782-43d8-92c5-2d3f1bd2253f')) 'Operator must be able to retrieve cluster credentials'
Assert-True ($roleTemplate.Contains('b1ff04bb-8a4e-4dc4-8eb5-8693973ce19b')) 'Operator must administer the Kubernetes data plane through Azure RBAC'
Assert-True (-not $roleTemplate.Contains('0ab0b1a8-8aac-4efd-b8c2-3ee1fb270be8')) 'ARM Cluster Admin does not grant Azure RBAC data-plane access'
Assert-True ($cluster.dependsOn.Count -ge 3) 'Cluster must wait for prerequisite identity permissions'

$valid = @{ OperatorPrincipalId = '11111111-1111-1111-1111-111111111111'; ApiServerAuthorizedIpRanges = @('203.0.113.4/32') }
$parameters = & $generator @valid | ConvertFrom-Json -AsHashtable
Assert-True ($parameters.parameters.nodeVmSize.value -eq 'Standard_D8s_v5') 'Generator preserves CPU demo defaults'
Assert-True ($parameters.parameters.apiServerAuthorizedIpRanges.value.Count -eq 1) 'Single CIDR remains an array'
foreach ($parameterName in $template.parameters.Keys) {
    Assert-True ($parameters.parameters.ContainsKey($parameterName)) "Generator must populate $parameterName"
}
Assert-True ($parameters.parameters.Count -eq $template.parameters.Count) 'Generator must not emit unknown template parameters'
$subnetParameters = & $generator -OperatorPrincipalId $valid.OperatorPrincipalId -ApiServerAuthorizedIpRanges @('203.0.113.0/24', '203.0.113.0/24') -NodeVmSize Standard_D4s_v5 -NodeCount 2 | ConvertFrom-Json -AsHashtable
Assert-True ($subnetParameters.parameters.apiServerAuthorizedIpRanges.value.Count -eq 1) 'Repeated ranges are deduplicated'

$invalidCases = @(
    @{ ApiServerAuthorizedIpRanges = @('0.0.0.0/0') }
    @{ ApiServerAuthorizedIpRanges = @('203.0.113.1/24') }
    @{ ApiServerAuthorizedIpRanges = @('203.0.113.999/32') }
    @{ ApiServerAuthorizedIpRanges = @('::/0') }
    @{ ApiServerAuthorizedIpRanges = @('203.0.113.4') }
    @{ ApiServerAuthorizedIpRanges = @() }
    @{ OperatorPrincipalId = [guid]::Empty }
    @{ NodeVmSize = 'Standard_NC4as_T4_v3' }
    @{ NodeCount = 3 }
)
foreach ($invalid in $invalidCases) {
    $arguments = $valid.Clone()
    foreach ($key in $invalid.Keys) { $arguments[$key] = $invalid[$key] }
    $rejected = $false
    try { & $generator @arguments | Out-Null } catch { $rejected = $true }
    Assert-True $rejected "Unsafe parameter case must fail: $($invalid | ConvertTo-Json -Compress)"
}
Write-Output 'PASS: Bicep build (exit 0), CPU demo resources, Entra/identity/network/registry controls, parameter contract and 9 invalid configurations'