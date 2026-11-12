# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

#Requires -Version 7.0
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$installer = Join-Path $PSScriptRoot '../Install-RuntimeBase.ps1'
$global:aksInstallTestState = @{
    Requests = [Collections.Generic.List[object]]::new()
    ValuesPaths = [Collections.Generic.List[string]]::new()
    FailCommand = ''
    NamespaceExists = $true
}

function Assert-True([bool]$Condition, [string]$Message) {
    if (-not $Condition) { throw $Message }
}

function helm {
    $arguments = @($args)
    $state = $global:aksInstallTestState
    $state.Requests.Add(@('helm') + $arguments)
    $global:LASTEXITCODE = 0
    $path = $arguments[[Array]::IndexOf($arguments, '-f') + 1]
    $state.ValuesPaths.Add($path)
    $values = Get-Content -LiteralPath $path -Raw | ConvertFrom-Json -AsHashtable
    Assert-True ($values.fullnameOverride -eq 'demo') 'Installer must use consistent release naming'
    Assert-True ($values.postgres.auth.existingSecret -eq 'demo-database') 'Installer must use matching Secret names'
    Assert-True ($values.runtimeAuth.existingSecret -eq 'demo-runtime-auth') 'Installer must use matching runtime credentials'
    if ($arguments[0] -eq 'upgrade') {
        Assert-True ($arguments -contains '--wait=false') 'Migration hook must run before workload waits'
        Assert-True ($arguments -notcontains '--atomic') 'Atomic implicitly enables workload waiting'
        Assert-True ($arguments[[Array]::IndexOf($arguments, '--kube-context') + 1] -eq 'disposable-demo') 'Helm must select the explicit context'
        Assert-True ($arguments[[Array]::IndexOf($arguments, '--namespace') + 1] -eq 'demo-system') 'Helm must select the explicit namespace'
    }
    if ($state.FailCommand -eq "helm-$($arguments[0])") { $global:LASTEXITCODE = 1 }
}

function docker {
    $arguments = @($args)
    $state = $global:aksInstallTestState
    $state.Requests.Add(@('docker') + $arguments)
    $reference = [string]$arguments[3]
    $isDestination = -not $reference.Contains('@')
    $failure = if ($isDestination) { 'mirror-destination' } else { 'mirror-source' }
    $global:LASTEXITCODE = if ($state.FailCommand -eq $failure) { 1 } else { 0 }
    if ($global:LASTEXITCODE -ne 0) { return }
    if ($isDestination) {
        Assert-True ($reference.StartsWith('demotest.azurecr.io/')) 'Mirror verification must target the selected ACR'
        $digest = switch -Wildcard ($reference) {
            '*/third-party/pgvector:*' { 'sha256:ccc6e83d6e35e931dc7c5def2022729d5a6c370318d099181995567ff1fb4d6b' }
            '*/third-party/busybox:*' { 'sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662' }
            default { throw "Unexpected destination: $reference" }
        }
    }
    else {
        $digest = ($reference -split '@', 2)[1]
    }
    return @{ digest = $digest; manifests = @(@{ platform = @{ os = 'linux'; architecture = 'amd64' } }) } | ConvertTo-Json -Depth 5
}

function kubectl {
    $arguments = @($args)
    $manifest = $input | Out-String
    $state = $global:aksInstallTestState
    $state.Requests.Add(@('kubectl') + $arguments)
    $global:LASTEXITCODE = 0
    Assert-True (($arguments[0..3] -join ' ') -eq '--context disposable-demo --namespace demo-system') 'kubectl must select the explicit context and namespace'
    switch ($arguments[4]) {
        'wait' {
            Assert-True ($arguments -contains 'crd/certificates.cert-manager.io' -and $arguments -contains 'crd/issuers.cert-manager.io') 'Admission certificate prerequisites must be checked'
            if ($state.FailCommand -eq 'kubectl-wait') { $global:LASTEXITCODE = 1 }
        }
        'get' {
            if ($state.FailCommand -eq 'kubectl-get') { $global:LASTEXITCODE = 1; return }
            if ($arguments[5] -eq 'namespace' -and $state.NamespaceExists) { return 'namespace/demo-system' }
        }
        'create' {
            if ($arguments[5] -eq 'namespace') {
                Assert-True ($arguments[6] -eq 'demo-system') 'Installer must create only the selected namespace'
                $state.NamespaceExists = $true
                return
            }
            $secret = $manifest | ConvertFrom-Json -AsHashtable
            Assert-True ($secret.metadata.name -in @('demo-database', 'demo-runtime-auth')) 'Only runtime-base credentials are created'
        }
        'rollout' {
            if ($state.FailCommand -eq 'kubectl-rollout') { $global:LASTEXITCODE = 1 }
        }
        default { throw "Unexpected kubectl operation: $($arguments[4])" }
    }
}

