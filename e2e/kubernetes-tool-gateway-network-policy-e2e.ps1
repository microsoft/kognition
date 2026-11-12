# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

param(
    [string]$KubeContext = "",
    [string]$ControllerNamespace = "kognition-system",
    [string]$ReleaseName = "kognition",
    [string]$ToolTargetNamespace = "tool-targets",
    [string]$ModelTargetNamespace = "model-targets",
    [switch]$KeepResourcesOnFailure
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

. (Join-Path $PSScriptRoot "capability-mint.ps1")

$runID = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
$namespace = "tool-network-e2e-$runID"
$modelClassName = "mock-openai-class-$runID"
$publicHTTPSProbeName = "model-gateway-egress-probe-$runID"
$gatewayPortForward = $null
$succeeded = $false

function Invoke-Kubectl {
    param([Parameter(Mandatory = $true)][string[]]$Arguments)

    $contextArguments = @()
    if ($KubeContext) { $contextArguments = @("--context", $KubeContext) }
    $output = & kubectl @contextArguments @Arguments 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "kubectl $($Arguments -join ' ') failed:`n$($output -join "`n")"
    }
    return $output
}

function Apply-KubernetesObject {
    param([Parameter(Mandatory = $true)][hashtable]$Object)

    $contextArguments = @()
    if ($KubeContext) { $contextArguments = @("--context", $KubeContext) }
    $Object | ConvertTo-Json -Depth 30 -Compress | & kubectl @contextArguments apply -f - 2>&1 | Out-Host
    if ($LASTEXITCODE -ne 0) { throw "apply $($Object.kind)/$($Object.metadata.name) failed" }
}

function Wait-ForJsonPath {
    param([Parameter(Mandatory = $true)][string]$Resource, [Parameter(Mandatory = $true)][string]$JsonPath, [Parameter(Mandatory = $true)][string]$Expected, [int]$TimeoutSeconds = 120)

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        $contextArguments = @()
        if ($KubeContext) { $contextArguments = @("--context", $KubeContext) }
        $value = (& kubectl @contextArguments get $Resource -n $namespace "-o=jsonpath=$JsonPath" 2>$null | Out-String).Trim()
        if ($value -eq $Expected) { return }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "$Resource did not reach $JsonPath=$Expected within $TimeoutSeconds seconds"
}

function Wait-ForAgentInstancePhase {
    param([Parameter(Mandatory = $true)][string]$Name, [Parameter(Mandatory = $true)][string]$Phase, [int]$TimeoutSeconds = 180)

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        $instance = ((Invoke-Kubectl @("get", "agentinstance/$Name", "-n", $namespace, "-o", "json")) -join "`n" | ConvertFrom-Json)
        if ($instance.status.phase -eq $Phase) { return $instance }
        if ($instance.status.phase -eq "Failed") { throw "AgentInstance $Name failed: $($instance.status.conditions | ConvertTo-Json -Compress)" }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "AgentInstance $Name did not reach $Phase within $TimeoutSeconds seconds"
}

function Wait-ForReadyServiceEndpoint {
    param([Parameter(Mandatory = $true)][string]$Service, [Parameter(Mandatory = $true)][string]$ServiceNamespace)

    $deadline = [DateTime]::UtcNow.AddSeconds(60)
    do {
        $endpointSlices = ((Invoke-Kubectl @("get", "endpointslice", "-n", $ServiceNamespace, "-l", "kubernetes.io/service-name=$Service", "-o", "json")) -join "`n" | ConvertFrom-Json).items
        if (@($endpointSlices | ForEach-Object { $_.endpoints } | Where-Object { $_.conditions.ready -eq $true }).Count -gt 0) { return }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Service $Service did not publish a ready EndpointSlice within 60 seconds"
}

function Wait-ForPodSucceeded {
    param([Parameter(Mandatory = $true)][string]$Name, [Parameter(Mandatory = $true)][string]$PodNamespace)

    $deadline = [DateTime]::UtcNow.AddSeconds(60)
    do {
        $pod = ((Invoke-Kubectl @("get", "pod/$Name", "-n", $PodNamespace, "-o", "json")) -join "`n" | ConvertFrom-Json)
        if ($pod.status.phase -eq "Succeeded") { return }
        if ($pod.status.phase -eq "Failed") { throw "Pod $PodNamespace/$Name failed" }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Pod $PodNamespace/$Name did not reach Succeeded within 60 seconds"
}

function Get-AvailableTcpPort {
    $listener = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback, 0)
    try { $listener.Start(); return ([System.Net.IPEndPoint]$listener.LocalEndpoint).Port }
    finally { $listener.Stop() }
}

function Wait-ForHttpStatus {
    param([Parameter(Mandatory = $true)][string]$Uri, [Parameter(Mandatory = $true)][int]$ExpectedStatus)

    $deadline = [DateTime]::UtcNow.AddSeconds(30)
    do {
        try {
            $response = Invoke-WebRequest -Uri $Uri -SkipHttpErrorCheck -TimeoutSec 2
            if ($response.StatusCode -eq $ExpectedStatus) { return }
        } catch {}
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "$Uri did not return $ExpectedStatus within 30 seconds"
}

function Invoke-ExpectedStatus {
    param([Parameter(Mandatory = $true)][string]$Uri, [Parameter(Mandatory = $true)][int]$ExpectedStatus, [Parameter(Mandatory = $true)][string]$Authorization, [Parameter(Mandatory = $true)][string]$Body)

    $response = Invoke-WebRequest -Uri $Uri -Method Post -Headers @{ Authorization = $Authorization } -ContentType "application/json" -Body $Body -SkipHttpErrorCheck -TimeoutSec 45
    if ($response.StatusCode -ne $ExpectedStatus) { throw "POST $Uri returned $($response.StatusCode), want $ExpectedStatus. Body: $($response.Content)" }
    return $response
}

function Assert-PrivateTargetAllowlist {
    $policy = ((Invoke-Kubectl @("get", "networkpolicy/$ReleaseName-tool-gateway", "-n", $ControllerNamespace, "-o", "json")) -join "`n" | ConvertFrom-Json)
    $allowed = $false
    foreach ($rule in @($policy.spec.egress)) {
        $peers = $rule.PSObject.Properties["to"]
        $ports = $rule.PSObject.Properties["ports"]
        if (-not $peers -or -not $ports) { continue }
        foreach ($peer in @($peers.Value)) {
            $namespaceSelector = $peer.PSObject.Properties["namespaceSelector"]
            $podSelector = $peer.PSObject.Properties["podSelector"]
            if ($namespaceSelector -and $podSelector -and $namespaceSelector.Value.matchLabels.'kubernetes.io/metadata.name' -eq $ToolTargetNamespace -and $podSelector.Value.matchLabels.app -eq "allowed-tool" -and @($ports.Value | Where-Object { $_.port -eq 8080 -and $_.protocol -eq "TCP" }).Count -gt 0) {
                $allowed = $true
            }
        }
    }
    if (-not $allowed) { throw "Tool Gateway NetworkPolicy does not allow only the expected $ToolTargetNamespace/allowed-tool:8080 fixture target" }
}

function Assert-ControllerModelTargetAllowlist {
    $policy = ((Invoke-Kubectl @("get", "networkpolicy/$ReleaseName-k8s-controller", "-n", $ControllerNamespace, "-o", "json")) -join "`n" | ConvertFrom-Json)
    $allowed = $false
    foreach ($rule in @($policy.spec.egress)) {
        $peers = $rule.PSObject.Properties["to"]
        $ports = $rule.PSObject.Properties["ports"]
        if (-not $peers -or -not $ports) { continue }
        foreach ($peer in @($peers.Value)) {
            $namespaceSelector = $peer.PSObject.Properties["namespaceSelector"]
            $podSelector = $peer.PSObject.Properties["podSelector"]
            $namespaceLabels = if ($namespaceSelector) { $namespaceSelector.Value.PSObject.Properties["matchLabels"] } else { $null }
            $podLabels = if ($podSelector) { $podSelector.Value.PSObject.Properties["matchLabels"] } else { $null }
            if ($namespaceLabels -and $podLabels -and $namespaceLabels.Value.'kubernetes.io/metadata.name' -eq $ModelTargetNamespace -and $podLabels.Value.app -eq "mock-openai" -and @($ports.Value | Where-Object { $_.port -eq 8080 -and $_.protocol -eq "TCP" }).Count -gt 0) {
                $allowed = $true
            }
        }
    }
    if (-not $allowed) { throw "Kubernetes controller NetworkPolicy does not allow the expected $ModelTargetNamespace/mock-openai:8080 fixture provider" }
}

function Assert-ControllerLeadership {
    $contextArguments = @()
    if ($KubeContext) { $contextArguments = @("--context", $KubeContext) }
    $leaseOutput = & kubectl @contextArguments get lease/kognition-k8s-controller -n $ControllerNamespace -o json 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "Kubernetes controller has not acquired its leader lease; verify Kubernetes Service connectivity from Pods before running this E2E"
    }
    $lease = ($leaseOutput -join "`n" | ConvertFrom-Json)
    $holderIdentity = $lease.spec.holderIdentity
    $renewTime = $lease.spec.renewTime
    $leaseDurationSeconds = [int]$lease.spec.leaseDurationSeconds
    if (-not $holderIdentity -or -not $renewTime -or $leaseDurationSeconds -le 0) {
        throw "Kubernetes controller leader lease is incomplete; verify controller Kubernetes API connectivity before running this E2E"
    }
    $leaseAge = [DateTimeOffset]::UtcNow - [DateTimeOffset]::Parse($renewTime)
    if ($leaseAge.TotalSeconds -gt ($leaseDurationSeconds * 2)) {
        throw "Kubernetes controller leader lease has not been renewed for $([int]$leaseAge.TotalSeconds) seconds; verify Kubernetes Service connectivity from Pods before running this E2E"
    }
}

Write-Host "=== Kognition Tool Gateway NetworkPolicy E2E ===" -ForegroundColor Cyan

try {
    Invoke-Kubectl @("get", "deployment/$ReleaseName-tool-gateway", "-n", $ControllerNamespace) | Out-Null
    Invoke-Kubectl @("wait", "-n", $ControllerNamespace, "--for=condition=Available", "deployment/$ReleaseName-tool-gateway", "--timeout=120s") | Out-Null
    Assert-ControllerLeadership
    Assert-PrivateTargetAllowlist
    Assert-ControllerModelTargetAllowlist
    $targetNamespace = @{ apiVersion = "v1"; kind = "Namespace"; metadata = @{ name = $ToolTargetNamespace } }
    Apply-KubernetesObject $targetNamespace
    $modelTargetNamespaceObject = @{ apiVersion = "v1"; kind = "Namespace"; metadata = @{ name = $ModelTargetNamespace } }
    Apply-KubernetesObject $modelTargetNamespaceObject
    Invoke-Kubectl @("create", "namespace", $namespace) | Out-Null

    $toolServer = @'
import json
from http.server import BaseHTTPRequestHandler, HTTPServer

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/healthz":
            self.send_response(200); self.end_headers(); return
        self.send_error(404)
    def do_POST(self):
        if self.path not in ("/calculator", "/browser"):
            self.send_error(404); return
        size = int(self.headers.get("Content-Length", "0"))
        self.rfile.read(size)
        if self.path == "/browser":
            print("browser invocation", flush=True)
            body = json.dumps({"page": "Kognition"}).encode()
        else:
            print("calculator invocation", flush=True)
            body = json.dumps({"result": 391}).encode()
        self.send_response(200); self.send_header("Content-Type", "application/json"); self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def log_message(self, *_): pass

HTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
'@
    foreach ($target in @(@{ name = "allowed-tool"; app = "allowed-tool" }, @{ name = "blocked-tool"; app = "blocked-tool" })) {
        Apply-KubernetesObject @{ apiVersion = "v1"; kind = "ConfigMap"; metadata = @{ name = "$($target.name)-source"; namespace = $ToolTargetNamespace; labels = @{ "kognition.dev/e2e-run" = "$runID" } }; data = @{ "server.py" = $toolServer } }
        Apply-KubernetesObject @{ apiVersion = "apps/v1"; kind = "Deployment"; metadata = @{ name = $target.name; namespace = $ToolTargetNamespace; labels = @{ "kognition.dev/e2e-run" = "$runID" } }; spec = @{ replicas = 1; selector = @{ matchLabels = @{ app = $target.app } }; template = @{ metadata = @{ labels = @{ app = $target.app; "kognition.dev/e2e-run" = "$runID" } }; spec = @{ containers = @(@{ name = "tool"; image = "python:3.12-alpine"; command = @("python", "/app/server.py"); ports = @(@{ containerPort = 8080 }); readinessProbe = @{ httpGet = @{ path = "/healthz"; port = 8080 }; initialDelaySeconds = 1; periodSeconds = 1 }; volumeMounts = @(@{ name = "source"; mountPath = "/app" }) }); volumes = @(@{ name = "source"; configMap = @{ name = "$($target.name)-source" } }) } } } }
        Apply-KubernetesObject @{ apiVersion = "v1"; kind = "Service"; metadata = @{ name = $target.name; namespace = $ToolTargetNamespace; labels = @{ "kognition.dev/e2e-run" = "$runID" } }; spec = @{ selector = @{ app = $target.app }; ports = @(@{ port = 8080; targetPort = 8080 }) } }
        Invoke-Kubectl @("rollout", "status", "deployment/$($target.name)", "-n", $ToolTargetNamespace, "--timeout=120s") | Out-Null
        Wait-ForReadyServiceEndpoint -Service $target.name -ServiceNamespace $ToolTargetNamespace
    }

    Apply-KubernetesObject @{ apiVersion = "v1"; kind = "ConfigMap"; metadata = @{ name = "mock-openai-source"; namespace = $ModelTargetNamespace; labels = @{ "kognition.dev/e2e-run" = "$runID" } }; data = @{ "server.py" = @'
import json
from http.server import BaseHTTPRequestHandler, HTTPServer
class Handler(BaseHTTPRequestHandler):
    def respond(self, payload):
        body = json.dumps(payload).encode(); self.send_response(200); self.send_header("Content-Type", "application/json"); self.send_header("Content-Length", str(len(body))); self.end_headers(); self.wfile.write(body)
    def do_GET(self):
        if self.path == "/v1/models": self.respond({"object":"list","data":[{"id":"mock-tool-model","object":"model"}]}); return
        self.send_error(404)
    def do_POST(self):
        if self.path != "/v1/chat/completions": self.send_error(404); return
        request = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))) or "{}")
        if any(message.get("role") == "tool" for message in request.get("messages", [])):
            message, finish = {"role":"assistant","content":"The calculator returned 391."}, "stop"
        else:
            message, finish = {"role":"assistant","content":"","tool_calls":[{"id":"call-calculator","type":"function","function":{"name":"sandbox_calculator","arguments":"{\"expression\":\"17 * 23\"}"}}]}, "tool_calls"
        self.respond({"id":"mock","object":"chat.completion","created":0,"model":"mock-tool-model","choices":[{"index":0,"message":message,"finish_reason":finish}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}})
    def log_message(self, *_): pass
HTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
'@ } }
    Apply-KubernetesObject @{ apiVersion = "apps/v1"; kind = "Deployment"; metadata = @{ name = "mock-openai"; namespace = $ModelTargetNamespace; labels = @{ "kognition.dev/e2e-run" = "$runID" } }; spec = @{ replicas = 1; selector = @{ matchLabels = @{ app = "mock-openai" } }; template = @{ metadata = @{ labels = @{ app = "mock-openai"; "kognition.dev/e2e-run" = "$runID" } }; spec = @{ containers = @(@{ name = "mock-openai"; image = "python:3.12-alpine"; command = @("python", "/app/server.py"); ports = @(@{ containerPort = 8080 }); readinessProbe = @{ httpGet = @{ path = "/v1/models"; port = 8080 }; initialDelaySeconds = 1; periodSeconds = 1 }; volumeMounts = @(@{ name = "source"; mountPath = "/app" }) }); volumes = @(@{ name = "source"; configMap = @{ name = "mock-openai-source" } }) } } } }
    Apply-KubernetesObject @{ apiVersion = "v1"; kind = "Service"; metadata = @{ name = "mock-openai"; namespace = $ModelTargetNamespace; labels = @{ "kognition.dev/e2e-run" = "$runID" } }; spec = @{ selector = @{ app = "mock-openai" }; ports = @(@{ port = 443; targetPort = 8080 }) } }
    Invoke-Kubectl @("rollout", "status", "deployment/mock-openai", "-n", $ModelTargetNamespace, "--timeout=120s") | Out-Null
    Wait-ForReadyServiceEndpoint -Service "mock-openai" -ServiceNamespace $ModelTargetNamespace

    $agentName = "network-policy-agent"; $instanceName = "network-policy-instance"; $blockedInstanceName = "network-policy-blocked"; $allowedToolName = "sandbox-calculator"; $browserToolName = "remote-browser"; $shellToolClassName = "stdio-shell-$runID"; $shellToolName = "stdio-shell"; $blockedToolName = "blocked-calculator"
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "SecretRef"; metadata = @{ name = "mock-openai-key"; namespace = $namespace; annotations = @{ "kognition.dev/internal-secret-data" = '{"type":"api-key","apiKey":"mock"}' } }; spec = @{ backendType = "internal"; backendRef = "secret/mock-openai-key"; key = "apiKey" } }
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "ModelClass"; metadata = @{ name = $modelClassName; namespace = $namespace }; spec = @{ provisioner = "openai-compatible" } }
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "Model"; metadata = @{ name = "mock-tool-model"; namespace = $namespace }; spec = @{ className = $modelClassName; provider = "openai-compatible"; modelID = "mock-tool-model"; endpoint = "http://mock-openai.$ModelTargetNamespace.svc.cluster.local:443/v1"; capabilities = @("chat", "tool-calling"); credentialRef = @{ name = "mock-openai-key" } } }
    Wait-ForJsonPath -Resource "modelclass/$modelClassName" -JsonPath "{.status.phase}" -Expected "Ready"
    Wait-ForJsonPath -Resource "model/mock-tool-model" -JsonPath "{.status.phase}" -Expected "Ready"
    foreach ($tool in @(@{ name = $allowedToolName; target = "allowed-tool" }, @{ name = $blockedToolName; target = "blocked-tool" })) {
        Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "Tool"; metadata = @{ name = $tool.name; namespace = $namespace }; spec = @{ className = "sandbox-openapi"; protocol = "OpenAPI"; description = "Network policy E2E target."; endpoint = "http://$($tool.target).$ToolTargetNamespace.svc.cluster.local:8080/calculator"; metadata = @{ allowPrivateNetwork = "true" }; inputSchema = @{ type = "object"; properties = @{ expression = @{ type = "string" } }; required = @("expression") } } }
    }
    Wait-ForJsonPath -Resource "tool/$allowedToolName" -JsonPath "{.status.phase}" -Expected "Ready"
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "Tool"; metadata = @{ name = $browserToolName; namespace = $namespace }; spec = @{ className = "sandbox-openapi"; protocol = "browser"; description = "Remote Browser Tool Gateway target."; endpoint = "http://allowed-tool.$ToolTargetNamespace.svc.cluster.local:8080/browser"; metadata = @{ allowPrivateNetwork = "true" } } }
    Wait-ForJsonPath -Resource "tool/$browserToolName" -JsonPath "{.status.phase}" -Expected "Ready"
    $stdioServer = @'
