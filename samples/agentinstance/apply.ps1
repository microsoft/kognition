# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

$ErrorActionPreference = 'Stop'

$agent = & kubectl get agent hello-agent -n default -o name 2>$null
if (-not $agent) {
    throw "Agent 'hello-agent' is required. Install the simple-agent sample first."
}

# AgentInstances are one-shot invocations. Recreate the fixed-name example so
# repeated sample installs trigger a fresh run instead of preserving a terminal result.
& kubectl delete agentinstance hello-run-1 -n default --ignore-not-found --wait=true
if ($LASTEXITCODE -ne 0) { throw "Failed to delete the previous AgentInstance." }

& kubectl apply -f (Join-Path $PSScriptRoot 'manifests.yaml')
if ($LASTEXITCODE -ne 0) { throw "Failed to apply the AgentInstance sample." }