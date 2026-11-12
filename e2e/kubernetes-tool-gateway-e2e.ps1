# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

param(
    [string]$KubernetesContext = "",
    [string]$ControllerNamespace = "kognition-system",
    [string]$ReleaseName = "kognition",
    [string]$ToolTargetNamespace = "default",
    [string]$ToolTargetService = "tool-sandbox",
    [string]$ToolTargetDeployment = "tool-sandbox",
    [string]$ToolTargetPodLabel = "tool-sandbox",
    [switch]$ProvisionToolTarget,
    [string]$DemoRunId = "",
    [string]$ScopeNamespace = "",
    [string]$FixtureModelName = "placeholder",
    [switch]$RevocationOnly,
    [switch]$KeepScopeNamespace
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

. (Join-Path $PSScriptRoot "capability-mint.ps1")
. (Join-Path $PSScriptRoot "gateway-client-tracing.ps1")

$gatewayService = "$ReleaseName-tool-gateway"
$controllerDeployment = "$ReleaseName-k8s-controller"
$runID = if ($DemoRunId) { $DemoRunId } else { [string][DateTimeOffset]::UtcNow.ToUnixTimeSeconds() }
$namespace = if ($ScopeNamespace) { $ScopeNamespace } else { "tool-gateway-e2e-$runID" }
$ownsNamespace = -not $ScopeNamespace
$portForward = $null
$targetNamespaceCreated = $false
$kubectlContextArguments = if ($KubernetesContext) { @("--context", $KubernetesContext) } else { @() }
if ($ProvisionToolTarget) {
    $ToolTargetService = "tool-e2e-target-$runID"
    $ToolTargetDeployment = $ToolTargetService
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
        $content = $_.ErrorDetails.Message
        $reader = [IO.StreamReader]::new($response.GetResponseStream())
        try {
            if (-not $content) { $content = $reader.ReadToEnd() }
            return [PSCustomObject]@{ StatusCode = [int]$response.StatusCode; Content = $content }
        }
        finally {
            $reader.Dispose()
            $response.Dispose()
        }
    }
}

function Get-CostRecords {
    $configMaps = (& kubectl @kubectlContextArguments get configmaps -n $ControllerNamespace -l 'kognition.dev/cost-record=true' -o json | ConvertFrom-Json).items
    $records = @()
    foreach ($configMap in $configMaps) {
        $encoded = $configMap.binaryData.'records.json'
        if (-not $encoded) { continue }
        $records += @([Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($encoded)) | ConvertFrom-Json)
    }
    return @($records)
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
    param([Parameter(Mandatory = $true)][scriptblock]$Predicate, [int]$TimeoutSeconds = 45, [string]$Description = "expected durable Tool Gateway audit entry")

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        $entry = Get-AuditEntries | Where-Object $Predicate | Select-Object -First 1
        if ($entry) { return $entry }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Timed out waiting for $Description"
}

