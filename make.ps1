# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

#requires -Version 5.1
<#
.SYNOPSIS
    Windows-native shim that mirrors the project Makefile.

.DESCRIPTION
    Implements the same targets as the POSIX Makefile so contributors on
    Windows do not need GNU make. Variables are passed Make-style:

        .\make.ps1 <target> [VAR=value ...]

    Supported targets:
        all build clean tidy test integration vet fmt lint quality quality-deep
        proto proto-lint
        docker docker-k8s docker-standalone refresh-k8s-controller
        up down migrate
        deploy-standalone deploy-kubernetes
        undeploy-standalone undeploy-kubernetes
        install uninstall quickstart fresh-kubernetes-demo
        observability observability-install observability-uninstall jaeger jaeger-install jaeger-ui jaeger-uninstall grafana-ui
        foundry-creds
        samples-list samples-install samples-uninstall
        status logs port-forward e2e e2e-kubernetes-memory e2e-kubernetes-audit e2e-kubernetes-tool e2e-kubernetes-tool-resilience e2e-kubernetes-tool-monitoring e2e-kubernetes-runtime-tool e2e-kubernetes-tool-network-policy e2e-kubernetes-tool-exec-probe e2e-kubernetes-model e2e-kubernetes-capability-rotation e2e-kubernetes-agent-communication
        services install-hooks help

    Examples:
        .\make.ps1 build
        .\make.ps1 quickstart KUBECONFIG=target.kubeconfig CHART_PACKAGE=kognition.tgz INSTALL_VALUES=values.yaml WORKLOAD_NAMESPACES=team-a INSTALL_EXCLUSIVE=true
        .\make.ps1 samples-install SAMPLE=simple-agent
        .\make.ps1 status
        .\make.ps1 services ACTION=start SERVICE=postgres
        .\make.ps1 migrate
#>
param(
    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]]$Args
)

$ErrorActionPreference = 'Stop'
$RepoRoot = $PSScriptRoot
Set-Location $RepoRoot
. (Join-Path $RepoRoot 'samples\azure-settings.ps1')

# ----------------------------------------------------------------------------
# Argument parsing: first token is the target, rest are KEY=VALUE pairs.
# ----------------------------------------------------------------------------
function Parse-Args {
    param([string[]]$Tokens)

    $result = @{ Target = $null; Vars = @{} }
    if (-not $Tokens -or $Tokens.Count -eq 0) {
        return $result
    }

    $result.Target = $Tokens[0]
    if ($Tokens.Count -lt 2) { return $result }

    foreach ($t in $Tokens[1..($Tokens.Count - 1)]) {
        if ($t -match '^([A-Za-z_][A-Za-z0-9_]*)=(.*)$') {
            $result.Vars[$matches[1]] = $matches[2]
        } else {
            throw "Unrecognized argument '$t'. Use KEY=VALUE form."
        }
    }
    return $result
}

# ----------------------------------------------------------------------------
# Helpers
# ----------------------------------------------------------------------------
function Invoke-Native {
    param([string]$Tool, [string[]]$Arguments)
    & $Tool @Arguments
    if ($LASTEXITCODE -ne 0) {
        throw "$Tool exited with code $LASTEXITCODE"
    }
}

function Resolve-Tool {
    param([string]$Name)
    $cmd = Get-Command $Name -ErrorAction SilentlyContinue
    if (-not $cmd) {
        throw "Required tool '$Name' not found on PATH."
    }
    return $cmd.Source
}

$BinDir = Join-Path $RepoRoot 'bin'
$Binaries = @(
    @{ Name = 'kognition-apiserver';          Pkg = './cmd/kognition-apiserver' }
    @{ Name = 'kognition-controller-manager'; Pkg = './cmd/kognition-controller-manager' }
    @{ Name = 'kognitionctl';                 Pkg = './cmd/kognitionctl' }
    @{ Name = 'kognition-intent-gateway';     Pkg = './cmd/kognition-intent-gateway' }
    @{ Name = 'kognition-memory-policy-gateway'; Pkg = './cmd/kognition-memory-policy-gateway' }
    @{ Name = 'kognition-tool-gateway';          Pkg = './cmd/kognition-tool-gateway' }
    @{ Name = 'kognition-model-gateway';         Pkg = './cmd/kognition-model-gateway' }
    @{ Name = 'kognition-agent-communication-gateway'; Pkg = './cmd/kognition-agent-communication-gateway' }
    @{ Name = 'kognition-knowledge-gateway'; Pkg = './cmd/kognition-knowledge-gateway' }
    @{ Name = 'kognition-knowledge-azuresearch'; Pkg = './cmd/kognition-knowledge-azuresearch' }
)
$ChartFile = Join-Path $RepoRoot 'deploy/helm/kognition/Chart.yaml'
$AppVersion = $null
if (Test-Path $ChartFile) {
    $match = Select-String -Path $ChartFile -Pattern '^\s*appVersion:\s*"?([^"\s]+)"?\s*$' | Select-Object -First 1
    if ($match) { $AppVersion = $match.Matches[0].Groups[1].Value }
}

# Repo = image repository (no tag). ChartTagged = also tag with $AppVersion so chart picks it up.
$DockerImages = @(
    @{ Repo = 'kognition-apiserver';          Target = 'apiserver';      ChartTagged = $true  }
    @{ Repo = 'kognition-controller-manager'; Target = 'controller';     ChartTagged = $true  }
    @{ Repo = 'kognitionctl';                 Target = 'ctl';            ChartTagged = $false }
    @{ Repo = 'kognition-k8s-controller';     Target = 'k8s-controller'; ChartTagged = $true  }
    @{ Repo = 'kognition-memory-service';     Target = 'memory-service'; ChartTagged = $true  }
    @{ Repo = 'kognition-memory-policy-gateway'; Target = 'memory-policy-gateway'; ChartTagged = $true }
    @{ Repo = 'kognition-tool-gateway'; Target = 'tool-gateway'; ChartTagged = $true }
    @{ Repo = 'kognition-model-gateway'; Target = 'model-gateway'; ChartTagged = $true }
    @{ Repo = 'kognition-agent-communication-gateway'; Target = 'agent-communication-gateway'; ChartTagged = $true }
    @{ Repo = 'kognition-intent-gateway';     Target = 'intent-gateway'; ChartTagged = $true  }
    @{ Repo = 'kognition-knowledge-gateway'; Target = 'knowledge-gateway'; ChartTagged = $true }
    @{ Repo = 'kognition-knowledge-azuresearch'; Target = 'knowledge-azuresearch'; ChartTagged = $true }
)
# Python images use repository-root build contexts to include legal files.
# Chart-tagged so `make docker` refreshes what the
# k8s-controller / dispatcher launch.
$PythonImages = @(
    @{ Repo = 'kognition-agent-runtime'; Context = '.'; Dockerfile = 'runtime/agent/Dockerfile'; ChartTagged = $true }
    @{ Repo = 'kognition-kb-ingester';   Context = '.'; Dockerfile = 'runtime/ingester/Dockerfile'; ChartTagged = $true }
)
$Migrations = @(
    'internal/store/postgres/migrations/001_initial_schema.sql'
    'internal/store/postgres/migrations/002_revisions.sql'
    'internal/store/postgres/migrations/003_instance_events.sql'
    'internal/store/postgres/migrations/004_memory_composite_pk.sql'
    'internal/store/postgres/migrations/006_token_reservations.sql'
    'internal/store/postgres/migrations/007_token_reservation_quota_periods.sql'
    'internal/store/postgres/migrations/008_tool_invocation_receipts.sql'
    'internal/store/postgres/migrations/009_cognitive_audit_log.sql'
    'internal/store/postgres/migrations/010_cost_records.sql'
    'internal/store/postgres/migrations/011_usage_event_accounting.sql'
    'internal/store/postgres/migrations/012_usage_event_immutability.sql'
)

# ----------------------------------------------------------------------------
# Target implementations
# ----------------------------------------------------------------------------
function Target-Build {
    if (-not (Test-Path $BinDir)) {
        New-Item -ItemType Directory -Path $BinDir | Out-Null
    }
    foreach ($b in $Binaries) {
        $out = Join-Path $BinDir ("{0}.exe" -f $b.Name)
        Write-Host "go build -> $out"
        Invoke-Native 'go' @('build', '-o', $out, $b.Pkg)
    }
}

function Target-Clean {
    foreach ($p in @($BinDir)) {
        if (Test-Path $p) {
            Write-Host "rm -rf $p"
            Remove-Item -Recurse -Force $p
        }
    }
}

function Target-Tidy { Invoke-Native 'go' @('mod', 'tidy') }

function Invoke-BoundedTest {
    param([string[]]$Arguments)
    $python = Join-Path $RepoRoot '.venv/Scripts/python.exe'
    if (-not (Test-Path $python)) { $python = Resolve-Tool 'python' }
    Invoke-Native $python (@((Join-Path $RepoRoot 'tools/run_bounded.py'), '--timeout', '1800', '--', 'go') + $Arguments)
}

function Target-Test { Invoke-BoundedTest @('test', '-p=1', '-count=1', '-timeout=3m', '-race', '-cover', './...') }

function Target-Integration { Invoke-BoundedTest @('test', '-tags=integration', '-count=1', '-timeout=15m', './tests/integration/...') }

function Target-Vet { Invoke-Native 'go' @('vet', './...') }

function Target-Fmt {
    $offenders = & gofmt -l .
    if ($LASTEXITCODE -ne 0) { throw "gofmt failed with code $LASTEXITCODE" }
    if ($offenders) {
        Write-Host 'Files need formatting:'
        $offenders | ForEach-Object { Write-Host "  $_" }
        throw 'gofmt check failed'
    }
}

function Target-Lint {
    Target-Vet
    Target-Fmt
    Write-Host 'Lint passed'
}

function Target-Quality {
    Resolve-Tool 'golangci-lint' | Out-Null
    Invoke-Native 'golangci-lint' @('run', '--timeout=10m', './...')
}

function Target-QualityDeep {
    Resolve-Tool 'golangci-lint' | Out-Null
    Invoke-Native 'golangci-lint' @('run', '--timeout=10m', '--enable-only=gocognit,gocyclo,dupl', './...')
}

function Target-Proto {
    Resolve-Tool 'buf' | Out-Null
    Invoke-Native 'buf' @('generate')
}

function Target-ProtoLint {
    Resolve-Tool 'buf' | Out-Null
    Invoke-Native 'buf' @('lint')
}

function Target-Docker {
    foreach ($img in $DockerImages) {
        $tags = @("$($img.Repo):latest")
        if ($img.ChartTagged -and $AppVersion) {
            $tags += "$($img.Repo):$AppVersion"
        }
        $tagArgs = @()
        foreach ($t in $tags) { $tagArgs += @('-t', $t) }
        Write-Host ("docker build {0} --target {1} ." -f (($tags | ForEach-Object { "-t $_" }) -join ' '), $img.Target)
        Invoke-Native 'docker' (@('build') + $tagArgs + @('--target', $img.Target, '.'))
    }
    Build-PythonImages $PythonImages
}

function Build-PythonImages($images) {
    foreach ($img in $images) {
        Build-PythonImage $img
    }
}

function Build-PythonImage($img, [string]$ExtraTag = '') {
    $tags = @("$($img.Repo):latest")
    if ($img.ChartTagged -and $AppVersion) {
        $tags += "$($img.Repo):$AppVersion"
    }
    if ($ExtraTag) {
        $tags += "$($img.Repo):$ExtraTag"
    }
    $tagArgs = @()
    foreach ($t in $tags) { $tagArgs += @('-t', $t) }
    $fileArgs = @()
    if ($img.Dockerfile) { $fileArgs = @('-f', $img.Dockerfile) }
    Write-Host ("docker build {0} {1} {2}" -f (($tags | ForEach-Object { "-t $_" }) -join ' '), ($fileArgs -join ' '), $img.Context)
    Invoke-Native 'docker' (@('build') + $tagArgs + $fileArgs + @($img.Context))
}

function Test-DockerImageExists([string]$Image) {
    & docker image inspect $Image *> $null
    return $LASTEXITCODE -eq 0
}

function Add-FreshFallbackImageTag([string]$Repo, [string]$Tag) {
    $sourceTag = if ($AppVersion) { "$Repo`:$AppVersion" } else { "$Repo`:latest" }
    if (-not (Test-DockerImageExists $sourceTag)) {
        $sourceTag = "$Repo`:latest"
    }
    if (-not (Test-DockerImageExists $sourceTag)) { throw }

    $freshTag = "$Repo`:$Tag"
    Invoke-Native 'docker' @('tag', $sourceTag, $freshTag)
    Write-Warning "Image rebuild failed, but local image '$sourceTag' exists. Retagged it as '$freshTag' so Kubernetes does not reuse a stale cached tag."
}

function Build-AgentRuntimeOverlay([string]$Tag) {
    $sourceTag = if ($AppVersion) { "kognition-agent-runtime:$AppVersion" } else { 'kognition-agent-runtime:latest' }
    if (-not (Test-DockerImageExists $sourceTag)) {
        $sourceTag = 'kognition-agent-runtime:latest'
    }
    if (-not (Test-DockerImageExists $sourceTag)) { throw "No cached runtime image is available for a source overlay." }

    $freshTag = "kognition-agent-runtime:$Tag"
    $overlayDockerfile = Join-Path $RepoRoot 'runtime/agent/Dockerfile.overlay'
    Write-Host "docker build --build-arg RUNTIME_BASE_IMAGE=$sourceTag -f $overlayDockerfile -t $freshTag runtime/agent"
    Invoke-Native 'docker' @('build', '--build-arg', "RUNTIME_BASE_IMAGE=$sourceTag", '-f', $overlayDockerfile, '-t', $freshTag, (Join-Path $RepoRoot 'runtime/agent'))
    Write-Warning "Runtime dependency install failed; rebuilt '$freshTag' from current source over cached dependencies in '$sourceTag'."
}

