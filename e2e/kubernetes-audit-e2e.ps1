# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

param(
    [string]$ControllerNamespace = "kognition-system",
    [string]$ReleaseName = "kognition"
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$runID = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
$namespace = "audit-e2e-$runID"
$promptName = "audit-prompt"
$memoryClassName = "audit-memory-class-$runID"
$volumeHandle = "audit-volume-$runID"
$portForward = $null

function Invoke-Kubectl {
    param([Parameter(Mandatory = $true)][string[]]$Arguments)

    $output = & kubectl @Arguments 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "kubectl $($Arguments -join ' ') failed:`n$($output -join "`n")"
    }
    return $output
}

function Get-AvailableTcpPort {
    $listener = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback, 0)
    try {
        $listener.Start()
        return ([System.Net.IPEndPoint]$listener.LocalEndpoint).Port
    }
    finally {
        $listener.Stop()
    }
}

function Get-AuditEntries {
    $configMaps = (& kubectl get configmaps -n $ControllerNamespace -l 'kognition.dev/cognitive-audit=true' -o json | ConvertFrom-Json).items
    $entries = @()
    foreach ($configMap in $configMaps) {
        $encoded = $configMap.binaryData.'entries.json'
        if (-not $encoded) { continue }
        $entries += @([Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($encoded)) | ConvertFrom-Json)
    }
    return @($entries)
}

function Wait-ForAuditEntry {
    param([Parameter(Mandatory = $true)][scriptblock]$Predicate)

    $deadline = [DateTime]::UtcNow.AddSeconds(45)
    do {
        $entry = Get-AuditEntries | Where-Object $Predicate | Select-Object -First 1
        if ($entry) { return $entry }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Timed out waiting for the expected durable audit entry"
}

function Wait-ForHttpStatus {
    param([Parameter(Mandatory = $true)][string]$Uri)

    $deadline = [DateTime]::UtcNow.AddSeconds(30)
    do {
        try {
            $response = Invoke-WebRequest -Uri $Uri -Method Get -SkipHttpErrorCheck -TimeoutSec 2
            if ($response.StatusCode -eq 200) { return }
        }
        catch {
        }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "$Uri did not become healthy"
}

Write-Host "=== Kognition Kubernetes Audit E2E ===" -ForegroundColor Cyan

try {
    Invoke-Kubectl @("wait", "-n", $ControllerNamespace, "--for=condition=Available", "deployment/$ReleaseName-k8s-controller", "--timeout=120s") | Out-Null
    Invoke-Kubectl @("wait", "-n", $ControllerNamespace, "--for=condition=Available", "deployment/$ReleaseName-memory-service", "--timeout=120s") | Out-Null
    Invoke-Kubectl @("create", "namespace", $namespace) | Out-Null

    @"
apiVersion: kognition.dev/v1alpha1
kind: Prompt
metadata:
  name: $promptName
  namespace: $namespace
spec:
  content: "Audit the Kubernetes mutation boundary."
"@ | & kubectl apply -f - | Out-Host
    if ($LASTEXITCODE -ne 0) { throw "Prompt apply failed" }

    $mutationEntry = Wait-ForAuditEntry { $_.subjectKind -eq 'Prompt' -and $_.subjectRef -eq "$namespace/$promptName" -and $_.verb -eq 'attempt_create' }
    if ($mutationEntry.workspace -ne $namespace -or $mutationEntry.eventType -ne 'admission_attempt') {
        throw "Unexpected mutation audit entry: $($mutationEntry | ConvertTo-Json -Compress)"
    }
    $mutationEntries = @(Get-AuditEntries | Where-Object { $_.subjectKind -eq 'Prompt' -and $_.subjectRef -eq "$namespace/$promptName" -and $_.verb -eq 'attempt_create' })
    if ($mutationEntries.Count -ne 1) {
        throw "Expected one Prompt admission-attempt record, found $($mutationEntries.Count)"
    }

    @"
apiVersion: kognition.dev/v1alpha1
kind: MemoryClass
metadata:
  name: $memoryClassName
spec:
  backendType: inmemory
"@ | & kubectl apply -f - | Out-Host
    if ($LASTEXITCODE -ne 0) { throw "MemoryClass apply failed" }
    $clusterEntry = Wait-ForAuditEntry { $_.subjectKind -eq 'MemoryClass' -and $_.subjectRef -eq "/$memoryClassName" -and $_.verb -eq 'attempt_create' }
    if ($clusterEntry.workspace -ne 'cluster' -or $clusterEntry.eventType -ne 'admission_attempt') {
        throw "Unexpected cluster-scoped audit entry: $($clusterEntry | ConvertTo-Json -Compress)"
    }

    $tokenBase64 = (& kubectl get secret "$ReleaseName-memory-policy-gateway" -n $ControllerNamespace '-o=jsonpath={.data.upstream-token}' | Out-String).Trim()
    if (-not $tokenBase64) { throw "Memory policy gateway Secret has no upstream token" }
    $token = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($tokenBase64))
    $port = Get-AvailableTcpPort
    $portForward = Start-Job -ScriptBlock {
        param($Namespace, $Service, $Port)
        kubectl -n $Namespace port-forward "service/$Service" "${Port}:8090"
    } -ArgumentList $ControllerNamespace, "$ReleaseName-memory-service", $port
    $url = "http://127.0.0.1:$port"
    Wait-ForHttpStatus -Uri "$url/v1/memory/health"

    $headers = @{ Authorization = "Bearer $token" }
    $provision = Invoke-WebRequest -Uri "$url/v1/memory/provision" -Method Post -Headers $headers -ContentType 'application/json' -Body (@{ volume_handle = $volumeHandle } | ConvertTo-Json -Compress) -SkipHttpErrorCheck
    if ($provision.StatusCode -ne 201) { throw "Memory provision status=$($provision.StatusCode) body=$($provision.Content)" }
    $store = Invoke-WebRequest -Uri "$url/v1/memory/store" -Method Post -Headers $headers -ContentType 'application/json' -Body (@{ volume_handle = $volumeHandle; id = 'audit-record'; content = 'audit-safe-content' } | ConvertTo-Json -Compress) -SkipHttpErrorCheck
    if ($store.StatusCode -ne 201) { throw "Memory store status=$($store.StatusCode) body=$($store.Content)" }

    $memoryEntry = Wait-ForAuditEntry { $_.eventType -eq 'memory_op' -and $_.workspace -eq $volumeHandle -and $_.verb -eq 'store' }
    if ($memoryEntry.subjectRef -ne "$volumeHandle/audit-record") {
        throw "Unexpected memory audit entry: $($memoryEntry | ConvertTo-Json -Compress)"
    }
    if ($memoryEntry.agentIdentity) {
        throw "Raw memory service writes must not accept a caller-supplied AgentIdentity: $($memoryEntry | ConvertTo-Json -Compress)"
    }

    Write-Host "Kubernetes admission attempts and memory writes persisted immutable audit records." -ForegroundColor Green
    Write-Host "=== PASS ===" -ForegroundColor Green
}
finally {
    if ($portForward) {
        Stop-Job $portForward -ErrorAction SilentlyContinue
        Remove-Job $portForward -Force -ErrorAction SilentlyContinue
    }
    if ($namespace) {
        & kubectl delete namespace $namespace --ignore-not-found --wait=false 2>$null | Out-Null
    }
    if ($memoryClassName) {
        & kubectl delete memoryclass $memoryClassName --ignore-not-found 2>$null | Out-Null
    }
}
