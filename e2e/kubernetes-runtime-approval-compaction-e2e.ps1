# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

param(
    [string]$KubernetesContext = "",
    [string]$ControllerNamespace = "kognition-system",
    [string]$ReleaseName = "kognition"
)

# ADR-0014 RuntimeApproval compaction E2E. This test creates a terminal live
# approval and its shared quota ticket while the controller is paused, then
# proves that the deployed reconcilers replace the live object with a bounded
# tombstone and transition the same ticket from Live to Compacted.

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$controllerDeployment = "$ReleaseName-k8s-controller"
$webhookConfigName = "$ReleaseName-validating"
$controllerReplicas = $null
$originalFailurePolicies = $null
$runID = [string][DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds()
$namespace = "runtime-approval-compaction-$runID"
$invocationID = "compaction-$runID"
$approvalName = $null
$kubectlContextArguments = if ($KubernetesContext) { @("--context", $KubernetesContext) } else { @() }

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

    $Object | ConvertTo-Json -Depth 20 -Compress | & kubectl @kubectlContextArguments apply -f - 2>&1 | Out-Host
    if ($LASTEXITCODE -ne 0) {
        throw "apply $($Object.kind)/$($Object.metadata.name) failed"
    }
}

function Get-RuntimeApprovalName {
    param(
        [Parameter(Mandatory = $true)][string]$Workspace,
        [Parameter(Mandatory = $true)][string]$Invocation
    )

    $inputValue = "$($Workspace.Length):$Workspace$($Invocation.Length):$Invocation"
    $hash = [Security.Cryptography.SHA256]::HashData([Text.Encoding]::UTF8.GetBytes($inputValue))
    $alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"
    $bits = 0
    $buffer = 0
    $encoded = [Text.StringBuilder]::new()
    foreach ($value in $hash) {
        $buffer = ($buffer -shl 8) -bor $value
        $bits += 8
        while ($bits -ge 5) {
            $bits -= 5
            [void]$encoded.Append($alphabet[($buffer -shr $bits) -band 31])
        }
    }
    if ($bits -gt 0) {
        [void]$encoded.Append($alphabet[($buffer -shl (5 - $bits)) -band 31])
    }
    return "ra-$($encoded.ToString().Substring(0, 52).ToLowerInvariant())"
}

