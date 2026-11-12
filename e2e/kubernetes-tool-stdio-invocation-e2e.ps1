# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

param(
    [string]$KubeContext = "",
    [string]$ControllerNamespace = "kognition-system",
    [string]$ReleaseName = "kognition",
    [switch]$KeepResourcesOnFailure
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$runID = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
$namespace = "tool-stdio-e2e-$runID"
$toolClassName = "stdio-shell-$runID"
$toolName = "stdio-shell"
$runName = "stdio-invocation"
$succeeded = $false

function Invoke-Kubectl {
    param([Parameter(Mandatory = $true)][string[]]$Arguments)
    $contextArguments = @("--request-timeout=10s")
    if ($KubeContext) { $contextArguments = @("--context", $KubeContext, "--request-timeout=10s") }
    $output = & kubectl @contextArguments @Arguments 2>&1
    if ($LASTEXITCODE -ne 0) { throw "kubectl $($Arguments -join ' ') failed:`n$($output -join "`n")" }
    return $output
}

function Apply-KubernetesObject {
    param([Parameter(Mandatory = $true)][hashtable]$Object)
    $contextArguments = @()
    if ($KubeContext) { $contextArguments = @("--context", $KubeContext) }
    $Object | ConvertTo-Json -Depth 30 -Compress | & kubectl @contextArguments apply -f - 2>&1 | Out-Host
    if ($LASTEXITCODE -ne 0) { throw "apply $($Object.kind)/$($Object.metadata.name) failed" }
}

function Wait-ForJsonPath {
    param([Parameter(Mandatory = $true)][string]$Resource, [Parameter(Mandatory = $true)][string]$JsonPath, [Parameter(Mandatory = $true)][string]$Expected, [string]$ResourceNamespace = "", [int]$TimeoutSeconds = 120)
    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        $arguments = @("get", $Resource, "-o=jsonpath=$JsonPath")
        if ($ResourceNamespace) { $arguments += @("-n", $ResourceNamespace) }
        try {
            $value = (Invoke-Kubectl $arguments | Out-String).Trim()
            if ($value -eq $Expected) { return }
            if ($Resource -like "toolinvocationrun/*") {
                $phase = (Invoke-Kubectl @("get", $Resource, "-n", $ResourceNamespace, "-o=jsonpath={.status.phase}") | Out-String).Trim()
                if ($phase -eq "Failed") { throw "$Resource failed: $(Invoke-Kubectl @("get", $Resource, "-n", $ResourceNamespace, "-o=jsonpath={.status.errorCode}{': '}{.status.errorMessage}"))" }
            }
        } catch { throw }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "$Resource did not reach $JsonPath=$Expected within $TimeoutSeconds seconds"
}

function Assert-NoSandboxResources {
    $deadline = [DateTime]::UtcNow.AddSeconds(30)
    do {
        $jobs = (Invoke-Kubectl @("get", "jobs", "-n", $namespace, "-l", "kognition.dev/managed=stdio-invocation", "-o", "json") | Out-String | ConvertFrom-Json).items
        $policies = (Invoke-Kubectl @("get", "networkpolicies", "-n", $namespace, "-o", "json") | Out-String | ConvertFrom-Json).items | Where-Object { $_.metadata.generateName -like "kognition-stdio-invocation-*" }
        if (@($jobs).Count -eq 0 -and @($policies).Count -eq 0) { return }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Stdio sandbox resources were not cleaned up: jobs=$(@($jobs).Count) policies=$(@($policies).Count)"
}

Write-Host "=== Kognition Kubernetes Tool Stdio Invocation E2E ===" -ForegroundColor Cyan

try {
    Invoke-Kubectl @("get", "deployment/$ReleaseName-k8s-controller", "-n", $ControllerNamespace) | Out-Null
    Invoke-Kubectl @("wait", "-n", $ControllerNamespace, "--for=condition=Available", "deployment/$ReleaseName-k8s-controller", "--timeout=120s") | Out-Null
    Invoke-Kubectl @("create", "namespace", $namespace) | Out-Null

    $server = @'
import json, os, socket, sys
assert os.geteuid() == 65532
connection = socket.socket(); connection.settimeout(2)
assert connection.connect_ex(("1.1.1.1", 443)) != 0, "sandbox network unexpectedly reachable"
for line in sys.stdin:
    request = json.loads(line)
    if request.get("method") == "initialize":
        print(json.dumps({"jsonrpc":"2.0","id":request["id"],"result":{"protocolVersion":"2025-03-26"}}), flush=True)
    elif request.get("method") == "tools/call":
        print(json.dumps({"jsonrpc":"2.0","id":request["id"],"result":{"content":[],"status":"ok"}}), flush=True)
        break
'@
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "ToolClass"; metadata = @{ name = $toolClassName }; spec = @{ provisioner = "mcp"; executionTier = "pod"; sandboxImage = "python:3.12-alpine"; stdioCommand = @("/usr/local/bin/python", "-u", "-c", $server); sandboxDefaults = @{ timeout = "15s"; cpuMilli = 100; memoryMB = 64 }; resourceLimits = @{ maxCpuMilli = 250; maxMemoryMB = 128; maxTimeout = "15s" } } }
    Wait-ForJsonPath -Resource "toolclass/$toolClassName" -JsonPath "{.status.phase}" -Expected "Ready"
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "Tool"; metadata = @{ name = $toolName; namespace = $namespace }; spec = @{ className = $toolClassName; protocol = "shell"; description = "Class-owned MCP stdio Tool." } }
    Wait-ForJsonPath -Resource "tool/$toolName" -ResourceNamespace $namespace -JsonPath "{.status.phase}" -Expected "Ready"
    $version = (Invoke-Kubectl @("get", "tool/$toolName", "-n", $namespace, "-o=jsonpath={.metadata.resourceVersion}")) -join ""
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "ToolInvocationRun"; metadata = @{ name = $runName; namespace = $namespace }; spec = @{ toolName = $toolName; toolResourceVersion = [int64]$version; invocationID = "stdio-$runID"; input = @{ command = "status" } } }
    Wait-ForJsonPath -Resource "toolinvocationrun/$runName" -ResourceNamespace $namespace -JsonPath "{.status.phase}" -Expected "Ready" -TimeoutSeconds 150
    $run = (Invoke-Kubectl @("get", "toolinvocationrun/$runName", "-n", $namespace, "-o", "json") | Out-String | ConvertFrom-Json)
    if ($run.status.executionMode -ne "sandbox" -or $run.status.output.status -ne "ok") { throw "ToolInvocationRun status is incomplete: $($run.status | ConvertTo-Json -Compress)" }
    Assert-NoSandboxResources
    Write-Host "Class-owned MCP stdio Tools ran in non-root, no-egress Kubernetes Jobs and persisted normalized ToolInvocationRun results." -ForegroundColor Green
    Write-Host "=== PASS ===" -ForegroundColor Green
    $succeeded = $true
}
finally {
    $contextArguments = @(); if ($KubeContext) { $contextArguments = @("--context", $KubeContext) }
    if ($succeeded -or -not $KeepResourcesOnFailure) {
        if ($namespace) { & kubectl @contextArguments delete namespace $namespace --ignore-not-found --wait=false 2>$null | Out-Null }
        if ($toolClassName) { & kubectl @contextArguments delete "toolclass/$toolClassName" --ignore-not-found --wait=false 2>$null | Out-Null }
    }
}