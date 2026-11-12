// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

@description('Azure region.')
param location string
@description('Deterministic resource group suffix.')
param suffix string
@description('Resource tags.')
param tags object
@description('VNet linked to registry private DNS.')
param vnetId string
@description('Subnet for registry private endpoint.')
param privateEndpointSubnetId string

resource registry 'Microsoft.ContainerRegistry/registries@2025-11-01' = {
  name: 'acrkognition${suffix}'
  location: location
  tags: tags
  sku: { name: 'Premium' }
  properties: {
    adminUserEnabled: false
    anonymousPullEnabled: false
    publicNetworkAccess: 'Disabled'
    roleAssignmentMode: 'LegacyRegistryPermissions'
    networkRuleBypassOptions: 'None'
  }
}

resource zone 'Microsoft.Network/privateDnsZones@2024-06-01' = {
  name: 'privatelink.azurecr.io'
  location: 'global'
  tags: tags
}

resource link 'Microsoft.Network/privateDnsZones/virtualNetworkLinks@2024-06-01' = {
  parent: zone
  name: 'kognition'
  location: 'global'
  properties: { registrationEnabled: false, virtualNetwork: { id: vnetId } }
}

resource endpoint 'Microsoft.Network/privateEndpoints@2024-05-01' = {
  name: 'pe-acr-${suffix}'
  location: location
  tags: tags
  properties: {
    subnet: { id: privateEndpointSubnetId }
    privateLinkServiceConnections: [
      {
        name: 'registry'
        properties: { privateLinkServiceId: registry.id, groupIds: ['registry'] }
      }
    ]
  }
}

resource zoneGroup 'Microsoft.Network/privateEndpoints/privateDnsZoneGroups@2024-05-01' = {
  parent: endpoint
  name: 'default'
  properties: {
    privateDnsZoneConfigs: [{ name: 'acr', properties: { privateDnsZoneId: zone.id } }]
  }
}

output registryName string = registry.name
output loginServer string = registry.properties.loginServer