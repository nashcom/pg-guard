#!/bin/bash
# pg-guard -- docker/test_roundtrip.sh -- HA round trips against the docker/
# stack: promote guard, coordinated shutdown, and switchover, each
# confirmed via psql (role/replication state, same approach as verify.sh)
# and docker inspect (container exit/restart behavior), with curl only to
# trigger pg-guard's own API. Assumes a healthy two-node cluster is already
# up (run verify.sh first) -- flips which node is primary along the way,
# and restores the cluster to a healthy two-node state before finishing.
#
# Usage: ./test_roundtrip.sh    -- promote-guard, shutdown, switchover
#        ./test_roundtrip.sh -f -- also runs the failover round trip
#                                  (docker pause the primary, ~90s+, disruptive)

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
  echo "Usage: ./test_roundtrip.sh [-f]"
  echo "  (no args)  promote-guard, coordinated shutdown, and switchover round trips"
  echo "  -f         also runs the automatic-failover round trip (docker pause"
  echo "             the primary, ~90s+, disruptive)"
  echo
  echo "Assumes a healthy two-node cluster is already up (run verify.sh first)."
  exit 0
fi

set -uo pipefail
cd "$(dirname "$0")"

PASS=0
FAILURES=0
WARNINGS=0

delim() { echo "--------------------------------------------------------------------------------"; }

# ts: UTC timestamp on every line this script writes, same format as
# pg-guard's own logs (log.go) -- makes it straightforward to correlate a
# line here with the same moment in "docker compose logs".
ts() { date -u +"%Y-%m-%dT%H:%M:%SZ"; }

header()
{
  echo
  delim
  echo "$(ts) $*"
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
  printf "%s [${CLR_GREEN}PASS${CLR_RESET}] %s\n" "$(ts)" "$*"
  PASS=$((PASS + 1))
}

warn()
{
  printf "%s [${CLR_YELLOW}WARN${CLR_RESET}] %s\n" "$(ts)" "$*"
  WARNINGS=$((WARNINGS + 1))
}

fail()
{
  printf "%s [${CLR_RED}FAIL${CLR_RESET}] %s\n" "$(ts)" "$*"
  FAILURES=$((FAILURES + 1))
}

info()
{
  printf "%s [${CLR_BLUE}INFO${CLR_RESET}] %s\n" "$(ts)" "$*"
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

# role_of_service: pg_is_in_recovery() via psql -- "f" (primary) or "t"
# (standby) -- same approach verify.sh already uses, no jq/JSON parsing.
role_of_service()
{
  case "$1" in
    pg-traveler-0) psql0 'SELECT pg_is_in_recovery();' 2>/dev/null ;;
    pg-traveler-1) psql1 'SELECT pg_is_in_recovery();' 2>/dev/null ;;
  esac
}

# port_of_service: the API port (POST /api/*) -- see metrics_port_of_service
# for the separate GET /health|/ready|/status|/metrics listener. 8080/8081
# only ever mean plain HTTP, 8443/8444 only ever mean TLS -- see
# ../README.md's TLS section -- so which one's actually listening depends
# on tls_enabled() (defined below), read from .env the same way pg-guard
# itself reads it.
port_of_service()
{
  if tls_enabled; then
    case "$1" in
      pg-traveler-0) echo 8443 ;;
      pg-traveler-1) echo 8444 ;;
    esac
  else
    case "$1" in
      pg-traveler-0) echo 8080 ;;
      pg-traveler-1) echo 8081 ;;
    esac
  fi
}

metrics_port_of_service()
{
  case "$1" in
    pg-traveler-0) echo 9100 ;;
    pg-traveler-1) echo 9101 ;;
  esac
}

# tls_enabled/mtls_required: read straight from .env (not from whether
# tls/ca.crt happens to exist on disk -- generate-certs.sh may have been
# run without the result ever being activated). GET /health|/status|
# /metrics (the metrics listener) never use TLS regardless -- see
# ../README.md's TLS section -- so this only matters for the POST /api/*
# calls below.
tls_enabled()  { [ -f .env ] && grep -qE '^PG_GUARD_SSL_CERT_FILE=.+' .env; }
mtls_required() { [ -f .env ] && grep -qE '^PG_GUARD_MTLS_REQUIRE=true' .env; }

