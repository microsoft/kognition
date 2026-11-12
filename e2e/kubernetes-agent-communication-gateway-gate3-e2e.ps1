# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

param(
    [string]$KubernetesContext = "",
    [string]$ControllerNamespace = "kognition-system",
    [string]$ReleaseName = "kognition"
)

# ADR-0014 Runtime Autonomy Gate 3 - Communication plane Kubernetes E2E.
#
# Scenario: A source Agent with a live session capability and an authorizing
# CrossNamespaceGrant + AgentCommunicationPolicy exchanges an exact
# delegation capability for a target AgentService and then invokes it via
# the Agent Communication Gateway. No TrustProfile exists, so the resolver
# falls back to the built-in "supervised" AutonomyClass
# (ApprovalPolicy.ExternalCommunication="always", RequiresApproval=true).
# The invocation MUST persist a RuntimeApproval CR with plane=Communication,
# operation=invoke, status.decisionState=Pending, and MUST NOT be
# dispatched to the target AgentService.
#
# Retry invariance: replaying the invocation with the same invocation_id
# (the Gate 3 dedup key) must not create a second RuntimeApproval CR and
# must return the same Pending body.
#
# Persistence-only: this test verifies Gate 3 stops the delegation before
# upstream dispatch and durably records the pending decision. Progressing a
# RuntimeApproval to Approved is out of scope - the validating admission
# webhook rejects status writes from non-controller usernames.

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

. (Join-Path $PSScriptRoot "capability-mint.ps1")

$gatewayService = "$ReleaseName-agent-communication-gateway"
$controllerDeployment = "$ReleaseName-k8s-controller"
$webhookConfigName = "$ReleaseName-validating"
$gatewayPortForward = $null
$controllerReplicas = $null
$originalFailurePolicies = $null
$runID = [string][DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
$sourceNamespace = "agent-comm-gate3-src-$runID"
$targetNamespace = "agent-comm-gate3-tgt-$runID"
$kubectlContextArguments = if ($KubernetesContext) { @("--context", $KubernetesContext) } else { @() }

function Invoke-Kubectl {
    param([Parameter(Mandatory = $true)][string[]]$Arguments)

    $output = & kubectl @kubectlContextArguments @Arguments 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "kubectl $($Arguments -join ' ') failed:`n$($output -join "`n")"
    }
    return $output
}

function Get-AvailableTcpPort {
    $listener = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback, 0)
    try {
        $listener.Start()
        return ([System.Net.IPEndPoint]$listener.LocalEndpoint).Port
    }
    finally {
        $listener.Stop()
    }
}

function Start-ServicePortForward {
    param(
        [Parameter(Mandatory = $true)][string]$Namespace,
        [Parameter(Mandatory = $true)][string]$Service,
        [Parameter(Mandatory = $true)][int]$LocalPort,
        [Parameter(Mandatory = $true)][int]$ServicePort
    )

    $portForward = Start-Job -ScriptBlock {
        param($JobNamespace, $JobService, $JobLocalPort, $JobServicePort)
        kubectl -n $JobNamespace port-forward "service/$JobService" "${JobLocalPort}:${JobServicePort}"
    } -ArgumentList $Namespace, $Service, $LocalPort, $ServicePort
    $deadline = [DateTime]::UtcNow.AddSeconds(30)
    do {
        $output = @(Receive-Job $portForward -Keep -ErrorAction SilentlyContinue)
        if ($output -match 'Forwarding from') {
            return $portForward
        }
        if ($portForward.State -in @('Completed', 'Failed', 'Stopped')) {
            $details = $output -join "`n"
            Remove-Job $portForward -Force -ErrorAction SilentlyContinue
            throw "kubectl port-forward service/$Service failed to start: $details"
        }
        [Threading.Tasks.Task]::Delay(250).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)

    Stop-Job $portForward -ErrorAction SilentlyContinue
    $details = @(Receive-Job $portForward -ErrorAction SilentlyContinue) -join "`n"
    Remove-Job $portForward -Force -ErrorAction SilentlyContinue
    throw "kubectl port-forward service/$Service did not become ready within 30 seconds: $details"
}

