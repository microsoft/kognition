# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

param(
    [Parameter(Position = 0)]
    [string]$Action = "help",

    [Parameter(Position = 1)]
    [string]$Service
)

$ErrorActionPreference = "Stop"

function Show-Usage {
    Write-Host "Usage: services <start|stop|restart|log|status|list|down|help> [service-name]"
    Write-Host ""
    Write-Host "Examples:"
    Write-Host "  services start"
    Write-Host "  services start postgres"
    Write-Host "  services stop"
    Write-Host "  services log apiserver"
    Write-Host "  services status controller-manager"
    Write-Host "  services list"
}

function Invoke-Compose {
    param([string[]]$Cmd)

    & docker compose @Cmd
    if ($LASTEXITCODE -ne 0) {
        exit $LASTEXITCODE
    }
}

$repoRoot = Split-Path -Parent $MyInvocation.MyCommand.Path
Set-Location $repoRoot

try {
    & docker --version *> $null
} catch {
    Write-Error "Docker is required but was not found in PATH."
    exit 1
}

$normalized = $Action.ToLowerInvariant()

switch ($normalized) {
    "start" {
        if ($Service) {
            Invoke-Compose @("up", "-d", $Service)
        } else {
            Invoke-Compose @("up", "-d")
        }
    }

    "stop" {
        if ($Service) {
            Invoke-Compose @("stop", $Service)
        } else {
            Invoke-Compose @("stop")
        }
    }

    "restart" {
        if ($Service) {
            Invoke-Compose @("restart", $Service)
        } else {
            Invoke-Compose @("restart")
        }
    }

    "log" {
        if ($Service) {
            Invoke-Compose @("logs", "-f", $Service)
        } else {
            Invoke-Compose @("logs", "-f")
        }
    }

    "logs" {
        if ($Service) {
            Invoke-Compose @("logs", "-f", $Service)
        } else {
            Invoke-Compose @("logs", "-f")
        }
    }

    "status" {
        if ($Service) {
            Invoke-Compose @("ps", $Service)
        } else {
            Invoke-Compose @("ps")
        }
    }

    "list" {
        Invoke-Compose @("config", "--services")
    }

    "down" {
        if ($Service) {
            Write-Error "'down' does not support a single service. Use 'stop <service>' instead."
            exit 1
        }
        Invoke-Compose @("down")
    }

    "end" {
        if ($Service) {
            Write-Error "'end' does not support a single service. Use 'stop <service>' instead."
            exit 1
        }
        Invoke-Compose @("down")
    }

    "help" {
        Show-Usage
    }

    default {
        Write-Error "Unknown action '$Action'."
        Show-Usage
        exit 1
    }
}
