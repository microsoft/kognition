# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

param(
    [string]$KubernetesContext = "",
    [string]$ControllerNamespace = "kognition-system",
    [string]$ReleaseName = "kognition",
    [string]$DemoRunId = "",
    [string]$ScopeNamespace = "",
    [string]$FixtureModelName = "placeholder",
    [string]$ReportPath = "",
    [string]$AttemptId = "",
    [switch]$KeepScopeNamespace
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

. (Join-Path $PSScriptRoot "capability-mint.ps1")
. (Join-Path $PSScriptRoot "gateway-client-tracing.ps1")

$gatewayService = "$ReleaseName-memory-policy-gateway"
$rawService = "$ReleaseName-memory-service"
$controllerDeployment = "$ReleaseName-k8s-controller"
$gatewayPortForward = $null
$rawPortForward = $null
$runID = if ($DemoRunId) { $DemoRunId } else { [string][DateTimeOffset]::UtcNow.ToUnixTimeSeconds() }
$namespace = if ($ScopeNamespace) { $ScopeNamespace } else { "memory-boundary-$runID" }
$ownsNamespace = -not $ScopeNamespace
$kubectlContextArguments = if ($KubernetesContext) { @("--context", $KubernetesContext) } else { @() }

if ($AttemptId -and $AttemptId -notmatch '^[a-z0-9]([-a-z0-9]{0,18}[a-z0-9])?$') {
    throw "AttemptId must be 1-20 lowercase DNS-label characters"
}

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

function Get-AuditEntries {
    $configMaps = (& kubectl @kubectlContextArguments get configmaps -n $ControllerNamespace -l 'kognition.dev/cognitive-audit=true' -o json | ConvertFrom-Json).items
    $entries = @()
    foreach ($configMap in $configMaps) {
        $encoded = $configMap.binaryData.'entries.json'
        if (-not $encoded) { continue }
        $entries += @([Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($encoded)) | ConvertFrom-Json)
    }
    return @($entries)
}

function Wait-ForAuditEntry {
    param([Parameter(Mandatory = $true)][scriptblock]$Predicate, [int]$TimeoutSeconds = 45, [string]$Description = "expected durable memory audit entry")

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        $entry = Get-AuditEntries | Where-Object $Predicate | Select-Object -First 1
        if ($entry) { return $entry }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Timed out waiting for $Description"
}

function Apply-KubernetesObject {
    param([Parameter(Mandatory = $true)][hashtable]$Object)

    $Object | ConvertTo-Json -Depth 20 -Compress | & kubectl @kubectlContextArguments apply -f - 2>&1 | Out-Host
    if ($LASTEXITCODE -ne 0) {
        throw "apply $($Object.kind)/$($Object.metadata.name) failed"
    }
}

function Assert-MemorySession {
    $instance = Invoke-Kubectl @("get", "agentinstance/$instanceName", "-n", $namespace, "-o", "json") | ConvertFrom-Json
    if ($instance.status.phase -ne "Running" -or $instance.status.sessionID -ne $sessionID) { throw "Controller-owned Memory session is no longer active" }
    $profile = Invoke-Kubectl @("get", "trustprofile/$trustProfileName", "-n", $namespace, "-o", "json") | ConvertFrom-Json
    if ($profile.status.currentClass -ne "autonomous") { throw "Memory fixture is no longer autonomous" }
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
        if ($value -eq $Expected) {
            return
        }
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
        if ((-not $replicas -or $replicas -eq "0") -and $controllerPods.Count -eq 0) {
            return
        }
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

function Wait-ForMemoryGrantFinalizer {
    param([Parameter(Mandatory = $true)][string]$Name, [int]$TimeoutSeconds = 45)

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        $grant = (& kubectl @kubectlContextArguments get "memorygrant/$Name" -n $namespace -o json 2>$null | ConvertFrom-Json)
        $finalizersProperty = $grant.metadata.PSObject.Properties['finalizers']
        if ($finalizersProperty -and $finalizersProperty.Value -contains "kognition.dev/memory-grant-cleanup") {
            return
        }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "MemoryGrant/$Name did not receive its cleanup finalizer within $TimeoutSeconds seconds"
}

function Get-SecretKeyBase64 {
    param([Parameter(Mandatory = $true)][string]$Key)

    return (& kubectl @kubectlContextArguments get secret "$ReleaseName-memory-policy-gateway" -n $ControllerNamespace "-o=jsonpath={.data.$Key}" | Out-String).Trim()
}

function New-MemoryCapability {
    $token = Invoke-CapabilityMint -KeyBase64 $capabilityKey -FilePath "go" -ArgumentList @("run", "./e2e/mint-memory-capability.go", "-key-id", $keyID, "-namespace", $namespace, "-agent", $agentName, "-identity", $identityName, "-instance", $instanceName, "-instance-uid", $instanceUID, "-session", $sessionID, "-volume", $volumeHandle)
    if (-not $token) { throw "Capability helper returned an empty token" }
    return $token
}

function Invoke-HttpRequest {
    param(
        [Parameter(Mandatory = $true)][string]$Uri,
        [Parameter(Mandatory = $true)][string]$Method,
        [hashtable]$Headers = @{},
        [string]$Body = "",
        [int]$TimeoutSeconds = 30
    )

    $requestHeaders = @{} + $Headers
    if ($DemoRunId -and $Uri -like "$gatewayURL/*") {
        $requestHeaders['X-Kognition-Run-ID'] = $DemoRunId
    }
    $request = @{ Uri = $Uri; Method = $Method; Headers = $requestHeaders; TimeoutSec = $TimeoutSeconds; ErrorAction = "Stop" }
    if ($Body) {
        $request.ContentType = "application/json"
        $request.Body = $Body
    }
    if ($PSVersionTable.PSVersion.Major -ge 7) {
        $request.SkipHttpErrorCheck = $true
        return Invoke-TracedGatewayRequest -RequestParameters $request
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
            if ($response.StatusCode -eq $ExpectedStatus) {
                return
            }
        }
        catch {
        }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "$Uri did not return $ExpectedStatus within $TimeoutSeconds seconds"
}

function Invoke-ExpectedStatus {
    param(
        [Parameter(Mandatory = $true)][string]$Uri,
        [Parameter(Mandatory = $true)][int]$ExpectedStatus,
        [string]$Body = '{"volume_handle":"boundary-test","content":"must not persist"}',
        [string]$Authorization = "",
        [string]$IdempotencyKey = "",
        [string]$MemoryInvocationID = ""
    )

    $headers = @{}
    if ($Authorization) {
        $headers.Authorization = $Authorization
    }
    if ($IdempotencyKey) {
        $headers['Idempotency-Key'] = $IdempotencyKey
    }
    if ($MemoryInvocationID) {
        $headers['X-Kognition-Memory-Invocation-Id'] = $MemoryInvocationID
    }
    $response = Invoke-HttpRequest -Uri $Uri -Method Post -Headers $headers -Body $Body
    if ($response.StatusCode -ne $ExpectedStatus) {
        throw ("POST {0} returned {1}, want {2}. Body: {3}" -f $Uri, $response.StatusCode, $ExpectedStatus, $response.Content)
    }
}

function Invoke-ExpectedStatusEventually {
    param(
        [Parameter(Mandatory = $true)][string]$Uri,
        [Parameter(Mandatory = $true)][int]$ExpectedStatus,
        [Parameter(Mandatory = $true)][string]$Authorization,
        [string]$Body = "{}",
        [string]$IdempotencyKey = "",
        [string]$MemoryInvocationID = "",
        [int]$TimeoutSeconds = 30
    )

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        $headers = @{ Authorization = $Authorization }
        if ($IdempotencyKey) {
            $headers['Idempotency-Key'] = $IdempotencyKey
        }
        if ($MemoryInvocationID) {
            $headers['X-Kognition-Memory-Invocation-Id'] = $MemoryInvocationID
        }
        $response = Invoke-HttpRequest -Uri $Uri -Method Post -Headers $headers -Body $Body
        if ($response.StatusCode -eq $ExpectedStatus) {
            return
        }
        if ($response.StatusCode -ne 403) {
            throw ("POST {0} returned {1}, want {2}. Body: {3}" -f $Uri, $response.StatusCode, $ExpectedStatus, $response.Content)
        }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "POST $Uri remained forbidden while waiting for Memory policy convergence"
}

Write-Host "=== Kognition Kubernetes Memory Boundary E2E ===" -ForegroundColor Cyan

try {
    Invoke-Kubectl @("get", "deployment/$gatewayService", "-n", $ControllerNamespace) | Out-Null
    Invoke-Kubectl @("get", "deployment/$rawService", "-n", $ControllerNamespace) | Out-Null
    Invoke-Kubectl @("get", "deployment/$controllerDeployment", "-n", $ControllerNamespace) | Out-Null
    Invoke-Kubectl @("wait", "-n", $ControllerNamespace, "--for=condition=Available", "deployment/$gatewayService", "--timeout=120s") | Out-Null
    Invoke-Kubectl @("wait", "-n", $ControllerNamespace, "--for=condition=Available", "deployment/$rawService", "--timeout=120s") | Out-Null
    if ($ownsNamespace) {
        Invoke-Kubectl @("create", "namespace", $namespace) | Out-Null
    }

    $attemptSuffix = if ($AttemptId) { "-$AttemptId" } else { "" }
    $agentName = "memory-agent$attemptSuffix"
    $instanceName = "memory-instance$attemptSuffix"
    $claimName = "memory-claim$attemptSuffix"
    $grantName = "memory-grant$attemptSuffix"
    $continuingGrantName = "memory-reader$attemptSuffix"
    $trustProfileName = "memory-trust$attemptSuffix"
    $identityName = "agent-identity-$agentName"
    $sessionID = if ($DemoRunId) { "$DemoRunId`:memory-session" } else { "memory-session-$runID" }
    Wait-ForJsonPath -Resource "model/$FixtureModelName" -JsonPath "{.status.phase}" -Expected "Ready"
    Apply-KubernetesObject @{
        apiVersion = "kognition.dev/v1alpha1"
        kind = "Agent"
        metadata = @{ name = $agentName; namespace = $namespace }
        spec = @{ modelRef = @{ name = $FixtureModelName }; instructions = @{ inline = "Memory authorization integration fixture." }; replicas = 1 }
    }
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "ModelGrant"; metadata = @{ name = "$agentName-model"; namespace = $namespace }; spec = @{ agentRef = @{ name = $agentName }; modelRef = @{ name = $FixtureModelName } } }
    Wait-ForJsonPath -Resource "agentidentity/$identityName" -JsonPath "{.status.phase}" -Expected "Ready"
    $agentUID = (& kubectl @kubectlContextArguments get "agent/$agentName" -n $namespace -o jsonpath='{.metadata.uid}' | Out-String).Trim()
    if (-not $agentUID) { throw "Agent did not report a UID" }
    Apply-KubernetesObject @{
        apiVersion = "kognition.dev/v1alpha1"
        kind = "TrustProfile"
        metadata = @{ name = $trustProfileName; namespace = $namespace }
        spec = @{ agentRef = @{ name = $agentName }; agentIncarnationUID = $agentUID; initialClass = "autonomous"; initialScore = 90 }
    }
    Wait-ForJsonPath -Resource "trustprofile/$trustProfileName" -JsonPath "{.status.currentClass}" -Expected "autonomous"
    $spiffeID = (& kubectl @kubectlContextArguments get "agentidentity/$identityName" -n $namespace -o jsonpath='{.spec.spiffeID}' | Out-String).Trim()
    if (-not $spiffeID) { throw "AgentIdentity did not report a SPIFFE ID" }
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
        metadata = @{ name = "memory-writer"; namespace = $namespace }
        rules = @(@{ resources = @("Memory"); verbs = @("get", "update") })
    }
    Apply-KubernetesObject @{
        apiVersion = "kognition.dev/v1alpha1"
        kind = "AgentRoleBinding"
        metadata = @{ name = "memory-writer"; namespace = $namespace }
        spec = @{ roleRef = @{ name = "memory-writer" }; subjects = @(@{ kind = "AgentIdentity"; name = $identityName }) }
    }
    Apply-KubernetesObject @{
        apiVersion = "kognition.dev/v1alpha1"
        kind = "AgentInstance"
        metadata = @{ name = $instanceName; namespace = $namespace }
        spec = @{ agentRef = @{ name = $agentName }; revision = 1 }
    }
    Wait-ForJsonPath -Resource "agentinstance/$instanceName" -JsonPath "{.status.phase}" -Expected "Running" -TimeoutSeconds 180
    $sessionID = (Invoke-Kubectl @("get", "agentinstance/$instanceName", "-n", $namespace, "-o", "jsonpath={.status.sessionID}") | Out-String).Trim()
    if (-not $sessionID) { throw "Controller did not assign a Memory session" }
    Assert-MemorySession
    $instanceUID = (& kubectl @kubectlContextArguments get "agentinstance/$instanceName" -n $namespace -o jsonpath='{.metadata.uid}' | Out-String).Trim()
    if (-not $instanceUID) { throw "AgentInstance did not report a UID" }

    $gatewayPort = Get-AvailableTcpPort
    $rawPort = Get-AvailableTcpPort
    $gatewayPortForward = Start-ServicePortForward -Namespace $ControllerNamespace -Service $gatewayService -LocalPort $gatewayPort -ServicePort 8091
    $rawPortForward = Start-ServicePortForward -Namespace $ControllerNamespace -Service $rawService -LocalPort $rawPort -ServicePort 8090

    $gatewayURL = "http://127.0.0.1:$gatewayPort"
    $rawURL = "http://127.0.0.1:$rawPort"
    $invocationSuffix = if ($AttemptId) { ":$AttemptId" } else { "" }
    $memoryAllowKey = if ($DemoRunId) { "$DemoRunId`:memory-allow$invocationSuffix" } else { "gateway-record$invocationSuffix" }
    $memoryDenyKey = if ($DemoRunId) { "$DemoRunId`:memory-deny$invocationSuffix" } else { "memory-deny-$runID$invocationSuffix" }
    $memoryRevokedKey = if ($DemoRunId) { "$DemoRunId`:memory-revoked$invocationSuffix" } else { "memory-revoked-$runID$invocationSuffix" }
    $memoryContinuingKey = if ($DemoRunId) { "$DemoRunId`:memory-continuing$invocationSuffix" } else { "memory-continuing-$runID$invocationSuffix" }
    Wait-ForHttpStatus -Uri "$gatewayURL/v1/memory/health" -ExpectedStatus 200
    Wait-ForHttpStatus -Uri "$rawURL/v1/memory/health" -ExpectedStatus 200

    Invoke-ExpectedStatus -Uri "$gatewayURL/v1/memory/store" -ExpectedStatus 401
    Invoke-ExpectedStatus -Uri "$gatewayURL/v1/memory/store" -ExpectedStatus 401 -Authorization "Bearer invalid-runtime-capability"
    Invoke-ExpectedStatus -Uri "$rawURL/v1/memory/store" -ExpectedStatus 401

    $capabilityKey = Get-SecretKeyBase64 -Key "capability-hmac-key"
    $upstreamTokenBase64 = Get-SecretKeyBase64 -Key "upstream-token"
    if (-not $capabilityKey -or -not $upstreamTokenBase64) { throw "Memory gateway Secret is missing required keys" }
    $upstreamToken = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($upstreamTokenBase64))
    $keyIDBase64 = (& kubectl @kubectlContextArguments get secret "$ReleaseName-memory-policy-gateway" -n $ControllerNamespace '-o=jsonpath={.data.capability-hmac-key-id}' | Out-String).Trim()
    if (-not $keyIDBase64) { throw "Memory policy gateway Secret is missing capability-hmac-key-id" }
    $keyID = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($keyIDBase64))
    $capability = New-MemoryCapability

    Invoke-ExpectedStatus -Uri "$rawURL/v1/memory/provision" -ExpectedStatus 201 -Authorization "Bearer $upstreamToken" -Body ("{`"volume_handle`":`"$volumeHandle`"}")
    Invoke-ExpectedStatus -Uri "$rawURL/v1/memory/store" -ExpectedStatus 401 -Authorization "Bearer $capability" -IdempotencyKey $memoryDenyKey -Body ("{`"volume_handle`":`"$volumeHandle`",`"id`":`"bypass-record`",`"content`":`"denied`"}")

    Invoke-ExpectedStatus -Uri "$gatewayURL/v1/memory/store" -ExpectedStatus 403 -Authorization "Bearer $capability" -IdempotencyKey $memoryDenyKey -MemoryInvocationID $memoryDenyKey -Body ("{`"volume_handle`":`"$volumeHandle`",`"id`":`"$memoryDenyKey`",`"content`":`"denied`"}")
    $memoryDeniedAudit = Wait-ForAuditEntry -Description "missing-grant memory-operation audit" { $_.eventType -eq 'memory_op' -and $_.workspace -eq $namespace -and $_.PSObject.Properties['sessionId'] -and $_.sessionId -eq $sessionID -and $_.verb -eq 'store' -and $_.subjectRef -eq $volumeHandle -and $_.details.request.PSObject.Properties['invocationId'] -and $_.details.request.invocationId -eq $memoryDenyKey -and $_.details.outcome -eq 'denied' -and $_.details.reasonCode -eq 'grant_missing' }
    if (($DemoRunId -and $memoryDeniedAudit.details.runId -ne $DemoRunId) -or $memoryDeniedAudit.details.execution.status -ne 'blocked' -or $memoryDeniedAudit.details.execution.targetReached -ne $false -or $memoryDeniedAudit.details.authorization.grant -ne 'denied') {
        throw "Denied memory trust decision did not prove blocked execution: $($memoryDeniedAudit.details | ConvertTo-Json -Compress -Depth 10)"
    }

    Apply-KubernetesObject @{
        apiVersion = "kognition.dev/v1alpha1"
        kind = "MemoryGrant"
        metadata = @{ name = $grantName; namespace = $namespace }
        spec = @{ agentRef = @{ name = $agentName }; memoryClaimRef = @{ name = $claimName }; accessMode = "ReadWrite" }
    }
    Wait-ForMemoryGrantFinalizer -Name $grantName
    Assert-MemorySession
    $capability = New-MemoryCapability

    Invoke-ExpectedStatusEventually -Uri "$gatewayURL/v1/memory/store" -ExpectedStatus 201 -Authorization "Bearer $capability" -IdempotencyKey $memoryAllowKey -MemoryInvocationID $memoryAllowKey -Body ("{`"volume_handle`":`"$volumeHandle`",`"id`":`"$memoryAllowKey`",`"content`":`"authorized`"}")

    $stored = Invoke-HttpRequest -Uri "$rawURL/v1/memory/get/$volumeHandle/$memoryAllowKey" -Method Get -Headers @{ Authorization = "Bearer $upstreamToken" }
    $storedRecord = $stored.Content | ConvertFrom-Json
    if ($stored.StatusCode -ne 200 -or $storedRecord.content -ne "authorized" -or $storedRecord.metadata.'kognition.dev/memory-grant' -ne $grantName) {
        throw "Gateway write did not persist through the authenticated raw memory service: $($stored.Content)"
    }
    $memoryAudit = Wait-ForAuditEntry -Description "completed memory-operation audit" { $_.eventType -eq 'memory_op' -and $_.workspace -eq $namespace -and $_.PSObject.Properties['sessionId'] -and $_.sessionId -eq $sessionID -and $_.verb -eq 'store' -and $_.subjectRef -eq $volumeHandle -and $_.details.runId -eq $DemoRunId -and $_.details.request.operation -eq 'store' -and $_.details.request.targetRef.name -eq $volumeHandle -and $_.details.request.PSObject.Properties['invocationId'] -and $_.details.request.invocationId -eq $memoryAllowKey -and $_.details.authorization.grantRef.name -eq $grantName -and $_.details.outcome -eq 'completed' -and $_.details.reasonCode -eq 'target_completed' -and $_.details.execution.status -eq 'completed' -and $_.details.execution.targetReached -eq $true }
    if ($memoryAudit.agentIdentity -ne $spiffeID -or $memoryAudit.workspace -ne $namespace) {
        throw "Completed Memory audit AgentIdentity=$($memoryAudit.agentIdentity) Workspace=$($memoryAudit.workspace), want AgentIdentity=$spiffeID Workspace=$namespace"
    }

    # Keep the agent authorized through a second grant. The next assertions
    # prove record visibility follows write provenance, not only claim access.
    Apply-KubernetesObject @{
        apiVersion = "kognition.dev/v1alpha1"
        kind = "MemoryGrant"
        metadata = @{ name = $continuingGrantName; namespace = $namespace }
        spec = @{ agentRef = @{ name = $agentName }; memoryClaimRef = @{ name = $claimName }; accessMode = "ReadWrite" }
    }
    Wait-ForMemoryGrantFinalizer -Name $continuingGrantName

    # Deleting the original grant records a durable claim tombstone and hides
    # records it authorized, even though the second grant still authorizes the
    # same agent to access the memory claim.
    Invoke-Kubectl @("delete", "memorygrant/$grantName", "-n", $namespace, "--wait=false") | Out-Null
    $deadline = [DateTime]::UtcNow.AddSeconds(45)
    do {
        $claim = (& kubectl @kubectlContextArguments get "memoryclaim/$claimName" -n $namespace -o json | ConvertFrom-Json)
        $revocationProperty = $claim.metadata.annotations.PSObject.Properties['kognition.dev/revoked-memory-grants']
        $revocations = if ($revocationProperty) { $revocationProperty.Value } else { $null }
        if ($revocations -and ($revocations | ConvertFrom-Json) -contains $grantName) { break }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    if (-not $revocations -or -not (($revocations | ConvertFrom-Json) -contains $grantName)) {
        $observedAnnotations = $claim.metadata.annotations | ConvertTo-Json -Compress
        throw "MemoryClaim did not persist the revoked grant tombstone; observed annotations: $observedAnnotations"
    }
    $grantRevocationAudit = Wait-ForAuditEntry -Description "MemoryGrant revoke audit" { $_.eventType -eq 'grant_change' -and $_.workspace -eq $namespace -and $_.verb -eq 'revoke' -and $_.subjectRef -eq "$namespace/$grantName" }
    $claimBlockAudit = Wait-ForAuditEntry -Description "MemoryGrant claim revoke-block audit" { $_.eventType -eq 'grant_change' -and $_.workspace -eq $namespace -and $_.verb -eq 'revoke-block' -and $_.subjectRef -eq "$namespace/$grantName" }
    if (-not $grantRevocationAudit -or -not $claimBlockAudit) { throw "MemoryGrant revocation audit entries were not persisted" }

    Assert-MemorySession
    $capability = New-MemoryCapability

    $revokedGet = Invoke-HttpRequest -Uri "$gatewayURL/v1/memory/get/$volumeHandle/$memoryAllowKey" -Method Get -Headers @{ Authorization = "Bearer $capability"; 'Idempotency-Key' = $memoryRevokedKey }
    if ($revokedGet.StatusCode -ne 404) { throw "Revoked grant record remained readable: $($revokedGet.StatusCode) $($revokedGet.Content)" }
    foreach ($request in @(
        @{ path = "/v1/memory/list"; body = ("{`"volume_handle`":`"$volumeHandle`"}") },
        @{ path = "/v1/memory/search"; body = ("{`"volume_handle`":`"$volumeHandle`",`"query`":`"authorized`",`"top_k`":5}") }
    )) {
        $response = Invoke-HttpRequest -Uri "$gatewayURL$($request.path)" -Method Post -Headers @{ Authorization = "Bearer $capability" } -Body $request.body
        [object[]]$results = @()
        if ($response.Content.Trim() -ne '[]') { $results = @($response.Content | ConvertFrom-Json) }
        if ($response.StatusCode -ne 200 -or $results.Count -ne 0) { throw "Revoked grant record remained visible through $($request.path): $($response.StatusCode) $($response.Content)" }
    }
    Invoke-ExpectedStatus -Uri "$gatewayURL/v1/memory/store" -ExpectedStatus 201 -Authorization "Bearer $capability" -MemoryInvocationID $memoryContinuingKey -Body ("{`"volume_handle`":`"$volumeHandle`",`"id`":`"continuing-record`",`"content`":`"authorized after revocation`"}")

    # TC-0077: remove the final live grant and require the resulting denial to
    # reach the durable memory-operation audit log within five seconds.
    Invoke-Kubectl @("delete", "memorygrant/$continuingGrantName", "-n", $namespace, "--wait=false") | Out-Null
    Assert-MemorySession
    $capability = New-MemoryCapability
    $auditStopwatch = [Diagnostics.Stopwatch]::StartNew()
    Invoke-ExpectedStatus -Uri "$gatewayURL/v1/memory/search" -ExpectedStatus 403 -Authorization "Bearer $capability" -IdempotencyKey $memoryRevokedKey -MemoryInvocationID $memoryRevokedKey -Body ("{`"volume_handle`":`"$volumeHandle`",`"query`":`"authorized`",`"top_k`":5}")
    $auditStopwatch.Stop()
    if ($auditStopwatch.Elapsed.TotalSeconds -gt 5) { throw "Revoked memory denial audit latency was $($auditStopwatch.Elapsed.TotalSeconds) seconds, want <= 5" }
    $deniedAudit = Wait-ForAuditEntry -TimeoutSeconds 15 -Description "revoked memory-operation audit" { $_.eventType -eq 'memory_op' -and $_.workspace -eq $namespace -and $_.PSObject.Properties['sessionId'] -and $_.sessionId -eq $sessionID -and $_.verb -eq 'search' -and $_.subjectRef -eq $volumeHandle -and $_.details.request.PSObject.Properties['invocationId'] -and $_.details.request.invocationId -eq $memoryRevokedKey -and $_.details.outcome -eq 'revoked' -and $_.details.reasonCode -eq 'grant_revoked' }
    if (($DemoRunId -and $deniedAudit.details.runId -ne $DemoRunId) -or $deniedAudit.details.plane -ne 'memory' -or $deniedAudit.details.execution.status -ne 'blocked' -or $deniedAudit.details.execution.targetReached -ne $false -or $deniedAudit.details.authorization.grant -ne 'revoked' -or $deniedAudit.details.authorization.grantRef.name -ne $grantName -or $deniedAudit.details.request.operation -ne 'search' -or $deniedAudit.details.request.targetRef.name -ne $volumeHandle) {
        throw "Revoked memory trust decision did not contain the expected normalized evidence: $($deniedAudit.details | ConvertTo-Json -Compress -Depth 10)"
    }

    if ($ReportPath) {
        $reportDirectory = Split-Path $ReportPath -Parent
        if ($reportDirectory) { [IO.Directory]::CreateDirectory($reportDirectory) | Out-Null }
        [PSCustomObject]@{
            schemaVersion = "1.0"
            phase = "Memory"
            demoRunId = $DemoRunId
            namespace = $namespace
            attemptId = $AttemptId
            grantName = $grantName
            completedAt = [DateTimeOffset]::UtcNow.ToString("o")
            passed = $true
            invocationIds = [PSCustomObject]@{
                allow = $memoryAllowKey
                deny = $memoryDenyKey
                revoked = $memoryRevokedKey
            }
            assertions = @(
                [PSCustomObject]@{
                    name = "memory-allow"
                    passed = $true
                    timestamp = $memoryAudit.timestamp
                    invocationId = $memoryAllowKey
                    outcome = $memoryAudit.details.outcome
                    reasonCode = $memoryAudit.details.reasonCode
                    executionStatus = $memoryAudit.details.execution.status
                    targetReached = $memoryAudit.details.execution.targetReached
                    persistedGrant = $storedRecord.metadata.'kognition.dev/memory-grant'
                }
                [PSCustomObject]@{
                    name = "memory-deny"
                    passed = $true
                    timestamp = $memoryDeniedAudit.timestamp
                    invocationId = $memoryDenyKey
                    outcome = $memoryDeniedAudit.details.outcome
                    reasonCode = $memoryDeniedAudit.details.reasonCode
                    targetReached = $memoryDeniedAudit.details.execution.targetReached
                }
                [PSCustomObject]@{
                    name = "memory-revoked"
                    passed = $true
                    timestamp = $deniedAudit.timestamp
                    invocationId = $memoryRevokedKey
                    outcome = $deniedAudit.details.outcome
                    reasonCode = $deniedAudit.details.reasonCode
                    targetReached = $deniedAudit.details.execution.targetReached
                    auditLatencySeconds = $auditStopwatch.Elapsed.TotalSeconds
                }
            )
        } | ConvertTo-Json -Depth 20 | Set-Content -Path $ReportPath -Encoding utf8
        Write-Host "Memory assertion report: $ReportPath"
    }

    Write-Host "Memory gateway authorizes a bound capability, stamps grant provenance, blocks revoked records, durably audits grant cascades and revoked-operation denial within five seconds, and rejects direct raw-service access." -ForegroundColor Green
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
    if ($namespace -and -not $KeepScopeNamespace) {
        & kubectl @kubectlContextArguments delete namespace $namespace --ignore-not-found --wait=false 2>$null | Out-Null
    }
}
