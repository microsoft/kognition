# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$runID = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
$namespace = "kognition-parity-$runID"
$knowledgeBaseClassName = "parity-kb-class-$runID"
$modelClassName = "parity-model-class-$runID"

function Invoke-Kubectl {
    param([Parameter(Mandatory = $true)][string[]]$Arguments)

    $output = & kubectl @Arguments 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "kubectl $($Arguments -join ' ') failed:`n$($output -join "`n")"
    }
    return $output
}

function Set-KognitionObject {
    param([Parameter(Mandatory = $true)][hashtable]$Object)

    $json = $Object | ConvertTo-Json -Depth 30 -Compress
    $output = $json | & kubectl apply -f - 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "apply $($Object.kind)/$($Object.metadata.name) failed:`n$($output -join "`n")"
    }
}

function New-KognitionObject {
    param(
        [Parameter(Mandatory = $true)][string]$Kind,
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)][hashtable]$Spec
    )

    return @{
        apiVersion = 'kognition.dev/v1alpha1'
        kind       = $Kind
        metadata   = @{ name = $Name; namespace = $namespace }
        spec       = $Spec
    }
}

function New-ClusterKognitionObject {
    param(
        [Parameter(Mandatory = $true)][string]$Kind,
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)][hashtable]$Spec
    )

    return @{
        apiVersion = 'kognition.dev/v1alpha1'
        kind       = $Kind
        metadata   = @{ name = $Name }
        spec       = $Spec
    }
}

