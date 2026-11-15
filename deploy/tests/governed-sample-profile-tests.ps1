# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

#Requires -Version 7.0
$ErrorActionPreference = 'Stop'
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$originalDirectory = (Get-Location).Path
$previousProfile = $env:KOGNITION_SAMPLE_PROFILE_PATH
$previousVirtualEnv = $env:VIRTUAL_ENV
$scratch = (New-Item -ItemType Directory -Path (Join-Path $repoRoot ".sample-profile-tests-$([guid]::NewGuid().ToString('N'))")).FullName
. (Join-Path $repoRoot 'samples\azure-settings.ps1')
Import-Module powershell-yaml
$global:SampleProfileTestEvents = [Collections.Generic.List[object]]::new()
$global:SampleProfileTestFailure = ''
$global:SampleProfileTestInstance = $null
$global:SampleProfileTestEnrollment = 'kognition-work'

function Assert-True([bool]$Condition, [string]$Message) {
    if (-not $Condition) { throw $Message }
}
function Assert-ProbeSettingsEqual([System.Collections.IDictionary]$Actual, [System.Collections.IDictionary]$Expected, [string]$Message) {
    Assert-True ($null -ne $Actual -and $Actual.Count -eq $Expected.Count) $Message
    foreach ($key in $Expected.Keys) {
        Assert-True ($Actual.Contains($key) -and $Actual[$key] -ceq $Expected[$key]) "$Message ($key)"
    }
}
function Assert-Throws([scriptblock]$Action, [string]$Pattern) {
    $failure = $null
    try { & $Action | Out-Null } catch { $failure = $_.Exception.Message }
    Assert-True ($failure -and $failure -like $Pattern) "Expected failure '$Pattern', got '$failure'."
}
function global:kubectl {
    $command = $args -join ' '
    $body = @($input) -join "`n"
    $documents = if ($body -and $command -match '^(apply|label) ') { @(ConvertFrom-Yaml -Yaml $body -AllDocuments) } else { @() }
    $global:SampleProfileTestEvents.Add(@{ command = $command; documents = $documents })
    $global:LASTEXITCODE = 0
    if ($command -like 'get deployment *') {
        return (@{ items = @(@{ spec = @{ template = @{
            metadata = @{ labels = @{ 'app.kubernetes.io/instance' = 'kognition' } }
            spec = @{ containers = @(@{ env = @(@{ name = 'KOGNITION_MANAGED_NAMESPACES'; value = $global:SampleProfileTestEnrollment }) }) }
        } } }) } | ConvertTo-Json -Depth 20 -Compress)
    }
    if ($command -like 'get secret *') {
        Assert-True ($command -notmatch 'kognition-system|kognition-kb-pgvector') 'Governed install must never read the platform admin database Secret.'
        return '{"data":{"username":"ZmFrZQ==","password":"ZmFrZQ==","ca.crt":"ZmFrZQ=="}}'
    }
    if ($command -like 'get crd secretrefs.kognition.dev*') { return }
    if ($command -like 'get namespace *') { return 'namespace/test' }
    if ($command -like 'rollout status*') {
        if ($global:SampleProfileTestFailure -eq 'rollout') { $global:LASTEXITCODE = 1 }
        return
    }
    if ($command -like 'wait --for=condition=Ready=True modelclass/*') { return }
    if ($command -like 'get agent hello-agent *') { return 'agent/hello-agent' }
    if ($command -like 'get agentinstance *') {
        if ($global:SampleProfileTestInstance) { return ($global:SampleProfileTestInstance | ConvertTo-Json -Depth 10) }
        return
    }
    if ($command -like 'create namespace*') { return '{"apiVersion":"v1","kind":"Namespace","metadata":{"name":"kognition-providers"}}' }
    if ($command -like 'create configmap*') { return '{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"kognition-docs","namespace":"kognition-providers"}}' }
    if ($command -like 'apply *') {
        if ($global:SampleProfileTestFailure -eq 'credentials' -and @($documents | Where-Object { $_.kind -eq 'SecretRef' }).Count -gt 0) { $global:LASTEXITCODE = 1 }
        return
    }
    throw "Unexpected Kubernetes operation: $command"
}
function python { $global:LASTEXITCODE = 0 }
function python3 { $global:LASTEXITCODE = 0 }
function global:helm {
    $global:LASTEXITCODE = 0
    $global:SampleProfileTestEvents.Add(@{ command = "helm $($args -join ' ')"; documents = @() })
}

