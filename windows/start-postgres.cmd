@echo off
REM pg-guard - native Windows dev/test setup, step 2: run postgres.exe in
REM the foreground against D:\postgres, the same way pg-guard's supervisor
REM will eventually exec it as its child process. Run init-traveler-db.cmd
REM first if you haven't yet.
REM
REM Do NOT stop this with Ctrl+C to test graceful shutdown -- console
REM Ctrl+C is not guaranteed to be a clean shutdown on Windows. Use
REM stop-postgres.cmd (pg_ctl stop -m fast) from another window instead --
REM that's the exact mechanism pg-guard's Windows supervisor will use.

if "%~1"=="/?" goto :help
if "%~1"=="-h" goto :help
if "%~1"=="--help" goto :help
goto :run

:help
echo Usage: start-postgres.cmd
echo Runs postgres.exe in the foreground against PGDATA. No arguments.
echo Use stop-postgres.cmd for a clean shutdown -- not Ctrl+C.
exit /b 0

:run
setlocal

set "PG_BIN=C:\Program Files\PostgreSQL\18\bin"
set "PGDATA=D:\postgres"
set "PGPORT=5432"

echo Starting PostgreSQL in the foreground ...
echo   data directory: %PGDATA%
echo   port:           %PGPORT%
echo.
echo Use stop-postgres.cmd from another window for a clean shutdown.
echo.

"%PG_BIN%\postgres.exe" -D "%PGDATA%" -p %PGPORT%

endlocal
