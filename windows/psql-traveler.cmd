@echo off
REM pg-guard - native Windows dev/test: open an interactive psql session
REM against the traveler database for ad hoc queries. Assumes the instance
REM is already running (start-postgres.cmd).

if "%~1"=="/?" goto :help
if "%~1"=="-h" goto :help
if "%~1"=="--help" goto :help
goto :run

:help
echo Usage: psql-traveler.cmd
echo Opens an interactive psql session against the traveler database. No arguments.
exit /b 0

:run
setlocal

set "PG_BIN=C:\Program Files\PostgreSQL\18\bin"
set "PGPORT=5432"
set "SUPERUSER=postgres"
set "DB_NAME=traveler"

"%PG_BIN%\psql.exe" -U %SUPERUSER% -p %PGPORT% -d %DB_NAME%

endlocal
