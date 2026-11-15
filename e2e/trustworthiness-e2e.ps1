# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

param(
    [Parameter(Mandatory = $true)][string]$DemoRunId,
    [string]$KubernetesContext = "",
    [string]$ControllerNamespace = "kognition-system",
    [string]$ReleaseName = "kognition",
    [string]$ReportPath = "",
    [ValidateSet("presenter", "automated")][string]$ApprovalMode = "presenter",
    [switch]$KeepScopeNamespace,
    [switch]$ResumeRetainedScope
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

if ($DemoRunId -notmatch '^[a-z0-9]([-a-z0-9]{0,38}[a-z0-9])?$') {
    throw "DemoRunId must be 1-40 lowercase DNS-label characters"
}

$namespace = "kognition-demo-$DemoRunId"
$controllerDeployment = "$ReleaseName-k8s-controller"
$webhookService = "$ReleaseName-webhook"
$webhookTLSSecret = "$ReleaseName-webhook-tls"
$manifestsPath = Join-Path (Split-Path $PSScriptRoot -Parent) "samples/trustworthiness"
if (-not $ReportPath) {
    $ReportPath = Join-Path (Split-Path $PSScriptRoot -Parent) "test-results/trustworthiness-$DemoRunId.json"
}
$memoryPhaseReportPath = Join-Path (Split-Path $ReportPath -Parent) "memory-boundary-$DemoRunId.json"

$namespaceCreated = $false
$controllerReplicas = $null
$assertions = [Collections.Generic.List[object]]::new()
$timeline = [Collections.Generic.List[object]]::new()
$startedAt = [DateTimeOffset]::UtcNow
$failure = $null
$cleanup = [PSCustomObject]@{ status = "not-started"; evidence = "namespace was not created" }
$memoryPhaseReport = $null
$communicationCompleted = @()
$publicationReceipt = $null
$publicationContent = "Approved customer advisory for $DemoRunId"
$kubectlContextArguments = if ($KubernetesContext) { @("--context", $KubernetesContext) } else { @() }

function Invoke-Kubectl {
    param([Parameter(Mandatory = $true)][string[]]$Arguments)

    $output = & kubectl @kubectlContextArguments @Arguments 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "kubectl $($Arguments -join ' ') failed:`n$($output -join "`n")"
    }
    return $output | Where-Object { $_ -isnot [Management.Automation.ErrorRecord] }
}

function Remove-RehearsalScope {
    $cleanupErrors = [Collections.Generic.List[string]]::new()
    $retainedRecords = @()
    $deletions = @(
        @("delete", "namespace", $namespace, "--wait=false", "--ignore-not-found"),
        @("delete", "networkpolicy/trust-demo-workflow-$DemoRunId", "-n", $ControllerNamespace, "--ignore-not-found"),
        @("delete", "networkpolicy/trust-profile-provider-$DemoRunId", "-n", $ControllerNamespace, "--ignore-not-found"),
        @("delete", "modelclass/trust-profile-$DemoRunId", "--ignore-not-found", "--wait=false")
    )
    foreach ($arguments in $deletions) {
        try { Invoke-Kubectl $arguments | Out-Null }
        catch { $cleanupErrors.Add($_.Exception.Message) }
    }
    try { Invoke-Kubectl @("wait", "--for=delete", "namespace/$namespace", "--timeout=180s") | Out-Null }
    catch {
        $waitFailure = $_.Exception.Message
        try {
            $scope = Invoke-Kubectl @("get", "namespace/$namespace", "-o", "json") | ConvertFrom-Json
            if (-not $scope.metadata.deletionTimestamp -or $scope.status.phase -ne "Terminating") { throw "Namespace deletion is not in progress" }
            $resourceTypes = @(Invoke-Kubectl @("api-resources", "--namespaced=true", "--verbs=list", "-o", "name") | Where-Object { $_ } | Sort-Object -Unique)
            if (-not $resourceTypes.Count) { throw "Namespaced resource discovery returned no types" }
            $inventory = Invoke-Kubectl @("get", ($resourceTypes -join ','), "-n", $namespace, "-o", "json") | ConvertFrom-Json
            if (-not @($inventory.items).Count) { throw "Namespace remains without identifiable security retention records" }
            foreach ($record in $inventory.items) {
                $expectedFinalizer = switch ($record.kind) {
                    "RuntimeApprovalTombstone" { "runtimeapprovaltombstone.retention.kognition.dev" }
                    "RuntimeApprovalQuotaTicket" { "kognition.dev/runtimeapprovalquotaticket-retirement" }
                    default { throw "Unexpected remaining resource: $($record.kind)/$($record.metadata.name)" }
                }
                if ($record.metadata.namespace -ne $namespace -or $record.spec.workspace -ne $namespace -or
                    -not $record.metadata.deletionTimestamp -or @($record.metadata.finalizers).Count -ne 1 -or
                    $record.metadata.finalizers[0] -ne $expectedFinalizer -or
                    [DateTimeOffset]$record.spec.retentionDeadline -le [DateTimeOffset]::UtcNow) {
                    throw "Invalid retention record: $($record.kind)/$($record.metadata.name)"
                }
                if ($record.kind -eq "RuntimeApprovalQuotaTicket") {
                    $matching = @($inventory.items | Where-Object { $_.kind -eq "RuntimeApprovalTombstone" -and $_.metadata.name -eq $record.metadata.name -and $_.spec.retentionDeadline -eq $record.spec.retentionDeadline })
                    if ($matching.Count -ne 1) { throw "Quota ticket lacks its retained tombstone" }
                }
            }
            $retainedRecords = @($inventory.items | ForEach-Object {
                [PSCustomObject]@{ kind = $_.kind; name = $_.metadata.name; namespace = $namespace; retentionDeadline = $_.spec.retentionDeadline }
            })
        }
        catch { $cleanupErrors.Add("${waitFailure}`nRetention verification failed: $($_.Exception.Message)") }
    }
    try { Invoke-Kubectl @("wait", "--for=delete", "modelclass/trust-profile-$DemoRunId", "--timeout=180s") | Out-Null }
    catch { $cleanupErrors.Add($_.Exception.Message) }
    foreach ($arguments in @(
        @("get", "all,networkpolicy", "-A", "-l", "kognition.dev/demo-run=$DemoRunId", "-o", "name"),
        @("get", "modelclass/trust-profile-$DemoRunId", "--ignore-not-found", "-o", "name")
    )) {
        try {
            $remaining = (Invoke-Kubectl $arguments | Out-String).Trim()
            if ($remaining) { $cleanupErrors.Add("Remaining resources: $remaining") }
        }
        catch { $cleanupErrors.Add($_.Exception.Message) }
    }
    return [PSCustomObject]@{
        status = if ($cleanupErrors.Count) { "failed" } elseif ($retainedRecords.Count) { "retained" } else { "passed" }
        evidence = if ($cleanupErrors.Count) { $cleanupErrors -join "`n" } elseif ($retainedRecords.Count) { "namespace $namespace remains Terminating; workloads and run-owned external resources removed; $($retainedRecords.Count) security records retained until their mandated deadlines" } else { "namespace and run-owned resources removed" }
        retainedSecurityRecords = $retainedRecords
    }
}

