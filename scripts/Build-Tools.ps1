#requires -version 5.1

[CmdletBinding()]
param(
    [ValidatePattern('^[A-Za-z0-9._-]{1,64}$')]
    [string]$Version = 'dev',
    [string]$OutputDirectory,
    [switch]$ValidateSource
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
$sourceRoot = Join-Path $projectRoot 'src'
$assetRoot = Join-Path $sourceRoot 'cmd\builder\assets'
if (-not $OutputDirectory) {
    $OutputDirectory = Join-Path $projectRoot 'dist'
}
$OutputDirectory = [System.IO.Path]::GetFullPath($OutputDirectory)
[System.IO.Directory]::CreateDirectory($assetRoot) | Out-Null
[System.IO.Directory]::CreateDirectory($OutputDirectory) | Out-Null

$setupAsset = Join-Path $assetRoot 'SetupStub.exe'
$builderOutput = Join-Path $OutputDirectory 'KiemTheDeployForge-Builder.exe'
$setupTemp = Join-Path $assetRoot ('.SetupStub-' + [Guid]::NewGuid().ToString('N') + '.exe')
$builderTemp = Join-Path $OutputDirectory ('.Builder-' + [Guid]::NewGuid().ToString('N') + '.exe')
$overlayTemp = Join-Path $assetRoot ('.BuilderOverlay-' + [Guid]::NewGuid().ToString('N') + '.json')
$managedGoEnvironment = @('CGO_ENABLED', 'GOOS', 'GOARCH', 'GOAMD64', 'GOFIPS140', 'GOFLAGS', 'GOENV', 'GOTOOLCHAIN', 'GOWORK', 'GOEXPERIMENT')
$oldGoEnvironment = @{}
foreach ($name in $managedGoEnvironment) {
    $oldGoEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
}
$buildMutex = [System.Threading.Mutex]::new($false, 'Global\KiemTheDeployForge.BuildTools')
$buildMutexOwned = $false
$setupPublish = $null
$builderPublish = $null
$publishCommitted = $false

function Remove-StaleBuildArtifacts {
    param(
        [Parameter(Mandatory = $true)][string]$Directory,
        [Parameter(Mandatory = $true)][string[]]$Patterns
    )

    foreach ($pattern in $Patterns) {
        foreach ($artifact in @(Get-ChildItem -LiteralPath $Directory -Filter $pattern -File -Force -ErrorAction Stop)) {
            [System.IO.File]::Delete($artifact.FullName)
        }
    }
}

function Publish-Binary {
    param(
        [Parameter(Mandatory = $true)][string]$Source,
        [Parameter(Mandatory = $true)][string]$Destination,
        [Parameter(Mandatory = $true)][string]$Backup
    )

    $sourceInfo = Get-Item -LiteralPath $Source -ErrorAction Stop
    if ($sourceInfo.Length -le 0) {
        throw "Refusing to publish an empty binary: $Source"
    }

    if (Test-Path -LiteralPath $Destination -PathType Leaf) {
        $destinationFullPath = [System.IO.Path]::GetFullPath($Destination)
        $processName = [System.IO.Path]::GetFileNameWithoutExtension($destinationFullPath)
        foreach ($process in @(Get-Process -Name $processName -ErrorAction SilentlyContinue)) {
            try { $processPath = $process.Path } catch { $processPath = $null }
            if ($processPath -and [string]::Equals([System.IO.Path]::GetFullPath($processPath), $destinationFullPath, [StringComparison]::OrdinalIgnoreCase)) {
                throw "Cannot publish $Destination while process $($process.Id) is using it. Wait for the active package build to finish."
            }
        }

        [System.IO.File]::Replace($Source, $Destination, $Backup, $true)
        return [pscustomobject]@{ Destination = $Destination; Backup = $Backup; Created = $false }
    }
    [System.IO.File]::Move($Source, $Destination)
    return [pscustomobject]@{ Destination = $Destination; Backup = ''; Created = $true }
}

function Remove-PublishBackup {
    param([string]$Path)

    if (-not $Path) { return }
    for ($attempt = 1; (Test-Path -LiteralPath $Path) -and $attempt -le 20; $attempt++) {
        try {
            [System.IO.File]::Delete($Path)
        }
        catch [System.IO.IOException], [System.UnauthorizedAccessException] {
            if ($attempt -eq 20) { throw }
            Start-Sleep -Milliseconds ([Math]::Min(1000, $attempt * 100))
        }
    }
}

function Restore-PublishedBinary {
    param([psobject]$Publication)

    if ($null -eq $Publication) { return }
    if ($Publication.Created) {
        if (Test-Path -LiteralPath $Publication.Destination -PathType Leaf) {
            [System.IO.File]::Delete($Publication.Destination)
        }
        return
    }
    if (-not (Test-Path -LiteralPath $Publication.Backup -PathType Leaf)) {
        throw "Cannot roll back published binary because its backup is missing: $($Publication.Destination)"
    }
    if (Test-Path -LiteralPath $Publication.Destination -PathType Leaf) {
        $discard = Join-Path (Split-Path -Parent $Publication.Destination) ('.publish-backup-rollback-' + [Guid]::NewGuid().ToString('N'))
        try {
            [System.IO.File]::Replace($Publication.Backup, $Publication.Destination, $discard, $true)
        }
        finally {
            Remove-PublishBackup -Path $discard
        }
        return
    }
    [System.IO.File]::Move($Publication.Backup, $Publication.Destination)
}

function Assert-Amd64Pe {
    param([Parameter(Mandatory = $true)][string]$Path)

    $stream = [System.IO.File]::Open($Path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::Read)
    try {
        $reader = [System.IO.BinaryReader]::new($stream)
        try {
            if ($reader.ReadUInt16() -ne 0x5A4D) {
                throw "Build output is not a PE executable: $Path"
            }
            $stream.Position = 0x3C
            $peOffset = $reader.ReadInt32()
            if ($peOffset -lt 0x40 -or $peOffset -gt ($stream.Length - 6)) {
                throw "Build output has an invalid PE header: $Path"
            }
            $stream.Position = $peOffset
            if ($reader.ReadUInt32() -ne 0x00004550 -or $reader.ReadUInt16() -ne 0x8664) {
                throw "Build output is not Windows x64 (PE machine 0x8664): $Path"
            }
        }
        finally {
            $reader.Dispose()
        }
    }
    finally {
        $stream.Dispose()
    }
}

try {
    try {
        $buildMutexOwned = $buildMutex.WaitOne(0)
    }
    catch [System.Threading.AbandonedMutexException] {
        $buildMutexOwned = $true
    }
    if (-not $buildMutexOwned) {
        throw 'Another KiemTheDeployForge tool build is already running.'
    }

    # A hard-killed PowerShell process cannot execute its finally block. Once
    # the process mutex is ours, these exact private names cannot belong to a
    # live Build-Tools invocation and are safe to reclaim before the next build.
    Remove-StaleBuildArtifacts -Directory $assetRoot -Patterns @('.SetupStub-*.exe', '.BuilderOverlay-*.json', '.publish-backup-*')
    Remove-StaleBuildArtifacts -Directory $OutputDirectory -Patterns @('.Builder-*.exe', '.publish-backup-*')

    [Environment]::SetEnvironmentVariable('CGO_ENABLED', '0', 'Process')
    [Environment]::SetEnvironmentVariable('GOOS', 'windows', 'Process')
    [Environment]::SetEnvironmentVariable('GOARCH', 'amd64', 'Process')
    [Environment]::SetEnvironmentVariable('GOAMD64', 'v1', 'Process')
    [Environment]::SetEnvironmentVariable('GOFIPS140', 'off', 'Process')
    [Environment]::SetEnvironmentVariable('GOFLAGS', '', 'Process')
    [Environment]::SetEnvironmentVariable('GOENV', 'off', 'Process')
    [Environment]::SetEnvironmentVariable('GOTOOLCHAIN', 'local', 'Process')
    [Environment]::SetEnvironmentVariable('GOWORK', 'off', 'Process')
    [Environment]::SetEnvironmentVariable('GOEXPERIMENT', '', 'Process')
    $goExecutable = (Get-Command go -CommandType Application -ErrorAction Stop | Select-Object -First 1).Source
    $gofmtExecutable = Join-Path (Split-Path -Parent $goExecutable) 'gofmt.exe'
    if (-not (Test-Path -LiteralPath $gofmtExecutable -PathType Leaf)) {
        throw "gofmt.exe was not found beside the selected Go executable: $goExecutable"
    }
    Push-Location $sourceRoot
    try {
        if ($ValidateSource) {
            $unformatted = @(& $gofmtExecutable -l .)
            if ($LASTEXITCODE -ne 0) { throw 'gofmt check failed' }
            if ($unformatted.Count -gt 0) {
                throw "Source is not gofmt-clean: $($unformatted -join ', ')"
            }
            & $goExecutable test -mod=readonly ./...
            if ($LASTEXITCODE -ne 0) { throw 'go test failed' }
            & $goExecutable vet -mod=readonly ./...
            if ($LASTEXITCODE -ne 0) { throw 'go vet failed' }
        }

        # Both GUIs need an embedded application manifest that pulls in
        # Common-Controls 6.0, otherwise walk panics on TTM_ADDTOOL before the
        # main window appears. The resources are regenerated from the manifests
        # on every build so the shipped binaries can never drift from them.
        foreach ($resource in @(
                @{ Manifest = '.\cmd\builder\Builder.manifest'; Syso = '.\cmd\builder\rsrc_windows_amd64.syso' },
                @{ Manifest = '.\cmd\setup\Setup.manifest'; Syso = '.\cmd\setup\rsrc_windows_amd64.syso' })) {
            & $goExecutable run -mod=readonly .\cmd\genrsrc -manifest $resource.Manifest -out $resource.Syso | Out-Host
            if ($LASTEXITCODE -ne 0) { throw "Resource generation failed for $($resource.Manifest)" }
            if (-not (Test-Path -LiteralPath $resource.Syso -PathType Leaf)) {
                throw "Resource object was not produced: $($resource.Syso)"
            }
        }

        try {
            & $goExecutable build -mod=readonly -trimpath -buildvcs=false -ldflags "-s -w -H windowsgui -X main.buildVersion=$Version" -o $setupTemp .\cmd\setup
            if ($LASTEXITCODE -ne 0) { throw "Setup stub build failed with exit code $LASTEXITCODE" }
            Assert-Amd64Pe -Path $setupTemp

            $overlayReplace = @{}
            $overlayReplace[[System.IO.Path]::GetFullPath($setupAsset)] = [System.IO.Path]::GetFullPath($setupTemp)
            $overlay = @{ Replace = $overlayReplace }
            [System.IO.File]::WriteAllText($overlayTemp, ($overlay | ConvertTo-Json -Compress), [System.Text.UTF8Encoding]::new($false))

            & $goExecutable build -mod=readonly -trimpath -buildvcs=false -overlay $overlayTemp -ldflags '-s -w -H windowsgui' -o $builderTemp .\cmd\builder
            if ($LASTEXITCODE -ne 0) { throw "Builder build failed with exit code $LASTEXITCODE" }
            Assert-Amd64Pe -Path $builderTemp
            & (Join-Path $PSScriptRoot 'Audit-Build.ps1') -ArtifactPath @($setupTemp, $builderTemp) -GoExecutable $goExecutable | Out-Host

            $builderBackup = Join-Path $OutputDirectory ('.publish-backup-' + [Guid]::NewGuid().ToString('N'))
            $builderPublish = Publish-Binary -Source $builderTemp -Destination $builderOutput -Backup $builderBackup
            $setupBackup = Join-Path $assetRoot ('.publish-backup-' + [Guid]::NewGuid().ToString('N'))
            $setupPublish = Publish-Binary -Source $setupTemp -Destination $setupAsset -Backup $setupBackup
            $publishCommitted = $true
        }
        catch {
            $operationError = $_.Exception
            $rollbackErrors = @()
            if (-not $publishCommitted) {
                try { Restore-PublishedBinary -Publication $builderPublish } catch { $rollbackErrors += $_.Exception.Message }
                try { Restore-PublishedBinary -Publication $setupPublish } catch { $rollbackErrors += $_.Exception.Message }
            }
            if ($rollbackErrors.Count -gt 0) {
                throw "Tool build failed: $($operationError.Message). Rollback also failed: $($rollbackErrors -join ' | ')"
            }
            throw $operationError
        }
        Remove-PublishBackup -Path $setupPublish.Backup
        Remove-PublishBackup -Path $builderPublish.Backup
    }
    finally {
        Pop-Location
    }
}
finally {
    foreach ($name in $managedGoEnvironment) {
        [Environment]::SetEnvironmentVariable($name, $oldGoEnvironment[$name], 'Process')
    }
    if (Test-Path -LiteralPath $setupTemp) { Remove-Item -LiteralPath $setupTemp -Force }
    if (Test-Path -LiteralPath $builderTemp) { Remove-Item -LiteralPath $builderTemp -Force }
    if (Test-Path -LiteralPath $overlayTemp) { Remove-Item -LiteralPath $overlayTemp -Force }
    if ($buildMutexOwned) { $buildMutex.ReleaseMutex() }
    $buildMutex.Dispose()
}

$setupHash = (Get-FileHash -LiteralPath $setupAsset -Algorithm SHA256).Hash.ToLowerInvariant()
$builderHash = (Get-FileHash -LiteralPath $builderOutput -Algorithm SHA256).Hash.ToLowerInvariant()
Write-Output "SETUP_STUB=$setupAsset"
Write-Output "SETUP_STUB_SHA256=$setupHash"
Write-Output "BUILDER=$builderOutput"
Write-Output "BUILDER_SHA256=$builderHash"
