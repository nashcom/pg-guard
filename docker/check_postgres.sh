#!/bin/bash
# pg-guard -- docker/check_postgres.sh -- polls a node via pg_isready and
# prints UP/DOWN transitions with exact outage duration. Uses `docker
# compose exec SERVICE pg_isready -U postgres` -- the same tool/pattern
# verify.sh's liveness check and docker-compose.yml's own healthcheck:
# block already use, not a hand-rolled TCP/wire-protocol probe. pg_isready
# is Postgres's own purpose-built client tool: it performs the connection
# attempt correctly via real libpq (properly completing or declining TLS
# negotiation as needed), so there's no half-finished handshake to worry
# about and no "unexpected eof" noise in Postgres's own logs the way a
# partial hand-rolled probe can cause. Also sidesteps host-side tool
# availability entirely -- neither pg_isready nor nc are installed on this
# project's own dev host; pg_isready only needs to exist inside the
# container, where it always does (it ships with Postgres itself).
#
# Usage: ./check_postgres.sh [SERVICE] [INTERVAL_SEC]
#   SERVICE       docker compose service name to poll (default pg-traveler-0)
#   INTERVAL_SEC  poll interval in seconds (default 0.2)

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
  echo "Usage: ./check_postgres.sh [SERVICE] [INTERVAL_SEC]"
  echo "  SERVICE       docker compose service to poll (default pg-traveler-0)"
  echo "  INTERVAL_SEC  poll interval in seconds (default 0.2)"
  echo
  echo "Polls via 'docker compose exec SERVICE pg_isready -U postgres',"
  echo "printing UP/DOWN transitions with exact outage duration. Run from"
  echo "the docker/ directory (same as verify.sh etc.). Ctrl+C to stop."
  exit 0
fi

SERVICE="${1:-pg-traveler-0}"
INTERVAL="${2:-0.2}"

ts() { date -u +"%Y-%m-%dT%H:%M:%SZ"; }

echo "Monitoring PostgreSQL on service '${SERVICE}' (pg_isready)"
echo

probe()
{
  docker compose exec -T "$SERVICE" pg_isready -U postgres >/dev/null 2>&1
}

# UP starts at -1 ("not yet checked"), not 0/1 -- so the very first probe,
# whichever way it goes, always prints its result, rather than a target
# that's down from the first check ever running silent forever.
UP=-1
DOWN_START=0

while true; do
  if probe; then
    if [ "$UP" -ne 1 ]; then
      NOW=$(date +%s.%N)

      if [ "$DOWN_START" != "0" ]; then
        DURATION=$(awk "BEGIN {print $NOW - $DOWN_START}")
        printf "[%s] UP after %.3f seconds\n" "$(ts)" "$DURATION"
      else
        printf "[%s] UP\n" "$(ts)"
      fi

      UP=1
      DOWN_START=0
    fi
  else
    if [ "$UP" -ne 0 ]; then
      DOWN_START=$(date +%s.%N)
      printf "[%s] DOWN\n" "$(ts)"
      UP=0
    fi
  fi

  sleep "$INTERVAL"
done
