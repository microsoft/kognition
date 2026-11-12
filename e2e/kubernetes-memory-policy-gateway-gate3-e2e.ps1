# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

param(
    [string]$KubernetesContext = "",
    [string]$ControllerNamespace = "kognition-system",
    [string]$ReleaseName = "kognition"
)

# ADR-0014 Runtime Autonomy Gate 3 — Memory plane Kubernetes E2E.
#
# Scenario: An Agent with a valid runtime memory capability and an authorizing
# MemoryGrant issues a write (`store`) against the Memory Policy Gateway. No
# TrustProfile exists, so the resolver falls back to the built-in "supervised"
# AutonomyClass (ApprovalPolicy.MemoryWrite="always", RequiresApproval=true).
# Every write MUST persist a RuntimeApproval CR with plane=Memory,
# operation=store, status.decisionState=Pending, and MUST NOT reach the raw
# memory service (the record must not appear when queried through the
# authenticated raw endpoint).
#
# Retry invariance: repeating the request with the same
# X-Kognition-Memory-Invocation-Id header must not create a second
# RuntimeApproval CR; that header is the Gate 3 dedup key.
#
# The `delete` write is also covered so the two ReadWrite entry points that
# the gateway exposes (POST /v1/memory/store, DELETE /v1/memory/delete/<h>/<id>)
# are both proven to persist a Pending RuntimeApproval before dispatch.
#
# Persistence-only: this test verifies Gate 3 stops the invocation before
# target execution and durably records the pending decision. Progressing a
# RuntimeApproval to Approved is out of scope — the validating admission
# webhook rejects status writes from non-controller usernames.

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

. (Join-Path $PSScriptRoot "capability-mint.ps1")

