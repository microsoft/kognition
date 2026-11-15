# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

function Resolve-AzureSamplePython {
    param(
        [string]$RepositoryRoot,
        [PlatformID]$Platform = [Environment]::OSVersion.Platform
    )

    $windowsHost = $Platform -eq [PlatformID]::Win32NT
    $relative = if ($windowsHost) { 'Scripts\python.exe' } else { 'bin\python' }
    if ($env:VIRTUAL_ENV) {
        $python = Join-Path $env:VIRTUAL_ENV $relative
        if (-not (Test-Path -LiteralPath $python -PathType Leaf)) {
            throw "VIRTUAL_ENV must contain $relative. Reactivate a Python environment created for this host."
        }
        return $python
    }
    if ($RepositoryRoot) {
        $python = Join-Path (Join-Path $RepositoryRoot '.venv') $relative
        if (Test-Path -LiteralPath $python -PathType Leaf) { return $python }
    }
    $names = if ($windowsHost) { @('python', 'python3') } else { @('python3', 'python') }
    foreach ($name in $names) {
        $command = Get-Command $name -ErrorAction SilentlyContinue | Select-Object -First 1
        if ($command) {
            if ($command.Source) { return $command.Source }
            return $command.Name
        }
    }
    throw 'Python is required. Activate a host-native virtual environment or install python3/python on PATH.'
}

function Get-AzureSampleProcessOptions {
    param([PlatformID]$Platform = [Environment]::OSVersion.Platform)

    if ($Platform -eq [PlatformID]::Win32NT) { return @{ WindowStyle = 'Hidden' } }
    return @{}
}

