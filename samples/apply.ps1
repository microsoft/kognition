#!/usr/bin/env pwsh
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

<#
.SYNOPSIS
    Apply Kognition manifests to the API server (similar to kubectl apply).
.DESCRIPTION
    Reads a multi-document YAML file, converts each document to JSON,
    and POSTs it to the Kognition API server.
.PARAMETER Path
    Path to the YAML manifest file (supports multi-document with ---).
.PARAMETER Server
    API server base URL. Defaults to http://localhost:8080.
.PARAMETER DryRun
    Print the curl commands without executing them.
.EXAMPLE
    ./apply.ps1 -Path ../simple-agent/manifests.yaml
    ./apply.ps1 -Path ../full-stack-agent/manifests.yaml -Server http://kognition-apiserver:8080
#>
param(
    [Parameter(Mandatory = $true)]
    [string]$Path,

    [string]$Server = "http://localhost:8080",

    [switch]$DryRun
)

$ErrorActionPreference = "Stop"

# Check if powershell-yaml module is available
if (-not (Get-Module -ListAvailable -Name powershell-yaml)) {
    Write-Host "Installing powershell-yaml module..." -ForegroundColor Yellow
    Install-Module -Name powershell-yaml -Force -Scope CurrentUser
}
Import-Module powershell-yaml

# Read and split multi-document YAML
. (Join-Path $PSScriptRoot 'azure-settings.ps1')
$content = Read-AzureSampleManifest -Path $Path
$documents = $content -split "(?m)^---\s*$" | Where-Object { $_.Trim() -and -not ($_.Trim() -match "^#") }

# Map kind -> plural resource name
$kindToPlural = @{
    "Agent"                     = "agents"
    "AgentInstance"             = "agentinstances"
    "AgentJob"                  = "agentjobs"
    "Workflow"                  = "workflows"
    "WorkflowRun"              = "workflowruns"
    "Evaluation"                = "evaluations"
    "EvaluationDataset"         = "evaluationdatasets"
    "EvaluationRun"             = "evaluationruns"
    "MemoryClass"              = "memoryclasses"
    "MemoryRecord"             = "memoryrecords"
    "MemoryClaim"              = "memoryclaims"
    "MemoryVolume"             = "memoryvolumes"
    "KnowledgeBase"            = "knowledgebases"
    "ToolClass"                = "toolclasses"
    "Tool"                     = "tools"
    "ToolGrant"                = "toolgrants"
    "ModelClass"               = "modelclasses"
    "Model"                    = "models"
    "ModelInvocationProfile"   = "modelinvocationprofiles"
    "ModelGrant"               = "modelgrants"
    "AgentPolicy"              = "agentpolicies"
    "CognitiveAdmissionPolicy" = "cognitiveadmissionpolicies"
    "CognitiveQuota"           = "cognitivequotas"
    "CognitiveLimitRange"      = "cognitivelimitranges"
    "Workspace"                = "workspaces"
    "AgentIdentity"            = "agentidentities"
    "AgentRole"                = "agentroles"
    "AgentRoleBinding"         = "agentrolebindings"
    "AgentCommunicationPolicy" = "agentcommunicationpolicies"
    "MemoryGrant"              = "memorygrants"
    "AutonomyClass"            = "autonomyclasses"
    "TrustProfile"             = "trustprofiles"
    "AutonomyReview"           = "autonomyreviews"
    "AgentService"             = "agentservices"
    "IntentGateway"            = "intentgateways"
    "IntentRoute"              = "intentroutes"
    "Prompt"                   = "prompts"
    "SecretRef"                = "secretrefs"
    "Solution"                 = "solutions"
    "AgentAutoscaler"          = "agentautoscalers"
}

$success = 0
$failed = 0

foreach ($doc in $documents) {
    $trimmed = $doc.Trim()
    if (-not $trimmed) { continue }
    
    # Filter out comment-only documents
    $nonCommentLines = ($trimmed -split "`n") | Where-Object { $_ -and -not ($_.TrimStart().StartsWith("#")) }
    if (-not $nonCommentLines) { continue }

    try {
        $obj = ConvertFrom-Yaml $trimmed
    }
    catch {
        Write-Host "  SKIP: Could not parse YAML document" -ForegroundColor Yellow
        continue
    }

    if (-not $obj -or -not $obj.kind) { continue }

    $kind = $obj.kind
    $name = $obj.metadata.name
    $namespace = if ($obj.metadata.namespace) { $obj.metadata.namespace } else { "default" }

    $plural = $kindToPlural[$kind]
    if (-not $plural) {
        Write-Host "  UNKNOWN KIND: $kind (skipping)" -ForegroundColor Red
        $failed++
        continue
    }

    $url = "$Server/apis/kognition.dev/v1alpha1/namespaces/$namespace/$plural"
    $json = $obj | ConvertTo-Json -Depth 20 -Compress

    if ($DryRun) {
        Write-Host "  DRY-RUN: POST $url" -ForegroundColor Cyan
        Write-Host "    $kind/$name" -ForegroundColor Gray
    }
    else {
        Write-Host "  Applying $kind/$name ..." -NoNewline
        try {
            $null = Invoke-RestMethod -Uri $url -Method Post -ContentType "application/json" -Body $json -ErrorAction Stop
            Write-Host " created" -ForegroundColor Green
            $success++
        }
        catch {
            $statusCode = $_.Exception.Response.StatusCode.value__
            if ($statusCode -eq 409) {
                # Already exists — GET current to obtain resourceVersion, then PUT
                $updateUrl = "$url/$name"
                try {
                    $existing = Invoke-RestMethod -Uri $updateUrl -Method Get -ErrorAction Stop
                    $rv = $existing.metadata.resourceVersion
                    # Inject resourceVersion into the object for optimistic concurrency
                    if (-not $obj.metadata) { $obj.metadata = @{} }
                    $obj.metadata["resourceVersion"] = [int64]$rv
                    $updateJson = $obj | ConvertTo-Json -Depth 20 -Compress
                    $response = Invoke-RestMethod -Uri $updateUrl -Method Put -ContentType "application/json" -Body $updateJson -ErrorAction Stop
                    Write-Host " updated" -ForegroundColor Yellow
                    $success++
                }
                catch {
                    Write-Host " FAILED (update): $($_.Exception.Message)" -ForegroundColor Red
                    $failed++
                }
            }
            else {
                Write-Host " FAILED: $($_.Exception.Message)" -ForegroundColor Red
                $failed++
            }
        }
    }
}

Write-Host ""
Write-Host "Done: $success succeeded, $failed failed" -ForegroundColor $(if ($failed -gt 0) { "Yellow" } else { "Green" })
