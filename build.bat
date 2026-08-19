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

REM ---- embed Windows icon + manifest via go-winres ----
REM Produces cmd\nightme\rsrc_windows_<arch>.syso which the Go
REM linker auto-picks up from the main package's source dir and
REM embeds into the .exe's PE .rsrc section. Without this step the
REM .exe shows the default go toolchain icon.
where go-winres >nul 2>&1
if errorlevel 1 goto install_winres
goto have_winres
:install_winres
echo [build.bat] go-winres not on PATH; installing via `go install`...
go install github.com/tc-hib/go-winres@latest
if errorlevel 1 goto winres_install_failed
REM Make sure GOPATH\bin is on PATH for this script in case the
REM user has not added it to their system PATH yet.
for /f "tokens=*" %%g in ('go env GOPATH') do set "NM_GOPATH=%%g"
if defined NM_GOPATH set "PATH=!NM_GOPATH!\bin;%PATH%"
where go-winres >nul 2>&1
if errorlevel 1 goto winres_install_failed
goto have_winres
:winres_install_failed
echo [build.bat] ERROR: failed to install go-winres. Add %GOPATH%\bin to PATH or install manually. 1>&2
exit /b 1
:have_winres

pushd cmd\nightme
go-winres make --in assets\winres.json --out rsrc --arch 386,amd64,arm64 --product-version !VERSION! --file-version !VERSION!
set "WINRES_RC=!errorlevel!"
popd
if !WINRES_RC! neq 0 goto winres_failed
goto winres_ok
:winres_failed
echo [build.bat] ERROR: go-winres make failed. 1>&2
exit /b 1
:winres_ok

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