try {
    $parameters = @{ Context = 'disposable-demo'; Namespace = 'demo-system'; Release = 'demo'; ImageRegistry = 'demotest.azurecr.io'; ImageTag = 'build-123' }
    $result = & $installer @parameters
    $requests = $global:aksInstallTestState.Requests
    Assert-True (($requests[0][0..1] -join ' ') -eq 'helm lint') 'Lint precedes cluster changes'
    $firstKubectlIndex = -1
    $lastMirrorIndex = -1
    $upgradeIndex = -1
    for ($index = 0; $index -lt $requests.Count; $index++) {
        if ($requests[$index][0] -in @('docker', 'az')) { $lastMirrorIndex = $index }
        if ($requests[$index][0] -eq 'kubectl' -and $firstKubectlIndex -lt 0) { $firstKubectlIndex = $index }
        if ($requests[$index][0] -eq 'helm' -and $requests[$index][1] -eq 'upgrade') { $upgradeIndex = $index }
        if ($requests[$index][0] -eq 'kubectl' -and $requests[$index][5] -eq 'rollout') {
            Assert-True ($upgradeIndex -ge 0 -and $index -gt $upgradeIndex) 'Rollout waits must follow successful Helm hooks'
        }
    }
    Assert-True ($lastMirrorIndex -gt 0 -and $firstKubectlIndex -gt $lastMirrorIndex) 'Third-party mirror verification must precede every cluster interaction'
    Assert-True (@($requests | Where-Object { $_[0] -eq 'kubectl' -and $_[5] -eq 'rollout' }).Count -eq 8) 'Every stateful/control-plane workload is checked'
    Assert-True (($result -join '') -match 'Inference endpoints.*still require') 'Base installation must not claim an operational model demo'

    $requests.Clear()
    $global:aksInstallTestState.NamespaceExists = $false
    $null = & $installer @parameters
    $namespaceCreateIndex = -1
    $secretCreateIndex = -1
    for ($index = 0; $index -lt $requests.Count; $index++) {
        if ($requests[$index][0] -eq 'kubectl' -and $requests[$index][5] -eq 'create') {
            if ($requests[$index][6] -eq 'namespace') { $namespaceCreateIndex = $index }
            elseif ($secretCreateIndex -lt 0) { $secretCreateIndex = $index }
        }
    }
    Assert-True ($namespaceCreateIndex -ge 0 -and $secretCreateIndex -gt $namespaceCreateIndex) 'Missing namespace must be created before credentials'

    foreach ($failure in @('helm-lint', 'mirror-source', 'mirror-destination', 'kubectl-wait', 'kubectl-get', 'helm-upgrade', 'kubectl-rollout')) {
        $requests.Clear()
        $global:aksInstallTestState.FailCommand = $failure
        $rejected = $false
        try { & $installer @parameters | Out-Null } catch { $rejected = $true }
        Assert-True $rejected "Installer must stop on $failure"
        if ($failure -eq 'helm-lint') { Assert-True ($requests.Count -eq 1) 'Failed lint must not touch the cluster' }
        if ($failure -like 'mirror-*') { Assert-True (@($requests | Where-Object { $_[0] -eq 'kubectl' }).Count -eq 0) 'Failed mirror verification must prevent every cluster interaction' }
        if ($failure -eq 'kubectl-wait') { Assert-True (@($requests | Where-Object { $_[0] -eq 'kubectl' }).Count -eq 1) 'Missing cert-manager must fail before credentials or installation' }
        if ($failure -eq 'kubectl-get') { Assert-True (@($requests | Where-Object { $_[0] -eq 'helm' -and $_[1] -eq 'upgrade' }).Count -eq 0) 'Failed credential check must prevent installation' }
        if ($failure -eq 'helm-upgrade') { Assert-True (@($requests | Where-Object { $_[0] -eq 'kubectl' -and $_[5] -eq 'rollout' }).Count -eq 0) 'Failed migrations must prevent rollout success checks' }
        if ($failure -eq 'kubectl-rollout') { Assert-True (@($requests | Where-Object { $_[0] -eq 'kubectl' -and $_[5] -eq 'rollout' }).Count -eq 1) 'Stop on first failed rollout' }
    }
    foreach ($path in $global:aksInstallTestState.ValuesPaths) {
        Assert-True (-not (Test-Path -LiteralPath $path)) 'Temporary non-secret values must be cleaned up on success and failure'
    }
} finally {
    Remove-Variable aksInstallTestState -Scope Global
}
Write-Output 'PASS: explicit-context installation, third-party provenance before cluster access, credentials before release, hooks before rollouts, failure handling and temporary-file cleanup (mock tools; no cluster writes)'