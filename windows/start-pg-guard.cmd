@echo off
REM pg-guard - run postgres via pg-guard instead of directly. Same
REM environment variables, same PGDATA -- pg-guard just becomes the
REM supervising parent process instead of postgres.exe running bare.
REM
REM Unlike raw postgres.exe (see start-postgres.cmd), Ctrl+C here IS a safe
REM way to stop: pg-guard catches it and shells out to "pg_ctl stop -m fast"
REM itself (Windows has no real SIGTERM/SIGINT delivery to an arbitrary
REM child process, so pg-guard uses pg_ctl instead -- see README.md).
REM
REM Run init-traveler-db.cmd first if you haven't yet.

if "%~1"=="/?" goto :help
if "%~1"=="-h" goto :help
if "%~1"=="--help" goto :help
goto :run

:help
echo Usage: start-pg-guard.cmd
echo Runs postgres via pg-guard.exe as the supervising parent process. No arguments.
exit /b 0

:run
setlocal

set "PG_BIN=C:\Program Files\PostgreSQL\18\bin"
set "PG_GUARD_BIN=%~dp0..\bin\pg-guard.exe"
set "PGDATA=D:\postgres"
set "PG_GUARD_POSTGRES_BIN=%PG_BIN%\postgres.exe"
set "PG_GUARD_LOG_LEVEL=debug"
set "PG_GUARD_LOG_FORMAT=text"

echo Starting postgres via pg-guard (Ctrl+C for a graceful shutdown) ...
echo   PG_GUARD_POSTGRES_BIN: %PG_GUARD_POSTGRES_BIN%
echo   PGDATA:                %PGDATA%
echo.

"%PG_GUARD_BIN%"

endlocal
