@echo off
REM pg-guard - native Windows dev/test setup, step 1: initialize a data
REM directory and create the Traveler database. This mirrors what
REM docker-entrypoint.sh does on Linux (initdb + create POSTGRES_DB) --
REM there is no such script on Windows, so we do it by hand here as a
REM one-time setup step before pg-guard ever gets involved.
REM
REM Auth is "trust" (no password) for local dev/familiarization only --
REM NOT what the real HA setup will use (see README.md: POSTGRES_PASSWORD,
REM PG_GUARD_SSL_CERT_FILE/KEY_FILE/CA_FILE). Safe to re-run: skips initdb
REM if PGDATA already has a cluster, and tolerates the database already existing.

if "%~1"=="/?" goto :help
if "%~1"=="-h" goto :help
if "%~1"=="--help" goto :help
goto :run

:help
echo Usage: init-traveler-db.cmd
echo Initializes a data directory (if needed) and creates the Traveler
echo database. Auth is "trust" -- dev/test only. Safe to re-run. No arguments.
exit /b 0

:run
setlocal

set "PG_BIN=C:\Program Files\PostgreSQL\18\bin"
set "PGDATA=D:\postgres"
set "PGPORT=5432"
set "SUPERUSER=postgres"
set "DB_NAME=traveler"
set "DB_USER=postgres"

if exist "%PGDATA%\PG_VERSION" (
    echo Data directory "%PGDATA%" is already initialized - skipping initdb.
    goto :createdb
)

echo Initializing PostgreSQL data directory at "%PGDATA%" ...
"%PG_BIN%\initdb.exe" -D "%PGDATA%" -U %SUPERUSER% -A trust -E UTF8
if errorlevel 1 (
    echo initdb failed.
    exit /b 1
)

:createdb
echo Starting PostgreSQL temporarily to create the "%DB_NAME%" database ...
"%PG_BIN%\pg_ctl.exe" -D "%PGDATA%" -o "-p %PGPORT%" -w -l "%PGDATA%\init.log" start
if errorlevel 1 (
    echo Failed to start PostgreSQL for setup - see "%PGDATA%\init.log".
    exit /b 1
)

"%PG_BIN%\psql.exe" -U %SUPERUSER% -p %PGPORT% -d postgres -c "CREATE DATABASE %DB_NAME% WITH OWNER %DB_USER% ENCODING = 'UTF8' LOCALE_PROVIDER = icu ICU_LOCALE = 'und' TEMPLATE = template0;" 2>nul
if errorlevel 1 (
    echo Database "%DB_NAME%" already exists - continuing.
) else (
    echo Created database "%DB_NAME%" ^(owner %DB_USER%, ICU locale 'und', template0^).
)

echo Stopping temporary PostgreSQL instance ...
"%PG_BIN%\pg_ctl.exe" -D "%PGDATA%" -w stop -m fast

echo.
echo Done. "%PGDATA%" is ready with database "%DB_NAME%".
echo Use start-postgres.cmd to run it in the foreground.

endlocal
