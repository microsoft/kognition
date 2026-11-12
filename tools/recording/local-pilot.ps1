# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

[CmdletBinding()]
param(
    [Parameter(Mandatory)][ValidatePattern('^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$')][string]$RunId,
    [Parameter(Mandatory)][string]$OutputDirectory,
    [Parameter(Mandatory)][string]$FFmpegPath,
    [Parameter(Mandatory)][string]$FFprobePath,
    [string]$CredentialFile = '.env',
    [string]$BinaryDirectory
)

$ErrorActionPreference = 'Stop'
$repository = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$output = [IO.Path]::GetFullPath($OutputDirectory)
$runtimeDirectory = "$output.runtime"
if ((Test-Path -LiteralPath $output) -or (Test-Path -LiteralPath $runtimeDirectory)) {
    throw 'Use a new output directory and run ID; existing evidence is never overwritten.'
}
$credentialPath = (Resolve-Path -LiteralPath $CredentialFile).Path
$credentials = @{}
foreach ($line in Get-Content -LiteralPath $credentialPath) {
    if ($line -match '^\s*([A-Z][A-Z0-9_]*)\s*=\s*(.*)$') {
        $value = $Matches[2]
        if (($value.StartsWith('"') -and $value.EndsWith('"')) -or
            ($value.StartsWith("'") -and $value.EndsWith("'"))) {
            $value = $value.Substring(1, $value.Length - 2)
        }
        $credentials[$Matches[1]] = $value
    }
}
foreach ($key in @('AZURE_TENANT_ID', 'AZURE_CLIENT_ID', 'AZURE_CLIENT_SECRET',
        'MS_FOUNDRY_PROJECT_ENDPOINT', 'MS_FOUNDRY_MODEL',
        'MS_FOUNDRY_INVOCATION_CONTRACT', 'MS_FOUNDRY_API_VERSION')) {
    if ([string]::IsNullOrWhiteSpace($credentials[$key])) { throw "Missing credential setting $key" }
}
foreach ($tool in @($FFmpegPath, $FFprobePath)) {
    if (-not (Test-Path -LiteralPath $tool -PathType Leaf)) { throw 'Required media executable is missing.' }
}
$binaries = if ($BinaryDirectory) { (Resolve-Path -LiteralPath $BinaryDirectory).Path } else { $runtimeDirectory }
if ($BinaryDirectory) {
    foreach ($binary in @('kognitionctl', 'kognition-apiserver', 'kognition-controller-manager', 'kognition-migrate')) {
        if (-not (Test-Path -LiteralPath (Join-Path $binaries "$binary.exe") -PathType Leaf)) {
            throw "Frozen binary is missing: $binary"
        }
    }
}

function Invoke-Checked([string]$Executable, [string[]]$Arguments) {
    & $Executable @Arguments
    if ($LASTEXITCODE -ne 0) { throw "$Executable failed with exit code $LASTEXITCODE" }
}

function Free-Port {
    foreach ($port in 18090..18190) {
        $listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, $port)
        $listener.ExclusiveAddressUse = $true
        try {
            $listener.Start()
            return $port
        } catch [Net.Sockets.SocketException] {
            Write-Verbose "Loopback port $port is unavailable."
        } finally {
            $listener.Stop()
        }
    }
    throw 'No available loopback API port in the pilot range 18090 through 18190.'
}

