# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

<#
.SYNOPSIS
    Helm chart regression tests for kognition.
    Uses 'helm template' to validate rendering and assertions on output.

.DESCRIPTION
    This script validates all 7 bug fixes plus mode-exclusivity and conditional rendering.
    Run from the repo root: .\deploy\helm\kognition\tests\helm-regression-tests.ps1

.NOTES
    Requires: helm v3.x+ installed and on PATH.
#>

param([switch]$WorkflowExecutorOnly)

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

function Assert-RegexMatch {
    param([string]$Output, [string]$Pattern, [string]$TestName)
    if ($Output -match $Pattern) {
        $script:Passed++
    } else {
        $script:Failed++
        $script:Errors += "FAIL: $TestName - expected regex match '$Pattern'"
    }
}

function Assert-RegexNotMatch {
    param([string]$Output, [string]$Pattern, [string]$TestName)
    if ($Output -match $Pattern) {
        $script:Failed++
        $script:Errors += "FAIL: $TestName - expected NO regex match '$Pattern'"
    } else {
        $script:Passed++
    }
}

function Invoke-HelmTemplate {
    param([string[]]$SetValues)
    $helmParameters = @("template", "test-release", $ChartPath, "--kube-version", "1.30.3")
    $helmParameters += "--set"
    $helmParameters += "networkPolicy.kubeletProbeIngressCIDRs[0]=172.16.0.0/12"
    foreach ($v in $SetValues) {
        $helmParameters += "--set"
        $helmParameters += $v
    }
    $result = & helm @helmParameters 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "helm template failed: $result"
    }
    return ($result -join "`n")
}

function Get-EmbeddedDashboard {
    param([string]$Output, [string]$DataKey)
    $pattern = '(?m)^  ' + [regex]::Escape($DataKey) + ': \|\r?\n(?<json>(?:    [^\r\n]*(?:\r?\n|$))*)'
    if ($Output -notmatch $pattern) {
        throw "Dashboard data key '$DataKey' was not found in rendered Helm output"
    }
    $json = $Matches.json -replace '(?m)^    ', ''
    return ($json | ConvertFrom-Json -ErrorAction Stop)
}

function Assert-HelmTemplateFails {
    param([string[]]$SetValues, [string[]]$Pattern, [string]$TestName)
    $helmParameters = @("template", "test-release", $ChartPath, "--kube-version", "1.30.3")
    foreach ($v in $SetValues) {
        $helmParameters += "--set"
        $helmParameters += $v
    }
    $previousErrorActionPreference = $ErrorActionPreference
    try {
        # Windows PowerShell promotes native stderr to a terminating error
        # when ErrorActionPreference is Stop. These cases intentionally render
        # invalid values, so retain the exit code and output for assertion.
        $ErrorActionPreference = "Continue"
        $result = & helm @helmParameters 2>&1
    }
    finally {
        $ErrorActionPreference = $previousErrorActionPreference
    }
    $output = $result -join "`n"
    $matchedPattern = @($Pattern | Where-Object { $output -match [regex]::Escape($_) }).Count -gt 0
    if ($LASTEXITCODE -ne 0 -and $matchedPattern) {
        $script:Passed++
    } else {
        $script:Failed++
        $script:Errors += "FAIL: $TestName - expected failure containing one of '$($Pattern -join "', '")', got '$output'"
    }
}

Write-Host "========================================" -ForegroundColor Cyan
Write-Host " Kognition Helm Chart Regression Tests" -ForegroundColor Cyan
Write-Host "========================================" -ForegroundColor Cyan
Write-Host ""

$workflowDefault = Invoke-HelmTemplate -SetValues @("mode=kubernetes")
Assert-NotContains $workflowDefault "KOGNITION_WORKFLOW_EXECUTOR_PRINCIPALS" "Default profile does not invent an executor"
Assert-NotContains $workflowDefault "name: workflow-executor-token" "Default profile does not mount an executor credential"
Assert-Contains $workflowDefault "name: workflow-records.kognition.dev" "Default profile protects private execution records"
Assert-NotContains $workflowDefault "name: workflow-submission.kognition.dev" "Default profile does not route executor submissions"
$workflowExecutorValues = @(
    "mode=kubernetes",
    "k8sController.workflowExecutor.enabled=true",
    "k8sController.workflowExecutor.principals[0].method=tokenreview",
    "k8sController.workflowExecutor.principals[0].issuer=kubernetes.io/tokenreview",
    "k8sController.workflowExecutor.principals[0].subject=system:serviceaccount:default:test-release-kognition-controller",
    "k8sController.workflowExecutor.bindingAdminGroups[0]=workflow-operators"
)
$workflowProfile = Invoke-HelmTemplate -SetValues $workflowExecutorValues
foreach ($render in @($workflowDefault, $workflowProfile)) {
    Assert-Contains $render "(has(request.name) && (" "Name match tolerates nameless collection deletes so namespace deletion is not blocked"
}
Assert-Contains $workflowProfile "fieldPath: metadata.name" "Executor uses actual downward-API pod name"
Assert-Contains $workflowProfile "value: /var/run/kognition/workflow-executor/token" "Executor reads the projected credential file"
Assert-Contains $workflowProfile "audience: kognition.dev/workflow-executor" "Executor has a dedicated token audience"
Assert-Contains $workflowProfile "expirationSeconds: 600" "Executor credential rotates with a finite lifetime"
Assert-Contains $workflowProfile 'resources: ["tokenreviews"]' "Executor can independently review its identity"
Assert-Contains $workflowProfile 'resources: ["subjectaccessreviews"]' "Executor can independently review named authorization"
Assert-Contains $workflowProfile 'resourceNames: ["test-release-kognition-controller"]' "ServiceAccount read is limited to the executor"
Assert-Contains $workflowProfile "- test-release-kognition-workflow-webhook.default.svc" "TLS certificate covers the executor admission Service"
Assert-Contains $workflowProfile "variables.workflowRecord ||" "State policy admits the tightly shaped Workflow evidence"
Assert-Contains $workflowProfile "name: protected-workflow-record" "Private admission does not gate ordinary bootstrap state"
Assert-Contains $workflowProfile "[object, oldObject].exists(candidate" "Private admission covers old and new protected metadata"
Assert-RegexMatch $workflowProfile '(?s)name: workflow-records\.kognition\.dev.*?failurePolicy: Fail.*?name: test-release-kognition-workflow-webhook' "Private guard routes fail-closed to the executor"
Assert-RegexMatch $workflowProfile '(?s)name: workflow-submission\.kognition\.dev.*?name: test-release-kognition-workflow-webhook' "Workflow submission routes to the executor"
$ordinaryValidation = [regex]::Match($workflowProfile, '(?s)  - name: validate\.kognition\.dev.*?(?=\n  - name:)').Value
Assert-Contains $ordinaryValidation "- workflows" "Ordinary validator still validates definitions"
Assert-NotContains $ordinaryValidation "- workflowruns" "Executor is the only WorkflowRun validation owner"
Assert-RegexMatch $workflowProfile '(?s)name: test-release-kognition-webhook\r?\n(?:(?!\r?\n---).)*app.kubernetes.io/component: resource-webhooks' "Ordinary admission retains its distinct workload"
Assert-RegexMatch $workflowProfile '(?s)name: test-release-kognition-workflow-webhook\r?\n(?:(?!\r?\n---).)*app.kubernetes.io/component: k8s-controller' "Executor admission uses the controller workload"
foreach ($invalid in @(
    "webhook.enabled=false",
    "webhook.failurePolicy=Ignore",
    "webhook.port=8443",
    "k8sController.replicaCount=0",
    "k8sController.workflowExecutor.principals[0].method=oidc",
    "k8sController.workflowExecutor.principals[0].issuer=untrusted",
    "k8sController.workflowExecutor.principals[0].subject=system:serviceaccount:default:test-release-kognition-webhook",
    "k8sController.workflowExecutor.principals[0].unknown=untrusted",
    "k8sController.workflowExecutor.bindingAdminGroups[0]=system:authenticated",
    "k8sController.workflowExecutor.bindingAdminGroups[0]=system:serviceaccounts:default"
)) {
    Assert-HelmTemplateFails -SetValues ($workflowExecutorValues + $invalid) -Pattern @("workflowExecutor") -TestName "Executor rejects $invalid"
}
$collaborationValues = $workflowExecutorValues + @(
    "k8sController.managedNamespaces[0]=kognition-work",
    "k8sController.workflowExecutor.collaboration.enabled=true"
)
foreach ($render in @($workflowDefault, $workflowProfile)) {
    Assert-NotContains $render "KOGNITION_WORKFLOW_COLLABORATION_ENABLED" "Collaboration is not rendered unless enabled"
    Assert-NotContains $render "variables.collaborationLedger ||" "Collaboration ledger admission is not opened unless enabled"
}
$collaboration = Invoke-HelmTemplate -SetValues $collaborationValues
Assert-Contains $collaboration "KOGNITION_WORKFLOW_COLLABORATION_ENABLED" "Controller and admission webhooks receive the collaboration switch"
Assert-NotContains $collaboration "workflow-collaboration-token" "Collaboration needs no receiver credential"
Assert-NotContains $collaboration "workflow-collaboration-ca" "Collaboration needs no receiver CA"
Assert-NotContains $collaboration "variables.workflowActivation" "Collaboration needs no activation records"
Assert-Contains $collaboration "variables.collaborationLedger ||" "State policy admits the collaboration ledgers"
Assert-Contains $collaboration "candidate.metadata.name.matches('^wfc-[0-9a-f]{24}(-handles)?`$')" "Ledger admission is confined to collaboration names"
Assert-RegexMatch $collaboration '(?s)namespace: "kognition-work".*?configmaps\"\]\r?\n    verbs: \["update"\]' "Controller can update ledgers in managed namespaces"
Assert-Contains $collaboration "owner.kind in ['Workflow', 'WorkflowRun']" "Ledgers may carry only a single Workflow or WorkflowRun owner"
Assert-Contains $collaboration "(!has(owner.blockOwnerDeletion) || !owner.blockOwnerDeletion)" "Ledger owners cannot block owner deletion"
foreach ($invalid in @(
    "k8sController.workflowExecutor.collaboration.unknown=true"
)) {
    Assert-HelmTemplateFails -SetValues ($collaborationValues + $invalid) -Pattern @("collaboration", "Additional property") -TestName "Collaboration rejects $invalid"
}
Assert-NotContains $collaboration "KOGNITION_EXTERNAL_AGENTS_ENABLED" "ExternalAgent is off unless explicitly enabled"
Assert-NotContains $collaboration "KOGNITION_EXTERNAL_AGENT_ALLOWLIST" "ExternalAgent allow-list is not rendered unless enabled"
$externalAgentValues = $collaborationValues + @(
    "networkPolicy.enabled=true",
    "k8sController.workflowExecutor.collaboration.externalAgents.enabled=true",
    "k8sController.workflowExecutor.collaboration.externalAgents.egressAllowList[0]=agents.partner.example:8443",
    "k8sController.workflowExecutor.collaboration.externalAgents.egressAllowList[1]=*.partners.example:443"
)
$externalAgents = Invoke-HelmTemplate -SetValues $externalAgentValues
Assert-Contains $externalAgents "KOGNITION_EXTERNAL_AGENTS_ENABLED" "ExternalAgent switch is rendered when enabled"
Assert-Contains $externalAgents 'value: "agents.partner.example:8443,*.partners.example:443"' "Controller and webhooks receive the identical allow-list"
Assert-RegexMatch $externalAgents '(?s)name: test-release-kognition-k8s-controller\r?\n(?:(?!\r?\n---).)*?except:\s+- 10\.0\.0\.0/8(?:(?!\r?\n---).)*?port: 8443' "Controller egress on non-HTTPS allow-listed ports excludes private ranges"
Assert-RegexNotMatch $externalAgents '(?s)name: test-release-kognition-k8s-controller\r?\n(?:(?!\r?\n---).)*?- 192\.168\.0\.0/16\s+ports:\s+(?:- port: (?!443\b)\d+\s+protocol: TCP\s+)*- port: 443\b' "The private-range exclusion rule never lists port 443, where it would not restrict anything"
$externalAgentsHttpsOnly = Invoke-HelmTemplate -SetValues ($collaborationValues + @(
    "networkPolicy.enabled=true",
    "k8sController.workflowExecutor.collaboration.externalAgents.enabled=true",
    "k8sController.workflowExecutor.collaboration.externalAgents.egressAllowList[0]=*.partners.example:443"
))
Assert-Contains $externalAgentsHttpsOnly 'value: "*.partners.example:443"' "The HTTPS-only allow-list is still rendered for the controller"
Assert-RegexMatch $externalAgentsHttpsOnly '(?s)name: test-release-kognition-k8s-controller\r?\n(?:(?!\r?\n---).)*?port: 53' "The controller NetworkPolicy is rendered for the HTTPS-only case"
Assert-RegexNotMatch $externalAgentsHttpsOnly '(?s)name: test-release-kognition-k8s-controller\r?\n(?:(?!\r?\n---).)*?except:' "An allow-list on port 443 alone renders no ExternalAgent exclusion rule"
Assert-RegexMatch $externalAgents '(?s)KOGNITION_EXTERNAL_AGENTS_ENABLED.*?KOGNITION_EXTERNAL_AGENTS_ENABLED' "Admission webhooks receive the ExternalAgent switch as well as the controller"
foreach ($invalid in @(
    "k8sController.workflowExecutor.collaboration.externalAgents.egressAllowList[0]=https://agents.partner.example:8443",
    "k8sController.workflowExecutor.collaboration.externalAgents.egressAllowList[0]=agents.partner.example",
    "k8sController.workflowExecutor.collaboration.externalAgents.egressAllowList[0]=agents.partner.example:70000",
    "k8sController.workflowExecutor.collaboration.externalAgents.egressAllowList[0]=localhost:8443"
)) {
    Assert-HelmTemplateFails -SetValues ($externalAgentValues + $invalid) -Pattern @("egressAllowList") -TestName "ExternalAgent rejects allow-list entry $invalid"
}
Assert-HelmTemplateFails -SetValues ($workflowExecutorValues + @("k8sController.workflowExecutor.collaboration.externalAgents.enabled=true")) -Pattern @("requires workflowExecutor.collaboration.enabled") -TestName "ExternalAgent requires collaboration"
Assert-HelmTemplateFails -SetValues (@("mode=kubernetes", "k8sController.workflowExecutor.collaboration.enabled=true")) -Pattern @("collaboration") -TestName "Collaboration requires the native executor"
if ($WorkflowExecutorOnly) {
    if ($Failed -ne 0 -or $Passed -eq 0) { throw ($Errors -join "`n") }
    Write-Output "Workflow executor chart checks passed: $Passed"
    return
}

$makeScript = Get-Content (Join-Path $ChartPath "../../../make.ps1") -Raw
Assert-Contains $makeScript "'./tools/kognition-install', 'bootstrap', '--plan'" "Kubernetes bootstrap uses the verified installation plan"
Assert-Contains $makeScript "'--writers-frozen'" "Kubernetes bootstrap requires frozen writers"
Assert-NotContains $makeScript "`$Chart, '--reuse-values'" "Kubernetes upgrade does not retain stale computed chart defaults"

$prometheusValues = Get-Content (Join-Path $ChartPath "../../observability/prometheus-values.yaml") -Raw
Assert-RegexMatch $prometheusValues '(?m)^kube-state-metrics:\s*\r?\n\s+metricLabelsAllowlist:' "Kube-state-metrics exports dashboard pod labels"
Assert-Contains $prometheusValues 'pods=[kognition.dev/agent-name,kognition.dev/agent-namespace,kognition.dev/agent-instance]' "Pod label allowlist includes all agent dashboard join dimensions"

# ============================================================
# TEST GROUP 1: Mode Exclusivity - Standalone
# ============================================================
Write-Host "[Group 1] Mode Exclusivity - Standalone mode" -ForegroundColor Yellow
$output = Invoke-HelmTemplate -SetValues @("mode=standalone")

