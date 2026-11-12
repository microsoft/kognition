# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

function ConvertTo-NativeProcessArgument {
    param([Parameter(Mandatory = $true)][AllowEmptyString()][string]$Argument)

    if ($Argument.Length -gt 0 -and $Argument -notmatch '[\s"]') {
        return $Argument
    }

    $escaped = [Text.StringBuilder]::new('"')
    $backslashes = 0
    foreach ($character in $Argument.ToCharArray()) {
        if ($character -eq '\') {
            $backslashes++
            continue
        }
        if ($character -eq '"') {
            [void]$escaped.Append(('\' * (($backslashes * 2) + 1)))
        }
        else {
            [void]$escaped.Append(('\' * $backslashes))
        }
        [void]$escaped.Append($character)
        $backslashes = 0
    }
    [void]$escaped.Append(('\' * ($backslashes * 2)))
    [void]$escaped.Append('"')
    return $escaped.ToString()
}

function Invoke-CapabilityMint {
    param(
        [Parameter(Mandatory = $true)][string]$KeyBase64,
        [Parameter(Mandatory = $true)][string]$FilePath,
        [Parameter(Mandatory = $true)][string[]]$ArgumentList
    )

    $startInfo = [Diagnostics.ProcessStartInfo]::new()
    $startInfo.FileName = $FilePath
    $startInfo.UseShellExecute = $false
    $startInfo.RedirectStandardInput = $true
    $startInfo.RedirectStandardOutput = $true
    $startInfo.RedirectStandardError = $true
    if ($null -ne $startInfo.PSObject.Properties['ArgumentList']) {
        foreach ($argument in $ArgumentList) {
            $startInfo.ArgumentList.Add($argument)
        }
    }
    else {
        $startInfo.Arguments = (($ArgumentList | ForEach-Object { ConvertTo-NativeProcessArgument $_ }) -join ' ')
    }

    $process = [Diagnostics.Process]::new()
    $process.StartInfo = $startInfo
    try {
        if (-not $process.Start()) {
            throw "Failed to start capability mint helper '$FilePath'"
        }
        $process.StandardInput.Write($KeyBase64)
        $process.StandardInput.Close()
        $stdout = $process.StandardOutput.ReadToEnd()
        $stderr = $process.StandardError.ReadToEnd()
        $process.WaitForExit()
        if ($process.ExitCode -ne 0) {
            throw "Capability mint helper '$FilePath' failed with exit code $($process.ExitCode): $($stderr.Trim())"
        }
        return $stdout.Trim()
    }
    finally {
        $process.Dispose()
    }
}