$gatewayService = "$ReleaseName-memory-policy-gateway"
$rawService = "$ReleaseName-memory-service"
$controllerDeployment = "$ReleaseName-k8s-controller"
$webhookConfigName = "$ReleaseName-validating"
$gatewayPortForward = $null
$rawPortForward = $null
$controllerReplicas = $null
$originalFailurePolicies = $null
$runID = [string][DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
$namespace = "memory-gate3-$runID"
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

function Set-RunningInstanceStatus {
    $patchFile = [IO.Path]::GetTempFileName()
    try {
        @{ status = @{ phase = "Running"; sessionID = $sessionID } } | ConvertTo-Json -Compress | Set-Content -Path $patchFile -NoNewline
        Invoke-Kubectl @("patch", "agentinstance/$instanceName", "-n", $namespace, "--subresource=status", "--type=merge", "--patch-file", $patchFile) | Out-Null
    }
    finally {
        Remove-Item -Path $patchFile -Force -ErrorAction SilentlyContinue
    }
}

function Wait-ForJsonPath {
    param(
        [Parameter(Mandatory = $true)][string]$Resource,
        [Parameter(Mandatory = $true)][string]$JsonPath,
        [Parameter(Mandatory = $true)][string]$Expected,
        [int]$TimeoutSeconds = 90
    )

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        $value = (& kubectl @kubectlContextArguments get $Resource -n $namespace "-o=jsonpath=$JsonPath" 2>$null | Out-String).Trim()
        if ($value -eq $Expected) { return }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "$Resource did not reach $JsonPath=$Expected within $TimeoutSeconds seconds"
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

function Wait-ForMemoryGrantFinalizer {
    param([Parameter(Mandatory = $true)][string]$Name, [int]$TimeoutSeconds = 45)

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        $grant = (& kubectl @kubectlContextArguments get "memorygrant/$Name" -n $namespace -o json 2>$null | ConvertFrom-Json)
        if ($grant.metadata.finalizers -contains "kognition.dev/memory-grant-cleanup") { return }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "MemoryGrant/$Name did not receive its cleanup finalizer within $TimeoutSeconds seconds"
}

function Set-WebhookFailurePolicyIgnore {
    # The audit.kognition.dev validating webhook is served by the k8s-controller
    # Pod (via the kognition-webhook Service). Scaling the controller to 0 makes
    # every CREATE/UPDATE/DELETE on kognition.dev resources fail with 503, which
    # would prevent the memory-policy-gateway from creating the Gate 3
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

function Get-SecretKeyBase64 {
    param([Parameter(Mandatory = $true)][string]$Key)

    return (& kubectl @kubectlContextArguments get secret "$ReleaseName-memory-policy-gateway" -n $ControllerNamespace "-o=jsonpath={.data.$Key}" | Out-String).Trim()
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

function Wait-ForPendingMemoryApproval {
    param(
        [Parameter(Mandatory = $true)][string]$AgentName,
        [Parameter(Mandatory = $true)][string]$ClaimName,
        [Parameter(Mandatory = $true)][string]$Operation,
        [Parameter(Mandatory = $true)][string]$ExpectedClass,
        [Parameter(Mandatory = $true)][string]$InvocationID,
        [int]$TimeoutSeconds = 60
    )

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        $list = ((Invoke-Kubectl @("get", "runtimeapprovals", "-n", $namespace, "-o", "json")) -join "`n" | ConvertFrom-Json).items
        foreach ($ra in $list) {
            if ($ra.spec.plane -ne "Memory") { continue }
            if ($ra.spec.operation -ne $Operation) { continue }
            if ($ra.spec.agentRef.name -ne $AgentName) { continue }
            if ($ra.spec.targetRef.kind -ne "MemoryClaim") { continue }
            if ($ra.spec.targetRef.name -ne $ClaimName) { continue }
            if ($ra.spec.invocationID -ne $InvocationID) { continue }
            if ($ra.status.decisionState -ne "Pending") { continue }
            if ($ra.spec.autonomyClassRef.name -ne $ExpectedClass) {
                throw "RuntimeApproval $($ra.metadata.name) bound to AutonomyClass '$($ra.spec.autonomyClassRef.name)', want '$ExpectedClass'"
            }
            return $ra
        }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Pending Memory RuntimeApproval for Agent=$AgentName Claim=$ClaimName Operation=$Operation InvocationID=$InvocationID did not appear within $TimeoutSeconds seconds"
}

function Get-MemoryApprovalsForInvocation {
    param([Parameter(Mandatory = $true)][string]$InvocationID)

    $list = ((Invoke-Kubectl @("get", "runtimeapprovals", "-n", $namespace, "-o", "json")) -join "`n" | ConvertFrom-Json).items
    return @($list | Where-Object { $_.spec.plane -eq "Memory" -and $_.spec.invocationID -eq $InvocationID })
}

Write-Host "=== Kognition Memory Policy Gateway Gate 3 E2E ===" -ForegroundColor Cyan

try {
    Invoke-Kubectl @("get", "deployment/$gatewayService", "-n", $ControllerNamespace) | Out-Null
    Invoke-Kubectl @("get", "deployment/$rawService", "-n", $ControllerNamespace) | Out-Null
    Invoke-Kubectl @("get", "deployment/$controllerDeployment", "-n", $ControllerNamespace) | Out-Null
    Invoke-Kubectl @("wait", "-n", $ControllerNamespace, "--for=condition=Available", "deployment/$gatewayService", "--timeout=120s") | Out-Null
    Invoke-Kubectl @("wait", "-n", $ControllerNamespace, "--for=condition=Available", "deployment/$rawService", "--timeout=120s") | Out-Null
    Invoke-Kubectl @("create", "namespace", $namespace) | Out-Null

    $agentName = "gate3-memory-agent"
    $instanceName = "gate3-memory-instance"
    $claimName = "gate3-memory-claim"
    $grantName = "gate3-memory-grant"
    $identityName = "agent-identity-$agentName"
    $sessionID = "gate3-memory-session-$runID"

    # No TrustProfile: the resolver defaults to the built-in "supervised"
    # AutonomyClass (ApprovalPolicy.MemoryWrite="always", RequiresApproval=true),
    # so every write must gate to Pending.
    Apply-KubernetesObject @{
        apiVersion = "kognition.dev/v1alpha1"
        kind = "Agent"
        metadata = @{ name = $agentName; namespace = $namespace }
        spec = @{ modelRef = @{ name = "placeholder" }; instructions = @{ inline = "Gate 3 memory-plane fixture." }; replicas = 0 }
    }
    Wait-ForJsonPath -Resource "agentidentity/$identityName" -JsonPath "{.status.phase}" -Expected "Ready"

    Apply-KubernetesObject @{
        apiVersion = "kognition.dev/v1alpha1"
        kind = "MemoryClaim"
        metadata = @{ name = $claimName; namespace = $namespace }
        spec = @{ memoryClassName = "default"; type = "working"; capacity = "1Mi"; accessModes = @("ReadWrite") }
    }
    Wait-ForJsonPath -Resource "memoryclaim/$claimName" -JsonPath "{.status.phase}" -Expected "Bound"
    $volumeName = (& kubectl @kubectlContextArguments get "memoryclaim/$claimName" -n $namespace -o jsonpath='{.status.boundVolumeName}' | Out-String).Trim()
    if (-not $volumeName) { throw "MemoryClaim did not report a bound volume" }
    $volumeHandle = (& kubectl @kubectlContextArguments get "memoryvolume/$volumeName" -n $namespace -o jsonpath='{.spec.backendHandle}' | Out-String).Trim()
    if (-not $volumeHandle) { throw "MemoryVolume did not report a backend handle" }

    Apply-KubernetesObject @{
        apiVersion = "kognition.dev/v1alpha1"
        kind = "AgentRole"
        metadata = @{ name = "gate3-memory-writer"; namespace = $namespace }
        rules = @(@{ resources = @("Memory"); verbs = @("get", "update") })
    }
    Apply-KubernetesObject @{
        apiVersion = "kognition.dev/v1alpha1"
        kind = "AgentRoleBinding"
        metadata = @{ name = "gate3-memory-writer"; namespace = $namespace }
        spec = @{ roleRef = @{ name = "gate3-memory-writer" }; subjects = @(@{ kind = "AgentIdentity"; name = $identityName }) }
    }
    Apply-KubernetesObject @{
        apiVersion = "kognition.dev/v1alpha1"
        kind = "AgentInstance"
        metadata = @{ name = $instanceName; namespace = $namespace }
        spec = @{ agentRef = @{ name = $agentName }; revision = 1 }
    }

    # Provision the authorizing grant before we freeze the controller so the
    # Memory Policy Gateway AuthZ decision resolves to authorized and the only
    # remaining fail-closed stage is Gate 3.
    Apply-KubernetesObject @{
        apiVersion = "kognition.dev/v1alpha1"
        kind = "MemoryGrant"
        metadata = @{ name = $grantName; namespace = $namespace }
        spec = @{ agentRef = @{ name = $agentName }; memoryClaimRef = @{ name = $claimName }; accessMode = "ReadWrite" }
    }
    Wait-ForMemoryGrantFinalizer -Name $grantName

    # Freeze the AgentInstance reconciler by scaling the k8s-controller to 0.
    # With the controller running the reconciler would flip our synthetic
    # Running Phase back to Failed (SessionNotAlive) within ~1s because there
    # is no real runtime Pod backing this Agent. Scaling it down also takes
    # the kognition-webhook Service offline, so we first flip every webhook
    # in the kognition-validating configuration to failurePolicy=Ignore. That
    # lets the memory-policy-gateway create the Gate 3 RuntimeApproval CR
    # while the controller is down. Both are reverted in the finally block.
    $originalFailurePolicies = Set-WebhookFailurePolicyIgnore
    $controllerReplicas = [int]((& kubectl @kubectlContextArguments get "deployment/$controllerDeployment" -n $ControllerNamespace -o jsonpath='{.spec.replicas}' | Out-String).Trim())
    if ($controllerReplicas -lt 1) { $controllerReplicas = 1 }
    Invoke-Kubectl @("scale", "-n", $ControllerNamespace, "deployment/$controllerDeployment", "--replicas=0") | Out-Null
    Wait-ForDeploymentScaleDown -Deployment $controllerDeployment
    Set-RunningInstanceStatus
    $instanceUID = (& kubectl @kubectlContextArguments get "agentinstance/$instanceName" -n $namespace -o jsonpath='{.metadata.uid}' | Out-String).Trim()
    if (-not $instanceUID) { throw "AgentInstance did not report a UID" }

    $gatewayPort = Get-AvailableTcpPort
    $rawPort = Get-AvailableTcpPort
    $gatewayPortForward = Start-ServicePortForward -Namespace $ControllerNamespace -Service $gatewayService -LocalPort $gatewayPort -ServicePort 8091
    $rawPortForward = Start-ServicePortForward -Namespace $ControllerNamespace -Service $rawService -LocalPort $rawPort -ServicePort 8090

    $gatewayURL = "http://127.0.0.1:$gatewayPort"
    $rawURL = "http://127.0.0.1:$rawPort"
    Wait-ForHttpStatus -Uri "$gatewayURL/v1/memory/health" -ExpectedStatus 200
    Wait-ForHttpStatus -Uri "$rawURL/v1/memory/health" -ExpectedStatus 200

    $capabilityKey = Get-SecretKeyBase64 -Key "capability-hmac-key"
    $upstreamTokenBase64 = Get-SecretKeyBase64 -Key "upstream-token"
    if (-not $capabilityKey -or -not $upstreamTokenBase64) { throw "Memory gateway Secret is missing required keys" }
    $upstreamToken = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($upstreamTokenBase64))
    $keyIDBase64 = (& kubectl @kubectlContextArguments get secret "$ReleaseName-memory-policy-gateway" -n $ControllerNamespace '-o=jsonpath={.data.capability-hmac-key-id}' | Out-String).Trim()
    if (-not $keyIDBase64) { throw "Memory policy gateway Secret is missing capability-hmac-key-id" }
    $keyID = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($keyIDBase64))
    $capability = Invoke-CapabilityMint -KeyBase64 $capabilityKey -FilePath "go" -ArgumentList @("run", "./e2e/mint-memory-capability.go", "-key-id", $keyID, "-namespace", $namespace, "-agent", $agentName, "-identity", $identityName, "-instance", $instanceName, "-instance-uid", $instanceUID, "-session", $sessionID, "-volume", $volumeHandle)
    if (-not $capability) { throw "Capability helper returned an empty token" }

    # Provision the backend volume through the raw memory service so that a
    # (counterfactual) authorized store would succeed. Gate 3 must still stop
    # the request before it reaches the raw service.
    $rawHeaders = @{ Authorization = "Bearer $upstreamToken" }
    $provisionResponse = Invoke-HttpRequest -Uri "$rawURL/v1/memory/provision" -Method Post -Headers $rawHeaders -Body ("{`"volume_handle`":`"$volumeHandle`"}")
    if ($provisionResponse.StatusCode -ne 201) { throw "Provisioning the raw memory volume failed: $($provisionResponse.StatusCode) $($provisionResponse.Content)" }

    # ---- Scenario: `store` under supervised falls to Gate 3 Pending. ----
    $storeInvocationID = "gate3-store-$runID"
    $storeRecordID = "gate3-store-record-$runID"
    $storeBody = "{`"volume_handle`":`"$volumeHandle`",`"id`":`"$storeRecordID`",`"content`":`"must not persist through Gate 3`"}"
    $storeHeaders = @{ Authorization = "Bearer $capability"; "X-Kognition-Memory-Invocation-Id" = $storeInvocationID }

    $storeResponse = Invoke-HttpRequest -Uri "$gatewayURL/v1/memory/store" -Method Post -Headers $storeHeaders -Body $storeBody
    if ($storeResponse.StatusCode -ne 202) {
        throw "POST /v1/memory/store returned $($storeResponse.StatusCode), want 202 Accepted (Gate 3 Pending). Body: $($storeResponse.Content)"
    }
    if ($storeResponse.Content -notmatch 'memory access denied by runtime approval') {
        throw "POST /v1/memory/store body did not carry the Gate 3 denial reason: $($storeResponse.Content)"
    }
    $storeApproval = Wait-ForPendingMemoryApproval -AgentName $agentName -ClaimName $claimName -Operation "store" -ExpectedClass "supervised" -InvocationID $storeInvocationID

    if ($storeApproval.spec.workspace -ne $namespace) {
        throw "RuntimeApproval $($storeApproval.metadata.name) workspace = '$($storeApproval.spec.workspace)', want '$namespace'"
    }
    if ($storeApproval.spec.sessionRef -ne $sessionID) {
        throw "RuntimeApproval $($storeApproval.metadata.name) sessionRef = '$($storeApproval.spec.sessionRef)', want '$sessionID'"
    }
    if ($storeApproval.status.executionState -ne "NotStarted") {
        throw "RuntimeApproval $($storeApproval.metadata.name) executionState = '$($storeApproval.status.executionState)', want 'NotStarted' (Gate 3 must block execution before target dispatch)"
    }

    # Retry with the same InvocationID must NOT create a second approval CR.
    Start-Sleep -Milliseconds 250
    $retryResponse = Invoke-HttpRequest -Uri "$gatewayURL/v1/memory/store" -Method Post -Headers $storeHeaders -Body $storeBody
    if ($retryResponse.StatusCode -ne 202) {
        throw "Retried POST /v1/memory/store returned $($retryResponse.StatusCode), want 202. Body: $($retryResponse.Content)"
    }
    $storeApprovals = Get-MemoryApprovalsForInvocation -InvocationID $storeInvocationID
    if ($storeApprovals.Count -ne 1) {
        throw "Retried Gate 3 write created $($storeApprovals.Count) RuntimeApproval CRs for InvocationID=$storeInvocationID, want exactly 1 (dedup key)"
    }

    # Gate 3 must stop the write before dispatch, so the record must NOT be
    # visible through the authenticated raw memory service.
    $rawGet = Invoke-HttpRequest -Uri "$rawURL/v1/memory/get/$volumeHandle/$storeRecordID" -Method Get -Headers $rawHeaders
    if ($rawGet.StatusCode -ne 404) {
        throw "Raw memory service should not carry the Gate-3-blocked record; GET returned $($rawGet.StatusCode) Body: $($rawGet.Content)"
    }

    Write-Host "Gate 3 persisted Pending RuntimeApproval $($storeApproval.metadata.name) for Agent=$agentName Operation=store Class=supervised and blocked upstream dispatch." -ForegroundColor Green

    # ---- Scenario: `delete` under supervised also falls to Gate 3 Pending. ----
    # Seed one record through the raw service so the delete has a plausible
    # target; Gate 3 must still stop the delete before it reaches the backend.
    $preseededRecordID = "gate3-preseeded-record-$runID"
    $seedBody = "{`"volume_handle`":`"$volumeHandle`",`"id`":`"$preseededRecordID`",`"content`":`"seeded via raw service`"}"
    $seedResponse = Invoke-HttpRequest -Uri "$rawURL/v1/memory/store" -Method Post -Headers $rawHeaders -Body $seedBody
    if ($seedResponse.StatusCode -ne 201) { throw "Seeding raw memory record failed: $($seedResponse.StatusCode) $($seedResponse.Content)" }

    $deleteInvocationID = "gate3-delete-$runID"
    $deleteHeaders = @{ Authorization = "Bearer $capability"; "X-Kognition-Memory-Invocation-Id" = $deleteInvocationID }
    $deleteResponse = Invoke-HttpRequest -Uri "$gatewayURL/v1/memory/delete/$volumeHandle/$preseededRecordID" -Method Delete -Headers $deleteHeaders
    if ($deleteResponse.StatusCode -ne 202) {
        throw "DELETE /v1/memory/delete returned $($deleteResponse.StatusCode), want 202 Accepted (Gate 3 Pending). Body: $($deleteResponse.Content)"
    }
    $deleteApproval = Wait-ForPendingMemoryApproval -AgentName $agentName -ClaimName $claimName -Operation "delete" -ExpectedClass "supervised" -InvocationID $deleteInvocationID
    if ($deleteApproval.status.executionState -ne "NotStarted") {
        throw "RuntimeApproval $($deleteApproval.metadata.name) executionState = '$($deleteApproval.status.executionState)', want 'NotStarted'"
    }

    # The pre-seeded record MUST still exist: Gate 3 blocked the delete.
    $rawGetAfterDelete = Invoke-HttpRequest -Uri "$rawURL/v1/memory/get/$volumeHandle/$preseededRecordID" -Method Get -Headers $rawHeaders
    if ($rawGetAfterDelete.StatusCode -ne 200) {
        throw "Pre-seeded record was removed despite Gate 3 blocking the delete: GET returned $($rawGetAfterDelete.StatusCode) Body: $($rawGetAfterDelete.Content)"
    }

    Write-Host "Gate 3 persisted Pending RuntimeApproval $($deleteApproval.metadata.name) for Agent=$agentName Operation=delete Class=supervised and blocked upstream dispatch." -ForegroundColor Green

    Write-Host "=== PASS ===" -ForegroundColor Green
}
finally {
    if ($gatewayPortForward) {
        Stop-Job $gatewayPortForward -ErrorAction SilentlyContinue
        Remove-Job $gatewayPortForward -Force -ErrorAction SilentlyContinue
    }
    if ($rawPortForward) {
        Stop-Job $rawPortForward -ErrorAction SilentlyContinue
        Remove-Job $rawPortForward -Force -ErrorAction SilentlyContinue
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
    if ($namespace) {
        & kubectl @kubectlContextArguments delete namespace $namespace --ignore-not-found --wait=false 2>$null | Out-Null
    }
}