foreach ($component in @("apiserver", "controller-manager")) {
    foreach ($setting in @("allowPrivilegeEscalation: false", "readOnlyRootFilesystem: true", "runAsNonRoot: true", "- ALL")) {
        Assert-RegexMatch $output ('(?s)name: test-release-kognition-' + $component + '(?:(?!\r?\n---).)*?name: wait-for-postgres(?:(?!command:).)*' + [regex]::Escape($setting)) "$component database wait container: $setting"
    }
}
foreach ($template in @("postgres-statefulset", "kb-pgvector")) {
    $databaseOutput = (& helm template test-release $ChartPath --set kbPgvector.enabled=true --show-only "templates/$template.yaml") -join "`n"
    if ($LASTEXITCODE -ne 0) { throw "Database template failed: $template" }
    foreach ($identity in @("runAsUser: 65534", "runAsGroup: 65534", "fsGroup: 65534")) {
        Assert-Contains $databaseOutput $identity "$template uses a database identity above 10000"
    }
    Assert-RegexMatch $databaseOutput '(?s)name: postgres\s+image:.*?readOnlyRootFilesystem: true' "$template uses a read-only root filesystem"
    foreach ($mount in @("/var/lib/postgresql/data", "/var/run/postgresql", "/tmp")) {
        Assert-Contains $databaseOutput "mountPath: $mount" "$template has writable mount $mount"
    }
    foreach ($volume in @("postgres-run", "tmp")) {
        Assert-RegexMatch $databaseOutput ("name: " + $volume + '\s+emptyDir: \{\}') "$template declares $volume volume"
    }
}

# The pgvector init hook pod must match the kb-pgvector NetworkPolicy ingress peer or the hook hangs on upgrade.
$pgvectorInitOutput = (& helm template test-release $ChartPath --set kbPgvector.enabled=true --show-only "templates/kb-pgvector.yaml") -join "`n"
Assert-RegexMatch $pgvectorInitOutput '(?s)kind: Job.*?template:\s+metadata:\s+labels:[^\n]*\n(?:\s+app\.kubernetes\.io/[^\n]*\n)*?\s+app\.kubernetes\.io/part-of: kognition' "kb-pgvector init hook pod carries the part-of label required by NetworkPolicy ingress"

Assert-Contains $output "kind: Deployment" "Standalone has Deployments"
Assert-Contains $output "image: kognition-apiserver:" "Standalone uses locally built apiserver image"
Assert-Contains $output "image: kognition-controller-manager:" "Standalone uses locally built controller-manager image"
Assert-Contains $output "image: kognition-memory-service:" "Standalone uses locally built memory-service image"
Assert-Contains $output "name: KOGNITION_MEMORY_SERVICE_URL" "Standalone controller configures memory backend provisioning"
Assert-Contains $output "name: MEMORY_SERVICE_INTERNAL_TOKEN" "Standalone controller and memory service protect raw memory operations"
Assert-RegexMatch $output "name: test-release-kognition-apiserver" "Standalone has apiserver deployment"
Assert-RegexMatch $output "name: test-release-kognition-controller-manager" "Standalone has controller-manager deployment"
Assert-Contains $output "kind: StatefulSet" "Standalone has StatefulSet (postgres)"
Assert-Contains $output "kind: Secret" "Standalone has Secret"
Assert-Contains $output "kind: ConfigMap" "Standalone has ConfigMap (migrations)"
Assert-Contains $output "009_cognitive_audit_log.sql" "Standalone migration bundle includes durable audit storage"
Assert-Contains $output "010_cost_records.sql" "Standalone migration bundle includes durable cost storage"
Assert-Contains $output "011_usage_event_accounting.sql" "Standalone migration bundle includes usage event accounting"
Assert-Contains $output "012_usage_event_immutability.sql" "Standalone migration bundle enforces usage event immutability"
Assert-Contains $output "018_canary_operation_tombstones.sql" "Standalone migration bundle includes permanent canary tombstones"
Assert-RegexMatch $output '(?s)name: test-release-kognition-controller-manager(?:(?!\r?\n---)[\s\S])*?name: KOGNITION_CANARY_OPERATION_RETENTION\s+value: "720h"' "Standalone controller uses the default canary retention"
Assert-Contains $output "kind: ServiceAccount" "Standalone has ServiceAccount"

$standaloneRetentionOutput = Invoke-HelmTemplate -SetValues @("mode=standalone", "canaryOperationRetention=72h")
Assert-RegexMatch $standaloneRetentionOutput '(?s)name: test-release-kognition-controller-manager(?:(?!\r?\n---)[\s\S])*?name: KOGNITION_CANARY_OPERATION_RETENTION\s+value: "72h"' "Standalone controller accepts a canary retention override"

# Standalone must NOT have kubernetes-mode resources
Assert-NotContains $output "kind: ClusterRole" "Standalone has NO ClusterRole"
Assert-NotContains $output "kind: ClusterRoleBinding" "Standalone has NO ClusterRoleBinding"
Assert-NotContains $output "kind: ValidatingWebhookConfiguration" "Standalone has NO ValidatingWebhookConfiguration"
Assert-NotContains $output "kind: MutatingWebhookConfiguration" "Standalone has NO MutatingWebhookConfiguration"
Assert-RegexNotMatch $output "kind: (Issuer|Certificate)" "Standalone has NO cert-manager resources"
Assert-RegexNotMatch $output "name: test-release-kognition-k8s-controller" "Standalone has NO k8s-controller"

# ============================================================
# TEST GROUP 15: Runtime memory policy gateway
# ============================================================
Write-Host "[Group 15] Runtime memory policy gateway" -ForegroundColor Yellow
$output = Invoke-HelmTemplate -SetValues @("mode=kubernetes", "memoryPolicyGateway.enabled=true", "networkPolicy.enabled=true")
Assert-Contains $output "name: test-release-kognition-memory-policy-gateway" "Kubernetes deploys memory policy gateway"
Assert-Contains $output "image: kognition-memory-policy-gateway:" "Memory policy gateway uses first-party image"
Assert-Contains $output "name: KOGNITION_MEMORY_POLICY_GATEWAY_URL" "Kubernetes controller configures memory policy gateway URL"
Assert-Contains $output "name: KOGNITION_MEMORY_POLICY_GATEWAY_PORT" "Kubernetes controller configures memory policy gateway port"
Assert-Contains $output "name: KOGNITION_MEMORY_CAPABILITY_HMAC_KEY" "Kubernetes controller receives capability signing key"
Assert-Contains $output "name: KOGNITION_MEMORY_CAPABILITY_HMAC_KEY_ID" "Kubernetes controller receives capability signing key ID"
Assert-Contains $output "name: KOGNITION_MEMORY_CAPABILITY_HMAC_PREVIOUS_KEY" "Kubernetes controller receives optional previous capability key"
Assert-Contains $output "name: MEMORY_SERVICE_INTERNAL_TOKEN" "Memory service and controller receive internal upstream token"
Assert-Contains $output "name: KOGNITION_AUDIT_NAMESPACE" "Memory service persists write-ahead audit records in the release namespace"
Assert-RegexMatch $output '(?s)kind: Secret\s+metadata:\s+name: test-release-kognition-db(?:(?!\r?\n---)[\s\S])*?DATABASE_URL:' "Kubernetes memory policy stack renders its database Secret"
Assert-RegexMatch $output '(?s)kind: StatefulSet\s+metadata:\s+name: test-release-kognition-postgres' "Kubernetes memory policy stack renders bundled PostgreSQL when enabled"
Assert-RegexMatch $output '(?s)kind: ConfigMap\s+metadata:\s+name: test-release-kognition-migrations' "Kubernetes memory policy stack renders receipt-store migrations"
Assert-RegexMatch $output '(?s)kind: Deployment\s+metadata:\s+name: test-release-kognition-memory-policy-gateway(?:(?!\r?\n---)[\s\S])*?name: DATABASE_URL\s+valueFrom:\s+secretKeyRef:\s+name: test-release-kognition-db\s+key: DATABASE_URL' "Memory Policy Gateway receives its database URL"
Assert-RegexMatch $output '(?s)kind: Deployment\s+metadata:\s+name: test-release-kognition-memory-policy-gateway(?:(?!\r?\n---)[\s\S])*?name: KOGNITION_AUDIT_NAMESPACE\s+value: "default"' "Memory Policy Gateway receives the durable audit namespace"
Assert-RegexMatch $output '(?s)kind: Role\s+metadata:\s+name: test-release-kognition-memory-policy-gateway-audit\s+namespace: default(?:(?!\r?\n---)[\s\S])*?resources: \["configmaps"\]\s+verbs: \["get", "list", "create"\]' "Memory Policy Gateway can persist immutable audit ConfigMaps only in the release namespace"
Assert-RegexMatch $output '(?s)kind: ClusterRole\s+metadata:\s+name: test-release-kognition-memory-policy-gateway(?:(?!\r?\n---)[\s\S])*?resources: \["trustprofiles", "trustprofileindices", "autonomyclasses"\]\s+verbs: \["get", "list", "watch"\]' "Memory Policy Gateway can resolve indexed TrustProfiles across workspaces"
Assert-RegexMatch $output '(?s)kind: ClusterRole\s+metadata:\s+name: test-release-kognition-memory-policy-gateway(?:(?!\r?\n---)[\s\S])*?resources: \["runtimeapprovaltombstones"\]\s+verbs: \["get"\]' "Memory Policy Gateway can authenticate compacted RuntimeApproval replay fences"
Assert-RegexMatch $output '(?s)kind: ClusterRole\s+metadata:\s+name: test-release-kognition-memory-policy-gateway(?:(?!\r?\n---)[\s\S])*?resources: \["runtimeapprovalquotatickets"\]\s+verbs: \["get", "list", "create", "update"\]' "Memory Policy Gateway can reserve and bind RuntimeApproval quota tickets"
Assert-Contains $output "name: audit.kognition.dev" "Kubernetes audit admission webhook records every Kognition resource mutation"
Assert-Contains $output "path: /audit" "Kubernetes audit webhook routes to the write-ahead audit endpoint"
Assert-RegexMatch $output '(?s)resources:\s*\n\s*- configmaps\s*\n\s*verbs: \["get", "list", "watch", "create", "delete"\]' "Kubernetes controller can delete expired audit ConfigMaps"
Assert-RegexMatch $output '(?s)kind: Role\s+metadata:\s+name: test-release-kognition-controller\s+namespace: "default"(?:(?!\r?\n---)[\s\S])*?resources: \["configmaps"\]\s+verbs: \["update"\]' "Kubernetes controller can update receipt ledgers in the release namespace"
Assert-Contains $output "name: KOGNITION_RUNTIME_AUTH_TOKEN" "Kubernetes controller receives runtime control token"
Assert-Contains $output "name: KOGNITION_RUNTIME_AUTH_TOKEN_SECRET_NAME" "Kubernetes controller configures runtime token secret injection"
Assert-RegexMatch $output '(?s)kind: Deployment\s+metadata:\s+name: test-release-kognition-k8s-controller(?:(?!\r?\n---)[\s\S])*?startupProbe:\s+httpGet:\s+path: /healthz\s+port: health\s+failureThreshold: 60' "Kubernetes controller gates liveness on health startup"
Assert-Contains $output "networkpolicies" "Kubernetes controller can manage per-runtime NetworkPolicies"
Assert-Contains $output "app.kubernetes.io/component: memory-policy-gateway" "NetworkPolicy selects memory policy gateway"
Assert-NotContains $output "name: test-release-kognition-agent-runtime" "Chart does not create a release-namespace-only runtime policy"
$memoryMetricsOutput = Invoke-HelmTemplate -SetValues @("mode=kubernetes", "memoryPolicyGateway.enabled=true", "networkPolicy.enabled=true", "observability.metrics.serviceMonitor.enabled=true")
Assert-RegexMatch $memoryMetricsOutput '(?s)kind: Service\s+metadata:\s+name: test-release-kognition-memory-policy-gateway\s+namespace: default\s+labels:(?:(?!\r?\nspec:)[\s\S])*?app\.kubernetes\.io/component: memory-policy-gateway(?:(?!\r?\nspec:)[\s\S])*?app\.kubernetes\.io/instance: test-release' "Memory Policy Gateway Service satisfies its ServiceMonitor selector"
Assert-RegexMatch $memoryMetricsOutput '(?s)kind: ServiceMonitor\s+metadata:\s+name: test-release-kognition-memory-policy-gateway(?:(?!\r?\n---)[\s\S])*?app.kubernetes.io/component: memory-policy-gateway(?:(?!\r?\n---)[\s\S])*?port: http\s+path: /metrics' "Memory Policy Gateway metrics are discovered through its HTTP service"
Assert-RegexMatch $memoryMetricsOutput '(?s)kind: NetworkPolicy\s+metadata:\s+name: test-release-kognition-memory-policy-gateway(?:(?!\r?\n---)[\s\S])*?app.kubernetes.io/name: prometheus(?:(?!\r?\n---)[\s\S])*?port: 8091' "Memory Policy Gateway permits Prometheus metrics ingress"
Assert-HelmTemplateFails `
    -SetValues @("mode=kubernetes", "networkPolicy.enabled=true") `
    -Pattern "networkPolicy.kubeletProbeIngressCIDRs must list" `
    -TestName "NetworkPolicy: missing kubelet probe CIDRs fail fast"
$ciliumProbePolicyOutput = Invoke-HelmTemplate -SetValues @("mode=kubernetes", "k8sController.managedNamespaces[0]=team-a", "networkPolicy.enabled=true", "networkPolicy.ciliumHostProbeIngress=true", "toolGateway.enabled=true")
Assert-Contains $ciliumProbePolicyOutput "kind: CiliumNetworkPolicy" "Cilium: host probe policy renders when enabled"
Assert-Contains $ciliumProbePolicyOutput "kind: CiliumClusterwideNetworkPolicy" "Cilium: dynamic runtime probe policy renders for enrolled namespaces"
Assert-RegexMatch $ciliumProbePolicyOutput '(?s)kind: CiliumClusterwideNetworkPolicy(?:(?!\r?\n---)[\s\S])*?key: k8s:io\.kubernetes\.pod\.namespace\s+operator: In\s+values: \["team-a"\]' "Cilium: runtime probe ingress is limited to enrolled namespaces"
Assert-Contains $ciliumProbePolicyOutput "fromEntities:" "Cilium: host probe policy uses Cilium entities"
Assert-Contains $ciliumProbePolicyOutput "remote-node" "Cilium: host probe policy permits remote nodes"
Assert-NotContains $ciliumProbePolicyOutput "- world" "Cilium: probe policy does not permit world traffic"
Assert-Contains $ciliumProbePolicyOutput "- host" "Cilium: probe policy permits the local node"
Assert-Contains $ciliumProbePolicyOutput "- health" "Cilium: probe policy permits health endpoints"
Assert-RegexNotMatch $ciliumProbePolicyOutput "rules:\s*\n\s*http:" "Cilium: probe policy uses port-scoped L4 access for node-masqueraded probes"
Assert-Contains $ciliumProbePolicyOutput "kube-apiserver" "Cilium: control-plane egress explicitly permits the Kubernetes API"
Assert-RegexMatch $ciliumProbePolicyOutput '(?s)name: test-release-kognition-kube-apiserver-egress(?:(?!\r?\n---)[\s\S])*?k8s:app\.kubernetes\.io/component: k8s-controller(?:(?!\r?\n---)[\s\S])*?k8s:app\.kubernetes\.io/instance: test-release' "Cilium: Kubernetes API egress selects only this release's controller"
Assert-RegexNotMatch $ciliumProbePolicyOutput '(?s)name: test-release-kognition-kube-apiserver-egress(?:(?!\r?\n---)[\s\S])*?k8s:app\.kubernetes\.io/part-of: kognition' "Cilium: Kubernetes API egress must not isolate migrations or other control-plane pods"
Assert-RegexNotMatch $ciliumProbePolicyOutput '(?s)name: test-release-kognition-kube-apiserver-egress(?:(?!\r?\n---)[\s\S])*?toPorts:' "Cilium: Kubernetes API egress relies on the API-server entity across service DNAT"

# ============================================================
# TEST GROUP 15b: AKS Admissions Enforcer opt-out
# ============================================================
$enforcerLabel = 'admissions.enforcer/disabled: "true"'
$enforcerDefaultOutput = Invoke-HelmTemplate -SetValues @("mode=kubernetes")
Assert-NotContains $enforcerDefaultOutput "admissions.enforcer/disabled" "AKS enforcer: webhook configurations are untouched by default"
$enforcerOptOutOutput = Invoke-HelmTemplate -SetValues @("mode=kubernetes", "global.aksAdmissionsEnforcerOptOut=true")
$enforcerConfigurations = @($enforcerOptOutOutput -split '(?m)^---\s*$' | Where-Object { $_ -match '(?m)^kind: (Validating|Mutating)WebhookConfiguration' })
Assert-RegexMatch ([string]$enforcerConfigurations.Count) '^3$' "AKS enforcer: guard, validating and mutating webhook configurations all render"
foreach ($configuration in $enforcerConfigurations) {
    $configurationName = [regex]::Match($configuration, '(?m)^  name: (\S+)').Groups[1].Value
    Assert-RegexMatch $configuration ('(?s)metadata:(?:(?!\r?\nwebhooks:)[\s\S])*?labels:(?:(?!\r?\nwebhooks:)[\s\S])*?' + [regex]::Escape($enforcerLabel)) "AKS enforcer: $configurationName carries the opt-out label so the reviewed selector is not rewritten"
}
Assert-HelmTemplateFails `
    -SetValues @("mode=kubernetes", "global.aksAdmissionsEnforcerOptOut=maybe") `
    -Pattern "aksAdmissionsEnforcerOptOut" `
    -TestName "AKS enforcer: opt-out must be boolean"

