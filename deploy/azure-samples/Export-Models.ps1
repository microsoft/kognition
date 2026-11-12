# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

#Requires -Version 7.0
[CmdletBinding()]
param(
    [string]$EnvironmentFile = (Join-Path $PSScriptRoot '..\..\.env'),
    [string]$OutputDirectory = (Join-Path $PSScriptRoot '..\..\.azure\local-samples'),
    [string[]]$AgentNamespaces = @('default', 'kognition-demo', 'samples-multi-agent', 'samples-azure-blob', 'samples-governance', 'production', 'development')
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
. (Join-Path $repoRoot 'samples\azure-settings.ps1')
Import-Module powershell-yaml -ErrorAction Stop
$settings = Get-AzureSampleSettings -Path $EnvironmentFile
foreach ($key in @('MS_FOUNDRY_PROJECT_ENDPOINT', 'MS_FOUNDRY_MODEL', 'MS_FOUNDRY_EMBEDDING_MODEL', 'MS_FOUNDRY_INVOCATION_CONTRACT')) {
    if (-not $settings[$key]) { throw "Model export requires $key in the environment file." }
}
$endpoint = if ($settings['MS_FOUNDRY_INFERENCE_ENDPOINT']) { $settings['MS_FOUNDRY_INFERENCE_ENDPOINT'] } else { $settings['MS_FOUNDRY_PROJECT_ENDPOINT'] }
$helmValues = @{
    namespace = 'samples-multi-agent'
    credentials = @{ create = $false }
    modelClass = @{ create = $false }
    model = @{
        name = Get-AzureSampleChatModelName -Settings $settings
        modelID = $settings['MS_FOUNDRY_MODEL']
        endpoint = $endpoint
        apiVersion = $(if ($settings['MS_FOUNDRY_API_VERSION']) { $settings['MS_FOUNDRY_API_VERSION'] } else { '2024-12-01-preview' })
        invocationContract = $settings['MS_FOUNDRY_INVOCATION_CONTRACT']
    }
    invocationProfile = @{
        create = $true
        name = 'sample-chat-tools-v1'
        spec = (New-AzureSampleInvocationProfile).spec
        specDigest = Get-AzureSampleInvocationProfileDigest -Spec (New-AzureSampleInvocationProfile).spec
    }
}
$chatDocuments = @(ConvertFrom-Yaml -Yaml (Read-AzureSampleManifest -Path (Join-Path $repoRoot 'samples\simple-agent\manifests.yaml') -Settings $settings) -AllDocuments)
$embeddingDocuments = @(ConvertFrom-Yaml -Yaml (Read-AzureSampleManifest -Path (Join-Path $repoRoot 'samples\knowledge-base-kognition-docs\model.yaml') -Settings $settings) -AllDocuments)
$models = @($chatDocuments | Where-Object { $_['kind'] -eq 'Model' }) + @($embeddingDocuments | Where-Object { $_['kind'] -eq 'Model' -and $_['metadata']['name'] -eq 'text-embedding-3-small' })
$documents = @($chatDocuments | Where-Object { $_['kind'] -eq 'ModelClass' } | ForEach-Object { ConvertTo-AzureSampleYaml -Data $_ })
foreach ($namespace in ($AgentNamespaces | Select-Object -Unique)) {
    if ($namespace -eq 'samples-multi-agent') { continue }
    $documents += ConvertTo-AzureSampleYaml -Data (New-AzureSampleInvocationProfile -Namespace $namespace)
    $documents += ConvertTo-AzureSampleYaml -Data (New-AzureSampleInvocationProfile -Namespace $namespace -Embedding)
    foreach ($model in $models) {
        $model['metadata']['namespace'] = $namespace
        $documents += ConvertTo-AzureSampleYaml -Data $model
    }
}
New-Item -ItemType Directory -Path $OutputDirectory -Force | Out-Null
[IO.File]::WriteAllText((Join-Path $OutputDirectory 'models.yaml'), ($documents -join "`n"), [Text.UTF8Encoding]::new($false))
[IO.File]::WriteAllText((Join-Path $OutputDirectory 'helm-multi-agent-values.yaml'), (ConvertTo-AzureSampleYaml -Data $helmValues), [Text.UTF8Encoding]::new($false))
Write-Output 'Exported explicit invocation profiles, Model bindings and Helm values. No credentials, Azure resources or Kubernetes resources were changed.'
