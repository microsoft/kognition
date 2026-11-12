# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

#Requires -Version 7.0
[CmdletBinding()]
param(
    [ValidateSet('self-contained', 'azure-samples', 'azure-search')]
    [string]$Preset = 'self-contained',
    [ValidateSet('demo')]
    [string]$Environment = 'demo',
    [ValidateSet('none', 'foundry')]
    [string]$ChatProvider,
    [ValidateSet('none', 'foundry')]
    [string]$EmbeddingProvider,
    [ValidateSet('pgvector', 'azure-ai-search')]
    [string]$KnowledgeBackend,
    [ValidateSet('emulator', 'azure')]
    [string]$SampleBlob
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$presets = @{
    'self-contained' = @('none', 'none', 'pgvector', 'emulator')
    'azure-samples' = @('foundry', 'foundry', 'pgvector', 'azure')
    'azure-search' = @('foundry', 'foundry', 'azure-ai-search', 'azure')
}
$defaults = $presets[$Preset]
if (-not $ChatProvider) { $ChatProvider = $defaults[0] }
if (-not $EmbeddingProvider) { $EmbeddingProvider = $defaults[1] }
if (-not $KnowledgeBackend) { $KnowledgeBackend = $defaults[2] }
if (-not $SampleBlob) { $SampleBlob = $defaults[3] }
$enableFoundry = ($ChatProvider -eq 'foundry' -or $EmbeddingProvider -eq 'foundry')

[pscustomobject][ordered]@{
    schemaVersion = 2
    preset = $Preset
    environment = $Environment
    mode = 'kubernetes'
    chatProvider = $ChatProvider
    embeddingProvider = $EmbeddingProvider
    knowledgeBackend = $KnowledgeBackend
    sampleBlob = $SampleBlob
    localInferenceServices = @()
    nodeCompute = 'cpu'
    inferenceCompute = $(if ($enableFoundry) { 'foundry' } else { 'none' })
    gpuNodeCount = 0
    enableFoundry = $enableFoundry
    enableFoundryChat = ($ChatProvider -eq 'foundry')
    enableFoundryEmbeddings = ($EmbeddingProvider -eq 'foundry')
    enableSearch = ($KnowledgeBackend -eq 'azure-ai-search')
    enableSampleStorage = ($SampleBlob -eq 'azure')
    enableBackupStorage = $false
    postgresInstances = 1
    memoryEmbeddingDimension = 1536
    requiresLocalModelValidation = $false
    requiresEmbeddingValidation = ($EmbeddingProvider -eq 'foundry')
    requiresSearchValidation = ($KnowledgeBackend -eq 'azure-ai-search')
    applicationSelfContained = (-not $enableFoundry -and $KnowledgeBackend -eq 'pgvector' -and $SampleBlob -eq 'emulator')
}