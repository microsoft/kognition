# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

#Requires -Version 7.0
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
. (Join-Path $repoRoot 'samples\azure-settings.ps1')
Import-Module powershell-yaml

function Assert-Equal($Actual, $Expected, [string]$Message) {
    if ($Actual -cne $Expected) { throw "${Message}: expected '$Expected', got '$Actual'." }
}

$previousVirtualEnv = $env:VIRTUAL_ENV
$scratch = Join-Path $repoRoot ".sample-host-tests-$([guid]::NewGuid().ToString('N'))"
try {
    foreach ($directory in @('active\Scripts', 'active\bin', '.venv\Scripts', '.venv\bin')) {
        New-Item -ItemType Directory -Path (Join-Path $scratch $directory) -Force | Out-Null
    }
    foreach ($relative in @('active\Scripts\python.exe', 'active\bin\python', '.venv\Scripts\python.exe', '.venv\bin\python')) {
        Set-Content -LiteralPath (Join-Path $scratch $relative) -Value 'interpreter fixture'
    }
    $env:VIRTUAL_ENV = Join-Path $scratch 'active'
    foreach ($case in @(
        @{ platform = [PlatformID]::Win32NT; relative = 'Scripts\python.exe' },
        @{ platform = [PlatformID]::Unix; relative = 'bin\python' },
        @{ platform = [PlatformID]::MacOSX; relative = 'bin\python' }
    )) {
        Assert-Equal (Resolve-AzureSamplePython -Platform $case.platform -RepositoryRoot $scratch) (Join-Path $env:VIRTUAL_ENV $case.relative) "$($case.platform) uses the active environment before the repository environment"
        $env:VIRTUAL_ENV = $null
        Assert-Equal (Resolve-AzureSamplePython -Platform $case.platform -RepositoryRoot $scratch) (Join-Path $scratch ".venv\$($case.relative)") "$($case.platform) recognizes the repository environment"
        $env:VIRTUAL_ENV = Join-Path $scratch 'active'
    }
    $env:VIRTUAL_ENV = Join-Path $scratch 'missing'
    $failure = $null
    try { Resolve-AzureSamplePython -RepositoryRoot $scratch | Out-Null } catch { $failure = $_.Exception.Message }
    Assert-Equal ($failure -like '*VIRTUAL_ENV*') $true 'An invalid active environment must not silently use a different interpreter'
    $env:VIRTUAL_ENV = $null
    & {
        $available = @('python', 'python3')
        function Get-Command {
            param([string]$Name, $ErrorAction)
            if ($Name -in $available) { [pscustomobject]@{ Source = "selected-$Name"; Name = $Name } }
        }
        Assert-Equal (Resolve-AzureSamplePython -Platform Win32NT) 'selected-python' 'Windows retains its python command preference'
        Assert-Equal (Resolve-AzureSamplePython -Platform Unix) 'selected-python3' 'Unix prefers python3 when no environment is selected'
        Assert-Equal (Resolve-AzureSamplePython -Platform MacOSX) 'selected-python3' 'macOS prefers python3 when no environment is selected'
        $available = @('python3')
        Assert-Equal (Resolve-AzureSamplePython -Platform Win32NT) 'selected-python3' 'Windows also accepts python3 when python is unavailable'
        $available = @('python')
        Assert-Equal (Resolve-AzureSamplePython -Platform Unix) 'selected-python' 'Unix also accepts python when python3 is unavailable'
        $available = @()
        $failure = $null
        try { Resolve-AzureSamplePython | Out-Null } catch { $failure = $_.Exception.Message }
        Assert-Equal ($failure -like '*Python*') $true 'Missing interpreters produce an actionable error'
    }

    foreach ($platform in @([PlatformID]::Unix, [PlatformID]::MacOSX)) {
        Assert-Equal (Get-AzureSampleProcessOptions -Platform $platform).Count 0 "$platform must not receive Windows-only process options"
    }
    Assert-Equal (Get-AzureSampleProcessOptions -Platform Win32NT)['WindowStyle'] 'Hidden' 'Windows retains hidden background port-forwards'
    $processOptions = Get-AzureSampleProcessOptions
    $child = Start-Process -FilePath (Join-Path $PSHOME 'pwsh') -ArgumentList @('-NoProfile', '-NonInteractive', '-Command', 'exit 0') -PassThru -Wait @processOptions
    try { Assert-Equal $child.ExitCode 0 'The host-specific options launch a real child process' } finally { $child.Dispose() }

    foreach ($relative in @('make.ps1', 'samples\azure-settings.ps1', 'samples\knowledge-base-blob-azure\apply.ps1', 'samples\knowledge-base-blob-s3\apply.ps1')) {
        $tokens = $null
        $parseErrors = $null
        $syntax = [Management.Automation.Language.Parser]::ParseFile((Join-Path $repoRoot $relative), [ref]$tokens, [ref]$parseErrors)
        Assert-Equal $parseErrors.Count 0 "$relative parses"
        $unsupported = @($syntax.FindAll({
            param($node)
            $node -is [Management.Automation.Language.CommandAst] -and $node.GetCommandName() -eq 'Start-Process' -and
            @($node.CommandElements | Where-Object { $_ -is [Management.Automation.Language.CommandParameterAst] -and $_.ParameterName -eq 'WindowStyle' }).Count -gt 0
        }, $true))
        Assert-Equal $unsupported.Count 0 "$relative must not pass WindowStyle unconditionally"
        if ($relative -eq 'make.ps1') {
            $bounded = $syntax.Find({ param($node) $node -is [Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq 'Invoke-BoundedTest' }, $true)
            & {
                . ([scriptblock]::Create($bounded.Extent.Text))
                function Invoke-Native {
                    param([string]$Tool, [string[]]$Arguments)
                    Assert-Equal $Tool (Join-Path $env:VIRTUAL_ENV $(if ($IsWindows) { 'Scripts\python.exe' } else { 'bin\python' })) 'Bounded tests honor the active environment'
                    Assert-Equal ($Arguments -join '|') (@((Join-Path $RepoRoot 'tools/run_bounded.py'), '--timeout', '1800', '--', 'go', 'test', './internal/store/...') -join '|') 'Bounded tests preserve the timeout and native argument boundaries'
                }
                $env:VIRTUAL_ENV = Join-Path $scratch 'active'
                Invoke-BoundedTest -Arguments @('test', './internal/store/...')
            }
        }
    }
    & {
        $processEvents = [Collections.Generic.List[object]]::new()
        $originalEndpoint = $env:KOGNITION_S3_ENDPOINT
        $originalBlobConnection = $env:KOGNITION_BLOB_CONNECTION_STRING
        function Start-Process {
            param($FilePath, [string[]]$ArgumentList, [switch]$PassThru, $WindowStyle, $RedirectStandardOutput, $RedirectStandardError)
            Assert-Equal $FilePath 'kubectl' 'Fixture uploads launch only their owned port-forward'
            Assert-Equal ($ArgumentList -contains '--address=127.0.0.1') $true 'Fixture uploads bind only loopback'
            Assert-Equal ($ArgumentList -contains 'selected-fixture-namespace') $true 'Fixture uploads preserve the reviewed namespace'
            Assert-Equal $PSBoundParameters.ContainsKey('WindowStyle') $IsWindows 'Only Windows receives WindowStyle at the process boundary'
            $port = $ArgumentList[-1].Split(':')[0]
            Set-Content -LiteralPath $RedirectStandardOutput -Value "Forwarding from 127.0.0.1:$port"
            Set-Content -LiteralPath $RedirectStandardError -Value ''
            $processEvents.Add(@{ output = $RedirectStandardOutput; error = $RedirectStandardError })
            return [pscustomobject]@{ Id = 123456789; HasExited = $false }
        }
        function Stop-Process {
            param([int]$Id, [switch]$Force, $ErrorAction)
            Assert-Equal $Id 123456789 'Fixture cleanup stops only the process it started'
            $processEvents.Add('stopped')
        }
        function Invoke-UploadFixture {
            param([string]$Path)
            Assert-Equal (Test-Path -LiteralPath $Path -PathType Leaf) $true 'The selected interpreter receives the existing upload script'
            $global:LASTEXITCODE = $uploadExit
        }
        try {
            foreach ($provider in @('azure', 's3')) {
                foreach ($uploadExit in @(0, 7)) {
                    $failure = $null
                    try {
                        Invoke-AzureSampleFixtureUpload -Provider $provider -Namespace 'selected-fixture-namespace' -Python 'Invoke-UploadFixture' -AccountKey ([Convert]::ToBase64String([byte[]](1..64)))
                    } catch { $failure = $_.Exception.Message }
                    Assert-Equal ([bool]$failure) ($uploadExit -ne 0) "$provider uploads propagate interpreter failures"
                    if ($uploadExit -ne 0) { Assert-Equal ($failure -like '*no KnowledgeBase was applied*') $true 'Failed uploads retain the installation safety gate' }
                    Assert-Equal $processEvents[-1] 'stopped' 'Successful and failed uploads both stop their owned port-forward'
                    $logs = $processEvents[$processEvents.Count - 2]
                    Assert-Equal (Test-Path -LiteralPath $logs.output) $false 'Fixture cleanup removes owned stdout'
                    Assert-Equal (Test-Path -LiteralPath $logs.error) $false 'Fixture cleanup removes owned stderr'
                    Assert-Equal ([string]$env:KOGNITION_S3_ENDPOINT) ([string]$originalEndpoint) 'Fixture cleanup restores the S3 endpoint'
                    Assert-Equal ([string]$env:KOGNITION_BLOB_CONNECTION_STRING) ([string]$originalBlobConnection) 'Fixture cleanup restores the Blob connection'
                }
            }
        } finally { $global:LASTEXITCODE = 0 }
    }
} finally {
    $env:VIRTUAL_ENV = $previousVirtualEnv
    Remove-Item -LiteralPath $scratch -Recurse -Force -ErrorAction SilentlyContinue
}

$settings = @{
    MS_FOUNDRY_PROJECT_ENDPOINT = 'https://new-account.services.ai.azure.com/api/projects/new-project'
    MS_FOUNDRY_INFERENCE_ENDPOINT = 'https://new-account.cognitiveservices.azure.com'
    MS_FOUNDRY_MODEL = 'gpt-6-luna'
    MS_FOUNDRY_INVOCATION_CONTRACT = 'openai-tool-chat-v1'
    MS_FOUNDRY_EMBEDDING_MODEL = 'embedding-deployment'
    MS_FOUNDRY_API_VERSION = '2024-12-01-preview'
    MS_FOUNDRY_EMBEDDING_API_VERSION = '2024-02-01'
}
$modelCount = 0
foreach ($relative in @(
    'simple-agent\manifests.yaml',
    'k8s-mode-demo.yaml',
    'knowledge-base-kognition-docs\model.yaml',
    'knowledge-base-blob-azure\manifests.yaml',
    'knowledge-base-blob-s3\manifests.yaml'
)) {
    $path = Join-Path $repoRoot "samples\$relative"
    $original = @(ConvertFrom-Yaml -Yaml (Get-Content -LiteralPath $path -Raw) -AllDocuments)
    $allRendered = @(ConvertFrom-Yaml -Yaml (Read-AzureSampleManifest -Path $path -Settings $settings) -AllDocuments)
    $profiles = @($allRendered | Where-Object { $_['kind'] -eq 'ModelInvocationProfile' })
    $rendered = @($allRendered | Where-Object { $_['kind'] -ne 'ModelInvocationProfile' })
    Assert-Equal $rendered.Count $original.Count "$relative document count"
    for ($index = 0; $index -lt $rendered.Count; $index++) {
        $document = $rendered[$index]
        Assert-Equal $document['kind'] $original[$index]['kind'] "$relative resource kind"
        Assert-Equal $document['metadata']['name'] $original[$index]['metadata']['name'] "$relative resource alias"
        Assert-Equal $document['metadata']['namespace'] $original[$index]['metadata']['namespace'] "$relative namespace"
        if ($document['kind'] -eq 'Model') {
            $embedding = $original[$index]['spec']['modelID'] -like 'text-embedding-*'
            Assert-Equal $document['spec']['modelID'] $(if ($embedding) { 'embedding-deployment' } else { 'gpt-6-luna' }) "$relative deployment"
            Assert-Equal $document['spec']['endpoint'] $settings.MS_FOUNDRY_INFERENCE_ENDPOINT "$relative endpoint"
            Assert-Equal $document['spec']['credentialRef']['name'] 'foundry-creds' "$relative credential binding"
            Assert-Equal $document['spec']['invocationContract'] $(if ($embedding) { 'openai-embedding-v1' } else { 'openai-tool-chat-v1' }) "$relative explicit invocation contract"
            $profile = @($profiles | Where-Object { $_['metadata']['name'] -eq $document['spec']['invocationProfileRef']['name'] -and $_['metadata']['namespace'] -eq $document['metadata']['namespace'] })
            Assert-Equal $profile.Count 1 "$relative namespace-local invocation profile"
            Assert-Equal $document['spec']['invocationProfileRef']['specDigest'] (Get-AzureSampleInvocationProfileDigest -Spec $profile[0]['spec']) "$relative reviewed profile digest"
            Assert-Equal $document['spec']['parameters']['apiVersion'] $(if ($embedding) { '2024-02-01' } else { '2024-12-01-preview' }) "$relative API version"
            $modelCount++
        }
    }
    Assert-Equal (Read-AzureSampleManifest -Path $path -Settings @{}) (Get-Content -LiteralPath $path -Raw) 'Unconfigured samples preserve their original content'
}
Assert-Equal $modelCount 6 'Every manifest-backed Azure Model is configured'

$helmArguments = @(Get-AzureSampleHelmArguments -Settings $settings)
Assert-Equal ($helmArguments -contains 'model.modelID=gpt-6-luna') $true 'Helm deployment uses the configured model'
Assert-Equal ($helmArguments -contains "model.endpoint=$($settings.MS_FOUNDRY_INFERENCE_ENDPOINT)") $true 'Helm uses the same endpoint'
Assert-Equal (@(Get-AzureSampleHelmArguments -Settings @{}).Count) 0 'Unconfigured Helm samples preserve their defaults'

$encoded = ConvertTo-AzureSampleYaml -Data @{ apiVersion = '2024-02-01'; enabled = $false; dimension = 1536 }
Assert-Equal ($encoded.StartsWith("---`n")) $true 'Generated YAML must select multi-document YAML decoding'
$typed = $encoded.Substring(4) | ConvertFrom-Json
Assert-Equal ($typed.apiVersion -is [string]) $true 'Date-like API versions must remain strings'
Assert-Equal $typed.apiVersion '2024-02-01' 'The API version must not change'
Assert-Equal ($typed.enabled -is [bool]) $true 'Boolean settings must remain booleans'
Assert-Equal ($typed.dimension -is [long] -or $typed.dimension -is [int]) $true 'Numeric dimensions must remain numbers'

$temporary = Join-Path $repoRoot ".azure-settings-test-$([guid]::NewGuid().ToString('N')).env"
try {
    [IO.File]::WriteAllText($temporary, "MS_FOUNDRY_MODEL=gpt-6-luna`n")
    $rejected = $false
    try { Get-AzureSampleSettings -Path $temporary | Out-Null } catch { $rejected = $true }
    Assert-Equal $rejected $true 'Partial model configuration is rejected'
    [IO.File]::WriteAllText($temporary, "# Test settings`nAZURE_CLIENT_SECRET='fake value with = and spaces'`n")
    Assert-Equal (Get-AzureSampleSettings -Path $temporary)['AZURE_CLIENT_SECRET'] 'fake value with = and spaces' 'Quoted environment values are preserved'
} finally {
    if (Test-Path -LiteralPath $temporary) { Remove-Item -LiteralPath $temporary }
}

Write-Output "PASS: Windows/Linux/macOS interpreter and process contracts, bounded-test arguments, fixture upload failure/cleanup safeguards, $modelCount Azure Model bindings, stable resource aliases, Helm settings, environment parsing, and unconfigured compatibility."