function Add-Assertion {
    param(
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)][bool]$Passed,
        [Parameter(Mandatory = $true)][string]$Evidence
    )

    $assertions.Add([PSCustomObject]@{ name = $Name; passed = $Passed; evidence = $Evidence })
    if (-not $Passed) {
        throw "Assertion '$Name' failed: $Evidence"
    }
}

function Assert-ApprovalEvidence {
    param([Parameter(Mandatory = $true)][object]$Decision)

    $approval = Get-OptionalProperty -InputObject $Decision -Name "approval"
    $status = Get-OptionalProperty -InputObject $approval -Name "status"
    if ($status -notin @("not_evaluated", "not_required", "pending", "approved", "rejected")) { throw "Unexpected approval evidence: $status" }
    $earlyDenial = $Decision.plane -ne "model" -and $Decision.outcome -in @("denied", "revoked") -and $Decision.reasonCode -in @("grant_missing", "grant_revoked", "policy_denied", "capability_invalid", "identity_denied")
    if ($earlyDenial -and $status -ne "not_evaluated") { throw "Authorization denial must record approval not_evaluated" }
    if ($Decision.plane -eq "model" -and $status -ne "not_required") { throw "Model approval must be not_required" }
    if ($status -eq "not_evaluated" -and ($Decision.outcome -notin @("denied", "revoked") -or $Decision.execution.status -ne "blocked" -or $Decision.execution.targetReached -ne $false)) { throw "Unevaluated approval must block target execution" }
    $reference = Get-OptionalProperty -InputObject $approval -Name "decisionRef"
    if ($status -eq "not_evaluated") {
        if ($reference) { throw "Unexpected resolved approval reference" }
    } elseif ($status -ne "not_required" -or $reference) {
        if (-not $reference -or $reference.kind -ne "RuntimeApproval" -or $reference.namespace -ne $Decision.workspace -or -not $reference.name -or -not $reference.uid -or -not $reference.resourceVersion) { throw "Resolved approval requires a durable reference" }
    }
    if (($Decision.outcome -eq "pending" -or $Decision.reasonCode -eq "runtime_approval_pending") -and $status -ne "pending") { throw "Pending outcome must retain pending approval" }
}

function Import-ManifestTemplate {
    param(
        [Parameter(Mandatory = $true)][string]$FileName,
        [Parameter(Mandatory = $true)][hashtable]$Values
    )

    $content = Get-Content (Join-Path $manifestsPath $FileName) -Raw
    foreach ($entry in $Values.GetEnumerator()) {
        $content = $content.Replace(('${' + $entry.Key + '}'), [string]$entry.Value)
    }
    if ($content -match '\$\{[A-Z_]+\}') {
        throw "Manifest $FileName contains an unresolved template value"
    }
    $object = $content | ConvertFrom-Json
    $object | ConvertTo-Json -Depth 30 -Compress | & kubectl @kubectlContextArguments apply -f - 2>&1 | Out-Host
    if ($LASTEXITCODE -ne 0) {
        throw "Applying manifest $FileName failed"
    }
}

function Apply-KubernetesObject {
    param([Parameter(Mandatory = $true)][hashtable]$Object)

    $Object | ConvertTo-Json -Depth 30 -Compress | & kubectl @kubectlContextArguments apply -f - 2>&1 | Out-Host
    if ($LASTEXITCODE -ne 0) {
        throw "Applying $($Object.kind)/$($Object.metadata.name) failed"
    }
}

function Install-WorkflowFixtures {
    $targetName = "trust-demo-tool-target"
    $toolServer = Get-Content (Join-Path $manifestsPath "publication_server.py") -Raw
    $labels = @{ "app.kubernetes.io/part-of" = "trustworthiness"; "kognition.dev/demo-run" = $DemoRunId }
    Apply-KubernetesObject @{ apiVersion = "v1"; kind = "ConfigMap"; metadata = @{ name = $targetName; namespace = $namespace; labels = $labels }; data = @{ "server.py" = $toolServer } }
    Apply-KubernetesObject @{ apiVersion = "v1"; kind = "PersistentVolumeClaim"; metadata = @{ name = $targetName; namespace = $namespace; labels = $labels }; spec = @{ accessModes = @("ReadWriteOnce"); resources = @{ requests = @{ storage = "1Gi" } } } }
    Apply-KubernetesObject @{ apiVersion = "apps/v1"; kind = "Deployment"; metadata = @{ name = $targetName; namespace = $namespace; labels = $labels }; spec = @{ replicas = 1; strategy = @{ type = "Recreate" }; selector = @{ matchLabels = @{ app = $targetName } }; template = @{ metadata = @{ labels = @{ app = $targetName; "kognition.dev/demo-run" = $DemoRunId } }; spec = @{ containers = @(@{ name = "server"; image = "python:3.12-alpine"; command = @("python", "/app/server.py"); env = @(@{ name = "PUBLICATION_HOST"; value = "0.0.0.0" }, @{ name = "PUBLICATION_DATABASE"; value = "/data/publications.db" }); ports = @(@{ containerPort = 8080 }); readinessProbe = @{ httpGet = @{ path = "/healthz"; port = 8080 }; initialDelaySeconds = 1; periodSeconds = 1 }; volumeMounts = @(@{ name = "source"; mountPath = "/app"; readOnly = $true }, @{ name = "data"; mountPath = "/data" }) }); volumes = @(@{ name = "source"; configMap = @{ name = $targetName } }, @{ name = "data"; persistentVolumeClaim = @{ claimName = $targetName } }) } } } }
    Apply-KubernetesObject @{ apiVersion = "v1"; kind = "Service"; metadata = @{ name = $targetName; namespace = $namespace; labels = $labels }; spec = @{ selector = @{ app = $targetName }; ports = @(@{ port = 8080; targetPort = 8080 }) } }
    $controllerBaselinePolicy = Invoke-Kubectl @("get", "networkpolicy/$ReleaseName-k8s-controller", "-n", $ControllerNamespace, "--ignore-not-found", "-o", "name") | Out-String
    if ($controllerBaselinePolicy.Trim()) {
        Apply-KubernetesObject @{ apiVersion = "networking.k8s.io/v1"; kind = "NetworkPolicy"; metadata = @{ name = "trust-demo-workflow-$DemoRunId"; namespace = $ControllerNamespace; labels = $labels }; spec = @{ podSelector = @{ matchLabels = @{ "app.kubernetes.io/instance" = $ReleaseName }; matchExpressions = @(@{ key = "app.kubernetes.io/component"; operator = "In"; values = @("k8s-controller") }) }; policyTypes = @("Egress"); egress = @(@{ to = @(@{ namespaceSelector = @{ matchLabels = @{ "kubernetes.io/metadata.name" = $namespace } }; podSelector = @{ matchLabels = @{ app = $targetName } } }); ports = @(@{ protocol = "TCP"; port = 8080 }) }) } }
    }
    Apply-KubernetesObject @{ apiVersion = "networking.k8s.io/v1"; kind = "NetworkPolicy"; metadata = @{ name = $targetName; namespace = $namespace; labels = $labels }; spec = @{ podSelector = @{ matchLabels = @{ app = $targetName } }; policyTypes = @("Ingress"); ingress = @(@{ from = @(@{ namespaceSelector = @{ matchLabels = @{ "kubernetes.io/metadata.name" = $ControllerNamespace } }; podSelector = @{ matchLabels = @{ "app.kubernetes.io/instance" = $ReleaseName; "app.kubernetes.io/component" = "k8s-controller" } } }); ports = @(@{ protocol = "TCP"; port = 8080 }) }) } }
    Invoke-Kubectl @("rollout", "status", "deployment/$targetName", "-n", $namespace, "--timeout=120s") | Out-Null

    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "Tool"; metadata = @{ name = "tool-e2e-timestamp"; namespace = $namespace; labels = $labels }; spec = @{ className = "sandbox-openapi"; protocol = "OpenAPI"; description = "Trust workflow timestamp target."; endpoint = "http://$targetName.$namespace.svc.cluster.local:8080/timestamp"; metadata = @{ allowPrivateNetwork = "true" }; inputSchema = @{ type = "object"; properties = @{} } } }
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "ToolGrant"; metadata = @{ name = "trust-demo-tool-grant"; namespace = $namespace; labels = $labels }; spec = @{ agentRef = @{ name = "tool-e2e-agent" }; toolRef = @{ name = "tool-e2e-timestamp" }; verbs = @("invoke") } }
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "Tool"; metadata = @{ name = "trust-demo-publish"; namespace = $namespace; labels = $labels }; spec = @{ className = "sandbox-openapi"; protocol = "OpenAPI"; description = "Persist one approved advisory per run."; endpoint = "http://$targetName.$namespace.svc.cluster.local:8080/publish"; metadata = @{ allowPrivateNetwork = "true" }; inputSchema = @{ type = "object"; required = @("runID", "content"); properties = @{ runID = @{ type = "string" }; content = @{ type = "string" } } } } }
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "ToolGrant"; metadata = @{ name = "trust-demo-publish-grant"; namespace = $namespace; labels = $labels }; spec = @{ agentRef = @{ name = "tool-e2e-agent" }; toolRef = @{ name = "trust-demo-publish" }; verbs = @("invoke") } }
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "AgentRole"; metadata = @{ name = "trust-demo-tool-invoker"; namespace = $namespace; labels = $labels }; rules = @(@{ resources = @("Tool"); verbs = @("get", "invoke") }) }
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "AgentRoleBinding"; metadata = @{ name = "trust-demo-tool-invoker"; namespace = $namespace; labels = $labels }; spec = @{ roleRef = @{ name = "trust-demo-tool-invoker" }; subjects = @(@{ kind = "Agent"; name = "tool-e2e-agent" }) } }
}

