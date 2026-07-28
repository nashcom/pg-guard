# pgjdbc-ha-probe

Small Java 17 test and diagnostic client for a two-node PostgreSQL setup.
One file, one dependency (pgJDBC), no build tool -- `pgJDBC` is referenced
directly on the classpath. Listener mode uses the JDK's built-in
`jdk.httpserver` module and binds to loopback by default.

## Build & run

Fetch the pgJDBC driver jar (checksum-verified) via
`../bin/download-postgresql-jdbc.sh`, which drops it in `../bin/` as
`postgresql-jdbc.jar` (a fixed, version-less name -- see that script for
the actual pinned version/checksum), shared with `../hatest/`:

```bash
../bin/download-postgresql-jdbc.sh
javac -cp ../bin/postgresql-jdbc.jar PgTravelerProbe.java
java  -cp .:../bin/postgresql-jdbc.jar PgTravelerProbe probe --url "$PGTEST_URL"
```

Or point `-cp` at wherever you already have the jar (e.g. in a Domino
container) instead of using the download script.

On Windows, use `;` instead of `:` in `-cp`.

## Recommended test URL

```text
jdbc:postgresql://pg-traveler-0:5432,pg-traveler-1:5432/traveler?targetServerType=primary&hostRecheckSeconds=2&connectTimeout=3&loginTimeout=8&tcpKeepAlive=true
```

## One-shot probe

```bash
java -cp .:../bin/postgresql-jdbc.jar PgTravelerProbe probe \
  --url "$PGTEST_URL" \
  --user traveler \
  --password secret \
  --format json \
  --write-test
```

Exit code is `0` on success and `2` on probe failure.

## Continuous watch

```bash
java -cp .:../bin/postgresql-jdbc.jar PgTravelerProbe watch \
  --url "$PGTEST_URL" \
  --format text \
  --interval-ms 1000
```

## Listener mode

```bash
java -cp .:../bin/postgresql-jdbc.jar PgTravelerProbe serve \
  --url "$PGTEST_URL" \
  --bind 127.0.0.1 \
  --port 9187 \
  --interval-ms 1000
```

Endpoints:

```text
GET  /health/live
GET  /v1/status?format=json
GET  /v1/status?format=text
GET  /v1/status?format=headers
GET  /v1/probe?format=json&writeTest=true
POST /v1/probe?format=json&writeTest=true
```

Every HTTP response also carries `X-PG-PROBE-*` headers, making the result usable
by simple scripts without parsing JSON.

## Formats

### Text

```text
ok=true
role=primary
server_address=10.0.0.11
...
```

### Header-style body

```text
X-PG-PROBE-OK: true
X-PG-PROBE-ROLE: primary
X-PG-PROBE-SERVER-ADDRESS: 10.0.0.11
...
```

### JSON

```json
{"ok":true,"role":"primary","server_address":"10.0.0.11"}
```

## Probe behavior

The read probe reports:

- actual server address and port;
- PostgreSQL backend PID;
- primary or standby role through `pg_is_in_recovery()`;
- transaction read-only state;
- server version and optional cluster name;
- whether *this connection* actually ended up using TLS, plus its
  version/cipher, via `pg_stat_ssl` -- independent of what `sslmode` was
  requested, since the default `prefer` mode uses TLS silently if the
  server offers it, with no flag needed to see whether it happened;
- connection and total probe duration;
- SQL state and vendor code on failure.

`--write-test` starts a transaction, creates a temporary table, inserts one row,
and rolls the transaction back. It does not create persistent test data.

Note: pgJDBC's multi-host URL routing (`targetServerType`) only applies when
opening a *new* connection -- it does not migrate a connection that was
already open when its host failed. `watch` mode opens a fresh connection
every interval, so it measures how fast a new connection finds the current
primary, not what happens to a connection an application was already
holding open at the moment of failover.

## Security

Listener mode is intentionally bound to `127.0.0.1` by default. Do not expose it
to an untrusted network without authentication and TLS in front of it. Avoid
placing passwords directly on the command line; environment variables or a
protected configuration file are preferable.

## pgJDBC logging

pgJDBC uses `java.util.logging`. Run with:

```bash
java \
  -Djava.util.logging.config.file=logging.properties \
  -cp .:../bin/postgresql-jdbc.jar PgTravelerProbe probe --url "$PGTEST_URL"
```
