# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

#Requires -Version 7.0
$ErrorActionPreference = 'Stop'
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
$originalDirectory = (Get-Location).Path
$fixture = Join-Path ([IO.Path]::GetTempPath()) "kognition-blob-provisioning-$([guid]::NewGuid().ToString('N'))"
$fixture = (New-Item -ItemType Directory -Path $fixture).FullName
$calls = [Collections.Generic.List[string]]::new()
$subscription = '00000000-0000-0000-0000-000000000000'
$tenant = '11111111-1111-1111-1111-111111111111'
$scope = "/subscriptions/$subscription/resourceGroups/rg-kognition-samples-00000000/providers/Microsoft.Storage/storageAccounts/kognsamples00000000"
$environment = "AZURE_TENANT_ID=$tenant`nAZURE_CLIENT_ID=22222222-2222-2222-2222-222222222222`nAZURE_CLIENT_SECRET=test-only-credential`n"
$global:KognitionAzureProvisioningFixture = @{ Calls = $calls; Subscription = $subscription; Tenant = $tenant; Scope = $scope }

function Assert-True([bool]$Condition, [string]$Message) {
    if (-not $Condition) { throw $Message }
}

function global:az {
    $Arguments = @($args)
    $command = $Arguments -join ' '
    $global:KognitionAzureProvisioningFixture.Calls.Add($command)
    $global:LASTEXITCODE = 0
    switch -Regex ($command) {
        '^account show' { return (@{ id = $global:KognitionAzureProvisioningFixture.Subscription; tenantId = $global:KognitionAzureProvisioningFixture.Tenant } | ConvertTo-Json -Compress) }
        '^ad signed-in-user show' { return 'operator-object-id' }
        '^ad sp show' { return 'runtime-object-id' }
        '^group create' { return }
        '^storage account list' { return 'kognsamples00000000' }
        '^storage account show' {
            if ($command -match '--query publicNetworkAccess') { return 'Disabled' }
            return $global:KognitionAzureProvisioningFixture.Scope
        }
        '^role assignment (list|create)' { return }
        '^storage container-rm create' { return }
        '^storage container create' {
            $global:LASTEXITCODE = 1
            return 'ERROR: The request may be blocked by network rules of storage account.'
        }
        default { throw "Unexpected Azure operation in provisioning test: $command" }
    }
}

function global:kubectl {
    $command = $args -join ' '
    $global:LASTEXITCODE = 0
    if ($command -like 'get crd secretrefs.kognition.dev*') { return }
    if ($command -like 'get modelclass foundry*') {
        if ($global:KognitionAzureProvisioningFixture.ModelClass) {
            return ($global:KognitionAzureProvisioningFixture.ModelClass | ConvertTo-Json -Depth 5 -Compress)
        }
        return
    }
    throw "Unexpected Kubernetes operation in sample installer test: $command"
}

function global:helm {
    $global:LASTEXITCODE = 0
    $global:KognitionAzureProvisioningFixture.Calls.Add("helm $($args -join ' ')")
}