api_url()
{
  local service="$1" path="$2" scheme="http"
  tls_enabled && scheme="https"
  echo "$scheme://localhost:$(port_of_service "$service")$path"
}

# curl_api: POSTs to a service's API listener. Trusts the local test CA
# (generate-certs.sh) when TLS is active, so a round trip through this
# actually proves the whole chain works (cert loaded, CA trusted, handshake
# succeeds) -- not just that the plain-HTTP fallback still does. Presents
# this node's own cert too when PG_GUARD_MTLS_REQUIRE=true, since the peer
# would otherwise reject the connection at the handshake.
curl_api()
{
  local service="$1" path="$2"
  local -a extra=()
  if tls_enabled; then
    extra+=(--cacert tls/ca.crt)
    mtls_required && extra+=(--cert tls/tls.crt --key tls/tls.key)
  fi
  curl -s -o /tmp/roundtrip-resp.json -w '%{http_code}' "${extra[@]}" -X POST "$(api_url "$service" "$path")"
}

other_service()
{
  case "$1" in
    pg-traveler-0) echo pg-traveler-1 ;;
    pg-traveler-1) echo pg-traveler-0 ;;
  esac
}

primary_service()
{
  [ "$(role_of_service pg-traveler-0)" = "f" ] && { echo pg-traveler-0; return; }
  [ "$(role_of_service pg-traveler-1)" = "f" ] && { echo pg-traveler-1; return; }
}

wait_for_role()
{
  local service="$1" expected="$2" timeout="${3:-30}"
  local start=$SECONDS deadline=$((SECONDS + timeout)) last_heartbeat=$SECONDS
  while [ "$SECONDS" -lt "$deadline" ]; do
    [ "$(role_of_service "$service")" = "$expected" ] && return 0
    if [ $((SECONDS - last_heartbeat)) -ge 10 ]; then
      info "Still waiting on $service (target: $expected) -- $((SECONDS - start))s elapsed"
      last_heartbeat=$SECONDS
    fi
    sleep 1
  done
  return 1
}

# wait_for_api: postgres itself becoming queryable via psql (wait_for_role)
# and pg-guard's own API server actually listening are two different
# signals -- pg-guard blocks starting the API on ensureRoleAndDatabase
# finishing first (see bootstrap.go), so there's a real gap right after a
# restart/rejoin where psql already works but the API doesn't yet.
wait_for_api()
{
  local service="$1" timeout="${2:-30}"
  local deadline=$((SECONDS + timeout))
  while [ "$SECONDS" -lt "$deadline" ]; do
    curl -s -o /dev/null --max-time 2 "http://localhost:$(metrics_port_of_service "$service")/health" && return 0
    sleep 1
  done
  return 1
}

container_status() { docker inspect -f '{{.State.Status}}' "$1" 2>/dev/null; }
container_restart_count() { docker inspect -f '{{.RestartCount}}' "$1" 2>/dev/null; }

wait_for_container_status()
{
  local name="$1" expected="$2" timeout="${3:-30}"
  local deadline=$((SECONDS + timeout))
  while [ "$SECONDS" -lt "$deadline" ]; do
    [ "$(container_status "$name")" = "$expected" ] && return 0
    sleep 1
  done
  return 1
}

wait_for_restart_count_above()
{
  local name="$1" baseline="$2" timeout="${3:-30}"
  local deadline=$((SECONDS + timeout))
  while [ "$SECONDS" -lt "$deadline" ]; do
    local rc
    rc="$(container_restart_count "$name")"
    [ -n "$rc" ] && [ "$rc" -gt "$baseline" ] && return 0
    sleep 1
  done
  return 1
}

resp() { cat /tmp/roundtrip-resp.json 2>/dev/null; }

