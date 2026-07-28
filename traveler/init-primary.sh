#!/bin/bash
# Runs automatically via docker-entrypoint.sh's /docker-entrypoint-initdb.d/
# hook on first boot only. Adds what's needed for pg-traveler-1 to stream
# from this node: a replication role and a permissive pg_hba.conf entry
# (trust, dev/test only).
set -e

psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<-EOSQL
  CREATE ROLE replicator WITH REPLICATION LOGIN;
EOSQL

echo "host replication replicator all trust" >> "$PGDATA/pg_hba.conf"
