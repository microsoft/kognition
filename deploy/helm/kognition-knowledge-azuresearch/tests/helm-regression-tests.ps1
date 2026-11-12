# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

$ErrorActionPreference = "Stop"
$ChartPath = Join-Path $PSScriptRoot ".."
$Passed = 0
$Failed = 0
$Errors = @()

function Assert-Contains {
    param([string]$Output, [string]$Pattern, [string]$TestName)
    if ($Output -match [regex]::Escape($Pattern)) {
        $script:Passed++
    } else {
        $script:Failed++
        $script:Errors += "FAIL: $TestName - expected to contain '$Pattern'"
    }
}

function Assert-NotContains {
    param([string]$Output, [string]$Pattern, [string]$TestName)
    if ($Output -match [regex]::Escape($Pattern)) {
        $script:Failed++
        $script:Errors += "FAIL: $TestName - expected NOT to contain '$Pattern'"
    } else {
        $script:Passed++
    }
}

function Assert-Match {
    param([string]$Output, [string]$Pattern, [string]$TestName)
    if ($Output -match $Pattern) {
        $script:Passed++
    } else {
        $script:Failed++
        $script:Errors += "FAIL: $TestName - expected regex match '$Pattern'"
    }
}

function Invoke-Chart {
    param([string[]]$SetValues)
    $parameters = @("template", "azuresearch", $ChartPath, "--namespace", "knowledge-team")
    foreach ($value in $SetValues) {
        $parameters += @("--set", $value)
    }
    $result = & helm @parameters 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "helm template failed: $($result -join "`n")"
    }
    return $result -join "`n"
}

function Assert-ChartFails {
    param([string[]]$SetValues, [string]$Message, [string]$TestName)
    $parameters = @("template", "azuresearch", $ChartPath, "--namespace", "knowledge-team")
    foreach ($value in $SetValues) {
        $parameters += @("--set", $value)
    }
    $previousPreference = $ErrorActionPreference
    try {
        $ErrorActionPreference = "Continue"
        $result = & helm @parameters 2>&1
    } finally {
        $ErrorActionPreference = $previousPreference
    }
    $output = $result -join "`n"
    if ($LASTEXITCODE -ne 0 -and $output -match [regex]::Escape($Message)) {
        $script:Passed++
    } else {
        $script:Failed++
        $script:Errors += "FAIL: $TestName - expected failure containing '$Message', got '$output'"
    }
}

$requiredValues = @(
    "runtime.enabled=true",
    "runtime.tls.secretName=provider-tls",
    "runtime.clientPeerURI=spiffe://kognition.dev/ns/kognition-system/sa/knowledge-gateway",
    "runtime.workloadIdentity.clientId=00000000-0000-0000-0000-000000000001",
    "runtime.workloadIdentity.tenantId=00000000-0000-0000-0000-000000000002",
    "runtime.networkPolicy.kubernetesApiEgressCIDRs[0]=10.0.0.1/32"
)

$requiredCases = @(
    @{ Value = "runtime.tls.secretName=provider-tls"; Message = "runtime.tls.secretName is required"; Name = "TLS Secret is required" },
    @{ Value = "runtime.clientPeerURI=spiffe://kognition.dev/ns/kognition-system/sa/knowledge-gateway"; Message = "runtime.clientPeerURI is required"; Name = "Gateway peer URI is required" },
    @{ Value = "runtime.workloadIdentity.clientId=00000000-0000-0000-0000-000000000001"; Message = "runtime.workloadIdentity.clientId is required"; Name = "Workload identity client ID is required" },
    @{ Value = "runtime.workloadIdentity.tenantId=00000000-0000-0000-0000-000000000002"; Message = "runtime.workloadIdentity.tenantId is required"; Name = "Workload identity tenant ID is required" },
    @{ Value = "runtime.networkPolicy.kubernetesApiEgressCIDRs[0]=10.0.0.1/32"; Message = "runtime.networkPolicy.kubernetesApiEgressCIDRs must list"; Name = "Kubernetes API CIDRs are required" }
)
foreach ($case in $requiredCases) {
    $values = @($requiredValues | Where-Object { $_ -ne $case.Value })
    Assert-ChartFails -SetValues $values -Message $case.Message -TestName $case.Name
}