function Apply-KubernetesObject {
    param([Parameter(Mandatory = $true)][hashtable]$Object)

    $Object | ConvertTo-Json -Depth 20 -Compress | & kubectl @kubectlContextArguments apply -f - 2>&1 | Out-Host
    if ($LASTEXITCODE -ne 0) {
        throw "apply $($Object.kind)/$($Object.metadata.name) failed"
    }
}

function Patch-SubresourceStatus {
    param(
        [Parameter(Mandatory = $true)][string]$Resource,
        [Parameter(Mandatory = $true)][string]$Namespace,
        [Parameter(Mandatory = $true)][hashtable]$Status
    )

    $patchFile = [IO.Path]::GetTempFileName()
    try {
        @{ status = $Status } | ConvertTo-Json -Depth 20 -Compress | Set-Content -Path $patchFile -NoNewline
        Invoke-Kubectl @("patch", $Resource, "-n", $Namespace, "--subresource=status", "--type=merge", "--patch-file", $patchFile) | Out-Null
    }
    finally {
        Remove-Item -Path $patchFile -Force -ErrorAction SilentlyContinue
    }
}

function Wait-ForJsonPath {
    param(
        [Parameter(Mandatory = $true)][string]$Namespace,
        [Parameter(Mandatory = $true)][string]$Resource,
        [Parameter(Mandatory = $true)][string]$JsonPath,
        [Parameter(Mandatory = $true)][string]$Expected,
        [int]$TimeoutSeconds = 90
    )

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        $value = (& kubectl @kubectlContextArguments get $Resource -n $Namespace "-o=jsonpath=$JsonPath" 2>$null | Out-String).Trim()
        if ($value -eq $Expected) { return }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "$Resource in $Namespace did not reach $JsonPath=$Expected within $TimeoutSeconds seconds"
}

