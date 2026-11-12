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
$namespace = "tool-exec-probe-e2e-$runID"
$toolClassName = "exec-probe-$runID"
$toolName = "exec-probe"
$succeeded = $false

function Invoke-Kubectl {
    param([Parameter(Mandatory = $true)][string[]]$Arguments)

    $contextArguments = @("--request-timeout=10s")
    if ($KubeContext) { $contextArguments = @("--context", $KubeContext, "--request-timeout=10s") }
    $output = & kubectl @contextArguments @Arguments 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "kubectl $($Arguments -join ' ') failed:`n$($output -join "`n")"
    }
    return $output
}

function Apply-KubernetesObject {
    param([Parameter(Mandatory = $true)][hashtable]$Object)

    $contextArguments = @()
    if ($KubeContext) { $contextArguments = @("--context", $KubeContext) }
    $Object | ConvertTo-Json -Depth 30 -Compress | & kubectl @contextArguments apply -f - 2>&1 | Out-Host
    if ($LASTEXITCODE -ne 0) {
        throw "apply $($Object.kind)/$($Object.metadata.name) failed"
    }
}

function Wait-ForPhase {
    param(
        [Parameter(Mandatory = $true)][string]$Resource,
        [Parameter(Mandatory = $true)][string]$Expected,
        [string]$ResourceNamespace = "",
        [int]$TimeoutSeconds = 90
    )

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        $arguments = @("get", $Resource, "-o=jsonpath={.status.phase}")
        if ($ResourceNamespace) { $arguments += @("-n", $ResourceNamespace) }
        try {
            $value = (Invoke-Kubectl $arguments | Out-String).Trim()
            if ($value -eq $Expected) { return }
        }
        catch {
        }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "$Resource did not reach phase $Expected within $TimeoutSeconds seconds"
}

function Assert-NoExecProbeResources {
    $deadline = [DateTime]::UtcNow.AddSeconds(30)
    do {
        $jobs = (Invoke-Kubectl @("get", "jobs", "-n", $namespace, "-l", "kognition.dev/managed=exec-probe", "-o", "json") | Out-String | ConvertFrom-Json).items
        $policies = (Invoke-Kubectl @("get", "networkpolicies", "-n", $namespace, "-o", "json") | Out-String | ConvertFrom-Json).items |
            Where-Object { $_.metadata.generateName -like "kognition-exec-probe-*" }
        if (@($jobs).Count -eq 0 -and @($policies).Count -eq 0) { return }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Exec probe resources were not cleaned up: jobs=$(@($jobs).Count) policies=$(@($policies).Count)"
}

Write-Host "=== Kognition Kubernetes Tool Exec Probe E2E ===" -ForegroundColor Cyan

try {
    Invoke-Kubectl @("get", "deployment/$ReleaseName-k8s-controller", "-n", $ControllerNamespace) | Out-Null
    Invoke-Kubectl @("wait", "-n", $ControllerNamespace, "--for=condition=Available", "deployment/$ReleaseName-k8s-controller", "--timeout=120s") | Out-Null
    Invoke-Kubectl @("create", "namespace", $namespace) | Out-Null

    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "ToolClass"; metadata = @{ name = $toolClassName }; spec = @{ provisioner = "mcp"; executionTier = "pod"; transport = "http"; sandboxImage = "python:3.12-alpine"; sandboxDefaults = @{ timeout = "10s"; cpuMilli = 100; memoryMB = 64 }; resourceLimits = @{ maxCpuMilli = 250; maxMemoryMB = 128; maxTimeout = "10s" } } }
    Wait-ForPhase -Resource "toolclass/$toolClassName" -Expected "Ready"

    $successCommand = @("/usr/local/bin/python", "-c", "import os, socket`nassert os.geteuid() == 65532`ntry: socket.create_connection(('1.1.1.1', 443), timeout=2)`nexcept OSError: pass`nelse: raise AssertionError('sandbox network unexpectedly reachable')")
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "Tool"; metadata = @{ name = $toolName; namespace = $namespace }; spec = @{ className = $toolClassName; protocol = "MCP"; endpoint = "https://example.com/mcp"; healthProbe = @{ type = "exec"; command = $successCommand; interval = "30s"; timeout = "10s"; failureThreshold = 1; successThreshold = 1 } } }
    Wait-ForPhase -Resource "tool/$toolName" -ResourceNamespace $namespace -Expected "Ready"
    Assert-NoExecProbeResources

    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "Tool"; metadata = @{ name = $toolName; namespace = $namespace }; spec = @{ className = $toolClassName; protocol = "MCP"; endpoint = "https://example.com/mcp"; healthProbe = @{ type = "exec"; command = @("/usr/local/bin/python", "-c", "raise SystemExit(7)"); interval = "30s"; timeout = "10s"; failureThreshold = 1; successThreshold = 1 } } }
    Wait-ForPhase -Resource "tool/$toolName" -ResourceNamespace $namespace -Expected "Degraded"
    Assert-NoExecProbeResources

    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "Tool"; metadata = @{ name = $toolName; namespace = $namespace }; spec = @{ className = $toolClassName; protocol = "MCP"; endpoint = "https://example.com/mcp"; healthProbe = @{ type = "exec"; command = @("/usr/local/bin/python", "-c", "import time; time.sleep(30)"); interval = "30s"; timeout = "10s"; failureThreshold = 1; successThreshold = 1 } } }
    Wait-ForPhase -Resource "tool/$toolName" -ResourceNamespace $namespace -Expected "Degraded"
    Assert-NoExecProbeResources

    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "Tool"; metadata = @{ name = $toolName; namespace = $namespace }; spec = @{ className = $toolClassName; protocol = "MCP"; endpoint = "https://example.com/mcp"; healthProbe = @{ type = "exec"; command = $successCommand; interval = "30s"; timeout = "10s"; failureThreshold = 1; successThreshold = 1 } } }
    Wait-ForPhase -Resource "tool/$toolName" -ResourceNamespace $namespace -Expected "Ready"
    Assert-NoExecProbeResources

    Write-Host "Class-approved exec probes ran in one-shot non-root, no-egress sandbox Jobs, degraded on nonzero exit and deadline expiry, recovered on success, and cleaned up their Jobs and NetworkPolicies." -ForegroundColor Green
    Write-Host "=== PASS ===" -ForegroundColor Green
    $succeeded = $true
}
finally {
    $contextArguments = @()
    if ($KubeContext) { $contextArguments = @("--context", $KubeContext) }
    if ($succeeded -or -not $KeepResourcesOnFailure) {
        if ($namespace) { & kubectl @contextArguments delete namespace $namespace --ignore-not-found --wait=false 2>$null | Out-Null }
        if ($toolClassName) { & kubectl @contextArguments delete "toolclass/$toolClassName" --ignore-not-found --wait=false 2>$null | Out-Null }
    }
}