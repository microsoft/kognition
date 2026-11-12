# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest
$tokens = $null
$parseErrors = $null
$ast = [Management.Automation.Language.Parser]::ParseFile((Join-Path $PSScriptRoot "hackathon-trustworthiness-e2e.ps1"), [ref]$tokens, [ref]$parseErrors)
if ($parseErrors.Count) { throw ($parseErrors | Out-String) }
foreach ($name in @("Get-OptionalProperty", "Get-AuditRunID", "Assert-ApprovalEvidence")) {
    $helper = $ast.Find({ param($node) $node -is [Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq $name }, $true)
    . ([scriptblock]::Create($helper.Extent.Text))
}
$DemoRunId = "correlation-test"
$memoryPhaseReport = [PSCustomObject]@{ invocationIds = @{ allow = "attempt:allow"; deny = "attempt:deny"; revoked = "attempt:revoked" } }
$audits = @()
foreach ($case in @(
    @{ name = "allow"; verb = "store"; outcome = "completed"; reason = "target_completed" },
    @{ name = "deny"; verb = "store"; outcome = "denied"; reason = "grant_missing" },
    @{ name = "revoked"; verb = "search"; outcome = "revoked"; reason = "grant_revoked" }
)) {
    foreach ($variant in @("exact", "missing-invocation", "other-invocation", "other-run")) {
        $request = [PSCustomObject]@{}
        if ($variant -ne "missing-invocation") {
            $request | Add-Member invocationId $(if ($variant -eq "other-invocation") { "unrelated" } else { $memoryPhaseReport.invocationIds[$case.name] })
        }
        $audits += [PSCustomObject]@{
            eventType = "memory_op"; sessionId = "controller-assigned-session"; verb = $case.verb
            details = [PSCustomObject]@{
                runId = if ($variant -eq "other-run") { "unrelated" } else { $DemoRunId }
                request = $request; outcome = $case.outcome; reasonCode = $case.reason
                execution = @{ status = "completed"; targetReached = $true }
            }
        }
    }
}
foreach ($name in @("memoryAllowed", "memoryDenied", "memoryRevoked")) {
    $assignment = $ast.Find({ param($node) $node -is [Management.Automation.Language.AssignmentStatementAst] -and $node.Left.Extent.Text -ceq ('$' + $name) }, $true)
    . ([scriptblock]::Create($assignment.Extent.Text))
    $matched = @(Get-Variable -Name $name -ValueOnly)
    if ($matched.Count -ne 1 -or $matched[0].sessionId -ne "controller-assigned-session") { throw "$name did not match exactly the owned invocation" }
}
$keys = $ast.Find({ param($node) $node -is [Management.Automation.Language.HashtableAst] -and @($node.KeyValuePairs | ForEach-Object { $_.Item1.Extent.Text }) -contains "memoryAllow" }, $true)
$communicationCompleted = @([PSCustomObject]@{ details = @{ request = [PSCustomObject]@{ invocationId = "correlation-test:communication-actual-attempt" } } })
$reportedKeys = & ([scriptblock]::Create($keys.Extent.Text))
if ($reportedKeys.memoryAllow -ne "attempt:allow" -or $reportedKeys.memoryDeny -ne "attempt:deny" -or $reportedKeys.memoryRevoked -ne "attempt:revoked") { throw "Report replaced actual invocation IDs with synthesized values" }
if ($reportedKeys.communicationInvocation -ne "correlation-test:communication-actual-attempt") { throw "Report replaced actual Communication invocation ID" }
if ($reportedKeys.workflowApproval -ne "$DemoRunId`:workflow-approval" -or $reportedKeys.approvalDecision -ne "$DemoRunId`:approval-decision") { throw "Report lost workflow annotation keys" }
$communicationCompleted += $communicationCompleted[0]
$reportedKeys = & ([scriptblock]::Create($keys.Extent.Text))
if ($reportedKeys.communicationInvocation) { throw "Report selected an ambiguous Communication invocation" }
$memoryPhaseReport = $null
$communicationCompleted = @()
$reportedKeys = & ([scriptblock]::Create($keys.Extent.Text))
if ($reportedKeys.memoryAllow -or $reportedKeys.memoryDeny -or $reportedKeys.memoryRevoked -or $reportedKeys.communicationInvocation) { throw "Report invented invocation IDs without phase evidence" }
Write-Host "Memory and Communication identifiers use exact evidence, including real sessions, absent evidence, and ambiguous invocations."

$decision = '{"plane":"memory","outcome":"denied","reasonCode":"grant_missing","approval":{"status":"not_evaluated"},"execution":{"status":"blocked","targetReached":false}}' | ConvertFrom-Json
Assert-ApprovalEvidence $decision
foreach ($status in @("unavailable", "not_required", "pending", "approved", "rejected", "")) {
    $decision.approval.status = $status
    $rejected = $false
    try { Assert-ApprovalEvidence $decision } catch { $rejected = $true }
    if (-not $rejected) { throw "Accepted misleading approval status '$status'" }
}
$decision.approval.status = "not_evaluated"
$decision.execution.targetReached = $true
$rejected = $false
try { Assert-ApprovalEvidence $decision } catch { $rejected = $true }
if (-not $rejected) { throw "Accepted skipped approval after target execution" }
Write-Host "Approval acceptance rejects the historical unavailable state and contradictory execution evidence."

$resolved = '{"workspace":"fixture","plane":"memory","outcome":"completed","reasonCode":"target_completed","approval":{"status":"not_required","decisionRef":{"kind":"RuntimeApproval","namespace":"fixture","name":"approval","uid":"uid","resourceVersion":"1"}},"execution":{"status":"completed","targetReached":true}}' | ConvertFrom-Json
Assert-ApprovalEvidence $resolved
$resolved.outcome = "denied"
$resolved.reasonCode = "runtime_approval_pending"
$resolved.approval.status = "pending"
$resolved.execution.status = "blocked"
$resolved.execution.targetReached = $false
Assert-ApprovalEvidence $resolved
$resolved.approval.status = "not_required"
$rejected = $false
try { Assert-ApprovalEvidence $resolved } catch { $rejected = $true }
if (-not $rejected) { throw "Accepted pending approval as not_required" }
Write-Host "Durable not-required and pending denial evidence retain their resolved approval references."