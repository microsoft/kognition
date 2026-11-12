# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

[CmdletBinding()]
param(
    [ValidateSet('Create','Check','Typecheck','Leases','Runtime','ProductionTool','Production','Snapshot','Cleanup')][string]$Action = 'Check',
    [Parameter(Mandatory)][string]$Directory,
    [string]$NodeImage = 'kindest/node:v1.31.1',
    [ValidatePattern('^v[0-9]+\.[0-9]+\.[0-9]+$')][string]$ExpectedVersion = 'v1.31.1',
    [switch]$ReviewKnownArgumentFailure,
    [switch]$ReviewKnownCRDFailure,
    [switch]$IncludeGatewayHTTP,
    [switch]$ReviewVerifiedArtifactPreflight
)

$ErrorActionPreference = 'Stop'
$Directory = [IO.Path]::GetFullPath($Directory)
$recordPath = Join-Path $Directory 'authority.json'
$kubeconfig = Join-Path $Directory 'kubeconfig'

function Invoke-AuthorityCommand {
    param([string]$Program,[string[]]$Arguments)
    $output = & $Program @Arguments 2>&1
    if ($LASTEXITCODE -ne 0) { throw "$Program failed: $($output -join [Environment]::NewLine)" }
    return ($output -join [Environment]::NewLine)
}

function Get-AuthorityTargetName {
    param([guid]$RunID)
    if ($RunID -eq [guid]::Empty) { throw 'Authority target requires a nonempty run ID' }
    return "kog-auth-$($RunID.ToString('N'))"
}

function Get-OwnedAuthorityTarget {
    $record = Get-Content -Raw -LiteralPath $recordPath | ConvertFrom-Json
    $parsed = [guid]::Empty
    if (-not [guid]::TryParseExact($record.runID,'D',[ref]$parsed) -or $parsed -eq [guid]::Empty -or
        $record.name -ne (Get-AuthorityTargetName $parsed) -or $record.kubeconfig -ne $kubeconfig -or $record.cleanedUTC) {
        throw 'An active recorded authority target is required'
    }
    if ($record.expectedVersion -notmatch '^v[0-9]+\.[0-9]+\.[0-9]+$') { throw 'Authority target requires a recorded expected Kubernetes version' }
    $configuration = Invoke-AuthorityCommand kubectl @('--kubeconfig',$kubeconfig,'config','view','--minify','-o','json') | ConvertFrom-Json
    if ($configuration.clusters.Count -ne 1 -or $configuration.clusters[0].cluster.server -notmatch '^https://127\.0\.0\.1:[0-9]+$') { throw 'Authority target must use one explicit loopback API endpoint' }
    $nodes = Invoke-AuthorityCommand kubectl @('--kubeconfig',$kubeconfig,'--request-timeout=15s','get','nodes','-o','json') | ConvertFrom-Json
    $root = Invoke-AuthorityCommand kubectl @('--kubeconfig',$kubeconfig,'--request-timeout=15s','get','namespace','kube-system','-o','json') | ConvertFrom-Json
    if ($nodes.items.Count -ne 1 -or $nodes.items[0].metadata.name -ne "$($record.name)-control-plane" -or
        $nodes.items[0].metadata.labels.'kognition.dev/authority-acceptance-run' -ne $record.runID -or $root.metadata.uid -ne $record.clusterUID -or
        $nodes.items[0].status.nodeInfo.kubeletVersion -ne $record.expectedVersion) {
        throw 'Authority target ownership changed'
    }
    return $record
}

function Import-AuthorityImages {
    param([hashtable[]]$Images)
    $owner = Get-OwnedAuthorityTarget
    if ($owner.state -ne 'typecheck-passed') { throw 'Image import requires an unexecuted typechecked target' }
    $node = "$($owner.name)-control-plane"
    $archive = Join-Path $Directory 'production-images.tar'
    $destination = '/var/authority-production-images.tar'
    foreach ($image in $Images) {
        $local = @(Invoke-AuthorityCommand docker @('--context','desktop-linux','image','inspect',$image.name) | ConvertFrom-Json)
        if ($local.Count -ne 1 -or $local[0].Os -ne 'linux' -or $local[0].Architecture -ne 'amd64' -or $local[0].Descriptor.digest -notmatch '^sha256:[0-9a-f]{64}$') {
            throw 'Production image requires an exact Linux/amd64 artifact descriptor'
        }
        $image.sha256 = $local[0].Descriptor.digest
    }
    $null = Invoke-AuthorityCommand python (@($bounded,'--timeout','120','--','docker','--context','desktop-linux','save','--output',$archive) + @($Images | ForEach-Object { $_.name }))
    $hash = (Get-FileHash -LiteralPath $archive -Algorithm SHA256).Hash.ToLowerInvariant()
    $null = Invoke-AuthorityCommand python @($bounded,'--timeout','120','--','docker','--context','desktop-linux','cp',$archive,"${node}:$destination")
    $copied = Invoke-AuthorityCommand docker @('--context','desktop-linux','exec',$node,'sha256sum',$destination)
    if ($copied -notmatch "^$hash\s+$([regex]::Escape($destination))\s*$") { throw 'Copied production image archive hash mismatch' }
    $null = Invoke-AuthorityCommand python @($bounded,'--timeout','180','--','docker','--context','desktop-linux','exec',$node,'ctr','--namespace=k8s.io','images','import','--platform','linux/amd64','--digests',$destination)
    foreach ($image in $Images) {
        $reference = "docker.io/$($image.name)"
        $checked = Invoke-AuthorityCommand docker @('--context','desktop-linux','exec',$node,'ctr','--namespace=k8s.io','images','check',"name==$reference")
        if ($checked -notmatch "(?m)^$([regex]::Escape($reference))\s+\S+\s+$([regex]::Escape($image.sha256))\s+complete\s+\([0-9]+/[0-9]+\)[^\r\n]*\s+true\s*$") {
            throw "Imported production image digest, content completeness or unpacking mismatch: $reference"
        }
        $loaded = Invoke-AuthorityCommand docker @('--context','desktop-linux','exec',$node,'crictl','inspecti','-o','json',$image.name) | ConvertFrom-Json
        if ($loaded.status.id -notmatch '^sha256:[0-9a-f]{64}$' -or $loaded.status.repoTags -notcontains $reference) { throw "CRI cannot resolve the imported production image: $reference" }
        $image.criImageID = $loaded.status.id
        $image.archiveSHA256 = $hash
    }
    $null = Invoke-AuthorityCommand docker @('--context','desktop-linux','exec',$node,'rm','--',$destination)
    Remove-Item -LiteralPath $archive
}

