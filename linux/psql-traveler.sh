#!/usr/bin/env bash
# pg-guard - native Linux dev/test: open an interactive psql session
# against the traveler database for ad hoc queries. Assumes the instance
# is already running (start-postgres.sh). Mirrors windows/psql-traveler.cmd.

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
  cat <<'EOF'
Usage: ./psql-traveler.sh

Opens an interactive psql session against the traveler database. Assumes
the instance is already running (start-postgres.sh). Takes no arguments --
configured via env vars: PG_VERSION, PG_BIN, PGPORT, SUPERUSER, DB_NAME
(all optional, see defaults in this script).
EOF
  exit 0
fi

set -euo pipefail

PG_VERSION="${PG_VERSION:-17}"
PG_BIN="${PG_BIN:-/usr/lib/postgresql/${PG_VERSION}/bin}"
PGPORT="${PGPORT:-5432}"
SUPERUSER="${SUPERUSER:-postgres}"
DB_NAME="${DB_NAME:-traveler}"

exec "${PG_BIN}/psql" -U "${SUPERUSER}" -p "${PGPORT}" -d "${DB_NAME}"
