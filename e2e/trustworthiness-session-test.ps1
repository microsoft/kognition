# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest
$tokens = $null
$parseErrors = $null
$ast = [Management.Automation.Language.Parser]::ParseFile((Join-Path $PSScriptRoot "kubernetes-tool-gateway-e2e.ps1"), [ref]$tokens, [ref]$parseErrors)
if ($parseErrors.Count) { throw ($parseErrors | Out-String) }
$commands = @($ast.FindAll({ param($node) $node -is [Management.Automation.Language.CommandAst] }, $true))
$instanceRunningWait = $commands | Where-Object { $_.Extent.Text -match 'Wait-ForJsonPath\s+-Resource\s+"agentinstance/\$instanceName".*-Expected\s+"Running"' } | Select-Object -First 1
$agentReadyWait = $commands | Where-Object { $_.Extent.Text -match 'Wait-ForJsonPath\s+-Resource\s+"agent/\$agentName".*-Expected\s+"Ready"' } | Select-Object -First 1
$firstExchange = $commands | Where-Object { $_.GetCommandName() -eq "Exchange-ToolSessionCapability" } | Select-Object -First 1
if (-not $instanceRunningWait -or -not $agentReadyWait -or -not $firstExchange) { throw "Tool approval fixture readiness ordering is incomplete" }
if ($agentReadyWait.Extent.StartOffset -le $instanceRunningWait.Extent.StartOffset -or $agentReadyWait.Extent.StartOffset -ge $firstExchange.Extent.StartOffset) {
    throw "Agent Ready must be observed after the active instance starts and before capability exchange"
}
$helper = $ast.Find({ param($node) $node -is [Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq "Invoke-ToolWithActiveSession" }, $true)
. ([scriptblock]::Create($helper.Extent.Text))
$pendingHelper = $ast.Find({ param($node) $node -is [Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq "Invoke-ToolPendingWithActiveSession" }, $true)
. ([scriptblock]::Create($pendingHelper.Extent.Text))
$script:requests = @()
$script:sessionChecks = 0
$body = '{"invocation_id":"test:tool-allow","tool_name":"timestamp","input":{"timezone":"UTC"}}'
function Set-ActiveToolSession {
    param([string]$InstanceName, [string]$SessionID)
    if ($InstanceName -ne "owned-instance" -or $SessionID -ne "owned-session") { throw "Session changed" }
    $script:sessionChecks++
}
function Invoke-HttpRequest {
    param($Uri, $Method, $Headers, $Body)
    $script:requests += @{ Uri = $Uri; Method = $Method; Authorization = $Headers.Authorization; Body = $Body }
    if ($script:requests.Count -eq 1) { return $script:firstResponse }
    return $script:successResponse
}
$script:successResponse = @{ StatusCode = 200; Content = '{"invocation_id":"test:tool-allow","output":{"ok":true}}' }
foreach ($case in @(
    @{ Status = 503; Invocation = "test:tool-allow"; Code = "ToolInvocationUnavailable"; Retry = $true }
    @{ Status = 503; Invocation = "another-invocation"; Code = "ToolInvocationUnavailable"; Retry = $false }
    @{ Status = 503; Invocation = "test:tool-allow"; Code = "OtherFailure"; Retry = $false }
    @{ Status = 401; Invocation = "test:tool-allow"; Code = "ToolInvocationUnavailable"; Retry = $false }
)) {
    $script:requests = @()
    $script:sessionChecks = 0
    $script:firstResponse = @{ StatusCode = $case.Status; Content = (@{ invocation_id = $case.Invocation; error_code = $case.Code; runtime_approval_state = "unavailable" } | ConvertTo-Json -Compress) }
    $rejected = $false
    try { $null = Invoke-ToolWithActiveSession -InstanceName "owned-instance" -SessionID "owned-session" -Uri "http://localhost/invoke" -Authorization "Bearer test-capability" -Body $body }
    catch { $rejected = $true }
    if ($rejected -eq $case.Retry) { throw "Unexpected retry result for $($case | ConvertTo-Json -Compress)" }
    $expectedCalls = if ($case.Retry) { 2 } else { 1 }
    if ($requests.Count -ne $expectedCalls -or $sessionChecks -ne $expectedCalls) { throw "Retry skipped session validation or issued extra calls" }
    foreach ($request in $requests) {
        if ($request.Body -cne $body -or $request.Authorization -cne "Bearer test-capability" -or $request.Method -ne "Post") { throw "Retry changed the invocation binding" }
    }
}
$script:successResponse = @{ StatusCode = 202; Content = '{"invocation_id":"test:tool-allow","runtime_approval_state":"pending"}' }
foreach ($case in @(
    @{ Status = 503; Invocation = "test:tool-allow"; Code = "ToolInvocationUnavailable"; State = "unavailable"; Retry = $true }
    @{ Status = 503; Invocation = "another-invocation"; Code = "ToolInvocationUnavailable"; State = "unavailable"; Retry = $false }
    @{ Status = 503; Invocation = "test:tool-allow"; Code = "OtherFailure"; State = "unavailable"; Retry = $false }
    @{ Status = 503; Invocation = "test:tool-allow"; Code = "ToolInvocationUnavailable"; State = "denied"; Retry = $false }
    @{ Status = 401; Invocation = "test:tool-allow"; Code = "ToolInvocationUnavailable"; State = "unavailable"; Retry = $false }
)) {
    $script:requests = @()
    $script:sessionChecks = 0
    $script:firstResponse = @{ StatusCode = $case.Status; Content = (@{ invocation_id = $case.Invocation; error_code = $case.Code; runtime_approval_state = $case.State } | ConvertTo-Json -Compress) }
    $rejected = $false
    try { $null = Invoke-ToolPendingWithActiveSession -InstanceName "owned-instance" -SessionID "owned-session" -Uri "http://localhost/invoke" -Authorization "Bearer test-capability" -Body $body }
    catch { $rejected = $true }
    if ($rejected -eq $case.Retry) { throw "Unexpected pending retry result for $($case | ConvertTo-Json -Compress)" }
    $expectedCalls = if ($case.Retry) { 2 } else { 1 }
    if ($requests.Count -ne $expectedCalls -or $sessionChecks -ne $expectedCalls) { throw "Pending retry skipped session validation or issued extra calls" }
    foreach ($request in $requests) {
        if ($request.Body -cne $body -or $request.Authorization -cne "Bearer test-capability" -or $request.Method -ne "Post") { throw "Pending retry changed the invocation binding" }
    }
}
Write-Host "Tool retries preserve exact invocation and active session; unrelated failures remain terminal."

$phaseAst = [Management.Automation.Language.Parser]::ParseFile((Join-Path $PSScriptRoot "trust-profile-phases.ps1"), [ref]$tokens, [ref]$parseErrors)
if ($parseErrors.Count) { throw ($parseErrors | Out-String) }
$phaseCommands = @($phaseAst.FindAll({ param($node) $node -is [Management.Automation.Language.CommandAst] }, $true))
$running = $phaseCommands | Where-Object { $_.Extent.Text -match 'Wait-ForJsonPath\s+-Resource\s+"agentinstance/\$sourceInstance".*-Expected\s+"Running"' } | Select-Object -First 1
$ready = $phaseCommands | Where-Object { $_.Extent.Text -match 'Wait-ForJsonPath\s+-Resource\s+"agent/\$sourceAgent".*-Expected\s+"Ready"' } | Select-Object -First 1
$exchange = $phaseCommands | Where-Object { $_.Extent.Text -match 'Invoke-GatewayPost.*agent-services/capabilities' } | Select-Object -First 1
if (-not $running -or -not $ready -or -not $exchange -or $ready.Extent.StartOffset -le $running.Extent.StartOffset -or $ready.Extent.StartOffset -ge $exchange.Extent.StartOffset) {
    throw "Communication capability exchange must wait for the controller-owned source instance and Agent readiness"
}
Write-Host "Communication source readiness ordering passed."