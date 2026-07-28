#!/bin/bash
# pg-guard -- docker/kill_all.sh -- full teardown: docker compose down,
# plus removes the two data volumes (postgres-data-0/1) so the next
# "docker compose up" starts from a genuinely clean state. Takes no
# arguments.

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
  echo "Usage: ./kill_all.sh"
  echo "docker compose down, then removes postgres-data-0/postgres-data-1."
  exit 0
fi

docker compose down

docker volume rm postgres-data-0 2>/dev/null || true
docker volume rm postgres-data-1 2>/dev/null || true