function Invoke-KindLoadImage([string]$Tag, [string]$Cluster, [int]$TimeoutSeconds = 180) {
    Write-Host "kind load docker-image $Tag --name $Cluster"
    $stdout = New-TemporaryFile
    $stderr = New-TemporaryFile
    try {
        $process = Start-Process -FilePath 'kind' -ArgumentList @('load', 'docker-image', $Tag, '--name', $Cluster) -NoNewWindow -PassThru -RedirectStandardOutput $stdout.FullName -RedirectStandardError $stderr.FullName
        if (-not $process.WaitForExit($TimeoutSeconds * 1000)) {
            try { $process.Kill() } catch { }
            Write-Warning "  timed out loading $Tag into kind after ${TimeoutSeconds}s; continuing. Pods may fail if the node cannot pull this image."
            return
        }

        foreach ($file in @($stdout.FullName, $stderr.FullName)) {
            if (Test-Path $file) {
                Get-Content $file | Where-Object { $_ } | ForEach-Object { Write-Host "  $_" }
            }
        }
        if ($process.ExitCode -ne 0) {
            Write-Warning "  failed to load $Tag into kind (node will fall back to pulling it); continuing."
        }
    } finally {
        Remove-Item -Force -ErrorAction SilentlyContinue $stdout.FullName, $stderr.FullName
    }
}

function Target-DockerK8sForFreshDemo {
    param([string]$Tag = '')

    foreach ($repo in @('kognition-k8s-controller', 'kognition-memory-service', 'kognition-memory-policy-gateway', 'kognition-tool-gateway', 'kognition-model-gateway', 'kognition-agent-communication-gateway', 'kognition-intent-gateway', 'kognition-knowledge-gateway', 'kognition-knowledge-azuresearch')) {
        $img = $DockerImages | Where-Object { $_.Repo -eq $repo } | Select-Object -First 1
        if (-not $img) { throw "Docker image definition not found for $repo" }
        $tags = @("$($img.Repo):latest")
        if ($img.ChartTagged -and $AppVersion) { $tags += "$($img.Repo):$AppVersion" }
        if ($Tag) { $tags += "$($img.Repo):$Tag" }
        $tagArgs = @()
        foreach ($t in $tags) { $tagArgs += @('-t', $t) }
        Write-Host ("docker build {0} --target {1} ." -f (($tags | ForEach-Object { "-t $_" }) -join ' '), $img.Target)
        Invoke-Native 'docker' (@('build') + $tagArgs + @('--target', $img.Target, '.'))
    }

    $runtime = $PythonImages | Where-Object { $_.Repo -eq 'kognition-agent-runtime' } | Select-Object -First 1
    try {
        Build-PythonImage $runtime $Tag
    } catch {
        if ($Tag) {
            try {
                Build-AgentRuntimeOverlay $Tag
            } catch {
                Add-FreshFallbackImageTag $runtime.Repo $Tag
            }
        } else {
            $runtimeTag = if ($AppVersion) { "kognition-agent-runtime:$AppVersion" } else { 'kognition-agent-runtime:latest' }
            if (-not (Test-DockerImageExists $runtimeTag)) { throw }
            Write-Warning "Runtime image rebuild failed, but local image '$runtimeTag' exists. Continuing with the local image."
        }
    }

    $ingester = $PythonImages | Where-Object { $_.Repo -eq 'kognition-kb-ingester' } | Select-Object -First 1
    try {
        Build-PythonImage $ingester $Tag
    } catch {
        if ($Tag) { Add-FreshFallbackImageTag $ingester.Repo $Tag } else { throw }
    }
}

function Target-Up   { Invoke-Native 'docker' @('compose', 'up', '-d') }
function Target-Down { Invoke-Native 'docker' @('compose', 'down') }

# Target-KindLoad loads locally-built images into the kind cluster so pods can
# start with pullPolicy=IfNotPresent. Best-effort: no-ops when `kind` is not
# installed or no cluster exists, and tolerates individual load failures so a
# missing optional image (e.g. the KB ingester) doesn't abort a deploy.
function Target-KindLoad {
    if (-not (Get-Command kind -ErrorAction SilentlyContinue)) {
        Write-Host '==> kind not found; skipping image load (assuming images are reachable via a registry).'
        return
    }
    $clusters = @(& kind get clusters 2>$null | Where-Object { $_ })
    if (-not $clusters) {
        Write-Host '==> No kind clusters found; skipping image load.'
        return
    }
    # Prefer the cluster backing the current kube-context (kind-<name>), else first.
    $cluster = $clusters[0]
    $ctx = (& kubectl config current-context 2>$null)
    if ($ctx -and $ctx -match '^kind-(.+)$' -and ($clusters -contains $matches[1])) {
        $cluster = $matches[1]
    }

    $tags = @()
    if ($FreshKubernetesDemoImageTag) {
        foreach ($repo in @('kognition-k8s-controller', 'kognition-memory-service', 'kognition-memory-policy-gateway', 'kognition-tool-gateway', 'kognition-model-gateway', 'kognition-agent-communication-gateway', 'kognition-intent-gateway', 'kognition-agent-runtime', 'kognition-kb-ingester')) {
            $tags += "$repo`:$FreshKubernetesDemoImageTag"
        }
    } else {
        # Chart-tagged images use $AppVersion with pullPolicy=IfNotPresent.
        foreach ($img in ($DockerImages + $PythonImages | Where-Object { $_.ChartTagged })) {
            if ($AppVersion) { $tags += "$($img.Repo):$AppVersion" }
            $tags += "$($img.Repo):latest"
        }
    }
    if (-not $FreshKubernetesDemoImageTag) {
        # Docker's containerd image store may retain pgvector's multi-platform
        # attestations without every referenced platform blob. Build a local,
        # provenance-free wrapper so kind imports only the host platform.
        $pgvectorTag = 'pgvector/pgvector:pg16'
        if (& docker image inspect $pgvectorTag 2>$null) {
            $kindTag = 'pgvector/pgvector:pg16-kind'
            $dockerBuildExitCode = 1
            $previousErrorActionPreference = $ErrorActionPreference
            try {
                $ErrorActionPreference = 'SilentlyContinue'
                "FROM $pgvectorTag" | & docker build --provenance=false --platform linux/amd64 --load -t $kindTag -f - . 2>$null | Out-Null
                $dockerBuildExitCode = $LASTEXITCODE
            } finally {
                $ErrorActionPreference = $previousErrorActionPreference
            }
            if ($dockerBuildExitCode -eq 0) {
                & docker tag $kindTag $pgvectorTag
            } else {
                Write-Warning "Could not prepare the single-platform pgvector image; kind may fall back to pulling it."
            }
        }
        $tags += $pgvectorTag
    }

    foreach ($tag in ($tags | Select-Object -Unique)) {
        if (-not (& docker image inspect $tag 2>$null)) { continue }
        # containerd can retain the previous target when the same mutable tag is
        # imported repeatedly. Remove the node-local tag first so the next load
        # deterministically points at the image just built by this workspace.
        $nodes = @(& kind get nodes --name $cluster 2>$null | Where-Object { $_ })
        $previousErrorActionPreference = $ErrorActionPreference
        try {
            $ErrorActionPreference = 'SilentlyContinue'
            foreach ($node in $nodes) {
                & docker exec $node ctr --namespace=k8s.io images remove $tag 2>$null | Out-Null
            }
        } finally {
            $ErrorActionPreference = $previousErrorActionPreference
        }
        Invoke-KindLoadImage $tag $cluster
    }
}

function Target-RefreshK8sController {
    Resolve-Tool 'docker' | Out-Null
    Resolve-Tool 'kind' | Out-Null
    Resolve-Tool 'kubectl' | Out-Null

    $deployment = "$Release-k8s-controller"
    $image = (& kubectl -n $Namespace get deployment $deployment -o jsonpath='{.spec.template.spec.containers[?(@.name=="k8s-controller")].image}' 2>$null)
    if ($LASTEXITCODE -ne 0 -or -not $image) {
        throw "Could not resolve the k8s-controller image from deployment '$deployment' in namespace '$Namespace'."
    }

    $clusters = @(& kind get clusters 2>$null | Where-Object { $_ })
    if ($clusters.Count -eq 0) { throw 'No kind clusters found.' }
    $cluster = $clusters[0]
    $context = (& kubectl config current-context 2>$null)
    if ($context -and $context -match '^kind-(.+)$' -and ($clusters -contains $matches[1])) {
        $cluster = $matches[1]
    }

    Write-Host "Building only $image..."
    Invoke-Native 'docker' @('build', '-t', $image, '--target', 'k8s-controller', '.')

    $nodes = @(& kind get nodes --name $cluster 2>$null | Where-Object { $_ })
    if ($nodes.Count -eq 0) { throw "No nodes found for kind cluster '$cluster'." }
    foreach ($node in $nodes) {
        & docker exec $node ctr --namespace=k8s.io images remove $image 2>$null | Out-Null
    }
    Invoke-KindLoadImage $image $cluster

    Write-Host "Restarting deployment/$deployment..."
    Invoke-Native 'kubectl' @('-n', $Namespace, 'rollout', 'restart', "deployment/$deployment")
    Invoke-Native 'kubectl' @('-n', $Namespace, 'rollout', 'status', "deployment/$deployment", '--timeout=120s')
}

function Target-Migrate {
    Invoke-Native 'go' @('run', './cmd/kognition-migrate')
}

function Target-Services {
    param([hashtable]$Vars)
    $script = Join-Path $RepoRoot 'services.ps1'
    if (-not (Test-Path $script)) { throw "services.ps1 not found at $script" }

    $action  = if ($Vars.ContainsKey('ACTION'))  { $Vars['ACTION'] }  else { 'help' }
    $service = if ($Vars.ContainsKey('SERVICE')) { $Vars['SERVICE'] } else { '' }

    if ($service) {
        & $script $action $service
    } else {
        & $script $action
    }
    if ($LASTEXITCODE -ne 0) { throw "services.ps1 exited with code $LASTEXITCODE" }
}

function Target-InstallHooks {
    Invoke-Native 'git' @('config', 'core.hooksPath', '.githooks')
    Write-Host 'Git hooks installed from .githooks'
}

function Use-HelmReuseValues {
    if (-not (Get-Command helm -ErrorAction SilentlyContinue)) { return $false }
    & helm status $Release --namespace $Namespace 2>$null | Out-Null
    return ($LASTEXITCODE -eq 0)
}

function Ensure-LocalRuntimeAuthSecret {
    $secretName = "$Release-runtime-auth"
    & kubectl get namespace $Namespace 2>$null | Out-Null
    if ($LASTEXITCODE -ne 0) {
        Invoke-Native 'kubectl' @('create', 'namespace', $Namespace) | Out-Null
    }
    & kubectl get secret $secretName --namespace $Namespace 2>$null | Out-Null
    if ($LASTEXITCODE -eq 0) { return $secretName }

    $tokenBytes = New-Object byte[] 48
    $materialBytes = New-Object byte[] 48
    $random = [Security.Cryptography.RandomNumberGenerator]::Create()
    try {
        $random.GetBytes($tokenBytes)
        $random.GetBytes($materialBytes)
    }
    finally {
        $random.Dispose()
    }
    $token = [Convert]::ToBase64String($tokenBytes)
    $material = [Convert]::ToBase64String($materialBytes)
    $bindingKeys = @(@{
        version = 1
        epoch = 1
        algorithm = 'HMAC-SHA-256'
        domain = 'kognition.dev/runtime-approval/action/v1'
        status = 'Active'
        material = $material
    }) | ConvertTo-Json -Compress -AsArray
    $manifest = @{
        apiVersion = 'v1'
        kind = 'Secret'
        metadata = @{ name = $secretName; namespace = $Namespace }
        type = 'Opaque'
        stringData = @{
            'runtime-control-token' = $token
            'runtime-approval-binding-keys.json' = $bindingKeys
        }
    } | ConvertTo-Json -Depth 8
    $manifestPath = Join-Path ([System.IO.Path]::GetTempPath()) "kognition-runtime-auth-$PID.json"
    try {
        Set-Content -LiteralPath $manifestPath -Value $manifest -Encoding utf8
        Invoke-Native 'kubectl' @('apply', '-f', $manifestPath) | Out-Null
    }
    finally {
        Remove-Item -LiteralPath $manifestPath -Force -ErrorAction SilentlyContinue
    }
    return $secretName
}

function Backup-LocalRuntimeAuthSecret {
    param([string]$SecretName)

    $secretJson = & kubectl get secret $SecretName --namespace $Namespace -o json
    if ($LASTEXITCODE -ne 0) { throw "kubectl exited with code $LASTEXITCODE" }
    $secret = $secretJson | ConvertFrom-Json
    $backup = @{
        apiVersion = 'v1'
        kind = 'Secret'
        metadata = @{ name = $SecretName; namespace = $Namespace }
        type = $secret.type
        data = $secret.data
    } | ConvertTo-Json -Depth 8
    $backupPath = Join-Path ([System.IO.Path]::GetTempPath()) "kognition-runtime-auth-backup-$PID.json"
    Set-Content -LiteralPath $backupPath -Value $backup -Encoding utf8
    return $backupPath
}

function Restore-LocalRuntimeAuthSecret {
    param([string]$BackupPath)

    try {
        Invoke-Native 'kubectl' @('apply', '-f', $BackupPath) | Out-Null
    }
    finally {
        Remove-Item -LiteralPath $BackupPath -Force -ErrorAction SilentlyContinue
    }
}

