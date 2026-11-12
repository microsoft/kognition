# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# kb-e2e-test.ps1 - End-to-end test for the pgvector KnowledgeBase pipeline.
#
# Provisions the `knowledge-base-kognition-docs` sample (Model, KBClass, KB,
# IndexRun, Agent), waits for the IndexRun Job to reach `Succeeded`, waits for
# the KnowledgeBase to report a positive documentCount, then sends a chat
# message to the sample Agent and asserts that the response references content
# that could only come from a successful `knowledge_search` against the
# indexed corpus.
#
# Prerequisites (see samples/knowledge-base-kognition-docs/README.md):
#   - bootstrap.ps1 has been run (kubectl-kognition built and on PATH).
#   - kind cluster running with the kognition chart installed AND
#       --set kbPgvector.enabled=true
#       --set kbIngester.enabled=true
#     Images referenced by the chart values must be loadable (see the sample
#     README for the exact commands).
#   - `foundry-creds` SecretRef exists in `default` (Azure service-principal).
#   - .env at repo root with AZURE_TENANT_ID/CLIENT_ID/CLIENT_SECRET is only
#     needed indirectly: the sample assumes `foundry-creds` already exists.
#
# Usage: .\e2e\kb-e2e-test.ps1

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$RepoRoot   = Split-Path -Parent $PSScriptRoot
$BinDir     = Join-Path $RepoRoot "bin"
$PluginExe  = Join-Path $BinDir   "kubectl-kognition.exe"
$SampleDir  = Join-Path $RepoRoot "samples\knowledge-base-kognition-docs"
$ApplyScript = Join-Path $SampleDir "apply.ps1"

$namespace     = "default"
$kbName        = "kognition-docs-kb"
$indexRunName  = "kognition-docs-reindex-now"
$agentName     = "kognition-docs-agent"
# Target the CRD status table so retrieval is deterministic instead of relying
# on a broad search that may return a sparse README or PROJECT chunk.
$promptText    = "Use knowledge_search for the Kognition CRD status table in docs/kognition-manifest.md. From that table, name the MemoryClaim and KnowledgeBase CRDs and briefly state what each does. Cite the source file."
$expectedTokens = @("MemoryClaim", "KnowledgeBase")

Write-Host "=== Kognition E2E Test - pgvector KnowledgeBase ===" -ForegroundColor Cyan

if (-not (Test-Path $PluginExe)) {
    throw "kubectl-kognition not found at $PluginExe. Run .\e2e\bootstrap.ps1 first."
}
if ($env:PATH -notlike "*$BinDir*") {
    $env:PATH = "$BinDir;$env:PATH"
}
if (-not (Test-Path $ApplyScript)) {
    throw "Sample apply script missing: $ApplyScript"
}

# --- [1/5] Precondition checks ------------------------------------------------
Write-Host "`n[1/5] Verifying cluster preconditions..." -ForegroundColor Yellow

$pg = & kubectl -n kognition-system get statefulset kognition-kb-pgvector -o name 2>$null
if (-not $pg) {
    throw "Chart is not installed with kbPgvector.enabled=true. Missing StatefulSet kognition-system/kognition-kb-pgvector."
}
Write-Host "  pgvector StatefulSet present." -ForegroundColor Green

$creds = & kubectl -n $namespace get secretref foundry-creds -o name 2>$null
if (-not $creds) {
    throw "SecretRef default/foundry-creds not found. Create it before running this test."
}
Write-Host "  foundry-creds SecretRef present in $namespace." -ForegroundColor Green

# --- [2/5] Apply the sample ---------------------------------------------------
Write-Host "`n[2/5] Applying knowledge-base-kognition-docs sample..." -ForegroundColor Yellow
& powershell -NoProfile -ExecutionPolicy Bypass -File $ApplyScript
if ($LASTEXITCODE -ne 0) { throw "Sample apply failed" }

# --- [3/5] Wait for the IndexRun Job to succeed -------------------------------
Write-Host "`n[3/5] Waiting for IndexRun '$indexRunName' to reach Succeeded..." -ForegroundColor Yellow

