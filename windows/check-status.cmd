@echo off
REM pg-guard - native Windows dev/test: quick non-interactive health check
REM against an already-running instance (run start-postgres.cmd first).
REM Uses pg_isready and psql -- the same "live query against a running
REM server" category of tool pg-guard itself will use pgx for later
REM (see README.md: Command Execution vs. Direct Connection).

if "%~1"=="/?" goto :help
if "%~1"=="-h" goto :help
if "%~1"=="--help" goto :help
goto :run

:help
echo Usage: check-status.cmd
echo Health check against an already-running instance: pg_isready, server
echo version, databases, recovery/role state, database size. No arguments.
exit /b 0

:run
setlocal

set "PG_BIN=C:\Program Files\PostgreSQL\18\bin"
set "PGPORT=5432"
set "SUPERUSER=postgres"
set "DB_NAME=traveler"

echo === pg_isready ===
"%PG_BIN%\pg_isready.exe" -p %PGPORT% -U %SUPERUSER%
echo.

echo === server version ===
"%PG_BIN%\psql.exe" -U %SUPERUSER% -p %PGPORT% -d postgres -c "SELECT version();"
echo.

echo === databases ===
"%PG_BIN%\psql.exe" -U %SUPERUSER% -p %PGPORT% -d postgres -c "\l"
echo.

echo === recovery / role state ===
REM false = primary (or a standalone instance), true = standby in recovery
"%PG_BIN%\psql.exe" -U %SUPERUSER% -p %PGPORT% -d postgres -c "SELECT pg_is_in_recovery();"
echo.

echo === "%DB_NAME%" database size ===
"%PG_BIN%\psql.exe" -U %SUPERUSER% -p %PGPORT% -d postgres -c "SELECT pg_size_pretty(pg_database_size('%DB_NAME%'));"

endlocal