try {
    $env:VIRTUAL_ENV = $null
    $map = @{
        default = 'kognition-work'; 'kognition-demo' = 'kognition-samples-demo'
        'samples-governance' = 'kognition-samples-governance'; 'samples-multi-agent' = 'kognition-samples-multi-agent'
        'samples-azure-blob' = 'kognition-samples-azure-blob'; production = 'kognition-samples-production'; development = 'kognition-samples-development'
        'samples-azure-blob-services' = 'kognition-providers'; 'samples-s3-blob-services' = 'kognition-providers'; 'samples-kognition-docs-services' = 'kognition-providers'
        'samples-tool-sandbox' = 'kognition-providers'
    }
    $profile = @{
        schemaVersion = 1; namespaceMap = $map; readinessTimeoutSeconds = 900
        knowledgeBackend = @{
            name = 'sample-pgvector-v1'; host = 'postgres.kognition-providers.svc.cluster.local'; port = 5432
            database = 'sample_knowledge'; table = 'sample_chunks'; embeddingDimensions = 1536
            credentialsSecretRef = @{ name = 'sample-db'; usernameKey = 'username'; passwordKey = 'password' }
            tls = @{ mode = 'verify-full'; caSecretRef = @{ name = 'sample-ca'; key = 'ca.crt' } }
        }
        fixtures = @{
            azuriteImage = "private.example/azurite@sha256:$('1' * 64)"
            s3Image = "private.example/seaweedfs@sha256:$('2' * 64)"
            docsImage = "private.example/docs@sha256:$('3' * 64)"
        }
        budgets = @{ agents = 64; tools = 32; workflows = 32; maxInputTokens = 1000000; maxOutputTokens = 200000 }
    }
    $profilePath = Join-Path $scratch 'profile.json'
    $profile | ConvertTo-Json -Depth 20 | Set-Content -LiteralPath $profilePath
    $env:KOGNITION_SAMPLE_PROFILE_PATH = $profilePath
    Assert-True ((Get-AzureSampleProfile).knowledgeBackend.name -eq 'sample-pgvector-v1') 'Explicit profile is parsed.'
    Assert-True ((Get-AzureSampleReadinessTimeoutSeconds) -eq 900) 'Governed readiness uses the explicitly reviewed 15-minute indexing budget.'
    Assert-True ((Get-AzureSampleReadinessTimeoutSeconds -Profile $null) -eq 300) 'Unprofiled readiness preserves the legacy five-minute budget.'

    foreach ($case in @(
        { param($p) $p.schemaVersion = 2 },
        { param($p) $p.namespaceMap.Remove('default') },
        { param($p) $p.namespaceMap.Remove('samples-tool-sandbox') },
        { param($p) $p.namespaceMap.production = 'kognition-work' },
        { param($p) $p.knowledgeBackend.tls.mode = 'disable' },
        { param($p) $p.knowledgeBackend.credentialsSecretRef.Remove('passwordKey') },
        { param($p) $p.knowledgeBackend.embeddingDimensions = 3072 },
        { param($p) $p.fixtures.s3Image = 'minio:latest' },
        { param($p) $p.budgets.agents = 30 },
        { param($p) $p.readinessTimeoutSeconds = 59 },
        { param($p) $p.readinessTimeoutSeconds = 3601 },
        { param($p) $p.readinessTimeoutSeconds = '900' },
        { param($p) $p.Remove('readinessTimeoutSeconds') }
    )) {
        $invalid = ConvertFrom-Yaml -Yaml ($profile | ConvertTo-Json -Depth 20)
        & $case $invalid
        $invalid | ConvertTo-Json -Depth 20 | Set-Content -LiteralPath (Join-Path $scratch 'invalid.json')
        Assert-Throws { Get-AzureSampleProfile -Path (Join-Path $scratch 'invalid.json') } '*SAMPLE_PROFILE*'
    }

    foreach ($relative in @('knowledge-base-blob-azure\manifests.yaml', 'knowledge-base-blob-s3\manifests.yaml', 'knowledge-base-kognition-docs\kb-class.yaml')) {
        $documents = @(ConvertFrom-Yaml -Yaml (Read-AzureSampleManifest -Path (Join-Path $repoRoot "samples\$relative") -Settings @{}) -AllDocuments)
        $backend = @($documents | Where-Object { $_.kind -eq 'PgvectorBackend' })
        $class = @($documents | Where-Object { $_.kind -eq 'KnowledgeBaseClass' })
        Assert-True ($backend.Count -eq 1 -and $backend[0].metadata.namespace -eq 'kognition-work') "$relative requires a namespace-local backend."
        Assert-True ($backend[0].spec.tls.mode -eq 'verify-full' -and $backend[0].spec.credentialsSecretRef.name -eq 'sample-db') 'Backend uses reviewed TLS and scoped Secret references.'
        Assert-True (-not $class[0].spec.Contains('vectorStore') -and $class[0].spec.backendRef.name -eq 'sample-pgvector-v1') 'Class must use governed backendRef without a raw connection.'
        Assert-True ($class[0].spec.chunkStrategy.size -eq 512 -and $class[0].spec.embeddingModelRef.name -eq 'text-embedding-3-small') 'Chunk and embedding identities are preserved.'
        Assert-True (@($documents | Where-Object { $_.kind -eq 'KnowledgeBase' -and $_.spec.refreshPolicy.mode -ne 'onDemand' }).Count -eq 0) 'Governed indexing is bounded on demand.'
        Assert-True (@($documents | Where-Object { $_.kind -eq 'Agent' -and -not $_.spec.knowledgeBaseClaims }).Count -eq 0) 'Knowledge Agents retain the claim path.'
    }
    $inline = @(ConvertFrom-Yaml -Yaml (Read-AzureSampleManifest -Path (Join-Path $repoRoot 'samples\knowledge-base\manifests.yaml') -Settings @{}) -AllDocuments)
    Assert-True (@($inline | Where-Object { $_.kind -eq 'PgvectorBackend' }).Count -eq 0) 'Inline knowledge must remain inline.'
    $providerRuns = @(
        @{ path = 'knowledge-base-blob-azure\manifests.yaml'; name = 'azure-docs-reindex-now'; kb = 'azure-docs-kb' },
        @{ path = 'knowledge-base-blob-s3\manifests.yaml'; name = 's3-docs-reindex-now'; kb = 's3-docs-kb' },
        @{ path = 'knowledge-base-kognition-docs\indexrun.yaml'; name = 'kognition-docs-reindex-now'; kb = 'kognition-docs-kb' }
    )
    $legacySettings = @{
        MS_FOUNDRY_PROJECT_ENDPOINT = 'https://fake.example/api/projects/fake'; MS_FOUNDRY_MODEL = 'gpt-6-luna'
        MS_FOUNDRY_EMBEDDING_MODEL = 'text-embedding-3-small'; MS_FOUNDRY_INVOCATION_CONTRACT = 'openai-tool-chat-v1'
    }
    $probePath = Join-Path $repoRoot 'samples\probes\manifests.yaml'
    $originalProbeYaml = Get-Content -LiteralPath $probePath -Raw
    $originalProbeAgent = @(ConvertFrom-Yaml -Yaml $originalProbeYaml -AllDocuments | Where-Object { $_.kind -eq 'Agent' })[0]
    foreach ($settings in @(@{}, $legacySettings)) {
        $env:KOGNITION_SAMPLE_PROFILE_PATH = $profilePath
        $probeAgents = @(ConvertFrom-Yaml -Yaml (Read-AzureSampleManifest -Path $probePath -Settings $settings) -AllDocuments | Where-Object { $_.kind -eq 'Agent' })
        Assert-True ($probeAgents.Count -eq 1 -and $probeAgents[0].metadata.name -eq 'monitored-agent' -and $probeAgents[0].metadata.namespace -eq 'kognition-work') 'Governed rendering retains monitored-agent in the mapped namespace.'
        $probes = $probeAgents[0].spec.probes
        Assert-True (-not $probes.Contains('reasoningProbe') -and $probes.Count -eq 2) 'Governed monitored-agent omits recurring paid reasoning canaries without inventing probe API fields.'
        foreach ($name in @('healthProbe', 'readinessProbe')) {
            Assert-ProbeSettingsEqual $probes[$name] $originalProbeAgent.spec.probes[$name] "Governed monitored-agent preserves every $name setting."
        }
        $env:KOGNITION_SAMPLE_PROFILE_PATH = $null
        $legacyProbeYaml = Read-AzureSampleManifest -Path $probePath -Settings $settings
        if ($settings.Count -eq 0) {
            Assert-True ($legacyProbeYaml -ceq $originalProbeYaml) 'Unconfigured legacy probe rendering is byte-for-byte unchanged.'
        }
        $legacyProbeAgent = @(ConvertFrom-Yaml -Yaml $legacyProbeYaml -AllDocuments | Where-Object { $_.kind -eq 'Agent' })[0]
        Assert-True ($legacyProbeAgent.spec.probes.Count -eq $originalProbeAgent.spec.probes.Count) 'Legacy monitored-agent retains all three probe kinds with or without Azure model settings.'
        foreach ($name in $originalProbeAgent.spec.probes.Keys) {
            Assert-ProbeSettingsEqual $legacyProbeAgent.spec.probes[$name] $originalProbeAgent.spec.probes[$name] "Legacy monitored-agent preserves every $name setting with or without Azure model settings."
        }
    }
    $env:KOGNITION_SAMPLE_PROFILE_PATH = $profilePath
    foreach ($case in $providerRuns) {
        $path = Join-Path $repoRoot "samples\$($case.path)"
        $rendered = Read-AzureSampleManifest -Path $path -Settings @{}
        $documents = if ([string]::IsNullOrWhiteSpace($rendered)) { @() } else { @(ConvertFrom-Yaml -Yaml $rendered -AllDocuments) }
        Assert-True (@($documents | Where-Object { $_.kind -eq 'KnowledgeBaseIndexRun' }).Count -eq 0) "$($case.path) must rely on automatic initial provider indexing without a redundant named run."
        if ($case.path -like '*\manifests.yaml') {
            $knowledgeBases = @($documents | Where-Object { $_.kind -eq 'KnowledgeBase' })
            Assert-True ($knowledgeBases.Count -eq 1 -and $knowledgeBases[0].metadata.name -eq $case.kb -and $knowledgeBases[0].spec.refreshPolicy.mode -eq 'onDemand') "$($case.path) retains its initially indexed KB."
        } else {
            Assert-True ([string]::IsNullOrWhiteSpace($rendered)) 'The governed docs indexrun-only manifest renders no document.'
        }
        $env:KOGNITION_SAMPLE_PROFILE_PATH = $null
        Assert-True ((Read-AzureSampleManifest -Path $path -Settings @{}) -ceq (Get-Content -LiteralPath $path -Raw)) "$($case.path) preserves byte-for-byte unconfigured legacy rendering."
        $legacy = @(ConvertFrom-Yaml -Yaml (Read-AzureSampleManifest -Path $path -Settings $legacySettings) -AllDocuments)
        $runs = @($legacy | Where-Object { $_.kind -eq 'KnowledgeBaseIndexRun' })
        Assert-True ($runs.Count -eq 1 -and $runs[0].metadata.name -eq $case.name -and $runs[0].metadata.namespace -eq 'default' -and $runs[0].spec.knowledgeBaseName -eq $case.kb) "$($case.path) retains its named run with legacy Azure model settings."
        $env:KOGNITION_SAMPLE_PROFILE_PATH = $profilePath
    }
    $inlineRunPath = Join-Path $repoRoot 'samples\knowledge-base\indexrun.yaml'
    foreach ($selectedProfile in @($profilePath, $null)) {
        $env:KOGNITION_SAMPLE_PROFILE_PATH = $selectedProfile
        $runs = @(ConvertFrom-Yaml -Yaml (Read-AzureSampleManifest -Path $inlineRunPath -Settings @{}) -AllDocuments | Where-Object { $_ })
        Assert-True ($runs.Count -eq 1 -and $runs[0].kind -eq 'KnowledgeBaseIndexRun' -and $runs[0].metadata.name -eq 'orbital-notes-reindex-now' -and $runs[0].spec.knowledgeBaseName -eq 'orbital-notes-kb') 'Both governed and legacy rendering retain the inline native reindex demo.'
        $expectedNamespace = if ($selectedProfile) { 'kognition-work' } else { 'default' }
        Assert-True ($runs[0].metadata.namespace -eq $expectedNamespace) 'The retained inline reindex demo uses the selected namespace.'
    }
    $env:KOGNITION_SAMPLE_PROFILE_PATH = $profilePath
    $retainedRunPath = Join-Path $scratch 'retained-indexruns.yaml'
    @(
        @{ name = 'separate-reviewed-run'; namespace = 'default'; kb = 'azure-docs-kb' },
        @{ name = 'azure-docs-reindex-now'; namespace = 'other-workload'; kb = 'azure-docs-kb' },
        @{ name = 'azure-docs-reindex-now'; namespace = 'default'; kb = 'other-kb' }
    ) | ForEach-Object {
        ConvertTo-AzureSampleYaml -Data @{
            apiVersion = 'kognition.dev/v1alpha1'; kind = 'KnowledgeBaseIndexRun'
            metadata = @{ name = $_.name; namespace = $_.namespace }; spec = @{ knowledgeBaseName = $_.kb }
        }
    } | Set-Content -LiteralPath $retainedRunPath
    $retainedRuns = @(ConvertFrom-Yaml -Yaml (Read-AzureSampleManifest -Path $retainedRunPath -Settings @{}) -AllDocuments | Where-Object { $_ })
    Assert-True ($retainedRuns.Count -eq 3) 'Governed exclusions are limited to the exact bundled run, namespace and KB identities.'
    $emptyRunPath = Join-Path $repoRoot 'samples\knowledge-base-kognition-docs\indexrun.yaml'
    $global:SampleProfileTestEvents.Clear()
    $global:LASTEXITCODE = 1
    Invoke-AzureSampleApply -Path $emptyRunPath
    Assert-True ($LASTEXITCODE -eq 0) 'An excluded apply is a successful no-op even after a previous native command failure.'
    $global:LASTEXITCODE = 1
    Invoke-AzureSampleDelete -Path $emptyRunPath
    Assert-True ($LASTEXITCODE -eq 0) 'An excluded delete is a successful no-op even after a previous native command failure.'
    Assert-True ($global:SampleProfileTestEvents.Count -eq 0) 'Empty governed indexrun rendering must not reach kubectl apply or delete.'
    $schedule = @(ConvertFrom-Yaml -Yaml (Read-AzureSampleManifest -Path (Join-Path $repoRoot 'samples\knowledge-base\indexschedule.yaml') -Settings @{}) -AllDocuments)
    Assert-True $schedule[0].spec.suspend 'Governed profile suspends index schedules.'
    foreach ($fixture in @(
        @{ path = 'knowledge-base-blob-azure\azurite.yaml'; key = 'azuriteImage' },
        @{ path = 'knowledge-base-blob-s3\s3-store.yaml'; key = 's3Image' },
        @{ path = 'knowledge-base-kognition-docs\docs-server.yaml'; key = 'docsImage' },
        @{ path = 'workflows\tool-sandbox.yaml'; key = 'docsImage' }
    )) {
        $documents = @(ConvertFrom-Yaml -Yaml (Read-AzureSampleManifest -Path (Join-Path $repoRoot "samples\$($fixture.path)") -Settings @{}) -AllDocuments)
        Assert-True (@($documents | Where-Object { $_.kind -eq 'Namespace' })[0].metadata.name -eq 'kognition-providers') 'Fixture Namespace name is mapped.'
        $deployment = @($documents | Where-Object { $_.kind -eq 'Deployment' })[0]
        Assert-True ($deployment.metadata.namespace -eq 'kognition-providers' -and $deployment.spec.template.spec.containers[0].image -eq $profile.fixtures[$fixture.key]) 'Fixture namespace and exact digest are honored.'
    }
    $toolDocuments = @(ConvertFrom-Yaml -Yaml (Read-AzureSampleManifest -Path (Join-Path $repoRoot 'samples\workflows\tool-steps-http.yaml') -Settings @{}) -AllDocuments)
    $toolEndpoints = @($toolDocuments | Where-Object { $_.kind -eq 'Tool' } | ForEach-Object { $_.spec.endpoint })
    Assert-True ($toolEndpoints.Count -gt 0 -and @($toolEndpoints | Where-Object { $_ -notlike 'http://tool-sandbox.kognition-providers.svc.cluster.local:8080/*' }).Count -eq 0) 'Workflow Tool endpoints follow the native fixture namespace mapping.'
    $global:SampleProfileTestEvents.Clear()
    Invoke-AzureSampleApply -Path (Join-Path $repoRoot 'samples\workflows\tool-sandbox.yaml')
    Assert-True (@($global:SampleProfileTestEvents | Where-Object { $_.command -eq 'rollout status deployment/tool-sandbox -n kognition-providers --timeout=900s' }).Count -eq 1) 'Governed native Tool fixture must pass bounded rollout readiness.'
    $global:SampleProfileTestFailure = 'rollout'
    Assert-Throws { Invoke-AzureSampleApply -Path (Join-Path $repoRoot 'samples\workflows\tool-sandbox.yaml') } '*Tool fixture did not become ready*'
    $global:SampleProfileTestFailure = ''
    $env:KOGNITION_SAMPLE_PROFILE_PATH = $null
    $legacyToolFixture = Read-AzureSampleManifest -Path (Join-Path $repoRoot 'samples\workflows\tool-sandbox.yaml') -Settings @{}
    Assert-True ($legacyToolFixture -ceq (Get-Content -LiteralPath (Join-Path $repoRoot 'samples\workflows\tool-sandbox.yaml') -Raw)) 'Unprofiled Tool fixture content preserves its legacy namespace and image.'
    $env:KOGNITION_SAMPLE_PROFILE_PATH = $profilePath
    $referencePath = Join-Path $scratch 'references.yaml'
    @'
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata: {name: references, namespace: default}
subjects: [{kind: ServiceAccount, name: worker, namespace: default}]
roleRef: {kind: Role, name: default}
---
apiVersion: kognition.dev/v1alpha1
kind: Agent
metadata: {name: dns}
spec:
  runtime: {class: default}
  instructions: {inline: "http://minio.samples-s3-blob-services.svc.cluster.local:9000"}
  arguments: ["http://azurite.samples-azure-blob-services.svc:10000", "default"]
'@ | Set-Content -LiteralPath $referencePath
    $references = @(ConvertFrom-Yaml -Yaml (Read-AzureSampleManifest -Path $referencePath -Settings @{}) -AllDocuments)
    Assert-True ($references[0].subjects[0].namespace -eq 'kognition-work' -and $references[0].roleRef.name -eq 'default') 'RBAC namespace references map without rewriting generic names.'
    Assert-True ($references[1].metadata.namespace -eq 'kognition-work' -and $references[1].spec.runtime.class -eq 'default') 'Implicit namespaces map without rewriting runtime classes.'
    Assert-True ($references[1].spec.instructions.inline -eq 'http://minio.kognition-providers.svc.cluster.local:9000') 'Service DNS references map structurally.'
    Assert-True ($references[1].spec.arguments[0] -eq 'http://azurite.kognition-providers.svc:10000' -and $references[1].spec.arguments[1] -eq 'default') 'DNS references inside arrays map without changing generic values.'
    $quota = @(ConvertFrom-Yaml -Yaml (Read-AzureSampleManifest -Path (Join-Path $repoRoot 'samples\governance\quota.yaml') -Settings @{}) -AllDocuments)[0]
    Assert-True ($quota.spec.hard.agents -eq 64 -and $quota.spec.tokenBudget.maxOutputTokens -eq 200000) 'Full-bundle quota stays sufficient and bounded.'
    Assert-True (@(Get-AzureSampleQuotaHelmArguments) -contains 'quota.hard.agents=64') 'Helm uses the same quota contract.'

    # Exercise the supported full-install entrypoint: it must honor the supplied
    # profile before its safety refusal and restore the caller's environment.
    New-Item -ItemType Directory -Path (Join-Path $scratch 'samples') | Out-Null
    Copy-Item (Join-Path $repoRoot 'make.ps1') (Join-Path $scratch 'make.ps1')
    Copy-Item (Join-Path $repoRoot 'samples\azure-settings.ps1') (Join-Path $scratch 'samples\azure-settings.ps1')
    @'
MS_FOUNDRY_PROJECT_ENDPOINT=https://fake.example/api/projects/fake
MS_FOUNDRY_MODEL=gpt-6-luna
MS_FOUNDRY_EMBEDDING_MODEL=text-embedding-3-small
MS_FOUNDRY_INVOCATION_CONTRACT=openai-tool-chat-v1
AZURE_TENANT_ID=fake-tenant
AZURE_CLIENT_ID=fake-client
AZURE_CLIENT_SECRET=fake-test-only
'@ | Set-Content -LiteralPath (Join-Path $scratch '.env')
    $env:KOGNITION_SAMPLE_PROFILE_PATH = $null
    $global:SampleProfileTestEvents.Clear()
    Assert-Throws { & (Join-Path $scratch 'make.ps1') samples-full-install "SAMPLE_PROFILE=$profilePath" } '*kognition-samples-demo*'
    Assert-True (-not $env:KOGNITION_SAMPLE_PROFILE_PATH) 'Profile process environment is restored after failure.'
    Assert-True ($global:SampleProfileTestEvents.Count -eq 1 -and $global:SampleProfileTestEvents[0].command -like 'get deployment *') 'Enrollment refusal precedes every sample write.'
    $global:SampleProfileTestEvents.Clear()
    Assert-Throws { & (Join-Path $scratch 'make.ps1') samples-full-install "SAMPLE_PROFILE=$(Join-Path $scratch 'invalid.json')" } '*SAMPLE_PROFILE*'
    Assert-True ($global:SampleProfileTestEvents.Count -eq 0) 'Invalid profile fails before even accessing Kubernetes.'
    $chartDirectory = New-Item -ItemType Directory -Path (Join-Path $scratch 'samples\helm-governance')
    Set-Content -LiteralPath (Join-Path $chartDirectory.FullName 'Chart.yaml') -Value 'name: helm-governance'
    $global:SampleProfileTestEvents.Clear()
    & (Join-Path $scratch 'make.ps1') samples-install SAMPLE=helm-governance "SAMPLE_PROFILE=$profilePath"
    $helmCall = @($global:SampleProfileTestEvents | Where-Object { $_.command -like 'helm *' })[0].command
    Assert-True ($helmCall -like '*--namespace kognition-work*' -and $helmCall -like '*namespace=kognition-work*' -and $helmCall -like '*quota.hard.agents=64*') 'Targeted Helm installation uses the same mapped namespace and bounded quota.'
    Assert-True (-not $env:KOGNITION_SAMPLE_PROFILE_PATH) 'Profile process environment is restored after success.'
    $credentialText = Get-Content -LiteralPath (Join-Path $scratch 'foundry-creds.yaml') -Raw
    Assert-True ($credentialText -like '*namespace: kognition-work*' -and $credentialText -notlike '*namespace: default*') 'Generated credentials use the mapped namespace.'
    $workflowDirectory = New-Item -ItemType Directory -Path (Join-Path $scratch 'samples\workflows')
    Copy-Item (Join-Path $repoRoot 'samples\workflows\tool-sandbox.yaml') (Join-Path $workflowDirectory.FullName 'tool-sandbox.yaml')
    Set-Content -LiteralPath (Join-Path $workflowDirectory.FullName 'a-dependent-workflow.yaml') -Value 'kind: Workflow'
    $global:SampleProfileTestEnrollment = @($map.Values | Select-Object -Unique) -join ','
    $global:SampleProfileTestFailure = 'rollout'
    $global:SampleProfileTestEvents.Clear()
    Assert-Throws { & (Join-Path $scratch 'make.ps1') samples-full-install "SAMPLE_PROFILE=$profilePath" } '*Tool fixture did not become ready*'
    Assert-True (@($global:SampleProfileTestEvents | Where-Object { $_.command -eq 'rollout status deployment/tool-sandbox -n kognition-providers --timeout=900s' }).Count -eq 1) 'Full install requires the mapped native Tool fixture to become ready.'
    Assert-True (@($global:SampleProfileTestEvents | ForEach-Object { $_.documents } | Where-Object { $_.kind -eq 'Workflow' }).Count -eq 0) 'Full install applies and waits for the native Tool fixture before dependent Workflow manifests.'
    Assert-True (-not $env:KOGNITION_SAMPLE_PROFILE_PATH) 'Native Tool readiness refusal restores the caller profile environment.'
    $global:SampleProfileTestFailure = ''
    Set-Location -LiteralPath $repoRoot
    $env:KOGNITION_SAMPLE_PROFILE_PATH = $profilePath

    # Native tool boundary doubles preserve the public orchestration and renderer.
    function Invoke-AzureSampleFixtureUpload {
        param($Provider, $Namespace, $Python, $AccountKey)
        $global:SampleProfileTestEvents.Add(@{ command = "verified-upload $Provider $Namespace"; documents = @() })
        if ($global:SampleProfileTestFailure -eq 'upload') { throw 'Verified upload failed.' }
    }
    foreach ($source in @('azure', 's3', 'docs')) {
        $directory = switch ($source) { azure { 'knowledge-base-blob-azure' }; s3 { 'knowledge-base-blob-s3' }; docs { 'knowledge-base-kognition-docs' } }
        $global:SampleProfileTestEvents.Clear()
        Invoke-AzureSampleKnowledgeInstall -Source $source -SampleDirectory (Join-Path $repoRoot "samples\$directory")
        $events = @($global:SampleProfileTestEvents)
        $applied = @($events | Where-Object { $_.command -like 'apply *' } | ForEach-Object { $_.documents })
        Assert-True (@($applied | Where-Object { $_.kind -eq 'KnowledgeBase' }).Count -eq 1) "$source installation still creates the automatically indexed KB."
        Assert-True (@($applied | Where-Object { $_.kind -eq 'KnowledgeBaseIndexRun' }).Count -eq 0) "$source installation must not create redundant provider reindex runs."
        Assert-True (@($events | Where-Object { $_.command -like 'apply *' -and @($_.documents | Where-Object { $_ }).Count -eq 0 }).Count -eq 0) "$source installation never applies an empty rendered document."
        $firstKB = @($events | Where-Object { @($_.documents | Where-Object { $_.kind -eq 'KnowledgeBase' }).Count -gt 0 })[0]
        $rollout = @($events | Where-Object { $_.command -like 'rollout status*' })[0]
        Assert-True ([array]::IndexOf($events, $rollout) -lt [array]::IndexOf($events, $firstKB)) "$source fixture readiness precedes initial KB indexing."
        if ($source -ne 'docs') {
            $uploadEvent = @($events | Where-Object { $_.command -like 'verified-upload*' })[0]
            $credentialEvent = @($events | Where-Object { @($_.documents | Where-Object { $_.kind -eq 'SecretRef' }).Count -gt 0 })[0]
            Assert-True ([array]::IndexOf($events, $uploadEvent) -lt [array]::IndexOf($events, $credentialEvent) -and [array]::IndexOf($events, $credentialEvent) -lt [array]::IndexOf($events, $firstKB)) "$source verified corpus and credentials precede KB creation."
        }
        Assert-True (@($events | ForEach-Object { $_.documents } | Where-Object { ($_.kind -eq 'Secret' -and $_.metadata.name -ne 'azurite-account') -or ($_.kind -eq 'SecretRef' -and $_.metadata.name -eq 'kognition-kb-pgvector') }).Count -eq 0) 'Governed paths never copy a raw/admin database credential.'
    }
    foreach ($failure in @('rollout', 'upload', 'credentials')) {
        $global:SampleProfileTestEvents.Clear()
        $global:SampleProfileTestFailure = $failure
        Assert-Throws { Invoke-AzureSampleKnowledgeInstall -Source azure -SampleDirectory (Join-Path $repoRoot 'samples\knowledge-base-blob-azure') } '*'
        Assert-True (@($global:SampleProfileTestEvents | ForEach-Object { $_.documents } | Where-Object { $_.kind -eq 'KnowledgeBase' }).Count -eq 0) "$failure failure must prevent initial indexing."
    }
    $global:SampleProfileTestFailure = ''
    $global:SampleProfileTestInstance = @{ metadata = @{ labels = @{ 'kognition.dev/sample-bundle' = 'compatible' } }; spec = @{ agentRef = @{ name = 'hello-agent' } }; status = @{ phase = 'Ready' } }
    $global:SampleProfileTestEvents.Clear()
    & (Join-Path $repoRoot 'samples\agentinstance\apply.ps1')
    Assert-True (@($global:SampleProfileTestEvents | Where-Object { $_.command -match '^(delete|apply) ' }).Count -eq 0) 'Governed one-shot reinstall preserves the original invocation outcome.'
    Assert-True (@($global:SampleProfileTestEvents | Where-Object { $_.command -notlike '*-n kognition-work*' }).Count -eq 0) 'One-shot queries use mapped default.'
    $global:SampleProfileTestInstance.metadata.labels.Clear()
    Assert-Throws { & (Join-Path $repoRoot 'samples\agentinstance\apply.ps1') } '*not owned*'

    Write-Output 'PASS: governed profile validation, namespace/reference integrity, digest fixtures, governed backend/claims, bounded budgets, full-install propagation/refusal, paid reasoning canary exclusion with health/readiness and legacy probe preservation, automatic provider indexing without duplicate named runs, legacy/native reindex preservation, empty-render no-ops, source ordering/failure safeguards, and one-shot preservation.'
} finally {
    Set-Location -LiteralPath $originalDirectory
    [Environment]::SetEnvironmentVariable('KOGNITION_SAMPLE_PROFILE_PATH', $previousProfile, 'Process')
    [Environment]::SetEnvironmentVariable('VIRTUAL_ENV', $previousVirtualEnv, 'Process')
    Remove-Item Function:\kubectl -ErrorAction SilentlyContinue
    Remove-Item Function:\helm -ErrorAction SilentlyContinue
    Remove-Item -LiteralPath $scratch -Recurse -Force
    Remove-Variable SampleProfileTestEvents, SampleProfileTestFailure, SampleProfileTestInstance, SampleProfileTestEnrollment -Scope Global -ErrorAction SilentlyContinue
}
