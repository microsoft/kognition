# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$tokens = $null
$parseErrors = $null
$syntax = [Management.Automation.Language.Parser]::ParseFile((Join-Path $PSScriptRoot 'gateway-authority-acceptance.ps1'), [ref]$tokens, [ref]$parseErrors)
if ($parseErrors.Count) { throw ($parseErrors | Out-String) }
$loader = $syntax.Find({ param($node) $node -is [Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq 'Import-AuthorityImages' }, $true)
if (-not $loader) { throw 'An ownership-checked containerd image importer is required' }
. ([scriptblock]::Create($loader.Extent.Text))
$cleanup = $syntax.Find({ param($node) $node -is [Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq 'Remove-OwnedAuthorityCluster' }, $true)
if (-not $cleanup) { throw 'Cleanup must preserve the sealed private kubeconfig' }
. ([scriptblock]::Create($cleanup.Extent.Text))
$Directory = Join-Path ([IO.Path]::GetTempPath()) ("authority-loader-test-" + [guid]::NewGuid().ToString('N'))
$null = New-Item -ItemType Directory -Path $Directory
$bounded = 'bounded-fixture'
$digest = 'sha256:' + ('1' * 64)
$criID = 'sha256:' + ('2' * 64)
$reference = 'docker.io/local/authority:fixture'
$script:scenario = ''
$script:imported = $false
$script:ownershipChecks = 0

function Get-OwnedAuthorityTarget {
    $script:ownershipChecks++
    return @{name='owned-fixture';state=$(if ($script:scenario -eq 'started') {'production-tool-started'} else {'typecheck-passed'})}
}

function Invoke-AuthorityCommand {
    param([string]$Program,[string[]]$Arguments)
    if ($Program -eq 'kind') {
        if ($env:DOCKER_CONTEXT -ne 'desktop-linux') { throw 'Wrong cleanup Docker context' }
        if ($Arguments[0] -eq 'get') { return 'desktop' }
        $configIndex = [array]::IndexOf($Arguments, '--kubeconfig') + 1
        if ($Arguments[0] -ne 'delete' -or $Arguments[3] -ne 'owned-fixture' -or $configIndex -le 0 -or $Arguments[$configIndex] -ne (Join-Path $Directory 'cleanup-kubeconfig')) { throw 'Cleanup must use only its disposable kubeconfig copy' }
        Set-Content -LiteralPath $Arguments[$configIndex] -Value 'kind removed the fixture context'
        if ($script:scenario -eq 'cleanup-fail') { throw 'Synthetic kind cleanup failure' }
        return ''
    }
    if ($Program -eq 'python') {
        $separator = [array]::IndexOf($Arguments, '--')
        if ($separator -lt 0) { throw 'A bounded command is required' }
        $Program = $Arguments[$separator + 1]
        $Arguments = $Arguments[($separator + 2)..($Arguments.Length - 1)]
    }
    if ($Program -ne 'docker' -or $Arguments[0] -ne '--context' -or $Arguments[1] -ne 'desktop-linux') { throw 'Unexpected container target' }
    switch ($Arguments[2]) {
        'image' { return (@{Os='linux';Architecture='amd64';Descriptor=@{digest=$digest}} | ConvertTo-Json -Depth 4 -AsArray) }
        'save' { Set-Content -LiteralPath $Arguments[4] -Value 'synthetic image archive'; return '' }
        'cp' { if ($Arguments[4] -ne 'owned-fixture-control-plane:/var/authority-production-images.tar') { throw 'Wrong import node' }; return '' }
        'exec' {
            if ($Arguments[3] -ne 'owned-fixture-control-plane') { throw 'Wrong inspection node' }
            switch ($Arguments[4]) {
                'sha256sum' {
                    $hash = (Get-FileHash -LiteralPath (Join-Path $Directory 'production-images.tar') -Algorithm SHA256).Hash.ToLowerInvariant()
                    if ($script:scenario -eq 'archive') { $hash = '0' * 64 }
                    return "$hash  /var/authority-production-images.tar"
                }
                'ctr' {
                    if ($Arguments[7] -eq 'import') {
                        if ($Arguments -notcontains '--namespace=k8s.io' -or $Arguments -notcontains 'linux/amd64') { throw 'Import scope or platform missing' }
                        if ($script:scenario -eq 'import') { throw 'Synthetic containerd import failure' }
                        $script:imported = $true
                        return ''
                    }
                    $checkedDigest = if ($script:scenario -eq 'digest') { 'sha256:' + ('3' * 64) } else { $digest }
                    $unpacked = if ($script:scenario -eq 'unpacked') { 'false' } else { 'true' }
                    return "$reference application/vnd.oci.image.index.v1+json $checkedDigest complete (2/2) 1 MiB/1 MiB $unpacked"
                }
                'crictl' {
                    $tags = if ($script:scenario -eq 'cri') { @('docker.io/local/wrong:fixture') } else { @($reference) }
                    return (@{status=@{id=$criID;repoTags=$tags}} | ConvertTo-Json -Depth 4)
                }
                'rm' { return '' }
            }
        }
    }
    throw "Unexpected image command: $Arguments"
}

try {
    foreach ($scenario in @('pass','started','archive','import','digest','unpacked','cri')) {
        $script:scenario = $scenario
        $script:imported = $false
        $images = @(@{name='local/authority:fixture'})
        $failure = $null
        try { Import-AuthorityImages $images } catch { $failure = $_.Exception.Message }
        if ($scenario -eq 'pass') {
            if ($failure -or -not $script:imported -or $images[0].sha256 -ne $digest -or $images[0].criImageID -ne $criID -or $images[0].archiveSHA256 -notmatch '^[0-9a-f]{64}$') { throw "Valid OCI index/CRI configuration identities were not preserved: $failure" }
        } else {
            $expected = @{started='unexecuted typechecked';archive='archive hash mismatch';import='containerd import failure';digest='content completeness';unpacked='content completeness';cri='CRI cannot resolve'}[$scenario]
            if (-not $failure -or $failure -notmatch [regex]::Escape($expected)) { throw "Expected $scenario rejection, got: $failure" }
            if ($scenario -in @('started','archive','import') -and $script:imported) { throw 'Unverified archive or target was imported' }
        }
    }
    if ($script:ownershipChecks -ne 7) { throw 'Every import must revalidate ownership' }
    Write-Host 'PASS: owned direct import, OCI/CRI identities, archive integrity, unpacking and fail-closed errors'
    $kubeconfig = Join-Path $Directory 'kubeconfig'
    Set-Content -LiteralPath $kubeconfig -Value 'sealed synthetic fixture context'
    $sealed = (Get-FileHash -LiteralPath $kubeconfig -Algorithm SHA256).Hash
    $priorDockerContext = $env:DOCKER_CONTEXT
    try {
        $env:DOCKER_CONTEXT = 'preserved-test-context'
        foreach ($scenario in @('cleanup-pass','cleanup-fail','cleanup-existing')) {
            $script:scenario = $scenario
            $copy = Join-Path $Directory 'cleanup-kubeconfig'
            if ($scenario -eq 'cleanup-existing') { Set-Content -LiteralPath $copy -Value 'prior cleanup evidence' }
            $failure = $null
            try { Remove-OwnedAuthorityCluster } catch { $failure = $_.Exception.Message }
            if ((Get-FileHash -LiteralPath $kubeconfig -Algorithm SHA256).Hash -ne $sealed -or $env:DOCKER_CONTEXT -ne 'preserved-test-context') { throw 'Cleanup changed sealed evidence or the caller Docker context' }
            if ($scenario -eq 'cleanup-pass') {
                if ($failure -or (Test-Path -LiteralPath $copy)) { throw "Successful cleanup did not preserve evidence: $failure" }
            } else {
                $expected = if ($scenario -eq 'cleanup-fail') { 'Synthetic kind cleanup failure' } else { 'prior cleanup configuration requires review' }
                if (-not $failure -or $failure -notmatch [regex]::Escape($expected) -or -not (Test-Path -LiteralPath $copy)) { throw "Failed cleanup did not retain its private recovery copy: $failure" }
                Remove-Item -LiteralPath $copy
            }
        }
    } finally { $env:DOCKER_CONTEXT = $priorDockerContext }
    Write-Host 'PASS: cleanup preserves sealed kubeconfig bytes and Docker context across success, failure and interrupted attempts'
} finally {
    $archive = Join-Path $Directory 'production-images.tar'
    if (Test-Path -LiteralPath $archive) { Remove-Item -LiteralPath $archive }
    foreach ($name in @('kubeconfig','cleanup-kubeconfig')) {
        $path = Join-Path $Directory $name
        if (Test-Path -LiteralPath $path) { Remove-Item -LiteralPath $path }
    }
    Remove-Item -LiteralPath $Directory
}
