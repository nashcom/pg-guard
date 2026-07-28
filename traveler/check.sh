#!/bin/bash
# pg-guard -- traveler/check.sh -- quick one-shot PgTravelerProbe.java call
# against localhost:5432/5433 with a write-test, JSON output piped through
# jq. A saved shortcut, not a general tool -- edit PG_HOST/PG_USER/
# PG_JDBC_JAR below directly if your setup differs. Takes no arguments.

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
  echo "Usage: ./check.sh"
  echo "One-shot PgTravelerProbe.java probe (--write-test, JSON) against localhost:5432/5433."
  exit 0
fi

PG_HOST=localhost
PG_USER=postgres
PG_JDBC_JAR=../bin/postgresql-jdbc.jar

export PGTEST_URL="jdbc:postgresql://$PG_HOST:5432,$PG_HOST:5433/traveler?targetServerType=primary&hostRecheckSeconds=2&connectTimeout=3&loginTimeout=8&tcpKeepAlive=true"

java -cp .:$PG_JDBC_JAR PgTravelerProbe probe --url "$PGTEST_URL" --user "$PG_USER" --format json --write-test | jq
