# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

param(
    [string]$ControllerNamespace = "kognition-system",
    [string]$ObservabilityNamespace = "kognition-observability",
    [string]$ReleaseName = "kognition",
    [string]$ObservabilityReleaseName = "kognition-observability"
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$gatewayService = "$ReleaseName-tool-gateway"
$expectedAlerts = @(
    @{ Name = "KognitionToolGatewayInvocationFailures"; Query = 'sum(rate(kognition_tool_gateway_invocations_total{outcome!="success"}[5m])) > 0' },
    @{ Name = "KognitionToolGatewayCircuitOpen"; Query = 'sum(rate(kognition_tool_gateway_resilience_events_total{reason="circuit_open"}[5m])) > 0' },
    @{ Name = "KognitionToolGatewayConcurrencyRejected"; Query = 'sum(rate(kognition_tool_gateway_resilience_events_total{reason="concurrency_limit"}[5m])) > 0' },
    @{ Name = "KognitionToolGatewayAuditCompletionFailed"; Query = 'sum(rate(kognition_tool_gateway_resilience_events_total{reason="audit_completion_failed"}[5m])) > 0' }
)
$prometheusPortForward = $null
$grafanaPortForward = $null

function Invoke-Kubectl {
    param([Parameter(Mandatory = $true)][string[]]$Arguments)

    $output = & kubectl @Arguments 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "kubectl $($Arguments -join ' ') failed:`n$($output -join "`n")"
    }
    return $output
}

function Get-AvailableTcpPort {
    $listener = [System.Net.Sockets.TcpListener]::new([System.Net.IPAddress]::Loopback, 0)
    try {
        $listener.Start()
        return ([System.Net.IPEndPoint]$listener.LocalEndpoint).Port
    }
    finally {
        $listener.Stop()
    }
}

function Start-KubectlPortForward {
    param(
        [Parameter(Mandatory = $true)][string]$Namespace,
        [Parameter(Mandatory = $true)][string]$Service,
        [Parameter(Mandatory = $true)][int]$LocalPort,
        [Parameter(Mandatory = $true)][int]$RemotePort
    )

    return Start-Job -ScriptBlock {
        param($PortForwardNamespace, $PortForwardService, $PortForwardLocalPort, $PortForwardRemotePort)
        kubectl -n $PortForwardNamespace port-forward "svc/$PortForwardService" "${PortForwardLocalPort}:$PortForwardRemotePort"
    } -ArgumentList $Namespace, $Service, $LocalPort, $RemotePort
}

function Get-KubernetesService {
    param(
        [Parameter(Mandatory = $true)][string]$Namespace,
        [Parameter(Mandatory = $true)][string]$LabelSelector,
        [Parameter(Mandatory = $true)][string]$Description
    )

    $services = ((Invoke-Kubectl @("get", "service", "-n", $Namespace, "-l", $LabelSelector, "-o", "json")) -join "`n" | ConvertFrom-Json).items
    if (@($services).Count -ne 1) {
        $names = @($services | ForEach-Object { $_.metadata.name }) -join ", "
        throw "Expected one $Description Service for selector '$LabelSelector', found $(@($services).Count): $names"
    }
    return $services[0]
}

function Get-KubernetesSecret {
    param(
        [Parameter(Mandatory = $true)][string]$Namespace,
        [Parameter(Mandatory = $true)][string]$LabelSelector,
        [Parameter(Mandatory = $true)][string]$Description
    )

    $secrets = ((Invoke-Kubectl @("get", "secret", "-n", $Namespace, "-l", $LabelSelector, "-o", "json")) -join "`n" | ConvertFrom-Json).items
    if (@($secrets).Count -ne 1) {
        $names = @($secrets | ForEach-Object { $_.metadata.name }) -join ", "
        throw "Expected one $Description Secret for selector '$LabelSelector', found $(@($secrets).Count): $names"
    }
    return $secrets[0]
}

function Get-ServicePort {
    param(
        [Parameter(Mandatory = $true)][object]$Service,
        [Parameter(Mandatory = $true)][string]$PortName
    )

    $port = @($Service.spec.ports | Where-Object { $_.name -eq $PortName }) | Select-Object -First 1
    if (-not $port) { throw "Service $($Service.metadata.name) has no '$PortName' port" }
    return [int]$port.port
}

function Wait-ForHttp {
    param(
        [Parameter(Mandatory = $true)][string]$Uri,
        [hashtable]$Headers = @{},
        [int]$TimeoutSeconds = 30
    )

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        try {
            return Invoke-RestMethod -Uri $Uri -Headers $Headers -TimeoutSec 2
        }
        catch {
            [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
        }
    } while ([DateTime]::UtcNow -lt $deadline)

    throw "$Uri did not become available within $TimeoutSeconds seconds"
}

function Invoke-PrometheusQuery {
    param(
        [Parameter(Mandatory = $true)][string]$BaseUri,
        [Parameter(Mandatory = $true)][string]$Query
    )

    return Wait-ForHttp -Uri "$BaseUri/api/v1/query?query=$([uri]::EscapeDataString($Query))"
}

function Get-PrometheusScalar {
    param(
        [Parameter(Mandatory = $true)][string]$BaseUri,
        [Parameter(Mandatory = $true)][string]$Query
    )

    $result = Invoke-PrometheusQuery -BaseUri $BaseUri -Query $Query
    if ($result.data.result.Count -eq 0) { return [double]0 }
    if ($result.data.result.Count -ne 1) { throw "Prometheus query returned $($result.data.result.Count) results, want one: $Query" }
    return [double]$result.data.result[0].value[1]
}

function Wait-ForPrometheusIncrease {
    param(
        [Parameter(Mandatory = $true)][string]$BaseUri,
        [Parameter(Mandatory = $true)][string]$Query,
        [Parameter(Mandatory = $true)][double]$Before,
        [Parameter(Mandatory = $true)][string]$Description,
        [int]$TimeoutSeconds = 120
    )

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        $after = Get-PrometheusScalar -BaseUri $BaseUri -Query $Query
        if ($after -gt $Before) { return $after }
        [Threading.Tasks.Task]::Delay(2000).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)

    throw "$Description did not increase after the resilience run: before=$Before"
}

