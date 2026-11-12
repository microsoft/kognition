# Copyright (c) Microsoft Corporation.
# Licensed under the MIT License.

#Requires -Version 7.0
[CmdletBinding()]
param(
    [Parameter(Mandatory)][ValidatePattern('^[a-z0-9]{5,50}$')][string]$RegistryName,
    [ValidateSet('pgvector', 'busybox', 'jaeger', 'azurite', 'minio')]
    [string[]]$ImageName = @('pgvector', 'busybox', 'jaeger', 'azurite', 'minio'),
    [switch]$Import
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$PSNativeCommandUseErrorActionPreference = $false

$catalog = @{
    pgvector = @{
        Source = 'docker.io/pgvector/pgvector@sha256:ccc6e83d6e35e931dc7c5def2022729d5a6c370318d099181995567ff1fb4d6b'
        Repository = 'third-party/pgvector'
    }
    busybox = @{
        Source = 'docker.io/library/busybox@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662'
        Repository = 'third-party/busybox'
    }
    jaeger = @{
        Source = 'docker.io/jaegertracing/all-in-one@sha256:4fd2d70fa347d6a47e79fcb06b1c177e6079f92cba88b083153d56263082135e'
        Repository = 'third-party/jaegertracing/all-in-one'
    }
    azurite = @{
        Source = 'mcr.microsoft.com/azure-storage/azurite@sha256:830430c1da1a2d537e08f3e6764dd1f5ae00cf0346bcaf625b968ec3f0971fd5'
        Repository = 'third-party/azure-storage/azurite'
    }
    minio = @{
        Source = 'quay.io/minio/minio:RELEASE.2025-09-07T16-13-09Z'
        ImageId = 'sha256:14cea493d9a34af32f524e538b8346cf79f3321eff8e708c1e2960462bd8936e'
        BinaryDigest = '7c5bd8512c6e966455b1d198209358b2d191c77a83ab377c4073281065fb855f'
        DestinationDigest = 'sha256:a1a8bd4ac40ad7881a245bab97323e18f971e4d4cba2c2007ec1bedd21cbaba2'
        Repository = 'third-party/minio/minio'
    }
}

function Invoke-NativeJson([string]$Description, [scriptblock]$Command) {
    $output = & $Command
    if ($LASTEXITCODE -ne 0) { throw "$Description failed." }
    try { return ($output | Out-String | ConvertFrom-Json) }
    catch { throw "$Description returned invalid JSON." }
}

function Get-Platforms([object]$Manifest, [string]$Description) {
    $platforms = @(
        $Manifest.manifests |
            ForEach-Object { $_.platform } |
            Where-Object { $_ -and $_.os -and $_.architecture -and $_.os -ne 'unknown' -and $_.architecture -ne 'unknown' } |
            ForEach-Object {
                $platform = "$($_.os)/$($_.architecture)"
                $variantProperty = $_.PSObject.Properties['variant']
                if ($null -ne $variantProperty -and $variantProperty.Value) { $platform += "/$($variantProperty.Value)" }
                $platform
            } |
            Sort-Object -Unique
    )
    if ($platforms.Count -eq 0) { throw "$Description has no verifiable platform metadata." }
    return $platforms
}

foreach ($name in $ImageName) {
    $image = $catalog[$name]
    if ($name -eq 'minio') {
        $imageId = (& docker image inspect $image.Source --format '{{.Id}}' | Out-String).Trim()
        if ($LASTEXITCODE -ne 0) { throw 'Local MinIO image inspection failed.' }
        if ($imageId -ne $image.ImageId) {
            throw "Local MinIO image mismatch: expected $($image.ImageId), received $imageId."
        }
        $binaryOutput = (& docker run --rm --entrypoint sha256sum $image.Source /usr/bin/minio | Out-String).Trim()
        if ($LASTEXITCODE -ne 0) { throw 'Local MinIO binary verification failed.' }
        $binaryDigest = ($binaryOutput -split '\s+', 2)[0]
        if ($binaryDigest -ne $image.BinaryDigest) {
            throw "Local MinIO binary mismatch: expected $($image.BinaryDigest), received $binaryDigest."
        }

        $importTag = "source-$($image.ImageId.Substring(7, 12))"
        $destinationTag = "$RegistryName.azurecr.io/$($image.Repository):$importTag"
        if ($Import) {
            $null = & docker tag $image.Source $destinationTag 2>&1
            if ($LASTEXITCODE -ne 0) { throw 'ACR tag creation failed for minio.' }
            $null = & docker push $destinationTag 2>&1
            if ($LASTEXITCODE -ne 0) { throw 'ACR push failed for minio.' }
        }

        $destinationManifest = Invoke-NativeJson 'Destination inspection for minio' {
            docker buildx imagetools inspect $destinationTag --format '{{json .Manifest}}'
        }
        if ($image.DestinationDigest -and $destinationManifest.digest -ne $image.DestinationDigest) {
            throw "Destination digest mismatch for minio: expected $($image.DestinationDigest), received $($destinationManifest.digest)."
        }

        if ($Import) {
            & docker image rm $destinationTag | Out-Null
            if ($LASTEXITCODE -ne 0) { throw 'Local ACR tag removal failed for minio.' }
        }
        & docker pull $destinationTag | Out-Null
        if ($LASTEXITCODE -ne 0) { throw 'ACR round-trip pull failed for minio.' }
        $roundTripOutput = (& docker run --rm --entrypoint sha256sum $destinationTag /usr/bin/minio | Out-String).Trim()
        if ($LASTEXITCODE -ne 0) { throw 'ACR round-trip payload verification failed for minio.' }
        $roundTripDigest = ($roundTripOutput -split '\s+', 2)[0]
        if ($roundTripDigest -ne $image.BinaryDigest) {
            throw "ACR round-trip payload mismatch for minio: expected $($image.BinaryDigest), received $roundTripDigest."
        }
        $platform = (& docker image inspect $destinationTag --format '{{.Os}}/{{.Architecture}}' | Out-String).Trim()
        if ($LASTEXITCODE -ne 0 -or $platform -ne 'linux/amd64') {
            throw "ACR round-trip platform mismatch for minio: expected linux/amd64, received $platform."
        }

        [pscustomobject]@{
            Name = $name
            Source = $image.Source
            Destination = "$RegistryName.azurecr.io/$($image.Repository)@$($destinationManifest.digest)"
            Platforms = $platform
            Imported = [bool]$Import
        }
        continue
    }

    $expectedDigest = ($image.Source -split '@', 2)[1]
    $sourceManifest = Invoke-NativeJson "Source inspection for $name" {
        docker buildx imagetools inspect $image.Source --format '{{json .Manifest}}'
    }
    if ($sourceManifest.digest -ne $expectedDigest) {
        throw "Source digest mismatch for ${name}: expected $expectedDigest, received $($sourceManifest.digest)."
    }
    $sourcePlatforms = @(Get-Platforms $sourceManifest "Source manifest for $name")
    $importTag = "source-$($expectedDigest.Substring(7, 12))"

    if ($Import) {
        $null = & docker buildx imagetools create --tag "$RegistryName.azurecr.io/$($image.Repository):$importTag" $image.Source 2>&1
        if ($LASTEXITCODE -ne 0) { throw "ACR publication failed for $name." }
    }

    $destinationReference = "$RegistryName.azurecr.io/$($image.Repository):$importTag"
    $destinationManifest = Invoke-NativeJson "Destination inspection for $name" {
        docker buildx imagetools inspect $destinationReference --format '{{json .Manifest}}'
    }
    if ($destinationManifest.digest -ne $expectedDigest) {
        throw "Destination digest mismatch for ${name}: expected $expectedDigest, received $($destinationManifest.digest)."
    }
    $destinationPlatforms = @(Get-Platforms $destinationManifest "Destination manifest for $name")
    if (($sourcePlatforms -join ',') -ne ($destinationPlatforms -join ',')) {
        throw "Destination platforms mismatch for ${name}: expected $($sourcePlatforms -join ','), received $($destinationPlatforms -join ',')."
    }

    [pscustomobject]@{
        Name = $name
        Source = $image.Source
        Destination = "$RegistryName.azurecr.io/$($image.Repository)@$expectedDigest"
        Platforms = $sourcePlatforms -join ','
        Imported = [bool]$Import
    }
}