try {
    New-Item -ItemType Directory -Path (Join-Path $fixture 'samples') | Out-Null
    Copy-Item -LiteralPath (Join-Path $repoRoot 'make.ps1') -Destination (Join-Path $fixture 'make.ps1')
    Copy-Item -LiteralPath (Join-Path $repoRoot 'samples\azure-settings.ps1') -Destination (Join-Path $fixture 'samples\azure-settings.ps1')
    [IO.File]::WriteAllText((Join-Path $fixture '.env'), $environment)
    & (Join-Path $fixture 'make.ps1') samples-provision-azure DEFER_BLOB_DATA_PLANE=true
    Assert-True (@($calls | Where-Object { $_ -like 'storage container-rm create*' }).Count -eq 1) 'Deferred provisioning must create only the container through ARM.'
    Assert-True (@($calls | Where-Object { $_ -like 'storage container create*' -or $_ -like 'storage blob *' }).Count -eq 0) 'Deferred provisioning must not attempt Storage data-plane operations.'
    Assert-True (@($calls | Where-Object { $_ -like 'role assignment create*' -and $_ -like '*Storage Blob Data Reader*' -and $_ -like "*$scope/blobServices/default/containers/product-docs*" }).Count -eq 1) 'Runtime Blob read permission must use the container scope.'
    Assert-True (@($calls | Where-Object { $_ -like '*Storage Blob Data Contributor*' }).Count -eq 0) 'Deferred provisioning must not grant unneeded operator upload access.'
    $generated = Get-Content -LiteralPath (Join-Path $fixture '.env') -Raw
    Assert-True ($generated.Contains('KOGNITION_AZURE_BLOB_DATA_PLANE_STATUS=pending-private-connectivity')) 'Deferred status must be persisted explicitly.'
    Assert-True ($generated.Contains('AZURE_CLIENT_SECRET=test-only-credential')) 'Unrelated credential settings must be preserved.'
    Assert-True (-not $generated.Contains('SharedAccessSignature')) 'Provisioning must not introduce an expiring SAS.'

    $calls.Clear()
    [IO.File]::WriteAllText((Join-Path $fixture '.env'), $environment)
    $failure = $null
    try {
        & (Join-Path $fixture 'make.ps1') samples-provision-azure
    } catch {
        $failure = $_.Exception.Message
    }
    Assert-True ($null -ne $failure -and $failure.Contains('networking denied access (publicNetworkAccess=Disabled)')) 'Private Storage denial must report its network cause.'
    Assert-True (@($calls | Where-Object { $_ -like 'storage container create*' }).Count -eq 1) 'A network denial must not enter the RBAC retry loop.'
    Assert-True (@($calls | Where-Object { $_ -like 'storage account update*' }).Count -eq 0) 'Provisioning must never override Storage network restrictions.'
    Assert-True ((Get-Content -LiteralPath (Join-Path $fixture '.env') -Raw) -ceq $environment) 'Failed provisioning must not publish success-shaped Blob settings.'
    $chartDirectory = New-Item -ItemType Directory -Path (Join-Path $fixture 'samples\helm-multi-agent')
    [IO.File]::WriteAllText((Join-Path $chartDirectory.FullName 'Chart.yaml'), "name: helm-multi-agent`n")
    $modelEnvironment = $environment + "MS_FOUNDRY_PROJECT_ENDPOINT=https://example.services.ai.azure.com/api/projects/example`nMS_FOUNDRY_MODEL=gpt-6-luna`nMS_FOUNDRY_EMBEDDING_MODEL=text-embedding-3-small`nMS_FOUNDRY_INVOCATION_CONTRACT=openai-tool-chat-v1`n"
    foreach ($case in @(
        @{ Release = 'external-release'; Namespace = 'samples-multi-agent'; Reuse = $true },
        @{ Release = 'helm-multi-agent'; Namespace = 'samples-multi-agent'; Reuse = $false },
        @{ Release = 'helm-multi-agent'; Namespace = 'another-namespace'; Reuse = $true },
        @{ Release = ''; Namespace = ''; Reuse = $false }
    )) {
        $calls.Clear()
        $global:KognitionAzureProvisioningFixture.ModelClass = if ($case.Release) {
            @{ metadata = @{ annotations = @{
                'meta.helm.sh/release-name' = $case.Release
                'meta.helm.sh/release-namespace' = $case.Namespace
            } } }
        } else { $null }
        [IO.File]::WriteAllText((Join-Path $fixture '.env'), $modelEnvironment)
        & (Join-Path $fixture 'make.ps1') samples-install SAMPLE=helm-multi-agent AGENT_NAMESPACE=samples-multi-agent
        $helmCalls = @($calls | Where-Object { $_ -like 'helm upgrade --install*' })
        Assert-True ($helmCalls.Count -eq 1) 'The targeted Helm sample must be installed once.'
        Assert-True ($helmCalls[0].Contains('modelClass.create=false') -eq $case.Reuse) 'Helm must reuse external classes while retaining its own class on upgrade.'
        Assert-True ($helmCalls[0].Contains('model.modelID=gpt-6-luna')) 'The targeted Helm sample must use the configured deployment.'
    }
    Write-Output 'PASS: private Storage deferral, management-plane container/RBAC setup, no upload/SAS, preserved credentials, and immediate explicit network errors.'
    Write-Output 'PASS: targeted Helm model binding and class ownership for fresh, shared, and same-release upgrades.'
} finally {
    Set-Location -LiteralPath $originalDirectory
    Remove-Item -LiteralPath Function:\az
    Remove-Item -LiteralPath Function:\kubectl
    Remove-Item -LiteralPath Function:\helm
    Remove-Variable -Scope Global -Name KognitionAzureProvisioningFixture
    if (Test-Path -LiteralPath $fixture) {
        $resolved = (Resolve-Path -LiteralPath $fixture).Path
        if ((Split-Path -Leaf $resolved) -notlike 'kognition-blob-provisioning-*') { throw 'Unexpected temporary fixture path.' }
        Remove-Item -LiteralPath $resolved -Recurse -Force
    }
}
