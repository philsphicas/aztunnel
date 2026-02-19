// Azure Relay namespace and hybrid connections for e2e tests.
// Deploy: az deployment group create -g <rg> -f infra/e2e.bicep -p userIds='["<obj-id>"]'

@description('Name for the Azure Relay namespace (must be globally unique). Defaults to aztunnel-e2e-<hash>.')
param relayName string = 'aztunnel-e2e-${uniqueString(resourceGroup().id)}'

@description('Azure region')
param location string = resourceGroup().location

@description('User object IDs to grant Relay Listener + Sender roles')
param userIds array = []

@description('Service principal object IDs to grant Relay Listener + Sender roles')
param servicePrincipalIds array = []

@description('Group object IDs to grant Relay Listener + Sender roles')
param groupIds array = []

resource relayNamespace 'Microsoft.Relay/namespaces@2021-11-01' = {
  name: relayName
  location: location
  sku: {
    name: 'Standard'
    tier: 'Standard'
  }
}

resource hycoEntra 'Microsoft.Relay/namespaces/hybridConnections@2021-11-01' = {
  parent: relayNamespace
  name: 'e2e-entra'
  properties: {
    requiresClientAuthorization: true
  }
}

resource hycoSas 'Microsoft.Relay/namespaces/hybridConnections@2021-11-01' = {
  parent: relayNamespace
  name: 'e2e-sas'
  properties: {
    requiresClientAuthorization: true
  }
}

// Separate SAS auth rules for listener (Listen) and sender (Send).
// batchSize(1) avoids MessagingGatewayTooManyRequests throttling.
var sasRules = [
  { name: 'e2e-listener', right: 'Listen' }
  { name: 'e2e-sender', right: 'Send' }
]

@batchSize(1)
resource sasAuthRules 'Microsoft.Relay/namespaces/hybridConnections/authorizationRules@2021-11-01' = [
  for rule in sasRules: {
    parent: hycoSas
    name: rule.name
    properties: {
      rights: [
        rule.right
      ]
    }
  }
]

// RBAC: build cross-product of principals × roles for a single assignment loop.
var relayRoles = [
  '26e0b698-aa6d-4085-9386-aadae190014d' // Azure Relay Listener
  '26baccc8-eea7-41f1-98f4-1762cc7f685d' // Azure Relay Sender
]

var allPrincipals = concat(
  map(userIds, id => { id: id, type: 'User' }),
  map(servicePrincipalIds, id => { id: id, type: 'ServicePrincipal' }),
  map(groupIds, id => { id: id, type: 'Group' })
)

var assignments = flatten(map(relayRoles, role => map(allPrincipals, p => {
  roleId: role
  principalId: p.id
  principalType: p.type
})))

resource roleAssignments 'Microsoft.Authorization/roleAssignments@2022-04-01' = [
  for a in assignments: {
    name: guid(relayNamespace.id, a.principalId, a.roleId)
    scope: relayNamespace
    properties: {
      roleDefinitionId: subscriptionResourceId('Microsoft.Authorization/roleDefinitions', a.roleId)
      principalId: a.principalId
      principalType: a.principalType
    }
  }
]

output relayNamespaceName string = relayNamespace.name
output hycoEntraName string = hycoEntra.name
output hycoSasName string = hycoSas.name
output sasListenerRuleName string = sasAuthRules[0].name
output sasSenderRuleName string = sasAuthRules[1].name