function Wait-ForCostRecord {
    param([Parameter(Mandatory = $true)][scriptblock]$Predicate)

    $deadline = [DateTime]::UtcNow.AddSeconds(45)
    do {
        $record = Get-CostRecords | Where-Object $Predicate | Select-Object -First 1
        if ($record) { return $record }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Timed out waiting for the expected durable cost record"
}

function Apply-KubernetesObject {
    param([Parameter(Mandatory = $true)][hashtable]$Object)

    $Object | ConvertTo-Json -Depth 30 -Compress | & kubectl @kubectlContextArguments apply -f - 2>&1 | Out-Host
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
        $value = (& kubectl @kubectlContextArguments get $Resource -n $namespace "-o=jsonpath=$JsonPath" 2>$null | Out-String).Trim()
        if ($value -eq $Expected) { return }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "$Resource did not reach $JsonPath=$Expected within $TimeoutSeconds seconds"
}

function Wait-ForDeploymentScaleDown {
    param(
        [Parameter(Mandatory = $true)][string]$Deployment,
        [string]$Namespace = $ControllerNamespace
    )

    $deadline = [DateTime]::UtcNow.AddSeconds(60)
    do {
        $replicas = (& kubectl @kubectlContextArguments get "deployment/$Deployment" -n $Namespace -o jsonpath='{.status.replicas}' 2>$null | Out-String).Trim()
        if (-not $replicas -or $replicas -eq "0") { return }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Deployment/$Deployment in namespace $Namespace did not scale down within 60 seconds"
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
        [string]$Body = "{}",
        [string]$Authorization = ""
    )

    $headers = @{}
    if ($Authorization) { $headers.Authorization = $Authorization }
    $response = Invoke-HttpRequest -Uri $Uri -Method Post -Headers $headers -Body $Body
    if ($response.StatusCode -ne $ExpectedStatus) {
        throw ("POST {0} returned {1}, want {2}. Body: {3}" -f $Uri, $response.StatusCode, $ExpectedStatus, $response.Content)
    }
    return $response
}

function Get-ToolGatewayEvents {
    param([Parameter(Mandatory = $true)][string]$InstanceName)

    return @(((& kubectl @kubectlContextArguments get events -n $namespace --field-selector "involvedObject.name=$InstanceName" -o json | ConvertFrom-Json).items) |
        Where-Object { $_.reason -in @("ToolInvocationAuthorized", "ToolInvocationCompleted") })
}

function Set-ActiveToolSession {
    param(
        [Parameter(Mandatory = $true)][string]$InstanceName,
        [Parameter(Mandatory = $true)][string]$SessionID
    )

    $instance = Invoke-Kubectl @("get", "agentinstance/$InstanceName", "-n", $namespace, "-o", "json") | ConvertFrom-Json
    if ($instance.status.phase -ne "Running" -or $instance.status.sessionID -ne $SessionID) { throw "Controller-owned Tool session is no longer active" }
}

function Exchange-ToolSessionCapability {
    param(
        [Parameter(Mandatory = $true)][string]$InstanceName,
        [Parameter(Mandatory = $true)][string]$SessionID,
        [Parameter(Mandatory = $true)][string]$Uri,
        [Parameter(Mandatory = $true)][string]$Authorization,
        [scriptblock]$RefreshAuthorization,
        [Parameter(Mandatory = $true)][string]$Body
    )

    if ($RefreshAuthorization) {
        $Authorization = & $RefreshAuthorization
        if (-not $Authorization) { throw "Tool session capability refresh returned an empty authorization value" }
    }
    $stopwatch = [Diagnostics.Stopwatch]::StartNew()
    $response = $null
    do {
        Set-ActiveToolSession -InstanceName $InstanceName -SessionID $SessionID
        $response = Invoke-HttpRequest -Uri $Uri -Method Post -Headers @{ Authorization = $Authorization } -Body $Body
        if ($response.StatusCode -eq 200) { return $response }
        if ($response.StatusCode -eq 401 -and $RefreshAuthorization) {
            $Authorization = & $RefreshAuthorization
            if (-not $Authorization) { throw "Tool session capability refresh returned an empty authorization value" }
            continue
        }
        if ($response.StatusCode -ne 403) { throw "POST $Uri returned $($response.StatusCode), want 200. Body: $($response.Content)" }
        [Threading.Tasks.Task]::Delay(250).GetAwaiter().GetResult()
    } while ($stopwatch.Elapsed -lt [TimeSpan]::FromSeconds(20))
    $instanceState = (& kubectl @kubectlContextArguments get "agentinstance/$InstanceName" -n $namespace -o json 2>$null | Out-String).Trim()
    $identityState = (& kubectl @kubectlContextArguments get "agentidentity/agent-identity-$agentName" -n $namespace -o json 2>$null | Out-String).Trim()
    throw "Tool Gateway session capability exchange did not observe the active session within 20 seconds. Last response: $($response.Content). AgentInstance: $instanceState. AgentIdentity: $identityState"
}

function Invoke-ToolWithActiveSession {
    param(
        [Parameter(Mandatory = $true)][string]$InstanceName,
        [Parameter(Mandatory = $true)][string]$SessionID,
        [Parameter(Mandatory = $true)][string]$Uri,
        [Parameter(Mandatory = $true)][string]$Authorization,
        [Parameter(Mandatory = $true)][string]$Body
    )

    $deadline = [DateTime]::UtcNow.AddSeconds(20)
    $response = $null
    $invocationID = ($Body | ConvertFrom-Json).invocation_id
    do {
        Set-ActiveToolSession -InstanceName $InstanceName -SessionID $SessionID
        $response = Invoke-HttpRequest -Uri $Uri -Method Post -Headers @{ Authorization = $Authorization } -Body $Body
        if ($response.StatusCode -eq 200) { return $response }
        $retryUnavailable = $false
        if ($response.StatusCode -eq 503) {
            $outcome = $response.Content | ConvertFrom-Json
            $retryUnavailable = $outcome.invocation_id -ceq $invocationID -and $outcome.error_code -eq "ToolInvocationUnavailable" -and $outcome.runtime_approval_state -eq "unavailable"
        }
        if ($response.StatusCode -ne 403 -and -not $retryUnavailable) { throw "POST $Uri returned $($response.StatusCode), want 200. Body: $($response.Content)" }
        [Threading.Tasks.Task]::Delay(250).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Tool Gateway invocation did not complete within 20 seconds. Last response: $($response.Content)"
}

function Invoke-ToolPendingWithActiveSession {
    param(
        [Parameter(Mandatory = $true)][string]$InstanceName,
        [Parameter(Mandatory = $true)][string]$SessionID,
        [Parameter(Mandatory = $true)][string]$Uri,
        [Parameter(Mandatory = $true)][string]$Authorization,
        [Parameter(Mandatory = $true)][string]$Body
    )

    $deadline = [DateTime]::UtcNow.AddSeconds(20)
    $response = $null
    $invocationID = ($Body | ConvertFrom-Json).invocation_id
    do {
        Set-ActiveToolSession -InstanceName $InstanceName -SessionID $SessionID
        $response = Invoke-HttpRequest -Uri $Uri -Method Post -Headers @{ Authorization = $Authorization } -Body $Body
        if ($response.StatusCode -eq 202) { return $response }
        $retryUnavailable = $false
        if ($response.StatusCode -eq 503) {
            $outcome = $response.Content | ConvertFrom-Json
            $retryUnavailable = $outcome.invocation_id -ceq $invocationID -and $outcome.error_code -eq "ToolInvocationUnavailable" -and $outcome.runtime_approval_state -eq "unavailable"
        }
        if ($response.StatusCode -ne 403 -and -not $retryUnavailable) { throw "POST $Uri returned $($response.StatusCode), want 202. Body: $($response.Content)" }
        [Threading.Tasks.Task]::Delay(250).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Tool Gateway invocation did not observe the active session within 20 seconds. Last response: $($response.Content)"
}

function Approve-RuntimeApproval {
    param(
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)][string]$InvocationID
    )

    $approval = Invoke-Kubectl @("get", "runtimeapproval/$Name", "-n", $namespace, "-o", "json") | Out-String | ConvertFrom-Json
    if ($approval.spec.invocationID -ne $InvocationID -or $approval.status.decisionState -ne "Pending" -or $approval.status.executionState -ne "NotStarted") {
        throw "RuntimeApproval/$Name is not the expected pending invocation: $($approval | ConvertTo-Json -Compress -Depth 10)"
    }
    $decidedAt = [DateTime]::UtcNow.ToString("o")
    $issuer = "kognition-e2e"
    $subject = "tool-gateway-e2e"
    $patch = @{ status = @{
        decisionState = "Approved"
        decidedByIssuer = $issuer
        decidedBySubject = $subject
        decidedAt = $decidedAt
        decisionAudit = @{
            id = "e2e-$runID"
            payload = @{
                workspace = $namespace
                invocationID = $InvocationID
                approvalUID = $approval.metadata.uid
                decision = "Approved"
                issuer = $issuer
                subject = $subject
                decidedAt = $decidedAt
            }
        }
    } } | ConvertTo-Json -Depth 10 -Compress
    $patchFile = [IO.Path]::GetTempFileName()
    try {
        Set-Content -Path $patchFile -Value $patch -NoNewline
        Invoke-Kubectl @("--as=system:serviceaccount:$ControllerNamespace`:$gatewayService", "patch", "runtimeapproval/$Name", "-n", $namespace, "--subresource=status", "--type=merge", "--patch-file", $patchFile) | Out-Null
    }
    finally {
        Remove-Item -Path $patchFile -Force -ErrorAction SilentlyContinue
    }
    Wait-ForJsonPath -Resource "runtimeapproval/$Name" -JsonPath "{.status.decisionState}" -Expected "Approved"
}

function Get-ProvisionedToolTargetCount {
    $script = 'import json, urllib.request; print(json.loads(urllib.request.urlopen("http://127.0.0.1:8080/count").read())["count"])'
    return [int]((Invoke-Kubectl @("exec", "deployment/$ToolTargetDeployment", "-n", $ToolTargetNamespace, "--", "python", "-c", $script) | Out-String).Trim())
}

Write-Host "=== Kognition Kubernetes Tool Gateway E2E ===" -ForegroundColor Cyan

try {
    Invoke-Kubectl @("get", "deployment/$gatewayService", "-n", $ControllerNamespace) | Out-Null
    Invoke-Kubectl @("wait", "-n", $ControllerNamespace, "--for=condition=Available", "deployment/$gatewayService", "--timeout=120s") | Out-Null
    Invoke-Kubectl @("get", "deployment/$controllerDeployment", "-n", $ControllerNamespace) | Out-Null
    if ($ProvisionToolTarget) {
        & kubectl @kubectlContextArguments get namespace $ToolTargetNamespace 2>$null | Out-Null
        if ($LASTEXITCODE -ne 0) {
            Invoke-Kubectl @("create", "namespace", $ToolTargetNamespace) | Out-Null
            $targetNamespaceCreated = $true
        }
        $toolServer = @'
import datetime, json, time
from http.server import BaseHTTPRequestHandler, HTTPServer
request_count = 0
class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/healthz": self.send_response(200); self.end_headers(); return
        if self.path == "/count":
            body = json.dumps({"count": request_count}).encode()
            self.send_response(200); self.send_header("Content-Type", "application/json"); self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body); return
        self.send_error(404)
    def do_POST(self):
        global request_count
        if self.path != "/timestamp": self.send_error(404); return
        self.rfile.read(int(self.headers.get("Content-Length", "0")))
        request_count += 1
        body = json.dumps({"timestamp": datetime.datetime.now(datetime.timezone.utc).isoformat(), "unix": int(time.time())}).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json"); self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def log_message(self, *_): pass
HTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
'@
        Apply-KubernetesObject @{ apiVersion = "v1"; kind = "ConfigMap"; metadata = @{ name = $ToolTargetService; namespace = $ToolTargetNamespace }; data = @{ "server.py" = $toolServer } }
        Apply-KubernetesObject @{ apiVersion = "apps/v1"; kind = "Deployment"; metadata = @{ name = $ToolTargetDeployment; namespace = $ToolTargetNamespace }; spec = @{ replicas = 1; selector = @{ matchLabels = @{ app = $ToolTargetPodLabel; "kognition.dev/e2e-run" = "$runID" } }; template = @{ metadata = @{ labels = @{ app = $ToolTargetPodLabel; "kognition.dev/e2e-run" = "$runID" } }; spec = @{ containers = @(@{ name = "server"; image = "python:3.12-alpine"; command = @("python", "/app/server.py"); ports = @(@{ containerPort = 8080 }); readinessProbe = @{ httpGet = @{ path = "/healthz"; port = 8080 }; initialDelaySeconds = 1; periodSeconds = 1 }; volumeMounts = @(@{ name = "source"; mountPath = "/app" }) }); volumes = @(@{ name = "source"; configMap = @{ name = $ToolTargetService } }) } } } }
        Apply-KubernetesObject @{ apiVersion = "v1"; kind = "Service"; metadata = @{ name = $ToolTargetService; namespace = $ToolTargetNamespace }; spec = @{ selector = @{ app = $ToolTargetPodLabel; "kognition.dev/e2e-run" = "$runID" }; ports = @(@{ port = 8080; targetPort = 8080 }) } }
    }
    else {
        Invoke-Kubectl @("get", "deployment/$ToolTargetDeployment", "-n", $ToolTargetNamespace) | Out-Null
    }
    Invoke-Kubectl @("rollout", "status", "deployment/$ToolTargetDeployment", "-n", $ToolTargetNamespace, "--timeout=120s") | Out-Null
    if ($ownsNamespace) {
        Invoke-Kubectl @("create", "namespace", $namespace) | Out-Null
    }

    $agentName = "tool-e2e-agent"
    $identityName = "agent-identity-$agentName"
    $instanceName = "tool-e2e-instance"
    $toolName = "tool-e2e-timestamp"
    $grantName = "tool-e2e-grant"
    $policyName = "tool-e2e-policy"
    $sessionID = if ($DemoRunId) { "$DemoRunId`:tool-session" } else { "tool-e2e-session-$runID" }
    $invocationID = if ($DemoRunId) { "$DemoRunId`:tool-allow" } else { "tool-e2e-invocation-$runID" }

    Wait-ForJsonPath -Resource "model/$FixtureModelName" -JsonPath "{.status.phase}" -Expected "Ready"
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "Agent"; metadata = @{ name = $agentName; namespace = $namespace }; spec = @{ modelRef = @{ name = $FixtureModelName }; instructions = @{ inline = "Tool Gateway E2E fixture." }; replicas = 1 } }
    Wait-ForJsonPath -Resource "agentidentity/$identityName" -JsonPath "{.status.phase}" -Expected "Ready"
    $spiffeID = (& kubectl @kubectlContextArguments get "agentidentity/$identityName" -n $namespace -o jsonpath='{.spec.spiffeID}' | Out-String).Trim()
    if (-not $spiffeID) { throw "AgentIdentity did not report a SPIFFE ID" }
    if ($RevocationOnly) {
        Invoke-Kubectl @("get", "tool/$toolName", "-n", $namespace) | Out-Null
    }
    else {
        Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "Tool"; metadata = @{ name = $toolName; namespace = $namespace }; spec = @{ className = "sandbox-openapi"; protocol = "OpenAPI"; description = "Tool Gateway E2E target."; endpoint = "http://$ToolTargetService.$ToolTargetNamespace.svc.cluster.local:8080/timestamp"; metadata = @{ allowPrivateNetwork = "true" }; inputSchema = @{ type = "object"; properties = @{ timezone = @{ type = "string" }; includeUnix = @{ type = "boolean" } } } } }
    }
    Wait-ForJsonPath -Resource "tool/$toolName" -JsonPath "{.status.phase}" -Expected "Ready"
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "ToolGrant"; metadata = @{ name = $grantName; namespace = $namespace }; spec = @{ agentRef = @{ name = $agentName }; toolRef = @{ name = $toolName }; verbs = @("invoke") } }
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "ModelGrant"; metadata = @{ name = "tool-e2e-model"; namespace = $namespace }; spec = @{ agentRef = @{ name = $agentName }; modelRef = @{ name = $FixtureModelName } } }
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "AgentRole"; metadata = @{ name = "tool-invoker"; namespace = $namespace }; rules = @(@{ resources = @("Tool"); verbs = @("get", "invoke") }) }
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "AgentRoleBinding"; metadata = @{ name = "tool-invoker"; namespace = $namespace }; spec = @{ roleRef = @{ name = "tool-invoker" }; subjects = @(@{ kind = "Agent"; name = $agentName }) } }
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "AgentInstance"; metadata = @{ name = $instanceName; namespace = $namespace }; spec = @{ agentRef = @{ name = $agentName }; revision = 1 } }

    Wait-ForJsonPath -Resource "agentinstance/$instanceName" -JsonPath "{.status.phase}" -Expected "Running" -TimeoutSeconds 180
    Wait-ForJsonPath -Resource "agent/$agentName" -JsonPath "{.status.phase}" -Expected "Ready" -TimeoutSeconds 180
    $sessionID = (Invoke-Kubectl @("get", "agentinstance/$instanceName", "-n", $namespace, "-o", "jsonpath={.status.sessionID}") | Out-String).Trim()
    if (-not $sessionID) { throw "Controller did not assign a Tool session" }
    $instanceUID = (& kubectl @kubectlContextArguments get "agentinstance/$instanceName" -n $namespace -o jsonpath='{.metadata.uid}' | Out-String).Trim()
    $toolVersion = (& kubectl @kubectlContextArguments get "tool/$toolName" -n $namespace -o jsonpath='{.metadata.resourceVersion}' | Out-String).Trim()
    if (-not $instanceUID -or -not $toolVersion) { throw "Fixture did not receive instance UID or Tool resource version" }

    $gatewayPort = Get-AvailableTcpPort
    $portForward = Start-ServicePortForward -Namespace $ControllerNamespace -Service $gatewayService -LocalPort $gatewayPort -ServicePort 8092
    $gatewayURL = "http://127.0.0.1:$gatewayPort"
    Wait-ForHttpStatus -Uri "$gatewayURL/healthz" -ExpectedStatus 200
    Invoke-ExpectedStatus -Uri "$gatewayURL/v1/tools/capabilities" -ExpectedStatus 401 -Body ("{`"tool_name`":`"$toolName`",`"invocation_id`":`"$invocationID`"}") | Out-Null

    $keyBase64 = (& kubectl @kubectlContextArguments get secret "$ReleaseName-tool-gateway" -n $ControllerNamespace '-o=jsonpath={.data.capability-hmac-key}' | Out-String).Trim()
    $keyIDBase64 = (& kubectl @kubectlContextArguments get secret "$ReleaseName-tool-gateway" -n $ControllerNamespace '-o=jsonpath={.data.capability-hmac-key-id}' | Out-String).Trim()
    if (-not $keyBase64 -or -not $keyIDBase64) { throw "Tool Gateway capability Secret is missing active key material or ID" }
    $keyID = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($keyIDBase64))
    $mintSessionAuthorization = {
        $capability = Invoke-CapabilityMint -KeyBase64 $keyBase64 -FilePath "go" -ArgumentList @("run", "./e2e/mint-tool-session-capability.go", "-key-id", $keyID, "-namespace", $namespace, "-agent", $agentName, "-identity", $identityName, "-instance", $instanceName, "-instance-uid", $instanceUID, "-session", $sessionID)
        if (-not $capability) { throw "Tool session capability helper returned an empty token" }
        return "Bearer $capability"
    }
    $sessionAuthorization = & $mintSessionAuthorization

    $invocationInput = @{ timezone = "UTC"; includeUnix = $true }
    if ($RevocationOnly) {
        if (-not $DemoRunId -or -not $ScopeNamespace) {
            throw "RevocationOnly requires DemoRunId and ScopeNamespace"
        }
        $revokedInvocationID = "$DemoRunId`:tool-revoked"
        $revocationExchangeBody = @{ tool_name = $toolName; invocation_id = $revokedInvocationID; input = $invocationInput } | ConvertTo-Json -Compress
        $revocationExchange = Exchange-ToolSessionCapability -InstanceName $instanceName -SessionID $sessionID -Uri "$gatewayURL/v1/tools/capabilities" -Authorization $sessionAuthorization -RefreshAuthorization $mintSessionAuthorization -Body $revocationExchangeBody
        $revocationExchangeResult = $revocationExchange.Content | ConvertFrom-Json
        $revokedCapability = $revocationExchangeResult.capability
        $revokedToolVersion = [int64]$revocationExchangeResult.tool_resource_version
        if (-not $revokedCapability -or $revokedToolVersion -le 0) { throw "Tool capability exchange returned no revocation-test capability binding" }

        $matchingGrants = @((Invoke-Kubectl @("get", "toolgrants", "-n", $namespace, "-o", "json") | ConvertFrom-Json).items | Where-Object {
            $_.spec.agentRef.name -eq $agentName -and $_.spec.toolRef.name -eq $toolName
        })
        if ($matchingGrants.Count -lt 1) { throw "RevocationOnly found no ToolGrant to revoke" }
        foreach ($grant in $matchingGrants) {
            Invoke-Kubectl @("delete", "toolgrant/$($grant.metadata.name)", "-n", $namespace, "--wait=true") | Out-Null
        }
        try {
            Set-ActiveToolSession -InstanceName $instanceName -SessionID $sessionID
            $revokedBody = @{ tool_name = $toolName; tool_resource_version = $revokedToolVersion; invocation_id = $revokedInvocationID; input = $invocationInput } | ConvertTo-Json -Compress
            Invoke-ExpectedStatus -Uri "$gatewayURL/v1/tools/invoke" -ExpectedStatus 403 -Authorization "Bearer $revokedCapability" -Body $revokedBody | Out-Null
            Invoke-ExpectedStatus -Uri "$gatewayURL/v1/tools/invoke" -ExpectedStatus 403 -Authorization "Bearer $revokedCapability" -Body $revokedBody | Out-Null
            $revokedAudit = Wait-ForAuditEntry -Description "revoked Tool Gateway trust decision" { $_.eventType -eq 'tool_call' -and $_.workspace -eq $namespace -and $_.details.request.invocationId -eq $revokedInvocationID -and $_.details.outcome -eq 'revoked' -and $_.details.reasonCode -eq 'grant_revoked' }
            if ($revokedAudit.details.runId -ne $DemoRunId -or $revokedAudit.details.execution.status -ne 'blocked' -or $revokedAudit.details.execution.targetReached -ne $false -or $revokedAudit.details.authorization.grant -ne 'revoked') {
                throw "Revoked Tool Gateway trust decision did not prove blocked execution: $($revokedAudit.details | ConvertTo-Json -Compress -Depth 10)"
            }
        }
        finally {
            foreach ($grant in $matchingGrants) {
                Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "ToolGrant"; metadata = @{ name = $grant.metadata.name; namespace = $namespace }; spec = @{ agentRef = @{ name = $grant.spec.agentRef.name }; toolRef = @{ name = $grant.spec.toolRef.name }; verbs = @($grant.spec.verbs) } }
            }
        }
        Write-Host "Tool Gateway blocked the retained run's exact invocation after live ToolGrant revocation." -ForegroundColor Green
        Write-Host "=== PASS ===" -ForegroundColor Green
        return
    }
    $exchangeBody = @{ tool_name = $toolName; invocation_id = $invocationID; input = $invocationInput } | ConvertTo-Json -Compress
    $exchange = Exchange-ToolSessionCapability -InstanceName $instanceName -SessionID $sessionID -Uri "$gatewayURL/v1/tools/capabilities" -Authorization $sessionAuthorization -RefreshAuthorization $mintSessionAuthorization -Body $exchangeBody
    $exchangeResult = $exchange.Content | ConvertFrom-Json
    $toolCapability = $exchangeResult.capability
    $issuedToolVersion = [int64]$exchangeResult.tool_resource_version
    if (-not $toolCapability -or $issuedToolVersion -le 0) { throw "Tool capability exchange returned no executable capability binding" }
    $revokedInvocationID = if ($DemoRunId) { "$DemoRunId`:tool-revoked" } else { "revoked-$invocationID" }
    $revocationExchangeBody = @{ tool_name = $toolName; invocation_id = $revokedInvocationID; input = $invocationInput } | ConvertTo-Json -Compress
    Set-ActiveToolSession -InstanceName $instanceName -SessionID $sessionID
    $invokeBody = @{ tool_name = $toolName; tool_resource_version = $issuedToolVersion; invocation_id = $invocationID; input = $invocationInput } | ConvertTo-Json -Compress
    $targetCountBefore = if ($ProvisionToolTarget) { Get-ProvisionedToolTargetCount } else { $null }
    $pendingInvoke = Invoke-ToolPendingWithActiveSession -InstanceName $instanceName -SessionID $sessionID -Uri "$gatewayURL/v1/tools/invoke" -Authorization "Bearer $toolCapability" -Body $invokeBody
    $pendingResult = $pendingInvoke.Content | ConvertFrom-Json
    if ($pendingResult.invocation_id -ne $invocationID -or $pendingResult.runtime_approval_state -ne "pending" -or -not $pendingResult.runtime_approval -or -not $pendingResult.runtime_approval.name) {
        throw "Tool Gateway 202 response did not identify the pending RuntimeApproval: $($pendingInvoke.Content)"
    }
    $approvalName = $pendingResult.runtime_approval.name
    $pendingApproval = Invoke-Kubectl @("get", "runtimeapproval/$approvalName", "-n", $namespace, "-o", "json") | Out-String | ConvertFrom-Json
    if ($pendingApproval.spec.invocationID -ne $invocationID -or $pendingApproval.status.decisionState -ne "Pending" -or $pendingApproval.status.executionState -ne "NotStarted") {
        throw "Tool Gateway did not persist the expected Pending RuntimeApproval: $($pendingApproval | ConvertTo-Json -Compress -Depth 10)"
    }
    if ($ProvisionToolTarget -and (Get-ProvisionedToolTargetCount) -ne $targetCountBefore) {
        throw "Pending RuntimeApproval reached the Tool target before approval"
    }
    $preApprovalCostRecords = @(Get-CostRecords | Where-Object { $_.workspace -eq $namespace -and $_.operationType -eq 'tool_execution' -and $_.sessionId -eq $sessionID -and $_.toolName -eq $toolName })
    if ($preApprovalCostRecords.Count -ne 0) {
        throw "Pending RuntimeApproval created a Tool execution cost record before approval: $($preApprovalCostRecords | ConvertTo-Json -Compress -Depth 10)"
    }
    if (@(Get-ToolGatewayEvents -InstanceName $instanceName).Count -ne 0) {
        throw "Pending RuntimeApproval emitted an authorization or completion event before approval"
    }
    Approve-RuntimeApproval -Name $approvalName -InvocationID $invocationID
    $invoke = Invoke-ToolWithActiveSession -InstanceName $instanceName -SessionID $sessionID -Uri "$gatewayURL/v1/tools/invoke" -Authorization "Bearer $toolCapability" -Body $invokeBody
    $invokeResult = $invoke.Content | ConvertFrom-Json
    if ($invokeResult.invocation_id -ne $invocationID -or -not $invokeResult.output) { throw "Tool Gateway response did not contain the expected invocation output: $($invoke.Content)" }
    if ($ProvisionToolTarget -and (Get-ProvisionedToolTargetCount) -ne ($targetCountBefore + 1)) {
        throw "Approved RuntimeApproval did not reach the Tool target exactly once"
    }
    $costRecord = Wait-ForCostRecord { $_.workspace -eq $namespace -and $_.operationType -eq 'tool_execution' -and $_.sessionId -eq $sessionID -and $_.toolName -eq $toolName }
    if ($costRecord.agentIdentity -ne $spiffeID -or $costRecord.costUnits -ne 0) {
        throw "Unexpected Tool Gateway cost record: $($costRecord | ConvertTo-Json -Compress)"
    }
    $duplicate = Invoke-ExpectedStatus -Uri "$gatewayURL/v1/tools/invoke" -ExpectedStatus 200 -Authorization "Bearer $toolCapability" -Body $invokeBody
    $duplicateResult = $duplicate.Content | ConvertFrom-Json
    if ($duplicateResult.invocation_id -ne $invocationID -or ($duplicateResult.output | ConvertTo-Json -Compress) -ne ($invokeResult.output | ConvertTo-Json -Compress)) { throw "Tool Gateway replay did not return the stored invocation result: $($duplicate.Content)" }
    $conflictExchange = Exchange-ToolSessionCapability -InstanceName $instanceName -SessionID $sessionID -Uri "$gatewayURL/v1/tools/capabilities" -Authorization $sessionAuthorization -RefreshAuthorization $mintSessionAuthorization -Body $exchangeBody
    $conflictCapability = ($conflictExchange.Content | ConvertFrom-Json).capability
    if (-not $conflictCapability) { throw "Tool capability refresh returned no executable capability binding" }
    $conflictingBody = @{ tool_name = $toolName; tool_resource_version = $issuedToolVersion; invocation_id = $invocationID; input = @{ timezone = "UTC"; includeUnix = $false } } | ConvertTo-Json -Compress
    Invoke-ExpectedStatus -Uri "$gatewayURL/v1/tools/invoke" -ExpectedStatus 403 -Authorization "Bearer $conflictCapability" -Body $conflictingBody | Out-Null

    $deadline = [DateTime]::UtcNow.AddSeconds(30)
    do {
        $events = Get-ToolGatewayEvents -InstanceName $instanceName
        $reasons = @($events.reason)
        if ($reasons -contains "ToolInvocationAuthorized" -and $reasons -contains "ToolInvocationCompleted") { break }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    if ($reasons -notcontains "ToolInvocationAuthorized" -or $reasons -notcontains "ToolInvocationCompleted" -or $events.Count -ne 2) { throw "Tool Gateway replay did not persist exactly one authorization and completion lifecycle" }
    $details = @($events | ForEach-Object { $_.metadata.annotations."kognition.dev/event-details" } | Where-Object { $_ } | ConvertFrom-Json)
    if (@($details | Where-Object invocationId -eq $invocationID).Count -lt 2) { throw "Durable Tool Gateway events do not carry the invocation ID" }
    $authorizedAudit = Wait-ForAuditEntry -Description "authorized Tool Gateway trust decision" { $_.eventType -eq 'tool_call' -and $_.workspace -eq $namespace -and $_.details.request.invocationId -eq $invocationID -and $_.details.outcome -eq 'allowed' -and $_.details.reasonCode -eq 'authorized' }
    $completedAudit = Wait-ForAuditEntry -Description "completed Tool Gateway trust decision" { $_.eventType -eq 'tool_call' -and $_.workspace -eq $namespace -and $_.details.request.invocationId -eq $invocationID -and $_.details.outcome -eq 'completed' -and $_.details.reasonCode -eq 'target_completed' }
    $replayedAudit = Wait-ForAuditEntry -Description "replayed Tool Gateway trust decision" { $_.eventType -eq 'tool_call' -and $_.workspace -eq $namespace -and $_.details.request.invocationId -eq $invocationID -and $_.details.outcome -eq 'replayed' -and $_.details.reasonCode -eq 'replay_detected' }
    if (($DemoRunId -and ($authorizedAudit.details.runId -ne $DemoRunId -or $completedAudit.details.runId -ne $DemoRunId -or $replayedAudit.details.runId -ne $DemoRunId)) -or $authorizedAudit.details.plane -ne 'tool' -or $authorizedAudit.details.execution.status -ne 'not_started' -or $completedAudit.details.execution.status -ne 'completed' -or $completedAudit.details.execution.targetReached -ne $true -or $replayedAudit.details.execution.status -ne 'not_started' -or $authorizedAudit.details.request.targetRef.name -ne $toolName -or $authorizedAudit.details.authorization.grant -ne 'allowed') {
        throw "Tool Gateway trust decisions did not contain the expected normalized evidence. authorized=$($authorizedAudit.details | ConvertTo-Json -Compress -Depth 10) completed=$($completedAudit.details | ConvertTo-Json -Compress -Depth 10) replayed=$($replayedAudit.details | ConvertTo-Json -Compress -Depth 10)"
    }

    # Mint the revocation-test capability just before deleting the ToolGrant so its
    # short TTL (~1m) hasn't elapsed by the time we present it after the delete.
    $revocationExchange = Exchange-ToolSessionCapability -InstanceName $instanceName -SessionID $sessionID -Uri "$gatewayURL/v1/tools/capabilities" -Authorization $sessionAuthorization -RefreshAuthorization $mintSessionAuthorization -Body $revocationExchangeBody
    $revocationExchangeResult = $revocationExchange.Content | ConvertFrom-Json
    $revokedCapability = $revocationExchangeResult.capability
    $revokedToolVersion = [int64]$revocationExchangeResult.tool_resource_version
    if (-not $revokedCapability -or $revokedToolVersion -le 0) { throw "Tool capability exchange returned no revocation-test capability binding" }
    Invoke-Kubectl @("delete", "toolgrant/$grantName", "-n", $namespace, "--wait=true") | Out-Null
    Set-ActiveToolSession -InstanceName $instanceName -SessionID $sessionID
    $revokedBody = @{ tool_name = $toolName; tool_resource_version = $revokedToolVersion; invocation_id = $revokedInvocationID; input = $invocationInput } | ConvertTo-Json -Compress
    Invoke-ExpectedStatus -Uri "$gatewayURL/v1/tools/invoke" -ExpectedStatus 403 -Authorization "Bearer $revokedCapability" -Body $revokedBody | Out-Null
    $revokedAudit = Wait-ForAuditEntry -Description "revoked Tool Gateway trust decision" { $_.eventType -eq 'tool_call' -and $_.workspace -eq $namespace -and $_.details.request.invocationId -eq $revokedInvocationID -and $_.details.outcome -eq 'revoked' -and $_.details.reasonCode -eq 'grant_revoked' }
    if (($DemoRunId -and $revokedAudit.details.runId -ne $DemoRunId) -or $revokedAudit.details.execution.status -ne 'blocked' -or $revokedAudit.details.execution.targetReached -ne $false -or $revokedAudit.details.authorization.grant -ne 'revoked') {
        throw "Revoked Tool Gateway trust decision did not prove blocked execution: $($revokedAudit.details | ConvertTo-Json -Compress -Depth 10)"
    }
    Invoke-ExpectedStatus -Uri "$gatewayURL/v1/tools/capabilities" -ExpectedStatus 403 -Authorization (& $mintSessionAuthorization) -Body $revocationExchangeBody | Out-Null

    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "AgentPolicy"; metadata = @{ name = $policyName; namespace = $namespace }; spec = @{ scope = "agent"; enforce = $true; targetAgents = @($agentName); toolGrants = @(@{ toolRef = $toolName; allow = $true }) } }
    Set-ActiveToolSession -InstanceName $instanceName -SessionID $sessionID
    $fallbackInvocationID = if ($DemoRunId) { "$DemoRunId`:tool-policy-fallback" } else { "policy-fallback-$invocationID" }
    $fallbackExchangeBody = @{ tool_name = $toolName; invocation_id = $fallbackInvocationID; input = $invocationInput } | ConvertTo-Json -Compress
    $fallbackExchange = Exchange-ToolSessionCapability -InstanceName $instanceName -SessionID $sessionID -Uri "$gatewayURL/v1/tools/capabilities" -Authorization $sessionAuthorization -RefreshAuthorization $mintSessionAuthorization -Body $fallbackExchangeBody
    $fallbackExchangeResult = $fallbackExchange.Content | ConvertFrom-Json
    if (-not $fallbackExchangeResult.capability -or [int64]$fallbackExchangeResult.tool_resource_version -le 0) { throw "AgentPolicy fallback did not issue an executable tool capability" }
    Set-ActiveToolSession -InstanceName $instanceName -SessionID $sessionID
    $fallbackBody = @{ tool_name = $toolName; tool_resource_version = [int64]$fallbackExchangeResult.tool_resource_version; invocation_id = $fallbackInvocationID; input = $invocationInput } | ConvertTo-Json -Compress
    $fallbackPending = Invoke-ToolPendingWithActiveSession -InstanceName $instanceName -SessionID $sessionID -Uri "$gatewayURL/v1/tools/invoke" -Authorization "Bearer $($fallbackExchangeResult.capability)" -Body $fallbackBody
    $fallbackPendingResult = $fallbackPending.Content | ConvertFrom-Json
    if ($fallbackPendingResult.invocation_id -ne $fallbackInvocationID -or $fallbackPendingResult.runtime_approval_state -ne "pending" -or -not $fallbackPendingResult.runtime_approval.name) {
        throw "AgentPolicy fallback did not return a pending RuntimeApproval: $($fallbackPending.Content)"
    }
    Approve-RuntimeApproval -Name $fallbackPendingResult.runtime_approval.name -InvocationID $fallbackInvocationID
    $fallbackInvoke = Invoke-ToolWithActiveSession -InstanceName $instanceName -SessionID $sessionID -Uri "$gatewayURL/v1/tools/invoke" -Authorization "Bearer $($fallbackExchangeResult.capability)" -Body $fallbackBody
    if (($fallbackInvoke.Content | ConvertFrom-Json).invocation_id -ne $fallbackInvocationID) { throw "AgentPolicy fallback invocation returned the wrong invocation ID: $($fallbackInvoke.Content)" }
    if ($KeepScopeNamespace) {
        Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "ToolGrant"; metadata = @{ name = $grantName; namespace = $namespace }; spec = @{ agentRef = @{ name = $agentName }; toolRef = @{ name = $toolName }; verbs = @("invoke") } }
    }

    Write-Host "Tool Gateway enforced durable approval retry, native ToolGrant authorization and revocation, then authorized execution through the AgentPolicy compatibility fallback." -ForegroundColor Green
    Write-Host "=== PASS ===" -ForegroundColor Green
}
finally {
    if ($portForward) {
        Stop-Job $portForward -ErrorAction SilentlyContinue
        Remove-Job $portForward -Force -ErrorAction SilentlyContinue
    }
    if ($namespace -and -not $KeepScopeNamespace) {
        & kubectl @kubectlContextArguments delete namespace $namespace --ignore-not-found --wait=false 2>$null | Out-Null
    }
    if ($ProvisionToolTarget -and -not $KeepScopeNamespace) {
        & kubectl @kubectlContextArguments delete deployment,service,configmap $ToolTargetService -n $ToolTargetNamespace --ignore-not-found --wait=false 2>$null | Out-Null
        if ($targetNamespaceCreated) {
            & kubectl @kubectlContextArguments delete namespace $ToolTargetNamespace --ignore-not-found --wait=false 2>$null | Out-Null
        }
    }
}