# ============================================================
# TEST GROUP 16: Runtime Tool Gateway
# ============================================================
foreach ($mode in @("kubernetes", "standalone")) {
    $existingDatabaseOutput = Invoke-HelmTemplate -SetValues @("mode=$mode", "postgres.auth.existingSecret=demo-database", "postgres.auth.password=must-not-render", "memoryService.backendType=pgvector")
    Assert-NotContains $existingDatabaseOutput "must-not-render" "$mode existing database Secret keeps credentials out of Helm manifests"
    Assert-RegexNotMatch $existingDatabaseOutput '(?s)kind: Secret\s+metadata:\s+name: test-release-kognition-(db|postgres)\b' "$mode existing database Secret suppresses generated database credentials"
    Assert-RegexMatch $existingDatabaseOutput 'name: POSTGRES_PASSWORD\s+valueFrom:\s+secretKeyRef:\s+name: demo-database\s+key: password' "$mode PostgreSQL uses existing password"
    Assert-RegexMatch $existingDatabaseOutput 'name: DATABASE_URL\s+valueFrom:\s+secretKeyRef:\s+name: demo-database\s+key: DATABASE_URL' "$mode database client uses existing DSN"
    Assert-RegexNotMatch $existingDatabaseOutput 'secretKeyRef:\s+name: test-release-kognition-db\s+key: DATABASE_URL' "$mode all database consumers use selected Secret"
}
$externalDatabaseOutput = Invoke-HelmTemplate -SetValues @("mode=kubernetes", "postgres.enabled=false", "postgres.auth.existingSecret=external-database", "memoryService.backendType=pgvector")
Assert-RegexMatch $externalDatabaseOutput 'secretKeyRef:\s+name: external-database\s+key: DATABASE_URL' "External PostgreSQL also supports an existing Secret"
Assert-RegexNotMatch $externalDatabaseOutput '(?s)kind: StatefulSet\s+metadata:\s+name: test-release-kognition-postgres' "External database does not deploy bundled PostgreSQL"

Write-Host "[Group 16] Runtime Tool Gateway" -ForegroundColor Yellow
$output = Invoke-HelmTemplate -SetValues @("mode=kubernetes", "toolGateway.enabled=true", "networkPolicy.enabled=true")
Assert-Contains $output "name: test-release-kognition-tool-gateway" "Kubernetes deploys Tool Gateway"
Assert-Contains $output "image: kognition-tool-gateway:" "Tool Gateway uses first-party image"
Assert-Contains $output "name: KOGNITION_TOOL_GATEWAY_URL" "Kubernetes controller configures Tool Gateway URL"
Assert-Contains $output "name: KOGNITION_TOOL_CAPABILITY_HMAC_KEY" "Kubernetes controller receives Tool Gateway signing key"
Assert-Contains $output "name: KOGNITION_TOOL_CAPABILITY_HMAC_KEY_ID" "Kubernetes controller receives Tool Gateway signing key ID"
Assert-Contains $output "name: KOGNITION_TOOL_CAPABILITY_HMAC_PREVIOUS_KEY" "Kubernetes controller receives optional previous Tool Gateway key"
Assert-Contains $output "app.kubernetes.io/component: tool-gateway" "NetworkPolicy selects Tool Gateway"
Assert-NotContains $output "app: approved-tool" "Tool Gateway omits unlisted private target Pods by default"
Assert-Contains $output 'resources: ["leases"]' "Tool Gateway can coordinate shared resilience with Kubernetes Leases"
Assert-RegexMatch $output '(?s)resources: \["cognitivequotas/status"\]\s+verbs: \["get", "update", "patch"\]' "Tool Gateway can reserve tool-call quota through CognitiveQuota status"
Assert-RegexMatch $output '(?s)kind: ClusterRole\s+metadata:\s+name: test-release-kognition-tool-gateway(?:(?!\r?\n---)[\s\S])*?resources: \["runtimeapprovaltombstones"\]\s+verbs: \["get"\]' "Tool Gateway can authenticate compacted RuntimeApproval replay fences"
Assert-RegexMatch $output '(?s)kind: ClusterRole\s+metadata:\s+name: test-release-kognition-tool-gateway(?:(?!\r?\n---)[\s\S])*?resources: \["runtimeapprovalquotatickets"\]\s+verbs: \["get", "list", "create", "update"\]' "Tool Gateway can reserve and bind RuntimeApproval quota tickets"
Assert-RegexMatch $output '(?s)kind: Role\s+metadata:\s+name: test-release-kognition-tool-gateway-ledger\s+namespace: default(?:(?!\r?\n---)[\s\S])*?resources: \["configmaps"\]\s+verbs: \["get", "list", "create", "update"\]' "Tool Gateway can update ledger ConfigMaps only in the release namespace"

$agentCommunicationGatewayOutput = Invoke-HelmTemplate -SetValues @("mode=kubernetes", "agentCommunicationGateway.enabled=true", "networkPolicy.enabled=true")
Assert-Contains $agentCommunicationGatewayOutput "name: test-release-kognition-agent-communication-gateway" "Kubernetes deploys Agent Communication Gateway"
Assert-Contains $agentCommunicationGatewayOutput "image: kognition-agent-communication-gateway:" "Agent Communication Gateway uses first-party image"
Assert-Contains $agentCommunicationGatewayOutput "name: KOGNITION_AGENT_COMMUNICATION_GATEWAY_URL" "Kubernetes controller configures Agent Communication Gateway URL"
Assert-Contains $agentCommunicationGatewayOutput "name: AGENT_DELEGATION_CAPABILITY_HMAC_KEY" "Agent Communication Gateway receives delegation signing key"
Assert-Contains $agentCommunicationGatewayOutput "name: KOGNITION_RUNTIME_AUTH_TOKEN" "Agent Communication Gateway receives runtime control token"
Assert-Contains $agentCommunicationGatewayOutput "name: KOGNITION_RUNTIME_APPROVAL_BINDING_KEYS_JSON" "Agent Communication Gateway receives RuntimeApproval binding keys"
Assert-RegexMatch $agentCommunicationGatewayOutput '(?s)name: KOGNITION_RUNTIME_APPROVAL_MAXIMUM_CLOCK_SKEW\s+value: "30s"' "Agent Communication Gateway receives RuntimeApproval maximum clock skew"
Assert-RegexMatch $agentCommunicationGatewayOutput '(?s)name: KOGNITION_RUNTIME_APPROVAL_WRITER_SERVICE_ACCOUNTS\s+value: "test-release-kognition-tool-gateway,test-release-kognition-memory-policy-gateway,test-release-kognition-agent-communication-gateway"' "Kubernetes admission trusts enabled RuntimeApproval gateway writers"
Assert-RegexMatch $agentCommunicationGatewayOutput '(?s)kind: ClusterRole\s+metadata:\s+name: test-release-kognition-agent-communication-gateway(?:(?!\r?\n---)[\s\S])*?resources: \["trustprofiles", "trustprofileindices", "autonomyclasses"\]\s+verbs: \["get", "list", "watch"\]' "Agent Communication Gateway can resolve autonomy policy across workspaces"
Assert-RegexMatch $agentCommunicationGatewayOutput '(?s)kind: ClusterRole\s+metadata:\s+name: test-release-kognition-agent-communication-gateway(?:(?!\r?\n---)[\s\S])*?resources: \["runtimeapprovals"\]\s+verbs: \["get", "list", "watch", "create", "update", "patch"\]' "Agent Communication Gateway can coordinate RuntimeApprovals"
Assert-RegexMatch $agentCommunicationGatewayOutput '(?s)kind: ClusterRole\s+metadata:\s+name: test-release-kognition-agent-communication-gateway(?:(?!\r?\n---)[\s\S])*?resources: \["runtimeapprovaltombstones"\]\s+verbs: \["get"\]' "Agent Communication Gateway can authenticate compacted RuntimeApproval replay fences"
Assert-RegexMatch $agentCommunicationGatewayOutput '(?s)kind: ClusterRole\s+metadata:\s+name: test-release-kognition-agent-communication-gateway(?:(?!\r?\n---)[\s\S])*?resources: \["runtimeapprovalquotatickets"\]\s+verbs: \["get", "list", "create", "update"\]' "Agent Communication Gateway can reserve and bind RuntimeApproval quota tickets"
Assert-Contains $agentCommunicationGatewayOutput 'resources: ["agents", "agentinstances", "agentidentities", "agentservices", "agentcommunicationpolicies", "crossnamespacegrants"]' "Agent Communication Gateway reads only delegation authorization resources"
Assert-RegexMatch $agentCommunicationGatewayOutput '(?s)kind: ClusterRole\s+metadata:\s+name: test-release-kognition-agent-communication-gateway(?:(?!\r?\n---)[\s\S])*?resources: \["leases"\]\s+verbs: \["create"\]' "Agent Communication Gateway can create admission-constrained RuntimeApproval leases"
Assert-RegexMatch $agentCommunicationGatewayOutput '(?s)kind: ClusterRole\s+metadata:\s+name: test-release-kognition-agent-communication-gateway(?:(?!\r?\n---)[\s\S])*?resources: \["leases"\]\s+resourceNames: \["kognition-runtime-approval-records"\]\s+verbs: \["get", "update"\]' "Agent Communication Gateway reads and updates only its named RuntimeApproval binding-key lease"
Assert-RegexNotMatch $agentCommunicationGatewayOutput '(?s)kind: ClusterRole\s+metadata:\s+name: test-release-kognition-agent-communication-gateway(?:(?!\r?\n---)[\s\S])*?resources: \["leases"\]\s+verbs: \["get", "create", "update"\]' "Agent Communication Gateway has no unrestricted cluster-wide lease reads or updates"
Assert-Contains $agentCommunicationGatewayOutput "app.kubernetes.io/component: agent-communication-gateway" "NetworkPolicy selects Agent Communication Gateway"
$legacyAgentCommunicationGatewayOutput = Invoke-HelmTemplate -SetValues @("mode=kubernetes", "agentCommunicationGateway.enabled=true", "agentCommunicationGateway.image=null", "agentCommunicationGateway.resources=null")
Assert-Contains $legacyAgentCommunicationGatewayOutput "name: test-release-kognition-agent-communication-gateway" "Agent Communication Gateway renders with nested defaults absent from reused release values"
$agentCommunicationMonitoringOutput = Invoke-HelmTemplate -SetValues @("mode=kubernetes", "agentCommunicationGateway.enabled=true", "agentCommunicationGateway.monitoring.serviceMonitor.enabled=true", "agentCommunicationGateway.monitoring.prometheusRule.enabled=true", "agentCommunicationGateway.monitoring.grafanaDashboard.enabled=true")
Assert-Contains $agentCommunicationMonitoringOutput "kind: ServiceMonitor" "Agent Communication Gateway renders opt-in ServiceMonitor"
Assert-Contains $agentCommunicationMonitoringOutput "kognition_agent_communication_gateway_requests_total" "Agent Communication Gateway alert rules reference request metrics"
Assert-Contains $agentCommunicationMonitoringOutput "KognitionAgentCommunicationGatewayFailures" "Agent Communication Gateway renders failure alert"
Assert-Contains $agentCommunicationMonitoringOutput "Kognition Agent Communication Gateway" "Agent Communication Gateway renders Grafana dashboard"

# ============================================================
# TEST GROUP 16A: Experimental plugin host packaging
# ============================================================
Write-Host "[Group 16A] Experimental plugin host packaging" -ForegroundColor Yellow
$pluginHostDisabledOutput = Invoke-HelmTemplate -SetValues @("mode=kubernetes")
Assert-NotContains $pluginHostDisabledOutput "KOGNITION_PLUGIN_HOST_ENABLED" "Plugin host: disabled by default"
Assert-NotContains $pluginHostDisabledOutput "name: plugin-sockets" "Plugin host: socket volume omitted by default"

$pluginHostOutput = Invoke-HelmTemplate -SetValues @(
    "mode=kubernetes",
    "k8sController.pluginHost.enabled=true",
    "k8sController.pluginHost.socketDir=/run/test-plugins",
    "k8sController.pluginHost.hostVersion=test-host",
    "k8sController.pluginHost.discoveryInterval=11s",
    "k8sController.pluginHost.probeInterval=12s",
    "k8sController.pluginHost.probeTimeout=3s",
    "k8sController.pluginHost.connectTimeout=4s",
    "k8sController.pluginHost.reconnectBackoff=1s",
    "k8sController.pluginHost.maxReconnectBackoff=5s",
    "k8sController.pluginHost.sidecars[0].name=test-runtime",
    "k8sController.pluginHost.sidecars[0].image=example.com/runtime-plugin:v1",
    "k8sController.pluginHost.sidecars[0].args[0]=--socket=/run/test-plugins/runtime.sock"
)
Assert-RegexMatch $pluginHostOutput 'name: KOGNITION_PLUGIN_HOST_ENABLED\s+value: "true"' "Plugin host: opt-in flag renders"
Assert-RegexMatch $pluginHostOutput 'name: KOGNITION_PLUGIN_SOCKET_DIR\s+value: "/run/test-plugins"' "Plugin host: socket environment renders"
Assert-RegexMatch $pluginHostOutput 'name: KOGNITION_PLUGIN_HOST_VERSION\s+value: "test-host"' "Plugin host: version environment renders"
Assert-RegexMatch $pluginHostOutput 'name: KOGNITION_PLUGIN_DISCOVERY_INTERVAL\s+value: "11s"' "Plugin host: discovery environment renders"
Assert-RegexMatch $pluginHostOutput 'name: KOGNITION_PLUGIN_MAX_RECONNECT_BACKOFF\s+value: "5s"' "Plugin host: reconnect environment renders"
Assert-RegexMatch $pluginHostOutput 'name: plugin-sockets\s+mountPath: "/run/test-plugins"' "Plugin host: writable socket mount renders"
Assert-RegexMatch $pluginHostOutput 'name: plugin-sockets\s+emptyDir: \{\}' "Plugin host: pod-local socket volume renders"
Assert-RegexMatch $pluginHostOutput 'readOnlyRootFilesystem: true' "Plugin host: controller root filesystem remains read-only"
Assert-RegexMatch $pluginHostOutput '(?s)- name: test-runtime\s+image: "example.com/runtime-plugin:v1"(?:(?!\n\s*- name:)[\s\S])*?allowPrivilegeEscalation: false(?:(?!\n\s*- name:)[\s\S])*?readOnlyRootFilesystem: true' "Plugin host: sidecar inherits secure container defaults"
Assert-RegexMatch $pluginHostOutput '(?s)- name: test-runtime.*?volumeMounts:\s+- name: plugin-sockets\s+mountPath: "/run/test-plugins"' "Plugin host: sidecar mounts the shared socket directory"
Assert-HelmTemplateFails `
    -SetValues @("mode=kubernetes", "k8sController.pluginHost.enabled=not-a-boolean") `
    -Pattern @("got string, want boolean", "Expected: boolean, given: string") `
    -TestName "Plugin host: schema rejects a non-boolean enable flag"
Assert-HelmTemplateFails `
    -SetValues @("mode=kubernetes", "k8sController.pluginHost.probeInterval=not-a-duration") `
    -Pattern "Does not match pattern" `
    -TestName "Plugin host: schema rejects invalid duration syntax"
