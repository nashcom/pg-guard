#!/bin/bash
# pg-guard -- docker/switchover-loop.sh -- repeatedly triggers
# POST /api/switchover against whichever node is currently primary, waiting
# for each cycle to fully stabilize (new primary confirmed, old primary
# restarted and rejoined as standby) before the next one. Meant to run in a
# second terminal alongside ../hatest/ (via ./hatest.sh in a first
# terminal) to exercise several failover cycles during one hatest run,
# without hatest itself needing any pg-guard API access -- see the root
# README's discussion of why that stayed a deliberate scope boundary.
#
# Assumes a healthy two-node cluster is already up -- unlike
# test_roundtrip.sh, this does NOT tear down/rebuild the stack first: doing
# so would kill whatever hatest is connected to and wipe its in-progress
# test data. Run hatest.sh (or test_roundtrip.sh's Clean start) first if
# the stack isn't already up.
#
# Usage: ./switchover-loop.sh [--count N] [--interval-sec N]
#   --count N          number of switchover cycles, default 6 -- even by
#                       default so the cluster ends back on its original
#                       primary/standby configuration (each switchover
#                       flips it, so an odd count would leave things
#                       swapped from where they started); 0 = run until
#                       Ctrl+C
#   --interval-sec N    pause after each cycle stabilizes before the next,
#                       default 10 (lets hatest accumulate more writes
#                       between failovers, and gives replication a moment
#                       to settle)

set -uo pipefail
cd "$(dirname "$0")"

usage()
{
  cat <<'EOF'
Usage: ./switchover-loop.sh [--count N] [--interval-sec N]

  --count N          number of switchover cycles, default 6 -- even by
                      default so the cluster ends back on its original
                      primary/standby configuration (each switchover flips
                      it, so an odd count would leave things swapped from
                      where they started); 0 = run until Ctrl+C
  --interval-sec N    pause after each cycle stabilizes before the next,
                      default 10 (lets hatest accumulate more writes
                      between failovers, and gives replication a moment to
                      settle)

Assumes a healthy two-node cluster is already up -- does NOT tear down or
rebuild the stack first (unlike test_roundtrip.sh), since that would kill
whatever hatest is connected to and wipe its in-progress test data. Run
hatest.sh (or test_roundtrip.sh's Clean start) first if the stack isn't
already up.
EOF
}

COUNT=6
INTERVAL_SEC=10
while [ $# -gt 0 ]; do
  case "$1" in
    -h|--help) usage; exit 0 ;;
    --count) COUNT="$2"; shift 2 ;;
    --interval-sec) INTERVAL_SEC="$2"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 1 ;;
  esac
done

CYCLES_OK=0
CYCLES_FAILED=0

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
CLR_YELLOW="\033[33m"
CLR_BLUE="\033[34m"
CLR_RESET="\033[0m"

pass() { printf "%s [${CLR_GREEN}PASS${CLR_RESET}] %s\n" "$(ts)" "$*"; }
warn() { printf "%s [${CLR_YELLOW}WARN${CLR_RESET}] %s\n" "$(ts)" "$*"; }
fail() { printf "%s [${CLR_RED}FAIL${CLR_RESET}] %s\n" "$(ts)" "$*"; }
info() { printf "%s [${CLR_BLUE}INFO${CLR_RESET}] %s\n" "$(ts)" "$*"; }

# --- helpers below are the same ones test_roundtrip.sh uses, kept
# identical so both scripts read the cluster's state the same way ---

psql0() { docker compose exec -T pg-traveler-0 psql -U postgres -d postgres -tAc "$1"; }
psql1() { docker compose exec -T pg-traveler-1 psql -U postgres -d postgres -tAc "$1"; }

role_of_service()
{
  case "$1" in
    pg-traveler-0) psql0 'SELECT pg_is_in_recovery();' 2>/dev/null ;;
    pg-traveler-1) psql1 'SELECT pg_is_in_recovery();' 2>/dev/null ;;
  esac
}

