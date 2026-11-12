# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

param(
    [string]$Server = 'http://127.0.0.1:8080',
    [string]$KognitionNamespace = 'kognition-system'
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$runID = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
$namespace = "standalone-ingest-$runID"
$className = "standalone-pgvector-$runID"
$modelName = "standalone-embedding-$runID"
$kbName = "standalone-docs-$runID"
$runName = "standalone-index-$runID"
$pgSecretName = "standalone-pgvector-$runID"
$created = [System.Collections.Generic.List[object]]::new()

function Invoke-Api {
    param([string]$Method, [string]$Url, [object]$Body, [int[]]$ExpectedStatus)
    $parameters = @{ Uri = $Url; Method = $Method; SkipHttpErrorCheck = $true; TimeoutSec = 30 }
    if ($null -ne $Body) {
        $parameters.ContentType = 'application/json'
        $parameters.Body = $Body | ConvertTo-Json -Depth 40 -Compress
    }
    $response = Invoke-WebRequest @parameters
    if ($response.StatusCode -notin $ExpectedStatus) {
        throw "$Method $Url returned $($response.StatusCode): $($response.Content)"
    }
    if ($response.Content) { return $response.Content | ConvertFrom-Json }
    return $null
}

function Get-ResourceUrl {
    param([string]$Plural, [string]$Name = '')
    $url = "$Server/apis/kognition.dev/v1alpha1/namespaces/$namespace/$Plural"
    if ($Name) { $url += "/$Name" }
    return $url
}

function Add-Resource {
    param([string]$Plural, [hashtable]$Object)
    $createdObject = Invoke-Api POST (Get-ResourceUrl $Plural) $Object @(200, 201)
    if ($createdObject.kind -ne $Object.kind -or $createdObject.metadata.name -ne $Object.metadata.name) {
        throw "created resource identity mismatch for $($Object.kind)/$($Object.metadata.name)"
    }
    $created.Add([pscustomobject]@{ Plural = $Plural; Name = $Object.metadata.name })
}

function Remove-Resource {
    param([string]$Plural, [string]$Name)
    $deadline = [DateTime]::UtcNow.AddSeconds(20)
    do {
        $response = Invoke-WebRequest -Uri (Get-ResourceUrl $Plural $Name) -Method DELETE -SkipHttpErrorCheck -TimeoutSec 10
        if ($response.StatusCode -in @(200, 202, 204, 404)) { return }
        if ($response.StatusCode -ne 409) {
            throw "DELETE $Plural/$Name returned $($response.StatusCode): $($response.Content)"
        }
        [Threading.Tasks.Task]::Delay(250).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "DELETE $Plural/$Name remained conflicted"
}

try {
    Add-Resource 'secretrefs' @{
        apiVersion = 'kognition.dev/v1alpha1'; kind = 'SecretRef'
        metadata = @{ name = $pgSecretName; namespace = $namespace }
        spec = @{ backendType = 'env'; backendRef = 'KOGNITION_KB_PGVECTOR_DSN'; key = 'connectionString' }
    }
    Add-Resource 'models' @{
        apiVersion = 'kognition.dev/v1alpha1'; kind = 'Model'
        metadata = @{ name = $modelName; namespace = $namespace }
        spec = @{
            className = 'local'; provider = 'deterministic'; modelID = 'deterministic-local'
        }
    }
    Add-Resource 'knowledgebaseclasses' @{
        apiVersion = 'kognition.dev/v1alpha1'; kind = 'KnowledgeBaseClass'
        metadata = @{ name = $className }
        spec = @{
            provisioner = 'pgvector'; indexMode = 'dense'; reclaimPolicy = 'Delete'
            embeddingModelRef = @{ name = $modelName }
            chunkStrategy = @{ size = 128; overlap = 16; mode = 'recursive' }
            vectorStore = @{ type = 'pgvector'; pgvector = @{
                connectionSecretRef = @{ name = $pgSecretName }
                table = 'kognition_kb_chunks'; dimension = 1536
            } }
        }
    }
    Add-Resource 'knowledgebases' @{
        apiVersion = 'kognition.dev/v1alpha1'; kind = 'KnowledgeBase'
        metadata = @{ name = $kbName; namespace = $namespace }
        spec = @{
            knowledgeBaseClassName = $className
            sources = @(
                @{ type = 'inline'; data = 'Standalone ingestion writes real vectors for this document.' },
                @{ type = 'inline'; data = 'A second document proves aggregate document and chunk statistics.' }
            )
        }
    }

    $deadline = [DateTime]::UtcNow.AddMinutes(3)
    do {
        $kb = Invoke-Api GET (Get-ResourceUrl 'knowledgebases' $kbName) $null @(200)
        if ($kb.status.phase -eq 'Failed') { throw "KnowledgeBase failed: $($kb.status.conditions | ConvertTo-Json -Compress)" }
        if ($kb.status.phase -eq 'Ready' -and [int64]$kb.status.documentCount -ge 2 -and [int64]$kb.status.indexedChunks -gt 0) { break }
        [Threading.Tasks.Task]::Delay(1000).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    if ($kb.status.phase -ne 'Ready') { throw "KnowledgeBase did not become Ready: phase=$($kb.status.phase)" }

    $rowCount = (& kubectl exec -n $KognitionNamespace statefulset/kognition-kb-pgvector -- psql -U kognition_kb -d kognition_kb -tAc "SELECT COUNT(*) FROM kognition_kb_chunks WHERE kb_namespace = '$namespace' AND kb_name = '$kbName';" 2>$null | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or [int64]$rowCount -le 0) { throw "pgvector row verification failed: count=$rowCount" }

    Add-Resource 'knowledgebaseindexruns' @{
        apiVersion = 'kognition.dev/v1alpha1'; kind = 'KnowledgeBaseIndexRun'
        metadata = @{ name = $runName; namespace = $namespace }
        spec = @{ knowledgeBaseName = $kbName }
    }
    $deadline = [DateTime]::UtcNow.AddMinutes(3)
    do {
        $indexRun = Invoke-Api GET (Get-ResourceUrl 'knowledgebaseindexruns' $runName) $null @(200)
        if ($indexRun.status.phase -eq 'Failed') { throw "IndexRun failed: $($indexRun.status.message)" }
        if ($indexRun.status.phase -eq 'Succeeded') { break }
        [Threading.Tasks.Task]::Delay(1000).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    if ($indexRun.status.phase -ne 'Succeeded' -or [int64]$indexRun.status.documentCount -lt 2 -or [int64]$indexRun.status.indexedChunks -le 0) {
        throw "IndexRun did not complete real ingestion: phase=$($indexRun.status.phase), documents=$($indexRun.status.documentCount), chunks=$($indexRun.status.indexedChunks), message=$($indexRun.status.message)"
    }
    if ($indexRun.status.message -match 'skipped') { throw "IndexRun used the skipped path: $($indexRun.status.message)" }

    Write-Host "Standalone real ingestion passed: documents=$($kb.status.documentCount), chunks=$($kb.status.indexedChunks), rows=$rowCount; IndexRun chunks=$($indexRun.status.indexedChunks)." -ForegroundColor Green
}
finally {
    for ($index = $created.Count - 1; $index -ge 0; $index--) {
        $entry = $created[$index]
        try { Remove-Resource $entry.Plural $entry.Name } catch { Write-Warning "cleanup failed for $($entry.Plural)/$($entry.Name): $_" }
    }
    & kubectl exec -n $KognitionNamespace statefulset/kognition-kb-pgvector -- psql -U kognition_kb -d kognition_kb -c "DELETE FROM kognition_kb_chunks WHERE kb_namespace = '$namespace' AND kb_name = '$kbName';" 2>$null | Out-Null
}
