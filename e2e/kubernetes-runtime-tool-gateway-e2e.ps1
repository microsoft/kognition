# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

param(
    [string]$ControllerNamespace = "kognition-system",
    [string]$ReleaseName = "kognition"
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$runID = [DateTimeOffset]::UtcNow.ToUnixTimeSeconds()
$namespace = "runtime-tool-gateway-e2e-$runID"
$runtimePortForward = $null
$runtimeApprovalJob = $null

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
        [int]$TimeoutSeconds = 120
    )

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        $value = (& kubectl get $Resource -n $namespace "-o=jsonpath=$JsonPath" 2>$null | Out-String).Trim()
        if ($value -eq $Expected) { return }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "$Resource did not reach $JsonPath=$Expected within $TimeoutSeconds seconds"
}

function Wait-ForAgentInstanceCompletion {
    param([Parameter(Mandatory = $true)][string]$Name)

    $deadline = [DateTime]::UtcNow.AddSeconds(180)
    do {
        $instance = & kubectl get "agentinstance/$Name" -n $namespace -o json 2>$null | ConvertFrom-Json
        if ($instance.status.phase -eq "Ready") { return $instance }
        if ($instance.status.phase -eq "Failed") {
            throw "AgentInstance $Name failed: $($instance.status.conditions | ConvertTo-Json -Compress)"
        }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "AgentInstance $Name did not complete within 180 seconds"
}

function Wait-ForReadyServiceEndpoint {
    param([Parameter(Mandatory = $true)][string]$Service)

    $deadline = [DateTime]::UtcNow.AddSeconds(30)
    do {
        $endpointSlices = ((Invoke-Kubectl @("get", "endpointslice", "-n", $namespace, "-l", "kubernetes.io/service-name=$Service", "-o", "json")) -join "`n" | ConvertFrom-Json).items
        $readyEndpoints = @($endpointSlices | ForEach-Object { $_.endpoints } | Where-Object { $_.conditions.ready -eq $true })
        if ($readyEndpoints.Count -gt 0) { return }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Service $Service did not publish a ready EndpointSlice within 30 seconds"
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

function Get-RuntimeStatus {
    param(
        [Parameter(Mandatory = $true)][string]$Uri,
        [Parameter(Mandatory = $true)][string]$Authorization,
        [Parameter(Mandatory = $true)][System.Management.Automation.Job]$PortForward
    )

    $deadline = [DateTime]::UtcNow.AddSeconds(30)
    $lastError = $null
    do {
        try {
            $response = Invoke-WebRequest -Uri $Uri -Headers @{ Authorization = $Authorization } -TimeoutSec 2
            if ($response.StatusCode -eq 200) { return $response.Content | ConvertFrom-Json }
        }
        catch {
            $lastError = $_
        }
        if ($PortForward.State -ne "Running") {
            $portForwardOutput = (Receive-Job $PortForward -Keep -ErrorAction SilentlyContinue 2>&1 | Out-String).Trim()
            throw "Runtime port-forward job $($PortForward.Id) exited with state '$($PortForward.State)': $portForwardOutput"
        }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Runtime status endpoint did not become available: $Uri. Last error: $($lastError.Exception.Message)"
}

Write-Host "=== Kognition Runtime Tool Gateway E2E ===" -ForegroundColor Cyan

try {
    Invoke-Kubectl @("get", "deployment/$ReleaseName-tool-gateway", "-n", $ControllerNamespace) | Out-Null
    Invoke-Kubectl @("wait", "-n", $ControllerNamespace, "--for=condition=Available", "deployment/$ReleaseName-tool-gateway", "--timeout=120s") | Out-Null
    Invoke-Kubectl @("get", "deployment/tool-sandbox", "-n", "default") | Out-Null
    Invoke-Kubectl @("wait", "-n", "default", "--for=condition=Available", "deployment/tool-sandbox", "--timeout=120s") | Out-Null
    Invoke-Kubectl @("create", "namespace", $namespace) | Out-Null

    $modelClassName = "mock-openai-class"
    $modelName = "mock-tool-model"
    $agentName = "tool-runtime-agent"
    $instanceName = "tool-runtime-instance"
    $toolName = "sandbox-calculator"
    $quotaName = "tool-runtime-quota"

    Apply-KubernetesObject @{ apiVersion = "v1"; kind = "ConfigMap"; metadata = @{ name = "mock-openai-source"; namespace = $namespace }; data = @{ "server.py" = @'
import json
import time
from http.server import BaseHTTPRequestHandler, HTTPServer

class Handler(BaseHTTPRequestHandler):
    def respond(self, payload):
        body = json.dumps(payload).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == "/v1/models":
            self.respond({"object": "list", "data": [{"id": "mock-tool-model", "object": "model"}]})
            return
        self.send_error(404)

    def do_POST(self):
        if self.path != "/v1/chat/completions":
            self.send_error(404)
            return
        size = int(self.headers.get("Content-Length", "0"))
        request = json.loads(self.rfile.read(size) or "{}")
        messages = request.get("messages", [])
        tool_results = [message for message in messages if message.get("role") == "tool"]
        quota_case = any("quota-exhaustion" in str(message.get("content", "")) for message in messages)
        pending = False
        if tool_results:
            try:
                pending = str(json.loads(tool_results[-1].get("content", "{}")).get("runtime_approval_state", "")).lower() == "pending"
            except (TypeError, ValueError):
                pass
        if tool_results and not pending:
            message = {"role": "assistant", "content": "The calculator returned 391."}
            finish_reason = "stop"
        else:
            if pending:
                time.sleep(0.25)
            elif quota_case:
                time.sleep(5)
            message = {"role": "assistant", "content": "", "tool_calls": [{"id": "call-calculator", "type": "function", "function": {"name": "sandbox_calculator", "arguments": "{\"expression\":\"17 * 23\"}"}}]}
            finish_reason = "tool_calls"
        self.respond({"id": "mock-completion", "object": "chat.completion", "created": 0, "model": "mock-tool-model", "choices": [{"index": 0, "message": message, "finish_reason": finish_reason}], "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}})

    def log_message(self, *_):
        pass

HTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
'@ } }
    Apply-KubernetesObject @{ apiVersion = "apps/v1"; kind = "Deployment"; metadata = @{ name = "mock-openai"; namespace = $namespace }; spec = @{ replicas = 1; selector = @{ matchLabels = @{ app = "mock-openai" } }; template = @{ metadata = @{ labels = @{ app = "mock-openai" } }; spec = @{ containers = @(@{ name = "mock-openai"; image = "python:3.12-alpine"; command = @("python", "/app/server.py"); ports = @(@{ containerPort = 8080 }); readinessProbe = @{ httpGet = @{ path = "/v1/models"; port = 8080 }; initialDelaySeconds = 1; periodSeconds = 1 }; volumeMounts = @(@{ name = "source"; mountPath = "/app" }) }); volumes = @(@{ name = "source"; configMap = @{ name = "mock-openai-source" } }) } } } }
    Apply-KubernetesObject @{ apiVersion = "v1"; kind = "Service"; metadata = @{ name = "mock-openai"; namespace = $namespace }; spec = @{ selector = @{ app = "mock-openai" }; ports = @(@{ port = 8080; targetPort = 8080 }) } }
    Invoke-Kubectl @("rollout", "status", "deployment/mock-openai", "-n", $namespace, "--timeout=120s") | Out-Null
    Wait-ForReadyServiceEndpoint -Service "mock-openai"

    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "SecretRef"; metadata = @{ name = "mock-openai-key"; namespace = $namespace; annotations = @{ "kognition.dev/internal-secret-data" = '{"type":"api-key","apiKey":"mock"}' } }; spec = @{ backendType = "internal"; backendRef = "secret/mock-openai-key"; key = "apiKey" } }
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "ModelClass"; metadata = @{ name = $modelClassName; namespace = $namespace }; spec = @{ provisioner = "openai-compatible" } }
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "Model"; metadata = @{ name = $modelName; namespace = $namespace }; spec = @{ className = $modelClassName; provider = "openai-compatible"; modelID = $modelName; endpoint = "http://mock-openai.$namespace.svc.cluster.local:8080/v1"; capabilities = @("chat", "tool-calling"); credentialRef = @{ name = "mock-openai-key" } } }
    Wait-ForJsonPath -Resource "modelclass/$modelClassName" -JsonPath "{.status.phase}" -Expected "Ready"
    Wait-ForJsonPath -Resource "model/$modelName" -JsonPath "{.status.phase}" -Expected "Ready"

    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "Tool"; metadata = @{ name = $toolName; namespace = $namespace }; spec = @{ className = "sandbox-openapi"; protocol = "OpenAPI"; description = "Evaluate arithmetic."; endpoint = "http://tool-sandbox.samples-tool-sandbox.svc.cluster.local:8080/calculator"; metadata = @{ allowPrivateNetwork = "true" }; inputSchema = @{ type = "object"; properties = @{ expression = @{ type = "string" } }; required = @("expression") } } }
    Wait-ForJsonPath -Resource "tool/$toolName" -JsonPath "{.status.phase}" -Expected "Ready"
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "Agent"; metadata = @{ name = $agentName; namespace = $namespace }; spec = @{ modelRef = @{ name = $modelName }; instructions = @{ inline = "Use sandbox_calculator for arithmetic and report its result." }; tools = @(@{ ref = @{ name = $toolName } }) } }
    Wait-ForJsonPath -Resource "agentidentity/agent-identity-$agentName" -JsonPath "{.status.phase}" -Expected "Ready"
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "ModelGrant"; metadata = @{ name = "tool-runtime-model"; namespace = $namespace }; spec = @{ agentRef = @{ name = $agentName }; modelRef = @{ name = $modelName } } }
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "AgentPolicy"; metadata = @{ name = "tool-runtime-grants"; namespace = $namespace }; spec = @{ scope = "agent"; enforce = $true; targetAgents = @($agentName); toolGrants = @(@{ toolRef = $toolName; allow = $true }) } }
    Wait-ForJsonPath -Resource "agentpolicy/tool-runtime-grants" -JsonPath "{.status.phase}" -Expected "Ready"
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "AgentRole"; metadata = @{ name = "tool-invoker"; namespace = $namespace }; rules = @(@{ resources = @("Tool"); verbs = @("get", "invoke") }) }
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "AgentRoleBinding"; metadata = @{ name = "tool-invoker"; namespace = $namespace }; spec = @{ roleRef = @{ name = "tool-invoker" }; subjects = @(@{ kind = "Agent"; name = $agentName }) } }
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "CognitiveQuota"; metadata = @{ name = $quotaName; namespace = $namespace }; spec = @{ hard = @{}; costBudget = @{ maxCostPerPeriod = 0; maxToolCallsPerPeriod = 2 } } }
    Wait-ForReadyServiceEndpoint -Service "agent-$agentName"
    $runtimeApprovalJob = Start-Job -ScriptBlock {
        param($RuntimeNamespace, $ControllerNamespace, $GatewayService, $RunID)
        $deadline = [DateTime]::UtcNow.AddSeconds(120)
        do {
            $items = (& kubectl get runtimeapprovals -n $RuntimeNamespace -o json 2>$null | ConvertFrom-Json).items
            $approval = @($items | Where-Object { $_.status.decisionState -eq "Pending" -and $_.status.executionState -eq "NotStarted" } | Select-Object -First 1)
            if ($approval.Count -eq 1) {
                $decidedAt = [DateTime]::UtcNow.ToString("o")
                $issuer = "kognition-e2e"
                $subject = "runtime-tool-gateway-e2e"
                $patch = @{ status = @{
                    decisionState = "Approved"
                    decidedByIssuer = $issuer
                    decidedBySubject = $subject
                    decidedAt = $decidedAt
                    decisionAudit = @{
                        id = "e2e-$RunID"
                        payload = @{
                            workspace = $RuntimeNamespace
                            invocationID = $approval[0].spec.invocationID
                            approvalUID = $approval[0].metadata.uid
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
                    & kubectl "--as=system:serviceaccount:$ControllerNamespace`:$GatewayService" patch "runtimeapproval/$($approval[0].metadata.name)" -n $RuntimeNamespace --subresource=status --type=merge --patch-file $patchFile | Out-Null
                    if ($LASTEXITCODE -ne 0) { throw "failed to approve RuntimeApproval/$($approval[0].metadata.name)" }
                    return
                }
                finally {
                    Remove-Item -Path $patchFile -Force -ErrorAction SilentlyContinue
                }
            }
            [Threading.Tasks.Task]::Delay(250).GetAwaiter().GetResult()
        } while ([DateTime]::UtcNow -lt $deadline)
        throw "pending RuntimeApproval was not observed within 120 seconds"
    } -ArgumentList $namespace, $ControllerNamespace, "$ReleaseName-tool-gateway", $runID
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "AgentInstance"; metadata = @{ name = $instanceName; namespace = $namespace }; spec = @{ agentRef = @{ name = $agentName }; revision = 1; sessionContext = @{ input = "What is 17 * 23? Use the calculator." } } }

    $instance = Wait-ForAgentInstanceCompletion -Name $instanceName
    Wait-Job $runtimeApprovalJob -Timeout 5 | Out-Null
    Receive-Job $runtimeApprovalJob -ErrorAction Stop | Out-Null
    Remove-Job $runtimeApprovalJob -Force
    $runtimeApprovalJob = $null
    if ($instance.status.output -notmatch "391") { throw "Runtime output did not include the Tool result: $($instance.status.output)" }
    $details = @($instance.status.toolCallsDetail)
    $pendingDetails = if ($details.Count -gt 1) { @($details[0..($details.Count - 2)]) } else { @() }
    $terminalDetail = if ($details.Count -gt 0) { $details[-1] } else { $null }
    $mismatchedDetails = @($details | Where-Object {
        $_.name -ne "sandbox_calculator" -or $_.args -ne $details[0].args
    })
    $nonPendingRetries = @($pendingDetails | Where-Object {
        $_.result -notmatch '"runtime_approval_state"\s*:\s*"pending"'
    })
    if ($details.Count -lt 2 -or
        $mismatchedDetails.Count -gt 0 -or
        $nonPendingRetries.Count -gt 0 -or
        $terminalDetail.result -notmatch '"result"\s*:\s*391') {
        throw "AgentInstance did not retain the expected pending-then-terminal Tool retry detail: $($details | ConvertTo-Json -Compress)"
    }

    $podName = (& kubectl get pod -n $namespace -l "kognition.dev/agent-name=$agentName" -o jsonpath='{.items[0].metadata.name}' | Out-String).Trim()
    if (-not $podName) { throw "Runtime Pod was not created" }
    $identity = ((Invoke-Kubectl @("get", "agentidentity/agent-identity-$agentName", "-n", $namespace, "-o", "json")) -join "`n" | ConvertFrom-Json)
    if (-not $identity.spec.spiffeID) { throw "Automatic AgentIdentity did not define a SPIFFE URI" }
    $runtimeAuthSource = ((Invoke-Kubectl @("get", "pod/$podName", "-n", $namespace, "-o", "json")) -join "`n" | ConvertFrom-Json).spec.containers[0].env |
        Where-Object { $_.name -eq "KOGNITION_RUNTIME_AUTH_TOKEN" } |
        Select-Object -First 1
    if (-not $runtimeAuthSource.valueFrom.secretKeyRef) { throw "Runtime Pod does not source its control token from a Secret" }
    $encodedRuntimeToken = (Invoke-Kubectl @("get", "secret/$($runtimeAuthSource.valueFrom.secretKeyRef.name)", "-n", $namespace, "-o", "jsonpath={.data.$($runtimeAuthSource.valueFrom.secretKeyRef.key)}")) -join ""
    $runtimeToken = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($encodedRuntimeToken))
    $runtimePort = Get-AvailableTcpPort
    $runtimePortForward = Start-Job -ScriptBlock { param($RuntimeNamespace, $RuntimePod, $Port) kubectl -n $RuntimeNamespace port-forward "pod/$RuntimePod" "${Port}:8080" } -ArgumentList $namespace, $podName, $runtimePort
    $runtimeStatus = Get-RuntimeStatus -Uri "http://127.0.0.1:$runtimePort/status?session_id=$($instance.status.sessionID)" -Authorization "Bearer $runtimeToken" -PortForward $runtimePortForward
    if ($runtimeStatus.spiffe_id -ne $identity.spec.spiffeID) {
        throw "Runtime session SPIFFE URI = '$($runtimeStatus.spiffe_id)', want '$($identity.spec.spiffeID)'"
    }
    $resolvedTools = (& kubectl get pod $podName -n $namespace -o jsonpath='{.spec.containers[0].env[?(@.name=="KOGNITION_TOOLS_RESOLVED")].value}' | Out-String).Trim() | ConvertFrom-Json
    $endpoint = $resolvedTools[0].PSObject.Properties["endpoint"]
    if ($endpoint -and $endpoint.Value) { throw "Gateway-enabled runtime received a direct Tool endpoint" }

    $reasons = @(((& kubectl get events -n $namespace --field-selector "involvedObject.kind=AgentInstance,involvedObject.name=$instanceName" -o json | ConvertFrom-Json).items).reason)
    if ($reasons -notcontains "ToolInvocationAuthorized" -or $reasons -notcontains "ToolInvocationCompleted") {
        throw "Runtime Tool Gateway invocation did not persist both lifecycle events: $($reasons -join ', ')"
    }

    $revokedInstanceName = "$instanceName-revoked"
    Invoke-Kubectl @("delete", "agentpolicy/tool-runtime-grants", "-n", $namespace, "--wait=true") | Out-Null
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "AgentInstance"; metadata = @{ name = $revokedInstanceName; namespace = $namespace }; spec = @{ agentRef = @{ name = $agentName }; revision = 1; sessionContext = @{ input = "What is 17 * 23? Use the calculator." } } }
    $revokedInstance = Wait-ForAgentInstanceCompletion -Name $revokedInstanceName
    $revokedDetails = @($revokedInstance.status.toolCallsDetail)
    if ($revokedDetails.Count -ne 1 -or $revokedDetails[0].name -ne "sandbox_calculator" -or $revokedDetails[0].result -notmatch "403") {
        throw "Runtime Tool Gateway did not surface policy revocation to the Tool call detail: $($revokedDetails | ConvertTo-Json -Compress)"
    }
    $revokedReasons = @(((& kubectl get events -n $namespace --field-selector "involvedObject.kind=AgentInstance,involvedObject.name=$revokedInstanceName" -o json | ConvertFrom-Json).items).reason)
    if ($revokedReasons -contains "ToolInvocationAuthorized" -or $revokedReasons -contains "ToolInvocationCompleted") {
        throw "Revoked runtime Tool call persisted execution lifecycle events: $($revokedReasons -join ', ')"
    }

    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "AgentPolicy"; metadata = @{ name = "tool-runtime-grants"; namespace = $namespace }; spec = @{ scope = "agent"; enforce = $true; targetAgents = @($agentName); toolGrants = @(@{ toolRef = $toolName; allow = $true }) } }
    Wait-ForJsonPath -Resource "agentpolicy/tool-runtime-grants" -JsonPath "{.status.phase}" -Expected "Ready"
    $quotaInstanceName = "$instanceName-quota-exhausted"
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "AgentInstance"; metadata = @{ name = $quotaInstanceName; namespace = $namespace }; spec = @{ agentRef = @{ name = $agentName }; revision = 1; sessionContext = @{ input = "quota-exhaustion: What is 17 * 23? Use the calculator." } } }
    Wait-ForJsonPath -Resource "agentinstance/$quotaInstanceName" -JsonPath "{.status.phase}" -Expected "Running"
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "CognitiveQuota"; metadata = @{ name = $quotaName; namespace = $namespace }; spec = @{ hard = @{}; costBudget = @{ maxCostPerPeriod = 0; maxToolCallsPerPeriod = 1 } } }
    $quotaInstance = Wait-ForAgentInstanceCompletion -Name $quotaInstanceName
    $quotaDetails = @($quotaInstance.status.toolCallsDetail)
    if ($quotaDetails.Count -ne 1 -or $quotaDetails[0].name -ne "sandbox_calculator" -or $quotaDetails[0].result -notmatch "403") {
        throw "Runtime Tool Gateway did not surface quota exhaustion to the Tool call detail: $($quotaDetails | ConvertTo-Json -Compress)"
    }
    $quotaReasons = @(((& kubectl get events -n $namespace --field-selector "involvedObject.kind=AgentInstance,involvedObject.name=$quotaInstanceName" -o json | ConvertFrom-Json).items).reason)
    if ($quotaReasons -contains "ToolInvocationAuthorized" -or $quotaReasons -contains "ToolInvocationCompleted") {
        throw "Quota-exhausted runtime Tool call persisted execution lifecycle events: $($quotaReasons -join ', ')"
    }
    $toolCallsUsed = (& kubectl get "cognitivequota/$quotaName" -n $namespace -o jsonpath='{.status.costUsage.toolCalls}' | Out-String).Trim()
    if ($toolCallsUsed -ne "1") { throw "CognitiveQuota tool calls = '$toolCallsUsed', want exactly 1 successful Tool execution" }

    Write-Host "Runtime received the AgentIdentity SPIFFE URI in session status, invoked the Tool Gateway through a capability exchange, and enforced live AgentPolicy revocation and CognitiveQuota exhaustion before subsequent Tool executions." -ForegroundColor Green
    Write-Host "=== PASS ===" -ForegroundColor Green
}
catch {
    Write-Warning "Runtime Tool Gateway E2E failed: $($_.Exception.Message)"
    Write-Host "=== FAILURE DIAGNOSTICS: RESOURCES ===" -ForegroundColor Yellow
    & kubectl get models,modelclasses,secretrefs,pods,deployments,services -n $namespace -o wide 2>&1 | Out-Host
    Write-Host "=== FAILURE DIAGNOSTICS: EVENTS ===" -ForegroundColor Yellow
    & kubectl get events -n $namespace --sort-by=.metadata.creationTimestamp 2>&1 | Out-Host
    throw
}
finally {
    if ($runtimeApprovalJob) {
        Stop-Job $runtimeApprovalJob -ErrorAction SilentlyContinue
        Remove-Job $runtimeApprovalJob -Force -ErrorAction SilentlyContinue
    }
    if ($runtimePortForward) {
        try {
            Stop-Job $runtimePortForward -ErrorAction Stop
            Remove-Job $runtimePortForward -Force -ErrorAction Stop
        }
        catch {
            Write-Warning "Failed to clean up runtime port-forward job $($runtimePortForward.Id): $($_.Exception.Message)"
        }
    }
    & kubectl delete namespace $namespace --ignore-not-found --wait=false 2>$null | Out-Null
}