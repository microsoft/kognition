# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# Apply the Azure Blob knowledge-base sample end-to-end, backed by Azurite
# (the Azure Storage emulator) running in-cluster — no external Azure account.
#
#   1. Deploy Azurite (Deployment + Service) and wait for it to be Ready.
#   2. Upload samples/docs/*.md into Azurite's `product-docs` container
#      (via a temporary port-forward from this host).
#   3. Create the `azurite-blob-creds` SecretRef (connection string whose
#      BlobEndpoint targets the in-cluster Azurite Service).
#   4. Wire the pgvector Secret/SecretRef into `default`.
#   5. Apply Model -> KBClass -> KB -> Claim -> Agent -> IndexRun.
#
# Idempotent: safe to re-run. Run via `.\make.ps1 samples-install
# SAMPLE=knowledge-base-blob-azure` (or as part of a bare `samples-install`).
# Requires kubectl; the upload step needs Python with azure-storage-blob
# (the repo venv has it).

$ErrorActionPreference = "Stop"

$sampleDir = $PSScriptRoot
. (Join-Path $sampleDir '..\azure-settings.ps1')
if (Get-AzureSampleProfile) {
    Invoke-AzureSampleKnowledgeInstall -Source azure -SampleDirectory $sampleDir
    return
}
$sourceNs  = if ($env:KOGNITION_PLATFORM_NAMESPACE) { $env:KOGNITION_PLATFORM_NAMESPACE } else { "kognition-system" }
$targetNs  = "default"
$serviceNs = "samples-azure-blob-services"
$pgSecret  = "kognition-kb-pgvector"
$sourceSecret = if ($env:KOGNITION_PLATFORM_RELEASE) { "$env:KOGNITION_PLATFORM_RELEASE-kb-pgvector" } else { $pgSecret }

# Azurite account: the name is fixed, the key is a per-installation random value
# kept in the `azurite-account` Secret (never committed).
$azKey          = Initialize-AzuriteAccount -Namespace $serviceNs
$inClusterConn  = New-AzuriteConnectionString -AccountKey $azKey -BlobEndpoint "http://azurite.$serviceNs.svc.cluster.local:10000"
$localPort = 10001
$localConn      = New-AzuriteConnectionString -AccountKey $azKey -BlobEndpoint "http://127.0.0.1:$localPort"
$azAccount      = $script:AzuriteAccountName

# --- 0. Best-effort: ensure the Azurite image is in the kind cluster ---------
if ((Get-Command kind -ErrorAction SilentlyContinue) -and (Get-Command docker -ErrorAction SilentlyContinue)) {
    $clusters = @(& kind get clusters 2>$null | Where-Object { $_ })
    if ($clusters) {
        $cluster = $clusters[0]
        $ctx = (& kubectl config current-context 2>$null)
        if ($ctx -and $ctx -match '^kind-(.+)$' -and ($clusters -contains $matches[1])) { $cluster = $matches[1] }
        $img = "mcr.microsoft.com/azure-storage/azurite@sha256:830430c1da1a2d537e08f3e6764dd1f5ae00cf0346bcaf625b968ec3f0971fd5"
        $imageID = (& docker image ls --quiet $img | Select-Object -First 1)
        if (-not $imageID) {
            $pull = Start-Process -FilePath 'docker' -ArgumentList @('pull', $img) -NoNewWindow -Wait -PassThru
            if ($pull.ExitCode -ne 0) { throw "docker failed to pull $img" }
            $imageID = (& docker image ls --quiet $img | Select-Object -First 1)
        }
        if ($imageID) {
            Write-Host "== kind load $img"
            $load = Start-Process -FilePath 'kind' -ArgumentList @('load', 'docker-image', $img, '--name', $cluster) -NoNewWindow -Wait -PassThru
            if ($load.ExitCode -ne 0) { Write-Warning "kind could not preload $img; Kubernetes will pull it from the registry." }
        }
    }
}

# --- 1. Deploy Azurite -------------------------------------------------------
Write-Host "== Deploying Azurite (Azure Storage emulator)"
kubectl apply -f (Join-Path $sampleDir 'azurite.yaml')
if ($LASTEXITCODE -ne 0) { throw "azurite apply failed" }
kubectl -n $serviceNs rollout status deploy/azurite --timeout=120s
if ($LASTEXITCODE -ne 0) { throw "azurite did not become ready" }

