#!/bin/bash
# pg-guard -- docker/restart.sh -- docker compose down + up -d, preserving
# the data volumes (unlike kill_all.sh, which also wipes them). Takes no
# arguments.

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
  echo "Usage: ./restart.sh"
  echo "docker compose down + up -d, keeping the existing data volumes."
  exit 0
fi

docker compose down
echo
docker compose up -d
echo