# run_docker: every one-shot docker/docker compose lifecycle command in this
# script goes through here instead of being called directly -- centralizes
# the blank-line-before-and-after spacing convention in one place (a single
# spot to change it again, or add more logic later, e.g. logging the exact
# command run) instead of duplicating "echo; docker ...; echo" at every call
# site. Preserves the wrapped command's exit status, since "docker compose
# up --wait" is called inside an if/then. Deliberately does NOT redirect
# "$@"'s own output -- callers used to suppress it with >/dev/null, but that
# redirection would also swallow this function's own blank-line echoes;
# letting these lifecycle commands' normally-terse output show is a fair
# trade for that.
#
# Not used for docker inspect/docker compose exec calls inside the
# wait_for_*/role_of_service helpers -- those run every ~1s in polling
# loops, where this spacing would flood the output instead of clarifying it.
run_docker()
{
  echo
  docker "$@"
  local rc=$?
  echo
  return $rc
}

header "Clean start"

if tls_enabled; then
  if mtls_required; then
    info "TLS: enabled, mutual TLS required (PG_GUARD_MTLS_REQUIRE=true in .env)"
  else
    info "TLS: enabled (PG_GUARD_SSL_CERT_FILE set in .env)"
  fi
else
  info "TLS: disabled (plain HTTP) -- set PG_GUARD_SSL_CERT_FILE/PG_GUARD_SSL_KEY_FILE in .env to test with TLS"
fi

info "Tearing down any previous stack..."

run_docker compose down

run_docker volume rm postgres-data-0 postgres-data-1 2>/dev/null || true

info "Starting a fresh stack and waiting for both nodes to report healthy (pg_isready)..."

if run_docker compose up -d --wait --wait-timeout 200; then
  pass "Both containers report healthy"
else
  # Bail out immediately here, matching hatest.sh's identical "Clean start"
  # step -- without this, every check from here on (roles, promote guard,
  # switchover, failover) runs against containers that never came up,
  # producing a wall of confusing, unrelated-looking failures ("Pg-traveler-0
  # is not primary") instead of pointing at the actual root cause. See
  # docker compose ps / logs for why the containers didn't become healthy
  # (a common one: docker/.env and docker/tls/ are both gitignored --
  # PG_GUARD_SSL_CERT_FILE set in .env without the matching tls/ files
  # present, e.g. after a fresh clone, makes pg-guard fail config
  # validation and never start postgres at all).
  fail "Stack did not become healthy within 200s -- containers failed to start (see docker compose ps / logs)"
  exit 1
fi

echo

[ "$(role_of_service pg-traveler-0)" = "f" ] && pass "Pg-traveler-0 is primary" || fail "Pg-traveler-0 is not primary"
[ "$(role_of_service pg-traveler-1)" = "t" ] && pass "Pg-traveler-1 is standby" || fail "Pg-traveler-1 is not standby"

header "Promote guard"

PRIMARY="$(primary_service)"
STANDBY="$(other_service "$PRIMARY")"
info "Current primary: $PRIMARY, current standby: $STANDBY"

code="$(curl_api "$STANDBY" /api/promote)"
info "POST $STANDBY/api/promote -> $code: $(resp)"

[ "$code" = "409" ] && pass "Unforced promote on standby refused with 409" || fail "Expected 409, got $code"
[ "$(role_of_service "$PRIMARY")" = "f" ] && pass "$PRIMARY still primary after refused promote" || fail "$PRIMARY role changed"
[ "$(role_of_service "$STANDBY")" = "t" ] && pass "$STANDBY still standby after refused promote" || fail "$STANDBY role changed"

header "Coordinated shutdown"

PRIMARY="$(primary_service)"
STANDBY="$(other_service "$PRIMARY")"
info "Shutting down current primary $PRIMARY; expecting $STANDBY to become primary"

code="$(curl_api "$PRIMARY" /api/shutdown)"
info "POST $PRIMARY/api/shutdown -> $code: $(resp)"
[ "$code" = "200" ] && pass "Shutdown request accepted" || fail "Expected 200, got $code"

