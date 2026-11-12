# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

[CmdletBinding()]
param([string]$OutputDirectory = '')

$ErrorActionPreference = 'Stop'
$root = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..'))
if (-not $OutputDirectory) { $OutputDirectory = $root }
$output = [IO.Path]::GetFullPath($OutputDirectory)
if (-not (Test-Path -LiteralPath $output -PathType Container)) {
    throw 'Output directory must exist'
}
$encoding = [Text.UTF8Encoding]::new($false)
$previousToolchain = $env:GOTOOLCHAIN
$goDirective = Select-String -LiteralPath (Join-Path $root 'go.mod') -Pattern '^go (\d+\.\d+\.\d+)$'
if (@($goDirective).Count -ne 1) { throw 'go.mod must pin a release toolchain version' }
$env:GOTOOLCHAIN = 'go' + $goDirective.Matches[0].Groups[1].Value
Push-Location $root
try {
    $rows = @(go list -mod=readonly -deps -test -f "{{if .Module}}{{.Module.Path}}`t{{.Module.Version}}`t{{.Module.Dir}}{{end}}" ./...)
    if ($LASTEXITCODE -ne 0) { throw 'Cannot resolve Go package and test dependencies' }
    $modules = @($rows | Where-Object { $_ } | Sort-Object -Unique | ConvertFrom-Csv -Delimiter "`t" -Header Path,Version,Directory)
    $notice = [Text.StringBuilder]::new()
    $null = $notice.AppendLine('Kognition Third-Party Notices')
    $null = $notice.AppendLine('')
    $null = $notice.AppendLine('This file preserves legal texts for Go package/test dependencies in this tree.')
    $null = $notice.AppendLine('It does not change their licenses or apply the first-party MIT license to them.')
    $null = $notice.AppendLine('')
    $null = $notice.AppendLine('SCOPE: Go dependency legal files, including nested files, and Go toolchain legal texts.')
    $null = $notice.AppendLine('The npm lockfile inventory and original legal texts are in third-party/npm/.')
    $null = $notice.AppendLine('Python runtime images generate /usr/share/kognition/python/NOTICE and inventory.json')
    $null = $notice.AppendLine('from their exact installed distributions after removing build-only packages.')
    $null = $notice.AppendLine('Completeness is NOT certified. Native libraries, Python wheels, optional npm packages,')
    $null = $notice.AppendLine('generated code, external documents, fonts/media and shipped image contents need')
    $null = $notice.AppendLine('distribution-specific component-governance review before publication.')
    $null = $notice.AppendLine('Distribution-specific review remains required; no release clearance is asserted.')
    $inventory = @()
    foreach ($module in $modules | Sort-Object Path) {
        if (-not $module.Version) { continue }
        $texts = @()
        if ($module.Directory -and (Test-Path -LiteralPath $module.Directory)) {
            $texts = @(Get-ChildItem -LiteralPath $module.Directory -File -Recurse | Where-Object {
                $_.Name -match '^(LICENSE|LICENCE|COPYING|COPYRIGHT|NOTICE)(\..*|-.*)?$' -and
                $_.Extension -notin @('.go', '.py', '.pyc', '.js', '.ts', '.json')
            } | Sort-Object FullName)
        }
        $null = $notice.AppendLine('')
        $null = $notice.AppendLine('================================================================================')
        $null = $notice.AppendLine("Component: $($module.Path)")
        $null = $notice.AppendLine("Version: $($module.Version)")
        $null = $notice.AppendLine("Source: https://pkg.go.dev/$($module.Path)@$($module.Version)")
        $files = @()
        foreach ($text in $texts) {
            $contents = [IO.File]::ReadAllText($text.FullName)
            $name = [IO.Path]::GetRelativePath($module.Directory, $text.FullName).Replace('\', '/')
            $null = $notice.AppendLine('')
            $null = $notice.AppendLine("--- Original $name ---")
            $null = $notice.AppendLine($contents)
            $files += [ordered]@{name=$name;sha256=(Get-FileHash -LiteralPath $text.FullName).Hash.ToLowerInvariant()}
        }
        if (-not $texts.Count) {
            $null = $notice.AppendLine('UNRESOLVED: no cached module-root legal text found; review required.')
        }
        $inventory += [ordered]@{component=$module.Path;version=$module.Version;texts=$files;status=$(if($texts.Count){'Text retained; distribution review pending'}else{'Unresolved'})}
    }
    $goRoot = go env GOROOT
    if ($LASTEXITCODE -ne 0) { throw 'Cannot locate Go toolchain legal files' }
    $goVersion = go env GOVERSION
    if ($LASTEXITCODE -ne 0) { throw 'Cannot identify Go toolchain' }
    $runtimeTexts = @()
    $null = $notice.AppendLine('')
    $null = $notice.AppendLine('================================================================================')
    $null = $notice.AppendLine("Component: Go toolchain and runtime ($goVersion)")
    foreach ($name in @('LICENSE', 'PATENTS')) {
        $path = Join-Path $goRoot $name
        if (-not (Test-Path -LiteralPath $path -PathType Leaf)) { throw "Go legal file missing: $name" }
        $null = $notice.AppendLine("--- Original $name ---")
        $null = $notice.AppendLine([IO.File]::ReadAllText($path))
        $runtimeTexts += [ordered]@{name=$name;sha256=(Get-FileHash -LiteralPath $path).Hash.ToLowerInvariant()}
    }
    [IO.File]::WriteAllText((Join-Path $output 'NOTICE'),$notice.ToString().Replace("`r`n", "`n"),$encoding)
    $report = [ordered]@{
        scope='Go package/test dependency legal files including nested files; not complete distribution clearance'
        sourceManifestSHA256=(Get-FileHash go.mod).Hash.ToLowerInvariant()
        modules=$inventory
        goToolchain=[ordered]@{version=$goVersion;texts=$runtimeTexts}
        excludedScopes=@('Python/native distributions; generate inside each exact image','npm packages; see third-party/npm','Container OS/base image contents','External documents and visual assets','Copied/adapted source provenance')
    }
    [IO.File]::WriteAllText((Join-Path $output 'go-notice-inventory.json'),($report | ConvertTo-Json -Depth 8).Replace("`r`n", "`n") + "`n",$encoding)
    @{modules=$inventory.Count;unresolved=@($inventory | Where-Object {$_.status -eq 'Unresolved'}).Count;output=$output;completeDistributionClearance=$false} | ConvertTo-Json
} finally {
    Pop-Location
    $env:GOTOOLCHAIN = $previousToolchain
}