Assert-HelmTemplateFails `
    -SetValues @("mode=kubernetes", "k8sController.pluginHost.enabled=true", "k8sController.pluginHost.sidecars[0].name=Invalid_Name", "k8sController.pluginHost.sidecars[0].image=example/plugin:v1") `
    -Pattern "Does not match pattern" `
    -TestName "Plugin host: schema rejects an invalid sidecar name"

# ============================================================
# TEST GROUP 17: Runtime Model Gateway
# ============================================================
Write-Host "[Group 17] Runtime Model Gateway" -ForegroundColor Yellow
$modelGatewayOutput = Invoke-HelmTemplate -SetValues @("mode=kubernetes", "modelGateway.enabled=true", "networkPolicy.enabled=true")
Assert-Contains $modelGatewayOutput "name: test-release-kognition-model-gateway" "Kubernetes deploys Model Gateway"
Assert-RegexMatch $modelGatewayOutput '(?s)kind: Service\s+metadata:\s+name: test-release-kognition-model-gateway(?:(?!\r?\n---)[\s\S])*?labels:(?:(?!\r?\nspec:)[\s\S])*?app\.kubernetes\.io/component: model-gateway\s+app\.kubernetes\.io/instance: test-release\s+spec:' "Model Gateway Service labels match its ServiceMonitor selector"
Assert-Contains $modelGatewayOutput "image: kognition-model-gateway:" "Model Gateway uses first-party image"
Assert-Contains $modelGatewayOutput "name: KOGNITION_MODEL_GATEWAY_URL" "Kubernetes controller configures Model Gateway URL"
Assert-Contains $modelGatewayOutput "name: KOGNITION_MODEL_CAPABILITY_HMAC_KEY" "Kubernetes controller receives Model Gateway signing key"
Assert-Contains $modelGatewayOutput "app.kubernetes.io/component: model-gateway" "NetworkPolicy selects Model Gateway"
$knowledgeGatewayOutput = Invoke-HelmTemplate -SetValues @("mode=kubernetes", "kbIngester.enabled=true", "knowledgeGateway.enabled=true", "knowledgeGateway.tls.secretName=knowledge-gateway-tls", "knowledgeGateway.runtimeTLS.secretName=knowledge-gateway-runtime-tls", "knowledgeGateway.runtimeTLS.serverName=test-release-kognition-knowledge-gateway.default.svc.cluster.local", "knowledgeGateway.provider.address=knowledge-azuresearch.default.svc.cluster.local:9443", "knowledgeGateway.provider.serverName=knowledge-azuresearch.default.svc.cluster.local", "knowledgeGateway.provider.peerURI=spiffe://kognition.dev/provider/azure-ai-search", "knowledgeGateway.provider.kubernetesApiEgressCIDRs[0]=10.0.0.1/32", "knowledgeGateway.provider.networkPolicy.namespaceSelector.kubernetes\.io/metadata\.name=default", "knowledgeGateway.provider.networkPolicy.podSelector.app\.kubernetes\.io/name=kognition-knowledge-azuresearch", "networkPolicy.enabled=true", "networkPolicy.ciliumHostProbeIngress=true", "observability.metrics.serviceMonitor.enabled=true")
Assert-Contains $knowledgeGatewayOutput "name: test-release-kognition-knowledge-gateway" "Kubernetes deploys Knowledge Gateway"
Assert-Contains $knowledgeGatewayOutput "image: kognition-knowledge-gateway:" "Knowledge Gateway uses first-party image"
Assert-Contains $knowledgeGatewayOutput "name: KOGNITION_KNOWLEDGE_GATEWAY_URL" "Kubernetes controller configures Knowledge Gateway URL"
Assert-Contains $knowledgeGatewayOutput "https://test-release-kognition-knowledge-gateway.default.svc.cluster.local:8095" "Runtime Knowledge Gateway URL uses HTTPS"
Assert-Contains $knowledgeGatewayOutput "name: KOGNITION_KNOWLEDGE_RUNTIME_TLS_CERT_FILE" "Knowledge Gateway receives a distinct runtime server identity"
Assert-Contains $knowledgeGatewayOutput "name: KOGNITION_KNOWLEDGE_GATEWAY_CA_SECRET_NAME" "Controller receives the runtime trust source"
Assert-RegexMatch $knowledgeGatewayOutput '(?s)name: runtime-tls\s+secret:\s+secretName: knowledge-gateway-runtime-tls\s+items:\s+- \{key: tls\.crt, path: tls\.crt\}\s+- \{key: tls\.key, path: tls\.key\}' "Gateway runtime TLS mount excludes the CA from its served identity volume"
Assert-Contains $knowledgeGatewayOutput "name: KOGNITION_KNOWLEDGE_CAPABILITY_HMAC_KEY" "Kubernetes controller receives Knowledge Gateway signing key"
Assert-Contains $knowledgeGatewayOutput "name: KNOWLEDGE_CAPABILITY_HMAC_KEY" "Knowledge Gateway receives its verification key"
Assert-Contains $knowledgeGatewayOutput 'object.metadata.name.startsWith("kognition-mch-knowledge-gateway-")' "Knowledge Gateway has its own passive-health component"
Assert-Contains $knowledgeGatewayOutput "request.operation != 'DELETE'" "Gateway health writers cannot delete publications"
Assert-RegexMatch $knowledgeGatewayOutput '(?s)kind: Role\s+metadata:\s+name: test-release-kognition-knowledge-gateway-cost(?:(?!\r?\n---)[\s\S])*?resources: \["configmaps"\]\s+verbs: \["get", "list", "create", "update"\]' "Knowledge Gateway reuses existing ConfigMap verbs without delete"
Assert-RegexMatch $knowledgeGatewayOutput '(?s)kind: Role\s+metadata:\s+name: test-release-kognition-knowledge-gateway\s+namespace: default(?:(?!\r?\n---)[\s\S])*?resources: \["agents", "agentinstances", "agentidentities", "knowledgebaseclaims", "knowledgebases", "models", "modelinvocationprofiles", "modelgrants", "agentpolicies", "secretrefs", "cognitivequotas"\]\s+verbs: \["get", "list", "watch"\]' "Knowledge Gateway resolves current query and embedding authorization only in enrolled namespaces"
Assert-RegexMatch $knowledgeGatewayOutput '(?s)kind: ClusterRole\s+metadata:\s+name: test-release-kognition-knowledge-gateway(?:(?!\r?\n---)[\s\S])*?resources: \["knowledgebaseclasses", "modelclasses"\]\s+verbs: \["get", "list"\]' "Knowledge Gateway cluster role reads only provider and Model classes"
Assert-RegexNotMatch $knowledgeGatewayOutput '(?s)kind: ClusterRole\s+metadata:\s+name: test-release-kognition-knowledge-gateway(?:(?!\r?\n---)[\s\S])*?resources: \[[^\]]*"(?:secrets|secretrefs|models|agents)"' "Knowledge Gateway does not receive cluster-wide tenant or credential reads"
Assert-Contains $knowledgeGatewayOutput 'resources: ["azuresearchbackends"]' "Knowledge Gateway reads only the registered provider configuration"
Assert-Contains $knowledgeGatewayOutput "name: KOGNITION_KNOWLEDGE_PROVIDER_NAME" "Kubernetes controller receives provider identity for indexing"
Assert-Contains $knowledgeGatewayOutput "name: KOGNITION_KNOWLEDGE_TLS_SECRET_NAME" "Kubernetes controller receives provider TLS source for indexing"
Assert-RegexMatch $knowledgeGatewayOutput '(?s)kind: ClusterRole\s+metadata:\s+name: test-release-kognition-controller(?:(?!\r?\n---)[\s\S])*?apiGroups: \["knowledge\.kognition\.dev"\]\s+resources: \["knowledgebaseclasses"\]\s+verbs: \["get"\]' "Kubernetes controller can resolve cluster-scoped provider classes"
Assert-RegexMatch $knowledgeGatewayOutput '(?s)kind: Role\s+metadata:\s+name: test-release-kognition-controller\s+namespace: "default"(?:(?!\r?\n---)[\s\S])*?apiGroups: \["knowledge\.kognition\.dev"\]\s+resources: \["knowledgebases"\]\s+verbs: \["get"\]' "Kubernetes controller can resolve namespaced provider bindings"
Assert-Contains $knowledgeGatewayOutput "app.kubernetes.io/component: knowledge-gateway" "NetworkPolicy selects Knowledge Gateway"
Assert-RegexMatch $knowledgeGatewayOutput '(?s)kind: ServiceMonitor\s+metadata:\s+name: test-release-kognition-knowledge-gateway(?:(?!\r?\n---)[\s\S])*?scheme: https(?:(?!\r?\n---)[\s\S])*?name: knowledge-gateway-runtime-tls' "Knowledge Gateway metrics use verified HTTPS"
Assert-RegexMatch $knowledgeGatewayOutput '(?s)name: test-release-kognition-knowledge-gateway(?:(?!\r?\n---)[\s\S])*?cidr: 10\.0\.0\.1/32(?:(?!\r?\n---)[\s\S])*?port: 443' "Knowledge Gateway limits Kubernetes API egress to configured CIDRs"
Assert-RegexMatch $knowledgeGatewayOutput '(?s)kind: NetworkPolicy\s+metadata:\s+name: test-release-kognition-knowledge-gateway(?:(?!\r?\n---)[\s\S])*?cidr: 0\.0\.0\.0/0\s+ports:\s+- port: 443\s+protocol: TCP' "Knowledge Gateway owns HTTPS embedding-provider egress while runtimes remain gateway-only"
Assert-RegexMatch $knowledgeGatewayOutput '(?s)name: test-release-kognition-knowledge-gateway-kube-apiserver-egress(?:(?!\r?\n---)[\s\S])*?k8s:app\.kubernetes\.io/component: knowledge-gateway(?:(?!\r?\n---)[\s\S])*?k8s:app\.kubernetes\.io/instance: test-release(?:(?!\r?\n---)[\s\S])*?toEntities:\s+- kube-apiserver' "Cilium: Knowledge Gateway can resolve live bindings through the Kubernetes API"
Assert-Contains $modelGatewayOutput "name: MODEL_CAPABILITY_HMAC_KEY" "Model Gateway receives its signing key"
Assert-Contains $modelGatewayOutput "name: KOGNITION_COST_NAMESPACE" "Model Gateway persists normalized cost records in the release namespace"
Assert-Contains $modelGatewayOutput 'resources: ["agents", "agentinstances", "agentidentities", "agentpolicies", "modelclasses", "modelgrants", "models", "modelinvocationprofiles", "secretrefs"]' "Model Gateway reads only required policy, profile and credential resources"
Assert-RegexMatch $modelGatewayOutput '(?s)resources: \["modelinvocationprofiles"\]\s+verbs: \["get", "list", "watch"\]' "Invocation profiles are read-only to the controller"
Assert-RegexMatch $modelGatewayOutput '(?s)resources: \["cognitivequotas"\]\s+verbs: \["get", "list"\]' "Model Gateway reads CognitiveQuota specifications without mutation"
Assert-RegexMatch $modelGatewayOutput '(?s)resources: \["cognitivequotas/status"\]\s+verbs: \["get", "update"\]' "Model Gateway projects model usage through CognitiveQuota status"
Assert-RegexMatch $modelGatewayOutput '(?s)kind: Role\s+metadata:\s+name: test-release-kognition-model-gateway-cost\s+namespace: default(?:(?!\r?\n---)[\s\S])*?resources: \["configmaps"\]\s+verbs: \["get", "list", "create", "update"\]' "Model Gateway can persist cost and audit ConfigMaps only in the release namespace"
Assert-RegexMatch $modelGatewayOutput '(?s)kind: Role\s+metadata:\s+name: test-release-kognition-model-gateway-cost\s+namespace: default(?:(?!\r?\n---)[\s\S])*?resources: \["leases"\]\s+verbs: \["get", "create", "update"\]' "Model Gateway can serialize usage projection through release-namespace Leases"
$privateModelOutput = Invoke-HelmTemplate -SetValues @("mode=kubernetes", "modelGateway.enabled=true", "networkPolicy.enabled=true", "modelGateway.privateEgress[0].namespaceSelector.kubernetes\.io/metadata\.name=models", "modelGateway.privateEgress[0].podSelector.app=vllm", "modelGateway.privateEgress[0].ports[0]=8080")
Assert-RegexMatch $privateModelOutput '(?s)kind: NetworkPolicy\s+metadata:\s+name: test-release-kognition-model-gateway(?:(?!\r?\n---)[\s\S])*?namespaceSelector:\s+matchLabels:\s+kubernetes\.io/metadata\.name: models(?:(?!\r?\n---)[\s\S])*?podSelector:\s+matchLabels:\s+app: vllm(?:(?!\r?\n---)[\s\S])*?port: 8080' "Model Gateway permits only the explicitly selected private provider target"
Assert-RegexMatch $privateModelOutput '(?s)kind: NetworkPolicy\s+metadata:\s+name: test-release-kognition-k8s-controller(?:(?!\r?\n---)[\s\S])*?namespaceSelector:\s+matchLabels:\s+kubernetes\.io/metadata\.name: models(?:(?!\r?\n---)[\s\S])*?podSelector:\s+matchLabels:\s+app: vllm(?:(?!\r?\n---)[\s\S])*?port: 8080' "Kubernetes controller validates only the declared private Model provider target"
$legacyModelGatewayOutput = Invoke-HelmTemplate -SetValues @("mode=kubernetes", "modelGateway.enabled=true", "modelGateway.service=null", "modelGateway.image=null", "modelGateway.resources=null", "networkPolicy.enabled=true")
Assert-Contains $legacyModelGatewayOutput "name: test-release-kognition-model-gateway" "Model Gateway renders with nested defaults absent from reused release values"

$privateToolOutput = Invoke-HelmTemplate -SetValues @(
    "mode=kubernetes",
    "toolGateway.enabled=true",
    "networkPolicy.enabled=true",
    "toolGateway.privateEgress[0].namespaceSelector.kubernetes\.io/metadata\.name=tools",
    "toolGateway.privateEgress[0].podSelector.app=approved-tool",
    "toolGateway.privateEgress[0].ports[0]=8080"
)
Assert-RegexMatch $privateToolOutput '(?s)kind: NetworkPolicy\s+metadata:\s+name: test-release-kognition-tool-gateway(?:(?!\r?\n---)[\s\S])*?namespaceSelector:\s+matchLabels:\s+kubernetes\.io/metadata\.name: tools(?:(?!\r?\n---)[\s\S])*?podSelector:\s+matchLabels:\s+app: approved-tool(?:(?!\r?\n---)[\s\S])*?port: 8080' "Tool Gateway permits only the explicitly selected private target"
Assert-RegexMatch $privateToolOutput '(?s)kind: NetworkPolicy\s+metadata:\s+name: test-release-kognition-k8s-controller(?:(?!\r?\n---)[\s\S])*?namespaceSelector:\s+matchLabels:\s+kubernetes\.io/metadata\.name: tools(?:(?!\r?\n---)[\s\S])*?podSelector:\s+matchLabels:\s+app: approved-tool(?:(?!\r?\n---)[\s\S])*?port: 8080' "Kubernetes controller health-checks only the declared private Tool target"
Assert-RegexMatch $privateToolOutput '(?s)kind: NetworkPolicy\s+metadata:\s+name: test-release-kognition-k8s-controller(?:(?!\r?\n---)[\s\S])*?namespaceSelector: \{\}\s+podSelector:\s+matchExpressions:\s+- key: kognition\.dev/agent\s+operator: Exists(?:(?!\r?\n---)[\s\S])*?port: 8080' "Kubernetes controller can invoke only agent runtime Pods"
$memoryControllerPolicyOutput = Invoke-HelmTemplate -SetValues @("mode=kubernetes", "memoryPolicyGateway.enabled=true", "networkPolicy.enabled=true")
Assert-RegexMatch $memoryControllerPolicyOutput '(?s)kind: NetworkPolicy\s+metadata:\s+name: test-release-kognition-k8s-controller(?:(?!\r?\n---)[\s\S])*?podSelector:\s+matchLabels:\s+app\.kubernetes\.io/component: memory-service\s+app\.kubernetes\.io/instance: test-release(?:(?!\r?\n---)[\s\S])*?port: 8090' "Kubernetes controller reaches only the release memory service for memory lifecycle operations"

$toolMonitoringOutput = Invoke-HelmTemplate -SetValues @("mode=kubernetes", "toolGateway.enabled=true", "toolGateway.monitoring.serviceMonitor.enabled=true", "toolGateway.monitoring.prometheusRule.enabled=true", "toolGateway.monitoring.grafanaDashboard.enabled=true")
Assert-Contains $toolMonitoringOutput "kind: ServiceMonitor" "Tool Gateway renders opt-in ServiceMonitor"
Assert-Contains $toolMonitoringOutput "path: /metrics" "Tool Gateway ServiceMonitor scrapes metrics endpoint"
Assert-RegexMatch $toolMonitoringOutput '(?s)kind: Service\s+metadata:\s+name: test-release-kognition-tool-gateway\s+namespace: default\s+labels:.*?app\.kubernetes\.io/instance: test-release' "Tool Gateway Service satisfies ServiceMonitor instance selector"
Assert-Contains $toolMonitoringOutput "kind: PrometheusRule" "Tool Gateway renders opt-in alert rules"
Assert-Contains $toolMonitoringOutput "KognitionToolGatewayCircuitOpen" "Tool Gateway alert rules include circuit alert"
Assert-Contains $toolMonitoringOutput "KognitionToolGatewayAuditCompletionFailed" "Tool Gateway alert rules include audit completion alert"
Assert-Contains $toolMonitoringOutput '"title": "Tool Gateway Operations"' "Tool Gateway renders the scoped operations dashboard"
Assert-Contains $toolMonitoringOutput '"refresh": "5s"' "Tool Gateway dashboard refreshes every five seconds"
Assert-Contains $toolMonitoringOutput '"time": {"from": "now-15m", "to": "now"}' "Tool Gateway dashboard defaults to the latest 15 minutes"
Assert-Contains $toolMonitoringOutput '"title": "Invocations (15m)"' "Tool Gateway dashboard includes invocation total"
Assert-Contains $toolMonitoringOutput '"title": "Invocation Outcome Mix"' "Tool Gateway dashboard includes outcome mix"
Assert-Contains $toolMonitoringOutput '"title": "P95 Invocation Latency"' "Tool Gateway dashboard includes p95 latency"
Assert-Contains $toolMonitoringOutput '"title": "Resilience Events (15m)"' "Tool Gateway dashboard includes resilience total"
Assert-Contains $toolMonitoringOutput '"title": "Resilience Event Mix"' "Tool Gateway dashboard includes resilience mix"
Assert-Contains $toolMonitoringOutput 'sum by (outcome) (rate(kognition_tool_gateway_invocations_total[5m]))' "Tool Gateway outcome panel uses its low-cardinality outcome label"
Assert-Contains $toolMonitoringOutput 'histogram_quantile(0.95, sum by (le) (rate(kognition_tool_gateway_invocation_duration_seconds_bucket[5m])))' "Tool Gateway latency panel aggregates only histogram buckets"
Assert-Contains $toolMonitoringOutput 'sum by (reason) (rate(kognition_tool_gateway_resilience_events_total[5m]))' "Tool Gateway resilience panel uses its low-cardinality reason label"
Assert-RegexNotMatch $toolMonitoringOutput 'sum by \((agent|identity|invocation|session|tool|trace)' "Tool Gateway dashboard adds no high-cardinality grouping"

$kognitionMonitoringOutput = Invoke-HelmTemplate -SetValues @(
    "mode=kubernetes",
    "modelGateway.enabled=true",
    "observability.metrics.serviceMonitor.enabled=true",
    "observability.metrics.serviceMonitor.labels.release=kognition-observability",
    "observability.grafanaDashboards.enabled=true"
)
Assert-RegexMatch $kognitionMonitoringOutput '(?s)kind: Service\s+metadata:\s+name: test-release-kognition-k8s-controller-metrics\s+namespace: default\s+labels:(?:(?!\r?\nspec:)[\s\S])*?app\.kubernetes\.io/component: k8s-controller(?:(?!\r?\nspec:)[\s\S])*?app\.kubernetes\.io/instance: test-release\s+spec:(?:(?!\r?\n---)[\s\S])*?name: metrics\s+port: 8080\s+targetPort: metrics' "Controller metrics Service metadata satisfies the ServiceMonitor selector and exposes its named metrics port"
Assert-RegexMatch $kognitionMonitoringOutput '(?s)kind: ServiceMonitor\s+metadata:\s+name: test-release-kognition-k8s-controller(?:(?!\r?\n---)[\s\S])*?release: kognition-observability(?:(?!\r?\n---)[\s\S])*?app\.kubernetes\.io/component: k8s-controller(?:(?!\r?\n---)[\s\S])*?port: metrics\s+path: /metrics' "Controller ServiceMonitor is discoverable and scrapes the metrics endpoint"
Assert-RegexMatch $kognitionMonitoringOutput '(?s)kind: ServiceMonitor\s+metadata:\s+name: test-release-kognition-model-gateway(?:(?!\r?\n---)[\s\S])*?release: kognition-observability(?:(?!\r?\n---)[\s\S])*?app\.kubernetes\.io/component: model-gateway(?:(?!\r?\n---)[\s\S])*?port: http\s+path: /metrics' "Model Gateway ServiceMonitor is discoverable and scrapes the shared HTTP endpoint"

foreach ($component in @('k8s-controller', 'model-gateway', 'memory-policy-gateway')) {
    Assert-RegexMatch $kognitionMonitoringOutput "(?s)kind: ServiceMonitor\s+metadata:\s+name: test-release-kognition-$component\s(?:(?!\r?\n---)[\s\S])*?honorLabels: true" "$component preserves the workspace namespace on application metrics"
}
$standaloneMonitoringOutput = Invoke-HelmTemplate -SetValues @('mode=standalone', 'observability.metrics.serviceMonitor.enabled=true')
foreach ($component in @('controller-manager', 'apiserver')) {
    Assert-RegexMatch $standaloneMonitoringOutput "(?s)kind: ServiceMonitor\s+metadata:\s+name: test-release-kognition-$component\s(?:(?!\r?\n---)[\s\S])*?honorLabels: true" "$component preserves the workspace namespace on application metrics"
}

$agentsDashboard = Get-EmbeddedDashboard -Output $kognitionMonitoringOutput -DataKey "kognition-agents-360.json"
$workflowDashboard = Get-EmbeddedDashboard -Output $kognitionMonitoringOutput -DataKey "kognition-workflow-360.json"
$agentsPanelTitles = @($agentsDashboard.panels.title) -join "`n"
$agentsVariables = @($agentsDashboard.templating.list.name) -join "`n"
$agentsQueries = @($agentsDashboard.panels | ForEach-Object { @($_.targets) | ForEach-Object { $_.expr } }) -join "`n"
$agentsEconomicPanels = @($agentsDashboard.panels | Where-Object { @($_.targets).expr -match 'kognition_model_usage_' })
$agentsEconomicQueries = @($agentsEconomicPanels | ForEach-Object { @($_.targets) | ForEach-Object { $_.expr } }) -join "`n"
$agentsEconomicText = @($agentsEconomicPanels | ForEach-Object { "$($_.title)`n$($_.description)" }) -join "`n"
$agentsGovernancePanels = @($agentsDashboard.panels | Where-Object { @($_.targets).expr -match 'kognition_trust_decisions_total' })
$agentsGovernanceQueries = @($agentsGovernancePanels | ForEach-Object { @($_.targets) | ForEach-Object { $_.expr } }) -join "`n"
$agentsGovernanceText = @($agentsGovernancePanels | ForEach-Object { "$($_.title)`n$($_.description)" }) -join "`n"
$workflowPanelTitles = @($workflowDashboard.panels.title) -join "`n"
$workflowVariables = @($workflowDashboard.templating.list.name) -join "`n"
$workflowQueries = @($workflowDashboard.panels | ForEach-Object { @($_.targets) | ForEach-Object { $_.expr } }) -join "`n"
$workflowEconomicPanels = @($workflowDashboard.panels | Where-Object { @($_.targets).expr -match 'kognition_model_usage_' })
$workflowEconomicQueries = @($workflowEconomicPanels | ForEach-Object { @($_.targets) | ForEach-Object { $_.expr } }) -join "`n"
$workflowEconomicText = @($workflowEconomicPanels | ForEach-Object { "$($_.title)`n$($_.description)" }) -join "`n"
$workflowGovernancePanels = @($workflowDashboard.panels | Where-Object { @($_.targets).expr -match 'kognition_trust_decisions_total' })
$workflowGovernanceQueries = @($workflowGovernancePanels | ForEach-Object { @($_.targets) | ForEach-Object { $_.expr } }) -join "`n"
$workflowGovernanceText = @($workflowDashboard.panels | Where-Object { $_.title -eq 'Workspace Governance & Trust' -or @($_.targets).expr -match 'kognition_trust_decisions_total' } | ForEach-Object { "$($_.title)`n$($_.description)" }) -join "`n"
$forbiddenEconomicIdentityPattern = '(run_id|session_id|trace_id|span_id|invocation_id|grant(_id|_ref)?|profile(_id)?|uid|revision|resource_version|agent_instance|agent_job)'
$forbiddenGovernanceIdentityPattern = '(agent|workflow|step|participant|run_id|session_id|trace_id|span_id|invocation_id|grant_id|grant_ref|policy_ref|profile_id|uid|revision|resource_version|agent_instance|agent_job)\s*(=|=~|!=|!~|,|\))'