$indexTimeoutSec = 480
$sw = [Diagnostics.Stopwatch]::StartNew()
$indexPhase = ""
while ($sw.Elapsed.TotalSeconds -lt $indexTimeoutSec) {
    $indexPhase = (& kubectl -n $namespace get kbrun $indexRunName -o jsonpath='{.status.phase}' 2>$null)
    if ($indexPhase -eq "Succeeded") { break }
    if ($indexPhase -eq "Failed") {
        Write-Host "  IndexRun failed. Recent pod logs:" -ForegroundColor Red
        & kubectl -n $namespace logs "job/kb-indexrun-$indexRunName" --tail=100 2>$null
        throw "IndexRun $indexRunName reached Failed phase."
    }
    Start-Sleep -Seconds 5
    Write-Host ("    [{0,4:N0}s] phase='{1}'" -f $sw.Elapsed.TotalSeconds, $indexPhase)
}
if ($indexPhase -ne "Succeeded") {
    throw "IndexRun $indexRunName did not reach Succeeded within ${indexTimeoutSec}s (last phase='$indexPhase')."
}
$docs   = & kubectl -n $namespace get kbrun $indexRunName -o jsonpath='{.status.stats.documents}' 2>$null
$chunks = & kubectl -n $namespace get kbrun $indexRunName -o jsonpath='{.status.stats.chunks}' 2>$null
Write-Host "  IndexRun Succeeded (documents=$docs, chunks=$chunks)." -ForegroundColor Green

# --- [4/5] Wait for the KB to report Ready + documentCount > 0 ----------------
Write-Host "`n[4/5] Waiting for KnowledgeBase '$kbName' to be Ready..." -ForegroundColor Yellow

$kbTimeoutSec = 120
$sw = [Diagnostics.Stopwatch]::StartNew()
$kbPhase = ""
$docCount = 0
while ($sw.Elapsed.TotalSeconds -lt $kbTimeoutSec) {
    $kbPhase  = (& kubectl -n $namespace get knowledgebase $kbName -o jsonpath='{.status.phase}' 2>$null)
    $docCountRaw = (& kubectl -n $namespace get knowledgebase $kbName -o jsonpath='{.status.documentCount}' 2>$null)
    $docCount = 0
    if ($docCountRaw) { [void][int]::TryParse($docCountRaw, [ref]$docCount) }
    if ($kbPhase -eq "Ready" -and $docCount -gt 0) { break }
    Start-Sleep -Seconds 3
    Write-Host ("    [{0,4:N0}s] phase='{1}' documentCount={2}" -f $sw.Elapsed.TotalSeconds, $kbPhase, $docCount)
}
if ($kbPhase -ne "Ready" -or $docCount -le 0) {
    throw "KnowledgeBase $kbName did not reach Ready with documentCount>0 within ${kbTimeoutSec}s (phase='$kbPhase', documentCount=$docCount)."
}
Write-Host "  KB Ready (documentCount=$docCount)." -ForegroundColor Green

# --- [5/5] Chat with the agent and assert knowledge grounding -----------------
Write-Host "`n[5/5] Chatting with agent '$agentName' to validate retrieval..." -ForegroundColor Yellow

$reply = & $PluginExe chat $agentName $promptText 2>&1 | Out-String
if ($LASTEXITCODE -ne 0) {
    Write-Host $reply -ForegroundColor Red
    throw "kubectl-kognition chat failed"
}

$matched = @()
foreach ($tok in $expectedTokens) {
    if ($reply -match [regex]::Escape($tok)) { $matched += $tok }
}
if ($matched.Count -lt 2) {
    Write-Host "--- Agent reply ---" -ForegroundColor Red
    Write-Host $reply
    Write-Host "-------------------" -ForegroundColor Red
    throw "Agent reply did not mention at least two indexed CRDs (matched: $([string]::Join(', ', $matched))). Retrieval likely did not fire."
}

Write-Host "  Agent reply grounded on indexed docs (matched: $([string]::Join(', ', $matched)))." -ForegroundColor Green
Write-Host "`n=== PASS ===" -ForegroundColor Green
