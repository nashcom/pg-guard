# Testing pg-guard

This is the index for every dev/test tool in this repo -- how to bring up
a stack, verify replication, run the HA round trips, and do deeper
failover-survival testing. See [`README.md`](README.md) for pg-guard itself
(architecture, configuration, behavior); this file is purely about
exercising it.

Every script below (`docker/`, `linux/`, `windows/`, `bin/`) supports
`-h`/`--help` (`/?` too on the `.cmd` scripts) for its full usage/flags --
this index gives the gist of each, not the exhaustive reference.

## CI

`.github/workflows/build.yml` runs on every push/PR: `gofmt -l` (fails on
any unformatted file), `go vet ./...`, then `./build.sh` -- exactly the
three checks used by hand throughout this project's own development.
Linux-only for now (the actual deployment target this whole test suite
exercises); the Windows target still gets built as part of `./build.sh`
(so a Windows-specific break is still caught), just not published as an
artifact yet. `fetch-depth: 0` on checkout is load-bearing, not
boilerplate -- `build.sh`'s version embedding (`git describe`) needs full
tag history, which a shallow checkout doesn't have.

Also has a manual "Run workflow" trigger (`workflow_dispatch`, in the
Actions tab) for re-running the same checks on demand without a dummy
commit. On an actual version tag push (`vX.Y.Z`) -- or a manual dispatch
run pointed at one via that button's branch/tag picker -- it additionally
creates a GitHub Release and attaches the built Linux binary; a plain
branch push or PR never does, since neither has a tag ref.

## Smoke test: single node, no HA

The fastest way to see `pg-guard` running at all -- no peer, no HA logic,
just PID-1 supervision and self-bootstrap in isolation (root
`docker-compose.yml`):

```bash
./build.sh                    # builds bin/pg-guard-linux
docker compose up -d
curl localhost:9100/status    # confirm it's up and self-bootstrapped
docker compose down -v
```

## Two-node HA stack

