# pg-guard Docker stack

`docker-compose.yml` runs a two-node PostgreSQL streaming replication +
full pg-guard HA stack. Both nodes self-bootstrap (no init container --
see `../src/bootstrap.go`): `pg-traveler-0` (no ordinal-`1` suffix wins the
tiebreak) initializes as primary via `initdb`; `pg-traveler-1` waits for
it to report primary, then clones via `pg_basebackup`. Naming follows the
Kubernetes StatefulSet ordinal convention: `pg-traveler-0`/`pg-traveler-1`
(role is runtime state, not baked into which container is which -- it
flips on switchover).

## Database authentication

`POSTGRES_PASSWORD` is optional (defaults to unset, same trust-auth
convention as the root `docker-compose.yml`'s single-node smoke test and
`../linux/init-traveler-db.sh`) -- but set a real value in `docker/.env`
(gitignored -- copy `.env.example`) once Traveler's HA server needs to
connect, since it needs a real password to reach whichever node is
currently primary. `docker-compose.yml` reads it via
`${POSTGRES_PASSWORD:-}`.

Setting `POSTGRES_PASSWORD` at first bootstrap upgrades auth to
`scram-sha-256` (see [`../README.md`](../README.md)) -- but only for **network** connections
(`--auth-host`): replication and anything Traveler does over TCP. **Local**
(Unix-socket) connections stay trust (`--auth-local`, see
`../src/bootstrap.go`'s `bootstrapAsPrimary`), so `docker compose exec ...
psql` -- what `verify.sh`/`test_roundtrip.sh` both use -- keeps working without
needing `PGPASSWORD` set for those calls; that's a deliberate split, not an
oversight. TLS (`PG_GUARD_SSL_CERT_FILE`/`PG_GUARD_SSL_KEY_FILE`/
`PG_GUARD_SSL_CA_FILE`) is a separate, orthogonal setting -- see
`generate-certs.sh` and [`../README.md`](../README.md)'s TLS section.

### Setting a password later

You don't need to decide this at first bootstrap -- use
`pg-guard -set-password` instead of editing `docker/.env` and restarting.
It reads the new password from stdin (never argv, so it doesn't show up in
`ps`), connects over the local Unix socket (always trust, regardless of the
current network-facing auth mode -- see above), sets it on the `postgres`
user and replication role, upgrades `pg_hba.conf`'s application-access line
to `scram-sha-256`, and reloads. Run it against **every node** -- role
password changes replicate automatically from the primary, but
`pg_hba.conf` is a local file on each node, not replicated:

```bash
echo -n 'newpassword' | docker compose exec -T pg-traveler-0 /pg-guard -set-password
echo -n 'newpassword' | docker compose exec -T pg-traveler-1 /pg-guard -set-password
```

Unlike editing `.env`, this also cleanly handles **rotating** an
already-set password -- no restart, and no need to already know the old
value, since the local socket connection never depends on the network auth
mode it's changing. Never downgrades back to trust.

## Two listeners: API (8080) and metrics (9100)

`PG_GUARD_API_PORT` (`8080`/`8081` mapped here) serves only the mutating
`POST /api/*` routes (promote/shutdown/rejoin/switchover/maintenance/
start/reboot-notice) -- the two nodes talking to each other, not Traveler
talking to Postgres. `PG_GUARD_METRICS_PORT` (`9100`/`9101` mapped here,
matching `node_exporter`'s conventional port) serves `GET
/health`|`/ready`|`/status`|`/metrics`, unauthenticated, always -- separate
listeners so TLS can eventually apply to the API port alone (see
[`../README.md`](../README.md)'s TLS section). `docker/get-status.sh` and
`test_roundtrip.sh`'s `wait_for_api` already target the metrics port.

## HA API authentication

Gates the API listener above, optional and off by default; see
[`../README.md`](../README.md)'s Authentication section for the full explanation. Set
`PG_GUARD_API_TOKEN` in `docker/.env` (same value on both nodes) to
require a bearer token; set `PG_GUARD_PEER_VERIFY=ip` to additionally
require the caller resolve-match `PG_GUARD_PEER_HOST` (`ip` mode, not
`dns` -- Compose's reverse DNS won't match a plain service name here, same
reasoning as the replication grant's IP-scoped `pg_hba.conf` rule). The
metrics listener has no auth surface at all -- by design, not an
oversight.

## TLS

Off by default (plain HTTP) -- see [`../README.md`](../README.md)'s TLS
section for what setting `PG_GUARD_SSL_CERT_FILE`/`PG_GUARD_SSL_KEY_FILE`
actually switches on. `./generate-certs.sh` makes a local test CA + leaf
cert covering both node names + `localhost`, mounted at
`/run/secrets/tls` on both nodes -- symmetric setup, identical cert on
each. See also "Running it" below for what happens if `.env` enables TLS
but `tls/` wasn't (re)generated.

## `postgres:18` image conventions

`postgres:18` changed the image's own conventions vs. `17`: `PGDATA` moved
to a version-namespaced `/var/lib/postgresql/18/docker`, and the `VOLUME`
it declares is the parent `/var/lib/postgresql`, not `.../data` --
confirmed via `docker inspect postgres:18`.

## Data volumes

Each node's data lives in a named Docker volume (`postgres-data-0`,
`postgres-data-1` -- named by node ordinal, not by current role, since
that can flip), not a bind mount -- a bind mount under the repo hits real
permission trouble when run from WSL2 against a Windows-drive path
(`drvfs` has limited POSIX `chmod` support, and the container's `postgres`
UID won't match whatever UID owns the host directory). Named volumes
sidestep all of that; Docker manages the storage itself.

## Building the pg-guard binary

The pg-guard binary is bind-mounted directly (a single read-only file
mount is fine -- it's the writable Postgres data directory that was the
problem), not built by this compose file -- rebuild it before testing a
change (or just run `../build.sh`, which builds both platforms into
`../bin/`):

```bash
cd ../src && GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o ../bin/pg-guard-linux .
```

## Container naming and hostname

`container_name` (for DNS resolution/exec) and `hostname` (for the
container's own view of itself, e.g. `hostname`/`os.Hostname()`) are both
set explicitly and kept in sync -- Compose's `container_name` alone does
NOT set the container's own hostname (that defaults to the container ID
otherwise), which matters here since the README's peer-derivation design
has pg-guard look up its own hostname to find its `-0`/`-1` peer.

## Startup ordering

No `depends_on` between the two nodes -- both start in parallel;
pg-guard's own bootstrap retry loop (up to 120s) handles the ordering
that used to require `depends_on: service_healthy` against the init
containers. Expect a few normal, non-alarming "peer not primary yet,
retrying" lines from `pg-traveler-1` in the first several seconds. Both
healthchecks' generous `start_period` (150s) accounts for that worst-case
120s peer-wait plus the `pg_basebackup` clone itself -- `docker compose up
-d --wait` blocks on exactly this signal instead of a fixed sleep.

## Shutdown timing

`stop_grace_period` must exceed the full worst-case coordinated-shutdown
budget, or Compose SIGKILLs the whole container out from under pg-guard
mid-handover -- which also means postgres itself dies abruptly (a
container-wide SIGKILL, not just pg-guard's SIGTERM) rather than via its own
graceful shutdown. When the stopped node is primary, the budget is:

| Step | Worst case |
|---|---|
| role check (`isInRecovery`) | 3s |
| initial peer status check | 2s |
| `stopLocal` (`PG_GUARD_SHUTDOWN_WAIT` + force-kill confirm) | 30s + 5s |
| peer promote request | **70s** -- `pg_promote()` can legitimately take that long, so the HTTP client used for this call is given a 70s timeout, not a short one |
| confirmation poll (waiting for peer to report primary) | 10s |
| **total** | **~120s** |

`docker-compose.yml` sets `stop_grace_period: 150s` to leave margin above
that ~120s worst case. If you change `PG_GUARD_SHUTDOWN_WAIT`, re-derive this
number rather than guessing -- it does not shrink the 70s promote-wait or the
10s confirmation poll, both of which are independent of that setting.

## Shutdown mode

`PG_GUARD_SHUTDOWN_MODE` defaults to `switchover` here -- a plain
`POST /api/shutdown` or `docker stop` promotes the peer, same as
`POST /api/switchover` (see [`../README.md`](../README.md)'s Shutdown Modes). If you'd
rather a routine restart *not* flip primary/standby (some admins prefer
this -- an unnecessary role flip can be more disruptive than a brief
outage), uncomment `PG_GUARD_SHUTDOWN_MODE: "reboot"` in
`docker-compose.yml` on both nodes: `POST /api/shutdown` then just
notifies the peer to suppress automatic failover for
`PG_GUARD_REBOOT_GRACE_PERIOD` and comes back as primary unchanged.
`POST /api/switchover` and `POST /api/maintenance` are unaffected by this
setting either way -- both always promote the peer, an actual intentional
handoff.

## Backup default

`PG_GUARD_BACKUP_DIR` defaults here to `/var/lib/postgresql/pg-guard-backups`
(inside the same `postgres-data-N` volume as `PGDATA`) purely so `pg-guard
backup`/`POST /api/backup` work with zero setup in this test stack --
pg-guard itself has no built-in default (see [`../README.md`](../README.md)'s
Backup section: it fails loud rather than silently pick a location). Sharing
`PGDATA`'s volume is fine for testing but is **not** what a real deployment
should do -- if that volume/disk is ever lost, the backups are lost right
along with it, defeating the point. Override `PG_GUARD_BACKUP_DIR` in
`docker/.env` to point at genuinely separate storage, or set
`PG_GUARD_BACKUP_COMMAND` instead (mutually exclusive -- see
[`../README.md`](../README.md)) to pipe through something like `borg`/`restic`.
`PG_GUARD_BACKUP_ENABLED` is left at its default (`false`) here -- the
on-demand command works regardless; nothing runs on a schedule unless you
opt in.

## `/tmp` is tmpfs

Both nodes mount `/tmp` as `tmpfs`. Everything pg-guard itself stages
under `/tmp/pg-guard` -- TLS cert/key/CA copies (`stageTLSFile`, `tls.go`,
see [`../README.md`](../README.md)'s TLS section), the `initdb` password
file (`bootstrapAsPrimary`, `bootstrap.go`), and -- when
`PG_GUARD_METRICS_MODE` is `textfile`/`both` -- `pg-guard.prom`
(`PG_GUARD_TEXTFILE_DIR` defaults to `/tmp/pg-guard/metrics` here) -- is
regenerated on every startup or refresh cycle, never something that needs
to survive a restart. Backing all of `/tmp` this way means none of it ever
touches real disk, not even briefly; it also covers any scratch space
`pg_basebackup`/`initdb`/`psql` use on their own when invoked via `docker
compose exec`.

`PG_GUARD_STATE_FILE` is the opposite -- it defaults to
`/var/lib/postgresql/pg-guard-state.json`, inside the `postgres-data-N`
volume alongside `PGDATA` (same convenience-default pattern as
`PG_GUARD_BACKUP_DIR` above), specifically so it *does* survive a restart.
See [`../README.md`](../README.md)'s Automatic Restart section.

## Running it

```bash
docker compose up -d   # trust auth by default; cp .env.example .env first to set a real POSTGRES_PASSWORD
./verify.sh          # replication check: liveness, roles, replication connection, data actually replicates
./test_roundtrip.sh        # HA round trips: promote guard, coordinated shutdown, switchover (add -f for automatic failover too)
./kill_all.sh         # tear down and remove both volumes for a clean re-run
```

`docker/.env` and `docker/tls/` are both gitignored local state -- neither
exists on a fresh clone/checkout. If `.env` sets `PG_GUARD_SSL_CERT_FILE`
(e.g. copied over from a previous checkout) but `tls/` wasn't regenerated
alongside it, pg-guard fails config validation at startup (`reading
/run/secrets/tls/tls.crt: no such file or directory`) and the container
never becomes healthy -- confirmed as a real, confusing failure mode:
`docker compose up -d --wait` just times out, with nothing in that error
pointing at the missing cert file (see its own log,
`docker compose logs pg-traveler-0`). Run `./generate-certs.sh` to
(re)create `tls/` whenever `.env` has TLS enabled and this is a fresh
checkout; delete/comment out `PG_GUARD_SSL_CERT_FILE` in `.env` to run
without TLS instead.
