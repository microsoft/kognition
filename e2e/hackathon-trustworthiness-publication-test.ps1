# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$tokens = $null
$parseErrors = $null
$scriptAst = [Management.Automation.Language.Parser]::ParseFile((Join-Path $PSScriptRoot "hackathon-trustworthiness-e2e.ps1"), [ref]$tokens, [ref]$parseErrors)
if ($parseErrors.Count) { throw ($parseErrors | Out-String) }
foreach ($functionName in @("Assert-PublicationProof", "Submit-WorkflowApproval", "Get-OptionalProperty", "Wait-ForWorkflowRun")) {
    $functionAst = $scriptAst.Find({ param($node) $node -is [Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq $functionName }, $true)
    if ($null -eq $functionAst) { throw "Missing $functionName" }
    . ([scriptblock]::Create($functionAst.Extent.Text))
}

$runID = "proof-test"
$namespace = "kognition-hack-$runID"
& {
    $script:workflowReads = 0
    function Invoke-Kubectl {
        param([string[]]$Arguments)
        $script:workflowReads++
        if ($script:workflowReads -eq 1) { return '{"metadata":{"name":"trust-demo-run"}}' }
        if ($script:workflowReads -eq 2) { return '{"metadata":{"name":"trust-demo-run"},"status":{}}' }
        return '{"metadata":{"name":"trust-demo-run"},"status":{"phase":"Pending","activeStep":"publish"}}'
    }
    $observed = Wait-ForWorkflowRun -Description "new resource status" -Predicate {
        param($run)
        $status = Get-OptionalProperty -InputObject $run -Name "status"
        (Get-OptionalProperty -InputObject $status -Name "activeStep") -eq "publish"
    } -TimeoutSeconds 5
    if ($workflowReads -ne 3 -or $observed.status.phase -ne "Pending") { throw "Workflow wait did not tolerate initial missing status" }
}
$content = "Approved customer advisory for $runID"
$fixture = @{
    Run = @{
        metadata = @{ name = "trust-demo-run"; namespace = $namespace; annotations = @{ "kognition.dev/gate-entered.publish" = "2026-09-14T12:00:00Z"; "kognition.dev/approved-steps" = "publish" } }
        status = @{ phase = "Ready"; completedSteps = @("prepare", "publish"); snapshotSpec = @{
            steps = @(@{ name = "publish"; type = "tool"; toolRef = @{ name = "trust-demo-publish" }; input = (@{ runID = $runID; content = $content } | ConvertTo-Json -Compress) })
            approvalGates = @(@{ to = "publish"; requiredApprovers = 1 })
        } }
    }
    Approval = @{ metadata = @{ namespace = $namespace; creationTimestamp = "2026-09-14T12:00:01Z" }; spec = @{ workflowRunRef = @{ name = "trust-demo-run" }; step = "publish"; iteration = 0; decision = "approve" } }
    Receipt = @{ runID = $runID; content = $content; publicationCount = 1; publishedAt = "2026-09-14T12:00:02Z" }
}
$fixtureJson = $fixture | ConvertTo-Json -Depth 20
$valid = $fixtureJson | ConvertFrom-Json
Assert-PublicationProof -Run $valid.Run -Approval $valid.Approval -Receipt $valid.Receipt -RunID $runID -Content $content

$liveTiming = $fixtureJson | ConvertFrom-Json
$liveTiming.Run.metadata.annotations.'kognition.dev/gate-entered.publish' = "2026-09-14T19:23:36.540267536Z"
$liveTiming.Approval.metadata.creationTimestamp = [DateTime]::Parse("2026-09-14T19:23:37Z", $null, [Globalization.DateTimeStyles]::RoundtripKind)
$liveTiming.Receipt.publishedAt = [DateTime]::Parse("2026-09-14T19:23:37.813269+00:00", $null, [Globalization.DateTimeStyles]::RoundtripKind)
Assert-PublicationProof -Run $liveTiming.Run -Approval $liveTiming.Approval -Receipt $liveTiming.Receipt -RunID $runID -Content $content

$cases = @(
    @{ Name = "simulated step"; Mutate = { param($proof) $proof.Run.status.snapshotSpec.steps[0].type = "agent" } }
    @{ Name = "wrong input"; Mutate = { param($proof) $proof.Run.status.snapshotSpec.steps[0].input = '{"runID":"another-run","content":"wrong"}' } }
    @{ Name = "missing gate"; Mutate = { param($proof) $proof.Run.status.snapshotSpec.approvalGates = @() } }
    @{ Name = "unreconciled approval"; Mutate = { param($proof) $proof.Run.metadata.annotations.'kognition.dev/approved-steps' = "prepare" } }
    @{ Name = "wrong approval scope"; Mutate = { param($proof) $proof.Approval.metadata.namespace = "other" } }
    @{ Name = "denied approval"; Mutate = { param($proof) $proof.Approval.spec.decision = "deny" } }
    @{ Name = "stale iteration"; Mutate = { param($proof) $proof.Approval.spec.iteration = 1 } }
    @{ Name = "approval before gate"; Mutate = { param($proof) $proof.Approval.metadata.creationTimestamp = "2026-09-14T11:59:00Z" } }
    @{ Name = "publication before approval"; Mutate = { param($proof) $proof.Receipt.publishedAt = "2026-09-14T12:00:00Z" } }
    @{ Name = "missing publication"; Mutate = { param($proof) $proof.Receipt.publicationCount = 0 } }
    @{ Name = "wrong receipt run"; Mutate = { param($proof) $proof.Receipt.runID = "another-run" } }
    @{ Name = "wrong receipt content"; Mutate = { param($proof) $proof.Receipt.content = "wrong" } }
)
foreach ($case in $cases) {
    $proof = $fixtureJson | ConvertFrom-Json
    & $case.Mutate $proof
    $rejected = $false
    try { Assert-PublicationProof -Run $proof.Run -Approval $proof.Approval -Receipt $proof.Receipt -RunID $runID -Content $content }
    catch { $rejected = $true }
    if (-not $rejected) { throw "Accepted invalid proof: $($case.Name)" }
}

$script:submitted = @()
$script:promptCount = 0
$script:answer = "cancel"
function Read-Host { param([string]$Prompt) $script:promptCount++; return $script:answer }
function Import-ManifestTemplate { param([string]$FileName, [hashtable]$Values) $script:submitted += $Values }
$rejected = $false
try { Submit-WorkflowApproval -Values @{ APPROVER = "presenter" } }
catch { $rejected = $true }
if (-not $rejected -or $submitted.Count -ne 0) { throw "Cancellation submitted an approval" }
$script:answer = "publish"
Submit-WorkflowApproval -Values @{ APPROVER = "presenter" }
Submit-WorkflowApproval -Mode automated -Values @{ APPROVER = "runner" }
if ($promptCount -ne 2 -or $submitted.Count -ne 2 -or $submitted[1].APPROVAL_REASON -notmatch "Automated test") { throw "Approval mode contract failed" }
Write-Host "Publication proof: valid evidence accepted, $($cases.Count) invalid cases rejected; presenter confirmation and automated labeling passed."

& {
    $ReleaseName = "kognition"
    $ControllerNamespace = "test"
    $script:existingPolicies = @()
    function Invoke-Kubectl {
        param([string[]]$Arguments)
        if ($Arguments[0] -ne "get") { throw "Policy discovery attempted mutation" }
        if ($Arguments[1] -in $script:existingPolicies) { return $Arguments[1] }
    }
    $phaseAst = [Management.Automation.Language.Parser]::ParseFile((Join-Path $PSScriptRoot "hackathon-trust-profile-phases.ps1"), [ref]$tokens, [ref]$parseErrors)
    if ($parseErrors.Count) { throw ($parseErrors | Out-String) }
    $selection = $phaseAst.Find({ param($node) $node -is [Management.Automation.Language.AssignmentStatementAst] -and $node.Left.Extent.Text -eq '$isolatedComponents' }, $true)
    $baseline = $scriptAst.Find({ param($node) $node -is [Management.Automation.Language.AssignmentStatementAst] -and $node.Left.Extent.Text -eq '$controllerBaselinePolicy' }, $true)
    if ($null -eq $selection -or $null -eq $baseline) { throw "Missing baseline policy discovery" }
    foreach ($expected in @(@(), @("model-gateway"), @("k8s-controller"), @("model-gateway", "k8s-controller"))) {
        $script:existingPolicies = @($expected | ForEach-Object { "networkpolicy/kognition-$_" })
        . ([scriptblock]::Create($selection.Extent.Text))
        . ([scriptblock]::Create($baseline.Extent.Text))
        if (($isolatedComponents -join ",") -ne ($expected -join ",")) { throw "Provider isolation did not match baseline" }
        if ([bool]$controllerBaselinePolicy.Trim() -ne ("k8s-controller" -in $expected)) { throw "Publication isolation did not match baseline" }
    }
}
Write-Host "Network policy discovery: absent, partial, and full baseline isolation passed."