function Refresh-AgentRuntimes {
    if (-not (Get-Command kubectl -ErrorAction SilentlyContinue)) { return }
    $agentsJson = (& kubectl get agents -A -o json 2>$null) -join "`n"
    if ($LASTEXITCODE -ne 0 -or -not $agentsJson) { return }
    $agents = @((ConvertFrom-Json $agentsJson).items)
    if ($agents.Count -eq 0) { return }
    $stamp = (Get-Date).ToUniversalTime().ToString('yyyyMMddHHmmss')
    Write-Host 'Refreshing agent runtime deployments so inherited settings are reconciled...'
    $namespaces = New-Object 'System.Collections.Generic.HashSet[string]'
    foreach ($agent in $agents) {
        $ns = $agent.metadata.namespace
        $name = $agent.metadata.name
        if (-not $ns -or -not $name) { continue }
        [void]$namespaces.Add($ns)
        & kubectl annotate agent $name -n $ns "kognition.dev/reconcile-at=$stamp" --overwrite 2>$null | Out-Null
    }
    foreach ($ns in $namespaces) {
        & kubectl rollout status deployment -n $ns -l app.kubernetes.io/managed-by=kognition --timeout=120s 2>$null | Out-Null
    }
}

# ---------------------------------------------------------------------------
# Deployment – Standalone (Helm, default mode)
# ---------------------------------------------------------------------------

function Target-DeployStandalone {
    Resolve-Tool 'helm' | Out-Null
    Resolve-Tool 'kubectl' | Out-Null
    Target-KindLoad
    $runtimeAuthSecret = Ensure-LocalRuntimeAuthSecret
    $runtimeAuthBackup = Backup-LocalRuntimeAuthSecret $runtimeAuthSecret
    $helmArgs = @(
        'upgrade', '--install', $Release, $Chart,
        '--namespace', $Namespace, '--create-namespace',
        '--set', 'apiserver.image.pullPolicy=IfNotPresent',
        '--set', 'controllerManager.image.pullPolicy=IfNotPresent',
        '--set', 'memoryService.image.pullPolicy=IfNotPresent',
        '--set', 'runtimeAuth.create=false',
        '--set', "runtimeAuth.existingSecret=$runtimeAuthSecret",
        '--set', 'kbPgvector.enabled=true',
        '--set', 'kbPgvector.image.pullPolicy=IfNotPresent',
        '--set', 'kbIngester.enabled=true',
        '--set', 'kbIngester.image.repository=kognition-kb-ingester',
        '--set', 'kbIngester.image.pullPolicy=IfNotPresent'
    )
    Write-Host "helm $($helmArgs -join ' ')"
    try {
        Invoke-Native 'helm' $helmArgs
    }
    finally {
        Restore-LocalRuntimeAuthSecret $runtimeAuthBackup
    }
    Write-Host 'Restarting deployments to activate the kind-loaded images...'
    Invoke-Native 'kubectl' @('-n', $Namespace, 'rollout', 'restart', "deployment/$Release-apiserver", "deployment/$Release-controller-manager", "deployment/$Release-memory-service")
    Write-Host 'Waiting for standalone deployment rollouts...'
    Invoke-Native 'kubectl' @('-n', $Namespace, 'rollout', 'status', "deployment/$Release-apiserver", '--timeout=120s')
    Invoke-Native 'kubectl' @('-n', $Namespace, 'rollout', 'status', "deployment/$Release-controller-manager", '--timeout=120s')
    Invoke-Native 'kubectl' @('-n', $Namespace, 'rollout', 'status', "deployment/$Release-memory-service", '--timeout=120s')
}

function Target-UndeployStandalone {
    Write-Host "helm uninstall $Release --namespace $Namespace"
    & helm uninstall $Release --namespace $Namespace 2>$null
    & kubectl delete secret "$Release-runtime-auth" --namespace $Namespace --ignore-not-found 2>$null | Out-Null
    Write-Host 'Release uninstalled. CRDs are retained by Helm design.'
}

# ---------------------------------------------------------------------------
# Deployment – Kubernetes mode (CRD-based, webhooks)
# ---------------------------------------------------------------------------

function Target-DeployKubernetes {
    if (-not $parsed.Vars['INSTALL_PLAN']) {
        Target-FreshKubernetes
        return
    }
    if ($parsed.Vars['INSTALL_WRITERS_FROZEN'] -ne 'true') { throw 'INSTALL_WRITERS_FROZEN=true is required after controlled drain preparation.' }
    Invoke-Native 'go' @('run', './tools/kognition-install', 'bootstrap', '--plan', $parsed.Vars['INSTALL_PLAN'], '--writers-frozen')
}

function Target-FreshKubernetes {
    foreach ($name in @('KUBECONFIG', 'CHART_PACKAGE', 'INSTALL_VALUES', 'WORKLOAD_NAMESPACES')) {
        if (-not $parsed.Vars[$name]) { throw "$name is required for a fresh Kubernetes installation. See docs/getting-started/kubernetes.md." }
    }
    if ($parsed.Vars['INSTALL_EXCLUSIVE'] -ne 'true') { throw 'INSTALL_EXCLUSIVE=true confirms exclusive writes to the selected installation namespaces.' }
    $parameters = @{
        Kubeconfig = $parsed.Vars['KUBECONFIG']
        Chart = $parsed.Vars['CHART_PACKAGE']
        Values = $parsed.Vars['INSTALL_VALUES']
        WorkloadNamespace = @($parsed.Vars['WORKLOAD_NAMESPACES'] -split ',')
        Namespace = $Namespace
        Release = $Release
        Exclusive = $true
    }
    if ($parsed.Vars['KUBE_CONTEXT']) { $parameters.Context = $parsed.Vars['KUBE_CONTEXT'] }
    if ($parsed.Vars['INSTALL_STATE']) { $parameters.StateDirectory = $parsed.Vars['INSTALL_STATE'] }
    if ($parsed.Vars['INSTALL_AUTHORITY_NAMESPACE']) { $parameters.AuthorityNamespace = $parsed.Vars['INSTALL_AUTHORITY_NAMESPACE'] }
    if ($parsed.Vars['INSTALL_REPLICAS']) { $parameters.Replicas = [int]$parsed.Vars['INSTALL_REPLICAS'] }
    & (Join-Path $RepoRoot 'bootstrap.ps1') @parameters
}

function Target-UndeployKubernetes {
    Write-Host "helm uninstall $Release --namespace $Namespace"
    & helm uninstall $Release --namespace $Namespace 2>$null
    & kubectl delete secret "$Release-runtime-auth" --namespace $Namespace --ignore-not-found 2>$null | Out-Null
    & kubectl delete lease kognition-runtime-approval-binding-keys --namespace $Namespace --ignore-not-found 2>$null | Out-Null
    Write-Host 'Release uninstalled. CRDs are retained by Helm design.'
}

# ---------------------------------------------------------------------------
# Foundry / Azure credentials (generated from .env)
# ---------------------------------------------------------------------------

# Target-FoundryCreds regenerates foundry-creds.yaml from the repo-root .env
# file and applies it as a SecretRef so agents resolve Azure credentials.
# The agent runtime needs AZURE_TENANT_ID/CLIENT_ID/CLIENT_SECRET (or a
# secretless credential mode); the dispatcher derives those env vars from this
# SecretRef via the model's credentialRef. See resolveCredentialEnv in
# internal/k8s/dispatcher/dispatcher.go.
function Target-FoundryCreds {
    param([switch]$WriteOnly)

    $envFile = Join-Path $RepoRoot '.env'
    if (-not (Test-Path $envFile)) {
        Write-Host "==> No .env file at $envFile; skipping foundry-creds (agents will need credentials applied manually)."
        return
    }

    $envVars = @{}
    foreach ($line in Get-Content $envFile) {
        $trimmed = $line.Trim()
        if (-not $trimmed -or $trimmed.StartsWith('#')) { continue }
        if ($trimmed -match '^([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*)$') {
            $envVars[$matches[1]] = $matches[2].Trim().Trim('"').Trim("'")
        }
    }

    $tenantId     = $envVars['AZURE_TENANT_ID']
    $clientId     = $envVars['AZURE_CLIENT_ID']
    $clientSecret = $envVars['AZURE_CLIENT_SECRET']
    $credType     = if ($envVars['AZURE_CREDENTIAL_TYPE']) { $envVars['AZURE_CREDENTIAL_TYPE'] } else { 'client-secret' }

    if (-not $tenantId -or -not $clientId) {
        Write-Host '==> .env has no AZURE_TENANT_ID / AZURE_CLIENT_ID; skipping foundry-creds.'
        return
    }
    if ($credType -eq 'client-secret' -and -not $clientSecret) {
        Write-Host '==> .env has no AZURE_CLIENT_SECRET for client-secret auth; skipping foundry-creds.'
        return
    }

    # Target namespace(s): AGENT_NAMESPACE, comma- or space-separated for
    # multiple (PowerShell turns an unquoted comma list into a space list).
    $namespaces = @($AgentNamespace -split '[,\s]+' | ForEach-Object { $_.Trim() } | Where-Object { $_ })
    if (-not $namespaces) { $namespaces = @('default') }

    $secretData = [ordered]@{
        clientId       = $clientId
        clientSecret   = $clientSecret
        credentialType = $credType
        type           = 'azure-identity'
        tenantId       = $tenantId
    }
    $json = ($secretData | ConvertTo-Json -Compress).Replace("'", "''")

    $docs = foreach ($ns in $namespaces) {
@"
---
apiVersion: kognition.dev/v1alpha1
kind: SecretRef
metadata:
  name: foundry-creds
  namespace: $ns
  annotations:
    kognition.dev/internal-secret-data: '$json'
spec:
  backendType: internal
  backendRef: foundry-creds
  key: token
"@
    }
    $header = @"
# Azure AD credentials for Foundry, generated from .env by ``make foundry-creds``.
# Gitignored (see .gitignore) - never commit real secrets.
# Namespace(s): $($namespaces -join ', ')  (override with AGENT_NAMESPACE=ns1,ns2)
"@
    $manifest = $header + ($docs -join "`n")

    $outFile = Join-Path $RepoRoot 'foundry-creds.yaml'
    Set-Content -Path $outFile -Value $manifest -Encoding utf8
    Write-Host "==> Wrote $outFile (SecretRef 'foundry-creds' in: $($namespaces -join ', '); $credType)."

    if (-not $WriteOnly -and (Get-Command kubectl -ErrorAction SilentlyContinue)) {
        $crd = & kubectl get crd secretrefs.kognition.dev -o name 2>$null
        if ($crd) {
            foreach ($ns in $namespaces) {
                & kubectl create namespace $ns --dry-run=client -o yaml 2>$null | & kubectl apply -f - 2>$null | Out-Null
            }
            Write-Host "kubectl apply -f $outFile"
            & kubectl apply -f $outFile 2>&1 | ForEach-Object { Write-Host $_ }
        } else {
            Write-Host '==> SecretRef CRD not installed; skipping apply (install kubernetes mode first).'
        }
    }
}

# ---------------------------------------------------------------------------
# One-shot install / uninstall
# ---------------------------------------------------------------------------

function Target-Install {
    param([hashtable]$Vars)
    $mode = if ($Vars.ContainsKey('MODE')) { $Vars['MODE'] } else { 'standalone' }
    switch ($mode) {
        'standalone' { Target-DeployStandalone }
        'kubernetes' { Target-DeployKubernetes }
        default { throw "Invalid MODE '$mode'. Use MODE=standalone (default) or MODE=kubernetes." }
    }
}

function Target-Uninstall {
    $haveKubectl = [bool](Get-Command kubectl -ErrorAction SilentlyContinue)

    # Discover the Kognition CRD short names (plural, e.g. "tools", "agents").
    function Get-KognitionKinds {
        (& kubectl get crd -o name 2>$null |
            Where-Object { $_ -match '\.kognition\.dev$' } |
            ForEach-Object { (($_ -replace '^.*/', '') -split '\.')[0] })
    }

    # Best-effort graceful delete of custom resources while the controller is
    # still running, so its finalizers can clean up child objects (Deployments,
    # PVCs, ...). --wait=false keeps this from blocking if the controller is slow.
    if ($haveKubectl) {
        foreach ($k in (Get-KognitionKinds)) {
            & kubectl delete $k --all --all-namespaces --wait=false --ignore-not-found 2>$null | Out-Null
        }
    }

    if (Get-Command helm -ErrorAction SilentlyContinue) {
        Write-Host "==> Uninstalling Helm release '$Release' from namespace '$Namespace'..."
        & helm uninstall $Release --namespace $Namespace 2>$null
    }

    if ($haveKubectl) {
        # The controller (and its finalizer logic) is gone now. Any custom
        # resource still carrying a "kognition.dev/*" finalizer would block CRD
        # and namespace deletion forever, so strip them before deleting.
        Write-Host '==> Removing finalizers from any remaining Kognition resources...'
        foreach ($k in (Get-KognitionKinds)) {
            $items = (& kubectl get $k -A -o jsonpath="{range .items[*]}{.metadata.namespace}/{.metadata.name}{'\n'}{end}" 2>$null) -split "`n" | Where-Object { $_ }
            foreach ($line in $items) {
                $parts = $line -split '/', 2; $ns = $parts[0]; $name = $parts[1]
                if ($ns) { & kubectl patch $k $name -n $ns --type merge -p '{"metadata":{"finalizers":null}}' 2>$null | Out-Null }
                else { & kubectl patch $k $name --type merge -p '{"metadata":{"finalizers":null}}' 2>$null | Out-Null }
            }
        }

        Write-Host '==> Deleting Kognition CRDs (cascades to all custom resources)...'
        $crds = & kubectl get crd -o name 2>$null | Where-Object { $_ -match '\.kognition\.dev$' }
        if ($crds) { & kubectl delete @crds --ignore-not-found --wait=false 2>$null }
        Write-Host "==> Deleting namespace '$Namespace'..."
        & kubectl delete namespace $Namespace --ignore-not-found --wait=false 2>$null
    }
    Write-Host '==> Kognition fully uninstalled.'
}

function Wait-KognitionUninstalled {
    if (-not (Get-Command kubectl -ErrorAction SilentlyContinue)) { return }

    $deadline = (Get-Date).AddMinutes(3)
    do {
        $crds = @(& kubectl get crd -o name 2>$null | Where-Object { $_ -match '\.kognition\.dev$' })
        $ns = & kubectl get namespace $Namespace -o name 2>$null
        if (-not $crds -and -not $ns) { return }
        Write-Host '==> Waiting for Kognition CRDs and namespace to finish deleting...'
        Start-Sleep -Seconds 5
    } while ((Get-Date) -lt $deadline)

    throw "Timed out waiting for Kognition CRDs or namespace '$Namespace' to be removed."
}