# --- 2. Upload docs into Azurite via a temporary port-forward ----------------
Write-Host "== Uploading sample docs into Azurite ($azAccount/product-docs)"
$processOptions = Get-AzureSampleProcessOptions
$pf = Start-Process kubectl -ArgumentList @('port-forward', '-n', $serviceNs, 'svc/azurite', "${localPort}:10000") -PassThru @processOptions
try {
    Start-Sleep -Seconds 3
    $py = Resolve-AzureSamplePython
    $env:KOGNITION_BLOB_CONNECTION_STRING = $localConn
    & $py (Join-Path $sampleDir 'upload_docs.py')
    $uploadExit = $LASTEXITCODE
    Remove-Item Env:\KOGNITION_BLOB_CONNECTION_STRING -ErrorAction SilentlyContinue
    if ($uploadExit -ne 0) { throw "Doc upload to Azurite failed; refusing to index an empty sample container." }
}
finally {
    if ($pf -and -not $pf.HasExited) { Stop-Process -Id $pf.Id -Force -ErrorAction SilentlyContinue }
}

# --- 3. SecretRef wrapping the Azurite connection string (in-cluster endpoint)-
Write-Host "== Ensuring azurite-blob-creds SecretRef"
$blobPayload = @{ connectionString = $inClusterConn } | ConvertTo-Json -Compress
$srBlob = @"
apiVersion: kognition.dev/v1alpha1
kind: SecretRef
metadata:
  name: azurite-blob-creds
  namespace: $targetNs
  annotations:
    kognition.dev/internal-secret-data: '$blobPayload'
spec:
  backendType: internal
  backendRef: azurite-blob-creds
  key: connectionString
"@
$srBlob | kubectl apply -f -
if ($LASTEXITCODE -ne 0) { throw "azurite SecretRef apply failed" }

# --- 4. Ensure the pgvector Secret + SecretRef exist in the target namespace --
Write-Host "== Ensuring pgvector Secret exists in $targetNs"
$connStr = kubectl -n $sourceNs get secret $sourceSecret -o jsonpath='{.data.connectionString}' | ForEach-Object { [System.Text.Encoding]::UTF8.GetString([System.Convert]::FromBase64String($_)) }
$pwStr   = kubectl -n $sourceNs get secret $sourceSecret -o jsonpath='{.data.password}'         | ForEach-Object { [System.Text.Encoding]::UTF8.GetString([System.Convert]::FromBase64String($_)) }
if (-not $connStr) { throw "Could not read connectionString from $sourceNs/$sourceSecret (is kbPgvector.enabled=true?)" }
# The chart ships the DSN with the bare service name, which only resolves inside
# kognition-system. Rewrite the host to the cross-namespace FQDN so the ingester
# Job (which runs in $targetNs) can reach it.
$fqdn = "$sourceSecret.$sourceNs.svc.cluster.local"
$connStr = $connStr -replace "@${sourceSecret}:", "@${fqdn}:"
kubectl create secret generic $pgSecret -n $targetNs `
    --from-literal="connectionString=$connStr" `
    --from-literal="password=$pwStr" `
    --dry-run=client -o yaml | kubectl apply -f -
if ($LASTEXITCODE -ne 0) { throw "Secret copy failed" }

Write-Host "== Ensuring SecretRef CR wraps the pgvector Secret"
$payload = @{ connectionString = $connStr; password = $pwStr } | ConvertTo-Json -Compress
$srYaml = @"
apiVersion: kognition.dev/v1alpha1
kind: SecretRef
metadata:
  name: $pgSecret
  namespace: $targetNs
  annotations:
    kognition.dev/internal-secret-data: '$payload'
spec:
  backendType: internal
  backendRef: $pgSecret
  key: connectionString
"@
$srYaml | kubectl apply -f -
if ($LASTEXITCODE -ne 0) { throw "SecretRef apply failed" }

# --- 5. Apply the sample manifests -------------------------------------------
Write-Host "== kubectl apply -f manifests.yaml"
Invoke-AzureSampleApply -Path (Join-Path $sampleDir 'manifests.yaml')
if ($LASTEXITCODE -ne 0) { throw "manifest apply failed" }

Write-Host ""
Write-Host "Done. Watch the ingest with:"
Write-Host "  kubectl -n $targetNs get knowledgebase azure-docs-kb -o wide -w"