function Wait-ForPrometheusPositive {
    param(
        [Parameter(Mandatory = $true)][string]$BaseUri,
        [Parameter(Mandatory = $true)][string]$Query,
        [Parameter(Mandatory = $true)][string]$Description,
        [int]$TimeoutSeconds = 120
    )

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        if ((Get-PrometheusScalar -BaseUri $BaseUri -Query $Query) -gt 0) { return }
        [Threading.Tasks.Task]::Delay(2000).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)

    throw "$Description did not produce a positive Prometheus series"
}

function Convert-PrometheusDurationToSeconds {
    param([Parameter(Mandatory = $true)][string]$Duration)

    if ($Duration -notmatch '^(?<value>[1-9][0-9]*)(?<unit>ms|s|m|h)$') {
        throw "Unsupported ServiceMonitor scrape interval '$Duration'"
    }
    $value = [double]$Matches.value
    switch ($Matches.unit) {
        "ms" { return [int][Math]::Ceiling($value / 1000) }
        "s" { return [int]$value }
        "m" { return [int]($value * 60) }
        "h" { return [int]($value * 3600) }
    }
}

function Format-PromQL {
    param([Parameter(Mandatory = $true)][string]$Query)

    return [regex]::Replace($Query, "\s+", "")
}

function Invoke-GrafanaPrometheusQuery {
    param(
        [Parameter(Mandatory = $true)][string]$BaseUri,
        [Parameter(Mandatory = $true)][hashtable]$Headers,
        [Parameter(Mandatory = $true)][string]$DatasourceUID,
        [Parameter(Mandatory = $true)][string]$DatasourceType,
        [Parameter(Mandatory = $true)][string]$Expression
    )

    $body = @{
        from = "now-5m"
        queries = @(@{
            refId = "A"
            datasource = @{ type = $DatasourceType; uid = $DatasourceUID }
            expr = $Expression
            instant = $true
            range = $false
            format = "time_series"
        })
        to = "now"
    } | ConvertTo-Json -Depth 5 -Compress
    try {
        return Invoke-RestMethod -Uri "$BaseUri/api/ds/query" -Method Post -Headers $Headers -ContentType "application/json" -Body $body -TimeoutSec 10
    }
    catch {
        throw "Grafana datasource query failed for uid='$DatasourceUID', type='$DatasourceType', expression='$Expression': $($_.Exception.Message)"
    }
}

