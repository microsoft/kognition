# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

param(
    [string]$ControllerNamespace = "kognition-system",
    [string]$ReleaseName = "kognition",
    [ValidateRange(0, 600)][int]$PostReplacementObservationSeconds = 0
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

. (Join-Path $PSScriptRoot "capability-mint.ps1")

$gatewayService = "$ReleaseName-tool-gateway"
$controllerDeployment = "$ReleaseName-k8s-controller"
$runID = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
$namespace = "tool-gateway-resilience-e2e-$runID"
$targetService = "resilience-target"
$gatewayPrimaryPortForward = $null
$gatewaySecondaryPortForward = $null
$targetPortForward = $null
$controllerReplicas = $null
$gatewayReplicas = $null

function Invoke-Kubectl {
    param([Parameter(Mandatory = $true)][string[]]$Arguments)

    $output = & kubectl @Arguments 2>&1
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

function Wait-ForHttpStatus {
    param(
        [Parameter(Mandatory = $true)][string]$Uri,
        [Parameter(Mandatory = $true)][int]$ExpectedStatus,
        [int]$TimeoutSeconds = 30
    )

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        try {
            $response = Invoke-WebRequest -Uri $Uri -Method Get -SkipHttpErrorCheck -TimeoutSec 2
            if ($response.StatusCode -eq $ExpectedStatus) { return }
        }
        catch {
        }
        [Threading.Tasks.Task]::Delay(250).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "$Uri did not return $ExpectedStatus within $TimeoutSeconds seconds"
}

function Wait-ForReplacementGatewayPod {
    param(
        [Parameter(Mandatory = $true)][string]$LabelSelector,
        [Parameter(Mandatory = $true)][string[]]$OriginalPodNames,
        [int]$TimeoutSeconds = 120
    )

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        $pods = ((Invoke-Kubectl @("get", "pods", "-n", $ControllerNamespace, "-l", $LabelSelector, "-o", "json")) -join "`n" | ConvertFrom-Json).items
        $replacement = @($pods | Where-Object {
            $_.status.phase -eq "Running" -and
            $OriginalPodNames -notcontains $_.metadata.name -and
            @($_.status.conditions | Where-Object { $_.type -eq "Ready" -and $_.status -eq "True" }).Count -gt 0
        }) | Select-Object -First 1
        if ($replacement) { return $replacement }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)

    throw "Tool Gateway did not create a Ready replacement Pod within $TimeoutSeconds seconds"
}

function Set-ActiveToolSession {
    param(
        [Parameter(Mandatory = $true)][string]$InstanceName,
        [Parameter(Mandatory = $true)][string]$SessionID
    )

    Invoke-Kubectl @("patch", "agentinstance/$InstanceName", "-n", $namespace, "--subresource=status", "--type=merge", "-p", ("{`"status`":{`"phase`":`"Running`",`"sessionID`":`"$SessionID`"}}")) | Out-Null
}

function Exchange-ToolSessionCapability {
    param(
        [Parameter(Mandatory = $true)][string]$InstanceName,
        [Parameter(Mandatory = $true)][string]$SessionID,
        [Parameter(Mandatory = $true)][string]$Uri,
        [Parameter(Mandatory = $true)][string]$Authorization,
        [Parameter(Mandatory = $true)][string]$Body
    )

    $deadline = [DateTime]::UtcNow.AddSeconds(20)
    do {
        Set-ActiveToolSession -InstanceName $InstanceName -SessionID $SessionID
        $response = Invoke-WebRequest -Uri $Uri -Method Post -Headers @{ Authorization = $Authorization } -ContentType "application/json" -Body $Body -SkipHttpErrorCheck
        if ($response.StatusCode -eq 200) { return $response }
        if ($response.StatusCode -ne 403) { throw "POST $Uri returned $($response.StatusCode), want 200. Body: $($response.Content)" }
        [Threading.Tasks.Task]::Delay(250).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Tool Gateway session capability exchange did not observe the active session within 20 seconds"
}

function Get-ToolVersion {
    param([Parameter(Mandatory = $true)][string]$ToolName)

    return [int64](((& kubectl get "tool/$ToolName" -n $namespace -o jsonpath='{.metadata.resourceVersion}' | Out-String).Trim()))
}

function Get-ToolCapability {
    param(
        [Parameter(Mandatory = $true)][string]$ToolName,
        [Parameter(Mandatory = $true)][string]$InvocationID,
        [Parameter(Mandatory = $true)][string]$GatewayURL,
        [Parameter(Mandatory = $true)][string]$SessionCapability,
        [Parameter(Mandatory = $true)][string]$InstanceName,
        [Parameter(Mandatory = $true)][string]$SessionID
    )

    $exchangeBody = @{ tool_name = $ToolName; invocation_id = $InvocationID; input = @{} } | ConvertTo-Json -Compress
    $exchange = Exchange-ToolSessionCapability -InstanceName $InstanceName -SessionID $SessionID -Uri "$GatewayURL/v1/tools/capabilities" -Authorization "Bearer $SessionCapability" -Body $exchangeBody
    $capability = ($exchange.Content | ConvertFrom-Json).capability
    if (-not $capability) { throw "Tool capability exchange returned no capability for $ToolName" }
    return $capability
}

function Invoke-ToolRequest {
    param(
        [Parameter(Mandatory = $true)][string]$ToolName,
        [Parameter(Mandatory = $true)][string]$InvocationID,
        [Parameter(Mandatory = $true)][string]$GatewayURL,
        [Parameter(Mandatory = $true)][string]$Capability
    )

    $body = @{ tool_name = $ToolName; tool_resource_version = (Get-ToolVersion -ToolName $ToolName); invocation_id = $InvocationID; input = @{} } | ConvertTo-Json -Compress
    return Invoke-WebRequest -Uri "$GatewayURL/v1/tools/invoke" -Method Post -Headers @{ Authorization = "Bearer $Capability" } -ContentType "application/json" -Body $body -SkipHttpErrorCheck
}

function Invoke-ToolExpectStatus {
    param(
        [Parameter(Mandatory = $true)][string]$ToolName,
        [Parameter(Mandatory = $true)][string]$InvocationID,
        [Parameter(Mandatory = $true)][int]$ExpectedStatus,
        [Parameter(Mandatory = $true)][string]$GatewayURL,
        [Parameter(Mandatory = $true)][string]$SessionCapability,
        [Parameter(Mandatory = $true)][string]$InstanceName,
        [Parameter(Mandatory = $true)][string]$SessionID
    )

    $capability = Get-ToolCapability -ToolName $ToolName -InvocationID $InvocationID -GatewayURL $GatewayURL -SessionCapability $SessionCapability -InstanceName $InstanceName -SessionID $SessionID
    $response = Invoke-ToolRequest -ToolName $ToolName -InvocationID $InvocationID -GatewayURL $GatewayURL -Capability $capability
    if ($response.StatusCode -ne $ExpectedStatus) {
        throw "Tool $ToolName invocation returned $($response.StatusCode), want $ExpectedStatus. Body: $($response.Content)"
    }
    return $response
}

function Get-TargetStats {
    param([Parameter(Mandatory = $true)][string]$TargetURL)

    return (Invoke-WebRequest -Uri "$TargetURL/stats" -Method Get -TimeoutSec 2).Content | ConvertFrom-Json
}

function Get-Receipt {
    param(
        [Parameter(Mandatory = $true)][string]$InvocationID,
        [Parameter(Mandatory = $true)][string]$ControllerNamespace
    )

    $configMaps = ((Invoke-Kubectl @("get", "configmap", "-n", $ControllerNamespace, "-l", "kognition.dev/tool-invocation-ledger=true", "-o", "json")) -join "`n" | ConvertFrom-Json).items
    $configMap = @($configMaps | Where-Object { $_.metadata.annotations."kognition.dev/workload-namespace" -eq $namespace }) | Select-Object -First 1
    if (-not $configMap) { throw "Tool Gateway receipt ConfigMap was not created for $namespace" }
    $ledger = $configMap.data."receipts.json" | ConvertFrom-Json
    $receipt = $ledger.receipts.PSObject.Properties[$InvocationID].Value
    if (-not $receipt) { throw "Tool Gateway receipt $InvocationID was not persisted" }
    return $receipt
}

function Get-MetricCounter {
    param(
        [Parameter(Mandatory = $true)][string]$Metrics,
        [Parameter(Mandatory = $true)][string]$Metric
    )

    $line = @($Metrics -split "`n" | Where-Object { $_ -match "^$([regex]::Escape($Metric))\s+" }) | Select-Object -First 1
    if (-not $line) { return [double]0 }
    return [double](($line -split '\s+')[-1])
}

function Get-MetricCounterSum {
    param(
        [Parameter(Mandatory = $true)][string[]]$Metrics,
        [Parameter(Mandatory = $true)][string]$Metric
    )

    return [double]($Metrics | ForEach-Object { Get-MetricCounter -Metrics $_ -Metric $Metric } | Measure-Object -Sum).Sum
}

function Assert-CounterIncreased {
    param(
        [Parameter(Mandatory = $true)][double]$Before,
        [Parameter(Mandatory = $true)][double]$After,
        [Parameter(Mandatory = $true)][string]$Name
    )

    if ($After -le $Before) { throw "$Name did not increase: before=$Before after=$After" }
}

Write-Host "=== Kognition Tool Gateway Resilience E2E ===" -ForegroundColor Cyan

try {
    Invoke-Kubectl @("wait", "-n", $ControllerNamespace, "--for=condition=Available", "deployment/$gatewayService", "--timeout=120s") | Out-Null
    Invoke-Kubectl @("get", "deployment/$controllerDeployment", "-n", $ControllerNamespace) | Out-Null
    $gatewayReplicas = (& kubectl get "deployment/$gatewayService" -n $ControllerNamespace -o jsonpath='{.spec.replicas}' | Out-String).Trim()
    if (-not $gatewayReplicas) { $gatewayReplicas = "1" }
    Invoke-Kubectl @("scale", "deployment/$gatewayService", "-n", $ControllerNamespace, "--replicas=2") | Out-Null
    Invoke-Kubectl @("rollout", "status", "deployment/$gatewayService", "-n", $ControllerNamespace, "--timeout=120s") | Out-Null
    $gatewayLabelSelector = "app.kubernetes.io/component=tool-gateway,app.kubernetes.io/instance=$ReleaseName"
    $gatewayPods = @(((& kubectl get pods -n $ControllerNamespace -l $gatewayLabelSelector -o json | ConvertFrom-Json).items | Where-Object { $_.status.phase -eq "Running" } | Sort-Object { $_.metadata.name }))
    if ($gatewayPods.Count -ne 2) { throw "Tool Gateway replica setup found $($gatewayPods.Count) running Pods, want 2" }
    $originalGatewayPodNames = @($gatewayPods | ForEach-Object { $_.metadata.name })
    Invoke-Kubectl @("create", "namespace", $namespace) | Out-Null

    Apply-KubernetesObject @{ apiVersion = "v1"; kind = "ConfigMap"; metadata = @{ name = "resilience-target-source"; namespace = $namespace }; data = @{ "server.py" = @'
import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

counts = {"flaky": 0, "nonidempotent": 0, "slow": 0, "hold": 0, "circuit": 0}
lock = threading.Lock()

class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def respond(self, status, payload):
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        try:
            self.wfile.write(body)
        except BrokenPipeError:
            pass

    def count(self, name):
        with lock:
            counts[name] += 1
            return counts[name]

    def do_GET(self):
        if self.path in ("/health", "/stats"):
            with lock:
                return self.respond(200, {"ok": True, "counts": counts})
        return self.respond(200, {"ok": True})

    def do_POST(self):
        if self.path == "/flaky":
            attempt = self.count("flaky")
            return self.respond(503 if attempt == 1 else 200, {"attempt": attempt})
        if self.path == "/nonidempotent":
            attempt = self.count("nonidempotent")
            return self.respond(503, {"attempt": attempt})
        if self.path == "/slow":
            attempt = self.count("slow")
            time.sleep(2)
            return self.respond(200, {"attempt": attempt})
        if self.path == "/hold":
            attempt = self.count("hold")
            time.sleep(2)
            return self.respond(200, {"attempt": attempt})
        if self.path == "/circuit":
            attempt = self.count("circuit")
            return self.respond(503 if attempt == 1 else 200, {"attempt": attempt})
        return self.respond(404, {"error": "not found"})

ThreadingHTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
'@ } }
    Apply-KubernetesObject @{ apiVersion = "apps/v1"; kind = "Deployment"; metadata = @{ name = $targetService; namespace = $namespace }; spec = @{ replicas = 1; selector = @{ matchLabels = @{ app = $targetService } }; template = @{ metadata = @{ labels = @{ app = $targetService } }; spec = @{ containers = @(@{ name = $targetService; image = "python:3.12-alpine"; command = @("python", "/app/server.py"); ports = @(@{ containerPort = 8080 }); volumeMounts = @(@{ name = "source"; mountPath = "/app" }) }); volumes = @(@{ name = "source"; configMap = @{ name = "resilience-target-source" } }) } } } }
    Apply-KubernetesObject @{ apiVersion = "v1"; kind = "Service"; metadata = @{ name = $targetService; namespace = $namespace }; spec = @{ selector = @{ app = $targetService }; ports = @(@{ port = 8080; targetPort = 8080 }) } }
    Invoke-Kubectl @("rollout", "status", "deployment/$targetService", "-n", $namespace, "--timeout=120s") | Out-Null

    $gatewayPrimaryPort = Get-AvailableTcpPort
    $gatewaySecondaryPort = Get-AvailableTcpPort
    $targetPort = Get-AvailableTcpPort
    $gatewayPrimaryPortForward = Start-Job -ScriptBlock { param($Namespace, $Pod, $Port) kubectl -n $Namespace port-forward "pod/$Pod" "${Port}:8092" } -ArgumentList $ControllerNamespace, $gatewayPods[0].metadata.name, $gatewayPrimaryPort
    $gatewaySecondaryPortForward = Start-Job -ScriptBlock { param($Namespace, $Pod, $Port) kubectl -n $Namespace port-forward "pod/$Pod" "${Port}:8092" } -ArgumentList $ControllerNamespace, $gatewayPods[1].metadata.name, $gatewaySecondaryPort
    $targetPortForward = Start-Job -ScriptBlock { param($Namespace, $Service, $Port) kubectl -n $Namespace port-forward "service/$Service" "${Port}:8080" } -ArgumentList $namespace, $targetService, $targetPort
    $gatewayURL = "http://127.0.0.1:$gatewayPrimaryPort"
    $gatewaySecondaryURL = "http://127.0.0.1:$gatewaySecondaryPort"
    $targetURL = "http://127.0.0.1:$targetPort"
    Wait-ForHttpStatus -Uri "$gatewayURL/healthz" -ExpectedStatus 200
    Wait-ForHttpStatus -Uri "$gatewaySecondaryURL/healthz" -ExpectedStatus 200
    Wait-ForHttpStatus -Uri "$targetURL/health" -ExpectedStatus 200

    $agentName = "resilience-agent"
    $identityName = "agent-identity-$agentName"
    $instanceName = "resilience-instance"
    $policyName = "resilience-grants"
    $sessionID = "resilience-session-$runID"
    $toolDefinitions = @(
        @{ name = "resilience-retry"; path = "flaky"; policy = @{ idempotent = $true; retryMaxAttempts = 2 } },
        @{ name = "resilience-nonidempotent"; path = "nonidempotent"; policy = @{} },
        @{ name = "resilience-timeout"; path = "slow"; policy = @{ timeout = "200ms" } },
        @{ name = "resilience-concurrency"; path = "hold"; policy = @{ maxConcurrent = 1 } },
        @{ name = "resilience-circuit"; path = "circuit"; policy = @{ circuitBreaker = @{ failureThreshold = 1; cooldown = "5s" } } }
    )

    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "Agent"; metadata = @{ name = $agentName; namespace = $namespace }; spec = @{ modelRef = @{ name = "placeholder" }; instructions = @{ inline = "Tool Gateway resilience E2E fixture." }; replicas = 0 } }
    Wait-ForJsonPath -Resource "agentidentity/$identityName" -JsonPath "{.status.phase}" -Expected "Ready"
    foreach ($definition in $toolDefinitions) {
        Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "Tool"; metadata = @{ name = $definition.name; namespace = $namespace }; spec = @{ className = "sandbox-openapi"; protocol = "OpenAPI"; description = "Tool Gateway resilience E2E target."; endpoint = "http://$targetService.$namespace.svc.cluster.local:8080/$($definition.path)"; metadata = @{ allowPrivateNetwork = "true" }; inputSchema = @{ type = "object" }; executionPolicy = $definition.policy } }
        Wait-ForJsonPath -Resource "tool/$($definition.name)" -JsonPath "{.status.phase}" -Expected "Ready"
    }
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "AgentPolicy"; metadata = @{ name = $policyName; namespace = $namespace }; spec = @{ scope = "agent"; enforce = $true; targetAgents = @($agentName); toolGrants = @($toolDefinitions | ForEach-Object { @{ toolRef = $_.name; allow = $true } }) } }
    Wait-ForJsonPath -Resource "agentpolicy/$policyName" -JsonPath "{.status.phase}" -Expected "Ready"
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "AgentRole"; metadata = @{ name = "tool-invoker"; namespace = $namespace }; rules = @(@{ resources = @("Tool"); verbs = @("get", "invoke") }) }
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "AgentRoleBinding"; metadata = @{ name = "tool-invoker"; namespace = $namespace }; spec = @{ roleRef = @{ name = "tool-invoker" }; subjects = @(@{ kind = "Agent"; name = $agentName }) } }
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "AgentInstance"; metadata = @{ name = $instanceName; namespace = $namespace }; spec = @{ agentRef = @{ name = $agentName }; revision = 1 } }

    $controllerReplicas = (& kubectl get "deployment/$controllerDeployment" -n $ControllerNamespace -o jsonpath='{.spec.replicas}' | Out-String).Trim()
    if (-not $controllerReplicas) { $controllerReplicas = "1" }
    Invoke-Kubectl @("scale", "deployment/$controllerDeployment", "-n", $ControllerNamespace, "--replicas=0") | Out-Null
    Invoke-Kubectl @("rollout", "status", "deployment/$controllerDeployment", "-n", $ControllerNamespace, "--timeout=120s") | Out-Null
    $instanceUID = (& kubectl get "agentinstance/$instanceName" -n $namespace -o jsonpath='{.metadata.uid}' | Out-String).Trim()
    $keyBase64 = (& kubectl get secret "$ReleaseName-tool-gateway" -n $ControllerNamespace '-o=jsonpath={.data.capability-hmac-key}' | Out-String).Trim()
    $keyIDBase64 = (& kubectl get secret "$ReleaseName-tool-gateway" -n $ControllerNamespace '-o=jsonpath={.data.capability-hmac-key-id}' | Out-String).Trim()
    if (-not $instanceUID -or -not $keyBase64 -or -not $keyIDBase64) { throw "Tool Gateway fixture is missing instance identity or active capability key material" }
    $keyID = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($keyIDBase64))
    $sessionCapability = Invoke-CapabilityMint -KeyBase64 $keyBase64 -FilePath "go" -ArgumentList @("run", "./e2e/mint-tool-session-capability.go", "-key-id", $keyID, "-namespace", $namespace, "-agent", $agentName, "-identity", $identityName, "-instance", $instanceName, "-instance-uid", $instanceUID, "-session", $sessionID)
    if (-not $sessionCapability) { throw "Tool session capability helper returned an empty token" }

    $primaryMetricsBefore = (Invoke-WebRequest -Uri "$gatewayURL/metrics" -Method Get).Content
    $secondaryMetricsBefore = (Invoke-WebRequest -Uri "$gatewaySecondaryURL/metrics" -Method Get).Content
    $concurrencyBefore = Get-MetricCounter -Metrics $secondaryMetricsBefore -Metric 'kognition_tool_gateway_resilience_events_total{reason="concurrency_limit"}'
    $circuitBefore = Get-MetricCounter -Metrics $secondaryMetricsBefore -Metric 'kognition_tool_gateway_resilience_events_total{reason="circuit_open"}'
    $transientBefore = Get-MetricCounter -Metrics $primaryMetricsBefore -Metric 'kognition_tool_gateway_resilience_events_total{reason="transient_failure"}'
    $successBefore = Get-MetricCounter -Metrics $secondaryMetricsBefore -Metric 'kognition_tool_gateway_invocations_total{outcome="success"}'

    Invoke-ToolExpectStatus -ToolName "resilience-retry" -InvocationID "retry-$runID" -ExpectedStatus 200 -GatewayURL $gatewayURL -SessionCapability $sessionCapability -InstanceName $instanceName -SessionID $sessionID | Out-Null
    $stats = Get-TargetStats -TargetURL $targetURL
    if ($stats.counts.flaky -ne 2) { throw "Idempotent transient Tool was called $($stats.counts.flaky) times, want 2" }

    $nonIdempotentInvocation = "nonidempotent-$runID"
    Invoke-ToolExpectStatus -ToolName "resilience-nonidempotent" -InvocationID $nonIdempotentInvocation -ExpectedStatus 403 -GatewayURL $gatewayURL -SessionCapability $sessionCapability -InstanceName $instanceName -SessionID $sessionID | Out-Null
    $stats = Get-TargetStats -TargetURL $targetURL
    if ($stats.counts.nonidempotent -ne 1) { throw "Non-idempotent transient Tool was called $($stats.counts.nonidempotent) times, want 1" }

    $timeoutInvocation = "timeout-$runID"
    $timeoutCapability = Get-ToolCapability -ToolName "resilience-timeout" -InvocationID $timeoutInvocation -GatewayURL $gatewayURL -SessionCapability $sessionCapability -InstanceName $instanceName -SessionID $sessionID
    $timer = [Diagnostics.Stopwatch]::StartNew()
    $timeoutResponse = Invoke-ToolRequest -ToolName "resilience-timeout" -InvocationID $timeoutInvocation -GatewayURL $gatewayURL -Capability $timeoutCapability
    $timer.Stop()
    if ($timeoutResponse.StatusCode -ne 403) { throw "Tool resilience-timeout invocation returned $($timeoutResponse.StatusCode), want 403. Body: $($timeoutResponse.Content)" }
    if ($timer.Elapsed -gt [TimeSpan]::FromSeconds(1)) { throw "Gateway timeout returned after $($timer.Elapsed), want under 1 second" }
    $timeoutReceipt = Get-Receipt -InvocationID $timeoutInvocation -ControllerNamespace $ControllerNamespace
    if ($timeoutReceipt.status -ne "Failed" -or $timeoutReceipt.errorMessage -notmatch "ToolGatewayTimeout") { throw "Timeout receipt did not preserve the gateway timeout failure: $($timeoutReceipt | ConvertTo-Json -Compress)" }

    $holdInvocation = "hold-$runID"
    $holdCapability = Get-ToolCapability -ToolName "resilience-concurrency" -InvocationID $holdInvocation -GatewayURL $gatewayURL -SessionCapability $sessionCapability -InstanceName $instanceName -SessionID $sessionID
    $holdBody = @{ tool_name = "resilience-concurrency"; tool_resource_version = (Get-ToolVersion -ToolName "resilience-concurrency"); invocation_id = $holdInvocation; input = @{} } | ConvertTo-Json -Compress
    $holdRequest = Start-Job -ScriptBlock { param($Uri, $Capability, $Body) Invoke-WebRequest -Uri $Uri -Method Post -Headers @{ Authorization = "Bearer $Capability" } -ContentType "application/json" -Body $Body -SkipHttpErrorCheck } -ArgumentList "$gatewayURL/v1/tools/invoke", $holdCapability, $holdBody
    [Threading.Tasks.Task]::Delay(300).GetAwaiter().GetResult()
    Invoke-ToolExpectStatus -ToolName "resilience-concurrency" -InvocationID "concurrency-$runID" -ExpectedStatus 403 -GatewayURL $gatewaySecondaryURL -SessionCapability $sessionCapability -InstanceName $instanceName -SessionID $sessionID | Out-Null
    $holdResult = Receive-Job -Job $holdRequest -Wait
    Remove-Job -Job $holdRequest -Force
    if ($holdResult.StatusCode -ne 200) { throw "First concurrent Tool invocation returned $($holdResult.StatusCode), want 200" }
    $stats = Get-TargetStats -TargetURL $targetURL
    if ($stats.counts.hold -ne 1) { throw "Concurrency-limited Tool reached its target $($stats.counts.hold) times, want 1" }

    Invoke-ToolExpectStatus -ToolName "resilience-circuit" -InvocationID "circuit-open-$runID" -ExpectedStatus 403 -GatewayURL $gatewayURL -SessionCapability $sessionCapability -InstanceName $instanceName -SessionID $sessionID | Out-Null
    $stats = Get-TargetStats -TargetURL $targetURL
    if ($stats.counts.circuit -ne 1) { throw "Circuit failure target count = $($stats.counts.circuit), want 1" }
    $primaryMetricsAfterCircuit = (Invoke-WebRequest -Uri "$gatewayURL/metrics" -Method Get).Content
    Assert-CounterIncreased -Before $transientBefore -After (Get-MetricCounter -Metrics $primaryMetricsAfterCircuit -Metric 'kognition_tool_gateway_resilience_events_total{reason="transient_failure"}') -Name "primary transient failure metric"
    Invoke-Kubectl @("delete", "pod", $gatewayPods[0].metadata.name, "-n", $ControllerNamespace, "--wait=true") | Out-Null
    $replacementGatewayPod = Wait-ForReplacementGatewayPod -LabelSelector $gatewayLabelSelector -OriginalPodNames $originalGatewayPodNames
    Wait-ForHttpStatus -Uri "$gatewaySecondaryURL/healthz" -ExpectedStatus 200
    Invoke-ToolExpectStatus -ToolName "resilience-circuit" -InvocationID "circuit-blocked-$runID" -ExpectedStatus 403 -GatewayURL $gatewaySecondaryURL -SessionCapability $sessionCapability -InstanceName $instanceName -SessionID $sessionID | Out-Null
    $stats = Get-TargetStats -TargetURL $targetURL
    if ($stats.counts.circuit -ne 1) { throw "Open circuit called its target $($stats.counts.circuit) times, want 1" }
    Invoke-ToolExpectStatus -ToolName "resilience-timeout" -InvocationID "post-replacement-timeout-$runID" -ExpectedStatus 403 -GatewayURL $gatewaySecondaryURL -SessionCapability $sessionCapability -InstanceName $instanceName -SessionID $sessionID | Out-Null
    [Threading.Tasks.Task]::Delay(5200).GetAwaiter().GetResult()
    Invoke-ToolExpectStatus -ToolName "resilience-circuit" -InvocationID "circuit-recovered-$runID" -ExpectedStatus 200 -GatewayURL $gatewaySecondaryURL -SessionCapability $sessionCapability -InstanceName $instanceName -SessionID $sessionID | Out-Null
    $stats = Get-TargetStats -TargetURL $targetURL
    if ($stats.counts.circuit -ne 2) { throw "Recovered circuit target count = $($stats.counts.circuit), want 2" }

    $secondaryMetricsAfter = (Invoke-WebRequest -Uri "$gatewaySecondaryURL/metrics" -Method Get).Content
    Assert-CounterIncreased -Before $concurrencyBefore -After (Get-MetricCounter -Metrics $secondaryMetricsAfter -Metric 'kognition_tool_gateway_resilience_events_total{reason="concurrency_limit"}') -Name "cross-replica concurrency limit metric"
    Assert-CounterIncreased -Before $circuitBefore -After (Get-MetricCounter -Metrics $secondaryMetricsAfter -Metric 'kognition_tool_gateway_resilience_events_total{reason="circuit_open"}') -Name "post-replacement circuit-open metric"
    Assert-CounterIncreased -Before $successBefore -After (Get-MetricCounter -Metrics $secondaryMetricsAfter -Metric 'kognition_tool_gateway_invocations_total{outcome="success"}') -Name "post-replacement successful invocation metric"

    if ($PostReplacementObservationSeconds -gt 0) {
        Write-Host "Holding replacement state for $PostReplacementObservationSeconds seconds so Prometheus can scrape resilience metrics." -ForegroundColor DarkGray
        [Threading.Tasks.Task]::Delay($PostReplacementObservationSeconds * 1000).GetAwaiter().GetResult()
    }

    Write-Host "Two Tool Gateway replicas enforced timeout, idempotent retry, no-retry, shared concurrency, circuit persistence after Pod replacement ($($replacementGatewayPod.metadata.name)), durable receipts, and resilience metrics." -ForegroundColor Green
    Write-Host "=== PASS ===" -ForegroundColor Green
}
finally {
    if ($gatewayPrimaryPortForward) {
        Stop-Job $gatewayPrimaryPortForward -ErrorAction SilentlyContinue
        Remove-Job $gatewayPrimaryPortForward -Force -ErrorAction SilentlyContinue
    }
    if ($gatewaySecondaryPortForward) {
        Stop-Job $gatewaySecondaryPortForward -ErrorAction SilentlyContinue
        Remove-Job $gatewaySecondaryPortForward -Force -ErrorAction SilentlyContinue
    }
    if ($targetPortForward) {
        Stop-Job $targetPortForward -ErrorAction SilentlyContinue
        Remove-Job $targetPortForward -Force -ErrorAction SilentlyContinue
    }
    if ($null -ne $controllerReplicas) {
        & kubectl scale "deployment/$controllerDeployment" -n $ControllerNamespace "--replicas=$controllerReplicas" 2>$null | Out-Null
        & kubectl wait -n $ControllerNamespace --for=condition=Available "deployment/$controllerDeployment" --timeout=120s 2>$null | Out-Null
    }
    if ($null -ne $gatewayReplicas) {
        & kubectl scale "deployment/$gatewayService" -n $ControllerNamespace "--replicas=$gatewayReplicas" 2>$null | Out-Null
        & kubectl rollout status "deployment/$gatewayService" -n $ControllerNamespace --timeout=120s 2>$null | Out-Null
    }
    if ($namespace) {
        & kubectl delete namespace $namespace --ignore-not-found --wait=false 2>$null | Out-Null
    }
}