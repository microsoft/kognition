# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# Validates TC-0143: one kognitionctl apply creates and runs a five-agent workflow.

[CmdletBinding()]
param(
    [string]$KubernetesContext = "docker-desktop",
    [string]$ModelName = "gpt-4-1",
    [int]$TimeoutSeconds = 900,
    [switch]$KeepResources
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$repoRoot = Split-Path -Parent $PSScriptRoot
$sourceScenario = Join-Path $repoRoot "samples\five-agent-workflow\scenario.yaml"
$tempRoot = Join-Path ([System.IO.Path]::GetTempPath()) ("kognition-mvp1-" + [guid]::NewGuid().ToString("N"))
$tempScenario = Join-Path $tempRoot "scenario.yaml"
$cli = Join-Path $tempRoot "kognitionctl.exe"
$token = [guid]::NewGuid().ToString("N").Substring(0, 8)
$prefix = "mvp1-$token"
$runName = "$prefix-five-agent-run"
$label = "app.kubernetes.io/part-of=$prefix-five-agent-workflow"

function Invoke-Kubectl {
    param([Parameter(ValueFromRemainingArguments = $true)][string[]]$Arguments)

    $output = & kubectl --context $KubernetesContext @Arguments 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "kubectl $($Arguments -join ' ') failed:`n$($output -join [Environment]::NewLine)"
    }
    return $output
}

function Get-WorkflowRun {
    $json = & kubectl --context $KubernetesContext get workflowrun $runName -n default -o json 2>$null
    if ($LASTEXITCODE -ne 0 -or [string]::IsNullOrWhiteSpace(($json -join ""))) {
        return $null
    }
    return ($json -join [Environment]::NewLine) | ConvertFrom-Json
}

New-Item -ItemType Directory -Path $tempRoot -Force | Out-Null
$applied = $false

try {
    Write-Host "=== MVP-1 Five-Agent Workflow E2E ===" -ForegroundColor Cyan

    $null = Invoke-Kubectl get "model/$ModelName" --namespace=default --output=name
    Write-Host "Prerequisite available: model/$ModelName" -ForegroundColor DarkGray

    $availableResources = @(Invoke-Kubectl api-resources --api-group=kognition.dev --output=name)
    $requiredCRDs = @("agents.kognition.dev", "agentcommunicationpolicies.kognition.dev", "modelgrants.kognition.dev", "workflows.kognition.dev", "workflowruns.kognition.dev")
    foreach ($resource in $requiredCRDs) {
        if ($availableResources -notcontains $resource) {
            throw "Required resource API '$resource' is unavailable"
        }
    }

    $scenario = Get-Content -LiteralPath $sourceScenario -Raw
    $scenario = $scenario.Replace("mvp1-", "$prefix-")
    $scenario = $scenario.Replace("gpt-4-1", $ModelName)
    Set-Content -LiteralPath $tempScenario -Value $scenario -Encoding utf8

    Write-Host "Building kognitionctl from the current source tree..." -ForegroundColor Yellow
    & go build -o $cli ./cmd/kognitionctl
    if ($LASTEXITCODE -ne 0) {
        throw "Failed to build kognitionctl"
    }

    Write-Host "Applying the complete scenario with one kognitionctl invocation..." -ForegroundColor Yellow
    $applyOutput = & $cli --mode kubernetes --kube-context $KubernetesContext apply -f $tempScenario 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "kognitionctl apply failed:`n$($applyOutput -join [Environment]::NewLine)"
    }
    $applied = $true
    $applyOutput | ForEach-Object { Write-Host "  $_" }

    $appliedResources = @($applyOutput | Where-Object { $_ -match " applied$" })
    if ($appliedResources.Count -ne 13) {
        throw "Expected one command to apply 13 YAML documents; observed $($appliedResources.Count)"
    }

    Write-Host "Waiting for WorkflowRun $runName..." -ForegroundColor Yellow
    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    $run = $null
    do {
        $run = Get-WorkflowRun
        $phase = if ($null -ne $run -and $null -ne $run.status) { [string]$run.status.phase } else { "" }
        if ($phase -in @("Ready", "Failed")) {
            break
        }
        Start-Sleep -Seconds 5
    } while ([DateTime]::UtcNow -lt $deadline)

    if ($null -eq $run -or [string]$run.status.phase -ne "Ready") {
        Write-Host "Workflow diagnostics:" -ForegroundColor Red
        & kubectl --context $KubernetesContext get workflowrun $runName -n default -o yaml 2>&1 | ForEach-Object { Write-Host $_ }
        & kubectl --context $KubernetesContext get agentjobs,agentinstances -n default -l $label -o wide 2>&1 | ForEach-Object { Write-Host $_ }
        throw "WorkflowRun did not reach Ready before the timeout"
    }

    $expectedSteps = @("plan", "research", "analyze", "write", "review")
    $completedSteps = @($run.status.completedSteps)
    foreach ($step in $expectedSteps) {
        if ($completedSteps -notcontains $step) {
            throw "Expected completed step '$step'; completed steps: $($completedSteps -join ', ')"
        }
    }
    if ($completedSteps.Count -ne $expectedSteps.Count) {
        throw "Expected exactly five completed steps; observed $($completedSteps.Count)"
    }

    $stepStatuses = @($run.status.stepStatuses)
    foreach ($step in $expectedSteps) {
        $status = @($stepStatuses | Where-Object { $_.name -eq $step })
        if ($status.Count -ne 1 -or [string]::IsNullOrWhiteSpace([string]$status[0].output)) {
            throw "Step '$step' did not record one non-empty output"
        }
    }

    Write-Host "MVP-1 PASSED: one apply processed 13 documents and five agents completed end-to-end." -ForegroundColor Green
}
finally {
    if ($applied -and -not $KeepResources) {
        Write-Host "Cleaning up MVP-1 resources..." -ForegroundColor Yellow
        & kubectl --context $KubernetesContext delete -f $tempScenario --ignore-not-found --wait=false 2>&1 | ForEach-Object { Write-Host "  $_" }
    }
    if (Test-Path -LiteralPath $tempRoot) {
        Remove-Item -LiteralPath $tempRoot -Recurse -Force -ErrorAction SilentlyContinue
    }
}