function Stop-ManagedJaegerPortForward {
    $pidFile = Join-Path $RepoRoot '.kognition-jaeger-port-forward.pid'
    if (-not (Test-Path $pidFile)) { return }

    $raw = (Get-Content $pidFile -ErrorAction SilentlyContinue | Select-Object -First 1)
    $pidValue = 0
    if ([int]::TryParse($raw, [ref]$pidValue)) {
        $proc = Get-Process -Id $pidValue -ErrorAction SilentlyContinue
        if ($proc) {
            Write-Host "==> Stopping existing managed Jaeger port-forward (PID $pidValue)..."
            Stop-Process -Id $pidValue -Force -ErrorAction SilentlyContinue
        }
    }
    Remove-Item $pidFile -Force -ErrorAction SilentlyContinue
}

function Start-ManagedJaegerPortForward {
    Resolve-Tool 'kubectl' | Out-Null

    $pidFile = Join-Path $RepoRoot '.kognition-jaeger-port-forward.pid'
    $logDir = Join-Path $RepoRoot '.tmp'
    if (-not (Test-Path $logDir)) { New-Item -ItemType Directory -Path $logDir | Out-Null }

    if (Test-Path $pidFile) {
        $raw = (Get-Content $pidFile -ErrorAction SilentlyContinue | Select-Object -First 1)
        $pidValue = 0
        if ([int]::TryParse($raw, [ref]$pidValue)) {
            $proc = Get-Process -Id $pidValue -ErrorAction SilentlyContinue
            if ($proc) {
                Write-Host "==> Jaeger port-forward already running (PID $pidValue)."
                Write-Host '==> Jaeger UI: http://localhost:16686'
                return
            }
        }
        Remove-Item $pidFile -Force -ErrorAction SilentlyContinue
    }

    $listener = $null
    if (Get-Command Get-NetTCPConnection -ErrorAction SilentlyContinue) {
        $listener = Get-NetTCPConnection -LocalPort 16686 -State Listen -ErrorAction SilentlyContinue | Select-Object -First 1
    }
    if ($listener) {
        Write-Warning 'Port 16686 is already listening; assuming Jaeger UI is already forwarded.'
        Write-Host '==> Jaeger UI: http://localhost:16686'
        return
    }

    $kubectl = Resolve-Tool 'kubectl'
    $stdout = Join-Path $logDir 'jaeger-port-forward.out.log'
    $stderr = Join-Path $logDir 'jaeger-port-forward.err.log'
    $args = @('port-forward', '-n', $ObservabilityNamespace, 'svc/jaeger-collector', '16686:16686')
    $proc = Start-Process -FilePath $kubectl -ArgumentList $args -PassThru -WindowStyle Hidden -RedirectStandardOutput $stdout -RedirectStandardError $stderr
    Set-Content -Path $pidFile -Value $proc.Id -Encoding ascii
    Write-Host "==> Started Jaeger port-forward (PID $($proc.Id)). Logs: $stdout, $stderr"
    Write-Host '==> Jaeger UI: http://localhost:16686'
}

function Invoke-TargetWithRetry {
    param(
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)][scriptblock]$Script,
        [int]$Attempts = 2
    )

    for ($attempt = 1; $attempt -le $Attempts; $attempt++) {
        try {
            & $Script
            return
        } catch {
            if ($attempt -ge $Attempts) { throw }
            Write-Warning "$Name failed on attempt $attempt of ${Attempts}: $($_.Exception.Message)"
            Write-Host "==> Retrying $Name..."
        }
    }
}

function Ensure-CertManagerForKubernetes {
    & kubectl get crd certificates.cert-manager.io 2>$null | Out-Null
    $ready = $LASTEXITCODE -eq 0
    if ($ready) {
        foreach ($deployment in @('cert-manager', 'cert-manager-cainjector', 'cert-manager-webhook')) {
            & kubectl -n cert-manager rollout status "deployment/$deployment" '--timeout=5s' 2>$null | Out-Null
            if ($LASTEXITCODE -ne 0) {
                $ready = $false
                break
            }
        }
    }
    if ($ready) { return }

    Write-Host '==> Installing cert-manager required by Kubernetes webhook TLS...'
    Invoke-Native 'helm' @('repo', 'add', 'jetstack', 'https://charts.jetstack.io', '--force-update')
    Invoke-Native 'helm' @('upgrade', '--install', 'cert-manager', 'jetstack/cert-manager', '--namespace', 'cert-manager', '--create-namespace', '--set', 'crds.enabled=true', '--wait', '--timeout', '5m')
}

function Target-FreshKubernetesDemo {
    Write-Host 'Legacy alias: installs a new release only; never resets a cluster or installs samples.'
    Target-FreshKubernetes
}

# ---------------------------------------------------------------------------
# Docker – mode-specific builds
# ---------------------------------------------------------------------------

function Target-DockerK8s {
    Target-DockerK8sForFreshDemo -Tag $FreshKubernetesDemoImageTag
}

function Target-DockerStandalone {
    $images = $DockerImages | Where-Object { $_.Target -in @('apiserver', 'controller', 'memory-service') }
    foreach ($img in $images) {
        $tags = @("$($img.Repo):latest")
        if ($img.ChartTagged -and $AppVersion) { $tags += "$($img.Repo):$AppVersion" }
        $tagArgs = @()
        foreach ($t in $tags) { $tagArgs += @('-t', $t) }
        Write-Host ("docker build {0} --target {1} ." -f (($tags | ForEach-Object { "-t $_" }) -join ' '), $img.Target)
        Invoke-Native 'docker' (@('build') + $tagArgs + @('--target', $img.Target, '.'))
    }
    Build-PythonImages ($PythonImages | Where-Object { $_.Repo -eq 'kognition-kb-ingester' })
}

# ---------------------------------------------------------------------------
# Samples
# ---------------------------------------------------------------------------

function Target-SamplesList {
    Write-Host 'Available samples:'
    $samplesDir = Join-Path $RepoRoot 'samples'
    Get-ChildItem -Path $samplesDir -Directory | ForEach-Object {
        Write-Host "  $($_.Name)"
    }
    Get-ChildItem -Path $samplesDir -Filter '*.yaml' -File | ForEach-Object {
        Write-Host "  $($_.Name)"
    }
}

function Target-SamplesProvisionAzure {
    Resolve-Tool 'az' | Out-Null

    $settings = Get-AzureSampleSettings
    $context = (& az account show -o json --only-show-errors | Out-String) | ConvertFrom-Json
    if ($LASTEXITCODE -ne 0) { throw 'Could not read the active Azure CLI context.' }
    $account = $context.id
    $userObjectID = (& az ad signed-in-user show --query id -o tsv | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or -not $account -or -not $userObjectID) {
        throw 'Azure CLI login does not expose an active subscription and signed-in user.'
    }
    if ($settings['KOGNITION_AZURE_SUBSCRIPTION_ID'] -and $settings['KOGNITION_AZURE_SUBSCRIPTION_ID'] -ne $account) {
        throw 'The Azure CLI subscription does not match the sample environment.'
    }
    if ($settings['AZURE_TENANT_ID'] -ne $context.tenantId -or -not $settings['AZURE_CLIENT_ID'] -or -not $settings['AZURE_CLIENT_SECRET']) {
        throw 'Configure a service principal in the active tenant before provisioning Azure samples. Use deploy\azure-samples\Configure.ps1.'
    }
    $runtimeObjectID = (& az ad sp show --id $settings['AZURE_CLIENT_ID'] --query id -o tsv --only-show-errors | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or -not $runtimeObjectID) { throw 'The configured service principal does not exist in the active tenant.' }
    $suffix = ($account -replace '-', '').Substring(0, 8).ToLowerInvariant()
    $resourceGroup = "rg-kognition-samples-$suffix"
    $storageAccount = "kognsamples$suffix"
    $container = 'product-docs'
    $prefix = 'mark7'
    $location = if ($parsed.Vars['AZURE_LOCATION']) { $parsed.Vars['AZURE_LOCATION'] } elseif ($settings['KOGNITION_AZURE_LOCATION']) { $settings['KOGNITION_AZURE_LOCATION'] } else { 'eastus' }
    $deferDataPlane = $parsed.Vars['DEFER_BLOB_DATA_PLANE'] -eq 'true'
    $publicNetworkAccess = if ($deferDataPlane) { 'Disabled' } else { 'Enabled' }

    Invoke-Native 'az' @('group', 'create', '--name', $resourceGroup, '--location', $location, '--tags', 'purpose=kognition-samples', 'environment=local-test', '-o', 'none')
    $existingAccount = (& az storage account list --resource-group $resourceGroup --query "[?name=='$storageAccount'].name" -o tsv --only-show-errors | Out-String).Trim()
    if ($LASTEXITCODE -ne 0) { throw 'Could not inspect existing sample Storage accounts.' }
    if (-not $existingAccount) {
        Invoke-Native 'az' @('storage', 'account', 'create', '--name', $storageAccount, '--resource-group', $resourceGroup, '--location', $location, '--sku', 'Standard_LRS', '--kind', 'StorageV2', '--https-only', 'true', '--min-tls-version', 'TLS1_2', '--allow-blob-public-access', 'false', '--allow-shared-key-access', 'false', '--public-network-access', $publicNetworkAccess, '--tags', 'purpose=kognition-samples', 'environment=local-test', '-o', 'none')
    }
    $scope = (& az storage account show --name $storageAccount --resource-group $resourceGroup --query id -o tsv | Out-String).Trim()
    if ($LASTEXITCODE -ne 0 -or -not $scope) { throw 'Could not resolve the sample Storage account scope.' }
    if ($deferDataPlane) {
        Invoke-Native 'az' @('storage', 'container-rm', 'create', '--storage-account', $storageAccount, '--resource-group', $resourceGroup, '--name', $container, '--public-access', 'off', '-o', 'none')
    } else {
        $assignment = (& az role assignment list --assignee $userObjectID --scope $scope --role 'Storage Blob Data Contributor' --query '[0].id' -o tsv --only-show-errors | Out-String).Trim()
        if ($LASTEXITCODE -ne 0) { throw 'Could not inspect operator Blob upload permissions.' }
        if (-not $assignment) {
            Invoke-Native 'az' @('role', 'assignment', 'create', '--assignee-object-id', $userObjectID, '--assignee-principal-type', 'User', '--role', 'Storage Blob Data Contributor', '--scope', $scope, '-o', 'none')
        }
        $deadline = [DateTime]::UtcNow.AddMinutes(10)
        do {
            $containerError = (& az storage container create --account-name $storageAccount --name $container --auth-mode login --only-show-errors -o none 2>&1 | Out-String).Trim()
            $containerExitCode = $LASTEXITCODE
            if ($containerExitCode -eq 0) { break }
            if ($containerError -match 'network rules') {
                $networkAccess = (& az storage account show --name $storageAccount --resource-group $resourceGroup --query publicNetworkAccess -o tsv --only-show-errors | Out-String).Trim()
                throw "Azure Blob data-plane networking denied access (publicNetworkAccess=$networkAccess). Use an approved private-network runner or explicitly defer the Blob data plane with Configure.ps1 -DeferBlobDataPlane. No network policy was changed. $containerError"
            }
            if ($containerError -notmatch 'AuthorizationPermissionMismatch|AuthorizationFailure|required permissions') {
                throw "Azure Blob container creation failed: $containerError"
            }
            [Threading.Tasks.Task]::Delay(5000).GetAwaiter().GetResult()
        } while ([DateTime]::UtcNow -lt $deadline)
        if ($containerExitCode -ne 0) { throw "Azure Blob container authorization did not become available: $containerError" }
        Invoke-Native 'az' @('storage', 'blob', 'upload-batch', '--account-name', $storageAccount, '--destination', $container, '--source', (Join-Path $RepoRoot 'samples/docs'), '--destination-path', $prefix, '--auth-mode', 'login', '--overwrite', 'true', '-o', 'none')
    }
    $containerScope = "$scope/blobServices/default/containers/$container"
    $readerAssignment = (& az role assignment list --assignee $runtimeObjectID --scope $containerScope --role 'Storage Blob Data Reader' --query '[0].id' -o tsv --only-show-errors | Out-String).Trim()
    if ($LASTEXITCODE -ne 0) { throw 'Could not inspect runtime Blob read permissions.' }
    if (-not $readerAssignment) {
        Invoke-Native 'az' @('role', 'assignment', 'create', '--assignee-object-id', $runtimeObjectID, '--assignee-principal-type', 'ServicePrincipal', '--role', 'Storage Blob Data Reader', '--scope', $containerScope, '-o', 'none')
    }

    $envFile = Join-Path $RepoRoot '.env'
    $existing = if (Test-Path $envFile) { Get-Content $envFile } else { @() }
    $filtered = @($existing | Where-Object { $_ -notmatch '^\s*KOGNITION_AZURE_BLOB_(CONNECTION_STRING|ACCOUNT|RESOURCE_GROUP|CONTAINER|PREFIX|DATA_PLANE_STATUS)=' -and $_ -notmatch '^# Generated by samples-provision-azure\.' })
    $filtered += @(
        '# Generated by samples-provision-azure. Blob reads use the configured service principal.',
        "KOGNITION_AZURE_BLOB_ACCOUNT=$storageAccount",
        "KOGNITION_AZURE_BLOB_RESOURCE_GROUP=$resourceGroup",
        "KOGNITION_AZURE_BLOB_CONTAINER=$container",
        "KOGNITION_AZURE_BLOB_PREFIX=$prefix",
        "KOGNITION_AZURE_BLOB_DATA_PLANE_STATUS=$(if ($deferDataPlane) { 'pending-private-connectivity' } else { 'uploaded' })"
    )
    Set-Content -Path $envFile -Value $filtered -Encoding utf8
    Write-Host "Provisioned Azure Blob sample storage $resourceGroup/$storageAccount and wrote local settings to $envFile." -ForegroundColor Green
    if ($deferDataPlane) {
        Write-Warning 'DEFERRED: Blob upload and data-plane verification were not attempted. Private network connectivity is required; Storage network restrictions were preserved.'
    }
}