function Get-OptionalProperty {
    param(
        [AllowNull()][object]$InputObject,
        [Parameter(Mandatory = $true)][string]$Name,
        [AllowNull()][object]$Default = $null
    )

    if ($null -ne $InputObject -and $null -ne $InputObject.PSObject.Properties[$Name]) {
        return $InputObject.PSObject.Properties[$Name].Value
    }
    return $Default
}

function Get-AuditRunID {
    param([AllowNull()][object]$Entry)

    $details = Get-OptionalProperty -InputObject $Entry -Name "details"
    return Get-OptionalProperty -InputObject $details -Name "runId"
}

function Test-EndpointReady {
    param([AllowNull()][object]$Endpoint)

    $conditions = Get-OptionalProperty -InputObject $Endpoint -Name "conditions"
    return (Get-OptionalProperty -InputObject $conditions -Name "ready" -Default $false) -eq $true
}

function Assert-ServiceHasReadyEndpoint {
    param(
        [AllowNull()][object[]]$EndpointSlices,
        [Parameter(Mandatory = $true)][string]$Service
    )

    $readyEndpoints = @($EndpointSlices | ForEach-Object {
        @(Get-OptionalProperty -InputObject $_ -Name "endpoints" -Default @())
    } | Where-Object { Test-EndpointReady -Endpoint $_ })
    if ($readyEndpoints.Count -lt 1) {
        throw "Service/$Service has no ready EndpointSlice addresses"
    }
    return $readyEndpoints.Count
}

function Wait-ForWorkflowRun {
    param(
        [Parameter(Mandatory = $true)][scriptblock]$Predicate,
        [Parameter(Mandatory = $true)][string]$Description,
        [int]$TimeoutSeconds = 45
    )

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    $terminalFailure = $false
    do {
        $run = Invoke-Kubectl @("get", "workflowrun/trust-demo-run", "-n", $namespace, "-o", "json") | ConvertFrom-Json
        $status = Get-OptionalProperty -InputObject $run -Name "status" -Default ([PSCustomObject]@{})
        if ((Get-OptionalProperty -InputObject $status -Name "phase") -in @("Failed", "Error")) {
            $terminalFailure = $true
            break
        }
        if (& $Predicate $run) {
            return $run
        }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)

    $jobs = Invoke-Kubectl @("get", "agentjobs", "-n", $namespace, "-o", "json") | ConvertFrom-Json
    $events = Invoke-Kubectl @("get", "events", "-n", $namespace, "--field-selector", "involvedObject.kind=WorkflowRun,involvedObject.name=trust-demo-run", "-o", "json") | ConvertFrom-Json
    $runStatus = Get-OptionalProperty -InputObject $run -Name "status" -Default ([PSCustomObject]@{})
    $snapshot = [PSCustomObject]@{
        workflowRun = [PSCustomObject]@{
            phase = Get-OptionalProperty -InputObject $runStatus -Name "phase"
            activeStep = Get-OptionalProperty -InputObject $runStatus -Name "activeStep"
            completedSteps = @(Get-OptionalProperty -InputObject $runStatus -Name "completedSteps" -Default @())
            conditions = @(Get-OptionalProperty -InputObject $runStatus -Name "conditions" -Default @() | ForEach-Object {
                [PSCustomObject]@{
                    type = Get-OptionalProperty -InputObject $_ -Name "type"
                    status = Get-OptionalProperty -InputObject $_ -Name "status"
                    reason = Get-OptionalProperty -InputObject $_ -Name "reason"
                    message = Get-OptionalProperty -InputObject $_ -Name "message"
                }
            })
        }
        agentJobs = @($jobs.items | ForEach-Object {
            $labels = Get-OptionalProperty -InputObject $_.metadata -Name "labels" -Default ([PSCustomObject]@{})
            $jobStatus = Get-OptionalProperty -InputObject $_ -Name "status" -Default ([PSCustomObject]@{})
            $owners = @(Get-OptionalProperty -InputObject $_.metadata -Name "ownerReferences" -Default @())
            [PSCustomObject]@{
                name = $_.metadata.name
                phase = Get-OptionalProperty -InputObject $jobStatus -Name "phase"
                step = Get-OptionalProperty -InputObject $labels -Name "kognition.dev/workflow-step"
                owner = if ($owners.Count -gt 0) { Get-OptionalProperty -InputObject $owners[0] -Name "name" } else { $null }
                output = Get-OptionalProperty -InputObject $jobStatus -Name "output"
            }
        })
        events = @($events.items | Select-Object -Last 10 | ForEach-Object {
            [PSCustomObject]@{
                reason = Get-OptionalProperty -InputObject $_ -Name "reason"
                message = Get-OptionalProperty -InputObject $_ -Name "message"
            }
        })
    } | ConvertTo-Json -Depth 10 -Compress
    if ($terminalFailure) { throw "WorkflowRun failed while waiting for $Description. Last observed state: $snapshot" }
    throw "Timed out waiting for $Description. Last observed state: $snapshot"
}

