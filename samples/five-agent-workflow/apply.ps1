# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

$ErrorActionPreference = "Stop"
. (Join-Path $PSScriptRoot '..\azure-settings.ps1')

$scenario = Join-Path $PSScriptRoot "scenario.yaml"
$namespace = Get-AzureSampleNamespace
$agentSelector = "app.kubernetes.io/part-of=mvp1-five-agent-workflow"
$runName = "mvp1-five-agent-run"

Write-Host "== Applying five-agent workflow resources"
Invoke-AzureSampleApply -Path $scenario
if ($LASTEXITCODE -ne 0) { throw "Five-agent workflow apply failed" }

Write-Host "== Waiting for the five workflow Agents to become ready"
& kubectl wait --for=condition=Ready=True agent -n $namespace -l $agentSelector --timeout=300s
if ($LASTEXITCODE -ne 0) { throw "Five-agent workflow Agents did not become ready" }

$runPhase = (& kubectl get workflowrun $runName -n $namespace -o jsonpath='{.status.phase}' 2>$null | Out-String).Trim()
if ($runPhase -in @("Failed", "Error")) {
    throw "WorkflowRun $runName is $runPhase; preserve it for diagnosis and create a separate verification run"
}