function Install-SampleToolDemoTrustProfiles {
    param(
        [switch]$IncludeSolutionAgents,
        [switch]$RequireLabeledAgent
    )

    $autonomyClass = @{
        apiVersion = 'kognition.dev/v1alpha1'
        kind = 'AutonomyClass'
        metadata = @{ name = 'autonomous' }
        spec = @{
            level = 90
            requiresApproval = $false
            maxCostPerAction = 100
            allowedTools = @('*')
            approvalPolicy = @{
                toolInvocation = 'never'
                memoryWrite = 'never'
                externalCommunication = 'high-risk-only'
                costThreshold = 50
            }
            constraints = @{
                maxConcurrentActions = 10
                maxTokenBudgetPerAction = 16384
            }
        }
    }
    $autonomyClass | ConvertTo-Json -Depth 20 -Compress | & kubectl apply -f - | Out-Host
    if ($LASTEXITCODE -ne 0) { throw 'Failed to apply the autonomous class for Tool demo Agents.' }

    $deadline = [DateTime]::UtcNow.AddMinutes(2)
    do {
        $labeledJSON = (& kubectl get agents -A -l 'kognition.dev/sample-tool-demo=true' -o json 2>$null | Out-String)
        $labeled = @(($labeledJSON | ConvertFrom-Json).items)
        $solutionAgents = @()
        if ($IncludeSolutionAgents) {
            foreach ($solutionName in @('analyze-dataset', 'write-blog-post')) {
                $solutionJSON = (& kubectl get agents -A -l "kognition.dev/solution=$solutionName" -o json 2>$null | Out-String)
                $solutionAgents += @(($solutionJSON | ConvertFrom-Json).items |
                    Where-Object { $_.spec.PSObject.Properties['tools'] -and @($_.spec.tools | Where-Object { $_.ref.name }).Count -gt 0 })
            }
        }
        $agents = @($labeled + $solutionAgents | Sort-Object { "$($_.metadata.namespace)/$($_.metadata.name)" } -Unique)
        $hasExpectedLabeledAgents = -not $RequireLabeledAgent -or $labeled.Count -gt 0
        $hasExpectedSolutionAgents = -not $IncludeSolutionAgents -or $solutionAgents.Count -eq 4
        if ($hasExpectedLabeledAgents -and $hasExpectedSolutionAgents) { break }
        [Threading.Tasks.Task]::Delay(1000).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)

    if ($RequireLabeledAgent -and $labeled.Count -eq 0) {
        throw 'No Agent labeled kognition.dev/sample-tool-demo=true appeared before the TrustProfile deadline.'
    }
    if ($IncludeSolutionAgents -and $solutionAgents.Count -ne 4) {
        throw "The sample Solutions did not produce all four Tool-bearing Agents before the TrustProfile deadline."
    }

    foreach ($agent in $agents) {
        $agentName = $agent.metadata.name
        $namespace = $agent.metadata.namespace
        $agentUID = $agent.metadata.uid
        if (-not $agentUID) { throw "Agent $namespace/$agentName has no Kubernetes UID." }
        $profileName = "$agentName-sample-tool-trust"

        $existingProfileJSON = (& kubectl get trustprofile $profileName -n $namespace -o json 2>$null | Out-String)
        if ($LASTEXITCODE -eq 0) {
            $existingProfile = $existingProfileJSON | ConvertFrom-Json
            if ($existingProfile.spec.agentIncarnationUID -ne "$agentUID") {
                if ($existingProfile.metadata.labels.'kognition.dev/sample-tool-demo' -ne 'true') {
                    throw "Refusing to replace stale TrustProfile $namespace/$profileName because it is not owned by the Tool demo installer."
                }
                Invoke-Native 'kubectl' @('delete', 'trustprofile', $profileName, '-n', $namespace, '--wait=true')
            }
        }

        $trustProfile = @{
            apiVersion = 'kognition.dev/v1alpha1'
            kind = 'TrustProfile'
            metadata = @{
                name = $profileName
                namespace = $namespace
                labels = @{ 'kognition.dev/sample-tool-demo' = 'true' }
            }
            spec = @{
                agentRef = @{ name = $agentName }
                agentIncarnationUID = "$agentUID"
                initialScore = 100
                initialClass = 'autonomous'
                maxClass = 'autonomous'
                evaluationInterval = '24h'
            }
        }
        $trustProfile | ConvertTo-Json -Depth 20 -Compress | & kubectl apply -f - | Out-Host
        if ($LASTEXITCODE -ne 0) { throw "Failed to apply TrustProfile for Agent $namespace/$agentName." }
    }

    foreach ($agent in $agents) {
        $agentName = $agent.metadata.name
        $namespace = $agent.metadata.namespace
        $agentUID = "$($agent.metadata.uid)"
        $expectedTools = @($agent.spec.tools | ForEach-Object { $_.ref.name } | Sort-Object -Unique)

        $policiesJSON = (& kubectl get agentpolicies -n $namespace -o json | Out-String)
        if ($LASTEXITCODE -ne 0) { throw "Failed to inspect AgentPolicies in namespace $namespace." }
        $matchingPolicies = @(($policiesJSON | ConvertFrom-Json).items | Where-Object {
            $_.spec.PSObject.Properties['enforce'] -and $_.spec.enforce -eq $true -and
            $_.spec.PSObject.Properties['targetAgents'] -and @($_.spec.targetAgents) -contains $agentName
        })
        $hasExactGrantPolicy = $false
        foreach ($policy in $matchingPolicies) {
            if (-not $policy.spec.PSObject.Properties['toolGrants']) { continue }
            $grantedTools = @($policy.spec.toolGrants | Where-Object { $_.allow -eq $true } |
                ForEach-Object { $_.toolRef } | Sort-Object -Unique)
            if (($grantedTools -join "`n") -eq ($expectedTools -join "`n")) {
                $hasExactGrantPolicy = $true
                break
            }
        }
        if (-not $hasExactGrantPolicy) {
            throw "Agent $namespace/$agentName lacks one enforced AgentPolicy granting exactly: $($expectedTools -join ', ')."
        }

        $rolesJSON = (& kubectl get agentroles -n $namespace -o json | Out-String)
        $bindingsJSON = (& kubectl get agentrolebindings -n $namespace -o json | Out-String)
        if ($LASTEXITCODE -ne 0) { throw "Failed to inspect Tool RBAC in namespace $namespace." }
        $roles = @(($rolesJSON | ConvertFrom-Json).items)
        $bindings = @(($bindingsJSON | ConvertFrom-Json).items | Where-Object {
            @($_.spec.subjects | Where-Object { $_.kind -eq 'Agent' -and $_.name -eq $agentName }).Count -gt 0
        })
        $hasToolRole = $false
        foreach ($binding in $bindings) {
            $roleName = $binding.spec.roleRef.name
            $role = @($roles | Where-Object { $_.metadata.name -eq $roleName }) | Select-Object -First 1
            if ($role -and @($role.rules | Where-Object {
                    @($_.resources) -contains 'Tool' -and
                    @($_.verbs) -contains 'get' -and
                    @($_.verbs) -contains 'invoke'
                }).Count -gt 0) {
                $hasToolRole = $true
                break
            }
        }
        if (-not $hasToolRole) {
            throw "Agent $namespace/$agentName lacks AgentRoleBinding access to get and invoke Tools."
        }

        $profileName = "$agentName-sample-tool-trust"
        $profileJSON = (& kubectl get trustprofile $profileName -n $namespace -o json | Out-String)
        if ($LASTEXITCODE -ne 0) { throw "TrustProfile $namespace/$profileName was not created." }
        $profile = $profileJSON | ConvertFrom-Json
        if ($profile.spec.agentRef.name -ne $agentName -or
            $profile.spec.agentIncarnationUID -ne $agentUID -or
            $profile.spec.initialClass -ne 'autonomous' -or
            $profile.spec.maxClass -ne 'autonomous') {
            throw "TrustProfile $namespace/$profileName is not bound autonomously to the live Agent UID."
        }
    }

    Write-Host "Applied UID-bound autonomous TrustProfiles for $($agents.Count) Tool demo Agents." -ForegroundColor Green
}

function Assert-SampleControllerNamespaces {
    param([string[]]$RequiredNamespaces)
    $deploymentJSON = (& kubectl get deployment -n $Namespace -l 'app.kubernetes.io/component=k8s-controller' -o json | Out-String)
    if ($LASTEXITCODE -ne 0) { throw 'Unable to inspect controller namespace enrollment before sample installation.' }
    $deployments = @(($deploymentJSON | ConvertFrom-Json).items | Where-Object {
        $_.spec.template.metadata.labels.'app.kubernetes.io/instance' -ceq $Release
    })
    if ($deployments.Count -eq 0) { return }
    if ($deployments.Count -ne 1) { throw 'Unable to inspect controller namespace enrollment: expected one controller Deployment.' }
    $deployment = $deployments[0]
    $enrollment = @($deployment.spec.template.spec.containers.env | Where-Object { $_.name -eq 'KOGNITION_MANAGED_NAMESPACES' })
    if ($enrollment.Count -eq 0) { return }
    $managed = @($enrollment | ForEach-Object { $_.value -split ',' })
    $missing = @($RequiredNamespaces | Where-Object { $managed -cnotcontains $_ })
    if ($missing.Count -gt 0) {
        throw "Controller does not manage sample namespaces: $($missing -join ', '). Create these namespaces and enroll them in k8sController.managedNamespaces before installing samples."
    }
}

