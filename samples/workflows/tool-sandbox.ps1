# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

[CmdletBinding(SupportsShouldProcess = $true)]
param(
    [Parameter(Mandatory = $true)]
    [ValidateSet('Install', 'Uninstall')]
    [string]$Action,

    [Parameter(Mandatory = $true)]
    [ValidateNotNullOrEmpty()]
    [string]$Context,

    [string]$Kubeconfig
)

$ErrorActionPreference = 'Stop'
$manifest = Join-Path $PSScriptRoot 'tool-sandbox.yaml'
$connection = @('--context', $Context, '--request-timeout=30s')
if ($Kubeconfig) {
    if (-not (Test-Path -LiteralPath $Kubeconfig -PathType Leaf)) {
        throw "Kubeconfig file not found: $Kubeconfig"
    }
    $connection += @('--kubeconfig', (Resolve-Path -LiteralPath $Kubeconfig).Path)
}
if (-not (Test-Path -LiteralPath $manifest -PathType Leaf)) {
    throw 'The sample tool-sandbox.yaml manifest is missing.'
}

function Invoke-SandboxKubectl {
    param([string[]]$Arguments)
    $output = & kubectl @connection @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "Sample sandbox command failed (exit $LASTEXITCODE): kubectl $($Arguments -join ' ')"
    }
    return $output
}

if (-not $PSCmdlet.ShouldProcess(
    "$Context/samples-tool-sandbox and ToolClasses sandbox-openapi,sandbox-mcp",
    "$Action the optional sample server"
)) {
    return
}

if ($Action -eq 'Install') {
    Invoke-SandboxKubectl -Arguments @('apply', '--filename', $manifest)
    Invoke-SandboxKubectl -Arguments @('rollout', 'status', 'deployment/tool-sandbox',
        '--namespace', 'samples-tool-sandbox', '--timeout=120s')
    return
}

$json = (Invoke-SandboxKubectl -Arguments @('get', 'tools.kognition.dev', '--all-namespaces', '--output=json')) -join "`n"
$tools = $json | ConvertFrom-Json
if (-not $tools -or $tools.PSObject.Properties.Name -notcontains 'items') {
    throw 'Cannot verify sample ToolClass references: kubectl returned no resource list.'
}
$references = @($tools.items | Where-Object { $_.spec.className -in @('sandbox-openapi', 'sandbox-mcp') })
if ($references.Count) {
    $names = $references | ForEach-Object { "$($_.metadata.namespace)/$($_.metadata.name)" }
    throw "Remove the sample Tools before uninstalling their shared sandbox: $($names -join ', ')."
}
Invoke-SandboxKubectl -Arguments @('delete', 'deployment/tool-sandbox', 'service/tool-sandbox',
    'configmap/tool-sandbox-src', '--namespace', 'samples-tool-sandbox',
    '--ignore-not-found', '--wait=true', '--timeout=120s')
Invoke-SandboxKubectl -Arguments @('delete', 'toolclasses.kognition.dev/sandbox-openapi',
    'toolclasses.kognition.dev/sandbox-mcp', '--ignore-not-found', '--wait=true', '--timeout=120s')
Write-Output 'Sample server removed. The namespace is retained to preserve unrelated resources.'
