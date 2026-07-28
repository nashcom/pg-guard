#!/usr/bin/env bash
# pg-guard - native Linux dev/test: graceful shutdown via pg_ctl stop -m fast.
# Mirrors windows/stop-postgres.cmd. On Linux, pg-guard itself uses direct
# signal forwarding rather than pg_ctl (see stop_linux.go) -- this script is
# for manual/interactive use only.

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
  cat <<'EOF'
Usage: ./stop-postgres.sh

Graceful shutdown via pg_ctl stop -m fast. For manual/interactive use only
-- pg-guard itself uses direct signal forwarding on Linux (see
stop_linux.go), not pg_ctl. Takes no arguments -- configured via env vars:
PG_VERSION, PG_BIN, PGDATA (all optional, see defaults in this script).
EOF
  exit 0
fi

set -euo pipefail

PG_VERSION="${PG_VERSION:-17}"
PG_BIN="${PG_BIN:-/usr/lib/postgresql/${PG_VERSION}/bin}"
PGDATA="${PGDATA:-/var/lib/postgresql/pgdata}"

echo "Stopping PostgreSQL (fast shutdown) via pg_ctl ..."
"${PG_BIN}/pg_ctl" -D "${PGDATA}" stop -m fast
