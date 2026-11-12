# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

[CmdletBinding()]
param(
    [ValidateSet('Check', 'Apply')][string]$Mode = 'Check',
    [string[]]$Paths = @()
)

$ErrorActionPreference = 'Stop'

function Add-SourceCopyrightHeader {
    param([string]$Content, [string]$Extension)

    $copyright = 'Copyright (c) Microsoft Corporation.'
    $license = 'Licensed under the MIT License.'
    $beginning = ($Content -split "`n" | Select-Object -First 25) -join "`n"
    if ($beginning.Contains($copyright) -and $beginning.Contains($license)) {
        return $Content
    }
    if ($beginning -match '(?i)Copyright|SPDX-License-Identifier|Licensed under|Apache License') {
        throw 'Existing legal notice requires ownership review; it will not be replaced'
    }

    $newline = if ($Content.Contains("`r`n")) { "`r`n" } else { "`n" }
    $prefix = switch ($Extension) {
        { $_ -in @('.go', '.js', '.ts', '.proto', '.bicep') } { '//'; break }
        { $_ -in @('.py', '.ps1', '.sh', '.yaml', '.yml', '.dockerfile', '.makefile', '.requirements', '.ini', '.env', '.gitignore', '.gitattributes', '.dockerignore') } { '#'; break }
        '.cmd' { '@REM'; break }
        '.sql' { '--'; break }
        '.css' { '/*'; break }
        { $_ -in @('.html', '.svg') } { '<!--'; break }
        { $_ -in @('.tpl', '.helmtxt') } { '{{/*'; break }
        default { throw 'Unsupported source type; no header added' }
    }
    $header = if ($prefix -eq '/*') {
        "/* $copyright$newline * $license$newline */$newline$newline"
    } elseif ($prefix -eq '<!--') {
        "<!-- $copyright$newline     $license -->$newline$newline"
    } elseif ($prefix -eq '{{/*') {
        "{{/* $copyright$newline     $license */}}$newline$newline"
    } else {
        "$prefix $copyright$newline$prefix $license$newline$newline"
    }

    $offset = 0
    if ($Content.StartsWith([string][char]0xfeff, [StringComparison]::Ordinal)) { $offset = 1 }
    $remaining = $Content.Substring($offset)
    if ($remaining.StartsWith('#!', [StringComparison]::Ordinal)) {
        $end = $Content.IndexOf("`n", $offset)
        if ($end -lt 0) { throw 'Shebang has no line terminator' }
        $offset = $end + 1
    }
    if ($Extension -eq '.py') {
        $remaining = $Content.Substring($offset)
        if ($remaining -match '^#[^\r\n]*coding[:=][^\r\n]*(\r?\n)') {
            $offset += $Matches[0].Length
        }
    }
    if ($Extension -eq '.dockerfile' -and $Content.Substring($offset) -match '^#\s*syntax=[^\r\n]*(\r?\n)') {
        $offset += $Matches[0].Length
    }
    if ($Extension -eq '.svg' -and $Content.Substring($offset) -match '^<\?xml[^\r\n]*\?>(\r?\n)') {
        $offset += $Matches[0].Length
    }
    return $Content.Insert($offset, $header)
}

$root = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
$encoding = [Text.UTF8Encoding]::new($false, $true)
Push-Location $root
try {
    $tracked = @(git -c core.quotepath=false ls-files --cached --others --exclude-standard | Sort-Object -Unique)
    if ($LASTEXITCODE -ne 0) { throw 'Cannot enumerate source files' }
    $supported = @('.go', '.py', '.js', '.ts', '.ps1', '.sh', '.sql', '.proto', '.css', '.yaml', '.yml', '.cmd', '.html', '.svg', '.tpl', '.bicep', '.ini', '.gitignore', '.gitattributes', '.dockerignore')
    $sources = @($tracked | Where-Object {
        ([IO.Path]::GetExtension($_) -in $supported -or
            [IO.Path]::GetFileName($_) -match '^(Dockerfile($|\.)|.*\.Dockerfile$|Makefile$|services$|requirements[^/]*\.txt$)' -or
            $_ -match '/templates/NOTES\.txt$|\.env\.example$|^\.githooks/(pre-commit|commit-msg)$|^tools/trace$') -and
        $_ -notmatch '^(vendor|third_party|node_modules)/' -and
        (Test-Path -LiteralPath $_ -PathType Leaf)
    })
    if ($Paths.Count) {
        foreach ($path in $Paths) {
            if ($path -notin $sources) { throw "Path is not a first-party source: $path" }
        }
        $sources = @($Paths)
    }

    $pending = @()
    $blocked = @()
    foreach ($source in $sources) {
        $fullPath = Join-Path $root $source
        $bytes = [IO.File]::ReadAllBytes($fullPath)
        $content = $encoding.GetString($bytes)
        try {
            $extension = [IO.Path]::GetExtension($source)
            if ([IO.Path]::GetFileName($source) -match '^(Dockerfile($|\.)|.*\.Dockerfile$)') { $extension = '.dockerfile' }
            if ([IO.Path]::GetFileName($source) -eq 'Makefile') { $extension = '.makefile' }
            if ([IO.Path]::GetFileName($source) -eq 'services') { $extension = '.sh' }
            if ([IO.Path]::GetFileName($source) -match '^requirements.*\.txt$') { $extension = '.requirements' }
            if ($source -match '/templates/NOTES\.txt$') { $extension = '.helmtxt' }
            if ($source -match '\.env\.example$') { $extension = '.env' }
            if ($source -match '^\.githooks/(pre-commit|commit-msg)$|^tools/trace$') { $extension = '.sh' }
            $updated = Add-SourceCopyrightHeader $content $extension
            if ($updated -ne $content) {
                $pending += [pscustomobject]@{Path=$source;Original=$bytes;Updated=$encoding.GetBytes($updated)}
            }
        } catch {
            $blocked += $source
        }
    }
    if ($blocked.Count) {
        @{mode=$Mode;blocked=$blocked;reason='Existing notices preserved; review ownership before any bulk changes'} | ConvertTo-Json
        exit 1
    }
    if ($Mode -eq 'Apply') {
        foreach ($entry in $pending) {
            $fullPath = Join-Path $root $entry.Path
            $current = [Convert]::ToBase64String([IO.File]::ReadAllBytes($fullPath))
            if ($current -ne [Convert]::ToBase64String($entry.Original)) {
                throw "Source changed after review; no overwrite: $($entry.Path)"
            }
            [IO.File]::WriteAllBytes($fullPath, $entry.Updated)
            if ([Convert]::ToBase64String([IO.File]::ReadAllBytes($fullPath)) -ne [Convert]::ToBase64String($entry.Updated)) {
                throw "Header write verification failed: $($entry.Path)"
            }
        }
    }
    @{mode=$Mode;sourceFiles=$sources.Count;missingHeaders=$pending.Count;modified=$(if($Mode -eq 'Apply'){$pending.Count}else{0});scope='Tracked and nonignored first-party code; external dependencies and notices unchanged'} | ConvertTo-Json
    if ($Mode -eq 'Check' -and $pending.Count) { exit 1 }
} finally {
    Pop-Location
}