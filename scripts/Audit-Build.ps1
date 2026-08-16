#requires -version 5.1

[CmdletBinding()]
param(
    [string[]]$ArtifactPath,
    [string]$GoExecutable
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
if (-not $ArtifactPath -or $ArtifactPath.Count -eq 0) {
    throw 'At least one release artifact is required for audit.'
}
$projectRoot = Split-Path -Parent $PSScriptRoot
$sourceRoot = Join-Path $projectRoot 'src'

function Get-PeMachine {
    param([Parameter(Mandatory = $true)][string]$Path)

    $stream = [System.IO.File]::Open($Path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::Read, [System.IO.FileShare]::Read)
    try {
        $reader = [System.IO.BinaryReader]::new($stream)
        try {
            if ($reader.ReadUInt16() -ne 0x5A4D) {
                throw "Artifact is not a PE executable: $Path"
            }
            $stream.Position = 0x3C
            $peOffset = $reader.ReadInt32()
            if ($peOffset -lt 0x40 -or $peOffset -gt ($stream.Length - 6)) {
                throw "Artifact has an invalid PE header offset: $Path"
            }
            $stream.Position = $peOffset
            if ($reader.ReadUInt32() -ne 0x00004550) {
                throw "Artifact has an invalid PE signature: $Path"
            }
            return $reader.ReadUInt16()
        }
        finally {
            $reader.Dispose()
        }
    }
    finally {
        $stream.Dispose()
    }
}

# Setup now deploys the bot directory, creates its MySQL account and rewrites
# its env file, so the bare word "bot" is legitimate release vocabulary. Only
# the bot's internal scope must still stay out of Builder and Setup: its AI,
# platform, runtime, licensing and activation material.
$forbiddenArtifactScopePatterns = [ordered]@{
    'bot-internals' = '(?i)(?<![a-z0-9])bot[-_ ](?:ai|chat|key|licen[cs]e|platform|runtime|behaviou?r|controller|manager|logic|fsm|combat|quota|activation)(?![a-z0-9])'
    'license' = '(?i)licen[cs]'
    'src-bot' = '(?i)(?<![a-z0-9])src[-_ ]+bot(?![a-z0-9])'
    'key-activation' = '(?i)(?<![a-z0-9])key[-_ ]*activation(?![a-z0-9])'
    'phase8' = '(?i)(?<![a-z0-9])phase[-_ ]*8(?![a-z0-9])'
    'phase9' = '(?i)(?<![a-z0-9])phase[-_ ]*9(?![a-z0-9])'
}

function Assert-ArtifactScope {
    param([Parameter(Mandatory = $true)][string]$Path)

    $bytes = [System.IO.File]::ReadAllBytes($Path)
    $views = [System.Collections.Generic.List[string]]::new()
    $views.Add([System.Text.Encoding]::ASCII.GetString($bytes))

    $evenByteCount = $bytes.Length - ($bytes.Length % 2)
    if ($evenByteCount -gt 0) {
        $views.Add([System.Text.Encoding]::Unicode.GetString($bytes, 0, $evenByteCount))
    }
    $oddByteCount = $bytes.Length - 1
    $oddByteCount -= $oddByteCount % 2
    if ($oddByteCount -gt 0) {
        $views.Add([System.Text.Encoding]::Unicode.GetString($bytes, 1, $oddByteCount))
    }

    foreach ($entry in $forbiddenArtifactScopePatterns.GetEnumerator()) {
        foreach ($view in $views) {
            if ([System.Text.RegularExpressions.Regex]::IsMatch($view, $entry.Value, [System.Text.RegularExpressions.RegexOptions]::CultureInvariant)) {
                throw "Forbidden scope marker '$($entry.Key)' found in release artifact: $Path"
            }
        }
    }
}

$goExecutable = if ($GoExecutable) {
    (Resolve-Path -LiteralPath $GoExecutable -ErrorAction Stop).Path
}
else {
    (Get-Command go -CommandType Application -ErrorAction Stop | Select-Object -First 1).Source
}
if (-not (Test-Path -LiteralPath $goExecutable -PathType Leaf)) {
    throw "Go executable is not a regular file: $goExecutable"
}
$goFlags = (& $goExecutable env GOFLAGS).Trim()
if ($goFlags) {
    throw "GOFLAGS must be empty for a reproducible release build: $goFlags"
}
# Go 1.25 echoes the literal 'off'; Go 1.26 reports an empty value for a
# disabled env file and the resolved config path otherwise. Both spellings mean
# the same thing: no per-user go env file can influence this build.
$goEnv = (& $goExecutable env GOENV).Trim()
if ($goEnv -ne 'off' -and $goEnv -ne '') {
    throw "GOENV must be off for a release build: $goEnv"
}
$goToolchain = (& $goExecutable env GOTOOLCHAIN).Trim()
if ($goToolchain -ne 'local') {
    throw "GOTOOLCHAIN must be local for a release build: $goToolchain"
}
$goWork = (& $goExecutable env GOWORK).Trim()
if ($goWork -ne 'off') {
    throw "GOWORK must be off for a release build: $goWork"
}
$goExperiment = [Environment]::GetEnvironmentVariable('GOEXPERIMENT', 'Process')
if ($goExperiment) {
    throw "GOEXPERIMENT must be empty for a release build: $goExperiment"
}
$cgoEnabled = (& $goExecutable env CGO_ENABLED).Trim()
$goOS = (& $goExecutable env GOOS).Trim()
$goArch = (& $goExecutable env GOARCH).Trim()
$goAMD64 = (& $goExecutable env GOAMD64).Trim()
$goFIPS140 = (& $goExecutable env GOFIPS140).Trim()
if ($cgoEnabled -ne '0' -or $goOS -ne 'windows' -or $goArch -ne 'amd64' -or $goAMD64 -ne 'v1' -or $goFIPS140 -ne 'off') {
    throw "Release target must be CGO_ENABLED=0 GOOS=windows GOARCH=amd64 GOAMD64=v1 GOFIPS140=off; got CGO_ENABLED=$cgoEnabled GOOS=$goOS GOARCH=$goArch GOAMD64=$goAMD64 GOFIPS140=$goFIPS140"
}
$goVersion = (& $goExecutable version).Trim()

$sensitivePatterns = @(
    '-----BEGIN (RSA |EC |OPENSSH |)PRIVATE KEY-----',
    '(?i)AWS_SECRET_ACCESS_KEY\s*=',
    '(?i)CLIENT_SECRET\s*=',
    '(?i)PRIVATE_KEY\s*='
)
$sourceFiles = Get-ChildItem -LiteralPath $projectRoot -Recurse -File -Force |
    Where-Object { $_.Extension -in @('.go', '.ps1', '.md', '.json', '.yaml', '.yml', '.env', '.pem', '.key') }
foreach ($file in $sourceFiles) {
    if ($file.FullName -eq $PSCommandPath) { continue }
    $text = [System.IO.File]::ReadAllText($file.FullName)
    foreach ($pattern in $sensitivePatterns) {
        if ($text -match $pattern) {
            throw "Sensitive material pattern found in $($file.FullName)"
        }
    }
}

$keyLikeFiles = Get-ChildItem -LiteralPath $projectRoot -Recurse -File -Force |
    Where-Object { $_.Extension -in @('.pem', '.pfx', '.p12', '.key') }
if ($keyLikeFiles) {
    throw "Private-key-like file found in project: $($keyLikeFiles[0].FullName)"
}

Push-Location $sourceRoot
try {
    & $goExecutable list -mod=readonly -m all | Out-Null
    if ($LASTEXITCODE -ne 0) { throw 'go list -m all failed' }
    $modulePath = (& $goExecutable env GOMOD).Trim()
}
finally {
    Pop-Location
}

foreach ($artifact in $ArtifactPath) {
    $resolved = (Resolve-Path -LiteralPath $artifact).Path
    $machine = Get-PeMachine -Path $resolved
    if ($machine -ne 0x8664) {
        throw ('Artifact is not Windows amd64 (PE machine 0x{0:x4}): {1}' -f $machine, $resolved)
    }
    & $goExecutable version -m $resolved | Out-Null
    if ($LASTEXITCODE -ne 0) { throw "go version -m failed for $resolved" }
    Assert-ArtifactScope -Path $resolved
    Write-Output "PE_AMD64=$resolved"
    Write-Output "SCOPE_GATE=$resolved"
}

Write-Output 'AUDIT=PASS'
Write-Output "GOFLAGS=$goFlags"
Write-Output "GOENV=$goEnv"
Write-Output "GOTOOLCHAIN=$goToolchain"
Write-Output "GOWORK=$goWork"
Write-Output "GOAMD64=$goAMD64"
Write-Output "GOFIPS140=$goFIPS140"
Write-Output "GO_EXECUTABLE=$goExecutable"
Write-Output "GO_VERSION=$goVersion"
Write-Output "MODULE=$modulePath"