function Wait-ForDeploymentScaleDown {
    param([Parameter(Mandatory = $true)][string]$Deployment)

    $deadline = [DateTime]::UtcNow.AddSeconds(60)
    do {
        $replicas = (& kubectl @kubectlContextArguments get "deployment/$Deployment" -n $ControllerNamespace -o jsonpath='{.status.replicas}' 2>$null | Out-String).Trim()
        $controllerPods = @(& kubectl @kubectlContextArguments get pods -n $ControllerNamespace -l 'app.kubernetes.io/component=k8s-controller' -o name 2>$null | Where-Object { $_ })
        if ((-not $replicas -or $replicas -eq "0") -and $controllerPods.Count -eq 0) { return }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Deployment/$Deployment did not scale down within 60 seconds"
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
        metadata = @{ name = "admission-readiness-probe"; namespace = $ControllerNamespace }
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

function Set-WebhookFailurePolicyIgnore {
    # The audit.kognition.dev validating webhook is served by the k8s-controller
    # Pod (via the kognition-webhook Service). Scaling the controller to 0 makes
    # every CREATE/UPDATE/DELETE on kognition.dev resources fail with 503, which
    # would prevent the agent-communication-gateway from creating the Gate 3
    # RuntimeApproval CR. Flipping every webhook in the kognition-validating
    # configuration to failurePolicy=Ignore lets those k8s API calls succeed
    # while the controller is down; the original policies are restored on
    # cleanup. Also captures each webhook's original policy on the returned
    # object so Restore-WebhookFailurePolicy can put it back verbatim.
    $config = (Invoke-Kubectl @("get", "validatingwebhookconfiguration", $webhookConfigName, "-o", "json")) -join "`n" | ConvertFrom-Json
    $originals = @{}
    $patchEntries = @()
    for ($i = 0; $i -lt $config.webhooks.Count; $i++) {
        $originals[$config.webhooks[$i].name] = [string]$config.webhooks[$i].failurePolicy
        $patchEntries += @{ op = "replace"; path = "/webhooks/$i/failurePolicy"; value = "Ignore" }
    }
    $patchFile = [IO.Path]::GetTempFileName()
    try {
        ConvertTo-Json -InputObject $patchEntries -Compress -Depth 5 | Set-Content -Path $patchFile -NoNewline
        Invoke-Kubectl @("patch", "validatingwebhookconfiguration", $webhookConfigName, "--type=json", "--patch-file", $patchFile) | Out-Null
    }
    finally {
        Remove-Item -Path $patchFile -Force -ErrorAction SilentlyContinue
    }
    return $originals
}

function Restore-WebhookFailurePolicy {
    param([Parameter(Mandatory = $true)][hashtable]$Originals)

    $config = (Invoke-Kubectl @("get", "validatingwebhookconfiguration", $webhookConfigName, "-o", "json")) -join "`n" | ConvertFrom-Json
    $patchEntries = @()
    for ($i = 0; $i -lt $config.webhooks.Count; $i++) {
        $name = $config.webhooks[$i].name
        if (-not $Originals.ContainsKey($name)) { continue }
        $patchEntries += @{ op = "replace"; path = "/webhooks/$i/failurePolicy"; value = $Originals[$name] }
    }
    if ($patchEntries.Count -eq 0) { return }
    $patchFile = [IO.Path]::GetTempFileName()
    try {
        ConvertTo-Json -InputObject $patchEntries -Compress -Depth 5 | Set-Content -Path $patchFile -NoNewline
        Invoke-Kubectl @("patch", "validatingwebhookconfiguration", $webhookConfigName, "--type=json", "--patch-file", $patchFile) | Out-Null
    }
    finally {
        Remove-Item -Path $patchFile -Force -ErrorAction SilentlyContinue
    }
}

function Get-SecretKeyString {
    param(
        [Parameter(Mandatory = $true)][string]$Secret,
        [Parameter(Mandatory = $true)][string]$Key
    )

    return (& kubectl @kubectlContextArguments get secret $Secret -n $ControllerNamespace "-o=jsonpath={.data.$Key}" | Out-String).Trim()
}

function Invoke-HttpRequest {
    param(
        [Parameter(Mandatory = $true)][string]$Uri,
        [Parameter(Mandatory = $true)][string]$Method,
        [hashtable]$Headers = @{},
        [string]$Body = "",
        [int]$TimeoutSeconds = 30
    )

    $request = @{ Uri = $Uri; Method = $Method; Headers = $Headers; TimeoutSec = $TimeoutSeconds; ErrorAction = "Stop" }
    if ($Body) {
        $request.ContentType = "application/json"
        $request.Body = $Body
    }
    if ($PSVersionTable.PSVersion.Major -ge 7) {
        $request.SkipHttpErrorCheck = $true
        return Invoke-WebRequest @request
    }
    try {
        return Invoke-WebRequest @request -UseBasicParsing
    }
    catch {
        if (-not $_.Exception.Response) { throw }
        $response = $_.Exception.Response
        $reader = [IO.StreamReader]::new($response.GetResponseStream())
        try {
            return [PSCustomObject]@{ StatusCode = [int]$response.StatusCode; Content = $reader.ReadToEnd() }
        }
        finally {
            $reader.Dispose()
            $response.Dispose()
        }
    }
}

function Wait-ForHttpStatus {
    param(
        [Parameter(Mandatory = $true)][string]$Uri,
        [Parameter(Mandatory = $true)][int]$ExpectedStatus,
        [int]$TimeoutSeconds = 30
    )

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        try {
            $response = Invoke-HttpRequest -Uri $Uri -Method Get -TimeoutSeconds 2
            if ($response.StatusCode -eq $ExpectedStatus) { return }
        }
        catch { }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "$Uri did not return $ExpectedStatus within $TimeoutSeconds seconds"
}

function Wait-ForPendingCommunicationApproval {
    param(
        [Parameter(Mandatory = $true)][string]$SourceAgent,
        [Parameter(Mandatory = $true)][string]$TargetServiceName,
        [Parameter(Mandatory = $true)][string]$TargetNamespaceName,
        [Parameter(Mandatory = $true)][string]$ExpectedClass,
        [Parameter(Mandatory = $true)][string]$InvocationID,
        [int]$TimeoutSeconds = 60
    )

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        $list = ((Invoke-Kubectl @("get", "runtimeapprovals", "-n", $sourceNamespace, "-o", "json")) -join "`n" | ConvertFrom-Json).items
        foreach ($ra in $list) {
            if ($ra.spec.plane -ne "Communication") { continue }
            if ($ra.spec.operation -ne "invoke") { continue }
            if ($ra.spec.agentRef.name -ne $SourceAgent) { continue }
            if ($ra.spec.targetRef.kind -ne "AgentService") { continue }
            if ($ra.spec.targetRef.name -ne $TargetServiceName) { continue }
            if ($ra.spec.targetRef.namespace -ne $TargetNamespaceName) { continue }
            if ($ra.spec.invocationID -ne $InvocationID) { continue }
            if ($ra.status.decisionState -ne "Pending") { continue }
            if ($ra.spec.autonomyClassRef.name -ne $ExpectedClass) {
                throw "RuntimeApproval $($ra.metadata.name) bound to AutonomyClass '$($ra.spec.autonomyClassRef.name)', want '$ExpectedClass'"
            }
            return $ra
        }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Pending Communication RuntimeApproval for Agent=$SourceAgent Target=$TargetNamespaceName/$TargetServiceName InvocationID=$InvocationID did not appear within $TimeoutSeconds seconds"
}

function Get-CommunicationApprovalsForInvocation {
    param([Parameter(Mandatory = $true)][string]$InvocationID)

    $list = ((Invoke-Kubectl @("get", "runtimeapprovals", "-n", $sourceNamespace, "-o", "json")) -join "`n" | ConvertFrom-Json).items
    return @($list | Where-Object { $_.spec.plane -eq "Communication" -and $_.spec.invocationID -eq $InvocationID })
}

Write-Host "=== Kognition Agent Communication Gateway Gate 3 E2E ===" -ForegroundColor Cyan

try {
    Invoke-Kubectl @("get", "deployment/$gatewayService", "-n", $ControllerNamespace) | Out-Null
    Invoke-Kubectl @("get", "deployment/$controllerDeployment", "-n", $ControllerNamespace) | Out-Null
    Invoke-Kubectl @("wait", "-n", $ControllerNamespace, "--for=condition=Available", "deployment/$gatewayService", "--timeout=120s") | Out-Null
    Invoke-Kubectl @("create", "namespace", $sourceNamespace) | Out-Null
    Invoke-Kubectl @("create", "namespace", $targetNamespace) | Out-Null

    $sourceAgent = "gate3-comm-planner"
    $targetAgent = "gate3-comm-reviewer"
    $sourceInstance = "gate3-comm-planner-instance"
    $sourceIdentity = "agent-identity-$sourceAgent"
    $targetService = "gate3-comm-review-service"
    $grantName = "gate3-comm-allow-source"
    $policyName = "gate3-comm-allow-planner"
    $sessionID = "gate3-comm-session-$runID"

    # No TrustProfile: the resolver defaults to the built-in "supervised"
    # AutonomyClass (ApprovalPolicy.ExternalCommunication="always",
    # RequiresApproval=true), so every delegation invoke must gate to Pending.
    Apply-KubernetesObject @{
        apiVersion = "kognition.dev/v1alpha1"
        kind = "Agent"
        metadata = @{ name = $sourceAgent; namespace = $sourceNamespace }
        spec = @{ modelRef = @{ name = "placeholder" }; instructions = @{ inline = "Gate 3 delegation source fixture." }; replicas = 0 }
    }
    Wait-ForJsonPath -Namespace $sourceNamespace -Resource "agentidentity/$sourceIdentity" -JsonPath "{.status.phase}" -Expected "Ready"

    Apply-KubernetesObject @{
        apiVersion = "kognition.dev/v1alpha1"
        kind = "Agent"
        metadata = @{ name = $targetAgent; namespace = $targetNamespace }
        spec = @{ modelRef = @{ name = "placeholder" }; instructions = @{ inline = "Gate 3 delegation target fixture." }; replicas = 0 }
    }

    Apply-KubernetesObject @{
        apiVersion = "kognition.dev/v1alpha1"
        kind = "AgentService"
        metadata = @{ name = $targetService; namespace = $targetNamespace }
        spec = @{ agentRef = @{ name = $targetAgent }; port = 8080 }
    }
    # Provision AuthZ before freezing the controller so the only remaining
    # fail-closed stage between exchange and dispatch is Gate 3.
    Apply-KubernetesObject @{
        apiVersion = "kognition.dev/v1alpha1"
        kind = "CrossNamespaceGrant"
        metadata = @{ name = $grantName; namespace = $targetNamespace }
        spec = @{ from = $targetNamespace; to = $sourceNamespace; resources = @(@{ kind = "Agent"; name = $targetAgent }) }
    }
    Apply-KubernetesObject @{
        apiVersion = "kognition.dev/v1alpha1"
        kind = "AgentCommunicationPolicy"
        metadata = @{ name = $policyName; namespace = $targetNamespace }
        spec = @{ defaultDeny = $true; allowFrom = @(@{ name = $sourceAgent }); allowTo = @(@{ name = $targetAgent }) }
    }
    # Give the controller a beat to observe the AgentService and stamp its
    # UID/resourceVersion; the reconciler will leave it in Pending because
    # the Agent has replicas=0 and no runtime pod backs the target. We
    # synthesize Ready+readyInstances after freezing so Gate 3 runs.
    Apply-KubernetesObject @{
        apiVersion = "kognition.dev/v1alpha1"
        kind = "AgentInstance"
        metadata = @{ name = $sourceInstance; namespace = $sourceNamespace }
        spec = @{ agentRef = @{ name = $sourceAgent }; revision = 1 }
    }

    # Freeze the reconcilers: with the controller running our synthetic
    # AgentService Ready/ReadyInstances and AgentInstance Running phases
    # would be flipped back to Pending/Failed within ~1s because no runtime
    # pods back these Agents. Scaling the controller to 0 also takes the
    # kognition-webhook Service offline, so we first flip every webhook to
    # failurePolicy=Ignore. That lets the agent-communication-gateway
    # create the Gate 3 RuntimeApproval CR while the controller is down.
    # Both are reverted in the finally block.
    $originalFailurePolicies = Set-WebhookFailurePolicyIgnore
    $controllerReplicas = [int]((& kubectl @kubectlContextArguments get "deployment/$controllerDeployment" -n $ControllerNamespace -o jsonpath='{.spec.replicas}' | Out-String).Trim())
    if ($controllerReplicas -lt 1) { $controllerReplicas = 1 }
    Invoke-Kubectl @("scale", "-n", $ControllerNamespace, "deployment/$controllerDeployment", "--replicas=0") | Out-Null
    Wait-ForDeploymentScaleDown -Deployment $controllerDeployment

    Patch-SubresourceStatus -Resource "agentinstance/$sourceInstance" -Namespace $sourceNamespace -Status @{ phase = "Running"; sessionID = $sessionID }
    Patch-SubresourceStatus -Resource "agentservice/$targetService" -Namespace $targetNamespace -Status @{ phase = "Ready"; readyInstances = 1; activeRevision = 1 }

    $instanceUID = (& kubectl @kubectlContextArguments get "agentinstance/$sourceInstance" -n $sourceNamespace -o jsonpath='{.metadata.uid}' | Out-String).Trim()
    if (-not $instanceUID) { throw "Source AgentInstance did not report a UID" }

    $gatewayPort = Get-AvailableTcpPort
    $gatewayPortForward = Start-ServicePortForward -Namespace $ControllerNamespace -Service $gatewayService -LocalPort $gatewayPort -ServicePort 8094
    $gatewayURL = "http://127.0.0.1:$gatewayPort"
    Wait-ForHttpStatus -Uri "$gatewayURL/healthz" -ExpectedStatus 200

    $keyBase64 = Get-SecretKeyString -Secret "$gatewayService" -Key "capability-hmac-key"
    $keyIDBase64 = Get-SecretKeyString -Secret "$gatewayService" -Key "capability-hmac-key-id"
    if (-not $keyBase64 -or -not $keyIDBase64) { throw "Agent Communication Gateway capability Secret is incomplete" }
    $keyID = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($keyIDBase64))
    $sessionCapability = Invoke-CapabilityMint -KeyBase64 $keyBase64 -FilePath "go" -ArgumentList @("run", "./cmd/kognition-e2e-mint-agent-delegation", "-key-id", $keyID, "-namespace", $sourceNamespace, "-agent", $sourceAgent, "-identity", $sourceIdentity, "-instance", $sourceInstance, "-instance-uid", $instanceUID, "-session", $sessionID)
    if (-not $sessionCapability) { throw "Delegation session capability helper returned an empty token" }

    $invocationID = "gate3-comm-invoke-$runID"

    # Exchange the session capability for an exact delegation capability.
    # Gate 3 does NOT fire on exchange - only on invoke - so a 200 with a
    # bound capability proves AuthZ (source identity + target AgentService
    # + policy + cross-namespace grant) is fully authorized and Gate 3 is
    # the only remaining fail-closed stage. Retry briefly to absorb the
    # gateway informer cache catching up to the just-patched Running phase
    # and Ready AgentService status.
    $exchangeBody = @{ target_namespace = $targetNamespace; target_agent_service = $targetService; invocation_id = $invocationID } | ConvertTo-Json -Compress
    $exchangeDeadline = [DateTime]::UtcNow.AddSeconds(30)
    $exchangeResponse = $null
    do {
        $exchangeResponse = Invoke-HttpRequest -Uri "$gatewayURL/v1/agent-services/capabilities" -Method Post -Headers @{ Authorization = "Bearer $sessionCapability" } -Body $exchangeBody
        if ($exchangeResponse.StatusCode -eq 200) { break }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $exchangeDeadline)
    if ($exchangeResponse.StatusCode -ne 200) {
        throw "Delegation capability exchange returned $($exchangeResponse.StatusCode), want 200. Body: $($exchangeResponse.Content)"
    }
    $exactCapability = ($exchangeResponse.Content | ConvertFrom-Json).capability
    if (-not $exactCapability) { throw "Delegation exchange returned no exact capability" }

    # ---- Scenario: invoke under supervised falls to Gate 3 Pending. ----
    $invokeBody = @{ target_namespace = $targetNamespace; target_agent_service = $targetService; invocation_id = $invocationID; message = "gate 3 must block this" } | ConvertTo-Json -Compress
    $invokeHeaders = @{ Authorization = "Bearer $exactCapability" }
    $invokeResponse = Invoke-HttpRequest -Uri "$gatewayURL/v1/agent-services/invoke" -Method Post -Headers $invokeHeaders -Body $invokeBody
    if ($invokeResponse.StatusCode -ne 202) {
        throw "POST /v1/agent-services/invoke returned $($invokeResponse.StatusCode), want 202 Accepted (Gate 3 Pending). Body: $($invokeResponse.Content)"
    }
    $invokePayload = $invokeResponse.Content | ConvertFrom-Json
    if ($invokePayload.invocation_id -ne $invocationID) {
        throw "Gate 3 Pending body invocation_id = '$($invokePayload.invocation_id)', want '$invocationID'"
    }
    if ($invokePayload.runtime_approval_state -ne "Pending") {
        throw "Gate 3 body runtime_approval_state = '$($invokePayload.runtime_approval_state)', want 'Pending'"
    }
    if (-not $invokePayload.runtime_approval -or $invokePayload.runtime_approval.kind -ne "RuntimeApproval") {
        throw "Gate 3 body did not include the durable RuntimeApproval reference: $($invokeResponse.Content)"
    }
    if ($invokePayload.PSObject.Properties['session_id']) {
        throw "Gate 3 must not populate the success delegation body (session_id): $($invokeResponse.Content)"
    }

    $approval = Wait-ForPendingCommunicationApproval -SourceAgent $sourceAgent -TargetServiceName $targetService -TargetNamespaceName $targetNamespace -ExpectedClass "supervised" -InvocationID $invocationID

    if ($approval.spec.workspace -ne $sourceNamespace) {
        throw "RuntimeApproval $($approval.metadata.name) workspace = '$($approval.spec.workspace)', want '$sourceNamespace'"
    }
    if ($approval.spec.sessionRef -ne $sessionID) {
        throw "RuntimeApproval $($approval.metadata.name) sessionRef = '$($approval.spec.sessionRef)', want '$sessionID'"
    }
    if ($approval.spec.agentInstanceRef.name -ne $sourceInstance) {
        throw "RuntimeApproval $($approval.metadata.name) agentInstanceRef.name = '$($approval.spec.agentInstanceRef.name)', want '$sourceInstance'"
    }
    if ($approval.spec.agentIdentityRef.name -ne $sourceIdentity) {
        throw "RuntimeApproval $($approval.metadata.name) agentIdentityRef.name = '$($approval.spec.agentIdentityRef.name)', want '$sourceIdentity'"
    }
    if ($approval.status.executionState -ne "NotStarted") {
        throw "RuntimeApproval $($approval.metadata.name) executionState = '$($approval.status.executionState)', want 'NotStarted' (Gate 3 must block dispatch)"
    }

    # Retry with the same invocation_id must NOT create a second approval CR.
    # invocation_id is the Gate 3 dedup key, so the resolver must return the
    # existing Pending decision instead of issuing a duplicate.
    Start-Sleep -Milliseconds 250
    $retryResponse = Invoke-HttpRequest -Uri "$gatewayURL/v1/agent-services/invoke" -Method Post -Headers $invokeHeaders -Body $invokeBody
    if ($retryResponse.StatusCode -ne 202) {
        throw "Retried POST /v1/agent-services/invoke returned $($retryResponse.StatusCode), want 202. Body: $($retryResponse.Content)"
    }
    $retryPayload = $retryResponse.Content | ConvertFrom-Json
    if ($retryPayload.runtime_approval_state -ne "Pending" -or $retryPayload.invocation_id -ne $invocationID) {
        throw "Retried Gate 3 invocation did not return the persisted Pending decision: $($retryResponse.Content)"
    }
    if ($retryPayload.runtime_approval.name -ne $approval.metadata.name) {
        throw "Retried Gate 3 invocation returned RuntimeApproval '$($retryPayload.runtime_approval.name)', want '$($approval.metadata.name)' (dedup by invocation_id)"
    }
    $approvals = Get-CommunicationApprovalsForInvocation -InvocationID $invocationID
    if ($approvals.Count -ne 1) {
        throw "Retried Gate 3 invoke created $($approvals.Count) RuntimeApproval CRs for InvocationID=$invocationID, want exactly 1 (dedup key)"
    }

    Write-Host "Gate 3 persisted Pending RuntimeApproval $($approval.metadata.name) for Agent=$sourceAgent Target=$targetNamespace/$targetService Class=supervised and blocked upstream dispatch." -ForegroundColor Green

    Write-Host "=== PASS ===" -ForegroundColor Green
}
finally {
    if ($gatewayPortForward) {
        Stop-Job $gatewayPortForward -ErrorAction SilentlyContinue
        Remove-Job $gatewayPortForward -Force -ErrorAction SilentlyContinue
    }
    if ($controllerReplicas) {
        try {
            & kubectl @kubectlContextArguments scale -n $ControllerNamespace "deployment/$controllerDeployment" "--replicas=$controllerReplicas" 2>$null | Out-Null
            Wait-ForDeploymentReady -Deployment $controllerDeployment -Replicas $controllerReplicas
        }
        catch {
            Write-Warning "Failed to restore $controllerDeployment to $controllerReplicas replicas: $_"
        }
    }
    if ($originalFailurePolicies) {
        try {
            Restore-WebhookFailurePolicy -Originals $originalFailurePolicies
        }
        catch {
            Write-Warning "Failed to restore webhook failurePolicies: $_"
        }
    }
    if ($sourceNamespace) {
        & kubectl @kubectlContextArguments delete namespace $sourceNamespace --ignore-not-found --wait=false 2>$null | Out-Null
    }
    if ($targetNamespace) {
        & kubectl @kubectlContextArguments delete namespace $targetNamespace --ignore-not-found --wait=false 2>$null | Out-Null
    }
}
