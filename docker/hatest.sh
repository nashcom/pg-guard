#!/bin/bash
# pg-guard -- docker/hatest.sh -- compiles and runs ../hatest/HaTest.java
# (see ../hatest/README.md) against this docker-compose stack. Reads
# connection details from .env the same way test_roundtrip.sh/verify.sh do.
#
# Fetches the pgJDBC driver (checksum-verified) and compiles *before*
# touching Docker at all -- a compile error is cheap and local, and should
# fail fast on its own rather than only surfacing after the "Clean start"
# below has already spent 30-60s tearing down and rebuilding the whole
# stack for a run that was never going to get that far anyway.
#
# Always recompiles rather than checking staleness -- javac is fast enough
# that this costs nothing, and the Go predecessor of this tool got bitten
# once by a stale-binary check silently running old code after a fix.
#
# "Clean start" itself is the same pattern test_roundtrip.sh uses: tears
# down any previous stack, drops the data volumes, brings up a fresh one,
# waits for both nodes healthy.
#
# Extra args are passed straight through to "HaTest run", e.g.:
#   ./hatest.sh --duration-sec 30
#   ./hatest.sh --json > report.json

set -uo pipefail
cd "$(dirname "$0")"

delim() { echo "--------------------------------------------------------------------------------"; }
ts() { date -u +"%Y-%m-%dT%H:%M:%SZ"; }

header()
{
  echo
  delim
  echo "$(ts) $*"
  delim
  echo
}

CLR_RED="\033[31m"
CLR_GREEN="\033[32m"
CLR_BLUE="\033[34m"
CLR_RESET="\033[0m"

pass() { printf "%s [${CLR_GREEN}PASS${CLR_RESET}] %s\n" "$(ts)" "$*"; }
fail() { printf "%s [${CLR_RED}FAIL${CLR_RESET}] %s\n" "$(ts)" "$*"; }
info() { printf "%s [${CLR_BLUE}INFO${CLR_RESET}] %s\n" "$(ts)" "$*"; }

# run_docker: same convention as test_roundtrip.sh's helper of the same
# name -- blank-line spacing around lifecycle commands' own output,
# preserves exit status for the "up --wait" check below.
run_docker()
{
  echo
  docker "$@"
  local rc=$?
  echo
  return $rc
}

tls_enabled() { [ -f .env ] && grep -qE '^PG_GUARD_SSL_CERT_FILE=.+' .env; }

header "hatest build"

info "fetching pgJDBC driver..."
../bin/download-postgresql-jdbc.sh || exit 1

info "compiling HaTest.java..."
(cd ../hatest && javac -cp ../bin/postgresql-jdbc.jar HaTest.java) || {
  fail "compile failed -- stopping before touching the docker stack"
  exit 1
}
pass "HaTest.java compiled"

# -h/--help doesn't need the docker stack at all -- HaTest's own help check
# only looks at args[0], which this script otherwise hardcodes to "run",
# so a --help anywhere in "$@" would silently be swallowed as a bogus
# option value to the run subcommand instead of showing usage. Short-circuit
# here, before "Clean start"'s teardown/rebuild, rather than let that run
# pointlessly just to see usage text.
for arg in "$@"; do
  if [ "$arg" = "-h" ] || [ "$arg" = "--help" ]; then
    exec java -cp ../hatest:../bin/postgresql-jdbc.jar HaTest --help
  fi
done

header "Clean start"

info "Tearing down any previous stack..."
run_docker compose down
run_docker volume rm postgres-data-0 postgres-data-1 2>/dev/null || true

info "Starting a fresh stack and waiting for both nodes to report healthy (pg_isready)..."
if run_docker compose up -d --wait --wait-timeout 200; then
  pass "Both containers report healthy"
else
  fail "Stack did not become healthy within 200s (see docker compose ps / logs)"
  exit 1
fi

header "hatest run"

PASSWORD=""
if [ -f .env ]; then
  PASSWORD="$(grep -E '^POSTGRES_PASSWORD=' .env | tail -1 | cut -d= -f2-)"
fi

SSL_ARGS=(--sslmode disable)
if tls_enabled; then
  SSL_ARGS=(--sslmode verify-ca --sslrootcert tls/ca.crt)
fi

info "connecting: node1=localhost:5432 node2=localhost:5433 dbname=traveler user=postgres sslmode=${SSL_ARGS[1]}"

exec java -cp ../hatest:../bin/postgresql-jdbc.jar HaTest run \
  --node1 localhost:5432 --node2 localhost:5433 \
  --user postgres --password "$PASSWORD" --dbname traveler \
  "${SSL_ARGS[@]}" \
  "$@"
