#!/bin/bash

# Verifies the two-node replication setup actually works: both nodes up,
# correct primary/standby role, a replication connection present, and data
# written on the primary actually shows up on the standby (not just that
# the connection exists). Run after "docker compose up --build -d". Takes
# no arguments.

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
  echo "Usage: ./verify.sh"
  echo "Checks replication end-to-end against an already-running two-node stack."
  exit 0
fi

set -uo pipefail
cd "$(dirname "$0")"

PASS=0
FAILURES=0
WARNINGS=0

delim()
{
  echo "--------------------------------------------------------------------------------"
}

header()
{
  echo
  delim
  echo "$@"
  delim
  echo
}

# ANSI colors
CLR_RED="\033[31m"
CLR_GREEN="\033[32m"
CLR_YELLOW="\033[33m"
CLR_BLUE="\033[34m"
CLR_RESET="\033[0m"


pass()
{
    printf "[${CLR_GREEN}PASS${CLR_RESET}] %s\n" "$*"
    PASS=$((PASS + 1))
}

warn()
{
    printf "[${CLR_YELLOW}WARN${CLR_RESET}] %s\n" "$*"
    WARNINGS=$((WARNINGS + 1))
}

fail()
{
    printf "[${CLR_RED}FAIL${CLR_RESET}] %s\n" "$*"
    FAILURES=$((FAILURES + 1))
}

info()
{
    printf "[${CLR_BLUE}INFO${CLR_RESET}] %s\n" "$*"
}

check()
{
    local desc="$1"
    shift
    if "$@"; then
        pass "$desc"
    else
        fail "$desc"
    fi
}

psql0() { docker compose exec -T pg-traveler-0 psql -U postgres -d postgres -tAc "$1"; }
psql1() { docker compose exec -T pg-traveler-1 psql -U postgres -d postgres -tAc "$1"; }

header "liveness"

check "pg-traveler-0 is ready" docker compose exec -T pg-traveler-0 pg_isready -U postgres
check "pg-traveler-1 is ready" docker compose exec -T pg-traveler-1 pg_isready -U postgres

header "roles"

role0="$(psql0 'SELECT pg_is_in_recovery();')"
role1="$(psql1 'SELECT pg_is_in_recovery();')"
info "pg-traveler-0 pg_is_in_recovery() = $role0 (expect f)"
info "pg-traveler-1 pg_is_in_recovery() = $role1 (expect t)"

[ "$role0" = "f" ] && pass "pg-traveler-0 is primary" || fail "pg-traveler-0 should be primary"
[ "$role1" = "t" ] && pass "pg-traveler-1 is standby" || fail "pg-traveler-1 should be standby"

header "replication connection"

repl_rows="$(psql0 'SELECT count(*) FROM pg_stat_replication;')"
info "pg_stat_replication row count on pg-traveler-0 = $repl_rows (expect 1)"
[ "$repl_rows" = "1" ] && pass "exactly one replication connection" || fail "expected exactly one replication connection, got $repl_rows"

header "data actually replicates"

marker="verify-$(date +%s)-$RANDOM"
docker compose exec -T pg-traveler-0 psql -U postgres -d traveler -c "CREATE TABLE IF NOT EXISTS traveler_verify (marker text);" >/dev/null
docker compose exec -T pg-traveler-0 psql -U postgres -d traveler -c "INSERT INTO traveler_verify (marker) VALUES ('$marker');" >/dev/null

replicated=0
for attempt in $(seq 1 10); do
    found="$(docker compose exec -T pg-traveler-1 psql -U postgres -d traveler -tAc "SELECT count(*) FROM traveler_verify WHERE marker = '$marker';" 2>/dev/null)"
    if [ "$found" = "1" ]; then
        replicated=1
        break
    fi
    if [ "$attempt" -eq 5 ]; then
        warn "row not replicated after 5s -- still retrying"
    fi
    sleep 1
done
if [ "$replicated" = "1" ]; then
    pass "row written on pg-traveler-0 appeared on pg-traveler-1"
else
    fail "row written on pg-traveler-0 did not appear on pg-traveler-1 within 10s"
fi


header "RESULTS"
printf "Passed  : %3d\n" "$PASS"
printf "Warnings: %3d\n" "$WARNINGS"
printf "Failures: %3d\n" "$FAILURES"
echo

[ "$FAILURES" -eq 0 ]

