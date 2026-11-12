# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

#Requires -Version 7.0
[CmdletBinding()]
param(
    [Parameter(Mandatory)][ValidateNotNullOrEmpty()][string]$Context,
    [Parameter(Mandatory)][ValidatePattern('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')][string]$Namespace,
    [ValidatePattern('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')][string]$ReleaseFullname = 'kognition',
    [ValidatePattern('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')][string]$DatabaseSecret = 'kognition-database',
    [ValidatePattern('^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')][string]$RuntimeSecret = 'kognition-runtime-auth',
    [ValidatePattern('^[a-z][a-z0-9_]*$')][string]$DatabaseUser = 'kognition',
    [ValidatePattern('^[a-z][a-z0-9_]*$')][string]$DatabaseName = 'kognition'
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $false
$kubeArguments = @('--context', $Context, '--namespace', $Namespace)
if ($DatabaseSecret -eq $RuntimeSecret) { throw 'Database and runtime Secrets must have distinct names.' }

function Get-ExistingSecret([string]$Name) {
    $result = & kubectl @kubeArguments get secret $Name --ignore-not-found -o json 2>$null
    if ($LASTEXITCODE -ne 0) { throw "Cannot read Secret $Name in the selected context and namespace." }
    if (-not $result) { return $null }
    return ($result -join "`n" | ConvertFrom-Json -AsHashtable)
}

function Get-SecretValue($Secret, [string]$Key) {
    try {
        $value = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($Secret.data[$Key]))
        if ([string]::IsNullOrWhiteSpace($value)) { throw 'Empty value' }
        return $value
    } catch {
        throw "Existing Secret has a missing or invalid '$Key' field; refusing to replace credentials."
    }
}

function New-RandomCredential {
    return [Convert]::ToBase64String([Security.Cryptography.RandomNumberGenerator]::GetBytes(48))
}

function New-DatabaseUrl([string]$Password) {
    $encoded = [Uri]::EscapeDataString($Password)
    return "postgres://${DatabaseUser}:${encoded}@${ReleaseFullname}-postgres:5432/${DatabaseName}?sslmode=disable"
}

function New-ClusterSecret([string]$Name, [hashtable]$Values) {
    $manifest = @{
        apiVersion = 'v1'
        kind = 'Secret'
        metadata = @{ name = $Name; namespace = $Namespace }
        type = 'Opaque'
        stringData = $Values
    } | ConvertTo-Json -Depth 10 -Compress
    $null = $manifest | & kubectl @kubeArguments create -f - -o name 2>$null
    if ($LASTEXITCODE -ne 0) { throw "Cannot create Secret $Name; no existing credentials were overwritten. Retry after checking cluster access." }
}

$null = & kubectl @kubeArguments get namespace $Namespace -o name 2>$null
if ($LASTEXITCODE -ne 0) { throw 'The target namespace must already exist in the explicitly selected context.' }

$database = Get-ExistingSecret $DatabaseSecret
$runtime = Get-ExistingSecret $RuntimeSecret
if (-not $database) {
    $claim = & kubectl @kubeArguments get pvc "data-${ReleaseFullname}-postgres-0" --ignore-not-found -o name 2>$null
    if ($LASTEXITCODE -ne 0) { throw 'Cannot inspect PostgreSQL storage; refusing to generate database credentials.' }
    if ($claim) { throw 'PostgreSQL storage already exists but its credentials are missing. Restore the original database Secret before continuing.' }
}
if ($database) {
    $password = Get-SecretValue $database 'password'
    $databaseUrl = Get-SecretValue $database 'DATABASE_URL'
    if ($databaseUrl -cne (New-DatabaseUrl $password)) {
        throw 'Existing database credentials do not match the requested database settings; refusing to rotate them.'
    }
}
if ($runtime) {
    $null = Get-SecretValue $runtime 'runtime-control-token'
    $bindingJson = Get-SecretValue $runtime 'runtime-approval-binding-keys.json'
    try {
        $bindingKeys = @($bindingJson | ConvertFrom-Json -AsHashtable)
        if ($bindingKeys.Count -eq 0) { throw 'Missing keys' }
        $activeKeys = @($bindingKeys | Where-Object { $_.status -eq 'Active' })
        if ($activeKeys.Count -ne 1) { throw 'Expected one active key' }
        foreach ($bindingKey in $bindingKeys) {
            if ($bindingKey.algorithm -ne 'HMAC-SHA-256' -or $bindingKey.domain -ne 'kognition.dev/runtime-approval/action/v1' -or $bindingKey.version -ne 1 -or $bindingKey.epoch -lt 1 -or [Convert]::FromBase64String($bindingKey.material).Length -lt 32) {
                throw 'Invalid binding key'
            }
        }
    } catch {
        throw 'Existing runtime binding keys are invalid; refusing to replace signing material.'
    }
}

if (-not $database) {
    $password = New-RandomCredential
    New-ClusterSecret $DatabaseSecret @{ password = $password; DATABASE_URL = (New-DatabaseUrl $password) }
}
if (-not $runtime) {
    $bindingKeys = ConvertTo-Json -InputObject @(@{
        version = 1
        epoch = 1
        algorithm = 'HMAC-SHA-256'
        domain = 'kognition.dev/runtime-approval/action/v1'
        status = 'Active'
        material = (New-RandomCredential)
    }) -Compress
    New-ClusterSecret $RuntimeSecret @{
        'runtime-control-token' = (New-RandomCredential)
        'runtime-approval-binding-keys.json' = $bindingKeys
    }
}

[pscustomobject]@{
    Context = $Context
    Namespace = $Namespace
    DatabaseSecret = $DatabaseSecret
    RuntimeSecret = $RuntimeSecret
}