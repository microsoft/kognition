# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# Apply the S3 blob knowledge-base sample end-to-end, backed by a local fixture
# running in-cluster — no external AWS account.
#
#   1. Deploy MinIO (Deployment + Service) and wait for it to be Ready.
#   2. Upload samples/docs/*.md into an S3 bucket (via a temporary
#      port-forward from this host; boto3 is installed if missing).
#   3. Create the `s3-blob-creds` SecretRef (S3 access/secret keys).
#   4. Wire the pgvector Secret/SecretRef into `default`.
#   5. Apply Model -> KBClass -> KB -> Claim -> Agent -> IndexRun.
#
# Idempotent: safe to re-run. Run via `.\make.ps1 samples-install
# SAMPLE=knowledge-base-blob-s3` (or as part of a bare `samples-install`).

param(
    [string]$Kubeconfig = $env:KUBECONFIG,
    [ValidatePattern('^.+@sha256:[a-f0-9]{64}$')][string]$FixtureImage
)

$ErrorActionPreference = "Stop"
$sampleDir = $PSScriptRoot
. (Join-Path $sampleDir '..\azure-settings.ps1')
if (Get-AzureSampleProfile) {
    # The governed installer calls kubectl without --kubeconfig, so scope KUBECONFIG
    # to the explicitly selected file for the duration of the call.
    $previousKubeconfig = $env:KUBECONFIG
    try {
        if ($PSBoundParameters.ContainsKey('Kubeconfig')) {
            if (-not $Kubeconfig -or -not (Test-Path -LiteralPath $Kubeconfig -PathType Leaf)) {
                throw "Kubeconfig file not found: $Kubeconfig"
            }
            $env:KUBECONFIG = (Resolve-Path -LiteralPath $Kubeconfig).Path
        }
        Invoke-AzureSampleKnowledgeInstall -Source s3 -SampleDirectory $sampleDir
    } finally {
        $env:KUBECONFIG = $previousKubeconfig
    }
    return
}
if (-not $Kubeconfig -or -not (Test-Path -LiteralPath $Kubeconfig -PathType Leaf)) {
    throw 'Pass an explicit existing -Kubeconfig, or set KUBECONFIG to the approved target file.'
}
$kubeArguments = @('--kubeconfig', (Resolve-Path -LiteralPath $Kubeconfig).Path)
$targetUID = & kubectl @kubeArguments get namespace kube-system -o jsonpath='{.metadata.uid}'
if ($LASTEXITCODE -ne 0 -or -not $targetUID) { throw 'Cannot verify the explicitly selected cluster.' }

$sampleDir = $PSScriptRoot
. (Join-Path $sampleDir '..\azure-settings.ps1')
$sourceNs  = if ($env:KOGNITION_PLATFORM_NAMESPACE) { $env:KOGNITION_PLATFORM_NAMESPACE } else { "kognition-system" }
$targetNs  = "default"
$serviceNs = "samples-s3-blob-services"
$pgSecret  = "kognition-kb-pgvector"
$sourceSecret = if ($env:KOGNITION_PLATFORM_RELEASE) { "$env:KOGNITION_PLATFORM_RELEASE-kb-pgvector" } else { $pgSecret }

# Sample-only local S3 credentials (public defaults, not a secret).
$s3Access = "minioadmin"
$s3Secret = "minioadmin"
$localPort = 9010

