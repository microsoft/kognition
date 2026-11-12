# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# Apply the real Azure Blob knowledge-base profile using ignored local
# credentials. No credentials are stored in this repository.
param(
    [string]$TargetNamespace = "samples-azure-blob",
    [string]$PlatformNamespace = "kognition-system",
    [string]$SourceModelNamespace = "default"
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$sampleDir = $PSScriptRoot
. (Join-Path $sampleDir '..\azure-settings.ps1')
$repoRoot = (Resolve-Path (Join-Path $sampleDir "..\..")).Path
$envFile = Join-Path $repoRoot ".env"
$pgSecret = "kognition-kb-pgvector"

if (-not (Test-Path $envFile)) {
    throw "Missing $envFile. Run .\make.ps1 samples-provision-azure first."
}

$settings = @{}
foreach ($line in Get-Content $envFile) {
    if ($line -match '^\s*([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*)$') {
        $settings[$matches[1]] = $matches[2].Trim().Trim('"').Trim("'")
    }
}

$connectionString = $settings['KOGNITION_AZURE_BLOB_CONNECTION_STRING']
$storageAccount = $settings['KOGNITION_AZURE_BLOB_ACCOUNT']
$container = $settings['KOGNITION_AZURE_BLOB_CONTAINER']
$prefix = $settings['KOGNITION_AZURE_BLOB_PREFIX']
if (-not $container -or -not $prefix) {
    throw "Azure Blob sample settings are incomplete. Run .\make.ps1 samples-provision-azure first."
}
if (-not $storageAccount -and $connectionString -match '(?:^|;)BlobEndpoint=https://([^.]+)\.blob\.core\.windows\.net(?:/|;)') {
    $storageAccount = $matches[1]
}
if (-not $storageAccount) {
    throw 'Azure Blob sample settings do not identify a Storage account; set KOGNITION_AZURE_BLOB_ACCOUNT in .env.'
}
if (-not $connectionString) {
    foreach ($key in @('AZURE_TENANT_ID', 'AZURE_CLIENT_ID', 'AZURE_CLIENT_SECRET')) {
        if (-not $settings[$key]) { throw "Azure Blob identity authentication requires $key in .env." }
    }
    $chatModelName = Get-AzureSampleChatModelName -Settings $settings
}

if (-not (Get-Command az -ErrorAction SilentlyContinue)) {
    throw 'Azure CLI is required to verify Storage data-plane access.'
}
$containerExists = (& az storage container exists --account-name $storageAccount --name $container --auth-mode login --query exists -o tsv --only-show-errors | Out-String).Trim()
if ($LASTEXITCODE -ne 0 -or $containerExists -ne 'true') {
    throw "Could not access '$container' in Azure Storage account '$storageAccount'. Run from a runner with Blob data-plane network access and Storage Blob Data Reader permissions."
}

function Invoke-Kubectl {
    param([Parameter(Mandatory = $true)][string[]]$Arguments)

    & kubectl @Arguments
    if ($LASTEXITCODE -ne 0) { throw "kubectl $($Arguments -join ' ') failed" }
}

function Wait-ForPhase {
    param(
        [Parameter(Mandatory = $true)][string]$Resource,
        [Parameter(Mandatory = $true)][string]$Expected,
        [string]$Namespace = $TargetNamespace
    )

    $deadline = [DateTime]::UtcNow.AddMinutes(5)
    do {
        $phase = (& kubectl get $Resource -n $Namespace -o jsonpath='{.status.phase}' 2>$null | Out-String).Trim()
        if ($phase -eq $Expected) { return }
        if ($phase -in @('Failed', 'Error')) { throw "$Resource reached terminal phase $phase" }
        [Threading.Tasks.Task]::Delay(1000).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "$Resource did not reach $Expected within five minutes"
}

& kubectl get namespace $TargetNamespace 2>$null | Out-Null
if ($LASTEXITCODE -ne 0) { Invoke-Kubectl @('create', 'namespace', $TargetNamespace) }

# The full bundle creates both Foundry-backed Models in the source namespace.
# Copy them into the profile namespace before applying any dependent resource.
foreach ($modelName in @('text-embedding-3-small', $chatModelName)) {
    Wait-ForPhase -Resource "model/$modelName" -Expected 'Ready' -Namespace $SourceModelNamespace
}

$pgConnection = (& kubectl -n $PlatformNamespace get secret $pgSecret -o jsonpath='{.data.connectionString}' | ForEach-Object { [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($_)) })
$pgPassword = (& kubectl -n $PlatformNamespace get secret $pgSecret -o jsonpath='{.data.password}' | ForEach-Object { [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($_)) })
if (-not $pgConnection) { throw "Could not read $PlatformNamespace/$pgSecret" }
$pgConnection = $pgConnection -replace "@${pgSecret}:", "@$pgSecret.$PlatformNamespace.svc.cluster.local:"
$pgPayload = @{ connectionString = $pgConnection; password = $pgPassword } | ConvertTo-Json -Compress
$blobPayload = if ($connectionString) {
    @{ connectionString = $connectionString } | ConvertTo-Json -Compress
} else {
    @{ accountName = $storageAccount } | ConvertTo-Json -Compress
}
$blobKey = if ($connectionString) { 'connectionString' } else { 'accountName' }

@"
apiVersion: kognition.dev/v1alpha1
kind: SecretRef
metadata:
  name: $pgSecret
  namespace: $TargetNamespace
  annotations:
    kognition.dev/internal-secret-data: '$pgPayload'
spec:
  backendType: internal
  backendRef: $pgSecret
  key: connectionString
---
apiVersion: kognition.dev/v1alpha1
kind: SecretRef
metadata:
  name: azure-blob-creds
  namespace: $TargetNamespace
  annotations:
    kognition.dev/internal-secret-data: '$blobPayload'
spec:
  backendType: internal
  backendRef: azure-blob-creds
  key: $blobKey
"@ | kubectl apply -f -
if ($LASTEXITCODE -ne 0) { throw "Failed to apply generated Azure Blob SecretRefs" }

$kbClassName = 'azure-live-pgvector'
$kbClass = Get-Content (Join-Path $sampleDir 'kb-class.yaml') -Raw
$kbClass = $kbClass -replace 'name: pgvector-standard', "name: $kbClassName"
$kbClass | kubectl apply -f -
if ($LASTEXITCODE -ne 0) { throw "Failed to apply Azure Blob KnowledgeBaseClass" }

foreach ($modelName in @('text-embedding-3-small', $chatModelName)) {
    $sourceModel = (& kubectl get model $modelName -n $SourceModelNamespace -o json | ConvertFrom-Json)
    $sourceModel.metadata.namespace = $TargetNamespace
    $sourceModel.metadata.PSObject.Properties.Remove('resourceVersion')
    $sourceModel.metadata.PSObject.Properties.Remove('uid')
    $sourceModel.metadata.PSObject.Properties.Remove('creationTimestamp')
    $sourceModel.metadata.PSObject.Properties.Remove('generation')
    $sourceModel.metadata.PSObject.Properties.Remove('managedFields')
    $sourceModel.PSObject.Properties.Remove('status')
    $sourceModel | ConvertTo-Json -Depth 30 -Compress | kubectl apply -f -
    if ($LASTEXITCODE -ne 0) { throw "Failed to copy Model '$modelName' into $TargetNamespace" }
    Wait-ForPhase -Resource "model/$modelName" -Expected 'Ready'
}

Import-Module powershell-yaml -ErrorAction Stop
$kb = ConvertFrom-Yaml -Yaml (Get-Content (Join-Path $sampleDir 'kb.yaml') -Raw)
$kb['metadata']['namespace'] = $TargetNamespace
$kb['spec']['knowledgeBaseClassName'] = $kbClassName
$blob = $kb['spec']['sources'][0]['blob']
$blob['bucket'] = $container
$blob['prefix'] = $prefix
if (-not $connectionString) { $blob['endpoint'] = "https://$storageAccount.blob.core.windows.net" }
ConvertTo-AzureSampleYaml -Data $kb | kubectl apply -f -
if ($LASTEXITCODE -ne 0) { throw "Failed to apply Azure Blob KnowledgeBase" }

foreach ($manifest in @('agent.yaml', 'indexrun.yaml', 'indexschedule.yaml')) {
        $documents = @(ConvertFrom-Yaml -Yaml (Read-AzureSampleManifest -Path (Join-Path $sampleDir $manifest) -Settings $settings) -AllDocuments)
        $content = ($documents | Where-Object { $_ } | ForEach-Object {
            $_.metadata.namespace = $TargetNamespace
            ConvertTo-AzureSampleYaml -Data $_
        }) -join "`n"
        $content | kubectl apply -f -
        if ($LASTEXITCODE -ne 0) { throw "Failed to apply $manifest" }
}

@"
apiVersion: kognition.dev/v1alpha1
kind: ModelGrant
metadata:
    name: azure-docs-agent-$chatModelName
    namespace: $TargetNamespace
spec:
    agentRef:
        name: azure-docs-agent
    modelRef:
        name: $chatModelName
"@ | kubectl apply -f -
if ($LASTEXITCODE -ne 0) { throw "Failed to grant the Azure Blob sample Agent access to $chatModelName" }

Wait-ForPhase -Resource 'knowledgebase/azure-docs-kb' -Expected 'Ready'
Wait-ForPhase -Resource 'knowledgebaseindexrun/azure-docs-reindex-now' -Expected 'Succeeded'
Write-Host "Azure Blob knowledge-base sample is ready against $container/$prefix." -ForegroundColor Green