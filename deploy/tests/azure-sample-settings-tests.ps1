# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

#Requires -Version 7.0
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
. (Join-Path $repoRoot 'samples\azure-settings.ps1')
Import-Module powershell-yaml

function Assert-Equal($Actual, $Expected, [string]$Message) {
    if ($Actual -cne $Expected) { throw "${Message}: expected '$Expected', got '$Actual'." }
}

$settings = @{
    MS_FOUNDRY_PROJECT_ENDPOINT = 'https://new-account.services.ai.azure.com/api/projects/new-project'
    MS_FOUNDRY_INFERENCE_ENDPOINT = 'https://new-account.cognitiveservices.azure.com'
    MS_FOUNDRY_MODEL = 'gpt-6-luna'
    MS_FOUNDRY_INVOCATION_CONTRACT = 'openai-tool-chat-v1'
    MS_FOUNDRY_EMBEDDING_MODEL = 'embedding-deployment'
    MS_FOUNDRY_API_VERSION = '2024-12-01-preview'
    MS_FOUNDRY_EMBEDDING_API_VERSION = '2024-02-01'
}
$modelCount = 0
foreach ($relative in @(
    'simple-agent\manifests.yaml',
    'k8s-mode-demo.yaml',
    'knowledge-base-kognition-docs\model.yaml',
    'knowledge-base-blob-azure\manifests.yaml',
    'knowledge-base-blob-s3\manifests.yaml'
)) {
    $path = Join-Path $repoRoot "samples\$relative"
    $original = @(ConvertFrom-Yaml -Yaml (Get-Content -LiteralPath $path -Raw) -AllDocuments)
    $allRendered = @(ConvertFrom-Yaml -Yaml (Read-AzureSampleManifest -Path $path -Settings $settings) -AllDocuments)
    $profiles = @($allRendered | Where-Object { $_['kind'] -eq 'ModelInvocationProfile' })
    $rendered = @($allRendered | Where-Object { $_['kind'] -ne 'ModelInvocationProfile' })
    Assert-Equal $rendered.Count $original.Count "$relative document count"
    for ($index = 0; $index -lt $rendered.Count; $index++) {
        $document = $rendered[$index]
        Assert-Equal $document['kind'] $original[$index]['kind'] "$relative resource kind"
        Assert-Equal $document['metadata']['name'] $original[$index]['metadata']['name'] "$relative resource alias"
        Assert-Equal $document['metadata']['namespace'] $original[$index]['metadata']['namespace'] "$relative namespace"
        if ($document['kind'] -eq 'Model') {
            $embedding = $original[$index]['spec']['modelID'] -like 'text-embedding-*'
            Assert-Equal $document['spec']['modelID'] $(if ($embedding) { 'embedding-deployment' } else { 'gpt-6-luna' }) "$relative deployment"
            Assert-Equal $document['spec']['endpoint'] $settings.MS_FOUNDRY_INFERENCE_ENDPOINT "$relative endpoint"
            Assert-Equal $document['spec']['credentialRef']['name'] 'foundry-creds' "$relative credential binding"
            Assert-Equal $document['spec']['invocationContract'] $(if ($embedding) { 'openai-embedding-v1' } else { 'openai-tool-chat-v1' }) "$relative explicit invocation contract"
            $profile = @($profiles | Where-Object { $_['metadata']['name'] -eq $document['spec']['invocationProfileRef']['name'] -and $_['metadata']['namespace'] -eq $document['metadata']['namespace'] })
            Assert-Equal $profile.Count 1 "$relative namespace-local invocation profile"
            Assert-Equal $document['spec']['invocationProfileRef']['specDigest'] (Get-AzureSampleInvocationProfileDigest -Spec $profile[0]['spec']) "$relative reviewed profile digest"
            Assert-Equal $document['spec']['parameters']['apiVersion'] $(if ($embedding) { '2024-02-01' } else { '2024-12-01-preview' }) "$relative API version"
            $modelCount++
        }
    }
    Assert-Equal (Read-AzureSampleManifest -Path $path -Settings @{}) (Get-Content -LiteralPath $path -Raw) 'Unconfigured samples preserve their original content'
}
Assert-Equal $modelCount 6 'Every manifest-backed Azure Model is configured'

$helmArguments = @(Get-AzureSampleHelmArguments -Settings $settings)
Assert-Equal ($helmArguments -contains 'model.modelID=gpt-6-luna') $true 'Helm deployment uses the configured model'
Assert-Equal ($helmArguments -contains "model.endpoint=$($settings.MS_FOUNDRY_INFERENCE_ENDPOINT)") $true 'Helm uses the same endpoint'
Assert-Equal (@(Get-AzureSampleHelmArguments -Settings @{}).Count) 0 'Unconfigured Helm samples preserve their defaults'

$encoded = ConvertTo-AzureSampleYaml -Data @{ apiVersion = '2024-02-01'; enabled = $false; dimension = 1536 }
Assert-Equal ($encoded.StartsWith("---`n")) $true 'Generated YAML must select multi-document YAML decoding'
$typed = $encoded.Substring(4) | ConvertFrom-Json
Assert-Equal ($typed.apiVersion -is [string]) $true 'Date-like API versions must remain strings'
Assert-Equal $typed.apiVersion '2024-02-01' 'The API version must not change'
Assert-Equal ($typed.enabled -is [bool]) $true 'Boolean settings must remain booleans'
Assert-Equal ($typed.dimension -is [long] -or $typed.dimension -is [int]) $true 'Numeric dimensions must remain numbers'

$temporary = Join-Path $repoRoot ".azure-settings-test-$([guid]::NewGuid().ToString('N')).env"
try {
    [IO.File]::WriteAllText($temporary, "MS_FOUNDRY_MODEL=gpt-6-luna`n")
    $rejected = $false
    try { Get-AzureSampleSettings -Path $temporary | Out-Null } catch { $rejected = $true }
    Assert-Equal $rejected $true 'Partial model configuration is rejected'
    [IO.File]::WriteAllText($temporary, "# Test settings`nAZURE_CLIENT_SECRET='fake value with = and spaces'`n")
    Assert-Equal (Get-AzureSampleSettings -Path $temporary)['AZURE_CLIENT_SECRET'] 'fake value with = and spaces' 'Quoted environment values are preserved'
} finally {
    if (Test-Path -LiteralPath $temporary) { Remove-Item -LiteralPath $temporary }
}

Write-Output "PASS: $modelCount Azure Model bindings, stable resource aliases, Helm settings, environment parsing, and unconfigured compatibility."