wait_for_role "$STANDBY" f 20 && pass "$STANDBY promoted to primary" || fail "$STANDBY did not become primary within 20s"
wait_for_container_status "$PRIMARY" exited 15 && pass "$PRIMARY container exited" || fail "$PRIMARY container did not reach 'exited' within 15s"
sleep 2
st="$(container_status "$PRIMARY")"
[ "$st" = "exited" ] && pass "$PRIMARY stayed down 2s later (no unwanted restart on exit 0)" || fail "$PRIMARY status changed to '$st'"

info "Restarting $PRIMARY -- expecting it to rejoin as standby automatically"

run_docker compose start "$PRIMARY"

wait_for_role "$PRIMARY" t 120 && pass "$PRIMARY rejoined and reports standby" || fail "$PRIMARY did not become standby within 120s of restart"
wait_for_api "$PRIMARY" 30 || warn "$PRIMARY's API did not respond within 30s of its postgres becoming reachable"

header "Switchover"

PRIMARY="$(primary_service)"
STANDBY="$(other_service "$PRIMARY")"
info "Switching over current primary $PRIMARY; expecting $STANDBY to become primary and $PRIMARY to restart as standby"

baseline_rc="$(container_restart_count "$PRIMARY")"
code="$(curl_api "$PRIMARY" /api/switchover)"
info "POST $PRIMARY/api/switchover -> $code: $(resp)"
[ "$code" = "200" ] && pass "Switchover request accepted" || fail "Expected 200, got $code"

wait_for_role "$STANDBY" f 20 && pass "$STANDBY promoted to primary" || fail "$STANDBY did not become primary within 20s"
wait_for_restart_count_above "$PRIMARY" "$baseline_rc" 30 && pass "$PRIMARY container restarted (restart: on-failure fired on the sentinel exit code)" || fail "$PRIMARY container did not restart within 30s of switchover"
wait_for_role "$PRIMARY" t 120 && pass "$PRIMARY came back up and reports standby" || fail "$PRIMARY did not report standby within 120s of restart"
wait_for_api "$PRIMARY" 30 || warn "$PRIMARY's API did not respond within 30s of its postgres becoming reachable"

if [ "${1:-}" = "-f" ]; then
  header "Automatic failover"

  PRIMARY="$(primary_service)"
  STANDBY="$(other_service "$PRIMARY")"
  info "Pausing current primary $PRIMARY (docker pause, not the API); expecting $STANDBY to auto-promote within 90s"

  run_docker pause "$PRIMARY"

  wait_for_role "$STANDBY" f 90 && pass "$STANDBY auto-promoted to primary within 90s (no API call)" || fail "$STANDBY did not auto-promote within 90s"

  info "Unpausing + restarting $PRIMARY -- unpause alone resumes the same stale-primary process, only a real restart re-runs the startup rejoin check"

  run_docker unpause "$PRIMARY"

  run_docker compose restart "$PRIMARY"

  wait_for_role "$PRIMARY" t 120 && pass "$PRIMARY rejoined and reports standby" || fail "$PRIMARY did not report standby within 120s of restart -- cluster may need manual attention"
  wait_for_api "$PRIMARY" 30 || warn "$PRIMARY's API did not respond within 30s of its postgres becoming reachable"
fi

header "Final state"

final_primary_count=0
for svc in pg-traveler-0 pg-traveler-1; do
  [ "$(role_of_service "$svc")" = "f" ] && final_primary_count=$((final_primary_count + 1))
done
[ "$final_primary_count" = "1" ] && pass "Exactly one primary in the cluster at the end" || fail "Found $final_primary_count primaries -- split-brain or no primary, needs manual attention"

rm -f /tmp/roundtrip-resp.json

header "RESULTS"
echo "Passed  : $PASS"
echo "Warnings: $WARNINGS"
echo "Failures: $FAILURES"
echo "Runtime : $SECONDS seconds"
echo

[ "$FAILURES" -eq 0 ]
