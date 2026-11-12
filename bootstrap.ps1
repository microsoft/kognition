#!/usr/bin/env pwsh
# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

<#
.SYNOPSIS
    One-command bootstrap for a local Kognition environment with runnable,
    testable samples (including the real, pgvector-backed knowledge bases).

.DESCRIPTION
    Runs the full happy path end to end:
      1. Checks prerequisites (docker, kind, kubectl, helm, go, python).
      2. Verifies the repo-root .env (Azure SP + Foundry credentials).
      3. Builds the container images from source (`make docker`).
      4. Installs the platform in kubernetes mode — this kind-loads the images,
         enables the pgvector + KB ingester pipeline, and injects foundry-creds
         from .env (`make install MODE=kubernetes`).
      5. Installs every sample, including the script-based knowledge bases
         (`make samples-install`).
      6. Heals any KnowledgeBase that lost a startup race (resets Failed → retry)
         and waits for the KBs to reach Ready.

    Re-runnable and idempotent.

.PARAMETER SkipBuild
    Skip `make docker`. Use when the images are already built + kind-loaded.

.PARAMETER SkipSamples
    Install the platform only; don't install the samples.

.PARAMETER Studio
    Launch Kognition Studio at the end (rebuilds kognitionctl from source).

.EXAMPLE
    ./bootstrap.ps1
    ./bootstrap.ps1 -SkipBuild
    ./bootstrap.ps1 -Studio
#>
param(
    [switch]$SkipBuild,
    [switch]$SkipSamples,
    [switch]$Studio
)

$ErrorActionPreference = "Stop"
$RepoRoot = $PSScriptRoot
Push-Location $RepoRoot
try {
    function Step($n, $msg) { Write-Host "`n[$n] $msg" -ForegroundColor Cyan }
    function Ok($msg)  { Write-Host "  OK  $msg" -ForegroundColor Green }
    function Warn2($msg){ Write-Host "  !!  $msg" -ForegroundColor Yellow }

    # --- 1. Prerequisites ----------------------------------------------------
    Step "1/6" "Checking prerequisites"
    $required = @("docker", "kind", "kubectl", "helm", "go", "python")
    $missing = @()
    foreach ($t in $required) {
        if (Get-Command $t -ErrorAction SilentlyContinue) { Ok $t } else { $missing += $t }
    }
    if ($missing) { throw "Missing required tools: $($missing -join ', '). Install them and re-run." }
    if (-not (& kubectl cluster-info --request-timeout=5s 2>$null)) {
        throw "kubectl cannot reach a cluster. Start a kind cluster (or Docker Desktop Kubernetes) first."
    }
    Ok "cluster reachable"

    # --- 2. .env -------------------------------------------------------------
    Step "2/6" "Checking .env (Azure SP + Foundry credentials)"
    $envFile = Join-Path $RepoRoot ".env"
    if (-not (Test-Path $envFile)) {
        Warn2 ".env not found. Copy .env.example to .env and fill it in:"
        Warn2 "    Copy-Item .env.example .env"
        Warn2 "Continuing, but agents will have no credentials until you add it and re-run 'make foundry-creds'."
    } else {
        $keys = (Get-Content $envFile | ForEach-Object { if ($_ -match '^\s*([A-Za-z_][A-Za-z0-9_]*)\s*=') { $matches[1] } })
        foreach ($k in @("AZURE_TENANT_ID", "AZURE_CLIENT_ID", "AZURE_CLIENT_SECRET")) {
            if ($keys -contains $k) { Ok $k } else { Warn2 "$k missing from .env" }
        }
    }

    # --- 3. Build images -----------------------------------------------------
    if ($SkipBuild) {
        Step "3/6" "Skipping image build (--SkipBuild)"
    } else {
        Step "3/6" "Building container images (make docker)"
        & (Join-Path $RepoRoot "make.ps1") docker
        if ($LASTEXITCODE -ne 0) { throw "make docker failed" }
        Ok "images built"
    }

    # --- 4. Install platform -------------------------------------------------
    Step "4/6" "Installing platform (kubernetes mode: pgvector + KB ingester + foundry-creds)"
    & (Join-Path $RepoRoot "make.ps1") install MODE=kubernetes
    if ($LASTEXITCODE -ne 0) { throw "make install failed" }
    Ok "platform installed"

    # --- 5. Install samples --------------------------------------------------
    if ($SkipSamples) {
        Step "5/6" "Skipping samples (--SkipSamples)"
    } else {
        Step "5/6" "Installing samples (manifests + script-based KBs)"
        & (Join-Path $RepoRoot "make.ps1") samples-install
        if ($LASTEXITCODE -ne 0) { Warn2 "some samples did not apply cleanly (expected for standalone-only kinds)" }
        Ok "samples installed"
    }

    # --- 6. Heal + wait for knowledge bases ----------------------------------
    if (-not $SkipSamples) {
        Step "6/6" "Waiting for knowledge bases to reach Ready"
        # A KB whose first ingest lost a startup race lands in Failed and the
        # reconciler won't retry on its own; nudge it back to Pending once.
        $deadline = (Get-Date).AddMinutes(6)
        do {
            $rows = kubectl get knowledgebases -A -o jsonpath="{range .items[*]}{.metadata.namespace}/{.metadata.name}={.status.phase}{'\n'}{end}" 2>$null
            $kbs = @($rows -split "`n" | Where-Object { $_ -match '=' } | ForEach-Object {
                $p = $_ -split '='; [pscustomobject]@{ Ref = $p[0]; Phase = $p[1] }
            })
            $failed = @($kbs | Where-Object { $_.Phase -eq 'Failed' })
            foreach ($kb in $failed) {
                $ns, $name = $kb.Ref -split '/', 2
                Warn2 "$($kb.Ref) is Failed — resetting to retry ingest"
                kubectl -n $ns patch knowledgebase $name --subresource=status --type=merge -p '{"status":{"phase":"Pending"}}' 2>$null | Out-Null
            }
            $pending = @($kbs | Where-Object { $_.Phase -notin @('Ready') })
            if (-not $pending) { break }
            Start-Sleep 6
        } while ((Get-Date) -lt $deadline)

        Write-Host ""
        kubectl get knowledgebases -A -o custom-columns='NAMESPACE:.metadata.namespace,NAME:.metadata.name,PHASE:.status.phase,CHUNKS:.status.indexedChunks' 2>$null
        $stillFailed = @($kbs | Where-Object { $_.Phase -eq 'Failed' })
        if ($stillFailed) {
            Warn2 "Some KBs are still Failed. Common causes:"
            Warn2 "  - Azure Blob KBs need the SP to have 'Storage Blob Data Contributor' + firewall access."
            Warn2 "  - Check ingest logs: kubectl -n <ns> logs job/kb-ingest-<kb>-<gen>"
        } else {
            Ok "knowledge bases ready"
        }
    }

    Write-Host "`nBootstrap complete." -ForegroundColor Green
    Write-Host "Next:"
    Write-Host "  .\make.ps1 studio                 # visual builder + chat (rebuilds kognitionctl)"
    Write-Host "  kubectl get agents -A             # list agents"
    Write-Host "  Ask 'kognition-docs-agent' about the project, or 'orbital-docs-agent' about Mark 7."

    if ($Studio) {
        Step "+" "Launching Studio"
        & (Join-Path $RepoRoot "make.ps1") studio
    }
}
finally {
    Pop-Location
}