function Get-PublicationReceipt {
    $code = "import urllib.request; print(urllib.request.urlopen('http://127.0.0.1:8080/publications/$DemoRunId', timeout=10).read().decode())"
    return Invoke-Kubectl @("exec", "deployment/trust-demo-tool-target", "-n", $namespace, "--", "python", "-c", $code) | ConvertFrom-Json
}

function Submit-WorkflowApproval {
    param(
        [ValidateSet("presenter", "automated")][string]$Mode = "presenter",
        [Parameter(Mandatory = $true)][hashtable]$Values
    )

    if ($Mode -eq "presenter") {
        $confirmation = Read-Host "Publication count is zero. Type publish to approve $namespace/trust-demo-run/publish"
        if ($confirmation -cne "publish") { throw "Presenter did not approve publication" }
    }
    $approvalValues = $Values.Clone()
    $approvalValues.APPROVAL_REASON = if ($Mode -eq "presenter") { "Presenter explicitly approved customer-advisory publication." } else { "Automated test approval; not human approval evidence." }
    Import-ManifestTemplate -FileName "approval-decision.json.tmpl" -Values $approvalValues
}

function Assert-PublicationProof {
    param(
        [Parameter(Mandatory = $true)][object]$Run,
        [Parameter(Mandatory = $true)][object]$Approval,
        [Parameter(Mandatory = $true)][object]$Receipt,
        [Parameter(Mandatory = $true)][string]$RunID,
        [Parameter(Mandatory = $true)][string]$Content
    )

    $scope = "kognition-demo-$RunID"
    if ($Run.metadata.namespace -ne $scope -or $Run.metadata.name -ne "trust-demo-run" -or $Run.status.phase -ne "Ready" -or @($Run.status.completedSteps) -notcontains "publish") { throw "Publication workflow is incomplete or belongs to another run" }
    $steps = @($Run.status.snapshotSpec.steps | Where-Object { $_.name -eq "publish" })
    if ($steps.Count -ne 1 -or $steps[0].type -ne "tool" -or $steps[0].toolRef.name -ne "trust-demo-publish") { throw "Workflow lacks a real publication Tool step" }
    $inputValue = $steps[0].input | ConvertFrom-Json
    if ($inputValue.runID -ne $RunID -or $inputValue.content -ne $Content) { throw "Publication input does not match this run" }
    $gates = @($Run.status.snapshotSpec.approvalGates | Where-Object { $_.to -eq "publish" -and $_.requiredApprovers -ge 1 })
    if ($gates.Count -ne 1) { throw "Publication workflow lacks an approval gate" }
    $gateEntered = $Run.metadata.annotations.'kognition.dev/gate-entered.publish'
    $approved = $Run.metadata.annotations.'kognition.dev/approved-steps'
    if (-not $gateEntered -or @($approved -split ',' | ForEach-Object { $_.Trim() }) -notcontains "publish") { throw "Publication approval was not reconciled" }
    if ($Approval.metadata.namespace -ne $scope -or $Approval.spec.workflowRunRef.name -ne $Run.metadata.name -or $Approval.spec.step -ne "publish" -or $Approval.spec.iteration -ne 0 -or $Approval.spec.decision -ne "approve") { throw "Approval does not authorize this publication" }
    if ($Receipt.publicationCount -ne 1 -or $Receipt.runID -ne $RunID -or $Receipt.content -ne $Content) { throw "Publication receipt is missing or mismatched" }
    $approvedAt = [DateTimeOffset]$Approval.metadata.creationTimestamp
    if ($approvedAt -lt [DateTimeOffset]$gateEntered -or [DateTimeOffset]$Receipt.publishedAt -lt $approvedAt) { throw "Publication preceded its approval lifecycle" }
}