function Target-SamplesInstall {
    param([hashtable]$Vars)
    $sample = if ($Vars.ContainsKey('SAMPLE')) { $Vars['SAMPLE'] } else { '' }
    $samplesDir = Join-Path $RepoRoot 'samples'

    if ($sample) {
        $chartFile    = Join-Path $samplesDir "$sample/Chart.yaml"
        $applyScript  = Join-Path $samplesDir "$sample/apply.ps1"
        $manifestFile = Join-Path $samplesDir "$sample/manifests.yaml"
        $singleFile   = Join-Path $samplesDir $sample

        if (Test-Path $chartFile) {
            Write-Host "Installing Helm sample: $sample"
            $helmArguments = @('upgrade', '--install', $sample, (Join-Path $samplesDir $sample), '--namespace', $AgentNamespace, '--create-namespace')
            if ($sample -eq 'helm-multi-agent' -and (Get-AzureSampleSettings)['MS_FOUNDRY_PROJECT_ENDPOINT']) {
                Target-FoundryCreds
                $helmArguments += @('--set-string', "namespace=$AgentNamespace", '--set', 'credentials.create=false')
                $existingClass = (& kubectl get modelclass foundry --ignore-not-found -o json | Out-String).Trim()
                if ($LASTEXITCODE -ne 0) { throw 'Could not inspect the shared Foundry ModelClass before Helm sample installation.' }
                if ($existingClass) {
                    $class = $existingClass | ConvertFrom-Json
                    $annotations = $class.metadata.annotations
                    $ownedByRelease = $annotations -and
                        $annotations.'meta.helm.sh/release-name' -eq $sample -and
                        $annotations.'meta.helm.sh/release-namespace' -eq $AgentNamespace
                    if (-not $ownedByRelease) { $helmArguments += @('--set', 'modelClass.create=false') }
                }
                $helmArguments += @(Get-AzureSampleHelmArguments)
            }
            Invoke-Native 'helm' $helmArguments
        } elseif (Test-Path $applyScript) {
            Write-Host "Running sample installer: $applyScript"
            & $applyScript
            if ($LASTEXITCODE -ne 0) { throw "apply.ps1 failed for sample '$sample'." }
        } elseif (Test-Path $manifestFile) {
            Write-Host "kubectl apply -f $manifestFile"
            Invoke-AzureSampleApply -Path $manifestFile
        } elseif (Test-Path $singleFile) {
            Write-Host "kubectl apply -f $singleFile"
            Invoke-AzureSampleApply -Path $singleFile
        } else {
            throw "Sample '$sample' not found."
        }
        $toolTrustManifest = if (Test-Path $manifestFile) { $manifestFile } elseif (Test-Path $singleFile -PathType Leaf) { $singleFile } else { $null }
        $manifestRequiresToolTrust = $toolTrustManifest -and
            (Select-String -Path $toolTrustManifest -SimpleMatch 'kognition.dev/sample-tool-demo' -Quiet)
        if ($manifestRequiresToolTrust -or $sample -eq 'solution') {
            Install-SampleToolDemoTrustProfiles -IncludeSolutionAgents:($sample -eq 'solution') -RequireLabeledAgent:$manifestRequiresToolTrust
        }
    } else {
        Write-Host 'Installing the full compatible sample bundle...'
        $failed = @()
        $bundleLabelKey = 'kognition.dev/sample-bundle'
        $bundleLabelValue = 'compatible'
        $sampleNamespaces = @('default', 'kognition-demo', 'samples-governance', 'samples-multi-agent', 'samples-azure-blob', 'production', 'development')

        Assert-SampleControllerNamespaces -RequiredNamespaces $sampleNamespaces

        foreach ($sampleNamespace in $sampleNamespaces) {
            & kubectl get namespace $sampleNamespace 2>$null | Out-Null
            if ($LASTEXITCODE -ne 0) {
                Invoke-Native 'kubectl' @('create', 'namespace', $sampleNamespace)
            }
        }

        # Seed real credentials before provider-backed Models are created so
        # their first reconciliation cannot become terminally CredentialMissing.
        # Reapply again after all manifests to replace any sample placeholders.
        $originalAgentNamespace = $AgentNamespace
        try {
            $AgentNamespace = (@($originalAgentNamespace -split '[,\s]+') + $sampleNamespaces |
                Where-Object { $_ } | Select-Object -Unique) -join ','
            Target-FoundryCreds
        }
        finally {
            $AgentNamespace = $originalAgentNamespace
        }

        $samplePriority = @{
            'simple-agent'         = 0
            'workflows'            = 5
            'full-stack-agent'     = 10
            'multi-agent-workflow' = 20
            'routing'              = 90
            'agentinstance'        = 100
        }
        $explicitSkips = @{
            'autonomy'                   = 'standalone sample reuses research-agent; Tool demos provision the autonomous class without applying that conflicting Agent'
            'docs'                       = 'documentation-only directory'
            'helm-governance'            = 'installed below as the isolated sample-governance release'
            'helm-multi-agent'           = 'installed below as the isolated sample-multi-agent release'
            'helm-umbrella'              = 'requires an isolated cluster because it deploys a second platform controller and cluster-scoped webhooks'
            'knowledge-base-blob-e2e'    = 'real-Azure profile; run samples-provision-azure from an Azure-connected runner, then install explicitly'
            'kagent-observability'      = 'standalone recovery fixture; requires its own reviewed platform, namespaces and images'
        }
        Get-ChildItem -Path $samplesDir -Directory |
            Sort-Object @{ Expression = { if ($samplePriority.ContainsKey($_.Name)) { $samplePriority[$_.Name] } else { 50 } } }, Name |
            ForEach-Object {
            $dir   = $_
            $mf    = Join-Path $dir.FullName 'manifests.yaml'
            $apply = Join-Path $dir.FullName 'apply.ps1'
            if ($explicitSkips.ContainsKey($dir.Name)) {
                Write-Host "  skip $($dir.Name): $($explicitSkips[$dir.Name])"
            }
            elseif (Test-Path $apply) {
                # Script-based sample (e.g. knowledge-base-kognition-docs): the
                # apply.ps1 handles its own multi-file / ConfigMap / Secret wiring.
                Write-Host "  & $apply"
                & $apply
                if ($LASTEXITCODE -ne 0) {
                    Write-Warning "  sample '$($dir.Name)' apply.ps1 did not complete cleanly; continuing."
                    $failed += $dir.Name
                } else {
                    Get-ChildItem -Path $dir.FullName -Filter '*.yaml' -File |
                        Where-Object { $_.Name -notlike '*.template.yaml' -and (Select-String -Path $_.FullName -Pattern '^\s*kind\s*:' -Quiet) } |
                        ForEach-Object {
                            $previousErrorActionPreference = $ErrorActionPreference
                            try {
                                $ErrorActionPreference = 'Continue'
                                Read-AzureSampleManifest -Path $_.FullName | & kubectl label -f - "$bundleLabelKey=$bundleLabelValue" --overwrite 2>$null
                                $labelExitCode = $LASTEXITCODE
                                if ($labelExitCode -ne 0) {
                                    Read-AzureSampleManifest -Path $_.FullName | & kubectl get -f - -o name 2>$null | Out-Null
                                    if ($LASTEXITCODE -eq 0) { $failed += "$($dir.Name)/$($_.Name):label" }
                                }
                            }
                            finally {
                                $ErrorActionPreference = $previousErrorActionPreference
                            }
                        }
                }
            }
            elseif (Test-Path $mf) {
                Write-Host "  kubectl apply -f $mf"
                Read-AzureSampleManifest -Path $mf | & kubectl apply -f -
                if ($LASTEXITCODE -ne 0) {
                    Write-Warning "  sample '$($dir.Name)' did not apply cleanly; continuing."
                    $failed += $dir.Name
                } else {
                    Read-AzureSampleManifest -Path $mf | & kubectl label -f - "$bundleLabelKey=$bundleLabelValue" --overwrite 2>$null
                    if ($LASTEXITCODE -ne 0) { $failed += "$($dir.Name)/manifests.yaml:label" }
                }

                # Apply supplemental manifests such as schedules and explicit
                # index runs that live beside the primary multi-document file.
                Get-ChildItem -Path $dir.FullName -Filter '*.yaml' -File |
                    Where-Object { $_.FullName -ne $mf -and $_.Name -notlike '*.template.yaml' } |
                    Sort-Object Name |
                    ForEach-Object {
                        Write-Host "  kubectl apply -f $($_.FullName)"
                        Read-AzureSampleManifest -Path $_.FullName | & kubectl apply -f -
                        if ($LASTEXITCODE -ne 0) {
                            Write-Warning "  supplemental manifest '$($_.Name)' did not apply cleanly; continuing."
                            $failed += "$($dir.Name)/$($_.Name)"
                        } else {
                            Read-AzureSampleManifest -Path $_.FullName | & kubectl label -f - "$bundleLabelKey=$bundleLabelValue" --overwrite 2>$null
                            if ($LASTEXITCODE -ne 0) { $failed += "$($dir.Name)/$($_.Name):label" }
                        }
                    }
            }
            else {
                $manifests = @(Get-ChildItem -Path $dir.FullName -Filter '*.yaml' -File |
                    Where-Object { $_.Name -notlike '*.template.yaml' -and (Select-String -Path $_.FullName -Pattern '^\s*kind\s*:' -Quiet) } |
                    Sort-Object Name)
                foreach ($manifest in $manifests) {
                    Write-Host "  kubectl apply -f $($manifest.FullName)"
                    Read-AzureSampleManifest -Path $manifest.FullName | & kubectl apply -f -
                    if ($LASTEXITCODE -ne 0) {
                        Write-Warning "  manifest '$($manifest.Name)' did not apply cleanly; continuing."
                        $failed += "$($dir.Name)/$($manifest.Name)"
                    } else {
                        Read-AzureSampleManifest -Path $manifest.FullName | & kubectl label -f - "$bundleLabelKey=$bundleLabelValue" --overwrite 2>$null
                        if ($LASTEXITCODE -ne 0) { $failed += "$($dir.Name)/$($manifest.Name):label" }
                    }
                }
            }
        }

        $rootManifestPriority = @{ 'k8s-mode-demo.yaml' = 0; 'governance-demo.yaml' = 10 }
        Get-ChildItem -Path $samplesDir -Filter '*.yaml' -File |
            Where-Object { $_.Name -notlike '*.template.yaml' -and (Select-String -Path $_.FullName -Pattern '^\s*kind\s*:' -Quiet) } |
            Sort-Object @{ Expression = { if ($rootManifestPriority.ContainsKey($_.Name)) { $rootManifestPriority[$_.Name] } else { 5 } } }, Name |
            ForEach-Object {
                Write-Host "  kubectl apply -f $($_.FullName)"
                Read-AzureSampleManifest -Path $_.FullName | & kubectl apply -f -
                if ($LASTEXITCODE -ne 0) {
                    Write-Warning "  root manifest '$($_.Name)' did not apply cleanly; continuing."
                    $failed += $_.Name
                } else {
                    Read-AzureSampleManifest -Path $_.FullName | & kubectl label -f - "$bundleLabelKey=$bundleLabelValue" --overwrite 2>$null
                    if ($LASTEXITCODE -ne 0) { $failed += "$($_.Name):label" }
                }
            }

        # Apply generated credentials last so sample manifests can never
        # replace a working SecretRef with placeholders.
        $originalAgentNamespace = $AgentNamespace
        try {
            $AgentNamespace = (@($originalAgentNamespace -split '[,\s]+') + $sampleNamespaces |
                Where-Object { $_ } | Select-Object -Unique) -join ','
            Target-FoundryCreds
        }
        finally {
            $AgentNamespace = $originalAgentNamespace
        }
        if (-not (Test-Path (Join-Path $RepoRoot 'foundry-creds.yaml'))) {
            throw 'Full sample bundle requires Foundry credentials in .env so provider-backed Agents and KnowledgeBases are functional.'
        }

        Invoke-Native 'helm' @('upgrade', '--install', 'sample-governance', (Join-Path $samplesDir 'helm-governance'), '--namespace', 'samples-governance', '--create-namespace', '--set', 'namespace=samples-governance', '--set', "sampleBundleLabel=$bundleLabelValue")
        Invoke-Native 'helm' (@('upgrade', '--install', 'sample-multi-agent', (Join-Path $samplesDir 'helm-multi-agent'), '--namespace', 'samples-multi-agent', '--create-namespace', '--set', 'namespace=samples-multi-agent', '--set', 'credentials.create=false', '--set', 'modelClass.create=false', '--set', "sampleBundleLabel=$bundleLabelValue") + @(Get-AzureSampleHelmArguments))
        Invoke-AzureSampleApply -Path (Join-Path $samplesDir 'model-grants.yaml')
        Read-AzureSampleManifest -Path (Join-Path $samplesDir 'model-grants.yaml') | & kubectl label -f - "$bundleLabelKey=$bundleLabelValue" --overwrite 2>$null
        if ($LASTEXITCODE -ne 0) { $failed += 'model-grants.yaml:label' }
        Install-SampleToolDemoTrustProfiles -IncludeSolutionAgents
        if ($failed.Count -gt 0) {
            throw ("Sample bundle deployment failed: {0}" -f ($failed -join ', '))
        }

        $readiness = @{
            agents                    = @('Running', 'Ready')
            agentinstances            = @('Running', 'Ready')
            agentjobs                 = @('Running', 'Ready')
            models                    = @('Ready')
            knowledgebases            = @('Ready')
            knowledgebaseindexruns    = @('Succeeded')
            memoryclaims              = @('Bound')
            memoryvolumes             = @('Ready')
            agentidentities           = @('Ready')
            workflowruns              = @('Running', 'Ready')
        }
        $deadline = [DateTime]::UtcNow.AddMinutes(5)
        do {
            $notReady = @()
            foreach ($kind in @('agents', 'agentinstances', 'agentjobs', 'models', 'knowledgebases', 'knowledgebaseindexruns', 'memoryclaims', 'memoryvolumes', 'workflows', 'workflowruns', 'agentidentities')) {
                $resources = ((& kubectl get $kind -A -l "$bundleLabelKey=$bundleLabelValue" -o json 2>$null | Out-String) | ConvertFrom-Json).items
                $failedResources = @($resources | Where-Object { $_.status.phase -in @('Failed', 'Error') })
                if ($failedResources.Count -gt 0) {
                    $names = $failedResources | ForEach-Object { "$($_.metadata.namespace)/$($_.metadata.name)=$($_.status.phase)" }
                    throw "Sample bundle has unhealthy $kind resources: $($names -join ', ')"
                }
                if ($readiness.ContainsKey($kind)) {
                    $pending = @($resources | Where-Object { $_.status.phase -notin $readiness[$kind] })
                    $notReady += @($pending | ForEach-Object { "$kind/$($_.metadata.namespace)/$($_.metadata.name)=$($_.status.phase)" })
                }
            }
            if ($notReady.Count -eq 0) { break }
            [Threading.Tasks.Task]::Delay(1000).GetAwaiter().GetResult()
        } while ([DateTime]::UtcNow -lt $deadline)
        if ($notReady.Count -gt 0) { throw "Sample bundle resources did not become ready: $($notReady -join ', ')" }
        Write-Host 'Full compatible sample bundle is installed and has no terminal failed resources.' -ForegroundColor Green
    }
}

function Target-SamplesUninstall {
    param([hashtable]$Vars)
    $sample = if ($Vars.ContainsKey('SAMPLE')) { $Vars['SAMPLE'] } else { '' }
    $samplesDir = Join-Path $RepoRoot 'samples'

    if ($sample) {
        $chartFile    = Join-Path $samplesDir "$sample/Chart.yaml"
        $applyScript  = Join-Path $samplesDir "$sample/apply.ps1"
        $deleteScript = Join-Path $samplesDir "$sample/delete.ps1"
        $manifestFile = Join-Path $samplesDir "$sample/manifests.yaml"
        $sampleDir    = Join-Path $samplesDir $sample
        $singleFile   = Join-Path $samplesDir $sample

        if (Test-Path $chartFile) {
            Write-Host "Uninstalling Helm sample: $sample"
            & helm uninstall $sample --namespace $AgentNamespace 2>$null
            Write-Host 'Done.'
        } elseif (Test-Path $deleteScript) {
            Write-Host "Running sample teardown: $deleteScript"
            & $deleteScript
            if ($LASTEXITCODE -ne 0) { throw "delete.ps1 failed for sample '$sample'." }
        } elseif (Test-Path $applyScript) {
            # Script-based sample: no delete.ps1, so remove every manifest that
            # ships in the directory. Imperatively-created objects (e.g. the docs
            # ConfigMap) are left behind harmlessly and overwritten on re-apply.
            Write-Host "kubectl delete -f $sampleDir"
            Invoke-AzureSampleDelete -Path $sampleDir
        } elseif (Test-Path $manifestFile) {
            Write-Host "kubectl delete -f $manifestFile"
            Invoke-AzureSampleDelete -Path $manifestFile
        } elseif ((Test-Path $singleFile) -and -not (Test-Path $singleFile -PathType Container)) {
            Write-Host "kubectl delete -f $singleFile"
            Invoke-AzureSampleDelete -Path $singleFile
        } else {
            throw "Sample '$sample' not found."
        }
    } else {
        # --wait=false so the controller clears finalizers asynchronously; a
        # blocking delete would stall the whole loop on the first agent's
        # cleanup finalizer.
        Write-Host 'Uninstalling all samples...'
        $explicitSkips = @('docs', 'helm-governance', 'helm-multi-agent', 'helm-umbrella', 'knowledge-base-blob-e2e', 'kagent-observability')
        Get-ChildItem -Path $samplesDir -Directory |
            Where-Object { $explicitSkips -notcontains $_.Name } |
            Sort-Object Name -Descending |
            ForEach-Object {
            $dir   = $_
            $apply = Join-Path $dir.FullName 'apply.ps1'
            $mf    = Join-Path $dir.FullName 'manifests.yaml'
            if (Test-Path $apply) {
                Write-Host "  kubectl delete -f $($dir.FullName)"
                Invoke-AzureSampleDelete -Path $dir.FullName
            }
            elseif (Test-Path $mf) {
                Get-ChildItem -Path $dir.FullName -Filter '*.yaml' -File |
                    Where-Object { Select-String -Path $_.FullName -Pattern '^\s*kind\s*:' -Quiet } |
                    Sort-Object Name -Descending |
                    ForEach-Object {
                        Write-Host "  kubectl delete -f $($_.FullName)"
                        Invoke-AzureSampleDelete -Path $_.FullName
                    }
            }
            else {
                Get-ChildItem -Path $dir.FullName -Filter '*.yaml' -File |
                    Where-Object { Select-String -Path $_.FullName -Pattern '^\s*kind\s*:' -Quiet } |
                    Sort-Object Name -Descending |
                    ForEach-Object {
                        Write-Host "  kubectl delete -f $($_.FullName)"
                        Invoke-AzureSampleDelete -Path $_.FullName
                    }
            }
        }

        $rootManifestPriority = @{ 'governance-demo.yaml' = 0; 'mem0-memory-class.yaml' = 5; 'k8s-mode-demo.yaml' = 10 }
        Get-ChildItem -Path $samplesDir -Filter '*.yaml' -File |
            Where-Object { Select-String -Path $_.FullName -Pattern '^\s*kind\s*:' -Quiet } |
            Sort-Object @{ Expression = { if ($rootManifestPriority.ContainsKey($_.Name)) { $rootManifestPriority[$_.Name] } else { 5 } } }, Name |
            ForEach-Object {
                Write-Host "  kubectl delete -f $($_.FullName)"
                Invoke-AzureSampleDelete -Path $_.FullName
            }
        Write-Host 'Delete requests submitted; the controller finalizes cleanup in the background.'
    }
}