# --- 1. Deploy the S3-compatible store --------------------------------------
Write-Host "== Deploying the pinned S3-compatible store"
Import-Module powershell-yaml -ErrorAction Stop
$fixtureDocuments = @(ConvertFrom-Yaml -Yaml ([IO.File]::ReadAllText((Join-Path $sampleDir 's3-store.yaml'))) -AllDocuments)
$fixtureDeployment = @($fixtureDocuments | Where-Object { $_ -and $_.kind -eq 'Deployment' })
if ($fixtureDeployment.Count -ne 1) { throw 'S3 fixture must contain one reviewed Deployment.' }
if ($FixtureImage) { $fixtureDeployment[0].spec.template.spec.containers[0].image = $FixtureImage }
$image = $fixtureDeployment[0].spec.template.spec.containers[0].image
if ($image -notmatch '^.+@sha256:[a-f0-9]{64}$') { throw 'S3 fixture image must be digest pinned.' }
$context = & kubectl @kubeArguments config current-context
if ($LASTEXITCODE -ne 0) { throw 'Cannot identify the explicit kubeconfig context.' }
if ($context -match '^kind-(.+)$') {
    $cluster = $Matches[1]
    if (-not (Get-Command kind -ErrorAction SilentlyContinue) -or -not (Get-Command docker -ErrorAction SilentlyContinue)) {
        throw 'A local kind target requires kind and Docker for exact fixture image loading.'
    }
    $nodes = @(& kind get nodes --name $cluster)
    if ($LASTEXITCODE -ne 0 -or $nodes.Count -eq 0) { throw 'Cannot identify the explicitly selected kind nodes.' }
    $nodeInventory = & kubectl @kubeArguments get nodes -o json
    if ($LASTEXITCODE -ne 0) { throw 'Cannot verify the selected kind cluster against the kubeconfig.' }
    $selectedNodes = @(($nodeInventory -join "`n" | ConvertFrom-Json).items | ForEach-Object { $_.metadata.name })
    if (@(Compare-Object ($nodes | Sort-Object) ($selectedNodes | Sort-Object)).Count -ne 0) {
        throw 'Kind nodes differ from the explicitly selected Kubernetes API; no images were loaded.'
    }
    $local = & docker image inspect $image 2>$null
    if ($LASTEXITCODE -ne 0) {
        throw 'Build the reviewed S3 fixture recipe first, then pass its exact local digest using -FixtureImage.'
    }
    $alias = "kognition-manual/s3-fixture:reviewed-$($image.Substring($image.Length - 64, 12))"
    & docker image tag $image $alias
    if ($LASTEXITCODE -ne 0) { throw 'Cannot create the exact local fixture image alias.' }
    & kind load docker-image $alias --name $cluster
    if ($LASTEXITCODE -ne 0) { throw 'Exact S3 fixture image loading failed; no unrelated cluster will be used.' }
    foreach ($node in $nodes) {
        $references = @(& docker exec $node ctr --namespace k8s.io images list --quiet)
        if ($LASTEXITCODE -ne 0) { throw "Cannot inspect image bindings on node $node." }
        if ($references -notcontains $image) {
            & docker exec $node ctr --namespace k8s.io images tag "docker.io/$alias" $image
            if ($LASTEXITCODE -ne 0) { throw "Cannot bind the reviewed image digest on node $node." }
        }
        $runtime = & docker exec $node crictl inspecti $image
        if ($LASTEXITCODE -ne 0) { throw "Cannot resolve the reviewed fixture image on node $node." }
        $identity = $runtime -join "`n" | ConvertFrom-Json
        if ($identity.status.repoDigests -notcontains $image) { throw 'The kind node did not preserve the exact fixture digest.' }
    }
}
$fixtureYaml = ($fixtureDocuments | Where-Object { $_ } | ForEach-Object { ConvertTo-AzureSampleYaml -Data $_ }) -join "`n"
$fixtureYaml | kubectl @kubeArguments apply -f -
if ($LASTEXITCODE -ne 0) { throw "S3 fixture apply failed" }
kubectl @kubeArguments -n $serviceNs rollout status deploy/minio --timeout=300s
if ($LASTEXITCODE -ne 0) { throw "S3 fixture did not become ready" }

