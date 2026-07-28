#!/usr/bin/env bash
# pg-guard - native Linux dev/test setup, step 2: run postgres in the
# foreground against PGDATA, the same way pg-guard's supervisor execs it as
# its child process. Run init-traveler-db.sh first if you haven't yet.
# Mirrors windows/start-postgres.cmd.
#
# Unlike the Windows case, Ctrl+C here IS a clean shutdown -- postgres
# handles SIGINT natively on Linux (fast shutdown) -- but stop-postgres.sh
# is still useful to test the exact "pg_ctl stop"-independent path pg-guard
# itself uses (direct signal forwarding on Linux, see stop_linux.go).

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
  cat <<'EOF'
Usage: ./start-postgres.sh

Runs postgres in the foreground against PGDATA, the same way pg-guard's
supervisor execs it as its child process. Run init-traveler-db.sh first if
you haven't yet. Ctrl+C is a clean shutdown here (postgres handles SIGINT
natively on Linux). Takes no arguments -- configured via env vars:
PG_VERSION, PG_BIN, PGDATA, PGPORT (all optional, see defaults in this
script).
EOF
  exit 0
fi

set -euo pipefail

PG_VERSION="${PG_VERSION:-17}"
PG_BIN="${PG_BIN:-/usr/lib/postgresql/${PG_VERSION}/bin}"
PGDATA="${PGDATA:-/var/lib/postgresql/pgdata}"
PGPORT="${PGPORT:-5432}"

echo "Starting PostgreSQL in the foreground ..."
echo "  data directory: ${PGDATA}"
echo "  port:           ${PGPORT}"
echo

exec "${PG_BIN}/postgres" -D "${PGDATA}" -p "${PGPORT}"