import json, os, socket, sys
assert os.geteuid() == 65532
connection = socket.socket(); connection.settimeout(2)
assert connection.connect_ex(("1.1.1.1", 443)) != 0, "sandbox network unexpectedly reachable"
for line in sys.stdin:
    request = json.loads(line)
    if request.get("method") == "initialize":
        print(json.dumps({"jsonrpc":"2.0","id":request["id"],"result":{"protocolVersion":"2025-03-26"}}), flush=True)
    elif request.get("method") == "tools/call":
        print(json.dumps({"jsonrpc":"2.0","id":request["id"],"result":{"content":[],"status":"ok"}}), flush=True)
        break
'@
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "ToolClass"; metadata = @{ name = $shellToolClassName }; spec = @{ provisioner = "mcp"; executionTier = "pod"; sandboxImage = "python:3.12-alpine"; stdioCommand = @("/usr/local/bin/python", "-u", "-c", $stdioServer); sandboxDefaults = @{ timeout = "15s"; cpuMilli = 100; memoryMB = 64 }; resourceLimits = @{ maxCpuMilli = 250; maxMemoryMB = 128; maxTimeout = "15s" } } }
    Wait-ForJsonPath -Resource "toolclass/$shellToolClassName" -JsonPath "{.status.phase}" -Expected "Ready"
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "Tool"; metadata = @{ name = $shellToolName; namespace = $namespace }; spec = @{ className = $shellToolClassName; protocol = "shell"; description = "Class-owned MCP stdio Tool Gateway target." } }
    Wait-ForJsonPath -Resource "tool/$shellToolName" -JsonPath "{.status.phase}" -Expected "Ready"
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "Agent"; metadata = @{ name = $agentName; namespace = $namespace }; spec = @{ modelRef = @{ name = "mock-tool-model" }; instructions = @{ inline = "Use sandbox_calculator for arithmetic." }; tools = @(@{ ref = @{ name = $allowedToolName } }) } }
    Wait-ForJsonPath -Resource "agentidentity/agent-identity-$agentName" -JsonPath "{.status.phase}" -Expected "Ready"
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "ModelGrant"; metadata = @{ name = "network-model-grant"; namespace = $namespace }; spec = @{ agentRef = @{ name = $agentName }; modelRef = @{ name = "mock-tool-model" } } }
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "AgentPolicy"; metadata = @{ name = "network-tool-grants"; namespace = $namespace }; spec = @{ scope = "agent"; enforce = $true; targetAgents = @($agentName); toolGrants = @(@{ toolRef = $allowedToolName; allow = $true }, @{ toolRef = $browserToolName; allow = $true }, @{ toolRef = $shellToolName; allow = $true }, @{ toolRef = $blockedToolName; allow = $true }) } }
    Wait-ForJsonPath -Resource "agentpolicy/network-tool-grants" -JsonPath "{.status.phase}" -Expected "Ready"
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "AgentRole"; metadata = @{ name = "tool-invoker"; namespace = $namespace }; rules = @(@{ resources = @("Tool"); verbs = @("get", "invoke") }) }
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "AgentRoleBinding"; metadata = @{ name = "tool-invoker"; namespace = $namespace }; spec = @{ roleRef = @{ name = "tool-invoker" }; subjects = @(@{ kind = "Agent"; name = $agentName }) } }
    Wait-ForReadyServiceEndpoint -Service "agent-$agentName" -ServiceNamespace $namespace
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "AgentInstance"; metadata = @{ name = $instanceName; namespace = $namespace }; spec = @{ agentRef = @{ name = $agentName }; revision = 1; sessionContext = @{ input = "What is 17 * 23? Use the calculator." } } }
    $instance = Wait-ForAgentInstancePhase -Name $instanceName -Phase "Ready"
    if ($instance.status.output -notmatch "391") { throw "Runtime did not receive the allowed Tool Gateway result: $($instance.status.output)" }

    $podName = (Invoke-Kubectl @("get", "pod", "-n", $namespace, "-l", "kognition.dev/agent-name=$agentName", "-o", "jsonpath={.items[0].metadata.name}")) -join ""
    if (-not $podName) { throw "Runtime Pod was not created" }
    $runtimePolicy = (Invoke-Kubectl @("get", "networkpolicy", "-n", $namespace, "-l", "kognition.dev/agent=$agentName", "-o", "name")) -join ""
    if (-not $runtimePolicy) { throw "Runtime Pod is missing its controller-managed NetworkPolicy" }
    $dnsCheck = 'import socket; print(socket.gethostbyname("allowed-tool.' + $ToolTargetNamespace + '.svc.cluster.local"))'
    Invoke-Kubectl @("exec", "-n", $namespace, $podName, "--", "python", "-c", $dnsCheck) | Out-Null
    $directCheck = 'import sys, urllib.request' + "`n" + 'try:' + "`n" + ' urllib.request.urlopen("http://allowed-tool.' + $ToolTargetNamespace + '.svc.cluster.local:8080/calculator", timeout=5)' + "`n" + 'except Exception: sys.exit(0)' + "`n" + 'raise SystemExit("direct runtime-to-Tool request unexpectedly succeeded")'
    Invoke-Kubectl @("exec", "-n", $namespace, $podName, "--", "python", "-c", $directCheck) | Out-Null
    $publicHTTPSCheck = 'import ssl, urllib.request; context = ssl._create_unverified_context(); response = urllib.request.urlopen("https://1.1.1.1/cdn-cgi/trace", context=context, timeout=15); assert response.status == 200; print("HTTPS_STATUS", response.status)'
    Apply-KubernetesObject @{ apiVersion = "v1"; kind = "Pod"; metadata = @{ name = $publicHTTPSProbeName; namespace = $ControllerNamespace; labels = @{ "app.kubernetes.io/component" = "model-gateway"; "app.kubernetes.io/instance" = $ReleaseName } }; spec = @{ restartPolicy = "Never"; containers = @(@{ name = "probe"; image = "python:3.12-alpine"; command = @("python", "-c", $publicHTTPSCheck) }) } }
    Wait-ForPodSucceeded -Name $publicHTTPSProbeName -PodNamespace $ControllerNamespace
    $publicHTTPSResult = (Invoke-Kubectl @("logs", "pod/$publicHTTPSProbeName", "-n", $ControllerNamespace)) -join "`n"
    if ($publicHTTPSResult -notmatch "HTTPS_STATUS 200") { throw "Model Gateway public HTTPS probe did not return HTTP 200: $publicHTTPSResult" }

    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "AgentInstance"; metadata = @{ name = $blockedInstanceName; namespace = $namespace }; spec = @{ agentRef = @{ name = $agentName }; revision = 1 } }
    $blockedInstance = Wait-ForAgentInstancePhase -Name $blockedInstanceName -Phase "Running"
    $gatewayPort = Get-AvailableTcpPort
    $gatewayPortForward = Start-Job -ScriptBlock { param($Context, $Namespace, $Service, $Port) if ($Context) { kubectl --context $Context -n $Namespace port-forward "service/$Service" "${Port}:8092" } else { kubectl -n $Namespace port-forward "service/$Service" "${Port}:8092" } } -ArgumentList $KubeContext, $ControllerNamespace, "$ReleaseName-tool-gateway", $gatewayPort
    $gatewayURL = "http://127.0.0.1:$gatewayPort"
    Wait-ForHttpStatus -Uri "$gatewayURL/healthz" -ExpectedStatus 200
    $keyBase64 = (Invoke-Kubectl @("get", "secret/$ReleaseName-tool-gateway", "-n", $ControllerNamespace, "-o=jsonpath={.data.capability-hmac-key}")) -join ""
    $keyIDBase64 = (Invoke-Kubectl @("get", "secret/$ReleaseName-tool-gateway", "-n", $ControllerNamespace, "-o=jsonpath={.data.capability-hmac-key-id}")) -join ""
    $keyID = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($keyIDBase64))
    $sessionCapability = Invoke-CapabilityMint -KeyBase64 $keyBase64 -FilePath "go" -ArgumentList @("run", "./e2e/mint-tool-session-capability.go", "-key-id", $keyID, "-namespace", $namespace, "-agent", $agentName, "-identity", "agent-identity-$agentName", "-instance", $blockedInstanceName, "-instance-uid", $blockedInstance.metadata.uid, "-session", $blockedInstance.status.sessionID)
    if ([string]::IsNullOrWhiteSpace($sessionCapability)) { throw "Tool Gateway capability minting returned an empty token" }
    $exchangeBody = @{ tool_name = $blockedToolName; invocation_id = "blocked-$runID"; input = @{ expression = "17 * 23" } } | ConvertTo-Json -Compress
    $exchange = Invoke-ExpectedStatus -Uri "$gatewayURL/v1/tools/capabilities" -ExpectedStatus 200 -Authorization "Bearer $sessionCapability" -Body $exchangeBody
    $blockedToolCapability = ($exchange.Content | ConvertFrom-Json).capability
    $blockedToolVersion = (Invoke-Kubectl @("get", "tool/$blockedToolName", "-n", $namespace, "-o=jsonpath={.metadata.resourceVersion}")) -join ""
    $blockedInvoke = @{ tool_name = $blockedToolName; tool_resource_version = [int64]$blockedToolVersion; invocation_id = "blocked-$runID"; input = @{ expression = "17 * 23" } } | ConvertTo-Json -Compress
    Invoke-ExpectedStatus -Uri "$gatewayURL/v1/tools/invoke" -ExpectedStatus 403 -Authorization "Bearer $blockedToolCapability" -Body $blockedInvoke | Out-Null
    $blockedCalls = (Invoke-Kubectl @("logs", "deployment/blocked-tool", "-n", $ToolTargetNamespace)) -join "`n"
    if ($blockedCalls -match "calculator invocation") { throw "Undeclared private Tool received a request from the Tool Gateway" }

    $browserExchangeBody = @{ tool_name = $browserToolName; invocation_id = "browser-$runID"; input = @{ url = "https://1.1.1.1"; action = "navigate" } } | ConvertTo-Json -Compress
    $browserExchange = Invoke-ExpectedStatus -Uri "$gatewayURL/v1/tools/capabilities" -ExpectedStatus 200 -Authorization "Bearer $sessionCapability" -Body $browserExchangeBody
    $browserCapability = ($browserExchange.Content | ConvertFrom-Json).capability
    $browserToolVersion = (Invoke-Kubectl @("get", "tool/$browserToolName", "-n", $namespace, "-o=jsonpath={.metadata.resourceVersion}")) -join ""
    $browserInvoke = @{ tool_name = $browserToolName; tool_resource_version = [int64]$browserToolVersion; invocation_id = "browser-$runID"; input = @{ url = "https://1.1.1.1"; action = "navigate" } } | ConvertTo-Json -Compress
    $browserResponse = Invoke-ExpectedStatus -Uri "$gatewayURL/v1/tools/invoke" -ExpectedStatus 200 -Authorization "Bearer $browserCapability" -Body $browserInvoke
    if (($browserResponse.Content | ConvertFrom-Json).output.page -ne "Kognition") { throw "Remote Browser Tool did not return the expected page response: $($browserResponse.Content)" }
    $privateBrowserExchangeBody = @{ tool_name = $browserToolName; invocation_id = "browser-private-$runID"; input = @{ url = "https://169.254.169.254/latest/meta-data"; action = "navigate" } } | ConvertTo-Json -Compress
    $privateBrowserExchange = Invoke-ExpectedStatus -Uri "$gatewayURL/v1/tools/capabilities" -ExpectedStatus 200 -Authorization "Bearer $sessionCapability" -Body $privateBrowserExchangeBody
    $privateBrowserCapability = ($privateBrowserExchange.Content | ConvertFrom-Json).capability
    $privateBrowserInvoke = @{ tool_name = $browserToolName; tool_resource_version = [int64]$browserToolVersion; invocation_id = "browser-private-$runID"; input = @{ url = "https://169.254.169.254/latest/meta-data"; action = "navigate" } } | ConvertTo-Json -Compress
    $privateBrowserResponse = Invoke-ExpectedStatus -Uri "$gatewayURL/v1/tools/invoke" -ExpectedStatus 403 -Authorization "Bearer $privateBrowserCapability" -Body $privateBrowserInvoke
    if (($privateBrowserResponse.Content | ConvertFrom-Json).error_code -ne "ToolBrowserTargetDenied") { throw "Private Browser navigation returned the wrong error: $($privateBrowserResponse.Content)" }
    $browserCalls = (Invoke-Kubectl @("logs", "deployment/allowed-tool", "-n", $ToolTargetNamespace)) -join "`n"
    if ([regex]::Matches($browserCalls, "browser invocation").Count -ne 1) { throw "Browser service received a denied private navigation target: $browserCalls" }

    $shellExchangeBody = @{ tool_name = $shellToolName; invocation_id = "shell-$runID"; input = @{ command = "status" } } | ConvertTo-Json -Compress
    $shellExchange = Invoke-ExpectedStatus -Uri "$gatewayURL/v1/tools/capabilities" -ExpectedStatus 200 -Authorization "Bearer $sessionCapability" -Body $shellExchangeBody
    $shellCapability = ($shellExchange.Content | ConvertFrom-Json).capability
    $shellToolVersion = (Invoke-Kubectl @("get", "tool/$shellToolName", "-n", $namespace, "-o=jsonpath={.metadata.resourceVersion}")) -join ""
    $shellInvoke = @{ tool_name = $shellToolName; tool_resource_version = [int64]$shellToolVersion; invocation_id = "shell-$runID"; input = @{ command = "status" } } | ConvertTo-Json -Compress
    $shellResponse = Invoke-ExpectedStatus -Uri "$gatewayURL/v1/tools/invoke" -ExpectedStatus 200 -Authorization "Bearer $shellCapability" -Body $shellInvoke
    if (($shellResponse.Content | ConvertFrom-Json).output.status -ne "ok") { throw "Shell Tool Gateway invocation did not return the MCP result: $($shellResponse.Content)" }
    $shellRun = (Invoke-Kubectl @("get", "toolinvocationruns", "-n", $namespace, "-o", "json") | Out-String | ConvertFrom-Json).items | Where-Object { $_.spec.invocationID -eq "shell-$runID" }
    $shellRuns = @($shellRun)
    if ($shellRuns.Count -ne 1) { throw "Gateway did not persist exactly one Shell ToolInvocationRun: $($shellRuns | ConvertTo-Json -Compress)" }
    if ($shellRuns[0].status.phase -ne "Ready" -or $shellRuns[0].status.executionMode -ne "sandbox") { throw "Gateway did not persist a completed Shell ToolInvocationRun: $($shellRuns | ConvertTo-Json -Compress)" }
    $shellJobs = (Invoke-Kubectl @("get", "jobs", "-n", $namespace, "-l", "kognition.dev/managed=stdio-invocation", "-o", "json") | Out-String | ConvertFrom-Json).items
    $shellPolicies = (Invoke-Kubectl @("get", "networkpolicies", "-n", $namespace, "-o", "json") | Out-String | ConvertFrom-Json).items | Where-Object { $_.metadata.PSObject.Properties["generateName"] -and $_.metadata.generateName -like "kognition-stdio-invocation-*" }
    if (@($shellJobs).Count -ne 0 -or @($shellPolicies).Count -ne 0) { throw "Shell Tool sandbox resources were not cleaned up: jobs=$(@($shellJobs).Count) policies=$(@($shellPolicies).Count)" }

    Write-Host "Allowed private OpenAPI, remote Browser, and class-owned Shell Tool execution succeeded through the runtime Tool Gateway path; direct runtime access, private Browser navigation, and Tool Gateway access to an undeclared private endpoint were denied while DNS and public HTTPS remained available." -ForegroundColor Green
    Write-Host "=== PASS ===" -ForegroundColor Green
    $succeeded = $true
}
finally {
    if ($gatewayPortForward) { Stop-Job $gatewayPortForward -ErrorAction SilentlyContinue; Remove-Job $gatewayPortForward -Force -ErrorAction SilentlyContinue }
    if ($publicHTTPSProbeName) { Invoke-Kubectl @("delete", "pod/$publicHTTPSProbeName", "-n", $ControllerNamespace, "--ignore-not-found") | Out-Null }
    if ($succeeded -or -not $KeepResourcesOnFailure) {
        if ($namespace) { Invoke-Kubectl @("delete", "namespace", $namespace, "--ignore-not-found", "--wait=false") | Out-Null }
        if ($modelClassName) { Invoke-Kubectl @("delete", "modelclass/$modelClassName", "--ignore-not-found", "--wait=false") | Out-Null }
        if ($shellToolClassName) { Invoke-Kubectl @("delete", "toolclass/$shellToolClassName", "--ignore-not-found", "--wait=false") | Out-Null }
        if ($ToolTargetNamespace) { Invoke-Kubectl @("delete", "deployment,service,configmap", "-n", $ToolTargetNamespace, "-l", "kognition.dev/e2e-run=$runID", "--ignore-not-found") | Out-Null }
        if ($ModelTargetNamespace) { Invoke-Kubectl @("delete", "deployment,service,configmap", "-n", $ModelTargetNamespace, "-l", "kognition.dev/e2e-run=$runID", "--ignore-not-found") | Out-Null }
    }
}