Assert-Contains $agentsDashboard.uid "kognition-agents-360" "Agents 360 dashboard has a stable UID"
Assert-Contains $agentsDashboard.title "Kognition / Agents 360" "Agents 360 dashboard has the expected title"
Assert-Contains $agentsVariables "namespace" "Agents 360 provides an agent namespace selector"
Assert-Contains $agentsVariables "agent" "Agents 360 provides an agent selector"
Assert-Contains $agentsVariables "instance" "Agents 360 provides an instance selector"
Assert-Contains $agentsVariables "purpose" "Agents 360 provides an economic purpose selector"
Assert-Contains $agentsVariables "usage_state" "Agents 360 provides an explicit usage-evidence selector"
Assert-Contains $agentsVariables "pricing_state" "Agents 360 provides an explicit pricing-state selector"
Assert-Contains $agentsVariables "cost_tier" "Agents 360 provides a bounded cost-tier selector"
Assert-Contains $agentsPanelTitles "Instance Phase" "Agents 360 exposes runtime lifecycle state"
$instancePhasePanel = $agentsDashboard.panels | Where-Object { $_.title -eq "Instance Phase" }
Assert-Contains $instancePhasePanel.type "table" "Agents 360 renders current instance phase without requiring a time field"
Assert-Contains ([string]$instancePhasePanel.targets[0].instant) "True" "Agents 360 requests current instance phase as an instant query"
Assert-Contains $instancePhasePanel.targets[0].format "table" "Agents 360 requests current instance phase in table format"
Assert-Contains $agentsPanelTitles "Network Throughput by Agent" "Agents 360 exposes network throughput"
Assert-Contains $agentsPanelTitles "Delegation Outcomes (Platform-wide)" "Agents 360 labels shared delegation telemetry honestly"
Assert-Contains $agentsPanelTitles "Tool Outcomes (Platform-wide)" "Agents 360 labels shared tool telemetry honestly"
Assert-Contains $agentsPanelTitles "Model Economics" "Agents 360 separates durable model economics"
Assert-Contains $agentsPanelTitles "Average Provider Latency" "Agents 360 derives provider latency from durable duration and operation counters"
Assert-Contains $agentsPanelTitles "Operations by Cost Tier" "Agents 360 exposes bounded cost-tier volume"
Assert-Contains $agentsQueries "kube_pod_status_ready" "Agents 360 derives readiness from Kubernetes state metrics"
Assert-Contains $agentsQueries "label_kognition_dev_agent_instance" "Agents 360 joins runtime signals through stable instance labels"
Assert-Contains $agentsQueries "kognition_agent_communication_gateway_requests_total" "Agents 360 includes shared delegation outcomes"
Assert-Contains $agentsQueries "kognition_tool_gateway_invocations_total" "Agents 360 includes shared tool outcomes"
Assert-RegexNotMatch $agentsQueries '(run_id|session_id|trace_id|identity_id)' "Agents 360 avoids high-cardinality identity dimensions"
Assert-Contains $agentsEconomicQueries "kognition_model_usage_operations_total" "Agents 360 uses the durable model-operation counter"
Assert-Contains $agentsEconomicQueries "kognition_model_usage_tokens_total" "Agents 360 uses the durable model-token counter"
Assert-Contains $agentsEconomicQueries "kognition_model_usage_cost_units_total" "Agents 360 uses the durable model-cost counter"
Assert-Contains $agentsEconomicQueries "kognition_model_usage_provider_duration_milliseconds_total" "Agents 360 uses the durable provider-duration counter"
Assert-Contains $agentsEconomicText "reported" "Agents 360 explains reported usage evidence"
Assert-Contains $agentsEconomicText "estimated" "Agents 360 keeps estimated usage explicit"
Assert-Contains $agentsEconomicText "not_reported" "Agents 360 keeps missing usage evidence explicit"
Assert-Contains $agentsEconomicText "priced" "Agents 360 explains priced operations"
Assert-Contains $agentsEconomicText "unpriced" "Agents 360 keeps missing pricing explicit"
Assert-Contains $agentsEconomicText "attributed" "Agents 360 explains attributed operations"
Assert-Contains $agentsEconomicText "unattributed" "Agents 360 keeps missing attribution explicit"
Assert-Contains $agentsEconomicQueries "purpose" "Agents 360 scopes economics by bounded purpose"
Assert-RegexNotMatch $agentsEconomicQueries $forbiddenEconomicIdentityPattern "Agents 360 economics avoids execution identity dimensions"
Assert-Contains $agentsPanelTitles "Governance & Trust" "Agents 360 separates durable governance evidence"
Assert-Contains $agentsPanelTitles "Decisions by Plane and Outcome" "Agents 360 exposes bounded governance outcomes"
Assert-Contains $agentsPanelTitles "Authorization Evidence" "Agents 360 exposes authorization evidence states"
Assert-Contains $agentsPanelTitles "Approval State" "Agents 360 exposes approval evidence states"
Assert-Contains $agentsPanelTitles "Execution State" "Agents 360 exposes execution evidence states"
Assert-Contains $agentsPanelTitles "Target-Reached Evidence" "Agents 360 exposes explicit target evidence"
Assert-Contains $agentsGovernanceQueries "kognition_trust_decisions_total" "Agents 360 governance uses the durable TrustDecision counter"
Assert-Contains $agentsGovernanceQueries "plane, outcome" "Agents 360 governance groups by bounded plane and outcome"
Assert-Contains $agentsGovernanceQueries "reason_code" "Agents 360 governance exposes bounded reason codes"
Assert-Contains $agentsGovernanceQueries "identity_state" "Agents 360 governance exposes bounded identity evidence"
Assert-Contains $agentsGovernanceQueries "role_state" "Agents 360 governance exposes bounded role evidence"
Assert-Contains $agentsGovernanceQueries "grant_state" "Agents 360 governance exposes bounded grant evidence"
Assert-Contains $agentsGovernanceQueries "policy_state" "Agents 360 governance exposes bounded policy evidence"
Assert-Contains $agentsGovernanceQueries "approval_state" "Agents 360 governance exposes bounded approval evidence"
Assert-Contains $agentsGovernanceQueries "execution_state" "Agents 360 governance exposes bounded execution evidence"
Assert-Contains $agentsGovernanceQueries "target_reached_state" "Agents 360 governance exposes bounded target evidence"
Assert-Contains $agentsGovernanceText "not_evaluated" "Agents 360 keeps unevaluated authorization evidence explicit"
Assert-Contains $agentsGovernanceText "not_reported" "Agents 360 keeps missing governance evidence explicit"
Assert-Contains $agentsGovernanceText "unavailable" "Agents 360 keeps unavailable governance evidence explicit"
Assert-Contains $agentsGovernanceText "unknown" "Agents 360 keeps invalid bounded governance evidence explicit"
Assert-RegexNotMatch $agentsGovernanceQueries $forbiddenGovernanceIdentityPattern "Agents 360 governance avoids execution and resource identity dimensions"