function Assert-PeriodicRefreshSchedule {
    param([string]$KnowledgeBaseName, [string]$Interval)

    $scheduleName = "$KnowledgeBaseName-refresh"
    $deadline = [DateTime]::UtcNow.AddSeconds(30)
    do {
        $result = & kubectl get "knowledgebaseindexschedules/$scheduleName" -n $namespace -o json 2>$null
        if ($LASTEXITCODE -eq 0) {
            $schedule = ($result | Out-String) | ConvertFrom-Json
            if ($schedule.spec.knowledgeBaseName -ne $KnowledgeBaseName -or $schedule.spec.schedule -ne "@every $Interval") {
                throw "Generated refresh schedule has unexpected spec: $($schedule.spec | ConvertTo-Json -Compress)"
            }
            return
        }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Periodic refresh schedule $scheduleName was not synthesized within 30 seconds"
}

Write-Host "Creating disposable parity namespace $namespace"
Invoke-Kubectl @('create', 'namespace', $namespace) | Out-Null

try {
    $prerequisites = @(
        (New-ClusterKognitionObject 'ModelClass' $modelClassName @{
            provisioner = 'foundry'
        }),
        (New-KognitionObject 'Model' 'parity-model' @{
            className = $modelClassName
            modelID = 'parity-model'
            provider = 'foundry'
            endpoint = 'https://example.invalid'
            capabilities = @('chat')
        }),
        (New-KognitionObject 'Agent' 'parity-agent' @{
            modelRef = @{ name = 'parity-model' }
            instructions = @{ inline = 'Disposable live parity agent.' }
            replicas = 0
        }),
        (New-KognitionObject 'AgentIdentity' 'parity-identity' @{
            agentRef = @{ name = 'parity-agent' }
        }),
        (New-KognitionObject 'MemoryClaim' 'parity-claim' @{
            memoryClassName = 'default'
            type = 'working'
            capacity = '1Mi'
            accessModes = @('ReadWrite')
        }),
        (New-ClusterKognitionObject 'KnowledgeBaseClass' $knowledgeBaseClassName @{
            provisioner = 'inline'
            indexMode = 'inline'
            reclaimPolicy = 'Retain'
        }),
        (New-KognitionObject 'Workflow' 'parity-workflow' @{
            entryPoint = 'run'
            steps = @(@{
                name = 'run'
                agentRef = @{ name = 'parity-agent' }
                input = '{{.Input}}'
            })
        }),
        (New-KognitionObject 'KnowledgeBase' 'parity-knowledge-base' @{
            knowledgeBaseClassName = $knowledgeBaseClassName
            sources = @(@{ type = 'inline'; data = 'Disposable Kubernetes periodic refresh content.' })
            refreshPolicy = @{ mode = 'periodic'; interval = '1h' }
        })
    )
    foreach ($object in $prerequisites) {
        Set-KognitionObject $object
    }

    $agentIncarnationUID = (Invoke-Kubectl @('get', 'agent/parity-agent', '-n', $namespace, '-o', 'jsonpath={.metadata.uid}') | Out-String).Trim()
    if (-not $agentIncarnationUID) {
        throw 'Agent/parity-agent has an empty metadata.uid'
    }
    Set-KognitionObject (New-KognitionObject 'TrustProfile' 'parity-trust' @{
        agentRef = @{ name = 'parity-agent' }
        agentIncarnationUID = $agentIncarnationUID
        initialScore = 0.5
        initialClass = 'supervised'
    })

    Invoke-Kubectl @('wait', '-n', $namespace, '--for=jsonpath={.status.phase}=Bound', 'memoryclaim/parity-claim', '--timeout=60s') | Out-Null

    $approver = (Invoke-Kubectl @('auth', 'whoami', '-o', 'jsonpath={.status.userInfo.username}') | Out-String).Trim()
    if (-not $approver) {
        throw 'kubectl auth whoami returned an empty username'
    }

    $targets = @(
        @{ resource = 'agentautoscalers'; object = (New-KognitionObject 'AgentAutoscaler' 'parity-agent-autoscaler' @{
            agentRef = @{ name = 'parity-agent' }
            minReplicas = 1
            maxReplicas = 1
            metrics = @(@{ type = 'QueueDepth'; targetValue = 1 })
        }) },
        @{ resource = 'agentjobs'; object = (New-KognitionObject 'AgentJob' 'parity-agent-job' @{
            agentRef = @{ name = 'parity-agent' }
            completionPolicy = 'Any'
            input = 'live parity check'
        }) },
        @{ resource = 'agentservices'; object = (New-KognitionObject 'AgentService' 'parity-agent-service' @{
            agentRef = @{ name = 'parity-agent' }
            revisionPin = 1
            port = 8080
        }) },
        @{ resource = 'autonomyreviews'; object = (New-KognitionObject 'AutonomyReview' 'parity-autonomy-review' @{
            agentRef = @{ name = 'parity-agent' }
            actionDescription = 'Disposable live parity review.'
            currentClass = 'supervised'
            requestedClass = 'assisted'
            trustProfileRef = 'parity-trust'
        }) },
        @{ resource = 'crossnamespacegrants'; object = (New-KognitionObject 'CrossNamespaceGrant' 'parity-cross-namespace-grant' @{
            from = $namespace
            to = 'parity-target'
            resources = @(@{ kind = 'Model'; name = 'parity-model' })
        }) },
        @{ resource = 'evaluationdatasets'; object = (New-KognitionObject 'EvaluationDataset' 'parity-evaluation-dataset' @{
            version = 'v1'
            cases = @(@{
                id = 'case-1'
                input = 'live parity evaluation'
                expected = 'parity'
                tags = @('parity')
            })
        }) },
        @{ resource = 'evaluations'; object = (New-KognitionObject 'Evaluation' 'parity-evaluation' @{
            targetRef = @{ kind = 'Agent'; name = 'parity-agent' }
            datasetRef = @{ name = 'parity-evaluation-dataset'; version = 'v1' }
            graders = @(@{ name = 'task-success'; type = 'task_success' })
        }) },
        @{ resource = 'evaluationruns'; object = (New-KognitionObject 'EvaluationRun' 'parity-evaluation-run' @{
            evaluationRef = @{ name = 'parity-evaluation' }
            targetRef = @{ kind = 'Agent'; name = 'parity-agent' }
            datasetRef = @{ name = 'parity-evaluation-dataset'; version = 'v1' }
            graders = @(@{ name = 'task-success'; type = 'task_success' })
        }) },
        @{ resource = 'knowledgebaseindexruns'; object = (New-KognitionObject 'KnowledgeBaseIndexRun' 'parity-index-run' @{
            knowledgeBaseName = 'parity-knowledge-base'
        }) },
        @{ resource = 'knowledgebaseindexschedules'; object = (New-KognitionObject 'KnowledgeBaseIndexSchedule' 'parity-index-schedule' @{
            knowledgeBaseName = 'parity-knowledge-base'
            schedule = '0 0 1 1 *'
            suspend = $true
            concurrencyPolicy = 'Forbid'
        }) },
        @{ resource = 'memories'; object = (New-KognitionObject 'Memory' 'parity-memory' @{
            claimRef = @{ name = 'parity-claim' }
            type = 'working'
            ttl = '1h'
            retentionPolicy = 'truncate'
        }) },
        @{ resource = 'memorygrants'; object = (New-KognitionObject 'MemoryGrant' 'parity-memory-grant' @{
            agentRef = @{ name = 'parity-agent' }
            memoryClaimRef = @{ name = 'parity-claim' }
            accessMode = 'ReadWrite'
        }) },
        @{ resource = 'memoryrecords'; object = (New-KognitionObject 'MemoryRecord' 'parity-memory-record' @{
            memoryRef = @{ name = 'parity-memory' }
            content = 'Disposable live parity record.'
            metadata = @{ purpose = 'parity' }
            provenance = @{
                sourceAgentIdentity = 'parity-identity'
                sessionId = 'parity-session'
                reasoningStepId = 'parity-step'
                timestamp = [DateTime]::UtcNow.ToString('o')
            }
        }) },
        @{ resource = 'modelautoscalers'; object = (New-KognitionObject 'ModelAutoscaler' 'parity-model-autoscaler' @{
            agentRef = @{ name = 'parity-agent' }
            preferredModelRef = @{ name = 'parity-model' }
            stabilizationWindowSeconds = 60
        }) },
        @{ resource = 'modelgrants'; object = (New-KognitionObject 'ModelGrant' 'parity-model-grant' @{
            agentRef = @{ name = 'parity-agent' }
            modelRef = @{ name = 'parity-model' }
        }) },
        @{ resource = 'toolgrants'; object = (New-KognitionObject 'ToolGrant' 'parity-tool-grant' @{
            agentSelector = @{ purpose = 'resource-parity' }
            toolRef = @{ name = 'parity-tool' }
            verbs = @('invoke', 'inspect')
            argumentConstraints = @{
                type = 'object'
                properties = @{ query = @{ type = 'string'; minLength = 1 } }
                required = @('query')
            }
            rateLimits = @{ callsPerMinute = 12; callsPerHour = 120; callsPerDay = 1200 }
            conditions = @{
                validFrom = '2025-01-01T00:00:00Z'
                validUntil = '2035-01-01T00:00:00Z'
                requiredAgentLabels = @{ purpose = 'resource-parity' }
            }
        }) },
        @{ resource = 'workflowruns'; object = (New-KognitionObject 'WorkflowRun' 'parity-workflow-run' @{
            workflowRef = @{ name = 'parity-workflow' }
            input = 'live parity check'
        }) },
        @{ resource = 'approvaldecisions'; object = (New-KognitionObject 'ApprovalDecision' 'parity-approval-decision' @{
            workflowRunRef = @{ name = 'parity-workflow-run' }
            step = 'run'
            decision = 'approve'
            approver = $approver
            reason = 'Disposable live parity decision.'
        }) }
    )

    foreach ($target in $targets) {
        $object = $target.object
        $resource = $target.resource
        $name = $object.metadata.name
        Write-Host "Verifying $($object.kind)/$name"
        Set-KognitionObject $object
        $current = (Invoke-Kubectl @('get', "$resource/$name", '-n', $namespace, '-o', 'json') | Out-String) | ConvertFrom-Json
        if ($current.apiVersion -ne 'kognition.dev/v1alpha1' -or $current.kind -ne $object.kind -or $current.metadata.name -ne $name) {
            throw "GET $($object.kind)/$name returned the wrong resource identity: apiVersion=$($current.apiVersion), kind=$($current.kind), name=$($current.metadata.name)"
        }
        Invoke-Kubectl @('annotate', "$resource/$name", '-n', $namespace, 'kognition.dev/parity-updated=true', '--overwrite') | Out-Null
        $updated = (Invoke-Kubectl @('get', "$resource/$name", '-n', $namespace, '-o', 'json') | Out-String) | ConvertFrom-Json
        if ($updated.kind -ne $object.kind -or $updated.metadata.annotations.'kognition.dev/parity-updated' -ne 'true') {
            throw "$($object.kind)/$name annotation update did not round-trip"
        }
        $listed = (Invoke-Kubectl @('get', $resource, '-n', $namespace, '-o', 'json') | Out-String) | ConvertFrom-Json
        if ($listed.apiVersion -ne 'v1' -or $listed.kind -ne 'List') {
            throw "LIST $($object.kind) returned the wrong resource identity: apiVersion=$($listed.apiVersion), kind=$($listed.kind)"
        }
        if (-not @($listed.items | Where-Object { $_.metadata.name -eq $name -and $_.kind -eq $object.kind })) {
            throw "$($object.kind)/$name was not returned by list"
        }
    }

    $agentService = (Invoke-Kubectl @('get', 'agentservices/parity-agent-service', '-n', $namespace, '-o', 'json') | Out-String) | ConvertFrom-Json
    if ($agentService.spec.revisionPin -ne 1) {
        throw "AgentService revisionPin did not round-trip: $($agentService.spec | ConvertTo-Json -Compress)"
    }
    $toolGrant = (Invoke-Kubectl @('get', 'toolgrants/parity-tool-grant', '-n', $namespace, '-o', 'json') | Out-String) | ConvertFrom-Json
    if ($toolGrant.spec.agentSelector.purpose -ne 'resource-parity' -or
        @($toolGrant.spec.verbs).Count -ne 2 -or
        $toolGrant.spec.argumentConstraints.properties.query.minLength -ne 1 -or
        $toolGrant.spec.rateLimits.callsPerHour -ne 120 -or
        $toolGrant.spec.conditions.requiredAgentLabels.purpose -ne 'resource-parity') {
        throw "ToolGrant fields did not round-trip: $($toolGrant.spec | ConvertTo-Json -Compress -Depth 20)"
    }
    Assert-PeriodicRefreshSchedule 'parity-knowledge-base' '1h'

    Write-Host "Live create/read/update/list parity passed for $($targets.Count) native kinds." -ForegroundColor Green
}
finally {
    Write-Host "Deleting disposable parity namespace $namespace"
    & kubectl delete namespace $namespace --ignore-not-found --wait=true --timeout=120s 2>&1 | Out-Host
    if ($LASTEXITCODE -ne 0) {
        Write-Warning "Namespace cleanup did not finish within the timeout: $namespace"
    }
    foreach ($resource in @("knowledgebaseclasses/$knowledgeBaseClassName", "modelclasses/$modelClassName")) {
        & kubectl delete $resource --ignore-not-found 2>&1 | Out-Host
        if ($LASTEXITCODE -ne 0) {
            Write-Warning "Cluster-scoped fixture cleanup failed for $resource"
        }
    }
}