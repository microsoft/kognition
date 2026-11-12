# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

function Get-AzureSampleSettings {
    param([string]$Path = (Join-Path $PSScriptRoot '..\.env'))

    $settings = @{}
    if (Test-Path -LiteralPath $Path) {
        foreach ($line in Get-Content -LiteralPath $Path) {
            if ($line -match '^\s*([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*)$') {
                $value = $Matches[2].Trim()
                if ($value.Length -ge 2 -and (
                    ($value.StartsWith('"') -and $value.EndsWith('"')) -or
                    ($value.StartsWith("'") -and $value.EndsWith("'")))) {
                    $value = $value.Substring(1, $value.Length - 2)
                }
                $settings[$Matches[1]] = $value
            }
        }
    }
    $modelKeys = @('MS_FOUNDRY_PROJECT_ENDPOINT', 'MS_FOUNDRY_MODEL', 'MS_FOUNDRY_EMBEDDING_MODEL')
    $configured = @($modelKeys | Where-Object { $settings[$_] })
    if ($configured.Count -gt 0 -and $configured.Count -ne $modelKeys.Count) {
        throw "Azure sample settings require all of: $($modelKeys -join ', ')."
    }
    return $settings
}

function Read-AzureSampleManifest {
    param(
        [Parameter(Mandatory)][string]$Path,
        [hashtable]$Settings = (Get-AzureSampleSettings)
    )

    $content = Get-Content -LiteralPath $Path -Raw
    if (-not $Settings['MS_FOUNDRY_PROJECT_ENDPOINT']) { return $content }
    Import-Module powershell-yaml -ErrorAction Stop
    $chatModelName = Get-AzureSampleChatModelName -Settings $Settings
    $documents = @(ConvertFrom-Yaml -Yaml $content -AllDocuments)
    $profiles = @{}
    foreach ($document in $documents) {
        if (-not $document) { continue }
        Set-AzureSampleChatModelReferences -Value $document -ModelName $chatModelName
        if ($document['kind'] -eq 'ModelGrant') {
            $document['metadata']['name'] = $document['metadata']['name'].Replace('gpt-6-luna', $chatModelName)
        }
        if (-not $document -or $document['kind'] -ne 'Model' -or
            $document['spec']['provider'] -ne 'foundry') { continue }

        $spec = $document['spec']
        $embedding = $spec['modelID'] -like 'text-embedding-*'
        if (-not $embedding -and @($spec['capabilities']) -notcontains 'chat') {
            throw "Cannot select the Azure deployment for Model '$($document['metadata']['name'])' in $Path."
        }
        if (-not $embedding -and $document['metadata']['name'] -eq 'gpt-6-luna') {
            $document['metadata']['name'] = $chatModelName
        }
        $spec['modelID'] = if ($embedding) { $Settings['MS_FOUNDRY_EMBEDDING_MODEL'] } else { $Settings['MS_FOUNDRY_MODEL'] }
        if (-not $embedding -and -not $Settings['MS_FOUNDRY_INVOCATION_CONTRACT']) {
            throw 'Set MS_FOUNDRY_INVOCATION_CONTRACT explicitly; deployment aliases do not identify invocation requirements.'
        }
        $namespace = if ($document['metadata']['namespace']) { $document['metadata']['namespace'] } else { 'default' }
        $profile = New-AzureSampleInvocationProfile -Namespace $namespace -Embedding:$embedding
        $profiles["$namespace/$($profile.metadata.name)"] = $profile
        $spec['invocationProfileRef'] = @{ name = $profile.metadata.name; specDigest = (Get-AzureSampleInvocationProfileDigest -Spec $profile.spec) }
        $spec['invocationContract'] = if ($embedding) { 'openai-embedding-v1' } else { $Settings['MS_FOUNDRY_INVOCATION_CONTRACT'] }
        $spec['endpoint'] = if ($Settings['MS_FOUNDRY_INFERENCE_ENDPOINT']) {
            $Settings['MS_FOUNDRY_INFERENCE_ENDPOINT']
        } else {
            $Settings['MS_FOUNDRY_PROJECT_ENDPOINT']
        }
        $version = if ($embedding) { $Settings['MS_FOUNDRY_EMBEDDING_API_VERSION'] } else { $Settings['MS_FOUNDRY_API_VERSION'] }
        if ($version) {
            if (-not $spec['parameters']) { $spec['parameters'] = @{} }
            $spec['parameters']['apiVersion'] = $version
            $spec['parameters'].Remove('api-version')
        }
    }
    $rendered = @($profiles.Keys | Sort-Object | ForEach-Object { ConvertTo-AzureSampleYaml -Data $profiles[$_] })
    $rendered += @($documents | Where-Object { $_ } | ForEach-Object { ConvertTo-AzureSampleYaml -Data $_ })
    return $rendered -join "`n"
}

function Get-AzureSampleChatModelName {
    param([hashtable]$Settings = (Get-AzureSampleSettings))

    $name = $Settings['MS_FOUNDRY_MODEL']
    if (-not $name -or $name.Length -gt 253 -or @($name.Split('.') | Where-Object {
        $_.Length -gt 63 -or $_ -cnotmatch '^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$'
    }).Count -gt 0) {
        throw 'MS_FOUNDRY_MODEL must be a valid Kubernetes resource name so the sample Model and deployment names agree.'
    }
    return $name
}