# ---------------------------------------------------------------------------
# Status & diagnostics
# ---------------------------------------------------------------------------

function Target-Status {
    $helm    = Get-Command helm    -ErrorAction SilentlyContinue
    $kubectl = Get-Command kubectl -ErrorAction SilentlyContinue

    Write-Host '=== Helm Release ==='
    if ($helm) {
        $status = & $helm.Source status $Release --namespace $Namespace 2>$null
        if ($LASTEXITCODE -ne 0) {
            Write-Host '(not installed)'
        } else {
            $status | Where-Object { $_ -match '^(NAME|NAMESPACE|STATUS|REVISION):' } | ForEach-Object { Write-Host $_ }
        }
    } else { Write-Host '(helm not on PATH)' }

    Write-Host ''
    Write-Host "=== Pods ($Namespace) ==="
    if ($kubectl) {
        & $kubectl.Source get pods -n $Namespace -o wide 2>$null
        if ($LASTEXITCODE -ne 0) { Write-Host '(no pods)' }
    } else { Write-Host '(kubectl not on PATH)' }

    foreach ($res in @(
        @{ Title = 'Agents';           Kind = 'agents' }
        @{ Title = 'Workflows';        Kind = 'workflows' }
        @{ Title = 'Agent Instances';   Kind = 'agentinstances' }
        @{ Title = 'Workflow Runs';     Kind = 'workflowruns' }
    )) {
        Write-Host ''
        Write-Host "=== $($res.Title) ==="
        if ($kubectl) {
            & $kubectl.Source get $res.Kind --all-namespaces 2>$null
            if ($LASTEXITCODE -ne 0) { Write-Host '(none)' }
        } else { Write-Host '(kubectl not on PATH)' }
    }
}

function Target-Logs {
    param([hashtable]$Vars)
    $component = if ($Vars.ContainsKey('COMPONENT')) { $Vars['COMPONENT'] } else { '' }
    if ($component) {
        Invoke-Native 'kubectl' @('logs', '-n', $Namespace, '-l', "app.kubernetes.io/name=$component", '-f', '--tail=100')
    } else {
        Invoke-Native 'kubectl' @('logs', '-n', $Namespace, '-l', "app.kubernetes.io/instance=$Release", '-f', '--tail=50', '--max-log-requests=10')
    }
}

function Target-PortForward {
    Invoke-Native 'kubectl' @('port-forward', '-n', $Namespace, "svc/$Release-apiserver", '8080:8080')
}

function Update-KognitionObservabilitySettings {
    param([bool]$Enabled)

    & helm status $Release --namespace $Namespace 2>$null | Out-Null
    if ($LASTEXITCODE -ne 0) {
        Write-Host "Helm release '$Release' is not installed in namespace '$Namespace' yet."
        Write-Host 'Install Kognition with: .\make.ps1 install MODE=kubernetes'
        Write-Host 'Then rerun: .\make.ps1 observability-install'
        return
    }

    $helmArgs = @(
        'upgrade', $Release, $Chart,
        '--namespace', $Namespace,
        '--reuse-values',
        '--set', "observability.openTelemetry.enabled=$($Enabled.ToString().ToLower())",
        '--set', "observability.openTelemetry.endpoint=$(if ($Enabled) { $JaegerOTLPEndpoint } else { '' })",
        '--set', ("observability.metrics.serviceMonitor.enabled=$($Enabled.ToString().ToLower())"),
        '--set', ("observability.grafanaDashboards.enabled=$($Enabled.ToString().ToLower())"),
        '--set', ("toolGateway.monitoring.serviceMonitor.enabled=$($Enabled.ToString().ToLower())"),
        '--set', ("toolGateway.monitoring.prometheusRule.enabled=$($Enabled.ToString().ToLower())"),
        '--set', ("toolGateway.monitoring.grafanaDashboard.enabled=$($Enabled.ToString().ToLower())"),
        '--set', 'observability.metrics.serviceMonitor.labels.release=kognition-observability',
        '--set-string', 'observability.grafanaDashboards.labels.grafana_dashboard=1',
        '--set', 'toolGateway.monitoring.serviceMonitor.labels.release=kognition-observability',
        '--set', 'toolGateway.monitoring.prometheusRule.labels.release=kognition-observability',
        '--set-string', 'toolGateway.monitoring.grafanaDashboard.labels.grafana_dashboard=1'
    )
    if ($Enabled) {
        $helmArgs += @(
            '--set', 'observability.openTelemetry.protocol=grpc',
            '--set', 'observability.openTelemetry.serviceName=kognition',
            '--set', 'observability.openTelemetry.environment=dev'
        )
    }
    Write-Host "helm $($helmArgs -join ' ')"
    Invoke-Native 'helm' $helmArgs
    & kubectl -n $Namespace get "deployment/$Release-k8s-controller" 2>$null | Out-Null
    if ($LASTEXITCODE -eq 0) {
        Invoke-Native 'kubectl' @('-n', $Namespace, 'rollout', 'status', "deployment/$Release-k8s-controller", '--timeout=120s')
    } else {
        & kubectl -n $Namespace get "deployment/$Release-controller-manager" 2>$null | Out-Null
        if ($LASTEXITCODE -eq 0) {
            Invoke-Native 'kubectl' @('-n', $Namespace, 'rollout', 'status', "deployment/$Release-controller-manager", '--timeout=120s')
        }
    }
    if ($Enabled) { Refresh-AgentRuntimes }
}

function Target-ObservabilityInstall {
    Resolve-Tool 'kubectl' | Out-Null
    Resolve-Tool 'helm' | Out-Null
    & helm status $Release --namespace $Namespace 2>$null | Out-Null
    if ($LASTEXITCODE -ne 0) {
        throw "Kognition Helm release '$Release' is not installed in namespace '$Namespace'. Install Kognition with: .\make.ps1 install MODE=kubernetes; then rerun: .\make.ps1 observability-install"
    }
    $manifest = Join-Path $RepoRoot 'deploy/observability/jaeger.yaml'
    $prometheusValues = Join-Path $RepoRoot 'deploy/observability/prometheus-values.yaml'
    if (-not (Test-Path $manifest)) { throw "Jaeger manifest not found at $manifest" }
    if (-not (Test-Path $prometheusValues)) { throw "Prometheus values file not found at $prometheusValues" }

    Write-Host "kubectl create namespace $ObservabilityNamespace --dry-run=client -o yaml | kubectl apply -f -"
    $namespaceYaml = & kubectl create namespace $ObservabilityNamespace --dry-run=client -o yaml
    if ($LASTEXITCODE -ne 0) { throw "kubectl create namespace dry-run failed with code $LASTEXITCODE" }
    $namespaceYaml | & kubectl apply -f -
    if ($LASTEXITCODE -ne 0) { throw "kubectl apply namespace failed with code $LASTEXITCODE" }

    Invoke-Native 'kubectl' @('apply', '-n', $ObservabilityNamespace, '-f', $manifest)
    Invoke-Native 'kubectl' @('-n', $ObservabilityNamespace, 'rollout', 'status', 'deployment/jaeger', '--timeout=120s')

    Invoke-Native 'helm' @('repo', 'add', 'prometheus-community', 'https://prometheus-community.github.io/helm-charts', '--force-update')
    Invoke-Native 'helm' @('upgrade', '--install', 'kognition-observability', 'prometheus-community/kube-prometheus-stack', '--version', $PrometheusStackVersion, '--namespace', $ObservabilityNamespace, '--values', $prometheusValues, '--wait', '--timeout', '10m')
    Invoke-Native 'kubectl' @('-n', $ObservabilityNamespace, 'rollout', 'status', 'deployment/kognition-observability-grafana', '--timeout=180s')
    Invoke-Native 'kubectl' @('-n', $ObservabilityNamespace, 'wait', '--for=condition=Available', 'deployment', '-l', 'app=kube-prometheus-stack-operator,release=kognition-observability', '--timeout=180s')
    Update-KognitionObservabilitySettings -Enabled $true

    Write-Host 'Observability bundle installed.'
    Write-Host 'Jaeger UI: .\make.ps1 jaeger-ui, then open http://localhost:16686'
    Write-Host 'Grafana UI: .\make.ps1 grafana-ui, then open http://localhost:3000'
}

function Target-JaegerInstall { Target-ObservabilityInstall }

function Target-JaegerUI {
    Invoke-Native 'kubectl' @('port-forward', '-n', $ObservabilityNamespace, 'svc/jaeger-collector', '16686:16686')
}

function Target-GrafanaUI {
    Invoke-Native 'kubectl' @('port-forward', '-n', $ObservabilityNamespace, 'svc/kognition-observability-grafana', '3000:80')
}

function Target-ObservabilityUninstall {
    $manifest = Join-Path $RepoRoot 'deploy/observability/jaeger.yaml'
    if (Get-Command kubectl -ErrorAction SilentlyContinue) {
        & kubectl delete -n $ObservabilityNamespace -f $manifest --ignore-not-found
    }
    if (Get-Command helm -ErrorAction SilentlyContinue) {
        & helm uninstall 'kognition-observability' --namespace $ObservabilityNamespace 2>$null
        Update-KognitionObservabilitySettings -Enabled $false
    }
}

function Target-JaegerUninstall { Target-ObservabilityUninstall }

# Target-Studio rebuilds kognitionctl into bin\ and launches the visual
# builder from THAT fresh binary — sidestepping any stale `go install`ed
# kognitionctl on PATH whose embedded assets predate recent Studio features.
function Target-Studio {
    param([hashtable]$Vars)
    if (-not (Test-Path $BinDir)) { New-Item -ItemType Directory -Path $BinDir | Out-Null }
    $exe = Join-Path $BinDir 'kognitionctl.exe'
    Write-Host "go build -> $exe"
    Invoke-Native 'go' @('build', '-o', $exe, './cmd/kognitionctl')

    $mode = if ($Vars.ContainsKey('MODE')) { $Vars['MODE'] } else { 'kubernetes' }
    $studioArgs = @('studio', '--mode', $mode, '--namespace', $AgentNamespace, '--open')
    Write-Host "$exe $($studioArgs -join ' ')"
    & $exe @studioArgs
    if ($LASTEXITCODE -ne 0) { throw "studio exited with code $LASTEXITCODE" }
}

function Target-E2E {
    & pwsh -NoProfile -File (Join-Path $RepoRoot 'e2e/e2e-test.ps1')
    if ($LASTEXITCODE -ne 0) { throw "e2e tests failed with code $LASTEXITCODE" }
}

function Target-E2EKubernetesMemory {
    & pwsh -NoProfile -File (Join-Path $RepoRoot 'e2e/kubernetes-memory-boundary-e2e.ps1') -ControllerNamespace $Namespace -ReleaseName $Release
    if ($LASTEXITCODE -ne 0) { throw "Kubernetes memory boundary E2E failed with code $LASTEXITCODE" }
}

function Target-E2EKubernetesAudit {
    & pwsh -NoProfile -File (Join-Path $RepoRoot 'e2e/kubernetes-audit-e2e.ps1') -ControllerNamespace $Namespace -ReleaseName $Release
    if ($LASTEXITCODE -ne 0) { throw "Kubernetes audit E2E failed with code $LASTEXITCODE" }
}

function Target-E2EKubernetesTool {
    & pwsh -NoProfile -File (Join-Path $RepoRoot 'e2e/kubernetes-tool-gateway-e2e.ps1') -ControllerNamespace $Namespace -ReleaseName $Release
    if ($LASTEXITCODE -ne 0) { throw "Kubernetes Tool Gateway E2E failed with code $LASTEXITCODE" }
}

function Target-E2EKubernetesToolResilience {
    & pwsh -NoProfile -File (Join-Path $RepoRoot 'e2e/kubernetes-tool-gateway-resilience-e2e.ps1') -ControllerNamespace $Namespace -ReleaseName $Release
    if ($LASTEXITCODE -ne 0) { throw "Kubernetes Tool Gateway resilience E2E failed with code $LASTEXITCODE" }
}

function Target-E2EKubernetesToolMonitoring {
    Target-ObservabilityInstall
    & pwsh -NoProfile -File (Join-Path $RepoRoot 'e2e/kubernetes-tool-gateway-monitoring-e2e.ps1') -ControllerNamespace $Namespace -ObservabilityNamespace $ObservabilityNamespace -ReleaseName $Release
    if ($LASTEXITCODE -ne 0) { throw "Kubernetes Tool Gateway monitoring E2E failed with code $LASTEXITCODE" }
}

