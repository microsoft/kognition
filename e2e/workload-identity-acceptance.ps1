# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

[CmdletBinding()]
param(
    [ValidateSet('Check', 'Create', 'Prepare', 'Seed', 'Provision', 'Verify', 'Migrate', 'Gateways', 'Candidate', 'CandidateContinue', 'Cleanup')][string]$Action = 'Check',
    [Parameter(Mandatory)][string]$Directory,
    [string]$NodeImage = 'kindest/node:v1.31.1',
    [switch]$ExerciseOutage,
    [switch]$ExerciseRecreation,
    [ValidateSet('', 'kubernetes', 'standalone')][string]$RecreationMode = '',
    [ValidateSet('', 'kubernetes', 'standalone')][string]$GatewayMode = '',
    [ValidateSet('identity','candidate')][string]$Profile = 'identity',
    [ValidateSet('','kubernetes')][string]$CandidateMode = '',
    [switch]$ReviewCandidateOutcomes
)

$ErrorActionPreference = 'Stop'
$Directory = [IO.Path]::GetFullPath($Directory)
$recordPath = Join-Path $Directory 'cluster.json'
$kubeconfig = Join-Path $Directory 'kubeconfig'

function Invoke-FixtureCommand {
    param([string]$Program, [string[]]$Arguments)
    $output = & $Program @Arguments 2>&1
    if ($LASTEXITCODE -ne 0) { throw "$Program failed: $($output -join [Environment]::NewLine)" }
    return ($output -join [Environment]::NewLine)
}

function Get-OwnedFixture {
    if (-not (Test-Path -LiteralPath $recordPath)) { throw 'Recorded isolated cluster identity is required' }
    $record = Get-Content -Raw -LiteralPath $recordPath | ConvertFrom-Json
    $parsed = [guid]::Empty
    if (-not [guid]::TryParseExact($record.runID, 'D', [ref]$parsed) -or $parsed -eq [guid]::Empty -or
        $record.name -ne "kognition-$($record.runID)" -or $record.kubeconfig -ne $kubeconfig) {
        throw 'Fixture ownership record is invalid'
    }
    $configuration = Invoke-FixtureCommand kubectl @('--kubeconfig', $kubeconfig, 'config', 'view', '--minify', '-o', 'json') | ConvertFrom-Json
    if ($configuration.clusters.Count -ne 1 -or $configuration.clusters[0].cluster.server -notmatch '^https://127\.0\.0\.1:[0-9]+$') {
        throw 'Fixture must use an explicit loopback API endpoint'
    }
    $nodes = Invoke-FixtureCommand kubectl @('--kubeconfig', $kubeconfig, '--request-timeout=15s', 'get', 'nodes', '-o', 'json') | ConvertFrom-Json
    $root = Invoke-FixtureCommand kubectl @('--kubeconfig', $kubeconfig, '--request-timeout=15s', 'get', 'namespace', 'kube-system', '-o', 'json') | ConvertFrom-Json
    if ($nodes.items.Count -ne 1 -or $nodes.items[0].metadata.name -ne "$($record.name)-control-plane" -or
        $nodes.items[0].metadata.labels.'kognition.dev/identity-acceptance-run' -ne $record.runID -or
        $root.metadata.uid -ne $record.clusterUID) { throw 'Selected cluster no longer matches recorded fixture ownership' }
    return $record
}

function Add-FixtureObject {
    param([hashtable]$Object)
    $output = $Object | ConvertTo-Json -Depth 30 | & kubectl --kubeconfig $kubeconfig --request-timeout=15s create -f - 2>&1
    if ($LASTEXITCODE -ne 0) { throw "Fixture create failed: $output" }
}

function Ensure-CandidateUsageLock {
    $role=Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','candidate-acceptance','get','role','candidate-usage-lock','--ignore-not-found','-o','json')
    if($role) {
        $existing=$role | ConvertFrom-Json
        if($existing.rules.Count -ne 1 -or ($existing.rules[0].apiGroups -join ',') -ne 'coordination.k8s.io' -or ($existing.rules[0].resources -join ',') -ne 'leases' -or (($existing.rules[0].verbs | Sort-Object) -join ',') -ne 'create,get,update' -or $existing.rules[0].resourceNames) {throw 'Existing candidate usage-lock permissions differ from the reviewed scope'}
    } else {
        Add-FixtureObject @{apiVersion='rbac.authorization.k8s.io/v1';kind='Role';metadata=@{name='candidate-usage-lock';namespace='candidate-acceptance'};rules=@(@{apiGroups=@('coordination.k8s.io');resources=@('leases');verbs=@('get','create','update')})}
    }
    $binding=Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','candidate-acceptance','get','rolebinding','candidate-usage-lock','--ignore-not-found','-o','json')
    if($binding) {
        $existing=$binding | ConvertFrom-Json
        if($existing.roleRef.apiGroup -ne 'rbac.authorization.k8s.io' -or $existing.roleRef.kind -ne 'Role' -or $existing.roleRef.name -ne 'candidate-usage-lock' -or $existing.subjects.Count -ne 1 -or $existing.subjects[0].kind -ne 'ServiceAccount' -or $existing.subjects[0].name -ne 'runner' -or $existing.subjects[0].namespace -ne 'candidate-acceptance') {throw 'Existing candidate usage-lock authority differs from the reviewed scope'}
    } else {
        Add-FixtureObject @{apiVersion='rbac.authorization.k8s.io/v1';kind='RoleBinding';metadata=@{name='candidate-usage-lock';namespace='candidate-acceptance'};roleRef=@{apiGroup='rbac.authorization.k8s.io';kind='Role';name='candidate-usage-lock'};subjects=@(@{kind='ServiceAccount';name='runner';namespace='candidate-acceptance'})}
    }
}

if ($Action -eq 'Create') {
    if (Test-Path -LiteralPath $Directory) { throw 'Create requires a new directory; existing fixtures are never reused implicitly' }
    foreach ($program in @('kind', 'docker', 'kubectl', 'helm', 'go')) { $null = Get-Command $program -ErrorAction Stop }
    $null = Invoke-FixtureCommand docker @('info', '--format', '{{.OSType}}')
    $runID = [guid]::NewGuid().ToString('D')
    $name = "kognition-$runID"
    $null = New-Item -ItemType Directory -Path $Directory
    $configPath = Join-Path $Directory 'kind.json'
    $configuration = @{
        kind = 'Cluster'; apiVersion = 'kind.x-k8s.io/v1alpha4'; name = $name
        networking = @{ apiServerAddress = '127.0.0.1'; disableDefaultCNI = ($Profile -ne 'candidate'); podSubnet = '10.244.0.0/16' }
        nodes = @(@{ role = 'control-plane'; labels = @{ 'kognition.dev/identity-acceptance-run' = $runID; 'kognition.dev/acceptance-run' = $runID } })
    }
    $configuration | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $configPath -Encoding utf8NoBOM
    $null = Invoke-FixtureCommand kind @('create', 'cluster', '--name', $name, '--image', $NodeImage, '--config', $configPath, '--kubeconfig', $kubeconfig, '--wait', '120s')
    $root = Invoke-FixtureCommand kubectl @('--kubeconfig', $kubeconfig, '--request-timeout=15s', 'get', 'namespace', 'kube-system', '-o', 'json') | ConvertFrom-Json
    @{ runID = $runID; name = $name; kubeconfig = $kubeconfig; clusterUID = $root.metadata.uid; nodeImage = $NodeImage; profile=$Profile; createdUTC = [DateTime]::UtcNow.ToString('o') } |
        ConvertTo-Json | Set-Content -LiteralPath $recordPath -Encoding utf8NoBOM
    Get-OwnedFixture | ConvertTo-Json
    return
}

