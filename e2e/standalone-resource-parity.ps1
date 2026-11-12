# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

param(
    [string]$Server = 'http://127.0.0.1:8080'
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$runID = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
$namespace = "standalone-parity-$runID"
$modelClassName = "parity-model-class-$runID"
$memoryClassName = "parity-memory-class-$runID"
$knowledgeBaseClassName = "parity-kb-class-$runID"
$created = [System.Collections.Generic.List[object]]::new()

$plurals = @{
    Agent = 'agents'
    AgentAutoscaler = 'agentautoscalers'
    AgentIdentity = 'agentidentities'
    AgentJob = 'agentjobs'
    AgentService = 'agentservices'
    ApprovalDecision = 'approvaldecisions'
    AutonomyReview = 'autonomyreviews'
    CrossNamespaceGrant = 'crossnamespacegrants'
    Evaluation = 'evaluations'
    EvaluationDataset = 'evaluationdatasets'
    EvaluationRun = 'evaluationruns'
    KnowledgeBase = 'knowledgebases'
    KnowledgeBaseClass = 'knowledgebaseclasses'
    KnowledgeBaseIndexRun = 'knowledgebaseindexruns'
    KnowledgeBaseIndexSchedule = 'knowledgebaseindexschedules'
    Memory = 'memories'
    MemoryClass = 'memoryclasses'
    MemoryClaim = 'memoryclaims'
    MemoryGrant = 'memorygrants'
    MemoryRecord = 'memoryrecords'
    Model = 'models'
    ModelAutoscaler = 'modelautoscalers'
    ModelClass = 'modelclasses'
    ModelGrant = 'modelgrants'
    TrustProfile = 'trustprofiles'
    Workflow = 'workflows'
    WorkflowRun = 'workflowruns'
}

function Get-ResourceUrl {
    param([string]$Kind, [string]$Name = '')
    $url = "$Server/apis/kognition.dev/v1alpha1/namespaces/$namespace/$($plurals[$Kind])"
    if ($Name) { $url += "/$Name" }
    return $url
}

function Invoke-Api {
    param(
        [string]$Method,
        [string]$Url,
        [object]$Body,
        [int[]]$ExpectedStatus
    )

    $params = @{ Uri = $Url; Method = $Method; SkipHttpErrorCheck = $true }
    if ($null -ne $Body) {
        $params.ContentType = 'application/json'
        $params.Body = $Body | ConvertTo-Json -Depth 30 -Compress
    }
    $response = Invoke-WebRequest @params
    if ($response.StatusCode -notin $ExpectedStatus) {
        throw "$Method $Url returned $($response.StatusCode): $($response.Content)"
    }
    if ($response.Content) {
        return $response.Content | ConvertFrom-Json
    }
    return $null
}

function New-ParityObject {
    param([string]$Kind, [string]$Name, [hashtable]$Spec)
    return @{
        apiVersion = 'kognition.dev/v1alpha1'
        kind = $Kind
        metadata = @{ name = $Name; namespace = $namespace }
        spec = $Spec
    }
}

function Add-Object {
    param([hashtable]$Object)
    $kind = $Object.kind
    $name = $Object.metadata.name
    $createdObject = Invoke-Api POST (Get-ResourceUrl $kind) $Object @(200, 201)
    if ($createdObject.apiVersion -ne 'kognition.dev/v1alpha1' -or $createdObject.kind -ne $kind -or $createdObject.metadata.name -ne $name) {
        throw "POST $kind/$name returned the wrong resource identity: apiVersion=$($createdObject.apiVersion), kind=$($createdObject.kind), name=$($createdObject.metadata.name)"
    }
    $created.Add([pscustomobject]@{ Kind = $kind; Name = $name })
}

function Remove-Object {
    param([string]$Kind, [string]$Name)

    $deadline = [DateTime]::UtcNow.AddSeconds(10)
    do {
        $response = Invoke-WebRequest -Uri (Get-ResourceUrl $Kind $Name) -Method DELETE -SkipHttpErrorCheck
        if ($response.StatusCode -in @(200, 202, 204, 404)) {
            return
        }
        if ($response.StatusCode -ne 409) {
            throw "DELETE $Kind/$Name returned $($response.StatusCode): $($response.Content)"
        }
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "DELETE $Kind/$Name remained conflicted for 10 seconds"
}

function Assert-PeriodicRefreshSchedule {
    param([string]$KnowledgeBaseName, [string]$Interval)

    $scheduleName = "$KnowledgeBaseName-refresh"
    $deadline = [DateTime]::UtcNow.AddSeconds(30)
    do {
        $response = Invoke-WebRequest -Uri (Get-ResourceUrl 'KnowledgeBaseIndexSchedule' $scheduleName) -Method GET -SkipHttpErrorCheck
        if ($response.StatusCode -eq 200) {
            $schedule = $response.Content | ConvertFrom-Json
            if ($schedule.spec.knowledgeBaseName -ne $KnowledgeBaseName -or $schedule.spec.schedule -ne "@every $Interval") {
                throw "Generated refresh schedule has unexpected spec: $($schedule.spec | ConvertTo-Json -Compress)"
            }
            return
        }
        if ($response.StatusCode -ne 404) {
            throw "GET generated refresh schedule returned $($response.StatusCode): $($response.Content)"
        }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Periodic refresh schedule $scheduleName was not synthesized within 30 seconds"
}

try {
    $prerequisites = @(
        (New-ParityObject 'ModelClass' $modelClassName @{ provisioner = 'foundry' }),
        (New-ParityObject 'Model' 'parity-model' @{
            className = $modelClassName; modelID = 'parity-model'; provider = 'foundry'; endpoint = 'https://example.invalid'
        }),
        (New-ParityObject 'Agent' 'parity-agent' @{
            modelRef = @{ name = 'parity-model' }; instructions = @{ inline = 'Disposable standalone parity agent.' }; replicas = 1
        }),
        (New-ParityObject 'AgentIdentity' 'parity-identity' @{ agentRef = @{ name = 'parity-agent' } }),
        (New-ParityObject 'MemoryClass' $memoryClassName @{ backendType = 'in-memory'; reclaimPolicy = 'Delete' }),
        (New-ParityObject 'KnowledgeBaseClass' $knowledgeBaseClassName @{ provisioner = 'inline'; indexMode = 'inline'; reclaimPolicy = 'Retain' }),
        (New-ParityObject 'MemoryClaim' 'parity-claim' @{
            memoryClassName = $memoryClassName; type = 'working'; capacity = '1Mi'; accessModes = @('ReadWrite')
        }),
        (New-ParityObject 'TrustProfile' 'parity-trust' @{
            agentRef = @{ name = 'parity-agent' }; initialScore = 0.5; initialClass = 'supervised'
        }),
        (New-ParityObject 'Workflow' 'parity-workflow' @{
            entryPoint = 'run'; steps = @(@{ name = 'run'; agentRef = @{ name = 'parity-agent' }; input = '{{.Input}}' })
        }),
        (New-ParityObject 'KnowledgeBase' 'parity-knowledge-base' @{
            knowledgeBaseClassName = $knowledgeBaseClassName
            sources = @(@{ type = 'inline'; data = 'Disposable standalone periodic refresh content.' })
            refreshPolicy = @{ mode = 'periodic'; interval = '1h' }
        })
    )
    foreach ($object in $prerequisites) { Add-Object $object }

    $targets = @(
        (New-ParityObject 'AgentAutoscaler' 'parity-agent-autoscaler' @{
            agentRef = @{ name = 'parity-agent' }; minReplicas = 1; maxReplicas = 2; metrics = @(@{ type = 'QueueDepth'; targetValue = 1 })
        }),
        (New-ParityObject 'AgentJob' 'parity-agent-job' @{
            agentRef = @{ name = 'parity-agent' }; completionPolicy = 'Any'; input = 'standalone parity check'
        }),
        (New-ParityObject 'AgentService' 'parity-agent-service' @{
            agentRef = @{ name = 'parity-agent' }; revisionPin = 1; port = 8080
        }),
        (New-ParityObject 'AutonomyReview' 'parity-autonomy-review' @{
            agentRef = @{ name = 'parity-agent' }; actionDescription = 'Standalone parity review.'; currentClass = 'supervised'; requestedClass = 'assisted'; trustProfileRef = 'parity-trust'
        }),
        (New-ParityObject 'CrossNamespaceGrant' 'parity-cross-namespace-grant' @{
            from = $namespace; to = 'parity-target'; resources = @(@{ kind = 'Model'; name = 'parity-model' })
        }),
        (New-ParityObject 'EvaluationDataset' 'parity-evaluation-dataset' @{
            version = 'v1'; cases = @(@{ id = 'case-1'; input = 'standalone parity evaluation'; expected = 'parity'; tags = @('parity') })
        }),
        (New-ParityObject 'Evaluation' 'parity-evaluation' @{
            targetRef = @{ kind = 'Agent'; name = 'parity-agent' }
            datasetRef = @{ name = 'parity-evaluation-dataset'; version = 'v1' }
            graders = @(@{ name = 'task-success'; type = 'task_success' })
        }),
        (New-ParityObject 'EvaluationRun' 'parity-evaluation-run' @{
            evaluationRef = @{ name = 'parity-evaluation' }
            targetRef = @{ kind = 'Agent'; name = 'parity-agent' }
            datasetRef = @{ name = 'parity-evaluation-dataset'; version = 'v1' }
            graders = @(@{ name = 'task-success'; type = 'task_success' })
        }),
        (New-ParityObject 'KnowledgeBaseIndexRun' 'parity-index-run' @{
            knowledgeBaseName = 'parity-knowledge-base'
        }),
        (New-ParityObject 'KnowledgeBaseIndexSchedule' 'parity-index-schedule' @{
            knowledgeBaseName = 'parity-knowledge-base'; schedule = '0 0 1 1 *'; suspend = $true; concurrencyPolicy = 'Forbid'
        }),
        (New-ParityObject 'Memory' 'parity-memory' @{
            claimRef = @{ name = 'parity-claim' }; type = 'working'; ttl = '1h'; retentionPolicy = 'truncate'
        }),
        (New-ParityObject 'MemoryGrant' 'parity-memory-grant' @{
            agentRef = @{ name = 'parity-agent' }; memoryClaimRef = @{ name = 'parity-claim' }; accessMode = 'ReadWrite'
        }),
        (New-ParityObject 'MemoryRecord' 'parity-memory-record' @{
            memoryRef = @{ name = 'parity-memory' }; content = 'Standalone parity record.'; metadata = @{ purpose = 'parity' }
            provenance = @{
                sourceAgentIdentity = 'parity-identity'; sessionId = 'parity-session'; reasoningStepId = 'parity-step'; timestamp = [DateTime]::UtcNow.ToString('o')
            }
        }),
        (New-ParityObject 'ModelAutoscaler' 'parity-model-autoscaler' @{
            agentRef = @{ name = 'parity-agent' }; preferredModelRef = @{ name = 'parity-model' }; stabilizationWindowSeconds = 60
        }),
        (New-ParityObject 'ModelGrant' 'parity-model-grant' @{
            agentRef = @{ name = 'parity-agent' }; modelRef = @{ name = 'parity-model' }
        }),
        (New-ParityObject 'WorkflowRun' 'parity-workflow-run' @{
            workflowRef = @{ name = 'parity-workflow' }; input = 'standalone parity check'
        }),
        (New-ParityObject 'ApprovalDecision' 'parity-approval-decision' @{
            workflowRunRef = @{ name = 'parity-workflow-run' }; step = 'run'; decision = 'approve'; approver = 'standalone-parity'; reason = 'Standalone parity decision.'
        })
    )

    foreach ($object in $targets) {
        $kind = $object.kind
        $name = $object.metadata.name
        Write-Host "Verifying $kind/$name"
        Add-Object $object

        $current = Invoke-Api GET (Get-ResourceUrl $kind $name) $null @(200)
        if ($current.apiVersion -ne 'kognition.dev/v1alpha1' -or $current.kind -ne $kind -or $current.metadata.name -ne $name) {
            throw "GET $kind/$name returned the wrong resource identity: apiVersion=$($current.apiVersion), kind=$($current.kind), name=$($current.metadata.name)"
        }
        $current.metadata | Add-Member -NotePropertyName annotations -NotePropertyValue @{ 'kognition.dev/parity-updated' = 'true' } -Force
        $updated = $null
        $updateDeadline = [DateTime]::UtcNow.AddSeconds(10)
        do {
            $response = Invoke-WebRequest -Uri (Get-ResourceUrl $kind $name) -Method PUT -ContentType 'application/json' -Body ($current | ConvertTo-Json -Depth 30 -Compress) -SkipHttpErrorCheck
            if ($response.StatusCode -eq 200) {
                $updated = $response.Content | ConvertFrom-Json
                break
            }
            $statusChanged = $kind -eq 'AutonomyReview' -and $response.StatusCode -eq 422
            if ($response.StatusCode -ne 409 -and -not $statusChanged) {
                throw "PUT $kind/$name returned $($response.StatusCode): $($response.Content)"
            }
            $current = Invoke-Api GET (Get-ResourceUrl $kind $name) $null @(200)
            $current.metadata | Add-Member -NotePropertyName annotations -NotePropertyValue @{ 'kognition.dev/parity-updated' = 'true' } -Force
        } while ([DateTime]::UtcNow -lt $updateDeadline)
        if ($null -eq $updated -or $updated.kind -ne $kind -or $updated.metadata.annotations.'kognition.dev/parity-updated' -ne 'true') {
            throw "$kind/$name annotation update did not round-trip"
        }

        $list = Invoke-Api GET (Get-ResourceUrl $kind) $null @(200)
        if ($list.apiVersion -ne 'kognition.dev/v1alpha1' -or $list.kind -ne "${kind}List") {
            throw "LIST $kind returned the wrong resource identity: apiVersion=$($list.apiVersion), kind=$($list.kind)"
        }
        if (-not @($list.items | Where-Object { $_.metadata.name -eq $name })) {
            throw "$kind/$name was not returned by list"
        }
        if (@($list.items | Where-Object { $_.metadata.name -eq $name -and $_.kind -ne $kind })) {
            throw "LIST $kind returned $name with the wrong kind"
        }
    }

    $agentService = Invoke-Api GET (Get-ResourceUrl 'AgentService' 'parity-agent-service') $null @(200)
    if ($agentService.spec.revisionPin -ne 1) {
        throw "AgentService revisionPin did not round-trip: $($agentService.spec | ConvertTo-Json -Compress)"
    }
    Assert-PeriodicRefreshSchedule 'parity-knowledge-base' '1h'

    $agentReady = $false
    $agentDeadline = [DateTime]::UtcNow.AddSeconds(60)
    do {
        $agent = Invoke-Api GET (Get-ResourceUrl 'Agent' 'parity-agent') $null @(200)
        $phase = if ($agent.status.PSObject.Properties['phase']) { [string]$agent.status.phase } else { '' }
        $readyReplicas = if ($agent.status.PSObject.Properties['readyReplicas']) { [int]$agent.status.readyReplicas } else { 0 }
        if ($phase -eq 'Ready' -and $readyReplicas -eq 1) {
            $agentReady = $true
            break
        }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $agentDeadline)
    if (-not $agentReady) {
        $instances = Invoke-Api GET "$Server/apis/kognition.dev/v1alpha1/namespaces/$namespace/agentinstances" $null @(200)
        foreach ($instance in @($instances.items | Where-Object { $_.spec.agentRef.name -eq 'parity-agent' })) {
            $readyCondition = @($instance.status.conditions | Where-Object { $_.type -eq 'Ready' } | Select-Object -Last 1)
            Write-Warning "AgentInstance $($instance.metadata.name): phase=$($instance.status.phase), reason=$($readyCondition.reason), message=$($readyCondition.message)"
        }
        throw "standalone warm Agent did not converge: phase=$phase, ready=$readyReplicas"
    }

    Write-Host "Standalone CRUD parity passed for $($targets.Count) native kinds; warm Agent is Ready." -ForegroundColor Green
}
finally {
    for ($index = $created.Count - 1; $index -ge 0; $index--) {
        $entry = $created[$index]
        try {
            Remove-Object $entry.Kind $entry.Name
        }
        catch {
            Write-Warning "cleanup failed for $($entry.Kind)/$($entry.Name): $_"
        }
    }
}