function Remove-OwnedAuthorityCluster {
    $target = Get-OwnedAuthorityTarget
    $cleanupConfig = Join-Path $Directory 'cleanup-kubeconfig'
    if (Test-Path -LiteralPath $cleanupConfig) { throw 'A prior cleanup configuration requires review before another attempt' }
    $retainedHash = (Get-FileHash -LiteralPath $kubeconfig -Algorithm SHA256).Hash
    Copy-Item -LiteralPath $kubeconfig -Destination $cleanupConfig -ErrorAction Stop
    if ((Get-FileHash -LiteralPath $cleanupConfig -Algorithm SHA256).Hash -ne $retainedHash) { throw 'Cleanup kubeconfig copy hash mismatch' }
    $priorDockerContext = $env:DOCKER_CONTEXT
    try {
        $env:DOCKER_CONTEXT = 'desktop-linux'
        $null = Invoke-AuthorityCommand kind @('delete','cluster','--name',$target.name,'--kubeconfig',$cleanupConfig)
        if (@((Invoke-AuthorityCommand kind @('get','clusters')) -split '\r?\n') -contains $target.name) { throw 'Owned authority target remains after cleanup' }
    } finally { $env:DOCKER_CONTEXT = $priorDockerContext }
    if ((Get-FileHash -LiteralPath $kubeconfig -Algorithm SHA256).Hash -ne $retainedHash) { throw 'Sealed kubeconfig changed during cleanup' }
    Remove-Item -LiteralPath $cleanupConfig
}

if ($Action -eq 'Create') {
    if (Test-Path -LiteralPath $Directory) { throw 'Authority creation requires a new directory; prior targets cannot be reused' }
    if ($NodeImage -notmatch "^kindest/node:$([regex]::Escape($ExpectedVersion))(@sha256:[0-9a-f]{64})?$") { throw 'Node image must match the explicitly expected Kubernetes version' }
    foreach ($program in @('kind','docker','kubectl','helm','go','python')) { $null = Get-Command $program -ErrorAction Stop }
    $runID = [guid]::NewGuid().ToString('D')
    $name = Get-AuthorityTargetName ([guid]$runID)
    if ("$name-control-plane".Length -gt 64) { throw 'Authority node hostname exceeds the Linux limit' }
    $null = New-Item -ItemType Directory -Path $Directory
    $configuration = @{
        kind='Cluster';apiVersion='kind.x-k8s.io/v1alpha4';name=$name
        networking=@{apiServerAddress='127.0.0.1'}
        nodes=@(@{role='control-plane';labels=@{'kognition.dev/authority-acceptance-run'=$runID}})
    }
    $configuration | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath (Join-Path $Directory 'kind.json') -Encoding utf8NoBOM
    @{runID=$runID;name=$name;kubeconfig=$kubeconfig;nodeImage=$NodeImage;expectedVersion=$ExpectedVersion;createdUTC=[DateTime]::UtcNow.ToString('o');state='creating'} |
        ConvertTo-Json | Set-Content -LiteralPath $recordPath -Encoding utf8NoBOM
    $creation=& python (Join-Path (Split-Path -Parent $PSScriptRoot) 'tools/run_bounded.py') --timeout 300 -- kind create cluster --retain --name $name --image $NodeImage --config (Join-Path $Directory 'kind.json') --kubeconfig $kubeconfig --wait 120s 2>&1
    $creationExit=$LASTEXITCODE
    $creation | Set-Content -LiteralPath (Join-Path $Directory 'create.log') -Encoding utf8NoBOM
    Write-Output $creation
    if($creationExit -ne 0){throw 'Owned authority creation failed or timed out; retain its record and inspect it before any continuation'}
    $root = Invoke-AuthorityCommand kubectl @('--kubeconfig',$kubeconfig,'--request-timeout=15s','get','namespace','kube-system','-o','json') | ConvertFrom-Json
    $record = Get-Content -Raw -LiteralPath $recordPath | ConvertFrom-Json
    $record | Add-Member -NotePropertyName clusterUID -NotePropertyValue $root.metadata.uid
    $record.state='created'
    $record | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $recordPath -Encoding utf8NoBOM
}