function Wait-ForDeploymentReady {
    param(
        [Parameter(Mandatory = $true)][string]$Deployment,
        [Parameter(Mandatory = $true)][int]$Replicas,
        [int]$TimeoutSeconds = 120
    )

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    $admissionProbe = @{
        apiVersion = "kognition.dev/v1alpha1"
        kind = "AgentCommunicationPolicy"
        metadata = @{
            name = "admission-readiness-probe"
            namespace = $ControllerNamespace
        }
        spec = @{ defaultDeny = $true }
    } | ConvertTo-Json -Depth 10 -Compress
    do {
        $deploymentState = (& kubectl @kubectlContextArguments get "deployment/$Deployment" -n $ControllerNamespace -o json 2>$null | ConvertFrom-Json)
        $readyReplicas = if ($deploymentState.status.PSObject.Properties['readyReplicas']) { [int]$deploymentState.status.readyReplicas } else { 0 }
        $availableReplicas = if ($deploymentState.status.PSObject.Properties['availableReplicas']) { [int]$deploymentState.status.availableReplicas } else { 0 }
        if ($deploymentState.status.observedGeneration -ge $deploymentState.metadata.generation -and
            $readyReplicas -eq $Replicas -and
            $availableReplicas -eq $Replicas) {
            $null = $admissionProbe | & kubectl @kubectlContextArguments apply --dry-run=server -f - 2>$null
            if ($LASTEXITCODE -eq 0) { return }
        }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Deployment/$Deployment did not reach $Replicas ready replicas within $TimeoutSeconds seconds"
}

function Assert-ControlPlaneReady {
    $recovery = "Restore it with: helm upgrade $ReleaseName .\deploy\helm\kognition --namespace $ControllerNamespace --reuse-values --wait --timeout 5m"
    try {
        $deployment = Invoke-Kubectl @("get", "deployment/$controllerDeployment", "-n", $ControllerNamespace, "-o", "json") | ConvertFrom-Json
        $replicas = [int]$deployment.spec.replicas
        if ($replicas -lt 1) {
            throw "Deployment/$controllerDeployment has zero desired replicas"
        }
        Wait-ForDeploymentReady -Deployment $controllerDeployment -Replicas $replicas
        Invoke-Kubectl @("get", "service/$webhookService", "-n", $ControllerNamespace) | Out-Null
        Invoke-Kubectl @("get", "secret/$webhookTLSSecret", "-n", $ControllerNamespace) | Out-Null

        $endpointSlices = (Invoke-Kubectl @("get", "endpointslice", "-n", $ControllerNamespace, "-l", "kubernetes.io/service-name=$webhookService", "-o", "json") | ConvertFrom-Json).items
        $readyEndpointCount = Assert-ServiceHasReadyEndpoint -EndpointSlices $endpointSlices -Service $webhookService
        return [PSCustomObject]@{ replicas = $replicas; readyEndpoints = $readyEndpointCount }
    }
    catch {
        throw "Kognition admission control plane is not ready: $($_.Exception.Message). $recovery"
    }
}

function Get-AuditEntries {
    $configMaps = (Invoke-Kubectl @("get", "configmaps", "-n", $ControllerNamespace, "-l", "kognition.dev/cognitive-audit=true", "-o", "json") | ConvertFrom-Json).items
    $entries = @()
    foreach ($configMap in $configMaps) {
        $encoded = $configMap.binaryData.'entries.json'
        if ($encoded) {
            $entries += @([Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($encoded)) | ConvertFrom-Json)
        }
    }
    return @($entries)
}

try {
    if (-not (Get-Command kubectl -ErrorAction SilentlyContinue)) {
        throw "kubectl is required"
    }
    Invoke-Kubectl @("cluster-info") | Out-Null
    $controlPlane = Assert-ControlPlaneReady
    Add-Assertion -Name "control-plane-ready" -Passed $true -Evidence "deployment $controllerDeployment has $($controlPlane.replicas) ready replicas and service $webhookService has $($controlPlane.readyEndpoints) ready endpoints"
    foreach ($crd in @("workflows.kognition.dev", "workflowruns.kognition.dev", "approvaldecisions.kognition.dev")) {
        Invoke-Kubectl @("get", "crd/$crd") | Out-Null
    }

    $existingNamespace = (& kubectl @kubectlContextArguments get namespace $namespace --ignore-not-found -o name 2>$null | Out-String).Trim()
    if ($LASTEXITCODE -ne 0) { throw "Failed to check namespace $namespace" }
    if ($ResumeRetainedScope) {
        Add-Assertion -Name "preflight-retained-scope" -Passed ([bool]$existingNamespace) -Evidence $(if ($existingNamespace) { "namespace $namespace exists for retained evidence collection" } else { "namespace $namespace does not exist" })
    }
    else {
        Add-Assertion -Name "preflight-clean-scope" -Passed (-not $existingNamespace) -Evidence $(if ($existingNamespace) { "namespace $namespace already exists" } else { "namespace $namespace was absent" })
    }
}
catch {
    $failure = $_.Exception.Message
}

try {
    if ($failure) { throw $failure }

    $controllerReplicas = (Invoke-Kubectl @("get", "deployment/$controllerDeployment", "-n", $ControllerNamespace, "-o", "jsonpath={.spec.replicas}") | Out-String).Trim()
    if (-not $controllerReplicas) { $controllerReplicas = "1" }

    if ($ResumeRetainedScope) {
        $retainedAudits = @(Get-AuditEntries | Where-Object { $_.workspace -eq $namespace -and (Get-AuditRunID -Entry $_) -eq $DemoRunId })
        $retainedToolEvidence = @($retainedAudits | Where-Object { $_.eventType -eq "tool_call" -and $_.details.request.invocationId -in @("$DemoRunId`:tool-allow", "$DemoRunId`:tool-revoked") })
        $retainedModelCompleted = @($retainedAudits | Where-Object { $_.eventType -eq "model_call" -and $_.details.outcome -eq "completed" -and $_.details.reasonCode -eq "target_completed" -and $_.details.execution.targetReached -eq $true })
        $retainedCommunicationCompleted = @($retainedAudits | Where-Object { $_.eventType -eq "communication" -and $_.verb -eq "invoke" -and $_.details.outcome -eq "completed" -and $_.details.reasonCode -eq "target_completed" -and $_.details.execution.targetReached -eq $true })
        if (-not (Test-Path $memoryPhaseReportPath)) {
            throw "Retained Memory evidence artifact does not exist: $memoryPhaseReportPath"
        }
        $memoryPhaseReport = Get-Content $memoryPhaseReportPath -Raw | ConvertFrom-Json
        $memoryAssertions = @($memoryPhaseReport.assertions)
        $memoryAllowedArtifact = @($memoryAssertions | Where-Object { $_.name -eq "memory-allow" -and $_.passed -eq $true -and $_.invocationId -eq $memoryPhaseReport.invocationIds.allow -and $_.outcome -eq "completed" -and $_.reasonCode -eq "target_completed" -and $_.executionStatus -eq "completed" -and $_.targetReached -eq $true -and $_.persistedGrant -eq $memoryPhaseReport.grantName })
        $memoryDeniedArtifact = @($memoryAssertions | Where-Object { $_.name -eq "memory-deny" -and $_.passed -eq $true -and $_.invocationId -eq $memoryPhaseReport.invocationIds.deny -and $_.outcome -eq "denied" -and $_.reasonCode -eq "grant_missing" -and $_.targetReached -eq $false })
        $memoryRevokedArtifact = @($memoryAssertions | Where-Object { $_.name -eq "memory-revoked" -and $_.passed -eq $true -and $_.invocationId -eq $memoryPhaseReport.invocationIds.revoked -and $_.outcome -eq "revoked" -and $_.reasonCode -eq "grant_revoked" -and $_.targetReached -eq $false -and $_.auditLatencySeconds -le 5 })
        $memoryArtifactValid = $memoryPhaseReport.schemaVersion -eq "1.0" -and $memoryPhaseReport.phase -eq "Memory" -and $memoryPhaseReport.demoRunId -eq $DemoRunId -and $memoryPhaseReport.namespace -eq $namespace -and $memoryPhaseReport.passed -eq $true -and $memoryAllowedArtifact.Count -eq 1 -and $memoryDeniedArtifact.Count -eq 1 -and $memoryRevokedArtifact.Count -eq 1
        Add-Assertion -Name "tool-phase" -Passed ($retainedToolEvidence.Count -ge 2) -Evidence "$($retainedToolEvidence.Count) retained Tool allow/revoke audit entries found"
        Add-Assertion -Name "memory-phase" -Passed $memoryArtifactValid -Evidence "run-bound Memory artifact proves target execution, missing-grant denial, and live revocation within five seconds"
        Add-Assertion -Name "model-phase" -Passed ($retainedModelCompleted.Count -gt 0) -Evidence "$($retainedModelCompleted.Count) retained Model target completion audit entries found"
        Add-Assertion -Name "communication-phase" -Passed ($retainedCommunicationCompleted.Count -gt 0) -Evidence "$($retainedCommunicationCompleted.Count) retained Communication target completion audit entries found"
    }
    else {
        Invoke-Kubectl @("create", "namespace", $namespace) | Out-Null
        $namespaceCreated = $true

        & (Join-Path $PSScriptRoot "trust-profile-phases.ps1") -KubernetesContext $KubernetesContext -ControllerNamespace $ControllerNamespace -ReleaseName $ReleaseName -DemoRunId $DemoRunId -ScopeNamespace $namespace -KeepScopeNamespace
        $assertions.Add([PSCustomObject]@{ name = "model-phase"; passed = $true; evidence = "a signed source-session capability completed a real Model Gateway provider call" })
        $assertions.Add([PSCustomObject]@{ name = "communication-phase"; passed = $true; evidence = "a signed source session exchanged an exact capability and invoked a ready AgentService" })

        & (Join-Path $PSScriptRoot "kubernetes-tool-gateway-e2e.ps1") -KubernetesContext $KubernetesContext -ControllerNamespace $ControllerNamespace -ReleaseName $ReleaseName -DemoRunId $DemoRunId -ScopeNamespace $namespace -FixtureModelName "trust-profile-model" -ToolTargetNamespace $namespace -ProvisionToolTarget -KeepScopeNamespace
        $assertions.Add([PSCustomObject]@{ name = "tool-phase"; passed = $true; evidence = "allow, replay, conflict, and live revocation assertions passed" })
        Wait-ForDeploymentReady -Deployment $controllerDeployment -Replicas $controllerReplicas

        & (Join-Path $PSScriptRoot "kubernetes-memory-boundary-e2e.ps1") -KubernetesContext $KubernetesContext -ControllerNamespace $ControllerNamespace -ReleaseName $ReleaseName -DemoRunId $DemoRunId -ScopeNamespace $namespace -FixtureModelName "trust-profile-model" -ReportPath $memoryPhaseReportPath -KeepScopeNamespace
        $memoryPhaseReport = Get-Content $memoryPhaseReportPath -Raw | ConvertFrom-Json
        $assertions.Add([PSCustomObject]@{ name = "memory-phase"; passed = $true; evidence = "allow, direct denial, and live grant revocation assertions passed without restarting the policy gateway" })

        Wait-ForDeploymentReady -Deployment $controllerDeployment -Replicas $controllerReplicas

    }

    Install-WorkflowFixtures
    $requesterInfo = Invoke-Kubectl @("auth", "whoami", "-o", "json") | ConvertFrom-Json
    if (-not $requesterInfo.status.userInfo.username) { throw "Could not determine the authenticated Kubernetes requester" }
    $templateValues = @{
        NAMESPACE = $namespace
        DEMO_RUN_ID = $DemoRunId
        WORKFLOW_KEY = "$DemoRunId`:workflow-approval"
        APPROVAL_KEY = "$DemoRunId`:approval-decision"
        APPROVER = $requesterInfo.status.userInfo.username
    }
    $existingWorkflowRun = (& kubectl @kubectlContextArguments get workflowrun/trust-demo-run -n $namespace --ignore-not-found -o json 2>$null | Out-String).Trim()
    if ($LASTEXITCODE -ne 0) { throw "Failed to inspect existing WorkflowRun trust-demo-run" }
    if ($existingWorkflowRun -and ($existingWorkflowRun | ConvertFrom-Json).status.phase -eq "Failed") {
        throw "Retained WorkflowRun failed; preserve its evidence and use a fresh DemoRunId"
    }
    Import-ManifestTemplate -FileName "workflow-communication-policy.json.tmpl" -Values $templateValues
    Import-ManifestTemplate -FileName "workflow-approval.json.tmpl" -Values $templateValues
    Import-ManifestTemplate -FileName "workflow-run.json.tmpl" -Values $templateValues

    $currentWorkflowRun = Invoke-Kubectl @("get", "workflowrun/trust-demo-run", "-n", $namespace, "-o", "json") | ConvertFrom-Json
    $currentRunStatus = Get-OptionalProperty -InputObject $currentWorkflowRun -Name "status" -Default ([PSCustomObject]@{})
    $retainedApprovalComplete = $ResumeRetainedScope -and
        (Get-OptionalProperty -InputObject $currentRunStatus -Name "phase") -eq "Ready" -and
        @(Get-OptionalProperty -InputObject $currentRunStatus -Name "completedSteps" -Default @()) -contains "publish"
    if ($retainedApprovalComplete) {
        $runAnnotations = Get-OptionalProperty -InputObject $currentWorkflowRun.metadata -Name "annotations" -Default ([PSCustomObject]@{})
        $gateEnteredValue = Get-OptionalProperty -InputObject $runAnnotations -Name "kognition.dev/gate-entered.publish"
        $approvedSteps = Get-OptionalProperty -InputObject $runAnnotations -Name "kognition.dev/approved-steps"
        $snapshotSpec = Get-OptionalProperty -InputObject $currentRunStatus -Name "snapshotSpec" -Default ([PSCustomObject]@{})
        $approvalGates = @(Get-OptionalProperty -InputObject $snapshotSpec -Name "approvalGates" -Default @())
        $publishGate = @($approvalGates | Where-Object { $_.to -eq "publish" -and $_.requiredApprovers -ge 1 })
        $publishSteps = @($snapshotSpec.steps | Where-Object { $_.name -eq "publish" -and $_.type -eq "tool" -and $_.toolRef.name -eq "trust-demo-publish" })
        $approval = Invoke-Kubectl @("get", "approvaldecision/trust-demo-approval", "-n", $namespace, "-o", "json") | ConvertFrom-Json
        if (-not $gateEnteredValue -or $publishGate.Count -ne 1) { throw "Retained WorkflowRun lacks durable publish approval-gate evidence" }
        if ($publishSteps.Count -ne 1) { throw "Retained WorkflowRun used simulated publication; use a fresh DemoRunId" }
        if ($approval.spec.workflowRunRef.name -ne "trust-demo-run" -or $approval.spec.step -ne "publish" -or $approval.spec.decision -ne "approve") { throw "Retained ApprovalDecision does not explicitly approve trust-demo-run/publish" }
        if (-not $approvedSteps -or @($approvedSteps -split ',' | ForEach-Object { $_.Trim() }) -notcontains "publish") { throw "Retained WorkflowRun does not record reconciled publish approval" }
        $publicationReceipt = Get-PublicationReceipt
        Assert-PublicationProof -Run $currentWorkflowRun -Approval $approval -Receipt $publicationReceipt -RunID $DemoRunId -Content $publicationContent
        Add-Assertion -Name "workflow-waiting-approval" -Passed $true -Evidence "controller recorded publish gate entry at $gateEnteredValue before the explicit approval at $($approval.metadata.creationTimestamp)"
        Add-Assertion -Name "approval-decision-reconciled" -Passed $true -Evidence "retained ApprovalDecision explicitly approved publish and WorkflowRun recorded the approved step"
        Add-Assertion -Name "workflow-publish-completed" -Passed $true -Evidence "retained advisory content and durable receipt match the completed Tool workflow"
    }
    else {
        $null = Wait-ForWorkflowRun -Description "the explicit approval gate" -Predicate {
            param($run)
            $runStatus = Get-OptionalProperty -InputObject $run -Name "status" -Default ([PSCustomObject]@{})
            $conditions = @(Get-OptionalProperty -InputObject $runStatus -Name "conditions" -Default @())
            @($conditions | Where-Object { $_.reason -eq "WaitingApproval" }).Count -gt 0 -and
                (Get-OptionalProperty -InputObject $runStatus -Name "activeStep") -eq "publish"
        }
        $beforeApproval = Get-PublicationReceipt
        Add-Assertion -Name "workflow-waiting-approval" -Passed ($beforeApproval.publicationCount -eq 0) -Evidence "publish remained gated with WaitingApproval and zero target publications"

        Submit-WorkflowApproval -Mode $ApprovalMode -Values $templateValues
        $null = Wait-ForWorkflowRun -Description "ApprovalDecision reconciliation" -Predicate {
            param($run)
            $metadata = Get-OptionalProperty -InputObject $run -Name "metadata" -Default ([PSCustomObject]@{})
            $annotations = Get-OptionalProperty -InputObject $metadata -Name "annotations" -Default ([PSCustomObject]@{})
            $approved = Get-OptionalProperty -InputObject $annotations -Name "kognition.dev/approved-steps"
            $approved -and @($approved -split ',' | ForEach-Object { $_.Trim() }) -contains "publish"
        } -TimeoutSeconds 300
        Add-Assertion -Name "approval-decision-reconciled" -Passed $true -Evidence "ApprovalDecision trust-demo-approval persisted and approved publish"

        $completedRun = Wait-ForWorkflowRun -Description "approval-protected publish completion" -Predicate {
            param($run)
            $runStatus = Get-OptionalProperty -InputObject $run -Name "status" -Default ([PSCustomObject]@{})
            $conditions = @(Get-OptionalProperty -InputObject $runStatus -Name "conditions" -Default @())
            $waiting = @($conditions | Where-Object { $_.reason -eq "WaitingApproval" })
            (Get-OptionalProperty -InputObject $runStatus -Name "phase") -eq "Ready" -and
                @(Get-OptionalProperty -InputObject $runStatus -Name "completedSteps" -Default @()) -contains "publish" -and
                $waiting.Count -eq 0
        }
        $publicationReceipt = Get-PublicationReceipt
        $approval = Invoke-Kubectl @("get", "approvaldecision/trust-demo-approval", "-n", $namespace, "-o", "json") | ConvertFrom-Json
        Assert-PublicationProof -Run $completedRun -Approval $approval -Receipt $publicationReceipt -RunID $DemoRunId -Content $publicationContent
        Add-Assertion -Name "workflow-publish-completed" -Passed $true -Evidence "target persisted the run-bound advisory after approval; WorkflowRun reached $($completedRun.status.phase) naturally"
    }

    Invoke-Kubectl @("rollout", "restart", "deployment/trust-demo-tool-target", "-n", $namespace) | Out-Null
    Invoke-Kubectl @("rollout", "status", "deployment/trust-demo-tool-target", "-n", $namespace, "--timeout=120s") | Out-Null
    $afterRestart = Get-PublicationReceipt
    Add-Assertion -Name "publication-survives-restart" -Passed ($afterRestart.publicationCount -eq 1 -and $afterRestart.content -eq $publicationReceipt.content -and $afterRestart.publishedAt -eq $publicationReceipt.publishedAt) -Evidence "target pod restart preserved exactly one advisory and its original receipt"

    $audits = @(Get-AuditEntries | Where-Object { $_.workspace -eq $namespace })
    $toolAllowed = @($audits | Where-Object { $_.eventType -eq "tool_call" -and (Get-AuditRunID -Entry $_) -eq $DemoRunId -and $_.details.request.invocationId -eq "$DemoRunId`:tool-allow" -and $_.details.outcome -eq "allowed" })
    $toolRevoked = @($audits | Where-Object { $_.eventType -eq "tool_call" -and (Get-AuditRunID -Entry $_) -eq $DemoRunId -and $_.details.request.invocationId -eq "$DemoRunId`:tool-revoked" -and $_.details.outcome -eq "revoked" })
    $memoryAllowed = @($audits | Where-Object { $_.eventType -eq "memory_op" -and (Get-AuditRunID -Entry $_) -eq $DemoRunId -and (Get-OptionalProperty -InputObject $_.details.request -Name "invocationId") -eq $memoryPhaseReport.invocationIds.allow -and $_.verb -eq "store" -and $_.details.outcome -eq "completed" -and $_.details.reasonCode -eq "target_completed" -and $_.details.execution.status -eq "completed" -and $_.details.execution.targetReached -eq $true })
    $memoryDenied = @($audits | Where-Object { $_.eventType -eq "memory_op" -and (Get-AuditRunID -Entry $_) -eq $DemoRunId -and (Get-OptionalProperty -InputObject $_.details.request -Name "invocationId") -eq $memoryPhaseReport.invocationIds.deny -and $_.verb -eq "store" -and $_.details.outcome -eq "denied" -and $_.details.reasonCode -eq "grant_missing" })
    $memoryRevoked = @($audits | Where-Object { $_.eventType -eq "memory_op" -and (Get-AuditRunID -Entry $_) -eq $DemoRunId -and (Get-OptionalProperty -InputObject $_.details.request -Name "invocationId") -eq $memoryPhaseReport.invocationIds.revoked -and $_.verb -eq "search" -and $_.details.outcome -eq "revoked" -and $_.details.reasonCode -eq "grant_revoked" })
    $modelAllowed = @($audits | Where-Object { $_.eventType -eq "model_call" -and (Get-AuditRunID -Entry $_) -eq $DemoRunId -and $_.details.outcome -eq "allowed" -and $_.details.reasonCode -eq "authorized" })
    $modelCompleted = @($audits | Where-Object { $_.eventType -eq "model_call" -and (Get-AuditRunID -Entry $_) -eq $DemoRunId -and $_.details.outcome -eq "completed" -and $_.details.reasonCode -eq "target_completed" -and $_.details.execution.targetReached -eq $true })
    $communicationAllowed = @($audits | Where-Object { $_.eventType -eq "communication" -and (Get-AuditRunID -Entry $_) -eq $DemoRunId -and $_.verb -eq "exchange" -and $_.details.outcome -eq "allowed" -and $_.details.reasonCode -eq "authorized" })
    $communicationCompleted = @($audits | Where-Object { $_.eventType -eq "communication" -and (Get-AuditRunID -Entry $_) -eq $DemoRunId -and $_.verb -eq "invoke" -and $_.details.outcome -eq "completed" -and $_.details.reasonCode -eq "target_completed" -and $_.details.execution.targetReached -eq $true })
    $memoryEvidenceComplete = ($memoryAllowed.Count -gt 0 -and $memoryDenied.Count -gt 0 -and $memoryRevoked.Count -gt 0) -or ($null -ne $memoryPhaseReport -and $memoryPhaseReport.passed -eq $true)
    $fourPlaneEvidence = $toolAllowed.Count -gt 0 -and $toolRevoked.Count -gt 0 -and $memoryEvidenceComplete -and $modelAllowed.Count -gt 0 -and $modelCompleted.Count -gt 0 -and $communicationAllowed.Count -gt 0 -and $communicationCompleted.Count -gt 0
    Add-Assertion -Name "decision-audit-correlation" -Passed $fourPlaneEvidence -Evidence "correlated run $DemoRunId across Tool allow/revoke, Memory allow/deny/revoke, Model allow/completion, and Communication exchange/completion evidence"

    $normalizedDecisions = @($audits | Where-Object { (Get-AuditRunID -Entry $_) -eq $DemoRunId -and (Get-OptionalProperty -InputObject $_.details -Name "schemaVersion") -eq 1 })
    if ($normalizedDecisions.Count -eq 0) { throw "No normalized decisions for approval verification" }
    foreach ($entry in $normalizedDecisions) { Assert-ApprovalEvidence -Decision $entry.details }
    Add-Assertion -Name "decision-approval-semantics" -Passed $true -Evidence "$($normalizedDecisions.Count) normalized records distinguish skipped, applicable, and resolved approval evidence"

    foreach ($entry in ($audits | Where-Object { ($_.eventType -in @("tool_call", "memory_op", "model_call", "communication") -and (Get-AuditRunID -Entry $_) -eq $DemoRunId) -or $_.eventType -eq "grant_change" } | Sort-Object timestamp)) {
        $timeline.Add([PSCustomObject]@{
            timestamp = $entry.timestamp
            source = "CognitiveAuditLog"
            plane = Get-OptionalProperty -InputObject $entry.details -Name "plane"
            eventType = $entry.eventType
            operation = $entry.verb
            outcome = Get-OptionalProperty -InputObject $entry.details -Name "outcome"
            reasonCode = Get-OptionalProperty -InputObject $entry.details -Name "reasonCode"
            subjectRef = $entry.subjectRef
        })
    }
    if ($memoryPhaseReport) {
        foreach ($entry in @($memoryPhaseReport.assertions | Sort-Object timestamp)) {
            $timeline.Add([PSCustomObject]@{
                timestamp = $entry.timestamp
                source = "MemoryPhaseReport"
                plane = "memory"
                eventType = "memory_op"
                operation = $entry.name
                outcome = $entry.outcome
                reasonCode = $entry.reasonCode
                subjectRef = $entry.invocationId
            })
        }
    }
    $approval = Invoke-Kubectl @("get", "approvaldecision/trust-demo-approval", "-n", $namespace, "-o", "json") | ConvertFrom-Json
    $timeline.Add([PSCustomObject]@{
        timestamp = $approval.metadata.creationTimestamp
        source = "ApprovalDecision"
        plane = "workflow"
        eventType = "approval_decision"
        operation = $approval.spec.decision
        outcome = "approved"
        reasonCode = if ($ResumeRetainedScope) { "retained_approval_decision" } elseif ($ApprovalMode -eq "presenter") { "presenter_approval" } else { "automated_approval_decision" }
        subjectRef = "$namespace/trust-demo-run/publish"
    })
    $assertions.Add([PSCustomObject]@{ name = "decision-timeline"; passed = $true; evidence = "$($timeline.Count) durable evidence entries collected" })
}
catch {
    $failure = $_.Exception.Message
}
finally {
    if ($namespaceCreated -and -not $KeepScopeNamespace) {
        $cleanup = Remove-RehearsalScope
        $cleaned = $cleanup.status -eq "passed"
        $assertions.Add([PSCustomObject]@{ name = "cleanup"; passed = $cleaned; evidence = $cleanup.evidence })
        if (-not $cleaned -and -not $failure) { $failure = $cleanup.evidence }
    }
    elseif ($namespaceCreated -and $KeepScopeNamespace) {
        $cleanup = [PSCustomObject]@{ status = "retained"; evidence = "namespace $namespace retained by explicit request" }
    }
    elseif ($ResumeRetainedScope) {
        $cleanup = [PSCustomObject]@{ status = "not-owned"; evidence = "retained namespace $namespace was not modified by cleanup" }
    }

    $requiredAssertions = @(
        "control-plane-ready",
        $(if ($ResumeRetainedScope) { "preflight-retained-scope" } else { "preflight-clean-scope" }),
        "tool-phase",
        "memory-phase",
        "model-phase",
        "communication-phase",
        "workflow-waiting-approval",
        "approval-decision-reconciled",
        "workflow-publish-completed",
        "publication-survives-restart",
        "decision-audit-correlation",
        "decision-approval-semantics",
        "decision-timeline"
    )
    if (-not $ResumeRetainedScope -and -not $KeepScopeNamespace) { $requiredAssertions += "cleanup" }
    $recordedAssertionNames = @($assertions | ForEach-Object { $_.name })
    $missingAssertions = @($requiredAssertions | Where-Object { $_ -notin $recordedAssertionNames })
    if ($missingAssertions.Count -gt 0 -and -not $failure) {
        $failure = "Incomplete E2E run; missing assertions: $($missingAssertions -join ', ')"
    }

    $reportDirectory = Split-Path $ReportPath -Parent
    if ($reportDirectory) { [IO.Directory]::CreateDirectory($reportDirectory) | Out-Null }
    $report = [PSCustomObject]@{
        schemaVersion = "1.0"
        demoRunId = $DemoRunId
        namespace = $namespace
        verificationScope = "kubernetes-gated-publication"
        deployedDemoParityVerified = $false
        recordingReady = $false
        approvalSubmission = if ($ResumeRetainedScope) { "retained-decision" } else { $ApprovalMode }
        publicationCompletion = "durable-tool-target"
        publicationReceipt = $publicationReceipt
        startedAt = $startedAt.ToString("o")
        completedAt = [DateTimeOffset]::UtcNow.ToString("o")
        passed = -not $failure -and @($assertions | Where-Object { -not $_.passed }).Count -eq 0
        failure = $failure
        cleanup = $cleanup
        idempotencyKeys = [PSCustomObject]@{
            toolAllow = "$DemoRunId`:tool-allow"
            toolRevoked = "$DemoRunId`:tool-revoked"
            memoryAllow = if ($memoryPhaseReport) { $memoryPhaseReport.invocationIds.allow } else { $null }
            memoryDeny = if ($memoryPhaseReport) { $memoryPhaseReport.invocationIds.deny } else { $null }
            memoryRevoked = if ($memoryPhaseReport) { $memoryPhaseReport.invocationIds.revoked } else { $null }
            communicationInvocation = if ($communicationCompleted.Count -eq 1) { Get-OptionalProperty -InputObject $communicationCompleted[0].details.request -Name "invocationId" } else { $null }
            workflowApproval = "$DemoRunId`:workflow-approval"
            approvalDecision = "$DemoRunId`:approval-decision"
        }
        assertions = @($assertions)
        runProof = [PSCustomObject]@{
            schemaVersion = 1
            scenario = "Customer advisory with runtime protection"
            protectedAction = "Approval-gated advisory publication with a durable receipt"
            mode = "kubernetes"
            reportRef = Split-Path $ReportPath -Leaf
            memoryPhaseReportRef = Split-Path $memoryPhaseReportPath -Leaf
        }
        decisionTimeline = @($timeline | Sort-Object timestamp)
    }
    $report | ConvertTo-Json -Depth 30 | Set-Content -Path $ReportPath -Encoding utf8
    Write-Host "Assertion report: $ReportPath"
}

if ($failure) {
    throw $failure
}
Write-Host "Demo trustworthiness E2E passed for $DemoRunId" -ForegroundColor Green
