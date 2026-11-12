# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

param(
    [string]$TargetNamespace = "samples-azure-blob"
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest
. (Join-Path $PSScriptRoot '..\azure-settings.ps1')
$settings = Get-AzureSampleSettings
$chatModelName = if ($settings['MS_FOUNDRY_MODEL']) { Get-AzureSampleChatModelName -Settings $settings } else { 'gpt-6-luna' }

foreach ($resource in @(
    "modelgrant/azure-docs-agent-$chatModelName",
    'knowledgebaseindexschedule/azure-docs-reindex-daily',
    'knowledgebaseindexrun/azure-docs-reindex-now',
    'agent/azure-docs-agent',
    'knowledgebaseclaim/azure-docs-claim',
    'knowledgebase/azure-docs-kb',
    'model/text-embedding-3-small',
    "model/$chatModelName",
    'secretref/azure-blob-creds',
    'secretref/kognition-kb-pgvector'
)) {
    & kubectl delete $resource -n $TargetNamespace --ignore-not-found --wait=false 2>$null
    if ($LASTEXITCODE -ne 0) { throw "Failed to delete $TargetNamespace/$resource" }
}

& kubectl delete knowledgebaseclass azure-live-pgvector --ignore-not-found --wait=false 2>$null
if ($LASTEXITCODE -ne 0) { throw 'Failed to delete KnowledgeBaseClass/azure-live-pgvector' }