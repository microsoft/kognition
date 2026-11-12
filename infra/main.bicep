// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

targetScope = 'resourceGroup'

@description('Azure region. Deployment preflight must verify CPU SKU and Kubernetes availability here.')
param location string = resourceGroup().location

@description('Object ID of the user or group granted demo cluster administration. Not an application/client ID.')
param operatorPrincipalId string

@description('Type of the operator principal.')
@allowed(['User', 'Group', 'ServicePrincipal'])
param operatorPrincipalType string = 'User'

@description('Public IPv4 CIDRs allowed to reach the authenticated Kubernetes API. Never use 0.0.0.0/0.')
@minLength(1)
param apiServerAuthorizedIpRanges array

@description('Explicit regional Kubernetes version. No automatic fallback to another version.')
param kubernetesVersion string = '1.35'

@description('CPU-only node size. Validate runtime capacity and regional availability before deployment.')
@allowed(['Standard_D4s_v5', 'Standard_D8s_v5'])
param nodeVmSize string = 'Standard_D8s_v5'

@description('Demo CPU node count. One node is not highly available.')
@minValue(1)
@maxValue(2)
param nodeCount int = 1

var suffix = uniqueString(resourceGroup().id)
var tags = {
  application: 'kognition'
  environment: 'demo'
  managedBy: 'bicep'
}

module network './modules/network.bicep' = {
  name: 'network'
  params: { location: location, suffix: suffix, tags: tags }
}

module registry './modules/registry.bicep' = {
  name: 'registry'
  params: {
    location: location
    suffix: suffix
    tags: tags
    vnetId: network.outputs.vnetId
    privateEndpointSubnetId: network.outputs.privateEndpointSubnetId
  }
}

module cluster './modules/cluster.bicep' = {
  name: 'cluster'
  params: {
    location: location
    suffix: suffix
    tags: tags
    vnetName: network.outputs.vnetName
    registryName: registry.outputs.registryName
    kubernetesVersion: kubernetesVersion
    nodeVmSize: nodeVmSize
    nodeCount: nodeCount
    operatorPrincipalId: operatorPrincipalId
    operatorPrincipalType: operatorPrincipalType
    apiServerAuthorizedIpRanges: apiServerAuthorizedIpRanges
  }
}

output clusterName string = cluster.outputs.clusterName
output clusterId string = cluster.outputs.clusterId
output registryName string = registry.outputs.registryName
output registryLoginServer string = registry.outputs.loginServer
output oidcIssuerUrl string = cluster.outputs.oidcIssuerUrl
output inferenceCompute string = 'none'
output environment string = 'demo'