$output = Invoke-Chart -SetValues $requiredValues
Assert-Contains $output 'azure.workload.identity/client-id: "00000000-0000-0000-0000-000000000001"' "ServiceAccount has Azure client ID"
Assert-Contains $output 'azure.workload.identity/tenant-id: "00000000-0000-0000-0000-000000000002"' "ServiceAccount has Azure tenant ID"
Assert-Contains $output 'azure.workload.identity/use: "true"' "Pod opts into workload identity"
Assert-Match $output '(?s)name: AZURE_TENANT_ID\s+value: "00000000-0000-0000-0000-000000000002"' "Provider receives explicit tenant ID"
Assert-Match $output '(?s)name: AZURE_CLIENT_ID\s+value: "00000000-0000-0000-0000-000000000001"' "Provider receives explicit client ID"
Assert-Match $output '(?s)name: AZURE_FEDERATED_TOKEN_FILE\s+value: /var/run/secrets/azure/tokens/azure-identity-token' "Provider uses projected Azure token path"
Assert-Match $output '(?s)kind: Role\s+metadata:\s+name: azuresearch-kognition-knowledge-azuresearch\s+namespace: knowledge-team' "Provider RBAC is namespace scoped"
Assert-Match $output '(?s)kind: RoleBinding\s+metadata:\s+name: azuresearch-kognition-knowledge-azuresearch\s+namespace: knowledge-team' "Provider binding is namespace scoped"
Assert-Match $output '(?s)kind: ClusterRole\s+metadata:\s+name: azuresearch-kognition-knowledge-azuresearch(?:(?!\r?\n---)[\s\S])*?resources: \["knowledgebaseclasses"\]\s+verbs: \["get", "list", "watch"\]' "Provider can resolve cluster-scoped KnowledgeBaseClasses"
Assert-Match $output '(?s)kind: ClusterRoleBinding\s+metadata:\s+name: azuresearch-kognition-knowledge-azuresearch(?:(?!\r?\n---)[\s\S])*?kind: ServiceAccount\s+name: azuresearch-kognition-knowledge-azuresearch\s+namespace: knowledge-team' "Provider class access is bound to its ServiceAccount"
Assert-Match $output '(?s)kind: NetworkPolicy.*?ingress:.*?app.kubernetes.io/component: knowledge-gateway.*?port: 9443' "Knowledge Gateway Pods can call provider"
Assert-Match $output '(?s)kind: NetworkPolicy.*?ingress:.*?kognition.dev/component: kb-ingester.*?port: 9443' "Provider indexing Jobs can call provider"
Assert-Match $output '(?s)egress:.*?port: 53\s+protocol: UDP.*?port: 53\s+protocol: TCP' "Provider permits DNS egress"
Assert-Match $output '(?s)cidr: 10.0.0.1/32.*?port: 443' "Provider permits configured Kubernetes API egress"
Assert-Contains $output "cidr: 0.0.0.0/0" "Standard NetworkPolicy provides documented HTTPS fallback"

$customServicePortOutput = Invoke-Chart -SetValues ($requiredValues + @("runtime.service.port=10443"))
Assert-Match $customServicePortOutput '(?s)kind: Service(?:(?!\r?\n---)[\s\S])*?port: 10443\s+targetPort: grpc' "Service port remains configurable"
Assert-Match $customServicePortOutput '(?s)kind: NetworkPolicy(?:(?!\r?\n---)[\s\S])*?ingress:(?:(?!\r?\n---)[\s\S])*?port: 9443' "NetworkPolicy targets the provider container port"

$splitNamespaceOutput = Invoke-Chart -SetValues ($requiredValues + @(
    "runtime.networkPolicy.gatewayNamespaceSelector.kubernetes\.io/metadata\.name=kognition-system"
))
Assert-Match $splitNamespaceOutput '(?s)kubernetes.io/metadata.name: kognition-system.*?app.kubernetes.io/component: knowledge-gateway' "Gateway ingress can use the core namespace"
Assert-Match $splitNamespaceOutput '(?s)kubernetes.io/metadata.name: "knowledge-team".*?kognition.dev/component: kb-ingester' "Gateway namespace override preserves tenant-local indexing ingress"

$ciliumValues = $requiredValues + @(
    "runtime.networkPolicy.ciliumFQDN.enabled=true",
    "runtime.networkPolicy.ciliumFQDN.matchNames[0]=login.microsoftonline.com",
    "runtime.networkPolicy.ciliumFQDN.matchNames[1]=example.search.windows.net"
)
$ciliumOutput = Invoke-Chart -SetValues $ciliumValues
Assert-Contains $ciliumOutput "kind: CiliumNetworkPolicy" "Cilium FQDN policy renders"
Assert-Contains $ciliumOutput 'matchName: "login.microsoftonline.com"' "Cilium allows the Entra host"
Assert-Contains $ciliumOutput 'matchName: "example.search.windows.net"' "Cilium allows the configured Search host"
Assert-NotContains $ciliumOutput "cidr: 0.0.0.0/0" "Cilium mode removes broad HTTPS egress"

Write-Host "Passed: $Passed  Failed: $Failed"
if ($Failed -gt 0) {
    $Errors | ForEach-Object { Write-Error $_ }
    exit 1
}