function Get-AzureSampleProfile {
    param([string]$Path = $env:KOGNITION_SAMPLE_PROFILE_PATH)

    if (-not $Path) { return $null }
    if (-not (Test-Path -LiteralPath $Path -PathType Leaf)) { throw 'SAMPLE_PROFILE must identify an existing private JSON file.' }
    try {
        $json = Get-Content -LiteralPath $Path -Raw
        $null = ConvertFrom-Json -InputObject $json -ErrorAction Stop
        Import-Module powershell-yaml -ErrorAction Stop
        $profile = ConvertFrom-Yaml -Yaml $json
    } catch { throw 'SAMPLE_PROFILE must contain valid JSON (powershell-yaml is required).' }
    if ($profile -isnot [System.Collections.IDictionary] -or
        ($profile['schemaVersion'] -isnot [int] -and $profile['schemaVersion'] -isnot [long]) -or $profile['schemaVersion'] -ne 1) {
        throw 'SAMPLE_PROFILE requires schemaVersion 1.'
    }
    $readinessTimeout = $profile['readinessTimeoutSeconds']
    if (($readinessTimeout -isnot [int] -and $readinessTimeout -isnot [long]) -or
        $readinessTimeout -lt 60 -or $readinessTimeout -gt 3600) {
        throw 'SAMPLE_PROFILE readinessTimeoutSeconds must be an explicit integer between 60 and 3600.'
    }
    $map = $profile['namespaceMap']
    $workloads = @('default', 'kognition-demo', 'samples-governance', 'samples-multi-agent', 'samples-azure-blob', 'production', 'development')
    $fixtures = @('samples-azure-blob-services', 'samples-s3-blob-services', 'samples-kognition-docs-services', 'samples-tool-sandbox')
    if ($map -isnot [System.Collections.IDictionary]) { throw 'SAMPLE_PROFILE requires namespaceMap.' }
    foreach ($name in @($workloads + $fixtures)) {
        if (-not $map[$name]) { throw "SAMPLE_PROFILE namespaceMap requires '$name'." }
    }
    foreach ($name in $map.Keys) {
        if ($name -cnotmatch '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$' -or
            $map[$name] -isnot [string] -or $map[$name] -cnotmatch '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$' -or
            $map[$name] -in @('default', 'kube-system', 'kube-public', 'kube-node-lease') -or
            ($map.Contains($map[$name]) -and $map[$name] -cne $name)) {
            throw 'SAMPLE_PROFILE namespaceMap requires valid, non-system, idempotent namespace mappings.'
        }
    }
    $targets = @($workloads | ForEach-Object { $map[$_] })
    if (@($targets | Select-Object -Unique).Count -ne $workloads.Count -or
        @($fixtures | Where-Object { $targets -contains $map[$_] }).Count -gt 0) {
        throw 'SAMPLE_PROFILE must isolate workload namespaces from each other and from fixtures.'
    }
    $backend = $profile['knowledgeBackend']
    if ($backend -isnot [System.Collections.IDictionary]) { throw 'SAMPLE_PROFILE requires knowledgeBackend.' }
    if (@($backend.Keys | Where-Object { $_ -notin @('name', 'host', 'port', 'database', 'table', 'embeddingDimensions', 'credentialsSecretRef', 'tls') }).Count -gt 0) {
        throw 'SAMPLE_PROFILE knowledgeBackend accepts only connection coordinates, dimensions, and scoped Secret/TLS references.'
    }
    foreach ($name in @('name', 'host', 'database', 'table')) {
        if ($backend[$name] -isnot [string] -or -not $backend[$name].Trim()) { throw "SAMPLE_PROFILE knowledgeBackend requires '$name'." }
    }
    if ($backend['name'] -cnotmatch '^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$' -or
        $backend['host'] -cnotmatch '^[a-zA-Z0-9][a-zA-Z0-9.-]*$' -or
        $backend['database'] -cnotmatch '^[a-zA-Z_][a-zA-Z0-9_]*$' -or
        $backend['table'] -cnotmatch '^[a-zA-Z_][a-zA-Z0-9_]*$' -or
        ($backend['port'] -isnot [int] -and $backend['port'] -isnot [long]) -or $backend['port'] -lt 1 -or $backend['port'] -gt 65535 -or
        ($backend['embeddingDimensions'] -isnot [int] -and $backend['embeddingDimensions'] -isnot [long]) -or $backend['embeddingDimensions'] -ne 1536) {
        throw 'SAMPLE_PROFILE requires valid backend identifiers, integer port and embeddingDimensions=1536.'
    }
    if ($backend['tls'] -isnot [System.Collections.IDictionary]) { throw 'SAMPLE_PROFILE requires tls.' }
    foreach ($entry in @(
        @{ value = $backend['credentialsSecretRef']; keys = @('name', 'usernameKey', 'passwordKey') },
        @{ value = $backend['tls']['caSecretRef']; keys = @('name', 'key') }
    )) {
        if ($entry.value -isnot [System.Collections.IDictionary]) { throw 'SAMPLE_PROFILE requires credentialsSecretRef and tls.caSecretRef.' }
        foreach ($key in $entry.keys) {
            if ($entry.value[$key] -isnot [string] -or $entry.value[$key] -cnotmatch '^[a-zA-Z0-9._-]+$') {
                throw "SAMPLE_PROFILE requires valid Secret reference '$key'."
            }
            if ($entry.value['name'].Length -gt 253 -or @($entry.value['name'].Split('.') | Where-Object {
                $_.Length -gt 63 -or $_ -cnotmatch '^[a-z0-9](?:[-a-z0-9]*[a-z0-9])?$'
            }).Count -gt 0) { throw 'SAMPLE_PROFILE requires valid Kubernetes Secret names.' }
        }
    }
    if ($backend['tls']['mode'] -cne 'verify-full') { throw 'SAMPLE_PROFILE requires tls.mode=verify-full.' }
    if ($profile['fixtures'] -isnot [System.Collections.IDictionary]) { throw 'SAMPLE_PROFILE requires fixtures.' }
    foreach ($image in @('azuriteImage', 's3Image', 'docsImage')) {
        if ($profile['fixtures'][$image] -isnot [string] -or
            $profile['fixtures'][$image] -cnotmatch '^[a-z0-9][a-z0-9./:_-]*@sha256:[a-f0-9]{64}$' -or
            $profile['fixtures'][$image] -match ':latest@') {
            throw "SAMPLE_PROFILE fixtures.$image must be an explicit digest-pinned reviewed image."
        }
    }
    if ($profile['budgets'] -isnot [System.Collections.IDictionary]) { throw 'SAMPLE_PROFILE requires explicit bounded budgets.' }
    foreach ($limit in @(
        @{ key = 'agents'; min = 48; max = 512 }, @{ key = 'tools'; min = 32; max = 256 },
        @{ key = 'workflows'; min = 32; max = 256 }, @{ key = 'maxInputTokens'; min = 100000; max = 10000000 },
        @{ key = 'maxOutputTokens'; min = 32768; max = 2000000 }
    )) {
        $value = $profile['budgets'][$limit.key]
        if (($value -isnot [int] -and $value -isnot [long]) -or $value -lt $limit.min -or $value -gt $limit.max) {
            throw "SAMPLE_PROFILE budgets.$($limit.key) must be an integer between $($limit.min) and $($limit.max)."
        }
    }
    return $profile
}

