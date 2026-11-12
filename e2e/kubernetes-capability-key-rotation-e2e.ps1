# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

param(
    [string]$ControllerNamespace = "kognition-system",
    [string]$ReleaseName = "kognition"
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

. (Join-Path $PSScriptRoot "capability-mint.ps1")

$runID = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
$namespace = "capability-rotation-e2e-$runID"
$gatewayService = "$ReleaseName-tool-gateway"
$gatewaySecret = "$ReleaseName-tool-gateway"
$controllerDeployment = "$ReleaseName-k8s-controller"
$portForward = $null
$controllerReplicas = $null

function Invoke-Kubectl {
    param([Parameter(Mandatory = $true)][string[]]$Arguments)

    $output = & kubectl @Arguments 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "kubectl $($Arguments -join ' ') failed:`n$($output -join "`n")"
    }
    return $output
}

function Apply-KubernetesObject {
    param([Parameter(Mandatory = $true)][hashtable]$Object)

    $Object | ConvertTo-Json -Depth 30 -Compress | & kubectl apply -f - 2>&1 | Out-Host
    if ($LASTEXITCODE -ne 0) {
        throw "apply $($Object.kind)/$($Object.metadata.name) failed"
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
        $value = (& kubectl get $Resource -n $namespace "-o=jsonpath=$JsonPath" 2>$null | Out-String).Trim()
        if ($value -eq $Expected) { return }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "$Resource did not reach $JsonPath=$Expected within $TimeoutSeconds seconds"
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

function Exchange-Capability {
    param(
        [Parameter(Mandatory = $true)][string]$Uri,
        [Parameter(Mandatory = $true)][string]$Capability,
        [Parameter(Mandatory = $true)][string]$ToolName,
        [Parameter(Mandatory = $true)][string]$InvocationID
    )

    $body = @{ tool_name = $ToolName; invocation_id = $InvocationID; input = @{} } | ConvertTo-Json -Compress
    $response = Invoke-WebRequest -Uri $Uri -Method Post -Headers @{ Authorization = "Bearer $Capability" } -ContentType "application/json" -Body $body -SkipHttpErrorCheck
    if ($response.StatusCode -ne 200) {
        throw "Tool capability exchange returned $($response.StatusCode): $($response.Content)"
    }
    return ($response.Content | ConvertFrom-Json).capability
}

function Set-ActiveToolSession {
    param([Parameter(Mandatory = $true)][string]$InstanceName, [Parameter(Mandatory = $true)][string]$SessionID)

    Invoke-Kubectl @("patch", "agentinstance/$InstanceName", "-n", $namespace, "--subresource=status", "--type=merge", "-p", ("{`"status`":{`"phase`":`"Running`",`"sessionID`":`"$SessionID`"}}")) | Out-Null
}

Write-Host "=== Kognition Capability Key Rotation E2E ===" -ForegroundColor Cyan

