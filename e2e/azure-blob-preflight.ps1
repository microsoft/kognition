# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

param(
    [Parameter(Mandatory = $true)]
    [string]$StorageAccount,

    [string]$Container = 'product-docs',
    [string]$Prefix = 'mark7/'
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

if (-not (Get-Command az -ErrorAction SilentlyContinue)) {
    throw 'Azure CLI is required for the real Azure Blob E2E preflight.'
}
if (-not (Test-Path '.env')) {
    throw 'Repository .env is required and must contain AZURE_TENANT_ID, AZURE_CLIENT_ID, and AZURE_CLIENT_SECRET.'
}

$keys = @{}
Get-Content .env | ForEach-Object {
    if ($_ -match '^\s*([A-Za-z_][A-Za-z0-9_]*)\s*=') {
        $keys[$matches[1]] = $true
    }
}
foreach ($required in @('AZURE_TENANT_ID', 'AZURE_CLIENT_ID', 'AZURE_CLIENT_SECRET')) {
    if (-not $keys[$required]) {
        throw ".env is missing $required"
    }
}

$account = & az storage account show --name $StorageAccount --query '{name:name,allowSharedKeyAccess:allowSharedKeyAccess}' -o json 2>$null
if ($LASTEXITCODE -ne 0 -or -not $account) {
    throw "Azure Storage account '$StorageAccount' was not found in the current subscription."
}
$account = $account | ConvertFrom-Json
$containerExists = & az storage container exists --account-name $StorageAccount --name $Container --auth-mode login --query exists -o tsv 2>$null
if ($LASTEXITCODE -ne 0) {
    throw "Could not access '$Container' in '$StorageAccount' with Entra login. Verify data-plane RBAC and network access."
}
if ($containerExists -ne 'true') {
    throw "Container '$Container' does not exist in '$StorageAccount'."
}

$blobCount = & az storage blob list --account-name $StorageAccount --container-name $Container --auth-mode login --query "length([?starts_with(name, '$Prefix')])" -o tsv 2>$null
if ($LASTEXITCODE -ne 0) {
    throw "Could not list blobs under '$Prefix'. Verify Storage Blob Data Reader access."
}
if ([int]$blobCount -lt 1) {
    throw "No blobs found under '$Container/$Prefix'."
}

Write-Host "Azure Blob preflight passed: account=$StorageAccount container=$Container prefix=$Prefix blobs=$blobCount" -ForegroundColor Green