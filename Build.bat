@echo off
setlocal EnableExtensions DisableDelayedExpansion
chcp 65001 >nul

REM Build KiemTheDeployForge-Builder.exe from the Go sources.
REM Requires only a Go installation; no MinGW, windres or other extra tooling.
REM
REM   Build.bat            build and publish dist\KiemTheDeployForge-Builder.exe
REM   Build.bat check      also run gofmt, go vet and go test before building

set "PROJECT_ROOT=%~dp0"
if "%PROJECT_ROOT:~-1%"=="\" set "PROJECT_ROOT=%PROJECT_ROOT:~0,-1%"

echo ============================================================
echo  KIEM THE DEPLOY FORGE - BUILD
echo ============================================================
echo.

where go >nul 2>&1
if errorlevel 1 (
    echo [ERROR] Go was not found on PATH.
    echo         Install Go from https://go.dev/dl/ then reopen this window.
    goto :failed
)
for /f "delims=" %%v in ('go version') do echo [1/3] Go toolchain : %%v

if not defined SystemRoot (
    echo [ERROR] SystemRoot is unavailable; trusted Windows PowerShell cannot be resolved.
    goto :failed
)
set "POWERSHELL_EXE=%SystemRoot%\System32\WindowsPowerShell\v1.0\powershell.exe"
if not exist "%POWERSHELL_EXE%" (
    echo [ERROR] Trusted Windows PowerShell was not found: "%POWERSHELL_EXE%"
    goto :failed
)

set "VALIDATE="
if /i "%~1"=="check" set "VALIDATE=-ValidateSource"
if defined VALIDATE (
    echo [2/3] Mode         : full check ^(gofmt + go vet + go test^) then build
) else (
    echo [2/3] Mode         : build only ^(run "Build.bat check" to also run the tests^)
)
echo [3/3] Building Setup.exe stub and the Builder, then auditing both...
echo.

REM Build-Tools.ps1 regenerates the GUI manifest resources, builds Setup and the
REM Builder as 64-bit binaries, verifies the PE headers, runs Audit-Build.ps1 and
REM only then publishes over dist\KiemTheDeployForge-Builder.exe.
"%POWERSHELL_EXE%" -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "%PROJECT_ROOT%\scripts\Build-Tools.ps1" %VALIDATE%
if errorlevel 1 goto :failed

echo.
echo ============================================================
echo  BUILD OK
echo ============================================================
echo.
echo  Builder : %PROJECT_ROOT%\dist\KiemTheDeployForge-Builder.exe
echo.
echo  Open the Builder, pick the Client, Server and Bot directories plus
echo  jxaccount.sql and an output directory, then press "TAO SETUP + ISO".
echo  Progress is reported as a percentage for every stage, including the
echo  ISO write and its verification pass.
echo.
pause
exit /b 0

:failed
echo.
echo ============================================================
echo  BUILD FAILED
echo ============================================================
echo.
pause
exit /b 1
