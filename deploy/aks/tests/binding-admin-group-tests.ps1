# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

#Requires -Version 7.0
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$script = Join-Path $PSScriptRoot '../Initialize-BindingAdminGroup.ps1'
$existingId = '11111111-1111-4111-8111-111111111111'
$newId = '22222222-2222-4222-8222-222222222222'
$userId = '33333333-3333-4333-8333-333333333333'
$global:groupTest = @{ Groups = @(); Members = @(); Calls = [System.Collections.Generic.List[string]]::new() }

function Assert-True([bool]$Condition, [string]$Message) {
    if (-not $Condition) { throw $Message }
}

function az {
    $arguments = @($args)
    $global:groupTest.Calls.Add(($arguments[0..2] -join ' '))
    $global:LASTEXITCODE = 0
    switch (($arguments[0..2] -join ' ')) {
        'ad group list' { ConvertTo-Json -InputObject @($global:groupTest.Groups) -Compress }
        'ad group create' { $newId }
        'ad signed-in-user show' { $userId }
        'ad group member' {
            $member = $arguments[[Array]::IndexOf($arguments, '--member-id') + 1]
            if ($arguments[3] -eq 'check') { if ($global:groupTest.Members -contains $member) { 'true' } else { 'false' }; return }
            $global:groupTest.Members += $member
        }
        default { throw "Unexpected az call: $($arguments -join ' ')" }
    }
}

# New group: created once, signed-in user added.
$id = & $script
Assert-True ($id -eq $newId) 'A missing group must be created and its object ID returned'
Assert-True ($global:groupTest.Members -contains $userId) 'The signed-in operator must be able to approve bindings'
Assert-True (@($global:groupTest.Calls | Where-Object { $_ -eq 'ad group create' }).Count -eq 1) 'Group must be created exactly once'

# Existing group: reused, members not duplicated, nothing created.
$global:groupTest = @{ Groups = @($existingId); Members = @($userId); Calls = [System.Collections.Generic.List[string]]::new() }
$id = & $script
Assert-True ($id -eq $existingId) 'An existing group must be reused'
Assert-True (-not ($global:groupTest.Calls -contains 'ad group create')) 'An existing group must not be recreated'
Assert-True (@($global:groupTest.Members).Count -eq 1) 'Existing membership must not be duplicated'

# Ambiguous names fail closed.
$global:groupTest = @{ Groups = @($existingId, $newId); Members = @(); Calls = [System.Collections.Generic.List[string]]::new() }
$failed = $false
try { & $script | Out-Null } catch { $failed = $true }
Assert-True $failed 'Duplicate display names must not silently pick a group'

# Explicit members and no implicit user.
$global:groupTest = @{ Groups = @($existingId); Members = @(); Calls = [System.Collections.Generic.List[string]]::new() }
& $script -SkipSignedInUser -MemberObjectId $userId | Out-Null
Assert-True ($global:groupTest.Members -contains $userId -and @($global:groupTest.Members).Count -eq 1) 'Explicit members are added'
Assert-True (-not ($global:groupTest.Calls -contains 'ad signed-in-user show')) 'SkipSignedInUser must not look up the operator'

Write-Output 'PASS: Entra binding-admin group is created idempotently, reused safely and fails closed on ambiguity'
