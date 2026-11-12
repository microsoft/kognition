# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

param(
    [string]$ControllerNamespace = "kognition-system",
    [string]$ReleaseName = "kognition"
)

# ADR-0014 Runtime Autonomy Gate 3 — TC-0172 Kubernetes-mode E2E.
#
# Scenario: Agent bound to a TrustProfile with initialClass="assisted".
# The built-in "assisted" AutonomyClass sets ApprovalPolicy.ToolInvocation
# ="high-risk-only". The Tool under test carries no risk classification.
# The resolver's EvaluateActionPolicy returns Pending for "high-risk-only"
# whenever the caller cannot conclusively prove low risk, so every Tool
# invocation must persist a RuntimeApproval CR with
# status.decisionState=Pending and MUST NOT emit ToolInvocationAuthorized
# nor ToolInvocationCompleted events on the AgentInstance.
#
# Persistence-only per BACKLOG.md wording: this test verifies Gate 3 stops
# the invocation before target execution and durably records the pending
# decision under the TrustProfile-selected AutonomyClass. After all assertions,
# teardown rejects the approval through the trusted Tool Gateway identity and
# waits for normal decision delivery and compaction.

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$runID = "{0}-{1}" -f [DateTimeOffset]::UtcNow.ToString("yyyyMMddHHmmssfff"), [Guid]::NewGuid().ToString("N").Substring(0, 8)
$namespace = "tool-gateway-tc0172-e2e-$runID"
$gatewayService = "$ReleaseName-tool-gateway"

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