function Assert-GrafanaPanelData {
    param(
        [Parameter(Mandatory = $true)][string]$BaseUri,
        [Parameter(Mandatory = $true)][hashtable]$Headers,
        [Parameter(Mandatory = $true)][string]$DatasourceUID,
        [Parameter(Mandatory = $true)][string]$DatasourceType,
        [Parameter(Mandatory = $true)][object]$Panel,
        [int]$TimeoutSeconds = 120
    )

    if (-not $Panel.targets -or $Panel.targets.Count -lt 1) { throw "Grafana panel '$($Panel.title)' has no targets configured" }
    $expression = $Panel.targets[0].expr
    if (-not $expression) { throw "Grafana panel '$($Panel.title)' has no PromQL expression" }

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        $result = Invoke-GrafanaPrometheusQuery -BaseUri $BaseUri -Headers $Headers -DatasourceUID $DatasourceUID -DatasourceType $DatasourceType -Expression $expression
        $frames = @($result.results.A.frames)
        $hasRows = @($frames | Where-Object { $_.data.values.Count -gt 0 -and $_.data.values[0].Count -gt 0 }).Count -gt 0
        if ($hasRows) { return }
        [Threading.Tasks.Task]::Delay(2000).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)

    throw "Grafana panel '$($Panel.title)' returned no data through its configured datasource"
}

function Assert-ToolGatewayTarget {
    param(
        [Parameter(Mandatory = $true)][string]$BaseUri,
        [Parameter(Mandatory = $true)][string]$Service
    )

    $deadline = [DateTime]::UtcNow.AddSeconds(90)
    do {
        $targets = Wait-ForHttp -Uri "$BaseUri/api/v1/targets?state=active"
        $target = @($targets.data.activeTargets | Where-Object {
            $_.health -eq "up" -and $_.labels.service -eq $Service
        }) | Select-Object -First 1
        if ($target) { return $target }
        [Threading.Tasks.Task]::Delay(2000).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)

    throw "Prometheus did not scrape a healthy $Service target within 90 seconds"
}

Write-Host "=== Kognition Tool Gateway Monitoring E2E ===" -ForegroundColor Cyan