$record = Get-OwnedAuthorityTarget
if ($Action -in @('ProductionTool','Production')) {
    if ($record.state -ne 'typecheck-passed') { throw 'Production acceptance requires a fresh typechecked target without prior workflows' }
    $fullProduction = $Action -eq 'Production'
    function Get-AuthorityCandidateInventory {
        $root = Split-Path -Parent $PSScriptRoot
        $paths = @((Invoke-AuthorityCommand git @('ls-files','--cached','--others','--exclude-standard','--','*.go','go.mod','go.sum')) -split '\r?\n' | Where-Object { $_ })
        $paths += @('e2e\gateway-authority-acceptance.ps1','e2e\gateway-authority-acceptance-test.ps1','e2e\fixtures\Dockerfile.gateway-authority')
        $paths += @(Get-ChildItem -LiteralPath (Join-Path $root 'deploy\helm\kognition') -Recurse -File | ForEach-Object { [IO.Path]::GetRelativePath($root,$_.FullName) })
        foreach ($path in ($paths | ForEach-Object { $_.Replace('/',[IO.Path]::DirectorySeparatorChar) } | Sort-Object -Unique)) {
            @{path=$path;sha256=(Get-FileHash -LiteralPath (Join-Path $root $path) -Algorithm SHA256).Hash.ToLowerInvariant()}
        }
    }
    $candidate = @(Get-AuthorityCandidateInventory)
    $candidate | ConvertTo-Json -Depth 5 | Set-Content -LiteralPath (Join-Path $Directory 'production-source-inventory.json') -Encoding utf8NoBOM
    $build = Join-Path $Directory 'production-build'
    if (Test-Path -LiteralPath $build) { throw 'Production artifacts already exist; review the prior preflight instead of silently replacing it' }
    $null = New-Item -ItemType Directory -Path $build
    $binaryPath = Join-Path $build 'gateway-authority-production.test'
    $oldGOOS,$oldGOARCH,$oldCGO = $env:GOOS,$env:GOARCH,$env:CGO_ENABLED
    try {
        $env:GOOS='linux'
        $env:GOARCH='amd64'
        $env:CGO_ENABLED='0'
        $null = Invoke-AuthorityCommand go @('build','-trimpath','-o',(Join-Path $build 'kognition-k8s-controller'),'.\cmd\kognition-k8s-controller')
        $null = Invoke-AuthorityCommand go @('build','-trimpath','-o',(Join-Path $build 'kognition-tool-gateway'),'.\cmd\kognition-tool-gateway')
        if ($fullProduction) {
            $null = Invoke-AuthorityCommand go @('build','-trimpath','-o',(Join-Path $build 'kognition-memory-policy-gateway'),'.\cmd\kognition-memory-policy-gateway')
            $null = Invoke-AuthorityCommand go @('build','-trimpath','-o',(Join-Path $build 'kognition-agent-communication-gateway'),'.\cmd\kognition-agent-communication-gateway')
        }
        $null = Invoke-AuthorityCommand go @('test','-tags=integration','-trimpath','-c','-o',$binaryPath,'.\internal\traceability')
    } finally {
        $env:GOOS=$oldGOOS
        $env:GOARCH=$oldGOARCH
        $env:CGO_ENABLED=$oldCGO
    }
    $bounded = Join-Path (Split-Path -Parent $PSScriptRoot) 'tools\run_bounded.py'
    $dockerfile = Join-Path $PSScriptRoot 'fixtures\Dockerfile.gateway-authority'
    $images = @(
        @{name="local/kognition-authority-controller:$($record.runID)";binary='kognition-k8s-controller'},
        @{name="local/kognition-authority-tool:$($record.runID)";binary='kognition-tool-gateway'},
        @{name="local/kognition-authority-provider:$($record.runID)";binary='gateway-authority-production.test'}
    )
    if ($fullProduction) {
        $images += @(
            @{name="local/kognition-authority-memory:$($record.runID)";binary='kognition-memory-policy-gateway'},
            @{name="local/kognition-authority-communication:$($record.runID)";binary='kognition-agent-communication-gateway'}
        )
    }
    $priorDockerContext = $env:DOCKER_CONTEXT
    try {
        $env:DOCKER_CONTEXT='desktop-linux'
        foreach ($image in $images) {
            $buildLog = Invoke-AuthorityCommand python @($bounded,'--timeout','240','--','docker','--context','desktop-linux','build','--network','none','--pull=false','--file',$dockerfile,'--build-arg',"BINARY=$($image.binary)",'--tag',$image.name,$build)
            $buildLog | Set-Content -LiteralPath (Join-Path $Directory "$($image.binary)-image.log") -Encoding utf8NoBOM
            $image.binarySHA256 = (Get-FileHash -LiteralPath (Join-Path $build $image.binary) -Algorithm SHA256).Hash.ToLowerInvariant()
        }
        if ($fullProduction) {
            $database = @{name="local/kognition-authority-postgres:$($record.runID)";source='docker.io/pgvector/pgvector@sha256:ccc6e83d6e35e931dc7c5def2022729d5a6c370318d099181995567ff1fb4d6b'}
            $null = Invoke-AuthorityCommand docker @('--context','desktop-linux','image','inspect',$database.source)
            $null = Invoke-AuthorityCommand docker @('--context','desktop-linux','tag',$database.source,$database.name)
            $images += $database
        }
        Import-AuthorityImages $images
        if ($fullProduction -and $database.sha256 -ne ($database.source -split '@')[1]) { throw 'Imported database artifact does not match the chart-pinned digest' }
    } finally {
        $env:DOCKER_CONTEXT=$priorDockerContext
    }
    $images | ConvertTo-Json -Depth 5 | Set-Content -LiteralPath (Join-Path $Directory 'production-images.json') -Encoding utf8NoBOM
    $manifestPath = Join-Path $Directory 'production-chart.yaml'
    $renderArguments = @(
        'template','security-test','deploy\helm\kognition','--namespace','control','--include-crds','--kube-version',$record.expectedVersion.TrimStart('v'),
        '--set','mode=kubernetes','--set','k8sController.enabled=true','--set','k8sController.managedNamespaces[0]=tenant','--set','k8sController.agentNamespace=tenant',
        '--set','runtimeAuth.existingSecret=authority-runtime-auth','--set','webhook.certManager.enabled=false','--set','webhook.failurePolicy=Fail',
        '--set','toolGateway.enabled=true','--set','toolGateway.replicaCount=2',
        '--set','memoryPolicyGateway.enabled=true','--set','agentCommunicationGateway.enabled=true','--set','modelGateway.enabled=true',
        '--set','k8sController.image.repository=local/kognition-authority-controller','--set',"k8sController.image.tag=$($record.runID)",'--set','k8sController.image.pullPolicy=Never',
        '--set','toolGateway.image.repository=local/kognition-authority-tool','--set',"toolGateway.image.tag=$($record.runID)",'--set','toolGateway.image.pullPolicy=Never'
    )
    if ($fullProduction) {
        $renderArguments += @(
            '--set','memoryPolicyGateway.replicaCount=2','--set','agentCommunicationGateway.replicaCount=2',
            '--set','memoryPolicyGateway.image.repository=local/kognition-authority-memory','--set',"memoryPolicyGateway.image.tag=$($record.runID)",'--set','memoryPolicyGateway.image.pullPolicy=Never',
            '--set','agentCommunicationGateway.image.repository=local/kognition-authority-communication','--set',"agentCommunicationGateway.image.tag=$($record.runID)",'--set','agentCommunicationGateway.image.pullPolicy=Never'
        )
    }
    $manifest = Invoke-AuthorityCommand helm $renderArguments
    $manifest | Set-Content -LiteralPath $manifestPath -Encoding utf8NoBOM
    $inventoryPath = Join-Path $Directory 'authority-crd-inventory.json'
    $definitions = @(Get-ChildItem -LiteralPath 'deploy\helm\kognition\crds' -File -Filter '*.yaml' | ForEach-Object {
        if ($_.Name -notmatch '^kognition\.dev_([a-z]+)\.yaml$') { throw "Unexpected chart CRD filename: $($_.Name)" }
        "$($Matches[1]).kognition.dev"
    } | Sort-Object)
    $definitions | ConvertTo-Json | Set-Content -LiteralPath $inventoryPath -Encoding utf8NoBOM
    $node = "$($record.name)-control-plane"
    foreach ($artifact in @(
        @{path=$binaryPath;destination='/var/gateway-authority-production.test'},
        @{path=$manifestPath;destination='/var/production-chart.yaml'},
        @{path=$inventoryPath;destination='/var/authority-crd-inventory.json'}
    )) {
        $null = Invoke-AuthorityCommand python @($bounded,'--timeout','60','--','docker','--context','desktop-linux','cp',$artifact.path,"${node}:$($artifact.destination)")
        $copied = Invoke-AuthorityCommand docker @('--context','desktop-linux','exec',$node,'sha256sum',$artifact.destination)
        $expected = (Get-FileHash -LiteralPath $artifact.path -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($copied -notmatch "^$expected\s") { throw 'Copied production acceptance artifact hash mismatch' }
    }
    $current = @(Get-AuthorityCandidateInventory)
    if (Compare-Object @($candidate | ForEach-Object { "$($_.path):$($_.sha256)" }) @($current | ForEach-Object { "$($_.path):$($_.sha256)" })) {
        throw 'Candidate changed during production preflight; no workflows may start'
    }
    $record = Get-OwnedAuthorityTarget
    if ($record.state -ne 'typecheck-passed') { throw 'Authority target state changed during production preflight' }
    $record.state = if ($fullProduction) { 'production-started' } else { 'production-tool-started' }
    $record | Add-Member -NotePropertyName productionSourceSHA256 -NotePropertyValue ((Get-FileHash -LiteralPath (Join-Path $Directory 'production-source-inventory.json') -Algorithm SHA256).Hash.ToLowerInvariant())
    $record | Add-Member -NotePropertyName productionManifestSHA256 -NotePropertyValue ((Get-FileHash -LiteralPath $manifestPath -Algorithm SHA256).Hash.ToLowerInvariant())
    $record | Add-Member -NotePropertyName productionBinarySHA256 -NotePropertyValue ((Get-FileHash -LiteralPath $binaryPath -Algorithm SHA256).Hash.ToLowerInvariant())
    $record | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $recordPath -Encoding utf8NoBOM
    $testName = if ($fullProduction) { 'TestGatewayAuthorityProductionExecution' } else { 'TestGatewayAuthorityProductionToolExecution' }
    $logName = if ($fullProduction) { 'production.log' } else { 'production-tool.log' }
    $log = & python $bounded --timeout 900 -- docker --context desktop-linux exec -e "KOGNITION_AUTHORITY_RUN_ID=$($record.runID)" -e "KOGNITION_AUTHORITY_CLUSTER_UID=$($record.clusterUID)" -e "KOGNITION_ADMISSION_EXPECTED_VERSION=$($record.expectedVersion)" -e KOGNITION_AUTHORITY_KUBECONFIG=/etc/kubernetes/admin.conf -e KOGNITION_AUTHORITY_MANIFEST=/var/production-chart.yaml $node /var/gateway-authority-production.test "-test.run=^$testName`$" '-test.timeout=14m' '-test.v' 2>&1
    $exit = $LASTEXITCODE
    $log | Set-Content -LiteralPath (Join-Path $Directory $logName) -Encoding utf8NoBOM
    Write-Output $log
    $current = @(Get-AuthorityCandidateInventory)
    if (Compare-Object @($candidate | ForEach-Object { "$($_.path):$($_.sha256)" }) @($current | ForEach-Object { "$($_.path):$($_.sha256)" })) {
        throw 'Candidate changed during production execution; retain evidence without certifying this source'
    }
    if ($exit -ne 0 -or ($log -join "`n") -notmatch '(?m)^PASS\s*$' -or ($log -join "`n") -match '--- SKIP:') { throw 'Production acceptance did not pass; retain the exact target and evidence without replay' }
    if ($fullProduction) {
        foreach ($case in @('tool','memory-store','memory-delete','communication')) {
            if (($log -join "`n") -notmatch "--- PASS: $testName/$case ") { throw "Production $case workflow did not pass" }
        }
    }
    $record.state = if ($fullProduction) { 'production-passed' } else { 'production-tool-passed' }
    $receiptField = if ($fullProduction) { 'productionSHA256' } else { 'productionToolSHA256' }
    $record | Add-Member -NotePropertyName $receiptField -NotePropertyValue ((Get-FileHash -LiteralPath (Join-Path $Directory $logName) -Algorithm SHA256).Hash.ToLowerInvariant())
    $record | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $recordPath -Encoding utf8NoBOM
}
if ($Action -eq 'Typecheck') {
    if ($record.state -ne 'created') { throw 'Typechecking requires an untouched recorded target; implicit retries are refused' }
    $record.state='typecheck-started'
    $record | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $recordPath -Encoding utf8NoBOM
    $oldKubeconfig,$oldIsolated,$oldVersion=$env:KOGNITION_ADMISSION_KUBECONFIG,$env:KOGNITION_ADMISSION_ISOLATED,$env:KOGNITION_ADMISSION_EXPECTED_VERSION
    try {
        $env:KOGNITION_ADMISSION_KUBECONFIG=$kubeconfig
        $env:KOGNITION_ADMISSION_ISOLATED='true'
        $env:KOGNITION_ADMISSION_EXPECTED_VERSION=$record.expectedVersion
        $log=& go test -tags=integration ./internal/traceability -run '^TestControllerAdmissionPolicyTypeChecking$' -count=1 -timeout=8m -v 2>&1
        $exit=$LASTEXITCODE
        $log | Set-Content -LiteralPath (Join-Path $Directory 'typecheck.log') -Encoding utf8NoBOM
        Write-Output $log
        if ($exit -ne 0 -or ($log -join "`n") -notmatch '(?m)^PASS\s*$' -or ($log -join "`n") -match '--- SKIP:') { throw 'Owned controller-manager typechecking did not pass; retain evidence without implicit retry' }
        $record.state='typecheck-passed'
        $record | Add-Member -NotePropertyName typecheckSHA256 -NotePropertyValue ((Get-FileHash (Join-Path $Directory 'typecheck.log') -Algorithm SHA256).Hash.ToLowerInvariant())
        $record | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $recordPath -Encoding utf8NoBOM
    } finally {
        $env:KOGNITION_ADMISSION_KUBECONFIG=$oldKubeconfig
        $env:KOGNITION_ADMISSION_ISOLATED=$oldIsolated
        $env:KOGNITION_ADMISSION_EXPECTED_VERSION=$oldVersion
    }
}
if ($Action -eq 'Leases') {
    if ($ReviewKnownArgumentFailure) {
        $failedLog=Join-Path $Directory 'leases.log'
        $failure=Get-Content -Raw -LiteralPath $failedLog
        if ($record.state -ne 'leases-started' -or $failure -notmatch 'flag provided but not defined: -test' -or $failure -match '(?m)^=== RUN') { throw 'Only the retained pre-test argument failure can be reviewed for continuation' }
        $existing=Invoke-AuthorityCommand kubectl @('--kubeconfig',$kubeconfig,'--request-timeout=15s','get','namespaces','control','tenant','outside','--ignore-not-found','-o','json') | ConvertFrom-Json
        if ($existing.items.Count -ne 0) { throw 'Acceptance fixtures exist; continuation after an argument failure is refused' }
        Copy-Item -LiteralPath $failedLog -Destination (Join-Path $Directory 'leases-argument-failure.log') -ErrorAction Stop
        $record | Add-Member -NotePropertyName reviewedArgumentFailureSHA256 -NotePropertyValue ((Get-FileHash -LiteralPath $failedLog -Algorithm SHA256).Hash.ToLowerInvariant())
        $record.state='typecheck-passed'
        $record | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $recordPath -Encoding utf8NoBOM
    }
    if ($record.state -ne 'typecheck-passed') { throw 'Live Lease acceptance requires a fresh passed typecheck; implicit retries are refused' }
    $record.state='leases-started'
    $record | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $recordPath -Encoding utf8NoBOM
    $manifest=Invoke-AuthorityCommand helm @('template','security-test','deploy/helm/kognition','--namespace','control','--kube-version',$record.expectedVersion.TrimStart('v'),'--set','mode=kubernetes','--set','k8sController.enabled=true','--set','k8sController.managedNamespaces[0]=tenant','--set','toolGateway.enabled=true','--set','memoryPolicyGateway.enabled=true','--set','agentCommunicationGateway.enabled=true')
    $manifestPath=Join-Path $Directory 'authority-chart.yaml'
    $manifest | Set-Content -LiteralPath $manifestPath -Encoding utf8NoBOM
    $binaryPath=Join-Path $Directory 'gateway-authority.test'
    $oldGOOS,$oldGOARCH,$oldCGO=$env:GOOS,$env:GOARCH,$env:CGO_ENABLED
    try {
        $env:GOOS='linux'
        $env:GOARCH='amd64'
        $env:CGO_ENABLED='0'
        $null=Invoke-AuthorityCommand go @('test','-tags=integration','-c','-o',$binaryPath,'./internal/traceability')
    } finally {
        $env:GOOS=$oldGOOS
        $env:GOARCH=$oldGOARCH
        $env:CGO_ENABLED=$oldCGO
    }
    $node="$($record.name)-control-plane"
    $bounded=Join-Path (Split-Path -Parent $PSScriptRoot) 'tools/run_bounded.py'
    foreach ($artifact in @(@{path=$binaryPath;destination='/var/gateway-authority.test'},@{path=$manifestPath;destination='/var/authority-chart.yaml'})) {
        $null=Invoke-AuthorityCommand python @($bounded,'--timeout','60','--','docker','--context','desktop-linux','cp',$artifact.path,"${node}:$($artifact.destination)")
        $copied=Invoke-AuthorityCommand docker @('--context','desktop-linux','exec',$node,'sha256sum',$artifact.destination)
        $expected=(Get-FileHash -LiteralPath $artifact.path -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($copied -notmatch "^$expected\s") { throw 'Copied acceptance artifact hash mismatch' }
    }
    $log=& python $bounded --timeout 300 -- docker --context desktop-linux exec -e "KOGNITION_AUTHORITY_RUN_ID=$($record.runID)" -e "KOGNITION_AUTHORITY_CLUSTER_UID=$($record.clusterUID)" -e "KOGNITION_ADMISSION_EXPECTED_VERSION=$($record.expectedVersion)" -e KOGNITION_AUTHORITY_KUBECONFIG=/etc/kubernetes/admin.conf -e KOGNITION_AUTHORITY_MANIFEST=/var/authority-chart.yaml $node /var/gateway-authority.test '-test.run=^TestGatewayAuthorityBoundTokenLeaseAdmission$' '-test.timeout=4m' '-test.v' 2>&1
    $exit=$LASTEXITCODE
    $log | Set-Content -LiteralPath (Join-Path $Directory 'leases.log') -Encoding utf8NoBOM
    Write-Output $log
    if ($exit -ne 0 -or ($log -join "`n") -notmatch '(?m)^PASS\s*$' -or ($log -join "`n") -match '--- SKIP:') { throw 'Live gateway Lease acceptance did not pass; retain evidence without implicit retry' }
    $record.state='leases-passed'
    $record | Add-Member -NotePropertyName leasesSHA256 -NotePropertyValue ((Get-FileHash -LiteralPath (Join-Path $Directory 'leases.log') -Algorithm SHA256).Hash.ToLowerInvariant())
    $record | Add-Member -NotePropertyName manifestSHA256 -NotePropertyValue ((Get-FileHash -LiteralPath $manifestPath -Algorithm SHA256).Hash.ToLowerInvariant())
    $record | Add-Member -NotePropertyName binarySHA256 -NotePropertyValue ((Get-FileHash -LiteralPath $binaryPath -Algorithm SHA256).Hash.ToLowerInvariant())
    $record | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $recordPath -Encoding utf8NoBOM
}
if ($Action -eq 'Runtime') {
    if ($ReviewVerifiedArtifactPreflight) {
        if (-not $IncludeGatewayHTTP -or $record.state -ne 'runtime-started' -or -not $record.gatewayHTTPRequested -or (Test-Path -LiteralPath (Join-Path $Directory 'runtime.log'))) { throw 'Only an unexecuted HTTP artifact preflight can be reviewed' }
        $existing=Invoke-AuthorityCommand kubectl @('--kubeconfig',$kubeconfig,'--request-timeout=15s','get','crd','runtimeapprovals.kognition.dev','--ignore-not-found','-o','name')
        if (-not [string]::IsNullOrWhiteSpace($existing)) { throw 'RuntimeApproval fixtures exist; artifact-only continuation is refused' }
        $processes=Invoke-AuthorityCommand docker @('--context','desktop-linux','exec',"$($record.name)-control-plane",'ps','-eo','pid,comm,args')
        if ($processes -match '(?m)^\s*[0-9]+\s+gateway-authori') { throw 'An acceptance runner remains active' }
        $hashes=Get-Content -Raw -LiteralPath (Join-Path $Directory 'http-crd-hashes.json') | ConvertFrom-Json
        $copied=Invoke-AuthorityCommand docker (@('--context','desktop-linux','exec',"$($record.name)-control-plane",'sha256sum')+@($hashes | ForEach-Object {"/var/authority-http-crds/$($_.name)"}))
        foreach ($artifact in $hashes) { if ($copied -notmatch "(?m)^$($artifact.sha256)[ \t]+/var/authority-http-crds/$([regex]::Escape($artifact.name))\r?$") { throw 'Artifact-only continuation requires exact copied hashes' } }
        @{runID=$record.runID;clusterUID=$record.clusterUID;reviewedUTC=[DateTime]::UtcNow.ToString('o');outcome='verified-artifacts-no-runtime-fixtures';hashes=$hashes} | ConvertTo-Json -Depth 6 | Set-Content -LiteralPath (Join-Path $Directory 'artifact-preflight-review.json') -Encoding utf8NoBOM
        $record.state='leases-passed'
        $record | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $recordPath -Encoding utf8NoBOM
    }
    if ($ReviewKnownCRDFailure) {
        $failedLog=Join-Path $Directory 'runtime.log'
        $failure=Get-Content -Raw -LiteralPath $failedLog
        if ($record.state -ne 'runtime-started' -or $failure -notmatch 'unexpected RuntimeApproval CRD' -or $failure -match 'Retained \*') { throw 'Only the retained pre-fixture CRD parse failure can be reviewed for continuation' }
        $existing=Invoke-AuthorityCommand kubectl @('--kubeconfig',$kubeconfig,'--request-timeout=15s','get','crd','runtimeapprovals.kognition.dev','--ignore-not-found','-o','name')
        if (-not [string]::IsNullOrWhiteSpace($existing)) { throw 'RuntimeApproval CRD already exists; pre-fixture continuation is refused' }
        Copy-Item -LiteralPath $failedLog -Destination (Join-Path $Directory 'runtime-crd-parse-failure.log') -ErrorAction Stop
        $record | Add-Member -NotePropertyName reviewedCRDParseFailureSHA256 -NotePropertyValue ((Get-FileHash -LiteralPath $failedLog -Algorithm SHA256).Hash.ToLowerInvariant())
        $record.state='leases-passed'
        $record | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $recordPath -Encoding utf8NoBOM
    }
    if ($record.state -ne 'leases-passed') { throw 'RuntimeApproval acceptance requires passed real-token Lease acceptance; implicit retries are refused' }
    $record.state='runtime-started'
    $record | Add-Member -Force -NotePropertyName gatewayHTTPRequested -NotePropertyValue ([bool]$IncludeGatewayHTTP)
    $record | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $recordPath -Encoding utf8NoBOM
    $binaryPath=Join-Path $Directory 'gateway-authority-runtime.test'
    $oldGOOS,$oldGOARCH,$oldCGO=$env:GOOS,$env:GOARCH,$env:CGO_ENABLED
    try {
        $env:GOOS='linux'
        $env:GOARCH='amd64'
        $env:CGO_ENABLED='0'
        $null=Invoke-AuthorityCommand go @('test','-tags=integration','-c','-o',$binaryPath,'./internal/traceability')
    } finally {
        $env:GOOS=$oldGOOS
        $env:GOARCH=$oldGOARCH
        $env:CGO_ENABLED=$oldCGO
    }
    $crdPath=Join-Path $Directory 'runtimeapproval-crd.yaml'
    Copy-Item -LiteralPath 'deploy/helm/kognition/crds/kognition.dev_runtimeapprovals.yaml' -Destination $crdPath
    $node="$($record.name)-control-plane"
    $bounded=Join-Path (Split-Path -Parent $PSScriptRoot) 'tools/run_bounded.py'
    foreach ($artifact in @(@{path=$binaryPath;destination='/var/gateway-authority-runtime.test'},@{path=$crdPath;destination='/var/runtimeapproval-crd.yaml'})) {
        $null=Invoke-AuthorityCommand python @($bounded,'--timeout','60','--','docker','--context','desktop-linux','cp',$artifact.path,"${node}:$($artifact.destination)")
        $copied=Invoke-AuthorityCommand docker @('--context','desktop-linux','exec',$node,'sha256sum',$artifact.destination)
        $expected=(Get-FileHash -LiteralPath $artifact.path -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($copied -notmatch "^$expected\s") { throw 'Copied RuntimeApproval acceptance artifact hash mismatch' }
    }
    if ($IncludeGatewayHTTP) {
        $definitions=Join-Path $Directory 'http-crds'
        $null=New-Item -ItemType Directory -Path $definitions -Force
        $hashes=@()
        foreach ($plural in @('agents','agentidentities','agentinstances','agentpolicies','agentroles','agentrolebindings','tools','toolgrants','toolinvocationruns','cognitivequotas','trustprofiles','trustprofileindices','autonomyclasses','runtimeapprovaltombstones','runtimeapprovalquotatickets')) {
            $name="kognition.dev_$plural.yaml"
            Copy-Item -LiteralPath (Join-Path 'deploy/helm/kognition/crds' $name) -Destination (Join-Path $definitions $name)
            $hashes+=@{name=$name;sha256=(Get-FileHash -LiteralPath (Join-Path $definitions $name) -Algorithm SHA256).Hash.ToLowerInvariant()}
        }
        $hashes | ConvertTo-Json -Depth 5 | Set-Content -LiteralPath (Join-Path $Directory 'http-crd-hashes.json') -Encoding utf8NoBOM
        $null=Invoke-AuthorityCommand python @($bounded,'--timeout','60','--','docker','--context','desktop-linux','cp',$definitions,"${node}:/var/authority-http-crds")
        $copied=Invoke-AuthorityCommand docker (@('--context','desktop-linux','exec',$node,'sha256sum')+@($hashes | ForEach-Object {"/var/authority-http-crds/$($_.name)"}))
        foreach ($artifact in $hashes) { if ($copied -notmatch "(?m)^$($artifact.sha256)[ \t]+/var/authority-http-crds/$([regex]::Escape($artifact.name))\r?$") { throw 'Copied HTTP fixture CRD hash mismatch' } }
    }
    $httpEnabled=([bool]$IncludeGatewayHTTP).ToString().ToLowerInvariant()
    $log=& python $bounded --timeout 300 -- docker --context desktop-linux exec -e "KOGNITION_AUTHORITY_RUN_ID=$($record.runID)" -e "KOGNITION_AUTHORITY_CLUSTER_UID=$($record.clusterUID)" -e "KOGNITION_ADMISSION_EXPECTED_VERSION=$($record.expectedVersion)" -e KOGNITION_AUTHORITY_KUBECONFIG=/etc/kubernetes/admin.conf -e KOGNITION_AUTHORITY_APPROVAL_CRD=/var/runtimeapproval-crd.yaml -e "KOGNITION_AUTHORITY_GATEWAY_HTTP=$httpEnabled" -e KOGNITION_AUTHORITY_CRD_DIRECTORY=/var/authority-http-crds $node /var/gateway-authority-runtime.test '-test.run=^TestGatewayAuthorityRuntimeApprovalAdmission$' '-test.timeout=4m' '-test.v' 2>&1
    $exit=$LASTEXITCODE
    $log | Set-Content -LiteralPath (Join-Path $Directory 'runtime.log') -Encoding utf8NoBOM
    Write-Output $log
    if ($exit -ne 0 -or ($log -join "`n") -notmatch '(?m)^PASS\s*$' -or ($log -join "`n") -match '--- SKIP:') { throw 'Live RuntimeApproval acceptance did not pass; retain evidence without implicit retry' }
    if ($IncludeGatewayHTTP -and ($log -join "`n") -notmatch '--- PASS: .*/tool-http-concurrent-execution') { throw 'Requested gateway HTTP acceptance did not pass' }
    $record.state='runtime-passed'
    $record | Add-Member -NotePropertyName runtimeSHA256 -NotePropertyValue ((Get-FileHash -LiteralPath (Join-Path $Directory 'runtime.log') -Algorithm SHA256).Hash.ToLowerInvariant())
    $record | Add-Member -NotePropertyName runtimeBinarySHA256 -NotePropertyValue ((Get-FileHash -LiteralPath $binaryPath -Algorithm SHA256).Hash.ToLowerInvariant())
    $record | Add-Member -NotePropertyName runtimeCRDSHA256 -NotePropertyValue ((Get-FileHash -LiteralPath $crdPath -Algorithm SHA256).Hash.ToLowerInvariant())
    $record | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $recordPath -Encoding utf8NoBOM
}
if ($Action -eq 'Snapshot') {
    if (Test-Path -LiteralPath (Join-Path $Directory 'evidence-index.json')) {
        $priorIndex=Join-Path $Directory 'evidence-index.json'
        if (-not $record.evidenceSHA256 -or (Get-FileHash -LiteralPath $priorIndex -Algorithm SHA256).Hash.ToLowerInvariant() -ne $record.evidenceSHA256) { throw 'Prior evidence index changed; snapshot replacement is refused' }
        $history=[guid]::NewGuid().ToString('N')
        foreach ($name in @('authority.json','retained-cluster.json','evidence-index.json')) { Copy-Item -LiteralPath (Join-Path $Directory $name) -Destination (Join-Path $Directory "history-$history-$name") -ErrorAction Stop }
    }
    $processes=Invoke-AuthorityCommand docker @('--context','desktop-linux','exec',"$($record.name)-control-plane",'ps','-eo','pid,comm,args')
    if ($processes -match '(?m)^\s*[0-9]+\s+gateway-authori') { throw 'Acceptance runner remains active; evidence review is not complete' }
    $snapshot=[ordered]@{runID=$record.runID;clusterUID=$record.clusterUID;state=$record.state;capturedUTC=[DateTime]::UtcNow.ToString('o');runnerProcesses=$processes}
    foreach ($resource in @('nodes','namespaces','pods','leases','validatingadmissionpolicies','validatingadmissionpolicybindings','validatingwebhookconfigurations','roles','rolebindings','clusterroles','clusterrolebindings')) {
        $snapshot[$resource]=Invoke-AuthorityCommand kubectl @('--kubeconfig',$kubeconfig,'--request-timeout=20s','get',$resource,'--all-namespaces','-o','json') | ConvertFrom-Json
    }
    $crd=Invoke-AuthorityCommand kubectl @('--kubeconfig',$kubeconfig,'--request-timeout=15s','get','crd','runtimeapprovals.kognition.dev','--ignore-not-found','-o','name')
    if (-not [string]::IsNullOrWhiteSpace($crd)) { $snapshot['approvals']=Invoke-AuthorityCommand kubectl @('--kubeconfig',$kubeconfig,'--request-timeout=15s','get','runtimeapprovals','--all-namespaces','-o','json') | ConvertFrom-Json }
    foreach ($resource in @('runtimeapprovalquotatickets','runtimeapprovaltombstones','toolinvocationruns')) {
        $definition=Invoke-AuthorityCommand kubectl @('--kubeconfig',$kubeconfig,'--request-timeout=15s','get','crd',"$resource.kognition.dev",'--ignore-not-found','-o','name')
        if (-not [string]::IsNullOrWhiteSpace($definition)) { $snapshot[$resource]=Invoke-AuthorityCommand kubectl @('--kubeconfig',$kubeconfig,'--request-timeout=15s','get',$resource,'--all-namespaces','-o','json') | ConvertFrom-Json }
    }
    foreach ($namespace in @('control','tenant')) { $snapshot["$namespace-configmaps"]=Invoke-AuthorityCommand kubectl @('--kubeconfig',$kubeconfig,'--request-timeout=15s','get','configmaps','-n',$namespace,'-o','json') | ConvertFrom-Json }
    $snapshotPath=Join-Path $Directory 'retained-cluster.json'
    $snapshot | ConvertTo-Json -Depth 100 | Set-Content -LiteralPath $snapshotPath -Encoding utf8NoBOM
    $index=@(Get-ChildItem -LiteralPath $Directory -File | Where-Object { $_.Name -notin @('authority.json','evidence-index.json') } | ForEach-Object { @{name=$_.Name;sha256=(Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash.ToLowerInvariant()} })
    $index | ConvertTo-Json -Depth 5 | Set-Content -LiteralPath (Join-Path $Directory 'evidence-index.json') -Encoding utf8NoBOM
    $record | Add-Member -Force -NotePropertyName evidenceSHA256 -NotePropertyValue ((Get-FileHash -LiteralPath (Join-Path $Directory 'evidence-index.json') -Algorithm SHA256).Hash.ToLowerInvariant())
    $record | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $recordPath -Encoding utf8NoBOM
}
if ($Action -eq 'Cleanup') {
    $indexPath=Join-Path $Directory 'evidence-index.json'
    if (-not $record.evidenceSHA256 -or (Get-FileHash -LiteralPath $indexPath -Algorithm SHA256).Hash.ToLowerInvariant() -ne $record.evidenceSHA256) { throw 'Retained reviewed evidence is required before cleanup' }
    foreach ($artifact in (Get-Content -Raw -LiteralPath $indexPath | ConvertFrom-Json)) {
        if ($artifact.name -ne [IO.Path]::GetFileName($artifact.name) -or (Get-FileHash -LiteralPath (Join-Path $Directory $artifact.name) -Algorithm SHA256).Hash.ToLowerInvariant() -ne $artifact.sha256) { throw 'Retained acceptance evidence changed' }
    }
    $snapshot=Get-Content -Raw -LiteralPath (Join-Path $Directory 'retained-cluster.json') | ConvertFrom-Json
    if ($snapshot.clusterUID -ne $record.clusterUID -or $snapshot.runID -ne $record.runID -or $snapshot.state -ne $record.state) { throw 'Retained evidence does not match the active target' }
    $fixtureInvocations=@('human-tool-gateway','human-memory-policy-gateway','human-agent-communication-gateway','recovery-tool-gateway','recovery-memory-policy-gateway','recovery-agent-communication-gateway')
    $closedFixtureOnly=$record.state -eq 'runtime-started' -and $record.gatewayHTTPRequested -and $snapshot.approvals.items.Count -eq 6 -and $snapshot.runtimeapprovalquotatickets.items.Count -eq 0 -and @($snapshot.approvals.items | Where-Object {$_.spec.invocationID -notin $fixtureInvocations -or $_.status.executionState -notin @('Completed','Failed')}).Count -eq 0
    $productionClosedOnly = $false
    if ($record.state -in @('production-tool-passed','production-passed')) {
        $expectedApprovals = if ($record.state -eq 'production-passed') { 4 } else { 1 }
        $productionClosedOnly = $snapshot.approvals.items.Count -eq $expectedApprovals -and @($snapshot.approvals.items | Where-Object {
            $_.spec.invocationID -notmatch '^authority-(http|memory-(store|delete)|communication)-[0-9a-f-]{36}$' -or $_.status.executionState -ne 'Completed' -or -not $_.status.claimant -or $_.status.fencingToken -lt 1
        }).Count -eq 0
        if (-not $productionClosedOnly) { throw 'Production cleanup requires the exact completed fixture claims' }
        $live = Invoke-AuthorityCommand kubectl @('--kubeconfig',$kubeconfig,'--request-timeout=15s','get','runtimeapprovals','--all-namespaces','-o','json') | ConvertFrom-Json
        $retainedApprovals = @($snapshot.approvals.items | ForEach-Object { "$($_.metadata.uid):$($_.metadata.resourceVersion):$($_.status.executionState)" } | Sort-Object)
        $liveApprovals = @($live.items | ForEach-Object { "$($_.metadata.uid):$($_.metadata.resourceVersion):$($_.status.executionState)" } | Sort-Object)
        if (Compare-Object $retainedApprovals $liveApprovals) { throw 'Production claims changed after the reviewed snapshot; cleanup is refused' }
    }
    if ($record.state -ne 'runtime-passed' -and -not $productionClosedOnly -and -not $closedFixtureOnly -and -not ($record.state -eq 'runtime-started' -and $snapshot.approvals.items.Count -eq 3 -and @($snapshot.approvals.items | Where-Object {$_.status.claimant -or $_.status.executionState -ne 'NotStarted'}).Count -eq 0)) { throw 'Acceptance outcome requires review before cleanup; active or uncertain claims cannot be discarded' }
    $processes=Invoke-AuthorityCommand docker @('--context','desktop-linux','exec',"$($record.name)-control-plane",'ps','-eo','pid,comm,args')
    if ($processes -match '(?m)^\s*[0-9]+\s+gateway-authori') { throw 'Acceptance runner remains active; cleanup is refused' }
    $record | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath (Join-Path $Directory 'retained-target.json') -Encoding utf8NoBOM
    Remove-OwnedAuthorityCluster
    $record | Add-Member -NotePropertyName cleanedUTC -NotePropertyValue ([DateTime]::UtcNow.ToString('o'))
    $record.state='cleaned'
    $record | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $recordPath -Encoding utf8NoBOM
}
$record | ConvertTo-Json -Depth 8