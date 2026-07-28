#!/bin/bash
# pg-guard -- docker/get-metrics.sh -- dumps GET /metrics (Prometheus text
# format, comment lines stripped) from both nodes' metrics listeners
# (:9100/:9101, always plain HTTP regardless of TLS config). Takes no
# arguments.

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
  echo "Usage: ./get-metrics.sh"
  echo "Dumps GET /metrics from both nodes (pg-traveler-0:9100, pg-traveler-1:9101)."
  exit 0
fi

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



header "pg-traveler-0"
curl -s --connect-timeout 4 http://localhost:9100/metrics | grep -v "^#"

header "pg-traveler-1"
curl -s --connect-timeout 4 http://localhost:9101/metrics | grep -v "^#"

echo
