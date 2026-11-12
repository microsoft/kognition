# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

#Requires -Version 7.0
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$scriptPath = Join-Path $PSScriptRoot '../Sync-ThirdPartyImages.ps1'
$global:thirdPartyImageTestState = @{
    Scenario = 'success'
    Requests = [Collections.Generic.List[object]]::new()
}

function Assert-True([bool]$Condition, [string]$Message) {
    if (-not $Condition) { throw $Message }
}

function docker {
    $arguments = @($args)
    $state = $global:thirdPartyImageTestState
    $state.Requests.Add(@('docker') + $arguments)
    if ($arguments[0] -eq 'buildx' -and $arguments[2] -eq 'create') {
        $global:LASTEXITCODE = if ($state.Scenario -eq 'import-failure') { 1 } else { 0 }
        return 'simulated buildx publication output'
    }
    if ($arguments[0] -eq 'image' -and $arguments[1] -eq 'inspect') {
        $global:LASTEXITCODE = 0
        if ($arguments[2] -like 'demotest.azurecr.io/*') { return 'linux/amd64' }
        return 'sha256:14cea493d9a34af32f524e538b8346cf79f3321eff8e708c1e2960462bd8936e'
    }
    if ($arguments[0] -eq 'run') {
        $global:LASTEXITCODE = 0
        return '7c5bd8512c6e966455b1d198209358b2d191c77a83ab377c4073281065fb855f  /usr/bin/minio'
    }
    if ($arguments[0] -in @('tag', 'push', 'pull') -or ($arguments[0] -eq 'image' -and $arguments[1] -eq 'rm')) {
        $global:LASTEXITCODE = 0
        if ($arguments[0] -eq 'push') { return 'simulated push output' }
        return
    }
    $global:LASTEXITCODE = if ($state.Scenario -eq 'source-failure') { 1 } else { 0 }
    if ($global:LASTEXITCODE -ne 0) { return }
    $reference = $arguments[3]
    $digest = if ($reference -like 'demotest.azurecr.io/*') {
        switch -Wildcard ($reference) {
            '*/third-party/pgvector:*' { 'sha256:ccc6e83d6e35e931dc7c5def2022729d5a6c370318d099181995567ff1fb4d6b' }
            '*/third-party/busybox:*' { 'sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662' }
            '*/third-party/minio/minio:*' { 'sha256:a1a8bd4ac40ad7881a245bab97323e18f971e4d4cba2c2007ec1bedd21cbaba2' }
            default { throw "Unexpected destination: $reference" }
        }
    } else {
        ($reference -split '@', 2)[1]
    }
    if ($state.Scenario -eq 'source-digest-mismatch') { $digest = 'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' }
    if ($state.Scenario -eq 'destination-digest-mismatch' -and $reference -like 'demotest.azurecr.io/*') { $digest = 'sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb' }
    return @{
        digest = $digest
        manifests = @(
            @{ platform = @{ os = 'linux'; architecture = 'amd64' } },
            @{ platform = @{ os = 'linux'; architecture = if ($state.Scenario -eq 'platform-mismatch' -and $reference -like 'demotest.azurecr.io/*') { 's390x' } else { 'arm64' } } },
            @{ platform = @{ os = 'unknown'; architecture = 'unknown' } }
        )
    } | ConvertTo-Json -Depth 6
}

function az {
    $arguments = @($args)
    $state = $global:thirdPartyImageTestState
    $state.Requests.Add(@('az') + $arguments)
    throw "Unexpected az invocation: $($arguments -join ' ')"
}

try {
    $result = @(& $scriptPath -RegistryName 'demotest' -ImageName pgvector, busybox)
    Assert-True ($result.Count -eq 2) 'Existing mirrors must be verified without import'
    Assert-True (@($global:thirdPartyImageTestState.Requests | Where-Object { $_[0] -eq 'az' -and $_[2] -eq 'import' }).Count -eq 0) 'Verification must not mutate ACR'

    $global:thirdPartyImageTestState.Requests.Clear()
    $result = @(& $scriptPath -RegistryName 'demotest' -ImageName pgvector, busybox -Import)
    Assert-True ($result.Count -eq 2 -and @($result | Where-Object Imported).Count -eq 2) 'Imports must be reported'
    Assert-True (@($global:thirdPartyImageTestState.Requests | Where-Object { $_[0] -eq 'docker' -and $_[1] -eq 'buildx' -and $_[3] -eq 'create' }).Count -eq 2) 'Every selected image must be published from the client'
    Assert-True (@($global:thirdPartyImageTestState.Requests | Where-Object { $_[0] -eq 'az' -and $_[2] -eq 'import' }).Count -eq 0) 'The server-side ACR importer must not be used'

    $global:thirdPartyImageTestState.Requests.Clear()
    $result = @(& $scriptPath -RegistryName 'demotest' -ImageName minio -Import)
    Assert-True ($result.Count -eq 1 -and $result[0].Name -eq 'minio' -and $result[0].Imported) 'MinIO publication must return exactly one structured result'
    Assert-True ($result[0].Destination -eq 'demotest.azurecr.io/third-party/minio/minio@sha256:a1a8bd4ac40ad7881a245bab97323e18f971e4d4cba2c2007ec1bedd21cbaba2') 'MinIO must use its independently verified private digest'
    Assert-True (@($global:thirdPartyImageTestState.Requests | Where-Object { $_[0] -eq 'docker' -and $_[1] -eq 'run' }).Count -eq 2) 'MinIO payload must be verified before and after publication'
    Assert-True (@($global:thirdPartyImageTestState.Requests | Where-Object { $_[0] -eq 'docker' -and $_[1] -eq 'pull' }).Count -eq 1) 'MinIO must be pulled back from ACR for verification'

    foreach ($scenario in @('source-failure', 'source-digest-mismatch', 'import-failure', 'destination-digest-mismatch', 'platform-mismatch')) {
        $global:thirdPartyImageTestState.Scenario = $scenario
        $global:thirdPartyImageTestState.Requests.Clear()
        $rejected = $false
        try { & $scriptPath -RegistryName 'demotest' -ImageName pgvector -Import | Out-Null } catch { $rejected = $true }
        Assert-True $rejected "Mirror workflow must reject $scenario"
        if ($scenario -like 'source-*') {
            Assert-True (@($global:thirdPartyImageTestState.Requests | Where-Object { $_[0] -eq 'az' }).Count -eq 0) 'Unverified source must prevent every ACR request'
        }
        if ($scenario -eq 'import-failure') {
            Assert-True (@($global:thirdPartyImageTestState.Requests | Where-Object { $_[0] -eq 'docker' -and $_[1] -eq 'buildx' -and $_[2] -eq 'imagetools' -and $_[3] -eq 'inspect' -and $_[4] -like 'demotest.azurecr.io/*' }).Count -eq 0) 'Failed import must stop before destination verification'
        }
    }
} finally {
    Remove-Variable thirdPartyImageTestState -Scope Global
}
Write-Output 'PASS: immutable sources, optional imports, destination digests and platform metadata are independently verified (mock tools; no registry writes)'