function Set-AzureSampleChatModelReferences {
    param([object]$Value, [string]$ModelName)

    if ($Value -is [System.Collections.IDictionary]) {
        if ($Value['modelRef'] -is [System.Collections.IDictionary] -and $Value['modelRef']['name'] -eq 'gpt-6-luna') {
            $Value['modelRef']['name'] = $ModelName
        }
        if ($Value['kind'] -eq 'Model' -and $Value['name'] -eq 'gpt-6-luna') {
            $Value['name'] = $ModelName
        }
        foreach ($key in @($Value.Keys)) {
            Set-AzureSampleChatModelReferences -Value $Value[$key] -ModelName $ModelName
        }
    } elseif ($Value -is [System.Collections.IEnumerable] -and $Value -isnot [string]) {
        foreach ($item in $Value) {
            Set-AzureSampleChatModelReferences -Value $item -ModelName $ModelName
        }
    }
}

function New-AzureSampleInvocationProfile {
    param([string]$Namespace = 'default', [switch]$Embedding)

    $spec = if ($Embedding) {
        @{ operation = 'embedding' }
    } else {
        @{
            operation = 'chat'
            defaults = @{ maxCompletionTokens = 4096; reasoning = 'disabled' }
            limits = @{ maxCompletionTokens = 16384 }
            requirements = @{ toolCalling = 'required'; reasoning = 'disabled' }
            parameterHandling = @{ unknown = 'Reject'; unsupported = 'Reject' }
        }
    }
    @{
        apiVersion = 'kognition.dev/v1alpha1'
        kind = 'ModelInvocationProfile'
        metadata = @{ name = $(if ($Embedding) { 'sample-embedding-v1' } else { 'sample-chat-tools-v1' }); namespace = $Namespace }
        spec = $spec
    }
}

function Get-AzureSampleInvocationProfileDigest {
    param([Parameter(Mandatory)][System.Collections.IDictionary]$Spec)

    function ConvertTo-SortedSampleSpec($Value) {
        if ($Value -is [System.Collections.IDictionary]) {
            $result = [ordered]@{}
            foreach ($key in ($Value.Keys | Sort-Object -CaseSensitive)) {
                if ($Value[$key] -is [System.Collections.IDictionary] -and $Value[$key].Count -eq 0) { continue }
                $result[$key] = ConvertTo-SortedSampleSpec $Value[$key]
            }
            return $result
        }
        return $Value
    }
    $canonical = ConvertTo-SortedSampleSpec $Spec | ConvertTo-Json -Depth 20 -Compress
    $bytes = [Text.Encoding]::UTF8.GetBytes("kognition.model-invocation/v1`n$canonical")
    $algorithm = [Security.Cryptography.SHA256]::Create()
    try {
        return 'sha256:' + (($algorithm.ComputeHash($bytes) | ForEach-Object { $_.ToString('x2') }) -join '')
    } finally {
        $algorithm.Dispose()
    }
}

function ConvertTo-AzureSampleYaml {
    param([Parameter(Mandatory)][object]$Data)

    # JSON-compatible YAML preserves date-like strings. The document marker
    # keeps Kubernetes' decoder in YAML mode for multi-document streams.
    return "---`n" + (ConvertTo-Json -InputObject $Data -Depth 100)
}

function Invoke-AzureSampleApply {
    param([Parameter(Mandatory)][string]$Path)

    Read-AzureSampleManifest -Path $Path | & kubectl apply -f -
    if ($LASTEXITCODE -ne 0) { throw "Sample manifest apply failed: $Path" }
}

function Invoke-AzureSampleDelete {
    param([Parameter(Mandatory)][string]$Path)

    $files = if (Test-Path -LiteralPath $Path -PathType Container) {
        Get-ChildItem -LiteralPath $Path -File -Filter '*.yaml' | Where-Object {
            $_.Name -notlike '*.template.yaml' -and (Select-String -LiteralPath $_.FullName -Pattern '^\s*kind\s*:' -Quiet)
        }
    } else {
        Get-Item -LiteralPath $Path
    }
    foreach ($file in $files) {
        Read-AzureSampleManifest -Path $file.FullName | & kubectl delete -f - --ignore-not-found --wait=false
        if ($LASTEXITCODE -ne 0) { throw "Sample manifest deletion failed: $($file.FullName)" }
    }
}

function Get-AzureSampleHelmArguments {
    param([hashtable]$Settings = (Get-AzureSampleSettings))

    if (-not $Settings['MS_FOUNDRY_PROJECT_ENDPOINT']) { return }
    if (-not $Settings['MS_FOUNDRY_INVOCATION_CONTRACT']) {
        throw 'Set MS_FOUNDRY_INVOCATION_CONTRACT before installing the Azure Helm sample.'
    }
    $endpoint = if ($Settings['MS_FOUNDRY_INFERENCE_ENDPOINT']) {
        $Settings['MS_FOUNDRY_INFERENCE_ENDPOINT']
    } else {
        $Settings['MS_FOUNDRY_PROJECT_ENDPOINT']
    }
    @('--set-string', "model.name=$(Get-AzureSampleChatModelName -Settings $Settings)",
        '--set-string', "model.modelID=$($Settings['MS_FOUNDRY_MODEL'])",
        '--set-string', "model.endpoint=$endpoint",
        '--set-string', "model.invocationContract=$($Settings['MS_FOUNDRY_INVOCATION_CONTRACT'])",
        '--set', 'invocationProfile.create=true',
        '--set-string', 'invocationProfile.name=sample-chat-tools-v1',
        '--set-string', "invocationProfile.specDigest=$(Get-AzureSampleInvocationProfileDigest -Spec (New-AzureSampleInvocationProfile).spec)",
        '--set-json', "invocationProfile.spec=$((New-AzureSampleInvocationProfile).spec | ConvertTo-Json -Depth 10 -Compress)")
    if ($Settings['MS_FOUNDRY_API_VERSION']) {
        @('--set-string', "model.apiVersion=$($Settings['MS_FOUNDRY_API_VERSION'])")
    }
}