function Set-WebhookFailurePolicyIgnore {
    $config = (Invoke-Kubectl @("get", "validatingwebhookconfiguration", $webhookConfigName, "-o", "json")) -join "`n" | ConvertFrom-Json
    $originals = @{}
    $patchEntries = @()
    for ($index = 0; $index -lt $config.webhooks.Count; $index++) {
        $originals[$config.webhooks[$index].name] = [string]$config.webhooks[$index].failurePolicy
        $patchEntries += @{ op = "replace"; path = "/webhooks/$index/failurePolicy"; value = "Ignore" }
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
    for ($index = 0; $index -lt $config.webhooks.Count; $index++) {
        $name = [string]$config.webhooks[$index].name
        if ($Originals.ContainsKey($name)) {
            $patchEntries += @{ op = "replace"; path = "/webhooks/$index/failurePolicy"; value = $Originals[$name] }
        }
    }
    if ($patchEntries.Count -gt 0) {
        $patchFile = [IO.Path]::GetTempFileName()
        try {
            ConvertTo-Json -InputObject $patchEntries -Compress -Depth 5 | Set-Content -Path $patchFile -NoNewline
            Invoke-Kubectl @("patch", "validatingwebhookconfiguration", $webhookConfigName, "--type=json", "--patch-file", $patchFile) | Out-Null
        }
        finally {
            Remove-Item -Path $patchFile -Force -ErrorAction SilentlyContinue
        }
    }
}

function Wait-ForControllerScale {
    param([Parameter(Mandatory = $true)][int]$Replicas)

    if ($Replicas -eq 0) {
        $deadline = [DateTime]::UtcNow.AddSeconds(120)
        do {
            $ready = (& kubectl @kubectlContextArguments get "deployment/$controllerDeployment" -n $ControllerNamespace -o jsonpath='{.status.readyReplicas}' | Out-String).Trim()
            if (-not $ready -or [int]$ready -eq 0) { return }
            [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
        } while ([DateTime]::UtcNow -lt $deadline)
        throw "$controllerDeployment did not scale down within 120 seconds"
    }
    Invoke-Kubectl @("wait", "-n", $ControllerNamespace, "--for=condition=Available", "deployment/$controllerDeployment", "--timeout=120s") | Out-Null
}

function Wait-ForCompaction {
    $deadline = [DateTime]::UtcNow.AddSeconds(120)
    do {
        & kubectl @kubectlContextArguments get "runtimeapproval/$approvalName" -n $namespace 2>$null | Out-Null
        $liveExists = $LASTEXITCODE -eq 0
        $tombstoneJson = & kubectl @kubectlContextArguments get "runtimeapprovaltombstone/$approvalName" -n $namespace -o json 2>$null
        $tombstoneExists = $LASTEXITCODE -eq 0
        $ticketJson = & kubectl @kubectlContextArguments get "runtimeapprovalquotaticket/$approvalName" -n $namespace -o json 2>$null
        $ticketExists = $LASTEXITCODE -eq 0
        if (-not $liveExists -and $tombstoneExists -and $ticketExists) {
            $tombstone = ($tombstoneJson -join "`n") | ConvertFrom-Json
            $ticket = ($ticketJson -join "`n") | ConvertFrom-Json
            if ($ticket.spec.phase -eq "Compacted" -and $ticket.spec.compactedAt -and $ticket.spec.retentionDeadline) {
                return @{ Tombstone = $tombstone; Ticket = $ticket }
            }
        }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "RuntimeApproval $namespace/$approvalName did not compact within 120 seconds"
}

Write-Host "=== Kognition RuntimeApproval Compaction E2E ===" -ForegroundColor Cyan

try {
    Invoke-Kubectl @("get", "deployment/$controllerDeployment", "-n", $ControllerNamespace) | Out-Null
    foreach ($crd in @("runtimeapprovals.kognition.dev", "runtimeapprovaltombstones.kognition.dev", "runtimeapprovalquotatickets.kognition.dev")) {
        Invoke-Kubectl @("get", "crd/$crd") | Out-Null
    }
    Invoke-Kubectl @("create", "namespace", $namespace) | Out-Null
    $approvalName = Get-RuntimeApprovalName -Workspace $namespace -Invocation $invocationID
    $createdAt = [DateTime]::UtcNow.AddMinutes(-1).ToString("o")
    $expiresAt = [DateTime]::UtcNow.AddHours(1).ToString("o")
    $binding = @{
        format = "kognition-runtime-action-v1"
        algorithm = "HMAC-SHA-256"
        domain = "kognition.dev/runtime-approval/action/v1"
        epoch = 1
        keyVersion = 1
        tag = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
    }
    $agentRef = @{ kind = "Agent"; scope = "Namespaced"; namespace = $namespace; name = "compaction-agent"; uid = "agent-uid-$runID"; resourceVersion = 1 }
    $reference = { param($Kind, $Name, $UID) @{ kind = $Kind; scope = "Namespaced"; namespace = $namespace; name = $Name; uid = $UID; resourceVersion = 1 } }

    $originalFailurePolicies = Set-WebhookFailurePolicyIgnore
    $controllerReplicas = [int]((& kubectl @kubectlContextArguments get "deployment/$controllerDeployment" -n $ControllerNamespace -o jsonpath='{.spec.replicas}' | Out-String).Trim())
    if ($controllerReplicas -lt 1) { $controllerReplicas = 1 }
    Invoke-Kubectl @("scale", "deployment/$controllerDeployment", "-n", $ControllerNamespace, "--replicas=0") | Out-Null
    Wait-ForControllerScale -Replicas 0

    Apply-KubernetesObject @{
        apiVersion = "kognition.dev/v1alpha1"
        kind = "RuntimeApproval"
        metadata = @{ name = $approvalName; namespace = $namespace }
        spec = @{
            workspace = $namespace; invocationID = $invocationID; initialDecision = "NotRequired"; plane = "Tool"; operation = "invoke"
            agentRef = $agentRef
            agentIdentityRef = & $reference "AgentIdentity" "compaction-agent" "identity-uid-$runID"
            agentInstanceRef = & $reference "AgentInstance" "compaction-instance" "instance-uid-$runID"
            sessionRef = "compaction-session-$runID"
            targetRef = & $reference "Tool" "compaction-tool" "tool-uid-$runID"
            autonomyClassRef = @{ kind = "AutonomyClass"; scope = "Cluster"; name = "autonomous"; uid = "class-uid-$runID"; resourceVersion = 1 }
            authorizationRefs = @()
            binding = $binding; createdAt = $createdAt; expiresAt = $expiresAt
        }
    }
    $approvalUID = (& kubectl @kubectlContextArguments get "runtimeapproval/$approvalName" -n $namespace -o jsonpath='{.metadata.uid}' | Out-String).Trim()
    if (-not $approvalUID) { throw "RuntimeApproval did not receive a UID" }

    Apply-KubernetesObject @{
        apiVersion = "kognition.dev/v1alpha1"
        kind = "RuntimeApprovalQuotaTicket"
        metadata = @{ name = $approvalName; namespace = $namespace; finalizers = @("kognition.dev/runtimeapprovalquotaticket-retirement") }
        spec = @{
            workspace = $namespace; invocationID = $invocationID; runtimeApprovalUID = $approvalUID
            agentRef = $agentRef; binding = $binding; phase = "Live"; chargedBytes = 1024; createdAt = $createdAt
        }
    }
    $decidedAt = [DateTime]::UtcNow.ToString("o")
    $statusPatch = @{
        status = @{
            decisionState = "NotRequired"
            decisionEvidenceDelivered = $true
            decisionAudit = @{
                id = "rad-$runID"
                payload = @{
                    workspace = $namespace
                    invocationID = $invocationID
                    approvalUID = $approvalUID
                    decision = "NotRequired"
                    decidedAt = $decidedAt
                }
            }
            decidedAt = $decidedAt
            executionState = "Completed"
            executionDisposition = "Succeeded"
        }
    } | ConvertTo-Json -Depth 6 -Compress
    Invoke-Kubectl @("patch", "runtimeapproval/$approvalName", "-n", $namespace, "--subresource=status", "--type=merge", "-p", $statusPatch) | Out-Null

    Invoke-Kubectl @("scale", "deployment/$controllerDeployment", "-n", $ControllerNamespace, "--replicas=$controllerReplicas") | Out-Null
    Wait-ForControllerScale -Replicas $controllerReplicas
    $result = Wait-ForCompaction

    $tombstone = $result.Tombstone
    $ticket = $result.Ticket
    if ($tombstone.spec.workspace -ne $namespace -or $tombstone.spec.invocationID -ne $invocationID -or
        $tombstone.spec.runtimeApprovalUID -ne $approvalUID -or $tombstone.spec.binding.tag -ne $binding.tag -or
        $tombstone.spec.decisionState -ne "NotRequired" -or $tombstone.spec.executionState -ne "Completed" -or
        $tombstone.spec.executionDisposition -ne "Succeeded") {
        throw "RuntimeApprovalTombstone did not preserve the replay contract"
    }
    if ($ticket.spec.runtimeApprovalUID -ne $approvalUID -or $ticket.spec.phase -ne "Compacted" -or
        $ticket.spec.retentionDeadline -ne $tombstone.spec.retentionDeadline) {
        throw "RuntimeApprovalQuotaTicket did not remain shared across live and tombstone phases"
    }
    $retention = [DateTimeOffset]::Parse($tombstone.spec.retentionDeadline) - [DateTimeOffset]::Parse($tombstone.spec.compactedAt)
    if ([Math]::Abs(($retention - [TimeSpan]::FromDays(90)).TotalSeconds) -gt 1) {
        throw "Tombstone retention was $retention, want 90 days"
    }

    Write-Host "Compacted $namespace/$approvalName into a 90-day replay tombstone while preserving one quota ticket." -ForegroundColor Green
    Write-Host "=== PASS ===" -ForegroundColor Green
}
finally {
    if ($approvalName) {
        & kubectl @kubectlContextArguments patch "runtimeapprovaltombstone/$approvalName" -n $namespace --type=merge -p '{"metadata":{"finalizers":[]}}' 2>$null | Out-Null
        & kubectl @kubectlContextArguments delete "runtimeapprovaltombstone/$approvalName" -n $namespace --ignore-not-found --wait=false 2>$null | Out-Null
        & kubectl @kubectlContextArguments patch "runtimeapprovalquotaticket/$approvalName" -n $namespace --type=merge -p '{"metadata":{"finalizers":[]}}' 2>$null | Out-Null
        & kubectl @kubectlContextArguments delete "runtimeapprovalquotaticket/$approvalName" -n $namespace --ignore-not-found --wait=false 2>$null | Out-Null
    }
    if ($controllerReplicas) {
        try {
            & kubectl @kubectlContextArguments scale "deployment/$controllerDeployment" -n $ControllerNamespace "--replicas=$controllerReplicas" 2>$null | Out-Null
            Wait-ForControllerScale -Replicas $controllerReplicas
        }
        catch { Write-Warning "Failed to restore $controllerDeployment to $controllerReplicas replicas: $_" }
    }
    if ($originalFailurePolicies) {
        try { Restore-WebhookFailurePolicy -Originals $originalFailurePolicies }
        catch { Write-Warning "Failed to restore webhook failurePolicies: $_" }
    }
    & kubectl @kubectlContextArguments delete namespace $namespace --ignore-not-found --wait=false 2>$null | Out-Null
}