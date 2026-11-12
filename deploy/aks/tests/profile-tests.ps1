# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

#Requires -Version 7.0
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$resolver = Join-Path $PSScriptRoot '../Resolve-Profile.ps1'

function Assert-Equal($Actual, $Expected, [string]$Message) {
    if ($Actual -cne $Expected) { throw "${Message}: expected '$Expected', got '$Actual'" }
}

$defaultProfile = & $resolver
Assert-Equal $defaultProfile.mode 'kubernetes' 'Only Kubernetes mode is supported'
Assert-Equal $defaultProfile.applicationSelfContained $true 'Default is self-contained'
Assert-Equal $defaultProfile.environment 'demo' 'Sizing is demo-only'
Assert-Equal $defaultProfile.schemaVersion 2 'Optional inference profile contract'
Assert-Equal $defaultProfile.nodeCompute 'cpu' 'AKS nodes use CPU'
Assert-Equal $defaultProfile.inferenceCompute 'none' 'Base has no inference service'
Assert-Equal $defaultProfile.chatProvider 'none' 'Chat is optional'
Assert-Equal $defaultProfile.embeddingProvider 'none' 'Embeddings are optional'
Assert-Equal $defaultProfile.localInferenceServices.Count 0 'No local inference deployment'
Assert-Equal $defaultProfile.gpuNodeCount 0 'Default does not allocate GPUs'
Assert-Equal $defaultProfile.enableFoundry $false 'Default excludes Foundry'
Assert-Equal $defaultProfile.enableSearch $false 'Default excludes Search'
Assert-Equal $defaultProfile.enableSampleStorage $false 'Default excludes sample Blob'
Assert-Equal $defaultProfile.enableBackupStorage $false 'Demo excludes backup Blob'

foreach ($preset in @('azure-samples', 'azure-search')) {
    $profile = & $resolver -Preset $preset
    Assert-Equal $profile.gpuNodeCount 0 'Hosted inference does not allocate GPU nodes'
    Assert-Equal $profile.requiresLocalModelValidation $false 'Hosted inference does not require local model validation'
    Assert-Equal $profile.postgresInstances 1 'Hosted inference retains durable memory database'
    Assert-Equal $profile.enableSearch ($preset -eq 'azure-search') 'Search is selected explicitly'
}

$cases = 0
foreach ($environment in @('demo')) {
    foreach ($chat in @('none', 'foundry')) {
        foreach ($embeddings in @('none', 'foundry')) {
            foreach ($backend in @('pgvector', 'azure-ai-search')) {
                foreach ($blob in @('emulator', 'azure')) {
                    $profile = & $resolver -Environment $environment -ChatProvider $chat -EmbeddingProvider $embeddings -KnowledgeBackend $backend -SampleBlob $blob
                    $foundry = ($chat -eq 'foundry' -or $embeddings -eq 'foundry')
                    Assert-Equal $profile.gpuNodeCount 0 'No combination allocates GPUs'
                    Assert-Equal $profile.nodeCompute 'cpu' 'AKS compute remains CPU'
                    Assert-Equal $profile.inferenceCompute $(if ($foundry) { 'foundry' } else { 'none' }) 'Optional inference placement'
                    Assert-Equal $profile.enableFoundry $foundry 'Foundry selection'
                    Assert-Equal $profile.localInferenceServices.Count 0 'No combination deploys local models'
                    Assert-Equal $profile.requiresLocalModelValidation $false 'No mandatory local model gate'
                    Assert-Equal $profile.requiresEmbeddingValidation ($embeddings -eq 'foundry') 'Optional embedding gate'
                    Assert-Equal $profile.applicationSelfContained (-not $foundry -and $backend -eq 'pgvector' -and $blob -eq 'emulator') 'Self-contained excludes managed dependencies'
                    Assert-Equal $profile.enableFoundryChat ($chat -eq 'foundry') 'Chat deployment selection'
                    Assert-Equal $profile.enableFoundryEmbeddings ($embeddings -eq 'foundry') 'Embedding deployment selection'
                    Assert-Equal $profile.enableSearch ($backend -eq 'azure-ai-search') 'Backend selection'
                    Assert-Equal $profile.enableSampleStorage ($blob -eq 'azure') 'Sample Blob selection'
                    Assert-Equal $profile.enableBackupStorage $false 'No production backup infrastructure'
                    Assert-Equal $profile.postgresInstances 1 'Single demo database instance'
                    Assert-Equal $profile.memoryEmbeddingDimension 1536 'Memory dimension remains compatible'
                    $cases++
                }
            }
        }
    }
}
$rejected = $false
try { & $resolver -ChatProvider 'unknown' | Out-Null } catch { $rejected = $true }
Assert-Equal $rejected $true 'Unknown providers must fail instead of falling back'
foreach ($selector in @('ChatProvider', 'EmbeddingProvider')) {
    $rejected = $false
    try { & $resolver @{$selector = 'local'} | Out-Null } catch { $rejected = $true }
    Assert-Equal $rejected $true 'Local inference is outside the AKS profile contract'
}
$rejected = $false
try { & $resolver -Environment 'prod' | Out-Null } catch { $rejected = $true }
Assert-Equal $rejected $true 'Production sizing is outside the demo deployment contract'
Write-Output "PASS: CPU-only demo defaults, named presets, $cases provider combinations, invalid provider and production rejection"