tls_enabled()  { [ -f .env ] && grep -qE '^PG_GUARD_SSL_CERT_FILE=.+' .env; }
mtls_required() { [ -f .env ] && grep -qE '^PG_GUARD_MTLS_REQUIRE=true' .env; }

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

api_url()
{
  local service="$1" path="$2" scheme="http"
  tls_enabled && scheme="https"
  echo "$scheme://localhost:$(port_of_service "$service")$path"
}

curl_api()
{
  local service="$1" path="$2"
  local -a extra=()
  if tls_enabled; then
    extra+=(--cacert tls/ca.crt)
    mtls_required && extra+=(--cert tls/tls.crt --key tls/tls.key)
  fi
  curl -s -o /tmp/switchover-loop-resp.json -w '%{http_code}' "${extra[@]}" -X POST "$(api_url "$service" "$path")"
}

resp() { cat /tmp/switchover-loop-resp.json 2>/dev/null; }

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
  local deadline=$((SECONDS + timeout))
  while [ "$SECONDS" -lt "$deadline" ]; do
    [ "$(role_of_service "$service")" = "$expected" ] && return 0
    sleep 1
  done
  return 1
}

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

container_restart_count() { docker inspect -f '{{.RestartCount}}' "$1" 2>/dev/null; }

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

# --- the loop itself ---

summary()
{
  header "RESULTS"
  echo "Cycles OK    : $CYCLES_OK"
  echo "Cycles failed: $CYCLES_FAILED"
  echo "Runtime      : $SECONDS seconds"
  echo
  rm -f /tmp/switchover-loop-resp.json
}
trap summary EXIT

if [ "$COUNT" -eq 0 ]; then
  info "running until Ctrl+C (interval ${INTERVAL_SEC}s between cycles)"
else
  info "running $COUNT switchover cycles (interval ${INTERVAL_SEC}s between cycles)"
fi

i=0
while [ "$COUNT" -eq 0 ] || [ "$i" -lt "$COUNT" ]; do
  i=$((i + 1))
  header "Cycle $i"

  PRIMARY="$(primary_service)"
  if [ -z "$PRIMARY" ]; then
    fail "no node currently reports primary -- cluster unhealthy, stopping"
    CYCLES_FAILED=$((CYCLES_FAILED + 1))
    break
  fi
  STANDBY="$(other_service "$PRIMARY")"
  info "switching over current primary $PRIMARY (-> $STANDBY becomes primary)"

  baseline_rc="$(container_restart_count "$PRIMARY")"
  code="$(curl_api "$PRIMARY" /api/switchover)"
  info "POST $PRIMARY/api/switchover -> $code: $(resp)"

  ok=1
  if [ "$code" != "200" ]; then
    fail "expected 200, got $code"
    ok=0
  fi
  wait_for_role "$STANDBY" f 20 && pass "$STANDBY promoted to primary" || { fail "$STANDBY did not become primary within 20s"; ok=0; }
  wait_for_restart_count_above "$PRIMARY" "$baseline_rc" 30 && pass "$PRIMARY container restarted" || { fail "$PRIMARY container did not restart within 30s"; ok=0; }
  wait_for_role "$PRIMARY" t 120 && pass "$PRIMARY rejoined as standby" || { fail "$PRIMARY did not report standby within 120s"; ok=0; }
  wait_for_api "$PRIMARY" 30 || warn "$PRIMARY's API did not respond within 30s of its postgres becoming reachable"

  if [ "$ok" = "1" ]; then
    CYCLES_OK=$((CYCLES_OK + 1))
  else
    CYCLES_FAILED=$((CYCLES_FAILED + 1))
    fail "cycle $i did not fully stabilize -- stopping rather than compounding onto an unhealthy cluster"
    break
  fi

  if [ "$COUNT" -eq 0 ] || [ "$i" -lt "$COUNT" ]; then
    info "sleeping ${INTERVAL_SEC}s before next cycle..."
    sleep "$INTERVAL_SEC"
  fi
done
