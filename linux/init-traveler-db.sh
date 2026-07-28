#!/usr/bin/env bash
# pg-guard - native Linux dev/test setup, step 1: initialize a data
# directory and create the Traveler database. Mirrors windows/init-traveler-db.cmd.
#
# Auth is "trust" (no password) for local dev/familiarization only -- NOT
# what the real HA setup will use (see README.md: POSTGRES_PASSWORD,
# PG_GUARD_SSL_CERT_FILE/KEY_FILE/CA_FILE). Safe to re-run: skips initdb if
# PGDATA already has a cluster, and tolerates the database already existing.

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
  cat <<'EOF'
Usage: ./init-traveler-db.sh

Initializes a data directory (if not already done) and creates the
Traveler database. Auth is "trust" -- dev/test only. Safe to re-run. Takes
no arguments -- configured via env vars: PG_VERSION, PG_BIN, PGDATA,
PGPORT, SUPERUSER, DB_NAME, DB_USER (all optional, see defaults in this
script).
EOF
  exit 0
fi

set -euo pipefail

PG_VERSION="${PG_VERSION:-17}"
PG_BIN="${PG_BIN:-/usr/lib/postgresql/${PG_VERSION}/bin}"
PGDATA="${PGDATA:-/var/lib/postgresql/pgdata}"
PGPORT="${PGPORT:-5432}"
SUPERUSER="${SUPERUSER:-postgres}"
DB_NAME="${DB_NAME:-traveler}"
DB_USER="${DB_USER:-postgres}"

if [ -f "${PGDATA}/PG_VERSION" ]; then
    echo "Data directory \"${PGDATA}\" is already initialized - skipping initdb."
else
    echo "Initializing PostgreSQL data directory at \"${PGDATA}\" ..."
    "${PG_BIN}/initdb" -D "${PGDATA}" -U "${SUPERUSER}" -A trust -E UTF8
fi

echo "Starting PostgreSQL temporarily to create the \"${DB_NAME}\" database ..."
"${PG_BIN}/pg_ctl" -D "${PGDATA}" -o "-p ${PGPORT}" -w -l "${PGDATA}/init.log" start

if "${PG_BIN}/psql" -U "${SUPERUSER}" -p "${PGPORT}" -d postgres -c \
    "CREATE DATABASE ${DB_NAME} WITH OWNER ${DB_USER} ENCODING = 'UTF8' LOCALE_PROVIDER = icu ICU_LOCALE = 'und' TEMPLATE = template0;" \
    2>/dev/null; then
    echo "Created database \"${DB_NAME}\" (owner ${DB_USER}, ICU locale 'und', template0)."
else
    echo "Database \"${DB_NAME}\" already exists - continuing."
fi

echo "Stopping temporary PostgreSQL instance ..."
"${PG_BIN}/pg_ctl" -D "${PGDATA}" -w stop -m fast

echo
echo "Done. \"${PGDATA}\" is ready with database \"${DB_NAME}\"."
echo "Use start-postgres.sh to run it in the foreground."
