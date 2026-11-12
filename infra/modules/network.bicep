// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

@description('Azure region.')
param location string
@description('Deterministic resource group suffix.')
param suffix string
@description('Resource tags.')
param tags object

resource vnet 'Microsoft.Network/virtualNetworks@2024-05-01' = {
  name: 'vnet-kognition-${suffix}'
  location: location
  tags: tags
  properties: {
    addressSpace: { addressPrefixes: ['10.240.0.0/16'] }
    subnets: [
      { name: 'nodes', properties: { addressPrefix: '10.240.0.0/22' } }
      {
        name: 'private-endpoints'
        properties: {
          addressPrefix: '10.240.4.0/24'
          privateEndpointNetworkPolicies: 'Disabled'
        }
      }
    ]
  }
}

output vnetId string = vnet.id
output vnetName string = vnet.name
output privateEndpointSubnetId string = '${vnet.id}/subnets/private-endpoints'