Assert-Contains $workflowDashboard.uid "kognition-workflow-360" "Workflow 360 dashboard has a stable UID"
Assert-Contains $workflowDashboard.title "Kognition / Workflow 360" "Workflow 360 dashboard has the expected title"
Assert-Contains $workflowVariables "namespace" "Workflow 360 provides a namespace selector"
Assert-Contains $workflowVariables "workflow" "Workflow 360 provides a workflow selector"
Assert-Contains $workflowVariables "step" "Workflow 360 provides a step selector"
Assert-Contains $workflowVariables "phase" "Workflow 360 provides a step phase selector"
Assert-Contains $workflowVariables "participant" "Workflow 360 provides a stable Agent participant selector"
Assert-Contains $workflowVariables "purpose" "Workflow 360 provides an economic purpose selector"
Assert-Contains $workflowVariables "usage_state" "Workflow 360 provides an explicit usage-evidence selector"
Assert-Contains $workflowVariables "pricing_state" "Workflow 360 provides an explicit pricing-state selector"
Assert-Contains $workflowPanelTitles "Active Runs" "Workflow 360 exposes active run volume"
Assert-Contains $workflowPanelTitles "Terminal Outcomes" "Workflow 360 exposes terminal outcome mix"
Assert-Contains $workflowPanelTitles "Mean Retained Step Durations" "Workflow 360 exposes retained mean step duration"
Assert-Contains $workflowPanelTitles "Model Economics" "Workflow 360 separates durable model economics"
Assert-Contains $workflowPanelTitles "Average Provider Latency" "Workflow 360 derives provider latency from durable duration and operation counters"
Assert-Contains $workflowPanelTitles "Workflow Participants by Agent" "Workflow 360 exposes stable participant attribution"
Assert-Contains $workflowPanelTitles "Workflow Usage Evidence State" "Workflow 360 exposes explicit usage evidence states"
Assert-Contains $workflowPanelTitles "Workflow Pricing State" "Workflow 360 exposes explicit pricing states"
Assert-Contains $workflowPanelTitles "Workflow Attribution State" "Workflow 360 exposes explicit attribution states"
Assert-Contains $workflowPanelTitles "Operations by Purpose" "Workflow 360 exposes bounded purpose volume"
Assert-Contains $workflowQueries "kognition_workflow_active_runs" "Workflow 360 queries the active-runs gauge"
Assert-Contains $workflowQueries "kognition_workflow_retained_runs" "Workflow 360 queries retained terminal gauges"
Assert-Contains $workflowQueries "kognition_workflow_retained_step_duration_observations" "Workflow 360 computes means from retained observations"
Assert-RegexNotMatch $workflowQueries 'kognition_workflow_runs_total|kognition_workflow_step_duration_seconds_' "Workflow 360 does not query removed event metrics"
Assert-RegexNotMatch $workflowQueries '(rate|increase)\(kognition_workflow_retained_' "Workflow 360 never treats retained gauges as counters"
Assert-Contains $workflowQueries 'max by (namespace, workflow, step, phase)' "Workflow 360 deduplicates replica projections"
Assert-RegexNotMatch $workflowQueries '(run_id|session_id|trace_id|identity_id)' "Workflow 360 avoids high-cardinality run identity dimensions"
Assert-Contains $workflowEconomicQueries "kognition_model_usage_operations_total" "Workflow 360 uses the durable model-operation counter"
Assert-Contains $workflowEconomicQueries "kognition_model_usage_tokens_total" "Workflow 360 uses the durable model-token counter"
Assert-Contains $workflowEconomicQueries "kognition_model_usage_cost_units_total" "Workflow 360 uses the durable model-cost counter"
Assert-Contains $workflowEconomicQueries "kognition_model_usage_provider_duration_milliseconds_total" "Workflow 360 uses the durable provider-duration counter"
Assert-Contains $workflowEconomicText "reported" "Workflow 360 explains reported usage evidence"
Assert-Contains $workflowEconomicText "estimated" "Workflow 360 keeps estimated usage explicit"
Assert-Contains $workflowEconomicText "not_reported" "Workflow 360 keeps missing usage evidence explicit"
Assert-Contains $workflowEconomicText "priced" "Workflow 360 explains priced operations"
Assert-Contains $workflowEconomicText "unpriced" "Workflow 360 keeps missing pricing explicit"
Assert-Contains $workflowEconomicText "attributed" "Workflow 360 explains attributed operations"
Assert-Contains $workflowEconomicText "unattributed" "Workflow 360 keeps missing attribution explicit"
Assert-Contains $workflowEconomicText "reasoning-probe" "Workflow 360 documents reasoning-probe purpose coverage"
Assert-RegexNotMatch $workflowEconomicQueries $forbiddenEconomicIdentityPattern "Workflow 360 economics avoids execution identity dimensions"
Assert-Contains $workflowPanelTitles "Workspace Governance & Trust" "Workflow 360 labels TrustDecision evidence as workspace-scoped"
Assert-Contains $workflowPanelTitles "Workspace Decisions by Plane and Outcome" "Workflow 360 exposes bounded workspace governance outcomes"
Assert-Contains $workflowPanelTitles "Workspace Authorization Evidence" "Workflow 360 exposes workspace authorization evidence states"
Assert-Contains $workflowPanelTitles "Workspace Approval State" "Workflow 360 exposes workspace approval evidence states"
Assert-Contains $workflowPanelTitles "Workspace Execution State" "Workflow 360 exposes workspace execution evidence states"
Assert-Contains $workflowPanelTitles "Workspace Target-Reached Evidence" "Workflow 360 exposes explicit workspace target evidence"
Assert-Contains $workflowGovernanceQueries "kognition_trust_decisions_total" "Workflow 360 governance uses the durable TrustDecision counter"
Assert-Contains $workflowGovernanceQueries "plane, outcome" "Workflow 360 governance groups by bounded plane and outcome"
Assert-Contains $workflowGovernanceQueries "reason_code" "Workflow 360 governance exposes bounded reason codes"
Assert-Contains $workflowGovernanceQueries "identity_state" "Workflow 360 governance exposes bounded identity evidence"
Assert-Contains $workflowGovernanceQueries "role_state" "Workflow 360 governance exposes bounded role evidence"
Assert-Contains $workflowGovernanceQueries "grant_state" "Workflow 360 governance exposes bounded grant evidence"
Assert-Contains $workflowGovernanceQueries "policy_state" "Workflow 360 governance exposes bounded policy evidence"
Assert-Contains $workflowGovernanceQueries "approval_state" "Workflow 360 governance exposes bounded approval evidence"
Assert-Contains $workflowGovernanceQueries "execution_state" "Workflow 360 governance exposes bounded execution evidence"
Assert-Contains $workflowGovernanceQueries "target_reached_state" "Workflow 360 governance exposes bounded target evidence"
Assert-Contains $workflowGovernanceText "not_evaluated" "Workflow 360 keeps unevaluated workspace authorization evidence explicit"
Assert-Contains $workflowGovernanceText "not_reported" "Workflow 360 keeps missing workspace governance evidence explicit"
Assert-Contains $workflowGovernanceText "unavailable" "Workflow 360 keeps unavailable workspace governance evidence explicit"
Assert-Contains $workflowGovernanceText "unknown" "Workflow 360 keeps invalid bounded workspace governance evidence explicit"
Assert-Contains $workflowGovernanceText "not attributed to workflow runs, steps, participants, identities, or resource references" "Workflow 360 discloses the governance attribution boundary"
Assert-RegexNotMatch $workflowGovernanceQueries $forbiddenGovernanceIdentityPattern "Workflow 360 governance avoids workflow, execution, and resource identity dimensions"

$defaultObservabilityOutput = Invoke-HelmTemplate -SetValues @("mode=kubernetes")
Assert-NotContains $defaultObservabilityOutput "kognition-agents-360" "Agents 360 remains opt-in by default"
Assert-NotContains $defaultObservabilityOutput "kognition-workflow-360" "Workflow 360 remains opt-in by default"
$standaloneDashboardsOutput = Invoke-HelmTemplate -SetValues @("mode=standalone", "observability.grafanaDashboards.enabled=true", "observability.metrics.serviceMonitor.enabled=true")
Assert-NotContains $standaloneDashboardsOutput "kognition-agents-360" "Agents 360 does not render in standalone mode"
Assert-NotContains $standaloneDashboardsOutput "kognition-workflow-360" "Workflow 360 does not render in standalone mode"
Assert-NotContains $standaloneDashboardsOutput "k8s-controller-metrics" "Controller metrics discovery does not render in standalone mode"
Assert-RegexMatch $standaloneDashboardsOutput '(?s)kind: Service\s+metadata:\s+name: test-release-kognition-controller-manager-metrics(?:(?!\r?\n---)[\s\S])*?app\.kubernetes\.io/component: controller-manager(?:(?!\r?\n---)[\s\S])*?name: metrics\s+port: 8080\s+targetPort: metrics' "Standalone controller-manager metrics Service satisfies its ServiceMonitor selector"
Assert-RegexMatch $standaloneDashboardsOutput '(?s)kind: ServiceMonitor\s+metadata:\s+name: test-release-kognition-controller-manager(?:(?!\r?\n---)[\s\S])*?app\.kubernetes\.io/component: controller-manager(?:(?!\r?\n---)[\s\S])*?port: metrics\s+path: /metrics' "Standalone controller-manager ServiceMonitor scrapes its metrics endpoint"
Assert-RegexMatch $standaloneDashboardsOutput '(?s)kind: ServiceMonitor\s+metadata:\s+name: test-release-kognition-apiserver(?:(?!\r?\n---)[\s\S])*?app\.kubernetes\.io/component: apiserver(?:(?!\r?\n---)[\s\S])*?port: metrics\s+path: /metrics' "Standalone API server ServiceMonitor scrapes the private metrics port"
Assert-RegexMatch $standaloneDashboardsOutput '(?s)kind: Service\s+metadata:\s+name: test-release-kognition-apiserver-metrics(?:(?!\r?\n---)[\s\S])*?name: metrics\s+port: 9090\s+targetPort: metrics' "Standalone API metrics has a dedicated internal Service"
Assert-RegexMatch $standaloneDashboardsOutput 'name: KOGNITION_METRICS_ADDR\s+value: ":9090"' "Standalone metrics listener binds the pod interface when monitoring is enabled"
$privateMetricsOutput = Invoke-HelmTemplate -SetValues @("mode=standalone", "networkPolicy.enabled=true", "observability.metrics.serviceMonitor.enabled=true")
Assert-RegexMatch $privateMetricsOutput '(?s)kind: NetworkPolicy\s+metadata:\s+name: test-release-kognition-apiserver(?:(?!\r?\n---)[\s\S])*?app.kubernetes.io/name: prometheus\s+ports:\s+- port: 9090' "Standalone API metrics ingress is restricted to Prometheus pods"

$standaloneTLSMonitoringOutput = Invoke-HelmTemplate -SetValues @("mode=standalone", "apiserver.tls.enabled=true", "apiserver.tls.secretName=apiserver-tls", "observability.metrics.serviceMonitor.enabled=true")
Assert-RegexMatch $standaloneTLSMonitoringOutput '(?s)kind: ServiceMonitor\s+metadata:\s+name: test-release-kognition-apiserver(?:(?!\r?\n---)[\s\S])*?port: metrics\s+path: /metrics' "API TLS does not change the private metrics port"
Assert-RegexNotMatch $standaloneTLSMonitoringOutput '(?s)kind: ServiceMonitor\s+metadata:\s+name: test-release-kognition-apiserver(?:(?!\r?\n---)[\s\S])*?scheme: https' "Private metrics does not inherit public API TLS configuration"

$standaloneIngesterOutput = Invoke-HelmTemplate -SetValues @(
    "mode=standalone",
    "kbIngester.enabled=true",
    "kbPgvector.enabled=true"
)
Assert-Contains $standaloneIngesterOutput "name: KOGNITION_KB_INGESTER_URL" "Standalone configures the HTTP ingester launcher"
Assert-Contains $standaloneIngesterOutput "name: KOGNITION_KB_PGVECTOR_DSN" "Standalone injects the pgvector DSN"
Assert-RegexMatch $standaloneIngesterOutput "(?s)name: KOGNITION_KB_PGVECTOR_DSN.*?secretKeyRef:.*?key: connectionString" "Standalone sources the pgvector DSN from Secret"
Assert-Contains $standaloneIngesterOutput "name: kb-ingester" "Standalone renders the ingester sidecar"
Assert-Contains $standaloneIngesterOutput "value: 127.0.0.1:8082" "Standalone ingester binds to pod-local loopback"

$standaloneOTelOutput = Invoke-HelmTemplate -SetValues @(
    "mode=standalone",
    "observability.openTelemetry.enabled=true",
    "observability.openTelemetry.endpoint=http://otel-collector:4318",
    "observability.openTelemetry.protocol=http/protobuf",
    "observability.openTelemetry.sampleRate=0.5"
)
Assert-Contains $standaloneOTelOutput "name: KOGNITION_OTEL_TRACES_ENABLED" "Standalone renders OpenTelemetry enablement"
Assert-Contains $standaloneOTelOutput 'value: "http://otel-collector:4318"' "Standalone renders OpenTelemetry endpoint"
Assert-Contains $standaloneOTelOutput "name: KOGNITION_OTEL_TRACES_PROTOCOL" "Standalone renders OpenTelemetry protocol"
Assert-Contains $standaloneOTelOutput 'value: "0.5"' "Standalone renders OpenTelemetry sample rate"

# ============================================================
# TEST GROUP 2: Mode Exclusivity - Kubernetes
# ============================================================
Write-Host "[Group 2] Mode Exclusivity - Kubernetes mode" -ForegroundColor Yellow
$output = Invoke-HelmTemplate -SetValues @("mode=kubernetes")

