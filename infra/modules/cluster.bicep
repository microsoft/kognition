// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

@description('Azure region.')
param location string
@description('Deterministic resource group suffix.')
param suffix string
@description('Resource tags.')
param tags object
@description('Existing VNet from network module.')
param vnetName string
@description('Existing private registry from registry module.')
param registryName string
@description('Explicit Kubernetes version.')
param kubernetesVersion string
@description('CPU node size.')
@allowed(['Standard_D4s_v5', 'Standard_D8s_v5'])
param nodeVmSize string
@description('Demo node count.')
@minValue(1)
@maxValue(2)
param nodeCount int
@description('Operator object ID.')
param operatorPrincipalId string
@description('Operator principal type.')
@allowed(['User', 'Group', 'ServicePrincipal'])
param operatorPrincipalType string
@description('Public IPv4 CIDRs allowed to reach the authenticated API.')
@minLength(1)
param apiServerAuthorizedIpRanges array

resource vnet 'Microsoft.Network/virtualNetworks@2024-05-01' existing = { name: vnetName }
resource subnet 'Microsoft.Network/virtualNetworks/subnets@2024-05-01' existing = {
  parent: vnet
  name: 'nodes'
}
resource registry 'Microsoft.ContainerRegistry/registries@2025-11-01' existing = { name: registryName }

resource controlPlane 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = {
  name: 'id-aks-control-${suffix}'
  location: location
  tags: tags
}
resource kubelet 'Microsoft.ManagedIdentity/userAssignedIdentities@2023-01-31' = {
  name: 'id-aks-kubelet-${suffix}'
  location: location
  tags: tags
}

resource networkRole 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(subnet.id, controlPlane.id, 'NetworkContributor')
  scope: subnet
  properties: {
    principalId: controlPlane.properties.principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', '4d97b98b-1d4f-4787-a291-c67834d212e7')
  }
}
resource identityRole 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(kubelet.id, controlPlane.id, 'ManagedIdentityOperator')
  scope: kubelet
  properties: {
    principalId: controlPlane.properties.principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', 'f1a07417-d97a-45cb-824c-7a7467783830')
  }
}
resource pullRole 'Microsoft.Authorization/roleAssignments@2022-04-01' = {
  name: guid(registry.id, kubelet.id, 'AcrPull')
  scope: registry
  properties: {
    principalId: kubelet.properties.principalId
    principalType: 'ServicePrincipal'
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', '7f951dda-4ed3-4680-a7ca-43fe172d538d')
  }
}

resource cluster 'Microsoft.ContainerService/managedClusters@2025-05-01' = {
  name: 'aks-kognition-${suffix}'
  location: location
  tags: tags
  sku: { name: 'Base', tier: 'Free' }
  identity: { type: 'UserAssigned', userAssignedIdentities: { '${controlPlane.id}': {} } }
  properties: {
    dnsPrefix: 'kognition-${suffix}'
    kubernetesVersion: kubernetesVersion
    enableRBAC: true
    disableLocalAccounts: true
    aadProfile: { managed: true, enableAzureRBAC: true, tenantID: subscription().tenantId }
    apiServerAccessProfile: { enablePrivateCluster: false, authorizedIPRanges: apiServerAuthorizedIpRanges }
    oidcIssuerProfile: { enabled: true }
    securityProfile: { workloadIdentity: { enabled: true } }
    autoUpgradeProfile: { upgradeChannel: 'patch', nodeOSUpgradeChannel: 'NodeImage' }
    identityProfile: {
      kubeletidentity: {
        resourceId: kubelet.id
        clientId: kubelet.properties.clientId
        objectId: kubelet.properties.principalId
      }
    }
    agentPoolProfiles: [
      {
        name: 'cpu'
        count: nodeCount
        vmSize: nodeVmSize
        mode: 'System'
        type: 'VirtualMachineScaleSets'
        osType: 'Linux'
        osSKU: 'AzureLinux'
        osDiskType: 'Managed'
        osDiskSizeGB: 128
        enableNodePublicIP: false
        vnetSubnetID: subnet.id
        maxPods: 80
        upgradeSettings: { maxSurge: '1' }
      }
    ]
    networkProfile: {
      networkPlugin: 'azure'
      networkPluginMode: 'overlay'
      networkDataplane: 'cilium'
      networkPolicy: 'cilium'
      loadBalancerSku: 'standard'
      outboundType: 'loadBalancer'
      podCidr: '10.244.0.0/16'
      serviceCidr: '10.250.0.0/16'
      dnsServiceIP: '10.250.0.10'
    }
    storageProfile: { diskCSIDriver: { enabled: true }, fileCSIDriver: { enabled: false } }
  }
  dependsOn: [networkRole, identityRole, pullRole]
}

resource operatorAccess 'Microsoft.Authorization/roleAssignments@2022-04-01' = [for roleId in [
  '4abbcc35-e782-43d8-92c5-2d3f1bd2253f'
  'b1ff04bb-8a4e-4dc4-8eb5-8693973ce19b'
]: {
  name: guid(cluster.id, operatorPrincipalId, roleId)
  scope: cluster
  properties: {
    principalId: operatorPrincipalId
    principalType: operatorPrincipalType
    roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', roleId)
  }
}]

output clusterName string = cluster.name
output clusterId string = cluster.id
output oidcIssuerUrl string = cluster.properties.oidcIssuerProfile.issuerURL