# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

#Requires -Version 7.0
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$initializer = Join-Path $PSScriptRoot '../Initialize-Secrets.ps1'
$global:aksSecretTestState = @{
    Secrets = @{}
    Requests = [Collections.Generic.List[object]]::new()
    FailReads = $false
    FailCreates = $false
    RetainedDatabase = $false
    FailVolumeRead = $false
}

function Assert-True([bool]$Condition, [string]$Message) {
    if (-not $Condition) { throw $Message }
}

function kubectl {
    $Arguments = @($args)
    $InputObject = $input | Out-String
    $state = $global:aksSecretTestState
    $state.Requests.Add(@($Arguments))
    $global:LASTEXITCODE = 0
    Assert-True (($Arguments[0..3] -join ' ') -eq '--context disposable-demo --namespace demo') 'Every request must select the explicit context and namespace'
    if ($Arguments[4] -eq 'get') {
        if ($state.FailReads) { $global:LASTEXITCODE = 1; return }
        if ($Arguments[5] -eq 'namespace') { return 'namespace/demo' }
        if ($Arguments[5] -eq 'pvc') {
            Assert-True ($Arguments[6] -eq 'data-kognition-postgres-0') 'Inspect the actual chart-owned database claim'
            if ($state.FailVolumeRead) { $global:LASTEXITCODE = 1; return }
            if ($state.RetainedDatabase) { return 'persistentvolumeclaim/data-kognition-postgres-0' }
            return
        }
        if ($state.Secrets.ContainsKey($Arguments[6])) {
            return ($state.Secrets[$Arguments[6]] | ConvertTo-Json -Depth 10)
        }
        return
    }
    Assert-True ($Arguments[4] -eq 'create') 'Bootstrap must only read or create; never apply, patch or replace credentials'
    if ($state.FailCreates) { $global:LASTEXITCODE = 1; return }
    $manifest = $InputObject | ConvertFrom-Json -AsHashtable
    Assert-True ($manifest.metadata.namespace -eq 'demo') 'Secret manifest must select the namespace'
    $data = @{}
    foreach ($key in $manifest.stringData.Keys) {
        $data[$key] = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($manifest.stringData[$key]))
    }
    $state.Secrets[$manifest.metadata.name] = @{ data = $data }
    return "secret/$($manifest.metadata.name)"
}

$arguments = @{ Context = 'disposable-demo'; Namespace = 'demo' }
$result = & $initializer @arguments
Assert-True ($global:aksSecretTestState.Secrets.Count -eq 2) 'Bootstrap creates database and runtime Secrets'
Assert-True (@($result).Count -eq 1) 'Bootstrap emits only non-secret metadata'
Assert-True ($result.DatabaseSecret -eq 'kognition-database') 'Bootstrap reports Secret names'
$firstRun = $global:aksSecretTestState.Secrets | ConvertTo-Json -Depth 10 -Compress
$null = & $initializer @arguments
Assert-True (($global:aksSecretTestState.Secrets | ConvertTo-Json -Depth 10 -Compress) -ceq $firstRun) 'A rerun must preserve all credentials and signing material'
Assert-True (@($global:aksSecretTestState.Requests | Where-Object { $_[4] -eq 'create' }).Count -eq 2) 'A rerun must not write existing Secrets'
$password = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($global:aksSecretTestState.Secrets['kognition-database'].data.password))
$databaseUrl = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($global:aksSecretTestState.Secrets['kognition-database'].data.DATABASE_URL))
Assert-True ([Convert]::FromBase64String($password).Length -eq 48) 'Database password has 384 bits of random material'
Assert-True ($databaseUrl.Contains([Uri]::EscapeDataString($password))) 'Database URL escapes the random password'
Assert-True (($result | ConvertTo-Json) -notmatch [regex]::Escape($password)) 'No credentials appear in bootstrap output'

$rejected = $false
try { & $initializer @arguments -DatabaseName changed | Out-Null } catch { $rejected = $true }
Assert-True $rejected 'Changing database identity must fail instead of overwriting stored credentials'
$global:aksSecretTestState.Secrets['kognition-runtime-auth'].data['runtime-approval-binding-keys.json'] = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes('[]'))
$rejected = $false
try { & $initializer @arguments | Out-Null } catch { $rejected = $true }
Assert-True $rejected 'Invalid signing keys must not be replaced'
$global:aksSecretTestState.FailReads = $true
$rejected = $false
try { & $initializer @arguments | Out-Null } catch { $rejected = $true }
Assert-True $rejected 'Cluster read failures must fail closed'
$global:aksSecretTestState.FailReads = $false
$global:aksSecretTestState.Secrets.Clear()
foreach ($scenario in @('RetainedDatabase', 'FailVolumeRead')) {
    $global:aksSecretTestState.Requests.Clear()
    $global:aksSecretTestState[$scenario] = $true
    $rejected = $false
    try { & $initializer @arguments | Out-Null } catch { $rejected = $true }
    Assert-True $rejected 'Missing database credentials must fail closed when persistent data exists or cannot be inspected'
    Assert-True (@($global:aksSecretTestState.Requests | Where-Object { $_[4] -eq 'create' }).Count -eq 0) 'Recovery checks must precede all credential writes'
    $global:aksSecretTestState[$scenario] = $false
}
$global:aksSecretTestState.FailCreates = $true
$rejected = $false
try { & $initializer @arguments | Out-Null } catch { $rejected = $true }
Assert-True $rejected 'Cluster write failures must not report success'
Assert-True ($global:aksSecretTestState.Secrets.Count -eq 0) 'Failed creation leaves no fabricated local success state'
Remove-Variable aksSecretTestState -Scope Global
Write-Output 'PASS: explicit context, random credentials, escaped DSN, rerun preservation, signing-key validation and fail-closed cluster access (mock kubectl; no cluster writes)'