@echo off
REM build-stt.bat - compile nightme-stt.exe into bin\ for Windows.
REM
REM Cmd-native equivalent of `make build-stt`. Sidesteps the
REM GnuWin32 make + PowerShell compatibility issues by using
REM plain cmd syntax, mirroring build.bat's structure so the two
REM scripts read the same shape at a glance. Output is identical
REM to what `make build-stt` produces (bin\nightme-stt.exe with
REM version metadata baked in).
REM
REM Requires Go 1.21+, git, and powershell on PATH (same as
REM build.bat — see that script for the rationale).
REM
REM CC must be set by the caller on windows-11-arm to the MSYS2
REM CLANGARM64 clang (`C:\msys64\clangarm64\bin\clang.exe`).
REM The default `clang` on Windows PATH is the Visual Studio LLVM
REM build, which doesn't understand the aarch64-w64-mingw32
REM sysroot and fails to translate cgo's `-L` flags into lld-link's
REM `/LIBPATH` for the sherpa-onnx .lib files that live in Go's
REM module cache. On amd64 hosts the default mingw gcc is correct,
REM so this script leaves CC untouched and trusts the caller.

setlocal EnableExtensions EnableDelayedExpansion

REM ---- version metadata ----
set "TMPOUT=%TEMP%\nightme-build-stt-%RANDOM%.tmp"

if defined VERSION goto got_version
set "VERSION=0.1.0"
:got_version

if defined GIT_COMMIT goto got_commit
git rev-parse --short HEAD > "%TMPOUT%" 2>nul
if errorlevel 1 goto no_revparse
set /p GIT_COMMIT=<"%TMPOUT%"
goto got_commit
:no_revparse
set "GIT_COMMIT=local"
:got_commit

del "%TMPOUT%" 2>nul

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
echo [build-stt.bat] ERROR: `go` not on PATH. Install Go 1.21+ and retry. 1>&2
exit /b 1
:preconditions_ok

if not exist bin mkdir bin

echo [build-stt.bat] VERSION    = !VERSION!
echo [build-stt.bat] GIT_COMMIT = !GIT_COMMIT!
echo [build-stt.bat] BUILD_DATE = !BUILD_DATE!
echo [build-stt.bat] CC         = !CC!
echo [build-stt.bat] Building bin\nightme-stt.exe (cgo_sherpa) ...

REM CGO must be enabled for the cgo_sherpa binding; on Windows
REM runners it's the default, but pin CGO_ENABLED=1 explicitly so
REM a CI env override (`CGO_ENABLED=0` for the main static build)
REM can't silently break this half of the build.
set CGO_ENABLED=1

go build -tags cgo_sherpa -ldflags "-X github.com/cnlangzi/nightme/internal/version.Version=!VERSION! -X github.com/cnlangzi/nightme/internal/version.GitCommit=!GIT_COMMIT! -X github.com/cnlangzi/nightme/internal/version.BuildDate=!BUILD_DATE!" -o bin\nightme-stt.exe .\cmd\nightme-stt
if errorlevel 1 goto build_failed
goto build_ok
:build_failed
echo [build-stt.bat] Build FAILED. 1>&2
exit /b 1
:build_ok

echo [build-stt.bat] ok: bin\nightme-stt.exe
endlocal