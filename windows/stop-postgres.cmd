@echo off
REM pg-guard - native Windows dev/test setup: graceful shutdown via
REM pg_ctl stop -m fast. This is exactly the mechanism pg-guard's Windows
REM supervisor will shell out to -- os.Process.Signal() has no real
REM SIGTERM/SIGINT equivalent on Windows, so pg_ctl stop is the only
REM correct way to stop postgres.exe cleanly on this platform.

if "%~1"=="/?" goto :help
if "%~1"=="-h" goto :help
if "%~1"=="--help" goto :help
goto :run

:help
echo Usage: stop-postgres.cmd
echo Graceful shutdown via pg_ctl stop -m fast. No arguments.
exit /b 0

:run
setlocal

set "PG_BIN=C:\Program Files\PostgreSQL\18\bin"
set "PGDATA=D:\postgres"

echo Stopping PostgreSQL (fast shutdown) via pg_ctl ...
"%PG_BIN%\pg_ctl.exe" -D "%PGDATA%" stop -m fast

endlocal
