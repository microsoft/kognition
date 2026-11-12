# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

param(
    [Parameter(Mandatory = $true)][string]$DemoRunId,
    [Parameter(Mandatory = $true)][string]$ScopeNamespace,
    [string]$KubernetesContext = "",
    [string]$ControllerNamespace = "kognition-system",
    [string]$ReleaseName = "kognition",
    [switch]$CommunicationOnly,
    [switch]$KeepScopeNamespace
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

. (Join-Path $PSScriptRoot "capability-mint.ps1")
. (Join-Path $PSScriptRoot "gateway-client-tracing.ps1")

$kubectlContextArguments = if ($KubernetesContext) { @("--context", $KubernetesContext) } else { @() }
$modelClassName = "trust-profile-$DemoRunId"
$modelName = "trust-profile-model"
$sourceAgent = "trust-profile-source"
$targetAgent = "trust-profile-reviewer"
$sourceInstance = "trust-profile-source"
$targetService = "trust-profile-review"
$communicationPolicy = "trust-profile-communication"
$providerName = "trust-profile-provider"
$providerPolicy = "trust-profile-provider-$DemoRunId"
$modelGatewayService = "$ReleaseName-model-gateway"
$communicationGatewayService = "$ReleaseName-agent-communication-gateway"
$modelCapabilityHelper = Join-Path ([IO.Path]::GetTempPath()) "kognition-trust-profile-model-$DemoRunId-$PID.exe"
$modelForward = $null
$communicationForward = $null
$phasePassed = $false

function Invoke-Kubectl {
    param([Parameter(Mandatory = $true)][string[]]$Arguments)

    $output = & kubectl @kubectlContextArguments @Arguments 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "kubectl $($Arguments -join ' ') failed:`n$($output -join "`n")"
    }
    return $output
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
        [int]$TimeoutSeconds = 120
    )

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        $value = (& kubectl @kubectlContextArguments get $Resource -n $ScopeNamespace "-o=jsonpath=$JsonPath" 2>$null | Out-String).Trim()
        if ($value -eq $Expected) { return }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "$Resource did not reach $JsonPath=$Expected within $TimeoutSeconds seconds"
}

function Get-AvailableTcpPort {
    $listener = [Net.Sockets.TcpListener]::new([Net.IPAddress]::Loopback, 0)
    try {
        $listener.Start()
        return ([Net.IPEndPoint]$listener.LocalEndpoint).Port
    }
    finally {
        $listener.Stop()
    }
}

function Start-KubectlPortForward {
    param(
        [Parameter(Mandatory = $true)][string]$Namespace,
        [Parameter(Mandatory = $true)][string]$Service,
        [Parameter(Mandatory = $true)][int]$RemotePort
    )

    $localPort = Get-AvailableTcpPort
    $job = Start-Job -ScriptBlock {
        param($Context, $Namespace, $Service, $LocalPort, $RemotePort)
        $contextArguments = if ($Context) { @("--context", $Context) } else { @() }
        & kubectl @contextArguments -n $Namespace port-forward "service/$Service" "${LocalPort}:$RemotePort"
    } -ArgumentList $KubernetesContext, $Namespace, $Service, $localPort, $RemotePort

    $url = "http://127.0.0.1:$localPort"
    $deadline = [DateTime]::UtcNow.AddSeconds(30)
    do {
        try {
            $response = Invoke-WebRequest -Uri "$url/healthz" -TimeoutSec 2 -SkipHttpErrorCheck
            if ($response.StatusCode -eq 200) {
                return [PSCustomObject]@{ Job = $job; URL = $url }
            }
        }
        catch {
        }
        if ($job.State -ne "Running") { break }
        [Threading.Tasks.Task]::Delay(250).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)

    $details = @(Receive-Job $job -Keep -ErrorAction SilentlyContinue) -join "`n"
    Stop-Job $job -ErrorAction SilentlyContinue
    Remove-Job $job -Force -ErrorAction SilentlyContinue
    throw "Port-forward for service/$Service did not become ready: $details"
}

function Invoke-GatewayPost {
    param(
        [Parameter(Mandatory = $true)][string]$Uri,
        [Parameter(Mandatory = $true)][string]$Capability,
        [Parameter(Mandatory = $true)][string]$Body,
        [Parameter(Mandatory = $true)][int]$ExpectedStatus
    )

    $headers = @{
        Authorization = "Bearer $Capability"
        "X-Kognition-Run-ID" = $DemoRunId
    }
    $response = Invoke-TracedGatewayRequest -RequestParameters @{ Uri = $Uri; Method = "Post"; Headers = $headers; ContentType = "application/json"; Body = $Body; SkipHttpErrorCheck = $true }
    if ($response.StatusCode -ne $ExpectedStatus) {
        throw "POST $Uri returned $($response.StatusCode), want $ExpectedStatus. Body: $($response.Content)"
    }
    return $response
}

function Get-OptionalProperty {
    param([AllowNull()][object]$Object, [Parameter(Mandatory = $true)][string]$Name)

    if ($null -eq $Object) { return $null }
    $property = $Object.PSObject.Properties[$Name]
    if ($null -eq $property) { return $null }
    return $property.Value
}

function Get-AuditEntries {
    $configMaps = (Invoke-Kubectl @("get", "configmaps", "-n", $ControllerNamespace, "-l", "kognition.dev/cognitive-audit=true", "-o", "json") | Out-String | ConvertFrom-Json).items
    $entries = @()
    foreach ($configMap in $configMaps) {
        $binaryData = Get-OptionalProperty -Object $configMap -Name "binaryData"
        $encoded = Get-OptionalProperty -Object $binaryData -Name "entries.json"
        if (-not $encoded) { continue }
        $entries += @([Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($encoded)) | ConvertFrom-Json)
    }
    return @($entries)
}

function Wait-ForCommunicationCompletionAudit {
    param([Parameter(Mandatory = $true)][string]$InvocationID, [int]$TimeoutSeconds = 45)

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        $entry = Get-AuditEntries | Where-Object {
            $details = Get-OptionalProperty -Object $_ -Name "details"
            $requestDetails = Get-OptionalProperty -Object $details -Name "request"
            $execution = Get-OptionalProperty -Object $details -Name "execution"
            (Get-OptionalProperty -Object $_ -Name "eventType") -eq "communication" -and
                (Get-OptionalProperty -Object $_ -Name "workspace") -eq $ScopeNamespace -and
                (Get-OptionalProperty -Object $_ -Name "verb") -eq "invoke" -and
                (Get-OptionalProperty -Object $details -Name "runId") -eq $DemoRunId -and
                (Get-OptionalProperty -Object $requestDetails -Name "invocationId") -eq $InvocationID -and
                (Get-OptionalProperty -Object $details -Name "outcome") -eq "completed" -and
                (Get-OptionalProperty -Object $details -Name "reasonCode") -eq "target_completed" -and
                (Get-OptionalProperty -Object $execution -Name "status") -eq "completed" -and
                (Get-OptionalProperty -Object $execution -Name "targetReached") -eq $true
        } | Select-Object -First 1
        if ($entry) { return $entry }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "Timed out waiting for exact terminal Communication audit evidence for invocation $InvocationID"
}

function Approve-RuntimeApproval {
    param(
        [Parameter(Mandatory = $true)][string]$Name,
        [Parameter(Mandatory = $true)][string]$InvocationID
    )

    $approval = Invoke-Kubectl @("get", "runtimeapproval/$Name", "-n", $ScopeNamespace, "-o", "json") | Out-String | ConvertFrom-Json
    if ($approval.spec.invocationID -ne $InvocationID -or $approval.status.decisionState -ne "Pending" -or $approval.status.executionState -ne "NotStarted") {
        throw "RuntimeApproval/$Name is not the expected pending Communication invocation: $($approval | ConvertTo-Json -Compress -Depth 10)"
    }
    $decidedAt = [DateTime]::UtcNow.ToString("o")
    $issuer = "kognition-hackathon-e2e"
    $subject = "trust-profile"
    $patch = @{ status = @{
        decisionState = "Approved"
        decidedByIssuer = $issuer
        decidedBySubject = $subject
        decidedAt = $decidedAt
        decisionAudit = @{
            id = "trust-profile-$PID"
            payload = @{
                workspace = $ScopeNamespace
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
        Invoke-Kubectl @("--as=system:serviceaccount:$ControllerNamespace`:$communicationGatewayService", "patch", "runtimeapproval/$Name", "-n", $ScopeNamespace, "--subresource=status", "--type=merge", "--patch-file", $patchFile) | Out-Null
    }
    finally {
        Remove-Item -Path $patchFile -Force -ErrorAction SilentlyContinue
    }
    Wait-ForJsonPath -Resource "runtimeapproval/$Name" -JsonPath "{.status.decisionState}" -Expected "Approved"
}

Write-Host "=== Hackathon Model and Communication trust phases ===" -ForegroundColor Cyan

try {
    if (-not $CommunicationOnly) {
        Invoke-Kubectl @("get", "deployment/$modelGatewayService", "-n", $ControllerNamespace) | Out-Null
    }
    Invoke-Kubectl @("get", "deployment/$communicationGatewayService", "-n", $ControllerNamespace) | Out-Null
    $requiredDeployments = @("deployment/$communicationGatewayService")
    if (-not $CommunicationOnly) { $requiredDeployments += "deployment/$modelGatewayService" }
    Invoke-Kubectl (@("wait", "-n", $ControllerNamespace, "--for=condition=Available") + $requiredDeployments + @("--timeout=120s")) | Out-Null

    if (-not $CommunicationOnly) {
    $previousGOOS = $env:GOOS
    $previousGOARCH = $env:GOARCH
    $previousCGOEnabled = $env:CGO_ENABLED
    try {
        $env:GOOS = "windows"
        $env:GOARCH = "amd64"
        $env:CGO_ENABLED = "0"
        & go build -o $modelCapabilityHelper ./e2e/mint-model-session-capability.go
        if ($LASTEXITCODE -ne 0) { throw "build Model Gateway capability helper failed" }
    }
    finally {
        if ($null -eq $previousGOOS) { Remove-Item Env:GOOS -ErrorAction SilentlyContinue } else { $env:GOOS = $previousGOOS }
        if ($null -eq $previousGOARCH) { Remove-Item Env:GOARCH -ErrorAction SilentlyContinue } else { $env:GOARCH = $previousGOARCH }
        if ($null -eq $previousCGOEnabled) { Remove-Item Env:CGO_ENABLED -ErrorAction SilentlyContinue } else { $env:CGO_ENABLED = $previousCGOEnabled }
    }
    }

    Apply-KubernetesObject @{ apiVersion = "v1"; kind = "ConfigMap"; metadata = @{ name = "$providerName-source"; namespace = $ScopeNamespace; labels = @{ "kognition.dev/demo-run" = $DemoRunId } }; data = @{ "server.py" = @'
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
            self.respond({"object":"list","data":[{"id":"trust-profile-model","object":"model"}]})
            return
        self.send_error(404)
    def do_POST(self):
        if self.path != "/v1/chat/completions":
            self.send_error(404)
            return
        self.respond({"id":"trust-profile","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"trust profile completed"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":2,"total_tokens":4}})
    def log_message(self, *_):
        pass

HTTPServer(("0.0.0.0", 8080), Handler).serve_forever()
'@ } }
    Apply-KubernetesObject @{ apiVersion = "apps/v1"; kind = "Deployment"; metadata = @{ name = $providerName; namespace = $ScopeNamespace; labels = @{ "kognition.dev/demo-run" = $DemoRunId } }; spec = @{ replicas = 1; selector = @{ matchLabels = @{ app = $providerName } }; template = @{ metadata = @{ labels = @{ app = $providerName; "kognition.dev/demo-run" = $DemoRunId } }; spec = @{ containers = @(@{ name = "provider"; image = "python:3.12-alpine"; command = @("python", "/app/server.py"); ports = @(@{ containerPort = 8080 }); readinessProbe = @{ httpGet = @{ path = "/v1/models"; port = 8080 }; initialDelaySeconds = 1; periodSeconds = 1 }; volumeMounts = @(@{ name = "source"; mountPath = "/app" }) }); volumes = @(@{ name = "source"; configMap = @{ name = "$providerName-source" } }) } } } }
    Apply-KubernetesObject @{ apiVersion = "v1"; kind = "Service"; metadata = @{ name = $providerName; namespace = $ScopeNamespace; labels = @{ "kognition.dev/demo-run" = $DemoRunId } }; spec = @{ selector = @{ app = $providerName }; ports = @(@{ port = 8080; targetPort = 8080 }) } }
    Invoke-Kubectl @("rollout", "status", "deployment/$providerName", "-n", $ScopeNamespace, "--timeout=120s") | Out-Null

    $isolatedComponents = @("model-gateway", "k8s-controller" | Where-Object {
        (Invoke-Kubectl @("get", "networkpolicy/$ReleaseName-$_", "-n", $ControllerNamespace, "--ignore-not-found", "-o", "name") | Out-String).Trim()
    })
    if ($isolatedComponents.Count -gt 0) {
    Apply-KubernetesObject @{ apiVersion = "networking.k8s.io/v1"; kind = "NetworkPolicy"; metadata = @{ name = $providerPolicy; namespace = $ControllerNamespace; labels = @{ "kognition.dev/demo-run" = $DemoRunId } }; spec = @{ podSelector = @{ matchLabels = @{ "app.kubernetes.io/instance" = $ReleaseName }; matchExpressions = @(@{ key = "app.kubernetes.io/component"; operator = "In"; values = $isolatedComponents }) }; policyTypes = @("Egress"); egress = @(
        @{ to = @(@{ namespaceSelector = @{ matchLabels = @{ "kubernetes.io/metadata.name" = "kube-system" } }; podSelector = @{ matchLabels = @{ "k8s-app" = "kube-dns" } } }); ports = @(@{ protocol = "UDP"; port = 53 }, @{ protocol = "TCP"; port = 53 }) },
        @{ to = @(@{ namespaceSelector = @{ matchLabels = @{ "kubernetes.io/metadata.name" = $ScopeNamespace } }; podSelector = @{ matchLabels = @{ app = $providerName } } }); ports = @(@{ protocol = "TCP"; port = 8080 }) }
    ) } }
    }

    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "SecretRef"; metadata = @{ name = "trust-profile-model-key"; namespace = $ScopeNamespace; annotations = @{ "kognition.dev/internal-secret-data" = '{"type":"api-key","apiKey":"trust-profile-provider-key"}' }; labels = @{ "kognition.dev/demo-run" = $DemoRunId } }; spec = @{ backendType = "internal"; backendRef = "secret/trust-profile-model-key"; key = "apiKey" } }
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "ModelClass"; metadata = @{ name = $modelClassName; labels = @{ "kognition.dev/demo-run" = $DemoRunId } }; spec = @{ provisioner = "openai-compatible" } }
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "Model"; metadata = @{ name = $modelName; namespace = $ScopeNamespace; labels = @{ "kognition.dev/demo-run" = $DemoRunId } }; spec = @{ className = $modelClassName; provider = "openai-compatible"; modelID = $modelName; endpoint = "http://$providerName.$ScopeNamespace.svc.cluster.local:8080/v1"; capabilities = @("chat"); credentialRef = @{ name = "trust-profile-model-key" } } }
    Wait-ForJsonPath -Resource "model/$modelName" -JsonPath "{.status.phase}" -Expected "Ready"

    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "Agent"; metadata = @{ name = $sourceAgent; namespace = $ScopeNamespace; labels = @{ "kognition.dev/demo-run" = $DemoRunId } }; spec = @{ modelRef = @{ name = $modelName }; instructions = @{ inline = "Create a run-scoped trust profile." } } }
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "Agent"; metadata = @{ name = $targetAgent; namespace = $ScopeNamespace; labels = @{ "kognition.dev/demo-run" = $DemoRunId } }; spec = @{ modelRef = @{ name = $modelName }; instructions = @{ inline = "Review the run-scoped trust profile." } } }
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "ModelGrant"; metadata = @{ name = "trust-profile-source-model"; namespace = $ScopeNamespace; labels = @{ "kognition.dev/demo-run" = $DemoRunId } }; spec = @{ agentRef = @{ name = $sourceAgent }; modelRef = @{ name = $modelName } } }
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "ModelGrant"; metadata = @{ name = "trust-profile-reviewer-model"; namespace = $ScopeNamespace; labels = @{ "kognition.dev/demo-run" = $DemoRunId } }; spec = @{ agentRef = @{ name = $targetAgent }; modelRef = @{ name = $modelName } } }
    Wait-ForJsonPath -Resource "agentidentity/agent-identity-$sourceAgent" -JsonPath "{.status.phase}" -Expected "Ready"

    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "AgentService"; metadata = @{ name = $targetService; namespace = $ScopeNamespace; labels = @{ "kognition.dev/demo-run" = $DemoRunId } }; spec = @{ agentRef = @{ name = $targetAgent }; port = 8080 } }
    Wait-ForJsonPath -Resource "agentservice/$targetService" -JsonPath "{.status.phase}" -Expected "Ready" -TimeoutSeconds 180
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "AgentCommunicationPolicy"; metadata = @{ name = $communicationPolicy; namespace = $ScopeNamespace; labels = @{ "kognition.dev/demo-run" = $DemoRunId } }; spec = @{ defaultDeny = $true; allowFrom = @(@{ name = $sourceAgent }); allowTo = @(@{ name = $targetAgent }) } }
    Apply-KubernetesObject @{ apiVersion = "kognition.dev/v1alpha1"; kind = "AgentInstance"; metadata = @{ name = $sourceInstance; namespace = $ScopeNamespace; labels = @{ "kognition.dev/demo-run" = $DemoRunId } }; spec = @{ agentRef = @{ name = $sourceAgent }; revision = 1 } }
    Wait-ForJsonPath -Resource "agentinstance/$sourceInstance" -JsonPath "{.status.phase}" -Expected "Running" -TimeoutSeconds 180

    $sourceUID = (Invoke-Kubectl @("get", "agentinstance/$sourceInstance", "-n", $ScopeNamespace, "-o", "jsonpath={.metadata.uid}") | Out-String).Trim()
    $sessionID = (Invoke-Kubectl @("get", "agentinstance/$sourceInstance", "-n", $ScopeNamespace, "-o", "jsonpath={.status.sessionID}") | Out-String).Trim()
    $modelVersion = (Invoke-Kubectl @("get", "model/$modelName", "-n", $ScopeNamespace, "-o", "jsonpath={.metadata.resourceVersion}") | Out-String).Trim()
    if (-not $sourceUID -or -not $sessionID -or -not $modelVersion) { throw "Source session did not expose the capability binding fields" }

    if (-not $CommunicationOnly) {
        $modelForward = Start-KubectlPortForward -Namespace $ControllerNamespace -Service $modelGatewayService -RemotePort 8093
        $modelKeyBase64 = (Invoke-Kubectl @("get", "secret/$modelGatewayService", "-n", $ControllerNamespace, "-o", "jsonpath={.data.capability-hmac-key}") | Out-String).Trim()
        $modelKeyIDBase64 = (Invoke-Kubectl @("get", "secret/$modelGatewayService", "-n", $ControllerNamespace, "-o", "jsonpath={.data.capability-hmac-key-id}") | Out-String).Trim()
        $modelKeyID = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($modelKeyIDBase64))
        $modelCapability = Invoke-CapabilityMint -KeyBase64 $modelKeyBase64 -FilePath $modelCapabilityHelper -ArgumentList @("-key-id", $modelKeyID, "-namespace", $ScopeNamespace, "-agent", $sourceAgent, "-identity", "agent-identity-$sourceAgent", "-instance", $sourceInstance, "-instance-uid", $sourceUID, "-session", $sessionID, "-model", $modelName, "-model-resource-version", $modelVersion)
        $modelBody = @{ model = "client-selected-model"; messages = @(@{ role = "user"; content = "Build the complete trust profile for $DemoRunId" }) } | ConvertTo-Json -Depth 10 -Compress
        Invoke-GatewayPost -Uri "$($modelForward.URL)/v1/chat/completions" -Capability $modelCapability -Body $modelBody -ExpectedStatus 200 | Out-Null
    }

    $communicationForward = Start-KubectlPortForward -Namespace $ControllerNamespace -Service $communicationGatewayService -RemotePort 8094
    $communicationKeyBase64 = (Invoke-Kubectl @("get", "secret/$communicationGatewayService", "-n", $ControllerNamespace, "-o", "jsonpath={.data.capability-hmac-key}") | Out-String).Trim()
    $communicationKeyIDBase64 = (Invoke-Kubectl @("get", "secret/$communicationGatewayService", "-n", $ControllerNamespace, "-o", "jsonpath={.data.capability-hmac-key-id}") | Out-String).Trim()
    $communicationKeyID = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($communicationKeyIDBase64))
    $sessionCapability = Invoke-CapabilityMint -KeyBase64 $communicationKeyBase64 -FilePath "go" -ArgumentList @("run", "./cmd/kognition-e2e-mint-agent-delegation", "-key-id", $communicationKeyID, "-namespace", $ScopeNamespace, "-agent", $sourceAgent, "-identity", "agent-identity-$sourceAgent", "-instance", $sourceInstance, "-instance-uid", $sourceUID, "-session", $sessionID)
    $invocationID = "$DemoRunId`:communication-$([Guid]::NewGuid().ToString('N'))"
    $communicationMessage = "Review the complete trust profile for $DemoRunId"
    $exchangeBody = @{ target_namespace = $ScopeNamespace; target_agent_service = $targetService; invocation_id = $invocationID; message = $communicationMessage } | ConvertTo-Json -Compress
    $pendingExchange = Invoke-GatewayPost -Uri "$($communicationForward.URL)/v1/agent-services/capabilities" -Capability $sessionCapability -Body $exchangeBody -ExpectedStatus 202
    $pending = $pendingExchange.Content | ConvertFrom-Json
    if ($pending.runtime_approval_state -ne "pending" -or -not $pending.runtime_approval -or -not $pending.runtime_approval.name) {
        throw "Communication capability exchange did not identify the pending RuntimeApproval: $($pendingExchange.Content)"
    }
    Approve-RuntimeApproval -Name $pending.runtime_approval.name -InvocationID $invocationID
    $exchange = Invoke-GatewayPost -Uri "$($communicationForward.URL)/v1/agent-services/capabilities" -Capability $sessionCapability -Body $exchangeBody -ExpectedStatus 200
    $exactCapability = ($exchange.Content | ConvertFrom-Json).capability
    if (-not $exactCapability) { throw "Communication capability exchange returned an empty exact capability" }
    $invokeBody = @{ target_namespace = $ScopeNamespace; target_agent_service = $targetService; invocation_id = $invocationID; message = $communicationMessage } | ConvertTo-Json -Compress
    Invoke-GatewayPost -Uri "$($communicationForward.URL)/v1/agent-services/invoke" -Capability $exactCapability -Body $invokeBody -ExpectedStatus 202 | Out-Null
    Wait-ForCommunicationCompletionAudit -InvocationID $invocationID | Out-Null

    $phasePassed = $true
    $completedPhases = if ($CommunicationOnly) { "Agent communication" } else { "Model execution and agent communication" }
    Write-Host "$completedPhases completed for run $DemoRunId." -ForegroundColor Green
}
finally {
    foreach ($forward in @($modelForward, $communicationForward)) {
        if ($null -ne $forward) {
            Stop-Job $forward.Job -ErrorAction SilentlyContinue
            Remove-Job $forward.Job -Force -ErrorAction SilentlyContinue
        }
    }
    if (Test-Path $modelCapabilityHelper) { Remove-Item $modelCapabilityHelper -Force -ErrorAction SilentlyContinue }
    if (-not $KeepScopeNamespace -and -not $CommunicationOnly) {
        & kubectl @kubectlContextArguments delete "networkpolicy/$providerPolicy" -n $ControllerNamespace --ignore-not-found 2>$null | Out-Null
        & kubectl @kubectlContextArguments delete "modelclass/$modelClassName" --ignore-not-found 2>$null | Out-Null
    }
}

if (-not $phasePassed) { throw "Hackathon Model and Communication trust phases did not complete" }