function Get-AzureSampleReadinessTimeoutSeconds {
    param([object]$Profile = (Get-AzureSampleProfile))
    if ($Profile) { return $Profile['readinessTimeoutSeconds'] }
    return 300
}

function Get-AzureSampleNamespace {
    param([string]$Namespace = 'default', [object]$Profile = (Get-AzureSampleProfile))
    if ($Profile -and $Profile['namespaceMap'].Contains($Namespace)) { return $Profile['namespaceMap'][$Namespace] }
    return $Namespace
}

function Set-AzureSampleNamespaceReferences {
    param([object]$Value, [System.Collections.IDictionary]$Map)

    if ($Value -is [System.Collections.IDictionary]) {
        foreach ($key in @($Value.Keys)) {
            if ($key -in @('namespace', 'kubernetes.io/metadata.name') -and $Map.Contains([string]$Value[$key])) {
                $Value[$key] = $Map[[string]$Value[$key]]
            } elseif ($Value[$key] -is [string]) {
                foreach ($source in $Map.Keys) {
                    $Value[$key] = $Value[$key] -creplace ("\." + [regex]::Escape($source) + '\.svc(?=[.:/]|$)'), ".$($Map[$source]).svc"
                }
            } else {
                Set-AzureSampleNamespaceReferences -Value $Value[$key] -Map $Map
            }
        }
    } elseif ($Value -is [System.Collections.IList]) {
        for ($index = 0; $index -lt $Value.Count; $index++) {
            if ($Value[$index] -is [string]) {
                foreach ($source in $Map.Keys) {
                    $Value[$index] = $Value[$index] -creplace ("\." + [regex]::Escape($source) + '\.svc(?=[.:/]|$)'), ".$($Map[$source]).svc"
                }
            } else {
                Set-AzureSampleNamespaceReferences -Value $Value[$index] -Map $Map
            }
        }
    }
}

