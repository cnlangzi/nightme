@echo off
REM restart.bat - build and restart the nightme daemon.
REM
REM Cmd-native equivalent of `make restart`. Calls build.bat
REM first (so a stale binary can't restart a daemon with the
REM old code), then invokes the binary's own `restart` sub-
REM command, which signals the running daemon to exit and a
REM fresh one to spawn.
REM
REM Failure modes:
REM   - build.bat fails (compile error, no go on PATH, ...)
REM     -> restart.bat exits 1 BEFORE touching the daemon.
REM   - bin\nightme.exe is missing after build -> exits 1.
REM   - no running daemon to restart -> `nightme restart`
REM     exits non-zero but the build is still ok; we surface
REM     the underlying error to the operator.

setlocal

call "%~dp0build.bat"
if errorlevel 1 goto build_failed
goto build_ok
:build_failed
echo [restart.bat] build failed; not touching the daemon. 1>&2
exit /b 1
:build_ok

if not exist bin\nightme.exe goto no_binary
goto have_binary
:no_binary
echo [restart.bat] bin\nightme.exe missing after build; aborting. 1>&2
exit /b 1
:have_binary

echo [restart.bat] invoking nightme restart ...
bin\nightme.exe restart
REM Preserve the daemon-control exit code - a non-zero from
REM `nightme restart` (e.g. "no daemon to restart") should
REM propagate so CI / wrappers can see it.
exit /b %errorlevel%
endlocal
