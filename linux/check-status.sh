#!/usr/bin/env bash
# pg-guard - native Linux dev/test: quick non-interactive health check
# against an already-running instance (run start-postgres.sh first).
# Mirrors windows/check-status.cmd. Uses pg_isready and psql -- the same
# "live query against a running server" category of tool pg-guard itself
# will use pgx for later (see README.md: Command Execution vs. Direct
# Connection).

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
  cat <<'EOF'
Usage: ./check-status.sh

Non-interactive health check against an already-running instance (run
start-postgres.sh first): pg_isready, server version, databases,
recovery/role state, database size. Takes no arguments -- configured via
env vars: PG_VERSION, PG_BIN, PGPORT, SUPERUSER, DB_NAME (all optional,
see defaults in this script).
EOF
  exit 0
fi

set -euo pipefail

PG_VERSION="${PG_VERSION:-17}"
PG_BIN="${PG_BIN:-/usr/lib/postgresql/${PG_VERSION}/bin}"
PGPORT="${PGPORT:-5432}"
SUPERUSER="${SUPERUSER:-postgres}"
DB_NAME="${DB_NAME:-traveler}"

echo "=== pg_isready ==="
"${PG_BIN}/pg_isready" -p "${PGPORT}" -U "${SUPERUSER}"
echo

echo "=== server version ==="
"${PG_BIN}/psql" -U "${SUPERUSER}" -p "${PGPORT}" -d postgres -c "SELECT version();"
echo

echo "=== databases ==="
"${PG_BIN}/psql" -U "${SUPERUSER}" -p "${PGPORT}" -d postgres -c "\l"
echo

echo "=== recovery / role state ==="
# false = primary (or a standalone instance), true = standby in recovery
"${PG_BIN}/psql" -U "${SUPERUSER}" -p "${PGPORT}" -d postgres -c "SELECT pg_is_in_recovery();"
echo

echo "=== \"${DB_NAME}\" database size ==="
"${PG_BIN}/psql" -U "${SUPERUSER}" -p "${PGPORT}" -d postgres -c \
    "SELECT pg_size_pretty(pg_database_size('${DB_NAME}'));"