# --- 2. Upload docs via a temporary port-forward ----------------------------
Write-Host "== Uploading sample docs into S3 (product-docs bucket)"
$py = Resolve-AzureSamplePython
# boto3 is required for the upload; install it into the active env if missing.
& $py -c "import boto3" 2>$null
if ($LASTEXITCODE -ne 0) {
    Write-Host "   installing boto3..."
    & $py -m pip install boto3
    if ($LASTEXITCODE -ne 0) { throw 'Cannot install the missing S3 upload SDK.' }
}
$listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, $localPort)
try { $listener.Start() }
catch [Net.Sockets.SocketException] { throw "Upload port $localPort is already in use; refusing an unrelated listener." }
finally { $listener.Stop() }
$forwardOutput = [IO.Path]::GetTempFileName()
$forwardError = [IO.Path]::GetTempFileName()
$environment = @{}
foreach ($name in @('KOGNITION_S3_ENDPOINT','KOGNITION_S3_ACCESS_KEY','KOGNITION_S3_SECRET_KEY')) {
    $environment[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
}
$pf = $null
try {
    $forwardArguments = @('--kubeconfig', ('"{0}"' -f $kubeArguments[1]), 'port-forward', '--address=127.0.0.1', '-n', $serviceNs, 'svc/minio', "${localPort}:9000")
    $processOptions = Get-AzureSampleProcessOptions
    $pf = Start-Process kubectl -ArgumentList $forwardArguments -PassThru @processOptions -RedirectStandardOutput $forwardOutput -RedirectStandardError $forwardError
    $deadline = [DateTime]::UtcNow.AddSeconds(30)
    while ((Get-Content -LiteralPath $forwardOutput -Raw) -notmatch "Forwarding from 127\.0\.0\.1:$localPort") {
        if ($pf.HasExited) { throw 'The owned S3 upload port-forward exited before binding.' }
        if ([DateTime]::UtcNow -ge $deadline) { throw 'The owned S3 upload port-forward did not bind before its deadline.' }
        Start-Sleep -Milliseconds 200
    }
    $env:KOGNITION_S3_ENDPOINT   = "http://127.0.0.1:$localPort"
    $env:KOGNITION_S3_ACCESS_KEY = $s3Access
    $env:KOGNITION_S3_SECRET_KEY = $s3Secret
    & $py (Join-Path $sampleDir 'upload_docs.py')
    $uploadExit = $LASTEXITCODE
    if ($uploadExit -ne 0) { throw "Doc upload to S3 failed; refusing to index an empty sample bucket." }
}
finally {
    if ($pf -and -not $pf.HasExited) { Stop-Process -Id $pf.Id -Force }
    foreach ($name in $environment.Keys) { [Environment]::SetEnvironmentVariable($name, $environment[$name], 'Process') }
    Remove-Item -LiteralPath $forwardOutput,$forwardError -ErrorAction Stop
}

# --- 3. SecretRef with the S3 access/secret keys -----------------------------
Write-Host "== Ensuring s3-blob-creds SecretRef"
$s3Payload = @{ accessKeyId = $s3Access; secretAccessKey = $s3Secret } | ConvertTo-Json -Compress
$srS3 = @"
apiVersion: kognition.dev/v1alpha1
kind: SecretRef
metadata:
  name: s3-blob-creds
  namespace: $targetNs
  annotations:
    kognition.dev/internal-secret-data: '$s3Payload'
spec:
  backendType: internal
  backendRef: s3-blob-creds
  key: accessKeyId
"@
$srS3 | kubectl @kubeArguments apply -f -
if ($LASTEXITCODE -ne 0) { throw "s3 SecretRef apply failed" }

# --- 4. Refresh the pgvector Secret + SecretRef in the target namespace -----
Write-Host "== Ensuring pgvector Secret exists in $targetNs"
$connStr = kubectl @kubeArguments -n $sourceNs get secret $sourceSecret -o jsonpath='{.data.connectionString}' | ForEach-Object { [System.Text.Encoding]::UTF8.GetString([System.Convert]::FromBase64String($_)) }
$pwStr   = kubectl @kubeArguments -n $sourceNs get secret $sourceSecret -o jsonpath='{.data.password}'         | ForEach-Object { [System.Text.Encoding]::UTF8.GetString([System.Convert]::FromBase64String($_)) }
if (-not $connStr) { throw "Could not read connectionString from $sourceNs/$sourceSecret (is kbPgvector.enabled=true?)" }
$fqdn = "$sourceSecret.$sourceNs.svc.cluster.local"
$connStr = $connStr -replace "@${sourceSecret}:", "@${fqdn}:"
kubectl @kubeArguments create secret generic $pgSecret -n $targetNs `
    --from-literal="connectionString=$connStr" `
    --from-literal="password=$pwStr" `
    --dry-run=client -o yaml | kubectl @kubeArguments apply -f -
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
$srYaml | kubectl @kubeArguments apply -f -
if ($LASTEXITCODE -ne 0) { throw "SecretRef apply failed" }

# --- 5. Apply the sample manifests -------------------------------------------
Write-Host "== kubectl apply -f manifests.yaml"
Read-AzureSampleManifest -Path (Join-Path $sampleDir 'manifests.yaml') | kubectl @kubeArguments apply -f -
if ($LASTEXITCODE -ne 0) { throw "manifest apply failed" }

Write-Host ""
Write-Host "Done. Watch the ingest with:"
Write-Host "  kubectl -n $targetNs get knowledgebase s3-docs-kb -o wide -w"
