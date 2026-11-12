# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

function Invoke-TracedGatewayRequest {
    param([Parameter(Mandatory = $true)][hashtable]$RequestParameters)

    $parameters = @{} + $RequestParameters
    $parameters.Headers = @{} + $RequestParameters.Headers
    $activity = [Diagnostics.Activity]::new("kognition.e2e.gateway.request")
    $null = $activity.SetIdFormat([Diagnostics.ActivityIdFormat]::W3C)
    $null = $activity.Start()
    $parameters.Headers.traceparent = $activity.Id
    $statusCode = $null
    try {
        $response = Invoke-WebRequest @parameters
        $statusCode = [int]$response.StatusCode
        return $response
    }
    finally {
        $activity.Stop()
        $tracePath = $env:KOGNITION_E2E_TRACE_FILE
        if (-not $tracePath) {
            $tracePath = Join-Path $PSScriptRoot "../test-results/gateway-client-traces.jsonl"
        }
        $null = New-Item -ItemType Directory -Force -Path (Split-Path -Parent $tracePath)
        [ordered]@{
            name = $activity.OperationName
            traceId = $activity.TraceId.ToString()
            spanId = $activity.SpanId.ToString()
            startedAt = $activity.StartTimeUtc.ToString("o")
            durationMs = $activity.Duration.TotalMilliseconds
            method = $parameters.Method
            path = ([Uri]$parameters.Uri).AbsolutePath
            runId = $parameters.Headers['X-Kognition-Run-ID']
            statusCode = $statusCode
        } | ConvertTo-Json -Compress | Add-Content -Path $tracePath -Encoding utf8
        $activity.Dispose()
    }
}