try {
    Invoke-Kubectl @("wait", "-n", $ControllerNamespace, "--for=condition=Available", "deployment/$gatewayService", "--timeout=120s") | Out-Null
    Invoke-Kubectl @("wait", "-n", $ControllerNamespace, "--for=condition=Available", "deployment/$controllerDeployment", "--timeout=120s") | Out-Null
    Invoke-Kubectl @("get", "deployment/tool-sandbox", "-n", "default") | Out-Null
    Invoke-Kubectl @("create", "namespace", $namespace) | Out-Null

    $agentName = "rotation-agent"
    $identityName = "agent-identity-$agentName"
    $instanceName = "rotation-instance"
    $toolName = "rotation-timestamp"
    $sessionID = "rotation-session-$runID"
    $oldInvocationID = "rotation-old-$runID"
    $newInvocationID = "rotation-new-$runID"

    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "Agent"; metadata = @{ name = $agentName; namespace = $namespace }; spec = @{ modelRef = @{ name = "placeholder" }; instructions = @{ inline = "Capability rotation E2E fixture." }; replicas = 0 } }
    Wait-ForJsonPath -Resource "agentidentity/$identityName" -JsonPath "{.status.phase}" -Expected "Ready"
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "Tool"; metadata = @{ name = $toolName; namespace = $namespace }; spec = @{ className = "sandbox-openapi"; protocol = "OpenAPI"; description = "Capability rotation target."; endpoint = "http://tool-sandbox.samples-tool-sandbox.svc.cluster.local:8080/timestamp"; metadata = @{ allowPrivateNetwork = "true" }; inputSchema = @{ type = "object"; properties = @{} } } }
    Wait-ForJsonPath -Resource "tool/$toolName" -JsonPath "{.status.phase}" -Expected "Ready"
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "AgentPolicy"; metadata = @{ name = "rotation-grants"; namespace = $namespace }; spec = @{ scope = "agent"; enforce = $true; targetAgents = @($agentName); toolGrants = @(@{ toolRef = $toolName; allow = $true }) } }
    Wait-ForJsonPath -Resource "agentpolicy/rotation-grants" -JsonPath "{.status.phase}" -Expected "Ready"
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "AgentRole"; metadata = @{ name = "tool-invoker"; namespace = $namespace }; rules = @(@{ resources = @("Tool"); verbs = @("get", "invoke") }) }
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "AgentRoleBinding"; metadata = @{ name = "tool-invoker"; namespace = $namespace }; spec = @{ roleRef = @{ name = "tool-invoker" }; subjects = @(@{ kind = "Agent"; name = $agentName }) } }
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "AgentInstance"; metadata = @{ name = $instanceName; namespace = $namespace }; spec = @{ agentRef = @{ name = $agentName }; revision = 1 } }

    # This direct gateway fixture has no runtime Pod. Pause status polling only
    # after admission has created every resource, then restore the controller
    # after proving previous-key overlap.
    $controllerReplicas = (& kubectl get "deployment/$controllerDeployment" -n $ControllerNamespace -o jsonpath='{.spec.replicas}' | Out-String).Trim()
    if (-not $controllerReplicas) { $controllerReplicas = "1" }
    Invoke-Kubectl @("scale", "deployment/$controllerDeployment", "-n", $ControllerNamespace, "--replicas=0") | Out-Null
    Invoke-Kubectl @("rollout", "status", "deployment/$controllerDeployment", "-n", $ControllerNamespace, "--timeout=120s") | Out-Null

    $instanceUID = (& kubectl get "agentinstance/$instanceName" -n $namespace -o jsonpath='{.metadata.uid}' | Out-String).Trim()
    $oldKey = (& kubectl get secret $gatewaySecret -n $ControllerNamespace -o jsonpath='{.data.capability-hmac-key}' | Out-String).Trim()
    $oldKeyIDBase64 = (& kubectl get secret $gatewaySecret -n $ControllerNamespace -o jsonpath='{.data.capability-hmac-key-id}' | Out-String).Trim()
    if (-not $instanceUID -or -not $oldKey -or -not $oldKeyIDBase64) { throw "Rotation fixture lacks instance or active key data" }
    $oldKeyID = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($oldKeyIDBase64))
    $oldSessionCapability = Invoke-CapabilityMint -KeyBase64 $oldKey -FilePath "go" -ArgumentList @("run", "./e2e/mint-tool-session-capability.go", "-key-id", $oldKeyID, "-namespace", $namespace, "-agent", $agentName, "-identity", $identityName, "-instance", $instanceName, "-instance-uid", $instanceUID, "-session", $sessionID)

    $alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
    $newKey = -join (1..48 | ForEach-Object { $alphabet[[System.Security.Cryptography.RandomNumberGenerator]::GetInt32($alphabet.Length)] })
    $newKeyBase64 = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($newKey))
    $newKeyID = "rotation-$runID"
    $newKeyIDBase64 = [Convert]::ToBase64String([Text.Encoding]::UTF8.GetBytes($newKeyID))
    $patch = @{ data = @{ "capability-hmac-key" = $newKeyBase64; "capability-hmac-key-id" = $newKeyIDBase64; "capability-hmac-previous-key" = $oldKey; "capability-hmac-previous-key-id" = $oldKeyIDBase64 } } | ConvertTo-Json -Compress
    Invoke-Kubectl @("patch", "secret/$gatewaySecret", "-n", $ControllerNamespace, "--type=merge", "-p", $patch) | Out-Null
    $storedPreviousKey = (& kubectl get secret $gatewaySecret -n $ControllerNamespace -o jsonpath='{.data.capability-hmac-previous-key}' | Out-String).Trim()
    $storedPreviousIDBase64 = (& kubectl get secret $gatewaySecret -n $ControllerNamespace -o jsonpath='{.data.capability-hmac-previous-key-id}' | Out-String).Trim()
    if ($storedPreviousKey -ne $oldKey -or $storedPreviousIDBase64 -ne $oldKeyIDBase64) { throw "Rotation Secret did not preserve the former active Tool Gateway key and ID" }
    Invoke-Kubectl @("rollout", "restart", "deployment/$gatewayService", "-n", $ControllerNamespace) | Out-Null
    Invoke-Kubectl @("rollout", "status", "deployment/$gatewayService", "-n", $ControllerNamespace, "--timeout=180s") | Out-Null

    $gatewayPort = Get-AvailableTcpPort
    $portForward = Start-Job -ScriptBlock { param($Namespace, $Service, $Port) kubectl -n $Namespace port-forward "service/$Service" "${Port}:8092" } -ArgumentList $ControllerNamespace, $gatewayService, $gatewayPort
    $gatewayURL = "http://127.0.0.1:$gatewayPort/v1/tools/capabilities"
    $deadline = [DateTime]::UtcNow.AddSeconds(30)
    $oldExactCapability = ""
    do {
        try {
            Set-ActiveToolSession -InstanceName $instanceName -SessionID $sessionID
            $oldExactCapability = Exchange-Capability -Uri $gatewayURL -Capability $oldSessionCapability -ToolName $toolName -InvocationID $oldInvocationID
            break
        }
        catch {
            [Threading.Tasks.Task]::Delay(250).GetAwaiter().GetResult()
        }
    } while ([DateTime]::UtcNow -lt $deadline)
    if (-not $oldExactCapability) { throw "Rotated Tool Gateway did not accept a capability signed by the previous key" }

    Invoke-Kubectl @("scale", "deployment/$controllerDeployment", "-n", $ControllerNamespace, "--replicas=$controllerReplicas") | Out-Null
    Invoke-Kubectl @("rollout", "status", "deployment/$controllerDeployment", "-n", $ControllerNamespace, "--timeout=180s") | Out-Null
    # The restarted controller has loaded the new active key. Pause it again
    # while validating the synthetic session, then finally restores replicas.
    Invoke-Kubectl @("scale", "deployment/$controllerDeployment", "-n", $ControllerNamespace, "--replicas=0") | Out-Null
    Invoke-Kubectl @("rollout", "status", "deployment/$controllerDeployment", "-n", $ControllerNamespace, "--timeout=120s") | Out-Null
    $newSessionCapability = Invoke-CapabilityMint -KeyBase64 $newKeyBase64 -FilePath "go" -ArgumentList @("run", "./e2e/mint-tool-session-capability.go", "-key-id", $newKeyID, "-namespace", $namespace, "-agent", $agentName, "-identity", $identityName, "-instance", $instanceName, "-instance-uid", $instanceUID, "-session", $sessionID)
    Set-ActiveToolSession -InstanceName $instanceName -SessionID $sessionID
    $newExactCapability = Exchange-Capability -Uri $gatewayURL -Capability $newSessionCapability -ToolName $toolName -InvocationID $newInvocationID
    if (-not $newExactCapability) { throw "Rotated Tool Gateway did not accept a capability signed by the new active key" }

    Write-Host "Tool Gateway accepted prior-key capabilities during overlap and new-key capabilities after controller rotation." -ForegroundColor Green
    Write-Host "=== PASS ===" -ForegroundColor Green
}
finally {
    if ($portForward) {
        Stop-Job $portForward -ErrorAction SilentlyContinue
        Remove-Job $portForward -Force -ErrorAction SilentlyContinue
    }
    if ($namespace) {
        & kubectl delete namespace $namespace --ignore-not-found --wait=false 2>$null | Out-Null
    }
    if ($null -ne $controllerReplicas) {
        & kubectl scale "deployment/$controllerDeployment" -n $ControllerNamespace "--replicas=$controllerReplicas" 2>$null | Out-Null
        & kubectl wait -n $ControllerNamespace --for=condition=Available "deployment/$controllerDeployment" --timeout=120s 2>$null | Out-Null
    }
}
