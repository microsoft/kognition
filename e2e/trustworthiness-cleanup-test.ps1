# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest
$tokens = $null
$parseErrors = $null
$ast = [Management.Automation.Language.Parser]::ParseFile((Join-Path $PSScriptRoot "trustworthiness-e2e.ps1"), [ref]$tokens, [ref]$parseErrors)
if ($parseErrors.Count) { throw ($parseErrors | Out-String) }
& {
    $nativeHelper = $ast.Find({ param($node) $node -is [Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq "Invoke-Kubectl" }, $true)
    . ([scriptblock]::Create($nativeHelper.Extent.Text))
    $kubectlContextArguments = @()
    function kubectl { & $env:ComSpec /d /c 'echo Warning: deprecated resource 1>&2 & echo {"items":[]}' }
    $result = Invoke-Kubectl @("get", "test") | ConvertFrom-Json
    if (@($result.items).Count) { throw "Native stderr corrupted JSON output" }
}
$helper = $ast.Find({ param($node) $node -is [Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq "Remove-RehearsalScope" }, $true)
. ([scriptblock]::Create($helper.Extent.Text))
$namespace = "kognition-demo-cleanup-test"
$DemoRunId = "cleanup-test"
$ControllerNamespace = "kognition-system"
function Invoke-Kubectl {
    param([string[]]$Arguments)
    $script:calls.Add(($Arguments -join " "))
    if ($script:calls.Count -eq $script:failAt) { throw "Injected cleanup failure" }
    if ($Arguments[0] -eq "wait" -and $Arguments[2] -eq "namespace/$namespace" -and $script:retentionScenario) { throw "Timed out waiting for namespace deletion" }
    if ($Arguments[0] -eq "api-resources") { return @("runtimeapprovaltombstones.kognition.dev", "runtimeapprovalquotatickets.kognition.dev", "pods") }
    if ($Arguments[0] -eq "get" -and $Arguments[1] -eq "namespace/$namespace") {
        return @{ metadata = @{ deletionTimestamp = "2026-01-01T00:00:00Z" }; status = @{ phase = "Terminating" } } | ConvertTo-Json
    }
    if ($Arguments[0] -eq "get" -and $Arguments[1] -match "runtimeapprovaltombstones") { return @{ items = $script:records } | ConvertTo-Json -Depth 10 }
}
$script:retentionScenario = $false
$script:records = @()
foreach ($failAt in 0..8) {
    $script:failAt = $failAt
    $script:calls = [Collections.Generic.List[string]]::new()
    $result = Remove-RehearsalScope
    $expected = if ($failAt -eq 0) { "passed" } else { "failed" }
    $expectedCalls = if ($failAt -eq 5) { 11 } else { 8 }
    if ($result.status -ne $expected -or $calls.Count -ne $expectedCalls) { throw "Cleanup short-circuited or hid failure at operation $failAt" }
    if ($calls[0] -notmatch '--wait=false' -or $calls[4] -notmatch '^wait --for=delete namespace/') { throw "Namespace wait preceded independent cleanup" }
    $modelWait = @($calls | Where-Object { $_ -eq "wait --for=delete modelclass/trust-profile-$DemoRunId --timeout=180s" })
    if ($modelWait.Count -ne 1 -or $calls.IndexOf($modelWait[0]) -ge $calls.IndexOf("get modelclass/trust-profile-$DemoRunId --ignore-not-found -o name")) { throw "ModelClass deletion was not awaited before inventory" }
    if (@($calls | Where-Object { $_ -match 'patch|replace|finaliz|--force' }).Count) { throw "Cleanup bypassed retention" }
}
$script:retentionScenario = $true
foreach ($scenario in @("valid", "workload", "expired", "wrong-finalizer", "wrong-workspace", "unmatched-ticket", "empty", "discovery-failure", "inventory-failure")) {
    $deadline = [DateTimeOffset]::UtcNow.AddDays(90).ToString("o")
    $script:records = @(
        @{ kind = "RuntimeApprovalTombstone"; metadata = @{ name = "approval-1"; namespace = $namespace; deletionTimestamp = "2026-01-01T00:00:00Z"; finalizers = @("runtimeapprovaltombstone.retention.kognition.dev") }; spec = @{ workspace = $namespace; retentionDeadline = $deadline } },
        @{ kind = "RuntimeApprovalQuotaTicket"; metadata = @{ name = "approval-1"; namespace = $namespace; deletionTimestamp = "2026-01-01T00:00:00Z"; finalizers = @("kognition.dev/runtimeapprovalquotaticket-retirement") }; spec = @{ workspace = $namespace; retentionDeadline = $deadline } }
    )
    $script:failAt = 0
    switch ($scenario) {
        "workload" { $script:records += @{ kind = "Pod"; metadata = @{ name = "remaining" } } }
        "expired" { $script:records[0].spec.retentionDeadline = [DateTimeOffset]::UtcNow.AddDays(-1).ToString("o") }
        "wrong-finalizer" { $script:records[0].metadata.finalizers = @("unexpected") }
        "wrong-workspace" { $script:records[0].spec.workspace = "other" }
        "unmatched-ticket" { $script:records[1].metadata.name = "other" }
        "empty" { $script:records = @() }
        "discovery-failure" { $script:failAt = 7 }
        "inventory-failure" { $script:failAt = 8 }
    }
    $script:calls = [Collections.Generic.List[string]]::new()
    $result = Remove-RehearsalScope
    $expected = if ($scenario -eq "valid") { "retained" } else { "failed" }
    if ($result.status -ne $expected) { throw "Incorrect retention acceptance for $scenario" }
    if ($scenario -eq "valid" -and $result.retainedSecurityRecords.Count -ne 2) { throw "Retained records missing from report" }
    if ($scenario -eq "valid" -and $result.evidence -notmatch 'remains Terminating') { throw "Retention report hid the terminating namespace" }
    if (@($calls | Where-Object { $_ -match 'patch|replace|finaliz|--force' }).Count) { throw "Cleanup bypassed retention" }
}
& {
    $waitHelper = $ast.Find({ param($node) $node -is [Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq "Wait-ForWorkflowRun" }, $true)
    $propertyHelper = $ast.Find({ param($node) $node -is [Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq "Get-OptionalProperty" }, $true)
    . ([scriptblock]::Create($propertyHelper.Extent.Text))
    . ([scriptblock]::Create($waitHelper.Extent.Text))
    function Invoke-Kubectl {
        param([string[]]$Arguments)
        switch ($Arguments[1]) {
            "workflowrun/trust-demo-run" { return '{"status":{"phase":"Failed","conditions":[{"reason":"StepFailed"}]}}' }
            "agentjobs" { return '{"items":[]}' }
            "events" { return '{"items":[{"involvedObject":{"kind":"WorkflowRun"},"reason":"ToolStepFailed","message":"tool step prepared failed: permission denied"}]}' }
        }
    }
    try {
        Wait-ForWorkflowRun -Description "approval gate" -Predicate { $false } -TimeoutSeconds 10 | Out-Null
        throw "Failed WorkflowRun was accepted"
    }
    catch {
        if ($_.Exception.Message -notmatch 'ToolStepFailed.*permission denied') { throw }
    }
}
Write-Host "Cleanup attempts independent deletions, reports valid retention, and rejects workloads or unverified security records."