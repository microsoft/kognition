# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

# Verifies Kubernetes IntentGateway HTTP serving through the dedicated Service.
# Prerequisite: deploy the Helm chart with mode=kubernetes and intentGateway.enabled=true.

param(
    [string]$ControllerNamespace = "default",
    [string]$ReleaseName = "kognition",
    [string]$BearerToken = ""
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$RepoRoot = Split-Path -Parent $PSScriptRoot
Push-Location $RepoRoot

$namespace = "intent-gateway-e2e-" + ([guid]::NewGuid().ToString("N").Substring(0, 8))
$serviceName = "$ReleaseName-intent-gateway"
$portForward = $null

function Invoke-Kubectl {
    param([Parameter(Mandatory = $true)][string[]]$Arguments)

    $output = & kubectl @Arguments 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "kubectl $($Arguments -join ' ') failed:`n$($output -join "`n")"
    }
    return $output
}

function Invoke-KubectlManifest {
    param([Parameter(Mandatory = $true)][hashtable]$Manifest)

    $json = $Manifest | ConvertTo-Json -Depth 8
    $output = $json | & kubectl apply -f - 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "kubectl apply manifest failed:`n$($output -join "`n")"
    }
    return $output
}

function Wait-ForJsonPath {
    param(
        [Parameter(Mandatory = $true)][string]$Resource,
        [Parameter(Mandatory = $true)][string]$JsonPath,
        [Parameter(Mandatory = $true)][string]$Expected,
        [int]$TimeoutSeconds = 90
    )

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        $value = (& kubectl get $Resource -n $namespace "-o=jsonpath=$JsonPath" 2>$null | Out-String).Trim()
        if ($value -eq $Expected) {
            return
        }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "$Resource did not reach $JsonPath=$Expected within $TimeoutSeconds seconds"
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

function Wait-ForPortForward {
    param(
        [Parameter(Mandatory = $true)][int]$HTTPPort,
        [Parameter(Mandatory = $true)][int]$GRPCPort,
        [int]$TimeoutSeconds = 30
    )

    $deadline = [DateTime]::UtcNow.AddSeconds($TimeoutSeconds)
    do {
        try {
            $ready = Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:$HTTPPort/readyz" -TimeoutSec 2
            $client = [System.Net.Sockets.TcpClient]::new()
            try {
                $grpcConnected = $client.ConnectAsync("127.0.0.1", $GRPCPort).Wait(2000)
            }
            finally {
                $client.Dispose()
            }
            if ($ready.StatusCode -eq 200 -and $grpcConnected) {
                return
            }
        }
        catch {
        }
        [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
    } while ([DateTime]::UtcNow -lt $deadline)
    throw "IntentGateway port-forward did not become ready within $TimeoutSeconds seconds"
}

Write-Host "=== Kognition E2E Test - Kubernetes IntentGateway ===" -ForegroundColor Cyan

try {
    Invoke-Kubectl @("get", "deployment/$serviceName", "-n", $ControllerNamespace) | Out-Null
    Invoke-Kubectl @("wait", "-n", $ControllerNamespace, "--for=condition=Available", "deployment/$serviceName", "--timeout=120s") | Out-Null
    Invoke-Kubectl @("create", "namespace", $namespace) | Out-Null

        $plannerAgent = @{
                apiVersion = "kognition.dev/v1alpha1"
                kind = "Agent"
                metadata = @{ name = "planner"; namespace = $namespace }
                spec = @{
                        modelRef = @{ name = "placeholder" }
                        instructions = @{ inline = "Test target for IntentGateway routing." }
                        replicas = 1
                }
        }
        $agentGateway = @{
                apiVersion = "kognition.dev/v1alpha1"
                kind = "IntentGateway"
                metadata = @{ name = "support"; namespace = $namespace }
                spec = @{
                        protocol = "http"
                        routes = @(@{
                                pattern = "deploy *"
                                targetKind = "Agent"
                                targetRef = @{ name = "planner" }
                        })
                }
        }
        Invoke-KubectlManifest -Manifest $plannerAgent | Out-Null
        Invoke-KubectlManifest -Manifest $agentGateway | Out-Null

    Wait-ForJsonPath -Resource "intentgateway/support" -JsonPath "{.status.phase}" -Expected "Ready"
    $endpoint = (& kubectl get intentgateway/support -n $namespace -o jsonpath='{.status.endpoint}' | Out-String).Trim()
    if ($endpoint -notmatch "^http://$([regex]::Escape($serviceName))\.$([regex]::Escape($ControllerNamespace))\.svc\.cluster\.local:8080/") {
        throw "IntentGateway endpoint is not service DNS: $endpoint"
    }

    $localPort = Get-AvailableTcpPort
    $grpcLocalPort = Get-AvailableTcpPort
    $portForward = Start-Job -ScriptBlock {
        param($Namespace, $Service, $HTTPPort, $GRPCPort)
        kubectl -n $Namespace port-forward "service/$Service" "${HTTPPort}:8080" "${GRPCPort}:8081"
    } -ArgumentList $ControllerNamespace, $serviceName, $localPort, $grpcLocalPort
    Wait-ForPortForward -HTTPPort $localPort -GRPCPort $grpcLocalPort

    $headers = @{}
    if ($BearerToken) {
        $headers.Authorization = "Bearer $BearerToken"
    }
    $response = Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:$localPort/apis/kognition.dev/v1alpha1/namespaces/$namespace/intentgateways/support/goals" -Headers $headers -ContentType "application/json" -Body '{"goal":"deploy the release"}'
    if (-not $response.solutionRef.name) {
        throw "Gateway response did not contain solutionRef.name"
    }
    $solution = (& kubectl get "solution/$($response.solutionRef.name)" -n $namespace -o json | ConvertFrom-Json)
    if ($solution.spec.target.kind -ne "Agent" -or $solution.spec.target.ref.name -ne "planner") {
        throw "Solution target does not match the routed Agent: $($solution.spec.target | ConvertTo-Json -Compress)"
    }

    $grpcArgs = @("./cmd/kognition-intent-gateway-client", "-address", "127.0.0.1:$grpcLocalPort", "-namespace", $namespace, "-gateway", "support", "-goal", "deploy through grpc")
    if ($BearerToken) {
        $grpcArgs += @("-bearer-token", $BearerToken)
    }
    $grpcResponse = (& go run @grpcArgs | ConvertFrom-Json)
    if (-not $grpcResponse.solutionName -or $grpcResponse.solutionNamespace -ne $namespace) {
        throw "gRPC gateway response is invalid: $($grpcResponse | ConvertTo-Json -Compress)"
    }
    $grpcSolution = (& kubectl get "solution/$($grpcResponse.solutionName)" -n $namespace -o json | ConvertFrom-Json)
    if ($grpcSolution.spec.target.kind -ne "Agent" -or $grpcSolution.spec.target.ref.name -ne "planner") {
        throw "gRPC Solution target does not match the routed Agent: $($grpcSolution.spec.target | ConvertTo-Json -Compress)"
    }

        Wait-ForJsonPath -Resource "agent/planner" -JsonPath "{.status.revisionCount}" -Expected "1"
        $deadline = [DateTime]::UtcNow.AddSeconds(90)
        $readyInstanceName = ""
        do {
            $instances = (& kubectl get agentinstance -n $namespace -o json | ConvertFrom-Json).items
            $readyInstance = $instances | Where-Object {
                $_.spec.agentRef.name -eq "planner" -and $_.metadata.ownerReferences.kind -eq "Agent" -and $_.metadata.ownerReferences.name -eq "planner"
            } | Select-Object -First 1
            if ($readyInstance) {
                $readyInstanceName = $readyInstance.metadata.name
                break
            }
            [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
        } while ([DateTime]::UtcNow -lt $deadline)
        if (-not $readyInstanceName) {
            throw "Planner Agent did not create an owned AgentInstance within 90 seconds"
        }

        $agentService = @{
            apiVersion = "kognition.dev/v1alpha1"
            kind = "AgentService"
            metadata = @{ name = "planner-service"; namespace = $namespace }
            spec = @{ agentRef = @{ name = "planner" } }
        }
        $serviceGateway = @{
            apiVersion = "kognition.dev/v1alpha1"
            kind = "IntentGateway"
            metadata = @{ name = "support-service"; namespace = $namespace }
            spec = @{
                protocol = "http"
                routes = @(@{
                    pattern = "service *"
                    targetKind = "AgentService"
                    targetRef = @{ name = "planner-service" }
                })
            }
        }
        Invoke-KubectlManifest -Manifest $agentService | Out-Null
        Invoke-KubectlManifest -Manifest $serviceGateway | Out-Null

        Invoke-Kubectl @("patch", "agentinstance/$readyInstanceName", "-n", $namespace, "--subresource=status", "--type=merge", "-p", '{"status":{"phase":"Ready"}}') | Out-Null
    Wait-ForJsonPath -Resource "agentservice/planner-service" -JsonPath "{.status.phase}" -Expected "Ready"
        Wait-ForJsonPath -Resource "intentgateway/support-service" -JsonPath "{.status.phase}" -Expected "Ready"

        $serviceResponse = Invoke-RestMethod -Method Post -Uri "http://127.0.0.1:$localPort/apis/kognition.dev/v1alpha1/namespaces/$namespace/intentgateways/support-service/goals" -Headers $headers -ContentType "application/json" -Body '{"goal":"service deployment"}'
        if (-not $serviceResponse.solutionRef.name) {
                throw "AgentService gateway response did not contain solutionRef.name"
        }
        $serviceSolution = (& kubectl get "solution/$($serviceResponse.solutionRef.name)" -n $namespace -o json | ConvertFrom-Json)
        if ($serviceSolution.spec.target.kind -ne "AgentService" -or $serviceSolution.spec.target.ref.name -ne "planner-service") {
                throw "Solution target does not match the routed AgentService: $($serviceSolution.spec.target | ConvertTo-Json -Compress)"
        }
        Wait-ForJsonPath -Resource "solution/$($serviceResponse.solutionRef.name)" -JsonPath "{.status.phase}" -Expected "Ready"
        $compiledJob = $serviceSolution.status.compiledResources | Where-Object { $_.kind -eq "AgentJob" } | Select-Object -First 1
        if (-not $compiledJob.name) {
                $serviceSolution = (& kubectl get "solution/$($serviceResponse.solutionRef.name)" -n $namespace -o json | ConvertFrom-Json)
                $compiledJob = $serviceSolution.status.compiledResources | Where-Object { $_.kind -eq "AgentJob" } | Select-Object -First 1
        }
        if (-not $compiledJob.name) {
                throw "AgentService Solution did not compile an AgentJob: $($serviceSolution.status | ConvertTo-Json -Compress)"
        }
        $serviceJob = (& kubectl get "agentjob/$($compiledJob.name)" -n $namespace -o json | ConvertFrom-Json)
        if ($serviceJob.spec.agentRef.name -ne "planner" -or $serviceJob.metadata.annotations.'kognition.dev/agent-service' -ne "planner-service") {
                throw "AgentService job did not preserve the routed Agent identity: $($serviceJob | ConvertTo-Json -Compress)"
        }
        $deadline = [DateTime]::UtcNow.AddSeconds(90)
        $serviceInstance = $null
        do {
            $instances = (& kubectl get agentinstance -n $namespace -o json | ConvertFrom-Json).items
            $serviceInstance = $instances | Where-Object {
                $_.metadata.ownerReferences.kind -eq "AgentJob" -and $_.metadata.ownerReferences.name -eq $compiledJob.name
            } | Select-Object -First 1
            if ($serviceInstance) {
                break
            }
            [Threading.Tasks.Task]::Delay(500).GetAwaiter().GetResult()
        } while ([DateTime]::UtcNow -lt $deadline)
        if (-not $serviceInstance) {
            throw "AgentService job did not create an AgentInstance within 90 seconds"
        }
        if ($serviceInstance.spec.agentRef.name -ne "planner" -or $serviceInstance.metadata.annotations.'kognition.dev/agent-service' -ne "planner-service") {
            throw "AgentService AgentInstance did not reach the dispatcher boundary correctly: $($serviceInstance | ConvertTo-Json -Compress)"
        }

    Invoke-Kubectl @("delete", "agent/planner", "-n", $namespace, "--wait=false") | Out-Null
    Wait-ForJsonPath -Resource "intentgateway/support" -JsonPath "{.status.phase}" -Expected "Pending"
    Write-Host "IntentGateway Kubernetes HTTP and gRPC serving E2E passed." -ForegroundColor Green
}
finally {
    if ($null -ne $portForward) {
        Stop-Job $portForward -ErrorAction SilentlyContinue
        Remove-Job $portForward -Force -ErrorAction SilentlyContinue
    }
    & kubectl delete namespace $namespace --wait=false 2>$null | Out-Null
    Pop-Location
}
