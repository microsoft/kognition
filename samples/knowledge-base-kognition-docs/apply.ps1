# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# Apply the "Kognition docs KB" sample end-to-end.
#
#   1. Build the `kognition-docs` ConfigMap from live repo files so the
#      indexed content always mirrors the working tree.
#   2. Copy the chart-managed pgvector Secret to `default` (chart provisions
#      only its own namespace).
#   3. Apply ModelClass -> Models -> KBClass -> docs Service -> KB -> Agent -> IndexRun.
#
# Idempotent: safe to re-run. Requires kubectl on PATH.

$ErrorActionPreference = "Stop"

$repoRoot   = (Resolve-Path (Join-Path $PSScriptRoot "..\..")).Path
$sampleDir  = $PSScriptRoot
. (Join-Path $sampleDir '..\azure-settings.ps1')
$sourceNs   = if ($env:KOGNITION_PLATFORM_NAMESPACE) { $env:KOGNITION_PLATFORM_NAMESPACE } else { "kognition-system" }
$targetNs   = "default"
$serviceNs  = "samples-kognition-docs-services"
$pgSecret   = "kognition-kb-pgvector"
$sourceSecret = if ($env:KOGNITION_PLATFORM_RELEASE) { "$env:KOGNITION_PLATFORM_RELEASE-kb-pgvector" } else { $pgSecret }

$docs = @(
    (Join-Path $repoRoot "README.md"),
    (Join-Path $repoRoot "PROJECT.md"),
    (Join-Path $repoRoot "BACKLOG.md"),
    (Join-Path $repoRoot "docs\kognition-manifest.md")
)

foreach ($f in $docs) {
    if (-not (Test-Path $f)) { throw "Missing doc file: $f" }
}

Write-Host "== Building kognition-docs ConfigMap from repo files"
# The namespace is also declared in docs-server.yaml, but the ConfigMap must
# exist before that manifest is applied.
kubectl create namespace $serviceNs --dry-run=client -o yaml | kubectl apply -f -
if ($LASTEXITCODE -ne 0) { throw "Service namespace apply failed" }
# Server-side apply so the (large) doc contents aren't duplicated into
# the `kubectl.kubernetes.io/last-applied-configuration` annotation,
# which is capped at 256 KiB — combined docs exceed that.
$cmArgs = @("create", "configmap", "kognition-docs", "-n", $serviceNs)
foreach ($f in $docs) { $cmArgs += @("--from-file=$f") }
$cmArgs += @("--dry-run=client", "-o", "yaml")
& kubectl @cmArgs | kubectl apply --server-side --force-conflicts -f -
if ($LASTEXITCODE -ne 0) { throw "ConfigMap apply failed" }

Write-Host "== Ensuring pgvector Secret exists in $targetNs"
$connStr = kubectl -n $sourceNs get secret $sourceSecret -o jsonpath='{.data.connectionString}' | ForEach-Object { [System.Text.Encoding]::UTF8.GetString([System.Convert]::FromBase64String($_)) }
$pwStr   = kubectl -n $sourceNs get secret $sourceSecret -o jsonpath='{.data.password}'         | ForEach-Object { [System.Text.Encoding]::UTF8.GetString([System.Convert]::FromBase64String($_)) }
if (-not $connStr) { throw "Could not read connectionString from $sourceNs/$sourceSecret" }
$fqdn = "$sourceSecret.$sourceNs.svc.cluster.local"
$connStr = $connStr -replace "@${sourceSecret}:", "@${fqdn}:"
kubectl create secret generic $pgSecret -n $targetNs `
    --from-literal="connectionString=$connStr" `
    --from-literal="password=$pwStr" `
    --dry-run=client -o yaml | kubectl apply -f -
if ($LASTEXITCODE -ne 0) { throw "Secret copy failed" }

Write-Host "== Ensuring SecretRef CR wraps the pgvector Secret"
# The controller looks up connection details via a Kognition `SecretRef`
# CR (annotation-backed), not a raw k8s Secret. Encode the same rewritten
# connection string + password into its internal-secret-data annotation.
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

# Apply in dependency order so referenced objects exist first.
$order = @(
    "model-class.yaml",
    "model.yaml",
    "kb-class.yaml",
    "docs-server.yaml",
    "kb.yaml",
    "agent.yaml",
    "indexrun.yaml"
)

foreach ($f in $order) {
    $path = Join-Path $sampleDir $f
    Write-Host "== kubectl apply -f $f"
    Invoke-AzureSampleApply -Path $path
    if ($LASTEXITCODE -ne 0) { throw "apply failed for $f" }

    # Models do not currently watch ModelClass changes. Ensure the class has
    # reached the backing store before creating Models to avoid a stale
    # ModelClassMissing condition with no reconciliation event.
    if ($f -eq "model-class.yaml") {
        Write-Host "   waiting for foundry ModelClass to be ready..."
        kubectl wait --for=condition=Ready=True modelclass/foundry --timeout=120s
        if ($LASTEXITCODE -ne 0) { throw "ModelClass readiness wait failed" }
    }

    # The KB's `url` sources are served by the in-cluster docs Service. Wait for
    # it to be Ready before the KnowledgeBase (and its ingest Job) are created,
    # otherwise the first ingest can race the docs pod coming up.
    if ($f -eq "docs-server.yaml") {
        Write-Host "   waiting for kognition-docs deployment to be ready..."
        kubectl -n $serviceNs rollout status deploy/kognition-docs --timeout=120s 2>$null
    }
}

Write-Host ""
Write-Host "Done. Watch the reindex with:"
Write-Host "  kubectl -n $targetNs get kbrun kognition-docs-reindex-now -o wide -w"
