# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

#Requires -Version 7.0
[CmdletBinding()]
param(
    [ValidateRange(0, 900)][int]$RbacTimeoutSeconds = 600,
    [switch]$SkipBlob
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
. (Join-Path $repoRoot 'samples\azure-settings.ps1')
$settings = Get-AzureSampleSettings
$required = @(
    'AZURE_TENANT_ID', 'AZURE_CLIENT_ID', 'AZURE_CLIENT_SECRET',
    'MS_FOUNDRY_INFERENCE_ENDPOINT', 'MS_FOUNDRY_MODEL', 'MS_FOUNDRY_EMBEDDING_MODEL'
)
if (-not $SkipBlob) {
    $required += @('KOGNITION_AZURE_BLOB_ACCOUNT', 'KOGNITION_AZURE_BLOB_CONTAINER', 'KOGNITION_AZURE_BLOB_PREFIX')
}
foreach ($key in $required) {
    if (-not $settings[$key]) { throw "Azure verification requires $key in .env." }
}
$endpoint = [uri]$settings['MS_FOUNDRY_INFERENCE_ENDPOINT']
if ($endpoint.Scheme -ne 'https' -or
    (-not $endpoint.Host.EndsWith('.cognitiveservices.azure.com') -and -not $endpoint.Host.EndsWith('.openai.azure.com'))) {
    throw 'Azure verification requires a trusted HTTPS Azure inference endpoint.'
}
if (-not $SkipBlob -and $settings['KOGNITION_AZURE_BLOB_ACCOUNT'] -notmatch '^[a-z0-9]{3,24}$') {
    throw 'Azure verification requires a valid Storage account name.'
}
$deadline = [DateTime]::UtcNow.AddSeconds($RbacTimeoutSeconds)

function Get-ServiceToken {
    param([string]$Scope)
    $response = Invoke-RestMethod -Method Post `
        -Uri "https://login.microsoftonline.com/$($settings['AZURE_TENANT_ID'])/oauth2/v2.0/token" `
        -ContentType 'application/x-www-form-urlencoded' `
        -Body @{
            client_id = $settings['AZURE_CLIENT_ID']
            client_secret = $settings['AZURE_CLIENT_SECRET']
            grant_type = 'client_credentials'
            scope = $Scope
        } -SkipHttpErrorCheck -StatusCodeVariable status -TimeoutSec 60
    if ($status -ne 200) { throw "Service-principal authentication failed for $Scope (HTTP $status)." }
    if (-not $response.access_token) { throw 'Entra returned no access token.' }
    return $response.access_token
}

function Invoke-Service {
    param([string]$Uri, [hashtable]$Headers, [string]$Method = 'Get', [hashtable]$Body)
    do {
        $arguments = @{
            Uri = $Uri
            Method = $Method
            Headers = $Headers
            SkipHttpErrorCheck = $true
            StatusCodeVariable = 'status'
            TimeoutSec = 90
        }
        if ($Body) {
            $arguments['ContentType'] = 'application/json'
            $arguments['Body'] = $Body | ConvertTo-Json -Depth 20 -Compress
        }
        $response = Invoke-RestMethod @arguments
        if ($status -eq 200) { return $response }
        if ($status -ne 403 -or [DateTime]::UtcNow -ge $deadline) {
            throw "Azure data-plane verification failed: $Method $Uri (HTTP $status)."
        }
        Write-Verbose 'Waiting for the newly assigned Azure data-plane role to propagate.'
        Start-Sleep -Seconds 10
    } while ($true)
}

$inferenceToken = Get-ServiceToken -Scope 'https://cognitiveservices.azure.com/.default'
$inferenceHeaders = @{ Authorization = "Bearer $inferenceToken" }
$chatDeployment = $settings['MS_FOUNDRY_MODEL']
$chatUri = "$($endpoint.AbsoluteUri.TrimEnd('/'))/openai/deployments/$([uri]::EscapeDataString($chatDeployment))/chat/completions?api-version=$($settings['MS_FOUNDRY_API_VERSION'])"
$reasoning = $chatDeployment.ToLowerInvariant().StartsWith('gpt-5') -or $chatDeployment.ToLowerInvariant().StartsWith('gpt-6')
$chatBody = @{ messages = @(@{ role = 'user'; content = 'Reply with exactly the word ready.' }) }
if ($reasoning) {
    $chatBody['max_completion_tokens'] = 1024
    if ($chatDeployment.ToLowerInvariant().StartsWith('gpt-6')) { $chatBody['reasoning_effort'] = 'none' }
} else {
    $chatBody['max_tokens'] = 128
}
$chat = Invoke-Service -Uri $chatUri -Method Post -Headers $inferenceHeaders -Body $chatBody
if ($chat.choices.Count -ne 1 -or $chat.choices[0].message.content.Trim().ToLowerInvariant() -ne 'ready') {
    throw 'The chat deployment did not return the expected smoke-test output.'
}
if ($chat.usage.total_tokens -le 0) { throw 'The chat deployment did not report token usage.' }
$chatBody['messages'] = @(@{ role = 'user'; content = 'Call sample_lookup with topic set to kognition. Do not answer directly.' })
$chatBody['tools'] = @(@{
    type = 'function'
    function = @{
        name = 'sample_lookup'
        description = 'Look up the requested sample topic.'
        parameters = @{
            type = 'object'
            properties = @{ topic = @{ type = 'string' } }
            required = @('topic')
            additionalProperties = $false
        }
    }
})
$chatBody['tool_choice'] = @{ type = 'function'; function = @{ name = 'sample_lookup' } }
$toolResult = Invoke-Service -Uri $chatUri -Method Post -Headers $inferenceHeaders -Body $chatBody
$calls = @($toolResult.choices[0].message.tool_calls)
if ($calls.Count -ne 1 -or $calls[0].function.name -ne 'sample_lookup' -or
    ($calls[0].function.arguments | ConvertFrom-Json).topic -ne 'kognition') {
    throw 'The chat deployment did not produce the expected structured tool call.'
}
$embeddingUri = "$($endpoint.AbsoluteUri.TrimEnd('/'))/openai/deployments/$([uri]::EscapeDataString($settings['MS_FOUNDRY_EMBEDDING_MODEL']))/embeddings?api-version=$($settings['MS_FOUNDRY_EMBEDDING_API_VERSION'])"
$embedding = Invoke-Service -Uri $embeddingUri -Method Post -Headers $inferenceHeaders -Body @{ input = @('Kognition Azure sample verification.') }
if ($embedding.data.Count -ne 1 -or $embedding.data[0].embedding.Count -ne 1536) {
    throw 'The embedding deployment must return exactly one 1536-dimensional vector for the sample pgvector schema.'
}
if ($SkipBlob) {
    Write-Output 'PASS: service-principal chat, structured tool calling, and 1536-dimensional embeddings.'
    Write-Warning 'DEFERRED: real Azure Blob upload and read verification require an approved private network path. Local Azurite/S3 samples are not affected.'
    return
}

$blobToken = Get-ServiceToken -Scope 'https://storage.azure.com/.default'
$blobHeaders = @{ Authorization = "Bearer $blobToken"; 'x-ms-version' = '2023-11-03' }
$containerUri = "https://$($settings['KOGNITION_AZURE_BLOB_ACCOUNT']).blob.core.windows.net/$($settings['KOGNITION_AZURE_BLOB_CONTAINER'])"
$prefix = $settings['KOGNITION_AZURE_BLOB_PREFIX'].Trim('/')
$listing = Invoke-Service -Uri "${containerUri}?restype=container&comp=list&prefix=$([uri]::EscapeDataString("$prefix/"))" -Headers $blobHeaders
$blobs = @($listing.EnumerationResults.Blobs.Blob)
$expected = @(Get-ChildItem -LiteralPath (Join-Path $repoRoot 'samples\docs') -File)
foreach ($file in $expected) {
    if (@($blobs | Where-Object { $_.Name -ceq "$prefix/$($file.Name)" }).Count -ne 1) {
        throw "The uploaded sample document is missing or duplicated: $($file.Name)."
    }
}
$download = Invoke-Service -Uri "$containerUri/$prefix/overview.md" -Headers $blobHeaders
$local = Get-Content -LiteralPath (Join-Path $repoRoot 'samples\docs\overview.md') -Raw
if ([string]$download -cne $local) { throw 'The service principal downloaded different Blob content from the uploaded sample document.' }

Write-Output "PASS: service-principal chat, structured tool calling, 1536-dimensional embeddings, and Blob listing/download ($($blobs.Count) documents). The operator Azure CLI login and Kubernetes were not changed."