Assert-RegexMatch $output "name: test-release-kognition-k8s-controller" "Kubernetes has k8s-controller deployment"
Assert-RegexMatch $output "name: test-release-kognition-intent-gateway" "Kubernetes has IntentGateway deployment"
Assert-Contains $output "image: kognition-intent-gateway:" "Kubernetes uses locally built IntentGateway image"
Assert-Contains $output "name: KOGNITION_INTENT_GATEWAY_ENDPOINT" "Kubernetes controller receives IntentGateway endpoint"
Assert-Contains $output "name: grpc" "Kubernetes renders IntentGateway gRPC port"
Assert-Contains $output "name: KOGNITION_GRPC_ADDR" "Kubernetes configures IntentGateway gRPC listener"
Assert-Contains $output "name: KOGNITION_CONTROLLER_NAMESPACE" "Kubernetes controller receives its control namespace"
Assert-RegexMatch $output '(?s)name: test-release-kognition-k8s-controller(?:(?!\r?\n---)[\s\S])*?name: KOGNITION_CANARY_OPERATION_RETENTION\s+value: "720h"' "Kubernetes controller uses the default canary retention"
Assert-RegexMatch $output '(?s)name: test-release-kognition-k8s-controller(?:(?!\r?\n---)[\s\S])*?name: KOGNITION_RUNTIME_APPROVAL_TOMBSTONE_RETENTION\s+value: "2160h"' "Kubernetes controller uses the minimum RuntimeApproval tombstone retention"
Assert-Contains $output "name: KOGNITION_MEMORY_SERVICE_URL" "Kubernetes controller configures memory backend provisioning"
Assert-Contains $output "kind: ClusterRole" "Kubernetes has ClusterRole"
Assert-Contains $output "kind: ClusterRoleBinding" "Kubernetes has ClusterRoleBinding"
Assert-RegexMatch $output '(?s)kind: ClusterRole.*?name: test-release-kognition-intent-gateway.*?resources: \["intentgateways", "workflows", "agentservices", "secretrefs"\].*?verbs: \["get"\]' "IntentGateway can read AgentService targets"
Assert-RegexMatch $output '(?s)kind: Role\s+metadata:\s+name: test-release-kognition-quota-ledger(?:(?!\r?\n---)[\s\S])*?resources:\s*\n\s*- configmaps\s*\n\s*verbs: \["get", "list", "create", "update"\]' "Kubernetes quota ledger role declares only the bounded ConfigMap verbs"
Assert-Contains $output "kind: ValidatingWebhookConfiguration" "Kubernetes has ValidatingWebhookConfiguration"
Assert-Contains $output "kind: MutatingWebhookConfiguration" "Kubernetes has MutatingWebhookConfiguration"
Assert-RegexMatch $output "(?s)name: validate\.kognition\.dev.*?- knowledgebases.*?- knowledgebaseindexruns.*?- knowledgebaseindexschedules.*?scope: Namespaced" "Validating webhook covers namespaced Knowledge Base resources"
Assert-RegexMatch $output "(?s)name: validate\.kognition\.dev.*?- knowledgebaseclasses.*?scope: Cluster" "Validating webhook covers cluster-scoped KnowledgeBaseClass"
Assert-RegexMatch $output "(?s)name: validate\.kognition\.dev.*?- trustprofiles.*?- runtimeapprovals.*?- runtimeapprovals/status.*?scope: Namespaced" "Validating webhook covers TrustProfile and RuntimeApproval base/status resources"
Assert-RegexMatch $output "(?s)name: mutate\.kognition\.dev.*?- toolclasses.*?scope: Cluster" "Mutating webhook treats ToolClass as cluster-scoped"
Assert-RegexMatch $output '(?s)resources:\s*\["runtimeapprovals"\]\s*verbs:\s*\["get", "list", "watch", "create", "update", "patch", "delete"\]' "RuntimeApproval base RBAC supports finalizer compaction"
Assert-RegexMatch $output '(?s)resources:\s*\["runtimeapprovals/status"\]\s*verbs:\s*\["get", "update"\]' "RuntimeApproval status RBAC grants dedicated update without patch/delete"
Assert-RegexMatch $output '(?s)resources:\s*\["runtimeapprovaltombstones"\]\s*verbs:\s*\["get", "list", "watch", "create", "update", "patch", "delete"\]' "RuntimeApproval tombstone RBAC supports compaction and retention"
Assert-Contains $output "kind: Issuer" "Kubernetes has cert-manager Issuer"
Assert-Contains $output "kind: Certificate" "Kubernetes has cert-manager Certificate"
Assert-Contains $output "kind: ServiceAccount" "Kubernetes has ServiceAccount"
Assert-Contains $output "- autonomyreviews/status" "Kubernetes validates AutonomyReview status decisions"
$kubernetesRetentionOutput = Invoke-HelmTemplate -SetValues @("mode=kubernetes", "canaryOperationRetention=0")
Assert-RegexMatch $kubernetesRetentionOutput '(?s)name: test-release-kognition-k8s-controller(?:(?!\r?\n---)[\s\S])*?name: KOGNITION_CANARY_OPERATION_RETENTION\s+value: "0"' "Kubernetes controller can disable canary retention"
$runtimeApprovalRetentionOutput = Invoke-HelmTemplate -SetValues @("mode=kubernetes", "runtimeApprovalTombstoneRetention=4320h")
Assert-RegexMatch $runtimeApprovalRetentionOutput '(?s)name: test-release-kognition-k8s-controller(?:(?!\r?\n---)[\s\S])*?name: KOGNITION_RUNTIME_APPROVAL_TOMBSTONE_RETENTION\s+value: "4320h"' "Kubernetes controller accepts a longer RuntimeApproval tombstone retention"
$autonomyReviewCRD = Get-Content (Join-Path $ChartPath "crds/kognition.dev_autonomyreviews.yaml") -Raw
Assert-Contains $autonomyReviewCRD "AutonomyReview spec is immutable after creation" "AutonomyReview CRD protects immutable review evidence"

$runtimeOutput = Invoke-HelmTemplate -SetValues @(
    "mode=kubernetes",
	"k8sController.agentNamespace=agent-runtimes",
    "k8sController.runtimeImages.autogen=registry.example/autogen:v1",
    "k8sController.runtimeControlTLS.workerSecretName=operator-worker-tls",
    "k8sController.runtimeControlTLS.existingCASecret=operator-worker-ca",
    "k8sController.runtimeImages.mcp=registry.example/mcp:v1"
)
Assert-Contains $runtimeOutput "name: KOGNITION_AGENT_NAMESPACE" "Kubernetes configures shared runtime namespace"
Assert-Contains $runtimeOutput 'value: "agent-runtimes"' "Kubernetes renders shared runtime namespace value"
Assert-Contains $runtimeOutput "name: KOGNITION_RUNTIME_AUTOGEN_IMAGE" "Kubernetes configures AutoGen runtime image"
Assert-Contains $runtimeOutput "name: KOGNITION_RUNTIME_TLS_SECRET_NAME" "AutoGen configures existing operator worker TLS Secret"
Assert-Contains $runtimeOutput "name: KOGNITION_RUNTIME_CA_FILE" "AutoGen controller uses explicit CA trust file"
Assert-Contains $runtimeOutput 'secretName: "operator-worker-ca"' "AutoGen controller mounts existing operator CA Secret"

$kubernetesOTelOutput = Invoke-HelmTemplate -SetValues @(
    "mode=kubernetes",
    "observability.openTelemetry.enabled=true",
    "observability.openTelemetry.endpoint=otel-collector.observability.svc:4317"
)
Assert-Contains $kubernetesOTelOutput "name: KOGNITION_OTEL_TRACES_ENABLED" "Kubernetes renders OpenTelemetry enablement"
Assert-Contains $kubernetesOTelOutput 'value: "otel-collector.observability.svc:4317"' "Kubernetes renders OpenTelemetry endpoint"
Assert-Contains $kubernetesOTelOutput "name: KOGNITION_OTEL_TRACES_SERVICE_NAME" "Kubernetes renders OpenTelemetry service name"
Assert-Contains $runtimeOutput 'value: "registry.example/autogen:v1"' "Kubernetes renders AutoGen runtime image value"
Assert-Contains $runtimeOutput "name: KOGNITION_RUNTIME_MCP_IMAGE" "Kubernetes configures MCP runtime image"
Assert-Contains $runtimeOutput 'value: "registry.example/mcp:v1"' "Kubernetes renders MCP runtime image value"

# Kubernetes must NOT have standalone control-plane resources
Assert-RegexMatch $output "name: test-release-kognition-postgres" "Kubernetes memory policy stack has PostgreSQL"
Assert-RegexNotMatch $output "name: test-release-kognition-apiserver" "Kubernetes has NO apiserver"
Assert-RegexNotMatch $output "name: test-release-kognition-controller-manager" "Kubernetes has NO controller-manager"
Assert-RegexMatch $output "name: test-release-kognition-migrations" "Kubernetes memory policy stack has migrations"

# ============================================================
# TEST GROUP 3: Bug #3 - Webhooks gated on mode=kubernetes
# ============================================================
Write-Host "[Group 3] Bug #3 - Webhooks only in kubernetes mode" -ForegroundColor Yellow
$standaloneOutput = Invoke-HelmTemplate -SetValues @("mode=standalone")
Assert-NotContains $standaloneOutput "kind: ValidatingWebhookConfiguration" "Bug3: No ValidatingWebhook in standalone"
Assert-NotContains $standaloneOutput "kind: MutatingWebhookConfiguration" "Bug3: No MutatingWebhook in standalone"
Assert-RegexNotMatch $standaloneOutput '(?s)kind: Service\s+metadata:\s+name: test-release-kognition-webhook-service(?:\s|$)' "Bug3: No webhook-service resource in standalone"

# ============================================================
# TEST GROUP 4: Bug #5 - init containers gated on postgres.enabled
# ============================================================
Write-Host "[Group 4] Bug #5 - Init containers gated on postgres.enabled" -ForegroundColor Yellow

$withPostgres = Invoke-HelmTemplate -SetValues @("mode=standalone", "postgres.enabled=true")
Assert-Contains $withPostgres "wait-for-postgres" "Bug5: wait-for-postgres present when postgres enabled"

$noPostgres = Invoke-HelmTemplate -SetValues @("mode=standalone", "postgres.enabled=false")
Assert-NotContains $noPostgres "wait-for-postgres" "Bug5: NO wait-for-postgres when postgres disabled"
Assert-NotContains $noPostgres "kind: StatefulSet" "Bug5: NO StatefulSet when postgres disabled"
# Migrations ConfigMap + Job still render when standalone uses external Postgres
# (postgres.enabled=false). The migrations Job runs against the external server.
Assert-Contains $noPostgres "kognition-migrations" "Bug5: migrations resources still rendered for external Postgres"

# ============================================================
# TEST GROUP 5: Bug #6 - Webhook port/volumes gated on webhook.enabled
# ============================================================
Write-Host "[Group 5] Bug #6 - Webhook port/volumes gated on webhook.enabled" -ForegroundColor Yellow

$withWebhook = Invoke-HelmTemplate -SetValues @("mode=kubernetes", "webhook.enabled=true")
Assert-Contains $withWebhook "containerPort: 9443" "Bug6: webhook port 9443 when webhook enabled"
Assert-Contains $withWebhook "webhook-tls" "Bug6: webhook-tls volume when webhook enabled"

$noWebhook = Invoke-HelmTemplate -SetValues @("mode=kubernetes", "webhook.enabled=false")
$optionalWebhookOutput = ($noWebhook -split '(?m)^---\s*$' | Where-Object { $_ -notmatch '# Source: kognition/templates/admission-guard.yaml' }) -join "`n"
Assert-NotContains $optionalWebhookOutput "containerPort: 9443" "Bug6: NO controller webhook port when webhook disabled"
Assert-NotContains $noWebhook "webhook-tls" "Bug6: NO webhook-tls volume when webhook disabled"
Assert-NotContains $optionalWebhookOutput "kind: ValidatingWebhookConfiguration" "Bug6: NO controller validating webhook when disabled"
Assert-Contains $noWebhook "kind: ValidatingWebhookConfiguration" "Independent admission guard remains enabled"
Assert-NotContains $noWebhook "kind: MutatingWebhookConfiguration" "Bug6: NO mutating webhook when disabled"

# ============================================================
# TEST GROUP 6: Bug #7 - Webhook service selector includes instance
# ============================================================
Write-Host "[Group 6] Bug #7 - Webhook service selector alignment" -ForegroundColor Yellow

$k8sOutput = Invoke-HelmTemplate -SetValues @("mode=kubernetes")

# Extract webhook-service section and check selector has instance
$webhookServiceSection = ($k8sOutput -split "---" | Where-Object { $_ -match "webhook-service" -and $_ -match "kind: Service" }) -join "`n"
Assert-Contains $webhookServiceSection "app.kubernetes.io/instance: test-release" "Bug7: webhook-service selector has instance label"
Assert-Contains $webhookServiceSection "app.kubernetes.io/component: resource-webhooks" "Bug7: webhook-service selects dedicated resource admission"
Assert-Contains $webhookServiceSection "app.kubernetes.io/name: kognition" "Bug7: webhook-service selector has name label"
$resourceWebhooksSection = ($k8sOutput -split "---" | Where-Object { $_ -match "resource-webhooks.yaml" -and $_ -match "kind: Deployment" }) -join "`n"
Assert-Contains $resourceWebhooksSection "app.kubernetes.io/instance: test-release" "Bug7: resource admission deployment has matching instance label"
Assert-Contains $resourceWebhooksSection "app.kubernetes.io/component: resource-webhooks" "Bug7: resource admission deployment has matching component label"
Assert-Contains $resourceWebhooksSection "app.kubernetes.io/name: kognition" "Bug7: resource admission deployment has matching name label"

# ============================================================
# TEST GROUP 7: Bug #1 - kognition.labels includes app.kubernetes.io/name
# ============================================================
Write-Host "[Group 7] Bug #1 - Labels include app.kubernetes.io/name" -ForegroundColor Yellow

$standaloneOutput = Invoke-HelmTemplate -SetValues @("mode=standalone")
$k8sOutput = Invoke-HelmTemplate -SetValues @("mode=kubernetes")

# All deployments in both modes should have app.kubernetes.io/name: kognition
Assert-Contains $standaloneOutput "app.kubernetes.io/name: kognition" "Bug1: standalone output has name label"
Assert-Contains $k8sOutput "app.kubernetes.io/name: kognition" "Bug1: kubernetes output has name label"

# ============================================================
# TEST GROUP 8: certManager.enabled=false
# ============================================================
Write-Host "[Group 8] certManager.enabled=false" -ForegroundColor Yellow

$noCertMgr = Invoke-HelmTemplate -SetValues @("mode=kubernetes", "webhook.certManager.enabled=false")
$optionalCertificateOutput = ($noCertMgr -split '(?m)^---\s*$' | Where-Object { $_ -notmatch '# Source: kognition/templates/admission-guard.yaml' }) -join "`n"
Assert-NotContains $optionalCertificateOutput "kind: Issuer" "No controller Issuer when its certManager integration is disabled"
Assert-NotContains $optionalCertificateOutput "kind: Certificate" "No controller Certificate when its certManager integration is disabled"
Assert-Contains $noCertMgr "kind: Certificate" "Independent admission guard retains its certificate"
# Webhook configs should still render
Assert-Contains $noCertMgr "kind: ValidatingWebhookConfiguration" "ValidatingWebhook still renders without certManager"
Assert-Contains $noCertMgr "kind: MutatingWebhookConfiguration" "MutatingWebhook still renders without certManager"

# ============================================================
# TEST GROUP 9: serviceAccount.create=false
# ============================================================
Write-Host "[Group 9] serviceAccount.create=false" -ForegroundColor Yellow

$noSA = Invoke-HelmTemplate -SetValues @("mode=standalone", "serviceAccount.create=false")
# Use regex to match standalone resource "kind: ServiceAccount" (not a subject reference)
Assert-RegexNotMatch $noSA "(?m)^kind: ServiceAccount" "No ServiceAccount resource when create=false (standalone)"

$noSAk8s = Invoke-HelmTemplate -SetValues @("mode=kubernetes", "serviceAccount.create=false")
Assert-RegexMatch $noSAk8s "(?s)kind: ServiceAccount.*?name: test-release-kognition-intent-gateway" "Gateway keeps its dedicated ServiceAccount when global ServiceAccount creation is disabled"

# ============================================================
# TEST GROUP 10: Bug #4 - NOTES.txt is mode-aware
# ============================================================
Write-Host "[Group 10] Bug #4 - NOTES.txt mode-aware" -ForegroundColor Yellow

# Helm versions differ in whether install dry-run is client-only. Inspect the
# template directly so this check never depends on a live cluster or release state.
$notesTemplate = Get-Content (Join-Path $ChartPath "templates/NOTES.txt") -Raw
Assert-Contains $notesTemplate 'if eq .Values.mode "standalone"' "Bug4: NOTES.txt branches on standalone mode"
Assert-RegexMatch $notesTemplate "(?i)Mode: Standalone" "Bug4: NOTES.txt labels standalone mode"
Assert-RegexMatch $notesTemplate "(?i)Mode: Kubernetes" "Bug4: NOTES.txt labels kubernetes mode"

