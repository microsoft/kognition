# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

param(
    [Parameter(Mandatory)][string]$Source,
    [Parameter(Mandatory)][string]$ReferenceContracts,
    [Parameter(Mandatory)][string]$CLI,
    [Parameter(Mandatory)][string]$OutputDirectory,
    [Parameter(Mandatory)][string]$Approver
)
$ErrorActionPreference = 'Stop'
$expected = '185978317a9ee40cec7023e729f03cf468192967cdc4f260555820fd772016f7'
if ((Get-FileHash $Source -Algorithm SHA256).Hash.ToLowerInvariant() -ne $expected) { throw 'Pinned source bytes differ' }
$reference = Get-Content $ReferenceContracts -Raw | ConvertFrom-Json -AsHashtable
if ($reference.sourceSHA256 -ne $expected -or $reference.upstreamCommit -ne 'b133e628c9534da4a68bf1e3d035426b9a768d9b' -or $reference.tools.Count -ne 5) { throw 'Pinned reference identity differs' }
$rawPlan = & $CLI import kagent plan -f $Source --sha256 $expected --target-namespace recovery-runtime --model porting-model
if ($LASTEXITCODE -ne 0) { throw 'Offline source review failed' }
$plan = $rawPlan | ConvertFrom-Json -AsHashtable
$null = New-Item -ItemType Directory -Path $OutputDirectory -Force
function Write-Artifact([string]$Name, $Value) {
    $path = Join-Path $OutputDirectory $Name
    [IO.File]::WriteAllText($path, ($Value | ConvertTo-Json -Depth 100), [Text.UTF8Encoding]::new($false))
    return $path
}
function Resource([string]$Kind, [string]$Name, $Spec, [bool]$Cluster = $false) {
    $metadata = @{name=$Name; labels=@{'kognition-fixture'='porting-20261008'}}
    if (-not $Cluster) { $metadata.namespace = 'recovery-runtime' }
    return @{apiVersion='kognition.dev/v1alpha1';kind=$Kind;metadata=$metadata;spec=$Spec}
}
$bindings = @()
$resources = [Collections.Generic.List[object]]::new()
$resources.Add((Resource 'ToolClass' 'porting-openapi' @{provisioner='openapi';executionTier='sidecar';transport='http';resourceLimits=@{maxCpuMilli=250;maxMemoryMB=128;maxTimeout='90s'}} $true))
for ($index=0; $index -lt 5; $index++) {
    $original = $plan.tools[$index]
    $tool = $reference.tools[$index]
    if ($tool.index -ne $index) { throw 'Reference tool order differs' }
    $name = "porting-tool-$index"
    $bindings += @{index=$index;provider=$original.provider;configSHA256=$original.configSHA256;targetTool=$name;runtimeName=$tool.runtimeName;schemaSHA256=$tool.schemaSHA256;implementationSHA256=$tool.implementationSHA256}
    $resources.Add((Resource 'Tool' $name @{
        className='porting-openapi';protocol='OpenAPI';description=$tool.description
        endpoint="http://porting-bridge.recovery-runtime.svc:443/tools/$($tool.runtimeName)"
        inputSchema=$tool.inputSchema
        metadata=@{allowPrivateNetwork='true';upstreamImplementationSHA256=$tool.implementationSHA256;upstreamSchemaSHA256=$tool.schemaSHA256}
        executionPolicy=@{timeout='90s';maxConcurrent=1;idempotent=$false;retryMaxAttempts=0}
    }))
}
$review = @{version='kognition.dev/kagent-import-review/v1';decision='Approved';approver=$Approver;sourceSHA256=$expected;targetNamespace='recovery-runtime';targetModel='porting-model';tools=$bindings}
$reviewPath = Write-Artifact 'proposed-import-review.json' $review
$reviewHash = (Get-FileHash $reviewPath -Algorithm SHA256).Hash.ToLowerInvariant()
$agentJSON = & $CLI import kagent render -f $Source --sha256 $expected --review $reviewPath --review-sha256 $reviewHash
if ($LASTEXITCODE -ne 0) { throw 'Reviewed offline Agent rendering failed' }
$agent = $agentJSON | ConvertFrom-Json -AsHashtable
if ($agent.spec.instructions.inline -cne $plan.systemMessage) { throw 'Imported prompt changed' }
$agent.metadata.labels = @{'kognition-fixture'='porting-20261008'}
$agent.metadata.annotations['kognition.dev/agent-delegation-enabled'] = 'false'
$resources.Add($agent)
$resources.Add((Resource 'ModelClass' 'porting-local-model' @{provisioner='openai-compatible';costTier='free';contextWindow=16384;modalities=@('text')} $true))
$key = Resource 'SecretRef' 'porting-model-key' @{backendType='internal';backendRef='fixture-only';key='apiKey'}
$key.metadata.annotations = @{'kognition.dev/internal-secret-data'='{"type":"api-key","apiKey":"fixture-only-not-a-credential"}'}
$resources.Add($key)
$resources.Add((Resource 'Model' 'porting-model' @{className='porting-local-model';provider='openai-compatible';modelID='gpt-4o';endpoint='http://porting-model.recovery-runtime.svc:443';capabilities=@('chat','tool-calling');credentialRef=@{name='porting-model-key'}}))
$child = Resource 'Agent' 'porting-promql' @{modelRef=@{name='porting-model'};instructions=@{inline=$reference.promqlPrompt};runtime=@{class='autogen';timeout='60s';maxTokens=128}}
$child.metadata.annotations = @{'kognition.dev/agent-delegation-enabled'='false'}
$resources.Add($child)
foreach ($name in @('observability-agent','porting-promql')) {
    $resources.Add((Resource 'ModelGrant' "$name-model" @{agentRef=@{name=$name};modelRef=@{name='porting-model'}}))
}
$resources.Add((Resource 'AgentPolicy' 'porting-tools' @{scope='agent';enforce=$true;targetAgents=@('observability-agent');toolGrants=@($bindings | ForEach-Object { @{toolRef=$_.targetTool;allow=$true} })}))
$role = Resource 'AgentRole' 'porting-tool-invoker' @{}
$role.Remove('spec')
$role.rules = @(@{resources=@('Tool');verbs=@('get','invoke')})
$resources.Add($role)
$resources.Add((Resource 'AgentRoleBinding' 'porting-tool-invoker' @{roleRef=@{name='porting-tool-invoker'};subjects=@(@{kind='Agent';name='observability-agent'})}))
$resourcesPath = Write-Artifact 'proposed-resources.json' @{apiVersion='v1';kind='List';items=$resources.ToArray()}
$contractPath = Write-Artifact 'model-contract.json' @{systemMessage=$plan.systemMessage;promqlPrompt=$reference.promqlPrompt;tools=$reference.tools}
[ordered]@{
    sourceSHA256=$expected
    importReviewSHA256=$reviewHash
    resourcesSHA256=(Get-FileHash $resourcesPath -Algorithm SHA256).Hash.ToLowerInvariant()
    fixtureContractSHA256=(Get-FileHash $contractPath -Algorithm SHA256).Hash.ToLowerInvariant()
    installed=$false
    effectsExecuted=$false
} | ConvertTo-Json