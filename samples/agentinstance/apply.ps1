# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

$ErrorActionPreference = 'Stop'
. (Join-Path $PSScriptRoot '..\azure-settings.ps1')
$namespace = Get-AzureSampleNamespace

$agent = & kubectl get agent hello-agent -n $namespace -o name 2>$null
if (-not $agent) {
    throw "Agent 'hello-agent' is required. Install the simple-agent sample first."
}

# AgentInstances are one-shot invocations. Recreate the fixed-name example so
# repeated sample installs trigger a fresh run instead of preserving a terminal result.
$existingJSON = (& kubectl get agentinstance hello-run-1 -n $namespace --ignore-not-found -o json 2>$null | Out-String).Trim()
if ($LASTEXITCODE -ne 0) { throw 'Failed to inspect the previous AgentInstance.' }
if ($existingJSON -and (Get-AzureSampleProfile)) {
    $existing = $existingJSON | ConvertFrom-Json
    if ($existing.spec.agentRef.name -ne 'hello-agent' -or
        $existing.metadata.labels.'kognition.dev/sample-bundle' -ne 'compatible') {
        throw 'Refusing to replace hello-run-1: the existing invocation is not owned by this sample bundle.'
    }
    if ($existing.status.phase -in @('Failed', 'Error')) {
        throw "The original hello-run-1 outcome is $($existing.status.phase); preserve it for diagnosis."
    }
    Write-Host "Preserving the original sample invocation hello-run-1 ($($existing.status.phase))."
    return
}
if (-not (Get-AzureSampleProfile)) {
    & kubectl delete agentinstance hello-run-1 -n $namespace --ignore-not-found --wait=true
    if ($LASTEXITCODE -ne 0) { throw "Failed to delete the previous AgentInstance." }
}

Invoke-AzureSampleApply -Path (Join-Path $PSScriptRoot 'manifests.yaml')
if ($LASTEXITCODE -ne 0) { throw "Failed to apply the AgentInstance sample." }