# ============================================================
# TEST GROUP 11: Helm lint passes all combinations
# ============================================================
Write-Host "[Group 11] Helm lint - all edge cases" -ForegroundColor Yellow

$lintCases = @(
    @(),
    @("mode=kubernetes"),
    @("mode=standalone", "postgres.enabled=false"),
    @("mode=kubernetes", "webhook.enabled=false"),
    @("mode=standalone", "serviceAccount.create=false"),
    @("mode=kubernetes", "webhook.certManager.enabled=false")
)

foreach ($case in $lintCases) {
    $helmParameters = @("lint", "--strict", $ChartPath)
    foreach ($v in $case) {
        $helmParameters += "--set"
        $helmParameters += $v
    }
    $lintResult = & helm @helmParameters 2>&1
    $caseName = if ($case.Count -eq 0) { "defaults" } else { $case -join ", " }
    if ($LASTEXITCODE -eq 0) {
        $script:Passed++
    } else {
        $script:Failed++
        $script:Errors += "FAIL: Helm lint failed for: $caseName - $lintResult"
    }
}

# ============================================================
# TEST GROUP 12: global.imageRegistry composition + migrations Job gating
# ============================================================
Write-Host "[Group 12] global.imageRegistry composition + migrations Job gating" -ForegroundColor Yellow

$withRegistry = Invoke-HelmTemplate -SetValues @("global.imageRegistry=ghcr.io/test-owner")
Assert-Contains $withRegistry "image: ghcr.io/test-owner/kognition-apiserver:" "Reg: apiserver image prefixed by global.imageRegistry"
Assert-Contains $withRegistry "image: ghcr.io/test-owner/kognition-controller-manager:" "Reg: controller-manager image prefixed"
Assert-Contains $withRegistry "image: ghcr.io/test-owner/kognition-memory-service:" "Reg: memory-service image prefixed"
Assert-NotContains $withRegistry "ghcr.io/test-owner/pgvector/pgvector" "Reg: namespaced postgres image NOT re-prefixed"
Assert-Contains $withRegistry "docker.io/pgvector/pgvector@sha256:ccc6e83d6e35e931dc7c5def2022729d5a6c370318d099181995567ff1fb4d6b" "Reg: upstream postgres image remains digest pinned"

$withThirdPartyRegistry = Invoke-HelmTemplate -SetValues @("global.thirdPartyImageRegistry=trusted.azurecr.io")
Assert-Contains $withThirdPartyRegistry "trusted.azurecr.io/third-party/pgvector@sha256:ccc6e83d6e35e931dc7c5def2022729d5a6c370318d099181995567ff1fb4d6b" "Reg: postgres uses its verified private mirror"
Assert-Contains $withThirdPartyRegistry "trusted.azurecr.io/third-party/busybox@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662" "Reg: wait containers use their verified private mirror"

$withRegistryK8s = Invoke-HelmTemplate -SetValues @("mode=kubernetes", "global.imageRegistry=ghcr.io/test-owner")
Assert-Contains $withRegistryK8s "image: ghcr.io/test-owner/kognition-k8s-controller:" "Reg: k8s-controller image prefixed"
Assert-Contains $withRegistryK8s "image: ghcr.io/test-owner/kognition-intent-gateway:" "Reg: IntentGateway image prefixed"

$standaloneJob = Invoke-HelmTemplate -SetValues @("mode=standalone")
Assert-RegexMatch $standaloneJob "name: .*-migrations" "Job: migrations Job renders in standalone mode"
Assert-Contains $standaloneJob '"helm.sh/hook": post-install,post-upgrade' "Standalone migrations retain Helm hook behavior"

$k8sPgvectorJob = Invoke-HelmTemplate -SetValues @("mode=kubernetes", "memoryService.backendType=pgvector")
Assert-RegexMatch $k8sPgvectorJob "name: .*-migrations" "Job: migrations Job renders in kubernetes+pgvector"

$k8sDefault = Invoke-HelmTemplate -SetValues @("mode=kubernetes")
Assert-RegexMatch $k8sDefault "name: .*-migrations" "Job: migrations Job renders for kubernetes memory receipts"
Assert-RegexMatch $k8sDefault 'name: test-release-kognition-migrations-[a-f0-9]{12}' "Kubernetes migration Job identity binds SQL content"
Assert-NotContains $k8sDefault '"helm.sh/hook": post-install,post-upgrade' "Kubernetes migrations are inventoried normal resources, not rejected hooks"
Assert-RegexMatch $k8sDefault '(?s)app.kubernetes.io/component: migrations.*?automountServiceAccountToken: false' "Migration workload does not mount Kubernetes API credentials"

# ============================================================
# TEST GROUP 13: Memory embedding configuration
# ============================================================
Write-Host "[Group 13] Memory embedding configuration" -ForegroundColor Yellow

$embeddingDisabled = Invoke-HelmTemplate -SetValues @()
Assert-NotContains $embeddingDisabled "MEMORY_EMBEDDING_PROVIDER" "Embedding: disabled by default"

$localEmbeddingEgress = Invoke-HelmTemplate -SetValues @(
    "mode=kubernetes",
    "networkPolicy.enabled=true",
    "memoryService.embedding.provider=ollama",
    "memoryService.embedding.endpoint=http://ollama.models:11434",
    "memoryService.embedding.model=all-minilm",
    "memoryService.privateEgress[0].namespaceSelector.kubernetes\.io/metadata\.name=models",
    "memoryService.privateEgress[0].podSelector.app=ollama",
    "memoryService.privateEgress[0].ports[0]=11434"
)
Assert-RegexMatch $localEmbeddingEgress '(?s)kind: NetworkPolicy\s+metadata:\s+name: test-release-kognition-memory-service(?:(?!\r?\n---)[\s\S])*?kubernetes.io/metadata.name: models(?:(?!\r?\n---)[\s\S])*?app: ollama(?:(?!\r?\n---)[\s\S])*?port: 11434' "Embedding: local provider egress is scoped to the raw memory service and exact namespace, labels and port"
Assert-HelmTemplateFails `
    -SetValues @("memoryService.privateEgress[0].ports[0]=11434") `
    -Pattern @("memoryService.privateEgress", "/memoryService/privateEgress") `
    -TestName "Embedding: private egress without peer selectors fails closed"
Assert-HelmTemplateFails `
    -SetValues @(
        "memoryService.privateEgress[0].namespaceSelector.app=models",
        "memoryService.privateEgress[0].podSelector.app=ollama",
        "memoryService.privateEgress[0].ports[0]=0"
    ) `
    -Pattern @("memoryService.privateEgress", "/memoryService/privateEgress") `
    -TestName "Embedding: invalid private provider port fails closed"

$embeddingEnabled = Invoke-HelmTemplate -SetValues @(
    "memoryService.embedding.provider=foundry",
    "memoryService.embedding.endpoint=https://example.openai.azure.com",
    "memoryService.embedding.model=my-embedding-deployment",
    "memoryService.embedding.apiVersion=2025-01-01-preview",
    "memoryService.embedding.apiKeySecret.name=memory-embedding",
    "memoryService.embedding.apiKeySecret.key=apiKey"
)
Assert-RegexMatch $embeddingEnabled 'name: MEMORY_EMBEDDING_PROVIDER\s+value: "foundry"' "Embedding: exact provider renders"
Assert-RegexMatch $embeddingEnabled 'name: MEMORY_EMBEDDING_ENDPOINT\s+value: "https://example.openai.azure.com"' "Embedding: exact endpoint renders"
Assert-RegexMatch $embeddingEnabled 'name: MEMORY_EMBEDDING_MODEL\s+value: "my-embedding-deployment"' "Embedding: exact model renders"
Assert-RegexMatch $embeddingEnabled 'name: MEMORY_EMBEDDING_API_VERSION\s+value: "2025-01-01-preview"' "Embedding: exact API version renders"
Assert-RegexMatch $embeddingEnabled 'name: MEMORY_EMBEDDING_API_KEY\s+valueFrom:\s+secretKeyRef:\s+name: "memory-embedding"\s+key: "apiKey"' "Embedding: exact API key Secret reference renders"
Assert-NotContains $embeddingEnabled 'azure.workload.identity/use: "true"' "Embedding: API-key Foundry does not enable workload identity"

$keylessEmbedding = Invoke-HelmTemplate -SetValues @(
    "memoryService.embedding.provider=foundry",
    "memoryService.embedding.endpoint=https://example.openai.azure.com",
    "memoryService.embedding.model=my-embedding-deployment"
)
Assert-Contains $keylessEmbedding 'azure.workload.identity/use: "true"' "Embedding: keyless Foundry enables workload identity"

$clientSecretEmbedding = Invoke-HelmTemplate -SetValues @(
    "memoryService.embedding.provider=foundry",
    "memoryService.embedding.endpoint=https://example.openai.azure.com",
    "memoryService.embedding.model=my-embedding-deployment",
    "memoryService.embedding.azureIdentitySecret=azure-samples-identity"
)
Assert-NotContains $clientSecretEmbedding 'azure.workload.identity/use: "true"' "Embedding: local service principal does not request workload federation"
foreach ($key in @('AZURE_TENANT_ID', 'AZURE_CLIENT_ID', 'AZURE_CLIENT_SECRET')) {
    Assert-RegexMatch $clientSecretEmbedding "name: $key\s+valueFrom:\s+secretKeyRef:\s+name: `"azure-samples-identity`"\s+key: $key" "Embedding: $key comes from the dedicated Secret"
}
Assert-HelmTemplateFails `
    -SetValues @(
        "memoryService.embedding.provider=foundry",
        "memoryService.embedding.endpoint=https://example.openai.azure.com",
        "memoryService.embedding.model=my-embedding-deployment",
        "memoryService.embedding.azureIdentitySecret=azure-samples-identity",
        "memoryService.embedding.apiKeySecret.name=memory-embedding",
        "memoryService.embedding.apiKeySecret.key=apiKey"
    ) `
    -Pattern "mutually exclusive" `
    -TestName "Embedding: conflicting identity and API-key configuration is rejected"
Assert-HelmTemplateFails `
    -SetValues @("memoryService.embedding.azureIdentitySecret=azure-samples-identity") `
    -Pattern "requires the foundry provider" `
    -TestName "Embedding: identity Secret without Foundry configuration is rejected"

$openAIEmbedding = Invoke-HelmTemplate -SetValues @(
    "memoryService.embedding.provider=openai-compatible",
    "memoryService.embedding.endpoint=https://api.openai.com/v1",
    "memoryService.embedding.model=text-embedding-3-small"
)
Assert-NotContains $openAIEmbedding 'azure.workload.identity/use: "true"' "Embedding: non-Foundry provider does not enable workload identity"

Assert-HelmTemplateFails `
    -SetValues @("memoryService.embedding.provider=foundry") `
    -Pattern "provider, endpoint, and model must be configured together" `
    -TestName "Embedding: partial configuration is rejected"
Assert-HelmTemplateFails `
    -SetValues @("memoryService.embedding.apiVersion=2025-01-01-preview") `
    -Pattern "apiVersion is only supported when provider is foundry" `
    -TestName "Embedding: orphaned API version is rejected"
Assert-HelmTemplateFails `
    -SetValues @(
        "memoryService.embedding.provider=openai-compatible",
        "memoryService.embedding.endpoint=https://api.openai.com/v1",
        "memoryService.embedding.model=text-embedding-3-small",
        "memoryService.embedding.apiVersion=2025-01-01-preview"
    ) `
    -Pattern "apiVersion is only supported when provider is foundry" `
    -TestName "Embedding: API version on non-Foundry provider is rejected"

Assert-HelmTemplateFails `
    -SetValues @(
        "memoryService.embedding.provider=foundry",
        "memoryService.embedding.endpoint=https://example.openai.azure.com",
        "memoryService.embedding.model=text-embedding-3-small",
        "memoryService.embedding.apiKeySecret.name=memory-embedding"
    ) `
    -Pattern "apiKeySecret.name and key must be configured together" `
    -TestName "Embedding: Secret name without key is rejected"
Assert-HelmTemplateFails `
    -SetValues @(
        "memoryService.embedding.provider=foundry",
        "memoryService.embedding.endpoint=https://example.openai.azure.com",
        "memoryService.embedding.model=text-embedding-3-small",
        "memoryService.embedding.apiKeySecret.key=apiKey"
    ) `
    -Pattern "apiKeySecret.name and key must be configured together" `
    -TestName "Embedding: Secret key without name is rejected"
Assert-HelmTemplateFails `
    -SetValues @(
        "memoryService.embedding.apiKeySecret.name=memory-embedding",
        "memoryService.embedding.apiKeySecret.key=apiKey"
    ) `
    -Pattern "apiKeySecret requires provider, endpoint, and model" `
    -TestName "Embedding: orphaned API key Secret is rejected"
# ============================================================
# TEST GROUP 14: Production values render hardened defaults
# ============================================================
Write-Host "[Group 14] Production values profile" -ForegroundColor Yellow

$productionValues = Join-Path $ChartPath "values-production.yaml"
$productionResult = & helm template test-release $ChartPath --kube-version 1.30.3 --values $productionValues --set "networkPolicy.kubeletProbeIngressCIDRs[0]=10.0.0.0/16" 2>&1
if ($LASTEXITCODE -ne 0) {
    $script:Failed++
    $script:Errors += "FAIL: production values render - $productionResult"
} else {
    $productionOutput = $productionResult -join "`n"
    Assert-RegexMatch $productionOutput '(?s)kind: Deployment\s+metadata:\s+name: test-release-kognition-k8s-controller(?:(?!\r?\n---)[\s\S])*?replicas: 0' "Prod: controller remains inactive until explicit activation"
    Assert-Contains $productionOutput "failurePolicy: Fail" "Prod: admission webhook fails closed"
    Assert-Contains $productionOutput "kind: NetworkPolicy" "Prod: network policies render"
    Assert-Contains $productionOutput "kind: PodDisruptionBudget" "Prod: disruption budget renders"
    Assert-NotContains $productionOutput "KOGNITION_PLUGIN_HOST_ENABLED" "Prod: experimental plugin host remains disabled"
    Assert-RegexMatch $productionOutput "(?s)kind: Deployment.*?name: test-release-kognition-tool-gateway.*?replicas: 3" "Prod: Tool Gateway runs three replicas"
    Assert-RegexMatch $productionOutput "(?s)kind: Deployment.*?name: test-release-kognition-intent-gateway.*?replicas: 3" "Prod: Intent Gateway runs three replicas"
    Assert-RegexMatch $productionOutput "(?s)kind: PodDisruptionBudget.*?name: test-release-kognition-tool-gateway" "Prod: Tool Gateway PDB renders"
    Assert-RegexMatch $productionOutput "(?s)kind: PodDisruptionBudget.*?name: test-release-kognition-intent-gateway" "Prod: Intent Gateway PDB renders"
    Assert-NotContains $productionOutput "app.kubernetes.io/component: memory-service" "Prod: process-local memory service disabled"
    Assert-NotContains $productionOutput "KOGNITION_PLUGIN_HOST_ENABLED" "Prod: experimental plugin host remains disabled"
}

# ============================================================
# RESULTS
# ============================================================
Write-Host ""
Write-Host "========================================" -ForegroundColor Cyan
Write-Host " RESULTS" -ForegroundColor Cyan
Write-Host "========================================" -ForegroundColor Cyan
Write-Host "  Passed: $Passed" -ForegroundColor Green
Write-Host "  Failed: $Failed" -ForegroundColor $(if ($Failed -gt 0) { "Red" } else { "Green" })
Write-Host ""

if ($Errors.Count -gt 0) {
    Write-Host "Failures:" -ForegroundColor Red
    foreach ($err in $Errors) {
        Write-Host "  - $err" -ForegroundColor Red
    }
    exit 1
} else {
    Write-Host "All regression tests passed!" -ForegroundColor Green
    exit 0
}
