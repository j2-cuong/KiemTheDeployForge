#requires -version 5.1

[CmdletBinding()]
param([switch]$IncludeIso)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$projectRoot = Split-Path -Parent $PSScriptRoot
$testRoot = Join-Path $projectRoot ('.smoke-' + [Guid]::NewGuid().ToString('N'))
$toolRoot = Join-Path $testRoot 'tool-project'
$toolSourceRoot = Join-Path $toolRoot 'src'
$toolBuilder = Join-Path $toolRoot 'dist\KiemTheDeployForge-Builder.exe'
$skipIso = -not $IncludeIso
$smokeMarkerName = '.kiemthedeployforge-smoke'
$smokeMarkerContent = "KiemTheDeployForge`r`nkind=smoke-test`r`n"
$smokeMutex = [System.Threading.Mutex]::new($false, 'Global\KiemTheDeployForge.SmokeTest')
$smokeMutexOwned = $false

function Write-AsciiFile {
    param([string]$Path, [string]$Content)
    [System.IO.Directory]::CreateDirectory((Split-Path -Parent $Path)) | Out-Null
    [System.IO.File]::WriteAllText($Path, $Content, [System.Text.Encoding]::ASCII)
}

function Remove-SmokeRoot {
    param([Parameter(Mandatory = $true)][string]$Path)

    $resolved = [System.IO.Path]::GetFullPath($Path)
    $prefix = [System.IO.Path]::GetFullPath($projectRoot).TrimEnd('\') + '\.smoke-'
    if (-not $resolved.StartsWith($prefix, [StringComparison]::OrdinalIgnoreCase)) {
        throw "Refusing to clean an unexpected smoke path: $resolved"
    }
    if (-not (Test-Path -LiteralPath $resolved)) {
        return
    }

    foreach ($iso in @(Get-ChildItem -LiteralPath $resolved -Recurse -Force -File -Filter '*.iso' -ErrorAction SilentlyContinue)) {
        Dismount-DiskImage -ImagePath $iso.FullName -ErrorAction SilentlyContinue | Out-Null
    }
    for ($attempt = 1; $attempt -le 12; $attempt++) {
        try {
            [System.IO.Directory]::Delete($resolved, $true)
            return
        }
        catch [System.IO.IOException], [System.UnauthorizedAccessException] {
            if ($attempt -eq 12) {
                throw
            }
            Start-Sleep -Milliseconds ([Math]::Min(2000, $attempt * 200))
        }
    }
}

function Test-OwnedSmokeRoot {
    param([Parameter(Mandatory = $true)][string]$Path)

    $marker = Join-Path $Path $smokeMarkerName
    $info = Get-Item -LiteralPath $marker -Force -ErrorAction SilentlyContinue
    if (-not $info -or $info.PSIsContainer -or $info.Length -ne $smokeMarkerContent.Length) {
        return $false
    }
    return [System.IO.File]::ReadAllText($marker) -eq $smokeMarkerContent
}

function Remove-AbandonedSmokeRoots {
    foreach ($directory in @(Get-ChildItem -LiteralPath $projectRoot -Directory -Force -Filter '.smoke-*' -ErrorAction Stop)) {
        if (Test-OwnedSmokeRoot -Path $directory.FullName) {
            Remove-SmokeRoot -Path $directory.FullName
        }
    }
}

try {
    try {
        $smokeMutexOwned = $smokeMutex.WaitOne(0)
    }
    catch [System.Threading.AbandonedMutexException] {
        $smokeMutexOwned = $true
    }
    if (-not $smokeMutexOwned) {
        throw 'Another KiemTheDeployForge smoke test is already running.'
    }
    Remove-AbandonedSmokeRoots
    [System.IO.Directory]::CreateDirectory($testRoot) | Out-Null
    [System.IO.File]::WriteAllText((Join-Path $testRoot $smokeMarkerName), $smokeMarkerContent, [System.Text.Encoding]::ASCII)
    [System.IO.Directory]::CreateDirectory($toolRoot) | Out-Null
    Copy-Item -LiteralPath (Join-Path $projectRoot 'src') -Destination $toolRoot -Recurse -Force
    Copy-Item -LiteralPath (Join-Path $projectRoot 'scripts') -Destination $toolRoot -Recurse -Force
    if (-not (Test-Path -LiteralPath (Join-Path $toolSourceRoot 'go.mod') -PathType Leaf)) { throw 'Hermetic smoke source copy is incomplete' }
    & (Join-Path $toolRoot 'scripts\Build-Tools.ps1') -OutputDirectory (Join-Path $toolRoot 'dist') | Out-Host
    if ($LASTEXITCODE -ne 0) { throw 'Build-Tools.ps1 failed' }
    if (-not (Test-Path -LiteralPath $toolBuilder -PathType Leaf)) { throw 'Hermetic smoke Builder was not produced' }

    $client = Join-Path $testRoot 'input\Client'
    $server = Join-Path $testRoot 'input\Server'
    $bot = Join-Path $testRoot 'input\Bot'
    $sql = Join-Path $testRoot 'input\jxaccount.sql'
    $output = Join-Path $testRoot 'output'
    [System.IO.Directory]::CreateDirectory($output) | Out-Null

    Write-AsciiFile (Join-Path $client 'Game.exe') 'fixture'
    Write-AsciiFile (Join-Path $client 'AutoPk\wjxtdAutoPro.exe') 'fixture'
    Write-AsciiFile (Join-Path $client 'user\uicommon.ini') "[Region_0]`r`n1_Address=192.168.1.10`r`n"
    Write-AsciiFile (Join-Path $client 'user\serverlistdebug.ini') "[Region_1]`r`n1_Address=192.168.1.10`r`n"
    Write-AsciiFile (Join-Path $client 'AutoPk\serverlist.ini') "[Region_0]`r`n0_Address=192.168.1.10`r`n"
    # The server's own start/stop batch files are the operator's business and
    # are deliberately not required by the Builder, so the fixture omits them.
    for ($index = 1; $index -le 9; $index++) {
        Write-AsciiFile (Join-Path $server "Gameserver\GS$index.exe") 'fixture'
        Write-AsciiFile (Join-Path $server "Gameserver\GS${index}servercfg.ini") "[GameServer]`r`nInIp=192.168.1.10`r`nOutIp=192.168.1.10`r`n"
    }
    Write-AsciiFile (Join-Path $bot 'loginprobe.exe') 'fixture'
    Write-AsciiFile (Join-Path $bot 'loginprobe.env') "BOT_DB_HOST=127.0.0.1`r`nBOT_DB_PORT=3306`r`nBOT_DB_USER=bot_writer`r`nBOT_DB_PASSWORD=1234`r`nBOT_DB_NAME=jxaccount`r`n#BOT_GAMESERVER_DIR=D:\fixture\Gameserver`r`n"
    Write-AsciiFile $sql "DROP TABLE IF EXISTS account;`r`nCREATE TABLE account (loginName varchar(32), password_hash varchar(32));`r`n"

    # The bot is optional, so the fixture exercises the path that packages one.
    # A release without a bot is covered by the manifest unit tests.
    $arguments = @(
        '--cli',
        '--client', $client,
        '--server', $server,
        '--bot', $bot,
        '--sql', $sql,
        '--output', $output
    )
    if ($skipIso) { $arguments += '--skip-iso' }
    # The Builder is linked as a GUI subsystem binary, so its exit code and its
    # diagnostics are only reliable through an explicitly awaited process with
    # redirected streams. This mirrors how the Setup plan step below is run.
    $builderStdout = Join-Path $testRoot 'builder.stdout.log'
    $builderStderr = Join-Path $testRoot 'builder.stderr.log'
    $builderProcess = Start-Process -FilePath $toolBuilder -ArgumentList $arguments -Wait -PassThru -WindowStyle Hidden `
        -RedirectStandardOutput $builderStdout -RedirectStandardError $builderStderr
    if ($builderProcess.ExitCode -ne 0) {
        $builderError = if (Test-Path -LiteralPath $builderStderr) { (Get-Content -Raw -LiteralPath $builderStderr).Trim() } else { '' }
        throw "Builder smoke run failed with exit code $($builderProcess.ExitCode): $builderError"
    }

    $setup = Join-Path $output 'Setup.exe'
    if ((Get-Item -LiteralPath $setup).Length -ge 268435456) {
        throw 'Setup.exe bootstrap unexpectedly contains the large payload'
    }
    $loosePayload = Join-Path $output 'Payload.ktpkg'
    if ($skipIso) {
        if (-not (Test-Path -LiteralPath $loosePayload -PathType Leaf)) {
            throw 'SkipIso output is missing the adjacent payload package'
        }
    }
    elseif (Test-Path -LiteralPath $loosePayload) {
        throw 'Release output leaked the temporary payload package outside the ISO'
    }
    if ($IncludeIso -and (Test-Path -LiteralPath (Join-Path $output 'README.txt'))) {
        throw 'Release output must contain only Setup.exe and KiemTheServer-Offline.iso'
    }
    $targetRoot = Join-Path $testRoot 'target\KiemTheServer'
    $setupArguments = @(
        '--cli-plan',
        '--setup', ('"' + $setup + '"'),
        '--install-root', ('"' + $targetRoot + '"'),
        '--verify'
    )
    $setupStdout = Join-Path $testRoot 'setup-plan.stdout.log'
    $setupStderr = Join-Path $testRoot 'setup-plan.stderr.log'
    $setupProcess = Start-Process -FilePath $setup -ArgumentList $setupArguments -Wait -PassThru -WindowStyle Hidden `
        -RedirectStandardOutput $setupStdout -RedirectStandardError $setupStderr
    if ($setupProcess.ExitCode -ne 0) {
        $setupError = if (Test-Path -LiteralPath $setupStderr) { [System.IO.File]::ReadAllText($setupStderr).Trim() } else { '' }
        $setupOutput = if (Test-Path -LiteralPath $setupStdout) { [System.IO.File]::ReadAllText($setupStdout).Trim() } else { '' }
        throw "Setup verification failed with exit code $($setupProcess.ExitCode). stderr=[$setupError] stdout=[$setupOutput]"
    }

    if ($IncludeIso) {
        $iso = Join-Path $output 'KiemTheServer-Offline.iso'
        $mounted = $null
        try {
            $mounted = Mount-DiskImage -ImagePath $iso -PassThru -ErrorAction Stop
            $volume = $mounted | Get-Volume | Select-Object -First 1
            if (-not $volume -or -not $volume.DriveLetter -or $volume.FileSystem -ne 'UDF') {
                throw 'Smoke ISO is not a readable UDF volume'
            }
            $names = @(Get-ChildItem -LiteralPath ($volume.DriveLetter + ':\') -Force | ForEach-Object Name | Sort-Object)
            $expected = @('manifests', 'Payload.ktpkg', 'README.txt', 'Setup.exe') | Sort-Object
            if ($names.Count -ne $expected.Count -or @(Compare-Object $names $expected).Count -ne 0) {
                throw "Unexpected ISO root entries: $($names -join ', ')"
            }
        }
        finally {
            if ($mounted) { Dismount-DiskImage -ImagePath $iso -ErrorAction SilentlyContinue | Out-Null }
        }
    }
    Write-Output 'SMOKE_TEST=PASS'
}
finally {
    try {
        Remove-SmokeRoot -Path $testRoot
    }
    finally {
        if ($smokeMutexOwned) { $smokeMutex.ReleaseMutex() }
        $smokeMutex.Dispose()
    }
}
