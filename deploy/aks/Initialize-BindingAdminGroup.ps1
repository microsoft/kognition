# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

#Requires -Version 7.0
<#
.SYNOPSIS
Creates (or reuses) the Microsoft Entra security group that may administer Kognition Workflow execution bindings.

.DESCRIPTION
AKS with Microsoft Entra integration presents Entra group object IDs as Kubernetes
group names. The script writes only that object ID to the pipeline, so pass it to
New-RuntimeValues.ps1 -WorkflowBindingAdminGroupId. It is idempotent and never
removes members or groups.
#>
[CmdletBinding()]
param(
    [ValidatePattern('^[A-Za-z0-9][A-Za-z0-9 ._-]{0,255}$')][string]$DisplayName = 'kognition-workflow-binding-admins',
    [guid[]]$MemberObjectId = @(),
    [switch]$SkipSignedInUser
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Invoke-Az {
    $output = & az @args 2>&1
    if ($LASTEXITCODE -ne 0) { throw "az $($args[0..2] -join ' ') failed: $($output -join ' ')" }
    $output
}

$existing = @(Invoke-Az ad group list --filter "displayName eq '$DisplayName'" --query '[].id' -o json | ConvertFrom-Json)
if ($existing.Count -gt 1) { throw "Multiple Entra groups are named '$DisplayName'; pass a unique -DisplayName." }
if ($existing.Count -eq 1) {
    $groupId = $existing[0]
} else {
    $nickname = ($DisplayName -replace '[^A-Za-z0-9-]', '-')
    $groupId = (Invoke-Az ad group create --display-name $DisplayName --mail-nickname $nickname `
        --description 'Kognition operators allowed to administer Workflow execution bindings' --query id -o tsv | Select-Object -First 1).Trim()
}
if ($groupId -notmatch '^[0-9a-fA-F]{8}-([0-9a-fA-F]{4}-){3}[0-9a-fA-F]{12}$') { throw 'Entra did not return a group object ID.' }

$members = [System.Collections.Generic.List[string]]::new()
foreach ($member in $MemberObjectId) { $members.Add($member.ToString()) }
if (-not $SkipSignedInUser) {
    $user = Invoke-Az ad signed-in-user show --query id -o tsv | Select-Object -First 1
    if ($user) { $members.Add($user.Trim()) }
}
foreach ($member in ($members | Sort-Object -Unique)) {
    $exists = (Invoke-Az ad group member check --group $groupId --member-id $member --query value -o tsv | Select-Object -First 1).Trim()
    if ($exists -ne 'true') { Invoke-Az ad group member add --group $groupId --member-id $member | Out-Null }
}
$groupId