try {
    $serviceMonitor = ((Invoke-Kubectl @("get", "servicemonitor/$gatewayService", "-n", $ControllerNamespace, "-o", "json")) -join "`n" | ConvertFrom-Json)
    if ($serviceMonitor.metadata.labels.release -ne $ObservabilityReleaseName) {
        throw "ServiceMonitor release label is '$($serviceMonitor.metadata.labels.release)', want '$ObservabilityReleaseName'"
    }
    if (-not $serviceMonitor.spec.endpoints -or $serviceMonitor.spec.endpoints.Count -lt 1) {
        throw "ServiceMonitor has no endpoints configured"
    }
    if ($serviceMonitor.spec.endpoints[0].path -ne "/metrics" -or $serviceMonitor.spec.endpoints[0].port -ne "http") {
        throw "ServiceMonitor does not scrape the Tool Gateway http /metrics endpoint"
    }
    $scrapeIntervalSeconds = Convert-PrometheusDurationToSeconds -Duration $serviceMonitor.spec.endpoints[0].interval
    $observationSeconds = [Math]::Max(45, $scrapeIntervalSeconds * 2)
    $metricWindowSeconds = [Math]::Max(120, $scrapeIntervalSeconds * 4)

    $prometheusRule = ((Invoke-Kubectl @("get", "prometheusrule/$gatewayService", "-n", $ControllerNamespace, "-o", "json")) -join "`n" | ConvertFrom-Json)
    if ($prometheusRule.metadata.labels.release -ne $ObservabilityReleaseName) {
        throw "PrometheusRule release label is '$($prometheusRule.metadata.labels.release)', want '$ObservabilityReleaseName'"
    }

    $prometheusPort = Get-AvailableTcpPort
    $grafanaPort = Get-AvailableTcpPort
    $prometheusService = Get-KubernetesService -Namespace $ObservabilityNamespace -LabelSelector "app=kube-prometheus-stack-prometheus,release=$ObservabilityReleaseName" -Description "Prometheus"
    $grafanaService = Get-KubernetesService -Namespace $ObservabilityNamespace -LabelSelector "app.kubernetes.io/name=grafana,app.kubernetes.io/instance=$ObservabilityReleaseName" -Description "Grafana"
    $prometheusPortForward = Start-KubectlPortForward -Namespace $ObservabilityNamespace -Service $prometheusService.metadata.name -LocalPort $prometheusPort -RemotePort (Get-ServicePort -Service $prometheusService -PortName "http-web")
    $grafanaPortForward = Start-KubectlPortForward -Namespace $ObservabilityNamespace -Service $grafanaService.metadata.name -LocalPort $grafanaPort -RemotePort (Get-ServicePort -Service $grafanaService -PortName "http-web")

    $prometheusUri = "http://127.0.0.1:$prometheusPort"
    $grafanaUri = "http://127.0.0.1:$grafanaPort"
    $target = Assert-ToolGatewayTarget -BaseUri $prometheusUri -Service $gatewayService
    $metricQueries = @{
        Invocations = "sum(kognition_tool_gateway_invocations_total{service=`"$gatewayService`"})"
        AuditCompletionFailed = "sum(kognition_tool_gateway_resilience_events_total{service=`"$gatewayService`",reason=`"audit_completion_failed`"})"
    }
    $resilienceMetricQueries = @{
        Failures = "sum(increase(kognition_tool_gateway_invocations_total{service=`"$gatewayService`",outcome!=`"success`"}[$($metricWindowSeconds)s]))"
        CircuitOpen = "sum(increase(kognition_tool_gateway_resilience_events_total{service=`"$gatewayService`",reason=`"circuit_open`"}[$($metricWindowSeconds)s]))"
        ConcurrencyRejected = "sum(increase(kognition_tool_gateway_resilience_events_total{service=`"$gatewayService`",reason=`"concurrency_limit`"}[$($metricWindowSeconds)s]))"
    }

    $resilienceScript = Join-Path $PSScriptRoot "kubernetes-tool-gateway-resilience-e2e.ps1"
    & pwsh -NoProfile -File $resilienceScript -ControllerNamespace $ControllerNamespace -ReleaseName $ReleaseName -PostReplacementObservationSeconds $observationSeconds
    if ($LASTEXITCODE -ne 0) { throw "Tool Gateway resilience E2E failed with code $LASTEXITCODE" }
    foreach ($metricName in $resilienceMetricQueries.Keys) {
        Wait-ForPrometheusPositive -BaseUri $prometheusUri -Query $resilienceMetricQueries[$metricName] -Description "Tool Gateway $metricName metric from the post-replacement resilience run"
    }

    $invocationsBefore = Get-PrometheusScalar -BaseUri $prometheusUri -Query $metricQueries.Invocations
    $auditCompletionFailuresBefore = Get-PrometheusScalar -BaseUri $prometheusUri -Query $metricQueries.AuditCompletionFailed

    $toolGatewayScript = Join-Path $PSScriptRoot "kubernetes-tool-gateway-e2e.ps1"
    & pwsh -NoProfile -File $toolGatewayScript -ControllerNamespace $ControllerNamespace -ReleaseName $ReleaseName
    if ($LASTEXITCODE -ne 0) { throw "Tool Gateway E2E failed with code $LASTEXITCODE" }

    Wait-ForPrometheusIncrease -BaseUri $prometheusUri -Query $metricQueries.Invocations -Before $invocationsBefore -Description "Tool Gateway invocation metric" | Out-Null
    $auditCompletionFailures = Get-PrometheusScalar -BaseUri $prometheusUri -Query $metricQueries.AuditCompletionFailed
    if ($auditCompletionFailures -ne $auditCompletionFailuresBefore) {
        throw "Tool Gateway recorded an unexpected audit completion failure during the resilience run"
    }

    $rules = Wait-ForHttp -Uri "$prometheusUri/api/v1/rules"
    $toolGatewayGroup = @($rules.data.groups | Where-Object { $_.name -eq "kognition.tool-gateway" }) | Select-Object -First 1
    if (-not $toolGatewayGroup) { throw "Prometheus has not loaded the kognition.tool-gateway rule group" }
    foreach ($expectedAlert in $expectedAlerts) {
        $rule = @($toolGatewayGroup.rules | Where-Object { $_.name -eq $expectedAlert.Name }) | Select-Object -First 1
        if (-not $rule -or $rule.health -ne "ok" -or -not $rule.lastEvaluation) {
            throw "Alert rule $($expectedAlert.Name) is missing or unhealthy"
        }
        if ((Format-PromQL -Query $rule.query) -ne (Format-PromQL -Query $expectedAlert.Query)) {
            throw "Alert rule $($expectedAlert.Name) has an unexpected PromQL expression: $($rule.query)"
        }
    }

    $grafanaSecret = Get-KubernetesSecret -Namespace $ObservabilityNamespace -LabelSelector "app.kubernetes.io/component=admin-secret,app.kubernetes.io/instance=$ObservabilityReleaseName" -Description "Grafana admin"
    $encodedPassword = (Invoke-Kubectl @("get", "secret/$($grafanaSecret.metadata.name)", "-n", $ObservabilityNamespace, "-o", "jsonpath={.data.admin-password}")) -join ""
    $password = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($encodedPassword))
    $authorization = [Convert]::ToBase64String([Text.Encoding]::ASCII.GetBytes("admin:$password"))
    $headers = @{ Authorization = "Basic $authorization" }
    Wait-ForHttp -Uri "$grafanaUri/api/health" -Headers $headers | Out-Null
    $datasources = Invoke-RestMethod -Uri "$grafanaUri/api/datasources" -Headers $headers -TimeoutSec 10
    $datasource = @($datasources | Where-Object { $_.type -eq "prometheus" -and $_.isDefault }) | Select-Object -First 1
    if (-not $datasource) { throw "Grafana has no default Prometheus datasource" }
    $dashboards = Wait-ForHttp -Uri "$grafanaUri/api/search?query=Tool%20Gateway%20Operations" -Headers $headers
    $dashboard = @($dashboards | Where-Object { $_.title -eq "Tool Gateway Operations" -and $_.uid -eq "tool-gateway-operations" }) | Select-Object -First 1
    if (-not $dashboard) { throw "Grafana has not loaded the Tool Gateway Operations dashboard" }
    $dashboardDefinition = Wait-ForHttp -Uri "$grafanaUri/api/dashboards/uid/$($dashboard.uid)" -Headers $headers
    if ($dashboardDefinition.dashboard.refresh -ne "5s") {
        throw "Tool Gateway Operations dashboard refresh is '$($dashboardDefinition.dashboard.refresh)', want '5s'"
    }
    if ($dashboardDefinition.dashboard.time.from -ne "now-15m" -or $dashboardDefinition.dashboard.time.to -ne "now") {
        throw "Tool Gateway Operations dashboard time range is '$($dashboardDefinition.dashboard.time.from)' to '$($dashboardDefinition.dashboard.time.to)', want 'now-15m' to 'now'"
    }
    $panels = @($dashboardDefinition.dashboard.panels)
    $expectedPanelTitles = @(
        "Invocations (15m)",
        "Invocation Outcome Mix",
        "P95 Invocation Latency",
        "Resilience Events (15m)",
        "Resilience Event Mix"
    )
    foreach ($panelTitle in $expectedPanelTitles) {
        if (-not @($panels | Where-Object { $_.title -eq $panelTitle }).Count) {
            throw "Tool Gateway Operations dashboard is missing panel '$panelTitle'"
        }
    }
    foreach ($panel in $panels) {
        Assert-GrafanaPanelData -BaseUri $grafanaUri -Headers $headers -DatasourceUID ([string]$datasource.uid) -DatasourceType ([string]$datasource.type) -Panel $panel
    }

    Write-Host "PASS: Prometheus scraped $($target.labels.instance), recorded Tool Gateway invocation metrics, evaluated all alerts, and Grafana loaded every Tool Gateway Operations panel." -ForegroundColor Green
}
finally {
    foreach ($portForward in @($prometheusPortForward, $grafanaPortForward)) {
        if ($portForward) {
            Stop-Job $portForward -ErrorAction SilentlyContinue
            Remove-Job $portForward -Force -ErrorAction SilentlyContinue
        }
    }
}