# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

$ErrorActionPreference = 'Stop'
$script = Join-Path $PSScriptRoot 'tool-sandbox.ps1'
$global:KognitionSandboxFixture = @{
    Calls = [Collections.Generic.List[object]]::new()
    Tools = @()
    FailApply = $false
}

function global:kubectl {
    $arguments = @($args)
    $global:KognitionSandboxFixture.Calls.Add($arguments)
    $global:LASTEXITCODE = 0
    if ($arguments -contains 'tools.kognition.dev') {
        return (@{items=$global:KognitionSandboxFixture.Tools} | ConvertTo-Json -Depth 8 -Compress)
    }
    if ($global:KognitionSandboxFixture.FailApply -and $arguments -contains 'apply') {
        $global:LASTEXITCODE = 1
    }
}

try {
    & $script -Action Install -Context sample-context
    $calls = $global:KognitionSandboxFixture.Calls
    if ($calls.Count -ne 2 -or $calls[0] -notcontains 'apply' -or
        $calls[1] -notcontains 'rollout' -or $calls[1] -notcontains 'deployment/tool-sandbox') {
        throw 'Installation must apply the sample manifest and wait for its exact Deployment.'
    }
    foreach ($call in $calls) {
        if ($call -notcontains '--context' -or $call -notcontains 'sample-context') {
            throw 'Every sample command must use the explicitly selected context.'
        }
    }
    if ($calls[0] -notcontains (Join-Path $PSScriptRoot 'tool-sandbox.yaml')) {
        throw 'Installation must use the server manifest under samples.'
    }

    $calls.Clear()
    & $script -Action Uninstall -Context sample-context
    if ($calls.Count -ne 3 -or $calls[0] -notcontains 'tools.kognition.dev') {
        throw 'Cleanup must check references and delete the two exact resource groups.'
    }
    foreach ($call in @($calls | Where-Object { $_ -contains 'delete' })) {
        if ($call -contains 'namespace' -or $call -contains 'namespaces' -or $call -contains '--all' -or $call -contains '--filename') {
            throw 'Sample cleanup must never delete the namespace or a broad resource selection.'
        }
    }
    if ($calls[1] -notcontains 'deployment/tool-sandbox' -or
        $calls[1] -notcontains 'service/tool-sandbox' -or
        $calls[1] -notcontains 'configmap/tool-sandbox-src' -or
        $calls[2] -notcontains 'toolclasses.kognition.dev/sandbox-mcp') {
        throw 'Cleanup must target only the sample server and its two ToolClasses.'
    }

    $calls.Clear()
    $global:KognitionSandboxFixture.Tools = @(
        @{metadata=@{namespace='demo';name='greet'};spec=@{className='sandbox-mcp'}}
    )
    $failure = $null
    try { & $script -Action Uninstall -Context sample-context } catch { $failure = $_.Exception.Message }
    if (-not $failure -or $failure -notlike '*demo/greet*' -or $calls.Count -ne 1) {
        throw 'Cleanup must fail before mutation while any Tool references the sample ToolClasses.'
    }

    $global:KognitionSandboxFixture.Tools = @()
    $global:KognitionSandboxFixture.FailApply = $true
    $calls.Clear()
    $failure = $null
    try { & $script -Action Install -Context sample-context } catch { $failure = $_.Exception.Message }
    if (-not $failure -or $calls.Count -ne 1) {
        throw 'Failed apply must surface its error and must not run a success-shaped readiness check.'
    }

    $global:KognitionSandboxFixture.FailApply = $false
    $global:LASTEXITCODE = 0
    $calls.Clear()
    & $script -Action Install -Context sample-context -WhatIf
    & $script -Action Uninstall -Context sample-context -WhatIf
    if ($calls.Count -ne 0) { throw 'WhatIf must not contact or mutate a Kubernetes cluster.' }
    Write-Output 'PASS: explicit-context install/readiness, reference-safe exact cleanup, error propagation and WhatIf.'
} finally {
    Remove-Item -LiteralPath Function:\kubectl
    Remove-Variable -Scope Global -Name KognitionSandboxFixture
}
