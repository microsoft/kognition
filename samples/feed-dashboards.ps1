#!/usr/bin/env pwsh
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

<#
.SYNOPSIS
    Fires a batch of WorkflowRuns to feed metrics dashboards with fresh
    activity.

.DESCRIPTION
    Creates several WorkflowRun resources against workflows that already
    exist in the `default` namespace. Each run drives a chain of agent
    invocations, producing token / latency / phase-transition metrics that
    the observability dashboards consume.

    Names are timestamp-suffixed so the script is safe to re-run.
#>
param(
    [string]$Namespace = "default",
    [int]$WaitMinutes = 6
)

$ErrorActionPreference = "Stop"
$stamp = Get-Date -Format "yyMMdd-HHmmss"

$runs = @(
    @{ workflow = "content-pipeline";         input = "the rise of agentic AI in enterprise operations" }
    @{ workflow = "content-pipeline";         input = "kubernetes-native governance for autonomous agents" }
    @{ workflow = "research-and-summarize";   input = "OpenTelemetry semantic conventions for GenAI workloads" }
    @{ workflow = "research-and-summarize";   input = "cost attribution patterns for multi-tenant LLM platforms" }
    @{ workflow = "mvp1-five-agent";          input = "observability blueprint for agent fleets" }
    @{ workflow = "mvp1-five-agent";          input = "trust boundaries between agents and tools" }
    @{ workflow = "write-blog-post-workflow"; input = "why declarative agents are the next Kubernetes moment" }
    @{ workflow = "analyze-dataset-workflow"; input = "quarterly agent-run telemetry across production namespaces" }
    @{ workflow = "wfdemo-parallel";          input = "the value of parallel dispatch in agent workflows" }
    @{ workflow = "wfdemo-conditional";       input = "My credit card was charged twice for the same order." }
    @{ workflow = "wfdemo-conditional";       input = "The application keeps crashing when I open the settings tab." }
    @{ workflow = "wfdemo-shared-memory";     input = "Kognition is Kubernetes for agents" }
)

$applied = @()
$failed  = @()

foreach ($r in $runs) {
    $sanitized = ($r.workflow -replace '[^a-z0-9]', '')
    $short     = $sanitized.Substring(0, [Math]::Min($sanitized.Length, 20))
    $name      = "load-$short-$stamp-$([guid]::NewGuid().ToString('N').Substring(0,4))"
    $doc    = @"
apiVersion: kognition.dev/v1alpha1
kind: WorkflowRun
metadata:
  name: $name
  namespace: $Namespace
  labels:
    kognition.dev/driver: metrics-workload
    workflow.kognition.dev/name: $($r.workflow)
spec:
  workflowRef:
    name: $($r.workflow)
  input: |
    $($r.input)
"@
    $tmp = New-TemporaryFile
    try {
        Set-Content -Path $tmp -Value $doc -Encoding UTF8
        Write-Host "Applying WorkflowRun $name ($($r.workflow))" -ForegroundColor Cyan
        $null = kubectl apply -f $tmp 2>&1
        if ($LASTEXITCODE -eq 0) {
            $applied += [pscustomobject]@{ name = $name; workflow = $r.workflow }
        } else {
            $failed  += [pscustomobject]@{ name = $name; workflow = $r.workflow }
        }
    } finally {
        Remove-Item $tmp -Force -ErrorAction SilentlyContinue
    }
}

Write-Host ""
Write-Host "Applied $($applied.Count) WorkflowRun(s); $($failed.Count) failed." -ForegroundColor Green

if ($applied.Count -eq 0) { exit 1 }

$deadline = (Get-Date).AddMinutes($WaitMinutes)
while ((Get-Date) -lt $deadline) {
    $status = kubectl get workflowrun -n $Namespace -l kognition.dev/driver=metrics-workload `
        -o jsonpath='{range .items[*]}{.metadata.name}={.status.phase}{"\n"}{end}' 2>$null
    if (-not $status) { Start-Sleep -Seconds 4; continue }
    $rows = $status -split "`n" | Where-Object { $_ }
    $terminal = 0
    $active   = 0
    foreach ($row in $rows) {
        $phase = ($row -split '=')[1]
        if ($phase -in @('Ready','Succeeded','Failed','Error','Cancelled')) { $terminal++ }
        else { $active++ }
    }
    Write-Host "  waiting... terminal=$terminal active=$active" -ForegroundColor DarkGray
    if ($active -eq 0 -and $terminal -eq $rows.Count) { break }
    Start-Sleep -Seconds 8
}

Write-Host ""
Write-Host "Final phases:" -ForegroundColor Green
kubectl get workflowrun -n $Namespace -l kognition.dev/driver=metrics-workload `
    -o=custom-columns=NAME:.metadata.name,WORKFLOW:.spec.workflowRef.name,PHASE:.status.phase,AGE:.metadata.creationTimestamp