function Target-E2EKubernetesRuntimeTool {
    & pwsh -NoProfile -File (Join-Path $RepoRoot 'e2e/kubernetes-runtime-tool-gateway-e2e.ps1') -ControllerNamespace $Namespace -ReleaseName $Release
    if ($LASTEXITCODE -ne 0) { throw "Kubernetes runtime Tool Gateway E2E failed with code $LASTEXITCODE" }
}

function Target-E2EKubernetesToolNetworkPolicy {
    & pwsh -NoProfile -File (Join-Path $RepoRoot 'e2e/kubernetes-tool-gateway-network-policy-e2e.ps1') -ControllerNamespace $Namespace -ReleaseName $Release
    if ($LASTEXITCODE -ne 0) { throw "Kubernetes Tool Gateway network-policy E2E failed with code $LASTEXITCODE" }
}

function Target-E2EKubernetesToolExecProbe {
    & pwsh -NoProfile -File (Join-Path $RepoRoot 'e2e/kubernetes-tool-exec-probe-e2e.ps1') -ControllerNamespace $Namespace -ReleaseName $Release
    if ($LASTEXITCODE -ne 0) { throw "Kubernetes Tool exec probe E2E failed with code $LASTEXITCODE" }
}

function Target-E2EKubernetesModel {
    & pwsh -NoProfile -File (Join-Path $RepoRoot 'e2e/kubernetes-model-gateway-e2e.ps1') -ControllerNamespace $Namespace -ReleaseName $Release
    if ($LASTEXITCODE -ne 0) { throw "Kubernetes Model Gateway E2E failed with code $LASTEXITCODE" }
}

function Target-E2EKubernetesCapabilityRotation {
    & pwsh -NoProfile -File (Join-Path $RepoRoot 'e2e/kubernetes-capability-key-rotation-e2e.ps1') -ControllerNamespace $Namespace -ReleaseName $Release
    if ($LASTEXITCODE -ne 0) { throw "Kubernetes capability key rotation E2E failed with code $LASTEXITCODE" }
}

function Target-E2EKubernetesAgentCommunication {
    & pwsh -NoProfile -File (Join-Path $RepoRoot 'e2e/kubernetes-agent-communication-gateway-e2e.ps1') -ControllerNamespace $Namespace -ReleaseName $Release
    if ($LASTEXITCODE -ne 0) { throw "Kubernetes Agent Communication Gateway E2E failed with code $LASTEXITCODE" }
}

function Target-Help {
        @(
                'Usage: .\make.ps1 <target> [KEY=VALUE ...]',
                '',
                'Targets:',
                '  all                  Build all binaries (default)',
                '  build                Build all binaries to bin\',
                '  clean                Remove build artifacts (bin\, proto/gen\)',
                '  tidy                 go mod tidy',
                '  test                 Go race/coverage tests (3m/package, 30m total; requires Python 3)',
                '  integration          Run deterministic public API and controller integration tests',
                '  vet                  go vet ./...',
                '  fmt                  Verify gofmt formatting',
                '  lint                 vet + fmt',
                '  quality              Run the full Go quality policy',
                '  quality-deep         Report complexity and duplication debt',
                '  proto                buf generate',
                '  proto-lint           buf lint',
                '  docker               Build all Docker images',
                '  docker-k8s           Build only Kubernetes-mode images',
                '  docker-standalone    Build only standalone-mode images',
                '  refresh-k8s-controller Rebuild, load, and restart the Kubernetes controller',
                '  up                   docker compose up -d',
                '  down                 docker compose down',
                '  deploy-standalone    Deploy standalone mode via Helm',
                '  deploy-kubernetes    Checked fresh install; INSTALL_PLAN selects the existing-release workflow',
                '  undeploy-standalone  Remove standalone Helm release',
                '  undeploy-kubernetes  Remove Kubernetes mode Helm release',
                '  install              Install the platform (MODE=standalone|kubernetes)',
                '  uninstall            Uninstall everything (release, CRDs, namespace)',
                '  fresh-kubernetes-demo  Legacy alias for a checked new Kubernetes installation; no reset',
                '  observability         Alias for observability-install',
                '  observability-install Install Jaeger, Prometheus, Grafana, and Kognition monitoring integrations',
                '  observability-uninstall Remove the development observability bundle and disable integrations',
                '  jaeger               Alias for jaeger-install',
                '  jaeger-install       Backward-compatible alias for observability-install',
                '  jaeger-ui            Port-forward Jaeger UI to localhost:16686',
                '  grafana-ui           Port-forward Grafana UI to localhost:3000',
                '  jaeger-uninstall     Backward-compatible alias for observability-uninstall',
                '  quickstart           Install a new Kubernetes release and start its controller after checks',
                '  foundry-creds        Regenerate foundry-creds.yaml from .env and apply it (AGENT_NAMESPACE=ns1,ns2)',
                '  samples-list         List available samples',
                '  samples-install      Install samples (SAMPLE=<name> for one)',
                '  samples-full-install Install and validate every compatible self-contained sample',
                '  samples-provision-azure Provision local-test Azure Blob storage and write ignored .env settings',
                '  samples-uninstall    Uninstall samples (SAMPLE=<name> for one)',
                '  status               Show platform and agent status',
                '  logs                 Tail platform logs (COMPONENT=<name>)',
                '  port-forward         Port-forward apiserver to localhost:8080',
                '  studio               Rebuild kognitionctl and launch Studio (MODE=standalone|kubernetes)',
                '  e2e                  Run end-to-end tests',
                '  e2e-kubernetes-memory Verify deployed memory gateway and raw-service authorization boundaries',
                '  e2e-kubernetes-audit Verify durable Kubernetes mutation and memory write audit records',
                '  e2e-kubernetes-tool   Verify Tool Gateway capability exchange, invocation, ledger, and policy revocation',
                '  e2e-kubernetes-tool-resilience Verify Tool Gateway timeout, retry, concurrency, circuit, and metrics behavior',
                '  e2e-kubernetes-runtime-tool Verify model-selected Tool execution through the runtime and Tool Gateway',
                '  e2e-kubernetes-tool-network-policy Verify Tool Gateway private egress with an enforcing CNI',
                '  e2e-kubernetes-tool-exec-probe Verify class-approved Tool exec health probes in Kubernetes',
                '  e2e-kubernetes-model  Verify Model Gateway authorization, runtime redaction, and provider credential isolation',
                '  e2e-kubernetes-capability-rotation Verify active and previous capability-key overlap during Tool Gateway rotation',
				'  e2e-kubernetes-agent-communication Verify cross-workspace AgentService delegation, replay safety, and live policy revocation',
                '  e2e-kubernetes-tool-monitoring Generate gateway traffic and verify Prometheus, alert rules, and Grafana dashboard panels',
                '  migrate              Apply SQL migrations against local Postgres',
                '  services             Run services.ps1 (ACTION=<...> [SERVICE=<...>])',
                '  install-hooks        Install git hooks from .githooks',
                '  help                 Show this help',
                '',
                'Variables:',
                '  NAMESPACE=<ns>            Helm namespace (default: kognition-system)',
                '  AGENT_NAMESPACE=<ns>      Agent samples namespace (default: default)',
                '  AGENT_IMAGE=<image>       Agent runtime image for Kubernetes mode',
                '  RELEASE=<name>            Helm release name (default: kognition)',
                '  OBSERVABILITY_NAMESPACE=<ns>  Development observability namespace (default: kognition-observability)',
                '  PROMETHEUS_STACK_VERSION=<version> Pinned kube-prometheus-stack chart version (default: 88.5.2)',
                '  JAEGER_OTLP_ENDPOINT=<host:port>  OTLP endpoint for Kognition pods',
                '  FRESH_IMAGE_TAG=<tag>      Use prebuilt, uniquely tagged Kubernetes images',
                '  MODE=<mode>               install mode: standalone|kubernetes (default: standalone)',
                '  SAMPLE=<name>             Sample to install/uninstall',
                '  COMPONENT=<name>          Component to filter logs',
                '  ACTION=<action>           Used by ''services'' target',
                '  SERVICE=<name>            Used by ''services'' target'
        ) -join [Environment]::NewLine | Write-Host
}

# ----------------------------------------------------------------------------
# Dispatch
# ----------------------------------------------------------------------------
$parsed = Parse-Args -Tokens $Args

# Helm / Kubernetes variables (overridable via KEY=VALUE args)
$Namespace      = if ($parsed.Vars.ContainsKey('NAMESPACE'))       { $parsed.Vars['NAMESPACE'] }       else { 'kognition-system' }
$AgentNamespace = if ($parsed.Vars.ContainsKey('AGENT_NAMESPACE')) { $parsed.Vars['AGENT_NAMESPACE'] } else { 'default' }
$AgentImageOverride = if ($parsed.Vars.ContainsKey('AGENT_IMAGE')) { $parsed.Vars['AGENT_IMAGE'] } else { '' }
$FreshKubernetesDemoImageTag = if ($parsed.Vars.ContainsKey('FRESH_IMAGE_TAG')) { $parsed.Vars['FRESH_IMAGE_TAG'] } else { '' }
$ObservabilityNamespace = if ($parsed.Vars.ContainsKey('OBSERVABILITY_NAMESPACE')) { $parsed.Vars['OBSERVABILITY_NAMESPACE'] } else { 'kognition-observability' }
$PrometheusStackVersion = if ($parsed.Vars.ContainsKey('PROMETHEUS_STACK_VERSION')) { $parsed.Vars['PROMETHEUS_STACK_VERSION'] } else { '88.5.2' }
$Release        = if ($parsed.Vars.ContainsKey('RELEASE'))         { $parsed.Vars['RELEASE'] }         else { 'kognition' }
$Chart          = Join-Path $RepoRoot 'deploy/helm/kognition'
$JaegerOTLPEndpoint = if ($parsed.Vars.ContainsKey('JAEGER_OTLP_ENDPOINT')) { $parsed.Vars['JAEGER_OTLP_ENDPOINT'] } else { "jaeger-collector.$ObservabilityNamespace.svc.cluster.local:4317" }

if (-not $parsed.Target) {
    Target-Help
    exit 0
}

switch ($parsed.Target) {
    'all'                  { Target-Build }
    'build'                { Target-Build }
    'clean'                { Target-Clean }
    'tidy'                 { Target-Tidy }
    'test'                 { Target-Test }
    'integration'          { Target-Integration }
    'vet'                  { Target-Vet }
    'fmt'                  { Target-Fmt }
    'lint'                 { Target-Lint }
    'quality'              { Target-Quality }
    'quality-deep'         { Target-QualityDeep }
    'proto'                { Target-Proto }
    'proto-lint'           { Target-ProtoLint }
    'docker'               { Target-Docker }
    'docker-k8s'           { Target-DockerK8s }
    'docker-standalone'    { Target-DockerStandalone }
    'refresh-k8s-controller' { Target-RefreshK8sController }
    'up'                   { Target-Up }
    'down'                 { Target-Down }
    'deploy-standalone'    { Target-DeployStandalone }
    'deploy-kubernetes'    { Target-DeployKubernetes }
    'undeploy-standalone'  { Target-UndeployStandalone }
    'undeploy-kubernetes'  { Target-UndeployKubernetes }
    'install'              { Target-Install -Vars $parsed.Vars }
    'uninstall'            { Target-Uninstall }
    'fresh-kubernetes-demo' { Target-FreshKubernetesDemo }
    'observability'        { Target-ObservabilityInstall }
    'observability-install' { Target-ObservabilityInstall }
    'observability-uninstall' { Target-ObservabilityUninstall }
    'jaeger'               { Target-ObservabilityInstall }
    'jaeger-install'       { Target-ObservabilityInstall }
    'jaeger-ui'            { Target-JaegerUI }
    'grafana-ui'           { Target-GrafanaUI }
    'jaeger-uninstall'     { Target-ObservabilityUninstall }
    'foundry-creds'        { Target-FoundryCreds -WriteOnly:($parsed.Vars['APPLY'] -eq 'false') }
    'samples-list'         { Target-SamplesList }
    'samples-install'      { Target-SamplesInstall -Vars $parsed.Vars }
    'samples-full-install' { Target-SamplesInstall -Vars @{} }
    'samples-provision-azure' { Target-SamplesProvisionAzure }
    'samples-uninstall'    { Target-SamplesUninstall -Vars $parsed.Vars }
    'status'               { Target-Status }
    'logs'                 { Target-Logs -Vars $parsed.Vars }
    'port-forward'         { Target-PortForward }
    'studio'               { Target-Studio -Vars $parsed.Vars }
    'quickstart'           { Target-FreshKubernetes }
    'e2e'                  { Target-E2E }
    'e2e-kubernetes-memory' { Target-E2EKubernetesMemory }
    'e2e-kubernetes-audit'  { Target-E2EKubernetesAudit }
    'e2e-kubernetes-tool'   { Target-E2EKubernetesTool }
    'e2e-kubernetes-tool-resilience' { Target-E2EKubernetesToolResilience }
    'e2e-kubernetes-tool-monitoring' { Target-E2EKubernetesToolMonitoring }
    'e2e-kubernetes-runtime-tool' { Target-E2EKubernetesRuntimeTool }
    'e2e-kubernetes-tool-network-policy' { Target-E2EKubernetesToolNetworkPolicy }
    'e2e-kubernetes-tool-exec-probe' { Target-E2EKubernetesToolExecProbe }
    'e2e-kubernetes-model' { Target-E2EKubernetesModel }
    'e2e-kubernetes-capability-rotation' { Target-E2EKubernetesCapabilityRotation }
	'e2e-kubernetes-agent-communication' { Target-E2EKubernetesAgentCommunication }
    'migrate'              { Target-Migrate }
    'services'             { Target-Services -Vars $parsed.Vars }
    'install-hooks'        { Target-InstallHooks }
    'help'                 { Target-Help }
    default {
        Write-Error "Unknown target '$($parsed.Target)'. Run '.\make.ps1 help' for the list."
        exit 1
    }
}

exit 0