New-Item -ItemType Directory -Path $runtimeDirectory | Out-Null
$children = [Collections.Generic.List[Diagnostics.Process]]::new()
$containerName = "kognition-recording-$RunId"
$containerId = $null
try {
    if (-not $BinaryDirectory) {
        Push-Location $repository
        try {
            foreach ($binary in @('kognitionctl', 'kognition-apiserver', 'kognition-controller-manager', 'kognition-migrate')) {
                Invoke-Checked 'go' @('build', '-o', (Join-Path $binaries "$binary.exe"), ".\cmd\$binary")
            }
        } finally { Pop-Location }
    }

    $existing = & docker ps -a --filter "name=^/$containerName$" --format '{{.ID}}'
    if ($LASTEXITCODE -ne 0 -or $existing) { throw 'Container preflight failed or the pilot container already exists.' }
    $env:POSTGRES_PASSWORD = [Convert]::ToHexString([Security.Cryptography.RandomNumberGenerator]::GetBytes(24))
    $containerId = & docker run --detach --name $containerName --label "kognition.dev/recording-run=$RunId" `
        --env POSTGRES_PASSWORD --env POSTGRES_USER=kognition --env POSTGRES_DB=kognition `
        --publish '127.0.0.1::5432' --health-cmd 'pg_isready -U kognition -d kognition' `
        --health-interval 2s --health-timeout 3s --health-retries 30 'pgvector/pgvector:pg16'
    if ($LASTEXITCODE -ne 0) { throw 'Unable to start the isolated database.' }
    $deadline = [DateTime]::UtcNow.AddSeconds(90)
    do {
        $health = & docker inspect --format '{{.State.Health.Status}}' $containerId
        if ($LASTEXITCODE -ne 0) { throw 'Unable to inspect the isolated database.' }
        if ($health -eq 'unhealthy') { throw 'The isolated database failed its health check.' }
        if ($health -ne 'healthy') { Start-Sleep -Seconds 1 }
    } while ($health -ne 'healthy' -and [DateTime]::UtcNow -lt $deadline)
    if ($health -ne 'healthy') { throw 'The isolated database did not become healthy.' }
    $port = & docker inspect --format '{{(index (index .NetworkSettings.Ports "5432/tcp") 0).HostPort}}' $containerId
    if ($LASTEXITCODE -ne 0) { throw 'Unable to resolve the isolated database port.' }
    $env:DATABASE_URL = "postgresql://kognition:$($env:POSTGRES_PASSWORD)@127.0.0.1:$port/kognition?sslmode=disable"
    $env:KOGNITION_DSN = $env:DATABASE_URL
    $env:KOGNITION_MIGRATION_BASELINE = ''
    Invoke-Checked (Join-Path $binaries 'kognition-migrate.exe') @()
    $env:KOGNITION_AUTH_ENABLED = 'true'
    $env:KOGNITION_BOOTSTRAP_TOKEN = [Convert]::ToBase64String([Security.Cryptography.RandomNumberGenerator]::GetBytes(32))
    $env:KOGNITION_AGENT_SERVICE_TOKEN = [Convert]::ToBase64String([Security.Cryptography.RandomNumberGenerator]::GetBytes(32))
    $env:KOGNITION_RUNTIME_AUTH_TOKEN = [Convert]::ToBase64String([Security.Cryptography.RandomNumberGenerator]::GetBytes(32))
    $env:KOGNITION_RECORDING_TOKEN = $env:KOGNITION_BOOTSTRAP_TOKEN
    $env:KOGNITION_RUNTIME_APPROVAL_BINDING_KEYS_JSON = ConvertTo-Json -Compress -InputObject @(@{
        version = 1; epoch = 1; algorithm = 'HMAC-SHA-256'; domain = 'kognition.dev/runtime-approval/action/v1'
        status = 'Active'; material = [Convert]::ToBase64String([Security.Cryptography.RandomNumberGenerator]::GetBytes(32))
    })
    $env:KOGNITION_METRICS_ADDR = '127.0.0.1:0'
    $env:KOGNITION_OTEL_TRACES_ENABLED = 'false'
    foreach ($key in @('KOGNITION_OIDC_ISSUER', 'KOGNITION_OIDC_REQUIRED_GROUPS',
            'KOGNITION_WORKLOAD_STATUS_REGISTRY_FILE', 'KOGNITION_KB_INGESTER_URL',
            'KOGNITION_RUNTIME_ENDPOINTS', 'KOGNITION_ARTIFACT_SIGNING_KEYS_FILE',
            'KOGNITION_ARTIFACT_SIGNING_KEYS_JSON')) {
        [Environment]::SetEnvironmentVariable($key, $null, 'Process')
    }
    $apiPort = Free-Port
    $env:LISTEN_ADDR = "127.0.0.1:$apiPort"
    $api = "http://127.0.0.1:$apiPort"
    foreach ($binary in @('kognition-apiserver', 'kognition-controller-manager')) {
        $child = Start-Process -FilePath (Join-Path $binaries "$binary.exe") -PassThru `
            -RedirectStandardOutput (Join-Path $runtimeDirectory "$binary.stdout.log") `
            -RedirectStandardError (Join-Path $runtimeDirectory "$binary.stderr.log")
        $children.Add($child)
    }
    $headers = @{ Authorization = "Bearer $($env:KOGNITION_BOOTSTRAP_TOKEN)" }
    $deadline = [DateTime]::UtcNow.AddSeconds(60)
    $ready = $false
    do {
        foreach ($child in $children) { if ($child.HasExited) { throw 'A pilot control-plane process exited during startup.' } }
        try {
            $healthResponse = Invoke-WebRequest "$api/readyz" -TimeoutSec 5 -SkipHttpErrorCheck
            if ($healthResponse.StatusCode -notin @(200, 503)) {
                throw "Unexpected API readiness status: $($healthResponse.StatusCode)"
            }
            $ready = $healthResponse.StatusCode -eq 200
        } catch [Net.Http.HttpRequestException] {
            if ([DateTime]::UtcNow -ge $deadline) { throw }
        } catch [Threading.Tasks.TaskCanceledException] {
            if ([DateTime]::UtcNow -ge $deadline) { throw }
        }
        if (-not $ready) { Start-Sleep -Milliseconds 500 }
    } while (-not $ready -and [DateTime]::UtcNow -lt $deadline)
    if (-not $ready) { throw 'The pilot API did not become ready.' }

    $namespace = 'default'
    $base = "$api/apis/kognition.dev/v1alpha1/namespaces/$namespace"
    $modelName = "pilot-$RunId-model"
    $secretName = "pilot-$RunId-credentials"
    $className = "pilot-$RunId-foundry"
    $profile = @{
        apiVersion = 'kognition.dev/v1alpha1'; kind = 'ModelInvocationProfile'
        metadata = @{ name = "pilot-$RunId-chat"; namespace = $namespace }
        spec = @{
            operation = 'chat'
            defaults = @{ maxCompletionTokens = 256; reasoning = 'disabled' }
            limits = @{ maxCompletionTokens = 256 }
            requirements = @{ reasoning = 'disabled'; toolCalling = 'forbidden' }
            parameterHandling = @{ unknown = 'Reject'; unsupported = 'Reject' }
        }
    }
    $profilePath = Join-Path $runtimeDirectory 'invocation-profile.json'
    $profile | ConvertTo-Json -Depth 12 | Set-Content -LiteralPath $profilePath -Encoding utf8NoBOM
    $profileDigest = & (Join-Path $binaries 'kognitionctl.exe') invocation-profile-digest -f $profilePath
    if ($LASTEXITCODE -ne 0 -or $profileDigest -cnotmatch '^sha256:[a-f0-9]{64}$') {
        throw 'Unable to compute the reviewed invocation profile digest.'
    }
    $azureIdentity = @{
        type = 'azure-identity'; credentialType = 'client-secret'
        tenantId = $credentials.AZURE_TENANT_ID; clientId = $credentials.AZURE_CLIENT_ID
        clientSecret = $credentials.AZURE_CLIENT_SECRET
    } | ConvertTo-Json -Compress
    $fixtures = @(
        @{ plural = 'modelclasses'; resource = @{
            apiVersion = 'kognition.dev/v1alpha1'; kind = 'ModelClass'; metadata = @{ name = $className }
            spec = @{ provisioner = 'foundry'; costTier = 'standard'; contextWindow = 128000; modalities = @('text') }
        }},
        @{ plural = 'secretrefs'; resource = @{
            apiVersion = 'kognition.dev/v1alpha1'; kind = 'SecretRef'
            metadata = @{ name = $secretName; namespace = $namespace; annotations = @{ 'kognition.dev/internal-secret-data' = $azureIdentity } }
            spec = @{ backendType = 'internal'; backendRef = "secret/$secretName"; key = 'token' }
        }},
        @{ plural = 'modelinvocationprofiles'; resource = $profile },
        @{ plural = 'models'; resource = @{
            apiVersion = 'kognition.dev/v1alpha1'; kind = 'Model'
            metadata = @{ name = $modelName; namespace = $namespace }
            spec = @{ className = $className; modelID = $credentials.MS_FOUNDRY_MODEL; provider = 'foundry'
                endpoint = $(if ($credentials.MS_FOUNDRY_INFERENCE_ENDPOINT) {
                    $credentials.MS_FOUNDRY_INFERENCE_ENDPOINT
                } else { $credentials.MS_FOUNDRY_PROJECT_ENDPOINT }); capabilities = @('chat')
                credentialRef = @{ name = $secretName }
                invocationContract = $credentials.MS_FOUNDRY_INVOCATION_CONTRACT
                invocationProfileRef = @{ name = $profile.metadata.name; specDigest = $profileDigest.Trim() }
                parameters = @{ apiVersion = $credentials.MS_FOUNDRY_API_VERSION } }
        }},
        @{ plural = 'cognitivequotas'; resource = @{
            apiVersion = 'kognition.dev/v1alpha1'; kind = 'CognitiveQuota'
            metadata = @{ name = "pilot-$RunId-budget"; namespace = $namespace }
            spec = @{ hard = @{ agents = 2 }; tokenBudget = @{ maxTotalTokens = 4096 } }
        }}
    )
    foreach ($fixture in $fixtures) {
        Invoke-RestMethod -Uri "$base/$($fixture.plural)" -Method Post -Headers $headers `
            -ContentType 'application/json' -Body ($fixture.resource | ConvertTo-Json -Depth 12 -Compress) | Out-Null
    }
    $deadline = [DateTime]::UtcNow.AddSeconds(90)
    do {
        $model = Invoke-RestMethod "$base/models/$modelName" -Headers $headers
        if ($model.status.phase -eq 'Failed') { throw 'The configured Foundry model failed live readiness; inspect the private runtime logs.' }
        if ($model.status.phase -ne 'Ready') { Start-Sleep -Milliseconds 500 }
    } while ($model.status.phase -ne 'Ready' -and [DateTime]::UtcNow -lt $deadline)
    if ($model.status.phase -ne 'Ready') { throw 'The live model did not become ready.' }
    Write-Host 'Isolated authenticated API and real model are ready. Starting browser capture.'
    Invoke-Checked 'node' @(
        (Join-Path $PSScriptRoot 'live-pilot.js'), '--cli', (Join-Path $binaries 'kognitionctl.exe'),
        '--mode', 'standalone', '--server', $api, '--namespace', $namespace, '--model', $modelName,
        '--run-id', $RunId, '--output-dir', $output, '--ffmpeg', $FFmpegPath, '--ffprobe', $FFprobePath,
        '--authorize-live-writes'
    )
} finally {
    foreach ($child in $children) {
        if (-not $child.HasExited) { Stop-Process -Id $child.Id -ErrorAction Stop }
    }
    if ($containerId) {
        & docker stop $containerId | Out-Null
        if ($LASTEXITCODE -ne 0) { Write-Error 'Unable to stop the owned pilot database.' }
    }
    [Environment]::SetEnvironmentVariable('KOGNITION_RECORDING_TOKEN', $null, 'Process')
    Write-Host "Pilot database retained, stopped: $containerName"
    Write-Host "Runtime logs: $runtimeDirectory"
}