$record = Get-OwnedFixture
if ($Action -eq 'CandidateContinue') {
    if($record.profile -ne 'candidate' -or $record.candidateRun -ne 'started' -or -not $CandidateMode -or $record.candidatekubernetes){throw 'Explicit unfinished candidate backend continuation required; accepted backends cannot be replayed'}
    $previous=Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','candidate-acceptance','get','pod','candidate','-o','json') | ConvertFrom-Json
    if($previous.status.phase -ne 'Failed'){throw 'Only a recorded terminated candidate failure can be continued'}
    $suffix=[guid]::NewGuid().ToString('N').Substring(0,8)
    $image="kognition-candidate-acceptance:$($record.runID)-$suffix"
    $oldGOOS,$oldGOARCH,$oldCGO=$env:GOOS,$env:GOARCH,$env:CGO_ENABLED
    try{$env:GOOS='linux';$env:GOARCH='amd64';$env:CGO_ENABLED='0';$null=Invoke-FixtureCommand go @('test','-c','-tags','integration','-o',(Join-Path $Directory 'acceptance'),'./internal/traceability')}finally{$env:GOOS=$oldGOOS;$env:GOARCH=$oldGOARCH;$env:CGO_ENABLED=$oldCGO}
    $cache=Join-Path $Directory 'tokenizer/9b5ad71b2ce5302211f9c61530b329a4922fc6a4'
    if((Get-FileHash -LiteralPath $cache -Algorithm SHA256).Hash.ToLowerInvariant() -ne $record.tokenizerSHA256){throw 'Frozen tokenizer differs from the reviewed profile'}
    $null=Invoke-FixtureCommand docker @('build','--quiet','-t',$image,'-f',(Join-Path $PSScriptRoot 'fixtures/Dockerfile.model-candidate'),$Directory)
    $archive=Join-Path $Directory "candidate-$suffix.tar";$node="$($record.name)-control-plane"
    $null=Invoke-FixtureCommand docker @('save','--platform','linux/amd64','-o',$archive,$image)
    $null=Invoke-FixtureCommand docker @('cp',$archive,"${node}:/var/candidate-continue.tar")
    $null=Invoke-FixtureCommand docker @('exec',$node,'ctr','--namespace=k8s.io','images','import','--platform','linux/amd64','/var/candidate-continue.tar')
    Ensure-CandidateUsageLock
    $pod=Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','candidate-acceptance','get','pod','candidate','-o','json') | ConvertFrom-Json -AsHashtable
    $pod.Remove('status');$pod.metadata=@{name="candidate-$suffix";namespace='candidate-acceptance';labels=@{'kognition.dev/acceptance-run'=$record.runID}}
    $pod.spec.containers[0].image=$image
    $pod.spec.containers[0].env+=@(@{name='KOGNITION_CANDIDATE_MODE';value=$CandidateMode},@{name='KOGNITION_CANDIDATE_ATTEMPT';value="-$suffix"})
    Add-FixtureObject $pod
    $created=Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','candidate-acceptance','get','pod',$pod.metadata.name,'-o','json') | ConvertFrom-Json
    @{mode=$CandidateMode;pod=$pod.metadata.name;podUID=$created.metadata.uid;image=$image;runID=$record.runID} | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $Directory "candidate-$suffix.json") -Encoding utf8NoBOM
    $null=Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','candidate-acceptance','wait','--for=condition=PodReadyToStartContainers',"pod/$($pod.metadata.name)",'--timeout=120s')
    $logs=Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','candidate-acceptance','logs',$pod.metadata.name,'--follow','--pod-running-timeout=60s')
    $logs | Set-Content -LiteralPath (Join-Path $Directory "candidate-$suffix.log") -Encoding utf8NoBOM
    Write-Output $logs
    if($logs -notmatch '(?m)^PASS\s*$' -or $logs -match '(?m)^FAIL\s*$|^panic:'){throw 'Candidate backend continuation failed; evidence retained, no automatic replay'}
    $null=Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','candidate-acceptance','wait','--for=jsonpath={.status.phase}=Succeeded',"pod/$($pod.metadata.name)",'--timeout=30s')
    $record | Add-Member -NotePropertyName "candidate$CandidateMode" -NotePropertyValue $pod.metadata.name -Force
    $record | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $recordPath -Encoding utf8NoBOM
}
if ($Action -eq 'Candidate') {
    if ($record.profile -ne 'candidate' -or $record.candidateRun) {throw 'Candidate acceptance requires a fresh candidate-profile cluster with no previous run'}
    $record | Add-Member -NotePropertyName candidateRun -NotePropertyValue 'started' -Force
    $record | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $recordPath -Encoding utf8NoBOM
    $root=Split-Path -Parent $PSScriptRoot
    $image="kognition-candidate-acceptance:$($record.runID)"
    $oldGOOS,$oldGOARCH,$oldCGO=$env:GOOS,$env:GOARCH,$env:CGO_ENABLED
    try {
        $env:GOOS='linux';$env:GOARCH='amd64';$env:CGO_ENABLED='0'
        $null=Invoke-FixtureCommand go @('test','-c','-tags','integration','-o',(Join-Path $Directory 'acceptance'),'./internal/traceability')
    } finally {$env:GOOS=$oldGOOS;$env:GOARCH=$oldGOARCH;$env:CGO_ENABLED=$oldCGO}
    $tokenizer=Join-Path $env:TEMP 'data-gym-cache/9b5ad71b2ce5302211f9c61530b329a4922fc6a4'
    $tokenizerHash='223921b76ee99bde995b7ff738513eef100fb51d18c93597a113bcffe865b2a7'
    if(-not(Test-Path -LiteralPath $tokenizer) -or (Get-FileHash -LiteralPath $tokenizer -Algorithm SHA256).Hash.ToLowerInvariant() -ne $tokenizerHash){throw 'Reviewed offline exact-tokenizer artifact unavailable; no runtime download is allowed'}
    $cache=Join-Path $Directory 'tokenizer';$null=New-Item -ItemType Directory -Path $cache
    Copy-Item -LiteralPath $tokenizer -Destination (Join-Path $cache '9b5ad71b2ce5302211f9c61530b329a4922fc6a4')
    if((Get-FileHash -LiteralPath (Join-Path $cache '9b5ad71b2ce5302211f9c61530b329a4922fc6a4') -Algorithm SHA256).Hash.ToLowerInvariant() -ne $tokenizerHash){throw 'Tokenizer copy verification failed'}
    $record | Add-Member -NotePropertyName tokenizerSHA256 -NotePropertyValue $tokenizerHash -Force
    $record | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $recordPath -Encoding utf8NoBOM
    $null=Invoke-FixtureCommand docker @('build','--quiet','-t',$image,'-f',(Join-Path $PSScriptRoot 'fixtures/Dockerfile.model-candidate'),$Directory)
    $archive=Join-Path $Directory 'candidate-images.tar'
    $node="$($record.name)-control-plane"
    $null=Invoke-FixtureCommand docker @('save','--platform','linux/amd64','-o',$archive,$image,'pgvector/pgvector:kind-local')
    $null=Invoke-FixtureCommand docker @('cp',$archive,"${node}:/var/candidate-images.tar")
    $null=Invoke-FixtureCommand docker @('exec',$node,'ctr','--namespace=k8s.io','images','import','--platform','linux/amd64','/var/candidate-images.tar')
    foreach($namespace in @('candidate-acceptance','candidate-kubernetes','candidate-standalone')) {Add-FixtureObject @{apiVersion='v1';kind='Namespace';metadata=@{name=$namespace;labels=@{'kognition.dev/acceptance-run'=$record.runID}}}}
    $password=[guid]::NewGuid().ToString('N')+[guid]::NewGuid().ToString('N')
    Add-FixtureObject @{apiVersion='v1';kind='Secret';metadata=@{name='database';namespace='candidate-acceptance'};stringData=@{password=$password;dsn="postgres://postgres:$password@database.candidate-acceptance.svc:5432/postgres?sslmode=disable"}}
    Add-FixtureObject @{apiVersion='v1';kind='Pod';metadata=@{name='database';namespace='candidate-acceptance';labels=@{app='candidate-database'}};spec=@{automountServiceAccountToken=$false;containers=@(@{name='database';image='pgvector/pgvector:kind-local';imagePullPolicy='Never';env=@(@{name='POSTGRES_PASSWORD';valueFrom=@{secretKeyRef=@{name='database';key='password'}}});resources=@{requests=@{cpu='50m';memory='128Mi'};limits=@{cpu='1';memory='512Mi'}};readinessProbe=@{exec=@{command=@('pg_isready','-U','postgres')};periodSeconds=2};volumeMounts=@(@{name='data';mountPath='/var/lib/postgresql/data'})});volumes=@(@{name='data';emptyDir=@{}})}}
    Add-FixtureObject @{apiVersion='v1';kind='Service';metadata=@{name='database';namespace='candidate-acceptance'};spec=@{selector=@{app='candidate-database'};ports=@(@{port=5432;targetPort=5432})}}
    $null=Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','candidate-acceptance','wait','--for=condition=Ready','pod/database','--timeout=120s')
    foreach($kind in @('models','modelclasses','cognitivequotas')) {$null=Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'apply','-f',(Join-Path $root "deploy/helm/kognition/crds/kognition.dev_$kind.yaml"));$null=Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'wait','--for=condition=Established',"crd/$kind.kognition.dev",'--timeout=60s')}
    Add-FixtureObject @{apiVersion='v1';kind='ServiceAccount';metadata=@{name='runner';namespace='candidate-acceptance'}}
    Add-FixtureObject @{apiVersion='rbac.authorization.k8s.io/v1';kind='ClusterRole';metadata=@{name='candidate-acceptance'};rules=@(@{apiGroups=@('');resources=@('nodes');verbs=@('list')},@{apiGroups=@('');resources=@('namespaces');resourceNames=@('kube-system');verbs=@('get')},@{apiGroups=@('kognition.dev');resources=@('modelclasses','modelclasses/status');verbs=@('get','create','update','patch')})}
    Add-FixtureObject @{apiVersion='rbac.authorization.k8s.io/v1';kind='ClusterRoleBinding';metadata=@{name='candidate-acceptance'};roleRef=@{apiGroup='rbac.authorization.k8s.io';kind='ClusterRole';name='candidate-acceptance'};subjects=@(@{kind='ServiceAccount';name='runner';namespace='candidate-acceptance'})}
    foreach($namespace in @('candidate-kubernetes','candidate-acceptance')) {
        Add-FixtureObject @{apiVersion='rbac.authorization.k8s.io/v1';kind='Role';metadata=@{name='candidate-acceptance';namespace=$namespace};rules=@(@{apiGroups=@('kognition.dev');resources=@('models','models/status','cognitivequotas','cognitivequotas/status');verbs=@('get','list','create','update','patch')},@{apiGroups=@('');resources=@('configmaps');verbs=@('get','list','create','update','patch')})}
        Add-FixtureObject @{apiVersion='rbac.authorization.k8s.io/v1';kind='RoleBinding';metadata=@{name='candidate-acceptance';namespace=$namespace};roleRef=@{apiGroup='rbac.authorization.k8s.io';kind='Role';name='candidate-acceptance'};subjects=@(@{kind='ServiceAccount';name='runner';namespace='candidate-acceptance'})}
    }
    Ensure-CandidateUsageLock
    $pod=@{
        apiVersion='v1';kind='Pod';metadata=@{name='candidate';namespace='candidate-acceptance';labels=@{'kognition.dev/acceptance-run'=$record.runID}}
        spec=@{
            serviceAccountName='runner';restartPolicy='Never';securityContext=@{runAsNonRoot=$true;runAsUser=65532;seccompProfile=@{type='RuntimeDefault'}}
            containers=@(@{name='acceptance';image=$image;imagePullPolicy='Never';args=@('-test.v','-test.run=^TestDeployedModelCandidatePromotionRollbackRecovery$','-test.timeout=360s');securityContext=@{readOnlyRootFilesystem=$true;allowPrivilegeEscalation=$false;capabilities=@{drop=@('ALL')}};resources=@{requests=@{cpu='100m';memory='128Mi'};limits=@{cpu='2';memory='768Mi'}};volumeMounts=@(@{name='tmp';mountPath='/tmp'});env=@(@{name='KOGNITION_MODEL_CANDIDATE_ACCEPTANCE';value='true'},@{name='KOGNITION_CANDIDATE_RUN_ID';value=$record.runID},@{name='KOGNITION_CANDIDATE_CLUSTER_UID';value=$record.clusterUID},@{name='KOGNITION_CANDIDATE_DATABASE_URL';valueFrom=@{secretKeyRef=@{name='database';key='dsn'}}})})
            volumes=@(@{name='tmp';emptyDir=@{sizeLimit='32Mi'}})
        }
    }
    Add-FixtureObject $pod
    $null=Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','candidate-acceptance','wait','--for=condition=PodReadyToStartContainers','pod/candidate','--timeout=120s')
    $logs=Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','candidate-acceptance','logs','candidate','--follow','--pod-running-timeout=60s')
    $logs | Set-Content -LiteralPath (Join-Path $Directory 'candidate.log') -Encoding utf8NoBOM
    Write-Output $logs
    if($logs -notmatch '(?m)^PASS\s*$' -or $logs -match '(?m)^FAIL\s*$|^panic:'){throw 'Candidate acceptance failed; outcome/evidence retained, implicit retries refused'}
    $null=Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','candidate-acceptance','wait','--for=jsonpath={.status.phase}=Succeeded','pod/candidate','--timeout=30s')
    $record | Add-Member -NotePropertyName candidateRun -NotePropertyValue 'passed' -Force
    $record | Add-Member -NotePropertyName candidateLogSHA256 -NotePropertyValue ((Get-FileHash -LiteralPath (Join-Path $Directory 'candidate.log') -Algorithm SHA256).Hash.ToLowerInvariant()) -Force
    $record | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $recordPath -Encoding utf8NoBOM
}
if ($Action -in @('Verify','Migrate','Gateways')) {
    if (-not $record.proxyPodUIDs -or -not $record.parentID) { throw 'Verification requires recorded incarnation enrollment' }
    $suffix = [guid]::NewGuid().ToString('N').Substring(0,8)
    $image = "kognition-identity-acceptance:$($record.runID)-$suffix"
    $oldGOOS, $oldGOARCH, $oldCGO = $env:GOOS, $env:GOARCH, $env:CGO_ENABLED
    try {
        $env:GOOS='linux';$env:GOARCH='amd64';$env:CGO_ENABLED='0'
        $null = Invoke-FixtureCommand go @('test','-c','-tags','integration','-o',(Join-Path $Directory 'acceptance'),'./internal/traceability')
    } finally { $env:GOOS=$oldGOOS;$env:GOARCH=$oldGOARCH;$env:CGO_ENABLED=$oldCGO }
    $null = Invoke-FixtureCommand docker @('build','--quiet','-t',$image,'-f',(Join-Path $PSScriptRoot 'fixtures/Dockerfile.workload-identity'),$Directory)
    $archive = Join-Path $Directory "verify-$suffix.tar"
    $node = "$($record.name)-control-plane"
    $null = Invoke-FixtureCommand docker @('save','--platform','linux/amd64','-o',$archive,$image)
    $null = Invoke-FixtureCommand docker @('cp',$archive,"${node}:/var/identity-verify.tar")
    $null = Invoke-FixtureCommand docker @('exec',$node,'ctr','--namespace=k8s.io','images','import','--platform','linux/amd64','/var/identity-verify.tar')
    $pod = Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','identity-acceptance','get','pod','prepare','-o','json') | ConvertFrom-Json -AsHashtable
    $pod.Remove('status')
    $pod.metadata = @{name="status-$suffix";namespace='identity-acceptance';labels=@{'kognition.dev/identity-acceptance-run'=$record.runID;'app.kubernetes.io/component'='k8s-controller'}}
    $pod.spec.containers[0].image=$image
    $pod.spec.containers[0].env=@($pod.spec.containers[0].env | Where-Object {$_.name -ne 'KOGNITION_IDENTITY_PREPARE'})
    $pod.spec.containers[0].env+=@(@{name='KOGNITION_SPIFFE_ID';value='spiffe://kognition.dev/controller/identity-status'},@{name='KOGNITION_SPIFFE_TRUST_DOMAIN';value='kognition.dev'},@{name='KOGNITION_SPIFFE_ENDPOINT_SOCKET';value='unix:///spiffe-workload-api/spire-agent.sock'},@{name='KOGNITION_WORKLOAD_STATUS_REGISTRY_FILE';value='/registry/registry.json'})
    $pod.spec.containers[0].volumeMounts+=@(@{name='workload-api';mountPath='/spiffe-workload-api';readOnly=$true},@{name='registry';mountPath='/registry';readOnly=$true})
    $pod.spec.volumes+=@(@{name='workload-api';csi=@{driver='csi.spiffe.io';readOnly=$true}},@{name='registry';configMap=@{name='registry'}})
    $gatewayIDs = @()
    if ($Action -eq 'Gateways') {
        $pod.spec.containers[0].args=@('-test.v','-test.run=^TestDeployedWorkloadIdentityGateways$','-test.timeout=180s')
        $pod.spec.containers[0].env+=@(@{name='KOGNITION_IDENTITY_GATEWAY_ACCEPTANCE';value='true'})
        if ($GatewayMode) {$pod.spec.containers[0].env+=@(@{name='KOGNITION_IDENTITY_GATEWAY_MODE';value=$GatewayMode})}
        $crds=@('agentinstances','tools','toolgrants','agentroles','agentrolebindings','agentpolicies','cognitivequotas','memoryclaims','memoryvolumes','memorygrants','trustprofiles','trustprofileindices','runtimeapprovals','runtimeapprovaltombstones','runtimeapprovalquotatickets','autonomyclasses')
        $root = Split-Path -Parent $PSScriptRoot
        foreach ($kind in $crds) {
            $path=Join-Path $root "deploy/helm/kognition/crds/kognition.dev_$kind.yaml"
            if (-not (Test-Path -LiteralPath $path)) {throw "Gateway fixture CRD is absent: $kind"}
            $null=Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'apply','-f',$path)
            $null=Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'wait','--for=condition=Established',"crd/$kind.kognition.dev",'--timeout=60s')
        }
        $secret=Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','identity-acceptance','get','secret','--field-selector=metadata.name=gateway-binding-keys','-o','json') | ConvertFrom-Json
        if ($secret.items.Count -eq 0) {
            $material=New-Object byte[] 32
            [Security.Cryptography.RandomNumberGenerator]::Fill($material)
            $keyJSON=ConvertTo-Json -InputObject @(@{version=1;epoch=1;algorithm='HMAC-SHA-256';domain='kognition.dev/runtime-approval/action/v1';status='Active';material=[Convert]::ToBase64String($material)}) -Compress
            Add-FixtureObject @{apiVersion='v1';kind='Secret';metadata=@{name='gateway-binding-keys';namespace='identity-acceptance';labels=@{'kognition.dev/identity-acceptance-run'=$record.runID}};immutable=$true;stringData=@{keys=$keyJSON}}
        } elseif ($secret.items.Count -ne 1 -or $secret.items[0].metadata.labels.'kognition.dev/identity-acceptance-run' -ne $record.runID) {throw 'Binding key Secret does not belong to this fixture'}
        $pod.spec.containers[0].env+=@(@{name='KOGNITION_RUNTIME_APPROVAL_BINDING_KEYS_JSON';valueFrom=@{secretKeyRef=@{name='gateway-binding-keys';key='keys'}}},@{name='KOGNITION_RUNTIME_APPROVAL_WORKLOAD_UID';valueFrom=@{fieldRef=@{fieldPath='metadata.uid'}}})
        $classes=Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'get','autonomyclasses','--field-selector=metadata.name=supervised','-o','json') | ConvertFrom-Json
        if ($classes.items.Count -eq 0) {
            Add-FixtureObject @{apiVersion='kognition.dev/v1alpha1';kind='AutonomyClass';metadata=@{name='supervised';labels=@{'kognition.dev/identity-acceptance-run'=$record.runID}};spec=@{level=0;requiresApproval=$false;approvalPolicy=@{toolInvocation='never';memoryWrite='always';externalCommunication='always'}}}
        } elseif ($classes.items.Count -ne 1 -or $classes.items[0].metadata.labels.'kognition.dev/identity-acceptance-run' -ne $record.runID) {throw 'AutonomyClass does not belong to fixture'}
        $existing=Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'get','clusterrole','identity-acceptance-reader','-o','json') | ConvertFrom-Json -AsHashtable
        if (-not $existing.rules.Where({$_.resources -contains 'autonomyclasses'})) {$existing.rules+=@(@{apiGroups=@('kognition.dev');resources=@('autonomyclasses');resourceNames=@('supervised');verbs=@('get')});$existing | ConvertTo-Json -Depth 10 | & kubectl --kubeconfig $kubeconfig replace -f - | Out-Null;if($LASTEXITCODE -ne 0){throw 'Fixture class-read grant failed'}}
        $resources=@($crds + @('agents','agentidentities','agentidentities/status'))
        foreach ($kind in $crds) { $resources+=@("$kind/status") }
        $role=Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','identity-kubernetes','get','role','identity-acceptance','-o','json') | ConvertFrom-Json -AsHashtable
        $role.rules=@(@{apiGroups=@('kognition.dev');resources=$resources;verbs=@('get','list','create','update','patch','delete')},@{apiGroups=@('');resources=@('configmaps','events');verbs=@('get','list','create','update','patch')},@{apiGroups=@('coordination.k8s.io');resources=@('leases');verbs=@('get','create','update')})
        $role | ConvertTo-Json -Depth 10 | & kubectl --kubeconfig $kubeconfig replace -f - | Out-Null
        if ($LASTEXITCODE -ne 0) {throw 'Gateway fixture namespace-scoped resource grant failed'}
        $live=Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','identity-kubernetes','get','agent','worker','-o','json') | ConvertFrom-Json
        $gatewayIDs += "spiffe://kognition.dev/workspace/identity-kubernetes/agent/worker/incarnation/$($live.metadata.uid)"
        $standaloneUID=(Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','identity-acceptance','exec','database','--','psql','-U','postgres','-tAc',"SELECT id::text FROM resources WHERE kind='Agent' AND namespace='identity-standalone' AND name='worker' AND deleted_at IS NULL;")).Trim()
        $parsed=[guid]::Empty
        if (-not [guid]::TryParseExact($standaloneUID,'D',[ref]$parsed) -or $parsed -eq [guid]::Empty) {throw 'Current standalone incarnation is invalid'}
        $gatewayIDs += "spiffe://kognition.dev/workspace/identity-standalone/agent/worker/incarnation/$standaloneUID"
        $gatewayIDs += @('spiffe://kognition.dev/gateway/tool','spiffe://kognition.dev/gateway/memory')
        $pod.metadata.labels.'kognition.dev/identity-profile'='gateway-fixture-operator'
    }
    if ($Action -eq 'Migrate') {
        $backup = Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','identity-acceptance','exec','database','--','pg_dump','-U','postgres')
        $backupPath = Join-Path $Directory "database-before-migration-$suffix.sql"
        $backup | Set-Content -LiteralPath $backupPath -Encoding utf8NoBOM
        $pod.spec.containers[0].env+=@(@{name='KOGNITION_IDENTITY_MIGRATE';value='true'})
    }
    if ($ExerciseRecreation) {
        $pod.spec.containers[0].env+=@(@{name='KOGNITION_IDENTITY_EXERCISE_RECREATION';value='true'})
        if ($RecreationMode) { $pod.spec.containers[0].env+=@(@{name='KOGNITION_IDENTITY_RECREATION_MODE';value=$RecreationMode}) }
        $role = Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','identity-kubernetes','get','role','identity-acceptance','-o','json') | ConvertFrom-Json -AsHashtable
        $role.rules[0].verbs+=@('delete')
        $role | ConvertTo-Json -Depth 10 | & kubectl --kubeconfig $kubeconfig replace -f - | Out-Null
        if ($LASTEXITCODE -ne 0) {throw 'Fixture-only normal deletion grant failed'}
    }
    if ($ExerciseOutage) {
        $pod.spec.containers[0].env+=@(@{name='KOGNITION_IDENTITY_EXERCISE_OUTAGE';value='true'},@{name='KOGNITION_IDENTITY_RUNNER_POD';value=$pod.metadata.name})
        Add-FixtureObject @{apiVersion='rbac.authorization.k8s.io/v1';kind='Role';metadata=@{name="marker-$suffix";namespace='identity-acceptance'};rules=@(@{apiGroups=@('');resources=@('pods');resourceNames=@($pod.metadata.name);verbs=@('get','patch')})}
        Add-FixtureObject @{apiVersion='rbac.authorization.k8s.io/v1';kind='RoleBinding';metadata=@{name="marker-$suffix";namespace='identity-acceptance'};roleRef=@{apiGroup='rbac.authorization.k8s.io';kind='Role';name="marker-$suffix"};subjects=@(@{kind='ServiceAccount';name='runner';namespace='identity-acceptance'})}
    }
    Add-FixtureObject $pod
    $created = Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','identity-acceptance','get','pod',$pod.metadata.name,'-o','json') | ConvertFrom-Json
    if ($Action -eq 'Verify') {
        $null = Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','identity-spire','exec','deployment/server','--','/opt/spire/bin/spire-server','entry','create','-parentID',$record.parentID,'-spiffeID','spiffe://kognition.dev/controller/identity-status','-selector','k8s:ns:identity-acceptance','-selector',"k8s:pod-uid:$($created.metadata.uid)",'-selector','k8s:container-name:acceptance','-x509SVIDTTL','60')
    }
    foreach ($identity in $gatewayIDs) {
        $null=Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','identity-spire','exec','deployment/server','--','/opt/spire/bin/spire-server','entry','create','-parentID',$record.parentID,'-spiffeID',$identity,'-selector','k8s:ns:identity-acceptance','-selector',"k8s:pod-uid:$($created.metadata.uid)",'-selector','k8s:container-name:acceptance','-x509SVIDTTL','60')
    }
    $result = @{pod=$pod.metadata.name;uid=$created.metadata.uid;image=$image;startedUTC=[DateTime]::UtcNow.ToString('o')}
    $result | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $Directory "status-$suffix.json") -Encoding utf8NoBOM
    if ($ExerciseOutage) {
        $null = Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','identity-acceptance','wait','--for=jsonpath={.metadata.annotations.kognition\.dev/identity-stage}=awaiting-outage',"pod/$($pod.metadata.name)",'--timeout=100s')
        $agent = Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','identity-spire','get','daemonset','agent','-o','json') | ConvertFrom-Json
        if ($agent.spec.template.spec.nodeSelector -or $agent.spec.template.spec.containers[0].image -ne 'ghcr.io/spiffe/spire-agent:1.15.3') {throw 'Unexpected platform-agent configuration; outage refused'}
        $bundle = Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','identity-spire','exec','deployment/server','--','/opt/spire/bin/spire-server','bundle','show','-format','pem')
        if ($bundle -notmatch 'BEGIN CERTIFICATE') {throw 'Current issuer bootstrap bundle unavailable'}
        $patch = @{data=@{'bundle.crt'=$bundle}} | ConvertTo-Json -Compress
        $null = Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','identity-spire','patch','configmap','bundle','--type=merge','-p',$patch)
        try {
            $null = Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','identity-spire','patch','daemonset','agent','--type=merge','-p','{"spec":{"template":{"spec":{"nodeSelector":{"kognition.dev/acceptance-outage":"true"}}}}}')
            $null = Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','identity-acceptance','wait','--for=jsonpath={.metadata.annotations.kognition\.dev/identity-stage}=outage-observed',"pod/$($pod.metadata.name)",'--timeout=45s')
        } finally {
            $null = Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','identity-spire','patch','daemonset','agent','--type=merge','-p','{"spec":{"template":{"spec":{"nodeSelector":null}}}}')
        }
    }
    $logs = Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','identity-acceptance','logs',$pod.metadata.name,'--follow','--pod-running-timeout=60s')
    $logs | Set-Content -LiteralPath (Join-Path $Directory "status-$suffix.log") -Encoding utf8NoBOM
    if ($logs -notmatch '(?m)^PASS\s*$' -or $logs -match '(?m)^FAIL\s*$|^panic:') { Write-Output $logs; throw "One-shot acceptance failed: $($pod.metadata.name); evidence preserved, no automatic retry" }
    try {
        $null = Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','identity-acceptance','wait','--for=jsonpath={.status.phase}=Succeeded',"pod/$($pod.metadata.name)",'--timeout=210s')
    } finally {
        $logs = Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','identity-acceptance','logs',$pod.metadata.name)
        $logs | Set-Content -LiteralPath (Join-Path $Directory "status-$suffix.log") -Encoding utf8NoBOM
        Write-Output $logs
    }
}
if ($Action -eq 'Provision') {
    $logs = Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','identity-acceptance','logs','prepare')
    $prepare = Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','identity-acceptance','get','pod','prepare','-o','json') | ConvertFrom-Json
    if ($prepare.status.phase -ne 'Succeeded' -or $prepare.spec.containers[0].env.Where({$_.name -eq 'KOGNITION_IDENTITY_RUN_ID'}).value -ne $record.runID) { throw 'Provision requires successful run-bound preparation' }
    $agents = @{}
    foreach ($mode in @('kubernetes','standalone')) {
        if ($logs -notmatch "FIXTURE_AGENT mode=$mode namespace=identity-$mode name=worker UID=([a-f0-9-]{36})") { throw 'Actual backend Agent UID evidence required' }
        $agents[$mode] = $Matches[1]
    }
    $image = "kognition-identity-acceptance:$($record.runID)"
    $node = Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'get','node',"$($record.name)-control-plane",'-o','json') | ConvertFrom-Json
    $parentID = "spiffe://kognition.dev/spire/agent/k8s_psat/identity-fixture/$($node.metadata.uid)"
    $gatewayJSON = @{model=@{url='https://model.identity-acceptance.svc:9443';peerID='spiffe://kognition.dev/gateway/model'};tool=@{url='https://tool.identity-acceptance.svc:9443';peerID='spiffe://kognition.dev/gateway/tool'};memory=@{url='https://memory.identity-acceptance.svc:9443';peerID='spiffe://kognition.dev/gateway/memory'};communication=@{url='https://communication.identity-acceptance.svc:9443';peerID='spiffe://kognition.dev/gateway/communication'}} | ConvertTo-Json -Compress -Depth 6
    $registrations = @()
    $podUIDs = @{}
    foreach ($mode in @('kubernetes','standalone')) {
        $name = "proxy-$mode"
        $identity = "spiffe://kognition.dev/workspace/identity-$mode/agent/worker/incarnation/$($agents[$mode])"
        $container = @{
            name='workload-identity-proxy'; image=$image; imagePullPolicy='Never'; command=@('/kognition-workload-identity-proxy')
            securityContext=@{readOnlyRootFilesystem=$true;allowPrivilegeEscalation=$false;capabilities=@{drop=@('ALL')}}
            resources=@{requests=@{cpu='25m';memory='32Mi'};limits=@{cpu='250m';memory='128Mi'}}
            volumeMounts=@(@{name='workload-api';mountPath='/spiffe-workload-api';readOnly=$true})
            readinessProbe=@{exec=@{command=@('/kognition-workload-identity-proxy','--health-check')};periodSeconds=2;timeoutSeconds=2}
            env=@(
                @{name='KOGNITION_NAMESPACE';value="identity-$mode"}
                @{name='KOGNITION_AGENT_NAME';value='worker'}
                @{name='KOGNITION_AGENT_UID';value=$agents[$mode]}
                @{name='KOGNITION_SPIFFE_TRUST_DOMAIN';value='kognition.dev'}
                @{name='KOGNITION_SPIFFE_ENDPOINT_SOCKET';value='unix:///spiffe-workload-api/spire-agent.sock'}
                @{name='KOGNITION_SPIFFE_GATEWAY_ENDPOINTS';value=$gatewayJSON}
                @{name='KOGNITION_WORKLOAD_STATUS_LISTEN_ADDR';value='0.0.0.0:8097'}
                @{name='KOGNITION_WORKLOAD_STATUS_CONTROLLER_ID';value='spiffe://kognition.dev/controller/identity-status'}
            )
        }
        $pod = @{
            apiVersion='v1'; kind='Pod'
            metadata=@{name=$name;namespace='identity-acceptance';labels=@{app=$name;'app.kubernetes.io/managed-by'='kognition';'kognition.dev/agent'="identity-$mode-worker";'kognition.dev/identity-acceptance-run'=$record.runID}}
            spec=@{
                automountServiceAccountToken=$false;restartPolicy='Always';tolerations=@(@{operator='Exists'})
                securityContext=@{runAsNonRoot=$true;runAsUser=65532;seccompProfile=@{type='RuntimeDefault'}}
                containers=@($container)
                volumes=@(@{name='workload-api';csi=@{driver='csi.spiffe.io';readOnly=$true}})
            }
        }
        Add-FixtureObject $pod
        $created = Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','identity-acceptance','get','pod',$name,'-o','json') | ConvertFrom-Json
        $podUIDs[$mode] = $created.metadata.uid
        $null = Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','identity-spire','exec','deployment/server','--','/opt/spire/bin/spire-server','entry','create','-parentID',$parentID,'-spiffeID',$identity,'-selector','k8s:ns:identity-acceptance','-selector',"k8s:pod-uid:$($created.metadata.uid)",'-selector','k8s:container-name:workload-identity-proxy','-x509SVIDTTL','60')
        Add-FixtureObject @{apiVersion='v1';kind='Service';metadata=@{name=$name;namespace='identity-acceptance'};spec=@{selector=@{app=$name};ports=@(@{name='identity-status';port=8097;targetPort=8097})}}
        $registrations += @{namespace="identity-$mode";agentName='worker';agentUID=$agents[$mode];endpoint="https://$name.identity-acceptance.svc:8097"}
    }
    Add-FixtureObject @{apiVersion='v1';kind='ConfigMap';metadata=@{name='registry';namespace='identity-acceptance'};immutable=$true;data=@{'registry.json'=($registrations | ConvertTo-Json -Compress -Depth 5)}}
    $record | Add-Member -NotePropertyName image -NotePropertyValue $image -Force
    $record | Add-Member -NotePropertyName agents -NotePropertyValue $agents -Force
    $record | Add-Member -NotePropertyName proxyPodUIDs -NotePropertyValue $podUIDs -Force
    $record | Add-Member -NotePropertyName parentID -NotePropertyValue $parentID -Force
    $record | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $recordPath -Encoding utf8NoBOM
    foreach ($mode in @('kubernetes','standalone')) { $null = Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','identity-acceptance','wait','--for=condition=Ready',"pod/proxy-$mode",'--timeout=120s') }
}
if ($Action -in @('Prepare', 'Seed')) {
    if ($record.image) { throw 'Prepared fixtures cannot be implicitly reused' }
    $root = Split-Path -Parent $PSScriptRoot
    $image = "kognition-identity-acceptance:$($record.runID)"
    if ($Action -eq 'Prepare') {
    $oldGOOS, $oldGOARCH, $oldCGO = $env:GOOS, $env:GOARCH, $env:CGO_ENABLED
    try {
        $env:GOOS = 'linux'; $env:GOARCH = 'amd64'; $env:CGO_ENABLED = '0'
        $null = Invoke-FixtureCommand go @('test', '-c', '-tags', 'integration', '-o', (Join-Path $Directory 'acceptance'), './internal/traceability')
        $null = Invoke-FixtureCommand go @('build', '-o', (Join-Path $Directory 'proxy'), './cmd/kognition-workload-identity-proxy')
    } finally { $env:GOOS = $oldGOOS; $env:GOARCH = $oldGOARCH; $env:CGO_ENABLED = $oldCGO }
    $null = Invoke-FixtureCommand docker @('build', '--quiet', '-t', $image, '-f', (Join-Path $PSScriptRoot 'fixtures/Dockerfile.workload-identity'), $Directory)
    $archive = Join-Path $Directory 'images.tar'
    $null = Invoke-FixtureCommand docker @('save', '--platform', 'linux/amd64', '-o', $archive, $image, 'pgvector/pgvector:kind-local')
    $node = "$($record.name)-control-plane"
    $null = Invoke-FixtureCommand docker @('cp', $archive, "${node}:/var/identity-images.tar")
    $null = Invoke-FixtureCommand docker @('exec', $node, 'ctr', '--namespace=k8s.io', 'images', 'import', '--platform', 'linux/amd64', '/var/identity-images.tar')
    foreach ($namespace in @('identity-acceptance', 'identity-kubernetes', 'identity-standalone')) {
        Add-FixtureObject @{apiVersion='v1';kind='Namespace';metadata=@{name=$namespace;labels=@{'kognition.dev/identity-acceptance-run'=$record.runID}}}
    }
    $password = [guid]::NewGuid().ToString('N') + [guid]::NewGuid().ToString('N')
    Add-FixtureObject @{apiVersion='v1';kind='Secret';metadata=@{name='database';namespace='identity-acceptance'};stringData=@{password=$password;dsn="postgres://postgres:$password@database.identity-acceptance.svc:5432/postgres?sslmode=disable"}}
    Add-FixtureObject @{apiVersion='v1';kind='Pod';metadata=@{name='database';namespace='identity-acceptance';labels=@{app='identity-database'}};spec=@{automountServiceAccountToken=$false;tolerations=@(@{operator='Exists'});containers=@(@{name='database';image='pgvector/pgvector:kind-local';imagePullPolicy='Never';env=@(@{name='POSTGRES_PASSWORD';valueFrom=@{secretKeyRef=@{name='database';key='password'}}});resources=@{requests=@{cpu='50m';memory='128Mi'};limits=@{cpu='1';memory='512Mi'}};readinessProbe=@{exec=@{command=@('pg_isready','-U','postgres')};periodSeconds=2};volumeMounts=@(@{name='data';mountPath='/var/lib/postgresql/data'})});volumes=@(@{name='data';emptyDir=@{}})}}
    Add-FixtureObject @{apiVersion='v1';kind='Service';metadata=@{name='database';namespace='identity-acceptance'};spec=@{selector=@{app='identity-database'};ports=@(@{port=5432;targetPort=5432})}}
    $null = Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','identity-acceptance','wait','--for=condition=Ready','pod/database','--timeout=120s')
    $output = Get-Content -Raw -LiteralPath (Join-Path $root 'internal/store/postgres/migrations/001_initial_schema.sql') | & kubectl --kubeconfig $kubeconfig -n identity-acceptance exec -i database -- psql -U postgres -v ON_ERROR_STOP=1 2>&1
    if ($LASTEXITCODE -ne 0) { throw "Isolated schema initialization failed: $output" }
    } else {
        foreach ($namespace in @('identity-acceptance','identity-kubernetes','identity-standalone')) {
            $existing = Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'get','namespace',$namespace,'-o','json') | ConvertFrom-Json
            if ($existing.metadata.labels.'kognition.dev/identity-acceptance-run' -ne $record.runID) { throw 'Seed requires run-owned prepared namespaces' }
        }
        $null = Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','identity-acceptance','exec','database','--','psql','-U','postgres','-v','ON_ERROR_STOP=1','-c','SELECT count(*) FROM resources;')
    }
    foreach ($crd in @('agents','agentidentities')) {
        $null = Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'apply','-f',(Join-Path $root "deploy/helm/kognition/crds/kognition.dev_$crd.yaml"))
        $null = Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'wait','--for=condition=Established',"crd/$crd.kognition.dev",'--timeout=60s')
    }
    Add-FixtureObject @{apiVersion='v1';kind='ServiceAccount';metadata=@{name='runner';namespace='identity-acceptance'}}
    Add-FixtureObject @{apiVersion='rbac.authorization.k8s.io/v1';kind='ClusterRole';metadata=@{name='identity-acceptance-reader'};rules=@(@{apiGroups=@('');resources=@('nodes');verbs=@('list')},@{apiGroups=@('');resources=@('namespaces');resourceNames=@('kube-system');verbs=@('get')})}
    Add-FixtureObject @{apiVersion='rbac.authorization.k8s.io/v1';kind='ClusterRoleBinding';metadata=@{name='identity-acceptance-reader'};roleRef=@{apiGroup='rbac.authorization.k8s.io';kind='ClusterRole';name='identity-acceptance-reader'};subjects=@(@{kind='ServiceAccount';name='runner';namespace='identity-acceptance'})}
    Add-FixtureObject @{apiVersion='rbac.authorization.k8s.io/v1';kind='Role';metadata=@{name='identity-acceptance';namespace='identity-kubernetes'};rules=@(@{apiGroups=@('kognition.dev');resources=@('agents','agentidentities','agentidentities/status');verbs=@('get','create','update')})}
    Add-FixtureObject @{apiVersion='rbac.authorization.k8s.io/v1';kind='RoleBinding';metadata=@{name='identity-acceptance';namespace='identity-kubernetes'};roleRef=@{apiGroup='rbac.authorization.k8s.io';kind='Role';name='identity-acceptance'};subjects=@(@{kind='ServiceAccount';name='runner';namespace='identity-acceptance'})}
    $prepare = @{apiVersion='v1';kind='Pod';metadata=@{name='prepare';namespace='identity-acceptance'};spec=@{serviceAccountName='runner';restartPolicy='Never';tolerations=@(@{operator='Exists'});securityContext=@{runAsNonRoot=$true;runAsUser=65532;seccompProfile=@{type='RuntimeDefault'}};containers=@(@{name='acceptance';image=$image;imagePullPolicy='Never';args=@('-test.v','-test.run=^TestDeployedWorkloadIdentityStatusProjection$','-test.timeout=180s');securityContext=@{readOnlyRootFilesystem=$true;allowPrivilegeEscalation=$false;capabilities=@{drop=@('ALL')}};resources=@{requests=@{cpu='100m';memory='128Mi'};limits=@{cpu='2';memory='512Mi'}};volumeMounts=@(@{name='tmp';mountPath='/tmp'});env=@(@{name='KOGNITION_IDENTITY_DEPLOYED_ACCEPTANCE';value='true'},@{name='KOGNITION_IDENTITY_PREPARE';value='true'},@{name='KOGNITION_IDENTITY_CLUSTER_UID';value=$record.clusterUID},@{name='KOGNITION_IDENTITY_RUN_ID';value=$record.runID},@{name='KOGNITION_IDENTITY_POSTGRES_DSN';valueFrom=@{secretKeyRef=@{name='database';key='dsn'}}})});volumes=@(@{name='tmp';emptyDir=@{sizeLimit='16Mi'}})}}
    Add-FixtureObject $prepare
    $null = Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','identity-acceptance','wait','--for=jsonpath={.status.phase}=Succeeded','pod/prepare','--timeout=180s')
    $logs = Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','identity-acceptance','logs','prepare')
    $agents = @{}
    foreach ($mode in @('kubernetes','standalone')) {
        if ($logs -notmatch "FIXTURE_AGENT mode=$mode namespace=identity-$mode name=worker UID=([a-f0-9-]{36})") { throw "Actual $mode Agent UID absent from preparation evidence" }
        $agents[$mode] = $Matches[1]
    }
    $record | Add-Member -NotePropertyName image -NotePropertyValue $image -Force
    $record | Add-Member -NotePropertyName agents -NotePropertyValue $agents -Force
    $record | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $recordPath -Encoding utf8NoBOM
    Write-Output $logs
}
if ($Action -eq 'Cleanup') {
    if ($record.profile -eq 'candidate') {
        $runner=Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','candidate-acceptance','get','pod','candidate','-o','json') | ConvertFrom-Json
        if($runner.status.phase -notin @('Failed','Succeeded')){throw 'Candidate runner is still active; cleanup refused'}
        $logs=Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','candidate-acceptance','logs','candidate')
        $maps=Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','candidate-acceptance','get','configmaps','-o','json') | ConvertFrom-Json
        $unknown=(Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','candidate-acceptance','exec','database','--','psql','-U','postgres','-tAc','SELECT count(*) FROM model_dispatches WHERE response IS NULL;')).Trim()
        $pods=Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','candidate-acceptance','get','pods','-o','json') | ConvertFrom-Json
        foreach($pod in $pods.items | Where-Object {$_.metadata.name -like 'candidate*'}) {
            if($pod.metadata.labels.'kognition.dev/acceptance-run' -ne $record.runID -or $pod.status.phase -notin @('Failed','Succeeded')) {throw 'Candidate cleanup found an active or foreign runner'}
        }
        if($runner.status.phase -eq 'Failed' -and $ReviewCandidateOutcomes) {
            if($record.candidatekubernetes -notmatch '^candidate-[0-9a-f]{8}$' -or $logs -notmatch 'mode=standalone.*unknown-calls=1' -or $logs -notmatch '--- PASS: TestDeployedModelCandidatePromotionRollbackRecovery/standalone' -or $unknown -ne '1') {throw 'Reviewed original standalone outcome is missing'}
            $continuation=Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','candidate-acceptance','get','pod',$record.candidatekubernetes,'-o','json') | ConvertFrom-Json
            $attempt=$record.candidatekubernetes.Substring('candidate-'.Length)
            $receipt=Get-Content -Raw -LiteralPath (Join-Path $Directory "candidate-$attempt.json") | ConvertFrom-Json
            if($receipt.runID -ne $record.runID -or $receipt.mode -ne 'kubernetes' -or $receipt.pod -ne $record.candidatekubernetes -or $receipt.podUID -ne $continuation.metadata.uid -or $receipt.image -ne $continuation.spec.containers[0].image -or $continuation.status.phase -ne 'Succeeded') {throw 'Kubernetes continuation identity or terminal outcome differs from its receipt'}
            $continuedLogs=Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','candidate-acceptance','logs',$record.candidatekubernetes)
            if($continuedLogs -notmatch 'mode=kubernetes.*unknown-calls=1 usage-events=4 settlements=4 protected-unknown=1' -or $continuedLogs -notmatch '(?m)^PASS\s*$') {throw 'Reviewed exact Kubernetes accounting proof is missing'}
            $claims=@($maps.items | Where-Object {$_.metadata.labels.'kognition.dev/model-dispatch-record' -eq 'true' -and $_.metadata.name.EndsWith('-claim')})
            $results=@($maps.items | Where-Object {$_.metadata.labels.'kognition.dev/model-dispatch-record' -eq 'true' -and $_.metadata.name.EndsWith('-result')})
            $unresolved=@($claims | Where-Object {$_.metadata.name.Replace('-claim','-result') -notin $results.metadata.name})
            if($claims.Count -ne 6 -or $results.Count -ne 5 -or $unresolved.Count -ne 1 -or $unresolved[0].data.workspace -ne 'candidate-kubernetes') {throw 'Unexpected Kubernetes provider claims; cleanup refused'}
            foreach($result in $results) {
                $claim=$claims | Where-Object {$_.metadata.name -eq $result.metadata.name.Replace('-result','-claim')}
                if(-not $result.immutable -or -not $claim.immutable -or $result.data.claimUID -ne $claim.metadata.uid -or $result.data.operation -ne $claim.data.operation) {throw 'Kubernetes terminal claim binding changed'}
            }
            $ledgerMap=@($maps.items | Where-Object {$_.metadata.labels.'kognition.dev/token-ledger' -eq 'true' -and $_.metadata.annotations.'kognition.dev/quota-namespace' -eq 'candidate-kubernetes'})
            if($ledgerMap.Count -ne 1) {throw 'Expected Kubernetes protected quota ledger is missing'}
            $ledger=$ledgerMap[0].data.'ledger.json' | ConvertFrom-Json -AsHashtable
            $reservationID='reasoning-canary:'+$unresolved[0].data.operation
            if($ledger.reservations.Count -ne 1 -or -not $ledger.reservations[$reservationID].canary -or $ledger.canaryDispositions.Count -ne 4) {throw 'Expected retained unknown reservation or exact settlements changed'}
            $proof=(Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','candidate-acceptance','exec','database','--','psql','-U','postgres','-tAc',"SELECT (SELECT count(*) FROM model_dispatches), (SELECT count(*) FROM token_reservations WHERE canary), (SELECT count(*) FROM canary_reservation_dispositions), (SELECT count(*) FROM cost_records WHERE usage_purpose = 'model-version-canary');")).Trim()
            if($proof -ne '6|1|4|4') {throw 'Standalone retained claims/settlements/usage differ from the reviewed matrix'}
        } elseif($runner.status.phase -eq 'Failed') {
            $total=(Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','candidate-acceptance','exec','database','--','psql','-U','postgres','-tAc','SELECT count(*) FROM model_dispatches;')).Trim()
            if($total -ne '0' -or @($maps.items | Where-Object {$_.metadata.name -ne 'kube-root-ca.crt'}).Count -ne 0){throw 'Failed candidate run may retain provider claims; cleanup requires explicit outcome review'}
        } elseif($unknown -ne '1' -or $logs -notmatch 'mode=standalone.*unknown-calls=1' -or $logs -notmatch 'mode=kubernetes.*unknown-calls=1') {throw 'Expected synthetic unknown-case evidence is missing; cleanup refused'}
        $evidence=Join-Path $Directory ('retained-'+[guid]::NewGuid().ToString('N'));$null=New-Item -ItemType Directory -Path $evidence
        $logs | Set-Content -LiteralPath (Join-Path $evidence 'candidate.log') -Encoding utf8NoBOM
        foreach($pod in $pods.items | Where-Object {$_.metadata.name -like 'candidate-*'}) {
            $podLogs=Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','candidate-acceptance','logs',$pod.metadata.name)
            $podLogs | Set-Content -LiteralPath (Join-Path $evidence "$($pod.metadata.name).log") -Encoding utf8NoBOM
        }
        Copy-Item -LiteralPath $recordPath -Destination (Join-Path $evidence 'cluster.json')
        foreach($receipt in Get-ChildItem -LiteralPath $Directory -Filter 'candidate-*.json') {Copy-Item -LiteralPath $receipt.FullName -Destination $evidence}
        $pods | ConvertTo-Json -Depth 60 | Set-Content -LiteralPath (Join-Path $evidence 'candidate-pods.json') -Encoding utf8NoBOM
        $backup=Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','candidate-acceptance','exec','database','--','pg_dump','-U','postgres')
        $backup | Set-Content -LiteralPath (Join-Path $evidence 'database.sql') -Encoding utf8NoBOM
        $maps | ConvertTo-Json -Depth 60 | Set-Content -LiteralPath (Join-Path $evidence 'candidate-ledgers.json') -Encoding utf8NoBOM
        foreach($kind in @('models','cognitivequotas')){$snapshot=Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','candidate-kubernetes','get',$kind,'-o','json');$snapshot | Set-Content -LiteralPath (Join-Path $evidence "$kind.json") -Encoding utf8NoBOM}
        $classes=Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'get','modelclasses','-o','json');$classes | Set-Content -LiteralPath (Join-Path $evidence 'modelclasses.json') -Encoding utf8NoBOM
        $hashes=@(Get-ChildItem -LiteralPath $evidence -File | ForEach-Object {@{name=$_.Name;sha256=(Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash.ToLowerInvariant()}})
        @{runID=$record.runID;clusterUID=$record.clusterUID;outcome=$runner.status.phase;syntheticUnknownClaimsRetained=$unknown;artifacts=$hashes} | ConvertTo-Json -Depth 10 | Set-Content -LiteralPath (Join-Path $evidence 'index.json') -Encoding utf8NoBOM
        $null=Invoke-FixtureCommand kind @('delete','cluster','--name',$record.name,'--kubeconfig',$kubeconfig)
        if(@((Invoke-FixtureCommand kind @('get','clusters')) -split '\r?\n') -contains $record.name){throw 'Owned candidate cluster remained after cleanup'}
        $record | Add-Member -NotePropertyName retainedEvidence -NotePropertyValue $evidence -Force
        $record | Add-Member -NotePropertyName cleanedUTC -NotePropertyValue ([DateTime]::UtcNow.ToString('o')) -Force
        $record | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $recordPath -Encoding utf8NoBOM
        $record | ConvertTo-Json -Depth 8
        return
    }
    $approvals=Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','identity-kubernetes','get','runtimeapprovals','-o','json') | ConvertFrom-Json
    foreach ($approval in $approvals.items) {
        if ($approval.status.executionState -in @('Claimed','Uncertain') -or
            ($approval.status.preparationToken -and $approval.status.preparationDeadline -and [DateTimeOffset]::Parse($approval.status.preparationDeadline) -gt [DateTimeOffset]::UtcNow)) {
            throw 'Active or uncertain fixture execution must be resolved normally before cleanup'
        }
    }
    $active=(Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','identity-acceptance','exec','database','--','psql','-U','postgres','-tAc',"SELECT count(*) FROM runtime_approvals WHERE execution_state IN ('Claimed','Uncertain') OR (execution_state='NotStarted' AND preparation_token IS NOT NULL AND preparation_deadline>NOW());")).Trim()
    if ($active -ne '0') {throw 'PostgreSQL retains active or uncertain fixture execution; cleanup refused'}
    $evidence=Join-Path $Directory ('retained-'+[guid]::NewGuid().ToString('N'))
    $null=New-Item -ItemType Directory -Path $evidence
    $backup=Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','identity-acceptance','exec','database','--','pg_dump','-U','postgres')
    $backup | Set-Content -LiteralPath (Join-Path $evidence 'database.sql') -Encoding utf8NoBOM
    foreach ($kind in @('agents','agentidentities','agentinstances','runtimeapprovals','runtimeapprovaltombstones','runtimeapprovalquotatickets')) {
        $snapshot=Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','identity-kubernetes','get',$kind,'-o','json')
        $snapshot | Set-Content -LiteralPath (Join-Path $evidence "$kind.json") -Encoding utf8NoBOM
    }
    $maps=Invoke-FixtureCommand kubectl @('--kubeconfig',$kubeconfig,'-n','identity-kubernetes','get','configmaps','-o','json') | ConvertFrom-Json
    @($maps.items | Where-Object {$_.metadata.name -like 'kognition-*'}) | ConvertTo-Json -Depth 60 |
        Set-Content -LiteralPath (Join-Path $evidence 'receipt-ledgers.json') -Encoding utf8NoBOM
    $index=@(Get-ChildItem -LiteralPath $Directory -Filter 'status-*.log' | ForEach-Object {@{name=$_.Name;sha256=(Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash.ToLowerInvariant();passed=((Get-Content -Raw -LiteralPath $_.FullName) -match '(?m)^PASS\s*$')}})
    $index+=@(Get-ChildItem -LiteralPath $evidence -File | ForEach-Object {@{name=$_.Name;sha256=(Get-FileHash -LiteralPath $_.FullName -Algorithm SHA256).Hash.ToLowerInvariant()}})
    @{runID=$record.runID;clusterUID=$record.clusterUID;capturedUTC=[DateTime]::UtcNow.ToString('o');activeExecutionCount=0;artifacts=$index} |
        ConvertTo-Json -Depth 10 | Set-Content -LiteralPath (Join-Path $evidence 'index.json') -Encoding utf8NoBOM
    $null = Invoke-FixtureCommand kind @('delete', 'cluster', '--name', $record.name)
    $remaining=@(Invoke-FixtureCommand kind @('get','clusters') | ForEach-Object {$_ -split '\r?\n'})
    if ($remaining -contains $record.name) {throw 'Task-owned cluster remained after cleanup'}
    $record | Add-Member -NotePropertyName retainedEvidence -NotePropertyValue $evidence -Force
    $record | Add-Member -NotePropertyName cleanedUTC -NotePropertyValue ([DateTime]::UtcNow.ToString('o')) -Force
    $record | ConvertTo-Json | Set-Content -LiteralPath $recordPath -Encoding utf8NoBOM
}
$record | ConvertTo-Json