function Wait-ForReadyServiceEndpoint {
    param([Parameter(Mandatory = $true)][string]$Service)

    $deadline = [DateTime]::UtcNow.AddSeconds(30)
    do {
        $endpointSlices = ((Invoke-Kubectl @("get", "endpointslice", "-n", $namespace, "-l", "kubernetes.io/service-name=$Service", "-o", "json")) -join "`n" | ConvertFrom-Json).items
        $readyEndpoints = @($endpointSlices | ForEach-Object { $_.endpoints } | Where-Object {
            if ($null -eq $_) { return $false }
            $conditions = $_.PSObject.Properties["conditions"]
            if ($null -eq $conditions -or $null -eq $conditions.Value) { return $true }
            $ready = $conditions.Value.PSObject.Properties["ready"]
            return $null -eq $ready -or $ready.Value -ne $false
        })
        if ($readyEndpoints.Count -gt 0) { return }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Service $Service did not publish a ready EndpointSlice within 30 seconds"
}

function Wait-ForPendingRuntimeApproval {
    param(
        [Parameter(Mandatory = $true)][string]$AgentName,
        [Parameter(Mandatory = $true)][string]$ToolName,
        [Parameter(Mandatory = $true)][string]$ExpectedClass,
        [int]$TimeoutSeconds = 180
    )

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        $list = ((Invoke-Kubectl @("get", "runtimeapprovals", "-n", $namespace, "-o", "json")) -join "`n" | ConvertFrom-Json).items
        foreach ($ra in $list) {
            if ($ra.spec.plane -ne "Tool") { continue }
            if ($ra.spec.operation -ne "invoke") { continue }
            if ($ra.spec.agentRef.name -ne $AgentName) { continue }
            if ($ra.spec.targetRef.name -ne $ToolName) { continue }
            if ($ra.status.decisionState -ne "Pending") { continue }
            if ($ra.spec.autonomyClassRef.name -ne $ExpectedClass) {
                throw "RuntimeApproval $($ra.metadata.name) bound to AutonomyClass '$($ra.spec.autonomyClassRef.name)', want '$ExpectedClass'"
            }
            return $ra
        }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Pending RuntimeApproval for Agent=$AgentName Tool=$ToolName did not appear within $TimeoutSeconds seconds"
}

function Complete-RuntimeApprovalsForCleanup {
    $approvals = ((Invoke-Kubectl @("get", "runtimeapprovals", "-n", $namespace, "-o", "json")) -join "`n" | ConvertFrom-Json).items
    foreach ($approval in @($approvals)) {
        if ($approval.status.decisionState -ne "Pending") { continue }

        $decidedAt = [DateTime]::UtcNow.ToString("o")
        $patch = @{ status = @{
            decisionState = "Rejected"
            decisionEvidenceDelivered = $true
            decidedByIssuer = "kognition-e2e"
            decidedBySubject = "test-cleanup"
            decidedAt = $decidedAt
            decisionAudit = @{
                id = "cleanup-$($approval.metadata.uid)"
                payload = @{
                    workspace = $namespace
                    invocationID = $approval.spec.invocationID
                    approvalUID = $approval.metadata.uid
                    decision = "Rejected"
                    issuer = "kognition-e2e"
                    subject = "test-cleanup"
                    decidedAt = $decidedAt
                }
            }
        } } | ConvertTo-Json -Depth 10 -Compress
        $patchFile = [IO.Path]::GetTempFileName()
        try {
            Set-Content -Path $patchFile -Value $patch -NoNewline
            Invoke-Kubectl @("--as=system:serviceaccount:$ControllerNamespace`:$gatewayService", "patch", "runtimeapproval/$($approval.metadata.name)", "-n", $namespace, "--subresource=status", "--type=merge", "--patch-file", $patchFile) | Out-Null
        }
        finally {
            Remove-Item -Path $patchFile -Force -ErrorAction SilentlyContinue
        }
        $approvalName = $approval.metadata.name
        Invoke-Kubectl @("wait", "-n", $namespace, "--for=delete", "runtimeapproval/$approvalName", "--timeout=60s") | Out-Null

        Invoke-Kubectl @("delete", "runtimeapprovaltombstone/$approvalName", "-n", $namespace, "--wait=false") | Out-Null
        Invoke-Kubectl @("patch", "runtimeapprovaltombstone/$approvalName", "-n", $namespace, "--type=merge", "-p", '{"metadata":{"finalizers":[]}}') | Out-Null
        Invoke-Kubectl @("wait", "-n", $namespace, "--for=delete", "runtimeapprovaltombstone/$approvalName", "--timeout=60s") | Out-Null

        Invoke-Kubectl @("delete", "runtimeapprovalquotaticket/$approvalName", "-n", $namespace, "--wait=false") | Out-Null
        Invoke-Kubectl @("wait", "-n", $namespace, "--for=delete", "runtimeapprovalquotaticket/$approvalName", "--timeout=60s") | Out-Null
    }
}

Write-Host "=== Kognition Tool Gateway Gate 3 TC-0172 E2E ===" -ForegroundColor Cyan

try {
    Invoke-Kubectl @("get", "deployment/$ReleaseName-tool-gateway", "-n", $ControllerNamespace) | Out-Null
    Invoke-Kubectl @("wait", "-n", $ControllerNamespace, "--for=condition=Available", "deployment/$ReleaseName-tool-gateway", "--timeout=120s") | Out-Null
    Invoke-Kubectl @("get", "deployment/tool-sandbox", "-n", "default") | Out-Null
    Invoke-Kubectl @("wait", "-n", "default", "--for=condition=Available", "deployment/tool-sandbox", "--timeout=120s") | Out-Null
    Invoke-Kubectl @("create", "namespace", $namespace) | Out-Null

    $modelClassName = "mock-openai-class"
    $modelName = "mock-tool-model"
    $agentName = "tc0172-agent"
    $instanceName = "tc0172-instance"
    $toolName = "sandbox-calculator"
    $quotaName = "tc0172-quota"
    $trustProfileName = "tc0172-trust-profile"

    Apply-KubernetesObject @{ apiVersion = "v1"; kind = "ConfigMap"; metadata = @{ name = "mock-openai-source"; namespace = $namespace }; data = @{ "server.py" = @'
import json
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
        has_tool_result = any(message.get("role") == "tool" for message in request.get("messages", []))
        if has_tool_result:
            message = {"role": "assistant", "content": "Done."}
            finish_reason = "stop"
        else:
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

    # Tool intentionally omits any risk classification metadata so that the
    # resolver, evaluating an "assisted" AutonomyClass whose
    # ApprovalPolicy.ToolInvocation="high-risk-only", cannot prove low risk
    # and MUST gate the invocation to Pending.
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "Tool"; metadata = @{ name = $toolName; namespace = $namespace }; spec = @{ className = "sandbox-openapi"; protocol = "OpenAPI"; description = "Evaluate arithmetic."; endpoint = "http://tool-sandbox.samples-tool-sandbox.svc.cluster.local:8080/calculator"; metadata = @{ allowPrivateNetwork = "true" }; inputSchema = @{ type = "object"; properties = @{ expression = @{ type = "string" } }; required = @("expression") } } }
    Wait-ForJsonPath -Resource "tool/$toolName" -JsonPath "{.status.phase}" -Expected "Ready"
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "Agent"; metadata = @{ name = $agentName; namespace = $namespace }; spec = @{ modelRef = @{ name = $modelName }; instructions = @{ inline = "Use sandbox_calculator for arithmetic and report its result." }; tools = @(@{ ref = @{ name = $toolName } }) } }
    Wait-ForJsonPath -Resource "agentidentity/agent-identity-$agentName" -JsonPath "{.status.phase}" -Expected "Ready"

    # Capture the live Agent incarnation UID so we can bind a TrustProfile to
    # it — the TrustProfile validator enforces immutable agentIncarnationUID
    # matching the current Agent.
    $agentUID = (Invoke-Kubectl @("get", "agent/$agentName", "-n", $namespace, "-o", "jsonpath={.metadata.uid}") | Out-String).Trim()
    if (-not $agentUID) { throw "Agent $agentName has no metadata.uid" }

    # Bind the Agent to the built-in "assisted" AutonomyClass. The k8s-controller
    # seeds "supervised", "assisted", and "autonomous" via a leader-elected
    # runnable; the validator resolves "assisted" through the "system" bucket.
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "TrustProfile"; metadata = @{ name = $trustProfileName; namespace = $namespace }; spec = @{ agentRef = @{ name = $agentName }; agentIncarnationUID = $agentUID; initialClass = "assisted"; initialScore = 50 } }
    Wait-ForJsonPath -Resource "trustprofile/$trustProfileName" -JsonPath "{.status.currentClass}" -Expected "assisted"

    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "ModelGrant"; metadata = @{ name = "tc0172-model"; namespace = $namespace }; spec = @{ agentRef = @{ name = $agentName }; modelRef = @{ name = $modelName } } }
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "AgentPolicy"; metadata = @{ name = "tc0172-grants"; namespace = $namespace }; spec = @{ scope = "agent"; enforce = $true; targetAgents = @($agentName); toolGrants = @(@{ toolRef = $toolName; allow = $true }) } }
    Wait-ForJsonPath -Resource "agentpolicy/tc0172-grants" -JsonPath "{.status.phase}" -Expected "Ready"
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "AgentRole"; metadata = @{ name = "tc0172-invoker"; namespace = $namespace }; rules = @(@{ resources = @("Tool"); verbs = @("get", "invoke") }) }
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "AgentRoleBinding"; metadata = @{ name = "tc0172-invoker"; namespace = $namespace }; spec = @{ roleRef = @{ name = "tc0172-invoker" }; subjects = @(@{ kind = "Agent"; name = $agentName }) } }
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "CognitiveQuota"; metadata = @{ name = $quotaName; namespace = $namespace }; spec = @{ hard = @{}; costBudget = @{ maxCostPerPeriod = 0; maxToolCallsPerPeriod = 10 } } }
    Wait-ForReadyServiceEndpoint -Service "agent-$agentName"

    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "AgentInstance"; metadata = @{ name = $instanceName; namespace = $namespace }; spec = @{ agentRef = @{ name = $agentName }; revision = 1; sessionContext = @{ input = "What is 17 * 23? Use the calculator." } } }

    $approval = Wait-ForPendingRuntimeApproval -AgentName $agentName -ToolName $toolName -ExpectedClass "assisted" -TimeoutSeconds 180
    Write-Host "Pending RuntimeApproval $($approval.metadata.name) bound to AutonomyClass assisted (Agent=$agentName Tool=$toolName)." -ForegroundColor Green

    if ($approval.status.executionState -ne "NotStarted") {
        throw "RuntimeApproval $($approval.metadata.name) executionState = '$($approval.status.executionState)', want 'NotStarted' (Gate 3 must block execution before target dispatch)"
    }
    if ($approval.spec.workspace -ne $namespace) {
        throw "RuntimeApproval $($approval.metadata.name) workspace = '$($approval.spec.workspace)', want '$namespace'"
    }
    if (-not $approval.spec.invocationID) {
        throw "RuntimeApproval $($approval.metadata.name) is missing spec.invocationID"
    }

    # Allow the runtime a brief window to (incorrectly) emit lifecycle events
    # before we assert their absence. 5s is well beyond the reconciler cadence.
    [Threading.Tasks.Task]::Delay(5000).GetAwaiter().GetResult()

    $events = ((Invoke-Kubectl @("get", "events", "-n", $namespace, "--field-selector", "involvedObject.kind=AgentInstance,involvedObject.name=$instanceName", "-o", "json")) -join "`n" | ConvertFrom-Json).items
    $reasons = @($events | ForEach-Object { $_.reason })
    if ($reasons -contains "ToolInvocationAuthorized") {
        throw "Gate 3 breach: ToolInvocationAuthorized event emitted for pending-gated Tool invocation. Reasons: $($reasons -join ', ')"
    }
    if ($reasons -contains "ToolInvocationCompleted") {
        throw "Gate 3 breach: ToolInvocationCompleted event emitted for pending-gated Tool invocation. Reasons: $($reasons -join ', ')"
    }

    Write-Host "Verified: no ToolInvocationAuthorized/Completed events on AgentInstance $instanceName." -ForegroundColor Green
    Write-Host "Gate 3 blocked execution and persisted the pending decision durably under TrustProfile-selected assisted class." -ForegroundColor Green
    Write-Host "=== PASS ===" -ForegroundColor Green
}
finally {
    if ($env:KOGNITION_E2E_KEEP_NS -eq "1") {
        Write-Host "KOGNITION_E2E_KEEP_NS=1: preserving namespace $namespace for diagnostics." -ForegroundColor Yellow
    } else {
        & kubectl get namespace $namespace 2>$null | Out-Null
        if ($LASTEXITCODE -eq 0) {
            Complete-RuntimeApprovalsForCleanup
            Invoke-Kubectl @("delete", "namespace", $namespace, "--wait=true", "--timeout=180s") | Out-Null
        }
    }
}