function ConvertTo-GovernedSampleDocuments {
    param([object[]]$Documents, [System.Collections.IDictionary]$Profile)

    $backendNamespaces = @{}
    $redundantProviderRuns = @{
        'azure-docs-reindex-now' = 'azure-docs-kb'
        's3-docs-reindex-now' = 's3-docs-kb'
        'kognition-docs-reindex-now' = 'kognition-docs-kb'
    }
    $providerNamespace = Get-AzureSampleNamespace -Profile $Profile
    $clusterKinds = @('Namespace', 'ClusterRole', 'ClusterRoleBinding', 'ValidatingAdmissionPolicy',
        'ValidatingAdmissionPolicyBinding', 'AutonomyClass', 'KnowledgeBaseClass', 'MemoryClass', 'ModelClass', 'ToolClass')
    foreach ($document in $Documents) {
        if (-not $document) { continue }
        if ($document['kind'] -eq 'SecretRef' -and $document['metadata']['name'] -in @('foundry-creds', 'kognition-kb-pgvector')) { continue }
        Set-AzureSampleNamespaceReferences -Value $document -Map $Profile['namespaceMap']
        if ($document['kind'] -eq 'Namespace') {
            $document['metadata']['name'] = Get-AzureSampleNamespace -Namespace $document['metadata']['name'] -Profile $Profile
        } elseif ($document['kind'] -notin $clusterKinds -and -not $document['metadata']['namespace']) {
            $document['metadata']['namespace'] = Get-AzureSampleNamespace -Profile $Profile
        }
        if ($document['kind'] -eq 'CrossNamespaceGrant') {
            foreach ($key in @('from', 'to')) {
                $document['spec'][$key] = Get-AzureSampleNamespace -Namespace $document['spec'][$key] -Profile $Profile
            }
        }
        $spec = $document['spec']
        # Initial provider indexing already owns the immutable generation.
        # These bundled demos would request a second operation for that generation.
        if ($document['kind'] -eq 'KnowledgeBaseIndexRun' -and
            $document['metadata']['namespace'] -eq $providerNamespace -and
            $redundantProviderRuns.ContainsKey($document['metadata']['name']) -and
            $spec['knowledgeBaseName'] -eq $redundantProviderRuns[$document['metadata']['name']]) { continue }
        if ($document['kind'] -in @('KnowledgeBaseClass', 'KnowledgeBase') -and
            ($spec['provisioner'] -eq 'pgvector' -or ($spec['vectorStore'] -and $spec['vectorStore']['type'] -eq 'pgvector'))) {
            $spec.Remove('vectorStore')
            $spec['backendRef'] = @{ apiGroup = 'pgvector.knowledge.kognition.dev'; kind = 'PgvectorBackend'; name = $Profile['knowledgeBackend']['name'] }
            $ns = if ($document['metadata']['namespace']) { $document['metadata']['namespace'] } else { Get-AzureSampleNamespace -Profile $Profile }
            $backendNamespaces[$ns] = $true
        }
        if ($document['kind'] -eq 'KnowledgeBase') { $spec['refreshPolicy'] = @{ mode = 'onDemand' } }
        if ($document['kind'] -eq 'KnowledgeBaseIndexSchedule') { $spec['suspend'] = $true }
        if ($document['kind'] -eq 'Agent' -and $spec['probes']) {
            $spec['probes'].Remove('reasoningProbe')
        }
        if ($document['kind'] -eq 'CognitiveQuota') {
            foreach ($key in @('agents', 'tools', 'workflows')) { $spec['hard'][$key] = $Profile['budgets'][$key] }
            $spec['tokenBudget'] = @{ maxInputTokens = $Profile['budgets']['maxInputTokens']; maxOutputTokens = $Profile['budgets']['maxOutputTokens'] }
            $spec['resetInterval'] = '24h'
        }
        if ($document['kind'] -eq 'AgentInstance') {
            if (-not $document['metadata']['labels']) { $document['metadata']['labels'] = @{} }
            $document['metadata']['labels']['kognition.dev/sample-bundle'] = 'compatible'
        }
        if ($document['kind'] -eq 'Deployment') {
            $images = @{ azurite = 'azuriteImage'; minio = 's3Image'; 'kognition-docs' = 'docsImage'; 'tool-sandbox' = 'docsImage' }
            $name = $document['metadata']['name']
            if ($images.ContainsKey($name)) {
                $spec['template']['spec']['containers'][0]['image'] = $Profile['fixtures'][$images[$name]]
            }
        }
        $document
    }
    foreach ($ns in ($backendNamespaces.Keys | Sort-Object)) {
        $spec = @{}
        foreach ($key in $Profile['knowledgeBackend'].Keys) {
            if ($key -ne 'name') { $spec[$key] = $Profile['knowledgeBackend'][$key] }
        }
        @{ apiVersion = 'pgvector.knowledge.kognition.dev/v1alpha1'; kind = 'PgvectorBackend'
            metadata = @{ name = $Profile['knowledgeBackend']['name']; namespace = $ns }; spec = $spec }
    }
}

