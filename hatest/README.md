# hatest

A standalone Java tool that measures what a real failover actually costs
an application with data already in flight -- as opposed to
`../traveler/PgTravelerProbe.java`, which answers a narrower question
("does a *new* connection find the current primary quickly"). `hatest`
writes real, persistent rows continuously and keeps writing across an
*externally* triggered failover, then reports concretely what survived.

General-purpose, not Traveler-specific: it creates and owns its own table
(`hatest_data`) and works against any two-node Postgres/pg-guard cluster.

`hatest` never talks to pg-guard's own API -- only to Postgres, on the two
addresses you give it. Trigger the failover yourself while a run is in
progress (`curl .../api/switchover`, `docker compose restart`, killing a
container, etc.).

One file, no build tool beyond `javac` -- pgJDBC is the only dependency,
referenced directly on the classpath, shared with `../traveler/PgTravelerProbe.java`.
Fetch it (checksum-verified) via `../bin/download-postgresql-jdbc.sh`, which
drops it in `../bin/` as `postgresql-jdbc.jar` (a fixed, version-less name
-- see that script for the actual pinned version/checksum).

## Build

```bash
cd hatest
../bin/download-postgresql-jdbc.sh
javac -cp ../bin/postgresql-jdbc.jar HaTest.java
```

## Run

Against this repo's own two-node docker-compose stack, use
**`docker/hatest.sh`** instead of building/invoking `HaTest` directly --
same "Clean start" pattern as `docker/test_roundtrip.sh` (tears down any
previous stack, drops the data volumes, brings up a fresh one, waits for
both nodes healthy), fetches the driver, compiles, and reads
`--node1`/`--node2`/`--user`/`--password`/`--sslmode`/`--sslrootcert`
straight from `docker/.env` -- one central place to run it:

```bash
cd docker
./hatest.sh --duration-sec 120
```

Against any other cluster, build and invoke `HaTest` directly:

```bash
java -cp .:../bin/postgresql-jdbc.jar HaTest run \
  --node1 localhost:5432 --node2 localhost:5433 \
  --user postgres --password changeme --dbname traveler \
  --duration-sec 120 --write-interval-ms 200
```

On Windows, use `;` instead of `:` in `-cp`.

(Ports above match `docker/docker-compose.yml`'s host-mapped Postgres ports
for `pg-traveler-0`/`pg-traveler-1` -- adjust to your actual setup.)

`--node1`/`--node2` are plain `host:port` -- the same values that would sit
inside a JDBC multi-host URL's host list
(`jdbc:postgresql://host1:port1,host2:port2/db`), just split into two flags
instead of one URL.

Add `--sslmode require|verify-ca|verify-full --sslrootcert ../docker/tls/ca.crt`
to test against a TLS-enabled Postgres (see the root README's TLS section
-- `docker/generate-certs.sh`'s output).

While it's running, trigger a failover from another terminal, e.g.:

```bash
curl -k -X POST https://localhost:8443/api/switchover
```

For several failover cycles during one run, use
`../docker/switchover-loop.sh --count 6 --interval-sec 10` instead of a
single manual `curl` -- it does not touch the docker-compose stack's
lifecycle (won't tear anything down mid-run), only triggers switchovers
against whichever node currently reports primary.

Add `--json` to print the final report as JSON instead of plain text.

## What it does

1. Polls `pg_is_in_recovery()` on both nodes to find the current primary.
   If neither is reachable at all, fails immediately with a clear message
   instead of grinding through the full `--duration-sec` retrying against
   something that was never up.
2. Creates `hatest_data` if it doesn't exist yet (a real table, not
   temporary -- durability *across* a promotion is exactly what's
   measured).
3. Every `--write-interval-ms`, inserts the next row on the current
   primary and tracks every write that got a successful commit.
4. Keeps re-checking both nodes' roles throughout the run. If a write
   fails (connection drop, read-only-transaction error mid-promotion), it
   pauses, waits for either node to report primary again, and resumes --
   recording the gap as an outage window (a rough RTO measurement). Prints
   a heartbeat every 5 seconds regardless, so a long quiet stretch (either
   writing normally or stuck in an outage) is never mistaken for a hang.
5. At the end, reads back everything under this run's `run_id` and
   compares against what it believes it committed.

Ctrl+C stops the write loop early and still runs the validation pass and
prints a report for whatever ran before the interrupt.

## Reading the report

- **lost (committed but missing)** -- the client got a commit
  acknowledgment for these rows, but they're gone. This is real data loss.
  A handful of rows lost in the instant of a promotion is *expected* under
  async replication (pg-guard doesn't configure
  `synchronous_standby_names`) -- the report states it plainly rather than
  pretending it shouldn't happen. If a report shows lost rows outside any
  outage window, that's worth investigating.
- **unexplained gaps** -- rows present in the database whose commit
  acknowledgment never reached the client (e.g. the connection dropped
  right after `COMMIT`, before the response arrived). This is an ambiguous
  "did my write succeed" case, not loss or corruption -- distinguished
  here so it isn't confused with either.
- **not acknowledged** -- writes the client attempted but never got any
  response for, and that never made it into the database either. The
  normal, expected shape of a write attempted during an outage window.
- **outage windows** -- periods where neither node reported itself
  primary. This is the rough RTO the test observed for that run.

Exit code is `0` only when `lost` is empty -- usable as a pass/fail gate.

## Table

```sql
CREATE TABLE IF NOT EXISTS hatest_data (
    run_id     text NOT NULL,
    local_seq  bigint NOT NULL,
    written_at timestamptz NOT NULL DEFAULT now(),
    node       text NOT NULL,
    value      text NOT NULL,
    PRIMARY KEY (run_id, local_seq)
);
```

Rows are scoped by `run_id` (a random hex string per invocation) and left
in place after a run -- re-running doesn't need to clean up first, and old
runs' data stays around for later inspection if useful. Drop the table
yourself if you want a clean slate.
