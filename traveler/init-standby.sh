#!/bin/bash
# Custom entrypoint for the standby: docker-entrypoint.sh only knows how to
# initdb a fresh cluster, not clone one from a peer, so this does the
# pg_basebackup step on first boot and then hands off to the normal
# entrypoint, which sees the now-populated PGDATA (with standby.signal) and
# just starts postgres in recovery mode as usual.
set -e

if [ ! -f "$PGDATA/PG_VERSION" ]; then
  pg_basebackup -h pg-traveler-0 -U replicator -D "$PGDATA" -P -R -X stream
fi

# pg_basebackup runs as root here (this script replaces the image's own
# entrypoint, so there's no root->postgres drop yet) -- it can leave
# intermediate directories (e.g. /var/lib/postgresql/18) owned by root,
# which blocks the postgres user from even traversing into PGDATA. Fix
# ownership on the whole tree, not just PGDATA itself.
chown -R postgres:postgres /var/lib/postgresql
chmod 700 "$PGDATA"

exec docker-entrypoint.sh postgres