function Assert-AzureSampleBackendPrerequisites {
    param([string[]]$Namespaces, [object]$Profile = (Get-AzureSampleProfile))
    if (-not $Profile) { return }
    $backend = $Profile['knowledgeBackend']
    foreach ($ns in ($Namespaces | Select-Object -Unique)) {
        foreach ($reference in @(
            @{ name = $backend['credentialsSecretRef']['name']; keys = @($backend['credentialsSecretRef']['usernameKey'], $backend['credentialsSecretRef']['passwordKey']) },
            @{ name = $backend['tls']['caSecretRef']['name']; keys = @($backend['tls']['caSecretRef']['key']) }
        )) {
            $secretJSON = (& kubectl get secret $reference.name -n $ns -o json 2>$null | Out-String)
            if ($LASTEXITCODE -ne 0) { throw "Provision the governed backend Secret '$($reference.name)' in '$ns' before installing samples." }
            $secret = $secretJSON | ConvertFrom-Json
            foreach ($key in $reference.keys) {
                if (-not $secret.data -or -not $secret.data.PSObject.Properties[$key] -or -not $secret.data.$key) {
                    throw "Governed backend Secret '$($reference.name)' in '$ns' requires key '$key'."
                }
            }
        }
    }
}

$script:AzuriteAccountName = 'devstoreaccount1'
$script:AzuriteAccountSecret = 'azurite-account'

# Azurite is configured with a per-installation random account key held in a Secret,
# so no storage key is ever committed or shared between installations.
function Initialize-AzuriteAccount {
    param([Parameter(Mandatory)][string]$Namespace)

    & kubectl get namespace $Namespace *> $null
    if ($LASTEXITCODE -ne 0) {
        & kubectl create namespace $Namespace | Out-Null
        if ($LASTEXITCODE -ne 0) { throw "Could not create the Azurite fixture namespace $Namespace." }
    }
    $existing = (& kubectl get secret $script:AzuriteAccountSecret -n $Namespace -o json --ignore-not-found | Out-String).Trim()
    if ($LASTEXITCODE -ne 0) { throw "Could not inspect the Azurite account Secret in $Namespace." }
    if ($existing) {
        $secret = $existing | ConvertFrom-Json
        $data = $secret.PSObject.Properties['data']
        $stored = if ($data -and $data.Value.PSObject.Properties['accountKey']) { $data.Value.accountKey } else { $null }
        if ($stored) { return [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($stored)) }
    }
    $bytes = New-Object byte[] 64
    $generator = [Security.Cryptography.RandomNumberGenerator]::Create()
    try { $generator.GetBytes($bytes) } finally { $generator.Dispose() }
    $key = [Convert]::ToBase64String($bytes)
    ConvertTo-AzureSampleYaml -Data @{
        apiVersion = 'v1'; kind = 'Secret'; type = 'Opaque'
        metadata = @{ name = $script:AzuriteAccountSecret; namespace = $Namespace
            labels = @{ 'app.kubernetes.io/name' = 'azurite' } }
        stringData = @{ accounts = "$($script:AzuriteAccountName):$key"; accountKey = $key }
    } | & kubectl apply -f - | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "Could not store the Azurite account Secret in $Namespace." }
    return $key
}

function New-AzuriteConnectionString {
    param([Parameter(Mandatory)][string]$AccountKey, [Parameter(Mandatory)][string]$BlobEndpoint)

    return "DefaultEndpointsProtocol=http;AccountName=$($script:AzuriteAccountName);AccountKey=$AccountKey;BlobEndpoint=$BlobEndpoint/$($script:AzuriteAccountName);"
}

