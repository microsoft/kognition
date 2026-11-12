# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

param(
    [string]$ControllerNamespace = "kognition-system",
    [string]$ReleaseName = "kognition",
    [string]$ProviderNamespace = "model-gateway-e2e"
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

. (Join-Path $PSScriptRoot "capability-mint.ps1")

$runID = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds()
$namespace = if ($ProviderNamespace -eq "model-gateway-e2e") { "$ProviderNamespace-$runID" } else { $ProviderNamespace }
$modelClassName = "mock-openai-class-$runID"
$gatewayService = "$ReleaseName-model-gateway"
$controllerDeployment = "$ReleaseName-k8s-controller"
$providerEgressPolicy = "model-gateway-e2e-provider-$runID"
$portForward = $null
$controllerReplicas = $null
$denyProbe = $null
$createdNamespace = $false
$denyProbePolicy = $null
$controllerLock = "model-gateway-e2e-controller-lock"
$controllerLockHolder = "model-gateway-e2e-$runID"
$controllerLockAcquired = $false
$testPassed = $false
$capabilityHelper = Join-Path ([IO.Path]::GetTempPath()) "kognition-mint-model-session-$runID.exe"

function Invoke-Kubectl {
    param([Parameter(Mandatory = $true)][string[]]$Arguments)
    $output = & kubectl @Arguments 2>&1
    if ($LASTEXITCODE -ne 0) { throw "kubectl $($Arguments -join ' ') failed:`n$($output -join "`n")" }
    return $output
}

function Apply-KubernetesObject {
    param([Parameter(Mandatory = $true)][hashtable]$Object)
    $Object | ConvertTo-Json -Depth 30 -Compress | & kubectl apply -f - 2>&1 | Out-Host
    if ($LASTEXITCODE -ne 0) { throw "apply $($Object.kind)/$($Object.metadata.name) failed" }
}

function Wait-ForJsonPath {
    param([Parameter(Mandatory = $true)][string]$Resource, [Parameter(Mandatory = $true)][string]$JsonPath, [Parameter(Mandatory = $true)][string]$Expected, [int]$TimeoutSeconds = 150)
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
    try { $listener.Start(); return ([System.Net.IPEndPoint]$listener.LocalEndpoint).Port } finally { $listener.Stop() }
}

function Get-CostRecords {
    $configMaps = (Invoke-Kubectl @("get", "configmaps", "-n", $ControllerNamespace, "-l", "kognition.dev/cost-record=true", "-o", "json") | Out-String | ConvertFrom-Json).items
    $records = @()
    foreach ($configMap in $configMaps) {
        $encoded = $configMap.binaryData.'records.json'
        if (-not $encoded) { continue }
        $records += @([Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($encoded)) | ConvertFrom-Json)
    }
    return @($records)
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

function Remove-TestCostRecords {
    param([Parameter(Mandatory = $true)][string]$Workspace)
    $configMaps = (Invoke-Kubectl @("get", "configmaps", "-n", $ControllerNamespace, "-l", "kognition.dev/cost-record=true", "-o", "json") | Out-String | ConvertFrom-Json).items
    foreach ($configMap in @($configMaps)) {
        $encoded = $configMap.binaryData.'records.json'
        if (-not $encoded) { continue }
        $records = @([Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($encoded)) | ConvertFrom-Json)
        if (@($records | Where-Object { $_.workspace -eq $Workspace }).Count -gt 0) {
            Invoke-Kubectl @("delete", "configmap/$($configMap.metadata.name)", "-n", $ControllerNamespace, "--ignore-not-found") | Out-Null
        }
    }
    $receipts = (Invoke-Kubectl @("get", "configmaps", "-n", $ControllerNamespace, "-l", "kognition.dev/usage-receipt=true", "-o", "json") | Out-String | ConvertFrom-Json).items
    foreach ($receipt in @($receipts)) {
        if ($receipt.metadata.annotations.'kognition.dev/workspace' -eq $Workspace) {
            Invoke-Kubectl @("delete", "configmap/$($receipt.metadata.name)", "-n", $ControllerNamespace, "--ignore-not-found") | Out-Null
        }
    }
}

function Invoke-Gateway {
    param([Parameter(Mandatory = $true)][string]$Uri, [Parameter(Mandatory = $true)][string]$Capability, [Parameter(Mandatory = $true)][int]$ExpectedStatus, [string]$Body = '{"model":"attacker-selected-model","messages":[{"role":"user","content":"hello"}]}')
    $response = Invoke-WebRequest -Uri "$Uri/v1/chat/completions" -Method Post -Headers @{ Authorization = "Bearer $Capability" } -ContentType "application/json" -Body $body -SkipHttpErrorCheck
    if ($response.StatusCode -ne $ExpectedStatus) { throw "Model Gateway returned $($response.StatusCode), want $ExpectedStatus. Body: $($response.Content)" }
    return $response
}

function Set-ActiveModelSession {
    param([Parameter(Mandatory = $true)][string]$InstanceName, [Parameter(Mandatory = $true)][string]$SessionID)
    Invoke-Kubectl @("patch", "agentinstance/$InstanceName", "-n", $namespace, "--subresource=status", "--type=merge", "-p", ("{`"status`":{`"phase`":`"Running`",`"sessionID`":`"$SessionID`"}}")) | Out-Null
}

function Acquire-ControllerLock {
    $deadline = [DateTime]::UtcNow.AddMinutes(3)
    do {
        $leaseText = (& kubectl get lease $controllerLock -n $ControllerNamespace -o json 2>$null | Out-String).Trim()
        if (-not $leaseText) {
            $leaseTime = [DateTime]::UtcNow.ToString("yyyy-MM-ddTHH:mm:ss.ffffffZ")
            $object = @{ apiVersion = "coordination.k8s.io/v1"; kind = "Lease"; metadata = @{ name = $controllerLock; namespace = $ControllerNamespace }; spec = @{ holderIdentity = $controllerLockHolder; leaseDurationSeconds = 900; acquireTime = $leaseTime; renewTime = $leaseTime } }
            $object | ConvertTo-Json -Depth 10 -Compress | & kubectl create -f - 2>$null | Out-Null
            if ($LASTEXITCODE -eq 0) { return }
        } else {
            $lease = $leaseText | ConvertFrom-Json
            $holder = [string]$lease.spec.holderIdentity
            $renewTime = [DateTime]::MinValue
            [void][DateTime]::TryParse([string]$lease.spec.renewTime, [ref]$renewTime)
            $expired = -not $holder -or [DateTime]::UtcNow -ge $renewTime.ToUniversalTime().AddSeconds([int]$lease.spec.leaseDurationSeconds)
            if ($expired) {
                $operations = @(
                    @{ op = "test"; path = "/metadata/resourceVersion"; value = [string]$lease.metadata.resourceVersion },
                    @{ op = "replace"; path = "/spec/holderIdentity"; value = $controllerLockHolder },
                    @{ op = "replace"; path = "/spec/leaseDurationSeconds"; value = 900 },
                    @{ op = "replace"; path = "/spec/renewTime"; value = [DateTime]::UtcNow.ToString("yyyy-MM-ddTHH:mm:ss.ffffffZ") }
                ) | ConvertTo-Json -Compress
                & kubectl patch lease $controllerLock -n $ControllerNamespace --type=json -p $operations 2>$null | Out-Null
                if ($LASTEXITCODE -eq 0) { return }
            }
        }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Timed out acquiring shared Model Gateway E2E controller lock"
}

function Wait-ForAgentInstanceCompletion {
    param([Parameter(Mandatory = $true)][string]$Name)
    $deadline = [DateTime]::UtcNow.AddSeconds(180)
    do {
        $instance = & kubectl get "agentinstance/$Name" -n $namespace -o json 2>$null | ConvertFrom-Json
        if ($instance.status.phase -eq "Ready") { return $instance }
        if ($instance.status.phase -eq "Failed") { throw "AgentInstance $Name failed: $($instance.status.conditions | ConvertTo-Json -Compress)" }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "AgentInstance $Name did not complete within 180 seconds"
}

Write-Host "=== Kognition Kubernetes Model Gateway E2E ===" -ForegroundColor Cyan

try {
    $previousGOOS = $env:GOOS
    $previousGOARCH = $env:GOARCH
    $previousCGOEnabled = $env:CGO_ENABLED
    try {
        $env:GOOS = "windows"
        $env:GOARCH = "amd64"
        $env:CGO_ENABLED = "0"
        & go build -o $capabilityHelper ./e2e/mint-model-session-capability.go
        if ($LASTEXITCODE -ne 0) { throw "build Model Gateway capability helper failed" }
    } finally {
        if ($null -eq $previousGOOS) { Remove-Item Env:GOOS -ErrorAction SilentlyContinue } else { $env:GOOS = $previousGOOS }
        if ($null -eq $previousGOARCH) { Remove-Item Env:GOARCH -ErrorAction SilentlyContinue } else { $env:GOARCH = $previousGOARCH }
        if ($null -eq $previousCGOEnabled) { Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue } else { $env:CGO_ENABLED = $previousCGOEnabled }
    }
    $cilium = & kubectl get daemonset cilium -n kube-system -o json 2>$null | ConvertFrom-Json
    if ($null -eq $cilium -or [int]$cilium.status.desiredNumberScheduled -lt 1 -or [int]$cilium.status.numberReady -ne [int]$cilium.status.desiredNumberScheduled) {
        throw "Cilium DaemonSet is not fully ready; this E2E requires Cilium policy enforcement"
    }
    Invoke-Kubectl @("get", "deployment/$gatewayService", "-n", $ControllerNamespace) | Out-Null
    Invoke-Kubectl @("wait", "-n", $ControllerNamespace, "--for=condition=Available", "deployment/$gatewayService", "--timeout=120s") | Out-Null
    Invoke-Kubectl @("create", "namespace", $namespace) | Out-Null
	$createdNamespace = $true

    Apply-KubernetesObject @{ apiVersion = "v1"; kind = "ConfigMap"; metadata = @{ name = "mock-openai-source"; namespace = $namespace }; data = @{ "server.py" = @'
import json
from http.server import BaseHTTPRequestHandler, HTTPServer

records = []
class Handler(BaseHTTPRequestHandler):
    def send_json(self, value):
        body = json.dumps(value).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def do_GET(self):
        if self.path == "/v1/models":
            self.send_json({"object":"list","data":[{"id":"provider-pinned-model","object":"model"}]})
            return
        if self.path == "/records":
            self.send_json(records)
            return
        self.send_error(404)
    def do_POST(self):
        if self.path != "/v1/chat/completions":
            self.send_error(404)
            return
        request = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))) or "{}")
        records.append({"authorization": self.headers.get("Authorization", ""), "model": request.get("model", "")})
        self.send_json({"id":"model-gateway-e2e","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"gateway reply"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}})
    def log_message(self, *_): pass
HTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
'@ } }
    Apply-KubernetesObject @{ apiVersion = "apps/v1"; kind = "Deployment"; metadata = @{ name = "mock-openai"; namespace = $namespace }; spec = @{ replicas = 1; selector = @{ matchLabels = @{ app = "mock-openai" } }; template = @{ metadata = @{ labels = @{ app = "mock-openai" } }; spec = @{ containers = @(@{ name = "mock-openai"; image = "python:3.12-alpine"; command = @("python", "/app/server.py"); ports = @(@{ containerPort = 8080 }); volumeMounts = @(@{ name = "source"; mountPath = "/app" }) }); volumes = @(@{ name = "source"; configMap = @{ name = "mock-openai-source" } }) } } } }
    Apply-KubernetesObject @{ apiVersion = "v1"; kind = "Service"; metadata = @{ name = "mock-openai"; namespace = $namespace }; spec = @{ selector = @{ app = "mock-openai" }; ports = @(@{ port = 8080; targetPort = 8080 }) } }
    Invoke-Kubectl @("rollout", "status", "deployment/mock-openai", "-n", $namespace, "--timeout=120s") | Out-Null
	$providerPodIP = (Invoke-Kubectl @("get", "pod", "-n", $namespace, "-l", "app=mock-openai", "-o", "jsonpath={.items[0].status.podIP}") | Out-String).Trim()
	if (-not $providerPodIP) { throw "mock provider Pod did not report an IP" }

    $denyProbe = "model-gateway-deny-probe-$runID"
    $denyProbePolicy = "$denyProbe-deny"
    Apply-KubernetesObject @{ apiVersion = "networking.k8s.io/v1"; kind = "NetworkPolicy"; metadata = @{ name = $denyProbePolicy; namespace = $ControllerNamespace }; spec = @{ podSelector = @{ matchLabels = @{ "kognition.dev/model-gateway-deny-probe" = "$runID" } }; policyTypes = @("Egress"); egress = @() } }
    & kubectl run $denyProbe -n $ControllerNamespace --image=python:3.12-alpine --restart=Never --labels="kognition.dev/model-gateway-deny-probe=$runID" --command -- python -c "import urllib.request; urllib.request.urlopen('http://${providerPodIP}:8080/v1/models', timeout=3)" 2>&1 | Out-Null
    $deadline = [DateTime]::UtcNow.AddSeconds(30)
    do {
        $probePhase = (& kubectl get pod $denyProbe -n $ControllerNamespace -o jsonpath='{.status.phase}' 2>$null | Out-String).Trim()
        if ($probePhase -in @("Failed", "Succeeded")) { break }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    $probe = & kubectl get pod $denyProbe -n $ControllerNamespace -o json | ConvertFrom-Json
    $probeLogs = (& kubectl logs $denyProbe -n $ControllerNamespace 2>&1 | Out-String)
    $terminated = $probe.status.containerStatuses[0].state.terminated
    if ($probePhase -ne "Failed" -or -not $probe.status.containerStatuses[0].imageID -or $null -eq $terminated -or $terminated.exitCode -eq 0 -or $probeLogs -notmatch '(?i)timed out') {
        throw "Cilium deny probe did not execute and time out as expected: phase=$probePhase termination=$($terminated | ConvertTo-Json -Compress) logs=$probeLogs"
    }
    Invoke-Kubectl @("delete", "pod/$denyProbe", "-n", $ControllerNamespace, "--wait=true") | Out-Null
    Invoke-Kubectl @("delete", "networkpolicy/$denyProbePolicy", "-n", $ControllerNamespace, "--wait=true") | Out-Null
    $denyProbePolicy = $null

    # NetworkPolicies are additive. This test-scoped policy permits only its
    # disposable provider target for the Gateway and controller health probe.
    Apply-KubernetesObject @{ apiVersion = "networking.k8s.io/v1"; kind = "NetworkPolicy"; metadata = @{ name = $providerEgressPolicy; namespace = $ControllerNamespace }; spec = @{ podSelector = @{ matchLabels = @{ "app.kubernetes.io/instance" = $ReleaseName }; matchExpressions = @(@{ key = "app.kubernetes.io/component"; operator = "In"; values = @("model-gateway", "k8s-controller") }) }; policyTypes = @("Egress"); egress = @(@{ to = @(@{ namespaceSelector = @{ matchLabels = @{ "kubernetes.io/metadata.name" = $namespace } }; podSelector = @{ matchLabels = @{ app = "mock-openai" } } }); ports = @(@{ protocol = "TCP"; port = 8080 }) }) } }

    $agentName = "model-gateway-agent"
    $modelName = "mock-model"
    $quotaName = "model-gateway-usage"
    $instanceName = "model-gateway-direct"
    $sessionID = "model-gateway-session-$runID"
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "SecretRef"; metadata = @{ name = "mock-model-key"; namespace = $namespace; annotations = @{ "kognition.dev/internal-secret-data" = '{"type":"api-key","apiKey":"provider-secret"}' } }; spec = @{ backendType = "internal"; backendRef = "secret/mock-model-key"; key = "apiKey" } }
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "ModelClass"; metadata = @{ name = $modelClassName }; spec = @{ provisioner = "openai-compatible"; costTier = "premium"; parameters = @{ "cost.inputMicrousdPerMillionTokens" = "2500000"; "cost.outputMicrousdPerMillionTokens" = "10000000" } } }
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "Model"; metadata = @{ name = $modelName; namespace = $namespace }; spec = @{ className = $modelClassName; modelID = "provider-pinned-model"; endpoint = "http://mock-openai.$namespace.svc.cluster.local:8080/v1"; capabilities = @("chat"); credentialRef = @{ name = "mock-model-key" } } }
    Wait-ForJsonPath -Resource "model/$modelName" -JsonPath "{.status.phase}" -Expected "Ready"
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "Agent"; metadata = @{ name = $agentName; namespace = $namespace }; spec = @{ modelRef = @{ name = $modelName }; instructions = @{ inline = "Reply with gateway reply." }; replicas = 1 } }
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "ModelGrant"; metadata = @{ name = "model-gateway-grant"; namespace = $namespace }; spec = @{ agentRef = @{ name = $agentName }; modelRef = @{ name = $modelName } } }
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "CognitiveQuota"; metadata = @{ name = $quotaName; namespace = $namespace }; spec = @{ hard = @{}; costBudget = @{ maxCostPerPeriod = 15; maxModelTierSeconds = @{ premium = 60 } } } }
    Wait-ForJsonPath -Resource "agentidentity/agent-identity-$agentName" -JsonPath "{.status.phase}" -Expected "Ready"
    $spiffeID = (& kubectl get "agentidentity/agent-identity-$agentName" -n $namespace -o jsonpath='{.spec.spiffeID}' | Out-String).Trim()
    if (-not $spiffeID) { throw "AgentIdentity did not report a SPIFFE ID" }
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "AgentInstance"; metadata = @{ name = $instanceName; namespace = $namespace }; spec = @{ agentRef = @{ name = $agentName }; revision = 1 } }

    Acquire-ControllerLock
    $controllerLockAcquired = $true
	$controllerReplicas = (Invoke-Kubectl @("get", "deployment/$controllerDeployment", "-n", $ControllerNamespace, "-o", "jsonpath={.spec.replicas}") | Out-String).Trim()
	if (-not $controllerReplicas) { throw "controller Deployment did not report spec.replicas" }
    Invoke-Kubectl @("scale", "deployment/$controllerDeployment", "-n", $ControllerNamespace, "--replicas=0") | Out-Null
    Invoke-Kubectl @("rollout", "status", "deployment/$controllerDeployment", "-n", $ControllerNamespace, "--timeout=120s") | Out-Null
    $instanceUID = (& kubectl get "agentinstance/$instanceName" -n $namespace -o jsonpath='{.metadata.uid}' | Out-String).Trim()
    $modelVersion = (& kubectl get "model/$modelName" -n $namespace -o jsonpath='{.metadata.resourceVersion}' | Out-String).Trim()
    Set-ActiveModelSession -InstanceName $instanceName -SessionID $sessionID

    $gatewayPort = Get-AvailableTcpPort
    $portForward = Start-Job -ScriptBlock { param($ns, $service, $port) kubectl -n $ns port-forward "service/$service" "${port}:8093" } -ArgumentList $ControllerNamespace, $gatewayService, $gatewayPort
    $gatewayURL = "http://127.0.0.1:$gatewayPort"
    $deadline = [DateTime]::UtcNow.AddSeconds(30); do { try { if ((Invoke-WebRequest "$gatewayURL/healthz" -SkipHttpErrorCheck).StatusCode -eq 200) { break } } catch {}; [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult() } while ([DateTime]::UtcNow -lt $deadline)
    $keyBase64 = (& kubectl get secret "$ReleaseName-model-gateway" -n $ControllerNamespace '-o=jsonpath={.data.capability-hmac-key}' | Out-String).Trim()
    $keyIDBase64 = (& kubectl get secret "$ReleaseName-model-gateway" -n $ControllerNamespace '-o=jsonpath={.data.capability-hmac-key-id}' | Out-String).Trim()
    $keyID = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($keyIDBase64))
    $capability = Invoke-CapabilityMint -KeyBase64 $keyBase64 -FilePath $capabilityHelper -ArgumentList @("-key-id", $keyID, "-namespace", $namespace, "-agent", $agentName, "-identity", "agent-identity-$agentName", "-instance", $instanceName, "-instance-uid", $instanceUID, "-session", $sessionID, "-model", $modelName, "-model-resource-version", $modelVersion)
    Invoke-Gateway -Uri $gatewayURL -Capability $capability -ExpectedStatus 400 -Body '{"model":"attacker-selected-model","stream":true,"messages":[{"role":"user","content":"hello"}]}' | Out-Null
    Invoke-Gateway -Uri $gatewayURL -Capability $capability -ExpectedStatus 200 | Out-Null
    $costRecord = Wait-ForCostRecord { $_.workspace -eq $namespace -and $_.operationType -eq 'model_inference' -and $_.sessionId -eq $sessionID -and $_.modelName -eq 'provider-pinned-model' }
    if ($costRecord.agentIdentity -ne $spiffeID -or $costRecord.reservationId -ne "agentinstance:$instanceUID" -or $costRecord.promptTokens -ne 2 -or $costRecord.completionTokens -ne 1 -or $costRecord.totalTokens -ne 3 -or $costRecord.costUnits -ne 15 -or $costRecord.modelClass -ne $modelClassName -or $costRecord.modelCostTier -ne "premium" -or -not $costRecord.pricingConfigured -or $costRecord.inputCostMicrousdPerMillionTokens -ne 2500000 -or $costRecord.outputCostMicrousdPerMillionTokens -ne 10000000 -or $costRecord.providerDurationMillis -lt 1) {
        throw "Unexpected Model Gateway cost record: $($costRecord | ConvertTo-Json -Compress)"
    }
    Wait-ForJsonPath -Resource "cognitivequota/$quotaName" -JsonPath "{.status.costUsage.totalCost}" -Expected "15"
    $premiumSeconds = (& kubectl get "cognitivequota/$quotaName" -n $namespace -o jsonpath='{.status.costUsage.modelTierSeconds.premium}' | Out-String).Trim()
    if (-not $premiumSeconds -or [int64]$premiumSeconds -lt 1) { throw "CognitiveQuota premium tier seconds = '$premiumSeconds', want at least 1" }
    Invoke-Gateway -Uri $gatewayURL -Capability $capability -ExpectedStatus 429 | Out-Null
    Invoke-Kubectl @("delete", "cognitivequota/$quotaName", "-n", $namespace, "--wait=true") | Out-Null

    # Restore the fail-closed audit webhook before mutating ModelGrant state.
    Invoke-Kubectl @("scale", "deployment/$controllerDeployment", "-n", $ControllerNamespace, "--replicas=$controllerReplicas") | Out-Null
    Invoke-Kubectl @("wait", "-n", $ControllerNamespace, "--for=condition=Available", "deployment/$controllerDeployment", "--timeout=120s") | Out-Null
    Invoke-Kubectl @("delete", "modelgrant/model-gateway-grant", "-n", $namespace, "--wait=true") | Out-Null
    Invoke-Gateway -Uri $gatewayURL -Capability $capability -ExpectedStatus 403 | Out-Null

    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "ModelGrant"; metadata = @{ name = "model-gateway-grant"; namespace = $namespace }; spec = @{ agentRef = @{ name = $agentName }; modelRef = @{ name = $modelName } } }

    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "AgentPolicy"; metadata = @{ name = "model-gateway-deny"; namespace = $namespace }; spec = @{ enforce = $true; scope = "agent"; targetAgents = @($agentName); rules = 'deny[msg] { input.action == "model_call"; msg := "Model Gateway E2E deny" }' } }
    Wait-ForJsonPath -Resource "agentpolicy/model-gateway-deny" -JsonPath "{.status.phase}" -Expected "Ready"
    Set-ActiveModelSession -InstanceName $instanceName -SessionID $sessionID
    Invoke-Gateway -Uri $gatewayURL -Capability $capability -ExpectedStatus 403 | Out-Null
    Invoke-Kubectl @("delete", "agentpolicy/model-gateway-deny", "-n", $namespace, "--wait=true") | Out-Null
    Set-ActiveModelSession -InstanceName $instanceName -SessionID $sessionID
    Invoke-Gateway -Uri $gatewayURL -Capability $capability -ExpectedStatus 200 | Out-Null

    $runtimeInstance = "model-gateway-runtime"
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "AgentInstance"; metadata = @{ name = $runtimeInstance; namespace = $namespace }; spec = @{ agentRef = @{ name = $agentName }; revision = 1; sessionContext = @{ input = "reply through the model gateway" } } }
    $instance = Wait-ForAgentInstanceCompletion -Name $runtimeInstance
    if ($instance.status.output -notmatch "gateway reply") { throw "Runtime output did not come from Model Gateway: $($instance.status.output)" }
    $podName = (& kubectl get pods -n $namespace -l "kognition.dev/agent-name=$agentName" -o jsonpath='{.items[0].metadata.name}' | Out-String).Trim()
    $envs = (& kubectl get pod $podName -n $namespace -o json | ConvertFrom-Json).spec.containers[0].env
    $envNames = @($envs.name)
    if ($envNames -contains "KOGNITION_MODEL_ENDPOINT" -or $envNames -contains "KOGNITION_API_KEY" -or $envNames -contains "KOGNITION_API_VERSION" -or $envNames -notcontains "KOGNITION_MODEL_GATEWAY_URL") { throw "Gateway runtime Pod environment does not redact direct provider configuration" }
    $recordsPod = (& kubectl get pod -n $namespace -l app=mock-openai -o jsonpath='{.items[0].metadata.name}' | Out-String).Trim()
    $records = (& kubectl exec -n $namespace $recordsPod -- python -c 'import urllib.request; print(urllib.request.urlopen("http://127.0.0.1:8080/records").read().decode())' | Out-String | ConvertFrom-Json)
    if (@($records | Where-Object { $_.authorization -eq "Bearer provider-secret" -and $_.model -eq "provider-pinned-model" }).Count -lt 2) { throw "Provider did not receive gateway credential and pinned Model ID: $($records | ConvertTo-Json -Compress)" }
	$testPassed = $true
}
finally {
	$cleanupErrors = [System.Collections.Generic.List[string]]::new()
    if ($portForward) { Stop-Job $portForward -ErrorAction SilentlyContinue; Remove-Job $portForward -Force -ErrorAction SilentlyContinue }
	if (Test-Path $capabilityHelper) { Remove-Item $capabilityHelper -Force -ErrorAction SilentlyContinue }
    if ($null -ne $controllerReplicas) { Invoke-Kubectl @("scale", "deployment/$controllerDeployment", "-n", $ControllerNamespace, "--replicas=$controllerReplicas") | Out-Null; Invoke-Kubectl @("wait", "-n", $ControllerNamespace, "--for=condition=Available", "deployment/$controllerDeployment", "--timeout=120s") | Out-Null }
    if ($denyProbe) { Invoke-Kubectl @("delete", "pod/$denyProbe", "-n", $ControllerNamespace, "--ignore-not-found") | Out-Null }
	if ($denyProbePolicy) { Invoke-Kubectl @("delete", "networkpolicy/$denyProbePolicy", "-n", $ControllerNamespace, "--ignore-not-found") | Out-Null }
    Invoke-Kubectl @("delete", "networkpolicy/$providerEgressPolicy", "-n", $ControllerNamespace, "--ignore-not-found") | Out-Null
    if ($createdNamespace) {
        Remove-TestCostRecords -Workspace $namespace
		Invoke-Kubectl @("delete", "namespace/$namespace", "--ignore-not-found", "--wait=true") | Out-Null
		Invoke-Kubectl @("delete", "modelclass/$modelClassName", "--ignore-not-found", "--wait=true") | Out-Null
    }
    if ($controllerLockAcquired) {
        $leaseText = (Invoke-Kubectl @("get", "lease/$controllerLock", "-n", $ControllerNamespace, "-o", "json") | Out-String).Trim()
        if ($leaseText) {
            $lease = $leaseText | ConvertFrom-Json
            $operations = @(
                @{ op = "test"; path = "/spec/holderIdentity"; value = $controllerLockHolder },
                @{ op = "replace"; path = "/spec/holderIdentity"; value = "" }
            ) | ConvertTo-Json -Compress
            Invoke-Kubectl @("patch", "lease/$controllerLock", "-n", $ControllerNamespace, "--type=json", "-p", $operations) | Out-Null
        }
    }
    if ($null -ne $controllerReplicas) {
		$restored = (Invoke-Kubectl @("get", "deployment/$controllerDeployment", "-n", $ControllerNamespace, "-o", "jsonpath={.spec.replicas}") | Out-String).Trim()
        if ($restored -ne [string]$controllerReplicas) { $cleanupErrors.Add("controller replicas restored to '$restored', want '$controllerReplicas'") }
    }
	if ($denyProbe -and (Invoke-Kubectl @("get", "pod/$denyProbe", "-n", $ControllerNamespace, "--ignore-not-found", "-o", "name"))) { $cleanupErrors.Add("deny probe remains") }
	if ($denyProbePolicy -and (Invoke-Kubectl @("get", "networkpolicy/$denyProbePolicy", "-n", $ControllerNamespace, "--ignore-not-found", "-o", "name"))) { $cleanupErrors.Add("deny policy remains") }
	if (Invoke-Kubectl @("get", "networkpolicy/$providerEgressPolicy", "-n", $ControllerNamespace, "--ignore-not-found", "-o", "name")) { $cleanupErrors.Add("provider egress policy remains") }
	if ($createdNamespace -and (Invoke-Kubectl @("get", "namespace/$namespace", "--ignore-not-found", "-o", "name"))) { $cleanupErrors.Add("test namespace remains") }
	if ($createdNamespace -and (Invoke-Kubectl @("get", "modelclass/$modelClassName", "--ignore-not-found", "-o", "name"))) { $cleanupErrors.Add("test ModelClass remains") }
    $leftoverCosts = @(Get-CostRecords | Where-Object { $_.workspace -eq $namespace }).Count
	$receiptItems = (Invoke-Kubectl @("get", "configmaps", "-n", $ControllerNamespace, "-l", "kognition.dev/usage-receipt=true", "-o", "json") | Out-String | ConvertFrom-Json).items
    $leftoverReceipts = @($receiptItems | Where-Object { $_.metadata.annotations.'kognition.dev/workspace' -eq $namespace }).Count
    if ($leftoverCosts -gt 0 -or $leftoverReceipts -gt 0) { $cleanupErrors.Add("accounting residue costs=$leftoverCosts receipts=$leftoverReceipts") }
    if ($cleanupErrors.Count -gt 0) { throw "Model Gateway E2E cleanup failed: $($cleanupErrors -join '; ')" }
}

if ($testPassed) {
    Write-Host "Model Gateway rejected streaming, persisted priced token usage, projected and enforced monetary/model-tier quota, enforced live grant and policy revocation, pinned the provider Model ID, redacted runtime provider configuration, and kept the provider credential inside the gateway." -ForegroundColor Green
    Write-Host "=== PASS ===" -ForegroundColor Green
}