`docker/docker-compose.yml` runs the full two-node stack -- pg-guard
self-bootstraps both nodes (see [`README.md`](README.md)'s Bootstrap section), no init
container needed. All commands below are run from `docker/`.

- **`./verify.sh`** -- checks replication end-to-end: both nodes up,
  correct primary/standby role, a replication connection present, data
  written on the primary actually shows up on the standby. Run this first
  after bringing the stack up.
- **`./test_roundtrip.sh [-f]`** -- the HA round trips: promote guard,
  coordinated shutdown, switchover, and (with `-f`) automatic failover.
  Tears down and rebuilds the stack fresh itself ("Clean start"), flips
  which node is primary along the way, and restores the cluster to a
  healthy two-node state before finishing.
- **`./kill_all.sh`** -- full teardown, including the data volumes.
- **`./restart.sh`** -- `docker compose down` + `up -d`, keeping the data
  volumes (lighter than `kill_all.sh`).
- **`./generate-certs.sh`** -- generates a local micro-CA + leaf cert for
  testing with TLS enabled (see [`README.md`](README.md)'s TLS section).

### On-demand backup

No dedicated script -- `pg-guard backup` is already a direct, one-line
command (see [`README.md`](README.md)'s Backup section), same as
`set-password`:

```bash
docker compose exec pg-traveler-0 /pg-guard backup
docker compose exec pg-traveler-0 /pg-guard backup -stdout > /tmp/pg-traveler-backup.tar.gz
```

Check `./get-status.sh` or `GET /metrics` (`postgres_ha_backup_*`, disk-based
and piped-command paths only, not `-stdout`) to confirm. To try
`PG_GUARD_BACKUP_COMMAND` (piping through an external tool instead of
writing to a directory -- see [`README.md`](README.md)'s Backup section),
set it in `docker/.env` (e.g. `PG_GUARD_BACKUP_COMMAND=gzip >
/tmp/pg-test-$(date -u +%s).tar.gz`) instead of `PG_GUARD_BACKUP_DIR`, then
trigger the same way.

## Live observation

- **`./get-status.sh`** -- one-shot, full `GET /status` JSON from both
  nodes. **`./get-status.sh -w [INTERVAL_SEC]`** -- watch mode: a
  side-by-side table (t-0 vs. t-1) of just the fields worth watching live
  (role, reachability, replication lag, every "something's in progress"
  flag including `backup_in_progress`), plus two computed rows per node:
  `time_since_change` and `last_backup` (time since that node's last backup
  *attempt* -- `never` if none this run, `FAILED <time>` if the last
  attempt didn't succeed, with the actual error text printed below the
  table in that case, so a currently-broken schedule is visible
  immediately rather than looking like "just hasn't run yet"), refreshed
  every second by default. Good to have running in its own terminal while triggering a
  switchover/failover -- or a backup -- elsewhere.
- **`./get-metrics.sh`** -- dumps `GET /metrics` (Prometheus text) from
  both nodes.

To try the textfile-collector mode (see [`README.md`](README.md)'s Metrics
section) instead of/alongside `GET /metrics`, set
`PG_GUARD_METRICS_MODE=textfile` (or `both`, to keep `GET /metrics` too)
in `docker/.env` -- `PG_GUARD_TEXTFILE_DIR` already has a default here
(`/tmp/pg-guard/metrics`, created automatically if it doesn't exist;
bind-mount that path if you want to inspect the file from outside the
container), so nothing else to configure. Confirm `pg-guard.prom` appears
and refreshes every `PG_GUARD_TEXTFILE_INTERVAL` (default `60s`):

```bash
docker compose exec pg-traveler-0 cat /tmp/pg-guard/metrics/pg-guard.prom
```

## Failover-survival testing

Two separate Java client-side tools, both plain `javac`/pgJDBC, no build
tool, neither part of pg-guard's own build -- see each one's own
README.md for full detail:

- **`traveler/PgTravelerProbe.java`** -- does a *new* JDBC connection find
  the current primary quickly (pgJDBC's own multi-host URL routing, no
  persistent data, reports TLS status via `pg_stat_ssl`). `traveler/check.sh`
  is a saved one-shot shortcut for it (`--write-test`, JSON, piped through
  `jq`) against `localhost:5432`/`5433` -- edit the host/user variables at
  the top of the script if your setup differs.
- **`hatest/HaTest.java`** -- what does an actual failover cost an
  application with data already in flight: writes real, persistent rows
  continuously across an *externally* triggered failover, then reports
  exactly what survived (lost rows, unexplained gaps, outage windows).
  `hatest` never triggers a failover itself -- that's a deliberate scope
  boundary, kept to talking only to the two Postgres endpoints, agnostic
  to *how* the failover happens (deliberate switchover, a crash, a network
  partition).

Run it against the docker stack via `docker/hatest.sh`, which reads
connection details from `docker/.env` the same way `verify.sh`/
`test_roundtrip.sh` do:

```bash
cd docker
./hatest.sh --duration-sec 120
```

To actually trigger a failover while `hatest` is running, pair it with
**`docker/switchover-loop.sh`** in a second terminal -- repeatedly
triggers `POST /api/switchover` against whichever node is currently
primary, waiting for each cycle to fully stabilize before the next one.
Unlike `test_roundtrip.sh`, it does **not** touch the stack's lifecycle
(no teardown/rebuild), since that would kill whatever `hatest` is
connected to and wipe its in-progress test data:

```bash
./switchover-loop.sh --count 6 --interval-sec 10
```

(`--count` defaults to an even number so the cluster ends back on its
original primary/standby configuration -- each switchover flips it.)

**`docker/check_postgres.sh [SERVICE] [INTERVAL_SEC]`** -- a lighter-weight
companion for exact downtime duration: polls one node via `pg_isready`
(same tool `verify.sh`'s liveness check and `docker-compose.yml`'s own
`healthcheck:` use) and prints `UP`/`DOWN` transitions with the precise
outage length, e.g. `UP after 2.341 seconds`. Doesn't measure application
data survival the way `hatest` does -- just "how long was it actually
unreachable" for a given node, useful on its own or alongside `hatest` for
a second, independent measurement of the same outage window:

```bash
./check_postgres.sh pg-traveler-0
```

### Recommended workflow for a real HA validation pass

Three terminals, all from `docker/`:

1. `./hatest.sh --duration-sec 180` -- the write loop, running the whole time.
2. `./switchover-loop.sh --count 6 --interval-sec 20` -- triggers several
   failover cycles during that run.
3. `./get-status.sh -w` -- live view of what's actually happening to both
   nodes while it runs.

Optionally add a fourth, `./check_postgres.sh pg-traveler-0`, for the
precise per-outage duration alongside `hatest`'s own end-of-run report.

Read `hatest`'s final report for the actual verdict (any lost rows are a
real bug worth investigating unless they line up with an outage window,
where a handful lost right at the promotion instant is expected under
async replication -- see [`hatest/README.md`](hatest/README.md)).

## Manual dev scripts (no pg-guard involved)

`windows/` and `linux/` contain parallel manual setup scripts (`.cmd`/`.sh`)
for getting a Traveler-named database up by hand outside any deployment
automation, entirely without pg-guard: `init-traveler-db` (initdb + create
the `traveler` database, ICU `und` locale, `template0`),
`start-postgres`/`stop-postgres`, `check-status`, `psql-traveler`. Useful
for isolating "is this a Postgres problem or a pg-guard problem" -- these
scripts touch nothing pg-guard-specific at all.

## Shared dependency

Both Java tools use the same pgJDBC driver jar, fetched (checksum-verified)
via `bin/download-postgresql-jdbc.sh` into `bin/postgresql-jdbc.jar` -- a
fixed, version-less name so nothing referencing the classpath needs to
change when the pinned version is bumped (only that script does).