function Invoke-AzureSampleFixtureUpload {
    param([ValidateSet('azure', 's3')][string]$Provider, [string]$Namespace, [string]$Python, [string]$AccountKey)

    if ($Provider -eq 'azure' -and [string]::IsNullOrWhiteSpace($AccountKey)) {
        throw 'The Azure fixture upload requires the installation-specific Azurite account key.'
    }

    $listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0)
    try { $listener.Start(); $port = $listener.LocalEndpoint.Port } finally { $listener.Stop() }
    $logBase = Join-Path (Split-Path $PSScriptRoot -Parent) ".sample-upload-$([guid]::NewGuid().ToString('N'))"
    $environment = @{}
    $values = @{
        KOGNITION_DOCS_DIR = (Join-Path $PSScriptRoot 'docs')
        KOGNITION_BLOB_CONTAINER = 'product-docs'; KOGNITION_BLOB_PREFIX = 'mark7'
        KOGNITION_S3_BUCKET = 'product-docs'; KOGNITION_S3_PREFIX = 'mark7'; KOGNITION_S3_REGION = 'us-east-1'
    }
    $service = if ($Provider -eq 'azure') { 'azurite' } else { 'minio' }
    $remotePort = if ($Provider -eq 'azure') { 10000 } else { 9000 }
    if ($Provider -eq 'azure') {
        $values['KOGNITION_BLOB_CONNECTION_STRING'] = New-AzuriteConnectionString -AccountKey $AccountKey -BlobEndpoint "http://127.0.0.1:$port"
    } else {
        $values['KOGNITION_S3_ENDPOINT'] = "http://127.0.0.1:$port"
        $values['KOGNITION_S3_ACCESS_KEY'] = 'minioadmin'
        $values['KOGNITION_S3_SECRET_KEY'] = 'minioadmin'
    }
    foreach ($name in $values.Keys) {
        $environment[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
        [Environment]::SetEnvironmentVariable($name, $values[$name], 'Process')
    }
    $pf = $null
    try {
        $processOptions = Get-AzureSampleProcessOptions
        $pf = Start-Process kubectl -ArgumentList @('port-forward', '--address=127.0.0.1', '-n', $Namespace, "svc/$service", "${port}:$remotePort") `
            -PassThru @processOptions -RedirectStandardOutput "$logBase.out" -RedirectStandardError "$logBase.err"
        $deadline = [DateTime]::UtcNow.AddSeconds(30)
        do {
            if ($pf.HasExited) { throw 'The owned sample upload port-forward exited before binding.' }
            if ((Get-Content -LiteralPath "$logBase.out" -Raw) -match "Forwarding from 127\.0\.0\.1:$port") { break }
            if ([DateTime]::UtcNow -ge $deadline) { throw 'The owned sample upload port-forward did not bind before its deadline.' }
            Start-Sleep -Milliseconds 200
        } while ($true)
        $directory = if ($Provider -eq 'azure') { 'knowledge-base-blob-azure' } else { 'knowledge-base-blob-s3' }
        & $Python (Join-Path $PSScriptRoot "$directory\upload_docs.py")
        if ($LASTEXITCODE -ne 0) { throw "Verified $Provider fixture upload failed; no KnowledgeBase was applied." }
    } finally {
        if ($pf -and -not $pf.HasExited) { Stop-Process -Id $pf.Id -Force -ErrorAction SilentlyContinue }
        foreach ($name in $environment.Keys) { [Environment]::SetEnvironmentVariable($name, $environment[$name], 'Process') }
        Remove-Item -LiteralPath "$logBase.out", "$logBase.err" -Force -ErrorAction SilentlyContinue
    }
}

function Invoke-AzureSampleKnowledgeInstall {
    param([ValidateSet('azure', 's3', 'docs')][string]$Source, [string]$SampleDirectory)

    $profile = Get-AzureSampleProfile
    if (-not $profile) { throw 'This installation path requires SAMPLE_PROFILE.' }
    $targetNamespace = Get-AzureSampleNamespace -Profile $profile
    Assert-AzureSampleBackendPrerequisites -Namespaces @($targetNamespace) -Profile $profile
    $fixture = switch ($Source) {
        'azure' { @{ file = 'azurite.yaml'; namespace = 'samples-azure-blob-services'; deployment = 'azurite' } }
        's3' { @{ file = 's3-store.yaml'; namespace = 'samples-s3-blob-services'; deployment = 'minio' } }
        'docs' { @{ file = 'docs-server.yaml'; namespace = 'samples-kognition-docs-services'; deployment = 'kognition-docs' } }
    }
    $serviceNamespace = Get-AzureSampleNamespace -Namespace $fixture.namespace -Profile $profile
    if ($Source -eq 'docs') {
        $root = Split-Path $PSScriptRoot -Parent
        $files = @('README.md', 'PROJECT.md', 'BACKLOG.md', 'docs\kognition-manifest.md') | ForEach-Object { Join-Path $root $_ }
        foreach ($file in $files) { if (-not (Test-Path -LiteralPath $file)) { throw "Missing source document: $file" } }
        & kubectl create namespace $serviceNamespace --dry-run=client -o yaml | & kubectl apply -f -
        if ($LASTEXITCODE -ne 0) { throw 'Docs fixture namespace setup failed.' }
        $arguments = @('create', 'configmap', 'kognition-docs', '-n', $serviceNamespace)
        $arguments += @($files | ForEach-Object { "--from-file=$_" })
        $arguments += @('--dry-run=client', '-o', 'yaml')
        $configMap = (& kubectl @arguments | Out-String)
        if ($LASTEXITCODE -ne 0) { throw 'Docs fixture ConfigMap rendering failed.' }
        $configMap | & kubectl apply --server-side -f -
        if ($LASTEXITCODE -ne 0) { throw 'Docs fixture ConfigMap setup failed.' }
    } else {
        $python = Resolve-AzureSamplePython
        $dependency = if ($Source -eq 'azure') { 'azure.storage.blob' } else { 'boto3' }
        & $python -c "import $dependency"
        if ($LASTEXITCODE -ne 0) { throw "Install the $dependency upload SDK in the selected Python environment before sample installation." }
    }
    $azuriteKey = if ($Source -eq 'azure') { Initialize-AzuriteAccount -Namespace $serviceNamespace } else { $null }
    Invoke-AzureSampleApply -Path (Join-Path $SampleDirectory $fixture.file)
    & kubectl rollout status "deployment/$($fixture.deployment)" -n $serviceNamespace --timeout=300s
    if ($LASTEXITCODE -ne 0) { throw "$Source fixture did not become ready; no KnowledgeBase was applied." }
    if ($Source -ne 'docs') {
        Invoke-AzureSampleFixtureUpload -Provider $Source -Namespace $serviceNamespace -Python $python -AccountKey $azuriteKey
        $payload = if ($Source -eq 'azure') {
            @{ connectionString = New-AzuriteConnectionString -AccountKey $azuriteKey -BlobEndpoint "http://azurite.$serviceNamespace.svc.cluster.local:10000" }
        } else { @{ accessKeyId = 'minioadmin'; secretAccessKey = 'minioadmin' } }
        $name = if ($Source -eq 'azure') { 'azurite-blob-creds' } else { 's3-blob-creds' }
        $key = if ($Source -eq 'azure') { 'connectionString' } else { 'accessKeyId' }
        ConvertTo-AzureSampleYaml -Data @{
            apiVersion = 'kognition.dev/v1alpha1'; kind = 'SecretRef'
            metadata = @{ name = $name; namespace = $targetNamespace
                annotations = @{ 'kognition.dev/internal-secret-data' = ($payload | ConvertTo-Json -Compress) } }
            spec = @{ backendType = 'internal'; backendRef = $name; key = $key }
        } | & kubectl apply -f -
        if ($LASTEXITCODE -ne 0) { throw "$Source fixture source credential setup failed; no KnowledgeBase was applied." }
    }
    $order = if ($Source -eq 'docs') {
        @('model-class.yaml', 'model.yaml', 'kb-class.yaml', 'kb.yaml', 'agent.yaml')
    } else { @('manifests.yaml') }
    foreach ($file in $order) {
        Invoke-AzureSampleApply -Path (Join-Path $SampleDirectory $file)
        if ($file -eq 'model-class.yaml') {
            & kubectl wait --for=condition=Ready=True modelclass/foundry --timeout=120s
            if ($LASTEXITCODE -ne 0) { throw 'Foundry ModelClass did not become ready.' }
        }
    }
}

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
    $sampleProfile = Get-AzureSampleProfile
    if (-not $Settings['MS_FOUNDRY_PROJECT_ENDPOINT'] -and -not $sampleProfile) { return $content }
    Import-Module powershell-yaml -ErrorAction Stop
    $chatModelName = if ($Settings['MS_FOUNDRY_PROJECT_ENDPOINT']) { Get-AzureSampleChatModelName -Settings $Settings } else { 'gpt-6-luna' }
    $documents = @(ConvertFrom-Yaml -Yaml $content -AllDocuments)
    if ($sampleProfile) {
        $documents = @(ConvertTo-GovernedSampleDocuments -Documents $documents -Profile $sampleProfile)
        $documents = @($documents | Where-Object { $_['kind'] -eq 'PgvectorBackend' }) +
            @($documents | Where-Object { $_['kind'] -ne 'PgvectorBackend' })
    }
    $profiles = @{}
    foreach ($document in $documents) {
        if (-not $document) { continue }
        Set-AzureSampleChatModelReferences -Value $document -ModelName $chatModelName
        if ($document['kind'] -eq 'ModelGrant') {
            $document['metadata']['name'] = $document['metadata']['name'].Replace('gpt-6-luna', $chatModelName)
        }
        if (-not $Settings['MS_FOUNDRY_PROJECT_ENDPOINT'] -or -not $document -or $document['kind'] -ne 'Model' -or
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

function Wait-AzureSampleToolFixture {
    param([Parameter(Mandatory)][string]$Path)

    if ((Split-Path -Leaf $Path) -ne 'tool-sandbox.yaml') { return }
    $profile = Get-AzureSampleProfile
    if (-not $profile) { return }
    $namespace = Get-AzureSampleNamespace -Namespace 'samples-tool-sandbox' -Profile $profile
    $timeout = Get-AzureSampleReadinessTimeoutSeconds -Profile $profile
    & kubectl rollout status deployment/tool-sandbox -n $namespace "--timeout=${timeout}s"
    if ($LASTEXITCODE -ne 0) { throw 'Tool fixture did not become ready; refusing to proceed with governed samples.' }
}

function Invoke-AzureSampleApply {
    param([Parameter(Mandatory)][string]$Path)

    $manifest = Read-AzureSampleManifest -Path $Path
    if ([string]::IsNullOrWhiteSpace($manifest)) {
        $global:LASTEXITCODE = 0
        return
    }
    $manifest | & kubectl apply -f -
    if ($LASTEXITCODE -ne 0) { throw "Sample manifest apply failed: $Path" }
    Wait-AzureSampleToolFixture -Path $Path
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
        $manifest = Read-AzureSampleManifest -Path $file.FullName
        if ([string]::IsNullOrWhiteSpace($manifest)) {
            $global:LASTEXITCODE = 0
            continue
        }
        $manifest | & kubectl delete -f - --ignore-not-found --wait=false
        if ($LASTEXITCODE -ne 0) { throw "Sample manifest deletion failed: $($file.FullName)" }
    }
}

function Get-AzureSampleQuotaHelmArguments {
    $profile = Get-AzureSampleProfile
    if (-not $profile) { return }
    foreach ($key in @('agents', 'tools', 'workflows')) { @('--set', "quota.hard.$key=$($profile['budgets'][$key])") }
    foreach ($key in @('maxInputTokens', 'maxOutputTokens')) { @('--set', "quota.tokenBudget.$key=$($profile['budgets'][$key])") }
    @('--set-string', 'quota.resetInterval=24h')
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
