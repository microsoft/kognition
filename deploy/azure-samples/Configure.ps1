# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

#Requires -Version 7.0
[CmdletBinding()]
param(
    [Parameter(Mandatory)][guid]$SubscriptionId,
    [Parameter(Mandatory)][string]$FoundryResourceGroup,
    [Parameter(Mandatory)][string]$FoundryAccount,
    [Parameter(Mandatory)][string]$FoundryProject,
    [Parameter(Mandatory)][string]$ChatDeployment,
    [Parameter(Mandatory)][ValidateSet('openai-chat-v1', 'openai-reasoning-chat-v1', 'openai-tool-chat-v1')][string]$ChatInvocationContract,
    [string]$EmbeddingDeployment = 'text-embedding-3-small',
    [string]$Location = 'eastus2',
    [string]$ServicePrincipalName = 'kognition-local-samples',
    [string]$PlatformNamespace = 'kognition-system',
    [string[]]$AgentNamespaces = @('default', 'kognition-demo', 'samples-multi-agent', 'samples-azure-blob', 'samples-governance', 'production', 'development'),
    [ValidateRange(1, 90)][int]$CredentialDays = 90,
    [switch]$RenewCredential,
    [switch]$DeferBlobDataPlane,
    [switch]$ValidateOnly
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..\..')).Path
. (Join-Path $repoRoot 'samples\azure-settings.ps1')
Import-Module powershell-yaml -ErrorAction Stop
$envFile = Join-Path $repoRoot '.env'
$settings = Get-AzureSampleSettings -Path $envFile

function Invoke-AzureJson {
    param([Parameter(Mandatory)][string[]]$Arguments)
    $output = & az @Arguments --only-show-errors --output json
    if ($LASTEXITCODE -ne 0) {
        throw "Azure operation '$($Arguments[0]) $($Arguments[1])' failed with exit code $LASTEXITCODE."
    }
    if ($output) { return (($output -join "`n") | ConvertFrom-Json) }
}

function Protect-CredentialFile {
    param([Parameter(Mandatory)][string]$Path)
    if ($IsWindows) {
        $sid = [Security.Principal.WindowsIdentity]::GetCurrent().User.Value
        & icacls.exe $Path /inheritance:r /grant:r "*${sid}:(F)" '*S-1-5-18:(F)' '*S-1-5-32-544:(F)' | Out-Null
        if ($LASTEXITCODE -ne 0) { throw "Could not restrict permissions on $Path." }
    } else {
        [IO.File]::SetUnixFileMode($Path, [IO.UnixFileMode]::UserRead -bor [IO.UnixFileMode]::UserWrite)
    }
}

function Write-LocalFile {
    param([string]$Path, [string]$Content, [switch]$Sensitive)
    if (-not (Test-Path -LiteralPath $Path)) { [IO.File]::WriteAllText($Path, '') }
    if ($Sensitive) { Protect-CredentialFile -Path $Path }
    [IO.File]::WriteAllText($Path, $Content, [Text.UTF8Encoding]::new($false))
}

function Save-Settings {
    param([System.Collections.IDictionary]$Updates)
    $seen = @{}
    $lines = if (Test-Path -LiteralPath $envFile) { @(Get-Content -LiteralPath $envFile) } else { @() }
    $result = @(foreach ($line in $lines) {
        if ($line -match '^\s*([A-Za-z_][A-Za-z0-9_]*)\s*=' -and $Updates.Contains($Matches[1])) {
            $key = $Matches[1]
            if (-not $seen.ContainsKey($key)) { "$key=$($Updates[$key])" }
            $seen[$key] = $true
        } else {
            $line
        }
    })
    foreach ($key in $Updates.Keys) {
        if (-not $seen.ContainsKey($key)) { $result += "$key=$($Updates[$key])" }
    }
    Write-LocalFile -Path $envFile -Content (($result -join "`n") + "`n") -Sensitive
}

function Ensure-RoleAssignment {
    param([string]$ObjectId, [string]$Role, [string]$Scope)
    $assignments = @(Invoke-AzureJson @('role', 'assignment', 'list', '--assignee', $ObjectId, '--scope', $Scope, '--role', $Role, '--subscription', "$SubscriptionId"))
    if ($assignments.Count -eq 0) {
        Invoke-AzureJson @('role', 'assignment', 'create', '--assignee-object-id', $ObjectId, '--assignee-principal-type', 'ServicePrincipal', '--role', $Role, '--scope', $Scope, '--subscription', "$SubscriptionId") | Out-Null
    }
}

$context = Invoke-AzureJson @('account', 'show', '--subscription', "$SubscriptionId")
$current = Invoke-AzureJson @('account', 'show')
if ($current.id -ne "$SubscriptionId" -or $current.tenantId -ne $context.tenantId) {
    throw 'The active Azure CLI context must match the selected subscription and tenant; no login or context is changed automatically.'
}
$account = Invoke-AzureJson @('cognitiveservices', 'account', 'show', '--resource-group', $FoundryResourceGroup, '--name', $FoundryAccount, '--subscription', "$SubscriptionId")
$deployments = @(Invoke-AzureJson @('cognitiveservices', 'account', 'deployment', 'list', '--resource-group', $FoundryResourceGroup, '--name', $FoundryAccount, '--subscription', "$SubscriptionId"))
foreach ($deployment in @($ChatDeployment, $EmbeddingDeployment)) {
    $match = @($deployments | Where-Object { $_.name -ceq $deployment -and $_.properties.provisioningState -eq 'Succeeded' })
    if ($match.Count -ne 1) { throw "Foundry deployment '$deployment' is missing or not ready." }
}
$project = Invoke-AzureJson @('rest', '--method', 'get', '--url', "https://management.azure.com$($account.id)/projects/${FoundryProject}?api-version=2025-06-01")
$projectEndpoint = $project.properties.endpoints.'AI Foundry API'
if (-not $projectEndpoint) { throw 'The Foundry project did not expose an AI Foundry API endpoint.' }
$suffix = ("$SubscriptionId" -replace '-', '').Substring(0, 8)
$storageAccount = "kognsamples$suffix"
$storageGroup = "rg-kognition-samples-$suffix"
$existingStorage = @(Invoke-AzureJson @('storage', 'account', 'list', '--subscription', "$SubscriptionId") | Where-Object { $_.name -ceq $storageAccount })
$availability = Invoke-AzureJson @('storage', 'account', 'check-name', '--name', $storageAccount)
if (-not $availability.nameAvailable -and ($existingStorage.Count -ne 1 -or $existingStorage[0].resourceGroup -ne $storageGroup)) {
    throw "Storage account name '$storageAccount' is not available in the approved resource group."
}
$usage = Invoke-AzureJson @('storage', 'account', 'show-usage', '--location', $Location, '--subscription', "$SubscriptionId")
if ($existingStorage.Count -eq 0 -and $usage.currentValue -ge $usage.limit) {
    throw "No Storage account capacity remains in $Location."
}
$armToken = Invoke-AzureJson @('account', 'get-access-token', '--resource', 'https://management.azure.com/', '--subscription', "$SubscriptionId")
$policyUri = "https://management.azure.com/subscriptions/$SubscriptionId/providers/Microsoft.Authorization/policyAssignments?api-version=2023-04-01&`$filter=atScope()"
$policyResponse = Invoke-RestMethod -Uri $policyUri -Headers @{ Authorization = "Bearer $($armToken.accessToken)" } -TimeoutSec 60
$effectivePolicies = @($policyResponse.value)
$armToken = $null
foreach ($path in @('.env', 'foundry-creds.yaml', '.azure\local-samples\identity-secret.yaml')) {
    & git -C $repoRoot check-ignore --quiet -- $path
    if ($LASTEXITCODE -ne 0) { throw "Credential output is not gitignored: $path" }
}
$apps = @(Invoke-AzureJson @('ad', 'app', 'list', '--display-name', $ServicePrincipalName) | Where-Object { $_.displayName -ceq $ServicePrincipalName })
if ($apps.Count -gt 1) { throw "Multiple app registrations are named '$ServicePrincipalName'; select a unique name." }
if ($ValidateOnly) {
    Write-Output "Effective policy assignments, including inherited scopes: $($effectivePolicies.properties.displayName -join '; ')."
    Write-Output "Validated subscription=$SubscriptionId tenant=$($context.tenantId) Foundry=$FoundryAccount/$FoundryProject chat=$ChatDeployment embedding=$EmbeddingDeployment Storage=$storageAccount region=$Location capacity=$($usage.currentValue)/$($usage.limit) deferBlobDataPlane=$DeferBlobDataPlane. Data-plane reachability is not asserted by this preflight. No resources or local configuration were changed."
    return
}

$newApp = $apps.Count -eq 0
$app = if ($newApp) {
    Invoke-AzureJson @('ad', 'app', 'create', '--display-name', $ServicePrincipalName, '--sign-in-audience', 'AzureADMyOrg')
} else {
    $apps[0]
}
$principals = @(Invoke-AzureJson @('ad', 'sp', 'list', '--filter', "appId eq '$($app.appId)'"))
if ($principals.Count -gt 1) { throw 'The app registration has an ambiguous service-principal identity.' }
$principal = if ($principals.Count -eq 0) {
    Invoke-AzureJson @('ad', 'sp', 'create', '--id', $app.appId)
} else {
    $principals[0]
}
if ($newApp -or $RenewCredential) {
    $expiry = [DateTime]::UtcNow.AddDays($CredentialDays).ToString('yyyy-MM-ddTHH:mm:ssZ')
    $credential = Invoke-AzureJson @('ad', 'app', 'credential', 'reset', '--id', $app.appId, '--append', '--display-name', 'kognition-local-samples', '--end-date', $expiry)
    $secret = $credential.password
    if (-not $secret) { throw 'Entra did not return a client secret.' }
} elseif ($settings['AZURE_TENANT_ID'] -eq $context.tenantId -and $settings['AZURE_CLIENT_ID'] -eq $app.appId -and $settings['AZURE_CLIENT_SECRET']) {
    $secret = $settings['AZURE_CLIENT_SECRET']
    $expiry = $settings['AZURE_CLIENT_SECRET_EXPIRES_ON']
} else {
    throw 'The existing service principal has no matching local credential. Use -RenewCredential to append a new credential without revoking existing credentials.'
}

Save-Settings ([ordered]@{
    AZURE_TENANT_ID = $context.tenantId
    AZURE_CLIENT_ID = $app.appId
    AZURE_CLIENT_SECRET = $secret
    AZURE_CLIENT_SECRET_EXPIRES_ON = $expiry
    AZURE_CREDENTIAL_TYPE = 'client-secret'
    KOGNITION_AZURE_SUBSCRIPTION_ID = "$SubscriptionId"
    KOGNITION_AZURE_LOCATION = $Location
    MS_FOUNDRY_PROJECT_ENDPOINT = $projectEndpoint
    MS_FOUNDRY_INFERENCE_ENDPOINT = $account.properties.endpoint.TrimEnd('/')
    MS_FOUNDRY_MODEL = $ChatDeployment
    MS_FOUNDRY_INVOCATION_CONTRACT = $ChatInvocationContract
    MS_FOUNDRY_EMBEDDING_MODEL = $EmbeddingDeployment
    MS_FOUNDRY_API_VERSION = '2024-12-01-preview'
    MS_FOUNDRY_EMBEDDING_API_VERSION = '2024-02-01'
})
Ensure-RoleAssignment -ObjectId $principal.id -Role 'Cognitive Services OpenAI User' -Scope $account.id
& (Join-Path $repoRoot 'make.ps1') samples-provision-azure "AZURE_LOCATION=$Location" "DEFER_BLOB_DATA_PLANE=$($DeferBlobDataPlane.ToString().ToLowerInvariant())"
if ($LASTEXITCODE -ne 0) { throw 'Azure Blob sample provisioning failed.' }
& (Join-Path $repoRoot 'make.ps1') foundry-creds APPLY=false "AGENT_NAMESPACE=$($AgentNamespaces -join ',')"
if ($LASTEXITCODE -ne 0) { throw 'Credential manifest generation failed.' }
Protect-CredentialFile -Path (Join-Path $repoRoot 'foundry-creds.yaml')

$outputDirectory = Join-Path $repoRoot '.azure\local-samples'
New-Item -ItemType Directory -Path $outputDirectory -Force | Out-Null
$identity = @{
    apiVersion = 'v1'
    kind = 'Secret'
    metadata = @{ name = 'azure-samples-identity'; namespace = $PlatformNamespace }
    type = 'Opaque'
    stringData = @{
        AZURE_TENANT_ID = $context.tenantId
        AZURE_CLIENT_ID = $app.appId
        AZURE_CLIENT_SECRET = $secret
    }
}
Write-LocalFile -Path (Join-Path $outputDirectory 'identity-secret.yaml') -Content (ConvertTo-AzureSampleYaml -Data $identity) -Sensitive
$memoryValues = @{ memoryService = @{ embedding = @{
    provider = 'foundry'
    endpoint = $account.properties.endpoint.TrimEnd('/')
    model = $EmbeddingDeployment
    apiVersion = '2024-02-01'
    azureIdentitySecret = 'azure-samples-identity'
} } }
Write-LocalFile -Path (Join-Path $outputDirectory 'memory-embedding-values.yaml') -Content (ConvertTo-AzureSampleYaml -Data $memoryValues)
& (Join-Path $PSScriptRoot 'Export-Models.ps1') -EnvironmentFile $envFile -OutputDirectory $outputDirectory -AgentNamespaces $AgentNamespaces
& (Join-Path $PSScriptRoot 'Test.ps1') -SkipBlob:$DeferBlobDataPlane
Write-Output "Configured Azure samples: tenant=$($context.tenantId), clientId=$($app.appId), Foundry=$FoundryAccount, Storage=$storageAccount, credentialExpiry=$expiry, blobDataPlaneDeferred=$DeferBlobDataPlane. Local manifests were generated; no Kubernetes resources were changed."
