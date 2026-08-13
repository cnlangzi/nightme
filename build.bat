@echo off
REM build.bat - compile nightme.exe into bin\ for Windows.
REM
REM Cmd-native equivalent of `make build`. Sidesteps the
REM GnuWin32 make + PowerShell compatibility issues by using
REM plain cmd syntax. Output is identical to what `make build`
REM produces (bin\nightme.exe with version metadata baked in).
REM
REM Uses `goto` for control flow because cmd's multi-line
REM `if / else ( ... )` block parsing is fragile (each branch
REM has to be on a single line, and long values are easy to
REM split wrong).
REM
REM Requires Go 1.21+, git, and powershell on PATH.

setlocal EnableExtensions EnableDelayedExpansion

REM ---- version metadata ----
set "TMPOUT=%TEMP%\nightme-build-%RANDOM%.tmp"

git describe --tags --always --dirty > "%TMPOUT%" 2>nul
if errorlevel 1 goto no_describe
set /p VERSION=<"%TMPOUT%"
goto got_describe
:no_describe
set "VERSION=0.1.0"
:got_describe

git rev-parse --short HEAD > "%TMPOUT%" 2>nul
if errorlevel 1 goto no_revparse
set /p GIT_COMMIT=<"%TMPOUT%"
goto got_revparse
:no_revparse
set "GIT_COMMIT=unknown"
:got_revparse

del "%TMPOUT%" 2>nul

REM ISO-8601 UTC timestamp. `date /t` is locale-dependent
REM (zh-CN returns "2026/08/13 zhou san"), so shell out to
REM PowerShell for a stable format. On PowerShell-less hosts
REM the binary reports "unknown" instead.
powershell -NoProfile -Command "(Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')" > "%TMPOUT%" 2>nul
if errorlevel 1 goto no_powershell
set /p BUILD_DATE=<"%TMPOUT%"
del "%TMPOUT%" 2>nul
goto got_powershell
:no_powershell
del "%TMPOUT%" 2>nul
set "BUILD_DATE=unknown"
:got_powershell

REM ---- preconditions ----
where go >nul 2>&1
if errorlevel 1 goto no_go
goto preconditions_ok
:no_go
echo [build.bat] ERROR: `go` not on PATH. Install Go 1.21+ and retry. 1>&2
exit /b 1
:preconditions_ok

if not exist bin mkdir bin

echo [build.bat] VERSION    = !VERSION!
echo [build.bat] GIT_COMMIT = !GIT_COMMIT!
echo [build.bat] BUILD_DATE = !BUILD_DATE!
echo [build.bat] Building bin\nightme.exe ...

go build -ldflags "-X github.com/cnlangzi/nightme/internal/version.Version=!VERSION! -X github.com/cnlangzi/nightme/internal/version.GitCommit=!GIT_COMMIT! -X github.com/cnlangzi/nightme/internal/version.BuildDate=!BUILD_DATE!" -o bin\nightme.exe .\cmd\nightme
if errorlevel 1 goto build_failed
goto build_ok
:build_failed
echo [build.bat] Build FAILED. 1>&2
exit /b 1
:build_ok

echo [build.bat] ok: bin\nightme.exe
endlocal
